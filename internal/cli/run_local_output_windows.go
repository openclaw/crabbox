//go:build windows

package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const privateRunOutputTempAttempts = 32

var (
	movePrivateRunOutputFileExW = windows.NewLazySystemDLL("kernel32.dll").NewProc("MoveFileExW")
	reopenPrivateRunOutputFile  = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReOpenFile")
)

func createPrivateRunOutputDir(path string) error {
	handles, err := openWindowsRunOutputDirectory(path, false)
	return errors.Join(err, closeWindowsRunOutputDirectories(handles))
}

func closeWindowsRunOutputDirectories(handles []windows.Handle) error {
	var err error
	for index := len(handles) - 1; index >= 0; index-- {
		err = errors.Join(err, windows.CloseHandle(handles[index]))
	}
	return err
}

func openWindowsRunOutputDirectory(path string, preserveUnwritable bool) ([]windows.Handle, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	absPath = filepath.Clean(absPath)
	volume := filepath.VolumeName(absPath)
	components := strings.FieldsFunc(strings.TrimPrefix(absPath, volume), func(r rune) bool {
		return r == '\\' || r == '/'
	})
	if volume == "" || len(components) == 0 {
		return nil, fmt.Errorf("private output directory must be below a Windows volume root")
	}
	rootPath := volume + string(os.PathSeparator)
	rootPathPtr, err := windows.UTF16PtrFromString(rootPath)
	if err != nil {
		return nil, err
	}
	rootHandle, err := windows.CreateFile(
		rootPathPtr,
		windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return nil, err
	}
	handles := []windows.Handle{rootHandle}
	ready := false
	defer func() {
		if !ready {
			_ = closeWindowsRunOutputDirectories(handles)
		}
	}()
	descriptor, user, err := privateWindowsSecurityDescriptor(true)
	if err != nil {
		return nil, err
	}
	parent := rootHandle
	for index, component := range components {
		final := index == len(components)-1
		var handle windows.Handle
		var created bool
		if preserveUnwritable {
			handle, created, err = openOrCreateWindowsFailureDirectoryAt(parent, component, descriptor)
		} else {
			handle, created, err = openOrCreatePrivateWindowsDirectoryAt(parent, component, descriptor, final)
			if err != nil {
				err = privateRunOutputWriteError{err}
			}
		}
		if err != nil {
			return nil, fmt.Errorf("open private output directory component %q: %w", component, err)
		}
		handles = append(handles, handle)
		if err := validatePrivateWindowsFileType(handle, true); err != nil {
			return nil, err
		}
		if created {
			if err := verifyPrivateWindowsHandle(handle, true, user); err != nil {
				return nil, err
			}
		} else if final && !preserveUnwritable {
			if err := securePrivateWindowsHandle(handle, true); err != nil {
				return nil, err
			}
		} else if final {
			descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
			if err != nil {
				return nil, err
			}
			owner, _, err := descriptor.Owner()
			if err != nil {
				return nil, err
			}
			if owner == nil || !owner.Equals(user) {
				return nil, fmt.Errorf("failure bundle directory must be owned by the current user")
			}
			if err := prepareWindowsFailureBundleDirectory(parent, component, handle, user); err != nil {
				return nil, err
			}
		}
		parent = handle
	}
	ready = true
	return handles, nil
}

func ensurePrivateRunOutputDir(path string) error {
	return createPrivateRunOutputDir(path)
}

func privateRunOutputPermissionError(err error) bool {
	return errors.Is(err, os.ErrPermission) || errors.Is(err, windows.ERROR_WRITE_PROTECT) ||
		errors.Is(err, windows.STATUS_ACCESS_DENIED) || errors.Is(err, windows.STATUS_MEDIA_WRITE_PROTECTED)
}

func openOrCreatePrivateWindowsDirectoryAt(parent windows.Handle, name string, descriptor *windows.SECURITY_DESCRIPTOR, secureExisting bool) (windows.Handle, bool, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return windows.InvalidHandle, false, err
	}
	objectAttributes := &windows.OBJECT_ATTRIBUTES{
		Length:             uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory:      parent,
		ObjectName:         objectName,
		Attributes:         windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
		SecurityDescriptor: descriptor,
	}
	status := &windows.IO_STATUS_BLOCK{}
	var handle windows.Handle
	access := uint32(windows.FILE_TRAVERSE | windows.FILE_READ_ATTRIBUTES | windows.READ_CONTROL | windows.SYNCHRONIZE)
	if secureExisting {
		access |= windows.WRITE_DAC
	}
	err = windows.NtCreateFile(
		&handle,
		access,
		objectAttributes,
		status,
		nil,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		windows.FILE_OPEN_IF,
		windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0,
		0,
	)
	if err != nil {
		return windows.InvalidHandle, false, err
	}
	const fileCreated = 2
	return handle, status.Information == fileCreated, nil
}

func openPrivateRunOutputFile(path string) (*os.File, error) {
	file, tempPath, err := createPrivateRunOutputTemp(path)
	if err != nil {
		return nil, err
	}
	if err := replacePrivateRunOutputTemp(tempPath, path); err != nil {
		_ = file.Close()
		_ = os.Remove(tempPath)
		return nil, err
	}
	return file, nil
}

func writePrivateRunOutputFile(path string, data []byte) error {
	file, tempPath, err := createPrivateRunOutputTemp(path)
	if err != nil {
		return err
	}
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(tempPath)
	}
	if _, err := io.Copy(file, bytes.NewReader(data)); err != nil {
		cleanup()
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	if err := replacePrivateRunOutputTemp(tempPath, path); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	return nil
}

func createPrivateRunOutputTemp(path string) (*os.File, string, error) {
	security, user, err := privateWindowsSecurityAttributes(false)
	if err != nil {
		return nil, "", err
	}
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	access := uint32(windows.GENERIC_WRITE | windows.FILE_READ_ATTRIBUTES | windows.READ_CONTROL)
	share := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE)
	for attempt := 0; attempt < privateRunOutputTempAttempts; attempt++ {
		token, err := randomHex(12)
		if err != nil {
			return nil, "", err
		}
		tempPath := filepath.Join(dir, "."+base+".crabbox-"+token)
		tempPathPtr, err := windows.UTF16PtrFromString(tempPath)
		if err != nil {
			return nil, "", err
		}
		handle, err := windows.CreateFile(
			tempPathPtr,
			access,
			share,
			security,
			windows.CREATE_NEW,
			windows.FILE_ATTRIBUTE_NORMAL,
			0,
		)
		if err != nil {
			if err == windows.ERROR_FILE_EXISTS || err == windows.ERROR_ALREADY_EXISTS {
				continue
			}
			return nil, "", privateRunOutputWriteError{err}
		}
		if err := verifyPrivateWindowsHandle(handle, false, user); err != nil {
			_ = windows.CloseHandle(handle)
			_ = os.Remove(tempPath)
			return nil, "", err
		}
		file := os.NewFile(uintptr(handle), tempPath)
		if file == nil {
			_ = windows.CloseHandle(handle)
			_ = os.Remove(tempPath)
			return nil, "", fmt.Errorf("create private output file handle")
		}
		return file, tempPath, nil
	}
	return nil, "", fmt.Errorf("allocate private output temporary file")
}

func securePrivateFile(file *os.File) error {
	if file == nil {
		return fmt.Errorf("private output file handle is unavailable")
	}
	handle, err := reOpenPrivateWindowsHandle(windows.Handle(file.Fd()), windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.WRITE_DAC, false)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	return securePrivateWindowsHandle(handle, false)
}

func openExistingPrivateRunOutputFile(path string) (*os.File, error) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		pathPtr,
		windows.GENERIC_READ|windows.READ_CONTROL|windows.WRITE_DAC,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, err
	}
	if err := securePrivateWindowsHandle(handle, false); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("open private output file handle")
	}
	return file, nil
}

func securePrivateWindowsPath(path string, directory bool) error {
	handle, err := openPrivateWindowsSecurityHandle(path, directory, windows.READ_CONTROL|windows.WRITE_DAC)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	return securePrivateWindowsHandle(handle, directory)
}

func verifyPrivateWindowsPath(path string, directory bool) error {
	handle, err := openPrivateWindowsSecurityHandle(path, directory, windows.READ_CONTROL)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	user, err := currentWindowsUserSID()
	if err != nil {
		return err
	}
	return verifyPrivateWindowsHandle(handle, directory, user)
}

func openPrivateWindowsSecurityHandle(path string, directory bool, access uint32) (windows.Handle, error) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, err
	}
	flags := uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if directory {
		flags |= windows.FILE_FLAG_BACKUP_SEMANTICS
	}
	handle, err := windows.CreateFile(
		pathPtr,
		access|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		flags,
		0,
	)
	if err != nil {
		return windows.InvalidHandle, err
	}
	return handle, nil
}

func securePrivateWindowsHandle(handle windows.Handle, directory bool) error {
	if err := validatePrivateWindowsFileType(handle, directory); err != nil {
		return err
	}
	user, err := currentWindowsUserSID()
	if err != nil {
		return err
	}
	acl, err := privateWindowsACL(user, directory)
	if err != nil {
		return fmt.Errorf("build private Windows access-control list: %w", err)
	}
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("inspect private Windows output owner: %w", err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return fmt.Errorf("read private Windows output owner: %w", err)
	}
	// WRITE_DAC does not imply WRITE_OWNER. Apply the private grant through
	// the retained handle before requesting ownership access to the same object.
	if err := windows.SetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		acl,
		nil,
	); err != nil {
		return fmt.Errorf("apply private Windows access-control list: %w", err)
	}
	if owner == nil || !owner.Equals(user) {
		securityHandle, err := reOpenPrivateWindowsHandle(handle, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.WRITE_OWNER, directory)
		if err != nil {
			return err
		}
		defer windows.CloseHandle(securityHandle)
		if err := windows.SetSecurityInfo(securityHandle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, user, nil, nil, nil); err != nil {
			return fmt.Errorf("apply private Windows output owner: %w", err)
		}
	}
	return verifyPrivateWindowsHandle(handle, directory, user)
}

func verifyPrivateWindowsHandle(handle windows.Handle, directory bool, currentUser *windows.SID) error {
	if err := validatePrivateWindowsFileType(handle, directory); err != nil {
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
	return validateCurrentUserPrivateWindowsDescriptor(descriptor, currentUser)
}

func validatePrivateWindowsFileType(handle windows.Handle, directory bool) error {
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

func currentWindowsUserSID() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("read current Windows user: %w", err)
	}
	if user == nil || user.User.Sid == nil {
		return nil, fmt.Errorf("read current Windows user: missing SID")
	}
	return user.User.Sid, nil
}

func privateWindowsACL(user *windows.SID, directory bool) (*windows.ACL, error) {
	var pinner runtime.Pinner
	pinner.Pin(user)
	defer pinner.Unpin()
	inheritance := uint32(0)
	if directory {
		inheritance = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE
	}
	return windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       inheritance,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user),
		},
	}}, nil)
}

func privateWindowsSecurityDescriptor(directory bool) (*windows.SECURITY_DESCRIPTOR, *windows.SID, error) {
	user, err := currentWindowsUserSID()
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

func privateWindowsSecurityAttributes(directory bool) (*windows.SecurityAttributes, *windows.SID, error) {
	descriptor, user, err := privateWindowsSecurityDescriptor(directory)
	if err != nil {
		return nil, nil, err
	}
	attributes := &windows.SecurityAttributes{SecurityDescriptor: descriptor}
	attributes.Length = uint32(unsafe.Sizeof(*attributes))
	return attributes, user, nil
}

func createPrivateWindowsFileAt(parent windows.Handle, name string) (*os.File, error) {
	descriptor, user, err := privateWindowsSecurityDescriptor(false)
	if err != nil {
		return nil, err
	}
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return nil, err
	}
	objectAttributes := &windows.OBJECT_ATTRIBUTES{
		Length:             uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory:      parent,
		ObjectName:         objectName,
		Attributes:         windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
		SecurityDescriptor: descriptor,
	}
	var handle windows.Handle
	err = windows.NtCreateFile(
		&handle,
		windows.GENERIC_WRITE|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.SYNCHRONIZE,
		objectAttributes,
		&windows.IO_STATUS_BLOCK{},
		nil,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_CREATE,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0,
		0,
	)
	if err != nil {
		return nil, err
	}
	if err := verifyPrivateWindowsHandle(handle, false, user); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	file := os.NewFile(uintptr(handle), name)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("create private Windows file handle")
	}
	return file, nil
}

func validateCurrentUserPrivateWindowsDescriptor(descriptor *windows.SECURITY_DESCRIPTOR, currentUser *windows.SID) error {
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

func reOpenPrivateWindowsHandle(handle windows.Handle, access uint32, directory bool) (windows.Handle, error) {
	flags := uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if directory {
		flags |= windows.FILE_FLAG_BACKUP_SEMANTICS
	}
	result, _, callErr := reopenPrivateRunOutputFile.Call(
		uintptr(handle),
		uintptr(access),
		uintptr(windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE),
		uintptr(flags),
	)
	reopened := windows.Handle(result)
	if reopened != windows.InvalidHandle {
		return reopened, nil
	}
	if callErr != nil && callErr != syscall.Errno(0) {
		return windows.InvalidHandle, fmt.Errorf("reopen private Windows output handle: %w", callErr)
	}
	return windows.InvalidHandle, fmt.Errorf("reopen private Windows output handle")
}

func replacePrivateRunOutputTemp(tempPath, path string) error {
	tempPathPtr, err := windows.UTF16PtrFromString(tempPath)
	if err != nil {
		return err
	}
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	const (
		moveFileReplaceExisting = 0x1
		moveFileWriteThrough    = 0x8
	)
	result, _, callErr := movePrivateRunOutputFileExW.Call(
		uintptr(unsafe.Pointer(tempPathPtr)),
		uintptr(unsafe.Pointer(pathPtr)),
		uintptr(moveFileReplaceExisting|moveFileWriteThrough),
	)
	if result != 0 {
		return nil
	}
	if callErr != nil && callErr != syscall.Errno(0) {
		return callErr
	}
	return fmt.Errorf("replace private Windows output")
}

func checkPrivateRunOutputReplaceable(_, _ string) error {
	return nil
}
