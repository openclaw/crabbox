//go:build darwin

package main

import (
	"syscall"
	"unsafe"
)

func pipeBufferedBytes(fd int) (int, error) {
	const fionread = 0x4004667f // _IOR('f', 127, int)
	var available int32
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), fionread, uintptr(unsafe.Pointer(&available)))
	if errno != 0 {
		return 0, errno
	}
	return int(available), nil
}
