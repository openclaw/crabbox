package replicate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestArchiveDataURLBuildsGzipDataURL(t *testing.T) {
	root := newReplicateTestRepo(t, map[string]string{"main.go": "package main\n", "README.md": "hello\n"})
	result, err := buildReplicateArchiveDataURL(context.Background(), Config{
		Replicate: ReplicateConfig{MaxArchiveBytes: 1024 * 1024},
	}, Runtime{}, core.Repo{Root: root, Name: "repo"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(result.DataURL, replicateArchiveDataURLPrefix) {
		t.Fatalf("data URL prefix missing: %q", result.DataURL[:min(len(result.DataURL), len(replicateArchiveDataURLPrefix))])
	}
	payload := strings.TrimPrefix(result.DataURL, replicateArchiveDataURLPrefix)
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatal(err)
	}
	files := readArchiveFiles(t, raw)
	if files["main.go"] != "package main\n" || files["README.md"] != "hello\n" {
		t.Fatalf("archive files=%v", files)
	}
	if result.Size <= 0 || len(result.Phases) == 0 {
		t.Fatalf("archive result=%#v", result)
	}
}

func TestMaxArchiveRejectsBeforePredictionCreate(t *testing.T) {
	root := newReplicateTestRepo(t, map[string]string{"large.txt": strings.Repeat("x", 4096)})
	createCalled := false
	_, err := buildArchiveThenCreatePrediction(context.Background(), Config{
		Replicate: ReplicateConfig{MaxArchiveBytes: 1},
	}, Runtime{}, core.Repo{Root: root, Name: "repo"}, func(context.Context, string) error {
		createCalled = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "replicate archive too large") {
		t.Fatalf("buildArchiveThenCreatePrediction error=%v", err)
	}
	if createCalled {
		t.Fatal("prediction create callback was called after max archive failure")
	}
}

func TestArchiveDataURLAllowsExplicitZeroMaxArchive(t *testing.T) {
	root := newReplicateTestRepo(t, map[string]string{"file.txt": "ok\n"})
	result, err := buildReplicateArchiveDataURL(context.Background(), Config{
		Replicate: ReplicateConfig{MaxArchiveBytes: 0},
	}, Runtime{}, core.Repo{Root: root, Name: "repo"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.DataURL == "" {
		t.Fatal("empty data URL")
	}
}

func buildArchiveThenCreatePrediction(ctx context.Context, cfg Config, rt Runtime, repo Repo, create func(context.Context, string) error) (replicateArchiveInput, error) {
	archive, err := buildReplicateArchiveDataURL(ctx, cfg, rt, repo, false)
	if err != nil {
		return replicateArchiveInput{}, err
	}
	if err := create(ctx, archive.DataURL); err != nil {
		return replicateArchiveInput{}, err
	}
	return archive, nil
}

func newReplicateTestRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	runGit(t, root, "config", "user.email", "alice@example.com")
	runGit(t, root, "config", "user.name", "Alice Example")
	for name, content := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-m", "initial")
	return root
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func readArchiveFiles(t *testing.T, payload []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := map[string]string{}
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		files[header.Name] = string(data)
	}
	return files
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
