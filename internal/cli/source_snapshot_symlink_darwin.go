package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func syncSourceSnapshotSymlinkMode(root *os.Root, relative string, observed os.FileInfo, mode os.FileMode) (result error) {
	parent, err := root.OpenFile(filepath.Dir(relative), os.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, parent.Close()) }()
	// Darwin symlinks have mutable permissions. Pin the link itself: following
	// chmod would change its target, and a replacement FIFO must not block open.
	fd, err := unix.Openat(int(parent.Fd()), filepath.Base(relative), unix.O_SYMLINK|unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	link := os.NewFile(uintptr(fd), relative)
	defer func() { result = errors.Join(result, link.Close()) }()
	if err := verifySourceSnapshotSymlink(root, relative, link, observed, 0, false); err != nil {
		return err
	}
	if err := link.Chmod(mode); err != nil {
		return err
	}
	return verifySourceSnapshotSymlink(root, relative, link, observed, mode, true)
}

func verifySourceSnapshotSymlink(root *os.Root, relative string, link *os.File, observed os.FileInfo, mode os.FileMode, checkMode bool) error {
	opened, err := link.Stat()
	if err != nil {
		return err
	}
	current, err := root.Lstat(relative)
	if err != nil {
		return err
	}
	if observed.Mode()&os.ModeSymlink == 0 || opened.Mode()&os.ModeSymlink == 0 || current.Mode()&os.ModeSymlink == 0 || !os.SameFile(observed, opened) || !os.SameFile(opened, current) {
		return fmt.Errorf("overlay snapshot symlink identity changed")
	}
	if checkMode && (sourceSnapshotSupportedMode(opened.Mode()) != mode || sourceSnapshotSupportedMode(current.Mode()) != mode) {
		return fmt.Errorf("overlay snapshot symlink mode changed")
	}
	return nil
}
