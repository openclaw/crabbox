//go:build windows

package cli

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

type failureBundleDirectory struct{ handles []windows.Handle }

func prepareFailureBundleDir(path string) (*failureBundleOutput, error) {
	handles, err := openWindowsRunOutputDirectory(path, true)
	if err != nil {
		return nil, err
	}
	return &failureBundleOutput{directory: failureBundleDirectory{handles: handles}}, nil
}

func openOrCreateWindowsFailureDirectoryAt(parent windows.Handle, name string, descriptor *windows.SECURITY_DESCRIPTOR) (windows.Handle, bool, error) {
	const access = windows.FILE_TRAVERSE | windows.FILE_READ_ATTRIBUTES | windows.READ_CONTROL
	handle, err := openWindowsFailureBundleAt(parent, name, access, windows.FILE_DIRECTORY_FILE, windows.FILE_OPEN, nil)
	if !errors.Is(err, windows.STATUS_OBJECT_NAME_NOT_FOUND) {
		// Failure to inspect an existing directory is not a creation denial.
		return handle, false, err
	}
	handle, err = openWindowsFailureBundleAt(parent, name, access, windows.FILE_DIRECTORY_FILE, windows.FILE_CREATE, descriptor)
	if errors.Is(err, windows.STATUS_OBJECT_NAME_COLLISION) {
		handle, err = openWindowsFailureBundleAt(parent, name, access, windows.FILE_DIRECTORY_FILE, windows.FILE_OPEN, nil)
		return handle, false, err
	}
	if err != nil {
		return windows.InvalidHandle, false, privateRunOutputWriteError{err}
	}
	return handle, true, nil
}

func prepareWindowsFailureBundleDirectory(parent windows.Handle, name string, handle windows.Handle, user *windows.SID) error {
	var volumeFlags uint32
	if err := windows.GetVolumeInformationByHandle(handle, nil, 0, nil, nil, &volumeFlags, nil, 0); err != nil {
		return err
	}
	if volumeFlags&windows.FILE_READ_ONLY_VOLUME != 0 {
		return privateRunOutputWriteError{windows.ERROR_WRITE_PROTECT}
	}
	// FILE_ADD_FILE (0x2) checks the existing DACL without creating a name.
	// The retained handles deny deletion, binding both opens to the same object.
	// ReOpenFile requires a CreateFile handle; traversal uses NtCreateFile.
	const fileAddFile = 0x2
	probe, err := openWindowsFailureBundleAt(parent, name, fileAddFile|windows.FILE_TRAVERSE, windows.FILE_DIRECTORY_FILE, windows.FILE_OPEN, nil)
	if err != nil {
		return privateRunOutputWriteError{err}
	}
	if err := windows.CloseHandle(probe); err != nil {
		return err
	}
	security, err := openWindowsFailureBundleAt(parent, name, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.WRITE_DAC, windows.FILE_DIRECTORY_FILE, windows.FILE_OPEN, nil)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(security)
	acl, err := privateWindowsACL(user, true)
	if err != nil {
		return err
	}
	if err := windows.SetSecurityInfo(security, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		return err
	}
	return verifyPrivateWindowsHandle(security, true, user)
}

// All names are single components beneath a retained, non-reparse directory.
// No backup intent: access probes must enforce the caller's existing rights.
func openWindowsFailureBundleAt(parent windows.Handle, name string, access, options, disposition uint32, descriptor *windows.SECURITY_DESCRIPTOR) (windows.Handle, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return windows.InvalidHandle, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		Length:             uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory:      parent,
		ObjectName:         objectName,
		Attributes:         windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
		SecurityDescriptor: descriptor,
	}
	var handle windows.Handle
	err = windows.NtCreateFile(&handle, access|windows.SYNCHRONIZE, attributes, &windows.IO_STATUS_BLOCK{}, nil,
		windows.FILE_ATTRIBUTE_NORMAL, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, disposition,
		options|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	if err != nil {
		return windows.InvalidHandle, err
	}
	return handle, nil
}

func (o *failureBundleOutput) directoryHandle() windows.Handle {
	return o.directory.handles[len(o.directory.handles)-1]
}

func (o *failureBundleOutput) createFile(name string) (*os.File, error) {
	descriptor, _, err := privateWindowsSecurityDescriptor(false)
	if err != nil {
		return nil, err
	}
	handle, err := openWindowsFailureBundleAt(o.directoryHandle(), name,
		windows.GENERIC_WRITE|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.DELETE,
		windows.FILE_NON_DIRECTORY_FILE, windows.FILE_CREATE, descriptor)
	if errors.Is(err, windows.STATUS_OBJECT_NAME_COLLISION) {
		return nil, os.ErrExist
	}
	if err != nil {
		return nil, privateRunOutputWriteError{err}
	}
	return os.NewFile(uintptr(handle), name), nil
}

func (o *failureBundleOutput) secureFile() error {
	user, err := currentWindowsUserSID()
	if err != nil {
		return err
	}
	return verifyPrivateWindowsHandle(windows.Handle(o.file.Fd()), false, user)
}

func (o *failureBundleOutput) validateDestination(name string) error {
	handle, err := openWindowsFailureBundleAt(o.directoryHandle(), name, windows.FILE_READ_ATTRIBUTES, 0, windows.FILE_OPEN, nil)
	if errors.Is(err, windows.STATUS_OBJECT_NAME_NOT_FOUND) {
		return nil
	}
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	if err := validatePrivateWindowsFileType(handle, false); err != nil {
		return fmt.Errorf("unsafe failure bundle destination %s: %w", name, err)
	}
	return nil
}

func (o *failureBundleOutput) rename(name string) error {
	encoded, err := windows.UTF16FromString(name)
	if err != nil {
		return err
	}
	// NT FILE_RENAME_INFORMATION resolves the destination relative to the
	// directory handle; the original open file, never a source name, is renamed.
	type renameInfo struct {
		ReplaceIfExists uint32
		RootDirectory   windows.Handle
		FileNameLength  uint32
		FileName        [1]uint16
	}
	size := int(unsafe.Offsetof(renameInfo{}.FileName)) + len(encoded)*2
	buffer := make([]byte, size)
	info := (*renameInfo)(unsafe.Pointer(&buffer[0]))
	info.ReplaceIfExists = 1
	info.RootDirectory = o.directoryHandle()
	info.FileNameLength = uint32((len(encoded) - 1) * 2)
	copy(unsafe.Slice(&info.FileName[0], len(encoded)), encoded)
	return windows.NtSetInformationFile(windows.Handle(o.file.Fd()), &windows.IO_STATUS_BLOCK{}, &buffer[0], uint32(size), windows.FileRenameInformation)
}

func (o *failureBundleOutput) remove() error {
	// Delete the owned file before closing it, without reopening a source name.
	deleteFile := byte(1)
	return windows.NtSetInformationFile(windows.Handle(o.file.Fd()), &windows.IO_STATUS_BLOCK{}, &deleteFile, 1, windows.FileDispositionInformation)
}

func (o *failureBundleOutput) closeDirectory() error {
	return closeWindowsRunOutputDirectories(o.directory.handles)
}
