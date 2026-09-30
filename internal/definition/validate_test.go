package definition

import (
	"os"
	"path/filepath"
	"testing"

	api "github.com/tkhq/infra-smoketest/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

func example(t *testing.T, name string) *api.SmokeTest {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "examples", name+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	obj := &api.SmokeTest{}
	if err = yaml.Unmarshal(b, obj); err != nil {
		t.Fatal(err)
	}
	return obj
}
func TestExamples(t *testing.T) {
	for _, name := range []string{"cluster-autoscaler", "ebs-csi", "aws-pod-identity-webhook"} {
		t.Run(name, func(t *testing.T) {
			if err := Validate(example(t, name).Spec, "example@sha256:abc"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestRejectInvalidDefinitions(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*api.SmokeTest)
	}{
		{"cycle", func(s *api.SmokeTest) { s.Spec.Stages[0].DependsOn = []string{"scale-up"} }},
		{"missing dependency", func(s *api.SmokeTest) { s.Spec.Stages[1].DependsOn = []string{"missing"} }},
		{"reference outside ancestors", func(s *api.SmokeTest) { s.Spec.Stages[2].DependsOn = []string{"empty-pool"} }},
		{"duplicate", func(s *api.SmokeTest) { s.Spec.Stages[1].Name = s.Spec.Stages[0].Name }},
		{"host network", func(s *api.SmokeTest) { s.Spec.Stages[1].CreatePod.Template.Spec.HostNetwork = true }},
		{"shell command", func(s *api.SmokeTest) {
			s.Spec.Stages[1].CreatePod.Template.Spec.Containers[0].Command = []string{"sh", "-c", "echo foo"}
		}},
		{"static credentials", func(s *api.SmokeTest) {
			s.Spec.Stages[1].CreatePod.Template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "AWS_ACCESS_KEY_ID", Value: "bad"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := example(t, "cluster-autoscaler")
			tc.edit(s)
			if err := Validate(s.Spec, "test"); err == nil {
				t.Fatal("invalid definition accepted")
			}
		})
	}
}
