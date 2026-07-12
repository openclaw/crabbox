//go:build !windows

package cli

import (
	"os"

	"golang.org/x/sys/unix"
)

func openArtifactReadOnly(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK, 0)
}

func openArtifactRootReadOnly(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|unix.O_NONBLOCK, 0)
}
