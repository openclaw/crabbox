//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestLoadExternalRoutingRejectsFinalSymlink(t *testing.T) {
	dir := filepath.Join(privateExternalRoutingTempDir(t), "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target.json")
	if err := os.WriteFile(target, []byte(`{"command":"provider"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "route.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadExternalRouting(link); err == nil || !strings.Contains(err.Error(), "without symlinks") {
		t.Fatalf("err=%v", err)
	}
}

func TestLoadExternalRoutingRejectsAncestorSymlink(t *testing.T) {
	root := privateExternalRoutingTempDir(t)
	realDir := filepath.Join(root, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "route.json"), []byte(`{"command":"provider"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(root, "linked")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadExternalRouting(filepath.Join(linkDir, "route.json")); err == nil || !strings.Contains(err.Error(), "without symlinks") {
		t.Fatalf("err=%v", err)
	}
}

func TestLoadExternalRoutingRejectsNonPrivateParent(t *testing.T) {
	root := privateExternalRoutingTempDir(t)
	dir := filepath.Join(root, "shared")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "route.json")
	if err := os.WriteFile(path, []byte(`{"command":"provider"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadExternalRouting(path); err == nil || !strings.Contains(err.Error(), "must not be accessible") {
		t.Fatalf("err=%v", err)
	}
}

func TestLoadExternalRoutingRejectsNonRegularFileWithoutBlocking(t *testing.T) {
	dir := filepath.Join(privateExternalRoutingTempDir(t), "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "route.json")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadExternalRouting(path); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("err=%v", err)
	}
}

func TestLoadExternalRoutingAllowsPrivateRouteBelowStickyTempRoot(t *testing.T) {
	dir, err := os.MkdirTemp(canonicalUnixTempRoot(t), "crabbox-routing-")
	if err != nil {
		t.Skipf("create Linux-style temp route: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "route.json")
	if err := os.WriteFile(path, []byte(`{"command":"provider"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadExternalRouting(path); err != nil {
		t.Fatalf("private route below sticky temp root: %v", err)
	}
}

func TestLoadExternalRoutingRejectsBroadIntermediateDirectory(t *testing.T) {
	root, err := os.MkdirTemp(canonicalUnixTempRoot(t), "crabbox-routing-broad-")
	if err != nil {
		t.Skipf("create Linux-style temp route: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.Chmod(root, 0o777); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "route.json")
	if err := os.WriteFile(path, []byte(`{"command":"provider"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadExternalRouting(path); err == nil || !strings.Contains(err.Error(), "must not be writable") {
		t.Fatalf("err=%v", err)
	}
}

func canonicalUnixTempRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		t.Skipf("resolve system temporary directory: %v", err)
	}
	return root
}
