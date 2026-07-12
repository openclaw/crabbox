//go:build windows

package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenArtifactReadOnlySharesDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "summary.md")
	if err := os.WriteFile(path, []byte("bound-summary"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := openArtifactReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove open summary: %v", err)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "bound-summary" {
		t.Fatalf("data=%q, want bound summary bytes", data)
	}
}

func TestOpenArtifactReadOnlyLongPath(t *testing.T) {
	dir := t.TempDir()
	for len(dir) < 300 {
		dir = filepath.Join(dir, strings.Repeat("nested", 8))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "summary.md")
	if err := os.WriteFile(path, []byte("long-path-summary"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := openArtifactReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "long-path-summary" {
		t.Fatalf("data=%q, want long-path summary bytes", data)
	}
}
