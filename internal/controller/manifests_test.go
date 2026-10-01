//go:build integration

package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"slices"
	"testing"

	api "github.com/tkhq/infra-smoketest/api/v1alpha1"
	"github.com/tkhq/infra-smoketest/internal/definition"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

func TestSharedAWSManifestReferences(t *testing.T) {
	output, err := exec.Command("kustomize", "build", "../../config/default").CombinedOutput()
	if err != nil {
		t.Fatalf("build manifests: %v\n%s", err, output)
	}
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = api.AddToScheme(scheme)
	objects := serializer.NewCodecFactory(scheme).UniversalDeserializer()
	documents := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
	var config *corev1.ConfigMap
	var account *corev1.ServiceAccount
	var deployment *appsv1.Deployment
	var role *rbacv1.Role
	for {
		var raw json.RawMessage
		if err := documents.Decode(&raw); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		obj, _, err := objects.Decode(raw, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		switch obj := obj.(type) {
		case *corev1.ConfigMap:
			if obj.Data["AWS_ACCOUNT_ID"] != "" {
				config = obj
			}
		case *corev1.ServiceAccount:
			if obj.Name == definition.IdentitySA {
				account = obj
			}
		case *appsv1.Deployment:
			deployment = obj
		case *rbacv1.Role:
			role = obj
		}
	}
	if config == nil || account == nil || deployment == nil || role == nil {
		t.Fatal("default deployment must include shared AWS configuration, identity ServiceAccount, controller, and RBAC")
	}
	_, expectedRole, err := ResolveAWSIdentity(config.Data["AWS_ACCOUNT_ID"], config.Data["AWS_IDENTITY_ROLE_NAME"], "")
	if err != nil || expectedRole == "" || account.Annotations["eks.amazonaws.com/role-arn"] != expectedRole {
		t.Fatalf("ServiceAccount annotation does not match shared AWS configuration: %v", err)
	}
	container := deployment.Spec.Template.Spec.Containers[0]
	for key, flag := range map[string]string{"AWS_ACCOUNT_ID": "--aws-account-id", "AWS_IDENTITY_ROLE_NAME": "--identity-role-name"} {
		found := false
		for _, env := range container.Env {
			if env.Name == key && env.ValueFrom != nil && env.ValueFrom.ConfigMapKeyRef != nil {
				ref := env.ValueFrom.ConfigMapKeyRef
				found = ref.Name == config.Name && ref.Key == key && config.Namespace == deployment.Namespace
			}
		}
		if !found || !slices.Contains(container.Args, flag+"=$("+key+")") {
			t.Fatalf("controller does not read %s from the generated ConfigMap", key)
		}
	}
	// Kustomize also rewrites Role.resourceNames. Sharing a ConfigMap's base
	// name with the ServiceAccount would incorrectly append the ConfigMap hash.
	for _, rule := range role.Rules {
		if slices.Contains(rule.Resources, "serviceaccounts") && slices.Contains(rule.Verbs, "get") && slices.Contains(rule.ResourceNames, account.Name) {
			return
		}
	}
	t.Fatal("controller cannot read the identity ServiceAccount with the rendered RBAC")
}
