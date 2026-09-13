package cli

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

func managedStateEntrySpelling(resolvedParent, name string, info os.FileInfo) (string, error) {
	parentInfo, err := os.Lstat(resolvedParent)
	if err != nil {
		return "", err
	}
	fd, err := unix.Open(resolvedParent, unix.O_EVTONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	file := os.NewFile(uintptr(fd), resolvedParent)
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !os.SameFile(parentInfo, opened) {
		return "", fmt.Errorf("managed-state transfer path changed during metadata inspection")
	}
	entry, err := unix.BytePtrFromString(name)
	if err != nil {
		return "", err
	}
	attrs := unix.Attrlist{Bitmapcount: unix.ATTR_BIT_MAP_COUNT, Commonattr: unix.ATTR_CMN_NAME | unix.ATTR_CMN_DEVID | unix.ATTR_CMN_FILEID}
	var buffer [unix.PathMax]byte
	// Query the entry without opening it: sockets reject open and FIFOs can
	// block even with O_EVTONLY. Darwin packs attributes on four-byte boundaries.
	_, _, errno := unix.Syscall6(unix.SYS_GETATTRLISTAT, uintptr(fd), uintptr(unsafe.Pointer(entry)), uintptr(unsafe.Pointer(&attrs)), uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)), unix.FSOPT_NOFOLLOW)
	if errno != 0 {
		return "", errno
	}
	total := int64(binary.NativeEndian.Uint32(buffer[0:4]))
	start := 4 + int64(int32(binary.NativeEndian.Uint32(buffer[4:8])))
	length := int64(binary.NativeEndian.Uint32(buffer[8:12]))
	if total < 24 || total > int64(len(buffer)) || start < 24 || length < 2 || start+length > total {
		return "", fmt.Errorf("invalid managed-state transfer entry attributes")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || binary.NativeEndian.Uint32(buffer[12:16]) != uint32(stat.Dev) || binary.NativeEndian.Uint64(buffer[16:24]) != stat.Ino {
		return "", fmt.Errorf("managed-state transfer path changed during metadata inspection")
	}
	rawName := buffer[start : start+length]
	if bytes.IndexByte(rawName, 0) != len(rawName)-1 {
		return "", fmt.Errorf("invalid managed-state transfer entry name")
	}
	// Keep the resolved parent, including its chosen firmlink namespace.
	spelling := string(rawName[:len(rawName)-1])
	if filepath.Base(spelling) != spelling || !strings.EqualFold(spelling, name) {
		return "", fmt.Errorf("ambiguous managed-state transfer path spelling")
	}
	candidate := filepath.Join(resolvedParent, spelling)
	current, err := os.Lstat(candidate)
	if err != nil {
		return "", err
	}
	if !os.SameFile(info, current) {
		return "", fmt.Errorf("managed-state transfer path changed during metadata inspection")
	}
	currentParent, err := os.Lstat(resolvedParent)
	if err != nil {
		return "", err
	}
	if !os.SameFile(opened, currentParent) {
		return "", fmt.Errorf("managed-state transfer parent changed during metadata inspection")
	}
	return candidate, nil
}
