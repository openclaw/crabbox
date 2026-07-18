package lume

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Lume delete accepts a mutable name. Lock, quarantine, and recheck the exact
// directory and config inodes before removing the claimed VM.
func deleteClaimedVMDirectory(cfg Config, name, expectedID string) error {
	expectedID = strings.TrimSpace(expectedID)
	if expectedID == "" {
		return exit(5, "refusing Lume delete %q without immutable identity", name)
	}
	if filepath.Base(name) != name || name == "." || name == ".." {
		return exit(5, "refusing unsafe Lume VM name %q", name)
	}
	root, err := lumeStorageRoot(cfg, "")
	if err != nil {
		return err
	}
	vmPath := filepath.Join(root, name)
	dir, err := os.Open(vmPath)
	if err != nil {
		return exit(5, "open Lume VM %s: %v", name, err)
	}
	defer dir.Close()
	dirInfo, err := dir.Stat()
	if err != nil || !dirInfo.IsDir() {
		return exit(5, "inspect Lume VM %s: %v", name, err)
	}
	config, err := os.Open(filepath.Join(vmPath, "config.json"))
	if err != nil {
		return exit(5, "open Lume identity %s: %v", name, err)
	}
	defer config.Close()
	locked, err := tryExclusiveFileLock(config)
	if err != nil {
		return exit(5, "lock Lume VM %s: %v", name, err)
	}
	if !locked {
		return exit(5, "refusing to delete running Lume VM %s", name)
	}
	defer unlockFile(config)
	configInfo, err := config.Stat()
	if err != nil {
		return exit(5, "inspect locked Lume VM %s: %v", name, err)
	}
	return quarantineAndDeleteLumeVM(vmPath, dirInfo, configInfo, expectedID, name)
}

func quarantineAndDeleteLumeVM(vmPath string, openedInfo, openedConfigInfo os.FileInfo, expectedID, name string) error {
	quarantineRoot, err := os.MkdirTemp(filepath.Dir(vmPath), ".crabbox-delete-")
	if err != nil {
		return exit(5, "create Lume delete quarantine %s: %v", name, err)
	}
	quarantined := filepath.Join(quarantineRoot, name)
	if err := os.Rename(vmPath, quarantined); err != nil {
		_ = os.Remove(quarantineRoot)
		return exit(5, "quarantine Lume VM %s: %v", name, err)
	}
	refuse := func(reason string) error {
		return restoreQuarantinedLumeVM(vmPath, quarantined, quarantineRoot, name, reason)
	}
	movedInfo, err := os.Lstat(quarantined)
	if err != nil || !os.SameFile(openedInfo, movedInfo) {
		return refuse("directory changed before deletion")
	}
	movedConfigInfo, err := os.Lstat(filepath.Join(quarantined, "config.json"))
	if err != nil || !os.SameFile(openedConfigInfo, movedConfigInfo) {
		return refuse("identity config changed before deletion")
	}
	actualID, err := lumeVMImmutableIDAtPath(quarantined, name)
	if err != nil || actualID != expectedID {
		reason := "immutable machine identity changed before deletion"
		if err != nil {
			reason = err.Error()
		}
		return refuse(reason)
	}
	if err := os.RemoveAll(quarantined); err != nil {
		return exit(5, "delete Lume VM %s: %v", name, err)
	}
	if err := os.Remove(quarantineRoot); err != nil {
		return exit(5, "remove Lume quarantine %s: %v", name, err)
	}
	return nil
}

func restoreQuarantinedLumeVM(vmPath, quarantined, quarantineRoot, name, reason string) error {
	if _, err := os.Lstat(vmPath); errors.Is(err, os.ErrNotExist) {
		if os.Rename(quarantined, vmPath) == nil {
			_ = os.Remove(quarantineRoot)
			return exit(4, "refusing to delete Lume VM %q: %s", name, reason)
		}
	} else if err != nil {
		return errors.Join(exit(4, "refusing to delete Lume VM %q: %s", name, reason), err)
	}
	return exit(4, "refusing to delete Lume VM %q: %s; preserved raced directory at %s", name, reason, quarantined)
}
