//go:build windows

package cli

import (
	"os"

	"github.com/openclaw/crabbox/internal/privatefile"
	"golang.org/x/sys/windows"
)

func localHistoryCreateDirectory(root *os.Root, name string) (bool, error) {
	descriptor, user, err := privatefile.WindowsDescriptor(true)
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
	if err := privatefile.VerifyWindowsHandle(handle, true, user); err != nil {
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
	user, err := privatefile.WindowsUserSID()
	if err != nil {
		return err
	}
	return privatefile.VerifyWindowsHandle(windows.Handle(file.Fd()), directory, user)
}
func localHistorySync(root *os.Root) error { return syncControllerDirectory(root.Name()) }
