package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunDelegatedArchiveSyncOwnsArchiveReplaceLifecycle(t *testing.T) {
	root := newDelegatedArchiveSyncRepo(t)
	cfg := baseConfig()
	cfg.Sync.Delete = true
	var uploadedPath string
	var uploadedBytes int
	var commands []string
	suffixes := []string{"archive", "staging"}

	phases, _, err := RunDelegatedArchiveSync(context.Background(), DelegatedArchiveSyncRequest{
		Config:              cfg,
		Repo:                Repo{Root: root},
		Workdir:             "/workspace/my app",
		TempPattern:         "crabbox-core-sync-*.tgz",
		RemoteArchiveDir:    "/tmp",
		RemoteArchivePrefix: "crabbox-core-",
		PhaseName:           "core_sync",
		Provider:            "test-provider",
		Stderr:              io.Discard,
		Suffix: func() string {
			value := suffixes[0]
			suffixes = suffixes[1:]
			return value
		},
		Upload: func(_ context.Context, remote string, body io.Reader) error {
			uploadedPath = remote
			data, err := io.ReadAll(body)
			uploadedBytes = len(data)
			return err
		},
		Exec: func(_ context.Context, command string) error {
			commands = append(commands, command)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if uploadedPath != "/tmp/crabbox-core-archive.tgz" || uploadedBytes == 0 {
		t.Fatalf("upload path=%q bytes=%d", uploadedPath, uploadedBytes)
	}
	joined := strings.Join(commands, "\n")
	for _, want := range []string{
		"mkdir -p '/workspace/.my app.crabbox-sync-staging'",
		"tar -xzf '/tmp/crabbox-core-archive.tgz' -C '/workspace/.my app.crabbox-sync-staging'",
		"mv '/workspace/.my app.crabbox-sync-staging' '/workspace/my app'",
		"rm -rf '/workspace/.my app.crabbox-sync-staging.previous'",
		"rm -f '/tmp/crabbox-core-archive.tgz'",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("commands missing %q:\n%s", want, joined)
		}
	}
	if got := phases[len(phases)-1].Name; got != "core_sync" {
		t.Fatalf("last phase=%q", got)
	}
}

func TestRunDelegatedArchiveSyncPreflightUsesFullArchive(t *testing.T) {
	root := newDelegatedArchiveSyncRepo(t)
	if err := os.WriteFile(filepath.Join(root, "one.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := baseConfig()
	cfg.Sync.FailFiles = 2
	cfg.Sync.FailBytes = 0
	var stderr bytes.Buffer
	uploaded := false

	_, _, err := RunDelegatedArchiveSync(context.Background(), DelegatedArchiveSyncRequest{
		Config:  cfg,
		Repo:    Repo{Root: root},
		Workdir: "/workspace",
		Stderr:  &stderr,
		Upload: func(context.Context, string, io.Reader) error {
			uploaded = true
			return nil
		},
		Exec: func(context.Context, string) error { return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "sync candidate too large: 2 files") {
		t.Fatalf("err=%v stderr=%q", err, stderr.String())
	}
	if uploaded {
		t.Fatal("upload ran after preflight failure")
	}
}

func TestRunDelegatedArchiveSyncCleanupOutlivesCanceledParent(t *testing.T) {
	root := newDelegatedArchiveSyncRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	var cleanupContextActive bool
	var calls int

	_, _, err := RunDelegatedArchiveSync(ctx, DelegatedArchiveSyncRequest{
		Config:  baseConfig(),
		Repo:    Repo{Root: root},
		Workdir: "/workspace",
		CleanupContext: func(parent context.Context) (context.Context, context.CancelFunc) {
			cleanupContextActive = parent.Err() == context.Canceled
			return context.WithTimeout(context.WithoutCancel(parent), time.Second)
		},
		Upload: func(context.Context, string, io.Reader) error { return nil },
		Exec: func(callCtx context.Context, command string) error {
			calls++
			if strings.HasPrefix(command, "tar ") {
				cancel()
				return context.Canceled
			}
			if strings.HasPrefix(command, "rm -f ") && callCtx.Err() != nil {
				t.Fatalf("cleanup context canceled: %v", callCtx.Err())
			}
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("err=%v", err)
	}
	if !cleanupContextActive || calls < 3 {
		t.Fatalf("cleanup active=%t calls=%d", cleanupContextActive, calls)
	}
}

func newDelegatedArchiveSyncRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range map[string]string{"one.txt": "one\n", "two.txt": "two\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"add", "."},
		{"commit", "-qm", "test: fixture"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	return root
}
