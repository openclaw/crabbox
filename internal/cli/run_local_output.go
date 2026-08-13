package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
)

const (
	privateRunOutputDirMode  = 0o700
	privateRunOutputFileMode = 0o600
)

func writePrivateRunOutputFileIfAbsent(path string, data []byte) (bool, error) {
	file, tempPath, err := createPrivateRunOutputTemp(path)
	if err != nil {
		return false, err
	}
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(tempPath)
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
