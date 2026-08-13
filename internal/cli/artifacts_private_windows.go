//go:build windows

package cli

import (
	"os"

	"golang.org/x/sys/windows"
)

func openArtifactBundleTemp(root *os.Root, name string, perm os.FileMode, private bool) (*os.File, error) {
	if !private {
		return root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	}
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	return createPrivateWindowsFileAt(windows.Handle(directory.Fd()), name)
}
