//go:build !windows

package cli

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
)

func localHistoryOpen(root *os.Root, name string, writable bool) (*os.File, error) {
	flags := os.O_RDONLY
	if writable {
		flags = os.O_RDWR
	}
	return root.OpenFile(name, flags|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
}
func localHistoryPrivate(file *os.File, directory bool) error {
	var s unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &s); err != nil {
		return err
	}
	mode := uint32(unix.S_IFREG)
	perm := uint32(0o600)
	if directory {
		mode = unix.S_IFDIR
		perm = 0o700
	}
	if uint32(s.Mode)&unix.S_IFMT != mode || uint32(s.Mode)&0o777 != perm || s.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("local history requires current-user private files and directories")
	}
	if !directory && s.Nlink != 1 {
		return fmt.Errorf("local history file must have a single link")
	}
	return nil
}
func localHistorySync(root *os.Root) error {
	file, err := root.Open(".")
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}
