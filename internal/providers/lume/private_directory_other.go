//go:build !windows

package lume

import (
	"fmt"
	"os"
)

func secureLumePrivateDirectory(path string) error {
	return os.Chmod(path, 0o700)
}

func verifyLumePrivateDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("mode is %v, want drwx------", info.Mode())
	}
	return nil
}
