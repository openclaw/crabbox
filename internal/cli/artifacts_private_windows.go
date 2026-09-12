//go:build windows

package cli

import (
	"errors"
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
	file, err := createPrivateWindowsFileAt(windows.Handle(directory.Fd()), name)
	if errors.Is(err, windows.STATUS_OBJECT_NAME_COLLISION) {
		// Match OpenFile's exclusive-create contract for callers opening existing locks.
		return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrExist}
	}
	return file, err
}
