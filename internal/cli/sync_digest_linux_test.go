package cli

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestLocalGitSeedSnapshotDetectsDirtyMmapEdit(t *testing.T) {
	repo, cfg := newLocalGitSnapshotFixture(t)
	path := filepath.Join(repo.Root, "clean.txt")
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	mapped, err := syscall.Mmap(int(file.Fd()), 0, len("base clean.txt\n"), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Munmap(mapped)
	// Fault the page writable before observation. Subsequent writes may leave
	// the metadata untouched, so even a matching ctime is not content proof.
	mapped[0] = 'b'
	before, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	changed := false
	snapshot, err := prepareLocalGitSeedSnapshotWithHook(context.Background(), repo, cfg, func(phase string, _ int, _ string) {
		if phase != "live_fingerprinted" || changed {
			return
		}
		changed = true
		copy(mapped, "edit clean.txt\n")
		after, err := file.Stat()
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("mmap preserved mtime=%t ctime=%t", before.ModTime().Equal(after.ModTime()), sameSyncDigestChangeTime(before, after))
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
		t.Fatalf("accepted stale mmap contents: %q, %v", got, err)
	}
}
