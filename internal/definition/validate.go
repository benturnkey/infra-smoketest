// Package definition validates the supported subset of native Kubernetes templates.
package definition

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	api "github.com/tkhq/infra-smoketest/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
)

const Namespace = "infra-smoketest"
const PoolLabel = "turnkey.engineering/designated-for"
const PoolValue = "smoke-tests"
const ProbeSA = "infra-smoketest-probe"
const IdentitySA = "infra-smoketest-aws"

func Timeout(s string) (time.Duration, error) {
	if s == "" {
		return 5 * time.Minute, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 || d > 25*time.Minute {
		return 0, fmt.Errorf("timeout must be between 0 and 25m")
	}
	return d, nil
}

func allowedFields(value any, names ...string) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var fields map[string]any
	if err = json.Unmarshal(b, &fields); err != nil {
		return err
	}
	for key := range fields {
		if !slices.Contains(names, key) {
			return fmt.Errorf("unsupported field %s", key)
		}
	}
	return nil
}

func Validate(spec api.SmokeTestSpec, image string) error {
	if len(spec.Stages) == 0 || len(spec.Stages) > 16 {
		return fmt.Errorf("require 1 to 16 stages")
	}
	stages := map[string]api.Stage{}
	claimNames := map[string]bool{}
	for _, s := range spec.Stages {
		if len(validation.IsDNS1123Label(s.Name)) > 0 {
			return fmt.Errorf("invalid stage name %q", s.Name)
		}
		if _, ok := stages[s.Name]; ok {
			return fmt.Errorf("duplicate stage %s", s.Name)
		}
		stages[s.Name] = s
		if _, err := Timeout(s.Timeout); err != nil {
			return fmt.Errorf("%s: %w", s.Name, err)
		}
		n := 0
		if s.CreatePod != nil {
			n++
		}
		if s.CreatePVC != nil {
			n++
		}
		if s.DeleteResource != nil {
			n++
		}
		if len(s.Assert) > 0 {
			n++
		}
		if n != 1 {
			return fmt.Errorf("%s: exactly one action or assertion set is required", s.Name)
		}
		if len(s.Assert) > 16 {
			return fmt.Errorf("%s: at most 16 assertions", s.Name)
		}
		if s.CreatePod != nil {
			if err := validatePod(s.CreatePod.Template, image); err != nil {
				return fmt.Errorf("%s: %w", s.Name, err)
			}
		}
		if s.CreatePVC != nil {
			t := s.CreatePVC.Template
			if len(validation.IsDNS1123Subdomain(t.Name)) != 0 || claimNames[t.Name] {
				return fmt.Errorf("PVC templates require unique valid logical names")
			}
			claimNames[t.Name] = true
			if err := allowedFields(t.ObjectMeta, "name", "creationTimestamp", "labels"); err != nil {
				return err
			}
			if err := allowedFields(t.Spec, "storageClassName", "accessModes", "resources", "volumeMode"); err != nil {
				return err
			}
			if t.Spec.StorageClassName == nil || *t.Spec.StorageClassName != "ebs-gp3" || len(t.Spec.AccessModes) != 1 || t.Spec.AccessModes[0] != corev1.ReadWriteOnce || t.Spec.Resources.Requests.Storage().Cmp(resource.MustParse("1Gi")) != 0 || (t.Spec.VolumeMode != nil && *t.Spec.VolumeMode != corev1.PersistentVolumeFilesystem) {
				return fmt.Errorf("PVC must request a fresh 1Gi RWO filesystem on ebs-gp3")
			}
		}
	}
	var ancestors func(string, map[string]bool) (map[string]bool, error)
	ancestors = func(name string, path map[string]bool) (map[string]bool, error) {
		if path[name] {
			return nil, fmt.Errorf("dependency cycle at %s", name)
		}
		s, ok := stages[name]
		if !ok {
			return nil, fmt.Errorf("unknown dependency %s", name)
		}
		path[name] = true
		defer delete(path, name)
		out := map[string]bool{}
		for _, dep := range s.DependsOn {
			a, err := ancestors(dep, path)
			if err != nil {
				return nil, err
			}
			out[dep] = true
			for k := range a {
				out[k] = true
			}
		}
		return out, nil
	}
	for _, s := range spec.Stages {
		a, err := ancestors(s.Name, map[string]bool{})
		if err != nil {
			return err
		}
		refs := []*api.ResourceRef{s.DeleteResource}
		for _, assert := range s.Assert {
			if !slices.Contains([]string{"ResourceStatus", "NodePoolEmpty", "InitiallyUnschedulable", "AutoscalerScaleUpObserved", "ScheduledOnNewNode", "VolumeBound", "VolumeAttached", "WebIdentity", "ProbeResult"}, assert.Type) {
				return fmt.Errorf("unknown assertion %s", assert.Type)
			}
			if assert.Type == "NodePoolEmpty" {
				if len(assert.NodeSelector) != 1 || assert.NodeSelector[PoolLabel] != PoolValue {
					return fmt.Errorf("empty-pool assertion must select the smoke pool")
				}
			} else if assert.Resource == nil {
				return fmt.Errorf("%s requires a resource", assert.Type)
			}
			if assert.Type == "ResourceStatus" && (assert.Status == nil || (assert.Status.Phase == "" && len(assert.Status.Conditions) == 0)) {
				return fmt.Errorf("ResourceStatus needs expected status fields")
			}
			refs = append(refs, assert.Resource)
		}
		for _, ref := range refs {
			if ref != nil {
				origin := stages[ref.FromStage]
				if !a[ref.FromStage] || (origin.CreatePod == nil && origin.CreatePVC == nil) {
					return fmt.Errorf("%s: resource must come from a creating ancestor", s.Name)
				}
			}
		}
		if s.DeleteResource != nil && s.DeleteResource.Relation != "" {
			return fmt.Errorf("only owned Pods and PVCs can be deleted")
		}
		if s.CreatePod != nil {
			for _, v := range s.CreatePod.Template.Spec.Volumes {
				found := false
				for name := range a {
					p := stages[name].CreatePVC
					if p != nil && p.Template.Name == v.PersistentVolumeClaim.ClaimName {
						if found {
							return fmt.Errorf("ambiguous PVC name")
						}
						found = true
					}
				}
				if !found {
					return fmt.Errorf("PVC volume must reference a creating ancestor")
				}
			}
		}
	}
	b, _ := json.Marshal(spec)
	if len(b) > 64*1024 {
		return fmt.Errorf("definition exceeds 64 KiB")
	}
	return nil
}

func validatePod(t corev1.PodTemplateSpec, image string) error {
	if err := allowedFields(t.ObjectMeta, "name", "creationTimestamp", "labels"); err != nil {
		return err
	}
	s := t.Spec
	if err := allowedFields(s, "containers", "volumes", "restartPolicy", "serviceAccountName", "automountServiceAccountToken", "nodeSelector", "affinity", "tolerations", "securityContext", "imagePullSecrets"); err != nil {
		return err
	}
	if s.RestartPolicy != corev1.RestartPolicyNever || len(s.Containers) != 1 {
		return fmt.Errorf("require one probe container and restartPolicy Never")
	}
	if s.ServiceAccountName != ProbeSA && s.ServiceAccountName != IdentitySA {
		return fmt.Errorf("unsupported serviceAccountName")
	}
	if s.AutomountServiceAccountToken != nil && *s.AutomountServiceAccountToken {
		return fmt.Errorf("API token automount is disabled")
	}
	if s.SecurityContext != nil {
		if err := allowedFields(s.SecurityContext, "runAsUser", "runAsGroup", "runAsNonRoot", "fsGroup", "seccompProfile"); err != nil {
			return err
		}
	}
	c := s.Containers[0]
	if err := allowedFields(c, "name", "image", "command", "args", "resources", "ports", "readinessProbe", "volumeMounts", "securityContext", "imagePullPolicy"); err != nil {
		return err
	}
	if c.Name != "probe" || (c.Image != "" && c.Image != image) || !slices.Equal(c.Command, []string{"/bin/infra-smoketest"}) || len(c.Args) != 2 || c.Args[0] != "probe" || !slices.Contains([]string{"ready", "storage-write", "storage-read", "identity"}, c.Args[1]) {
		return fmt.Errorf("require the approved image and probe command")
	}
	if c.SecurityContext != nil {
		if err := allowedFields(c.SecurityContext, "runAsUser", "runAsGroup", "runAsNonRoot", "readOnlyRootFilesystem", "allowPrivilegeEscalation", "capabilities", "seccompProfile"); err != nil {
			return err
		}
	}
	if c.Args[1] == "identity" {
		if s.ServiceAccountName != IdentitySA || t.Labels["pod-identity-webhook"] != "required" {
			return fmt.Errorf("identity probe requires the AWS ServiceAccount and required webhook label")
		}
	} else if s.ServiceAccountName != ProbeSA {
		return fmt.Errorf("only identity probe can use the AWS ServiceAccount")
	}
	if _, ok := t.Labels["eks.amazonaws.com/skip-pod-identity-webhook"]; ok {
		return fmt.Errorf("webhook skip label is forbidden")
	}
	if c.ReadinessProbe != nil && (c.ReadinessProbe.HTTPGet == nil || c.ReadinessProbe.Exec != nil || c.ReadinessProbe.TCPSocket != nil || c.ReadinessProbe.GRPC != nil || c.ReadinessProbe.HTTPGet.Host != "" || c.ReadinessProbe.HTTPGet.Path != "/readyz" || c.ReadinessProbe.HTTPGet.Port.IntVal != 8080) {
		return fmt.Errorf("only the local /readyz HTTP readinessProbe is supported")
	}
	for _, v := range s.Volumes {
		if v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim.ClaimName == "" {
			return fmt.Errorf("only Run-owned PVC volumes are supported")
		}
		if err := allowedFields(v.VolumeSource, "persistentVolumeClaim"); err != nil {
			return err
		}
	}
	if len(s.Volumes) > 1 || len(c.VolumeMounts) > 1 {
		return fmt.Errorf("at most one PVC mount")
	}
	for _, m := range c.VolumeMounts {
		if len(s.Volumes) != 1 || m.Name != s.Volumes[0].Name || m.MountPath != "/data" || m.SubPath != "" || m.SubPathExpr != "" || m.MountPropagation != nil {
			return fmt.Errorf("only a /data PVC mount is supported")
		}
	}
	if c.Args[1] == "storage-write" || c.Args[1] == "storage-read" {
		if len(c.VolumeMounts) != 1 {
			return fmt.Errorf("storage probe requires a /data PVC mount")
		}
	}
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		max := resource.MustParse("500m")
		if name == corev1.ResourceMemory {
			max = resource.MustParse("256Mi")
		}
		for _, values := range []corev1.ResourceList{c.Resources.Requests, c.Resources.Limits} {
			if q, ok := values[name]; ok && (q.Sign() < 0 || q.Cmp(max) > 0) {
				return fmt.Errorf("probe resource request/limit exceeds bound")
			}
		}
	}
	for name := range c.Resources.Requests {
		if name != corev1.ResourceCPU && name != corev1.ResourceMemory {
			return fmt.Errorf("unsupported resource request")
		}
	}
	return nil
}
