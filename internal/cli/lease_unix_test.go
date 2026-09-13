//go:build !windows

package cli

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestManagedStateTransferAliasesAndDanglingLinks(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	canonicalSource, err := filepath.EvalSymlinks(source)
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "source-alias")
	if err := os.Symlink(source, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", filepath.Join(source, "state"))
	if err := ValidateManagedStateTransferScope("fixture mount", alias); err == nil {
		t.Fatal("source alias missed managed namespace")
	}
	if got, err := NormalizeManagedStateTransferRoot(filepath.Join(alias, "absent", "leaf")); err != nil || got != filepath.Join(canonicalSource, "absent", "leaf") {
		t.Fatalf("missing suffix normalization=%q %v", got, err)
	}
	if err := os.Symlink("absent-target", filepath.Join(source, "ordinary-link")); err != nil {
		t.Fatal(err)
	}
	archive, err := CreateSyncArchive(context.Background(), Repo{Root: alias}, SyncManifest{Files: []string{"ordinary-link"}}, "dangling-marker-*.tgz")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	gz, err := gzip.NewReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	header, err := tar.NewReader(gz).Next()
	if err != nil || header.Name != "ordinary-link" || header.Typeflag != tar.TypeSymlink || header.Linkname != "absent-target" {
		t.Fatal("dangling link metadata was not preserved")
	}
}

func prepareLeaseSSHTestStateRoot(t *testing.T, root string) {
	t.Helper()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
}

func makeLeaseSSHTestConfigReadOnly(t *testing.T, path string) func() {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o700) })
	return func() {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o500 {
			t.Fatal("default config directory permissions changed")
		}
	}
}
