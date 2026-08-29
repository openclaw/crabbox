package cli

import (
	"fmt"
	"os"
	"path/filepath"
)

// Publish the complete record atomically and sync its namespace before remote effects.
func writeStateFileAtomic(path string, data []byte, syncDirectory func(string) error) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := replaceClaimFile(tmpPath, path); err != nil {
		return err
	}
	removeTemp = false
	if err := syncDirectory(dir); err != nil {
		return fmt.Errorf("sync state directory %s: %w", dir, err)
	}
	return nil
}
