//go:build windows

package cli

func syncControllerDirectory(string) error {
	// Windows has no portable directory-fsync equivalent. Controller namespace
	// mutations flush write-through replacement or tombstone handles instead.
	return nil
}
