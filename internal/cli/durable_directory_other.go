//go:build !windows

package cli

import "os"

func makePrivateDurableDirectories(path string) error {
	return os.MkdirAll(path, 0o700)
}
