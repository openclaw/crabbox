package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagedStateEntrySpellingRejectsReplacedPath(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "Marker.txt")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	original, err := fileSnapshotPathInfo(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := managedStateEntrySpelling(parent, filepath.Base(path), original); err != nil || got != path {
		t.Fatalf("unchanged path rejected: path=%q err=%v", got, err)
	}
	// Retain the original object so its identity cannot be reused by the replacement.
	if err := os.Rename(path, filepath.Join(parent, "Original.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := managedStateEntrySpelling(parent, filepath.Base(path), original); err == nil || !strings.Contains(err.Error(), "path changed during metadata inspection") || got != "" {
		t.Fatalf("replaced path must fail closed: path=%q err=%v", got, err)
	}
}
