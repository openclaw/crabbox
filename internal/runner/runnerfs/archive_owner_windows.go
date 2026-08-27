//go:build windows

package runnerfs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func lockArchiveFile(_ string) (func(), bool, error) {
	return nil, false, errors.New("archive publication requires a POSIX host")
}

func copyArchiveDirectoryIdentity(name string, _ os.FileInfo) string {
	return strings.ToLower(filepath.Clean(name))
}

// Publication stays unavailable until Windows marker ownership is verified.
func copyArchiveMarkerIsPrivate(info os.FileInfo) bool {
	return false
}

func archiveHasHardLinks(_ os.FileInfo) bool { return true }

func copyArchiveProcessIsAlive(_ int) bool { return true }

func copyArchiveProcessIdentity(_ int) (string, bool) {
	return "", false
}
