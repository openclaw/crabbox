//go:build !windows

package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

func ensurePrivateRunOutputDir(path string) error {
	if err := createPrivateRunOutputDir(path); err != nil {
		return err
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := setPrivateFDPermissions(uintptr(fd), privateRunOutputDirMode); err != nil {
		return err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if os.FileMode(stat.Mode).Perm() != privateRunOutputDirMode {
		return fmt.Errorf("private output directory mode is %#o, want %#o", os.FileMode(stat.Mode).Perm(), privateRunOutputDirMode)
	}
	return nil
}

func createPrivateRunOutputDir(path string) error {
	return os.MkdirAll(path, privateRunOutputDirMode)
}

func openPrivateRunOutputFile(path string) (*os.File, error) {
	file, tempPath, err := createPrivateRunOutputTemp(path)
	if err != nil {
		return nil, err
	}
	if err := os.Rename(tempPath, path); err != nil {
		_ = file.Close()
		_ = os.Remove(tempPath)
		return nil, err
	}
	return file, nil
}

func writePrivateRunOutputFile(path string, data []byte) error {
	file, tempPath, err := createPrivateRunOutputTemp(path)
	if err != nil {
		return err
	}
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(tempPath)
	}
	if _, err := io.Copy(file, bytes.NewReader(data)); err != nil {
		cleanup()
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	return nil
}

func createPrivateRunOutputTemp(path string) (*os.File, string, error) {
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, "."+filepath.Base(path)+".crabbox-*")
	if err != nil {
		return nil, "", err
	}
	tempPath := file.Name()
	if err := securePrivateFile(file); err != nil {
		_ = file.Close()
		_ = os.Remove(tempPath)
		return nil, "", err
	}
	return file, tempPath, nil
}

func securePrivateFile(file *os.File) error {
	if file == nil {
		return fmt.Errorf("private output file handle is unavailable")
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("private output must be a regular file")
	}
	if err := setPrivateFDPermissions(file.Fd(), privateRunOutputFileMode); err != nil {
		return err
	}
	info, err = file.Stat()
	if err != nil {
		return err
	}
	if got := info.Mode().Perm(); got != privateRunOutputFileMode {
		return fmt.Errorf("private output mode is %#o, want %#o", got, privateRunOutputFileMode)
	}
	return nil
}

func openExistingPrivateRunOutputFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open private output file handle")
	}
	if err := securePrivateFile(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func checkPrivateRunOutputReplaceable(label, path string) error {
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return exit(2, "%s: %v", label, err)
	}
	fileInfo, err := os.Lstat(path)
	if err != nil {
		return exit(2, "%s: %v", label, err)
	}
	dirStat, dirOK := dirInfo.Sys().(*syscall.Stat_t)
	fileStat, fileOK := fileInfo.Sys().(*syscall.Stat_t)
	if !dirOK || !fileOK {
		return nil
	}
	if !stickyRunOutputReplaceAllowed(uint32(os.Geteuid()), fileStat.Uid, dirStat.Uid, dirInfo.Mode()&os.ModeSticky != 0) {
		return exit(2, "%s: cannot replace %s in sticky directory owned by another user", label, path)
	}
	return nil
}

func stickyRunOutputReplaceAllowed(euid, fileUID, dirUID uint32, sticky bool) bool {
	return !sticky || euid == 0 || euid == fileUID || euid == dirUID
}
