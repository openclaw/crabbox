package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

func managedStateEntrySpelling(resolvedParent, name string, info os.FileInfo) (string, error) {
	path := filepath.Join(resolvedParent, name)
	fd, err := unix.Open(path, unix.O_EVTONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !os.SameFile(info, opened) {
		return "", fmt.Errorf("managed-state transfer path changed during metadata inspection")
	}
	var buffer [unix.PathMax]byte
	_, _, errno := unix.Syscall(unix.SYS_FCNTL, uintptr(fd), unix.F_GETPATH, uintptr(unsafe.Pointer(&buffer[0])))
	if errno != 0 {
		return "", errno
	}
	end := bytes.IndexByte(buffer[:], 0)
	if end <= 0 || !filepath.IsAbs(string(buffer[:end])) {
		return "", fmt.Errorf("invalid managed-state transfer descriptor path")
	}
	// Keep the resolved parent: a descriptor path must not redirect the scope
	// through a different hard-link name or a backing firmlink namespace.
	spelling := filepath.Base(string(buffer[:end]))
	if !strings.EqualFold(spelling, name) {
		return "", fmt.Errorf("ambiguous managed-state transfer path spelling")
	}
	candidate := filepath.Join(resolvedParent, spelling)
	current, err := os.Lstat(candidate)
	if err != nil {
		return "", err
	}
	if !os.SameFile(opened, current) {
		return "", fmt.Errorf("managed-state transfer path changed during metadata inspection")
	}
	return candidate, nil
}
