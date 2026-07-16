//go:build linux

package main

import (
	"syscall"
	"unsafe"
)

func pipeBufferedBytes(fd int) (int, error) {
	var available int32
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TIOCINQ, uintptr(unsafe.Pointer(&available)))
	if errno != 0 {
		return 0, errno
	}
	return int(available), nil
}
