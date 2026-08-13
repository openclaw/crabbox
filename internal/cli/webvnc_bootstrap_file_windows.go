//go:build windows

package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

const webVNCPortalBootstrapTempAttempts = 32

func createWebVNCPortalBootstrapFile() (string, string, *os.File, error) {
	directorySecurity, userSID, err := privateWindowsSecurityAttributes(true)
	if err != nil {
		return "", "", nil, err
	}
	fileSecurity, _, err := privateWindowsSecurityAttributes(false)
	if err != nil {
		return "", "", nil, err
	}
	for attempt := 0; attempt < webVNCPortalBootstrapTempAttempts; attempt++ {
		suffix, err := randomHex(16)
		if err != nil {
			return "", "", nil, err
		}
		dir := filepath.Join(os.TempDir(), "crabbox-webvnc-bootstrap-"+suffix)
		dirUTF16, err := windows.UTF16PtrFromString(dir)
		if err != nil {
			return "", "", nil, err
		}
		if err := windows.CreateDirectory(dirUTF16, directorySecurity); err != nil {
			if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS) {
				continue
			}
			return "", "", nil, err
		}
		cleanup := func() {
			_ = os.RemoveAll(dir)
		}
		if err := validateWebVNCPortalBootstrapWindowsPath(dir, userSID); err != nil {
			cleanup()
			return "", "", nil, fmt.Errorf("secure WebVNC bootstrap directory: %w", err)
		}

		path := filepath.Join(dir, "open.html")
		pathUTF16, err := windows.UTF16PtrFromString(path)
		if err != nil {
			cleanup()
			return "", "", nil, err
		}
		handle, err := windows.CreateFile(
			pathUTF16,
			windows.GENERIC_WRITE|windows.READ_CONTROL,
			windows.FILE_SHARE_READ,
			fileSecurity,
			windows.CREATE_NEW,
			windows.FILE_ATTRIBUTE_TEMPORARY,
			0,
		)
		if err != nil {
			cleanup()
			return "", "", nil, err
		}
		file := os.NewFile(uintptr(handle), path)
		if file == nil {
			_ = windows.CloseHandle(handle)
			cleanup()
			return "", "", nil, fmt.Errorf("create WebVNC bootstrap file handle")
		}
		if err := validateWebVNCPortalBootstrapWindowsPath(path, userSID); err != nil {
			_ = file.Close()
			cleanup()
			return "", "", nil, fmt.Errorf("secure WebVNC bootstrap file: %w", err)
		}
		return dir, path, file, nil
	}
	return "", "", nil, fmt.Errorf("allocate private WebVNC bootstrap directory")
}

func validateWebVNCPortalBootstrapWindowsPath(path string, currentUser *windows.SID) error {
	descriptor, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return err
	}
	return validateCurrentUserPrivateWindowsDescriptor(descriptor, currentUser)
}
