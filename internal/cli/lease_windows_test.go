//go:build windows

package cli

import (
	"path/filepath"
	"testing"
)

func TestEnsureTestboxLeaseDirectoryCreatesPrivateWindowsComponents(t *testing.T) {
	dirs := isolateTestUserDirs(t)
	const leaseID = "cbx_abcdef123456"
	leaseDir, err := ensureTestboxLeaseDirectory(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dirs.AppData, "crabbox", "testboxes", leaseID)
	if leaseDir != want {
		t.Fatalf("lease directory=%q want %q", leaseDir, want)
	}
	for _, path := range []string{
		filepath.Join(dirs.AppData, "crabbox"),
		filepath.Join(dirs.AppData, "crabbox", "testboxes"),
		want,
	} {
		if err := verifyPrivateWindowsPath(path, true); err != nil {
			t.Fatalf("verify private lease SSH directory %q: %v", path, err)
		}
	}
}
