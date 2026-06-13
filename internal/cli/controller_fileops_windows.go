//go:build windows

package cli

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func replaceControllerFile(tmpPath, path string) error {
	from, err := windows.UTF16PtrFromString(tmpPath)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

func removeControllerFile(path string) error {
	// A deterministic tombstone lets the next retry finish deletion after a
	// crash or sharing violation between the write-through rename and remove.
	tombstone := path + ".deleted"
	recovered := false
	if err := os.Remove(tombstone); err == nil {
		recovered = true
	} else if !errors.Is(err, os.ErrNotExist) {
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
			if recovered {
				return nil
			}
			return os.ErrNotExist
		}
		return err
	}
	if err := os.Remove(tombstone); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
