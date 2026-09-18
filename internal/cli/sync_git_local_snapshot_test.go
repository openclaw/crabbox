package cli

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

func newLocalGitSnapshotFixture(t *testing.T) (Repo, Config) {
	t.Helper()
	root := t.TempDir()
	runGit(t, root, "init", "-q")
	runGit(t, root, "config", "user.name", "Alice")
	runGit(t, root, "config", "user.email", "alice@example.com")
	for _, name := range []string{"clean.txt", "staged.txt", "deleted.txt", "cached-deleted.txt", "excluded.txt"} {
		mustWriteTestFile(t, filepath.Join(root, name), "base "+name+"\n")
	}
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-qm", "initial")
	return Repo{Root: root, Head: gitOutput(root, "rev-parse", "HEAD")}, baseConfig()
}

func TestLocalGitSeedSnapshotFullManifest(t *testing.T) {
	repo, cfg := newLocalGitSnapshotFixture(t)
	runGit(t, repo.Root, "checkout", "--detach", "-q")
	runGit(t, repo.Root, "config", "core.filemode", "false")
	runGit(t, repo.Root, "config", "core.autocrlf", "true")
	mustWriteTestFile(t, filepath.Join(repo.Root, ".gitattributes"), "*.txt text eol=crlf\n")
	mustWriteTestFile(t, filepath.Join(repo.Root, "staged.txt"), "staged bytes\r\n")
	runGit(t, repo.Root, "add", "staged.txt")
	mustWriteTestFile(t, filepath.Join(repo.Root, "untracked.txt"), "untracked bytes\r\n")
	if err := os.Remove(filepath.Join(repo.Root, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo.Root, "rm", "-q", "cached-deleted.txt")
	if runtime.GOOS != "windows" {
		if err := os.Chmod(filepath.Join(repo.Root, "clean.txt"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("clean.txt", filepath.Join(repo.Root, "link")); err != nil {
			t.Fatal(err)
		}
	}
	cfg.Sync.Excludes = append(cfg.Sync.Excludes, "excluded.txt")
	excludes, err := syncExcludes(repo.Root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := syncManifestFilteredRules(repo.Root, excludes, syncIncludes(cfg))
	if err != nil {
		t.Fatal(err)
	}
	manifest.Changed, manifest.ChangedBytes = changedPathSetBytes(repo.Root, append(slices.Clone(manifest.Files), manifest.Deleted...))
	manifest.OverlayFiles, manifest.OverlayBytes = overlayPathSetBytes(repo.Root, manifest.Files, manifest.Changed)
	snapshot, err := prepareLocalGitSeedSnapshot(context.Background(), repo, cfg, excludes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := snapshot.cleanup(); err != nil {
			t.Error(err)
		}
	})
	if !sameSyncManifest(snapshot.Manifest, manifest) || !sameSyncExcludeRules(snapshot.Excludes, excludes) {
		t.Fatalf("snapshot did not retain the complete manifest and excludes: %#v", snapshot.Manifest)
	}
	if !slices.Contains(snapshot.Manifest.Files, "clean.txt") || !slices.Contains(snapshot.Manifest.Deleted, "deleted.txt") || !slices.Contains(snapshot.Manifest.Deleted, "cached-deleted.txt") {
		t.Fatalf("missing clean file or deletion: %#v", snapshot.Manifest)
	}
	for _, rel := range manifest.Files {
		source, err := os.Lstat(filepath.Join(repo.Root, rel))
		if err != nil {
			t.Fatal(err)
		}
		copied, err := os.Lstat(filepath.Join(snapshot.Root, rel))
		if err != nil {
			t.Fatal(err)
		}
		if source.Mode() != copied.Mode() {
			t.Errorf("%s mode = %v, want %v", rel, copied.Mode(), source.Mode())
		}
		if source.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(filepath.Join(snapshot.Root, rel))
			if err != nil || target != "clean.txt" {
				t.Fatalf("copied symlink = %q, %v", target, err)
			}
			continue
		}
		want, err := os.ReadFile(filepath.Join(repo.Root, rel))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(snapshot.Root, rel))
		if err != nil || string(got) != string(want) {
			t.Errorf("%s contents = %q, %v; want %q", rel, got, err, want)
		}
	}
	for _, rel := range []string{".git", "deleted.txt", "cached-deleted.txt", "excluded.txt"} {
		if _, err := os.Lstat(filepath.Join(snapshot.Root, rel)); !os.IsNotExist(err) {
			t.Errorf("unexpected snapshot path %s: %v", rel, err)
		}
	}
	mustWriteTestFile(t, filepath.Join(repo.Root, "clean.txt"), "next edit\n")
	if content, err := os.ReadFile(filepath.Join(snapshot.Root, "clean.txt")); err != nil || string(content) != "base clean.txt\n" {
		t.Fatalf("accepted snapshot changed after a later source edit: %q, %v", content, err)
	}
}

func TestLocalGitSeedSnapshotLinkedWorktree(t *testing.T) {
	repo, cfg := newLocalGitSnapshotFixture(t)
	linked := filepath.Join(t.TempDir(), "linked")
	runGit(t, repo.Root, "worktree", "add", "--detach", "-q", linked, "HEAD")
	repo.Root = linked
	snapshot, err := prepareLocalGitSeedSnapshot(context.Background(), repo, cfg, SyncExcludeRules{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := snapshot.cleanup(); err != nil {
			t.Error(err)
		}
	}()
	if snapshot.Checkout.Head != repo.Head || snapshot.Checkout.IndexFingerprint == "" {
		t.Fatalf("unbound linked checkout: %#v", snapshot.Checkout)
	}
	if _, err := os.Lstat(filepath.Join(snapshot.Root, ".git")); !os.IsNotExist(err) {
		t.Fatalf("source administration was copied: %v", err)
	}
}

func TestLocalGitSeedSnapshotFingerprintFullFilesAndMetadata(t *testing.T) {
	repo, cfg := newLocalGitSnapshotFixture(t)
	excludes, err := syncExcludes(repo.Root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := syncManifestFilteredRules(repo.Root, excludes, nil)
	if err != nil {
		t.Fatal(err)
	}
	checkout, err := readLocalGitSeedCheckoutState(context.Background(), repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := localGitSeedSnapshotFingerprint(context.Background(), repo, cfg, manifest, excludes, checkout)
	if err != nil {
		t.Fatal(err)
	}
	checkDifferent := func(t *testing.T, cfg Config, manifest SyncManifest, excludes SyncExcludeRules, checkout gitOverlayCheckoutState) {
		t.Helper()
		got, err := localGitSeedSnapshotFingerprint(context.Background(), repo, cfg, manifest, excludes, checkout)
		if err != nil || got == baseline {
			t.Fatalf("fingerprint did not change: %q, %v", got, err)
		}
	}
	t.Run("clean file contents", func(t *testing.T) {
		mustWriteTestFile(t, filepath.Join(repo.Root, "clean.txt"), "next clean.txt\n")
		checkDifferent(t, cfg, manifest, excludes, checkout)
		mustWriteTestFile(t, filepath.Join(repo.Root, "clean.txt"), "base clean.txt\n")
	})
	t.Run("file mode", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("executable permission bits are POSIX metadata")
		}
		path := filepath.Join(repo.Root, "clean.txt")
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
		checkDifferent(t, cfg, manifest, excludes, checkout)
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("symlink identity", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlink creation requires Windows host privileges")
		}
		path := filepath.Join(repo.Root, "link")
		if err := os.Symlink("clean.txt", path); err != nil {
			t.Fatal(err)
		}
		withLink := manifest
		withLink.Files = append(slices.Clone(manifest.Files), "link")
		before, err := localGitSeedSnapshotFingerprint(context.Background(), repo, cfg, withLink, excludes, checkout)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("./clean.txt", path); err != nil {
			t.Fatal(err)
		}
		after, err := localGitSeedSnapshotFingerprint(context.Background(), repo, cfg, withLink, excludes, checkout)
		if err != nil || before == after {
			t.Fatalf("symlink identity did not change fingerprint: %q, %v", after, err)
		}
	})
	t.Run("head", func(t *testing.T) {
		changed := checkout
		changed.Head = "another-head"
		checkDifferent(t, cfg, manifest, excludes, changed)
	})
	t.Run("index", func(t *testing.T) {
		changed := checkout
		changed.IndexFingerprint = "another-index"
		checkDifferent(t, cfg, manifest, excludes, changed)
	})
	t.Run("deletions", func(t *testing.T) {
		changed := manifest
		changed.Deleted = []string{"removed.txt"}
		checkDifferent(t, cfg, changed, excludes, checkout)
	})
	t.Run("excludes", func(t *testing.T) {
		checkDifferent(t, cfg, manifest, excludes.append([]string{"generated/"}, syncExcludeConfigured), checkout)
	})
	t.Run("sync settings", func(t *testing.T) {
		changed := cfg
		changed.Sync.Delete = !cfg.Sync.Delete
		checkDifferent(t, changed, manifest, excludes, checkout)
	})
}

func TestGitSnapshotPolicyPreservesOverlaySelection(t *testing.T) {
	fixture := newGitOverlayFixture(t)
	mustWriteTestFile(t, filepath.Join(fixture.root, "unstaged.txt"), "changed\n")
	manifest, excludes := fixture.manifest(t)
	snapshot, err := prepareGitOverlaySnapshot(context.Background(), fixture.repo, fixture.cfg, excludes, nil, fixture.plan)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := snapshot.cleanup(); err != nil {
			t.Error(err)
		}
	}()
	if !sameSyncManifest(snapshot.Manifest, manifest) {
		t.Fatalf("overlay manifest changed: %#v", snapshot.Manifest)
	}
	if _, err := os.Stat(filepath.Join(snapshot.Root, "clean.txt")); !os.IsNotExist(err) {
		t.Fatalf("overlay unexpectedly copied clean tracked file: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(snapshot.Root, "unstaged.txt")); err != nil || string(got) != "changed\n" {
		t.Fatalf("overlay copied contents = %q, %v", got, err)
	}
	if want, err := syncFingerprintForManifest(context.Background(), fixture.repo, fixture.cfg, manifest, excludes, fixture.plan); err != nil || snapshot.Fingerprint != want {
		t.Fatalf("overlay fingerprint = %q, want %q, %v", snapshot.Fingerprint, want, err)
	}
}
