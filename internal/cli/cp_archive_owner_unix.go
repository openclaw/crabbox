//go:build !windows

package cli

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

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

func copyArchiveProcessIsAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
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
