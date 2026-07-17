//go:build windows

package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

const webVNCPortalBootstrapTempAttempts = 32

func createWebVNCPortalBootstrapFile() (string, string, *os.File, error) {
	security, userSID, err := webVNCPortalBootstrapSecurity()
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
		if err := windows.CreateDirectory(dirUTF16, security); err != nil {
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
			security,
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

func webVNCPortalBootstrapSecurity() (*windows.SecurityAttributes, *windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, nil, err
	}
	if user == nil || user.User.Sid == nil {
		return nil, nil, fmt.Errorf("current Windows user SID is unavailable")
	}
	sid := user.User.Sid.String()
	sd, err := windows.SecurityDescriptorFromString("O:" + sid + "D:P(A;;GA;;;" + sid + ")")
	if err != nil {
		return nil, nil, err
	}
	attributes := &windows.SecurityAttributes{SecurityDescriptor: sd}
	attributes.Length = uint32(unsafe.Sizeof(*attributes))
	return attributes, user.User.Sid, nil
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
	owner, _, err := descriptor.Owner()
	if err != nil {
		return err
	}
	if owner == nil || currentUser == nil || !owner.Equals(currentUser) {
		return fmt.Errorf("must be owned by the current user")
	}
	control, _, err := descriptor.Control()
	if err != nil {
		return err
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return fmt.Errorf("must have a protected access-control list")
	}
	return validateExternalRoutingWindowsPrivateDACL(descriptor, currentUser)
}
