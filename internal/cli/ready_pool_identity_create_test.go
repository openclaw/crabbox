package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateReadyPoolIdentityCreateLeaseRequiresExactAWSShape(t *testing.T) {
	expected := readyPoolIdentityCreateExpected{
		ImageID:      "ami-0123456789abcdef0",
		ServerType:   "m7i.large",
		Architecture: ArchitectureAMD64,
		Profile:      "linux-builder",
		RecipeDigest: "sha256:" + strings.Repeat("a", 64),
	}
	valid := CoordinatorLease{
		Provider:     "aws",
		TargetOS:     targetLinux,
		Architecture: "x86_64",
		ServerType:   "m7i.large",
		SSHHostKey:   "ssh-ed25519 AAAAauthoritative",
		Image:        &CoordinatorLeaseImage{ID: "ami-0123456789abcdef0"},
	}
	if err := validateReadyPoolIdentityCreateLease(valid, expected); err != nil {
		t.Fatalf("valid lease rejected: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*CoordinatorLease)
		want   string
	}{
		{name: "provider", mutate: func(lease *CoordinatorLease) { lease.Provider = "gcp" }, want: "requires an AWS lease"},
		{name: "target", mutate: func(lease *CoordinatorLease) { lease.TargetOS = targetWindows }, want: "native Linux"},
		{name: "host key", mutate: func(lease *CoordinatorLease) { lease.SSHHostKey = "" }, want: "authoritative coordinator SSH host key"},
		{name: "image", mutate: func(lease *CoordinatorLease) { lease.Image.ID = "ami-other" }, want: "expected AMI"},
		{name: "type", mutate: func(lease *CoordinatorLease) { lease.ServerType = "m7i.xlarge" }, want: "expected AWS instance type"},
		{name: "architecture", mutate: func(lease *CoordinatorLease) { lease.Architecture = ArchitectureARM64 }, want: "expected architecture"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lease := valid
			image := *valid.Image
			lease.Image = &image
			tc.mutate(&lease)
			err := validateReadyPoolIdentityCreateLease(lease, expected)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
		})
	}
}

func TestWriteReadyPoolIdentityAtomicCreatesPrivateCanonicalFile(t *testing.T) {
	output := filepath.Join(t.TempDir(), "identity.json")
	identity := CoordinatorReadyPoolIdentityV1{
		Schema:          readyPoolIdentitySchemaV1,
		Profile:         "linux-builder",
		RecipeDigest:    "sha256:" + strings.Repeat("a", 64),
		InventoryDigest: "sha256:" + strings.Repeat("b", 64),
		ImageID:         "ami-0123456789abcdef0",
		Architecture:    ArchitectureAMD64,
		SeedDigest:      "sha256:" + strings.Repeat("c", 64),
		CacheABIDigest:  "sha256:" + strings.Repeat("d", 64),
	}
	if err := writeReadyPoolIdentityAtomic(output, identity); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode=%o, want 600", got)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var decoded CoordinatorReadyPoolIdentityV1
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded != identity {
		t.Fatalf("decoded=%#v, want %#v", decoded, identity)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		t.Fatalf("identity lacks trailing newline: %q", data)
	}
	if err := writeReadyPoolIdentityAtomic(output, CoordinatorReadyPoolIdentityV1{}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("overwrite error=%v", err)
	}
}

func TestPoolIdentityCreateAppearsInKongHelp(t *testing.T) {
	var stdout strings.Builder
	err := (App{Stdout: &stdout, Stderr: &stdout}).Run(t.Context(), []string{"pool", "identity", "--help"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "create") {
		t.Fatalf("pool identity help=%q", stdout.String())
	}
}
