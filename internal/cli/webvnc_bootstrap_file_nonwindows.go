//go:build !windows

package cli

import (
	"os"
	"path/filepath"
)

func createWebVNCPortalBootstrapFile() (string, string, *os.File, error) {
	dir, err := os.MkdirTemp("", "crabbox-webvnc-bootstrap-*")
	if err != nil {
		return "", "", nil, err
	}
	cleanup := func() {
		_ = os.RemoveAll(dir)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		cleanup()
		return "", "", nil, err
	}
	path := filepath.Join(dir, "open.html")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		cleanup()
		return "", "", nil, err
	}
	return dir, path, file, nil
}
