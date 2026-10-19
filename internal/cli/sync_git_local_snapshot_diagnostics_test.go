package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalGitSnapshotInputMismatch(t *testing.T) {
	baseline := localGitSnapshotInputs{head: "head", root: "root", ignore: "ignore", index: []byte("index"), files: []byte("files"), deleted: []byte("deleted"), cached: []byte("cached")}
	if got := baseline.mismatch(baseline); got != "" {
		t.Fatalf("unchanged inputs: %s", got)
	}
	for _, test := range []struct {
		check  string
		change func(*localGitSnapshotInputs)
	}{
		{"head", func(v *localGitSnapshotInputs) { v.head = "changed" }},
		{"repository_root", func(v *localGitSnapshotInputs) { v.root = "changed" }},
		{"global_ignore", func(v *localGitSnapshotInputs) { v.ignore = "changed" }},
		{"sparse_checkout", func(v *localGitSnapshotInputs) { v.sparse = true }},
		{"index", func(v *localGitSnapshotInputs) { v.index = []byte("changed") }},
		{"file_inventory", func(v *localGitSnapshotInputs) { v.files = []byte("changed") }},
		{"deleted_inventory", func(v *localGitSnapshotInputs) { v.deleted = []byte("changed") }},
		{"cached_deletions", func(v *localGitSnapshotInputs) { v.cached = []byte("changed") }},
	} {
		t.Run(test.check, func(t *testing.T) {
			changed := baseline
			test.change(&changed)
			if got := baseline.mismatch(changed); got != test.check {
				t.Fatalf("mismatch=%q, want %q", got, test.check)
			}
		})
	}
}

func TestLocalGitSeedSnapshotDriftDiagnostics(t *testing.T) {
	for _, test := range []struct{ phase, stage, check string }{
		{"after_lstat", "copy", "read_identity"},
		{"snapshot_fingerprinted", "acceptance", "live_fingerprint"},
		{"live_fingerprinted", "acceptance", "accepted_fingerprint"},
		{"before_acceptance_inputs", "acceptance", "file_inventory"},
		{"manifest_built", "acceptance", "accepted_bytes"},
	} {
		t.Run(test.phase, func(t *testing.T) {
			repo, cfg := newLocalGitSnapshotFixture(t)
			var roots []string
			changed := 0
			snapshot, err := prepareLocalGitSeedSnapshotWithHook(context.Background(), repo, cfg, func(phase string, attempt int, root string) {
				if phase == "snapshot_created" {
					roots = append(roots, root)
				}
				if phase != test.phase || changed == attempt {
					return
				}
				changed = attempt
				if phase == "before_acceptance_inputs" {
					mustWriteTestFile(t, filepath.Join(repo.Root, fmt.Sprintf("new-%d.txt", attempt)), "private content")
					return
				}
				for _, name := range []string{"clean.txt", "staged.txt", "deleted.txt", "cached-deleted.txt", "excluded.txt"} {
					mustWriteTestFile(t, filepath.Join(repo.Root, name), strings.Repeat("private content", attempt+1))
				}
			})
			if !errors.Is(err, errSourceSnapshotDrift) || snapshot.Root != "" || len(roots) != gitOverlaySnapshotMaxAttempts {
				t.Fatalf("attempts=%d retained=%t error=%v", len(roots), snapshot.Root != "", err)
			}
			for attempt := 1; attempt <= gitOverlaySnapshotMaxAttempts; attempt++ {
				want := fmt.Sprintf("attempt=%d stage=%s check=%s", attempt, test.stage, test.check)
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("missing %q in %v", want, err)
				}
			}
			if len(err.Error()) > 512 || strings.Contains(err.Error(), repo.Root) || strings.Contains(err.Error(), "private content") || strings.Contains(err.Error(), ".txt") {
				t.Fatalf("unbounded or sensitive diagnostic: %v", err)
			}
			assertLocalSnapshotRootsCleaned(t, roots)
		})
	}
}

func TestLocalGitSeedSnapshotRetainsDifferentAttemptChecks(t *testing.T) {
	repo, cfg := newLocalGitSnapshotFixture(t)
	var roots []string
	changed := false
	_, err := prepareLocalGitSeedSnapshotWithHook(context.Background(), repo, cfg, func(phase string, attempt int, root string) {
		if phase == "snapshot_created" {
			roots = append(roots, root)
		}
		if attempt == 1 && phase == "after_lstat" && !changed {
			changed = true
			for _, name := range []string{"clean.txt", "staged.txt", "deleted.txt", "cached-deleted.txt", "excluded.txt"} {
				mustWriteTestFile(t, filepath.Join(repo.Root, name), "copy drift")
			}
		}
		if attempt == 2 && phase == "snapshot_fingerprinted" {
			mustWriteTestFile(t, filepath.Join(repo.Root, "clean.txt"), "fingerprint drift")
		}
		if attempt == 3 && phase == "before_acceptance_inputs" {
			mustWriteTestFile(t, filepath.Join(repo.Root, "new.txt"), "inventory drift")
		}
	})
	for _, want := range []string{"attempt=1 stage=copy check=read_identity", "attempt=2 stage=acceptance check=live_fingerprint", "attempt=3 stage=acceptance check=file_inventory"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %q in %v", want, err)
		}
	}
	assertLocalSnapshotRootsCleaned(t, roots)
}

func TestLocalGitSeedSnapshotDriftRecoveryAndCancellation(t *testing.T) {
	for _, cancelCopy := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%t", cancelCopy), func(t *testing.T) {
			repo, cfg := newLocalGitSnapshotFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var roots []string
			snapshot, err := prepareLocalGitSeedSnapshotWithHook(ctx, repo, cfg, func(phase string, attempt int, root string) {
				if phase == "snapshot_created" {
					roots = append(roots, root)
				}
				if attempt == 1 && phase == "snapshot_fingerprinted" {
					mustWriteTestFile(t, filepath.Join(repo.Root, "clean.txt"), "next bytes")
					if cancelCopy {
						cancel()
					}
				}
			})
			if cancelCopy {
				if !errors.Is(err, context.Canceled) || len(roots) != 1 || snapshot.Root != "" {
					t.Fatalf("cancellation retried or lost cause: roots=%d retained=%t error=%v", len(roots), snapshot.Root != "", err)
				}
			} else {
				if err != nil || len(roots) != 2 || snapshot.Root == "" {
					t.Fatalf("recovery failed: roots=%d error=%v", len(roots), err)
				}
				if err := snapshot.cleanup(); err != nil {
					t.Fatal(err)
				}
			}
			assertLocalSnapshotRootsCleaned(t, roots)
		})
	}
}

func TestLocalGitSeedSnapshotDriftCleanupFailureRetainsOwnership(t *testing.T) {
	repo, cfg := newLocalGitSnapshotFixture(t)
	var root, moved string
	builds := 0
	snapshot, err := prepareLocalGitSeedSnapshotWithHook(context.Background(), repo, cfg, func(phase string, _ int, snapshotRoot string) {
		if phase == "manifest_built" {
			builds++
		}
		if phase != "snapshot_fingerprinted" {
			return
		}
		root, moved = snapshotRoot, snapshotRoot+"-moved"
		if err := os.Rename(root, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		mustWriteTestFile(t, filepath.Join(repo.Root, "clean.txt"), "force drift")
	})
	if root == "" {
		t.Fatal("snapshot was not created")
	}
	// Restore the deliberately replaced owned path before retrying cleanup.
	defer func() {
		if err := os.Remove(root); err != nil {
			t.Error(err)
		}
		if err := os.Rename(moved, root); err != nil {
			t.Error(err)
		}
		if err := snapshot.cleanup(); err != nil {
			t.Error(err)
		}
	}()
	if !errors.Is(err, errSourceSnapshotDrift) || builds != 1 || snapshot.Root != root || snapshot.cleanupRoot == nil {
		t.Fatalf("cleanup failure retried or lost ownership: builds=%d retained=%t error=%v", builds, snapshot.Root != "", err)
	}
	var failure *localGitSnapshotFailure
	if !errors.As(err, &failure) || !strings.Contains(err.Error(), "after 3 attempts") || !strings.Contains(err.Error(), root) {
		t.Fatalf("cleanup diagnostic missing: %v", err)
	}
}

func TestLocalGitSnapshotFailureRedactsCauseAndPreservesIdentity(t *testing.T) {
	private := &os.PathError{Op: "read", Path: "/private/source/credential.txt", Err: os.ErrPermission}
	cause := errors.Join(errSourceSnapshotDrift, private, context.Canceled, errors.New(strings.Repeat("secret-content", 1000)))
	err := &localGitSnapshotFailure{3, "copy", "source_identity", cause}
	if !errors.Is(err, errSourceSnapshotDrift) || !errors.Is(err, os.ErrPermission) || !errors.Is(err, context.Canceled) {
		t.Fatalf("cause identity lost: %v", err)
	}
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) || pathErr != private {
		t.Fatal("typed cause lost")
	}
	if len(err.Error()) > 160 || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "credential") || strings.Contains(err.Error(), "secret-content") {
		t.Fatalf("sensitive or unbounded diagnostic: %v", err)
	}
}

func assertLocalSnapshotRootsCleaned(t *testing.T, roots []string) {
	t.Helper()
	for _, root := range roots {
		if _, err := os.Lstat(root); !os.IsNotExist(err) {
			t.Fatalf("discarded snapshot remains: %v", err)
		}
	}
}
