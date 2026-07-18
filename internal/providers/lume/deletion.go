package lume

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// deleteClaimedVMDirectory removes only the directory inode whose config was
// locked and whose immutable identity matches the lease claim. Lume's delete
// command accepts only a mutable name, so it cannot provide this fence.
func deleteClaimedVMDirectory(cfg Config, name, expectedID string) error {
	expectedID = strings.TrimSpace(expectedID)
	if expectedID == "" {
		return exit(5, "refusing to delete Lume VM %q without an immutable machine identity", name)
	}
	if filepath.Base(name) != name || name == "." || name == ".." {
		return exit(5, "refusing unsafe Lume VM name %q", name)
	}
	root, err := lumeStorageRoot(cfg, "")
	if err != nil {
		return err
	}
	vmPath := filepath.Join(root, name)
	info, err := os.Lstat(vmPath)
	if err != nil {
		return exit(5, "inspect Lume VM directory for %s: %v", name, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return exit(5, "refusing to delete non-directory Lume VM path %q", vmPath)
	}
	dir, err := os.Open(vmPath)
	if err != nil {
		return exit(5, "open Lume VM directory for %s: %v", name, err)
	}
	defer dir.Close()
	dirInfo, err := dir.Stat()
	if err != nil || !os.SameFile(info, dirInfo) {
		return exit(5, "Lume VM directory %q changed while opening it", name)
	}
	config, err := os.Open(filepath.Join(vmPath, "config.json"))
	if err != nil {
		return exit(5, "open Lume VM identity for %s: %v", name, err)
	}
	defer config.Close()
	locked, err := tryExclusiveFileLock(config)
	if err != nil {
		return exit(5, "lock Lume VM identity for %s: %v", name, err)
	}
	if !locked {
		return exit(5, "refusing to delete running Lume VM %s", name)
	}
	defer unlockFile(config)
	configInfo, err := config.Stat()
	if err != nil {
		return exit(5, "inspect locked Lume VM identity for %s: %v", name, err)
	}
	return quarantineAndDeleteLumeVM(vmPath, dirInfo, configInfo, expectedID, name)
}

func quarantineAndDeleteLumeVM(vmPath string, openedInfo, openedConfigInfo os.FileInfo, expectedID, name string) error {
	root := filepath.Dir(vmPath)
	quarantineRoot, err := os.MkdirTemp(root, ".crabbox-delete-")
	if err != nil {
		return exit(5, "create Lume deletion quarantine for %s: %v", name, err)
	}
	quarantined := filepath.Join(quarantineRoot, name)
	moved := false
	defer func() {
		if !moved {
			_ = os.Remove(quarantineRoot)
		}
	}()
	if err := os.Rename(vmPath, quarantined); err != nil {
		return exit(5, "quarantine Lume VM %s for deletion: %v", name, err)
	}
	moved = true
	movedInfo, err := os.Lstat(quarantined)
	if err != nil || !os.SameFile(openedInfo, movedInfo) {
		return restoreQuarantinedLumeVM(vmPath, quarantined, quarantineRoot, name,
			"directory changed before deletion")
	}
	movedConfigInfo, err := os.Lstat(filepath.Join(quarantined, "config.json"))
	if err != nil || !os.SameFile(openedConfigInfo, movedConfigInfo) {
		return restoreQuarantinedLumeVM(vmPath, quarantined, quarantineRoot, name,
			"identity config changed before deletion")
	}
	actualID, err := lumeVMImmutableIDAtPath(quarantined, name)
	if err != nil || actualID != expectedID {
		reason := "immutable machine identity changed before deletion"
		if err != nil {
			reason = err.Error()
		}
		return restoreQuarantinedLumeVM(vmPath, quarantined, quarantineRoot, name, reason)
	}
	if err := os.RemoveAll(quarantined); err != nil {
		return exit(5, "delete quarantined Lume VM %s: %v", name, err)
	}
	if err := os.Remove(quarantineRoot); err != nil {
		return exit(5, "remove Lume deletion quarantine for %s: %v", name, err)
	}
	return nil
}

func restoreQuarantinedLumeVM(vmPath, quarantined, quarantineRoot, name, reason string) error {
	if _, err := os.Lstat(vmPath); errors.Is(err, os.ErrNotExist) {
		if restoreErr := os.Rename(quarantined, vmPath); restoreErr == nil {
			_ = os.Remove(quarantineRoot)
			return exit(4, "refusing to delete Lume VM %q: %s", name, reason)
		}
	} else if err != nil {
		return errors.Join(exit(4, "refusing to delete Lume VM %q: %s", name, reason), err)
	}
	return exit(4, "refusing to delete Lume VM %q: %s; preserved raced directory at %s", name, reason, quarantined)
}
