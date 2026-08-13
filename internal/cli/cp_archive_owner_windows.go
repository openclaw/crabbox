//go:build windows

package cli

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func copyArchiveDirectoryIdentity(name string, _ os.FileInfo) string {
	return strings.ToLower(filepath.Clean(name))
}

// Windows user profiles and the lock directory carry the current user's ACL.
// Opening the private marker and its state successfully is the ownership proof.
func copyArchiveMarkerIsPrivate(info os.FileInfo) bool {
	return false
}

func copyArchiveProcessIsAlive(pid int) bool {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)
	var code uint32
	return windows.GetExitCodeProcess(handle, &code) == nil && code == 259
}

func copyArchiveProcessIdentity(_ int) (string, bool) {
	return "", false
}
