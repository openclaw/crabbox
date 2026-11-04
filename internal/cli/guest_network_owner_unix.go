//go:build !windows

package cli

import (
	"os"
	"syscall"
)

func guestPolicyFileOwnerTrusted(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && (stat.Uid == 0 || int(stat.Uid) == os.Geteuid())
}
