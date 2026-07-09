package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
)

const (
	privateRunOutputDirMode  = 0o700
	privateRunOutputFileMode = 0o600
)

func createPrivateRunOutputDir(path string) error {
	return os.MkdirAll(path, privateRunOutputDirMode)
}

func writePrivateRunOutputFileIfAbsent(path string, data []byte) (bool, error) {
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, "."+filepath.Base(path)+".crabbox-*")
	if err != nil {
		return false, err
	}
	tempPath := file.Name()
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(tempPath)
	}
	if err := file.Chmod(privateRunOutputFileMode); err != nil {
		cleanup()
		return false, err
	}
	if _, err := io.Copy(file, bytes.NewReader(data)); err != nil {
		cleanup()
		return false, err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tempPath)
		return false, err
	}
	// Publish a fully written key without replacing one won by a concurrent process.
	if err := os.Link(tempPath, path); err != nil {
		_ = os.Remove(tempPath)
		if errors.Is(err, os.ErrExist) {
			return false, nil
		}
		return false, err
	}
	if err := os.Remove(tempPath); err != nil {
		return true, err
	}
	return true, nil
}
