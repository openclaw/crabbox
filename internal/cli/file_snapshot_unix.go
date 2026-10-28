//go:build !windows

package cli

import "os"

func fileSnapshotPathInfo(path string) (os.FileInfo, error) {
	return os.Lstat(path)
}

func sameFileSnapshotIdentity(left, right os.FileInfo) bool {
	return os.SameFile(left, right)
}
