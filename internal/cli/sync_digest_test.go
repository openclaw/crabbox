package cli

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestSyncDigestRechecksSource(t *testing.T) {
	for _, mutation := range []string{"same-size preserved-mtime", "size", "inode", "mode"} {
		t.Run(mutation, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "file")
			mustWriteTestFile(t, path, "original")
			info, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			ctx := withSyncDigests(context.Background())
			before, err := sourceFileDigest(ctx, path, info)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := sourceFileDigest(ctx, path, info); err != nil || got != before {
				t.Fatalf("unchanged digest: %x, %v", got, err)
			}
			_, supported := syncDigestChangeTime(info)
			if supported && syncDigests(ctx).hashes != 1 {
				t.Fatal("unchanged source was hashed more than once")
			}
			time.Sleep(time.Millisecond)
			want := "modified"
			switch mutation {
			case "size":
				want = "a longer replacement"
			case "inode":
				if err := os.Rename(path, path+".old"); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if runtime.GOOS == "windows" {
					t.Skip("Windows permission bits are not POSIX modes")
				}
				want = "original"
				if err := os.Chmod(path, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if mutation != "mode" {
				mustWriteTestFile(t, path, want)
			}
			if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
			current, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := syncDigests(ctx).lookup(path, current); ok {
				t.Fatal("changed stat key reused the digest")
			}
			got, err := sourceFileDigest(ctx, path, current)
			if err != nil || got != sha256.Sum256([]byte(want)) {
				t.Fatalf("changed digest: %x, %v", got, err)
			}
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			if _, err := sourceFileDigest(canceled, path, current); !errors.Is(err, context.Canceled) {
				t.Fatalf("cached digest ignored cancellation: %v", err)
			}
		})
	}
}

func TestLocalGitSeedSnapshotDetectsPreservedMtimeEdit(t *testing.T) {
	repo, cfg := newLocalGitSnapshotFixture(t)
	path := filepath.Join(repo.Root, "clean.txt")
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := false
	snapshot, err := prepareLocalGitSeedSnapshotWithHook(context.Background(), repo, cfg, func(phase string, _ int, _ string) {
		if phase != "live_fingerprinted" || changed {
			return
		}
		changed = true
		time.Sleep(time.Millisecond)
		mustWriteTestFile(t, path, "edit clean.txt\n")
		if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := snapshot.cleanup(); err != nil {
			t.Error(err)
		}
	})
	got, err := os.ReadFile(filepath.Join(snapshot.Root, "clean.txt"))
	if err != nil || string(got) != "edit clean.txt\n" {
		t.Fatalf("accepted stale bytes after preserved-mtime edit: %q, %v", got, err)
	}
}

func TestLocalGitSeedSnapshotAvoidsRedundantSourceRead(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("platform does not expose the cache change timestamp")
	}
	repo, cfg := newLocalGitSnapshotFixture(t)
	counter := &sourceReadCounter{}
	ctx := context.WithValue(context.Background(), sourceReadCounterKey{}, counter)
	snapshot, err := prepareLocalGitSeedSnapshot(ctx, repo, cfg, SyncExcludeRules{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := snapshot.cleanup(); err != nil {
			t.Error(err)
		}
	})
	// Copy/hash the source, independently hash the snapshot, and read the live
	// bytes again for acceptance. Only the intermediate fingerprint is cached.
	if got, want := counter.bytes.Load(), 3*snapshot.Manifest.Bytes; got != want {
		t.Fatalf("logical bytes read = %d, want %d (copy + snapshot + final live read)", got, want)
	}
}

func TestSourceSnapshotRejectsChangedFinishedDirectory(t *testing.T) {
	source, destination := t.TempDir(), t.TempDir()
	mustWriteTestFile(t, filepath.Join(source, "a", "first"), "one")
	mustWriteTestFile(t, filepath.Join(source, "b", "second"), "two")
	visits := 0
	err := copySourceSnapshotWithHook(context.Background(), source, destination, []string{"a/first", "b/second"}, 1, func(phase string, _ int, _ string) {
		if phase != "after_lstat" {
			return
		}
		visits++
		if visits == 2 {
			if err := os.Rename(filepath.Join(destination, "a"), filepath.Join(destination, "old-a")); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(destination, "a"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	})
	if err == nil {
		t.Fatal("accepted a replaced directory outside the current file's ancestry")
	}
}
