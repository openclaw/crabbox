//go:build !windows

package cli

import (
	"fmt"
	"os"
)

func secureSSHTransportPath(path string, directory bool) error {
	mode := os.FileMode(0o600)
	if directory {
		mode = 0o700
	}
	if err := os.Chmod(path, mode); err != nil {
		return err
	}
	return verifySSHTransportPathPrivate(path, directory)
}

func verifySSHTransportPathPrivate(path string, directory bool) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	want := os.FileMode(0o600)
	if directory {
		want = 0o700
	}
	if info.Mode().Perm() != want {
		return fmt.Errorf("permissions=%o want %o", info.Mode().Perm(), want)
	}
	return nil
}
