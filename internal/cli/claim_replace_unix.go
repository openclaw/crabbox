//go:build !windows

package cli

import "os"

func replaceClaimFile(tmpPath, path string) error {
	return os.Rename(tmpPath, path)
}
