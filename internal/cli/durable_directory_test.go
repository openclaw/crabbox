package cli

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestEnsurePrivateDirectoryDurableSyncsFreshAncestorChain(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "new-state", "crabbox", "claims")
	var synced []string
	if err := ensurePrivateDirectoryDurableWithinWithSync(dir, base, func(path string) error {
		synced = append(synced, filepath.Clean(path))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(base, "new-state", "crabbox"),
		filepath.Join(base, "new-state"),
		base,
	}
	if !reflect.DeepEqual(synced, want) {
		t.Fatalf("synced=%q want=%q", synced, want)
	}
}

func TestEnsureCrabboxClaimNamespaceDurableRejectsRelativeXDGRoot(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join("relative-home", "state"))
	called := false
	err := ensureCrabboxClaimNamespaceDurableWithSync(func(string) error {
		called = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("error=%v", err)
	}
	if called {
		t.Fatal("relative state root reached directory sync")
	}
}

func TestEnsureCrabboxClaimNamespaceDurableResyncsExistingChain(t *testing.T) {
	base := t.TempDir()
	stateRoot := filepath.Join(base, "fresh", "state")
	t.Setenv("XDG_STATE_HOME", stateRoot)
	stateDir, err := crabboxStateDir()
	if err != nil {
		t.Fatal(err)
	}
	claimDir := filepath.Join(stateDir, "claims")
	if err := ensurePrivateDirectoryDurableWithinWithSync(claimDir, base, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	var synced []string
	if err := ensureCrabboxClaimNamespaceDurableWithSync(func(path string) error {
		synced = append(synced, filepath.Clean(path))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	boundary, err := privateDirectoryDurabilityBoundary(claimDir, stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(synced) == 0 || synced[0] != filepath.Clean(stateDir) || synced[len(synced)-1] != boundary {
		t.Fatalf("syncs=%q want first=%q last=%q", synced, stateDir, boundary)
	}
}
