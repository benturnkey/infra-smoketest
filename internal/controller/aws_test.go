package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tkhq/infra-smoketest/internal/definition"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestResolveAWSIdentity(t *testing.T) {
	const account = "123456789012"
	const role = "arn:aws:iam::123456789012:role/infra-smoketest-aws"
	for _, tc := range []struct {
		name, account, roleName, roleARN, wantAccount, wantRole, wantError string
	}{
		{name: "unconfigured"},
		{name: "shared account", account: account, wantAccount: account, wantRole: role},
		{name: "named role", account: account, roleName: "test-role", wantAccount: account, wantRole: "arn:aws:iam::123456789012:role/test-role"},
		{name: "legacy ARN", roleARN: role, wantAccount: account, wantRole: role},
		{name: "explicit matching settings", account: account, roleName: "infra-smoketest-aws", roleARN: role, wantAccount: account, wantRole: role},
		{name: "IAM path", roleARN: "arn:aws:iam::123456789012:role/smoke/test-role", wantAccount: account, wantRole: "arn:aws:iam::123456789012:role/smoke/test-role"},
		{name: "partition", roleARN: "arn:aws-us-gov:iam::123456789012:role/test-role", wantAccount: account, wantRole: "arn:aws-us-gov:iam::123456789012:role/test-role"},
		{name: "short account", account: "1234", wantError: "12 digits"},
		{name: "non-numeric account", account: "abcdefghijkl", wantError: "12 digits"},
		{name: "missing account", roleName: "test-role", wantError: "requires --aws-account-id"},
		{name: "invalid role", account: account, roleName: "test role", wantError: "IAM role name"},
		{name: "account mismatch", account: "000000000000", roleARN: role, wantError: "account does not match"},
		{name: "role mismatch", account: account, roleName: "other-role", roleARN: role, wantError: "does not match --identity-role-name"},
		{name: "invalid ARN", roleARN: "infra-smoketest-aws", wantError: "IAM role ARN"},
		{name: "user ARN", roleARN: "arn:aws:iam::123456789012:user/test", wantError: "IAM role ARN"},
		{name: "missing role", roleARN: "arn:aws:iam::123456789012:role/", wantError: "IAM role ARN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotAccount, gotRole, err := ResolveAWSIdentity(tc.account, tc.roleName, tc.roleARN)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("want error containing %q, got %v", tc.wantError, err)
				}
				return
			}
			if err != nil || gotAccount != tc.wantAccount || gotRole != tc.wantRole {
				t.Fatalf("got account %q, role %q, error %v", gotAccount, gotRole, err)
			}
		})
	}
}

func TestIdentityPodRequiresRoleInSnapshottedAccount(t *testing.T) {
	account, role, err := ResolveAWSIdentity("123456789012", "", "")
	if err != nil {
		t.Fatal(err)
	}
	run := testRun()
	run.Status.AWSAccountID, run.Status.ExpectedRoleARN = account, role
	stage := testPodStage("identity")
	stage.CreatePod.Template.Spec.ServiceAccountName = definition.IdentitySA
	stage.CreatePod.Template.Spec.NodeSelector = nil
	for _, annotatedRole := range []string{role, "arn:aws:iam::000000000000:role/infra-smoketest-aws", ""} {
		t.Run(annotatedRole, func(t *testing.T) {
			r := fakeReconciler(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: definition.IdentitySA, Namespace: definition.Namespace, Annotations: map[string]string{"eks.amazonaws.com/role-arn": annotatedRole}}})
			// Later controller configuration must not change this Run's expectation.
			r.AWSAccountID = "000000000000"
			pod, err := r.pod(context.Background(), run, stage, "probe")
			if annotatedRole != role {
				var f *failure
				if !errors.As(err, &f) || f.reason != "PreconditionFailed" {
					t.Fatalf("accepted mismatched ServiceAccount role: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			env := map[string]string{}
			for _, e := range pod.Spec.Containers[0].Env {
				env[e.Name] = e.Value
			}
			if env["SMOKETEST_AWS_ACCOUNT_ID"] != account || env["SMOKETEST_EXPECTED_ROLE_ARN"] != role {
				t.Fatalf("probe did not inherit snapshotted identity settings: %v", env)
			}
		})
	}
}
