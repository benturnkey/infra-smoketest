package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	api "github.com/tkhq/infra-smoketest/api/v1alpha1"
	"github.com/tkhq/infra-smoketest/internal/definition"
	"github.com/tkhq/infra-smoketest/internal/probe"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *Reconciler) observe(ctx context.Context, run *api.SmokeTestRun) error {
	events := &corev1.EventList{}
	if err := r.List(ctx, events, client.InNamespace(run.Namespace)); err != nil {
		return err
	}
	for i := range run.Status.Resources {
		rec := &run.Status.Resources[i]
		if rec.Kind != "Pod" || rec.UID == "" || rec.Deleted {
			continue
		}
		p := &corev1.Pod{}
		if err := r.Get(ctx, key(rec.Name), p); err != nil {
			if apierrors.IsNotFound(err) {
				return fail("AssertionFailed", "probe %s disappeared", rec.Name)
			}
			return err
		}
		if string(p.UID) != rec.UID || !owned(p, run) {
			return fail("AssertionFailed", "probe identity changed")
		}
		if rec.UnschedulableAt == nil {
			for _, c := range p.Status.Conditions {
				if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == "Unschedulable" {
					rec.UnschedulableAt = ptr.To(c.LastTransitionTime)
				}
			}
		}
		for _, e := range events.Items {
			if string(e.InvolvedObject.UID) != rec.UID {
				continue
			}
			at := e.CreationTimestamp
			if rec.UnschedulableAt == nil && e.Reason == "FailedScheduling" && (e.Source.Component == "default-scheduler" || e.ReportingController == "default-scheduler") {
				rec.UnschedulableAt = &at
			}
			if rec.ScaleUpAt == nil && e.Reason == "TriggeredScaleUp" && (e.Source.Component == "cluster-autoscaler" || e.ReportingController == "cluster-autoscaler") {
				rec.ScaleUpAt = &at
			}
		}
		if p.Spec.NodeName != "" {
			node := &corev1.Node{}
			if err := r.Get(ctx, types.NamespacedName{Name: p.Spec.NodeName}, node); err != nil {
				if apierrors.IsNotFound(err) {
					return fail("AssertionFailed", "scheduled node disappeared")
				}
				return err
			}
			if rec.NodeUID != "" && rec.NodeUID != string(node.UID) {
				return fail("AssertionFailed", "scheduled node identity changed")
			}
			rec.NodeName = node.Name
			rec.NodeUID = string(node.UID)
		}
		for _, status := range p.Status.ContainerStatuses {
			if status.Name != "probe" || status.State.Terminated == nil || rec.Result != nil {
				continue
			}
			t := status.State.Terminated
			if len(t.Message) > 2048 {
				return fail("AssertionFailed", "probe result exceeds size limit")
			}
			var result api.ProbeResult
			if err := json.Unmarshal([]byte(t.Message), &result); err != nil {
				return fail("AssertionFailed", "probe did not return valid structured evidence (exit %d)", t.ExitCode)
			}
			if result.Version != 1 || result.RunUID != string(run.UID) || result.Stage != rec.Stage || result.Container != "probe" {
				return fail("AssertionFailed", "probe result identifiers do not match")
			}
			rec.Result = &result
			if t.ExitCode != 0 || !result.Success {
				return fail("AssertionFailed", "probe %s failed: %s", rec.Stage, result.Error)
			}
		}
		if p.Status.Phase == corev1.PodFailed && rec.Result == nil {
			return fail("AssertionFailed", "probe failed: %s", p.Status.Reason)
		}
	}
	return r.observeClaims(ctx, run)
}

func (r *Reconciler) observeClaims(ctx context.Context, run *api.SmokeTestRun) error {
	for i := range run.Status.Resources {
		rec := &run.Status.Resources[i]
		if rec.Kind != "PersistentVolumeClaim" || rec.UID == "" {
			continue
		}
		pvc := &corev1.PersistentVolumeClaim{}
		err := r.Get(ctx, key(rec.Name), pvc)
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		if err == nil {
			if string(pvc.UID) != rec.UID || !owned(pvc, run) {
				if run.Status.CleanupStartedAt != nil {
					continue
				}
				return fail("AssertionFailed", "PVC identity changed")
			}
			if pvc.Spec.VolumeName != "" {
				pv := &corev1.PersistentVolume{}
				if err = r.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, pv); err != nil {
					if apierrors.IsNotFound(err) {
						continue
					}
					return err
				}
				if pv.Spec.ClaimRef == nil || string(pv.Spec.ClaimRef.UID) != rec.UID {
					return fail("AssertionFailed", "PV is not bound to this Run's PVC")
				}
				if rec.PVUID != "" && rec.PVUID != string(pv.UID) {
					return fail("AssertionFailed", "PV identity changed")
				}
				rec.PVName = pv.Name
				rec.PVUID = string(pv.UID)
				if pv.Spec.CSI != nil {
					rec.VolumeHandle = pv.Spec.CSI.VolumeHandle
				}
			}
		}
		if rec.PVName == "" {
			continue
		}
		attachments := &storagev1.VolumeAttachmentList{}
		if err = r.List(ctx, attachments); err != nil {
			return err
		}
		for _, va := range attachments.Items {
			if va.Spec.Source.PersistentVolumeName == nil || *va.Spec.Source.PersistentVolumeName != rec.PVName {
				continue
			}
			rec.AttachmentName = va.Name
			rec.AttachmentUID = string(va.UID)
			if va.Spec.Attacher == "ebs.csi.aws.com" && va.Status.Attached {
				for _, podRec := range run.Status.Resources {
					if podRec.Kind == "Pod" && !podRec.Deleted && podRec.NodeName == va.Spec.NodeName {
						p := &corev1.Pod{}
						if err = r.Get(ctx, key(podRec.Name), p); err != nil {
							if apierrors.IsNotFound(err) {
								continue
							}
							return err
						}
						if p.Spec.NodeName != va.Spec.NodeName {
							continue
						}
						csi := &storagev1.CSINode{}
						if err = r.Get(ctx, types.NamespacedName{Name: va.Spec.NodeName}, csi); err != nil {
							if apierrors.IsNotFound(err) {
								continue
							}
							return err
						}
						registered := false
						for _, driver := range csi.Spec.Drivers {
							if driver.Name == "ebs.csi.aws.com" {
								registered = true
							}
						}
						if !registered {
							continue
						}
						for _, v := range p.Spec.Volumes {
							if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == rec.Name {
								rec.Attached = true
							}
						}
					}
				}
			}
		}
	}
	return nil
}

func (r *Reconciler) related(ctx context.Context, run *api.SmokeTestRun, ref *api.ResourceRef) (client.Object, error) {
	rec := record(run, ref.FromStage)
	if rec == nil {
		return nil, nil
	}
	var obj client.Object
	objectKey := key(rec.Name)
	expected := rec.UID
	switch ref.Relation {
	case "scheduledNode":
		if rec.NodeName == "" {
			return nil, nil
		}
		obj = &corev1.Node{}
		objectKey = types.NamespacedName{Name: rec.NodeName}
		expected = rec.NodeUID
	case "boundPV":
		if rec.PVName == "" {
			return nil, nil
		}
		obj = &corev1.PersistentVolume{}
		objectKey = types.NamespacedName{Name: rec.PVName}
		expected = rec.PVUID
	case "volumeAttachment":
		if rec.AttachmentName == "" {
			return nil, nil
		}
		obj = &storagev1.VolumeAttachment{}
		objectKey = types.NamespacedName{Name: rec.AttachmentName}
		expected = rec.AttachmentUID
	case "":
		if rec.Kind == "Pod" {
			obj = &corev1.Pod{}
		} else {
			obj = &corev1.PersistentVolumeClaim{}
		}
	default:
		return nil, fail("PreconditionFailed", "unknown resource relationship")
	}
	if err := r.Get(ctx, objectKey, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if string(obj.GetUID()) != expected {
		return nil, fail("AssertionFailed", "correlated resource UID changed")
	}
	return obj, nil
}

func (r *Reconciler) assert(ctx context.Context, run *api.SmokeTestRun, a api.Assertion) (bool, string, error) {
	if a.Type == "NodePoolEmpty" {
		nodes := &corev1.NodeList{}
		if err := r.List(ctx, nodes); err != nil {
			return false, "", err
		}
		for _, n := range nodes.Items {
			if n.Labels[definition.PoolLabel] == definition.PoolValue {
				return false, "Smoke pool still has a Node", nil
			}
		}
		run.Status.BaselineNodes = nil
		for _, n := range nodes.Items {
			run.Status.BaselineNodes = append(run.Status.BaselineNodes, string(n.UID))
		}
		run.Status.BaselineAt = ptr.To(metav1.NewTime(r.now()))
		return true, "Pool empty", nil
	}
	rec := record(run, a.Resource.FromStage)
	if rec == nil {
		return false, "Waiting for resource", nil
	}
	switch a.Type {
	case "InitiallyUnschedulable":
		return rec.UnschedulableAt != nil, "Waiting for unschedulable evidence", nil
	case "AutoscalerScaleUpObserved":
		return rec.ScaleUpAt != nil, "Waiting for autoscaler Event", nil
	case "ProbeResult":
		if rec.Result == nil {
			return false, "Waiting for probe result", nil
		}
		if !rec.Result.Success {
			return false, "", fail("AssertionFailed", "probe failed")
		}
		for _, stage := range run.Status.Definition.Stages {
			if stage.Name != rec.Stage || stage.CreatePod == nil {
				continue
			}
			command := stage.CreatePod.Template.Spec.Containers[0].Args[1]
			if (command == "storage-write" || command == "storage-read") && rec.Result.Checksum != probe.Checksum(string(run.UID)) {
				return false, "", fail("AssertionFailed", "missing or incorrect sentinel checksum")
			}
			if command == "identity" {
				if err := probe.ValidateIdentity(run.Status.ExpectedRoleARN, rec.Result.Account, rec.Result.ARN); err != nil {
					return false, "", fail("AssertionFailed", "%v", err)
				}
			}
		}
		return true, "Probe passed", nil
	case "VolumeAttached":
		if !rec.Attached {
			return false, "Waiting for correlated EBS attachment", nil
		}
		return true, "EBS attached", nil
	}
	obj, err := r.related(ctx, run, a.Resource)
	if err != nil {
		return false, "", err
	}
	if obj == nil {
		return false, "Waiting for correlated resource", nil
	}
	switch a.Type {
	case "ResourceStatus":
		b, _ := json.Marshal(obj)
		var value struct {
			Status api.ExpectedStatus `json:"status"`
		}
		if err = json.Unmarshal(b, &value); err != nil {
			return false, "", err
		}
		want := a.Status
		if want.Phase != "" && want.Phase != value.Status.Phase {
			return false, "Waiting for phase " + want.Phase, nil
		}
		for _, expected := range want.Conditions {
			found := false
			for _, actual := range value.Status.Conditions {
				if actual.Type == expected.Type && actual.Status == expected.Status && (expected.Reason == "" || expected.Reason == actual.Reason) {
					found = true
				}
			}
			if !found {
				return false, "Waiting for condition " + expected.Type + "=" + expected.Status, nil
			}
		}
		return true, "Status matched", nil
	case "ScheduledOnNewNode":
		node, ok := obj.(*corev1.Node)
		if !ok {
			return false, "", fail("PreconditionFailed", "ScheduledOnNewNode needs scheduledNode relationship")
		}
		if run.Status.BaselineAt == nil || rec.CreatedAt == nil {
			return false, "", fail("PreconditionFailed", "missing node baseline")
		}
		for _, uid := range run.Status.BaselineNodes {
			if uid == string(node.UID) {
				return false, "", fail("AssertionFailed", "Pod scheduled on existing capacity")
			}
		}
		if !node.CreationTimestamp.After(rec.CreatedAt.Time) || node.Labels[definition.PoolLabel] != definition.PoolValue || node.Labels["turnkey.engineering/cloud-platform"] != "aws" || !strings.HasPrefix(node.Spec.ProviderID, "aws://") {
			return false, "Waiting for a new initialized AWS smoke node", nil
		}
		for _, taint := range node.Spec.Taints {
			if taint.Key == "node.cloudprovider.kubernetes.io/uninitialized" {
				return false, "Waiting for cloud initialization", nil
			}
		}
		return true, "New node correlated", nil
	case "VolumeBound":
		pvc, ok := obj.(*corev1.PersistentVolumeClaim)
		if !ok {
			return false, "", fail("PreconditionFailed", "VolumeBound needs a PVC")
		}
		if pvc.Status.Phase != corev1.ClaimBound || rec.PVName == "" {
			return false, "Waiting for Bound PVC", nil
		}
		pv := &corev1.PersistentVolume{}
		if err = r.Get(ctx, types.NamespacedName{Name: rec.PVName}, pv); err != nil {
			return false, "", err
		}
		if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != "ebs.csi.aws.com" || !strings.HasPrefix(pv.Spec.CSI.VolumeHandle, "vol-") || pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete || pv.Spec.ClaimRef == nil || string(pv.Spec.ClaimRef.UID) != rec.UID {
			return false, "", fail("AssertionFailed", "PV is not a dynamically bound EBS Delete volume")
		}
		if pv.CreationTimestamp.Before(&pvc.CreationTimestamp) || pv.Annotations["pv.kubernetes.io/provisioned-by"] != "ebs.csi.aws.com" {
			return false, "", fail("AssertionFailed", "PV was not dynamically provisioned for this claim")
		}
		return true, "EBS PV bound", nil
	case "WebIdentity":
		p, ok := obj.(*corev1.Pod)
		if !ok {
			return false, "", fail("PreconditionFailed", "WebIdentity needs a Pod")
		}
		return webhookMutation(p, run.Status.ExpectedRoleARN, run.Status.Region)
	}
	return false, "", fmt.Errorf("unimplemented assertion %s", a.Type)
}

func webhookMutation(p *corev1.Pod, role, region string) (bool, string, error) {
	for _, c := range p.Spec.Containers {
		if c.Name != "probe" {
			continue
		}
		env := map[string]string{}
		for _, e := range c.Env {
			env[e.Name] = e.Value
		}
		if env["AWS_ROLE_ARN"] != role || role == "" || env["AWS_WEB_IDENTITY_TOKEN_FILE"] == "" || env["AWS_REGION"] != region || env["AWS_DEFAULT_REGION"] != region || env["AWS_STS_REGIONAL_ENDPOINTS"] != "regional" {
			return false, "", fail("AssertionFailed", "webhook did not inject expected AWS environment")
		}
		for _, v := range p.Spec.Volumes {
			if v.Projected == nil {
				continue
			}
			for _, s := range v.Projected.Sources {
				if s.ServiceAccountToken == nil || s.ServiceAccountToken.Audience != "sts.amazonaws.com" {
					continue
				}
				for _, m := range c.VolumeMounts {
					if m.Name == v.Name && m.ReadOnly && strings.TrimRight(m.MountPath, "/")+"/"+s.ServiceAccountToken.Path == env["AWS_WEB_IDENTITY_TOKEN_FILE"] {
						return true, "Webhook mutation verified", nil
					}
				}
			}
		}
	}
	return false, "", fail("AssertionFailed", "webhook STS token projection is absent")
}
