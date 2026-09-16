//go:build !windows

package cli

import (
	"fmt"
	"os"
	"syscall"
)

func verifySelectedLeaseSSHOwnership(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode()&os.ModeSymlink != 0 ||
		(directory && !info.IsDir()) || (!directory && !info.Mode().IsRegular()) {
		return fmt.Errorf("generated lease SSH path must be an ordinary current-user-owned path")
	}
	return nil
}

func verifySelectedLeaseSSHRoot(path string) error {
	if err := verifySelectedLeaseSSHOwnership(path, true); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("selected lease SSH root must not be writable by group or others")
	}
	return nil
}

func openSelectedLeaseSSHWriteFile(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err == nil {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Geteuid()) || !info.Mode().IsRegular() {
			err = fmt.Errorf("generated lease SSH file must be regular and owned by the current user")
		}
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func openCreatedLeaseSSHFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}

func createPrivateSSHTransportDirectory(path string) error {
	return os.Mkdir(path, 0o700)
}

func secureSSHTransportPath(path string, directory bool) error {
	mode := os.FileMode(0o600)
	if directory {
		mode = 0o700
	}
	if err := os.Chmod(path, mode); err != nil {
		return err
	}
	return verifySSHTransportPathPrivate(path, directory)
}

func verifySSHTransportPathPrivate(path string, directory bool) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	want := os.FileMode(0o600)
	if directory {
		want = 0o700
	}
	if info.Mode().Perm() != want {
		return fmt.Errorf("permissions=%o want %o", info.Mode().Perm(), want)
	}
	return nil
}
