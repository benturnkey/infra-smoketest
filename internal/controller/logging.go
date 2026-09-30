package controller

import (
	"context"

	api "github.com/tkhq/infra-smoketest/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
)

// Log progress only after status is saved. Comparing persisted evidence keeps
// unchanged polling quiet without an in-memory cache that resets on restart.
func logStatusChanges(ctx context.Context, before, after *api.SmokeTestRunStatus) {
	log := ctrl.LoggerFrom(ctx)
	if before.Definition == nil && after.Definition != nil {
		log.Info("Accepted Run", "definitionUID", after.DefinitionUID, "definitionGeneration", after.DefinitionGeneration, "definitionHash", after.DefinitionHash, "probeImage", after.ProbeImage)
	}
	if before.Phase != after.Phase || before.Reason != after.Reason || before.Message != after.Message {
		log.Info("Run progress", "previousPhase", before.Phase, "phase", after.Phase, "reason", after.Reason, "detail", after.Message, "deadline", after.Deadline)
	}
	for _, condition := range after.Conditions {
		previous := meta.FindStatusCondition(before.Conditions, condition.Type)
		if previous == nil || previous.Status != condition.Status || previous.Reason != condition.Reason || previous.Message != condition.Message {
			log.Info("Run condition changed", "condition", condition.Type, "status", condition.Status, "reason", condition.Reason, "detail", condition.Message)
		}
	}
	previousStages := make(map[string]api.StageStatus, len(before.Stages))
	for _, stage := range before.Stages {
		previousStages[stage.Name] = stage
	}
	for _, stage := range after.Stages {
		previous, exists := previousStages[stage.Name]
		if !exists {
			log.Info("Stage started", "stage", stage.Name, "phase", stage.Phase, "deadline", stage.Deadline)
		} else if previous.Phase != stage.Phase || previous.Message != stage.Message {
			log.Info("Stage progress", "stage", stage.Name, "phase", stage.Phase, "detail", stage.Message, "deadline", stage.Deadline)
		}
	}
	previousResources := make(map[string]api.ResourceRecord, len(before.Resources))
	for _, resource := range before.Resources {
		previousResources[resource.Stage] = resource
	}
	for _, resource := range after.Resources {
		previous := previousResources[resource.Stage]
		resourceLog := log.WithValues("stage", resource.Stage, "resourceKind", resource.Kind, "resourceName", resource.Name, "resourceUID", resource.UID)
		if previous.UnschedulableAt == nil && resource.UnschedulableAt != nil {
			resourceLog.Info("Observed unschedulable Pod", "observedAt", resource.UnschedulableAt)
		}
		if previous.ScaleUpAt == nil && resource.ScaleUpAt != nil {
			resourceLog.Info("Observed autoscaler scale-up Event", "observedAt", resource.ScaleUpAt)
		}
		if resource.NodeUID != "" && previous.NodeUID != resource.NodeUID {
			resourceLog.Info("Observed Pod scheduled", "node", resource.NodeName, "nodeUID", resource.NodeUID)
		}
		if resource.PVUID != "" && previous.PVUID != resource.PVUID {
			resourceLog.Info("Observed bound PersistentVolume", "persistentVolume", resource.PVName, "persistentVolumeUID", resource.PVUID)
		}
		if !previous.Attached && resource.Attached {
			resourceLog.Info("Observed EBS volume attachment", "volumeAttachment", resource.AttachmentName)
		}
		if previous.Result == nil && resource.Result != nil {
			resourceLog.Info("Recorded probe result", "success", resource.Result.Success)
		}
	}
}
