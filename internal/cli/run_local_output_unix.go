//go:build !windows

package cli

import (
	"bytes"
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
	return unix.Fchmod(fd, privateRunOutputDirMode)
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
	if err := file.Chmod(privateRunOutputFileMode); err != nil {
		_ = file.Close()
		_ = os.Remove(tempPath)
		return nil, "", err
	}
	return file, tempPath, nil
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
