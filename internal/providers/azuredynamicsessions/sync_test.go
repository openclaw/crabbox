package azuredynamicsessions

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanAzureDynamicSessionsWorkspacePathRejectsUnsafeRoots(t *testing.T) {
	for _, workspace := range []string{"", "relative/work", "/", "/tmp", "/workspace", "/var"} {
		t.Run(workspace, func(t *testing.T) {
			if _, err := cleanAzureDynamicSessionsWorkspacePath(workspace); err == nil {
				t.Fatal("workspace should be rejected")
			}
		})
	}
}

func TestCleanAzureDynamicSessionsWorkspacePathCleansDedicatedAbsolutePath(t *testing.T) {
	got, err := cleanAzureDynamicSessionsWorkspacePath("/workspace/../workspace/crabbox/repo")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/workspace/crabbox/repo" {
		t.Fatalf("workspace = %q", got)
	}
}

func TestCreateAzureDynamicSessionsSyncArchiveRejectsUnsafeManifestPath(t *testing.T) {
	_, err := createAzureDynamicSessionsSyncArchive(t.Context(), Repo{Root: t.TempDir()}, SyncManifest{Files: []string{"../secret.txt"}})
	if err == nil || !strings.Contains(err.Error(), "unsafe sync path") {
		t.Fatalf("err = %v, want unsafe path", err)
	}
}

func TestCreateAzureDynamicSessionsSyncArchiveIncludesFilesAndSymlinks(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("file.txt", filepath.Join(repo, "link.txt")); err != nil {
		t.Fatal(err)
	}
	archive, err := createAzureDynamicSessionsSyncArchive(t.Context(), Repo{Root: repo}, SyncManifest{Files: []string{"file.txt", "link.txt"}})
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
	tr := tar.NewReader(gz)
	seen := map[string]tar.Header{}
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		seen[header.Name] = *header
	}
	if seen["file.txt"].Typeflag != tar.TypeReg {
		t.Fatalf("file header = %#v", seen["file.txt"])
	}
	if seen["link.txt"].Typeflag != tar.TypeSymlink || seen["link.txt"].Linkname != "file.txt" {
		t.Fatalf("link header = %#v", seen["link.txt"])
	}
}

func TestBuildAzureDynamicSessionsCommandPreservesArgvQuoting(t *testing.T) {
	got, err := buildAzureDynamicSessionsCommand([]string{"printf", "%s\n", "two words"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "'two words'") {
		t.Fatalf("command = %q, want quoted argument", got)
	}
}

func TestBuildAzureDynamicSessionsCommandPreservesSingleShellString(t *testing.T) {
	got, err := buildAzureDynamicSessionsCommand([]string{"npm test"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got != "npm test" {
		t.Fatalf("command = %q, want unquoted shell string", got)
	}
}

func TestBuildAzureDynamicSessionsCommandQuotesMultiArgShellCommand(t *testing.T) {
	got, err := buildAzureDynamicSessionsCommand([]string{"npm", "test", "&&", "npm", "run", "lint"}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := "'npm' 'test' && 'npm' 'run' 'lint'"
	if got != want {
		t.Fatalf("command = %q, want %q", got, want)
	}
}
