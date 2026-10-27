//go:build windows

package cli

import (
	"os"

	"github.com/openclaw/crabbox/internal/privatefile"
	"golang.org/x/sys/windows"
)

func publishPendingFinish(temp, path string) error {
	fromPath, err := privatefile.WindowsPath(temp)
	if err != nil {
		return err
	}
	toPath, err := privatefile.WindowsPath(path)
	if err != nil {
		return err
	}
	from, err := windows.UTF16PtrFromString(fromPath)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(toPath)
	if err != nil {
		return err
	}
	// No REPLACE_EXISTING: the original is immutable. Flush namespace publication
	// before allowing a terminal request to leave this process.
	return windows.MoveFileEx(from, to, windows.MOVEFILE_WRITE_THROUGH)
}

func openPendingFinishFile(path string) (*os.File, error) {
	name, err := privatefile.WindowsPath(path)
	if err != nil {
		return nil, err
	}
	ptr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(ptr, windows.GENERIC_READ|windows.READ_CONTROL, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	user, err := privatefile.WindowsUserSID()
	if err == nil {
		err = privatefile.VerifyWindowsHandle(handle, false, user)
	}
	if err != nil {
		windows.CloseHandle(handle)
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}
