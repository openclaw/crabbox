//go:build linux

package blacksmith

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// The caller supplies a completed regular temporary file beside its destination.
// The boolean reports a successful move, so cleanup never unlinks its former name.
func publishBlacksmithArtifactFile(source, destination string) (bool, error) {
	err := unix.Renameat2(unix.AT_FDCWD, source, unix.AT_FDCWD, destination, unix.RENAME_NOREPLACE)
	if err == nil {
		return true, nil
	}
	// renameat2 appeared in Linux 3.15; filesystem support arrived separately.
	// With fixed flags and same-directory regular files, rename(2)'s EINVAL
	// denotes unsupported flags, not its directory-cycle/flag-combination cases.
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) {
		return false, os.Link(source, destination)
	}
	return false, &os.LinkError{Op: "rename-noreplace", Old: source, New: destination, Err: err}
}
