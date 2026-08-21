package cli

import (
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

func TestSyncManifestBuildsCanonicalGitOverlay(t *testing.T) {
	root, repo, cfg, coherence := newGitOverlayTestRepo(t)
	writeFile(t, filepath.Join(root, "staged.txt"), "staged change\n")
	runGit(t, root, "add", "staged.txt")
	writeFile(t, filepath.Join(root, "unstaged.txt"), "unstaged change\n")
	writeFile(t, filepath.Join(root, "untracked.txt"), "untracked\n")
	if err := os.Remove(filepath.Join(root, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "mv", "renamed.txt", "renamed-new.txt")
	if err := os.Chmod(filepath.Join(root, "mode.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Remove(filepath.Join(root, "link")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("staged.txt", filepath.Join(root, "link")); err != nil {
			t.Fatal(err)
		}
	}

	manifest, err := syncManifest(root, configuredExcludes(cfg))
	if err != nil {
		t.Fatal(err)
	}
	wantOverlay := []string{"mode.sh", "renamed-new.txt", "staged.txt", "unstaged.txt", "untracked.txt"}
	if runtime.GOOS != "windows" {
		wantOverlay = append(wantOverlay, "link")
		slices.Sort(wantOverlay)
	}
	if !slices.Equal(manifest.OverlayFiles, wantOverlay) {
		t.Fatalf("overlay files=%v want %v; changed=%v files=%v", manifest.OverlayFiles, wantOverlay, manifest.Changed, manifest.Files)
	}
	for _, wantDeleted := range []string{"deleted.txt", "renamed.txt"} {
		if !slices.Contains(manifest.Deleted, wantDeleted) {
			t.Fatalf("deleted=%v missing %q", manifest.Deleted, wantDeleted)
		}
	}
	if slices.Contains(manifest.OverlayFiles, "clean.txt") {
		t.Fatalf("clean tracked file entered overlay: %v", manifest.OverlayFiles)
	}
	if manifest.OverlayBytes <= 0 || string(manifest.OverlayNUL()) != strings.Join(manifest.OverlayFiles, "\x00")+"\x00" {
		t.Fatalf("overlay encoding bytes=%d data=%q", manifest.OverlayBytes, manifest.OverlayNUL())
	}
	if err := validateGitOverlayManifest(repo, manifest); err != nil {
		t.Fatalf("valid overlay rejected: %v", err)
	}
	decision := decideGitOverlay(cfg, repo, SSHTarget{TargetOS: targetLinux}, manifest, coherence, false, false, false)
	if !decision.Requested || !decision.Enabled || decision.Reason != "" {
		t.Fatalf("decision=%#v", decision)
	}
}

func TestCleanGitOverlayHasNoTrackedPayload(t *testing.T) {
	root, repo, cfg, coherence := newGitOverlayTestRepo(t)
	manifest, err := syncManifest(root, configuredExcludes(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.OverlayFiles) != 0 || manifest.OverlayBytes != 0 || len(manifest.OverlayNUL()) != 0 {
		t.Fatalf("clean overlay=%#v", manifest)
	}
	if decision := decideGitOverlay(cfg, repo, SSHTarget{TargetOS: targetLinux}, manifest, coherence, false, false, false); !decision.Enabled {
		t.Fatalf("clean exact HEAD should use overlay: %#v", decision)
	}
}

func TestGitOverlayDecisionFallsBackWithoutChangingLegacyDefault(t *testing.T) {
	root, repo, cfg, coherence := newGitOverlayTestRepo(t)
	manifest, err := syncManifest(root, configuredExcludes(cfg))
	if err != nil {
		t.Fatal(err)
	}
	legacy := cfg
	legacy.Sync.GitOverlay = false
	if decision := decideGitOverlay(legacy, repo, SSHTarget{TargetOS: targetLinux}, manifest, coherence, false, false, false); decision.Requested || decision.Enabled || decision.Reason != "" {
		t.Fatalf("legacy decision=%#v", decision)
	}

	tests := []struct {
		name   string
		mutate func(*Config, *Repo, *gitCoherencePlan)
		target SSHTarget
		reason string
	}{
		{name: "git seed disabled", mutate: func(cfg *Config, _ *Repo, _ *gitCoherencePlan) { cfg.Sync.GitSeed = false }, target: SSHTarget{TargetOS: targetLinux}, reason: "git_seed_disabled"},
		{name: "delete disabled", mutate: func(cfg *Config, _ *Repo, _ *gitCoherencePlan) { cfg.Sync.Delete = false }, target: SSHTarget{TargetOS: targetLinux}, reason: "delete_disabled"},
		{name: "include whitelist", mutate: func(cfg *Config, _ *Repo, _ *gitCoherencePlan) { cfg.Sync.Includes = []string{"src"} }, target: SSHTarget{TargetOS: targetLinux}, reason: "include_whitelist"},
		{name: "credential origin", mutate: func(_ *Config, repo *Repo, plan *gitCoherencePlan) {
			repo.RemoteURL = fmt.Sprintf("https://runner%c%s%cexample.test/repo.git", ':', "not-forwarded", '@')
			*plan = gitCoherencePlan{}
		}, target: SSHTarget{TargetOS: targetLinux}, reason: "credential_origin"},
		{name: "unsupported target", mutate: func(_ *Config, _ *Repo, _ *gitCoherencePlan) {}, target: SSHTarget{TargetOS: targetMacOS}, reason: "unsupported_target"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testCfg, testRepo, testPlan := cfg, repo, coherence
			test.mutate(&testCfg, &testRepo, &testPlan)
			decision := decideGitOverlay(testCfg, testRepo, test.target, manifest, testPlan, test.reason == "credential_origin", false, false)
			if !decision.Requested || decision.Enabled || decision.Reason != test.reason {
				t.Fatalf("decision=%#v want fallback %q", decision, test.reason)
			}
		})
	}

	runGit(t, root, "sparse-checkout", "set", "--no-cone", "/*")
	if decision := decideGitOverlay(cfg, repo, SSHTarget{TargetOS: targetLinux}, manifest, coherence, false, false, false); decision.Enabled || decision.Reason != "sparse_checkout" {
		t.Fatalf("sparse decision=%#v", decision)
	}
}

func TestGitOverlayDecisionRejectsCheckoutTransforms(t *testing.T) {
	configs := []struct {
		name   string
		key    string
		value  string
		reason string
	}{
		{name: "autocrlf", key: "core.autocrlf", value: "true", reason: "core_autocrlf"},
		{name: "autocrlf input", key: "core.autocrlf", value: "input", reason: "core_autocrlf"},
		{name: "eol", key: "core.eol", value: "crlf", reason: "core_eol"},
		{name: "native eol", key: "core.eol", value: "native", reason: "core_eol"},
		{name: "symlinks", key: "core.symlinks", value: "false", reason: "core_symlinks"},
		{name: "filemode", key: "core.filemode", value: "false", reason: "core_filemode"},
	}
	for _, test := range configs {
		t.Run("config "+test.name, func(t *testing.T) {
			root, repo, cfg, coherence := newGitOverlayTestRepo(t)
			runGit(t, root, "config", test.key, test.value)
			manifest, err := syncManifest(root, configuredExcludes(cfg))
			if err != nil {
				t.Fatal(err)
			}
			decision := decideGitOverlay(cfg, repo, SSHTarget{TargetOS: targetLinux}, manifest, coherence, false, false, false)
			if decision.Enabled || decision.Reason != test.reason {
				t.Fatalf("decision=%#v", decision)
			}
		})
	}

	t.Run("normalized config", func(t *testing.T) {
		root, repo, cfg, coherence := newGitOverlayTestRepo(t)
		runGit(t, root, "config", "core.autocrlf", "false")
		runGit(t, root, "config", "core.eol", "lf")
		runGit(t, root, "config", "core.symlinks", "true")
		runGit(t, root, "config", "core.filemode", "true")
		manifest, err := syncManifest(root, configuredExcludes(cfg))
		if err != nil {
			t.Fatal(err)
		}
		if decision := decideGitOverlay(cfg, repo, SSHTarget{TargetOS: targetLinux}, manifest, coherence, false, false, false); !decision.Enabled || decision.Reason != "" {
			t.Fatalf("decision=%#v", decision)
		}
	})

	t.Run("committed attributes", func(t *testing.T) {
		root, repo, cfg, _ := newGitOverlayTestRepo(t)
		writeFile(t, filepath.Join(root, ".gitattributes"), "*.txt text=auto\n")
		runGit(t, root, "add", ".gitattributes")
		runGit(t, root, "commit", "-m", "text attributes")
		runGit(t, root, "push", "origin", "main")
		repo.Head = gitOutput(root, "rev-parse", "HEAD")
		coherence, blocked := syncGitCoherencePlan(cfg, repo)
		if blocked {
			t.Fatalf("credential-blocked coherence=%#v", coherence)
		}
		manifest, err := syncManifest(root, configuredExcludes(cfg))
		if err != nil {
			t.Fatal(err)
		}
		decision := decideGitOverlay(cfg, repo, SSHTarget{TargetOS: targetLinux}, manifest, coherence, false, false, false)
		if decision.Enabled || decision.Reason != "git_attribute_text" {
			t.Fatalf("decision=%#v", decision)
		}
	})

	attributes := []struct {
		name   string
		rule   string
		reason string
	}{
		{name: "text", rule: "* text=auto\n", reason: "git_attribute_text"},
		{name: "legacy crlf", rule: "* crlf\n", reason: "git_attribute_crlf"},
		{name: "eol", rule: "* eol=crlf\n", reason: "git_attribute_eol"},
		{name: "working tree encoding", rule: "* working-tree-encoding=UTF-16\n", reason: "git_attribute_working_tree_encoding"},
		{name: "filter", rule: "* filter=fixture\n", reason: "git_attribute_filter"},
		{name: "smudge", rule: "* smudge=fixture\n", reason: "git_attribute_smudge"},
		{name: "clean", rule: "* clean=fixture\n", reason: "git_attribute_clean"},
		{name: "ident", rule: "* ident\n", reason: "git_attribute_ident"},
	}
	for _, test := range attributes {
		t.Run("dirty attributes "+test.name, func(t *testing.T) {
			root, repo, cfg, coherence := newGitOverlayTestRepo(t)
			writeFile(t, filepath.Join(root, ".gitattributes"), test.rule)
			manifest, err := syncManifest(root, configuredExcludes(cfg))
			if err != nil {
				t.Fatal(err)
			}
			decision := decideGitOverlay(cfg, repo, SSHTarget{TargetOS: targetLinux}, manifest, coherence, false, false, false)
			if decision.Enabled || decision.Reason != test.reason {
				t.Fatalf("decision=%#v", decision)
			}
		})
	}
}

func TestSameSyncManifestIncludesProtectedTrackedExcludes(t *testing.T) {
	base := SyncManifest{
		Files:                    []string{"coverage/keep.txt"},
		ProtectedTrackedExcludes: []SyncProtectedTrackedExclude{{Path: "coverage/keep.txt", Pattern: "coverage"}},
	}
	changed := base
	changed.ProtectedTrackedExcludes = []SyncProtectedTrackedExclude{{Path: "coverage/keep.txt", Pattern: "dist"}}
	if sameSyncManifest(base, changed) {
		t.Fatal("protected tracked exclude change was ignored")
	}
}

func TestValidateGitOverlayPathRejectsSymlinkAncestors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink path safety")
	}
	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "child.txt"), "outside\n")
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := validateGitOverlayPath(root, "linked/child.txt"); err == nil || !strings.Contains(err.Error(), "symlink_ancestor") {
		t.Fatalf("symlink ancestor error=%v", err)
	}
	if err := os.Symlink("target.txt", filepath.Join(root, "leaf")); err != nil {
		t.Fatal(err)
	}
	if err := validateGitOverlayPath(root, "leaf"); err != nil {
		t.Fatalf("leaf symlink rejected: %v", err)
	}
}

func TestSyncFingerprintHashesSymlinkIdentityWithoutFollowingTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink fingerprint safety")
	}
	root, repo, cfg, coherence := newGitOverlayTestRepo(t)
	outside := filepath.Join(t.TempDir(), "outside.txt")
	writeFile(t, outside, "first external value\n")
	if err := os.Remove(filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	manifest, err := syncManifest(root, configuredExcludes(cfg))
	if err != nil {
		t.Fatal(err)
	}
	first, err := syncFingerprintForManifest(repo, cfg, manifest, configuredExcludes(cfg), coherence)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, outside, "changed external value\n")
	second, err := syncFingerprintForManifest(repo, cfg, manifest, configuredExcludes(cfg), coherence)
	if err != nil {
		t.Fatal(err)
	}
	if first == "" || first != second {
		t.Fatalf("fingerprint followed symlink target: first=%q second=%q", first, second)
	}
	legacy := cfg
	legacy.Sync.GitOverlay = false
	legacyFingerprint, err := syncFingerprintForManifest(repo, legacy, manifest, configuredExcludes(legacy), coherence)
	if err != nil {
		t.Fatal(err)
	}
	if legacyFingerprint == first {
		t.Fatal("legacy and overlay sync modes shared a fingerprint")
	}
}

func TestRemotePrepareGitOverlayResetsExactRequestedTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX Git overlay integration")
	}
	root, repo, _, coherence := newGitOverlayTestRepo(t)
	workdir := filepath.Join(t.TempDir(), "workdir")
	clone := exec.Command("git", "clone", repo.RemoteURL, workdir)
	if out, err := clone.CombinedOutput(); err != nil {
		t.Fatalf("clone remote workspace: %v\n%s", err, out)
	}
	writeFile(t, filepath.Join(workdir, "staged.txt"), "remote dirt\n")
	writeFile(t, filepath.Join(workdir, "remove-me.txt"), "remote untracked\n")
	writeFile(t, filepath.Join(workdir, ".env"), "stale ignored value\n")
	writeFile(t, filepath.Join(workdir, "node_modules", "cache.txt"), "keep node modules\n")
	writeFile(t, filepath.Join(workdir, ".pnpm-store", "cache.txt"), "keep pnpm\n")
	writeFile(t, filepath.Join(workdir, ".yarn", "cache", "package.zip"), "keep yarn cache\n")
	writeFile(t, filepath.Join(workdir, ".yarn", "unplugged", "package", "index.js"), "keep yarn unplugged\n")
	writeFile(t, filepath.Join(workdir, ".git", "info", "exclude"), ".env\nnode_modules/\n.pnpm-store/\n.yarn/cache/\n.yarn/unplugged/\n")

	command := remotePrepareGitOverlay(workdir, coherence)
	for _, forbidden := range []string{"--tags", "refs/heads/*", "secret"} {
		if strings.Contains(command, forbidden) {
			t.Fatalf("overlay command contains forbidden %q: %s", forbidden, command)
		}
	}
	if out, err := exec.Command("bash", "-lc", command).CombinedOutput(); err != nil {
		t.Fatalf("prepare overlay: %v\n%s", err, out)
	}
	if got := gitOutput(workdir, "rev-parse", "HEAD"); got != repo.Head {
		t.Fatalf("remote HEAD=%q want %q", got, repo.Head)
	}
	if got := gitOutput(workdir, "write-tree"); got != coherence.Tree {
		t.Fatalf("remote tree=%q want %q", got, coherence.Tree)
	}
	if status := gitOutput(workdir, "status", "--porcelain", "--untracked-files=normal"); status != "" {
		t.Fatalf("remote workspace remained dirty: %q", status)
	}
	if _, err := os.Stat(filepath.Join(workdir, "remove-me.txt")); !os.IsNotExist(err) {
		t.Fatalf("untracked path survived strict reset: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workdir, ".env")); !os.IsNotExist(err) {
		t.Fatalf("ignored path survived strict reset: %v", err)
	}
	for _, cachePath := range []string{
		"node_modules/cache.txt",
		".pnpm-store/cache.txt",
		".yarn/cache/package.zip",
		".yarn/unplugged/package/index.js",
	} {
		if _, err := os.Stat(filepath.Join(workdir, filepath.FromSlash(cachePath))); err != nil {
			t.Fatalf("ignored warm cache %q was removed: %v", cachePath, err)
		}
	}
	if got := gitOutput(root, "status", "--porcelain"); got != "" {
		t.Fatalf("source unexpectedly changed: %q", got)
	}
}

func TestRemotePrepareGitOverlayIsHermeticBeforeWorkspaceInspection(t *testing.T) {
	command := remotePrepareGitOverlay("/tmp/crabbox-overlay/workdir", gitCoherencePlan{
		RemoteURL: "/tmp/crabbox-overlay/origin.git",
		Target:    strings.Repeat("a", 40),
		Tree:      strings.Repeat("b", 40),
		Branch:    "main",
	})
	for _, required := range []string{
		"/usr/bin/env -i PATH=/usr/bin:/bin",
		"/bin/bash --noprofile --norc",
		"/usr/bin/git",
		"-c credential.helper=",
		"-c protocol.allow=never",
		"-c protocol.ext.allow=never",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_ASKPASS=/bin/false",
	} {
		if !strings.Contains(command, required) {
			t.Fatalf("hermetic overlay command missing %q", required)
		}
	}
	for _, forbidden := range []string{"bash -lc", "command git", `PATH="$PATH"`} {
		if strings.Contains(command, forbidden) {
			t.Fatalf("hermetic overlay command contains %q", forbidden)
		}
	}
	if wrapper, workspace := strings.Index(command, "git() {"), strings.Index(command, "usable_git_workspace"); wrapper < 0 || workspace < 0 || wrapper > workspace {
		t.Fatalf("trusted Git wrapper is not established before workspace inspection")
	}
}

func TestRemotePrepareGitOverlayIgnoresAmbientGitTransformsAndHooks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX Git overlay integration")
	}
	tests := []struct {
		name  string
		setup func(*testing.T, string, string, string, string) []string
	}{
		{
			name: "repository local",
			setup: func(t *testing.T, workdir, attributesFile, hooks, filter string) []string {
				t.Helper()
				attributes, err := os.ReadFile(attributesFile)
				if err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(workdir, ".git", "info", "attributes"), string(attributes))
				hook, err := os.ReadFile(filepath.Join(hooks, "post-checkout"))
				if err != nil {
					t.Fatal(err)
				}
				writeExecutable(t, filepath.Join(workdir, ".git", "hooks", "post-checkout"), string(hook))
				runGit(t, workdir, "config", "filter.attack.smudge", filter)
				runGit(t, workdir, "config", "filter.attack.required", "false")
				runGit(t, workdir, "config", "credential.helper", "preserved-helper")
				return nil
			},
		},
		{
			name: "runner global",
			setup: func(t *testing.T, _ string, attributes, hooks, filter string) []string {
				t.Helper()
				config := filepath.Join(t.TempDir(), "gitconfig")
				configureGitAttack(t, config, attributes, hooks, filter)
				return []string{
					"GIT_CONFIG_GLOBAL=" + config,
					"GIT_CONFIG_SYSTEM=/dev/null",
					"GIT_CONFIG_NOSYSTEM=1",
				}
			},
		},
		{
			name: "runner system",
			setup: func(t *testing.T, _ string, attributes, hooks, filter string) []string {
				t.Helper()
				config := filepath.Join(t.TempDir(), "gitconfig")
				configureGitAttack(t, config, attributes, hooks, filter)
				return []string{
					"GIT_CONFIG_GLOBAL=/dev/null",
					"GIT_CONFIG_SYSTEM=" + config,
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, repo, _, coherence := newGitOverlayTestRepo(t)
			workdir := filepath.Join(t.TempDir(), "workdir")
			clone := exec.Command("git", "clone", repo.RemoteURL, workdir)
			if out, err := clone.CombinedOutput(); err != nil {
				t.Fatalf("clone remote workspace: %v\n%s", err, out)
			}

			attackRoot := t.TempDir()
			filterMarker := filepath.Join(attackRoot, "filter-ran")
			hookMarker := filepath.Join(attackRoot, "hook-ran")
			filter := filepath.Join(attackRoot, "filter.sh")
			writeExecutable(t, filter, "#!/bin/sh\nprintf attacked >"+shellQuote(filterMarker)+"\nprintf 'filtered\\n'\n")
			hooks := filepath.Join(attackRoot, "hooks")
			if err := os.MkdirAll(hooks, 0o755); err != nil {
				t.Fatal(err)
			}
			writeExecutable(t, filepath.Join(hooks, "post-checkout"), "#!/bin/sh\nprintf attacked >"+shellQuote(hookMarker)+"\n")
			attributesPath := filepath.Join(attackRoot, "attributes")
			attributes := "clean.txt filter=attack\n"
			writeFile(t, attributesPath, attributes)
			reuseMarker := filepath.Join(workdir, ".git", "reuse-marker")
			writeFile(t, reuseMarker, "reused\n")

			extraEnv := test.setup(t, workdir, attributesPath, hooks, filter)
			attackEnv := childEnvironmentWithout(
				os.Environ(),
				"GIT_CONFIG_GLOBAL",
				"GIT_CONFIG_SYSTEM",
				"GIT_CONFIG_NOSYSTEM",
				"GIT_ATTR_NOSYSTEM",
			)
			attackEnv = append(attackEnv, extraEnv...)
			attributeProbe := exec.Command("git", "-C", workdir, "check-attr", "filter", "--", "clean.txt")
			attributeProbe.Env = attackEnv
			if out, err := attributeProbe.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "clean.txt: filter: attack" {
				t.Fatalf("activate ambient Git attack: err=%v out=%q", err, out)
			}
			command := exec.Command("bash", "-lc", remotePrepareGitOverlay(workdir, coherence))
			command.Env = attackEnv
			if out, err := command.CombinedOutput(); err != nil {
				t.Fatalf("prepare overlay: %v\n%s", err, out)
			}
			content, err := os.ReadFile(filepath.Join(workdir, "clean.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if got := string(content); got != "clean.txt\n" {
				t.Fatalf("checkout content=%q want repository bytes", got)
			}
			for _, marker := range []string{filterMarker, hookMarker} {
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatalf("ambient Git attack executed via %q: %v", marker, err)
				}
			}
			if got := gitOutput(workdir, "rev-parse", "HEAD"); got != repo.Head {
				t.Fatalf("remote HEAD=%q want %q", got, repo.Head)
			}
			if got := gitOutput(workdir, "write-tree"); got != coherence.Tree {
				t.Fatalf("remote tree=%q want %q", got, coherence.Tree)
			}
			if _, err := os.Stat(reuseMarker); err != nil {
				t.Fatalf("remote workspace was replaced instead of reused: %v", err)
			}
			if status := gitOutput(workdir, "status", "--porcelain", "--untracked-files=normal"); status != "" {
				t.Fatalf("remote workspace remained dirty: %q", status)
			}
			if test.name == "repository local" {
				if _, err := os.Stat(filepath.Join(workdir, ".git", "info", "attributes")); !os.IsNotExist(err) {
					t.Fatalf("repository info attributes survived: %v", err)
				}
				if got := gitOutput(workdir, "config", "--local", "--get", "credential.helper"); got != "preserved-helper" {
					t.Fatalf("credential helper=%q want preserved-helper", got)
				}
				filterConfig := exec.Command("git", "config", "--local", "--get-regexp", "^filter\\.")
				filterConfig.Dir = workdir
				if out, err := filterConfig.CombinedOutput(); err == nil || len(out) != 0 {
					t.Fatalf("local filter config survived: err=%v out=%q", err, out)
				}
			}
		})
	}
}

func TestRemotePrepareGitOverlayDisablesReferenceTransactionHooks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX Git overlay integration")
	}
	tests := []struct {
		name       string
		reuse      bool
		globalHook bool
		template   bool
		reject     bool
	}{
		{name: "reused repository hook", reuse: true},
		{name: "global hooks path and ext transport", globalHook: true, reject: true},
		{name: "clone template hook", template: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, repo, _, coherence := newGitOverlayTestRepo(t)
			workdir := filepath.Join(t.TempDir(), "workdir")
			if test.reuse {
				clone := exec.Command("git", "clone", repo.RemoteURL, workdir)
				if out, err := clone.CombinedOutput(); err != nil {
					t.Fatalf("clone remote workspace: %v\n%s", err, out)
				}
			}

			attackRoot := t.TempDir()
			marker := filepath.Join(attackRoot, "reference-transaction-ran")
			hook := "#!/bin/sh\nprintf fired >>" + shellQuote(marker) + "\n"
			var commandEnv []string
			switch {
			case test.reuse:
				writeExecutable(t, filepath.Join(workdir, ".git", "hooks", "reference-transaction"), hook)
				writeFile(t, filepath.Join(workdir, ".git", "reuse-marker"), "reused\n")
			case test.globalHook:
				hooks := filepath.Join(attackRoot, "hooks")
				if err := os.MkdirAll(hooks, 0o755); err != nil {
					t.Fatal(err)
				}
				writeExecutable(t, filepath.Join(hooks, "reference-transaction"), hook)
				config := filepath.Join(attackRoot, "gitconfig")
				configureGitFile(t, config, "core.hooksPath", hooks)
				transportOrigin := "ext::git-upload-pack " + repo.RemoteURL
				configureGitFile(t, config, "protocol.ext.allow", "always")
				coherence.RemoteURL = transportOrigin
				commandEnv = append(commandEnv, "GIT_CONFIG_GLOBAL="+config)
			case test.template:
				template := filepath.Join(attackRoot, "template")
				if err := os.MkdirAll(filepath.Join(template, "hooks"), 0o755); err != nil {
					t.Fatal(err)
				}
				writeExecutable(t, filepath.Join(template, "hooks", "reference-transaction"), hook)
				config := filepath.Join(attackRoot, "gitconfig")
				configureGitFile(t, config, "init.templateDir", template)
				commandEnv = append(commandEnv, "GIT_CONFIG_GLOBAL="+config)
			}
			commandEnv = append(commandEnv, "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1")

			command := exec.Command("/bin/bash", "--noprofile", "--norc", "-c", remotePrepareGitOverlay(workdir, coherence))
			command.Env = childEnvironmentWithout(
				os.Environ(),
				"GIT_CONFIG_GLOBAL",
				"GIT_CONFIG_SYSTEM",
				"GIT_CONFIG_NOSYSTEM",
			)
			command.Env = append(command.Env, commandEnv...)
			out, err := command.CombinedOutput()
			if test.reject {
				if err == nil || !strings.Contains(string(out), "transport 'ext' not allowed") {
					t.Fatalf("dangerous transport was not rejected: err=%v out=%q", err, out)
				}
			} else if err != nil {
				t.Fatalf("prepare overlay: %v\n%s", err, out)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("reference-transaction hook executed: %v", err)
			}
			if test.reject {
				return
			}
			if got := gitOutput(workdir, "rev-parse", "HEAD"); got != repo.Head {
				t.Fatalf("remote HEAD=%q want %q", got, repo.Head)
			}
			if test.reuse {
				if _, err := os.Stat(filepath.Join(workdir, ".git", "reuse-marker")); err != nil {
					t.Fatalf("remote workspace was replaced instead of fetched in place: %v", err)
				}
			}
			if test.template {
				if _, err := os.Stat(filepath.Join(workdir, ".git", "hooks", "reference-transaction")); !os.IsNotExist(err) {
					t.Fatalf("ambient clone template hook was installed: %v", err)
				}
			}
		})
	}
}

func TestRemotePrepareGitOverlayIgnoresHostilePathAndGlobalRewrites(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX Git overlay integration")
	}
	_, repo, _, coherence := newGitOverlayTestRepo(t)
	workdir := filepath.Join(t.TempDir(), "workdir")
	attackRoot := t.TempDir()
	pathMarker := filepath.Join(attackRoot, "path-git-ran")
	rewriteMarker := filepath.Join(attackRoot, "rewrite-ran")
	hookMarker := filepath.Join(attackRoot, "hook-ran")
	attackBin := filepath.Join(attackRoot, "bin")
	if err := os.MkdirAll(attackBin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(attackBin, "git"), "#!/bin/sh\nprintf path >"+shellQuote(pathMarker)+"\nexit 97\n")
	rewrite := filepath.Join(attackRoot, "rewrite.sh")
	writeExecutable(t, rewrite, "#!/bin/sh\nprintf rewrite >"+shellQuote(rewriteMarker)+"\nexit 98\n")
	hooks := filepath.Join(attackRoot, "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(hooks, "reference-transaction"), "#!/bin/sh\nprintf hook >"+shellQuote(hookMarker)+"\n")
	config := filepath.Join(attackRoot, "gitconfig")
	configureGitFile(t, config, "core.hooksPath", hooks)
	configureGitFile(t, config, "protocol.ext.allow", "always")
	configureGitFile(t, config, "url.ext::"+rewrite+".insteadof", repo.RemoteURL)

	activateRewrite := exec.Command("/usr/bin/git", "ls-remote", repo.RemoteURL)
	activateRewrite.Env = append(childEnvironmentWithout(os.Environ(), "GIT_CONFIG_GLOBAL"), "GIT_CONFIG_GLOBAL="+config)
	_, _ = activateRewrite.CombinedOutput()
	if _, err := os.Stat(rewriteMarker); err != nil {
		t.Fatalf("hostile global rewrite fixture did not activate: %v", err)
	}
	if err := os.Remove(rewriteMarker); err != nil {
		t.Fatal(err)
	}

	command := exec.Command("/bin/bash", "--noprofile", "--norc", "-c", remotePrepareGitOverlay(workdir, coherence))
	command.Env = append(
		childEnvironmentWithout(os.Environ(), "PATH", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM"),
		"PATH="+attackBin+":/usr/bin:/bin",
		"GIT_CONFIG_GLOBAL="+config,
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
	)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("prepare overlay: %v\n%s", err, out)
	}
	for _, marker := range []string{pathMarker, rewriteMarker, hookMarker} {
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("ambient Git attack executed via %q: %v", marker, err)
		}
	}
	if got := gitOutput(workdir, "rev-parse", "HEAD"); got != repo.Head {
		t.Fatalf("remote HEAD=%q want %q", got, repo.Head)
	}
}

func TestRemotePrepareGitOverlayReplacesLocalURLRewriteBeforeFetch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX Git overlay integration")
	}
	_, repo, _, coherence := newGitOverlayTestRepo(t)
	workdir := filepath.Join(t.TempDir(), "workdir")
	if out, err := exec.Command("/usr/bin/git", "clone", repo.RemoteURL, workdir).CombinedOutput(); err != nil {
		t.Fatalf("clone remote workspace: %v\n%s", err, out)
	}
	attackRoot := t.TempDir()
	marker := filepath.Join(attackRoot, "rewrite-ran")
	rewrite := filepath.Join(attackRoot, "rewrite.sh")
	writeExecutable(t, rewrite, "#!/bin/sh\nprintf rewrite >"+shellQuote(marker)+"\nexit 98\n")
	runGit(t, workdir, "config", "protocol.ext.allow", "always")
	runGit(t, workdir, "config", "url.ext::"+rewrite+".insteadof", repo.RemoteURL)
	reuseMarker := filepath.Join(workdir, ".git", "reuse-marker")
	writeFile(t, reuseMarker, "unsafe workspace\n")

	activateRewrite := exec.Command("/usr/bin/git", "-C", workdir, "ls-remote", repo.RemoteURL)
	_, _ = activateRewrite.CombinedOutput()
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("hostile local rewrite fixture did not activate: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}

	command := exec.Command("/bin/bash", "--noprofile", "--norc", "-c", remotePrepareGitOverlay(workdir, coherence))
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("prepare overlay: %v\n%s", err, out)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("local URL rewrite executed: %v", err)
	}
	if _, err := os.Stat(reuseMarker); !os.IsNotExist(err) {
		t.Fatalf("unsafe workspace was reused: %v", err)
	}
	if got := gitOutput(workdir, "rev-parse", "HEAD"); got != repo.Head {
		t.Fatalf("remote HEAD=%q want %q", got, repo.Head)
	}
}

func TestRemotePrepareGitOverlayDoesNotInvokeAmbientCredentialHelper(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX Git overlay integration")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
	}))
	defer server.Close()

	attackRoot := t.TempDir()
	marker := filepath.Join(attackRoot, "credential-helper-ran")
	helper := filepath.Join(attackRoot, "credential-helper.sh")
	writeExecutable(t, helper, "#!/bin/sh\nprintf helper >"+shellQuote(marker)+"\nprintf 'username=test\\npassword=test\\n'\n")
	config := filepath.Join(attackRoot, "gitconfig")
	configureGitFile(t, config, "credential.helper", helper)
	remoteURL := server.URL + "/repo.git"

	activateHelper := exec.Command("/usr/bin/git", "ls-remote", remoteURL)
	activateHelper.Env = append(
		childEnvironmentWithout(os.Environ(), "GIT_CONFIG_GLOBAL", "GIT_TERMINAL_PROMPT"),
		"GIT_CONFIG_GLOBAL="+config,
		"GIT_TERMINAL_PROMPT=0",
	)
	_, _ = activateHelper.CombinedOutput()
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("credential helper fixture did not activate: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}

	plan := gitCoherencePlan{
		RemoteURL: remoteURL,
		Target:    strings.Repeat("a", 40),
		Tree:      strings.Repeat("b", 40),
		Branch:    "main",
	}
	command := exec.Command("/bin/bash", "--noprofile", "--norc", "-c", remotePrepareGitOverlay(filepath.Join(t.TempDir(), "workdir"), plan))
	command.Env = append(
		childEnvironmentWithout(os.Environ(), "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM"),
		"GIT_CONFIG_GLOBAL="+config,
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
	)
	if out, err := command.CombinedOutput(); err == nil {
		t.Fatalf("unauthenticated origin unexpectedly succeeded: %q", out)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("ambient credential helper executed: %v", err)
	}
}

func TestRemotePrepareGitOverlayFreshCloneFallbackClassification(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX Git overlay integration")
	}
	_, _, _, coherence := newGitOverlayTestRepo(t)
	tests := []struct {
		name   string
		mutate func(*gitCoherencePlan)
		reason string
	}{
		{name: "missing advertised branch", mutate: func(plan *gitCoherencePlan) {
			plan.Branch = "moved-away"
		}, reason: "advertised_branch_missing"},
		{name: "missing planned target", mutate: func(plan *gitCoherencePlan) {
			plan.Target = strings.Repeat("c", 40)
			plan.Tree = strings.Repeat("d", 40)
		}, reason: "target_missing"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := coherence
			test.mutate(&plan)
			command := remotePrepareGitOverlay(filepath.Join(t.TempDir(), "workdir"), plan)
			out, err := exec.Command("bash", "-lc", command).CombinedOutput()
			reason, fallback := gitOverlayFallbackResult(string(out), err)
			if !fallback || reason != test.reason {
				t.Fatalf("reason=%q fallback=%t err=%v out=%q", reason, fallback, err, out)
			}
		})
	}
}

func TestRemotePrepareGitOverlayTransportFailureIsFatal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX Git overlay integration")
	}
	plan := gitCoherencePlan{
		RemoteURL: filepath.Join(t.TempDir(), "missing-origin.git"),
		Target:    strings.Repeat("a", 40),
		Tree:      strings.Repeat("b", 40),
		Branch:    "main",
	}
	command := remotePrepareGitOverlay(filepath.Join(t.TempDir(), "workdir"), plan)
	out, err := exec.Command("bash", "-lc", command).CombinedOutput()
	if err == nil {
		t.Fatalf("missing origin unexpectedly succeeded: %q", out)
	}
	if reason, fallback := gitOverlayFallbackResult(string(out), err); fallback || reason != "" || exitCode(err) == gitOverlayFallbackExitCode {
		t.Fatalf("transport failure became fallback: reason=%q fallback=%t err=%v out=%q", reason, fallback, err, out)
	}
}

func TestGitOverlayFallbackOnlyAcceptsResetInvariantExit(t *testing.T) {
	fallbackCmd := exec.Command("bash", "-lc", "printf '%sreset_failed\\n' '"+gitOverlayFallbackMarker+"' >&2; exit 78")
	out, err := fallbackCmd.CombinedOutput()
	if reason, ok := gitOverlayFallbackResult(string(out), err); !ok || reason != "reset_failed" {
		t.Fatalf("fallback reason=%q ok=%t err=%v out=%q", reason, ok, err, out)
	}
	transportCmd := exec.Command("bash", "-lc", "exit 255")
	out, err = transportCmd.CombinedOutput()
	if reason, ok := gitOverlayFallbackResult(string(out), err); ok || reason != "" {
		t.Fatalf("transport failure was swallowed: reason=%q ok=%t err=%v", reason, ok, err)
	}
}

func TestRemotePrepareGitOverlayNeverSerializesCredentialOrigin(t *testing.T) {
	userinfoValue := "not-forwarded"
	command := remotePrepareGitOverlay("/work/repo", gitCoherencePlan{
		RemoteURL: fmt.Sprintf("https://runner%c%s%cexample.test/repo.git", ':', userinfoValue, '@'),
		Target:    strings.Repeat("a", 40),
		Tree:      strings.Repeat("b", 40),
		Branch:    "main",
	})
	if command != "true" || strings.Contains(command, userinfoValue) {
		t.Fatalf("credential-bearing plan serialized: %q", command)
	}
}

func TestGitOverlayRunnerTelemetryCarriesTransferAndFallbackMetadata(t *testing.T) {
	overlayReport := timingReportFromRun("aws", "cbx_overlay", "overlay", runTimings{
		started:                time.Unix(0, 0),
		endToEndStartedAt:      time.Unix(0, 0),
		sync:                   300 * time.Millisecond,
		workspaceMode:          "overlay",
		workspaceTransferCount: 3,
		workspaceTransferBytes: 512,
		syncSteps:              syncStepTimings{gitSeed: 100 * time.Millisecond},
	}, 300*time.Millisecond, 0)
	overlayReport = finalizeTimingReport(overlayReport)
	var overlayPhase RunnerPhase
	for _, phase := range overlayReport.RunnerPhases {
		if phase.Name == "workspace.overlay" {
			overlayPhase = phase
		}
	}
	if overlayPhase.TransferCount != 3 || overlayPhase.TransferBytes != 512 || overlayPhase.Fallback || overlayPhase.Reason != "" {
		t.Fatalf("overlay phase=%#v", overlayPhase)
	}

	fallbackReport := timingReportFromRun("aws", "cbx_fallback", "fallback", runTimings{
		started:                 time.Unix(0, 0),
		endToEndStartedAt:       time.Unix(0, 0),
		sync:                    200 * time.Millisecond,
		workspaceMode:           "sync",
		workspaceTransferCount:  20,
		workspaceTransferBytes:  4096,
		workspaceFallback:       true,
		workspaceFallbackReason: "sparse_checkout",
	}, 200*time.Millisecond, 0)
	fallbackReport = finalizeTimingReport(fallbackReport)
	var fallbackPhase RunnerPhase
	for _, phase := range fallbackReport.RunnerPhases {
		if phase.Name == "workspace.sync" {
			fallbackPhase = phase
		}
	}
	if fallbackPhase.TransferCount != 20 || fallbackPhase.TransferBytes != 4096 || !fallbackPhase.Fallback || fallbackPhase.Reason != "sparse_checkout" {
		t.Fatalf("fallback phase=%#v", fallbackPhase)
	}
	encoded, err := json.Marshal(fallbackPhase)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"fallback":true`) || !strings.Contains(string(encoded), `"reason":"sparse_checkout"`) {
		t.Fatalf("fallback telemetry JSON=%s", encoded)
	}
}

func newGitOverlayTestRepo(t *testing.T) (string, Repo, Config, gitCoherencePlan) {
	t.Helper()
	root := t.TempDir()
	runGit(t, root, "init")
	runGit(t, root, "config", "user.email", "test@example.com")
	runGit(t, root, "config", "user.name", "Test")
	runGit(t, root, "config", "core.filemode", "true")
	runGit(t, root, "branch", "-M", "main")
	for _, name := range []string{"clean.txt", "staged.txt", "unstaged.txt", "deleted.txt", "renamed.txt"} {
		writeFile(t, filepath.Join(root, name), name+"\n")
	}
	writeFile(t, filepath.Join(root, "mode.sh"), "#!/bin/sh\nexit 0\n")
	if runtime.GOOS != "windows" {
		if err := os.Symlink("clean.txt", filepath.Join(root, "link")); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-m", "base")
	origin := filepath.Join(t.TempDir(), "origin.git")
	if out, err := exec.Command("git", "clone", "--bare", root, origin).CombinedOutput(); err != nil {
		t.Fatalf("create bare origin: %v\n%s", err, out)
	}
	runGit(t, root, "remote", "add", "origin", origin)
	runGit(t, root, "fetch", "origin")
	runGit(t, root, "branch", "--set-upstream-to=origin/main", "main")
	repo := Repo{
		Root:      root,
		Name:      "example",
		RemoteURL: origin,
		Head:      gitOutput(root, "rev-parse", "HEAD"),
		BaseRef:   "main",
	}
	cfg := baseConfig()
	cfg.Sync.GitOverlay = true
	coherence, blocked := syncGitCoherencePlan(cfg, repo)
	if blocked || !coherence.enabled() {
		t.Fatalf("coherence=%#v blocked=%t", coherence, blocked)
	}
	return root, repo, cfg, coherence
}

func configureGitAttack(t *testing.T, config, attributes, hooks, filter string) {
	t.Helper()
	for _, setting := range []struct {
		key   string
		value string
	}{
		{key: "core.attributesFile", value: attributes},
		{key: "core.hooksPath", value: hooks},
		{key: "filter.attack.smudge", value: filter},
		{key: "filter.attack.required", value: "false"},
	} {
		configureGitFile(t, config, setting.key, setting.value)
	}
}

func configureGitFile(t *testing.T, config, key, value string) {
	t.Helper()
	command := exec.Command("git", "config", "--file", config, key, value)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("configure Git file %s: %v\n%s", key, err, out)
	}
}
