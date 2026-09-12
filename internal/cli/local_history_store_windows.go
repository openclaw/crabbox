//go:build windows

package cli

import (
	"golang.org/x/sys/windows"
	"os"
)

func localHistoryOpen(root *os.Root, name string, writable bool) (*os.File, error) {
	flags := os.O_RDONLY
	if writable {
		flags = os.O_RDWR
	}
	return root.OpenFile(name, flags, 0)
}
func localHistoryPrivate(file *os.File, directory bool) error {
	user, err := currentWindowsUserSID()
	if err != nil {
		return err
	}
	return verifyPrivateWindowsHandle(windows.Handle(file.Fd()), directory, user)
}
func localHistorySync(root *os.Root) error { return syncControllerDirectory(root.Name()) }
