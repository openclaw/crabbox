//go:build !windows

package sshhost

import (
	"fmt"
	"os"
	"syscall"

	"github.com/gofrs/flock"
)

func secureStateDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("host state must be a private directory")
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || st.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("host state owner mismatch")
	}
	return nil
}
func lockMachine() (func(), error) {
	lock := flock.New("/var/run/crabbox-managed-host.lock")
	ok, err := lock.TryLock()
	if err != nil {
		return nil, err
	}
	if !ok {
		lock.Close()
		return nil, fmt.Errorf("another managed host agent owns this machine")
	}
	return func() { _ = lock.Close() }, nil
}
