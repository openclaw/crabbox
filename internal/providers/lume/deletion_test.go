package lume

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newDeleteFence(t *testing.T, name, machineID string) (string, os.FileInfo, *os.File, os.FileInfo, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", join(home, ".config"))
	putVM(t, home, name, machineID)
	path := join(home, ".lume", name)
	dir, err := os.Open(path)
	must(t, err)
	t.Cleanup(func() { _ = dir.Close() })
	dirInfo, err := dir.Stat()
	must(t, err)
	config, err := os.Open(join(path, "config.json"))
	must(t, err)
	t.Cleanup(func() { _ = config.Close() })
	configInfo, err := config.Stat()
	must(t, err)
	id, err := lumeVMImmutableIDAtPath(path, name)
	must(t, err)
	return path, dirInfo, config, configInfo, id
}

func TestDeleteRespectsResize(t *testing.T) {
	const name = "crabbox-resize-fence"
	path, _, _, _, id := newDeleteFence(t, name, "cmVzaXplLWZlbmNl")
	guard, err := os.OpenFile(join(filepath.Dir(path), "."+name+".resize.guard"), os.O_CREATE|os.O_RDWR, 0o600)
	must(t, err)
	locked, err := tryExclusiveFileLock(guard)
	if err != nil || !locked {
		t.Fatalf("lock resize guard=%v err=%v", locked, err)
	}
	want(t, deleteClaimedVM(base(), name, id), "during disk resize")
	must(t, unlockFile(guard))
	must(t, guard.Close())
	must(t, os.WriteFile(join(path, "resize.lock.json"), []byte(`{}`), 0o600))
	want(t, deleteClaimedVM(base(), name, id), "pending disk resize")
	must(t, os.Remove(join(path, "resize.lock.json")))
	oldUse := foreignVMUse
	foreignVMUse = func(string) (string, error) { return "process 123", nil }
	t.Cleanup(func() { foreignVMUse = oldUse })
	want(t, deleteClaimedVM(base(), name, id), "still in use")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("fenced VM was lost: %v", err)
	}
}

func TestDeleteKeepsReplacement(t *testing.T) {
	const name = "crabbox-delete-race"
	vmPath, originalInfo, config, configInfo, expectedID := newDeleteFence(t, name, "b3JpZ2luYWw=")
	originalPath := vmPath + "-moved"
	must(t, os.Rename(vmPath, originalPath))
	putVMAt(t, filepath.Dir(vmPath), name, "cmVwbGFjZW1lbnQ=")

	err := quarantineDeleteVM(vmPath, originalInfo, config, configInfo, expectedID, name)
	if err == nil || !strings.Contains(err.Error(), "directory changed") {
		t.Fatalf("raced deletion error=%v", err)
	}
	replacementID, err := lumeVMImmutableIDAtPath(vmPath, name)
	must(t, err)
	if replacementID == expectedID {
		t.Fatal("replacement identity was lost")
	}
	if _, err := os.Stat(originalPath); err != nil {
		t.Fatalf("original raced directory was lost: %v", err)
	}
}

func TestDeleteRestoresPartial(t *testing.T) {
	const name = "crabbox-delete-partial"
	vmPath, dirInfo, config, configInfo, expectedID := newDeleteFence(t, name, "cGFydGlhbA==")
	oldRemove := removeVMDir
	removeVMDir = func(path string) error {
		must(t, os.Remove(join(path, "config.json")))
		return errors.New("injected partial failure")
	}
	t.Cleanup(func() { removeVMDir = oldRemove })

	err := quarantineDeleteVM(vmPath, dirInfo, config, configInfo, expectedID, name)
	if err == nil || !strings.Contains(err.Error(), "partial Lume delete failed") {
		t.Fatalf("partial deletion error=%v", err)
	}
	gotID, err := lumeVMImmutableIDAtPath(vmPath, name)
	must(t, err)
	if gotID != expectedID {
		t.Fatalf("restored identity=%q want %q", gotID, expectedID)
	}
}
