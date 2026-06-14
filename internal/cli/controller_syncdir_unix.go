//go:build !windows

package cli

import (
	"fmt"
	"os"
)

func syncControllerDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("close directory: %w", err)
	}
	return nil
}
