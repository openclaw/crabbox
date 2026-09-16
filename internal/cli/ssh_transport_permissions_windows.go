//go:build windows

package cli

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func openSelectedLeaseSSHWriteFile(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	security, user, err := privateWindowsSecurityAttributes(false)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_WRITE|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, security, windows.OPEN_ALWAYS,
		windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	// OPEN_ALWAYS does not truncate. Existing files must already be private;
	// new files receive the protected current-user descriptor at creation.
	if err := verifyPrivateWindowsHandle(handle, false, user); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("open selected lease SSH file handle")
	}
	return file, nil
}

func openCreatedLeaseSSHFile(path string) (*os.File, error) {
	handle, err := openPrivateWindowsSecurityHandle(path, false, windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL|windows.WRITE_DAC)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("open created lease SSH file handle")
	}
	return file, nil
}

func verifySelectedLeaseSSHRoot(path string) error {
	handle, err := openPrivateWindowsSecurityHandle(path, true, windows.READ_CONTROL)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	if err := validatePrivateWindowsFileType(handle, true); err != nil {
		return err
	}
	user, err := currentWindowsUserSID()
	if err != nil {
		return err
	}
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	return validateSelectedLeaseSSHRootDescriptor(descriptor, user)
}

func validateSelectedLeaseSSHRootDescriptor(descriptor *windows.SECURITY_DESCRIPTOR, user *windows.SID) error {
	if descriptor == nil || user == nil {
		return fmt.Errorf("selected lease SSH root requires a current-user security descriptor")
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil || !owner.Equals(user) {
		return fmt.Errorf("selected lease SSH root must be owned by the current user")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil {
		return fmt.Errorf("selected lease SSH root requires a bounded access-control list")
	}
	const readAccess = windows.GENERIC_READ | windows.GENERIC_EXECUTE | windows.FILE_GENERIC_READ | windows.FILE_GENERIC_EXECUTE
	for index := uint16(0); index < dacl.AceCount; index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(index), &ace); err != nil || ace == nil {
			return fmt.Errorf("selected lease SSH root requires inspectable access-control entries")
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE:
		default:
			return fmt.Errorf("selected lease SSH root contains unsupported access-control grants")
		}
		// These grants do not apply to the base. Generated descendants use
		// protected current-user-only ACLs, rather than inheriting this ACL.
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 || ace.Mask == 0 {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if sid.Equals(user) || sid.IsWellKnown(windows.WinLocalSystemSid) || sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
			continue
		}
		if ace.Mask & ^windows.ACCESS_MASK(readAccess) != 0 {
			return fmt.Errorf("selected lease SSH root grants mutation access to an unrelated principal")
		}
	}
	return nil
}

func verifySelectedLeaseSSHOwnership(path string, directory bool) error {
	handle, err := openPrivateWindowsSecurityHandle(path, directory, windows.READ_CONTROL)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	if err := validatePrivateWindowsFileType(handle, directory); err != nil {
		return err
	}
	user, err := currentWindowsUserSID()
	if err != nil {
		return err
	}
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil || !owner.Equals(user) {
		return fmt.Errorf("generated lease SSH path must be owned by the current user")
	}
	return nil
}

func createPrivateSSHTransportDirectory(path string) error {
	return createPrivateRunOutputDir(path)
}

func secureSSHTransportPath(path string, directory bool) error {
	return securePrivateWindowsPath(path, directory)
}

func verifySSHTransportPathPrivate(path string, directory bool) error {
	return verifyPrivateWindowsPath(path, directory)
}
