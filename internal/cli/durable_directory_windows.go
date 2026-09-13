package cli

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func makePrivateDurableDirectories(path string) error {
	if info, err := os.Stat(path); err == nil {
		if info.IsDir() {
			return nil
		}
		return &os.PathError{Op: "mkdir", Path: path, Err: windows.ERROR_ALREADY_EXISTS}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	} else if filepath.Dir(path) == path {
		return err
	}
	if err := makePrivateDurableDirectories(filepath.Dir(path)); err != nil {
		return err
	}
	security, _, err := privateWindowsSecurityAttributes(true)
	if err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	// Claims and keys share the state namespace. Establish ownership when
	// creating it; never repair ownership or permissions of existing parents.
	if err := windows.CreateDirectory(name, security); err != nil {
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			if info, statErr := os.Stat(path); statErr == nil && info.IsDir() {
				return nil
			}
		}
		return err
	}
	return nil
}
