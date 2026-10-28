//go:build windows

package cli

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"testing"
)

func TestFileSnapshotPathInfoOpensContentDeniedFileForMetadata(t *testing.T) {
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "metadata-only.txt")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("icacls", path, "/deny", current.Username+":(RD)").CombinedOutput(); err != nil {
		t.Fatalf("icacls deny: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("icacls", path, "/remove:d", current.Username).Run() })
	if _, err := os.ReadFile(path); !os.IsPermission(err) {
		t.Fatalf("content read should fail with permission denied: %v", err)
	}
	info, err := fileSnapshotPathInfo(path)
	if err != nil {
		t.Fatalf("metadata-only inspection failed: %v", err)
	}
	again, err := fileSnapshotPathInfo(path)
	if err != nil || !sameFileSnapshotIdentity(info, again) {
		t.Fatalf("identity not stable: %v", err)
	}
}
