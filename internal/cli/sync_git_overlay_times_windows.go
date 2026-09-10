//go:build windows

package cli

import (
	"os"
	"time"

	"golang.org/x/sys/windows"
)

func syncGitOverlayFileTimes(file *os.File, modTime time.Time) error {
	value := windows.NsecToFiletime(modTime.UnixNano())
	return windows.SetFileTime(windows.Handle(file.Fd()), nil, &value, &value)
}

func normalizedGitOverlayFileTime(value time.Time) time.Time {
	filetime := windows.NsecToFiletime(value.UnixNano())
	return time.Unix(0, filetime.Nanoseconds()).In(value.Location())
}
