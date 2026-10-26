//go:build !darwin

package cli

import "os"

func syncSourceSnapshotSymlinkMode(*os.Root, string, os.FileInfo, os.FileMode) error {
	// Other hosts retain their platform's symlink permissions; the shared
	// copier still verifies them against the source before accepting metadata.
	return nil
}
