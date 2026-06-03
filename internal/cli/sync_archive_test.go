package cli

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestCreateSyncArchiveTreatsOptionLikeNamesAsFiles(t *testing.T) {
	root := t.TempDir()
	names := []string{
		"--checkpoint=1",
		"--checkpoint-action=exec=sh pwn.sh",
		"normal.txt",
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	archive, err := CreateSyncArchive(context.Background(), Repo{Root: root}, SyncManifest{Files: names}, "crabbox-sync-test-*.tgz")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(archive.Name())
	defer archive.Close()

	got := syncArchiveNames(t, archive)
	for _, name := range names {
		if !got[name] {
			t.Fatalf("archive missing %q; got %#v", name, got)
		}
	}
}

func TestCreateSyncArchiveRejectsUnsafePaths(t *testing.T) {
	_, err := CreateSyncArchive(context.Background(), Repo{Root: t.TempDir()}, SyncManifest{Files: []string{"../secret.txt"}}, "crabbox-sync-test-*.tgz")
	if err == nil {
		t.Fatal("expected unsafe path error")
	}
	var exitErr ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 6 {
		t.Fatalf("err=%#v, want sync exit error", err)
	}
}

func syncArchiveNames(t *testing.T, archive *os.File) map[string]bool {
	t.Helper()
	if _, err := archive.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	got := map[string]bool{}
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return got
		}
		if err != nil {
			t.Fatal(err)
		}
		got[header.Name] = true
	}
}
