//go:build windows

package privatefile

import (
	"strings"

	"golang.org/x/sys/windows"
)

// WindowsPath preserves existing device paths and extends long paths for Win32 calls.
func WindowsPath(path string) (string, error) {
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
