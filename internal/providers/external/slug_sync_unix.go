//go:build !windows

package external

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func syncSlugReservationDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func installSlugReservationFile(tmp, path string, syncDirectory func(string) error) error {
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("install external slug reservation: %w", err)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync external slug reservation directory: %w", err)
	}
	return nil
}

func removeSlugReservationFile(path string) error {
	return removeSlugReservationFileWithSync(path, syncSlugReservationDirectory)
}

func removeSlugReservationFileWithSync(path string, syncDirectory func(string) error) error {
	if err := os.Remove(path); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("sync removed external slug reservation directory: %w", err)
	}
	return nil
}
