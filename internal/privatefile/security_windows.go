//go:build windows

package privatefile

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// VerifyWindowsHandle checks the type, owner, and protected DACL of the retained object.
func VerifyWindowsHandle(handle windows.Handle, directory bool, currentUser *windows.SID) error {
	if err := ValidateWindowsFileType(handle, directory); err != nil {
		return err
	}
	descriptor, err := windows.GetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return fmt.Errorf("inspect private Windows access-control list: %w", err)
	}
	return ValidateWindowsDescriptor(descriptor, currentUser)
}

// ValidateWindowsFileType rejects reparse points and unexpected object types.
func ValidateWindowsFileType(handle windows.Handle, directory bool) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return fmt.Errorf("inspect private Windows output: %w", err)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("private Windows output must not be a reparse point")
	}
	isDirectory := info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	if isDirectory != directory {
		if directory {
			return fmt.Errorf("private Windows output must be a directory")
		}
		return fmt.Errorf("private Windows output must be a regular file")
	}
	return nil
}

// WindowsUserSID returns the user of the current process token.
func WindowsUserSID() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("read current Windows user: %w", err)
	}
	if user == nil || user.User.Sid == nil {
		return nil, fmt.Errorf("read current Windows user: missing SID")
	}
	return user.User.Sid, nil
}

// WindowsDescriptor grants only the current user access and disables inheritance
// from the parent. Directories propagate that grant to their children.
func WindowsDescriptor(directory bool) (*windows.SECURITY_DESCRIPTOR, *windows.SID, error) {
	user, err := WindowsUserSID()
	if err != nil {
		return nil, nil, err
	}
	inheritance := ""
	if directory {
		inheritance = "OICI"
	}
	descriptor, err := windows.SecurityDescriptorFromString("O:" + user.String() + "D:P(A;" + inheritance + ";GA;;;" + user.String() + ")")
	if err != nil {
		return nil, nil, fmt.Errorf("build private Windows security descriptor: %w", err)
	}
	return descriptor, user, nil
}

// WindowsAttributes prepares the private descriptor for a Windows creation call.
func WindowsAttributes(directory bool) (*windows.SecurityAttributes, *windows.SID, error) {
	descriptor, user, err := WindowsDescriptor(directory)
	if err != nil {
		return nil, nil, err
	}
	attributes := &windows.SecurityAttributes{SecurityDescriptor: descriptor}
	attributes.Length = uint32(unsafe.Sizeof(*attributes))
	return attributes, user, nil
}

// ValidateWindowsDescriptor rejects ownership or access grants outside currentUser.
func ValidateWindowsDescriptor(descriptor *windows.SECURITY_DESCRIPTOR, currentUser *windows.SID) error {
	if descriptor == nil || currentUser == nil {
		return fmt.Errorf("private Windows output must have a current-user security descriptor")
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return fmt.Errorf("read private Windows output owner: %w", err)
	}
	if owner == nil || !owner.Equals(currentUser) {
		return fmt.Errorf("private Windows output must be owned by the current user")
	}
	control, _, err := descriptor.Control()
	if err != nil {
		return fmt.Errorf("read private Windows output control flags: %w", err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return fmt.Errorf("private Windows output must have a protected access-control list")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil || dacl.AceCount == 0 {
		return fmt.Errorf("private Windows output must grant access only to the current user")
	}
	foundCurrentUserGrant := false
	for index := uint16(0); index < dacl.AceCount; index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(index), &ace); err != nil {
			return fmt.Errorf("inspect private Windows access-control entry: %w", err)
		}
		if ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return fmt.Errorf("private Windows output contains an unsupported access-control entry")
		}
		if ace.Mask == 0 {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.Equals(currentUser) {
			return fmt.Errorf("private Windows output grants access to an unrelated principal")
		}
		foundCurrentUserGrant = true
	}
	if !foundCurrentUserGrant {
		return fmt.Errorf("private Windows output must grant access to the current user")
	}
	return nil
}
