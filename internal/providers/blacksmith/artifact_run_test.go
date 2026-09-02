package blacksmith

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func requireBlacksmithArtifactShell(t *testing.T) {
	t.Helper()
	for _, command := range []string{"/bin/sh", "bash", "timeout"} {
		if _, err := exec.LookPath(command); err != nil {
			t.Skipf("synthetic Blacksmith shell fixtures require %s: %v", command, err)
		}
	}
}

func testWriteBlacksmithFile(t *testing.T, root, name, text string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func syntheticBlacksmithCommand(t *testing.T, req LocalCommandRequest) string {
	t.Helper()
	for _, arg := range req.Args {
		if strings.HasPrefix(arg, "/bin/sh -c ") {
			return arg
		}
	}
	t.Fatal("missing supervisor command")
	return ""
}

// Synthetic native transport only: executes the actual wrapper and generic
// artifact script locally. No native CLI, credentials, sync, or lease exists.
func runSyntheticBlacksmithCommand(t *testing.T, ctx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	t.Helper()
	if req.Dir == "" || req.Stdin != nil || !req.DisableOutputCapture || req.CancelGracePeriod <= 0 {
		t.Errorf("unbounded or changed native request: dir=%q stdin=%v capture=%t grace=%s", req.Dir, req.Stdin != nil, req.DisableOutputCapture, req.CancelGracePeriod)
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", syntheticBlacksmithCommand(t, req))
	cmd.Dir, cmd.Stdout, cmd.Stderr = req.Dir, req.Stdout, req.Stderr
	cmd.WaitDelay = time.Second
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "TMPDIR=" + t.TempDir()}
	cmd.Env = append(cmd.Env, req.Env...)
	err := cmd.Run()
	code := 0
	if err != nil {
		code = 1
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() > 0 {
			code = ee.ExitCode()
		}
	}
	return LocalCommandResult{ExitCode: code}, err
}

func readBlacksmithArchive(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := map[string]string{}
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		files[header.Name] = string(data)
	}
}

func TestBlacksmithArtifactRunShellAndTerminalExit(t *testing.T) {
	requireBlacksmithArtifactShell(t)
	for _, tt := range []struct {
		name, command, stdout, stderr string
		argv                          []string
		code                          int
	}{
		{name: "zero", command: "printf original > report; printf out; printf err >&2", stdout: "out", stderr: "err"},
		{name: "one", command: "printf original > report; exit 1", code: 1},
		{name: "explicit-exit", command: "printf original > report; exit 23; printf BAD", code: 23},
		{name: "exec", command: "printf original > report; exec bash -c 'exit 23'", code: 23},
		{name: "errexit", command: "set -e\nprintf original > report\nfalse\nprintf BAD", code: 1},
		{name: "trap-cd", command: "trap 'printf trapped' EXIT; printf original > report; mkdir child; cd child; printf wrong > report; exit 23", code: 23, stdout: "trapped"},
		{name: "argv", argv: []string{"printf", "%s", "a b", "'quoted'", "$HOME"}, stdout: "a b'quoted'$HOME"},
		{name: "env", argv: []string{"VALUE=a b", "bash", "-c", `printf '%s' "$VALUE"`}, stdout: "a b"},
		{name: "multiline", command: "printf 'line1\\nline2\\n'\nprintf original > report", stdout: "line1\nline2\n"},
		{name: "stdin", command: "read value; printf 'read=%s' $?; printf original > report", stdout: "read=1"},
		{name: "signal-like", command: "printf original > report; exit 137", code: 137},
	} {
		t.Run(tt.name, func(t *testing.T) {
			isolateBlacksmithOwnership(t)
			repo := t.TempDir()
			t.Chdir(repo)
			testWriteBlacksmithFile(t, repo, "report", "original")
			const id = "tbx_supervisor"
			testOwnedBlacksmithClaim(t, id, "jade-krill", repo)
			runs := 0
			runner := &blacksmithFuncRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
				runs++
				if req.Dir != repo {
					t.Error("native sync cwd changed")
				}
				return runSyntheticBlacksmithCommand(t, t.Context(), req)
			}}
			backend := newTestBlacksmithBackend(baseConfig(), runner)
			var stdout, stderr bytes.Buffer
			backend.rt.Stdout, backend.rt.Stderr = &stdout, &stderr
			command, shell := tt.argv, false
			if command == nil {
				command, shell = []string{tt.command}, true
			}
			result, err := backend.Run(t.Context(), RunRequest{ID: id, Repo: Repo{Root: repo}, Command: command, ShellMode: shell, ArtifactGlobs: []string{"report"}, TimingJSON: true})
			if result.ExitCode != tt.code || (err == nil) != (tt.code == 0) || runs != 1 || result.CommandText != blacksmithCommandString(command, shell) {
				t.Fatalf("code=%d err=%v runs=%d text=%q", result.ExitCode, err, runs, result.CommandText)
			}
			if stdout.String() != tt.stdout || !strings.Contains(stderr.String(), tt.stderr) {
				t.Fatalf("streams stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
			if strings.Contains(stdout.String()+stderr.String()+result.LogExcerpt, "__CRABBOX_ARTIFACT_") || strings.Contains(stdout.String()+stderr.String()+result.LogExcerpt, "\x1e") {
				t.Fatal("protocol leaked")
			}
			if tt.code >= 128 {
				if len(result.Artifacts) != 0 {
					t.Fatal("signal-like exit published")
				}
				return
			}
			if len(result.Artifacts) != 1 || readBlacksmithArchive(t, result.Artifacts[0].Path)["report"] != "original" {
				t.Fatalf("wrong archive: %+v", result.Artifacts)
			}
			info, err := os.Stat(result.Artifacts[0].Path)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("archive is not private")
			}
			if tt.code != 0 {
				bundles, _ := filepath.Glob(filepath.Join(repo, ".crabbox/captures/*.tar.gz"))
				if len(bundles) != 1 {
					t.Fatalf("failure bundles=%v", bundles)
				}
				for name, data := range readBlacksmithArchive(t, bundles[0]) {
					if strings.Contains(data, "\x1e") || strings.Contains(data, "__CRABBOX_ARTIFACT_") || strings.Contains(data, "H4sI") {
						t.Fatalf("payload leaked to %s", name)
					}
				}
			}
		})
	}
}

func TestBlacksmithArtifactRunCollectionFailurePreservesWorkload(t *testing.T) {
	requireBlacksmithArtifactShell(t)
	for _, kind := range []string{"required", "files", "compressed-bytes", "local-write"} {
		for _, code := range []int{0, 1, 23} {
			t.Run(fmt.Sprintf("%s/%d", kind, code), func(t *testing.T) {
				isolateBlacksmithOwnership(t)
				repo := t.TempDir()
				t.Chdir(repo)
				const id = "tbx_collectfailure"
				testOwnedBlacksmithClaim(t, id, "jade-krill", repo)
				req := RunRequest{ID: id, Repo: Repo{Root: repo}, Command: []string{fmt.Sprintf("exit %d", code)}, ArtifactGlobs: []string{"reports/**"}}
				switch kind {
				case "required":
					req.RequiredArtifactGlobs = []string{"reports/missing"}
				case "files":
					for i := 0; i <= core.DelegatedRunArtifactDefaultMaxFiles; i++ {
						testWriteBlacksmithFile(t, repo, fmt.Sprintf("reports/%d", i), "x")
					}
				case "compressed-bytes":
					data := make([]byte, core.DelegatedRunArtifactDefaultMaxBytes+1024)
					if _, err := rand.Read(data); err != nil {
						t.Fatal(err)
					}
					testWriteBlacksmithFile(t, repo, "reports/random", string(data))
				case "local-write":
					testWriteBlacksmithFile(t, repo, ".crabbox/runs", "not a directory")
				}
				runner := &blacksmithFuncRunner{fn: func(native LocalCommandRequest) (LocalCommandResult, error) {
					return runSyntheticBlacksmithCommand(t, t.Context(), native)
				}}
				backend := newTestBlacksmithBackend(baseConfig(), runner)
				result, err := backend.Run(t.Context(), req)
				want := code
				if code == 0 {
					want = 7
					if kind == "local-write" {
						want = 2
					}
				}
				var ee ExitError
				if !errors.As(err, &ee) || ee.Code != want || result.ExitCode != want || len(result.Artifacts) != 0 {
					t.Fatalf("result=%+v err=%v", result, err)
				}
			})
		}
	}
}

func testBlacksmithReceiptFrames(t *testing.T, command string, code int) (string, string, string) {
	t.Helper()
	match := regexp.MustCompile(`CRABBOX_BS_([0-9a-f]{64}):`).FindStringSubmatch(command)
	if len(match) != 2 {
		t.Fatal("no nonce")
	}
	prefix := "\x1eCRABBOX_BS_" + match[1] + ":"
	return prefix + "start\x1f", fmt.Sprintf("%sexit:%d\x1f", prefix, code), prefix + "end:0\x1f"
}

func TestBlacksmithArtifactReceiptAdversarial(t *testing.T) {
	archive := makeTarGz(t, map[string]string{"report": "synthetic"})
	payload := core.DelegatedRunArtifactBeginMarker + "\n" + base64.StdEncoding.EncodeToString(archive) + "\n" + core.DelegatedRunArtifactEndMarker + "\n"
	for _, kind := range []string{"valid", "missing-start", "missing-exit", "missing-end", "stale", "duplicate-start", "duplicate-exit", "duplicate-end", "order", "truncated", "malformed", "oversized-record", "partial-archive", "duplicate-archive", "native-before", "native-after", "nil-error-nonzero", "zero-code-error", "cancel", "overflow", "diagnostics-overflow", "wrong-stream"} {
		t.Run(kind, func(t *testing.T) {
			isolateBlacksmithOwnership(t)
			repo := t.TempDir()
			t.Chdir(repo)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var stdout, stderr bytes.Buffer
			runner := ownershipRunner(func(runCtx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
				start, exit, end := testBlacksmithReceiptFrames(t, syntheticBlacksmithCommand(t, req), 0)
				output := start + "ordinary\n" + exit + payload + end
				switch kind {
				case "missing-start":
					output = exit + payload + end
				case "missing-exit":
					output = start + payload + end
				case "missing-end":
					output = start + exit + payload
				case "stale":
					output = strings.ReplaceAll(output, "CRABBOX_BS_", "CRABBOX_OLD_")
				case "duplicate-start":
					output = start + output
				case "duplicate-exit":
					output = start + exit + exit + payload + end
				case "duplicate-end":
					output += end
				case "order":
					output = exit + start + payload + end
				case "truncated":
					output = start + exit + payload + end[:len(end)-1]
				case "malformed":
					output = start + strings.Replace(exit, "exit:0", "exit:00", 1) + payload + end
				case "oversized-record":
					output = "\x1e" + strings.Repeat("x", 1024) + payload
				case "partial-archive":
					output = start + exit + payload[:len(payload)/2] + end
				case "duplicate-archive":
					output = start + exit + payload + payload + end
				case "native-before":
					return LocalCommandResult{ExitCode: 1}, errors.New("transport lost")
				case "overflow":
					output = start + exit + core.DelegatedRunArtifactBeginMarker + "\n" + strings.Repeat("A", int(blacksmithArtifactOutputCaptureLimit(core.DelegatedRunArtifactDefaultMaxBytes))+1)
				case "diagnostics-overflow":
					output = start + exit + strings.Repeat("x", int(blacksmithArtifactDiagnosticCaptureBytes)+1)
				}
				writer := req.Stdout
				if kind == "wrong-stream" {
					writer = req.Stderr
				}
				chunk := 7
				if len(output) > 10000 {
					chunk = 8192
				}
				for len(output) > 0 {
					n := min(chunk, len(output))
					_, _ = io.WriteString(writer, output[:n])
					output = output[n:]
				}
				if kind == "overflow" || kind == "diagnostics-overflow" {
					if runCtx.Err() == nil {
						t.Error("overflow did not cancel")
					}
				}
				if kind == "cancel" {
					cancel()
				}
				if kind == "native-after" {
					return LocalCommandResult{ExitCode: 1}, errors.New("late transport loss")
				}
				if kind == "nil-error-nonzero" {
					return LocalCommandResult{ExitCode: 23}, nil
				}
				if kind == "zero-code-error" {
					return LocalCommandResult{}, errors.New("late transport loss")
				}
				return LocalCommandResult{}, nil
			})
			backend := newTestBlacksmithBackend(baseConfig(), runner)
			backend.rt.Stdout, backend.rt.Stderr = &stdout, &stderr
			code, _, collected, err := backend.runArtifactTestbox(ctx, RunRequest{Repo: Repo{Root: repo}, Command: []string{"true"}, ArtifactGlobs: []string{"report"}}, "tbx_receipt", nil, nil, nil, time.Second)
			if kind == "valid" {
				if err != nil || code != 0 || len(collected.Artifacts) != 1 {
					t.Fatalf("code=%d collected=%+v err=%v", code, collected, err)
				}
			} else if code == 0 && err == nil || len(collected.Artifacts) != 0 {
				t.Fatalf("accepted %s code=%d err=%v", kind, code, err)
			}
			if strings.Contains(stdout.String()+stderr.String(), "CRABBOX_") || strings.Contains(stdout.String()+stderr.String(), base64.StdEncoding.EncodeToString(archive)[:20]) || strings.Contains(stdout.String()+stderr.String(), "\x1e") {
				t.Fatal("payload leaked")
			}
		})
	}
}

func TestBlacksmithArtifactRunBudgets(t *testing.T) {
	requireBlacksmithArtifactShell(t)
	for _, kind := range []string{"workload-outlives-budget", "local-collection-stall", "remote-collection-timeout", "sync-stall", "startup-failure", "missing-timeout"} {
		for _, code := range []int{0, 1, 23} {
			t.Run(fmt.Sprintf("%s/%d", kind, code), func(t *testing.T) {
				isolateBlacksmithOwnership(t)
				repo := t.TempDir()
				t.Chdir(repo)
				testWriteBlacksmithFile(t, repo, "report", "synthetic")
				if kind == "startup-failure" {
					testWriteBlacksmithFile(t, repo, "bash-env", "exit 9\n")
				}
				if kind == "sync-stall" {
					t.Setenv("CRABBOX_BLACKSMITH_SYNC_TIMEOUT_MS", "20")
				}
				budget := 100 * time.Millisecond
				workloadWait := "0.2"
				if kind == "workload-outlives-budget" {
					budget, workloadWait = 3*time.Second, "3.2"
				}
				runner := ownershipRunner(func(runCtx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
					if kind == "startup-failure" {
						req.Env = []string{"BASH_ENV=" + filepath.Join(repo, "bash-env")}
					}
					switch kind {
					case "local-collection-stall":
						start, exit, _ := testBlacksmithReceiptFrames(t, syntheticBlacksmithCommand(t, req), code)
						fmt.Fprint(req.Stdout, start+exit)
						<-runCtx.Done()
						return LocalCommandResult{ExitCode: 1}, runCtx.Err()
					case "sync-stall":
						fmt.Fprintln(req.Stdout, "Syncing from repo root: /synthetic")
						<-runCtx.Done()
						return LocalCommandResult{ExitCode: 1}, runCtx.Err()
					case "remote-collection-timeout":
						// Exercise the actual timeout command, leaving the local
						// receipt deadline long enough to receive its terminal code.
						for i, arg := range req.Args {
							if strings.HasPrefix(arg, "/bin/sh -c ") {
								req.Args[i] = strings.Replace(arg, "set -euo pipefail", "sleep 1; set -euo pipefail", 1)
							}
						}
						// This fixture waits independently of the adapter's local
						// timer so the shell must enforce the remote timeout itself.
						return runSyntheticBlacksmithCommand(t, t.Context(), req)
					case "missing-timeout":
						for i, arg := range req.Args {
							req.Args[i] = strings.Replace(arg, "command -v timeout", "command -v crabbox_nonexistent_timeout", 1)
						}
					}
					return runSyntheticBlacksmithCommand(t, runCtx, req)
				})
				backend := newTestBlacksmithBackend(baseConfig(), runner)
				begin := time.Now()
				got, ended, artifacts, err := backend.runArtifactTestbox(t.Context(), RunRequest{Repo: Repo{Root: repo}, Command: []string{fmt.Sprintf("sleep %s; exit %d", workloadWait, code)}, ArtifactGlobs: []string{"report"}}, "tbx_budget", nil, nil, nil, budget)
				if kind == "workload-outlives-budget" {
					if err != nil || got != code || len(artifacts.Artifacts) != 1 || ended.Sub(begin) < budget {
						t.Fatalf("workload was bounded: code=%d err=%v", got, err)
					}
				} else {
					if err == nil || len(artifacts.Artifacts) != 0 || got == 0 {
						t.Fatalf("unconfirmed success code=%d err=%v", got, err)
					}
					if (kind == "local-collection-stall" || kind == "remote-collection-timeout") && code != 0 && got != code {
						t.Fatalf("lost workload exit %d: %d", code, got)
					}
					if kind == "sync-stall" && got != 124 {
						t.Fatalf("sync code=%d", got)
					}
				}
				if time.Since(begin) > budget+3*time.Second {
					t.Fatal("unbounded collection wait")
				}
			})
		}
	}
}

func TestBlacksmithArtifactRunInitialSourceAndTiming(t *testing.T) {
	requireBlacksmithArtifactShell(t)
	isolateBlacksmithOwnership(t)
	repo := t.TempDir()
	t.Chdir(repo)
	const id = "tbx_source"
	testOwnedBlacksmithClaim(t, id, "jade-krill", repo)
	runs := 0
	runner := &blacksmithFuncRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		runs++
		// A second synthetic native sync would overwrite this workload file.
		testWriteBlacksmithFile(t, req.Dir, "report", "local-sync-bytes")
		for i, arg := range req.Args {
			if strings.HasPrefix(arg, "/bin/sh -c ") {
				req.Args[i] = strings.Replace(arg, "set -euo pipefail", "sleep 0.2; set -euo pipefail", 1)
			}
		}
		return runSyntheticBlacksmithCommand(t, t.Context(), req)
	}}
	backend := newTestBlacksmithBackend(baseConfig(), runner)
	result, err := backend.Run(t.Context(), RunRequest{ID: id, Repo: Repo{Root: repo}, Command: []string{"printf workload-bytes > report"}, ArtifactGlobs: []string{"report"}})
	if err != nil || runs != 1 || len(result.Artifacts) != 1 {
		t.Fatalf("result=%+v runs=%d err=%v", result, runs, err)
	}
	if readBlacksmithArchive(t, result.Artifacts[0].Path)["report"] != "workload-bytes" {
		t.Fatal("native re-sync overwrote artifact")
	}
	if result.Total-result.Command < 200*time.Millisecond {
		t.Fatalf("collection included in command: command=%s total=%s", result.Command, result.Total)
	}
}

func TestBlacksmithArtifactRunProtectedAndRequiredPaths(t *testing.T) {
	requireBlacksmithArtifactShell(t)
	for _, required := range []string{"report", "dangling", "directory-link/file", ".git/private", ".crabbox/private"} {
		t.Run(required, func(t *testing.T) {
			isolateBlacksmithOwnership(t)
			repo := t.TempDir()
			t.Chdir(repo)
			for _, name := range []string{"report", "directory/file", ".git/private", ".crabbox/private"} {
				testWriteBlacksmithFile(t, repo, name, "synthetic")
			}
			for name, target := range map[string]string{"dangling": "missing", "directory-link": "directory", "leaf-link": "report"} {
				if err := os.Symlink(target, filepath.Join(repo, name)); err != nil {
					t.Fatal(err)
				}
			}
			runner := ownershipRunner(func(ctx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
				return runSyntheticBlacksmithCommand(t, ctx, req)
			})
			backend := newTestBlacksmithBackend(baseConfig(), runner)
			_, _, result, err := backend.runArtifactTestbox(t.Context(), RunRequest{Repo: Repo{Root: repo}, Command: []string{"true"}, ArtifactGlobs: []string{"**"}, RequiredArtifactGlobs: []string{required}}, "tbx_protected", nil, nil, nil, time.Second)
			if required != "report" {
				if err == nil || len(result.Artifacts) != 0 {
					t.Fatal("required path bypassed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			files := readBlacksmithArchive(t, result.Artifacts[0].Path)
			for name := range files {
				if strings.Contains(name, ".git/") || strings.Contains(name, ".crabbox/") || strings.HasPrefix(name, "directory-link/") || name == "dangling" {
					t.Fatalf("unsafe archive member %s", name)
				}
			}
			if _, ok := files["leaf-link"]; !ok {
				t.Fatal("regular leaf symlink contract changed")
			}
		})
	}
}

func TestBlacksmithArtifactRunClaimFence(t *testing.T) {
	for _, mode := range []string{"writer", "stop", "replacement-before-run"} {
		t.Run(mode, func(t *testing.T) {
			isolateBlacksmithOwnership(t)
			repo := t.TempDir()
			t.Chdir(repo)
			const id = "tbx_artifactfence"
			claim := testOwnedBlacksmithClaim(t, id, "jade-krill", repo)
			route, _, _ := blacksmithClaimBinding(claim)
			entered, release, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
			archive := makeTarGz(t, map[string]string{"report": "synthetic"})
			cfg := baseConfig()
			backend := newTestBlacksmithBackend(cfg, ownershipRunner(func(ctx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
				switch req.Args[1] {
				case "status":
					state := "ready"
					select {
					case <-stopped:
						state = "completed"
					default:
					}
					return LocalCommandResult{Stdout: testBlacksmithStatus(id, state)}, nil
				case "stop":
					close(stopped)
					return LocalCommandResult{}, nil
				case "run":
					start, exit, end := testBlacksmithReceiptFrames(t, syntheticBlacksmithCommand(t, req), 23)
					fmt.Fprint(req.Stdout, start+exit)
					close(entered)
					select {
					case <-release:
					case <-stopped:
						return LocalCommandResult{ExitCode: 1}, errors.New("stopped transport")
					case <-ctx.Done():
						return LocalCommandResult{ExitCode: 1}, ctx.Err()
					}
					fmt.Fprint(req.Stdout, core.DelegatedRunArtifactBeginMarker+"\n"+base64.StdEncoding.EncodeToString(archive)+"\n"+core.DelegatedRunArtifactEndMarker+"\n"+end)
					return LocalCommandResult{}, nil
				}
				t.Errorf("unexpected native action %s", req.Args[1])
				return LocalCommandResult{}, errors.New("unexpected")
			}))
			backend.route, backend.claim = &route, &claim
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			if mode == "replacement-before-run" {
				replacement := claim
				replacement.RepoRoot = "/different"
				if err := core.ReplaceLeaseClaimIfUnchanged(id, claim, replacement); err != nil {
					t.Fatal(err)
				}
				if err := backend.withOwnedTestbox(ctx, claim, func() error { t.Error("replaced claim reached workload"); return nil }); err == nil {
					t.Fatal("replacement accepted")
				}
				return
			}
			done := make(chan error, 1)
			go func() {
				done <- backend.withOwnedTestbox(ctx, claim, func() error {
					code, _, result, err := backend.runArtifactTestbox(ctx, RunRequest{Repo: Repo{Root: repo}, Command: []string{"exit 23"}, ArtifactGlobs: []string{"report"}}, id, nil, nil, nil, time.Second)
					if code != 23 || (mode == "writer" && (err != nil || len(result.Artifacts) != 1)) || (mode == "stop" && (err == nil || len(result.Artifacts) != 0)) {
						return fmt.Errorf("code=%d artifact count=%d err=%v", code, len(result.Artifacts), err)
					}
					return nil
				})
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("run never entered")
			}
			other := make(chan error, 1)
			if mode == "writer" {
				go func() {
					other <- core.WithDurableLeaseClaimLockContext(ctx, id, func(current *core.LeaseClaim, exists bool, save func() error) error {
						if _, err := os.Stat(core.LocalRunArtifactPath(repo, "", id, "blacksmith-artifacts.tgz")); err != nil {
							return fmt.Errorf("writer crossed publication fence: %w", err)
						}
						current.RepoRoot = "/replacement"
						return save()
					})
				}()
				select {
				case err := <-other:
					t.Fatalf("writer entered before publication: %v", err)
				case <-time.After(30 * time.Millisecond):
				}
				close(release)
			} else {
				go func() { other <- backend.stopClaimedTestbox(ctx, id, claim) }()
				select {
				case <-stopped:
				case <-ctx.Done():
					t.Fatal("shared fence blocked stop")
				}
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if err := <-other; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBlacksmithArtifactFailureCleanupAndClassification(t *testing.T) {
	requireBlacksmithArtifactShell(t)
	for _, mode := range []string{"collector-cleanup", "collector-diagnostic", "lease-cleanup"} {
		for _, code := range []int{0, 1, 23} {
			t.Run(fmt.Sprintf("%s/%d", mode, code), func(t *testing.T) {
				isolateBlacksmithOwnership(t)
				repo := t.TempDir()
				t.Chdir(repo)
				testWriteBlacksmithFile(t, repo, "report", "synthetic")
				const id = "tbx_failureprecedence"
				runner := &blacksmithFuncRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
					switch req.Args[1] {
					case "warmup":
						return LocalCommandResult{Stdout: id + "\n"}, nil
					case "stop":
						if mode == "lease-cleanup" {
							return LocalCommandResult{ExitCode: 49}, errors.New("synthetic cleanup failure")
						}
						return LocalCommandResult{}, nil
					case "run":
						for i, arg := range req.Args {
							if strings.HasPrefix(arg, "/bin/sh -c ") {
								switch mode {
								case "collector-cleanup":
									// Actual script EXIT trap still removes its temporary file,
									// then reports a synthetic cleanup failure after archive output.
									req.Args[i] = strings.Replace(arg, "set -euo pipefail", "set -euo pipefail\nrm() { command rm \"$@\"; return 9; }", 1)
								case "collector-diagnostic":
									req.Args[i] = strings.Replace(arg, "set -euo pipefail", "echo out of memory CRABBOX_PHASE:install; exit 9\nset -euo pipefail", 1)
								}
							}
						}
						return runSyntheticBlacksmithCommand(t, t.Context(), req)
					}
					return LocalCommandResult{}, nil
				}}
				cfg := baseConfig()
				cfg.Blacksmith.Workflow = ".github/workflows/testbox.yml"
				backend := newTestBlacksmithBackend(cfg, runner)
				var stderr bytes.Buffer
				backend.rt.Stderr = &stderr
				result, err := backend.Run(t.Context(), RunRequest{Repo: Repo{Root: repo}, Command: []string{fmt.Sprintf("printf 'CRABBOX_PHASE:test\\n'; exit %d", code)}, ArtifactGlobs: []string{"report"}, TimingJSON: true})
				want := code
				if code == 0 {
					want = 7
					if mode == "lease-cleanup" {
						want = 1
					}
				}
				var ee ExitError
				if !errors.As(err, &ee) || ee.Code != want || result.ExitCode != want {
					t.Fatalf("result=%+v err=%v", result, err)
				}
				if mode == "lease-cleanup" {
					if len(result.Artifacts) != 1 || !result.Session.Kept {
						t.Fatal("cleanup changed evidence or retention")
					}
				} else if len(result.Artifacts) != 0 {
					t.Fatal("failed collector published")
				}
				for _, line := range strings.Split(stderr.String(), "\n") {
					if !strings.HasPrefix(line, "{") {
						continue
					}
					var report core.TimingReport
					if err := json.Unmarshal([]byte(line), &report); err != nil {
						t.Fatal(err)
					}
					if report.ExitCode != want || report.CommandMs != result.Command.Milliseconds() || report.TotalMs != result.Total.Milliseconds() {
						t.Fatalf("timing changed: %+v", report)
					}
					baseline := core.ClassifyRunFailure(code, "CRABBOX_PHASE:test\n", report.CommandPhases)
					if code != 0 && (report.BlockedStage != baseline.BlockedStage || report.ResourceExhaustion != baseline.ResourceExhaustion || report.RetryLikely != baseline.RetryLikely) {
						t.Fatalf("collector reclassified workload: %+v", report)
					}
				}
				if strings.Contains(stderr.String(), "out of memory") || strings.Contains(stderr.String(), "H4sI") {
					t.Fatal("collector leaked to diagnostics")
				}
			})
		}
	}
}

func TestBlacksmithArtifactReceiptEverySplit(t *testing.T) {
	r, err := newBlacksmithArtifactReceipt(time.Now)
	if err != nil {
		t.Fatal(err)
	}
	start, exit, end := testBlacksmithReceiptFrames(t, r.command(RunRequest{Command: []string{"true"}}, time.Second), 23)
	wire := "before" + start + "during" + exit + "private" + end
	for split := 0; split <= len(wire); split++ {
		var visible bytes.Buffer
		receipt := &blacksmithArtifactReceipt{nonce: r.nonce, now: time.Now, output: &visible}
		ctx, cancel := context.WithCancelCause(t.Context())
		demux := blacksmithControlDemux{data: receipt.data, record: receipt.record, cancel: cancel}
		_, _ = demux.Write([]byte(wire[:split]))
		_, _ = demux.Write([]byte(wire[split:]))
		demux.finish()
		if demux.err != nil || ctx.Err() != nil || receipt.stage != 3 || receipt.code != 23 || receipt.payload.String() != "private" || visible.String() != "beforeduring" {
			t.Fatalf("split %d failed", split)
		}
		cancel(nil)
	}
}
