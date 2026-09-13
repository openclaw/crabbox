//go:build windows

package cli

import (
	"os"

	"golang.org/x/sys/windows"
)

func localHistoryCreateDirectory(root *os.Root, name string) (bool, error) {
	descriptor, user, err := privateWindowsSecurityDescriptor(true)
	if err != nil {
		return false, err
	}
	parent, err := root.Open(".")
	if err != nil {
		return false, err
	}
	defer parent.Close()
	// Existing directories are opened for validation only, never re-owned or secured.
	handle, created, err := openOrCreatePrivateWindowsDirectoryAt(windows.Handle(parent.Fd()), name, descriptor, false)
	if err != nil {
		return false, err
	}
	defer windows.CloseHandle(handle)
	if err := verifyPrivateWindowsHandle(handle, true, user); err != nil {
		return created, err
	}
	return created, nil
}

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
