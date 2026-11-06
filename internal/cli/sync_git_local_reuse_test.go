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

func TestGitLocalReceiverProbeReusesVerifiedWorkspace(t *testing.T) {
	plan, data := reuseFixture(t)
	workdir := t.TempDir()
	requireLocalReceiver(t, remoteGitLocalSeed(workdir, plan), data)
	requireLocalReceiver(t, remoteGitLocalSeedFinalize(workdir, plan), nil)
	mustWriteTestFile(t, filepath.Join(workdir, ".git/crabbox/git-hydrate-base"), "stale")
	mustWriteTestFile(t, filepath.Join(workdir, "accepted.txt"), "dirty edit after the first sync\n")
	out := requireLocalReceiver(t, remoteGitLocalSeedProbe(workdir, plan), nil)
	if strings.TrimSpace(string(out)) != "reused-workspace" {
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
	plan, _ := reuseFixture(t)
	workdir := t.TempDir()
	runGit(t, workdir, "init", "-q")
	if out, err := runLocalReceiver(t, remoteGitLocalSeedProbe(workdir, plan), nil); err == nil || strings.Contains(string(out), "reused-") {
		t.Fatalf("foreign repository reported reuse: %q %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(workdir, ".git/HEAD")); err != nil {
		t.Fatalf("foreign repository modified: %v", err)
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
		{"workspace hit", "reused-workspace", nil, "workspace", true},
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

func TestGitLocalReceiverProbeClearsHiddenIndexFlags(t *testing.T) {
	plan, data := reuseFixture(t)
	workdir := t.TempDir()
	requireLocalReceiver(t, remoteGitLocalSeed(workdir, plan), data)
	runGit(t, workdir, "update-index", "--skip-worktree", "accepted.txt")
	runGit(t, workdir, "update-index", "--assume-unchanged", "excluded.txt")
	requireLocalReceiver(t, remoteGitLocalSeedProbe(workdir, plan), nil)
	if flags := gitOutput(workdir, "ls-files", "-v"); strings.Contains(flags, "S ") || strings.Contains(flags, "h ") {
		t.Fatalf("hidden index flags survived reuse:\n%s", flags)
	}
}

func TestGitLocalReceiverReuseDropsWorkloadInstalledHooks(t *testing.T) {
	plan, data := reuseFixture(t)
	workdir := t.TempDir()
	requireLocalReceiver(t, remoteGitLocalSeed(workdir, plan), data)
	hook := filepath.Join(workdir, ".git/hooks/pre-commit")
	mustWriteTestFile(t, hook, "#!/bin/sh\nexit 1\n")
	mustWriteTestFile(t, filepath.Join(workdir, ".git/info/exclude"), "*\n")
	// Evict the cache so a probe hit repopulates it from the modified workspace.
	if err := os.RemoveAll(plan.CacheRoot); err != nil {
		t.Fatal(err)
	}
	requireLocalReceiver(t, remoteGitLocalSeedProbe(workdir, plan), nil)
	for _, path := range []string{hook, filepath.Join(workdir, ".git/info")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("workspace reuse kept %s: %v", path, err)
		}
	}
	fresh := t.TempDir()
	requireLocalReceiver(t, remoteGitLocalSeedProbe(fresh, plan), nil)
	if _, err := os.Lstat(filepath.Join(fresh, ".git/hooks")); !os.IsNotExist(err) {
		t.Fatalf("cache restore inherited a hooks directory: %v", err)
	}
}
