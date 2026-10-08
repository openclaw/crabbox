//go:build windows

package cli

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func windowsSFTPFileVersion(name string) ([4]uint16, error) {
	var version [4]uint16
	info, err := os.Lstat(name)
	if err != nil {
		return version, err
	}
	if !info.Mode().IsRegular() {
		return version, errors.New("OpenSSH executable is not a regular file")
	}
	size, err := windows.GetFileVersionInfoSize(name, nil)
	if err != nil {
		return version, err
	}
	if size == 0 || size > 1024*1024 {
		return version, errors.New("invalid OpenSSH version resource size")
	}
	data := make([]byte, size)
	if err := windows.GetFileVersionInfo(name, 0, size, unsafe.Pointer(&data[0])); err != nil {
		return version, err
	}
	var fixed *windows.VS_FIXEDFILEINFO
	var fixedSize uint32
	if err := windows.VerQueryValue(unsafe.Pointer(&data[0]), `\`, unsafe.Pointer(&fixed), &fixedSize); err != nil {
		return version, err
	}
	start, address := uintptr(unsafe.Pointer(&data[0])), uintptr(unsafe.Pointer(fixed))
	required := unsafe.Sizeof(windows.VS_FIXEDFILEINFO{})
	if fixed == nil || uintptr(fixedSize) < required || uintptr(len(data)) < required ||
		address < start || address > start+uintptr(len(data))-required || fixed.Signature != 0xfeef04bd {
		return version, errors.New("invalid OpenSSH fixed version resource")
	}
	return [4]uint16{uint16(fixed.FileVersionMS >> 16), uint16(fixed.FileVersionMS), uint16(fixed.FileVersionLS >> 16), uint16(fixed.FileVersionLS)}, nil
}
