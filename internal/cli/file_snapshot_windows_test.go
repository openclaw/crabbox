//go:build windows

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsFileSnapshotManagedTransferPath(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "Evidence.bin")
	if err := os.WriteFile(path, []byte("evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := normalizeManagedTransferPath(filepath.Join(root, "evidence.bin"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(got, want) || filepath.Base(got) != "Evidence.bin" {
		t.Fatalf("normalized path=%q want actual spelling %q", got, want)
	}
	data, err := os.ReadFile(got)
	if err != nil || string(data) != "evidence" {
		t.Fatalf("normalized path changed evidence: data=%q err=%v", data, err)
	}
}
