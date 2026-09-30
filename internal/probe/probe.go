// Package probe implements the small workloads executed inside test Pods.
package probe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	api "github.com/tkhq/infra-smoketest/api/v1alpha1"
)

func Sentinel(runUID string) []byte { return []byte("infra-smoketest/v1/" + runUID + "\n") }
func Checksum(runUID string) string {
	sum := sha256.Sum256(Sentinel(runUID))
	return hex.EncodeToString(sum[:])
}

func Disk(directory, runUID string, write bool) (string, error) {
	path := filepath.Join(directory, "sentinel")
	if write {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return "", err
		}
		_, err = f.Write(Sentinel(runUID))
		if err == nil {
			err = f.Sync()
		}
		err = errors.Join(err, f.Close())
		if err != nil {
			return "", err
		}
		d, err := os.Open(directory)
		if err != nil {
			return "", err
		}
		err = errors.Join(d.Sync(), d.Close())
		if err != nil {
			return "", err
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if string(data) != string(Sentinel(runUID)) {
		return "", errors.New("sentinel content does not match this Run")
	}
	return Checksum(runUID), nil
}

func ValidateIdentity(roleARN, account, callerARN string) error {
	role, err := arn.Parse(roleARN)
	if err != nil || role.Service != "iam" || !strings.HasPrefix(role.Resource, "role/") || role.AccountID == "" {
		return errors.New("expected role must be an IAM role ARN")
	}
	caller, err := arn.Parse(callerARN)
	roleName := role.Resource[strings.LastIndex(role.Resource, "/")+1:]
	prefix := "assumed-role/" + roleName + "/"
	if err != nil || caller.Service != "sts" || caller.Partition != role.Partition || caller.AccountID != role.AccountID || account != role.AccountID || !strings.HasPrefix(caller.Resource, prefix) || len(caller.Resource) == len(prefix) {
		return errors.New("STS caller does not match the expected role and account")
	}
	return nil
}

func Identity(ctx context.Context, expectedRole, region string) (string, string, error) {
	role := os.Getenv("AWS_ROLE_ARN")
	token := os.Getenv("AWS_WEB_IDENTITY_TOKEN_FILE")
	if expectedRole == "" || role != expectedRole || token == "" || region == "" || os.Getenv("AWS_REGION") != region || os.Getenv("AWS_DEFAULT_REGION") != region || os.Getenv("AWS_STS_REGIONAL_ENDPOINTS") != "regional" {
		return "", "", errors.New("missing or incorrect webhook identity configuration")
	}
	for _, key := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_CREDENTIALS_FULL_URI"} {
		if os.Getenv(key) != "" {
			return "", "", fmt.Errorf("unexpected credential source: %s", key)
		}
	}
	// Construct the only permitted credential provider explicitly. No config
	// loader, IMDS, environment credentials, profile, or endpoint overrides.
	cfg := aws.Config{Region: region, Credentials: aws.AnonymousCredentials{}, HTTPClient: &http.Client{Timeout: 20 * time.Second}, RetryMaxAttempts: 3}
	provider := stscreds.NewWebIdentityRoleProvider(sts.NewFromConfig(cfg), role, stscreds.IdentityTokenFile(token))
	cfg.Credentials = aws.NewCredentialsCache(provider)
	out, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", "", errors.New("STS web-identity authentication failed; inspect IAM trust and STS connectivity")
	}
	account, caller := aws.ToString(out.Account), aws.ToString(out.Arn)
	return account, caller, ValidateIdentity(expectedRole, account, caller)
}

func Run(ctx context.Context, command string) error {
	if command == "ready" {
		mux := http.NewServeMux()
		mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ready\n")) })
		srv := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() { <-ctx.Done(); _ = srv.Close() }()
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
	result := api.ProbeResult{Version: 1, RunUID: os.Getenv("SMOKETEST_RUN_UID"), Stage: os.Getenv("SMOKETEST_STAGE"), Container: "probe"}
	var err error
	if result.RunUID == "" || result.Stage == "" {
		err = errors.New("missing Run identity")
	} else {
		switch command {
		case "storage-write", "storage-read":
			result.Checksum, err = Disk("/data", result.RunUID, command == "storage-write")
		case "identity":
			result.Account, result.ARN, err = Identity(ctx, os.Getenv("SMOKETEST_EXPECTED_ROLE_ARN"), os.Getenv("SMOKETEST_REGION"))
		default:
			err = fmt.Errorf("unknown probe command %q", command)
		}
	}
	result.Success = err == nil
	if err != nil {
		result.Error = err.Error()
		if len(result.Error) > 512 {
			result.Error = result.Error[:512]
		}
	}
	data, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		return marshalErr
	}
	path := os.Getenv("SMOKETEST_RESULT_PATH")
	if path == "" {
		path = "/dev/termination-log"
	}
	if writeErr := os.WriteFile(path, data, 0600); writeErr != nil {
		return errors.Join(err, writeErr)
	}
	fmt.Println(string(data))
	return err
}
