package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestProbeImageDefaultsToController(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "manager", Namespace: "infra-smoketest"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "sidecar", Image: "wrong"}, {Name: "controller", Image: "ghcr.io/example/smoke@sha256:abc"}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	got, err := ResolveProbeImage(context.Background(), c, "", pod.Namespace, pod.Name, "controller")
	if err != nil || got != pod.Spec.Containers[1].Image {
		t.Fatalf("got %q: %v", got, err)
	}
	got, err = ResolveProbeImage(context.Background(), c, "override@sha256:def", "", "", "controller")
	if err != nil || got != "override@sha256:def" {
		t.Fatalf("override: %q %v", got, err)
	}
	if _, err = ResolveProbeImage(context.Background(), c, "", "", "", "controller"); err == nil {
		t.Fatal("silently chose an image without deployment context")
	}
	if _, err = ResolveProbeImage(context.Background(), c, "", pod.Namespace, pod.Name, "missing"); err == nil {
		t.Fatal("silently chose wrong container")
	}
}
