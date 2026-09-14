//go:build !windows

package cli

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func gitOverlayTemporaryDirectoryMode(finalMode os.FileMode) os.FileMode {
	return finalMode | 0o700
}

func syncGitOverlaySymlinkTimes(path string, modTime time.Time) error {
	value := unix.NsecToTimeval(modTime.UnixNano())
	return unix.Lutimes(path, []unix.Timeval{value, value})
}

func thawGitOverlaySnapshotFiles(*os.Root) error {
	return nil
}

func openGitOverlaySnapshotParent(path string) (*os.File, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("overlay snapshot parent is not a real directory")
	}
	handle, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	opened, err := handle.Stat()
	if err != nil {
		_ = handle.Close()
		return nil, err
	}
	after, err := os.Lstat(path)
	if err != nil {
		_ = handle.Close()
		return nil, err
	}
	if !opened.IsDir() ||
		!after.IsDir() ||
		after.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(before, opened) ||
		!os.SameFile(opened, after) {
		_ = handle.Close()
		return nil, fmt.Errorf("overlay snapshot parent changed while opening")
	}
	return handle, nil
}
