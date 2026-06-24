//go:build windows

package firecracker

import "syscall"

func signalProcess(int, syscall.Signal) error {
	return nil
}

func processAlive(int) bool {
	return false
}
