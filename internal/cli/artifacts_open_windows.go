//go:build windows

package cli

import (
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

func openArtifactReadOnly(path string) (*os.File, error) {
	openPath, err := artifactWindowsLongPath(path)
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

func artifactWindowsLongPath(path string) (string, error) {
	isSeparator := func(value byte) bool { return value == '\\' || value == '/' }
	if len(path) >= 4 {
		if strings.HasPrefix(path, `\??\`) ||
			(isSeparator(path[0]) && isSeparator(path[1]) && (path[2] == '?' || path[2] == '.') && isSeparator(path[3])) {
			return path, nil
		}
	}
	fullPath, err := windows.FullPath(path)
	if err != nil {
		return "", err
	}
	if len(fullPath) < 248 {
		return path, nil
	}
	fullPath = strings.ReplaceAll(fullPath, "/", `\`)
	if strings.HasPrefix(fullPath, `\\`) {
		return `\\?\UNC\` + strings.TrimLeft(fullPath, `\`), nil
	}
	return `\\?\` + fullPath, nil
}

func openArtifactRootReadOnly(root *os.Root, name string) (*os.File, error) {
	return root.Open(name)
}
