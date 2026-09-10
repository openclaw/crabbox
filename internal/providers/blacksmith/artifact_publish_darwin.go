//go:build darwin

package blacksmith

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// The caller supplies a completed regular temporary file beside its destination.
// The boolean reports a successful move, so cleanup never unlinks its former name.
func publishBlacksmithArtifactFile(source, destination string) (bool, error) {
	err := unix.RenamexNp(source, destination, unix.RENAME_EXCL)
	if err == nil {
		return true, nil
	}
	// Darwin rename(2) reports unsupported filesystem flags as ENOTSUP.
	if errors.Is(err, unix.ENOTSUP) {
		return false, os.Link(source, destination)
	}
	return false, &os.LinkError{Op: "rename-noreplace", Old: source, New: destination, Err: err}
}
