package sealosdevbox

import (
	"os"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestValidateDevboxSecretOwnerRequiresExactControllerUID(t *testing.T) {
	item := devboxItem{Metadata: devboxMeta{Name: "devbox-blue", Namespace: "team-a", UID: "uid-devbox"}}
	secret := devboxSecret{Metadata: devboxMeta{
		Name:      "devbox-blue",
		Namespace: "team-a",
		OwnerReferences: []ownerReference{{
			APIVersion: devboxGroupVersion,
			Kind:       devboxKind,
			Name:       "devbox-blue",
			UID:        "uid-devbox",
			Controller: true,
		}},
	}}
	if err := validateDevboxSecretOwner(secret, item); err != nil {
		t.Fatalf("exact owner rejected: %v", err)
	}

	secret.Metadata.OwnerReferences[0].UID = "uid-other"
	if err := validateDevboxSecretOwner(secret, item); err == nil {
		t.Fatal("Secret owned by another DevBox UID was accepted")
	}
}

func TestPersistDevboxKeyLeavesNoPrivateKeyWhenPublicWriteFails(t *testing.T) {
	isolateSealosState(t)
	leaseID := "cbx_123456abcdef"
	path, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path+".pub", 0o700); err != nil {
		t.Fatal(err)
	}
	_, err = persistDevboxKey(leaseID, devboxSecretKeys{PublicKey: "ssh-ed25519 AAA test", PrivateKey: "private\n"})
	if err == nil {
		t.Fatal("persist succeeded with a directory at the public key path")
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("private key residue after public write failure: %v", statErr)
	}
}

func TestPersistDevboxKeyIfClaimUnchangedRejectsStaleClaimBeforeWrite(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	leaseID := "cbx_stalekey123"
	slug := "blue"
	name := core.LeaseProviderName(leaseID, slug)
	server := claimExactSealosTarget(t, cfg, leaseID, slug, name, t.TempDir(), core.SSHTarget{})
	expected, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	keyPath, err := persistDevboxKey(leaseID, devboxSecretKeys{PublicKey: "ssh-ed25519 AAA winner", PrivateKey: "winner-private\n"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := core.UpdateLeaseClaimEndpointIfUnchanged(leaseID, expected, server, core.SSHTarget{Host: "winner.example.test", Port: "22"}); err != nil {
		t.Fatal(err)
	}

	_, writtenPath, err := persistDevboxKeyIfClaimUnchanged(leaseID, expected, server, devboxSecretKeys{PublicKey: "ssh-ed25519 AAA loser", PrivateKey: "loser-private\n"})
	if err == nil || !strings.Contains(err.Error(), "claim changed") {
		t.Fatalf("persist error=%v", err)
	}
	if writtenPath != "" {
		t.Fatalf("stale claim wrote key path %q", writtenPath)
	}
	privateKey, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(privateKey) != "winner-private\n" {
		t.Fatalf("stale claim replaced winning key: %q", privateKey)
	}
}
