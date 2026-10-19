//go:build !windows

package cli

import (
	"os"
	"testing"
)

func prepareConfigPrivateParent(t *testing.T, path string) {
	t.Helper()
}

func assertConfigPrivate(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("config mode=%04o want 0600", got)
	}
}
