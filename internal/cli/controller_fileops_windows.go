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

func replaceControllerFile(tmpPath, path string) error {
	return renameWindowsFile(tmpPath, path, windows.FILE_RENAME_REPLACE_IF_EXISTS|windows.FILE_RENAME_POSIX_SEMANTICS)
}

func openWindowsNamespaceFile(path string) (windows.Handle, error) {
	openPath, err := artifactWindowsLongPath(path)
	if err != nil {
		return windows.InvalidHandle, err
	}
	name, err := windows.UTF16PtrFromString(openPath)
	if err != nil {
		return windows.InvalidHandle, err
	}
	// Mutate the link entry, as path-based rename/delete did, not its referent.
	return windows.CreateFile(name, windows.DELETE|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_WRITE_THROUGH, 0)
}

func renameWindowsFile(from, to string, flags uint32) (err error) {
	// Use a full destination path with a null root handle for FileRenameInfoEx.
	fullPath := to
	if !filepath.IsAbs(fullPath) {
		fullPath, err = filepath.Abs(fullPath)
		if err != nil {
			return err
		}
	}
	fullPath, err = artifactWindowsLongPath(fullPath)
	if err != nil {
		return err
	}
	name, err := windows.UTF16FromString(fullPath)
	if err != nil {
		return err
	}
	// The HANDLE determines alignment; FileNameLength counts UTF-16 bytes,
	// excluding the terminator, and FileName starts before any tail padding.
	type renameInfo struct {
		Flags          uint32
		RootDirectory  windows.Handle
		FileNameLength uint32
		FileName       [1]uint16
	}
	var layout renameInfo
	buffer := make([]byte, int(unsafe.Sizeof(layout))+2*(len(name)-1))
	info := (*renameInfo)(unsafe.Pointer(&buffer[0]))
	info.Flags = flags
	info.FileNameLength = uint32(2 * (len(name) - 1))
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(&buffer[unsafe.Offsetof(layout.FileName)])), len(name)), name)
	handle, err := openWindowsNamespaceFile(from)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, windowsNamespaceOperationError("close namespace file", windows.CloseHandle(handle)))
	}()
	// POSIX replacement keeps old snapshots readable while new opens see the
	// replacement. Requires Windows 10 1709+; never fall back to MoveFileEx.
	// https://learn.microsoft.com/en-us/windows-hardware/drivers/ddi/ntifs/ns-ntifs-_file_rename_information
	if err := windows.SetFileInformationByHandle(handle, windows.FileRenameInfoEx, &buffer[0], uint32(len(buffer))); err != nil {
		return windowsNamespaceOperationError("rename namespace file", err)
	}
	// Callers sync staged data before publication. WRITE_THROUGH covers NTFS
	// rename metadata; flush the renamed handle too. Failure is postpublication:
	// preserve the destination and propagate it without replaying any action.
	// https://learn.microsoft.com/en-us/windows/win32/fileio/file-caching
	return windowsNamespaceOperationError("flush renamed file "+to, windows.FlushFileBuffers(handle))
}

func removeWindowsTombstone(path string) (err error) {
	handle, err := openWindowsNamespaceFile(path)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, windowsNamespaceOperationError("close tombstone", windows.CloseHandle(handle)))
	}()
	// Also flush on recovery: a previous rename may have published the tombstone
	// but failed its flush. A crash during deletion must leave it recoverable.
	if err := windows.FlushFileBuffers(handle); err != nil {
		return windowsNamespaceOperationError("flush tombstone "+path, err)
	}
	// POSIX delete frees the name when this handle closes, even while snapshot
	// readers survive. DeleteFile would leave .deleted occupied until they close.
	// https://learn.microsoft.com/en-us/windows-hardware/drivers/ddi/ntddk/ns-ntddk-_file_disposition_information_ex
	flags := uint32(windows.FILE_DISPOSITION_DELETE | windows.FILE_DISPOSITION_POSIX_SEMANTICS)
	return windowsNamespaceOperationError("delete tombstone "+path,
		windows.SetFileInformationByHandle(handle, windows.FileDispositionInfoEx, (*byte)(unsafe.Pointer(&flags)), uint32(unsafe.Sizeof(flags))))
}

func windowsNamespaceOperationError(operation string, err error) error {
	if err == nil {
		return nil
	}
	// Only a missing open is absence. Cleanup callers must not swallow later
	// rename, flush, disposition or close failures through IsNotExist.
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s failed after opening file: %v", operation, err)
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func removeControllerFile(path string) error {
	// A deterministic tombstone lets the next retry finish deletion after a
	// crash or sharing violation between the write-through rename and remove.
	tombstone := path + ".deleted"
	recovered := false
	if err := removeWindowsTombstone(tombstone); err == nil {
		recovered = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Do not replace an unexpected tombstone; recovery above owns its removal.
	if err := renameWindowsFile(path, tombstone, 0); err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			if recovered {
				return nil
			}
			return os.ErrNotExist
		}
		return err
	}
	if err := removeWindowsTombstone(tombstone); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
