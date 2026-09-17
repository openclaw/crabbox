//go:build !windows

package runnerfs

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// Use the same advisory lock primitive and path as the former local flock
// implementation, so different CLI versions still serialize publication.
func lockArchiveFile(name string) (func(), bool, error) {
	file, err := os.OpenFile(name, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, false, err
	}
	info, err := file.Stat()
	if err != nil || !archivePrivateFile(info) {
		_ = file.Close()
		return nil, false, errors.New("copy transaction lock is not private")
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN); _ = file.Close() }, true, nil
}

func copyArchiveDirectoryIdentity(name string, info os.FileInfo) string {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return name
	}
	return fmt.Sprintf("%d:%d", stat.Dev, stat.Ino)
}

func archivePrivateFile(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0o077 == 0 && ok && int(stat.Uid) == os.Geteuid()
}

func archiveHasHardLinks(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return !ok || stat.Nlink > 1
}

func archivePrivateDirectory(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return info.IsDir() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0o077 == 0 && ok && int(stat.Uid) == os.Geteuid()
}

func archiveTrustedDirectory(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return info.IsDir() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0o022 == 0 && ok && int(stat.Uid) == os.Geteuid()
}
