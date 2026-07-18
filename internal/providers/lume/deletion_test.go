package lume

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestQuarantineDeletePreservesRacedReplacement(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	const name = "crabbox-delete-race"
	writeLumeVMConfig(t, home, name, "b3JpZ2luYWw=")
	vmPath := filepath.Join(home, ".lume", name)
	dir, err := os.Open(vmPath)
	mustNoError(t, err)
	defer dir.Close()
	originalInfo, err := dir.Stat()
	mustNoError(t, err)
	configInfo, err := os.Stat(filepath.Join(vmPath, "config.json"))
	mustNoError(t, err)
	expectedID, err := lumeVMImmutableIDAtPath(vmPath, name)
	mustNoError(t, err)
	originalPath := vmPath + "-moved"
	mustNoError(t, os.Rename(vmPath, originalPath))
	writeLumeVMConfig(t, home, name, "cmVwbGFjZW1lbnQ=")

	err = quarantineAndDeleteLumeVM(vmPath, originalInfo, configInfo, expectedID, name)
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
