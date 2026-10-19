//go:build windows

package cli

import (
	"os"

	"github.com/openclaw/crabbox/internal/privatefile"
	"golang.org/x/sys/windows"
)

func openArtifactReadOnly(path string) (*os.File, error) {
	openPath, err := privatefile.WindowsPath(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	pathUTF16, err := windows.UTF16PtrFromString(openPath)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	handle, err := windows.CreateFile(
		pathUTF16,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}

func openArtifactRootReadOnly(root *os.Root, name string) (*os.File, error) {
	return root.Open(name)
}
