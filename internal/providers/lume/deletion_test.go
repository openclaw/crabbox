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
	writeVM(t, home, name, machineID)
	path := join(home, ".lume", name)
	dir, err := os.Open(path)
	noErr(t, err)
	t.Cleanup(func() { _ = dir.Close() })
	dirInfo, err := dir.Stat()
	noErr(t, err)
	config, err := os.Open(join(path, "config.json"))
	noErr(t, err)
	t.Cleanup(func() { _ = config.Close() })
	configInfo, err := config.Stat()
	noErr(t, err)
	id, err := lumeVMImmutableIDAtPath(path, name)
	noErr(t, err)
	return path, dirInfo, config, configInfo, id
}

func TestDeleteRespectsResize(t *testing.T) {
	const name = "crabbox-resize-fence"
	path, _, _, _, id := newDeleteFence(t, name, "cmVzaXplLWZlbmNl")
	guard, err := os.OpenFile(join(filepath.Dir(path), "."+name+".resize.guard"), os.O_CREATE|os.O_RDWR, 0o600)
	noErr(t, err)
	locked, err := tryExclusiveFileLock(guard)
	if err != nil || !locked {
		t.Fatalf("lock resize guard=%v err=%v", locked, err)
	}
	want(t, deleteClaimedVMDirectory(base(), name, id), "during disk resize")
	noErr(t, unlockFile(guard))
	noErr(t, guard.Close())
	noErr(t, os.WriteFile(join(path, "resize.lock.json"), []byte(`{}`), 0o600))
	want(t, deleteClaimedVMDirectory(base(), name, id), "pending disk resize")
	noErr(t, os.Remove(join(path, "resize.lock.json")))
	oldUse := foreignLumeVMDirectoryUse
	foreignLumeVMDirectoryUse = func(string) (string, error) { return "process 123", nil }
	t.Cleanup(func() { foreignLumeVMDirectoryUse = oldUse })
	want(t, deleteClaimedVMDirectory(base(), name, id), "still in use")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("fenced VM was lost: %v", err)
	}
}

func TestDeleteKeepsReplacement(t *testing.T) {
	const name = "crabbox-delete-race"
	vmPath, originalInfo, config, configInfo, expectedID := newDeleteFence(t, name, "b3JpZ2luYWw=")
	originalPath := vmPath + "-moved"
	noErr(t, os.Rename(vmPath, originalPath))
	writeVMAt(t, filepath.Dir(vmPath), name, "cmVwbGFjZW1lbnQ=")

	err := quarantineAndDeleteLumeVM(vmPath, originalInfo, config, configInfo, expectedID, name)
	if err == nil || !strings.Contains(err.Error(), "directory changed") {
		t.Fatalf("raced deletion error=%v", err)
	}
	replacementID, err := lumeVMImmutableIDAtPath(vmPath, name)
	noErr(t, err)
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
	oldRemove := removeLumeVMDirectory
	removeLumeVMDirectory = func(path string) error {
		noErr(t, os.Remove(join(path, "config.json")))
		return errors.New("injected partial failure")
	}
	t.Cleanup(func() { removeLumeVMDirectory = oldRemove })

	err := quarantineAndDeleteLumeVM(vmPath, dirInfo, config, configInfo, expectedID, name)
	if err == nil || !strings.Contains(err.Error(), "partial Lume delete failed") {
		t.Fatalf("partial deletion error=%v", err)
	}
	gotID, err := lumeVMImmutableIDAtPath(vmPath, name)
	noErr(t, err)
	if gotID != expectedID {
		t.Fatalf("restored identity=%q want %q", gotID, expectedID)
	}
}
