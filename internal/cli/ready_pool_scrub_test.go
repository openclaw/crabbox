package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRemoteReadyPoolScrubResetsLatestBranchAndPreservesIgnoredCaches(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX scrub integration")
	}
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "init")
	runGit(t, source, "config", "user.email", "test@example.com")
	runGit(t, source, "config", "user.name", "Test")
	runGit(t, source, "branch", "-M", "main")
	mustWriteTestFile(t, filepath.Join(source, ".gitignore"), "node_modules/\n*.ignored\n")
	mustWriteTestFile(t, filepath.Join(source, "proof.txt"), "base\n")
	runGit(t, source, "add", ".")
	runGit(t, source, "commit", "-m", "base")

	origin := filepath.Join(root, "origin.git")
	cloneBare := exec.Command("git", "clone", "--bare", source, origin)
	if out, err := cloneBare.CombinedOutput(); err != nil {
		t.Fatalf("create bare origin: %v\n%s", err, out)
	}
	runGit(t, source, "remote", "add", "origin", origin)

	workdir := filepath.Join(root, "workdir")
	clone := exec.Command("git", "clone", origin, workdir)
	if out, err := clone.CombinedOutput(); err != nil {
		t.Fatalf("clone workdir: %v\n%s", err, out)
	}
	runGit(t, workdir, "config", "user.email", "test@example.com")
	runGit(t, workdir, "config", "user.name", "Test")
	runGit(t, workdir, "checkout", "-b", "feature")
	mustWriteTestFile(t, filepath.Join(workdir, "proof.txt"), "dirty feature\n")
	mustWriteTestFile(t, filepath.Join(workdir, "untracked.txt"), "remove me\n")
	mustWriteTestFile(t, filepath.Join(workdir, "node_modules", "cache.txt"), "keep me\n")
	mustWriteTestFile(t, filepath.Join(workdir, ".pnpm-store", "cache.txt"), "remove unless ignored\n")
	mustWriteTestFile(t, filepath.Join(workdir, "task-state.ignored"), "remove me\n")
	for _, name := range []string{"sync-fingerprint", "sync-manifest", "sync-manifest.new", "sync-deleted.new"} {
		mustWriteTestFile(t, filepath.Join(workdir, ".git", "crabbox", name), "stale\n")
	}
	for _, name := range []string{"env", "scripts", "logs", "captures", "runs"} {
		mustWriteTestFile(t, filepath.Join(workdir, ".crabbox", name, "stale.txt"), "stale\n")
	}
	attackerSource := filepath.Join(root, "attacker-source")
	if err := os.Mkdir(attackerSource, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, attackerSource, "init")
	runGit(t, attackerSource, "config", "user.email", "test@example.com")
	runGit(t, attackerSource, "config", "user.name", "Test")
	runGit(t, attackerSource, "branch", "-M", "main")
	mustWriteTestFile(t, filepath.Join(attackerSource, "proof.txt"), "attacker\n")
	runGit(t, attackerSource, "add", ".")
	runGit(t, attackerSource, "commit", "-m", "attacker")
	attackerOrigin := filepath.Join(root, "attacker.git")
	cloneAttacker := exec.Command("git", "clone", "--bare", attackerSource, attackerOrigin)
	if out, err := cloneAttacker.CombinedOutput(); err != nil {
		t.Fatalf("create attacker origin: %v\n%s", err, out)
	}
	runGit(t, workdir, "remote", "set-url", "origin", attackerOrigin)
	runGit(t, workdir, "config", "remote.origin.uploadpack", "false")
	runGit(t, workdir, "config", "url."+attackerOrigin+".insteadOf", origin)

	mustWriteTestFile(t, filepath.Join(source, "proof.txt"), "latest main\n")
	runGit(t, source, "add", "proof.txt")
	runGit(t, source, "commit", "-m", "advance main")
	runGit(t, source, "push", "origin", "main")
	wantCommit := gitOutput(source, "rev-parse", "HEAD")

	cmd := exec.Command("bash", "-lc", remoteReadyPoolScrub(workdir, "main", origin))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("scrub failed: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != wantCommit {
		t.Fatalf("prepared commit=%q, want %q", got, wantCommit)
	}
	if got := gitOutput(workdir, "branch", "--show-current"); got != "main" {
		t.Fatalf("branch=%q, want main", got)
	}
	if got := gitOutput(workdir, "rev-parse", "HEAD"); got != wantCommit {
		t.Fatalf("HEAD=%q, want %q", got, wantCommit)
	}
	if got := gitOutput(workdir, "remote", "get-url", "origin"); got != origin {
		t.Fatalf("origin=%q, want trusted %q", got, origin)
	}
	if got := gitOutput(workdir, "status", "--porcelain", "--untracked-files=normal"); got != "" {
		t.Fatalf("worktree not clean: %q", got)
	}
	proof, err := os.ReadFile(filepath.Join(workdir, "proof.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(proof) != "latest main\n" {
		t.Fatalf("proof=%q", proof)
	}
	cache, err := os.ReadFile(filepath.Join(workdir, "node_modules", "cache.txt"))
	if err != nil {
		t.Fatalf("ignored dependency cache was removed: %v", err)
	}
	if string(cache) != "keep me\n" {
		t.Fatalf("cache=%q", cache)
	}
	if _, err := os.Stat(filepath.Join(workdir, "task-state.ignored")); !os.IsNotExist(err) {
		t.Fatalf("ignored task state remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workdir, ".pnpm-store")); !os.IsNotExist(err) {
		t.Fatalf("non-ignored cache remains: %v", err)
	}
	for _, name := range []string{"sync-fingerprint", "sync-manifest", "sync-manifest.new", "sync-deleted.new"} {
		if _, err := os.Stat(filepath.Join(workdir, ".git", "crabbox", name)); !os.IsNotExist(err) {
			t.Fatalf("stale metadata %s remains: %v", name, err)
		}
	}
	marker, err := os.ReadFile(filepath.Join(workdir, ".git", "crabbox", "git-hydrate-base"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(marker), "main "+wantCommit+"\n"; got != want {
		t.Fatalf("hydrate marker=%q, want %q", got, want)
	}
	for _, name := range []string{"env", "scripts", "logs", "captures", "runs"} {
		if _, err := os.Stat(filepath.Join(workdir, ".crabbox", name)); !os.IsNotExist(err) {
			t.Fatalf("run transient %s remains: %v", name, err)
		}
	}
}

func TestWindowsRemoteReadyPoolScrubBuildsVerifiedReset(t *testing.T) {
	decoded := decodePowerShellCommand(t, windowsRemoteReadyPoolScrub(`C:\crabbox\repo`, "main", "https://example.com/org/repo.git"))
	for _, want := range []string{
		"Get-ChildItem Env:GIT_*",
		"Remove-Item -ErrorAction Stop",
		"Join-Path $env:ProgramFiles 'Git\\cmd\\git.exe'",
		"& $git -C $tmp fetch --quiet --depth=1 origin",
		"& $git checkout --quiet -f -B $ref $targetCommit",
		"ready-pool scrub does not reuse submodule worktrees",
		"$cleanArgs = @('clean', '-ffdx', '--quiet')",
		"& $git check-ignore -q -- $cachePath",
		"[System.IO.FileAttributes]::ReparsePoint",
		"ready-pool .crabbox root must be a real directory",
		"ready-pool workspace root must be a real directory",
		"$env:HOME = $safeHome",
		"ready-pool trusted origin verification failed",
		"Remove-Item -LiteralPath $metadataPath -Force -ErrorAction Stop",
		"if (Test-Path -LiteralPath $metadataPath) { throw",
		"sync-fingerprint",
		"git-hydrate-base",
		"& $git status --porcelain --untracked-files=normal",
	} {
		if !strings.Contains(decoded, want) {
			t.Fatalf("Windows scrub missing %q in %q", want, decoded)
		}
	}
}

func TestReadyPoolRunNeedsTrustedRemote(t *testing.T) {
	for _, policy := range []string{"auto", "ready", ""} {
		if !readyPoolRunNeedsTrustedRemote(policy) {
			t.Fatalf("policy %q must validate the trusted origin", policy)
		}
	}
	for _, policy := range []string{"drain", "release", " DRAIN "} {
		if readyPoolRunNeedsTrustedRemote(policy) {
			t.Fatalf("policy %q must permit unconditional disposal", policy)
		}
	}
}

func TestRemoteReadyPoolScrubUsesIsolatedTrustedGitMetadata(t *testing.T) {
	got := remoteReadyPoolScrub("/work/repo", "main", "https://example.com/org/repo.git")
	for _, want := range []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"/usr/bin/git",
		"/bin/bash --noprofile --norc -c",
		"if [ -L \"$workdir\" ]",
		"HOME=\"$safe_home\"",
		"safe_git -C \"$tmp\" fetch",
		"ready-pool scrub does not reuse submodule worktrees",
		"safe_git check-ignore -q -- \"$cache_path\"",
		"safe_git clean \"${clean_args[@]}\"",
		"rm -rf -- .git",
		"safe_git remote set-url origin",
		"if ! status=",
		"if [ -L .crabbox ]",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("POSIX scrub missing %q in %q", want, got)
		}
	}
}

func TestTrustedReadyPoolRemoteURL(t *testing.T) {
	for _, value := range []string{"", "https://user@example.com/org/repo.git", "ssh://git@example.com/org/repo.git"} {
		if _, err := trustedReadyPoolRemoteURL(value); err == nil {
			t.Fatalf("unsafe origin %q was accepted", value)
		}
	}
	if _, err := trustedReadyPoolRemoteURL("git@github.com:example-org/repo.git"); err == nil {
		t.Fatal("SSH origin was accepted without a reusable authentication contract")
	}
	local := filepath.Join(t.TempDir(), "origin.git")
	if _, err := trustedReadyPoolRemoteURL(local); err == nil {
		t.Fatal("client-local origin was accepted for remote-host reuse")
	}
}

func TestRemoteReadyPoolScrubRejectsCrabboxSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlink integration")
	}
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "init")
	runGit(t, source, "config", "user.email", "test@example.com")
	runGit(t, source, "config", "user.name", "Test")
	runGit(t, source, "branch", "-M", "main")
	if err := os.Symlink("../outside", filepath.Join(source, ".crabbox")); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "add", ".crabbox")
	runGit(t, source, "commit", "-m", "tracked crabbox symlink")

	origin := filepath.Join(root, "origin.git")
	cloneBare := exec.Command("git", "clone", "--bare", source, origin)
	if out, err := cloneBare.CombinedOutput(); err != nil {
		t.Fatalf("create bare origin: %v\n%s", err, out)
	}
	workdir := filepath.Join(root, "workdir")
	clone := exec.Command("git", "clone", origin, workdir)
	if out, err := clone.CombinedOutput(); err != nil {
		t.Fatalf("clone workdir: %v\n%s", err, out)
	}
	sentinel := filepath.Join(root, "outside", "env", "sentinel.txt")
	mustWriteTestFile(t, sentinel, "keep\n")

	cmd := exec.Command("bash", "-lc", remoteReadyPoolScrub(workdir, "main", origin))
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("symlinked .crabbox root was accepted: %s", out)
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "keep\n" {
		t.Fatalf("outside sentinel changed: data=%q err=%v", data, err)
	}
}

func TestPreflightReadyPoolRemoteRequiresAnonymousFetch(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "init")
	runGit(t, source, "config", "user.email", "test@example.com")
	runGit(t, source, "config", "user.name", "Test")
	runGit(t, source, "branch", "-M", "main")
	mustWriteTestFile(t, filepath.Join(source, "proof.txt"), "proof\n")
	runGit(t, source, "add", ".")
	runGit(t, source, "commit", "-m", "base")

	origin := filepath.Join(root, "origin.git")
	clone := exec.Command("git", "clone", "--bare", source, origin)
	if out, err := clone.CombinedOutput(); err != nil {
		t.Fatalf("create bare origin: %v\n%s", err, out)
	}
	if err := preflightReadyPoolRemote(context.Background(), origin); err != nil {
		t.Fatalf("anonymous local origin rejected: %v", err)
	}
	if err := preflightReadyPoolRemote(context.Background(), filepath.Join(root, "missing.git")); err == nil {
		t.Fatal("missing origin passed anonymous fetch preflight")
	}
}
