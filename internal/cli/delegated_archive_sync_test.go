package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrepareDelegatedArchiveRejectsExistingGuardrailBeforeCreatingTempFile(t *testing.T) {
	root := newDelegatedArchiveSyncRepo(t)
	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)
	cfg := baseConfig()
	cfg.Sync.FailFiles = 2
	cfg.Sync.FailBytes = 0

	archive, err := PrepareDelegatedArchive(context.Background(), DelegatedArchivePreparationRequest{
		Config: cfg,
		Repo:   Repo{Root: root},
		Stderr: io.Discard,
	})
	if archive != nil || err == nil || !strings.Contains(err.Error(), "sync candidate too large: 2 files >= limit 2; use --force-sync-large or CRABBOX_SYNC_ALLOW_LARGE=1") {
		t.Fatalf("archive=%#v err=%v", archive, err)
	}
	entries, readErr := os.ReadDir(tempDir)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("temporary archives=%v err=%v", entries, readErr)
	}
}

func TestPrepareDelegatedArchiveCleansPartialArchiveOnFailure(t *testing.T) {
	root := newDelegatedArchiveSyncRepo(t)
	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)
	calls := 0
	now := func() time.Time {
		calls++
		if calls == 5 {
			if err := os.Remove(filepath.Join(root, "one.txt")); err != nil {
				t.Fatal(err)
			}
		}
		return time.Unix(0, int64(calls)*int64(time.Millisecond))
	}

	archive, err := PrepareDelegatedArchive(context.Background(), DelegatedArchivePreparationRequest{
		Config: baseConfig(), Repo: Repo{Root: root}, Stderr: io.Discard, Now: now,
	})
	if archive != nil || err == nil || !strings.Contains(err.Error(), "stat sync path one.txt") {
		t.Fatalf("archive=%#v err=%v", archive, err)
	}
	entries, readErr := os.ReadDir(tempDir)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("temporary archives=%v err=%v", entries, readErr)
	}
}

func TestRunDelegatedArchiveSyncConsumesPreparedSnapshotAndPreservesTiming(t *testing.T) {
	root := newDelegatedArchiveSyncRepo(t)
	cfg := baseConfig()
	var stderr bytes.Buffer
	calls := 0
	now := func() time.Time {
		calls++
		return time.Unix(0, int64(calls)*int64(7*time.Millisecond))
	}
	prepared, err := PrepareDelegatedArchive(context.Background(), DelegatedArchivePreparationRequest{
		Config: cfg, Repo: Repo{Root: root}, Stderr: &stderr, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	archivePath := prepared.File.Name()
	if prepared.Size <= 0 || len(prepared.Manifest.Files) != 2 {
		t.Fatalf("archive size=%d manifest=%#v", prepared.Size, prepared.Manifest)
	}
	if err := os.WriteFile(filepath.Join(root, "one.txt"), []byte("changed after preparation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Sync.FailFiles = 1
	uploads := 0
	var archivedOne string

	phases, total, err := RunDelegatedArchiveSync(context.Background(), DelegatedArchiveSyncRequest{
		Config: cfg, Repo: Repo{Root: root}, Workdir: "/workspace", Stderr: &stderr, Now: now,
		Upload: func(_ context.Context, _ string, body io.Reader) error {
			uploads++
			gz, err := gzip.NewReader(body)
			if err != nil {
				return err
			}
			defer gz.Close()
			entries := tar.NewReader(gz)
			for {
				header, err := entries.Next()
				if errors.Is(err, io.EOF) {
					return nil
				}
				if err != nil {
					return err
				}
				if header.Name == "one.txt" {
					content, err := io.ReadAll(entries)
					archivedOne = string(content)
					if err != nil {
						return err
					}
				}
			}
		},
		Exec: func(context.Context, string) error { return nil },
	}, prepared)
	if err != nil {
		t.Fatal(err)
	}
	if uploads != 1 || archivedOne != "one\n" {
		t.Fatalf("uploads=%d archived one.txt=%q", uploads, archivedOne)
	}
	if got := strings.Count(stderr.String(), "sync candidate:"); got != 1 {
		t.Fatalf("preflight count=%d stderr=%q", got, stderr.String())
	}
	for _, name := range []string{"manifest", "preflight", "archive"} {
		found := false
		for _, phase := range phases {
			if phase.Name == name {
				found = true
				if phase.Ms != 7 {
					t.Fatalf("phase %s=%dms, want 7ms", name, phase.Ms)
				}
			}
		}
		if !found {
			t.Fatalf("missing %s phase: %#v", name, phases)
		}
	}
	if total < 21*time.Millisecond {
		t.Fatalf("total=%s, want prepared archive phases included", total)
	}
	if _, err := os.Stat(archivePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prepared archive remains at %q: %v", archivePath, err)
	}
	if err := prepared.Close(); err != nil {
		t.Fatalf("idempotent second close: %v", err)
	}
}

func TestRunDelegatedArchiveSyncClosesPreparedArchiveOnEveryFailurePath(t *testing.T) {
	root := newDelegatedArchiveSyncRepo(t)
	tests := []struct {
		name      string
		configure func(*DelegatedArchiveSyncRequest)
	}{
		{name: "missing callbacks", configure: func(req *DelegatedArchiveSyncRequest) { req.Upload = nil }},
		{name: "missing workdir", configure: func(req *DelegatedArchiveSyncRequest) { req.Workdir = "" }},
		{name: "upload", configure: func(req *DelegatedArchiveSyncRequest) {
			req.Upload = func(context.Context, string, io.Reader) error { return errors.New("upload failed") }
		}},
		{name: "prepare", configure: func(req *DelegatedArchiveSyncRequest) {
			req.Exec = func(_ context.Context, command string) error {
				if strings.Contains(command, "mkdir -p ") {
					return errors.New("prepare failed")
				}
				return nil
			}
		}},
		{name: "extract", configure: func(req *DelegatedArchiveSyncRequest) {
			req.Exec = func(_ context.Context, command string) error {
				if strings.HasPrefix(command, "tar -xzf ") {
					return errors.New("extract failed")
				}
				return nil
			}
		}},
		{name: "replace", configure: func(req *DelegatedArchiveSyncRequest) {
			req.Config.Sync.Delete = true
			req.Replace = func(context.Context, string, string) error { return errors.New("replace failed") }
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			prepared, err := PrepareDelegatedArchive(context.Background(), DelegatedArchivePreparationRequest{
				Config: baseConfig(), Repo: Repo{Root: root}, Stderr: io.Discard,
			})
			if err != nil {
				t.Fatal(err)
			}
			archivePath := prepared.File.Name()
			req := DelegatedArchiveSyncRequest{
				Config: baseConfig(), Repo: Repo{Root: root}, Workdir: "/workspace", Stderr: io.Discard,
				Upload: func(context.Context, string, io.Reader) error { return nil },
				Exec:   func(context.Context, string) error { return nil },
			}
			test.configure(&req)
			if _, _, err := RunDelegatedArchiveSync(context.Background(), req, prepared); err == nil {
				t.Fatal("sync unexpectedly succeeded")
			}
			if _, err := os.Stat(archivePath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("prepared archive remains at %q: %v", archivePath, err)
			}
			if err := prepared.Close(); err != nil {
				t.Fatalf("idempotent second close: %v", err)
			}
		})
	}
}

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

func TestRunDelegatedArchiveSyncRejectsInScopeSparseOmissionBeforeUpload(t *testing.T) {
	root := newDelegatedArchiveSyncRepo(t)
	runGit(t, root, "sparse-checkout", "set", "--no-cone", "/one.txt")
	uploaded := false
	executed := false

	_, _, err := RunDelegatedArchiveSync(context.Background(), DelegatedArchiveSyncRequest{
		Config:  baseConfig(),
		Repo:    Repo{Root: root},
		Workdir: "/workspace",
		Upload: func(context.Context, string, io.Reader) error {
			uploaded = true
			return nil
		},
		Exec: func(context.Context, string) error {
			executed = true
			return nil
		},
	})
	var exitErr ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 6 {
		t.Fatalf("err=%v, want exit 6", err)
	}
	if !strings.Contains(err.Error(), `tracked path "two.txt"`) {
		t.Fatalf("err=%v", err)
	}
	if uploaded || executed {
		t.Fatalf("upload=%t exec=%t, want no transfer callbacks", uploaded, executed)
	}
}

func TestRunDelegatedArchiveSyncRejectsMixedGitlinkConflictBeforeUpload(t *testing.T) {
	root := newDelegatedArchiveSyncRepo(t)
	setUnmergedIndexModes(t, root, "one.txt", "100644", "160000", "100644")
	uploaded := false
	executed := false

	_, _, err := RunDelegatedArchiveSync(context.Background(), DelegatedArchiveSyncRequest{
		Config:  baseConfig(),
		Repo:    Repo{Root: root},
		Workdir: "/workspace",
		Upload: func(context.Context, string, io.Reader) error {
			uploaded = true
			return nil
		},
		Exec: func(context.Context, string) error {
			executed = true
			return nil
		},
	})
	var exitErr ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 6 {
		t.Fatalf("err=%v, want exit 6", err)
	}
	if !strings.Contains(err.Error(), `tracked path "one.txt"`) ||
		!strings.Contains(err.Error(), "mixed file mode 100644 at stage 1") {
		t.Fatalf("err=%v", err)
	}
	if uploaded || executed {
		t.Fatalf("upload=%t exec=%t, want no transfer callbacks", uploaded, executed)
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

func TestRunDelegatedArchiveSyncSupportsProviderReplace(t *testing.T) {
	root := newDelegatedArchiveSyncRepo(t)
	cfg := baseConfig()
	cfg.Sync.Delete = true
	var commands []string
	var replacedStaging string
	var replacedWorkdir string

	_, _, err := RunDelegatedArchiveSync(context.Background(), DelegatedArchiveSyncRequest{
		Config:  cfg,
		Repo:    Repo{Root: root},
		Workdir: "/workspace",
		Suffix:  func() string { return "fixed" },
		Upload:  func(context.Context, string, io.Reader) error { return nil },
		Exec: func(_ context.Context, command string) error {
			commands = append(commands, command)
			return nil
		},
		Replace: func(_ context.Context, stagingDir, workdir string) error {
			replacedStaging = stagingDir
			replacedWorkdir = workdir
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if replacedStaging != "/.workspace.crabbox-sync-fixed" || replacedWorkdir != "/workspace" {
		t.Fatalf("replace staging=%q workdir=%q", replacedStaging, replacedWorkdir)
	}
	if strings.Contains(strings.Join(commands, "\n"), "mv '/.workspace.crabbox-sync-fixed' '/workspace'") {
		t.Fatalf("default replacement ran despite provider hook: %#v", commands)
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
