package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTestboxKeyPathRejectsTraversalIDs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	for _, leaseID := range []string{"../target", "nested/target", `nested\target`, " cbx_123 "} {
		if path, err := testboxKeyPath(leaseID); err == nil {
			t.Fatalf("testboxKeyPath(%q)=%q, want error", leaseID, path)
		}
	}
}

func TestTestboxKeyPathAllowsSafeCustomIDs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	path, err := testboxKeyPath("morphvm_123")
	if err != nil {
		t.Fatal(err)
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(configDir, "crabbox", "testboxes", "morphvm_123", "id_ed25519")
	if path != want {
		t.Fatalf("testboxKeyPath()=%q want %q", path, want)
	}
}
