//go:build windows

package privatefile

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// CreateTemp creates a file with a protected current-user DACL and verifies the
// original handle before returning it for writing. The caller owns its cleanup.
func CreateTemp(dir, pattern string) (*os.File, error) {
	if strings.ContainsAny(pattern, `/\`) {
		return nil, &os.PathError{Op: "createtemp", Path: pattern, Err: errors.New("pattern contains path separator")}
	}
	if dir == "" {
		dir = os.TempDir()
	}
	prefix, suffix := pattern, ""
	if index := strings.LastIndexByte(pattern, '*'); index >= 0 {
		prefix, suffix = pattern[:index], pattern[index+1:]
	}
	security, user, err := WindowsAttributes(false)
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 32; attempt++ {
		path := filepath.Join(dir, prefix+rand.Text()+suffix)
		openPath, err := WindowsPath(path)
		if err != nil {
			return nil, err
		}
		pathPtr, err := windows.UTF16PtrFromString(openPath)
		if err != nil {
			return nil, err
		}
		handle, err := windows.CreateFile(pathPtr,
			windows.GENERIC_WRITE|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			security, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if err != nil {
			if errors.Is(err, windows.ERROR_FILE_EXISTS) || errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
				continue
			}
			return nil, &os.PathError{Op: "createtemp", Path: path, Err: err}
		}
		if err := VerifyWindowsHandle(handle, false, user); err != nil {
			_ = windows.CloseHandle(handle)
			_ = os.Remove(path)
			return nil, err
		}
		file := os.NewFile(uintptr(handle), path)
		if file == nil {
			_ = windows.CloseHandle(handle)
			_ = os.Remove(path)
			return nil, fmt.Errorf("create private Windows file handle")
		}
		return file, nil
	}
	return nil, &os.PathError{Op: "createtemp", Path: filepath.Join(dir, pattern), Err: os.ErrExist}
}
