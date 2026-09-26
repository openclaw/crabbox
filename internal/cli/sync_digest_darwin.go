package cli

import (
	"os"
	"syscall"
)

func syncDigestChangeTime(info os.FileInfo) ([2]int64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return [2]int64{}, false
	}
	return [2]int64{stat.Ctimespec.Sec, stat.Ctimespec.Nsec}, true
}
