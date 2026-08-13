//go:build !windows

package cli

import (
	"os"
)

func openArtifactBundleTemp(root *os.Root, name string, perm os.FileMode, private bool) (*os.File, error) {
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return nil, err
	}
	if private {
		if err := securePrivateFile(file); err != nil {
			_ = file.Close()
			return nil, err
		}
	}
	return file, nil
}
