package lume

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var removeLumeVMDirectory = os.RemoveAll

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
	resizeGuard, err := os.OpenFile(filepath.Join(root, "."+name+".resize.guard"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return exit(5, "open Lume resize guard %s: %v", name, err)
	}
	defer resizeGuard.Close()
	locked, err := tryExclusiveFileLock(resizeGuard)
	if err != nil {
		return exit(5, "lock Lume resize guard %s: %v", name, err)
	}
	if !locked {
		return exit(5, "refusing to delete Lume VM %s during disk resize", name)
	}
	defer unlockFile(resizeGuard)
	if _, err := os.Lstat(filepath.Join(vmPath, "resize.lock.json")); err == nil {
		return exit(5, "refusing to delete Lume VM %s with pending disk resize", name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return exit(5, "inspect Lume resize state %s: %v", name, err)
	}
	config, err := os.Open(filepath.Join(vmPath, "config.json"))
	if err != nil {
		return exit(5, "open Lume identity %s: %v", name, err)
	}
	defer config.Close()
	locked, err = tryExclusiveFileLock(config)
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
	return quarantineAndDeleteLumeVM(vmPath, dirInfo, config, configInfo, expectedID, name)
}

func quarantineAndDeleteLumeVM(vmPath string, openedInfo os.FileInfo, config *os.File, openedConfigInfo os.FileInfo, expectedID, name string) error {
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
	backup := filepath.Join(quarantineRoot, "config.json")
	if _, err := config.Seek(0, io.SeekStart); err != nil {
		return refuse("seek identity before deletion: " + err.Error())
	}
	data, err := io.ReadAll(io.LimitReader(config, 1<<20+1))
	if err != nil || len(data) == 0 || len(data) > 1<<20 {
		return refuse("read identity before deletion")
	}
	if err := os.WriteFile(backup, data, 0o600); err != nil {
		return refuse("preserve identity before deletion: " + err.Error())
	}
	backupID, err := lumeVMImmutableIDAtPath(quarantineRoot, name)
	if err != nil || backupID != expectedID {
		return refuse("verify identity backup before deletion")
	}
	if err := removeLumeVMDirectory(quarantined); err != nil {
		if _, statErr := os.Lstat(quarantined); statErr == nil {
			return restorePartiallyDeletedLumeVM(vmPath, quarantined, quarantineRoot, backup, name, expectedID, err)
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return exit(5, "inspect partial Lume VM %s: %v; identity backup at %s", name, statErr, backup)
		}
		_ = os.Remove(backup)
		_ = os.Remove(quarantineRoot)
		return exit(5, "delete Lume VM %s: %v", name, err)
	}
	if err := os.Remove(backup); err != nil {
		return exit(5, "remove Lume identity backup %s: %v", name, err)
	}
	if err := os.Remove(quarantineRoot); err != nil {
		return exit(5, "remove Lume quarantine %s: %v", name, err)
	}
	return nil
}

func restorePartiallyDeletedLumeVM(vmPath, quarantined, quarantineRoot, backup, name, expectedID string, deleteErr error) error {
	if _, err := os.Lstat(vmPath); !errors.Is(err, os.ErrNotExist) {
		return exit(5, "partial Lume delete failed for %s: %v; VM preserved at %s; identity backup at %s", name, deleteErr, quarantined, backup)
	}
	if err := os.Rename(quarantined, vmPath); err != nil {
		return exit(5, "restore partial Lume VM %s: %v; identity backup at %s", name, err, backup)
	}
	configPath := filepath.Join(vmPath, "config.json")
	_, configErr := os.Lstat(configPath)
	backupID, backupErr := lumeVMImmutableIDAtPath(quarantineRoot, name)
	if errors.Is(configErr, os.ErrNotExist) && backupErr == nil && backupID == expectedID {
		data, readErr := os.ReadFile(backup)
		if readErr != nil {
			configErr = readErr
		} else {
			configErr = os.WriteFile(configPath, data, 0o600)
		}
	}
	if configErr != nil || backupErr != nil || backupID != expectedID {
		return exit(5, "partial Lume delete failed for %s: %v; restored VM but kept identity backup at %s", name, deleteErr, backup)
	}
	actualID, identityErr := lumeVMImmutableIDAtPath(vmPath, name)
	if identityErr != nil || actualID != expectedID {
		return exit(5, "partial Lume delete failed for %s: %v; restored identity not verified; backup at %s", name, deleteErr, backup)
	}
	if err := os.Remove(backup); err != nil {
		return exit(5, "remove restored Lume identity backup %s: %v", name, err)
	}
	_ = os.Remove(quarantineRoot)
	return exit(5, "partial Lume delete failed for %s: %v; VM restored", name, deleteErr)
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
