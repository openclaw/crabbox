//go:build !windows

package cli

import "os"

func fileSnapshotPathInfo(path string) (os.FileInfo, error) {
	return os.Lstat(path)
}

func fileSnapshotHandleInfo(file *os.File) (os.FileInfo, error) {
	return file.Stat()
}

func sameFileSnapshotIdentity(left, right os.FileInfo) bool {
	return os.SameFile(left, right)
}
