package lume

import (
	"errors"
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
	config, err := os.Open(filepath.Join(vmPath, "config.json"))
	mustNoError(t, err)
	defer config.Close()
	configInfo, err := config.Stat()
	mustNoError(t, err)
	expectedID, err := lumeVMImmutableIDAtPath(vmPath, name)
	mustNoError(t, err)
	originalPath := vmPath + "-moved"
	mustNoError(t, os.Rename(vmPath, originalPath))
	writeLumeVMConfig(t, home, name, "cmVwbGFjZW1lbnQ=")

	err = quarantineAndDeleteLumeVM(vmPath, originalInfo, config, configInfo, expectedID, name)
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
	home := t.TempDir()
	t.Setenv("HOME", home)
	const name = "crabbox-delete-partial"
	writeLumeVMConfig(t, home, name, "cGFydGlhbA==")
	vmPath := filepath.Join(home, ".lume", name)
	dir, err := os.Open(vmPath)
	mustNoError(t, err)
	defer dir.Close()
	dirInfo, err := dir.Stat()
	mustNoError(t, err)
	config, err := os.Open(filepath.Join(vmPath, "config.json"))
	mustNoError(t, err)
	defer config.Close()
	configInfo, err := config.Stat()
	mustNoError(t, err)
	expectedID, err := lumeVMImmutableIDAtPath(vmPath, name)
	mustNoError(t, err)
	oldRemove := removeLumeVMDirectory
	removeLumeVMDirectory = func(path string) error {
		mustNoError(t, os.Remove(filepath.Join(path, "config.json")))
		return errors.New("injected partial failure")
	}
	t.Cleanup(func() { removeLumeVMDirectory = oldRemove })

	err = quarantineAndDeleteLumeVM(vmPath, dirInfo, config, configInfo, expectedID, name)
	if err == nil || !strings.Contains(err.Error(), "partial Lume delete failed") {
		t.Fatalf("partial deletion error=%v", err)
	}
	gotID, err := lumeVMImmutableIDAtPath(vmPath, name)
	mustNoError(t, err)
	if gotID != expectedID {
		t.Fatalf("restored identity=%q want %q", gotID, expectedID)
	}
}
