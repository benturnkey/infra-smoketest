package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	api "github.com/tkhq/infra-smoketest/api/v1alpha1"
	"github.com/tkhq/infra-smoketest/internal/definition"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

func logContext(output *bytes.Buffer) context.Context {
	return ctrl.LoggerInto(context.Background(), zap.New(zap.WriteTo(output)))
}

func logEntries(t *testing.T, output *bytes.Buffer) []map[string]any {
	t.Helper()
	var entries []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(output.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("invalid JSON log: %v: %s", err, line)
		}
		entries = append(entries, entry)
	}
	return entries
}

func TestPendingRunLogsTimeoutAndCleanup(t *testing.T) {
	var output bytes.Buffer
	ctx := logContext(&output)
	now := time.Date(2026, 9, 30, 19, 0, 0, 0, time.UTC)
	run := testRun()
	run.Finalizers = []string{Finalizer}
	run.Spec.TestRef.Name = "cluster-autoscaler"
	run.Status.Phase = "Running"
	run.Status.StartedAt = ptr.To(metav1.NewTime(now))
	run.Status.Deadline = ptr.To(metav1.NewTime(now.Add(25 * time.Minute)))
	create := testPodStage("ready")
	run.Status.Definition = &api.SmokeTestSpec{Stages: []api.Stage{
		create,
		{Name: "scale-up", DependsOn: []string{create.Name}, Timeout: "15m", Assert: []api.Assertion{
			{Type: "InitiallyUnschedulable", Resource: &api.ResourceRef{FromStage: create.Name}},
			{Type: "AutoscalerScaleUpObserved", Resource: &api.ResourceRef{FromStage: create.Name}},
		}},
	}}
	run.Status.Stages = []api.StageStatus{{Name: create.Name, Phase: "Succeeded"}}
	run.Status.Resources = []api.ResourceRecord{{Stage: create.Name, Kind: "Pod", Name: "pending-probe", UID: "pod-uid"}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pending-probe", Namespace: run.Namespace, UID: "pod-uid", OwnerReferences: owner(run)},
		Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{
			{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", LastTransitionTime: metav1.NewTime(now)},
		}},
	}
	r := fakeReconciler(run, pod)
	r.Now = func() time.Time { return now }
	drive := func() {
		t.Helper()
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key(run.Name)}); err != nil {
			t.Fatal(err)
		}
	}
	drive() // Observe the pending Pod and start the assertion stage.
	drive() // Record the missing autoscaler Event.
	size := output.Len()
	for range 3 {
		drive()
	}
	if output.Len() != size {
		t.Fatal("unchanged polling emitted more info logs")
	}
	now = now.Add(16 * time.Minute)
	drive() // Time out the assertion stage.
	drive() // Request Pod deletion.
	drive() // Complete cleanup and release the execution slot.
	if err := r.Get(ctx, key(run.Name), run); err != nil {
		t.Fatal(err)
	}
	if run.Status.Phase != "Failed" || run.Status.Reason != "TimedOut" || !meta.IsStatusConditionTrue(run.Status.Conditions, "CleanupComplete") {
		t.Fatalf("unexpected outcome: %+v", run.Status)
	}
	counts := map[string]int{}
	waiting, timedOut, cleaned := false, false, false
	for _, entry := range logEntries(t, &output) {
		if entry["run"] != key(run.Name).String() || entry["runUID"] != string(run.UID) || entry["test"] != run.Spec.TestRef.Name {
			t.Fatalf("log lacks Run correlation: %+v", entry)
		}
		counts[entry["msg"].(string)]++
		if entry["msg"] == "Stage progress" && entry["stage"] == "scale-up" && entry["detail"] == "AutoscalerScaleUpObserved: Waiting for autoscaler Event" && entry["deadline"] != nil {
			waiting = true
		}
		if entry["msg"] == "Run progress" && entry["phase"] == "CleaningUp" && entry["reason"] == "TimedOut" {
			timedOut = true
		}
		if entry["msg"] == "Run condition changed" && entry["condition"] == "CleanupComplete" && entry["status"] == "True" {
			cleaned = true
		}
	}
	if !waiting || !timedOut || !cleaned {
		t.Fatalf("missing wait, timeout, or cleanup logs: %s", output.String())
	}
	for _, message := range []string{"Stage started", "Observed unschedulable Pod", "Requested resource deletion", "Created diagnostic ConfigMap", "Released execution slot"} {
		if counts[message] != 1 {
			t.Fatalf("expected one %q log, got %d", message, counts[message])
		}
	}
}

func TestStatusLogsRequireSuccessfulSave(t *testing.T) {
	var output bytes.Buffer
	ctx := logContext(&output)
	run := testRun()
	r := fakeReconciler() // The Run does not exist, so saving must fail.
	before := run.Status.DeepCopy()
	run.Status.Phase = "Running"
	if err := r.save(ctx, run, before); err == nil {
		t.Fatal("expected missing Run to reject status update")
	}
	if output.Len() != 0 {
		t.Fatalf("logged unpersisted progress: %s", output.String())
	}
}

func TestResourceCreationLoggedOnce(t *testing.T) {
	var output bytes.Buffer
	ctx := logContext(&output)
	run := testRun()
	stage := testPodStage("ready")
	run.Status.Definition = &api.SmokeTestSpec{Stages: []api.Stage{stage}}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: definition.ProbeSA, Namespace: definition.Namespace}}
	r := fakeReconciler(sa)
	for range 3 {
		if _, _, err := r.step(ctx, run, stage); err != nil {
			t.Fatal(err)
		}
	}
	entries := logEntries(t, &output)
	if len(entries) != 1 || entries[0]["msg"] != "Created resource" || entries[0]["resourceKind"] != "Pod" || entries[0]["stage"] != stage.Name || entries[0]["resource"] != key(name(run, stage.Name)).String() {
		t.Fatalf("expected one successful Pod creation log: %s", output.String())
	}
}
