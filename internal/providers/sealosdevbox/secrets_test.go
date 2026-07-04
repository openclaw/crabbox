package sealosdevbox

import (
	"os"
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
