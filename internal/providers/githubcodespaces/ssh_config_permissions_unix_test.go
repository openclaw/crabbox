//go:build !windows

package githubcodespaces

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidatePrivateSSHConfigFileRequires0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte("Host sturdy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validatePrivateSSHConfigFile(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validatePrivateSSHConfigFile(path); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("err=%v", err)
	}
}
