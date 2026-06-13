//go:build !windows

package cli

import "os"

func replaceControllerFile(tmpPath, path string) error {
	return os.Rename(tmpPath, path)
}

func removeControllerFile(path string) error {
	return os.Remove(path)
}
