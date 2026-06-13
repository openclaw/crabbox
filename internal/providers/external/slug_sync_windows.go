//go:build windows

package external

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func syncSlugReservationDirectory(path string) error {
	// Windows has no portable directory-fsync equivalent. Reservation namespace
	// mutations below use MOVEFILE_WRITE_THROUGH instead.
	return nil
}

func installSlugReservationFile(tmp, path string, syncDirectory func(string) error) error {
	from, err := windows.UTF16PtrFromString(tmp)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return fmt.Errorf("install external slug reservation: %w", err)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync external slug reservation directory: %w", err)
	}
	return nil
}

func removeSlugReservationFile(path string) error {
	// A deterministic tombstone lets a later retry finish a deletion that was
	// interrupted after the write-through rename.
	tombstone := path + ".deleted"
	if err := os.Remove(tombstone); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	from, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(tombstone)
	if err != nil {
		return err
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_WRITE_THROUGH); err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil
		}
		return err
	}
	if err := os.Remove(tombstone); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
