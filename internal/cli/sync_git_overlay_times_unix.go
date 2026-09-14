//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package cli

import (
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func syncSourceSnapshotFileTimes(file *os.File, modTime time.Time) error {
	value := unix.NsecToTimeval(modTime.UnixNano())
	return unix.Futimes(int(file.Fd()), []unix.Timeval{value, value})
}

func normalizedSourceSnapshotFileTime(value time.Time) time.Time {
	timeval := unix.NsecToTimeval(value.UnixNano())
	return time.Unix(0, unix.TimevalToNsec(timeval)).In(value.Location())
}
