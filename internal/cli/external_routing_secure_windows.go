//go:build windows

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

func openExternalRoutingFile(path string) (*os.File, string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, "", fmt.Errorf("resolve external routing path: %w", err)
	}
	absPath = filepath.Clean(absPath)
	volume := filepath.VolumeName(absPath)
	remainder := strings.TrimPrefix(absPath, volume)
	remainder = strings.TrimLeft(remainder, `\/`)
	components := strings.FieldsFunc(remainder, func(r rune) bool { return r == '\\' || r == '/' })
	if len(components) == 0 {
		return nil, "", fmt.Errorf("external routing path must name a file")
	}

	// Windows has no public openat equivalent. Keep every no-follow ancestor
	// handle open without FILE_SHARE_DELETE until the final file is open; this
	// prevents any validated component from being renamed out from under us.
	directories := make([]windows.Handle, 0, len(components)-1)
	defer func() {
		for _, handle := range directories {
			_ = windows.CloseHandle(handle)
		}
	}()
	prefix := volume + string(os.PathSeparator)
	for index, component := range components[:len(components)-1] {
		prefix = filepath.Join(prefix, component)
		handle, openErr := openExternalRoutingWindowsHandle(prefix, true)
		if openErr != nil {
			return nil, "", fmt.Errorf("open external routing directory %q without reparse points: %w", component, openErr)
		}
		if validateErr := validateExternalRoutingWindowsHandle(handle, true, index == len(components)-2); validateErr != nil {
			_ = windows.CloseHandle(handle)
			return nil, "", fmt.Errorf("external routing directory %q: %w", component, validateErr)
		}
		directories = append(directories, handle)
	}

	handle, err := openExternalRoutingWindowsHandle(absPath, false)
	if err != nil {
		return nil, "", fmt.Errorf("open external routing file without reparse points: %w", err)
	}
	if err := validateExternalRoutingWindowsHandle(handle, false, true); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, "", err
	}
	file := os.NewFile(uintptr(handle), absPath)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, "", fmt.Errorf("create external routing file handle")
	}
	return file, absPath, nil
}

func openExternalRoutingWindowsHandle(path string, directory bool) (windows.Handle, error) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, err
	}
	flags := uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if directory {
		flags |= windows.FILE_FLAG_BACKUP_SEMANTICS
	}
	return windows.CreateFile(
		pathPtr,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		flags,
		0,
	)
}

func validateExternalRoutingWindowsHandle(handle windows.Handle, directory, requireCurrentOwner bool) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return fmt.Errorf("inspect external routing path: %w", err)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("must not be a reparse point")
	}
	isDirectory := info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	if directory != isDirectory {
		if directory {
			return fmt.Errorf("must be a directory")
		}
		return fmt.Errorf("external routing file must be a regular file")
	}
	if !requireCurrentOwner {
		return nil
	}
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("inspect external routing owner: %w", err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return fmt.Errorf("read external routing owner: %w", err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("read current routing user: %w", err)
	}
	if owner == nil || user == nil || user.User.Sid == nil || !owner.Equals(user.User.Sid) {
		return fmt.Errorf("must be owned by the current user")
	}
	if err := validateExternalRoutingWindowsPrivateDACL(descriptor, user.User.Sid); err != nil {
		return err
	}
	return nil
}

func validateExternalRoutingWindowsPrivateDACL(descriptor *windows.SECURITY_DESCRIPTOR, currentUser *windows.SID) error {
	if descriptor == nil || currentUser == nil {
		return fmt.Errorf("must have a private access-control list")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil {
		return fmt.Errorf("must have a private access-control list")
	}
	for index := uint16(0); index < dacl.AceCount; index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(index), &ace); err != nil {
			return fmt.Errorf("inspect external routing access-control entry: %w", err)
		}
		if ace == nil {
			return fmt.Errorf("must have an inspectable private access-control list")
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE:
		default:
			// Object/callback allow ACEs have different SID layouts. Refuse
			// them instead of accidentally treating an unparsed grant as safe.
			return fmt.Errorf("must not contain unsupported access-control grants")
		}
		if ace.Mask == 0 {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		allowed := sid.Equals(currentUser) ||
			sid.IsWellKnown(windows.WinLocalSystemSid) ||
			sid.IsWellKnown(windows.WinBuiltinAdministratorsSid)
		creatorOwnerInheritance := sid.IsWellKnown(windows.WinCreatorOwnerSid) && ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0
		if !allowed && !creatorOwnerInheritance {
			return fmt.Errorf("must not grant access to users other than the owner, SYSTEM, or Administrators")
		}
	}
	return nil
}
