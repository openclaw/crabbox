//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifySSHTransportPathPrivateAdmitsOwnerOnlyModes(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name      string
		mode      os.FileMode
		directory bool
		wantErr   bool
	}{
		{"owner read/write file", 0o600, false, false},
		{"owner read-only file", 0o400, false, false},
		{"group readable file", 0o640, false, true},
		{"other readable file", 0o604, false, true},
		{"owner directory", 0o700, true, false},
		{"group accessible directory", 0o750, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(tc.name, "/", "-"))
			if tc.directory {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tc.mode); err != nil {
				t.Fatal(err)
			}
			err := verifySSHTransportPathPrivate(path, tc.directory)
			if (err != nil) != tc.wantErr {
				t.Fatalf("mode %o: error=%v wantErr=%v", tc.mode, err, tc.wantErr)
			}
		})
	}
}
