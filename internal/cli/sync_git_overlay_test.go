package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

func TestGitOverlaySnapshotRetriesWhenCleanTrackedFileChanges(t *testing.T) {
	fixture := newGitOverlayFixture(t)
	_, excludes := fixture.manifest(t)
	attempts := 0
	snapshot, err := prepareGitOverlaySnapshotWithHook(fixture.repo, fixture.cfg, excludes, nil, fixture.plan, func(phase string, attempt int, _ string) {
		if phase == "snapshot_fingerprinted" {
			attempts = attempt
			if attempt == 1 {
				mustWriteTestFile(t, filepath.Join(fixture.root, "clean.txt"), "changed during fingerprint\n")
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snapshot.cleanup() }()
	if attempts != 2 {
		t.Fatalf("snapshot attempts=%d want 2", attempts)
	}
	if !slices.Contains(snapshot.Manifest.OverlayFiles, "clean.txt") {
		t.Fatalf("newly changed tracked file missing from overlay: %v", snapshot.Manifest.OverlayFiles)
	}
	if got, readErr := os.ReadFile(filepath.Join(snapshot.Root, "clean.txt")); readErr != nil || string(got) != "changed during fingerprint\n" {
		t.Fatalf("snapshot clean.txt=%q err=%v", got, readErr)
	}
	assertGitOverlaySnapshotFingerprint(t, fixture.repo, fixture.cfg, excludes, fixture.plan, snapshot)
}

func TestGitOverlaySnapshotRetriesSameSizeChangedFileMutation(t *testing.T) {
	fixture := newGitOverlayFixture(t)
	mustWriteTestFile(t, filepath.Join(fixture.root, "unstaged.txt"), "aaaa\n")
	_, excludes := fixture.manifest(t)
	attempts := 0
	snapshot, err := prepareGitOverlaySnapshotWithHook(fixture.repo, fixture.cfg, excludes, nil, fixture.plan, func(phase string, attempt int, _ string) {
		if phase == "snapshot_fingerprinted" {
			attempts = attempt
			if attempt == 1 {
				mustWriteTestFile(t, filepath.Join(fixture.root, "unstaged.txt"), "bbbb\n")
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snapshot.cleanup() }()
	if attempts != 2 {
		t.Fatalf("snapshot attempts=%d want 2", attempts)
	}
	if got, readErr := os.ReadFile(filepath.Join(snapshot.Root, "unstaged.txt")); readErr != nil || string(got) != "bbbb\n" {
		t.Fatalf("snapshot unstaged.txt=%q err=%v", got, readErr)
	}
	assertGitOverlaySnapshotFingerprint(t, fixture.repo, fixture.cfg, excludes, fixture.plan, snapshot)
}

func TestGitOverlaySnapshotRetriesReplacementAfterLstat(t *testing.T) {
	fixture := newGitOverlayFixture(t)
	source := filepath.Join(fixture.root, "unstaged.txt")
	mustWriteTestFile(t, source, "aaaa\n")
	info, err := os.Lstat(source)
	if err != nil {
		t.Fatal(err)
	}
	_, excludes := fixture.manifest(t)
	attempts := 0
	snapshot, err := prepareGitOverlaySnapshotWithHook(fixture.repo, fixture.cfg, excludes, nil, fixture.plan, func(phase string, attempt int, _ string) {
		if phase == "snapshot_created" {
			attempts = attempt
		}
		if phase != "after_lstat" || attempt != 1 {
			return
		}
		if err := os.Rename(source, source+".old"); err != nil {
			t.Fatal(err)
		}
		mustWriteTestFile(t, source, "bbbb\n")
		if err := os.Chmod(source, info.Mode().Perm()); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(source, info.ModTime(), info.ModTime()); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snapshot.cleanup() }()
	if attempts != 2 {
		t.Fatalf("snapshot attempts=%d want 2", attempts)
	}
	if got, readErr := os.ReadFile(filepath.Join(snapshot.Root, "unstaged.txt")); readErr != nil || string(got) != "bbbb\n" {
		t.Fatalf("snapshot unstaged.txt=%q err=%v", got, readErr)
	}
}

func TestGitOverlaySnapshotRetriesMetadataChangeAfterLstat(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes")
	}
	fixture := newGitOverlayFixture(t)
	source := filepath.Join(fixture.root, "unstaged.txt")
	mustWriteTestFile(t, source, "local change\n")
	if err := os.Chmod(source, 0o600); err != nil {
		t.Fatal(err)
	}
	_, excludes := fixture.manifest(t)
	attempts := 0
	snapshot, err := prepareGitOverlaySnapshotWithHook(fixture.repo, fixture.cfg, excludes, nil, fixture.plan, func(phase string, attempt int, _ string) {
		if phase == "snapshot_created" {
			attempts = attempt
		}
		if phase == "after_lstat" && attempt == 1 {
			if err := os.Chmod(source, 0o640); err != nil {
				t.Fatal(err)
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snapshot.cleanup() }()
	if attempts != 2 {
		t.Fatalf("snapshot attempts=%d want 2", attempts)
	}
	info, err := os.Stat(filepath.Join(snapshot.Root, "unstaged.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("snapshot mode=%#o want 0640", got)
	}
}

func TestGitOverlaySnapshotClassifiesDisappearanceAfterLstatAsDrift(t *testing.T) {
	for _, kind := range []string{"regular file", "symlink", "nested parent"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "symlink" && runtime.GOOS == "windows" {
				t.Skip("POSIX symlink integration")
			}
			sourceRoot := t.TempDir()
			snapshotRoot := t.TempDir()
			rel := "payload"
			source := filepath.Join(sourceRoot, rel)
			remove := source
			if kind == "nested parent" {
				rel = "nested/payload"
				source = filepath.Join(sourceRoot, filepath.FromSlash(rel))
				remove = filepath.Dir(source)
			}
			mustWriteTestFile(t, source, "payload\n")
			if kind == "symlink" {
				if err := os.Remove(source); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("target", source); err != nil {
					t.Fatal(err)
				}
			}
			err := copyGitOverlaySnapshotWithHook(sourceRoot, snapshotRoot, []string{rel}, 1, func(phase string, _ int, _ string) {
				if phase == "after_lstat" {
					if err := os.RemoveAll(remove); err != nil {
						t.Fatal(err)
					}
				}
			})
			if !errors.Is(err, errGitOverlaySnapshotDrift) {
				t.Fatalf("disappearance error=%v, want snapshot drift", err)
			}
		})
	}
}

func TestGitOverlaySnapshotStableSourceErrorsStayTerminal(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	mustWriteTestFile(t, source, "stable\n")
	observed, err := os.Lstat(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, operationErr := range []error{errors.New("synthetic source failure"), os.ErrPermission} {
		got := classifyGitOverlaySnapshotSourceError(source, observed, operationErr)
		if !errors.Is(got, operationErr) || errors.Is(got, errGitOverlaySnapshotDrift) {
			t.Fatalf("stable source error=%v, want terminal %v", got, operationErr)
		}
	}
}

func TestGitOverlaySnapshotRetriesIgnoreRuleDriftAndOwnsAcceptedRules(t *testing.T) {
	fixture := newGitOverlayFixture(t)
	mustWriteTestFile(t, filepath.Join(fixture.root, "generated.txt"), "generated\n")
	_, excludes := fixture.manifest(t)
	attempts := 0
	snapshot, err := prepareGitOverlaySnapshotWithHook(fixture.repo, fixture.cfg, excludes, nil, fixture.plan, func(phase string, attempt int, _ string) {
		if phase == "snapshot_created" {
			attempts = attempt
		}
		if phase == "snapshot_fingerprinted" && attempt == 1 {
			mustWriteTestFile(t, filepath.Join(fixture.root, ".crabboxignore"), "generated.txt\n")
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snapshot.cleanup() }()
	if attempts != 2 {
		t.Fatalf("snapshot attempts=%d want 2", attempts)
	}
	if slices.Contains(snapshot.Manifest.Files, "generated.txt") || slices.Contains(snapshot.Manifest.OverlayFiles, "generated.txt") {
		t.Fatalf("accepted snapshot retained newly excluded file: %+v", snapshot.Manifest)
	}
	if !slices.Contains(snapshot.Excludes.patterns(), "generated.txt") {
		t.Fatalf("accepted snapshot excludes=%v", snapshot.Excludes.patterns())
	}
	assertGitOverlaySnapshotFingerprint(t, fixture.repo, fixture.cfg, snapshot.Excludes, fixture.plan, snapshot)
}

func TestGitOverlaySnapshotDestinationFailureIsTerminal(t *testing.T) {
	fixture := newGitOverlayFixture(t)
	mustWriteTestFile(t, filepath.Join(fixture.root, "unstaged.txt"), "local change\n")
	_, excludes := fixture.manifest(t)
	attempts := 0
	var snapshotRoot string
	_, err := prepareGitOverlaySnapshotWithHook(fixture.repo, fixture.cfg, excludes, nil, fixture.plan, func(phase string, attempt int, root string) {
		if phase != "snapshot_created" {
			return
		}
		attempts = attempt
		snapshotRoot = root
		mustWriteTestFile(t, filepath.Join(root, "unstaged.txt"), "obstruction\n")
	})
	if snapshotRoot != "" {
		t.Cleanup(func() { _ = os.RemoveAll(snapshotRoot) })
	}
	if err == nil || errors.Is(err, errGitOverlaySnapshotDrift) {
		t.Fatalf("destination failure=%v", err)
	}
	if attempts != 1 {
		t.Fatalf("terminal destination failure attempts=%d want 1", attempts)
	}
}

func TestGitOverlaySnapshotABAMutationCannotDivergeFingerprint(t *testing.T) {
	fixture := newGitOverlayFixture(t)
	mustWriteTestFile(t, filepath.Join(fixture.root, "unstaged.txt"), "aaaa\n")
	_, excludes := fixture.manifest(t)
	snapshot, err := prepareGitOverlaySnapshotWithHook(fixture.repo, fixture.cfg, excludes, nil, fixture.plan, func(phase string, attempt int, _ string) {
		if attempt != 1 {
			return
		}
		switch phase {
		case "snapshot_fingerprinted":
			mustWriteTestFile(t, filepath.Join(fixture.root, "unstaged.txt"), "bbbb\n")
		case "before_live_fingerprint":
			mustWriteTestFile(t, filepath.Join(fixture.root, "unstaged.txt"), "aaaa\n")
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snapshot.cleanup() }()
	if got, readErr := os.ReadFile(filepath.Join(snapshot.Root, "unstaged.txt")); readErr != nil || string(got) != "aaaa\n" {
		t.Fatalf("snapshot unstaged.txt=%q err=%v", got, readErr)
	}
	assertGitOverlaySnapshotFingerprint(t, fixture.repo, fixture.cfg, excludes, fixture.plan, snapshot)
}

func TestGitOverlaySnapshotStableTreeReusesFingerprint(t *testing.T) {
	fixture := newGitOverlayFixture(t)
	mustWriteTestFile(t, filepath.Join(fixture.root, "unstaged.txt"), "stable local change\n")
	_, excludes := fixture.manifest(t)
	first, err := prepareGitOverlaySnapshot(fixture.repo, fixture.cfg, excludes, nil, fixture.plan)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.cleanup() }()
	second, err := prepareGitOverlaySnapshot(fixture.repo, fixture.cfg, excludes, nil, fixture.plan)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.cleanup() }()
	if first.Fingerprint == "" || first.Fingerprint != second.Fingerprint {
		t.Fatalf("stable snapshot fingerprints first=%q second=%q", first.Fingerprint, second.Fingerprint)
	}
	if !sameSyncManifest(first.Manifest, second.Manifest) {
		t.Fatalf("stable snapshot manifests diverged: first=%+v second=%+v", first.Manifest, second.Manifest)
	}
}

func TestGitOverlaySnapshotAcceptsStableStagedIndex(t *testing.T) {
	fixture := newGitOverlayFixture(t)
	mustWriteTestFile(t, filepath.Join(fixture.root, "staged.txt"), "stable staged change\n")
	runGit(t, fixture.root, "add", "staged.txt")
	wantState, err := captureGitOverlayCheckoutState(fixture.root)
	if err != nil {
		t.Fatal(err)
	}
	if wantState.IndexTree == fixture.plan.Tree {
		t.Fatalf("staged index tree=%q unexpectedly matches target tree", wantState.IndexTree)
	}
	_, excludes := fixture.manifest(t)
	snapshot, err := prepareGitOverlaySnapshot(fixture.repo, fixture.cfg, excludes, nil, fixture.plan)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snapshot.cleanup() }()
	if snapshot.Checkout != wantState {
		t.Fatalf("snapshot checkout=%+v want %+v", snapshot.Checkout, wantState)
	}
	if got, readErr := os.ReadFile(filepath.Join(snapshot.Root, "staged.txt")); readErr != nil || string(got) != "stable staged change\n" {
		t.Fatalf("snapshot staged.txt=%q err=%v", got, readErr)
	}
}

func TestGitOverlaySnapshotRetriesIndexMutation(t *testing.T) {
	fixture := newGitOverlayFixture(t)
	mustWriteTestFile(t, filepath.Join(fixture.root, "staged.txt"), "first staged value\n")
	runGit(t, fixture.root, "add", "staged.txt")
	_, excludes := fixture.manifest(t)
	attempts := 0
	snapshot, err := prepareGitOverlaySnapshotWithHook(fixture.repo, fixture.cfg, excludes, nil, fixture.plan, func(phase string, attempt int, _ string) {
		if phase == "initial_checkout_state_captured" {
			attempts = attempt
		}
		if phase == "before_final_checkout_state" && attempt == 1 {
			mustWriteTestFile(t, filepath.Join(fixture.root, "staged.txt"), "second staged value\n")
			runGit(t, fixture.root, "add", "staged.txt")
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snapshot.cleanup() }()
	if attempts != 2 {
		t.Fatalf("snapshot attempts=%d want 2", attempts)
	}
	if got, readErr := os.ReadFile(filepath.Join(snapshot.Root, "staged.txt")); readErr != nil || string(got) != "second staged value\n" {
		t.Fatalf("snapshot staged.txt=%q err=%v", got, readErr)
	}
	if snapshot.Checkout.IndexTree != gitOutput(fixture.root, "write-tree") {
		t.Fatalf("snapshot index tree=%q current=%q", snapshot.Checkout.IndexTree, gitOutput(fixture.root, "write-tree"))
	}
}

func TestGitOverlaySnapshotRetriesIndexMutationAcrossValidationWindows(t *testing.T) {
	for _, phase := range []string{"before_initial_validation_checkout_state", "before_accepted_checkout_state"} {
		t.Run(phase, func(t *testing.T) {
			fixture := newGitOverlayFixture(t)
			mustWriteTestFile(t, filepath.Join(fixture.root, "staged.txt"), "first staged value\n")
			runGit(t, fixture.root, "add", "staged.txt")
			_, excludes := fixture.manifest(t)
			attempts := 0
			snapshot, err := prepareGitOverlaySnapshotWithHook(fixture.repo, fixture.cfg, excludes, nil, fixture.plan, func(current string, attempt int, _ string) {
				attempts = max(attempts, attempt)
				if current == phase && attempt == 1 {
					mustWriteTestFile(t, filepath.Join(fixture.root, "staged.txt"), "second staged value\n")
					runGit(t, fixture.root, "add", "staged.txt")
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = snapshot.cleanup() }()
			if attempts != 2 {
				t.Fatalf("snapshot attempts=%d want 2", attempts)
			}
			if got, readErr := os.ReadFile(filepath.Join(snapshot.Root, "staged.txt")); readErr != nil || string(got) != "second staged value\n" {
				t.Fatalf("snapshot staged.txt=%q err=%v", got, readErr)
			}
		})
	}
}

func TestGitOverlaySnapshotFinalAcceptedManifestRejectsLateMutation(t *testing.T) {
	fixture := newGitOverlayFixture(t)
	mustWriteTestFile(t, filepath.Join(fixture.root, "unstaged.txt"), "first local value\n")
	_, excludes := fixture.manifest(t)
	attempts := 0
	snapshot, err := prepareGitOverlaySnapshotWithHook(fixture.repo, fixture.cfg, excludes, nil, fixture.plan, func(phase string, attempt int, _ string) {
		attempts = max(attempts, attempt)
		if phase == "before_final_checkout_state" && attempt == 1 {
			mustWriteTestFile(t, filepath.Join(fixture.root, "unstaged.txt"), "second local value\n")
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snapshot.cleanup() }()
	if attempts != 2 {
		t.Fatalf("snapshot attempts=%d want 2", attempts)
	}
	if got, readErr := os.ReadFile(filepath.Join(snapshot.Root, "unstaged.txt")); readErr != nil || string(got) != "second local value\n" {
		t.Fatalf("snapshot unstaged.txt=%q err=%v", got, readErr)
	}
}

func TestGitOverlaySnapshotHeadMutationDuringInitialValidationBecomesStaleTarget(t *testing.T) {
	fixture := newGitOverlayFixture(t)
	_, excludes := fixture.manifest(t)
	created := false
	_, err := prepareGitOverlaySnapshotWithHook(fixture.repo, fixture.cfg, excludes, nil, fixture.plan, func(phase string, attempt int, _ string) {
		if phase == "snapshot_created" {
			created = true
		}
		if phase == "before_initial_validation_checkout_state" && attempt == 1 {
			mustWriteTestFile(t, filepath.Join(fixture.root, "new-head.txt"), "new head\n")
			runGit(t, fixture.root, "add", "new-head.txt")
			runGit(t, fixture.root, "commit", "-qm", "advance during initial validation")
		}
	})
	if err == nil || !strings.Contains(err.Error(), "head_changed") || errors.Is(err, errGitOverlaySnapshotDrift) {
		t.Fatalf("advanced HEAD error=%v, want terminal stale target", err)
	}
	if created {
		t.Fatal("initial validation mutation created a snapshot before stale-target rejection")
	}
}

func TestGitOverlaySnapshotHeadMutationRetriesThenRejectsStaleTarget(t *testing.T) {
	fixture := newGitOverlayFixture(t)
	_, excludes := fixture.manifest(t)
	var snapshotRoots []string
	_, err := prepareGitOverlaySnapshotWithHook(fixture.repo, fixture.cfg, excludes, nil, fixture.plan, func(phase string, attempt int, root string) {
		if phase == "snapshot_created" {
			snapshotRoots = append(snapshotRoots, root)
		}
		if phase == "before_final_checkout_state" && attempt == 1 {
			mustWriteTestFile(t, filepath.Join(fixture.root, "new-head.txt"), "new head\n")
			runGit(t, fixture.root, "add", "new-head.txt")
			runGit(t, fixture.root, "commit", "-qm", "advance local head")
		}
	})
	if err == nil || !strings.Contains(err.Error(), "head_changed") || errors.Is(err, errGitOverlaySnapshotDrift) {
		t.Fatalf("advanced HEAD error=%v, want terminal stale target", err)
	}
	if len(snapshotRoots) != 1 {
		t.Fatalf("snapshot roots=%d want 1 before stale retry rejection", len(snapshotRoots))
	}
	if _, statErr := os.Stat(snapshotRoots[0]); !os.IsNotExist(statErr) {
		t.Fatalf("discarded snapshot %q still exists: %v", snapshotRoots[0], statErr)
	}
}

func TestGitOverlaySnapshotInitialTargetMismatchIsTerminal(t *testing.T) {
	fixture := newGitOverlayFixture(t)
	runGit(t, fixture.root, "checkout", "--quiet", "--detach", "HEAD^")
	_, excludes := fixture.manifest(t)
	created := false
	_, err := prepareGitOverlaySnapshotWithHook(fixture.repo, fixture.cfg, excludes, nil, fixture.plan, func(phase string, _ int, _ string) {
		created = created || phase == "snapshot_created"
	})
	if err == nil || !strings.Contains(err.Error(), "head_changed") || errors.Is(err, errGitOverlaySnapshotDrift) {
		t.Fatalf("initial target mismatch error=%v", err)
	}
	if created {
		t.Fatal("initial target mismatch created a snapshot")
	}
}

func TestGitOverlayCheckoutStateRejectsInvalidInitialState(t *testing.T) {
	t.Run("unborn head", func(t *testing.T) {
		root := t.TempDir()
		runGit(t, root, "init", "-q")
		if _, err := captureGitOverlayCheckoutState(root); err == nil || !strings.Contains(err.Error(), "unborn_head") || errors.Is(err, errGitOverlaySnapshotDrift) {
			t.Fatalf("unborn checkout error=%v", err)
		}
	})

	t.Run("invalid head", func(t *testing.T) {
		fixture := newGitOverlayFixture(t)
		mustWriteTestFile(t, filepath.Join(fixture.root, ".git", "HEAD"), "not a ref or object\n")
		if _, err := captureGitOverlayCheckoutState(fixture.root); err == nil || !strings.Contains(err.Error(), "invalid_head") || errors.Is(err, errGitOverlaySnapshotDrift) {
			t.Fatalf("invalid checkout error=%v", err)
		}
	})

	t.Run("corrupt index", func(t *testing.T) {
		fixture := newGitOverlayFixture(t)
		mustWriteTestFile(t, filepath.Join(fixture.root, ".git", "index"), "corrupt index\n")
		if _, err := captureGitOverlayCheckoutState(fixture.root); err == nil || !strings.Contains(err.Error(), "invalid_index") || errors.Is(err, errGitOverlaySnapshotDrift) {
			t.Fatalf("corrupt index error=%v", err)
		}
	})

	t.Run("unmerged index", func(t *testing.T) {
		fixture := newGitOverlayFixture(t)
		setUnmergedIndexModes(t, fixture.root, "clean.txt", "100644", "100644")
		if _, err := captureGitOverlayCheckoutState(fixture.root); err == nil || !strings.Contains(err.Error(), "unmerged_index") || errors.Is(err, errGitOverlaySnapshotDrift) {
			t.Fatalf("unmerged index error=%v", err)
		}
	})
}

func TestGitOverlayCheckoutStateAcceptsExactDetachedAndLinkedWorktree(t *testing.T) {
	fixture := newGitOverlayFixture(t)
	runGit(t, fixture.root, "checkout", "--quiet", "--detach", fixture.repo.Head)
	detached, err := captureGitOverlayCheckoutState(fixture.root)
	if err != nil {
		t.Fatalf("detached checkout: %v", err)
	}
	if detached.Head != fixture.repo.Head {
		t.Fatalf("detached head=%q want %q", detached.Head, fixture.repo.Head)
	}

	linked := filepath.Join(t.TempDir(), "linked")
	runGit(t, fixture.root, "worktree", "add", "--quiet", "--detach", linked, fixture.repo.Head)
	t.Cleanup(func() {
		_ = exec.Command("git", "-C", fixture.root, "worktree", "remove", "--force", linked).Run()
	})
	linkedRepo := fixture.repo
	linkedRepo.Root = linked
	linkedState, err := captureGitOverlayCheckoutState(linked)
	if err != nil {
		t.Fatalf("linked checkout: %v", err)
	}
	if linkedState.Head != fixture.repo.Head {
		t.Fatalf("linked head=%q want %q", linkedState.Head, fixture.repo.Head)
	}
	linkedPlan, blocked := syncGitCoherencePlan(fixture.cfg, linkedRepo)
	if blocked || !linkedPlan.enabled() {
		t.Fatalf("linked plan=%+v blocked=%t", linkedPlan, blocked)
	}
	excludes, err := syncExcludes(linked, fixture.cfg)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := prepareGitOverlaySnapshot(linkedRepo, fixture.cfg, excludes, nil, linkedPlan)
	if err != nil {
		t.Fatalf("linked snapshot: %v", err)
	}
	defer func() { _ = snapshot.cleanup() }()
	if snapshot.Checkout != linkedState {
		t.Fatalf("linked snapshot checkout=%+v want %+v", snapshot.Checkout, linkedState)
	}
}

func TestGitOverlayCheckoutStateIgnoresAmbientGitRouting(t *testing.T) {
	fixture := newGitOverlayFixture(t)
	want, err := captureGitOverlayCheckoutState(fixture.root)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "missing.git"))
	t.Setenv("GIT_WORK_TREE", t.TempDir())
	t.Setenv("GIT_INDEX_FILE", filepath.Join(t.TempDir(), "missing-index"))
	got, err := captureGitOverlayCheckoutState(fixture.root)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("ambient-routed checkout=%+v want %+v", got, want)
	}
}

func TestGitOverlayCheckoutStateRequiresStableDoubleSampleAndCanonicalRoot(t *testing.T) {
	for _, mutation := range []string{"head", "index"} {
		t.Run(mutation, func(t *testing.T) {
			fixture := newGitOverlayFixture(t)
			_, err := captureGitOverlayCheckoutStateWithHook(fixture.root, func() {
				switch mutation {
				case "head":
					mustWriteTestFile(t, filepath.Join(fixture.root, "new-head.txt"), "new head\n")
					runGit(t, fixture.root, "add", "new-head.txt")
					runGit(t, fixture.root, "commit", "-qm", "mutate head between samples")
				case "index":
					mustWriteTestFile(t, filepath.Join(fixture.root, "staged.txt"), "mutated index\n")
					runGit(t, fixture.root, "add", "staged.txt")
				}
			})
			if !errors.Is(err, errGitOverlaySnapshotDrift) {
				t.Fatalf("%s mutation error=%v, want checkout drift", mutation, err)
			}
		})
	}

	fixture := newGitOverlayFixture(t)
	nested := filepath.Join(fixture.root, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := captureGitOverlayCheckoutState(nested); err == nil || !strings.Contains(err.Error(), "checkout_root_mismatch") {
		t.Fatalf("nested checkout root error=%v", err)
	}
}

func TestGitOverlaySnapshotPreservesFileAndParentDirectoryModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes")
	}
	fixture := newGitOverlayFixture(t)
	privateDir := filepath.Join(fixture.root, "private")
	if err := os.Mkdir(privateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(privateDir, "script.sh")
	mustWriteTestFile(t, file, "#!/bin/sh\n")
	if err := os.Chmod(file, 0o4750); err != nil {
		t.Fatal(err)
	}
	modTime := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(file, modTime, modTime); err != nil {
		t.Fatal(err)
	}
	parentModTime := time.Unix(1_699_999_000, 123_456_000)
	if err := os.Chmod(privateDir, 0o1750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(privateDir, parentModTime, parentModTime); err != nil {
		t.Fatal(err)
	}
	_, excludes := fixture.manifest(t)
	snapshot, err := prepareGitOverlaySnapshot(fixture.repo, fixture.cfg, excludes, nil, fixture.plan)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snapshot.cleanup() }()
	parentInfo, err := os.Stat(filepath.Join(snapshot.Root, "private"))
	if err != nil {
		t.Fatal(err)
	}
	sourceParentInfo, err := os.Stat(privateDir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := gitOverlaySupportedMode(parentInfo.Mode()), gitOverlaySupportedMode(sourceParentInfo.Mode()); got != want {
		t.Fatalf("snapshot parent mode=%#o want %#o", got, want)
	}
	if want := normalizedGitOverlayFileTime(parentModTime); !parentInfo.ModTime().Equal(want) {
		t.Fatalf("snapshot parent mtime=%s want %s", parentInfo.ModTime(), want)
	}
	fileInfo, err := os.Stat(filepath.Join(snapshot.Root, "private", "script.sh"))
	if err != nil {
		t.Fatal(err)
	}
	sourceFileInfo, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := gitOverlaySupportedMode(fileInfo.Mode()), gitOverlaySupportedMode(sourceFileInfo.Mode()); got != want {
		t.Fatalf("snapshot file mode=%#o want %#o", got, want)
	}
	if !fileInfo.ModTime().Equal(modTime) {
		t.Fatalf("snapshot file mtime=%s want %s", fileInfo.ModTime(), modTime)
	}
}

func TestGitOverlaySnapshotCopiesThroughNestedReadOnlyParents(t *testing.T) {
	sourceRoot := t.TempDir()
	sourceParent := filepath.Join(sourceRoot, "locked")
	sourceDeep := filepath.Join(sourceParent, "deep")
	if err := os.MkdirAll(sourceDeep, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteTestFile(t, filepath.Join(sourceDeep, "payload.txt"), "payload\n")
	if err := os.Chmod(sourceDeep, 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sourceParent, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(sourceParent, 0o755)
		_ = os.Chmod(sourceDeep, 0o755)
	})

	snapshotRoot := t.TempDir()
	if err := copyGitOverlaySnapshot(sourceRoot, snapshotRoot, []string{"locked/deep/payload.txt"}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(snapshotRoot, "locked"), filepath.Join(snapshotRoot, "locked", "deep")} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o555 {
			t.Fatalf("snapshot parent %q mode=%#o want 0555", path, got)
		}
	}
	if got, err := os.ReadFile(filepath.Join(snapshotRoot, "locked", "deep", "payload.txt")); err != nil || string(got) != "payload\n" {
		t.Fatalf("snapshot payload=%q err=%v", got, err)
	}
	if err := os.Chmod(filepath.Join(snapshotRoot, "locked"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(snapshotRoot, "locked", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestGitOverlaySnapshotDefersSharedParentModesAndRestoresDeepestFirst(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory modes")
	}
	sourceRoot := t.TempDir()
	sourceParent := filepath.Join(sourceRoot, "locked")
	sourceDeep := filepath.Join(sourceParent, "deep")
	if err := os.MkdirAll(sourceDeep, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteTestFile(t, filepath.Join(sourceParent, "first.txt"), "first\n")
	mustWriteTestFile(t, filepath.Join(sourceDeep, "second.txt"), "second\n")
	if err := os.Chmod(sourceDeep, 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sourceParent, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(sourceParent, 0o755)
		_ = os.Chmod(sourceDeep, 0o755)
	})

	snapshotRoot := t.TempDir()
	var observed [][2]os.FileMode
	err := copyGitOverlaySnapshotWithHook(
		sourceRoot,
		snapshotRoot,
		[]string{"locked/first.txt", "locked/deep/second.txt"},
		1,
		func(phase string, _ int, _ string) {
			if phase != "before_parent_mode_restore" {
				return
			}
			parentInfo, parentErr := os.Stat(filepath.Join(snapshotRoot, "locked"))
			deepInfo, deepErr := os.Stat(filepath.Join(snapshotRoot, "locked", "deep"))
			if parentErr != nil || deepErr != nil {
				t.Fatalf("stat deferred parents: parent=%v deep=%v", parentErr, deepErr)
			}
			observed = append(observed, [2]os.FileMode{parentInfo.Mode().Perm(), deepInfo.Mode().Perm()})
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	tempMode := gitOverlayTemporaryDirectoryMode(0o555).Perm()
	wantObserved := [][2]os.FileMode{{tempMode, tempMode}, {tempMode, 0o555}}
	if !slices.Equal(observed, wantObserved) {
		t.Fatalf("restore observations=%#v want %#v", observed, wantObserved)
	}
	for _, path := range []string{filepath.Join(snapshotRoot, "locked"), filepath.Join(snapshotRoot, "locked", "deep")} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o555 {
			t.Fatalf("snapshot parent %q mode=%#o want 0555", path, got)
		}
	}
	if err := os.Chmod(filepath.Join(snapshotRoot, "locked"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(snapshotRoot, "locked", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestGitOverlaySnapshotPreservesSymlinkModificationTime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows symlink runtime coverage is platform-specific")
	}
	sourceRoot := t.TempDir()
	snapshotRoot := t.TempDir()
	source := filepath.Join(sourceRoot, "link")
	if err := os.Symlink("target", source); err != nil {
		t.Fatal(err)
	}
	wantTime := normalizedGitOverlayFileTime(time.Unix(1_650_000_000, 987_654_000))
	if err := syncGitOverlaySymlinkTimes(source, wantTime); err != nil {
		t.Fatal(err)
	}
	sourceInfo, err := os.Lstat(source)
	if err != nil {
		t.Fatal(err)
	}
	if !sourceInfo.ModTime().Equal(wantTime) {
		t.Fatalf("source symlink mtime=%s want %s", sourceInfo.ModTime(), wantTime)
	}
	if err := copyGitOverlaySnapshot(sourceRoot, snapshotRoot, []string{"link"}); err != nil {
		t.Fatal(err)
	}
	copiedInfo, err := os.Lstat(filepath.Join(snapshotRoot, "link"))
	if err != nil {
		t.Fatal(err)
	}
	if !copiedInfo.ModTime().Equal(wantTime) {
		t.Fatalf("snapshot symlink mtime=%s want %s", copiedInfo.ModTime(), wantTime)
	}
}

func TestGitOverlayRsyncFilesFromDoesNotApplySourceRootMetadata(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX rsync integration")
	}
	rsyncPath, err := exec.LookPath("rsync")
	if err != nil {
		t.Skipf("rsync unavailable: %v", err)
	}
	root := t.TempDir()
	sourceRoot := filepath.Join(root, "source")
	destinationRoot := filepath.Join(root, "destination")
	if err := os.Mkdir(sourceRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(destinationRoot, 0o751); err != nil {
		t.Fatal(err)
	}
	mustWriteTestFile(t, filepath.Join(sourceRoot, "payload"), "payload\n")
	sourceTime := time.Unix(1_500_000_000, 0)
	destinationTime := time.Unix(1_600_000_000, 0)
	if err := os.Chtimes(sourceRoot, sourceTime, sourceTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(destinationRoot, destinationTime, destinationTime); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(rsyncPath, "-a", "--from0", "--files-from=-", sourceRoot+string(os.PathSeparator), destinationRoot+string(os.PathSeparator))
	command.Stdin = bytes.NewReader([]byte("payload\x00"))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("rsync explicit file list: %v\n%s", err, output)
	}
	destinationInfo, err := os.Stat(destinationRoot)
	if err != nil {
		t.Fatal(err)
	}
	if destinationInfo.Mode().Perm() != 0o751 {
		t.Fatalf("destination root mode=%#o want 0751", destinationInfo.Mode().Perm())
	}
	if destinationInfo.ModTime().Equal(sourceTime) {
		t.Fatalf("destination root inherited source mtime %s", sourceTime)
	}
}

func TestGitOverlaySnapshotRejectsSourceParentSymlinkDriftBeforeModeRestore(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlinks")
	}
	sourceRoot := t.TempDir()
	sourceParent := filepath.Join(sourceRoot, "private")
	if err := os.Mkdir(sourceParent, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteTestFile(t, filepath.Join(sourceParent, "payload.txt"), "payload\n")
	replacement := t.TempDir()
	snapshotRoot := t.TempDir()
	mutated := false
	err := copyGitOverlaySnapshotWithHook(sourceRoot, snapshotRoot, []string{"private/payload.txt"}, 1, func(phase string, _ int, _ string) {
		if phase != "before_parent_mode_restore" || mutated {
			return
		}
		mutated = true
		if err := os.Rename(sourceParent, sourceParent+".old"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(replacement, sourceParent); err != nil {
			t.Fatal(err)
		}
	})
	if err == nil || !errors.Is(err, errGitOverlaySnapshotDrift) {
		t.Fatalf("source parent symlink drift was not rejected: %v", err)
	}
	info, statErr := os.Stat(filepath.Join(snapshotRoot, "private"))
	if statErr != nil {
		t.Fatal(statErr)
	}
	if got := info.Mode().Perm(); got&0o700 != 0o700 {
		t.Fatalf("failed snapshot parent mode=%#o want owner access", got)
	}
	if removeErr := os.RemoveAll(snapshotRoot); removeErr != nil {
		t.Fatalf("remove thawed snapshot: %v", removeErr)
	}
}

func TestGitOverlaySnapshotDestinationSymlinkDoesNotChmodTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlinks and directory modes")
	}
	sourceRoot := t.TempDir()
	sourceParent := filepath.Join(sourceRoot, "private")
	if err := os.Mkdir(sourceParent, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteTestFile(t, filepath.Join(sourceParent, "payload.txt"), "payload\n")
	if err := os.Chmod(sourceParent, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sourceParent, 0o755) })
	snapshotRoot := t.TempDir()
	outside := t.TempDir()
	if err := os.Chmod(outside, 0o511); err != nil {
		t.Fatal(err)
	}
	outsideModTime := time.Unix(1_600_000_000, 0)
	if err := os.Chtimes(outside, outsideModTime, outsideModTime); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(outside, 0o755) })
	mutated := false
	err := copyGitOverlaySnapshotWithHook(sourceRoot, snapshotRoot, []string{"private/payload.txt"}, 1, func(phase string, _ int, _ string) {
		if phase != "before_parent_mode_restore" || mutated {
			return
		}
		mutated = true
		destination := filepath.Join(snapshotRoot, "private")
		if err := os.Rename(destination, destination+".retained"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, destination); err != nil {
			t.Fatal(err)
		}
	})
	if err == nil || !strings.Contains(err.Error(), "parent path changed") {
		t.Fatalf("destination symlink was not rejected: %v", err)
	}
	outsideInfo, statErr := os.Stat(outside)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if got := outsideInfo.Mode().Perm(); got != 0o511 {
		t.Fatalf("destination symlink target mode=%#o want 0511", got)
	}
	if !outsideInfo.ModTime().Equal(outsideModTime) {
		t.Fatalf("destination symlink target mtime=%s want %s", outsideInfo.ModTime(), outsideModTime)
	}
	retainedInfo, statErr := os.Stat(filepath.Join(snapshotRoot, "private.retained"))
	if statErr != nil {
		t.Fatal(statErr)
	}
	if got := retainedInfo.Mode().Perm(); got != 0o755 {
		t.Fatalf("retained destination mode=%#o want thawed 0755", got)
	}
	if removeErr := os.RemoveAll(snapshotRoot); removeErr != nil {
		t.Fatalf("remove thawed snapshot: %v", removeErr)
	}
}

func TestGitOverlaySnapshotPartialRestoreFailureThawsParentsForCleanup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory modes")
	}
	sourceRoot := t.TempDir()
	sourceParent := filepath.Join(sourceRoot, "locked")
	sourceDeep := filepath.Join(sourceParent, "deep")
	if err := os.MkdirAll(sourceDeep, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteTestFile(t, filepath.Join(sourceDeep, "payload.txt"), "payload\n")
	if err := os.Chmod(sourceDeep, 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sourceParent, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(sourceParent, 0o755)
		_ = os.Chmod(sourceDeep, 0o755)
	})

	snapshotRoot := t.TempDir()
	restoreCount := 0
	err := copyGitOverlaySnapshotWithHook(sourceRoot, snapshotRoot, []string{"locked/deep/payload.txt"}, 1, func(phase string, _ int, _ string) {
		if phase != "before_parent_mode_restore" {
			return
		}
		restoreCount++
		if restoreCount == 2 {
			if err := os.Chmod(sourceParent, 0o755); err != nil {
				t.Fatal(err)
			}
		}
	})
	if err == nil || !errors.Is(err, errGitOverlaySnapshotDrift) {
		t.Fatalf("partial restore source drift was not rejected: %v", err)
	}
	if restoreCount != 2 {
		t.Fatalf("restore hooks=%d want 2", restoreCount)
	}
	for _, path := range []string{filepath.Join(snapshotRoot, "locked"), filepath.Join(snapshotRoot, "locked", "deep")} {
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if got := info.Mode().Perm(); got != 0o755 {
			t.Fatalf("thawed parent %q mode=%#o want 0755", path, got)
		}
	}
	if removeErr := os.RemoveAll(snapshotRoot); removeErr != nil {
		t.Fatalf("remove thawed partial snapshot: %v", removeErr)
	}
}

func TestGitOverlaySnapshotCopyFailureRetainsOwnershipWhenThawFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory modes")
	}
	sourceRoot := t.TempDir()
	sourceParent := filepath.Join(sourceRoot, "locked")
	sourceDeep := filepath.Join(sourceParent, "deep")
	if err := os.MkdirAll(sourceDeep, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteTestFile(t, filepath.Join(sourceDeep, "first.txt"), "first\n")
	mustWriteTestFile(t, filepath.Join(sourceRoot, "second.txt"), "second\n")
	if err := os.Chmod(sourceDeep, 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sourceParent, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(sourceParent, 0o755)
		_ = os.Chmod(sourceDeep, 0o755)
	})

	snapshot, err := newGitOverlaySnapshot()
	if err != nil {
		t.Fatal(err)
	}
	root := snapshot.Root
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	copyFailurePath := filepath.Join(root, "second.txt")
	readOnlyPath := filepath.Join(root, "locked", "deep")
	thawFailure := errors.New("synthetic thaw failure")
	thawCalls := 0
	var transferred *gitOverlaySnapshotParents
	err = copyGitOverlaySnapshotContentsWithThaw(
		sourceRoot,
		root,
		[]string{"locked/deep/first.txt", "second.txt"},
		1,
		func(phase string, _ int, _ string) {
			if phase != "after_lstat" || thawCalls != 0 {
				return
			}
			if _, statErr := os.Stat(filepath.Join(root, "locked", "deep", "first.txt")); statErr != nil {
				return
			}
			mustWriteTestFile(t, copyFailurePath, "obstruction\n")
			if chmodErr := os.Chmod(readOnlyPath, 0o555); chmodErr != nil {
				t.Fatal(chmodErr)
			}
		},
		snapshot.cleanupRoot,
		func(parents *gitOverlaySnapshotParents) error {
			thawCalls++
			transferred = parents
			return thawFailure
		},
	)
	if err == nil || !strings.Contains(err.Error(), "file exists") || !errors.Is(err, thawFailure) {
		t.Fatalf("copy/thaw error=%v", err)
	}
	if copyAt, thawAt := strings.Index(err.Error(), "file exists"), strings.Index(err.Error(), thawFailure.Error()); copyAt < 0 || thawAt < 0 || copyAt >= thawAt {
		t.Fatalf("copy/thaw error order=%q", err)
	}
	if thawCalls != 1 {
		t.Fatalf("thaw calls=%d want 1", thawCalls)
	}
	if transferred == nil || len(transferred.ordered) != 0 || transferred.byPath != nil {
		t.Fatalf("failed thaw retained duplicate ownership: %+v", transferred)
	}
	owner := snapshot.cleanupRoot
	owned := append([]*gitOverlaySnapshotParent(nil), owner.directories...)
	if len(owned) != 2 {
		t.Fatalf("owned snapshot parents=%d want 2", len(owned))
	}
	for _, parent := range owned {
		if parent.handle == nil {
			t.Fatalf("adopted parent %q lost its cleanup handle", parent.relative)
		}
	}
	info, statErr := os.Stat(readOnlyPath)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Mode().Perm() != 0o555 {
		t.Fatalf("partial read-only parent mode=%#o want 0555", info.Mode().Perm())
	}

	if err := snapshot.cleanup(); err != nil {
		t.Fatalf("cleanup after retained thaw failure: %v", err)
	}
	if snapshot.Root != "" || snapshot.cleanupRoot != nil {
		t.Fatalf("cleanup retained snapshot state: root=%q capability=%v", snapshot.Root, snapshot.cleanupRoot)
	}
	if _, statErr := os.Stat(root); !os.IsNotExist(statErr) {
		t.Fatalf("snapshot root survived cleanup: %v", statErr)
	}
	for _, parent := range owned {
		if parent.handle != nil {
			t.Fatalf("cleanup retained parent handle for %q", parent.relative)
		}
	}
	transferred.close()
	transferred.close()
	owner.close()
	owner.close()
}

func TestGitOverlaySnapshotParentIdentityRejectsReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory replacement")
	}
	sourceRoot := t.TempDir()
	sourceParent := filepath.Join(sourceRoot, "private")
	if err := os.Mkdir(sourceParent, 0o700); err != nil {
		t.Fatal(err)
	}
	snapshotRoot := t.TempDir()
	parents, err := newGitOverlaySnapshotParents(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer parents.close()
	observed, err := os.Lstat(sourceParent)
	if err != nil {
		t.Fatal(err)
	}
	identities, err := parents.ensure(snapshotRoot, "private", sourceParent, observed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(sourceParent, sourceParent+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(sourceParent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := verifyGitOverlaySnapshotParents(identities); err == nil || !errors.Is(err, errGitOverlaySnapshotDrift) || !strings.Contains(err.Error(), "changed during copy") {
		t.Fatalf("parent replacement was not rejected: %v", err)
	}
}

func TestGitOverlaySnapshotDriftLimitCleansEveryAttempt(t *testing.T) {
	fixture := newGitOverlayFixture(t)
	mustWriteTestFile(t, filepath.Join(fixture.root, "unstaged.txt"), "initial\n")
	_, excludes := fixture.manifest(t)
	var snapshotRoots []string
	_, err := prepareGitOverlaySnapshotWithHook(fixture.repo, fixture.cfg, excludes, nil, fixture.plan, func(phase string, attempt int, snapshotRoot string) {
		switch phase {
		case "snapshot_created":
			snapshotRoots = append(snapshotRoots, snapshotRoot)
		case "snapshot_fingerprinted":
			mustWriteTestFile(t, filepath.Join(fixture.root, "unstaged.txt"), fmt.Sprintf("changed-%d\n", attempt))
		}
	})
	if err == nil || !errors.Is(err, errGitOverlaySnapshotDrift) || !strings.Contains(err.Error(), "changed during snapshot creation") {
		t.Fatalf("snapshot drift error=%v", err)
	}
	if len(snapshotRoots) != gitOverlaySnapshotMaxAttempts {
		t.Fatalf("snapshot attempts=%d want %d", len(snapshotRoots), gitOverlaySnapshotMaxAttempts)
	}
	for _, snapshotRoot := range snapshotRoots {
		if _, statErr := os.Stat(snapshotRoot); !os.IsNotExist(statErr) {
			t.Fatalf("discarded snapshot %q still exists: %v", snapshotRoot, statErr)
		}
	}
}

func TestGitOverlaySnapshotCleanupRetriesThenClearsRoot(t *testing.T) {
	snapshot := gitOverlaySnapshot{Root: "/tmp/overlay-snapshot"}
	var (
		attempts int
		sleeps   []time.Duration
	)
	err := snapshot.cleanupWith(func(root string) error {
		attempts++
		if root != "/tmp/overlay-snapshot" {
			t.Fatalf("cleanup root=%q", root)
		}
		if attempts < gitOverlayCleanupMaxAttempts {
			return fmt.Errorf("transient cleanup failure %d", attempts)
		}
		return nil
	}, func(delay time.Duration) {
		sleeps = append(sleeps, delay)
	})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != gitOverlayCleanupMaxAttempts {
		t.Fatalf("cleanup attempts=%d want %d", attempts, gitOverlayCleanupMaxAttempts)
	}
	if !slices.Equal(sleeps, []time.Duration{gitOverlayCleanupRetryDelay, gitOverlayCleanupRetryDelay}) {
		t.Fatalf("cleanup sleeps=%v", sleeps)
	}
	if snapshot.Root != "" {
		t.Fatalf("cleanup retained root=%q", snapshot.Root)
	}
}

func TestGitOverlaySnapshotCleanupExhaustionRetainsRootForRetry(t *testing.T) {
	snapshot := gitOverlaySnapshot{Root: "/tmp/overlay-snapshot"}
	attempts := 0
	err := snapshot.cleanupWith(func(string) error {
		attempts++
		return errors.New("persistent cleanup failure")
	}, func(time.Duration) {})
	if err == nil || !strings.Contains(err.Error(), "after 3 attempts") {
		t.Fatalf("cleanup error=%v", err)
	}
	if attempts != gitOverlayCleanupMaxAttempts {
		t.Fatalf("cleanup attempts=%d want %d", attempts, gitOverlayCleanupMaxAttempts)
	}
	if snapshot.Root != "/tmp/overlay-snapshot" {
		t.Fatalf("failed cleanup root=%q", snapshot.Root)
	}
	if err := snapshot.cleanupWith(func(string) error {
		attempts++
		return nil
	}, func(time.Duration) {}); err != nil {
		t.Fatal(err)
	}
	if attempts != gitOverlayCleanupMaxAttempts+1 {
		t.Fatalf("cleanup attempts after retry=%d", attempts)
	}
	if snapshot.Root != "" {
		t.Fatalf("successful retry retained root=%q", snapshot.Root)
	}
}

func TestGitOverlaySnapshotCleanupIsConfinedAndThawsReadOnlyParents(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlink and special-mode integration")
	}
	fixture := newGitOverlayFixture(t)
	parent := filepath.Join(fixture.root, "private")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	mustWriteTestFile(t, filepath.Join(parent, "payload.txt"), "payload\n")
	if err := os.Chmod(parent, 0o1555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "sentinel")
	mustWriteTestFile(t, sentinel, "outside survives\n")
	if err := os.Symlink(outside, filepath.Join(fixture.root, "outside-link")); err != nil {
		t.Fatal(err)
	}
	_, excludes := fixture.manifest(t)
	snapshot, err := prepareGitOverlaySnapshot(fixture.repo, fixture.cfg, excludes, nil, fixture.plan)
	if err != nil {
		t.Fatal(err)
	}
	root := snapshot.Root
	if err := snapshot.cleanup(); err != nil {
		t.Fatal(err)
	}
	if snapshot.Root != "" || snapshot.cleanupRoot != nil {
		t.Fatalf("successful cleanup retained snapshot state: root=%q capability=%v", snapshot.Root, snapshot.cleanupRoot)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("snapshot root survived cleanup: %v", err)
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "outside survives\n" {
		t.Fatalf("cleanup escaped snapshot root: data=%q err=%v", got, err)
	}
}

func TestGitOverlaySnapshotCleanupRejectsRootReplacementAndRetriesOriginal(t *testing.T) {
	snapshot, err := newGitOverlaySnapshot()
	if err != nil {
		t.Fatal(err)
	}
	original := snapshot.Root
	retained := original + ".retained"
	if err := os.Rename(original, retained); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(original, 0o700); err != nil {
		t.Fatal(err)
	}
	replacementSentinel := filepath.Join(original, "replacement")
	mustWriteTestFile(t, replacementSentinel, "replacement survives\n")

	err = snapshot.cleanupWith(func(string) error {
		return snapshot.cleanupRoot.remove()
	}, func(time.Duration) {})
	if err == nil || !strings.Contains(err.Error(), "root identity changed") {
		t.Fatalf("root replacement cleanup error=%v", err)
	}
	if snapshot.Root != original || snapshot.cleanupRoot == nil {
		t.Fatalf("failed cleanup discarded retry state: root=%q capability=%v", snapshot.Root, snapshot.cleanupRoot)
	}
	if got, err := os.ReadFile(replacementSentinel); err != nil || string(got) != "replacement survives\n" {
		t.Fatalf("replacement root was touched: data=%q err=%v", got, err)
	}
	if err := os.RemoveAll(original); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(retained, original); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.cleanup(); err != nil {
		t.Fatal(err)
	}
	if snapshot.Root != "" || snapshot.cleanupRoot != nil {
		t.Fatalf("retry retained cleanup state: root=%q capability=%v", snapshot.Root, snapshot.cleanupRoot)
	}
}

func TestGitOverlaySnapshotConstructionErrorPrecedesCleanupExhaustion(t *testing.T) {
	fixture := newGitOverlayFixture(t)
	mustWriteTestFile(t, filepath.Join(fixture.root, "unstaged.txt"), "local change\n")
	_, excludes := fixture.manifest(t)
	cleanupFailure := errors.New("cleanup exhausted")
	var snapshotRoot string
	snapshot, err := prepareGitOverlaySnapshotWithCleanup(
		fixture.repo,
		fixture.cfg,
		excludes,
		nil,
		fixture.plan,
		func(phase string, _ int, root string) {
			if phase != "snapshot_created" {
				return
			}
			snapshotRoot = root
			if removeErr := os.Remove(filepath.Join(fixture.root, "unstaged.txt")); removeErr != nil {
				t.Fatal(removeErr)
			}
		},
		func(snapshot *gitOverlaySnapshot) error {
			return snapshot.cleanupWith(func(string) error {
				return cleanupFailure
			}, func(time.Duration) {})
		},
	)
	if snapshotRoot != "" {
		t.Cleanup(func() { _ = os.RemoveAll(snapshotRoot) })
	}
	if err == nil {
		t.Fatal("snapshot construction unexpectedly succeeded")
	}
	if snapshot.Root == "" || snapshot.cleanupRoot == nil {
		t.Fatalf("snapshot construction discarded cleanup capability: root=%q capability=%v", snapshot.Root, snapshot.cleanupRoot)
	}
	root := snapshot.Root
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if !errors.Is(err, cleanupFailure) {
		t.Fatalf("snapshot error omitted cleanup failure: %v", err)
	}
	primary := `snapshot overlay path "unstaged.txt"`
	if primaryAt, cleanupAt := strings.Index(err.Error(), primary), strings.Index(err.Error(), cleanupFailure.Error()); primaryAt < 0 || cleanupAt < 0 || primaryAt >= cleanupAt {
		t.Fatalf("snapshot error order=%q", err)
	}
	if !strings.Contains(err.Error(), "after 3 attempts") {
		t.Fatalf("snapshot error omitted exhausted cleanup attempts: %v", err)
	}
	if err := snapshot.cleanup(); err != nil {
		t.Fatalf("retry retained snapshot cleanup: %v", err)
	}
	if snapshot.Root != "" || snapshot.cleanupRoot != nil {
		t.Fatalf("retry retained snapshot state: root=%q capability=%v", snapshot.Root, snapshot.cleanupRoot)
	}
}

func TestGitOverlaySnapshotDriftJoinsCleanupExhaustion(t *testing.T) {
	fixture := newGitOverlayFixture(t)
	mustWriteTestFile(t, filepath.Join(fixture.root, "unstaged.txt"), "initial\n")
	_, excludes := fixture.manifest(t)
	cleanupFailure := errors.New("cleanup exhausted")
	var snapshotRoot string
	snapshot, err := prepareGitOverlaySnapshotWithCleanup(
		fixture.repo,
		fixture.cfg,
		excludes,
		nil,
		fixture.plan,
		func(phase string, attempt int, root string) {
			if phase == "snapshot_created" {
				snapshotRoot = root
			}
			if phase == "snapshot_fingerprinted" {
				mustWriteTestFile(t, filepath.Join(fixture.root, "unstaged.txt"), fmt.Sprintf("changed-%d\n", attempt))
			}
		},
		func(snapshot *gitOverlaySnapshot) error {
			return snapshot.cleanupWith(func(string) error {
				return cleanupFailure
			}, func(time.Duration) {})
		},
	)
	if snapshotRoot != "" {
		t.Cleanup(func() { _ = os.RemoveAll(snapshotRoot) })
	}
	if err == nil || !strings.Contains(err.Error(), "local Git overlay changed during snapshot creation") {
		t.Fatalf("snapshot drift error=%v", err)
	}
	if snapshot.Root == "" || snapshot.cleanupRoot == nil {
		t.Fatalf("snapshot drift discarded cleanup capability: root=%q capability=%v", snapshot.Root, snapshot.cleanupRoot)
	}
	root := snapshot.Root
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if !errors.Is(err, cleanupFailure) || !strings.Contains(err.Error(), "after 3 attempts") {
		t.Fatalf("snapshot drift omitted exhausted cleanup failure: %v", err)
	}
	if driftAt, cleanupAt := strings.Index(err.Error(), "local Git overlay changed"), strings.Index(err.Error(), cleanupFailure.Error()); driftAt < 0 || cleanupAt < 0 || driftAt >= cleanupAt {
		t.Fatalf("snapshot drift error order=%q", err)
	}
	if err := snapshot.cleanup(); err != nil {
		t.Fatalf("retry retained drift snapshot cleanup: %v", err)
	}
	if snapshot.Root != "" || snapshot.cleanupRoot != nil {
		t.Fatalf("retry retained drift snapshot state: root=%q capability=%v", snapshot.Root, snapshot.cleanupRoot)
	}
}

func assertGitOverlaySnapshotFingerprint(t *testing.T, repo Repo, cfg Config, excludes SyncExcludeRules, plan gitCoherencePlan, snapshot gitOverlaySnapshot) {
	t.Helper()
	snapshotRepo := repo
	snapshotRepo.Root = snapshot.Root
	fingerprint, err := syncFingerprintForManifest(snapshotRepo, cfg, snapshot.Manifest, excludes, plan)
	if err != nil {
		t.Fatal(err)
	}
	if fingerprint != snapshot.Fingerprint {
		t.Fatalf("snapshot fingerprint=%q recomputed=%q", snapshot.Fingerprint, fingerprint)
	}
}

func TestFinalizeGitOverlaySnapshotCleanupPreservesPrimaryFailure(t *testing.T) {
	primary := exit(17, "primary transfer failure")
	cleanupErr := errors.New("cleanup failure")
	var runErr error = primary
	var runFailure error = primary

	finalizeGitOverlaySnapshotCleanup(&runErr, &runFailure, func() error {
		return cleanupErr
	})

	for name, got := range map[string]error{"return": runErr, "recorded": runFailure} {
		var exitErr ExitError
		if !AsExitError(got, &exitErr) || exitErr.Code != primary.Code {
			t.Fatalf("%s failure=%v exit=%+v", name, got, exitErr)
		}
		if !strings.Contains(got.Error(), cleanupErr.Error()) {
			t.Fatalf("%s failure omitted cleanup diagnostic: %v", name, got)
		}
		if !strings.HasPrefix(got.Error(), primary.Message) {
			t.Fatalf("%s failure did not keep primary error first: %q", name, got)
		}
	}
}

func TestFinalizeGitOverlaySnapshotCleanupOnlyReturnsExitSix(t *testing.T) {
	cleanupErr := errors.New("cleanup failure")
	var runErr error
	var runFailure error

	finalizeGitOverlaySnapshotCleanup(&runErr, &runFailure, func() error {
		return cleanupErr
	})

	for name, got := range map[string]error{"return": runErr, "recorded": runFailure} {
		var exitErr ExitError
		if !AsExitError(got, &exitErr) || exitErr.Code != 6 {
			t.Fatalf("%s failure=%v exit=%+v", name, got, exitErr)
		}
		if !strings.Contains(got.Error(), cleanupErr.Error()) {
			t.Fatalf("%s failure omitted cleanup diagnostic: %v", name, got)
		}
	}
}

func TestTerminalGitOverlaySnapshotCleanupRetriesRetainedSnapshot(t *testing.T) {
	snapshot, err := newGitOverlaySnapshot()
	if err != nil {
		t.Fatal(err)
	}
	root := snapshot.Root
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	cleanupFailure := errors.New("cleanup exhausted")
	if err := snapshot.cleanupWith(func(string) error {
		return cleanupFailure
	}, func(time.Duration) {}); !errors.Is(err, cleanupFailure) {
		t.Fatalf("initial cleanup error=%v", err)
	}
	if snapshot.Root == "" || snapshot.cleanupRoot == nil {
		t.Fatalf("initial cleanup discarded ownership: root=%q capability=%v", snapshot.Root, snapshot.cleanupRoot)
	}
	if err := terminalGitOverlaySnapshotCleanup(&snapshot, func(snapshot *gitOverlaySnapshot) error {
		return snapshot.cleanup()
	}); err != nil {
		t.Fatalf("terminal cleanup retry: %v", err)
	}
	if snapshot.Root != "" || snapshot.cleanupRoot != nil {
		t.Fatalf("terminal cleanup retained state: root=%q capability=%v", snapshot.Root, snapshot.cleanupRoot)
	}
}

func TestTerminalGitOverlaySnapshotCleanupSkipsClearedSnapshot(t *testing.T) {
	snapshot := gitOverlaySnapshot{}
	calls := 0
	if err := terminalGitOverlaySnapshotCleanup(&snapshot, func(*gitOverlaySnapshot) error {
		calls++
		return errors.New("unexpected cleanup")
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("cleared snapshot cleanup calls=%d", calls)
	}
}

func TestTerminalGitOverlaySnapshotCleanupClosesHandlesAndPreservesPrimaryFailure(t *testing.T) {
	snapshot, err := newGitOverlaySnapshot()
	if err != nil {
		t.Fatal(err)
	}
	root := snapshot.Root
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	primary := exit(17, "primary transfer failure")
	cleanupFailure := errors.New("cleanup exhausted")
	var runErr error = primary
	var runFailure error = primary

	finalizeGitOverlaySnapshotCleanup(&runErr, &runFailure, func() error {
		return terminalGitOverlaySnapshotCleanup(&snapshot, func(*gitOverlaySnapshot) error {
			return cleanupFailure
		})
	})

	if snapshot.cleanupRoot != nil {
		t.Fatal("terminal cleanup failure retained open cleanup handles")
	}
	for name, got := range map[string]error{"return": runErr, "recorded": runFailure} {
		var exitErr ExitError
		if !AsExitError(got, &exitErr) || exitErr.Code != primary.Code {
			t.Fatalf("%s failure=%v exit=%+v", name, got, exitErr)
		}
		if primaryAt, cleanupAt := strings.Index(got.Error(), primary.Message), strings.Index(got.Error(), cleanupFailure.Error()); primaryAt < 0 || cleanupAt < 0 || primaryAt >= cleanupAt {
			t.Fatalf("%s failure order=%q", name, got)
		}
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

func TestRemotePrepareGitOverlayValidHintReconcilesWithoutNetwork(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX overlay integration")
	}
	fixture := newGitOverlayFixture(t)
	workdir := filepath.Join(t.TempDir(), "workspace")
	if output, err := exec.Command("git", "clone", "--quiet", fixture.origin, workdir).CombinedOutput(); err != nil {
		t.Fatalf("clone workspace: %v\n%s", err, output)
	}
	meta := filepath.Join(workdir, ".git", "crabbox")
	mustWriteTestFile(t, filepath.Join(meta, "sync-finalize-token"), "token")
	mustWriteTestFile(t, filepath.Join(meta, "sync-finalize-complete-token"), "token")
	mustWriteTestFile(t, filepath.Join(meta, "sync-fingerprint"), "fixed-boundary-fingerprint")
	mustWriteTestFile(t, filepath.Join(meta, "git-hydrate-base"), "main "+fixture.repo.Head+"\n")
	mustWriteTestFile(t, filepath.Join(workdir, "clean.txt"), "remote working tree drift\n")
	mustWriteTestFile(t, filepath.Join(workdir, "untracked.txt"), "remove me\n")

	attack := t.TempDir()
	marker := filepath.Join(attack, "ambient-command-ran")
	helper := filepath.Join(attack, "credential-helper")
	mustWriteTestFile(t, filepath.Join(attack, ".bash_profile"), "printf profile >"+shellQuote(marker)+"\n")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf helper >"+shellQuote(marker)+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "config", "--file", filepath.Join(attack, ".gitconfig"), "credential.helper", helper).CombinedOutput(); err != nil {
		t.Fatalf("configure hostile helper: %v\n%s", err, output)
	}
	if err := os.WriteFile(filepath.Join(attack, "git"), []byte("#!/bin/sh\nprintf path >"+shellQuote(marker)+"\nexit 99\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(workdir, ".git", "hooks", "reference-transaction")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nprintf hook >"+shellQuote(marker)+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(fixture.origin); err != nil {
		t.Fatal(err)
	}

	output, err := runOverlayCommand(
		t,
		remotePrepareGitOverlayWithHint(workdir, fixture.plan, "main", fixture.repo.Head, "fixed-boundary-fingerprint"),
		nil,
		"HOME="+attack,
		"PATH="+attack,
	)
	if err != nil {
		t.Fatalf("networkless overlay reconciliation: output=%q err=%v", output, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("networkless overlay reconciliation executed ambient profile, PATH, helper, or hook: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(workdir, "clean.txt")); err != nil || string(got) != "base clean.txt\n" {
		t.Fatalf("working tree drift was not repaired: data=%q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(workdir, "untracked.txt")); !os.IsNotExist(err) {
		t.Fatalf("untracked drift survived reconciliation: %v", err)
	}
	if baseline, err := os.ReadFile(filepath.Join(meta, "sync-manifest")); err != nil || !bytes.Contains(baseline, []byte("clean.txt\x00")) {
		t.Fatalf("networkless reconciliation baseline=%q err=%v", baseline, err)
	}
	for _, name := range []string{"sync-fingerprint", "git-hydrate-base"} {
		if _, err := os.Stat(filepath.Join(meta, name)); !os.IsNotExist(err) {
			t.Fatalf("networkless reconciliation retained stale %s: %v", name, err)
		}
	}
}

func TestRemotePrepareGitOverlayInvalidHintUsesNetworkAndRepairsGitState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX overlay integration")
	}
	for _, mismatch := range []string{"head", "index"} {
		t.Run(mismatch, func(t *testing.T) {
			fixture := newGitOverlayFixture(t)
			workdir := filepath.Join(t.TempDir(), "workspace")
			if output, err := exec.Command("git", "clone", "--quiet", fixture.origin, workdir).CombinedOutput(); err != nil {
				t.Fatalf("clone workspace: %v\n%s", err, output)
			}
			meta := filepath.Join(workdir, ".git", "crabbox")
			mustWriteTestFile(t, filepath.Join(meta, "sync-finalize-token"), "token")
			mustWriteTestFile(t, filepath.Join(meta, "sync-finalize-complete-token"), "token")
			mustWriteTestFile(t, filepath.Join(meta, "sync-fingerprint"), "stale-fingerprint")
			mustWriteTestFile(t, filepath.Join(meta, "git-hydrate-base"), "main "+fixture.repo.Head+"\n")
			switch mismatch {
			case "head":
				runGit(t, workdir, "checkout", "--quiet", "--detach", "HEAD^")
			case "index":
				mustWriteTestFile(t, filepath.Join(workdir, "clean.txt"), "staged remote drift\n")
				runGit(t, workdir, "add", "clean.txt")
			}
			output, err := runOverlayCommand(
				t,
				remotePrepareGitOverlayWithHint(workdir, fixture.plan, "main", fixture.repo.Head, "expected-fingerprint"),
				nil,
			)
			if err != nil {
				t.Fatalf("repair invalid hint: %v\n%s", err, output)
			}
			if got := gitOutput(workdir, "rev-parse", "HEAD"); got != fixture.repo.Head {
				t.Fatalf("%s mismatch HEAD=%q want %q", mismatch, got, fixture.repo.Head)
			}
			if got := gitOutput(workdir, "write-tree"); got != fixture.plan.Tree {
				t.Fatalf("%s mismatch index=%q want %q", mismatch, got, fixture.plan.Tree)
			}
		})
	}
}

func TestRemotePrepareGitOverlayStaleHintAttemptsNetwork(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX overlay integration")
	}
	fixture := newGitOverlayFixture(t)
	workdir := filepath.Join(t.TempDir(), "workspace")
	if output, err := exec.Command("git", "clone", "--quiet", fixture.origin, workdir).CombinedOutput(); err != nil {
		t.Fatalf("clone workspace: %v\n%s", err, output)
	}
	meta := filepath.Join(workdir, ".git", "crabbox")
	mustWriteTestFile(t, filepath.Join(meta, "sync-finalize-token"), "token")
	mustWriteTestFile(t, filepath.Join(meta, "sync-finalize-complete-token"), "token")
	mustWriteTestFile(t, filepath.Join(meta, "sync-fingerprint"), "stale-fingerprint")
	mustWriteTestFile(t, filepath.Join(meta, "git-hydrate-base"), "main "+fixture.repo.Head+"\n")
	if err := os.RemoveAll(fixture.origin); err != nil {
		t.Fatal(err)
	}
	output, err := runOverlayCommand(
		t,
		remotePrepareGitOverlayWithHint(workdir, fixture.plan, "main", fixture.repo.Head, "current-fingerprint"),
		nil,
	)
	reason, fallback, mutated := gitOverlayFallbackOutcome(string(output), err)
	if !fallback || mutated || reason != "origin_unavailable" {
		t.Fatalf("stale hint did not enter networkful path: reason=%q fallback=%t mutated=%t err=%v output=%q", reason, fallback, mutated, err, output)
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

func TestRemoteDiscardGitOverlayPendingMetadataIsHermetic(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX overlay cleanup integration")
	}
	fixture := newGitOverlayFixture(t)
	workdir := filepath.Join(t.TempDir(), "workspace")
	if output, err := runOverlayCommand(t, remotePrepareGitOverlay(workdir, fixture.plan), nil); err != nil {
		t.Fatalf("prepare overlay: %v\n%s", err, output)
	}
	const token = "abcdef0123456789abcdef0123456789"
	meta := filepath.Join(workdir, ".git", "crabbox")
	pending := []string{
		filepath.Join(meta, remoteSyncPendingManifestName(token)),
		filepath.Join(meta, remoteSyncPendingDeletedName(token)),
	}
	for _, path := range pending {
		mustWriteTestFile(t, path, "pending\n")
	}
	attack := t.TempDir()
	marker := filepath.Join(attack, "ambient-command-ran")
	for _, name := range []string{"rm", "git"} {
		if err := os.WriteFile(filepath.Join(attack, name), []byte("#!/bin/sh\nprintf fired >"+shellQuote(marker)+"\nexit 99\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if output, err := runOverlayCommand(
		t,
		remoteDiscardGitOverlaySyncPendingMetadata(workdir, token),
		nil,
		"HOME="+attack,
		"PATH="+attack,
		"GIT_CONFIG_GLOBAL="+filepath.Join(attack, "gitconfig"),
	); err != nil {
		t.Fatalf("discard overlay pending metadata: %v\n%s", err, output)
	}
	for _, path := range pending {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("pending metadata survived at %q: %v", path, err)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("overlay cleanup executed ambient command: %v", err)
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
	fmt.Fprintf(want, "v6\nremote=%s\nbranch=%s\nhead=%s\ntree=%s\n", fixture.plan.RemoteURL, fixture.plan.Branch, fixture.plan.Target, fixture.plan.Tree)
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
		"success", "file-origin", "runtime-state", "classified-fallback", "snapshot-prepare-fallback", "private-origin", "origin-unavailable", "missing-origin", "local-ineligible", "local-fingerprint-reuse", "remote-fingerprint-reuse", "unsafe-metadata",
		"local-hidden-fingerprint", "local-sparse-hidden-fingerprint", "local-config-hidden-fingerprint", "remote-hidden-fingerprint", "remote-skip-hidden-fingerprint", "remote-inspection-hidden-fingerprint",
		"ordinary-private-fresh", "ordinary-private-reused", "ordinary-unavailable-fresh", "ordinary-unavailable-reused", "ordinary-server-failure-reused",
		"symlinked-workdir", "symlinked-git", "symlinked-git-config", "symlinked-git-objects", "linked-worktree",
		"checkout-file-obstruction", "post-reset-cache", "post-reset-mass-deletion", "late-local-edit", "workload-cleanup", "rsync-failure", "rsync-cleanup-failure", "empty-overlay-finalize",
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
			if mode == "late-local-edit" || mode == "workload-cleanup" || mode == "rsync-failure" || mode == "rsync-cleanup-failure" {
				mustWriteTestFile(t, filepath.Join(fixture.root, "unstaged.txt"), "snapshot local change\n")
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
			rsyncSourceLog := filepath.Join(testRoot, "rsync-source")
			snapshotParent := filepath.Join(testRoot, "snapshots")
			if err := os.Mkdir(snapshotParent, 0o755); err != nil {
				t.Fatal(err)
			}
			workloadProbe := filepath.Join(testRoot, "workload-probe")
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
					fingerprintConfig, err := loadConfig()
					if err != nil {
						t.Fatal(err)
					}
					fingerprintConfig.Sync.GitOverlay = mode == "remote-fingerprint-reuse"
					excludes, err := syncExcludes(repo.Root, fingerprintConfig)
					if err != nil {
						t.Fatal(err)
					}
					manifest, err := syncManifestFilteredRules(repo.Root, excludes, syncIncludes(fingerprintConfig))
					if err != nil {
						t.Fatal(err)
					}
					coherence, _ := syncGitCoherencePlan(fingerprintConfig, repo)
					fingerprint, err := syncFingerprintForManifest(repo, fingerprintConfig, manifest, excludes, coherence)
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
					if mode == "remote-fingerprint-reuse" {
						mustWriteTestFile(t, filepath.Join(workdir, "clean.txt"), "remote working tree drift\n")
						if err := os.RemoveAll(fixture.origin); err != nil {
							t.Fatal(err)
						}
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
  *CRABBOX_SNAPSHOT_WORKLOAD_PROBE*)
    snapshot_root="$(cat "$CRABBOX_FAKE_RSYNC_SOURCE_LOG")"
    [ -n "$snapshot_root" ] && [ ! -e "$snapshot_root" ] || exit 86
    : > "$CRABBOX_FAKE_WORKLOAD_PROBE"
    exit 0
    ;;
	  *committed_token=*)
	    if [ "$CRABBOX_FAKE_OVERLAY_MODE" = empty-overlay-finalize ]; then
	      snapshot="$(/usr/bin/find "$TMPDIR" -maxdepth 1 -name 'crabbox-git-overlay-*' -print -quit)"
	      [ -z "$snapshot" ] || exit 87
	    fi
	    ;;
  *crabbox-ready*) exit 0 ;;
	  *".overlay-git."*)
	    case "$CRABBOX_FAKE_OVERLAY_MODE" in
	      classified-fallback) printf '%%scheckout_failed\n' %s >&2; exit %d ;;
	      remote-inspection-hidden-fingerprint) printf '%%sindex_inspection_failed\n' %s >&2; exit %d ;;
      checkout-file-obstruction)
        /bin/chmod 0555 "$CRABBOX_FAKE_OVERLAY_WORKDIR"
        /usr/bin/env HOME="$CRABBOX_FAKE_ATTACK_HOME" PATH="$CRABBOX_FAKE_ATTACK_BIN:/usr/bin:/bin" /bin/bash --noprofile --norc -c "$cmd"
        code=$?
        /bin/chmod 0755 "$CRABBOX_FAKE_OVERLAY_WORKDIR"
        exit "$code"
        ;;
      late-local-edit) printf 'late local change\n' > "$CRABBOX_FAKE_REPO_ROOT/clean.txt" ;;
	      empty-overlay-finalize)
	        snapshot="$(/usr/bin/find "$TMPDIR" -maxdepth 1 -name 'crabbox-git-overlay-*' -print -quit)"
	        [ -n "$snapshot" ] || exit 86
	        ;;
	    esac
	    ;;
esac
case "$CRABBOX_FAKE_OVERLAY_MODE" in
	  success|runtime-state|remote-fingerprint-reuse|checkout-file-obstruction|post-reset-cache|post-reset-mass-deletion|late-local-edit|workload-cleanup|rsync-failure|rsync-cleanup-failure|empty-overlay-finalize)
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
args=("$@")
source="${args[${#args[@]}-2]}"
source="${source%/}"
printf '%s\n' "$source" > "$CRABBOX_FAKE_RSYNC_SOURCE_LOG"
if [ "$CRABBOX_FAKE_OVERLAY_MODE" = rsync-cleanup-failure ]; then
  mv "$source" "$source.retained"
  mkdir "$source"
  printf 'replacement survives\n' > "$source/replacement-sentinel"
  exit 31
fi
if [ "$CRABBOX_FAKE_OVERLAY_MODE" = rsync-failure ]; then
  exit 31
fi
destination="${!#}"
workdir="${destination#*:}"
workdir="${workdir%%/}"
while IFS= read -r -d '' rel; do
  mkdir -p "$workdir/$(dirname "$rel")"
  cp -a "$source/$rel" "$workdir/$rel"
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
			t.Setenv("CRABBOX_FAKE_RSYNC_SOURCE_LOG", rsyncSourceLog)
			t.Setenv("CRABBOX_FAKE_REPO_ROOT", fixture.root)
			t.Setenv("CRABBOX_FAKE_OVERLAY_MODE", mode)
			t.Setenv("CRABBOX_FAKE_OVERLAY_WORKDIR", workdir)
			t.Setenv("CRABBOX_FAKE_ATTACK_HOME", attackHome)
			t.Setenv("CRABBOX_FAKE_ATTACK_BIN", attackBin)
			t.Setenv("CRABBOX_FAKE_WORKLOAD_PROBE", workloadProbe)
			t.Setenv("CRABBOX_FAKE_SSH_PORT", "22")
			t.Setenv("CRABBOX_FAKE_SSH_PROXY", "1")
			t.Setenv("TMPDIR", snapshotParent)
			snapshotGitLog := filepath.Join(testRoot, "snapshot-git.log")
			if mode == "snapshot-prepare-fallback" {
				previousGitExecutable := gitOverlayGitExecutable
				failingGitExecutable := filepath.Join(binDir, "snapshot-git")
				fallbackFile := filepath.Join(fixture.root, "snapshot-fallback.txt")
				counter := filepath.Join(testRoot, "snapshot-git.count")
				script := fmt.Sprintf(`#!/bin/sh
count=0
if [ -f %s ]; then count="$(cat %s)"; fi
count=$((count + 1))
printf '%%s\n' "$count" > %s
printf '%%s\n' "$*" >> %s
if [ "$count" -le 5 ]; then exec %s "$@"; fi
printf 'snapshot fallback live edit\n' > %s
exit 99
`,
					shellQuote(counter),
					shellQuote(counter),
					shellQuote(counter),
					shellQuote(snapshotGitLog),
					shellQuote(previousGitExecutable),
					shellQuote(fallbackFile),
				)
				if err := os.WriteFile(failingGitExecutable, []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
				gitOverlayGitExecutable = failingGitExecutable
				t.Cleanup(func() {
					gitOverlayGitExecutable = previousGitExecutable
				})
			}
			t.Cleanup(func() {
				matches, err := filepath.Glob(filepath.Join(snapshotParent, "crabbox-git-overlay-*"))
				if err != nil || len(matches) != 0 {
					t.Errorf("retained overlay snapshots=%v err=%v", matches, err)
				}
			})
			credentialMarker := ""
			if privateOrigin {
				credentialMarker = installGitOverlayCredentialCanary(t)
			}
			var stdout bytes.Buffer
			var stderr synchronizedBuffer
			app := App{Stdout: &stdout, Stderr: &stderr}
			runArgs := []string{"--provider", providerName, "--no-hydrate", "--sync-only"}
			if mode == "workload-cleanup" {
				runArgs = []string{"--provider", providerName, "--no-hydrate", "--", "CRABBOX_SNAPSHOT_WORKLOAD_PROBE"}
			}
			runErr := app.runCommand(context.Background(), runArgs)
			if credentialMarker != "" {
				if _, err := os.Stat(credentialMarker); !os.IsNotExist(err) {
					t.Fatalf("local SSH fixture consulted ambient Git credentials: %v", err)
				}
			}
			if mode == "rsync-failure" || mode == "rsync-cleanup-failure" {
				source, err := os.ReadFile(rsyncSourceLog)
				if err != nil {
					t.Fatal(err)
				}
				root := strings.TrimSpace(string(source))
				if filepath.Dir(root) != snapshotParent || !strings.HasPrefix(filepath.Base(root), "crabbox-git-overlay-") {
					t.Fatalf("rsync failure source=%q", source)
				}
				if mode == "rsync-cleanup-failure" {
					t.Cleanup(func() {
						for _, path := range []string{root, root + ".retained"} {
							if err := os.RemoveAll(path); err != nil {
								t.Errorf("remove fixture snapshot %q: %v", path, err)
							}
						}
					})
				}
				if runErr == nil || !strings.Contains(runErr.Error(), "rsync failed") {
					t.Fatalf("rsync failure not reported: err=%v stderr=%s", runErr, stderr.String())
				}
				meta := filepath.Join(workdir, ".git", "crabbox")
				for _, pattern := range []string{"sync-manifest.*.new", "sync-deleted.*.new"} {
					matches, err := filepath.Glob(filepath.Join(meta, pattern))
					if err != nil || len(matches) != 0 {
						t.Fatalf("pending metadata pattern=%q matches=%v err=%v", pattern, matches, err)
					}
				}
				if mode == "rsync-cleanup-failure" {
					if sentinel, err := os.ReadFile(filepath.Join(root, "replacement-sentinel")); err != nil || string(sentinel) != "replacement survives\n" {
						t.Fatalf("snapshot cleanup touched replacement: sentinel=%q err=%v", sentinel, err)
					}
					if retained, err := os.ReadFile(filepath.Join(root+".retained", "unstaged.txt")); err != nil || string(retained) != "snapshot local change\n" {
						t.Fatalf("retained snapshot payload=%q err=%v", retained, err)
					}
					var primary ExitError
					if !AsExitError(runErr, &primary) || primary.Code != 6 || !strings.HasPrefix(runErr.Error(), "rsync failed") {
						t.Fatalf("snapshot cleanup replaced primary rsync failure: err=%v exit=%+v", runErr, primary)
					}
					for _, diagnostic := range []string{"clean up immutable git overlay snapshot", "root identity changed", root} {
						if !strings.Contains(runErr.Error(), diagnostic) {
							t.Fatalf("returned error omitted snapshot cleanup diagnostic %q: %v", diagnostic, runErr)
						}
					}
				} else if _, err := os.Stat(root); !os.IsNotExist(err) {
					t.Fatalf("failed transfer retained snapshot %q: %v", root, err)
				}
				return
			}
			if mode == "workload-cleanup" {
				if _, err := os.Stat(workloadProbe); err != nil {
					t.Fatalf("workload did not observe a cleaned snapshot: %v\nstderr=%s", err, stderr.String())
				}
				source, err := os.ReadFile(rsyncSourceLog)
				if err != nil {
					t.Fatal(err)
				}
				if root := strings.TrimSpace(string(source)); root == "" || !strings.Contains(filepath.Base(root), "crabbox-git-overlay-") {
					t.Fatalf("workload rsync source=%q", source)
				} else if _, err := os.Stat(root); !os.IsNotExist(err) {
					t.Fatalf("snapshot survived until workload execution: %q: %v", root, err)
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
			case "success", "file-origin", "runtime-state", "local-fingerprint-reuse", "remote-fingerprint-reuse", "empty-overlay-finalize":
				if _, err := os.Stat(rsyncLog); !os.IsNotExist(err) {
					sshCommands, _ := os.ReadFile(sshLog)
					t.Fatalf("clean overlay/legacy fingerprint unexpectedly transferred a source payload: %v\nstderr=%s\nssh=%s", err, stderr.String(), sshCommands)
				}
				if mode == "local-fingerprint-reuse" && !strings.Contains(stderr.String(), "No changes detected, skipping sync") {
					t.Fatalf("unmodified fallback discarded the legacy fingerprint: %s", stderr.String())
				}
				if mode == "remote-fingerprint-reuse" {
					sshCommands, err := os.ReadFile(sshLog)
					if err != nil ||
						bytes.Contains(sshCommands, []byte("No changes detected, skipping sync")) ||
						!bytes.Contains(sshCommands, []byte("git checkout --quiet --force --detach")) ||
						!bytes.Contains(sshCommands, []byte("sync-manifest.")) ||
						!bytes.Contains(sshCommands, []byte("committed_token=")) {
						t.Fatalf("overlay reuse did not run a networkless reconciliation transaction: commands=%q err=%v", sshCommands, err)
					}
					if strings.Contains(stderr.String(), "No changes detected, skipping sync") {
						t.Fatalf("overlay reconciliation was reported as skipped: %s", stderr.String())
					}
					if !strings.Contains(stderr.String(), "sync_skipped=false") {
						t.Fatalf("overlay reconciliation emitted skipped telemetry: %s", stderr.String())
					}
					if string(content) != "base clean.txt\n" {
						t.Fatalf("overlay reconciliation did not repair remote drift: %q", content)
					}
				}
			case "classified-fallback", "snapshot-prepare-fallback", "private-origin", "origin-unavailable", "missing-origin", "local-ineligible", "linked-worktree", "checkout-file-obstruction", "post-reset-cache",
				"ordinary-private-fresh", "ordinary-private-reused", "ordinary-unavailable-fresh", "ordinary-unavailable-reused":
				transfer, err := os.ReadFile(rsyncLog)
				if err != nil || !bytes.Contains(transfer, []byte("clean.txt\x00")) {
					t.Fatalf("fallback omitted full manifest: data=%q err=%v", transfer, err)
				}
				wantReason := "checkout_failed"
				if mode == "snapshot-prepare-fallback" {
					wantReason = "invalid_head"
				} else if privateOrigin {
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
				if mode == "checkout-file-obstruction" || mode == "post-reset-cache" {
					assertGitOverlayRecoveryCoherent(t, workdir, repo)
				} else if mode != "snapshot-prepare-fallback" && !privateOrigin && !unavailableOrigin && mode != "missing-origin" {
					if _, err := os.Stat(filepath.Join(meta, "git-hydrate-base")); err != nil {
						t.Fatalf("unmodified fallback lost ordinary Git hydration metadata: %v", err)
					}
				}
				if mode == "snapshot-prepare-fallback" || privateOrigin || unavailableOrigin || mode == "missing-origin" {
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
				if mode == "classified-fallback" {
					source, err := os.ReadFile(rsyncSourceLog)
					if err != nil || strings.TrimSpace(string(source)) != repo.Root {
						t.Fatalf("fallback rsync source=%q want=%q err=%v", source, repo.Root, err)
					}
				}
				if mode == "snapshot-prepare-fallback" {
					transfer, err := os.ReadFile(rsyncLog)
					if err != nil || !bytes.Contains(transfer, []byte("snapshot-fallback.txt\x00")) {
						t.Fatalf("snapshot fallback did not rebuild live manifest: data=%q err=%v", transfer, err)
					}
					if content, err := os.ReadFile(filepath.Join(workdir, "snapshot-fallback.txt")); err != nil || string(content) != "snapshot fallback live edit\n" {
						t.Fatalf("snapshot fallback live file=%q err=%v", content, err)
					}
					source, err := os.ReadFile(rsyncSourceLog)
					if err != nil || strings.TrimSpace(string(source)) != repo.Root {
						t.Fatalf("snapshot fallback rsync source=%q want=%q err=%v", source, repo.Root, err)
					}
					invocations, err := os.ReadFile(snapshotGitLog)
					if err != nil {
						t.Fatal(err)
					}
					if got := len(bytes.Split(bytes.TrimSpace(invocations), []byte{'\n'})); got != 7 {
						t.Fatalf("snapshot Git invocations=%d want=7\n%s", got, invocations)
					}
					if !bytes.Contains(invocations, []byte("rev-parse --verify HEAD^{commit}")) {
						t.Fatalf("snapshot Git failure did not exercise checkout capture:\n%s", invocations)
					}
					sshCommands, err := os.ReadFile(sshLog)
					if err != nil {
						t.Fatal(err)
					}
					for _, forbidden := range []string{".overlay-git.", "git clone ", "git fetch ", "git ls-remote ", "refs/remotes/origin/"} {
						if bytes.Contains(sshCommands, []byte(forbidden)) {
							t.Fatalf("snapshot fallback ran forbidden Git path %q:\n%s", forbidden, sshCommands)
						}
					}
				}
			case "late-local-edit":
				transfer, err := os.ReadFile(rsyncLog)
				if err != nil || !bytes.Equal(transfer, []byte("unstaged.txt\x00")) {
					t.Fatalf("immutable overlay manifest=%q err=%v", transfer, err)
				}
				source, err := os.ReadFile(rsyncSourceLog)
				if err != nil {
					t.Fatal(err)
				}
				snapshotRoot := strings.TrimSpace(string(source))
				if snapshotRoot == "" || !strings.Contains(filepath.Base(snapshotRoot), "crabbox-git-overlay-") {
					t.Fatalf("overlay rsync source=%q", source)
				}
				if _, err := os.Stat(snapshotRoot); !os.IsNotExist(err) {
					t.Fatalf("overlay snapshot was not cleaned: %v", err)
				}
				if string(content) != "base clean.txt\n" {
					t.Fatalf("late edit leaked into current sync: %q", content)
				}
				if live, err := os.ReadFile(filepath.Join(fixture.root, "clean.txt")); err != nil || string(live) != "late local change\n" {
					t.Fatalf("late source edit=%q err=%v", live, err)
				}
				if remote, err := os.ReadFile(filepath.Join(workdir, "unstaged.txt")); err != nil || string(remote) != "snapshot local change\n" {
					t.Fatalf("snapshot payload=%q err=%v", remote, err)
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
