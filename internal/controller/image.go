package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ResolveProbeImage uses the deployed container image reference, not a compiled
// tag or imageID (which can be a runtime-specific, architecture-specific ID).
func ResolveProbeImage(ctx context.Context, reader client.Reader, override, namespace, podName, containerName string) (string, error) {
	if override != "" {
		return override, nil
	}
	if namespace == "" || podName == "" {
		return "", fmt.Errorf("POD_NAMESPACE and POD_NAME are required to discover the controller image; use --probe-image when running locally")
	}
	pod := &corev1.Pod{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, pod); err != nil {
		return "", fmt.Errorf("read controller Pod image: %w", err)
	}
	for _, container := range pod.Spec.Containers {
		if container.Name == containerName && container.Image != "" {
			return container.Image, nil
		}
	}
	return "", fmt.Errorf("controller container %q has no image", containerName)
}
