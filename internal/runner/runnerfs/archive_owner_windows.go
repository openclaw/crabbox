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

// Native Windows publication is unsupported.
func archivePrivateFile(info os.FileInfo) bool {
	return false
}

func archiveHasHardLinks(_ os.FileInfo) bool { return true }

func archiveLinkCount(_ os.FileInfo) (uint64, bool) { return 0, false }

func archivePrivateDirectory(_ os.FileInfo) bool { return false }

func archiveTrustedDirectory(_ os.FileInfo) bool { return false }
