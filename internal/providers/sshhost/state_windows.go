//go:build windows

package sshhost

import (
	"errors"
	"fmt"

	"github.com/openclaw/crabbox/internal/privatefile"
	"golang.org/x/sys/windows"
)

func secureStateDir(path string) error {
	security, user, err := privatefile.WindowsAttributes(true)
	if err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	if err = windows.CreateDirectory(name, security); err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return err
	}
	h, err := windows.CreateFile(name, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return privatefile.VerifyWindowsHandle(h, true, user)
}
func lockMachine() (func(), error) {
	name, _ := windows.UTF16PtrFromString(`Global\CrabboxManagedHost`)
	h, err := windows.CreateMutex(nil, false, name)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		if h != 0 {
			windows.CloseHandle(h)
		}
		return nil, fmt.Errorf("another managed host agent owns this machine")
	}
	if err != nil {
		return nil, err
	}
	return func() { windows.CloseHandle(h) }, nil
}
