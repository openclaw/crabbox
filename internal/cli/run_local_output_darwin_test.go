//go:build darwin

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrivateRunOutputRemovesInheritedDarwinACL(t *testing.T) {
	parent := t.TempDir()
	if output, err := exec.Command("chmod", "+a", "everyone allow read,file_inherit", parent).CombinedOutput(); err != nil {
		t.Fatalf("add inherited ACL: %v\n%s", err, output)
	}
	path := filepath.Join(parent, "private.txt")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertDarwinPathHasInheritedACL(t, path, true)
	if err := writePrivateRunOutputFile(path, []byte("private")); err != nil {
		t.Fatal(err)
	}
	assertRunOutputMode(t, path, privateRunOutputFileMode)
	assertDarwinPathHasInheritedACL(t, path, false)
}

func assertDarwinPathHasInheritedACL(t *testing.T, path string, want bool) {
	t.Helper()
	output, err := exec.Command("ls", "-le", path).CombinedOutput()
	if err != nil {
		t.Fatalf("inspect ACL: %v\n%s", err, output)
	}
	hasInherited := strings.Contains(string(output), " inherited ")
	if hasInherited != want {
		t.Fatalf("%s inherited ACL=%t, want %t\n%s", path, hasInherited, want, output)
	}
}
