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

	"github.com/openclaw/crabbox/internal/privatefile"
	"golang.org/x/sys/windows"
)

var movePrivateRunOutputFileExW = windows.NewLazySystemDLL("kernel32.dll").NewProc("MoveFileExW")

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
	descriptor, user, err := privatefile.WindowsDescriptor(true)
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
		if err := privatefile.ValidateWindowsFileType(handle, true); err != nil {
			return nil, err
		}
		if created {
			if err := privatefile.VerifyWindowsHandle(handle, true, user); err != nil {
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
	file, err := privatefile.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".crabbox-*")
	if err != nil {
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			return nil, "", privateRunOutputWriteError{err}
		}
		return nil, "", err
	}
	return file, file.Name(), nil
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
	user, err := privatefile.WindowsUserSID()
	if err != nil {
		return err
	}
	return privatefile.VerifyWindowsHandle(handle, directory, user)
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
	if err := privatefile.ValidateWindowsFileType(handle, directory); err != nil {
		return err
	}
	user, err := privatefile.WindowsUserSID()
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
	return privatefile.VerifyWindowsHandle(handle, directory, user)
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

func createPrivateWindowsFileAt(parent windows.Handle, name string) (*os.File, error) {
	descriptor, user, err := privatefile.WindowsDescriptor(false)
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
	if err := privatefile.VerifyWindowsHandle(handle, false, user); err != nil {
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

func reOpenPrivateWindowsHandle(handle windows.Handle, access uint32, directory bool) (windows.Handle, error) {
	// An empty relative name reopens the retained object without resolving a path.
	// ReOpenFile can deny directory handles even when the DACL permits the access.
	name, err := windows.NewNTUnicodeString("")
	if err != nil {
		return windows.InvalidHandle, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: handle,
		ObjectName:    name,
		Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	fileType := uint32(windows.FILE_NON_DIRECTORY_FILE)
	if directory {
		fileType = windows.FILE_DIRECTORY_FILE
	}
	var reopened windows.Handle
	err = windows.NtCreateFile(&reopened, access, attributes, &windows.IO_STATUS_BLOCK{}, nil,
		windows.FILE_ATTRIBUTE_NORMAL, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN, fileType|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT, 0, 0)
	if err != nil {
		// NtCreateFile returns NTStatus; retain the callers' Win32 error matching.
		return windows.InvalidHandle, fmt.Errorf("reopen private Windows output handle: %w", err.(windows.NTStatus).Errno())
	}
	return reopened, nil
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
