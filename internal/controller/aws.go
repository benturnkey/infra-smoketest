package controller

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/tkhq/infra-smoketest/internal/definition"
)

var accountIDPattern = regexp.MustCompile(`^[0-9]{12}$`)
var roleNamePattern = regexp.MustCompile(`^[A-Za-z0-9_+=,.@-]{1,64}$`)

// ResolveAWSIdentity resolves shared controller settings before accepting Runs.
// A full ARN remains supported for deployments using an IAM path or partition
// other than aws. With no AWS settings, non-identity tests can still run.
func ResolveAWSIdentity(accountID, roleName, roleARN string) (string, string, error) {
	if accountID != "" && !accountIDPattern.MatchString(accountID) {
		return "", "", fmt.Errorf("--aws-account-id must contain exactly 12 digits")
	}
	if roleName != "" && !roleNamePattern.MatchString(roleName) {
		return "", "", fmt.Errorf("--identity-role-name must be an IAM role name; use --expected-role-arn for a role with a path")
	}
	if roleARN != "" {
		role, err := arn.Parse(roleARN)
		if err != nil || role.Partition == "" || role.Service != "iam" || role.Region != "" || !accountIDPattern.MatchString(role.AccountID) || !strings.HasPrefix(role.Resource, "role/") || !roleNamePattern.MatchString(role.Resource[strings.LastIndex(role.Resource, "/")+1:]) {
			return "", "", fmt.Errorf("--expected-role-arn must be an IAM role ARN")
		}
		if accountID != "" && role.AccountID != accountID {
			return "", "", fmt.Errorf("--expected-role-arn account does not match --aws-account-id")
		}
		if roleName != "" && role.Resource != "role/"+roleName {
			return "", "", fmt.Errorf("--expected-role-arn does not match --identity-role-name")
		}
		return role.AccountID, roleARN, nil
	}
	if accountID == "" {
		if roleName != "" {
			return "", "", fmt.Errorf("--identity-role-name requires --aws-account-id")
		}
		return "", "", nil
	}
	if roleName == "" {
		roleName = definition.IdentitySA
	}
	return accountID, fmt.Sprintf("arn:aws:iam::%s:role/%s", accountID, roleName), nil
}
