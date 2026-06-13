//go:build !linux && !darwin

package cli

import (
	"fmt"
	"os"
)

type controllerStateLock struct{}

func acquireControllerStateLock(string) (*controllerStateLock, error) {
	return nil, fmt.Errorf("controller state locking is supported only on Linux and macOS")
}

func validateControllerStateDirectoryPath(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("controller state directory must be a directory")
	}
	return nil
}

func (l *controllerStateLock) Unlock() error { return nil }
