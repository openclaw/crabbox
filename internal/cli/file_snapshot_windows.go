//go:build windows

package cli

import (
	"errors"
	"os"
	"unsafe"

	"github.com/openclaw/crabbox/internal/privatefile"
	"golang.org/x/sys/windows"
)

type windowsFullFileID struct {
	Volume uint64
	ID     [16]byte
}

type windowsSnapshotInfo struct {
	os.FileInfo
	identity windowsFullFileID
}

func fileSnapshotHandleInfo(file *os.File) (os.FileInfo, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	var identity windowsFullFileID
	if err := windows.GetFileInformationByHandleEx(windows.Handle(file.Fd()), windows.FileIdInfo,
		(*byte)(unsafe.Pointer(&identity)), uint32(unsafe.Sizeof(identity))); err != nil {
		return nil, err
	}
	return windowsSnapshotInfo{FileInfo: info, identity: identity}, nil
}

func fileSnapshotPathInfo(path string) (_ os.FileInfo, resultErr error) {
	name, err := privatefile.WindowsPath(path)
	if err != nil {
		return nil, err
	}
	pointer, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(pointer, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, &os.PathError{Op: "inspect file identity", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(handle), path)
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	return fileSnapshotHandleInfo(file)
}

func sameFileSnapshotIdentity(left, right os.FileInfo) bool {
	a, aOK := left.(windowsSnapshotInfo)
	b, bOK := right.(windowsSnapshotInfo)
	// ReFS requires the full 128-bit FileIdInfo identity. Mixing Go directory
	// enumeration IDs with legacy handle IDs can reject the same file.
	return aOK && bOK && a.identity == b.identity
}
