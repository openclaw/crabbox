//go:build !windows

package runnerfs

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
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
	if err != nil || !copyArchiveMarkerIsPrivate(info) {
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

func copyArchiveMarkerIsPrivate(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0o077 == 0 && ok && int(stat.Uid) == os.Geteuid()
}

func archiveHasHardLinks(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return !ok || stat.Nlink > 1
}

func copyArchiveProcessIsAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func copyArchiveProcessIdentity(pid int) (string, bool) {
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return "", false
		}
		closing := strings.LastIndexByte(string(data), ')')
		if closing < 0 {
			return "", false
		}
		fields := strings.Fields(string(data)[closing+1:])
		if len(fields) <= 19 {
			return "", false
		}
		return fields[19], true
	}
	output, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	identity := strings.TrimSpace(string(output))
	return identity, err == nil && identity != ""
}
