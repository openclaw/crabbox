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
	"testing/synctest"
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

	phases, total, err := (ArchiveWorkspace{
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
	}).Sync(context.Background(), prepared)
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
	if total != 98*time.Millisecond {
		t.Fatalf("total=%s, want 77ms transfer plus 21ms saved preparation", total)
	}
	if _, err := os.Stat(archivePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prepared archive remains at %q: %v", archivePath, err)
	}
	if err := prepared.Close(); err != nil {
		t.Fatalf("idempotent second close: %v", err)
	}
}

func TestRunDelegatedArchiveSyncLocalPreparationCountsTimingOnce(t *testing.T) {
	root := newDelegatedArchiveSyncRepo(t)
	var stderr bytes.Buffer
	calls := 0
	phases, total, err := (ArchiveWorkspace{
		Config: baseConfig(), Repo: Repo{Root: root}, Workdir: "/workspace", Stderr: &stderr,
		Now: func() time.Time {
			calls++
			return time.Unix(0, int64(calls)*int64(7*time.Millisecond))
		},
		Upload: func(context.Context, string, io.Reader) error { return nil },
		Exec:   func(context.Context, string) error { return nil },
	}).Sync(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if total != 119*time.Millisecond || phases[len(phases)-1].Ms != 119 {
		t.Fatalf("phases=%v total=%s, want local elapsed 119ms without adding saved preparation", phases, total)
	}
	for _, phase := range phases[:3] {
		if phase.Ms != 7 {
			t.Fatalf("preparation phase=%v, want 7ms", phase)
		}
	}
	if got := strings.Count(stderr.String(), "sync candidate:"); got != 1 {
		t.Fatalf("preflight count=%d stderr=%q", got, stderr.String())
	}
}

func TestRunDelegatedArchiveSyncPreservesContinuousLocalDeadline(t *testing.T) {
	root := newDelegatedArchiveSyncRepo(t)
	synctest.Test(t, func(t *testing.T) {
		cfg := baseConfig()
		cfg.Sync.Timeout = 5 * time.Second
		calls := 0
		var archiveStart time.Time
		var transferCtx context.Context
		_, _, err := (ArchiveWorkspace{
			Config: cfg, Repo: Repo{Root: root}, Workdir: "/workspace",
			Now: func() time.Time {
				calls++
				switch calls {
				case 3:
					time.Sleep(20 * time.Second) // Planning is outside the transfer budget.
				case 6:
					archiveStart = time.Now()
					time.Sleep(2 * time.Second)
				}
				return time.Unix(0, 0) // Reporting time must not restart the real deadline.
			},
			Upload: func(ctx context.Context, _ string, body io.Reader) error {
				transferCtx = ctx
				deadline, ok := ctx.Deadline()
				if !ok || !deadline.Equal(archiveStart.Add(5*time.Second)) || time.Until(deadline) != 3*time.Second {
					t.Fatalf("deadline=%v archive start=%v remaining=%s", deadline, archiveStart, time.Until(deadline))
				}
				if _, ok := body.(*os.File); !ok {
					t.Fatalf("upload body=%T, want seekable archive file", body)
				}
				time.Sleep(3 * time.Second)
				synctest.Wait()
				return ctx.Err()
			},
			Exec: func(ctx context.Context, command string) error {
				if ctx.Err() != nil || !strings.HasPrefix(command, "rm -f ") {
					t.Fatalf("cleanup context=%v command=%q", ctx.Err(), command)
				}
				return nil
			},
		}).Sync(context.Background())
		if !errors.Is(err, context.DeadlineExceeded) || transferCtx == nil {
			t.Fatalf("sync err=%v transfer=%v", err, transferCtx)
		}
	})
}

func TestRunDelegatedArchiveSyncPreparedDeadlineBudget(t *testing.T) {
	root := newDelegatedArchiveSyncRepo(t)
	for _, test := range []struct {
		name       string
		timeout    time.Duration
		archiving  time.Duration
		parent     time.Duration
		remaining  time.Duration
		noDeadline bool
	}{
		{name: "allocation gap excluded", timeout: 5 * time.Second, archiving: 2 * time.Second, remaining: 3 * time.Second},
		{name: "parent wins", timeout: 5 * time.Second, parent: time.Second, remaining: time.Second},
		{name: "disabled retains parent", parent: time.Second, remaining: time.Second},
		{name: "disabled without parent", noDeadline: true},
		{name: "exhausted", timeout: 5 * time.Second, archiving: 5 * time.Second},
		{name: "overdrawn", timeout: 5 * time.Second, archiving: 6 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			prepared, err := PrepareDelegatedArchive(context.Background(), DelegatedArchivePreparationRequest{Config: baseConfig(), Repo: Repo{Root: root}})
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Close()
			prepared.ArchiveDuration = test.archiving
			synctest.Test(t, func(t *testing.T) {
				time.Sleep(time.Hour)
				ctx := context.Background()
				if test.parent > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, test.parent)
					defer cancel()
				}
				cfg := baseConfig()
				cfg.Sync.Timeout = test.timeout
				var uploaded bool
				_, _, err := (ArchiveWorkspace{
					Config: cfg, Workdir: "/workspace",
					Upload: func(ctx context.Context, _ string, _ io.Reader) error {
						uploaded = true
						deadline, ok := ctx.Deadline()
						if ok == test.noDeadline || (ok && time.Until(deadline) != test.remaining) {
							t.Fatalf("deadline=%v present=%v remaining=%s, want %s", deadline, ok, time.Until(deadline), test.remaining)
						}
						if ok {
							time.Sleep(test.remaining)
							synctest.Wait()
						}
						return ctx.Err()
					},
					Exec: func(ctx context.Context, _ string) error { return ctx.Err() },
				}).Sync(ctx, prepared)
				if !uploaded || (test.noDeadline && err != nil) || (!test.noDeadline && !errors.Is(err, context.DeadlineExceeded)) {
					t.Fatalf("uploaded=%v err=%v", uploaded, err)
				}
			})
		})
	}
}

func TestRunDelegatedArchiveSyncClosesPreparedArchiveOnEveryFailurePath(t *testing.T) {
	root := newDelegatedArchiveSyncRepo(t)
	tests := []struct {
		name      string
		configure func(*ArchiveWorkspace)
	}{
		{name: "missing callbacks", configure: func(req *ArchiveWorkspace) { req.Upload = nil }},
		{name: "missing workdir", configure: func(req *ArchiveWorkspace) { req.Workdir = "" }},
		{name: "upload", configure: func(req *ArchiveWorkspace) {
			req.Upload = func(context.Context, string, io.Reader) error { return errors.New("upload failed") }
		}},
		{name: "prepare", configure: func(req *ArchiveWorkspace) {
			req.Exec = func(_ context.Context, command string) error {
				if strings.Contains(command, "mkdir -p ") {
					return errors.New("prepare failed")
				}
				return nil
			}
		}},
		{name: "extract", configure: func(req *ArchiveWorkspace) {
			req.Exec = func(_ context.Context, command string) error {
				if strings.HasPrefix(command, "tar -xzf ") {
					return errors.New("extract failed")
				}
				return nil
			}
		}},
		{name: "replace", configure: func(req *ArchiveWorkspace) {
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
			req := ArchiveWorkspace{
				Config: baseConfig(), Repo: Repo{Root: root}, Workdir: "/workspace", Stderr: io.Discard,
				Upload: func(context.Context, string, io.Reader) error { return nil },
				Exec:   func(context.Context, string) error { return nil },
			}
			test.configure(&req)
			if _, _, err := (req).Sync(context.Background(), prepared); err == nil {
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

func TestRunDelegatedArchiveSyncClosesLocalArchiveOnFailure(t *testing.T) {
	root := newDelegatedArchiveSyncRepo(t)
	for _, stage := range []string{"upload", "extract", "replace"} {
		t.Run(stage, func(t *testing.T) {
			failure := errors.New(stage + " failed")
			var archive *os.File
			cleanups := 0
			_, _, err := (ArchiveWorkspace{
				Config: baseConfig(), Repo: Repo{Root: root}, Workdir: "/workspace",
				Upload: func(_ context.Context, _ string, body io.Reader) error {
					var ok bool
					archive, ok = body.(*os.File)
					if !ok {
						t.Fatalf("upload body=%T", body)
					}
					if stage == "upload" {
						return failure
					}
					return nil
				},
				Exec: func(_ context.Context, command string) error {
					if strings.HasPrefix(command, "rm -f ") {
						cleanups++
					}
					if stage == "extract" && strings.HasPrefix(command, "tar -xzf ") {
						return failure
					}
					return nil
				},
				Replace: func(context.Context, string, string) error { return failure },
			}).Sync(context.Background())
			if !errors.Is(err, failure) || archive == nil || cleanups != 1 {
				t.Fatalf("err=%v archive=%v cleanups=%d", err, archive, cleanups)
			}
			if _, err := archive.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("archive not closed: %v", err)
			}
			if _, err := os.Stat(archive.Name()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("archive not removed: %v", err)
			}
		})
	}
}

func TestRunDelegatedArchiveSyncInvalidLocalRequestDoesNotPrepare(t *testing.T) {
	for _, stage := range []string{"callbacks", "workdir"} {
		t.Run(stage, func(t *testing.T) {
			req := ArchiveWorkspace{
				Workdir: "/workspace",
				Now:     func() time.Time { t.Fatal("invalid request started preparation"); return time.Time{} },
				Upload:  func(context.Context, string, io.Reader) error { t.Fatal("unexpected upload"); return nil },
				Exec:    func(context.Context, string) error { t.Fatal("unexpected remote command"); return nil },
			}
			if stage == "callbacks" {
				req.Upload = nil
			} else {
				req.Workdir = ""
			}
			if _, _, err := (req).Sync(context.Background()); err == nil {
				t.Fatal("invalid request succeeded")
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

	phases, _, err := (ArchiveWorkspace{
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
	}).Sync(context.Background())
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

	_, _, err := (ArchiveWorkspace{
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
	}).Sync(context.Background())
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

	_, _, err := (ArchiveWorkspace{
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
	}).Sync(context.Background())
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

	_, _, err := (ArchiveWorkspace{
		Config:  cfg,
		Repo:    Repo{Root: root},
		Workdir: "/workspace",
		Stderr:  &stderr,
		Upload: func(context.Context, string, io.Reader) error {
			uploaded = true
			return nil
		},
		Exec: func(context.Context, string) error { return nil },
	}).Sync(context.Background())
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

	_, _, err := (ArchiveWorkspace{
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
	}).Sync(context.Background())
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
	var stderr bytes.Buffer

	_, _, err := (ArchiveWorkspace{
		Config:   baseConfig(),
		Repo:     Repo{Root: root},
		Workdir:  "/workspace",
		Provider: "test-provider",
		Stderr:   &stderr,
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
			if strings.HasPrefix(command, "rm -f ") {
				return exec.CommandContext(callCtx, "sh", "-ec", "rm() { return 7; }; "+command).Run()
			}
			return nil
		},
	}).Sync(ctx)
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("err=%v", err)
	}
	if !cleanupContextActive || calls < 3 {
		t.Fatalf("cleanup active=%t calls=%d", cleanupContextActive, calls)
	}
	if !strings.Contains(stderr.String(), "warning: test-provider sync cleanup failed: exit status 7") {
		t.Fatalf("missing cleanup warning: %s", stderr.String())
	}
}

func TestArchiveWorkspacePreparesBeforeBindingTransport(t *testing.T) {
	root := newDelegatedArchiveSyncRepo(t)
	cfg := baseConfig()
	cfg.Sync.FailFiles = 1
	req := RunRequest{Repo: Repo{Root: root}, ForceSyncLarge: true}
	workspace := NewArchiveWorkspace(cfg, Runtime{}, req, "fixture-provider", "/workspace/project")
	// Preparation must need neither a resource nor its transport. It carries the
	// same guardrail override and file selection into the later upload.
	archive, err := workspace.PrepareArchive(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	if !strings.HasPrefix(filepath.Base(archive.File.Name()), "crabbox-fixture-provider-sync-") {
		t.Fatalf("unexpected archive name: %s", archive.File.Name())
	}
	if err := os.WriteFile(filepath.Join(root, "one.txt"), []byte("later checkout\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	uploaded := false
	workspace.Upload = func(_ context.Context, _ string, body io.Reader) error {
		uploaded = true
		gz, err := gzip.NewReader(body)
		if err != nil {
			return err
		}
		defer gz.Close()
		reader := tar.NewReader(gz)
		for {
			header, err := reader.Next()
			if errors.Is(err, io.EOF) {
				t.Fatal("one.txt missing from prepared snapshot")
			}
			if err != nil {
				return err
			}
			if header.Name == "one.txt" {
				data, err := io.ReadAll(reader)
				if string(data) != "one\n" {
					t.Fatalf("uploaded changed checkout instead of prepared snapshot: %q", data)
				}
				return err
			}
		}
	}
	workspace.Exec = func(context.Context, string) error { return nil }
	phases, _, err := workspace.Sync(t.Context(), archive)
	if err != nil || !uploaded {
		t.Fatalf("upload=%v err=%v", uploaded, err)
	}
	if phases[len(phases)-1].Name != "fixture_provider_sync" {
		t.Fatalf("unexpected phase: %+v", phases)
	}
	if _, err := os.Stat(archive.File.Name()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("archive not removed after transfer: %v", err)
	}
}

func TestArchiveWorkspaceValidatesRemotePathBeforeOperations(t *testing.T) {
	invalid := errors.New("protected workspace")
	for _, sync := range []bool{false, true} {
		workspace := NewArchiveWorkspace(baseConfig(), Runtime{}, RunRequest{}, "fixture", "/")
		workspace.CleanWorkdir = func(string) (string, error) { return "", invalid }
		workspace.Upload = func(context.Context, string, io.Reader) error { t.Fatal("upload on invalid path"); return nil }
		workspace.Exec = func(context.Context, string) error { t.Fatal("exec on invalid path"); return nil }
		var err error
		if sync {
			_, _, err = workspace.Sync(t.Context())
		} else {
			err = workspace.Ensure(t.Context())
		}
		if !errors.Is(err, invalid) {
			t.Fatalf("sync=%v error=%v", sync, err)
		}
	}
	workspace := NewArchiveWorkspace(baseConfig(), Runtime{}, RunRequest{}, "fixture", "/workspace//project")
	workspace.CleanWorkdir = func(string) (string, error) { return "/workspace/project", nil }
	var command string
	workspace.Exec = func(_ context.Context, cmd string) error { command = cmd; return nil }
	if err := workspace.Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	if command != "mkdir -p "+ShellQuote("/workspace/project") {
		t.Fatalf("workspace was not normalized: %q", command)
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
