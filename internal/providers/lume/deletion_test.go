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
	writeLumeVMConfig(t, home, name, machineID)
	path := join(home, ".lume", name)
	dir, err := os.Open(path)
	mustNoError(t, err)
	t.Cleanup(func() { _ = dir.Close() })
	dirInfo, err := dir.Stat()
	mustNoError(t, err)
	config, err := os.Open(join(path, "config.json"))
	mustNoError(t, err)
	t.Cleanup(func() { _ = config.Close() })
	configInfo, err := config.Stat()
	mustNoError(t, err)
	id, err := lumeVMImmutableIDAtPath(path, name)
	mustNoError(t, err)
	return path, dirInfo, config, configInfo, id
}

func TestDeleteRespectsLumeResizeTransactions(t *testing.T) {
	const name = "crabbox-resize-fence"
	path, _, _, _, id := newDeleteFence(t, name, "cmVzaXplLWZlbmNl")
	guard, err := os.OpenFile(join(filepath.Dir(path), "."+name+".resize.guard"), os.O_CREATE|os.O_RDWR, 0o600)
	mustNoError(t, err)
	locked, err := tryExclusiveFileLock(guard)
	if err != nil || !locked {
		t.Fatalf("lock resize guard=%v err=%v", locked, err)
	}
	want(t, deleteClaimedVMDirectory(base(), name, id), "during disk resize")
	mustNoError(t, unlockFile(guard))
	mustNoError(t, guard.Close())
	mustNoError(t, os.WriteFile(join(path, "resize.lock.json"), []byte(`{}`), 0o600))
	want(t, deleteClaimedVMDirectory(base(), name, id), "pending disk resize")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("resize-fenced VM was lost: %v", err)
	}
}

func TestQuarantineDeletePreservesRacedReplacement(t *testing.T) {
	const name = "crabbox-delete-race"
	vmPath, originalInfo, config, configInfo, expectedID := newDeleteFence(t, name, "b3JpZ2luYWw=")
	originalPath := vmPath + "-moved"
	mustNoError(t, os.Rename(vmPath, originalPath))
	writeLumeVMConfigAt(t, filepath.Dir(vmPath), name, "cmVwbGFjZW1lbnQ=")

	err := quarantineAndDeleteLumeVM(vmPath, originalInfo, config, configInfo, expectedID, name)
	if err == nil || !strings.Contains(err.Error(), "directory changed") {
		t.Fatalf("raced deletion error=%v", err)
	}
	replacementID, err := lumeVMImmutableIDAtPath(vmPath, name)
	mustNoError(t, err)
	if replacementID == expectedID {
		t.Fatal("replacement identity was lost")
	}
	if _, err := os.Stat(originalPath); err != nil {
		t.Fatalf("original raced directory was lost: %v", err)
	}
}

func TestQuarantineDeleteRestoresPartialFailure(t *testing.T) {
	const name = "crabbox-delete-partial"
	vmPath, dirInfo, config, configInfo, expectedID := newDeleteFence(t, name, "cGFydGlhbA==")
	oldRemove := removeLumeVMDirectory
	removeLumeVMDirectory = func(path string) error {
		mustNoError(t, os.Remove(join(path, "config.json")))
		return errors.New("injected partial failure")
	}
	t.Cleanup(func() { removeLumeVMDirectory = oldRemove })

	err := quarantineAndDeleteLumeVM(vmPath, dirInfo, config, configInfo, expectedID, name)
	if err == nil || !strings.Contains(err.Error(), "partial Lume delete failed") {
		t.Fatalf("partial deletion error=%v", err)
	}
	gotID, err := lumeVMImmutableIDAtPath(vmPath, name)
	mustNoError(t, err)
	if gotID != expectedID {
		t.Fatalf("restored identity=%q want %q", gotID, expectedID)
	}
}
