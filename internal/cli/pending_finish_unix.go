//go:build !windows

package cli

import (
	"fmt"
	"os"
)

func publishPendingFinish(temp, path string) error { return os.Link(temp, path) }

func openPendingFinishFile(path string) (*os.File, error) {
	file, err := openControllerTokenFile(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err == nil && info.Mode().Perm() != 0400 && info.Mode().Perm() != 0600 {
		err = fmt.Errorf("pending finish must have mode 0400 or 0600")
	}
	if err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}
