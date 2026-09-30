package probe

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiskPersistenceAndCorruption(t *testing.T) {
	dir := t.TempDir()
	if _, err := Disk(dir, "run-a", false); err == nil {
		t.Fatal("reader accepted absent sentinel")
	}
	got, err := Disk(dir, "run-a", true)
	if err != nil || got != Checksum("run-a") {
		t.Fatalf("write: %s %v", got, err)
	}
	if _, err = Disk(dir, "run-a", false); err != nil {
		t.Fatal(err)
	}
	if _, err = Disk(dir, "run-b", false); err == nil {
		t.Fatal("accepted another Run's sentinel")
	}
	if _, err = Disk(dir, "run-a", true); err == nil {
		t.Fatal("writer silently overwrote existing data")
	}
	if err = os.WriteFile(filepath.Join(dir, "sentinel"), []byte("corrupted"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Disk(dir, "run-a", false); err == nil {
		t.Fatal("reader accepted corrupted data")
	}
}
func TestIdentityValidation(t *testing.T) {
	role := "arn:aws:iam::123456789012:role/path/infra-smoketest-aws"
	for _, tc := range []struct {
		account, arn string
		valid        bool
	}{
		{"123456789012", "arn:aws:sts::123456789012:assumed-role/infra-smoketest-aws/session", true},
		{"000000000000", "arn:aws:sts::123456789012:assumed-role/infra-smoketest-aws/session", false},
		{"123456789012", "arn:aws:sts::123456789012:assumed-role/worker-node/session", false},
		{"123456789012", role, false},
		{"123456789012", "arn:aws:sts::123456789012:assumed-role/infra-smoketest-aws/", false},
	} {
		if err := ValidateIdentity(role, tc.account, tc.arn); (err == nil) != tc.valid {
			t.Errorf("%s: %v", tc.arn, err)
		}
	}
}
