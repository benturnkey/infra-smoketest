package controller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	api "github.com/tkhq/infra-smoketest/api/v1alpha1"
	"github.com/tkhq/infra-smoketest/internal/definition"
	"github.com/tkhq/infra-smoketest/internal/probe"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func fakeReconciler(objects ...client.Object) *Reconciler {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = api.AddToScheme(s)
	return &Reconciler{Client: fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&api.SmokeTestRun{}, &corev1.Pod{}, &corev1.PersistentVolumeClaim{}).WithObjects(objects...).Build(), ProbeImage: "image@sha256:abc", Region: "us-east-1"}
}
func testRun() *api.SmokeTestRun {
	return &api.SmokeTestRun{ObjectMeta: metav1.ObjectMeta{Name: "run", Namespace: definition.Namespace, UID: "run-uid"}, Status: api.SmokeTestRunStatus{ProbeImage: "image@sha256:abc", Deadline: ptr.To(metav1.NewTime(time.Now().Add(time.Minute)))}}
}
func testPodStage(command string) api.Stage {
	return api.Stage{Name: "work", CreatePod: &api.PodAction{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{ServiceAccountName: definition.ProbeSA, RestartPolicy: corev1.RestartPolicyNever, NodeSelector: map[string]string{definition.PoolLabel: definition.PoolValue}, Containers: []corev1.Container{{Name: "probe", Command: []string{"/bin/infra-smoketest"}, Args: []string{"probe", command}}}}}}}
}

func TestCreationDoesNotAdoptCollidingPod(t *testing.T) {
	run := testRun()
	stage := testPodStage("ready")
	run.Status.Definition = &api.SmokeTestSpec{Stages: []api.Stage{stage}}
	run.Status.Resources = []api.ResourceRecord{{Stage: stage.Name, Kind: "Pod", Name: name(run, stage.Name)}}
	foreign := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name(run, stage.Name), Namespace: run.Namespace, UID: "foreign"}}
	r := fakeReconciler(foreign, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: definition.ProbeSA, Namespace: definition.Namespace}})
	if _, _, err := r.step(context.Background(), run, stage); err == nil {
		t.Fatal("adopted a foreign Pod")
	}
	got := &corev1.Pod{}
	if err := r.Get(context.Background(), key(foreign.Name), got); err != nil || got.UID != foreign.UID {
		t.Fatal("modified a foreign Pod")
	}
}

func TestCleanupRecoversUnrecordedClaimAndWaitsForPV(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	run := testRun()
	run.Status.Phase = "CleaningUp"
	run.Status.Reason = "AssertionsPassed"
	run.Status.CleanupStartedAt = ptr.To(metav1.NewTime(now.Add(-6 * time.Minute)))
	run.Status.Resources = []api.ResourceRecord{{Stage: "claim", Kind: "PersistentVolumeClaim", Name: "claim"}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "claim", Namespace: run.Namespace, UID: "claim-uid", OwnerReferences: owner(run)}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "pv"}}
	pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pv", UID: "pv-uid"}, Spec: corev1.PersistentVolumeSpec{ClaimRef: &corev1.ObjectReference{UID: pvc.UID}, PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: "ebs.csi.aws.com", VolumeHandle: "vol-123"}}}}
	r := fakeReconciler(pvc, pv)
	if err := r.cleanup(ctx, run, now); err != nil {
		t.Fatal(err)
	}
	if run.Status.Resources[0].PVUID != "pv-uid" {
		t.Fatal("lost claim-to-PV correlation after restart")
	}
	if err := r.Get(ctx, key(pvc.Name), &corev1.PersistentVolumeClaim{}); err != nil {
		t.Fatal("deleted claim before persisting PV evidence")
	}
	if err := r.cleanup(ctx, run, now); err != nil {
		t.Fatal(err)
	}
	if run.Status.Phase != "Failed" || run.Status.Reason != "CleanupFailed" || meta.IsStatusConditionTrue(run.Status.Conditions, "CleanupComplete") {
		t.Fatal("passed cleanup while PV remains")
	}
	if err := r.Delete(ctx, pv); err != nil {
		t.Fatal(err)
	}
	if err := r.cleanup(ctx, run, now); err != nil {
		t.Fatal(err)
	}
	if !meta.IsStatusConditionTrue(run.Status.Conditions, "CleanupComplete") || meta.IsStatusConditionTrue(run.Status.Conditions, "Succeeded") {
		t.Fatal("cleanup recovery erased the original failure")
	}
}

func TestAutoscalerCannotPassOnExistingCapacityOrMissingEvents(t *testing.T) {
	ctx := context.Background()
	run := testRun()
	now := metav1.Now()
	run.Status.BaselineAt = &now
	run.Status.BaselineNodes = []string{"existing"}
	run.Status.Resources = []api.ResourceRecord{{Stage: "demand", Kind: "Pod", UID: "pod", CreatedAt: &now, NodeName: "node", NodeUID: "existing"}}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", UID: "existing", Labels: map[string]string{definition.PoolLabel: definition.PoolValue}}}
	r := fakeReconciler(node)
	for _, kind := range []string{"InitiallyUnschedulable", "AutoscalerScaleUpObserved"} {
		done, _, err := r.assert(ctx, run, api.Assertion{Type: kind, Resource: &api.ResourceRef{FromStage: "demand"}})
		if err != nil || done {
			t.Fatalf("%s accepted absent historical evidence", kind)
		}
	}
	if _, _, err := r.assert(ctx, run, api.Assertion{Type: "ScheduledOnNewNode", Resource: &api.ResourceRef{FromStage: "demand", Relation: "scheduledNode"}}); err == nil {
		t.Fatal("accepted existing capacity")
	}
	if done, _, err := r.assert(ctx, run, api.Assertion{Type: "NodePoolEmpty"}); err != nil || done {
		t.Fatal("accepted leftover smoke node")
	}
}

func TestProbeEvidenceRejectsStaleAndIncompleteResults(t *testing.T) {
	ctx := context.Background()
	run := testRun()
	stage := testPodStage("storage-read")
	run.Status.Definition = &api.SmokeTestSpec{Stages: []api.Stage{stage}}
	run.Status.Resources = []api.ResourceRecord{{Stage: "work", Kind: "Pod", Name: "probe", UID: "pod-uid", Result: &api.ProbeResult{Success: true}}}
	r := fakeReconciler()
	if _, _, err := r.assert(ctx, run, api.Assertion{Type: "ProbeResult", Resource: &api.ResourceRef{FromStage: "work"}}); err == nil {
		t.Fatal("accepted successful storage result without checksum")
	}
	run.Status.Resources[0].Result.Checksum = probe.Checksum(string(run.UID))
	if done, _, err := r.assert(ctx, run, api.Assertion{Type: "ProbeResult", Resource: &api.ResourceRef{FromStage: "work"}}); err != nil || !done {
		t.Fatalf("valid checksum: %v", err)
	}
	run.Status.Resources[0].Result = nil
	stale, _ := json.Marshal(api.ProbeResult{Version: 1, RunUID: "another-run", Stage: "work", Container: "probe", Success: true})
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: run.Namespace, UID: "pod-uid", OwnerReferences: owner(run)}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "probe", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: string(stale)}}}}}}
	r = fakeReconciler(pod)
	if err := r.observe(ctx, run); err == nil {
		t.Fatal("accepted another Run's result")
	}
}

func TestWebhookRequiresInjectedProjection(t *testing.T) {
	role := "arn:aws:iam::123456789012:role/infra-smoketest-aws"
	p := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "probe", Env: []corev1.EnvVar{{Name: "AWS_ROLE_ARN", Value: role}, {Name: "AWS_REGION", Value: "us-east-1"}, {Name: "AWS_DEFAULT_REGION", Value: "us-east-1"}, {Name: "AWS_STS_REGIONAL_ENDPOINTS", Value: "regional"}, {Name: "AWS_WEB_IDENTITY_TOKEN_FILE", Value: "/identity/token"}}}}}}
	if _, _, err := webhookMutation(p, role, "us-east-1"); err == nil {
		t.Fatal("environment variables alone proved webhook mutation")
	}
	p.Spec.Volumes = []corev1.Volume{{Name: "identity", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Audience: "sts.amazonaws.com", Path: "token"}}}}}}}
	p.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "identity", MountPath: "/identity", ReadOnly: true}}
	if done, _, err := webhookMutation(p, role, "us-east-1"); err != nil || !done {
		t.Fatalf("valid mutation: %v", err)
	}
}
