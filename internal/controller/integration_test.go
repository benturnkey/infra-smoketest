//go:build integration

package controller

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	api "github.com/tkhq/infra-smoketest/api/v1alpha1"
	"github.com/tkhq/infra-smoketest/internal/definition"
	coordv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/diff"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

func newIntegrationClient(t *testing.T) client.Client {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("run integration tests through nix develop")
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{"../../config/crd"}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = api.AddToScheme(scheme)
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err = c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: definition.Namespace}}); err != nil {
		t.Fatal(err)
	}
	if err = c.Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: definition.ProbeSA, Namespace: definition.Namespace}}); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDefinitionMetadataRoundTrip(t *testing.T) {
	c := newIntegrationClient(t)
	ctx := context.Background()
	strict := client.FieldValidation(metav1.FieldValidationStrict)
	for _, name := range []string{"cluster-autoscaler", "ebs-csi", "aws-pod-identity-webhook", "cert-manager", "kube-state-metrics"} {
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("..", "..", "examples", name+".yaml"))
			if err != nil {
				t.Fatal(err)
			}
			test := &api.SmokeTest{}
			if err = yaml.Unmarshal(b, test); err != nil {
				t.Fatal(err)
			}
			test.Namespace = definition.Namespace
			if name == "cert-manager" {
				test.Spec.Stages[0].CreatePod.Template.Spec.Containers[0].Env = []corev1.EnvVar{
					{Name: definition.CertManagerIssuerNameEnv, Value: "cluster-ca"},
					{Name: definition.CertManagerIssuerKindEnv, Value: "ClusterIssuer"},
				}
			}
			want := test.Spec.DeepCopy()
			// Strict validation turns unknown-field warnings into errors, while
			// reading back the spec also detects silent field pruning.
			if err = c.Patch(ctx, test, client.Apply, client.FieldOwner("metadata-roundtrip"), strict); err != nil {
				t.Fatalf("apply definition with strict field validation: %v", err)
			}
			stored := &api.SmokeTest{}
			if err = c.Get(ctx, key(name), stored); err != nil {
				t.Fatal(err)
			}
			if !equality.Semantic.DeepEqual(want, &stored.Spec) {
				t.Fatalf("definition changed during storage (-want +got):\n%s", diff.Diff(want, &stored.Spec))
			}
			if err = definition.Validate(stored.Spec, "test/image@sha256:123"); err != nil {
				t.Fatalf("stored definition is no longer valid: %v", err)
			}
			// Run snapshots embed the same templates in a separate CRD schema.
			run := &api.SmokeTestRun{ObjectMeta: metav1.ObjectMeta{Name: "metadata-" + name, Namespace: definition.Namespace}, Spec: api.SmokeTestRunSpec{TestRef: api.TestReference{Name: name}}}
			if err = c.Create(ctx, run, strict); err != nil {
				t.Fatal(err)
			}
			run.Status.Definition = stored.Spec.DeepCopy()
			result := api.ProbeResult{Version: 1, RunUID: string(run.UID), Stage: "probe", Container: "probe", Success: true, CertificateSHA256: "test-fingerprint", MetricsPodUID: "test-pod-uid"}
			run.Status.Resources = []api.ResourceRecord{{Stage: "probe", Kind: "Pod", Name: "probe", Result: &result}}
			if err = c.Status().Update(ctx, run, strict); err != nil {
				t.Fatalf("save definition snapshot with strict field validation: %v", err)
			}
			storedRun := &api.SmokeTestRun{}
			if err = c.Get(ctx, key(run.Name), storedRun); err != nil {
				t.Fatal(err)
			}
			if !equality.Semantic.DeepEqual(want, storedRun.Status.Definition) {
				t.Fatalf("definition snapshot changed during storage (-want +got):\n%s", diff.Diff(want, storedRun.Status.Definition))
			}
			if len(storedRun.Status.Resources) != 1 || !equality.Semantic.DeepEqual(&result, storedRun.Status.Resources[0].Result) {
				t.Fatal("probe evidence was pruned from Run status")
			}
		})
	}
}

func TestAPIServerLifecycle(t *testing.T) {
	c := newIntegrationClient(t)
	ctx := context.Background()
	b, err := os.ReadFile("../../examples/cluster-autoscaler.yaml")
	if err != nil {
		t.Fatal(err)
	}
	test := &api.SmokeTest{}
	if err = yaml.Unmarshal(b, test); err != nil {
		t.Fatal(err)
	}
	// Use the real native template with a finite result assertion. No scheduler,
	// autoscaler or CSI is provided by envtest; their observations are simulated.
	test.Name = "lifecycle"
	test.Namespace = definition.Namespace
	create := test.Spec.Stages[1]
	create.DependsOn = nil
	test.Spec.Stages = []api.Stage{create, {Name: "result", DependsOn: []string{"demand"}, Assert: []api.Assertion{{Type: "ProbeResult", Resource: &api.ResourceRef{FromStage: "demand"}}}}}
	if err = c.Create(ctx, test); err != nil {
		t.Fatal(err)
	}
	run := &api.SmokeTestRun{ObjectMeta: metav1.ObjectMeta{Name: "first", Namespace: definition.Namespace}, Spec: api.SmokeTestRunSpec{TestRef: api.TestReference{Name: test.Name}}}
	if err = c.Create(ctx, run); err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{Client: c, ProbeImage: "test/image@sha256:123", AWSAccountID: "123456789012", ExpectedRoleARN: "arn:aws:iam::123456789012:role/infra-smoketest-aws", Region: "us-east-1"}
	drive := func(n int) {
		t.Helper()
		for range n {
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key(run.Name)}); err != nil {
				t.Fatal(err)
			}
		}
		if err := c.Get(ctx, key(run.Name), run); err != nil {
			t.Fatal(err)
		}
	}
	drive(8)
	if run.Status.ProbeImage != "test/image@sha256:123" || len(run.Status.Resources) != 1 || run.Status.Resources[0].UID == "" {
		t.Fatalf("missing persisted resource: %+v", run.Status)
	}
	p := &corev1.Pod{}
	if err = c.Get(ctx, key(run.Status.Resources[0].Name), p); err != nil {
		t.Fatal(err)
	}
	if p.Spec.Containers[0].Image != run.Status.ProbeImage {
		t.Fatal("probe image drift")
	}
	if run.Status.AWSAccountID != "123456789012" {
		t.Fatal("shared account was not persisted in the Run snapshot")
	}
	foundAccount := false
	for _, env := range p.Spec.Containers[0].Env {
		if env.Name == "SMOKETEST_AWS_ACCOUNT_ID" && env.Value == run.Status.AWSAccountID {
			foundAccount = true
		}
	}
	if !foundAccount {
		t.Fatal("non-identity probe did not inherit the shared account")
	}
	oldUID := p.UID
	// Emulate restart after Create returned but before the status write.
	run.Status.Resources[0].UID = ""
	run.Status.Resources[0].CreatedAt = nil
	run.Status.Stages[0].Phase = "Running"
	if err = c.Status().Update(ctx, run); err != nil {
		t.Fatal(err)
	}
	r = &Reconciler{Client: c, ProbeImage: "new/image@sha256:456", AWSAccountID: "000000000000"}
	drive(2)
	if run.Status.AWSAccountID != "123456789012" || run.Status.ExpectedRoleARN != "arn:aws:iam::123456789012:role/infra-smoketest-aws" {
		t.Fatal("controller restart changed the Run's AWS configuration")
	}
	if run.Status.Resources[0].UID != string(oldUID) || run.Status.ProbeImage != "test/image@sha256:123" {
		t.Fatal("restart duplicated a resource or changed the image snapshot")
	}
	// CRD admission must reject mutation of the execution request.
	mutated := run.DeepCopy()
	mutated.Spec.TestRef.Name = "other"
	if err = c.Update(ctx, mutated); !apierrors.IsInvalid(err) {
		t.Fatalf("immutable spec accepted: %v", err)
	}
	other := &api.SmokeTestRun{ObjectMeta: metav1.ObjectMeta{Name: "second", Namespace: definition.Namespace}, Spec: api.SmokeTestRunSpec{TestRef: api.TestReference{Name: test.Name}}}
	if err = c.Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		if _, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key(other.Name)}); err != nil {
			t.Fatal(err)
		}
	}
	if err = c.Get(ctx, key(other.Name), other); err != nil {
		t.Fatal(err)
	}
	if other.Status.StartedAt != nil {
		t.Fatal("overlapping Run activated")
	}
	result := api.ProbeResult{Version: 1, RunUID: string(run.UID), Stage: "demand", Container: "probe", Success: true}
	message, _ := json.Marshal(result)
	p.Status.Phase = corev1.PodSucceeded
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "probe", Image: p.Spec.Containers[0].Image, ImageID: "test", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: string(message)}}}}
	if err = c.Status().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	drive(4)
	// envtest has no kubelet: emulate completion of Pod deletion.
	if err = c.Delete(ctx, p, client.GracePeriodSeconds(0)); err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	drive(4)
	if run.Status.Phase != "Succeeded" || !meta.IsStatusConditionTrue(run.Status.Conditions, "CleanupComplete") {
		t.Fatalf("Run did not finish: %+v", run.Status)
	}
	artifact := &corev1.ConfigMap{}
	if err = c.Get(ctx, key(name(run, "diagnostics")), artifact); err != nil {
		t.Fatal(err)
	}
	var summary api.SmokeTestRunStatus
	if err = json.Unmarshal([]byte(artifact.Data["result.json"]), &summary); err != nil || summary.Phase != "Succeeded" {
		t.Fatalf("incorrect diagnostic result: %v %+v", err, summary)
	}
	lease := &coordv1.Lease{}
	if err = c.Get(ctx, key(LeaseName), lease); err != nil {
		t.Fatal(err)
	}
	if lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity != "" {
		t.Fatal("execution slot not released")
	}
	// The next Run can now activate, and its deadline remains stable on restart.
	for range 2 {
		if _, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key(other.Name)}); err != nil {
			t.Fatal(err)
		}
	}
	if err = c.Get(ctx, key(other.Name), other); err != nil {
		t.Fatal(err)
	}
	deadline := other.Status.Deadline.DeepCopy()
	r.Now = func() time.Time { return deadline.Add(time.Second) }
	if _, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key(other.Name)}); err != nil {
		t.Fatal(err)
	}
	if err = c.Get(ctx, key(other.Name), other); err != nil {
		t.Fatal(err)
	}
	if other.Status.Reason != "TimedOut" || !other.Status.Deadline.Equal(deadline) {
		t.Fatal("deadline reset on restart")
	}
}
