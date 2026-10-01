# Infrastructure smoke-test controller

A Kubernetes controller that runs declarative infrastructure checks and stores
their results in `SmokeTestRun` resources. The first definitions validate
Cluster Autoscaler scale-up, EBS CSI volume persistence, and the AWS IRSA
pod-identity webhook. See [the design](new-plan.md) for the intended MVP and
[examples](examples) for the current API.

The controller and probes are subcommands of **one binary and one image**.
Unless `--probe-image` is supplied, the controller reads its own Pod and uses
the `controller` container's `spec.image` for all probe Pods. Each Run snapshots
that reference before creating resources. Updating the Deployment image is
enough to update the default probes for new Runs; no second image setting is
required. Prefer a digest in the Deployment so the same reference always
identifies the same build.

## Build and test

Install Nix with flakes enabled, then use the locked development environment:

```sh
nix develop path:.
make generate
make check
make build
```

`make check` runs vet, race-enabled unit tests, API-server integration tests,
manifest builds, Terraform validation and mocked tests, formatting checks, and
GitHub Actions linting. The flake provides Go, controller-gen, kube-apiserver,
etcd, kubectl, Kustomize, Terraform, and the other tools. Envtest does not download
executables at test time. Integration tests start local API-server/etcd processes
and simulate infrastructure status.
They do not connect to your cluster or AWS account.

Nix also builds the package and container without a Docker daemon:

```sh
nix build path:.#default     # result/bin/infra-smoketest
nix build path:.#image       # result is a Docker image archive
```

The image contains both `controller` and `probe` commands, a CA certificate
bundle, and runs as UID/GID 65532. The default command starts the controller;
the generated probe Pods select their subcommand through native Kubernetes
`command` and `args`. Linux x86_64 and aarch64 flake outputs are defined; the
initial GitHub Actions workflow builds and publishes x86_64 images.

When changing dependencies, run `nix develop path:. --command go mod tidy`,
update the flake's `vendorHash` using the hash reported by a Nix build, and rerun
the checks. Keep the Go module lock and `flake.lock` in version control.

## GitHub Actions

[ci.yaml](.github/workflows/ci.yaml) tests PRs, `main`, and `v*` tags using the
same flake. It verifies generated CRDs/DeepCopy code and builds the combined
controller/probe image. After successful push builds, it publishes to
`ghcr.io/<repository-owner>/<repository-name>` using `GITHUB_TOKEN`:

- `sha-<full-commit>` for every published build.
- `latest` on `main`.
- The version tag on `v*` tag pushes.

PRs and manual workflow runs do not publish. Actions are pinned to commits.
Publishing requires the repository's Actions token to have package write
access. For private images, configure `imagePullSecrets` on the controller
Deployment and both probe ServiceAccounts. Nothing has been pushed or
published by preparing this repository.

## Install and run

The manifests in [config/default](config/default) install the namespace,
controller Deployment, controller/probe/identity ServiceAccounts, shared AWS
configuration, RBAC, and all three `SmokeTest` definitions from [examples](examples).
Install the CRDs from
[config/crd](config/crd) first. The controller is restricted to the
`infra-smoketest` namespace and has read-only cluster permissions. It does not
create IAM roles or ASGs. Kustomize installs the identity ServiceAccount;
the controller only reads it.

Before a live test, arrange these prerequisites through their owning repos:

- Autoscaling: a discoverable scale-from-zero ASG whose nodes have
  `turnkey.engineering/designated-for=smoke-tests` and the matching `NoSchedule`
  taint. Mirror the selector/taint in ASG node-template tags. Other workloads
  must not resize or use this pool during the test.
- Storage: the existing `ebs-gp3` class, EBS CSI nodes on ordinary AWS workers,
  and `Delete` reclaim policy. The controller checks gp3/encryption configuration
  and Kubernetes CSI state; it does not query actual EBS volume properties.
- Identity: [config/aws-pod-identity-webhook](config/aws-pod-identity-webhook)
  creates `infra-smoketest/infra-smoketest-aws` and is included in `config/default`.
  Set `AWS_ACCOUNT_ID` and `AWS_IDENTITY_ROLE_NAME` once in its
  [kustomization.yaml](config/aws-pod-identity-webhook/kustomization.yaml).
  The account defaults to `361645878370`, matching the existing `tvc-dev`
  GitOps configuration, and the role name defaults to `infra-smoketest-aws`.
  Kustomize builds the ServiceAccount's role ARN from these values, and the
  controller reads the same generated ConfigMap through `configMapKeyRef`.
  The role's OIDC trust must allow subject
  `system:serviceaccount:infra-smoketest:infra-smoketest-aws` and audience
  `sts.amazonaws.com`. The role needs no attached AWS service policy for
  `GetCallerIdentity`. The [Terraform module](config/terraform/aws) creates this
  role using the cluster's existing IAM OIDC provider; its runnable example reads
  the same shared account and role settings. Region defaults to
  `us-east-1` and can be set with `--region`.

Every Run saves the shared account as `status.awsAccountID`, alongside
`status.expectedRoleARN` and `status.region`. Every probe receives
`SMOKETEST_AWS_ACCOUNT_ID`, `SMOKETEST_EXPECTED_ROLE_ARN`, and `SMOKETEST_REGION`
from that snapshot; definitions do not need to repeat them. These are controller
supplied environment variables, not substitution expressions in SmokeTest YAML.
New probe implementations can read the shared account directly. The identity
probe already checks the caller account through the expected role ARN.

For custom controller deployments, `--aws-account-id=ACCOUNT` derives
`arn:aws:iam::ACCOUNT:role/infra-smoketest-aws`; `--identity-role-name=NAME`
selects a different role in that account. Existing `--expected-role-arn`
configuration remains supported, including IAM paths and other partitions;
when also supplying an account or role name, they must match the ARN.
Without AWS settings, autoscaler and storage tests can still run, while the
identity test fails its configuration precondition.

Changing the shared values and reapplying `config/default` changes the generated
ConfigMap name and rolls out the controller. Already accepted Runs retain their
snapshot. The ServiceAccount annotation must still match a Run's saved role
before it can create an identity Pod.

Make a deployment overlay to select your published image digest, for example:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../../config/default
images:
  - name: ghcr.io/tkhq/infra-smoketest
    newName: ghcr.io/YOUR-OWNER/YOUR-REPOSITORY
    digest: sha256:YOUR-DIGEST
```

The shared account flags require a controller image built from this revision.
Build/publish it and update the pinned digest before applying the Deployment.

Install the CRDs, then apply your overlay. The overlay includes the definitions
through `config/default`; no separate examples installation is needed. Installing
the definitions does not start a test; create a fresh Run for every execution:

```sh
kubectl apply --server-side -k config/crd
kubectl wait --for=condition=Established --timeout=60s \
  crd/smoketests.smoketest.turnkey.engineering \
  crd/smoketestruns.smoketest.turnkey.engineering
kubectl apply --server-side -k path/to/your-overlay
kubectl get smoketests -n infra-smoketest
kubectl create -f - <<'YAML'
apiVersion: smoketest.turnkey.engineering/v1alpha1
kind: SmokeTestRun
metadata:
  generateName: autoscaler-
  namespace: infra-smoketest
spec:
  testRef:
    name: cluster-autoscaler
  context:
    trigger: manual
YAML
kubectl get smoketestruns -n infra-smoketest
kubectl get smoketestrun RUN-NAME -n infra-smoketest -o yaml
```

Use server-side apply for installation: embedding native Kubernetes template
schemas makes these CRDs too large for some client-side apply annotations.

`spec.testRef` selects a `SmokeTest` definition in the Run's namespace. List
available definitions with `kubectl get smoketests -n infra-smoketest`, then set
`testRef.name` to one of the returned names. The default deployment installs
`cluster-autoscaler`, `ebs-csi`, and `aws-pod-identity-webhook`. Optional `testRef.uid`
and `testRef.generation` require a matching definition UID and generation;
otherwise, the Run snapshots the current definition when accepted.

Use `kubectl explain smoketestrun.spec.testRef` or
`kubectl explain smoketest.spec.stages` for field descriptions.

`kubectl get smoketestruns` includes `FAILEDSTAGE`, taken from the existing
`status.stages` entries whose phase is `Failed`. For example, a timeout while
waiting for autoscaler scale-up shows `TimedOut` and `scale-up`. The stage remains
visible through cleanup and in the final result. Runs with no failed stage,
including execution-slot queue timeouts and missing-definition failures, leave
the column empty. Use `kubectl get smoketestruns -o wide` for the full `MESSAGE`,
including whether the queue, a stage, or the overall execution deadline expired.
For observation failures or cancellation, `FAILEDSTAGE` identifies the stage that
was interrupted; the message explains the underlying failure.

These columns also work with existing Runs after applying the updated CRD with
`kubectl apply --server-side -k config/crd`; no controller image update is needed.

`kubectl wait --for=condition=Complete ...` only waits for a terminal result:
also inspect `Succeeded` and `CleanupComplete`; completion alone does not mean
the test passed. The API rejects changes to an existing Run's spec.

The native Pod templates in [examples](examples) omit `containers[].image` to
inherit the controller image. An explicit template image must match the resolved
probe image; set `--probe-image` on the controller to deliberately change the
approved probe build. For local development outside a Pod, supply this flag
because no deployed controller image can be discovered:

```sh
nix develop path:. --command go run ./cmd/infra-smoketest controller \
  --probe-image=ghcr.io/YOUR-OWNER/YOUR-REPOSITORY@sha256:YOUR-DIGEST
```

## Runtime behavior and limits

The controller logs Run acceptance, stage progress and deadlines, resource
creation/deletion, observed scheduling/storage evidence, and cleanup outcomes at
the default info level. Entries identify the Run, its UID, the test definition,
and the stage or resource where applicable. Unchanged waits are not logged on
every poll. Add `--zap-log-level=debug` to the controller arguments for reconcile
and waiting messages on each poll; `--zap-encoder=console` enables console output.

Follow logs with `kubectl logs -n infra-smoketest deployment/infra-smoketest -f`.
Run status remains the durable record of progress and results:
`kubectl get smoketestrun RUN-NAME -n infra-smoketest -o yaml`.

Run definitions, image references, stage deadlines, resource UIDs, transition
evidence, and probe results are persisted in Run status. Pod/PVC names are
deterministic; creation is recovered after a controller restart without adopting
unrelated resources. Probe image discovery uses the controller Pod's configured
image reference, not a runtime-specific `imageID`.

One execution Lease serializes all tests. A queued Run times out after ten
minutes; active execution allows 25 minutes plus five minutes for cleanup.
Submit longer batches sequentially. Cleanup deletes only Run-owned Pods/PVCs
and waits for recorded CSI dependents to disappear. It never deletes Nodes,
PVs, VolumeAttachments, or AWS resources directly. If cleanup stalls, the Run
fails, retains its finalizer and execution slot, and keeps attempting cleanup.
Inspect the remaining Pod/PVC/PV/attachment and repair its underlying problem;
do not remove finalizers to force a passing result. Drain Runs before removing
the controller or CRDs. A Lease referring to a forcibly removed Run requires
manual recovery after verifying that its resources have been cleaned up.

Cleaned Run records and their diagnostic ConfigMaps are retained for seven
days. The initial diagnostic artifact contains bounded status evidence; richer
Event/log bundles and infrastructure version inventory remain follow-on work.
Scale-down is not a required assertion. No live AWS/cluster validation is
claimed by the local tests: the smoke ASG and IAM prerequisites are still
required for acceptance in dev and preprod. GitOps deployment and promotion
integration remain separate work.
