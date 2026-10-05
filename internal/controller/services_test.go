package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	api "github.com/tkhq/infra-smoketest/api/v1alpha1"
	"github.com/tkhq/infra-smoketest/internal/definition"
	"github.com/tkhq/infra-smoketest/internal/probe"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestServiceProbePodPermissionsAndEvidence(t *testing.T) {
	for _, command := range []string{"cert-manager", "kube-state-metrics"} {
		t.Run(command, func(t *testing.T) {
			stage := testPodStage(command)
			stage.CreatePod.Template.Spec.NodeSelector = nil
			sa := definition.ProbeSA
			if command == "cert-manager" {
				sa = definition.CertManagerSA
				stage.CreatePod.Template.Spec.Containers[0].Env = []corev1.EnvVar{
					{Name: definition.CertManagerIssuerNameEnv, Value: "cluster-ca"},
					{Name: definition.CertManagerIssuerKindEnv, Value: "ClusterIssuer"},
				}
			} else {
				stage.CreatePod.Template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "KUBE_STATE_METRICS_URL", Value: "http://metrics.example/metrics"}}
			}
			stage.CreatePod.Template.Spec.ServiceAccountName = sa
			run := testRun()
			run.Status.Definition = &api.SmokeTestSpec{Stages: []api.Stage{stage}}
			r := fakeReconciler(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: sa, Namespace: run.Namespace}})
			pod, err := r.pod(context.Background(), run, stage, "probe-pod")
			if err != nil {
				t.Fatal(err)
			}
			if *pod.Spec.AutomountServiceAccountToken {
				t.Fatal("enabled general token automount")
			}
			if command == "cert-manager" {
				if len(pod.Spec.Volumes) != 1 || pod.Spec.Volumes[0].Projected == nil || *pod.Spec.Volumes[0].Projected.Sources[0].ServiceAccountToken.ExpirationSeconds != 600 {
					t.Fatal("cert-manager probe lacks bounded API token")
				}
			} else if len(pod.Spec.Volumes) != 0 {
				t.Fatal("metrics probe received credentials")
			}
			env := map[string]corev1.EnvVar{}
			for _, v := range pod.Spec.Containers[0].Env {
				env[v.Name] = v
			}
			if env["SMOKETEST_POD_UID"].ValueFrom == nil || env["SMOKETEST_POD_UID"].ValueFrom.FieldRef.FieldPath != "metadata.uid" {
				t.Fatal("Pod UID must come from the downward API")
			}
			if command == "kube-state-metrics" && env["KUBE_STATE_METRICS_URL"].Value != "http://metrics.example/metrics" {
				t.Fatal("lost custom metrics URL")
			}
			if command == "cert-manager" && (env[definition.CertManagerIssuerNameEnv].Value != "cluster-ca" || env[definition.CertManagerIssuerKindEnv].Value != "ClusterIssuer") {
				t.Fatal("lost existing issuer configuration")
			}
			result := &api.ProbeResult{Success: true}
			run.Status.Resources = []api.ResourceRecord{{Stage: stage.Name, Kind: "Pod", UID: "pod-uid", Result: result}}
			assertion := api.Assertion{Type: "ProbeResult", Resource: &api.ResourceRef{FromStage: stage.Name}}
			if _, _, err := r.assert(context.Background(), run, assertion); err == nil {
				t.Fatal("accepted a success flag without evidence")
			}
			result.CertificateSHA256 = strings.Repeat("ab", 32)
			result.MetricsPodUID = "stale-pod-uid"
			if command == "kube-state-metrics" {
				if _, _, err := r.assert(context.Background(), run, assertion); err == nil {
					t.Fatal("accepted another Pod's metrics")
				}
			}
			result.MetricsPodUID = "pod-uid"
			if done, _, err := r.assert(context.Background(), run, assertion); err != nil || !done {
				t.Fatalf("rejected valid evidence: %v", err)
			}
		})
	}
}

func TestExistingIssuerCleanupPreservesIssuer(t *testing.T) {
	for _, kind := range []string{"Issuer", "ClusterIssuer"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			run := testRun()
			stage := testPodStage("cert-manager")
			stage.CreatePod.Template.Spec.Containers[0].Env = []corev1.EnvVar{
				{Name: definition.CertManagerIssuerNameEnv, Value: "probe"},
				{Name: definition.CertManagerIssuerKindEnv, Value: kind},
			}
			run.Status.Definition = &api.SmokeTestSpec{Stages: []api.Stage{stage}}
			run.Status.Resources = []api.ResourceRecord{{Stage: "work", Kind: "Pod", Name: "probe", UID: "pod-uid"}}
			issuer := probe.CertManagerObject(kind, run.Namespace, "probe")
			if kind == "ClusterIssuer" {
				issuer.SetNamespace("")
			}
			issuer.SetUID("existing-issuer-uid")
			// Even when the selected issuer has the fixture name and owner, an
			// explicit selection must never put it in the controller's delete set.
			owners := []metav1.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: "probe", UID: "pod-uid"}}
			issuer.SetOwnerReferences(owners)
			request := probe.CertManagerObject("CertificateRequest", run.Namespace, "probe")
			request.SetUID("request-uid")
			request.SetOwnerReferences(owners)
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: run.Namespace, UID: "key-uid", OwnerReferences: owners}}
			r := fakeReconciler(issuer, request, secret)
			for range 2 {
				if remaining, err := r.cleanupCertificates(ctx, run); err != nil || !remaining {
					t.Fatalf("failed to clean fixture: %v", err)
				}
			}
			if remaining, err := r.cleanupCertificates(ctx, run); err != nil || remaining {
				t.Fatalf("cleanup did not complete: %v", err)
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(issuer), issuer); err != nil || issuer.GetUID() != "existing-issuer-uid" {
				t.Fatalf("existing issuer was deleted or replaced: %v", err)
			}
		})
	}
}

func TestCertificateCleanupAfterInterruptedProbe(t *testing.T) {
	for _, reason := range []string{"AssertionsPassed", "Cancelled", "TimedOut"} {
		t.Run(reason, func(t *testing.T) {
			ctx := context.Background()
			run := testRun()
			run.Status.Definition = &api.SmokeTestSpec{Stages: []api.Stage{testPodStage("cert-manager")}}
			run.Status.CleanupStartedAt = ptr.To(metav1.Now())
			run.Status.Phase, run.Status.Reason = "CleaningUp", reason
			// Simulate a restart with a saved Pod intent but no persisted UID.
			run.Status.Resources = []api.ResourceRecord{{Stage: "work", Kind: "Pod", Name: "probe"}}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: run.Namespace, UID: "probe-uid", OwnerReferences: owner(run)}}
			owners := []metav1.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID}}
			request := probe.CertManagerObject("CertificateRequest", run.Namespace, pod.Name)
			request.SetOwnerReferences(owners)
			request.SetUID("request-uid")
			request.SetFinalizers([]string{"test.example/hold"})
			issuer := probe.CertManagerObject("Issuer", run.Namespace, pod.Name)
			issuer.SetOwnerReferences(owners)
			issuer.SetUID("issuer-uid")
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: run.Namespace, UID: "key-uid", OwnerReferences: owners}}
			r := fakeReconciler(pod, request, issuer, secret)
			for range 4 {
				if err := r.cleanup(ctx, run, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			if run.Status.Resources[0].UID != string(pod.UID) || meta.IsStatusConditionTrue(run.Status.Conditions, "CleanupComplete") {
				t.Fatal("lost Pod identity or completed while a fixture remains")
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(secret), &corev1.Secret{}); err != nil {
				t.Fatal("deleted signing key before request finished deleting")
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(request), request); err != nil {
				t.Fatal(err)
			}
			request.SetFinalizers(nil)
			if err := r.Update(ctx, request); err != nil {
				t.Fatal(err)
			}
			for range 4 {
				if err := r.cleanup(ctx, run, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			if !meta.IsStatusConditionTrue(run.Status.Conditions, "CleanupComplete") || meta.IsStatusConditionTrue(run.Status.Conditions, "Succeeded") != (reason == "AssertionsPassed") {
				t.Fatalf("incorrect cleanup result: %+v", run.Status)
			}
			for _, obj := range []client.Object{issuer, secret} {
				if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); !apierrors.IsNotFound(err) {
					t.Fatalf("fixture survived cleanup: %v", err)
				}
			}
		})
	}
}

func TestCertificateCleanupDoesNotDeleteForeignFixtures(t *testing.T) {
	run := testRun()
	run.Status.Definition = &api.SmokeTestSpec{Stages: []api.Stage{testPodStage("cert-manager")}}
	run.Status.Resources = []api.ResourceRecord{{Stage: "work", Kind: "Pod", Name: "probe", UID: "expected-uid"}}
	issuer := probe.CertManagerObject("Issuer", run.Namespace, "probe")
	issuer.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: "probe", UID: "another-uid"}})
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: run.Namespace}}
	r := fakeReconciler(issuer, secret)
	if remaining, err := r.cleanupCertificates(context.Background(), run); remaining || err != nil {
		t.Fatalf("foreign fixture blocked cleanup: %v", err)
	}
	for _, obj := range []client.Object{issuer, secret} {
		if err := r.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
			t.Fatalf("deleted foreign fixture: %v", err)
		}
	}
}
