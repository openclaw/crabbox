//go:build windows

package cli

import (
	"bytes"
	"io"
	"os"
)

func ensurePrivateRunOutputDir(path string) error {
	if err := createPrivateRunOutputDir(path); err != nil {
		return err
	}
	return os.Chmod(path, privateRunOutputDirMode)
}

func openPrivateRunOutputFile(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, privateRunOutputFileMode)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(privateRunOutputFileMode); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := file.Truncate(0); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func writePrivateRunOutputFile(path string, data []byte) error {
	file, err := openPrivateRunOutputFile(path)
	if err != nil {
		return err
	}
	if _, err := io.Copy(file, bytes.NewReader(data)); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func checkPrivateRunOutputReplaceable(_, _ string) error {
	return nil
}
