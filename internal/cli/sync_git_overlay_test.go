package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

type gitOverlayFixture struct {
	root           string
	origin         string
	repo           Repo
	cfg            Config
	plan           gitCoherencePlan
	historicalBlob string
}

func newGitOverlayFixture(t *testing.T) gitOverlayFixture {
	t.Helper()
	root := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "init", "-q")
	runGit(t, root, "config", "user.email", "alice@example.com")
	runGit(t, root, "config", "user.name", "Alice")
	runGit(t, root, "branch", "-M", "main")
	mustWriteTestFile(t, filepath.Join(root, ".gitignore"), "node_modules/\n.yarn/cache/\n")
	for _, name := range []string{"clean.txt", "staged.txt", "unstaged.txt", "deleted.txt", "renamed.txt", "excluded.txt", "mode.sh", "historical.txt"} {
		mustWriteTestFile(t, filepath.Join(root, name), "base "+name+"\n")
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink("clean.txt", filepath.Join(root, "link")); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-qm", "history")
	historicalBlob := gitOutput(root, "rev-parse", "HEAD:historical.txt")
	runGit(t, root, "rm", "-q", "historical.txt")
	runGit(t, root, "commit", "-qm", "target")
	origin := filepath.Join(filepath.Dir(root), "origin.git")
	if out, err := exec.Command("git", "clone", "--quiet", "--bare", root, origin).CombinedOutput(); err != nil {
		t.Fatalf("create bare origin: %v\n%s", err, out)
	}
	runGit(t, origin, "config", "uploadpack.allowFilter", "true")
	runGit(t, root, "remote", "add", "origin", origin)
	runGit(t, root, "fetch", "-q", "origin", "+refs/heads/*:refs/remotes/origin/*")
	repo := Repo{Root: root, Name: "source", RemoteURL: origin, Head: gitOutput(root, "rev-parse", "HEAD"), BaseRef: "main"}
	cfg := baseConfig()
	cfg.Sync.GitOverlay = true
	cfg.Sync.Excludes = append(cfg.Sync.Excludes, "excluded.txt")
	plan, blocked := syncGitCoherencePlan(cfg, repo)
	if blocked || !plan.enabled() {
		t.Fatalf("overlay fixture plan=%#v blocked=%t", plan, blocked)
	}
	return gitOverlayFixture{root: root, origin: origin, repo: repo, cfg: cfg, plan: plan, historicalBlob: historicalBlob}
}

func (fixture gitOverlayFixture) manifest(t *testing.T) (SyncManifest, SyncExcludeRules) {
	t.Helper()
	excludes, err := syncExcludes(fixture.root, fixture.cfg)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := syncManifestFilteredRules(fixture.root, excludes, nil)
	if err != nil {
		t.Fatal(err)
	}
	return manifest, excludes
}

func runOverlayCommand(t *testing.T, command string, input []byte, environment ...string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command("/bin/bash", "--noprofile", "--norc", "-c", command)
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
	}
	cmd.Env = append(os.Environ(), environment...)
	return cmd.CombinedOutput()
}

func assertNoGitOverlayResidue(t *testing.T, workdir string) {
	t.Helper()
	for _, pattern := range []string{".overlay-git.*", ".overlay-seed.*"} {
		matches, err := filepath.Glob(filepath.Join(filepath.Dir(workdir), pattern))
		if err != nil || len(matches) != 0 {
			t.Fatalf("overlay residue pattern=%q matches=%v error=%v", pattern, matches, err)
		}
	}
	if hooks := gitOutput(workdir, "config", "--local", "--get", "core.hooksPath"); hooks != "" {
		t.Fatalf("overlay persisted temporary hooks path %q", hooks)
	}
}

func TestGitOverlayConfigDefaultFileEnvironmentAndShow(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	if baseConfig().Sync.GitOverlay {
		t.Fatal("Git overlay must default to off")
	}
	if err := os.WriteFile(configPath, []byte("sync:\n  gitOverlay: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil || !cfg.Sync.GitOverlay {
		t.Fatalf("file overlay=%t err=%v", cfg.Sync.GitOverlay, err)
	}
	var output bytes.Buffer
	app := App{Stdout: &output, Stderr: &bytes.Buffer{}}
	if err := app.configShow(nil); err != nil || !strings.Contains(output.String(), "git_overlay=true") {
		t.Fatalf("config text=%q err=%v", output.String(), err)
	}
	output.Reset()
	if err := app.configShow([]string{"--json"}); err != nil {
		t.Fatal(err)
	}
	var shown struct {
		Sync struct {
			GitOverlay bool `json:"gitOverlay"`
		} `json:"sync"`
	}
	if err := json.Unmarshal(output.Bytes(), &shown); err != nil || !shown.Sync.GitOverlay {
		t.Fatalf("config JSON=%q err=%v", output.Bytes(), err)
	}
	t.Setenv("CRABBOX_SYNC_GIT_OVERLAY", "false")
	cfg, err = loadConfig()
	if err != nil || cfg.Sync.GitOverlay {
		t.Fatalf("environment overlay=%t err=%v", cfg.Sync.GitOverlay, err)
	}
}

func TestSyncManifestGitOverlayPreservesDirtyShapes(t *testing.T) {
	fixture := newGitOverlayFixture(t)
	mustWriteTestFile(t, filepath.Join(fixture.root, "staged.txt"), "staged change\n")
	runGit(t, fixture.root, "add", "staged.txt")
	mustWriteTestFile(t, filepath.Join(fixture.root, "unstaged.txt"), "unstaged change\n")
	mustWriteTestFile(t, filepath.Join(fixture.root, "untracked.txt"), "untracked change\n")
	if err := os.Remove(filepath.Join(fixture.root, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	runGit(t, fixture.root, "mv", "renamed.txt", "renamed-new.txt")
	if err := os.Chmod(filepath.Join(fixture.root, "mode.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := []string{"mode.sh", "renamed-new.txt", "staged.txt", "unstaged.txt", "untracked.txt"}
	if runtime.GOOS != "windows" {
		if err := os.Remove(filepath.Join(fixture.root, "link")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("staged.txt", filepath.Join(fixture.root, "link")); err != nil {
			t.Fatal(err)
		}
		want = append(want, "link")
		slices.Sort(want)
	}
	manifest, _ := fixture.manifest(t)
	if !slices.Equal(manifest.OverlayFiles, want) {
		t.Fatalf("overlay=%v want=%v changed=%v", manifest.OverlayFiles, want, manifest.Changed)
	}
	for _, rel := range []string{"deleted.txt", "renamed.txt"} {
		if !slices.Contains(manifest.Deleted, rel) {
			t.Fatalf("deleted=%v missing=%q", manifest.Deleted, rel)
		}
	}
	if slices.Contains(manifest.Files, "excluded.txt") || slices.Contains(manifest.OverlayFiles, "clean.txt") {
		t.Fatalf("manifest authority violated: %#v", manifest)
	}
	if err := validateGitOverlayManifest(fixture.repo, manifest); err != nil {
		t.Fatalf("valid dirty overlay rejected: %v", err)
	}
}

func TestGitOverlayDecisionEligibilityAndDefaultOff(t *testing.T) {
	fixture := newGitOverlayFixture(t)
	manifest, _ := fixture.manifest(t)
	if len(manifest.OverlayFiles) != 0 || len(manifest.OverlayNUL()) != 0 {
		t.Fatalf("clean checkout has source payload: %#v", manifest)
	}
	legacy := fixture.cfg
	legacy.Sync.GitOverlay = false
	if got := decideGitOverlay(legacy, fixture.repo, SSHTarget{TargetOS: targetLinux}, manifest, fixture.plan, false, false, false); got != (gitOverlayDecision{}) {
		t.Fatalf("legacy decision=%#v", got)
	}
	tests := []struct {
		name    string
		target  SSHTarget
		mutate  func(*Config, *Repo, *gitCoherencePlan)
		full    bool
		actions bool
		blocked bool
		want    string
	}{
		{name: "linux", target: SSHTarget{TargetOS: targetLinux}},
		{name: "macos", target: SSHTarget{TargetOS: targetMacOS}, want: "unsupported_target"},
		{name: "wsl2", target: SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}, want: "unsupported_target"},
		{name: "full resync", target: SSHTarget{TargetOS: targetLinux}, full: true, want: "full_resync"},
		{name: "actions", target: SSHTarget{TargetOS: targetLinux}, actions: true, want: "actions_workspace"},
		{name: "delete disabled", target: SSHTarget{TargetOS: targetLinux}, mutate: func(cfg *Config, _ *Repo, _ *gitCoherencePlan) { cfg.Sync.Delete = false }, want: "delete_disabled"},
		{name: "seed disabled", target: SSHTarget{TargetOS: targetLinux}, mutate: func(cfg *Config, _ *Repo, _ *gitCoherencePlan) { cfg.Sync.GitSeed = false }, want: "git_seed_disabled"},
		{name: "include", target: SSHTarget{TargetOS: targetLinux}, mutate: func(cfg *Config, _ *Repo, _ *gitCoherencePlan) { cfg.Sync.Includes = []string{"src"} }, want: "include_whitelist"},
		{name: "credential", target: SSHTarget{TargetOS: targetLinux}, blocked: true, mutate: func(_ *Config, repo *Repo, _ *gitCoherencePlan) {
			repo.RemoteURL = fmt.Sprintf("https://%s:%s@example.test/repo.git", "user", "test-password")
		}, want: "credential_origin"},
		{name: "scp", target: SSHTarget{TargetOS: targetLinux}, mutate: func(_ *Config, repo *Repo, _ *gitCoherencePlan) { repo.RemoteURL = "git@example.test:repo.git" }, want: "unsupported_origin_transport"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, repo, plan := fixture.cfg, fixture.repo, fixture.plan
			if test.mutate != nil {
				test.mutate(&cfg, &repo, &plan)
			}
			got := decideGitOverlay(cfg, repo, test.target, manifest, plan, test.blocked, test.full, test.actions)
			if !got.Requested || got.Enabled != (test.want == "") || got.Reason != test.want {
				t.Fatalf("decision=%#v want=%q", got, test.want)
			}
		})
	}
}

func TestGitOverlayDecisionRejectsSparseGitlinksConflictsAndTransforms(t *testing.T) {
	for _, kind := range []string{"sparse", "excluded skip-worktree", "gitlink", "conflict", "attributes", "excluded target attributes", "local attributes", "autocrlf"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newGitOverlayFixture(t)
			switch kind {
			case "sparse":
				runGit(t, fixture.root, "sparse-checkout", "set", "--no-cone", "/*")
			case "excluded skip-worktree":
				runGit(t, fixture.root, "update-index", "--skip-worktree", "excluded.txt")
				if err := os.Remove(filepath.Join(fixture.root, "excluded.txt")); err != nil {
					t.Fatal(err)
				}
			case "gitlink":
				runGit(t, fixture.root, "update-index", "--add", "--cacheinfo", "160000,"+fixture.repo.Head+",submodule")
			case "conflict":
				setUnmergedIndexModes(t, fixture.root, "staged.txt", "100644", "100644")
			case "attributes":
				mustWriteTestFile(t, filepath.Join(fixture.root, ".gitattributes"), "*.txt text=auto\n")
			case "excluded target attributes":
				mustWriteTestFile(t, filepath.Join(fixture.root, ".gitattributes"), "excluded.txt text=auto\n")
				runGit(t, fixture.root, "add", ".gitattributes")
				runGit(t, fixture.root, "commit", "-qm", "excluded checkout transformation")
				runGit(t, fixture.root, "push", "-q", "origin", "main")
				fixture.repo.Head = gitOutput(fixture.root, "rev-parse", "HEAD")
				fixture.plan, _ = syncGitCoherencePlan(fixture.cfg, fixture.repo)
			case "local attributes":
				mustWriteTestFile(t, filepath.Join(fixture.root, ".git", "info", "attributes"), "*.txt filter=unsafe\n")
			case "autocrlf":
				runGit(t, fixture.root, "config", "core.autocrlf", "true")
			}
			manifest, _ := fixture.manifest(t)
			decision := decideGitOverlay(fixture.cfg, fixture.repo, SSHTarget{TargetOS: targetLinux}, manifest, fixture.plan, false, false, false)
			if decision.Enabled || decision.Reason == "" {
				t.Fatalf("unsafe %s accepted: %#v", kind, decision)
			}
		})
	}
}

func TestGitOverlayOriginAcceptsOnlyAnonymousSupportedTransports(t *testing.T) {
	tests := []struct {
		origin string
		want   bool
	}{
		{origin: "/srv/git/project.git", want: true},
		{origin: "relative/project.git", want: true},
		{origin: "file:///srv/git/project.git", want: true},
		{origin: "file://localhost/srv/git/project.git", want: true},
		{origin: "http://example.test/project.git", want: true},
		{origin: "https://example.test/project.git", want: true},
		{origin: fmt.Sprintf("https://%s:%s@example.test/project.git", "user", "test-password")},
		{origin: "https://example.test/project.git?token=private"},
		{origin: "git@example.test:project.git"},
		{origin: "ssh://git@example.test/project.git"},
		{origin: "git://example.test/project.git"},
		{origin: "ext::git-upload-pack /srv/git/project.git"},
		{origin: "file://remote-host/srv/git/project.git"},
	}
	for _, test := range tests {
		t.Run(test.origin, func(t *testing.T) {
			if got := gitOverlayOriginTransportSupported(test.origin); got != test.want {
				t.Fatalf("supported=%t want=%t", got, test.want)
			}
		})
	}
}

func TestRemoteGitOverlayPreparePruneTransferAndFinalize(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX overlay integration")
	}
	rsyncPath, err := exec.LookPath("rsync")
	if err != nil {
		t.Skip("rsync unavailable")
	}
	for _, reuse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reuse=%t", reuse), func(t *testing.T) {
			fixture := newGitOverlayFixture(t)
			mustWriteTestFile(t, filepath.Join(fixture.root, "staged.txt"), "staged payload\n")
			runGit(t, fixture.root, "add", "staged.txt")
			mustWriteTestFile(t, filepath.Join(fixture.root, "unstaged.txt"), "unstaged payload\n")
			mustWriteTestFile(t, filepath.Join(fixture.root, "untracked.txt"), "untracked payload\n")
			if err := os.Remove(filepath.Join(fixture.root, "deleted.txt")); err != nil {
				t.Fatal(err)
			}
			runGit(t, fixture.root, "mv", "renamed.txt", "renamed-new.txt")
			if err := os.Chmod(filepath.Join(fixture.root, "mode.sh"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(fixture.root, "link")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("staged.txt", filepath.Join(fixture.root, "link")); err != nil {
				t.Fatal(err)
			}
			manifest, excludes := fixture.manifest(t)
			fingerprint, err := syncFingerprintForManifest(fixture.repo, fixture.cfg, manifest, excludes, fixture.plan)
			if err != nil {
				t.Fatal(err)
			}
			workdir := filepath.Join(t.TempDir(), "remote")
			if reuse {
				if out, err := exec.Command("git", "clone", "--quiet", fixture.origin, workdir).CombinedOutput(); err != nil {
					t.Fatalf("clone existing workspace: %v\n%s", err, out)
				}
				mustWriteTestFile(t, filepath.Join(workdir, "node_modules", "cached.txt"), "trusted cache\n")
				mustWriteTestFile(t, filepath.Join(workdir, ".pnpm-store", "stale.txt"), "untrusted cache\n")
				mustWriteTestFile(t, filepath.Join(workdir, ".git", "info", "exclude"), ".pnpm-store/\n")
				mustWriteTestFile(t, filepath.Join(workdir, "previous.txt"), "previous managed file\n")
				mustWriteTestFile(t, filepath.Join(workdir, ".git", "crabbox", "sync-manifest"), "previous.txt\x00")
			}
			if out, err := runOverlayCommand(t, remotePrepareGitOverlay(workdir, fixture.plan), nil); err != nil {
				t.Fatalf("prepare failed: %v\n%s", err, out)
			}
			assertNoGitOverlayResidue(t, workdir)
			if got := gitOutput(workdir, "rev-parse", "HEAD"); got != fixture.repo.Head {
				t.Fatalf("remote HEAD=%q want=%q", got, fixture.repo.Head)
			}
			if !reuse {
				if gitOutput(workdir, "rev-parse", "--is-shallow-repository") != "false" {
					t.Fatal("fresh overlay omitted required commit ancestry")
				}
				if got := gitOutput(workdir, "rev-parse", "HEAD^"); got == "" {
					t.Fatal("fresh overlay omitted the parent commit")
				}
				if err := exec.Command("git", "-C", workdir, "--no-lazy-fetch", "cat-file", "-e", fixture.historicalBlob).Run(); err == nil {
					t.Fatal("fresh overlay materialized an unrelated historical blob")
				}
			} else {
				if content, err := os.ReadFile(filepath.Join(workdir, "node_modules", "cached.txt")); err != nil || string(content) != "trusted cache\n" {
					t.Fatalf("trusted dependency cache=%q err=%v", content, err)
				}
				if _, err := os.Stat(filepath.Join(workdir, ".pnpm-store")); !os.IsNotExist(err) {
					t.Fatalf("mutable .git/info/exclude authorized a stale cache: %v", err)
				}
				previous, err := os.ReadFile(filepath.Join(workdir, ".git", "crabbox", "sync-manifest"))
				if err != nil || !bytes.Contains(previous, []byte("previous.txt\x00")) || !bytes.Contains(previous, []byte("excluded.txt\x00")) {
					t.Fatalf("previous plus target manifest=%q err=%v", previous, err)
				}
			}
			const token = "0123456789abcdef0123456789abcdef"
			frame := []byte(syncManifestInputForTarget(SSHTarget{TargetOS: targetLinux}, manifest.NUL(), manifest.DeletedNUL()))
			if out, err := runOverlayCommand(t, remoteWriteSyncManifestsNew(workdir, token), frame); err != nil {
				t.Fatalf("write complete authoritative manifests: %v\n%s", err, out)
			}
			if out, err := runOverlayCommand(t, remotePruneGitOverlaySyncManifest(workdir, token), nil); err != nil {
				t.Fatalf("prune reset paths: %v\n%s", err, out)
			}
			for _, removed := range []string{"excluded.txt", "deleted.txt", "renamed.txt", "previous.txt"} {
				if _, err := os.Lstat(filepath.Join(workdir, removed)); !os.IsNotExist(err) {
					t.Fatalf("reset resurrected managed/excluded path %q: %v", removed, err)
				}
			}
			transfer := exec.Command(rsyncPath, "-a", "--files-from=-", "--from0", fixture.root+"/", workdir+"/")
			transfer.Stdin = bytes.NewReader(manifest.OverlayNUL())
			if out, err := transfer.CombinedOutput(); err != nil {
				t.Fatalf("transfer overlay: %v\n%s", err, out)
			}
			finalize := remoteFinalizeSync(workdir, remoteSyncFinalizeOptions{Token: token, Coherence: fixture.plan, Fingerprint: fingerprint, GitOverlay: true})
			if out, err := runOverlayCommand(t, finalize, nil); err != nil {
				t.Fatalf("finalize overlay: %v\n%s", err, out)
			}
			for _, name := range []string{"staged.txt", "unstaged.txt", "untracked.txt", "renamed-new.txt"} {
				local, _ := os.ReadFile(filepath.Join(fixture.root, name))
				remote, err := os.ReadFile(filepath.Join(workdir, name))
				if err != nil || !bytes.Equal(local, remote) {
					t.Fatalf("remote %s=%q want=%q err=%v", name, remote, local, err)
				}
			}
			if link, err := os.Readlink(filepath.Join(workdir, "link")); err != nil || link != "staged.txt" {
				t.Fatalf("symlink=%q err=%v", link, err)
			}
			if info, err := os.Stat(filepath.Join(workdir, "mode.sh")); err != nil || info.Mode().Perm()&0o111 == 0 {
				t.Fatalf("executable mode info=%v err=%v", info, err)
			}
			if got, err := os.ReadFile(filepath.Join(workdir, ".git", "crabbox", "sync-fingerprint")); err != nil || string(got) != fingerprint {
				t.Fatalf("overlay fingerprint=%q err=%v", got, err)
			}
			if _, err := os.Stat(filepath.Join(workdir, ".git", "crabbox", "git-hydrate-base")); !os.IsNotExist(err) {
				t.Fatalf("overlay triggered ordinary hydration: %v", err)
			}
		})
	}
}

func TestRemoteGitOverlayRejectsLinkedAndUnsafeSharedWorkspaces(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX overlay integration")
	}
	for _, kind := range []string{"linked", "rewrite", "filter", "alternates", "attributes", "symlinked git"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newGitOverlayFixture(t)
			base := filepath.Join(t.TempDir(), "base")
			if out, err := exec.Command("git", "clone", "--quiet", fixture.origin, base).CombinedOutput(); err != nil {
				t.Fatalf("clone workspace: %v\n%s", err, out)
			}
			workdir := base
			switch kind {
			case "linked":
				workdir = filepath.Join(filepath.Dir(base), "linked")
				runGit(t, base, "worktree", "add", "--detach", workdir, fixture.repo.Head)
			case "rewrite":
				runGit(t, workdir, "config", "url.ext::attacker.insteadOf", fixture.origin)
			case "filter":
				runGit(t, workdir, "config", "filter.attacker.smudge", "false")
			case "alternates":
				mustWriteTestFile(t, filepath.Join(workdir, ".git", "objects", "info", "alternates"), filepath.Join(fixture.root, ".git", "objects")+"\n")
			case "attributes":
				mustWriteTestFile(t, filepath.Join(workdir, ".git", "info", "attributes"), "*.txt filter=attacker\n")
			case "symlinked git":
				gitDir := filepath.Join(filepath.Dir(base), "external.git")
				if err := os.Rename(filepath.Join(workdir, ".git"), gitDir); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(gitDir, filepath.Join(workdir, ".git")); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(filepath.Join(base, ".git", "config"))
			if kind == "symlinked git" {
				before, err = os.ReadFile(filepath.Join(filepath.Dir(base), "external.git", "config"))
			}
			if err != nil {
				t.Fatal(err)
			}
			output, prepareErr := runOverlayCommand(t, remotePrepareGitOverlay(workdir, fixture.plan), nil)
			if reason, ok := gitOverlayFallbackResult(string(output), prepareErr); !ok || reason != "unsafe_git_workspace" {
				t.Fatalf("reason=%q fallback=%t err=%v output=%q", reason, ok, prepareErr, output)
			}
			afterPath := filepath.Join(base, ".git", "config")
			if kind == "symlinked git" {
				afterPath = filepath.Join(filepath.Dir(base), "external.git", "config")
			}
			after, err := os.ReadFile(afterPath)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("shared/local configuration was mutated: before=%q after=%q err=%v", before, after, err)
			}
			if kind == "linked" {
				const token = "11223344556677889900aabbccddeeff"
				frame := []byte(syncManifestInputForTarget(SSHTarget{TargetOS: targetLinux}, []byte("clean.txt\x00"), nil))
				writer := remoteWriteSyncManifestsNew(workdir, token)
				if output, err := runOverlayCommand(t, writer, frame); err != nil {
					t.Fatalf("write unchanged legacy fallback manifest: %v\n%s", err, output)
				}
				finalize := remoteFinalizeSync(workdir, remoteSyncFinalizeOptions{Token: token})
				if output, err := runOverlayCommand(t, finalize, nil); err != nil {
					t.Fatalf("finalize unchanged legacy fallback: %v\n%s", err, output)
				}
				if _, err := os.Stat(filepath.Join(workdir, ".crabbox", "sync-manifest")); !os.IsNotExist(err) {
					t.Fatalf("linked fallback relocated its established metadata: %v", err)
				}
				legacyMetadata := gitOutput(workdir, "rev-parse", "--git-path", "crabbox")
				if !filepath.IsAbs(legacyMetadata) {
					legacyMetadata = filepath.Join(workdir, legacyMetadata)
				}
				if _, err := os.Stat(filepath.Join(legacyMetadata, "sync-manifest")); err != nil {
					t.Fatalf("linked fallback did not preserve legacy metadata authority: %v", err)
				}
			}
			assertNoGitOverlayResidue(t, workdir)
		})
	}
}

func TestRemoteGitOverlayPrivateHTTPFallsBackWithoutCredentials(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX overlay integration")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "" {
			t.Errorf("overlay forwarded an Authorization header")
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="private"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
	}))
	defer server.Close()
	plan := gitCoherencePlan{RemoteURL: server.URL + "/private.git", Target: strings.Repeat("a", 40), Tree: strings.Repeat("b", 40), Branch: "main"}
	workdir := filepath.Join(t.TempDir(), "workspace")
	output, err := runOverlayCommand(t, remotePrepareGitOverlay(workdir, plan), nil)
	if reason, fallback := gitOverlayFallbackResult(string(output), err); !fallback || reason != "origin_auth_required" {
		t.Fatalf("private origin fallback=%t reason=%q err=%v output=%q", fallback, reason, err, output)
	}
	assertNoGitOverlayResidue(t, workdir)
}

func TestRemoteGitOverlayTransportFailuresRemainFatal(t *testing.T) {
	plan := gitCoherencePlan{RemoteURL: "http://127.0.0.1:1/missing.git", Target: strings.Repeat("a", 40), Tree: strings.Repeat("b", 40), Branch: "main"}
	output, err := runOverlayCommand(t, remotePrepareGitOverlay(filepath.Join(t.TempDir(), "workspace"), plan), nil)
	if err == nil {
		t.Fatalf("transport failure unexpectedly succeeded: %q", output)
	}
	if reason, fallback := gitOverlayFallbackResult(string(output), err); fallback {
		t.Fatalf("transport failure was downgraded to fallback %q: %q", reason, output)
	}
}

func TestRemoteGitOverlayPruneRejectsSymlinkAncestors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlink integration")
	}
	fixture := newGitOverlayFixture(t)
	workdir := filepath.Join(t.TempDir(), "workspace")
	if out, err := runOverlayCommand(t, remotePrepareGitOverlay(workdir, fixture.plan), nil); err != nil {
		t.Fatalf("prepare overlay: %v\n%s", err, out)
	}
	outside := t.TempDir()
	protected := filepath.Join(outside, "protected.txt")
	mustWriteTestFile(t, protected, "must survive\n")
	if err := os.Symlink(outside, filepath.Join(workdir, "transition")); err != nil {
		t.Fatal(err)
	}
	const token = "fedcba9876543210fedcba9876543210"
	meta := filepath.Join(workdir, ".git", "crabbox")
	mustWriteTestFile(t, filepath.Join(meta, remoteSyncPendingManifestName(token)), "")
	mustWriteTestFile(t, filepath.Join(meta, remoteSyncPendingDeletedName(token)), "transition/protected.txt\x00")
	output, err := runOverlayCommand(t, remotePruneGitOverlaySyncManifest(workdir, token), nil)
	if err == nil || !strings.Contains(string(output), "refuses symlink ancestor") {
		t.Fatalf("symlink ancestor deletion was accepted: err=%v output=%q", err, output)
	}
	if content, err := os.ReadFile(protected); err != nil || string(content) != "must survive\n" {
		t.Fatalf("outside sentinel=%q err=%v", content, err)
	}
}

func TestRemoteGitOverlayPruneRejectsProtectedAndMalformedManifestPaths(t *testing.T) {
	fixture := newGitOverlayFixture(t)
	workdir := filepath.Join(t.TempDir(), "workspace")
	if output, err := runOverlayCommand(t, remotePrepareGitOverlay(workdir, fixture.plan), nil); err != nil {
		t.Fatalf("prepare overlay: %v\n%s", err, output)
	}
	const token = "01230123012301230123012301230123"
	meta := filepath.Join(workdir, ".git", "crabbox")
	runtimeFile := filepath.Join(workdir, ".crabbox", "env", "helper")
	mustWriteTestFile(t, runtimeFile, "reserved runtime helper\n")
	for _, entry := range []string{".git/config", ".crabbox/env/helper", "nested/.git/config", "nested/.crabbox/env/helper", "../outside", "/absolute", "nested//file", "./file", "nested/./file", "nested/../file", ""} {
		t.Run(fmt.Sprintf("%q", entry), func(t *testing.T) {
			mustWriteTestFile(t, filepath.Join(workdir, "managed-sentinel"), "managed sentinel\n")
			mustWriteTestFile(t, filepath.Join(meta, "sync-manifest"), "managed-sentinel\x00"+entry+"\x00")
			mustWriteTestFile(t, filepath.Join(meta, remoteSyncPendingManifestName(token)), "")
			mustWriteTestFile(t, filepath.Join(meta, remoteSyncPendingDeletedName(token)), "")
			output, err := runOverlayCommand(t, remotePruneGitOverlaySyncManifest(workdir, token), nil)
			if err == nil || !strings.Contains(string(output), "unsafe overlay deletion path") {
				t.Fatalf("malicious historical entry accepted: err=%v output=%q", err, output)
			}
			for _, protected := range []string{runtimeFile, filepath.Join(workdir, "managed-sentinel"), filepath.Join(workdir, ".git", "config")} {
				if _, err := os.Stat(protected); err != nil {
					t.Fatalf("protected path %q was touched: %v", protected, err)
				}
			}
		})
	}
	mustWriteTestFile(t, filepath.Join(meta, "sync-manifest"), "unterminated")
	if output, err := runOverlayCommand(t, remotePruneGitOverlaySyncManifest(workdir, token), nil); err == nil || !strings.Contains(string(output), "malformed overlay deletion manifest") {
		t.Fatalf("unterminated historical manifest accepted: err=%v output=%q", err, output)
	}
	mustWriteTestFile(t, filepath.Join(meta, "sync-manifest"), "")
	mustWriteTestFile(t, filepath.Join(meta, remoteSyncPendingDeletedName(token)), ".crabbox/env/helper\x00")
	if output, err := runOverlayCommand(t, remotePruneGitOverlaySyncManifest(workdir, token), nil); err == nil || !strings.Contains(string(output), "unsafe overlay deletion path") {
		t.Fatalf("protected deleted-manifest entry accepted: err=%v output=%q", err, output)
	}
}

func TestRemoteGitOverlayPruneHandlesSafeShapeTransitions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlink integration")
	}
	fixture := newGitOverlayFixture(t)
	workdir := filepath.Join(t.TempDir(), "workspace")
	if output, err := runOverlayCommand(t, remotePrepareGitOverlay(workdir, fixture.plan), nil); err != nil {
		t.Fatalf("prepare overlay: %v\n%s", err, output)
	}
	const token = "45674567456745674567456745674567"
	meta := filepath.Join(workdir, ".git", "crabbox")
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "protected.txt")
	mustWriteTestFile(t, sentinel, "must remain outside\n")
	shape := filepath.Join(workdir, "shape")
	if err := os.Symlink(outside, shape); err != nil {
		t.Fatal(err)
	}
	mustWriteTestFile(t, filepath.Join(meta, "sync-manifest"), "shape/protected.txt\x00")
	mustWriteTestFile(t, filepath.Join(meta, remoteSyncPendingManifestName(token)), "shape\x00")
	mustWriteTestFile(t, filepath.Join(meta, remoteSyncPendingDeletedName(token)), "shape/protected.txt\x00")
	if output, err := runOverlayCommand(t, remotePruneGitOverlaySyncManifest(workdir, token), nil); err != nil {
		t.Fatalf("directory-to-symlink transition rejected: %v\n%s", err, output)
	}
	if content, err := os.ReadFile(sentinel); err != nil || string(content) != "must remain outside\n" {
		t.Fatalf("outside shape-transition sentinel=%q err=%v", content, err)
	}
	if err := os.Remove(shape); err != nil {
		t.Fatal(err)
	}
	mustWriteTestFile(t, filepath.Join(shape, "new.txt"), "new directory content\n")
	mustWriteTestFile(t, filepath.Join(meta, "sync-manifest"), "shape\x00")
	mustWriteTestFile(t, filepath.Join(meta, remoteSyncPendingManifestName(token)), "shape/new.txt\x00")
	mustWriteTestFile(t, filepath.Join(meta, remoteSyncPendingDeletedName(token)), "shape\x00")
	if output, err := runOverlayCommand(t, remotePruneGitOverlaySyncManifest(workdir, token), nil); err != nil {
		t.Fatalf("symlink-to-directory transition rejected: %v\n%s", err, output)
	}
	if _, err := os.Stat(filepath.Join(shape, "new.txt")); err != nil {
		t.Fatalf("replacement directory was removed: %v", err)
	}
	if err := os.Remove(filepath.Join(shape, "new.txt")); err != nil {
		t.Fatal(err)
	}
	mustWriteTestFile(t, filepath.Join(shape, "stale.txt"), "obsolete descendant\n")
	mustWriteTestFile(t, filepath.Join(meta, "sync-manifest"), "shape/stale.txt\x00")
	mustWriteTestFile(t, filepath.Join(meta, remoteSyncPendingManifestName(token)), "shape\x00")
	mustWriteTestFile(t, filepath.Join(meta, remoteSyncPendingDeletedName(token)), "shape/stale.txt\x00")
	if output, err := runOverlayCommand(t, remotePruneGitOverlaySyncManifest(workdir, token), nil); err != nil {
		t.Fatalf("stale descendant blocked pending leaf replacement: %v\n%s", err, output)
	}
	if _, err := os.Lstat(shape); !os.IsNotExist(err) {
		t.Fatalf("obsolete descendant directory was not removed: %v", err)
	}
	mustWriteTestFile(t, shape, "obsolete file obstruction\n")
	mustWriteTestFile(t, filepath.Join(meta, "sync-manifest"), "shape\x00")
	mustWriteTestFile(t, filepath.Join(meta, remoteSyncPendingManifestName(token)), "shape/new.txt\x00")
	mustWriteTestFile(t, filepath.Join(meta, remoteSyncPendingDeletedName(token)), "shape\x00")
	if output, err := runOverlayCommand(t, remotePruneGitOverlaySyncManifest(workdir, token), nil); err != nil {
		t.Fatalf("stale leaf blocked pending directory replacement: %v\n%s", err, output)
	}
	if _, err := os.Lstat(shape); !os.IsNotExist(err) {
		t.Fatalf("obsolete file obstruction was not removed: %v", err)
	}
	if err := os.Symlink(outside, shape); err != nil {
		t.Fatal(err)
	}
	if output, err := runOverlayCommand(t, remotePruneGitOverlaySyncManifest(workdir, token), nil); err != nil {
		t.Fatalf("stale symlink blocked pending directory replacement: %v\n%s", err, output)
	}
	if _, err := os.Lstat(shape); !os.IsNotExist(err) {
		t.Fatalf("obsolete symlink obstruction was not removed: %v", err)
	}
	if content, err := os.ReadFile(sentinel); err != nil || string(content) != "must remain outside\n" {
		t.Fatalf("obsolete symlink removal escaped the workspace: sentinel=%q err=%v", content, err)
	}
}

func TestRemoteGitOverlayPruneRejectsExcessiveDeletionsBeforeMutation(t *testing.T) {
	fixture := newGitOverlayFixture(t)
	workdir := filepath.Join(t.TempDir(), "workspace")
	if output, err := runOverlayCommand(t, remotePrepareGitOverlay(workdir, fixture.plan), nil); err != nil {
		t.Fatalf("prepare overlay: %v\n%s", err, output)
	}
	const token = "67896789678967896789678967896789"
	meta := filepath.Join(workdir, ".git", "crabbox")
	var old strings.Builder
	for index := range 200 {
		name := fmt.Sprintf("managed-%03d.txt", index)
		mustWriteTestFile(t, filepath.Join(workdir, name), "managed\n")
		old.WriteString(name)
		old.WriteByte(0)
	}
	mustWriteTestFile(t, filepath.Join(meta, "sync-manifest"), old.String())
	mustWriteTestFile(t, filepath.Join(meta, remoteSyncPendingManifestName(token)), "")
	mustWriteTestFile(t, filepath.Join(meta, remoteSyncPendingDeletedName(token)), "")
	if output, err := runOverlayCommand(t, remotePruneGitOverlaySyncManifest(workdir, token), nil); err == nil || !strings.Contains(string(output), "200 pending deletions") {
		t.Fatalf("mass deletion was not rejected: err=%v output=%q", err, output)
	}
	if _, err := os.Stat(filepath.Join(workdir, "managed-000.txt")); err != nil {
		t.Fatalf("mass-deletion guard ran after mutation: %v", err)
	}
}

func TestRemoteGitOverlayRejectsEscapingIgnoredCacheRoots(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlink integration")
	}
	fixture := newGitOverlayFixture(t)
	workdir := filepath.Join(t.TempDir(), "workspace")
	if out, err := exec.Command("git", "clone", "--quiet", fixture.origin, workdir).CombinedOutput(); err != nil {
		t.Fatalf("clone workspace: %v\n%s", err, out)
	}
	outside := t.TempDir()
	protected := filepath.Join(outside, "protected.txt")
	mustWriteTestFile(t, protected, "cache target must survive\n")
	if err := os.Symlink(outside, filepath.Join(workdir, "node_modules")); err != nil {
		t.Fatal(err)
	}
	output, err := runOverlayCommand(t, remotePrepareGitOverlay(workdir, fixture.plan), nil)
	if reason, fallback, mutated := gitOverlayFallbackOutcome(string(output), err); !fallback || !mutated || reason != "unsafe_cache_root" {
		t.Fatalf("cache fallback=%t mutated=%t reason=%q err=%v output=%q", fallback, mutated, reason, err, output)
	}
	if content, err := os.ReadFile(protected); err != nil || string(content) != "cache target must survive\n" {
		t.Fatalf("outside cache sentinel=%q err=%v", content, err)
	}
	assertNoGitOverlayResidue(t, workdir)
}

func TestRemoteGitOverlayRejectsUnfilteredHistory(t *testing.T) {
	fixture := newGitOverlayFixture(t)
	runGit(t, fixture.origin, "config", "uploadpack.allowFilter", "false")
	workdir := filepath.Join(t.TempDir(), "workspace")
	output, err := runOverlayCommand(t, remotePrepareGitOverlay(workdir, fixture.plan), nil)
	if reason, fallback, mutated := gitOverlayFallbackOutcome(string(output), err); !fallback || mutated || reason != "filtered_history_unsupported" {
		t.Fatalf("unfiltered history fallback=%t mutated=%t reason=%q err=%v output=%q", fallback, mutated, reason, err, output)
	}
	if _, err := os.Stat(workdir); !os.IsNotExist(err) {
		t.Fatalf("unfiltered remote mutated the workspace: %v", err)
	}
}

func TestRemoteGitOverlayRetainsFilteredFeatureAndBaseHistory(t *testing.T) {
	fixture := newGitOverlayFixture(t)
	largeHistory := filepath.Join(fixture.root, "large-history.bin")
	mustWriteTestFile(t, largeHistory, strings.Repeat("historical blob that must remain remote\n", 32768))
	runGit(t, fixture.root, "add", "large-history.bin")
	runGit(t, fixture.root, "commit", "-qm", "historical large blob")
	largeBlob := gitOutput(fixture.root, "rev-parse", "HEAD:large-history.bin")
	runGit(t, fixture.root, "rm", "-q", "large-history.bin")
	runGit(t, fixture.root, "commit", "-qm", "remove historical large blob")
	runGit(t, fixture.root, "push", "-q", "origin", "main")
	baseSHA := gitOutput(fixture.root, "rev-parse", "HEAD")
	runGit(t, fixture.root, "checkout", "-qb", "feature")
	mustWriteTestFile(t, filepath.Join(fixture.root, "feature.txt"), "target feature change\n")
	runGit(t, fixture.root, "add", "feature.txt")
	runGit(t, fixture.root, "commit", "-qm", "feature target")
	targetSHA := gitOutput(fixture.root, "rev-parse", "HEAD")
	mustWriteTestFile(t, filepath.Join(fixture.root, "tip-only.txt"), "newer branch tip\n")
	runGit(t, fixture.root, "add", "tip-only.txt")
	runGit(t, fixture.root, "commit", "-qm", "newer feature tip")
	tipSHA := gitOutput(fixture.root, "rev-parse", "HEAD")
	runGit(t, fixture.root, "push", "-q", "origin", "feature")
	runGit(t, fixture.root, "fetch", "-q", "origin", "+refs/heads/*:refs/remotes/origin/*")
	runGit(t, fixture.root, "checkout", "-q", "--detach", targetSHA)
	fixture.repo.Head = targetSHA
	fixture.cfg.Sync.BaseRef = "main"
	plan, blocked := syncGitCoherencePlan(fixture.cfg, fixture.repo)
	if blocked || plan.Branch != "feature" {
		t.Fatalf("feature coherence plan=%#v blocked=%t", plan, blocked)
	}
	workdir := filepath.Join(t.TempDir(), "workspace")
	if output, err := runOverlayCommand(t, remotePrepareGitOverlayWithBase(workdir, plan, "main", baseSHA), nil); err != nil {
		t.Fatalf("prepare filtered feature history: %v\n%s", err, output)
	}
	for ref, want := range map[string]string{"HEAD": targetSHA, "refs/remotes/origin/feature": tipSHA, "refs/remotes/origin/main": baseSHA} {
		if got := gitOutput(workdir, "rev-parse", ref); got != want {
			t.Fatalf("remote %s=%q want=%q", ref, got, want)
		}
	}
	if err := exec.Command("git", "-C", workdir, "--no-lazy-fetch", "cat-file", "-e", largeBlob).Run(); err == nil {
		t.Fatal("filtered history downloaded the large deleted historical blob")
	}
	for _, args := range [][]string{{"rev-parse", "HEAD^"}, {"merge-base", "origin/main", "HEAD"}, {"diff", "origin/main...HEAD"}} {
		if output, err := exec.Command("git", append([]string{"-C", workdir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("history workload git %v: %v\n%s", args, err, output)
		}
	}
	manifest, _ := fixture.manifest(t)
	const token = "13579bdf2468ace013579bdf2468ace0"
	frame := []byte(syncManifestInputForTarget(SSHTarget{TargetOS: targetLinux}, manifest.NUL(), manifest.DeletedNUL()))
	if output, err := runOverlayCommand(t, remoteWriteSyncManifestsNewWithMetadata(workdir, token, remotePlainSyncMetaDirScript()), frame); err != nil {
		t.Fatalf("write feature manifest: %v\n%s", err, output)
	}
	if output, err := runOverlayCommand(t, remotePruneGitOverlaySyncManifest(workdir, token), nil); err != nil {
		t.Fatalf("prune feature manifest: %v\n%s", err, output)
	}
	if output, err := runOverlayCommand(t, remoteFinalizeSync(workdir, remoteSyncFinalizeOptions{Token: token, Coherence: plan, GitOverlay: true, BaseRef: "main", BaseSHA: baseSHA}), nil); err != nil {
		t.Fatalf("finalize feature manifest: %v\n%s", err, output)
	}
	if marker, err := os.ReadFile(filepath.Join(workdir, ".git", "crabbox", "git-hydrate-base")); err != nil || string(marker) != "main "+baseSHA+"\n" {
		t.Fatalf("filtered base hydration marker=%q err=%v", marker, err)
	}
}

func TestRemoteGitOverlayRejectsSymlinkedMetadataAndRuntimeState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlink integration")
	}
	for _, kind := range []string{"metadata directory", "metadata manifest", "metadata fingerprint", "runtime directory", "runtime env"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newGitOverlayFixture(t)
			workdir := filepath.Join(t.TempDir(), "workspace")
			if output, err := exec.Command("git", "clone", "--quiet", fixture.origin, workdir).CombinedOutput(); err != nil {
				t.Fatalf("clone workspace: %v\n%s", err, output)
			}
			outside := t.TempDir()
			sentinel := filepath.Join(outside, "sentinel")
			mustWriteTestFile(t, sentinel, "outside remains untouched\n")
			meta := filepath.Join(workdir, ".git", "crabbox")
			var link, destination string
			wantReason := "unsafe_overlay_metadata"
			switch kind {
			case "metadata directory":
				link, destination = meta, outside
			case "metadata manifest", "metadata fingerprint":
				if err := os.Mkdir(meta, 0o755); err != nil {
					t.Fatal(err)
				}
				name := "sync-manifest"
				if kind == "metadata fingerprint" {
					name = "sync-fingerprint"
				}
				link, destination = filepath.Join(meta, name), sentinel
			case "runtime directory":
				link, destination, wantReason = filepath.Join(workdir, ".crabbox"), outside, "unsafe_runtime_state"
			case "runtime env":
				if err := os.Mkdir(filepath.Join(workdir, ".crabbox"), 0o755); err != nil {
					t.Fatal(err)
				}
				link, destination, wantReason = filepath.Join(workdir, ".crabbox", "env"), outside, "unsafe_runtime_state"
			}
			if err := os.Symlink(destination, link); err != nil {
				t.Fatal(err)
			}
			output, err := runOverlayCommand(t, remotePrepareGitOverlay(workdir, fixture.plan), nil)
			if reason, fallback, mutated := gitOverlayFallbackOutcome(string(output), err); !fallback || mutated || reason != wantReason {
				t.Fatalf("unsafe state fallback=%t mutated=%t reason=%q err=%v output=%q", fallback, mutated, reason, err, output)
			}
			if content, err := os.ReadFile(sentinel); err != nil || string(content) != "outside remains untouched\n" {
				t.Fatalf("outside sentinel=%q err=%v", content, err)
			}
		})
	}
}

func TestRemoteGitOverlayAcceptsAdvertisedBranchAncestor(t *testing.T) {
	fixture := newGitOverlayFixture(t)
	plan := fixture.plan
	plan.Target = gitOutput(fixture.root, "rev-parse", "HEAD^")
	plan.Tree = gitOutput(fixture.root, "rev-parse", plan.Target+"^{tree}")
	workdir := filepath.Join(t.TempDir(), "workspace")
	output, err := runOverlayCommand(t, remotePrepareGitOverlay(workdir, plan), nil)
	if err != nil || gitOutput(workdir, "rev-parse", "HEAD") != plan.Target {
		t.Fatalf("reachable advertised ancestor rejected: err=%v output=%q", err, output)
	}
	assertNoGitOverlayResidue(t, workdir)
}

func TestRemoteGitOverlayHooksAndGlobalConfigurationRemainHermetic(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX overlay integration")
	}
	fixture := newGitOverlayFixture(t)
	workdir := filepath.Join(t.TempDir(), "workspace")
	if out, err := exec.Command("git", "clone", "--quiet", fixture.origin, workdir).CombinedOutput(); err != nil {
		t.Fatalf("clone workspace: %v\n%s", err, out)
	}
	attack := t.TempDir()
	hookMarker := filepath.Join(attack, "hook-fired")
	helperMarker := filepath.Join(attack, "helper-fired")
	rewriteMarker := filepath.Join(attack, "rewrite-fired")
	hookBody := "#!/bin/sh\nprintf fired >" + shellQuote(hookMarker) + "\n"
	for _, name := range []string{"post-checkout", "reference-transaction"} {
		if err := os.WriteFile(filepath.Join(workdir, ".git", "hooks", name), []byte(hookBody), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	helper := filepath.Join(attack, "helper.sh")
	rewrite := filepath.Join(attack, "rewrite.sh")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf fired >"+shellQuote(helperMarker)+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rewrite, []byte("#!/bin/sh\nprintf fired >"+shellQuote(rewriteMarker)+"\nexit 99\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(attack, "gitconfig")
	for _, setting := range [][2]string{
		{"credential.helper", helper},
		{"protocol.ext.allow", "always"},
		{"url.ext::" + rewrite + ".insteadOf", fixture.origin},
	} {
		cmd := exec.Command("git", "config", "--file", global, setting[0], setting[1])
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("configure hostile global %q: %v\n%s", setting[0], err, out)
		}
	}
	if output, err := runOverlayCommand(t, remotePrepareGitOverlay(workdir, fixture.plan), nil, "GIT_CONFIG_GLOBAL="+global, "SSH_AUTH_SOCK="+filepath.Join(attack, "agent")); err != nil {
		t.Fatalf("hermetic overlay preparation failed: %v\n%s", err, output)
	}
	manifest, _ := fixture.manifest(t)
	const token = "00112233445566778899aabbccddeeff"
	frame := []byte(syncManifestInputForTarget(SSHTarget{TargetOS: targetLinux}, manifest.NUL(), manifest.DeletedNUL()))
	writer := remoteWriteSyncManifestsNewWithMetadata(workdir, token, "meta_dir=\"$PWD/.git/crabbox\"\n")
	if output, err := runOverlayCommand(t, writer, frame); err != nil {
		t.Fatalf("stage overlay finalization: %v\n%s", err, output)
	}
	finalize := remoteFinalizeSync(workdir, remoteSyncFinalizeOptions{Token: token, Coherence: fixture.plan, GitOverlay: true})
	if output, err := runOverlayCommand(t, finalize, nil, "GIT_CONFIG_GLOBAL="+global, "SSH_AUTH_SOCK="+filepath.Join(attack, "agent")); err != nil {
		t.Fatalf("hermetic overlay finalization failed: %v\n%s", err, output)
	}
	for _, marker := range []string{hookMarker, helperMarker, rewriteMarker} {
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("ambient hook/helper/rewrite executed: %q err=%v", marker, err)
		}
	}
	assertNoGitOverlayResidue(t, workdir)
}

func TestGitOverlayLocalSnapshotDetectsLateContentModesAndIndexChanges(t *testing.T) {
	for _, kind := range []string{"clean file edit", "dirty file edit", "mode change", "staged change", "symlink change", "new file"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "symlink change" && runtime.GOOS == "windows" {
				t.Skip("symlink unavailable")
			}
			fixture := newGitOverlayFixture(t)
			if kind == "dirty file edit" {
				mustWriteTestFile(t, filepath.Join(fixture.root, "unstaged.txt"), "first dirty value\n")
			}
			before, _ := fixture.manifest(t)
			beforeSnapshot, err := gitOverlayLocalSnapshot(fixture.repo, before)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "clean file edit", "dirty file edit":
				mustWriteTestFile(t, filepath.Join(fixture.root, "unstaged.txt"), "late changed value\n")
			case "mode change":
				if err := os.Chmod(filepath.Join(fixture.root, "mode.sh"), 0o755); err != nil {
					t.Fatal(err)
				}
			case "staged change":
				mustWriteTestFile(t, filepath.Join(fixture.root, "staged.txt"), "late staged value\n")
				runGit(t, fixture.root, "add", "staged.txt")
			case "symlink change":
				if err := os.Remove(filepath.Join(fixture.root, "link")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("unstaged.txt", filepath.Join(fixture.root, "link")); err != nil {
					t.Fatal(err)
				}
			case "new file":
				mustWriteTestFile(t, filepath.Join(fixture.root, "late.txt"), "late new file\n")
			}
			after, _ := fixture.manifest(t)
			afterSnapshot, err := gitOverlayLocalSnapshot(fixture.repo, after)
			if err != nil {
				t.Fatal(err)
			}
			if beforeSnapshot == afterSnapshot && sameSyncManifest(before, after) {
				t.Fatalf("late %s escaped post-preparation validation", kind)
			}
		})
	}
}

func TestGitOverlayFingerprintPreservesLegacySchemaAndHashesLinkIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlink integration")
	}
	fixture := newGitOverlayFixture(t)
	manifest, excludes := fixture.manifest(t)
	legacy := fixture.cfg
	legacy.Sync.GitOverlay = false
	got, err := syncFingerprintForManifest(fixture.repo, legacy, manifest, excludes, fixture.plan)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.New()
	fmt.Fprintf(want, "v5\nremote=%s\nbranch=%s\nhead=%s\ntree=%s\n", fixture.plan.RemoteURL, fixture.plan.Branch, fixture.plan.Target, fixture.plan.Tree)
	fmt.Fprintf(want, "delete=%t\nchecksum=%t\n", legacy.Sync.Delete, legacy.Sync.Checksum)
	fmt.Fprintf(want, "manifest=%x\n", sha256.Sum256(manifest.NUL()))
	fmt.Fprintf(want, "deleted=%x\n", sha256.Sum256(manifest.DeletedNUL()))
	for _, rule := range excludes.rules {
		fmt.Fprintf(want, "exclude=%d:%s\n", rule.origin, rule.pattern)
	}
	if expected := hex.EncodeToString(want.Sum(nil)); got != expected {
		t.Fatalf("default-off fingerprint changed: got=%q want=%q", got, expected)
	}
	outside := filepath.Join(filepath.Dir(fixture.root), "outside.txt")
	mustWriteTestFile(t, outside, "first external content\n")
	if err := os.Remove(filepath.Join(fixture.root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../outside.txt", filepath.Join(fixture.root, "link")); err != nil {
		t.Fatal(err)
	}
	manifest, excludes = fixture.manifest(t)
	first, err := syncFingerprintForManifest(fixture.repo, fixture.cfg, manifest, excludes, fixture.plan)
	if err != nil {
		t.Fatal(err)
	}
	mustWriteTestFile(t, outside, "second external content\n")
	second, err := syncFingerprintForManifest(fixture.repo, fixture.cfg, manifest, excludes, fixture.plan)
	if err != nil || first != second {
		t.Fatalf("overlay fingerprint followed symlink: first=%q second=%q err=%v", first, second, err)
	}
}

func TestGitOverlayTimingFieldsAreAdditiveAndDefaultOff(t *testing.T) {
	legacy, err := json.Marshal(timingReportFromRun("static", "lease", "slug", runTimings{}, time.Second, 0))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"syncMode", "syncTransferFiles", "syncTransferBytes", "syncFallbackReason"} {
		if bytes.Contains(legacy, []byte(field)) {
			t.Fatalf("default-off timing unexpectedly contains %s: %s", field, legacy)
		}
	}
	report := timingReportFromRun("static", "lease", "slug", runTimings{
		syncMode: "git-overlay", syncTransferFiles: 2, syncTransferBytes: 42, syncFallbackReason: "origin_auth_required",
	}, time.Second, 0)
	if report.SyncMode != "git-overlay" || report.SyncTransferFiles != 2 || report.SyncTransferBytes != 42 || report.SyncFallbackReason != "origin_auth_required" {
		t.Fatalf("overlay telemetry=%#v", report)
	}
}

func TestRunGitOverlaySuccessFallbackAndLateLocalEdit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX SSH runtime integration")
	}
	for _, mode := range []string{"success", "runtime-state", "classified-fallback", "private-origin", "local-ineligible", "local-fingerprint-reuse", "remote-fingerprint-reuse", "unsafe-metadata", "checkout-file-obstruction", "post-reset-cache", "post-reset-mass-deletion", "late-local-edit"} {
		t.Run(mode, func(t *testing.T) {
			clearConfigEnv(t)
			fixture := newGitOverlayFixture(t)
			if mode == "private-origin" {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					if request.Header.Get("Authorization") != "" {
						t.Errorf("overlay forwarded origin credentials")
					}
					w.Header().Set("WWW-Authenticate", `Basic realm="private"`)
					http.Error(w, "authentication required", http.StatusUnauthorized)
				}))
				t.Cleanup(server.Close)
				runGit(t, fixture.root, "remote", "set-url", "origin", server.URL+"/private.git")
			}
			if mode == "local-fingerprint-reuse" {
				runGit(t, fixture.root, "config", "core.autocrlf", "true")
			}
			if mode == "checkout-file-obstruction" {
				mustWriteTestFile(t, filepath.Join(fixture.root, "shape", "new.txt"), "replacement tracked directory file\n")
				runGit(t, fixture.root, "add", "shape/new.txt")
				runGit(t, fixture.root, "commit", "-qm", "replace obsolete managed file with tracked directory")
				runGit(t, fixture.root, "push", "-q", "origin", "main")
				runGit(t, fixture.root, "fetch", "-q", "origin", "+refs/heads/*:refs/remotes/origin/*")
			}
			if mode == "post-reset-mass-deletion" {
				for index := range 200 {
					mustWriteTestFile(t, filepath.Join(fixture.root, "bulk", fmt.Sprintf("tracked-%03d.txt", index)), "excluded tracked bulk\n")
				}
				runGit(t, fixture.root, "add", "bulk")
				runGit(t, fixture.root, "commit", "-qm", "tracked files excluded from synchronization")
				runGit(t, fixture.root, "push", "-q", "origin", "main")
				runGit(t, fixture.root, "fetch", "-q", "origin", "+refs/heads/*:refs/remotes/origin/*")
			}
			t.Chdir(fixture.root)
			testRoot := t.TempDir()
			isolateRunTestUserDirs(t, testRoot)
			repo, err := findRepo()
			if err != nil {
				t.Fatal(err)
			}
			remoteRoot := filepath.Join(testRoot, "remote")
			configPath := filepath.Join(testRoot, "config.yaml")
			config := fmt.Sprintf("workRoot: %q\nsync:\n  gitOverlay: true\n  gitSeed: true\n  delete: true\n  fingerprint: %t\n  exclude:\n    - excluded.txt\n", remoteRoot, mode == "local-fingerprint-reuse" || mode == "remote-fingerprint-reuse")
			if mode == "post-reset-mass-deletion" {
				config += "    - bulk\n"
			}
			if mode == "local-ineligible" {
				config += "  include:\n    - clean.txt\n"
			}
			if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CRABBOX_CONFIG", configPath)
			binDir := filepath.Join(testRoot, "bin")
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				t.Fatal(err)
			}
			sshLog := filepath.Join(testRoot, "ssh.log")
			rsyncLog := filepath.Join(testRoot, "rsync-manifest")
			attackHome := filepath.Join(testRoot, "hostile-home")
			attackBin := filepath.Join(testRoot, "hostile-bin")
			attackMarker := filepath.Join(testRoot, "hostile-executed")
			if err := os.MkdirAll(attackBin, 0o755); err != nil {
				t.Fatal(err)
			}
			mustWriteTestFile(t, filepath.Join(attackHome, ".bash_profile"), "printf profile >"+shellQuote(attackMarker)+"\n")
			for _, name := range []string{"python3", "python", "perl", "dd", "find", "git"} {
				if err := os.WriteFile(filepath.Join(attackBin, name), []byte("#!/bin/sh\nprintf hostile >"+shellQuote(attackMarker)+"\nexit 99\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			workdir := filepath.Join(remoteRoot, "cbx_overlay_runtime", repo.Name)
			if mode == "runtime-state" || mode == "checkout-file-obstruction" || mode == "post-reset-cache" || mode == "post-reset-mass-deletion" || mode == "classified-fallback" || mode == "local-fingerprint-reuse" || mode == "remote-fingerprint-reuse" || mode == "unsafe-metadata" {
				if err := os.MkdirAll(filepath.Dir(workdir), 0o755); err != nil {
					t.Fatal(err)
				}
				if output, err := exec.Command("git", "clone", "--quiet", fixture.origin, workdir).CombinedOutput(); err != nil {
					t.Fatalf("clone existing runtime workspace: %v\n%s", err, output)
				}
				if mode == "runtime-state" || mode == "post-reset-cache" || mode == "post-reset-mass-deletion" {
					mustWriteTestFile(t, filepath.Join(workdir, ".crabbox", "env", "persisted-helper"), "runtime helper survives\n")
				} else if mode == "classified-fallback" {
					mustWriteTestFile(t, filepath.Join(workdir, ".git", "crabbox", "sync-manifest"), "clean.txt\x00excluded.txt\x00")
				}
				if mode == "checkout-file-obstruction" {
					runGit(t, workdir, "checkout", "-q", "--detach", fixture.repo.Head)
					mustWriteTestFile(t, filepath.Join(workdir, "shape"), "stale untracked managed file\n")
					mustWriteTestFile(t, filepath.Join(workdir, ".git", "crabbox", "sync-manifest"), "shape\x00")
				}
				if mode == "local-fingerprint-reuse" || mode == "remote-fingerprint-reuse" {
					legacyConfig, err := loadConfig()
					if err != nil {
						t.Fatal(err)
					}
					legacyConfig.Sync.GitOverlay = false
					excludes, err := syncExcludes(repo.Root, legacyConfig)
					if err != nil {
						t.Fatal(err)
					}
					manifest, err := syncManifestFilteredRules(repo.Root, excludes, syncIncludes(legacyConfig))
					if err != nil {
						t.Fatal(err)
					}
					coherence, _ := syncGitCoherencePlan(legacyConfig, repo)
					fingerprint, err := syncFingerprintForManifest(repo, legacyConfig, manifest, excludes, coherence)
					if err != nil {
						t.Fatal(err)
					}
					meta := filepath.Join(workdir, ".git", "crabbox")
					mustWriteTestFile(t, filepath.Join(meta, "sync-manifest"), string(manifest.NUL()))
					mustWriteTestFile(t, filepath.Join(meta, "sync-finalize-token"), "legacy-token")
					mustWriteTestFile(t, filepath.Join(meta, "sync-finalize-complete-token"), "legacy-token")
					mustWriteTestFile(t, filepath.Join(meta, "sync-fingerprint"), fingerprint)
					mustWriteTestFile(t, filepath.Join(meta, "git-hydrate-base"), "main "+repo.Head+"\n")
					if err := os.Remove(filepath.Join(workdir, "excluded.txt")); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "unsafe-metadata" {
					outside := filepath.Join(testRoot, "outside-metadata")
					mustWriteTestFile(t, filepath.Join(outside, "sentinel"), "outside metadata remains untouched\n")
					if err := os.Symlink(outside, filepath.Join(workdir, ".git", "crabbox")); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "post-reset-cache" || mode == "post-reset-mass-deletion" {
					mustWriteTestFile(t, filepath.Join(workdir, ".git", "crabbox", "sync-manifest"), "clean.txt\x00")
					if err := os.Remove(filepath.Join(workdir, "excluded.txt")); err != nil {
						t.Fatal(err)
					}
					outside := filepath.Join(testRoot, "outside-cache")
					mustWriteTestFile(t, filepath.Join(outside, "sentinel"), "outside cache remains untouched\n")
					if err := os.Symlink(outside, filepath.Join(workdir, "node_modules")); err != nil {
						t.Fatal(err)
					}
				}
			}
			installWorkspaceOwnerAwareSSH(t, filepath.Join(binDir, "ssh"), fmt.Sprintf(`#!/bin/sh
cmd="$1"
printf '%%s\n---\n' "$cmd" >> "$CRABBOX_FAKE_SSH_LOG"
case "$cmd" in
  *crabbox-ready*) exit 0 ;;
  *".overlay-git."*)
    case "$CRABBOX_FAKE_OVERLAY_MODE" in
      classified-fallback|remote-fingerprint-reuse) printf '%%scheckout_failed\n' %s >&2; exit %d ;;
      checkout-file-obstruction)
        /bin/chmod 0555 "$CRABBOX_FAKE_OVERLAY_WORKDIR"
        /usr/bin/env HOME="$CRABBOX_FAKE_ATTACK_HOME" PATH="$CRABBOX_FAKE_ATTACK_BIN:/usr/bin:/bin" /bin/bash --noprofile --norc -c "$cmd"
        code=$?
        /bin/chmod 0755 "$CRABBOX_FAKE_OVERLAY_WORKDIR"
        exit "$code"
        ;;
      late-local-edit) printf 'late local change\n' > "$CRABBOX_FAKE_REPO_ROOT/clean.txt" ;;
    esac
    ;;
esac
case "$CRABBOX_FAKE_OVERLAY_MODE" in
  success|runtime-state|checkout-file-obstruction|post-reset-cache|post-reset-mass-deletion|late-local-edit)
    exec /usr/bin/env HOME="$CRABBOX_FAKE_ATTACK_HOME" PATH="$CRABBOX_FAKE_ATTACK_BIN:/usr/bin:/bin" /bin/bash --noprofile --norc -c "$cmd"
    ;;
esac
exec /bin/bash --noprofile --norc -c "$cmd"
`, shellQuote(gitOverlayFallbackMarker), gitOverlayFallbackExitCode))
			rsyncScript := `#!/bin/bash
set -euo pipefail
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
cat > "$tmp"
cp "$tmp" "$CRABBOX_FAKE_RSYNC_LOG"
destination="${!#}"
workdir="${destination#*:}"
workdir="${workdir%%/}"
while IFS= read -r -d '' rel; do
  mkdir -p "$workdir/$(dirname "$rel")"
  cp -a "$CRABBOX_FAKE_REPO_ROOT/$rel" "$workdir/$rel"
done < "$tmp"
`
			if err := os.WriteFile(filepath.Join(binDir, "rsync"), []byte(rsyncScript), 0o755); err != nil {
				t.Fatal(err)
			}
			providerName := runEnvProfileTestProvider{}.Name()
			runEnvProfileTestAcquireLease = func(AcquireRequest) (LeaseTarget, error) {
				return LeaseTarget{Server: Server{Provider: providerName}, SSH: SSHTarget{
					User: "crabbox", Host: "127.0.0.1", Port: "22", TargetOS: targetLinux, SSHConfigProxy: true,
				}, LeaseID: "cbx_overlay_runtime"}, nil
			}
			t.Cleanup(func() { runEnvProfileTestAcquireLease = nil })
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("CRABBOX_FAKE_SSH_LOG", sshLog)
			t.Setenv("CRABBOX_FAKE_RSYNC_LOG", rsyncLog)
			t.Setenv("CRABBOX_FAKE_REPO_ROOT", fixture.root)
			t.Setenv("CRABBOX_FAKE_OVERLAY_MODE", mode)
			t.Setenv("CRABBOX_FAKE_OVERLAY_WORKDIR", workdir)
			t.Setenv("CRABBOX_FAKE_ATTACK_HOME", attackHome)
			t.Setenv("CRABBOX_FAKE_ATTACK_BIN", attackBin)
			t.Setenv("CRABBOX_FAKE_SSH_PORT", "22")
			t.Setenv("CRABBOX_FAKE_SSH_PROXY", "1")
			var stdout, stderr bytes.Buffer
			app := App{Stdout: &stdout, Stderr: &stderr}
			runErr := app.runCommand(context.Background(), []string{"--provider", providerName, "--no-hydrate", "--sync-only"})
			if mode == "unsafe-metadata" {
				if runErr == nil || !strings.Contains(runErr.Error(), "unsafe_overlay_metadata") {
					t.Fatalf("unsafe metadata did not fail closed: err=%v stderr=%s", runErr, stderr.String())
				}
				if sentinel, err := os.ReadFile(filepath.Join(testRoot, "outside-metadata", "sentinel")); err != nil || string(sentinel) != "outside metadata remains untouched\n" {
					t.Fatalf("outside metadata sentinel=%q err=%v", sentinel, err)
				}
				if _, err := os.Stat(rsyncLog); !os.IsNotExist(err) {
					t.Fatalf("unsafe metadata continued into transfer: %v", err)
				}
				return
			}
			if mode == "post-reset-mass-deletion" {
				if runErr == nil || !strings.Contains(runErr.Error(), "remote sync prune failed") {
					t.Fatalf("post-reset mass deletion did not fail closed: err=%v stderr=%s", runErr, stderr.String())
				}
				for _, protected := range []string{"excluded.txt", "bulk/tracked-000.txt"} {
					if _, err := os.Stat(filepath.Join(workdir, filepath.FromSlash(protected))); err != nil {
						t.Fatalf("post-reset mass deletion mutated %q before its safety check: %v", protected, err)
					}
				}
				if _, err := os.Stat(attackMarker); !os.IsNotExist(err) {
					t.Fatalf("post-reset mass-deletion recovery executed hostile profile/PATH: %v", err)
				}
				return
			}
			if runErr != nil {
				t.Fatalf("runtime overlay mode=%s: %v\nstdout=%s\nstderr=%s", mode, runErr, stdout.String(), stderr.String())
			}
			content, err := os.ReadFile(filepath.Join(workdir, "clean.txt"))
			if err != nil {
				t.Fatalf("read synced clean file: %v\nstderr=%s", err, stderr.String())
			}
			switch mode {
			case "success", "runtime-state", "local-fingerprint-reuse", "remote-fingerprint-reuse":
				if _, err := os.Stat(rsyncLog); !os.IsNotExist(err) {
					sshCommands, _ := os.ReadFile(sshLog)
					t.Fatalf("clean overlay/legacy fingerprint unexpectedly transferred a source payload: %v\nstderr=%s\nssh=%s", err, stderr.String(), sshCommands)
				}
				if (mode == "local-fingerprint-reuse" || mode == "remote-fingerprint-reuse") && !strings.Contains(stderr.String(), "No changes detected, skipping sync") {
					t.Fatalf("unmodified fallback discarded the legacy fingerprint: %s", stderr.String())
				}
			case "classified-fallback", "private-origin", "local-ineligible", "checkout-file-obstruction", "post-reset-cache", "late-local-edit":
				transfer, err := os.ReadFile(rsyncLog)
				if err != nil || !bytes.Contains(transfer, []byte("clean.txt\x00")) {
					t.Fatalf("fallback omitted full manifest: data=%q err=%v", transfer, err)
				}
				wantReason := "checkout_failed"
				if mode == "private-origin" {
					wantReason = "origin_auth_required"
				} else if mode == "local-ineligible" {
					wantReason = "include_whitelist"
				} else if mode == "post-reset-cache" {
					wantReason = "unsafe_cache_root"
				} else if mode == "late-local-edit" {
					wantReason = "local_checkout_changed"
					if string(content) != "late local change\n" {
						t.Fatalf("late local edit omitted: %q", content)
					}
				}
				if !strings.Contains(stderr.String(), "git overlay fallback reason="+wantReason) {
					t.Fatalf("fallback reason missing: %s", stderr.String())
				}
				meta := filepath.Join(workdir, ".crabbox")
				if _, err := os.Stat(filepath.Join(workdir, ".git")); err == nil {
					meta = filepath.Join(workdir, ".git", "crabbox")
				}
				if mode == "late-local-edit" || mode == "checkout-file-obstruction" || mode == "post-reset-cache" {
					for _, stale := range []string{"sync-fingerprint", "git-hydrate-base"} {
						if _, err := os.Stat(filepath.Join(meta, stale)); !os.IsNotExist(err) {
							t.Fatalf("mutated-workspace recovery retained %s: %v", stale, err)
						}
					}
				} else if _, err := os.Stat(filepath.Join(meta, "git-hydrate-base")); err != nil {
					t.Fatalf("unmodified fallback lost ordinary Git hydration metadata: %v", err)
				}
			}
			if _, err := os.Stat(filepath.Join(workdir, "excluded.txt")); !os.IsNotExist(err) {
				t.Fatalf("excluded tracked source resurrected: %v", err)
			}
			if mode == "runtime-state" || mode == "post-reset-cache" {
				if helper, err := os.ReadFile(filepath.Join(workdir, ".crabbox", "env", "persisted-helper")); err != nil || string(helper) != "runtime helper survives\n" {
					t.Fatalf("overlay deleted reserved runtime state: helper=%q err=%v", helper, err)
				}
			}
			if mode == "post-reset-cache" {
				if sentinel, err := os.ReadFile(filepath.Join(testRoot, "outside-cache", "sentinel")); err != nil || string(sentinel) != "outside cache remains untouched\n" {
					t.Fatalf("post-reset fallback touched outside cache: sentinel=%q err=%v", sentinel, err)
				}
			}
			if mode == "checkout-file-obstruction" {
				if transferred, err := os.ReadFile(filepath.Join(workdir, "shape", "new.txt")); err != nil || string(transferred) != "replacement tracked directory file\n" {
					t.Fatalf("checkout fallback did not replace file obstruction with target directory: content=%q err=%v", transferred, err)
				}
				if transfer, err := os.ReadFile(rsyncLog); err != nil || !bytes.Contains(transfer, []byte("shape/new.txt\x00")) {
					t.Fatalf("checkout fallback omitted target directory file: transfer=%q err=%v", transfer, err)
				}
			}
			if mode == "success" || mode == "runtime-state" || mode == "checkout-file-obstruction" || mode == "post-reset-cache" || mode == "late-local-edit" {
				if _, err := os.Stat(attackMarker); !os.IsNotExist(err) {
					t.Fatalf("overlay production path executed hostile login profile/PATH: %v", err)
				}
			}
		})
	}
}
