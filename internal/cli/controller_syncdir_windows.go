//go:build windows

package cli

func syncControllerDirectory(string) error {
	// Windows has no portable directory-fsync equivalent. Controller namespace
	// mutations use write-through replacement or write-through tombstones.
	return nil
}
