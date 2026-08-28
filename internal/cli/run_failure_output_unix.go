//go:build !windows

package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type failureBundleDirectory struct{ fd int }

func prepareFailureBundleDir(path string) (*failureBundleOutput, error) {
	parentPath := filepath.Dir(path)
	if err := createPrivateRunOutputDir(parentPath); err != nil {
		return nil, err
	}
	const flags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	parent, err := unix.Open(parentPath, flags, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(parent)
	name := filepath.Base(path)
	fd, err := unix.Openat(parent, name, flags, 0)
	if errors.Is(err, unix.ENOENT) {
		if err := unix.Mkdirat(parent, name, privateRunOutputDirMode); err != nil && !errors.Is(err, unix.EEXIST) {
			return nil, privateRunOutputWriteError{err}
		}
		fd, err = unix.Openat(parent, name, flags, 0)
	}
	if err != nil {
		return nil, err
	}
	o := &failureBundleOutput{directory: failureBundleDirectory{fd: fd}}
	ready := false
	defer func() {
		if !ready {
			_ = o.Close()
		}
	}()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, err
	}
	if err := validateFailureBundleDirectoryOwner(stat.Uid, uint32(os.Geteuid())); err != nil {
		return nil, err
	}
	// Check existing access before granting any rights or exposing a temp name.
	if err := unix.Faccessat(fd, ".", unix.W_OK|unix.X_OK, unix.AT_EACCESS); err != nil {
		return nil, privateRunOutputWriteError{err}
	}
	if err := securePrivateRunOutputDirFD(fd); err != nil {
		return nil, err
	}
	ready = true
	return o, nil
}

func validateFailureBundleDirectoryOwner(owner, current uint32) error {
	if owner != current {
		return fmt.Errorf("failure bundle directory must be owned by the current user")
	}
	return nil
}

func (o *failureBundleOutput) createFile(name string) (*os.File, error) {
	fd, err := unix.Openat(o.directory.fd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, privateRunOutputFileMode)
	if err != nil {
		return nil, privateRunOutputWriteError{err}
	}
	return os.NewFile(uintptr(fd), name), nil
}

func (o *failureBundleOutput) secureFile() error { return securePrivateFile(o.file) }

func (o *failureBundleOutput) validateDestination(name string) error {
	var stat unix.Stat_t
	err := unix.Fstatat(o.directory.fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("unsafe failure bundle destination %s", name)
	}
	return nil
}

func (o *failureBundleOutput) rename(name string) error {
	return unix.Renameat(o.directory.fd, o.name, o.directory.fd, name)
}

func (o *failureBundleOutput) remove() error {
	err := unix.Unlinkat(o.directory.fd, o.name, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	return err
}

func (o *failureBundleOutput) closeDirectory() error { return unix.Close(o.directory.fd) }
