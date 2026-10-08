//go:build !windows

package cli

import (
	"errors"
	"os"
	"testing"
)

func makeTestDirectoryUnreadable(t *testing.T, path string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permission checks")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(path, info.Mode().Perm()); err != nil {
			t.Errorf("restore directory permissions: %v", err)
		}
	})
	if _, err := os.ReadDir(path); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("directory mode did not deny reads: %v", err)
	}
}
