//go:build windows

package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenGitOverlaySnapshotParentRejectsFilesAndReparsePoints(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if handle, err := openGitOverlaySnapshotParent(file); err == nil {
		_ = handle.Close()
		t.Fatal("regular file accepted as snapshot parent")
	}

	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	if handle, err := openGitOverlaySnapshotParent(link); err == nil {
		_ = handle.Close()
		t.Fatal("reparse point accepted as snapshot parent")
	}
}

func TestOpenGitOverlaySnapshotParentSupportsMetadataRestoration(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	handle, err := openGitOverlaySnapshotParent(parent)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()

	wantTime := normalizedGitOverlayFileTime(time.Unix(1_700_000_000, 123_456_700))
	if err := syncGitOverlayFileTimes(handle, wantTime); err != nil {
		t.Fatal(err)
	}
	if err := handle.Chmod(0o555); err != nil {
		t.Fatal(err)
	}
	info, err := handle.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(wantTime) {
		t.Fatalf("parent mtime=%s want %s", info.ModTime(), wantTime)
	}
	if info.Mode().Perm() != 0o555 {
		t.Fatalf("parent mode=%#o want 0555", info.Mode().Perm())
	}

	t.Run("symlink mtime", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "target")
		if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := target + ".link"
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("file symlinks unavailable: %v", err)
		}
		wantLinkTime := normalizedGitOverlayFileTime(time.Unix(1_650_000_000, 987_654_300))
		if err := syncGitOverlaySymlinkTimes(link, wantLinkTime); err != nil {
			t.Fatal(err)
		}
		linkInfo, err := os.Lstat(link)
		if err != nil {
			t.Fatal(err)
		}
		if !linkInfo.ModTime().Equal(wantLinkTime) {
			t.Fatalf("symlink mtime=%s want %s", linkInfo.ModTime(), wantLinkTime)
		}
	})
}

func TestGitOverlaySnapshotCleanupThawsReadOnlyFiles(t *testing.T) {
	snapshot, err := newGitOverlaySnapshot()
	if err != nil {
		t.Fatal(err)
	}
	root := snapshot.Root
	if err := os.WriteFile(filepath.Join(root, "read-only.txt"), []byte("content"), 0o444); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(root, "read-only.txt")); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm()&0o200 != 0 {
		t.Fatalf("test file mode=%#o want read-only", info.Mode().Perm())
	}
	if err := snapshot.cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("snapshot root survived cleanup: %v", err)
	}
}
