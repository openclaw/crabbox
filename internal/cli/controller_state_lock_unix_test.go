//go:build linux || darwin

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestControllerStateLockRejectsInsecureDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o770); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o770); err != nil {
		t.Fatal(err)
	}
	_, err := acquireControllerStateLock(filepath.Join(dir, "controller.json"))
	if err == nil || !strings.Contains(err.Error(), "writable by group or others") {
		t.Fatalf("insecure state directory error=%v", err)
	}
}

func TestControllerStateLockRejectsSymlinkWithoutChangingTarget(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("do not touch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "controller.json.lock")); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireControllerStateLock(filepath.Join(dir, "controller.json")); err == nil {
		t.Fatal("controller state lock followed a symlink")
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("symlink target mode changed to %o", info.Mode().Perm())
	}
}

func TestControllerStateLockRejectsBroadExistingFileWithoutChmod(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(dir, "controller.json.lock")
	if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := acquireControllerStateLock(filepath.Join(dir, "controller.json"))
	if err == nil || !strings.Contains(err.Error(), "accessible by group or others") {
		t.Fatalf("broad lock error=%v", err)
	}
	info, statErr := os.Stat(lockPath)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("existing lock mode changed to %o", info.Mode().Perm())
	}
}

func TestControllerStateDirectoryRequiresCurrentUserOwnership(t *testing.T) {
	stat := unix.Stat_t{Mode: unix.S_IFDIR | 0o700, Uid: uint32(os.Geteuid() + 1)}
	if err := validateControllerStateDirectoryStat(&stat); err == nil || !strings.Contains(err.Error(), "current user") {
		t.Fatalf("foreign-owned directory error=%v", err)
	}
}
