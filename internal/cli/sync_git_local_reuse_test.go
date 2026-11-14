package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func reuseFixture(t *testing.T) (gitLocalSeedPlan, []byte) {
	t.Helper()
	plan, data, _ := localReceiverFixture(t)
	plan.CacheRoot = filepath.Join(t.TempDir(), "git-seeds")
	return plan, data
}

func mustNotCreateGit(t *testing.T, workdir string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(workdir, ".git")); !os.IsNotExist(err) {
		t.Fatalf("probe miss must not publish metadata: %v", err)
	}
	if entries, _ := filepath.Glob(filepath.Join(workdir, ".crabbox-local-git.*")); len(entries) != 0 {
		t.Fatalf("staging residue: %v", entries)
	}
}

func TestGitLocalReceiverProbeMissLeavesWorkspaceUntouched(t *testing.T) {
	plan, _ := reuseFixture(t)
	workdir := t.TempDir()
	if out, err := runLocalReceiver(t, remoteGitLocalSeedProbe(workdir, plan), nil); err == nil || strings.Contains(string(out), "reused-") {
		t.Fatalf("empty host reported reuse: %q %v", out, err)
	}
	mustNotCreateGit(t, workdir)
}

func TestGitLocalReceiverProbeReplacesWorkspaceFromCache(t *testing.T) {
	plan, data := reuseFixture(t)
	workdir := t.TempDir()
	requireLocalReceiver(t, remoteGitLocalSeed(workdir, plan), data)
	requireLocalReceiver(t, remoteGitLocalSeedFinalize(workdir, plan), nil)
	mustWriteTestFile(t, filepath.Join(workdir, ".git/crabbox/git-hydrate-base"), "stale")
	mustWriteTestFile(t, filepath.Join(workdir, "accepted.txt"), "dirty edit after the first sync\n")
	out := requireLocalReceiver(t, remoteGitLocalSeedProbe(workdir, plan), nil)
	if strings.TrimSpace(string(out)) != "reused-cache" {
		t.Fatalf("probe output: %q", out)
	}
	for _, name := range []string{"crabbox-local-complete", "crabbox/sync-fingerprint", "crabbox/git-hydrate-base"} {
		if _, err := os.Stat(filepath.Join(workdir, ".git", name)); !os.IsNotExist(err) {
			t.Errorf("reuse must clear %s like an import: %v", name, err)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(workdir, "accepted.txt")); string(got) != "dirty edit after the first sync\n" {
		t.Fatalf("working file changed: %q", got)
	}
	requireLocalReceiver(t, remoteGitLocalSeedFinalize(workdir, plan), nil)
	if out := requireLocalReceiver(t, remoteGitLocalSeedFingerprint(workdir, plan), nil); string(out) != plan.Fingerprint {
		t.Fatalf("fingerprint after reuse: %q", out)
	}
}

func TestGitLocalReceiverProbeRestoresHostCacheForNewWorkspace(t *testing.T) {
	plan, data := reuseFixture(t)
	first := t.TempDir()
	requireLocalReceiver(t, remoteGitLocalSeed(first, plan), data)
	entry := filepath.Join(plan.CacheRoot, plan.Digest+".git")
	for _, name := range []string{"index", "crabbox", "crabbox-local-complete"} {
		if _, err := os.Lstat(filepath.Join(entry, name)); !os.IsNotExist(err) {
			t.Errorf("cache must not carry %s: %v", name, err)
		}
	}
	second := t.TempDir()
	mustWriteTestFile(t, filepath.Join(second, ".crabbox/sync-manifest"), "old.txt\x00")
	out := requireLocalReceiver(t, remoteGitLocalSeedProbe(second, plan), nil)
	if strings.TrimSpace(string(out)) != "reused-cache" {
		t.Fatalf("probe output: %q", out)
	}
	if gitOutput(second, "rev-parse", "HEAD") != plan.Head || gitOutput(second, "write-tree") != plan.Tree {
		t.Fatal("restored HEAD/index identity mismatch")
	}
	if got, err := os.ReadFile(filepath.Join(second, ".git/crabbox/sync-manifest")); err != nil || string(got) != "old.txt\x00" {
		t.Fatalf("prior manifest lost: %q %v", got, err)
	}
	requireLocalReceiver(t, remoteGitLocalSeedFinalize(second, plan), nil)
	if out := requireLocalReceiver(t, remoteGitLocalSeedFingerprint(second, plan), nil); string(out) != plan.Fingerprint {
		t.Fatalf("fingerprint after cache restore: %q", out)
	}
	// Removing the first workspace must not damage the second (hardlink safety).
	if err := os.RemoveAll(first); err != nil {
		t.Fatal(err)
	}
	if got := gitOutput(second, "show", "HEAD^:historical.txt"); got != "complete historical blob" {
		t.Fatalf("history after sibling removal: %q", got)
	}
}

func TestGitLocalReceiverProbeRejectsTamperedCache(t *testing.T) {
	plan, data := reuseFixture(t)
	requireLocalReceiver(t, remoteGitLocalSeed(t.TempDir(), plan), data)
	entry := filepath.Join(plan.CacheRoot, plan.Digest+".git")
	// Replace HEAD's commit so the entry no longer matches the artifact identity.
	if err := os.WriteFile(filepath.Join(entry, "HEAD"), []byte(strings.Repeat("0", len(plan.Head))), 0o600); err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	if out, err := runLocalReceiver(t, remoteGitLocalSeedProbe(workdir, plan), nil); err == nil || strings.Contains(string(out), "reused-") {
		t.Fatalf("tampered cache reused: %q %v", out, err)
	}
	mustNotCreateGit(t, workdir)
}

func TestGitLocalReceiverProbeErrorOnForeignRepositoryFallsBack(t *testing.T) {
	plan, data := reuseFixture(t)
	requireLocalReceiver(t, remoteGitLocalSeed(t.TempDir(), plan), data) // warm the cache
	workdir := t.TempDir()
	runGit(t, workdir, "init", "-q")
	if out, err := runLocalReceiver(t, remoteGitLocalSeedProbe(workdir, plan), nil); err == nil || strings.Contains(string(out), "reused-") {
		t.Fatalf("foreign repository reported reuse: %q %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(workdir, ".git/HEAD")); err != nil {
		t.Fatalf("foreign repository modified: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workdir, ".git/crabbox-local-owner")); !os.IsNotExist(err) {
		t.Fatalf("foreign repository was claimed: %v", err)
	}
}

func TestGitLocalReceiverCacheKeepsThreeNewestSubjects(t *testing.T) {
	plan, data := reuseFixture(t)
	for day, name := range []string{"a", "b", "c"} {
		old := filepath.Join(plan.CacheRoot, strings.Repeat(name, 64)+".git")
		if err := os.MkdirAll(old, 0o700); err != nil {
			t.Fatal(err)
		}
		past := time.Date(2020, 1, day+1, 0, 0, 0, 0, time.UTC)
		if err := os.Chtimes(old, past, past); err != nil {
			t.Fatal(err)
		}
	}
	requireLocalReceiver(t, remoteGitLocalSeed(t.TempDir(), plan), data)
	entries, _ := filepath.Glob(filepath.Join(plan.CacheRoot, "*.git"))
	if len(entries) != 3 {
		t.Fatalf("expected 3 retained subjects, got %v", entries)
	}
	if _, err := os.Stat(filepath.Join(plan.CacheRoot, strings.Repeat("a", 64)+".git")); !os.IsNotExist(err) {
		t.Fatalf("oldest entry survived: %v", err)
	}
}

func TestLocalGitSeedProbeSourceFailsOpen(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		err  error
		want string
		ok   bool
	}{
		{"retired workspace source", "reused-workspace", nil, "", false},
		{"cache hit", "reused-cache", nil, "cache", true},
		{"miss", "", errors.New("exit status 1"), "", false},
		{"error after acknowledgement", "reused-cache", errors.New("ssh: connection reset"), "", false},
		{"unknown source", "reused-elsewhere", nil, "", false},
		{"noise", "ok", nil, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := localGitSeedProbeSource(tc.out, tc.err)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("got %q %v", got, ok)
			}
		})
	}
}

func TestGitLocalReceiverCacheRestoreDoesNotShareMutableMetadata(t *testing.T) {
	plan, data := reuseFixture(t)
	first := t.TempDir()
	requireLocalReceiver(t, remoteGitLocalSeed(first, plan), data)
	second := t.TempDir()
	requireLocalReceiver(t, remoteGitLocalSeedProbe(second, plan), nil)
	entry := filepath.Join(plan.CacheRoot, plan.Digest+".git")
	for _, name := range []string{"HEAD", "config", "crabbox-local-owner"} {
		paths := []string{filepath.Join(first, ".git", name), filepath.Join(second, ".git", name), filepath.Join(entry, name)}
		infos := make([]os.FileInfo, len(paths))
		for i, path := range paths {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			infos[i] = info
		}
		for i := range infos {
			for j := i + 1; j < len(infos); j++ {
				if os.SameFile(infos[i], infos[j]) {
					t.Errorf("%s is shared between %s and %s", name, paths[i], paths[j])
				}
			}
		}
	}
}

func TestGitLocalReceiverReuseDiscardsEveryWorkloadMutation(t *testing.T) {
	plan, data := reuseFixture(t)
	workdir := t.TempDir()
	requireLocalReceiver(t, remoteGitLocalSeed(workdir, plan), data)
	git := filepath.Join(workdir, ".git")
	mustWriteTestFile(t, filepath.Join(git, "hooks/pre-commit"), "#!/bin/sh\nexit 1\n")
	mustWriteTestFile(t, filepath.Join(git, "info/exclude"), "*\n")
	mustWriteTestFile(t, filepath.Join(git, "refs/heads/base.lock"), "stale")
	runGit(t, workdir, "config", "core.filemode", "false")
	runGit(t, workdir, "update-index", "--skip-worktree", "accepted.txt")
	runGit(t, workdir, "update-index", "--assume-unchanged", "excluded.txt")
	requireLocalReceiver(t, remoteGitLocalSeedProbe(workdir, plan), nil)
	for _, name := range []string{"hooks", "info", "refs/heads/base.lock"} {
		if _, err := os.Lstat(filepath.Join(git, name)); !os.IsNotExist(err) {
			t.Errorf("reuse kept workload entry %s: %v", name, err)
		}
	}
	if got := gitOutput(workdir, "config", "core.filemode"); got != "true" {
		t.Fatalf("core.filemode not reset: %q", got)
	}
	if flags := gitOutput(workdir, "ls-files", "-v"); strings.Contains(flags, "S ") || strings.Contains(flags, "h ") {
		t.Fatalf("hidden index flags survived reuse:\n%s", flags)
	}
	runGit(t, workdir, "update-ref", "-d", "refs/heads/base")
	// The cache itself stays pristine for the next consumer.
	fresh := t.TempDir()
	requireLocalReceiver(t, remoteGitLocalSeedProbe(fresh, plan), nil)
	if _, err := os.Lstat(filepath.Join(fresh, ".git/hooks")); !os.IsNotExist(err) {
		t.Fatalf("cache restore inherited a hooks directory: %v", err)
	}
	if got := gitOutput(fresh, "config", "core.filemode"); got != "true" {
		t.Fatalf("cache restore filemode: %q", got)
	}
}

func TestGitLocalReceiverProbeNeverCachesFromAWorkspace(t *testing.T) {
	plan, data := reuseFixture(t)
	workdir := t.TempDir()
	requireLocalReceiver(t, remoteGitLocalSeed(workdir, plan), data)
	if err := os.RemoveAll(plan.CacheRoot); err != nil {
		t.Fatal(err)
	}
	if out, err := runLocalReceiver(t, remoteGitLocalSeedProbe(workdir, plan), nil); err == nil || strings.Contains(string(out), "reused-") {
		t.Fatalf("evicted cache must miss, not trust the workspace: %q %v", out, err)
	}
	if _, err := os.Stat(plan.CacheRoot); !os.IsNotExist(err) {
		t.Fatalf("probe populated the cache from a workspace: %v", err)
	}
}

func TestGitLocalReceiverProbeInterruptedSwapRetainsPreviousMetadata(t *testing.T) {
	plan, data := reuseFixture(t)
	workdir := t.TempDir()
	requireLocalReceiver(t, remoteGitLocalSeed(workdir, plan), data)
	mustWriteTestFile(t, filepath.Join(workdir, ".git/crabbox/sync-manifest"), "prior.txt\x00")
	shims := t.TempDir()
	// TERM the receiver exactly when it publishes the fresh copy, after the previous
	// metadata has been moved aside.
	mustWriteTestFile(t, filepath.Join(shims, "mv"), "#!/bin/sh\ncase \"$2\" in */git) kill -TERM $PPID; sleep 1; exit 1 ;; esac\nexec /bin/mv \"$@\"\n")
	if err := os.Chmod(filepath.Join(shims, "mv"), 0o755); err != nil {
		t.Fatal(err)
	}
	command := strings.Replace(remoteGitLocalSeedProbe(workdir, plan), "PATH=/usr/bin:/bin", "PATH="+shims+":/usr/bin:/bin", 1)
	if out, err := runLocalReceiver(t, command, nil); err == nil {
		t.Fatalf("interrupted probe succeeded: %s", out)
	}
	previous, err := filepath.Glob(filepath.Join(workdir, ".crabbox-local-git.*", "previous.git", "crabbox", "sync-manifest"))
	if err != nil || len(previous) != 1 {
		t.Fatalf("previous metadata and manifest not retained: %v %v", previous, err)
	}
	// .git is absent here: the canonical raw manifest must already be in place so the
	// fallback import (a probe miss) carries file ownership forward.
	if got, err := os.ReadFile(filepath.Join(workdir, ".crabbox/sync-manifest")); err != nil || string(got) != "prior.txt\x00" {
		t.Fatalf("manifest not at the raw location during the swap window: %q %v", got, err)
	}
	requireLocalReceiver(t, remoteGitLocalSeed(workdir, plan), data)
	if got, err := os.ReadFile(filepath.Join(workdir, ".git/crabbox/sync-manifest")); err != nil || string(got) != "prior.txt\x00" {
		t.Fatalf("fallback import lost file ownership: %q %v", got, err)
	}
}

func TestGitLocalReceiverProbeLeavesCanonicalManifestInsideGit(t *testing.T) {
	plan, data := reuseFixture(t)
	workdir := t.TempDir()
	requireLocalReceiver(t, remoteGitLocalSeed(workdir, plan), data)
	mustWriteTestFile(t, filepath.Join(workdir, ".git/crabbox/sync-manifest"), "owned.txt\x00")
	requireLocalReceiver(t, remoteGitLocalSeedProbe(workdir, plan), nil)
	if got, err := os.ReadFile(filepath.Join(workdir, ".git/crabbox/sync-manifest")); err != nil || string(got) != "owned.txt\x00" {
		t.Fatalf("manifest lost across a successful probe: %q %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(workdir, ".crabbox")); !os.IsNotExist(err) {
		t.Fatalf("temporary raw manifest location not cleaned: %v", err)
	}
}

func TestGitLocalReceiverStoreSweepsStaleCacheStaging(t *testing.T) {
	plan, data := reuseFixture(t)
	stale := filepath.Join(plan.CacheRoot, ".tmp.interrupted")
	fresh := filepath.Join(plan.CacheRoot, ".tmp.inflight")
	for _, dir := range []string{stale, fresh} {
		mustWriteTestFile(t, filepath.Join(dir, "git/HEAD"), "x")
	}
	old := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	requireLocalReceiver(t, remoteGitLocalSeed(t.TempDir(), plan), data)
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Errorf("stale staging survived: %v", err)
	}
	if _, err := os.Lstat(fresh); err != nil {
		t.Errorf("a concurrent store's staging must not be touched: %v", err)
	}
	if entries, _ := filepath.Glob(filepath.Join(plan.CacheRoot, ".tmp.*")); len(entries) != 1 {
		t.Errorf("own staging not cleaned: %v", entries)
	}
}

func TestGitLocalReceiverCacheHitPathsSweepStaleStaging(t *testing.T) {
	plan, data := reuseFixture(t)
	workdir := t.TempDir()
	requireLocalReceiver(t, remoteGitLocalSeed(workdir, plan), data)
	orphan := filepath.Join(plan.CacheRoot, ".tmp.killed")
	age := func() {
		mustWriteTestFile(t, filepath.Join(orphan, "git/HEAD"), "x")
		old := time.Now().Add(-3 * time.Hour)
		if err := os.Chtimes(orphan, old, old); err != nil {
			t.Fatal(err)
		}
	}
	age()
	requireLocalReceiver(t, remoteGitLocalSeed(workdir, plan), data) // reimport: entry already cached
	if _, err := os.Lstat(orphan); !os.IsNotExist(err) {
		t.Fatalf("reimport left an aged orphan: %v", err)
	}
	age()
	requireLocalReceiver(t, remoteGitLocalSeedProbe(workdir, plan), nil)
	if _, err := os.Lstat(orphan); !os.IsNotExist(err) {
		t.Fatalf("probe left an aged orphan: %v", err)
	}
}

func TestGitLocalReceiverConcurrentCachePublicationDoesNotNest(t *testing.T) {
	plan, data := reuseFixture(t)
	shims := t.TempDir()
	// A rival store publishes the entry between the existence check and the rename.
	mustWriteTestFile(t, filepath.Join(shims, "mv"), "#!/bin/sh\ncase \"$3\" in *git-seeds/*.git) mkdir -p \"$3\" ;; esac\nexec /bin/mv \"$@\"\n")
	if err := os.Chmod(filepath.Join(shims, "mv"), 0o755); err != nil {
		t.Fatal(err)
	}
	command := strings.Replace(remoteGitLocalSeed(t.TempDir(), plan), "PATH=/usr/bin:/bin", "PATH="+shims+":/usr/bin:/bin", 1)
	requireLocalReceiver(t, command, data)
	entry := filepath.Join(plan.CacheRoot, plan.Digest+".git")
	if _, err := os.Lstat(filepath.Join(entry, "git")); !os.IsNotExist(err) {
		t.Fatalf("losing store nested inside the winner: %v", err)
	}
}

func TestGitLocalReceiverSweepRemovesNestedLoserTrees(t *testing.T) {
	plan, data := reuseFixture(t)
	workdir := t.TempDir()
	requireLocalReceiver(t, remoteGitLocalSeed(workdir, plan), data)
	nested := filepath.Join(plan.CacheRoot, plan.Digest+".git", "git", "objects")
	mustWriteTestFile(t, filepath.Join(nested, "pack"), "history")
	requireLocalReceiver(t, remoteGitLocalSeedProbe(workdir, plan), nil)
	if _, err := os.Lstat(filepath.Join(plan.CacheRoot, plan.Digest+".git", "git")); !os.IsNotExist(err) {
		t.Fatalf("nested tree survived a cache touch: %v", err)
	}
}

func TestGitLocalReceiverRetentionBoundRepairedOnCacheHits(t *testing.T) {
	plan, data := reuseFixture(t)
	workdir := t.TempDir()
	requireLocalReceiver(t, remoteGitLocalSeed(workdir, plan), data)
	// Simulate a store interrupted after publishing a fourth subject, before pruning.
	for day, name := range []string{"a", "b", "c"} {
		old := filepath.Join(plan.CacheRoot, strings.Repeat(name, 64)+".git")
		if err := os.MkdirAll(old, 0o700); err != nil {
			t.Fatal(err)
		}
		past := time.Date(2020, 1, day+1, 0, 0, 0, 0, time.UTC)
		if err := os.Chtimes(old, past, past); err != nil {
			t.Fatal(err)
		}
	}
	for name, run := range map[string]func(){
		"probe":    func() { requireLocalReceiver(t, remoteGitLocalSeedProbe(workdir, plan), nil) },
		"reimport": func() { requireLocalReceiver(t, remoteGitLocalSeed(workdir, plan), data) },
	} {
		entries, _ := filepath.Glob(filepath.Join(plan.CacheRoot, "*.git"))
		if len(entries) != 4 {
			t.Fatalf("%s: fixture must hold four subjects, got %v", name, entries)
		}
		run()
		entries, _ = filepath.Glob(filepath.Join(plan.CacheRoot, "*.git"))
		if len(entries) != 3 {
			t.Fatalf("%s did not repair the retention bound: %v", name, entries)
		}
		if _, err := os.Stat(filepath.Join(plan.CacheRoot, plan.Digest+".git")); err != nil {
			t.Fatalf("%s evicted the live subject: %v", name, err)
		}
		// Re-create the oldest fixture entry for the next iteration.
		old := filepath.Join(plan.CacheRoot, strings.Repeat("a", 64)+".git")
		if err := os.MkdirAll(old, 0o700); err != nil {
			t.Fatal(err)
		}
		past := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
		if err := os.Chtimes(old, past, past); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGitLocalReceiverUpgradeFromCachelessWorkspace(t *testing.T) {
	plan, data := reuseFixture(t)
	workdir := t.TempDir()
	// A workspace written by a release without the host cache.
	requireLocalReceiver(t, remoteGitLocalSeed(workdir, plan), data)
	requireLocalReceiver(t, remoteGitLocalSeedFinalize(workdir, plan), nil)
	mustWriteTestFile(t, filepath.Join(workdir, ".git/crabbox/sync-manifest"), "synced.txt\x00")
	mustWriteTestFile(t, filepath.Join(workdir, "synced.txt"), "working data")
	if err := os.RemoveAll(plan.CacheRoot); err != nil {
		t.Fatal(err)
	}
	if out, err := runLocalReceiver(t, remoteGitLocalSeedProbe(workdir, plan), nil); err == nil || strings.Contains(string(out), "reused-") {
		t.Fatalf("cacheless host must miss: %q %v", out, err)
	}
	requireLocalReceiver(t, remoteGitLocalSeed(workdir, plan), data)
	if got, err := os.ReadFile(filepath.Join(workdir, ".git/crabbox/sync-manifest")); err != nil || string(got) != "synced.txt\x00" {
		t.Fatalf("existing manifest lost on upgrade: %q %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(workdir, "synced.txt")); err != nil || string(got) != "working data" {
		t.Fatalf("working data changed on upgrade: %q %v", got, err)
	}
	if out := requireLocalReceiver(t, remoteGitLocalSeedProbe(workdir, plan), nil); strings.TrimSpace(string(out)) != "reused-cache" {
		t.Fatalf("cache not populated by the post-upgrade import: %q", out)
	}
}

func TestGitLocalReceiverProbeDoesNotFollowPlantedManifestLink(t *testing.T) {
	plan, data := reuseFixture(t)
	workdir := t.TempDir()
	requireLocalReceiver(t, remoteGitLocalSeed(workdir, plan), data)
	mustWriteTestFile(t, filepath.Join(workdir, ".git/crabbox/sync-manifest"), "owned.txt\x00")
	outside := filepath.Join(t.TempDir(), "victim")
	mustWriteTestFile(t, outside, "untouched")
	if err := os.MkdirAll(filepath.Join(workdir, ".crabbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sync-manifest.new", "sync-manifest.tmp"} {
		if err := os.Symlink(outside, filepath.Join(workdir, ".crabbox", name)); err != nil {
			t.Fatal(err)
		}
	}
	requireLocalReceiver(t, remoteGitLocalSeedProbe(workdir, plan), nil)
	if got, _ := os.ReadFile(outside); string(got) != "untouched" {
		t.Fatalf("probe wrote through a planted link: %q", got)
	}
}

func TestGitLocalReceiverFragmentCacheEntryIsReplacedByImport(t *testing.T) {
	plan, data := reuseFixture(t)
	workdir := t.TempDir()
	requireLocalReceiver(t, remoteGitLocalSeed(workdir, plan), data)
	entry := filepath.Join(plan.CacheRoot, plan.Digest+".git")
	// An eviction interrupted after removing HEAD.
	if err := os.Remove(filepath.Join(entry, "HEAD")); err != nil {
		t.Fatal(err)
	}
	if out, err := runLocalReceiver(t, remoteGitLocalSeedProbe(workdir, plan), nil); err == nil || strings.Contains(string(out), "reused-") {
		t.Fatalf("fragment entry reused: %q %v", out, err)
	}
	requireLocalReceiver(t, remoteGitLocalSeed(workdir, plan), data)
	if out := requireLocalReceiver(t, remoteGitLocalSeedProbe(workdir, plan), nil); strings.TrimSpace(string(out)) != "reused-cache" {
		t.Fatalf("import did not repair the fragment entry: %q", out)
	}
}

func TestGitLocalReceiverProbeCleanupNeverFollowsRawDirectoryLink(t *testing.T) {
	plan, data := reuseFixture(t)
	workdir := t.TempDir()
	requireLocalReceiver(t, remoteGitLocalSeed(workdir, plan), data)
	// No Git-side manifest to stage, and .crabbox is a link to a directory outside the workspace.
	outsideDir := t.TempDir()
	victim := filepath.Join(outsideDir, "sync-manifest")
	mustWriteTestFile(t, victim, "untouched")
	if err := os.Symlink(outsideDir, filepath.Join(workdir, ".crabbox")); err != nil {
		t.Fatal(err)
	}
	requireLocalReceiver(t, remoteGitLocalSeedProbe(workdir, plan), nil)
	if got, err := os.ReadFile(victim); err != nil || string(got) != "untouched" {
		t.Fatalf("cleanup followed the .crabbox link: %q %v", got, err)
	}
}

func TestGitLocalReceiverCacheUpkeepNeverTraversesLinkedSubjects(t *testing.T) {
	plan, data := reuseFixture(t)
	requireLocalReceiver(t, remoteGitLocalSeed(t.TempDir(), plan), data)
	outside := t.TempDir()
	victim := filepath.Join(outside, "git", "keep")
	mustWriteTestFile(t, victim, "untouched")
	link := filepath.Join(plan.CacheRoot, strings.Repeat("e", 64)+".git")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	requireLocalReceiver(t, remoteGitLocalSeedProbe(t.TempDir(), plan), nil)
	if got, err := os.ReadFile(victim); err != nil || string(got) != "untouched" {
		t.Fatalf("upkeep deleted through a linked cache subject: %q %v", got, err)
	}
}
