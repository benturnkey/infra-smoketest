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
	for _, name := range []string{"cluster-autoscaler", "ebs-csi", "aws-pod-identity-webhook", "cert-manager", "kube-state-metrics"} {
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

func TestServiceProbeDefinitionRestrictions(t *testing.T) {
	for _, tc := range []struct {
		name, example string
		edit          func(*corev1.PodSpec)
	}{
		{"cert with ordinary account", "cert-manager", func(p *corev1.PodSpec) { p.ServiceAccountName = ProbeSA }},
		{"metrics with cert account", "kube-state-metrics", func(p *corev1.PodSpec) { p.ServiceAccountName = CertManagerSA }},
		{"automounted token", "cert-manager", func(p *corev1.PodSpec) { yes := true; p.AutomountServiceAccountToken = &yes }},
		{"arbitrary env", "kube-state-metrics", func(p *corev1.PodSpec) { p.Containers[0].Env[0].Name = "AWS_ACCESS_KEY_ID" }},
		{"file url", "kube-state-metrics", func(p *corev1.PodSpec) { p.Containers[0].Env[0].Value = "file:///etc/passwd" }},
		{"url credentials", "kube-state-metrics", func(p *corev1.PodSpec) { p.Containers[0].Env[0].Value = "https://user:pass@metrics/metrics" }},
		{"env reference", "kube-state-metrics", func(p *corev1.PodSpec) {
			p.Containers[0].Env[0].ValueFrom = &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{Key: "secret"}}
		}},
		{"user token projection", "cert-manager", func(p *corev1.PodSpec) {
			p.Volumes = []corev1.Volume{{Name: "token", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{}}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := example(t, tc.example)
			tc.edit(&obj.Spec.Stages[0].CreatePod.Template.Spec)
			if err := Validate(obj.Spec, "image"); err == nil {
				t.Fatal("accepted unsupported configuration")
			}
		})
	}
}

func TestCertManagerIssuerConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name  string
		env   []corev1.EnvVar
		valid bool
	}{
		{"default", nil, true},
		{"namespaced", []corev1.EnvVar{{Name: CertManagerIssuerNameEnv, Value: "existing-ca"}}, true},
		{"cluster", []corev1.EnvVar{{Name: CertManagerIssuerNameEnv, Value: "cluster-ca"}, {Name: CertManagerIssuerKindEnv, Value: "ClusterIssuer"}}, true},
		{"kind without name", []corev1.EnvVar{{Name: CertManagerIssuerKindEnv, Value: "ClusterIssuer"}}, false},
		{"wrong kind", []corev1.EnvVar{{Name: CertManagerIssuerNameEnv, Value: "ca"}, {Name: CertManagerIssuerKindEnv, Value: "Secret"}}, false},
		{"empty name", []corev1.EnvVar{{Name: CertManagerIssuerNameEnv}}, false},
		{"cross namespace", []corev1.EnvVar{{Name: CertManagerIssuerNameEnv, Value: "other/ca"}}, false},
		{"invalid name", []corev1.EnvVar{{Name: CertManagerIssuerNameEnv, Value: "UPPERCASE"}}, false},
		{"duplicate name", []corev1.EnvVar{{Name: CertManagerIssuerNameEnv, Value: "ca"}, {Name: CertManagerIssuerNameEnv, Value: "other-ca"}}, false},
		{"reference", []corev1.EnvVar{{Name: CertManagerIssuerNameEnv, Value: "ca", ValueFrom: &corev1.EnvVarSource{}}}, false},
		{"arbitrary env", []corev1.EnvVar{{Name: "AWS_ACCESS_KEY_ID", Value: "bad"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := example(t, "cert-manager")
			obj.Spec.Stages[0].CreatePod.Template.Spec.Containers[0].Env = tc.env
			if err := Validate(obj.Spec, "image"); (err == nil) != tc.valid {
				t.Fatalf("valid=%v, got %v", tc.valid, err)
			}
		})
	}
	ref, err := ParseIssuerRef("existing-ca", "")
	if err != nil || ref.Name != "existing-ca" || ref.Kind != "Issuer" {
		t.Fatalf("incorrect default issuer kind: %+v %v", ref, err)
	}
	obj := example(t, "kube-state-metrics")
	obj.Spec.Stages[0].CreatePod.Template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: CertManagerIssuerNameEnv, Value: "ca"}}
	if err := Validate(obj.Spec, "image"); err == nil {
		t.Fatal("accepted issuer configuration for another probe")
	}
}
