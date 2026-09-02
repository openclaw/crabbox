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
	"sync"
	"sync/atomic"
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

func newFailingGitFetchHTTPServer(t *testing.T, origin string) (string, *atomic.Bool) {
	t.Helper()
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Fatal(err)
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	var failedFetch atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		var requestBody bytes.Buffer
		if _, err := requestBody.ReadFrom(request.Body); err != nil {
			http.Error(response, err.Error(), http.StatusBadRequest)
			return
		}
		if request.Method == http.MethodPost && bytes.Contains(requestBody.Bytes(), []byte("command=fetch")) {
			failedFetch.Store(true)
			http.Error(response, "fetch transport unavailable", http.StatusServiceUnavailable)
			return
		}
		command := exec.Command(backend)
		command.Stdin = bytes.NewReader(requestBody.Bytes())
		command.Env = append(os.Environ(),
			"GIT_PROJECT_ROOT="+filepath.Dir(origin),
			"GIT_HTTP_EXPORT_ALL=1",
			"PATH_INFO="+request.URL.Path,
			"REQUEST_METHOD="+request.Method,
			"QUERY_STRING="+request.URL.RawQuery,
			"CONTENT_TYPE="+request.Header.Get("Content-Type"),
			fmt.Sprintf("CONTENT_LENGTH=%d", requestBody.Len()),
			"HTTP_GIT_PROTOCOL="+request.Header.Get("Git-Protocol"),
			"REMOTE_ADDR=127.0.0.1",
			"SERVER_PROTOCOL=HTTP/1.1",
		)
		output, err := command.Output()
		if err != nil {
			http.Error(response, err.Error(), http.StatusInternalServerError)
			return
		}
		headerEnd := bytes.Index(output, []byte("\r\n\r\n"))
		separatorLength := 4
		if headerEnd < 0 {
			headerEnd = bytes.Index(output, []byte("\n\n"))
			separatorLength = 2
		}
		if headerEnd < 0 {
			http.Error(response, "invalid git HTTP backend response", http.StatusInternalServerError)
			return
		}
		status := http.StatusOK
		headers := strings.Split(strings.ReplaceAll(string(output[:headerEnd]), "\r\n", "\n"), "\n")
		for _, header := range headers {
			name, value, ok := strings.Cut(header, ":")
			if !ok {
				continue
			}
			value = strings.TrimSpace(value)
			if strings.EqualFold(name, "Status") {
				if _, err := fmt.Sscanf(value, "%d", &status); err != nil {
					http.Error(response, "invalid git HTTP backend status", http.StatusInternalServerError)
					return
				}
				continue
			}
			response.Header().Add(name, value)
		}
		response.WriteHeader(status)
		_, _ = response.Write(output[headerEnd+separatorLength:])
	}))
	t.Cleanup(server.Close)
	return server.URL + "/" + filepath.Base(origin), &failedFetch
}

func installGitOverlayCredentialCanary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "credential-called")
	helper := filepath.Join(dir, "credential-canary")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf called >>"+shellQuote(marker)+"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(dir, "gitconfig")
	runGit(t, dir, "config", "--file", global, "credential.helper", "!"+shellQuote(helper))
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("GIT_CONFIG_PARAMETERS", "")
	t.Setenv("GIT_ASKPASS", helper)
	t.Setenv("SSH_ASKPASS", helper)
	// Keep a regressed fixture bounded even when the test runner has a TTY.
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	return marker
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
		{name: "missing", target: SSHTarget{TargetOS: targetLinux}, mutate: func(_ *Config, repo *Repo, _ *gitCoherencePlan) { repo.RemoteURL = " " }, want: "missing_origin"},
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

func TestGitOverlayDecisionRejectsAssumeUnchangedIndex(t *testing.T) {
	fixture := newGitOverlayFixture(t)
	runGit(t, fixture.root, "update-index", "--assume-unchanged", "clean.txt")
	mustWriteTestFile(t, filepath.Join(fixture.root, "clean.txt"), "hidden local edit\n")
	manifest, _ := fixture.manifest(t)
	if slices.Contains(manifest.Changed, "clean.txt") {
		t.Fatalf("assume-unchanged edit unexpectedly entered changed paths: %v", manifest.Changed)
	}
	decision := decideGitOverlay(fixture.cfg, fixture.repo, SSHTarget{TargetOS: targetLinux}, manifest, fixture.plan, false, false, false)
	if decision.Enabled || decision.Reason != "assume_unchanged_index" {
		t.Fatalf("decision=%#v", decision)
	}
}

func TestGitOverlayOriginAcceptsOnlyAnonymousSupportedTransports(t *testing.T) {
	tests := []struct {
		origin      string
		disposition gitOriginDisposition
	}{
		{origin: "", disposition: gitOriginAbsent},
		{origin: "  ", disposition: gitOriginAbsent},
		{origin: "/srv/git/project.git", disposition: gitOriginRemoteAttemptSafe},
		{origin: "../relative/project.git", disposition: gitOriginRemoteAttemptSafe},
		{origin: "file:///srv/git/project.git", disposition: gitOriginRemoteAttemptSafe},
		{origin: "file://localhost/srv/git/project.git", disposition: gitOriginRemoteAttemptSafe},
		{origin: "http://example.test/project.git", disposition: gitOriginRemoteAttemptSafe},
		{origin: "https://example.test/project.git", disposition: gitOriginRemoteAttemptSafe},
		{origin: fmt.Sprintf("https://%s:%s@example.test/project.git", "user", "test-password"), disposition: gitOriginNonForwardable},
		{origin: "https://example.test/project.git?", disposition: gitOriginNonForwardable},
		{origin: "https://example.test/project.git?token=private", disposition: gitOriginNonForwardable},
		{origin: "https://example.test/project.git#fragment", disposition: gitOriginNonForwardable},
		{origin: "git@example.test:project.git", disposition: gitOriginNonForwardable},
		{origin: "ssh://git@example.test/project.git", disposition: gitOriginNonForwardable},
		{origin: "git://example.test/project.git", disposition: gitOriginNonForwardable},
		{origin: "ext::git-upload-pack /srv/git/project.git", disposition: gitOriginNonForwardable},
		{origin: "file://remote-host/srv/git/project.git", disposition: gitOriginNonForwardable},
		{origin: "https://[::1", disposition: gitOriginNonForwardable},
	}
	for _, test := range tests {
		t.Run(test.origin, func(t *testing.T) {
			if got := classifyGitOrigin(test.origin); got != test.disposition {
				t.Fatalf("disposition=%v want=%v", got, test.disposition)
			}
			if got := gitOverlayOriginTransportSupported(test.origin); got != (test.disposition == gitOriginRemoteAttemptSafe) {
				t.Fatalf("supported=%t disposition=%v", got, test.disposition)
			}
		})
	}
}

func TestGitOverlayBoundaryViolationsFailClosedWithoutRejectingLegacyFallback(t *testing.T) {
	for _, reason := range []string{
		"unsafe_remote_root", "symlink_remote_root", "symlink_remote_parent", "symlink_git_directory", "symlink_git_config", "symlink_git_objects",
		"symlink_git_info", "symlink_git_attributes", "symlink_git_exclude", "symlink_git_objects_info", "symlink_git_alternates",
		"unsafe_git_metadata", "unsafe_overlay_metadata", "unsafe_runtime_state",
	} {
		if !gitOverlayBoundaryViolation(reason) {
			t.Errorf("boundary violation %q remained eligible for legacy sync", reason)
		}
	}
	for _, reason := range []string{
		"unsafe_git_workspace", "origin_mismatch", "non_git_workspace", "filtered_history_unsupported", "git_attribute_filter", "checkout_failed", "unsafe_cache_root",
	} {
		if gitOverlayBoundaryViolation(reason) {
			t.Errorf("conservative overlay fallback %q incorrectly failed closed", reason)
		}
	}
}

func TestRemotePrepareGitOverlayRejectsHiddenIndexBeforeMutation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX Git overlay integration")
	}
	fixture := newGitOverlayFixture(t)
	repo, coherence := fixture.repo, fixture.plan
	for _, test := range []struct {
		name    string
		flag    string
		reason  string
		present bool
	}{
		{name: "skip stale", flag: "--skip-worktree", reason: "skip_worktree_index", present: true},
		{name: "skip deleted", flag: "--skip-worktree", reason: "skip_worktree_index"},
		{name: "assume stale", flag: "--assume-unchanged", reason: "assume_unchanged_index", present: true},
		{name: "assume deleted", flag: "--assume-unchanged", reason: "assume_unchanged_index"},
	} {
		t.Run(test.name, func(t *testing.T) {
			workdir := filepath.Join(t.TempDir(), "workdir")
			if out, err := exec.Command("git", "clone", "--quiet", repo.RemoteURL, workdir).CombinedOutput(); err != nil {
				t.Fatalf("clone remote workspace: %v\n%s", err, out)
			}
			runGit(t, workdir, "update-index", test.flag, "clean.txt")
			cleanPath := filepath.Join(workdir, "clean.txt")
			if test.present {
				mustWriteTestFile(t, cleanPath, "hidden remote edit\n")
			} else if err := os.Remove(cleanPath); err != nil {
				t.Fatal(err)
			}

			out, err := runOverlayCommand(t, remotePrepareGitOverlay(workdir, coherence), nil)
			reason, fallback, mutated := gitOverlayFallbackOutcome(string(out), err)
			if !fallback || mutated || reason != test.reason {
				t.Fatalf("reason=%q fallback=%t mutated=%t err=%v out=%q", reason, fallback, mutated, err, out)
			}
			if test.present {
				got, readErr := os.ReadFile(cleanPath)
				if readErr != nil || string(got) != "hidden remote edit\n" {
					t.Fatalf("hidden file changed before fallback: data=%q err=%v", got, readErr)
				}
			} else if _, statErr := os.Stat(cleanPath); !os.IsNotExist(statErr) {
				t.Fatalf("missing hidden file was recreated before fallback: %v", statErr)
			}
			if _, statErr := os.Stat(filepath.Join(workdir, ".git", "crabbox", "sync-manifest")); !os.IsNotExist(statErr) {
				t.Fatalf("overlay wrote its baseline before hidden-index fallback: %v", statErr)
			}
		})
	}
}

func TestRemotePrepareGitOverlayRejectsSymlinkedParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlink integration")
	}
	fixture := newGitOverlayFixture(t)
	root, coherence := fixture.root, fixture.plan
	outside := t.TempDir()
	mustWriteTestFile(t, filepath.Join(outside, "sentinel.txt"), "keep\n")
	parent := filepath.Join(root, "lease")
	if err := os.Symlink(outside, parent); err != nil {
		t.Fatal(err)
	}
	workdir := filepath.Join(parent, "repo")

	out, err := runOverlayCommand(t, remotePrepareGitOverlay(workdir, coherence), nil)
	reason, fallback := gitOverlayFallbackResult(string(out), err)
	if !fallback || reason != "symlink_remote_parent" {
		t.Fatalf("reason=%q fallback=%t err=%v out=%q", reason, fallback, err, out)
	}
	if got, readErr := os.ReadFile(filepath.Join(outside, "sentinel.txt")); readErr != nil || string(got) != "keep\n" {
		t.Fatalf("outside sentinel changed: data=%q err=%v", got, readErr)
	}
	if _, statErr := os.Stat(filepath.Join(outside, "repo")); !os.IsNotExist(statErr) {
		t.Fatalf("overlay created workspace through symlinked parent: %v", statErr)
	}
}

func TestRemotePrepareGitOverlayRejectsSymlinkedGitMetadata(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlink integration")
	}
	fixture := newGitOverlayFixture(t)
	repo, coherence := fixture.repo, fixture.plan
	for _, test := range []struct {
		name   string
		reason string
		path   string
		dir    bool
	}{
		{name: "info", reason: "symlink_git_info", path: ".git/info", dir: true},
		{name: "attributes", reason: "symlink_git_attributes", path: ".git/info/attributes"},
		{name: "exclude", reason: "symlink_git_exclude", path: ".git/info/exclude"},
		{name: "objects info", reason: "symlink_git_objects_info", path: ".git/objects/info", dir: true},
		{name: "alternates", reason: "symlink_git_alternates", path: ".git/objects/info/alternates"},
		{name: "deep refs", reason: "unsafe_git_metadata", path: ".git/refs/crabbox", dir: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			workdir := filepath.Join(t.TempDir(), "workdir")
			if out, err := exec.Command("git", "clone", "--quiet", repo.RemoteURL, workdir).CombinedOutput(); err != nil {
				t.Fatalf("clone remote workspace: %v\n%s", err, out)
			}
			outside := t.TempDir()
			sentinel := filepath.Join(outside, "sentinel")
			mustWriteTestFile(t, sentinel, "keep\n")
			link := filepath.Join(workdir, filepath.FromSlash(test.path))
			if err := os.RemoveAll(link); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				t.Fatal(err)
			}
			target := sentinel
			if test.dir {
				target = outside
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}

			out, err := runOverlayCommand(t, remotePrepareGitOverlay(workdir, coherence), nil)
			reason, fallback := gitOverlayFallbackResult(string(out), err)
			if !fallback || reason != test.reason {
				t.Fatalf("reason=%q fallback=%t err=%v out=%q", reason, fallback, err, out)
			}
			if got, readErr := os.ReadFile(sentinel); readErr != nil || string(got) != "keep\n" {
				t.Fatalf("outside sentinel changed: data=%q err=%v", got, readErr)
			}
		})
	}
}

func TestRemotePrepareGitOverlayRejectsHostileLocalGitCommandsBeforeFetch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX Git overlay integration")
	}
	fixture := newGitOverlayFixture(t)
	repo, coherence := fixture.repo, fixture.plan
	for _, key := range []string{"core.alternateRefsCommand", "core.fsmonitor"} {
		t.Run(key, func(t *testing.T) {
			workdir := filepath.Join(t.TempDir(), "workdir")
			if out, err := exec.Command("git", "clone", "--quiet", repo.RemoteURL, workdir).CombinedOutput(); err != nil {
				t.Fatalf("clone remote workspace: %v\n%s", err, out)
			}
			marker := filepath.Join(t.TempDir(), "hostile-command-ran")
			command := filepath.Join(t.TempDir(), "hostile.sh")
			if err := os.WriteFile(command, []byte("#!/bin/sh\nprintf attacked >"+shellQuote(marker)+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			runGit(t, workdir, "config", key, command)

			out, err := runOverlayCommand(t, remotePrepareGitOverlay(workdir, coherence), nil)
			reason, fallback, mutated := gitOverlayFallbackOutcome(string(out), err)
			if !fallback || mutated || reason != "unsafe_git_workspace" {
				t.Fatalf("reason=%q fallback=%t mutated=%t err=%v out=%q", reason, fallback, mutated, err, out)
			}
			if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
				t.Fatalf("hostile %s command executed: %v", key, statErr)
			}
		})
	}
}

func TestRemotePrepareGitOverlayUsesTrustedTemporaryPaths(t *testing.T) {
	command := remotePrepareGitOverlay("/tmp/crabbox-overlay/workdir", gitCoherencePlan{
		RemoteURL: "https://example.test/repo.git",
		Target:    strings.Repeat("a", 40),
		Tree:      strings.Repeat("b", 40),
		Branch:    "main",
	})
	for _, want := range []string{
		`baseline_tmp="$(/usr/bin/mktemp "$checkout_root/.git/crabbox/sync-manifest.overlay.XXXXXX")"`,
		`cache_lookup_path="$cache_path"`,
		`check-ignore --no-index -v -z --stdin`,
		`if ! /usr/bin/find -P "$metadata_path" -type l -print -quit > "$git_runtime_root/git-metadata-symlinks"; then return 1; fi`,
		`[ ! -s "$git_runtime_root/git-metadata-symlinks" ] || return 1`,
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("overlay command missing %q", want)
		}
	}
	if strings.Contains(command, "sync-manifest.overlay.$$") {
		t.Fatal("overlay baseline manifest remains predictable")
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
	for _, kind := range []string{"linked", "rewrite", "filter", "alternates", "attributes", "symlinked git", "symlinked config", "symlinked objects"} {
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
			case "symlinked config", "symlinked objects":
				name := "config"
				if kind == "symlinked objects" {
					name = "objects"
				}
				path := filepath.Join(workdir, ".git", name)
				external := filepath.Join(filepath.Dir(base), "external-"+name)
				if err := os.Rename(path, external); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(external, path); err != nil {
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
			wantReason := "unsafe_git_workspace"
			switch kind {
			case "symlinked git":
				wantReason = "symlink_git_directory"
			case "symlinked config":
				wantReason = "symlink_git_config"
			case "symlinked objects":
				wantReason = "symlink_git_objects"
			}
			if reason, ok := gitOverlayFallbackResult(string(output), prepareErr); !ok || reason != wantReason {
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

func TestRemoteGitOverlayTransportFailuresFallBack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX overlay integration")
	}
	fixture := newGitOverlayFixture(t)
	fetchOrigin, failedFetch := newFailingGitFetchHTTPServer(t, fixture.origin)
	tests := []struct {
		name      string
		plan      gitCoherencePlan
		wantFetch bool
	}{
		{
			name: "ls-remote",
			plan: gitCoherencePlan{
				RemoteURL: "http://127.0.0.1:1/missing.git",
				Target:    strings.Repeat("a", 40),
				Tree:      strings.Repeat("b", 40),
				Branch:    "main",
			},
		},
		{
			name: "fetch",
			plan: gitCoherencePlan{
				RemoteURL: fetchOrigin,
				Target:    fixture.plan.Target,
				Tree:      fixture.plan.Tree,
				Branch:    fixture.plan.Branch,
			},
			wantFetch: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workdir := filepath.Join(t.TempDir(), "workspace")
			output, err := runOverlayCommand(t, remotePrepareGitOverlay(workdir, test.plan), nil)
			if reason, fallback := gitOverlayFallbackResult(string(output), err); !fallback || reason != "origin_unavailable" {
				t.Fatalf("transport fallback=%t reason=%q err=%v output=%q", fallback, reason, err, output)
			}
			if test.wantFetch && !failedFetch.Load() {
				t.Fatal("fetch transport failure was not exercised")
			}
			assertNoGitOverlayResidue(t, workdir)
		})
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

func TestRemoteGitOverlayRecoveryRollsBackInterruptedCoherenceInstall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX overlay recovery integration")
	}
	fixture := newGitCoherenceFixture(t)
	workdir := fixture.workspace(t, fixture.a, true)
	plan := fixture.plan(t, fixture.b)
	mustWriteTestFile(t, filepath.Join(workdir, "tracked.txt"), "B\n")
	const token = "86753098675309867530986753098675"
	stageCoherenceFinalize(t, workdir, token)
	meta := filepath.Join(workdir, ".git", "crabbox")
	mustWriteTestFile(t, filepath.Join(meta, "sync-fingerprint"), "stale overlay fingerprint")
	beforeIndex := coherenceIndexBytes(t, workdir)
	headLock := filepath.Join(workdir, ".git", "HEAD.lock")
	mustWriteTestFile(t, headLock, "active competing HEAD writer\n")

	hostileRoot := t.TempDir()
	hostileHome := filepath.Join(hostileRoot, "home")
	hostileBin := filepath.Join(hostileRoot, "bin")
	marker := filepath.Join(hostileRoot, "hostile-executed")
	mustWriteTestFile(t, filepath.Join(hostileHome, ".bash_profile"), "printf profile >"+shellQuote(marker)+"\n")
	if err := os.MkdirAll(hostileBin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"git", "mv", "cp", "awk"} {
		if err := os.WriteFile(filepath.Join(hostileBin, name), []byte("#!/bin/sh\nprintf hostile >"+shellQuote(marker)+"\nexit 99\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	hook := filepath.Join(workdir, ".git", "hooks", "reference-transaction")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nprintf hook >"+shellQuote(marker)+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	finalize := remoteFinalizeSync(workdir, remoteSyncFinalizeOptions{
		Token:         token,
		Fingerprint:   "must-not-publish",
		Coherence:     plan,
		PlainManifest: true,
		BaseRef:       "main",
		BaseSHA:       fixture.c,
	})
	output, err := runOverlayCommand(t, finalize, nil, "HOME="+hostileHome, "PATH="+hostileBin)
	if err == nil {
		t.Fatalf("occupied HEAD lock unexpectedly allowed recovery finalization: %s", output)
	}
	requireGitOutput(t, workdir, fixture.a, "rev-parse", "HEAD")
	requireGitOutput(t, workdir, "refs/heads/workspace", "symbolic-ref", "-q", "HEAD")
	if afterIndex := coherenceIndexBytes(t, workdir); !bytes.Equal(afterIndex, beforeIndex) {
		t.Fatal("failed hermetic recovery did not restore the original Git index")
	}
	if _, err := os.Stat(filepath.Join(meta, "sync-finalize-complete-token")); !os.IsNotExist(err) {
		t.Fatalf("failed recovery published completion: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(meta, "sync-finalize-lock")); !os.IsNotExist(err) {
		t.Fatalf("failed recovery stranded its finalization lock: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("failed recovery executed an ambient helper, hook, or profile: %v", err)
	}
	if err := os.Remove(headLock); err != nil {
		t.Fatal(err)
	}
	if output, err := runOverlayCommand(t, finalize, nil, "HOME="+hostileHome, "PATH="+hostileBin); err != nil {
		t.Fatalf("retry hermetic recovery finalization: %v\n%s", err, output)
	}
	requireGitOutput(t, workdir, fixture.b, "rev-parse", "HEAD")
	requireGitOutput(t, workdir, plan.Tree, "write-tree")
	if base, err := os.ReadFile(filepath.Join(meta, "git-hydrate-base")); err != nil || string(base) != "main "+fixture.c+"\n" {
		t.Fatalf("recovered base hydration marker=%q err=%v", base, err)
	}
	if _, err := os.Stat(filepath.Join(meta, "sync-fingerprint")); !os.IsNotExist(err) {
		t.Fatalf("recovery retained its stale overlay fingerprint: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("recovery retry executed an ambient helper, hook, or profile: %v", err)
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

func assertGitOverlayRecoveryCoherent(t *testing.T, workdir string, repo Repo) {
	t.Helper()
	if got := gitOutput(workdir, "rev-parse", "--verify", "HEAD^{commit}"); got != repo.Head {
		t.Fatalf("recovered remote HEAD=%q want=%q", got, repo.Head)
	}
	wantTree := gitOutput(repo.Root, "rev-parse", "--verify", repo.Head+"^{tree}")
	if got := gitOutput(workdir, "write-tree"); got != wantTree {
		t.Fatalf("recovered remote index tree=%q want=%q", got, wantTree)
	}
	baseSHA := gitHydrateBaseSHA(repo, repo.BaseRef)
	if got := gitOutput(workdir, "rev-parse", "--verify", "refs/remotes/origin/"+repo.BaseRef+"^{commit}"); got != baseSHA {
		t.Fatalf("recovered remote base=%q want=%q", got, baseSHA)
	}
	for _, args := range [][]string{
		{"rev-parse", "--verify", "HEAD^"},
		{"merge-base", "origin/" + repo.BaseRef, "HEAD"},
		{"diff", "origin/" + repo.BaseRef + "...HEAD"},
	} {
		if output, err := exec.Command("git", append([]string{"-C", workdir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("recovered history git %v: %v\n%s", args, err, output)
		}
	}
	markerPath := filepath.Join(workdir, ".git", "crabbox", "git-hydrate-base")
	if marker, err := os.ReadFile(markerPath); err != nil || string(marker) != repo.BaseRef+" "+baseSHA+"\n" {
		t.Fatalf("recovered hydration marker=%q err=%v", marker, err)
	}
	if _, err := os.Stat(filepath.Join(workdir, ".git", "crabbox", "sync-fingerprint")); !os.IsNotExist(err) {
		t.Fatalf("recovered workspace retained a stale overlay fingerprint: %v", err)
	}
}

func TestRunGitOverlaySuccessFallbackAndLateLocalEdit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX SSH runtime integration")
	}
	for _, mode := range []string{
		"success", "file-origin", "runtime-state", "classified-fallback", "private-origin", "origin-unavailable", "missing-origin", "local-ineligible", "local-fingerprint-reuse", "remote-fingerprint-reuse", "unsafe-metadata",
		"local-hidden-fingerprint", "local-sparse-hidden-fingerprint", "local-config-hidden-fingerprint", "remote-hidden-fingerprint", "remote-skip-hidden-fingerprint", "remote-inspection-hidden-fingerprint",
		"ordinary-private-fresh", "ordinary-private-reused", "ordinary-unavailable-fresh", "ordinary-unavailable-reused", "ordinary-server-failure-reused",
		"symlinked-workdir", "symlinked-git", "symlinked-git-config", "symlinked-git-objects", "linked-worktree",
		"checkout-file-obstruction", "post-reset-cache", "post-reset-mass-deletion", "late-local-edit",
	} {
		t.Run(mode, func(t *testing.T) {
			clearConfigEnv(t)
			fixture := newGitOverlayFixture(t)
			ordinary := strings.HasPrefix(mode, "ordinary-")
			reused := ordinary && strings.HasSuffix(mode, "-reused")
			hiddenFingerprint := strings.HasSuffix(mode, "-hidden-fingerprint")
			fingerprintReuse := mode == "local-fingerprint-reuse" || mode == "remote-fingerprint-reuse" || hiddenFingerprint
			privateOrigin := mode == "private-origin" || strings.HasPrefix(mode, "ordinary-private-")
			unavailableOrigin := mode == "origin-unavailable" || strings.HasPrefix(mode, "ordinary-unavailable-")
			switch mode {
			case "file-origin":
				runGit(t, fixture.root, "remote", "set-url", "origin", "file://"+filepath.ToSlash(fixture.origin))
			case "missing-origin":
				runGit(t, fixture.root, "remote", "remove", "origin")
			case "origin-unavailable", "ordinary-unavailable-fresh", "ordinary-unavailable-reused":
				unavailableOrigin := newGitTransportFailureHTTPServer(t) + "/unavailable.git"
				runGit(t, fixture.root, "remote", "set-url", "origin", unavailableOrigin)
			case "private-origin", "ordinary-private-fresh", "ordinary-private-reused":
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					if request.Header.Get("Authorization") != "" {
						t.Errorf("overlay forwarded origin credentials")
					}
					w.Header().Set("WWW-Authenticate", `Basic realm="private"`)
					http.Error(w, "authentication required", http.StatusUnauthorized)
				}))
				t.Cleanup(server.Close)
				runGit(t, fixture.root, "remote", "set-url", "origin", server.URL+"/private.git")
			case "ordinary-server-failure-reused":
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					http.Error(w, "origin temporarily unavailable", http.StatusServiceUnavailable)
				}))
				t.Cleanup(server.Close)
				runGit(t, fixture.root, "remote", "set-url", "origin", server.URL+"/unavailable.git")
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
			config := fmt.Sprintf("workRoot: %q\nsync:\n  gitOverlay: %t\n  gitSeed: true\n  delete: true\n  fingerprint: %t\n  exclude:\n    - excluded.txt\n", remoteRoot, !ordinary, ordinary || fingerprintReuse)
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
			fsmonitor := filepath.Join(attackBin, "fsmonitor")
			if err := os.WriteFile(fsmonitor, []byte("#!/bin/sh\nprintf hostile >"+shellQuote(attackMarker)+"\nexit 99\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			workdir := filepath.Join(remoteRoot, "cbx_overlay_runtime", repo.Name)
			if reused || mode == "runtime-state" || mode == "checkout-file-obstruction" || mode == "post-reset-cache" || mode == "post-reset-mass-deletion" || mode == "classified-fallback" || mode == "missing-origin" || fingerprintReuse || mode == "unsafe-metadata" || mode == "symlinked-workdir" || mode == "symlinked-git" || mode == "symlinked-git-config" || mode == "symlinked-git-objects" || mode == "linked-worktree" {
				if err := os.MkdirAll(filepath.Dir(workdir), 0o755); err != nil {
					t.Fatal(err)
				}
				clonePath := workdir
				if mode == "symlinked-workdir" {
					clonePath = filepath.Join(testRoot, "outside-workspace")
				} else if mode == "linked-worktree" {
					clonePath = filepath.Join(testRoot, "linked-worktree-base")
				}
				if output, err := exec.Command("git", "clone", "--quiet", fixture.origin, clonePath).CombinedOutput(); err != nil {
					t.Fatalf("clone existing runtime workspace: %v\n%s", err, output)
				}
				if mode == "symlinked-workdir" {
					mustWriteTestFile(t, filepath.Join(clonePath, "outside-sentinel"), "outside workspace remains untouched\n")
					if err := os.Symlink(clonePath, workdir); err != nil {
						t.Fatal(err)
					}
				} else if mode == "linked-worktree" {
					runGit(t, clonePath, "worktree", "add", "--detach", workdir, repo.Head)
					metadata := gitOutput(workdir, "rev-parse", "--git-path", "crabbox")
					if !filepath.IsAbs(metadata) {
						metadata = filepath.Join(workdir, metadata)
					}
					mustWriteTestFile(t, filepath.Join(metadata, "sync-manifest"), "clean.txt\x00excluded.txt\x00")
				} else if mode == "symlinked-git" || mode == "symlinked-git-config" || mode == "symlinked-git-objects" {
					gitPath := filepath.Join(workdir, ".git")
					if mode == "symlinked-git-config" {
						gitPath = filepath.Join(gitPath, "config")
					} else if mode == "symlinked-git-objects" {
						gitPath = filepath.Join(gitPath, "objects")
					}
					external := filepath.Join(testRoot, "outside-git-boundary")
					if err := os.Rename(gitPath, external); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(external, gitPath); err != nil {
						t.Fatal(err)
					}
					sentinel := filepath.Join(testRoot, "outside-sentinel")
					if mode == "symlinked-git" || mode == "symlinked-git-objects" {
						sentinel = filepath.Join(external, "outside-sentinel")
					}
					mustWriteTestFile(t, sentinel, "outside Git boundary remains untouched\n")
				}
				if mode == "runtime-state" || mode == "post-reset-cache" || mode == "post-reset-mass-deletion" {
					mustWriteTestFile(t, filepath.Join(workdir, ".crabbox", "env", "persisted-helper"), "runtime helper survives\n")
				} else if mode == "classified-fallback" {
					mustWriteTestFile(t, filepath.Join(workdir, ".git", "crabbox", "sync-manifest"), "clean.txt\x00excluded.txt\x00")
				} else if mode == "missing-origin" || reused {
					meta := filepath.Join(workdir, ".git", "crabbox")
					mustWriteTestFile(t, filepath.Join(meta, "sync-manifest"), "clean.txt\x00excluded.txt\x00")
					mustWriteTestFile(t, filepath.Join(meta, "sync-finalize-token"), "stale")
					mustWriteTestFile(t, filepath.Join(meta, "sync-finalize-complete-token"), "stale")
					mustWriteTestFile(t, filepath.Join(meta, "sync-fingerprint"), "stale")
					mustWriteTestFile(t, filepath.Join(meta, "git-hydrate-base"), "main stale\n")
					if reused {
						runGit(t, workdir, "remote", "set-url", "origin", repo.RemoteURL)
					} else {
						runGit(t, workdir, "config", "core.fsmonitor", fsmonitor)
					}
				}
				if mode == "checkout-file-obstruction" {
					runGit(t, workdir, "checkout", "-q", "--detach", fixture.repo.Head)
					mustWriteTestFile(t, filepath.Join(workdir, "shape"), "stale untracked managed file\n")
					mustWriteTestFile(t, filepath.Join(workdir, ".git", "crabbox", "sync-manifest"), "shape\x00")
				}
				if fingerprintReuse {
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
					if hiddenFingerprint {
						const token = "71717171717171717171717171717171"
						mustWriteTestFile(t, filepath.Join(meta, remoteSyncPendingManifestName(token)), string(manifest.NUL()))
						if out, err := runOverlayCommand(t, remoteFinalizeSync(workdir, remoteSyncFinalizeOptions{
							Token: token, Fingerprint: fingerprint, Coherence: coherence,
							HydrateGit: true, BaseRef: repo.BaseRef, BaseSHA: repo.Head,
						}), nil); err != nil {
							t.Fatalf("finalize ordinary fingerprint before hidden edit: %v\n%s", err, out)
						}
						editRoot, flag := workdir, "--assume-unchanged"
						if strings.HasPrefix(mode, "local-") {
							editRoot = fixture.root
						} else if mode == "remote-skip-hidden-fingerprint" {
							flag = "--skip-worktree"
						}
						runGit(t, editRoot, "update-index", flag, "clean.txt")
						if mode == "local-sparse-hidden-fingerprint" {
							runGit(t, editRoot, "config", "core.sparseCheckout", "true")
						} else if mode == "local-config-hidden-fingerprint" {
							runGit(t, editRoot, "config", "core.filemode", "false")
						}
						mustWriteTestFile(t, filepath.Join(editRoot, "clean.txt"), "hidden edit must not survive a fingerprint skip\n")
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
				if mode == "checkout-file-obstruction" || mode == "post-reset-cache" {
					hook := filepath.Join(workdir, ".git", "hooks", "reference-transaction")
					if err := os.WriteFile(hook, []byte("#!/bin/sh\nprintf hostile >"+shellQuote(attackMarker)+"\n"), 0o755); err != nil {
						t.Fatal(err)
					}
				}
			}
			installWorkspaceOwnerAwareSSH(t, filepath.Join(binDir, "ssh"), fmt.Sprintf(`#!/bin/sh
# These remote commands run locally, including the ordinary Git seed fallback.
# Keep the stand-in noninteractive and independent of the operator's Git auth.
unset GIT_CONFIG_PARAMETERS
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null GIT_CONFIG_COUNT=0
export GIT_TERMINAL_PROMPT=0 GIT_ASKPASS=/bin/false SSH_ASKPASS=/bin/false GCM_INTERACTIVE=Never
cmd="$1"
printf '%%s\n---\n' "$cmd" >> "$CRABBOX_FAKE_SSH_LOG"
case "$cmd" in
  *crabbox-ready*) exit 0 ;;
  *".overlay-git."*)
    case "$CRABBOX_FAKE_OVERLAY_MODE" in
      classified-fallback|remote-fingerprint-reuse) printf '%%scheckout_failed\n' %s >&2; exit %d ;;
      remote-inspection-hidden-fingerprint) printf '%%sindex_inspection_failed\n' %s >&2; exit %d ;;
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
`, shellQuote(gitOverlayFallbackMarker), gitOverlayFallbackExitCode, shellQuote(gitOverlayFallbackMarker), gitOverlayFallbackExitCode))
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
			credentialMarker := ""
			if privateOrigin {
				credentialMarker = installGitOverlayCredentialCanary(t)
			}
			var stdout bytes.Buffer
			var stderr synchronizedBuffer
			app := App{Stdout: &stdout, Stderr: &stderr}
			runErr := app.runCommand(context.Background(), []string{"--provider", providerName, "--no-hydrate", "--sync-only"})
			if credentialMarker != "" {
				if _, err := os.Stat(credentialMarker); !os.IsNotExist(err) {
					t.Fatalf("local SSH fixture consulted ambient Git credentials: %v", err)
				}
			}
			if mode == "ordinary-server-failure-reused" {
				if runErr == nil || !strings.Contains(runErr.Error(), "Git coherence fetch failed") {
					t.Fatalf("ordinary server failure was not fatal: err=%v stderr=%s", runErr, stderr.String())
				}
				if strings.Contains(stderr.String(), "git origin fallback") {
					t.Fatalf("ordinary server failure downgraded to manifest fallback: %s", stderr.String())
				}
				return
			}
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
			if mode == "symlinked-workdir" || mode == "symlinked-git" || mode == "symlinked-git-config" || mode == "symlinked-git-objects" {
				wantReason := map[string]string{
					"symlinked-workdir":     "symlink_remote_root",
					"symlinked-git":         "symlink_git_directory",
					"symlinked-git-config":  "symlink_git_config",
					"symlinked-git-objects": "symlink_git_objects",
				}[mode]
				if runErr == nil || !strings.Contains(runErr.Error(), wantReason) {
					t.Fatalf("unsafe Git boundary %q did not fail closed: err=%v stderr=%s", mode, runErr, stderr.String())
				}
				if _, err := os.Stat(rsyncLog); !os.IsNotExist(err) {
					t.Fatalf("unsafe Git boundary continued into rsync: %v", err)
				}
				sshCommands, err := os.ReadFile(sshLog)
				if err != nil || bytes.Contains(sshCommands, []byte("invalid sync manifest length")) {
					t.Fatalf("unsafe Git boundary wrote a remote sync manifest: commands=%q err=%v", sshCommands, err)
				}
				sentinel := filepath.Join(testRoot, "outside-sentinel")
				wantSentinel := "outside Git boundary remains untouched\n"
				if mode == "symlinked-workdir" {
					sentinel = filepath.Join(testRoot, "outside-workspace", "outside-sentinel")
					wantSentinel = "outside workspace remains untouched\n"
				} else if mode == "symlinked-git" || mode == "symlinked-git-objects" {
					sentinel = filepath.Join(testRoot, "outside-git-boundary", "outside-sentinel")
				}
				if content, err := os.ReadFile(sentinel); err != nil || string(content) != wantSentinel {
					t.Fatalf("unsafe Git boundary mutated outside sentinel=%q err=%v", content, err)
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
			case "local-hidden-fingerprint", "local-sparse-hidden-fingerprint", "local-config-hidden-fingerprint", "remote-hidden-fingerprint", "remote-skip-hidden-fingerprint", "remote-inspection-hidden-fingerprint":
				want := "base clean.txt\n"
				if strings.HasPrefix(mode, "local-") {
					want = "hidden edit must not survive a fingerprint skip\n"
				}
				if string(content) != want {
					t.Errorf("hidden-index fallback left stale bytes: got=%q want=%q; stderr=%s", content, want, stderr.String())
				}
				if strings.Contains(stderr.String(), "No changes detected, skipping sync") {
					t.Errorf("hidden-index fallback reused an ordinary fingerprint: %s", stderr.String())
				}
				manifest, _ := fixture.manifest(t)
				if transfer, readErr := os.ReadFile(rsyncLog); readErr != nil || !bytes.Equal(transfer, manifest.NUL()) {
					t.Errorf("hidden-index fallback omitted full manifest: got=%q want=%q err=%v", transfer, manifest.NUL(), readErr)
				}
				wantReason := "assume_unchanged_index"
				if mode == "remote-skip-hidden-fingerprint" {
					wantReason = "skip_worktree_index"
				} else if mode == "local-sparse-hidden-fingerprint" {
					wantReason = "sparse_checkout"
				} else if mode == "local-config-hidden-fingerprint" {
					wantReason = "core_filemode"
				} else if mode == "remote-inspection-hidden-fingerprint" {
					wantReason = "index_inspection_failed"
				}
				if !strings.Contains(stderr.String(), "git overlay fallback reason="+wantReason) {
					t.Errorf("hidden-index fallback reason missing: %s", stderr.String())
				}
				assertGitOverlayRecoveryCoherent(t, workdir, repo)
			case "success", "file-origin", "runtime-state", "local-fingerprint-reuse", "remote-fingerprint-reuse":
				if _, err := os.Stat(rsyncLog); !os.IsNotExist(err) {
					sshCommands, _ := os.ReadFile(sshLog)
					t.Fatalf("clean overlay/legacy fingerprint unexpectedly transferred a source payload: %v\nstderr=%s\nssh=%s", err, stderr.String(), sshCommands)
				}
				if (mode == "local-fingerprint-reuse" || mode == "remote-fingerprint-reuse") && !strings.Contains(stderr.String(), "No changes detected, skipping sync") {
					t.Fatalf("unmodified fallback discarded the legacy fingerprint: %s", stderr.String())
				}
			case "classified-fallback", "private-origin", "origin-unavailable", "missing-origin", "local-ineligible", "linked-worktree", "checkout-file-obstruction", "post-reset-cache", "late-local-edit",
				"ordinary-private-fresh", "ordinary-private-reused", "ordinary-unavailable-fresh", "ordinary-unavailable-reused":
				transfer, err := os.ReadFile(rsyncLog)
				if err != nil || !bytes.Contains(transfer, []byte("clean.txt\x00")) {
					t.Fatalf("fallback omitted full manifest: data=%q err=%v", transfer, err)
				}
				wantReason := "checkout_failed"
				if privateOrigin {
					wantReason = "origin_auth_required"
				} else if unavailableOrigin {
					wantReason = "origin_unavailable"
				} else if mode == "missing-origin" {
					wantReason = "missing_origin"
				} else if mode == "local-ineligible" {
					wantReason = "include_whitelist"
				} else if mode == "linked-worktree" {
					wantReason = "unsafe_git_workspace"
				} else if mode == "post-reset-cache" {
					wantReason = "unsafe_cache_root"
				} else if mode == "late-local-edit" {
					wantReason = "local_checkout_changed"
					if string(content) != "late local change\n" {
						t.Fatalf("late local edit omitted: %q", content)
					}
				}
				warningPrefix := "git overlay fallback reason="
				if ordinary {
					warningPrefix = "git origin fallback reason="
					manifest, _ := fixture.manifest(t)
					if !bytes.Equal(transfer, manifest.NUL()) {
						t.Fatalf("ordinary fallback transferred an incomplete manifest: got=%q want=%q", transfer, manifest.NUL())
					}
				}
				if !strings.Contains(stderr.String(), warningPrefix+wantReason) {
					t.Fatalf("fallback reason missing: %s", stderr.String())
				}
				meta := filepath.Join(workdir, ".crabbox")
				if gitMetadata := gitOutput(workdir, "rev-parse", "--git-path", "crabbox"); gitMetadata != "" {
					meta = gitMetadata
					if !filepath.IsAbs(meta) {
						meta = filepath.Join(workdir, meta)
					}
				}
				if mode == "late-local-edit" || mode == "checkout-file-obstruction" || mode == "post-reset-cache" {
					assertGitOverlayRecoveryCoherent(t, workdir, repo)
				} else if !privateOrigin && !unavailableOrigin && mode != "missing-origin" {
					if _, err := os.Stat(filepath.Join(meta, "git-hydrate-base")); err != nil {
						t.Fatalf("unmodified fallback lost ordinary Git hydration metadata: %v", err)
					}
				}
				if privateOrigin || unavailableOrigin || mode == "missing-origin" {
					for _, name := range []string{"sync-fingerprint", "git-hydrate-base"} {
						if _, err := os.Stat(filepath.Join(meta, name)); !os.IsNotExist(err) {
							t.Fatalf("hardened manifest retained %s: %v", name, err)
						}
					}
					sshCommands, err := os.ReadFile(sshLog)
					if err != nil {
						t.Fatal(err)
					}
					hardenedCommands := string(sshCommands)
					if privateOrigin || unavailableOrigin {
						commands := strings.Split(hardenedCommands, "\n---\n")
						attemptMarker := ".overlay-git."
						if ordinary {
							attemptMarker = "origin_git clone --quiet"
							if reused {
								attemptMarker = "Git coherence fetch failed"
							}
						}
						foundAttempt := false
						for index, command := range commands {
							if strings.Contains(command, attemptMarker) {
								hardenedCommands = strings.Join(commands[index+1:], "\n---\n")
								foundAttempt = true
								break
							}
						}
						if !foundAttempt {
							t.Fatalf("origin fallback did not attempt %q", attemptMarker)
						}
					}
					for _, forbidden := range []string{"git clone ", "git fetch ", "git ls-remote ", "repair_origin", "base_tmp=", "refs/remotes/origin/"} {
						if strings.Contains(hardenedCommands, forbidden) {
							t.Fatalf("hardened manifest ran forbidden Git path after fallback %q:\n%s", forbidden, hardenedCommands)
						}
					}
					if strings.Contains(stderr.String(), "No changes detected, skipping sync") {
						t.Fatalf("hardened manifest reused a fingerprint: %s", stderr.String())
					}
					if _, err := os.Stat(attackMarker); !os.IsNotExist(err) {
						t.Fatalf("hardened manifest executed hostile Git/profile state: %v", err)
					}
				}
				if mode == "linked-worktree" {
					if _, err := os.Stat(filepath.Join(workdir, ".crabbox", "sync-manifest")); !os.IsNotExist(err) {
						t.Fatalf("linked fallback relocated its established metadata: %v", err)
					}
					if _, err := os.Stat(filepath.Join(meta, "sync-manifest")); err != nil {
						t.Fatalf("linked fallback did not preserve legacy metadata authority: %v", err)
					}
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

func TestRunMissingOriginReplacementLeaseStaysPlainManifest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX SSH runtime integration")
	}
	clearConfigEnv(t)
	fixture := newGitOverlayFixture(t)
	runGit(t, fixture.root, "remote", "remove", "origin")
	t.Chdir(fixture.root)
	testRoot := t.TempDir()
	isolateRunTestUserDirs(t, testRoot)
	repo, err := findRepo()
	if err != nil {
		t.Fatal(err)
	}
	remoteRoot := filepath.Join(testRoot, "remote")
	leaseIDs := []string{"cbx_missing_first", "cbx_missing_replacement"}
	providerName := runReadyPoolPreflightTestProvider{}.Name()
	var (
		acquires atomic.Int32
		receipt  terminalRunReceipt
		mu       sync.Mutex
	)
	const runID = "run_missing_origin_replacement"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		lease := func(id, state string) CoordinatorLease {
			return CoordinatorLease{
				ID: id, Provider: providerName, TargetOS: targetLinux, State: state,
				Host: "127.0.0.1", SSHUser: "crabbox", SSHPort: "22", WorkRoot: remoteRoot,
			}
		}
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v1/runs":
			_ = json.NewEncoder(w).Encode(map[string]any{"run": CoordinatorRun{
				ID: runID, Provider: providerName, State: "running", StartedAt: "2026-08-29T00:00:00Z",
			}})
		case request.Method == http.MethodPost && request.URL.Path == "/v1/runs/"+runID+"/events":
			_ = json.NewEncoder(w).Encode(map[string]any{"event": CoordinatorRunEvent{
				RunID: runID, Seq: 1, Type: "run.event", CreatedAt: "2026-08-29T00:00:00Z",
			}})
		case request.Method == http.MethodPost && request.URL.Path == "/v1/runs/"+runID+"/finish":
			var body struct {
				Receipt terminalRunReceipt `json:"receipt"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mu.Lock()
			receipt = body.Receipt
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"run": CoordinatorRun{
				ID: runID, Provider: providerName, State: "succeeded", StartedAt: "2026-08-29T00:00:00Z",
			}})
		case request.Method == http.MethodGet && request.URL.Path == "/v1/runs/"+runID+"/receipt":
			mu.Lock()
			stored := receipt
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"receipt": stored})
		case request.Method == http.MethodPost && request.URL.Path == "/v1/leases":
			index := int(acquires.Add(1)) - 1
			if index >= len(leaseIDs) {
				http.Error(w, "unexpected acquisition", http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": lease(leaseIDs[index], "active")})
		case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/v1/leases/"):
			id := strings.TrimPrefix(request.URL.Path, "/v1/leases/")
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": lease(id, "active")})
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/heartbeat"):
			id := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/v1/leases/"), "/heartbeat")
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": lease(id, "active")})
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/release"):
			id := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/v1/leases/"), "/release")
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": lease(id, "released")})
		default:
			http.NotFound(w, request)
		}
	}))
	t.Cleanup(server.Close)

	configPath := filepath.Join(testRoot, "config.yaml")
	mustWriteTestFile(t, configPath, fmt.Sprintf(
		"coordinator: %q\ncoordinatorToken: test-token\nworkRoot: %q\nsync:\n  gitOverlay: true\n  gitSeed: true\n  delete: true\n  fingerprint: true\n",
		server.URL,
		remoteRoot,
	))
	binDir := filepath.Join(testRoot, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sshLog := filepath.Join(testRoot, "ssh.log")
	transferred := filepath.Join(testRoot, "transferred")
	failedReady := filepath.Join(testRoot, "failed-ready")
	installWorkspaceOwnerAwareSSH(t, filepath.Join(binDir, "ssh"), `#!/bin/sh
cmd="$1"
printf '%s\n---\n' "$cmd" >> "$CRABBOX_FAKE_SSH_LOG"
case "$cmd" in
  *crabbox-ready*)
    if [ -e "$CRABBOX_FAKE_TRANSFERRED" ] && [ ! -e "$CRABBOX_FAKE_FAILED_READY" ]; then
      : > "$CRABBOX_FAKE_FAILED_READY"
      exit 1
    fi
    exit 0
    ;;
esac
exec /bin/bash --noprofile --norc -c "$cmd"
`)
	if err := os.WriteFile(filepath.Join(binDir, "rsync"), []byte(`#!/bin/bash
set -euo pipefail
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
cat >"$tmp"
destination="${!#}"
workdir="${destination#*:}"
workdir="${workdir%%/}"
while IFS= read -r -d '' rel; do
  mkdir -p "$workdir/$(dirname "$rel")"
  cp -a "$CRABBOX_FAKE_REPO_ROOT/$rel" "$workdir/$rel"
done <"$tmp"
: > "$CRABBOX_FAKE_TRANSFERRED"
`), 0o755); err != nil {
		t.Fatal(err)
	}
	previousTimeout := runBeforeCommandSSHReadyTimeout
	// Stay below the ten-second readiness retry interval so the first forced
	// failure still replaces the lease, while healthy probes have time to start.
	runBeforeCommandSSHReadyTimeout = 5 * time.Second
	t.Cleanup(func() { runBeforeCommandSSHReadyTimeout = previousTimeout })
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_FAKE_SSH_LOG", sshLog)
	t.Setenv("CRABBOX_FAKE_REPO_ROOT", fixture.root)
	t.Setenv("CRABBOX_FAKE_TRANSFERRED", transferred)
	t.Setenv("CRABBOX_FAKE_FAILED_READY", failedReady)
	t.Setenv("CRABBOX_FAKE_SSH_PORT", "22")
	t.Setenv("CRABBOX_FAKE_SSH_PROXY", "1")

	var stdout, stderr bytes.Buffer
	if err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--provider", providerName, "--no-hydrate", "--", "true",
	}); err != nil {
		commands, _ := os.ReadFile(sshLog)
		t.Fatalf("replacement run: %v\nstdout=%s\nstderr=%s\nssh=%s", err, stdout.String(), stderr.String(), commands)
	}
	if got := acquires.Load(); got != 2 {
		t.Fatalf("acquisitions=%d want=2\n%s", got, stderr.String())
	}
	if got := strings.Count(stderr.String(), "git overlay fallback reason=missing_origin"); got != 2 {
		t.Fatalf("missing-origin classifications=%d want=2\n%s", got, stderr.String())
	}
	commands, err := os.ReadFile(sshLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"git clone ", "git fetch ", "git ls-remote ", "repair_origin", "base_tmp=", "refs/remotes/origin/"} {
		if bytes.Contains(commands, []byte(forbidden)) {
			t.Fatalf("replacement plain manifest ran forbidden Git path %q:\n%s", forbidden, commands)
		}
	}
	for _, id := range leaseIDs {
		metaDir := filepath.Join(remoteRoot, id, repo.Name, ".crabbox")
		if _, err := os.Stat(filepath.Join(metaDir, "sync-manifest")); err != nil {
			t.Fatalf("%s manifest: %v", id, err)
		}
		for _, name := range []string{"sync-fingerprint", "git-hydrate-base"} {
			if _, err := os.Stat(filepath.Join(metaDir, name)); !os.IsNotExist(err) {
				t.Fatalf("%s retained %s: %v", id, name, err)
			}
		}
	}
}
