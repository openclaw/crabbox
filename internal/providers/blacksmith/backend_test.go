package blacksmith

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type testClock struct{}

func (testClock) Now() time.Time { return time.Now() }

type blacksmithFuncRunner struct {
	calls [][]string
	fn    func(LocalCommandRequest) (LocalCommandResult, error)
}

func (r *blacksmithFuncRunner) Run(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	r.calls = append(r.calls, append([]string(nil), req.Args...))
	if r.fn != nil {
		return r.fn(req)
	}
	return LocalCommandResult{}, nil
}

type blockingSyncRunner struct{}

func (blockingSyncRunner) Run(ctx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	if req.Stdout != nil {
		_, _ = req.Stdout.Write([]byte("Syncing from repo root: /repo\n"))
	}
	<-ctx.Done()
	return LocalCommandResult{ExitCode: 1}, ctx.Err()
}

func newTestBlacksmithBackend(cfg Config, runner CommandRunner) *blacksmithBackend {
	return &blacksmithBackend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		rt:   Runtime{Stdout: io.Discard, Stderr: io.Discard, Clock: testClock{}, Exec: runner},
	}
}

func TestBlacksmithWarmupArgs(t *testing.T) {
	cfg := baseConfig()
	cfg.Blacksmith = BlacksmithConfig{
		Org:         "openclaw",
		Workflow:    ".github/workflows/testbox.yml",
		Job:         "check",
		Ref:         "main",
		IdleTimeout: 90*time.Minute + 10*time.Second,
	}
	cfg.Cache.Volumes = []core.CacheVolumeConfig{
		{Name: "pnpm-store", Key: "my-app-linux-node24-lock", Path: "/var/cache/crabbox/pnpm"},
	}
	got, err := blacksmithWarmupArgs(cfg, "ssh-ed25519 AAAA")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"--org", "openclaw",
		"testbox", "warmup", ".github/workflows/testbox.yml",
		"--job", "check",
		"--ref", "main",
		"--ssh-public-key", "ssh-ed25519 AAAA",
		"--sticky-disk", "my-app-linux-node24-lock:/var/cache/crabbox/pnpm",
		"--idle-timeout", "91",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args=%#v want %#v", got, want)
	}
}

func TestBlacksmithWarmupArgsFallsBackToTestboxActionsConfig(t *testing.T) {
	cfg := baseConfig()
	cfg.Actions.Workflow = ".github/workflows/ci-check-testbox.yml"
	cfg.Actions.Job = "check"
	cfg.Actions.Ref = "trunk"
	got, err := blacksmithWarmupArgs(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{".github/workflows/ci-check-testbox.yml", "--job", "check", "--ref", "trunk"} {
		if !containsString(got, want) {
			t.Fatalf("args missing %q: %#v", want, got)
		}
	}
}

func TestBlacksmithWarmupArgsFallsBackToArbitraryActionsWorkflowName(t *testing.T) {
	cfg := baseConfig()
	cfg.Actions.Workflow = ".github/workflows/ci.yml"
	cfg.Actions.Job = "integration"
	cfg.Actions.Ref = "trunk"
	got, err := blacksmithWarmupArgs(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{".github/workflows/ci.yml", "--job", "integration", "--ref", "trunk"} {
		if !containsString(got, want) {
			t.Fatalf("args missing %q: %#v", want, got)
		}
	}
}

func TestBlacksmithWarmupArgsDoesNotUseGenericActionsHydrateWorkflow(t *testing.T) {
	for _, workflow := range []string{
		"Crabbox Hydrate",
		".github/workflows/crabbox.yml",
		".github/workflows/crabbox-hydrate.yml",
		".github/workflows/hydrate.yml",
	} {
		cfg := baseConfig()
		cfg.Actions.Workflow = workflow
		cfg.Actions.Job = "hydrate"
		cfg.Actions.Ref = "main"
		_, err := blacksmithWarmupArgs(cfg, "")
		if err == nil || !strings.Contains(err.Error(), "requires blacksmith.workflow") {
			t.Fatalf("expected workflow error for %s, got %v", workflow, err)
		}
	}
}

func TestBlacksmithWarmupArgsPrefersExplicitConfigOverGenericActionsConfig(t *testing.T) {
	cfg := baseConfig()
	cfg.Actions.Workflow = ".github/workflows/crabbox-hydrate.yml"
	cfg.Actions.Job = "hydrate"
	cfg.Actions.Ref = "actions-ref"
	cfg.Blacksmith.Workflow = ".github/workflows/ci-check-testbox.yml"
	cfg.Blacksmith.Job = "check"
	cfg.Blacksmith.Ref = "testbox-ref"
	got, err := blacksmithWarmupArgs(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"testbox", "warmup", ".github/workflows/ci-check-testbox.yml",
		"--job", "check",
		"--ref", "testbox-ref",
		"--idle-timeout", "30",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args=%#v want %#v", got, want)
	}
}

func TestBlacksmithWarmupArgsExplicitWorkflowCanInheritActionsJobAndRef(t *testing.T) {
	cfg := baseConfig()
	cfg.Actions.Workflow = ".github/workflows/crabbox.yml"
	cfg.Actions.Job = "check"
	cfg.Actions.Ref = "trunk"
	cfg.Blacksmith.Workflow = ".github/workflows/testbox.yml"
	got, err := blacksmithWarmupArgs(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{".github/workflows/testbox.yml", "--job", "check", "--ref", "trunk"} {
		if !containsString(got, want) {
			t.Fatalf("args missing %q: %#v", want, got)
		}
	}
}

func TestBlacksmithWarmupArgsRequiresWorkflow(t *testing.T) {
	cfg := baseConfig()
	_, err := blacksmithWarmupArgs(cfg, "")
	if err == nil || !strings.Contains(err.Error(), "requires blacksmith.workflow") {
		t.Fatalf("expected workflow error, got %v", err)
	}
}

func TestBlacksmithRunRejectsEnvForwardingBeforeWarmup(t *testing.T) {
	var stderr bytes.Buffer
	runner := &blacksmithFuncRunner{}
	cfg := baseConfig()
	backend := &blacksmithBackend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		rt:   Runtime{Stdout: io.Discard, Stderr: &stderr, Clock: testClock{}, Exec: runner},
	}
	_, err := backend.Run(context.Background(), RunRequest{
		Repo:       Repo{Root: "/repo"},
		Command:    []string{"true"},
		EnvSummary: true,
		Options: core.LeaseOptions{
			EnvAllow: []string{"API_TOKEN"},
		},
		Env: map[string]string{"API_TOKEN": "secret-token-value"},
	})
	var exitErr ExitError
	if !core.AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("Run error=%v, want exit 2", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runner was called before validation: %#v", runner.calls)
	}
	out := stderr.String()
	for _, want := range []string{"provider=blacksmith-testbox", "behavior=unsupported", "configure secrets in the Testbox workflow"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stderr missing %q in %q", want, out)
		}
	}
	if strings.Contains(out, "secret-token-value") {
		t.Fatalf("stderr leaked env value: %q", out)
	}
}

func TestBlacksmithRunDoesNotRejectDefaultEnvMetadata(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	var stderr bytes.Buffer
	runner := &blacksmithFuncRunner{fn: func(LocalCommandRequest) (LocalCommandResult, error) {
		return LocalCommandResult{ExitCode: 1}, errors.New("warmup failed")
	}}
	cfg := baseConfig()
	cfg.Blacksmith.Workflow = ".github/workflows/testbox.yml"
	backend := &blacksmithBackend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		rt:   Runtime{Stdout: io.Discard, Stderr: &stderr, Clock: testClock{}, Exec: runner},
	}
	_, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Root: "/repo"},
		Command: []string{"true"},
		Options: core.LeaseOptions{
			EnvAllow: []string{"CI", "NODE_OPTIONS"},
		},
		Env: map[string]string{"NODE_OPTIONS": "--max-old-space-size=4096"},
	})
	if err == nil {
		t.Fatal("expected warmup failure")
	}
	if len(runner.calls) == 0 {
		t.Fatal("runner was not called")
	}
	if strings.Contains(stderr.String(), "behavior=unsupported") {
		t.Fatalf("default env metadata was rejected: %q", stderr.String())
	}
}

func TestBlacksmithRunRejectsExplicitDefaultEnvBeforeWarmup(t *testing.T) {
	var stderr bytes.Buffer
	runner := &blacksmithFuncRunner{}
	cfg := baseConfig()
	backend := &blacksmithBackend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		rt:   Runtime{Stdout: io.Discard, Stderr: &stderr, Clock: testClock{}, Exec: runner},
	}
	_, err := backend.Run(context.Background(), RunRequest{
		Repo:       Repo{Root: "/repo"},
		Command:    []string{"true"},
		EnvSummary: true,
		Options: core.LeaseOptions{
			EnvAllow: []string{"CI", "NODE_OPTIONS"},
		},
		Env: map[string]string{"NODE_OPTIONS": "--max-old-space-size=4096"},
	})
	var exitErr ExitError
	if !core.AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("Run error=%v, want exit 2", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runner was called before validation: %#v", runner.calls)
	}
}

func TestBlacksmithRunIgnoresConfiguredEnvAllowBeforeWarmup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	var stderr bytes.Buffer
	runner := &blacksmithFuncRunner{fn: func(LocalCommandRequest) (LocalCommandResult, error) {
		return LocalCommandResult{ExitCode: 1}, errors.New("warmup failed")
	}}
	cfg := baseConfig()
	cfg.Blacksmith.Workflow = ".github/workflows/testbox.yml"
	backend := &blacksmithBackend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		rt:   Runtime{Stdout: io.Discard, Stderr: &stderr, Clock: testClock{}, Exec: runner},
	}
	_, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Root: "/repo"},
		Command: []string{"true"},
		Options: core.LeaseOptions{
			EnvAllow: []string{"API_TOKEN"},
		},
		Env: map[string]string{"API_TOKEN": "secret-token-value"},
	})
	if err == nil {
		t.Fatal("expected warmup failure")
	}
	if len(runner.calls) == 0 {
		t.Fatal("runner was not called")
	}
	if strings.Contains(stderr.String(), "behavior=unsupported") {
		t.Fatalf("configured env allow was rejected: %q", stderr.String())
	}
	if strings.Contains(stderr.String(), "secret-token-value") {
		t.Fatalf("stderr leaked env value: %q", stderr.String())
	}
}

func TestBlacksmithWarmupFailureRemovesPendingKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	runner := &blacksmithFuncRunner{fn: func(LocalCommandRequest) (LocalCommandResult, error) {
		return LocalCommandResult{ExitCode: 1}, errors.New("exit status 1")
	}}

	cfg := baseConfig()
	cfg.Blacksmith.Workflow = ".github/workflows/testbox.yml"
	backend := newTestBlacksmithBackend(cfg, runner)
	_, _, err := backend.warmupLease(context.Background(), Repo{Root: "/repo"}, false, "")
	if err == nil {
		t.Fatal("expected warmup failure")
	}
	keyPath, keyErr := testboxKeyPath("tbx_probe")
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	entries, readErr := os.ReadDir(filepath.Dir(filepath.Dir(keyPath)))
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("pending key directories leaked: %v", entries)
	}
}

func TestBlacksmithWarmupFailureStopsPrintedTestbox(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	var stopped string
	runner := &blacksmithFuncRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		if len(req.Args) >= 3 && req.Args[0] == "testbox" && req.Args[1] == "stop" {
			for i, arg := range req.Args {
				if arg == "--id" && i+1 < len(req.Args) {
					stopped = req.Args[i+1]
				}
			}
			return LocalCommandResult{}, nil
		}
		return LocalCommandResult{ExitCode: 1, Stdout: "queued tbx_leaked123\n"}, errors.New("exit status 1")
	}}

	cfg := baseConfig()
	cfg.Blacksmith.Workflow = ".github/workflows/testbox.yml"
	backend := newTestBlacksmithBackend(cfg, runner)
	_, _, err := backend.warmupLease(context.Background(), Repo{Root: "/repo"}, false, "")
	if err == nil {
		t.Fatal("expected warmup failure")
	}
	if stopped != "tbx_leaked123" {
		t.Fatalf("stopped=%q, want tbx_leaked123", stopped)
	}
}

func TestBlacksmithWarmupFailureStopsNewListedTestbox(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	originalDelay := blacksmithCleanupDelay
	originalAttempts := blacksmithCleanupAttempts
	originalQuiet := blacksmithCleanupQuiet
	blacksmithCleanupDelay = time.Millisecond
	blacksmithCleanupAttempts = 3
	blacksmithCleanupQuiet = 1
	var stopped string
	listCalls := 0
	runner := &blacksmithFuncRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		if len(req.Args) >= 3 && req.Args[0] == "testbox" && req.Args[1] == "list" {
			listCalls++
			if listCalls < 3 {
				return LocalCommandResult{Stdout: "ID STATUS REPO WORKFLOW JOB REF CREATED\n"}, nil
			}
			return LocalCommandResult{Stdout: "tbx_async123 queued openclaw .github/workflows/testbox.yml check main 2026-05-04T21:23:47.000000Z\n"}, nil
		}
		if len(req.Args) >= 3 && req.Args[0] == "testbox" && req.Args[1] == "stop" {
			for i, arg := range req.Args {
				if arg == "--id" && i+1 < len(req.Args) {
					stopped = req.Args[i+1]
				}
			}
			return LocalCommandResult{}, nil
		}
		return LocalCommandResult{ExitCode: 1, Stdout: "workflow missing\n"}, errors.New("exit status 1")
	}}
	t.Cleanup(func() {
		blacksmithCleanupDelay = originalDelay
		blacksmithCleanupAttempts = originalAttempts
		blacksmithCleanupQuiet = originalQuiet
	})

	cfg := baseConfig()
	cfg.Blacksmith.Workflow = ".github/workflows/testbox.yml"
	cfg.Blacksmith.Job = "check"
	cfg.Blacksmith.Ref = "main"
	backend := newTestBlacksmithBackend(cfg, runner)
	_, _, err := backend.warmupLease(context.Background(), Repo{Root: "/repo"}, false, "")
	if err == nil {
		t.Fatal("expected warmup failure")
	}
	if stopped != "tbx_async123" {
		t.Fatalf("stopped=%q, want tbx_async123", stopped)
	}
}

func TestBlacksmithWarmupFailureContinuesAfterFirstDelayedStop(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	originalDelay := blacksmithCleanupDelay
	originalAttempts := blacksmithCleanupAttempts
	originalQuiet := blacksmithCleanupQuiet
	blacksmithCleanupDelay = time.Millisecond
	blacksmithCleanupAttempts = 5
	blacksmithCleanupQuiet = 1
	stopped := []string{}
	listCalls := 0
	runner := &blacksmithFuncRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		if len(req.Args) >= 3 && req.Args[0] == "testbox" && req.Args[1] == "list" {
			listCalls++
			switch listCalls {
			case 2:
				return LocalCommandResult{Stdout: "tbx_delayed1 queued openclaw .github/workflows/testbox.yml check main 2026-05-04T21:23:47.000000Z\n"}, nil
			case 3:
				return LocalCommandResult{Stdout: "tbx_delayed2 queued openclaw .github/workflows/testbox.yml check main 2026-05-04T21:23:48.000000Z\n"}, nil
			default:
				return LocalCommandResult{Stdout: "ID STATUS REPO WORKFLOW JOB REF CREATED\n"}, nil
			}
		}
		if len(req.Args) >= 3 && req.Args[0] == "testbox" && req.Args[1] == "stop" {
			for i, arg := range req.Args {
				if arg == "--id" && i+1 < len(req.Args) {
					stopped = append(stopped, req.Args[i+1])
				}
			}
			return LocalCommandResult{}, nil
		}
		return LocalCommandResult{ExitCode: 1, Stdout: "workflow missing\n"}, errors.New("exit status 1")
	}}
	t.Cleanup(func() {
		blacksmithCleanupDelay = originalDelay
		blacksmithCleanupAttempts = originalAttempts
		blacksmithCleanupQuiet = originalQuiet
	})

	cfg := baseConfig()
	cfg.Blacksmith.Workflow = ".github/workflows/testbox.yml"
	cfg.Blacksmith.Job = "check"
	cfg.Blacksmith.Ref = "main"
	backend := newTestBlacksmithBackend(cfg, runner)
	_, _, err := backend.warmupLease(context.Background(), Repo{Root: "/repo"}, false, "")
	if err == nil {
		t.Fatal("expected warmup failure")
	}
	if !reflect.DeepEqual(stopped, []string{"tbx_delayed1", "tbx_delayed2"}) {
		t.Fatalf("stopped=%v, want both delayed testboxes", stopped)
	}
}

func TestBlacksmithOneShotRunRemovesClaimAfterStop(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	var stderr bytes.Buffer
	runner := &blacksmithFuncRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		if len(req.Args) >= 3 && req.Args[0] == "testbox" && req.Args[1] == "warmup" {
			return LocalCommandResult{Stdout: "ready tbx_abc123\n"}, nil
		}
		if len(req.Args) >= 3 && req.Args[0] == "testbox" && req.Args[1] == "run" {
			if req.Stdout != nil {
				_, _ = req.Stdout.Write([]byte("https://github.com/example-org/my-app/actions/runs/123456789\n"))
			}
			return LocalCommandResult{}, nil
		}
		return LocalCommandResult{}, nil
	}}

	cfg := baseConfig()
	cfg.Blacksmith.Workflow = ".github/workflows/testbox.yml"
	backend := &blacksmithBackend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		rt:   Runtime{Stdout: io.Discard, Stderr: &stderr, Clock: testClock{}, Exec: runner},
	}
	_, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Root: "/repo"},
		Command: []string{"true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 4 {
		t.Fatalf("blacksmith calls=%d, want list/warmup/run/stop", len(runner.calls))
	}
	if claim, err := readLeaseClaim("tbx_abc123"); err != nil {
		t.Fatal(err)
	} else if claim.LeaseID != "" {
		t.Fatalf("claim leaked after one-shot stop: %#v", claim)
	}
	if keyPath, err := testboxKeyPath("tbx_abc123"); err != nil {
		t.Fatal(err)
	} else if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("key leaked after one-shot stop: %v", err)
	}
	for _, want := range []string{
		"blacksmith proof note: stopped one-shot Testbox after success",
		"backing GitHub Actions run may show a cancelled Testbox step",
		"actions=https://github.com/example-org/my-app/actions/runs/123456789",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr missing %q in:\n%s", want, stderr.String())
		}
	}
}

func TestBlacksmithKeptRunWritesLeaseOutput(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	runner := &blacksmithFuncRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		if len(req.Args) >= 3 && req.Args[0] == "testbox" && req.Args[1] == "warmup" {
			return LocalCommandResult{Stdout: "ready tbx_kept123\n"}, nil
		}
		if len(req.Args) >= 3 && req.Args[0] == "testbox" && req.Args[1] == "run" {
			if req.Stdout != nil {
				_, _ = req.Stdout.Write([]byte("https://github.com/example-org/my-app/actions/runs/987654321\n"))
			}
			return LocalCommandResult{}, nil
		}
		return LocalCommandResult{}, nil
	}}

	cfg := baseConfig()
	cfg.Blacksmith.Workflow = ".github/workflows/testbox.yml"
	backend := newTestBlacksmithBackend(cfg, runner)
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Root: "/repo"},
		Command: []string{"npm", "test"},
		Keep:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 3 {
		t.Fatalf("blacksmith calls=%d, want list/warmup/run without stop: %#v", len(runner.calls), runner.calls)
	}
	if result.Session == nil {
		t.Fatal("missing session handle")
	}
	got := result.Session
	if got.Provider != blacksmithTestboxProvider || got.LeaseID != "tbx_kept123" || got.Slug == "" || got.Reused || !got.Kept {
		t.Fatalf("lease output=%#v", got)
	}
	if got.ActionsURL != "https://github.com/example-org/my-app/actions/runs/987654321" || got.RunID != "987654321" {
		t.Fatalf("actions fields=%#v", got)
	}
	if got.CleanupCommand != "crabbox stop --provider blacksmith-testbox tbx_kept123" {
		t.Fatalf("cleanup command=%q", got.CleanupCommand)
	}
}

func TestBlacksmithActionsURLExtractionNormalizesProse(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		wantURL string
		wantID  string
	}{
		{
			name:    "parenthesized sentence",
			text:    "run: (https://github.com/example-org/my-app/actions/runs/123456).",
			wantURL: "https://github.com/example-org/my-app/actions/runs/123456",
			wantID:  "123456",
		},
		{
			name:    "markdown link",
			text:    "[run](https://github.com/example-org/my-app/actions/runs/222333)",
			wantURL: "https://github.com/example-org/my-app/actions/runs/222333",
			wantID:  "222333",
		},
		{
			name:    "query string",
			text:    "https://github.com/example-org/my-app/actions/runs/987654321?check_suite_focus=true.",
			wantURL: "https://github.com/example-org/my-app/actions/runs/987654321?check_suite_focus=true",
			wantID:  "987654321",
		},
		{
			name:    "attempt suffix",
			text:    "https://github.com/example-org/my-app/actions/runs/123456/attempts/1",
			wantURL: "https://github.com/example-org/my-app/actions/runs/123456/attempts/1",
			wantID:  "123456",
		},
		{
			name: "reject nonnumeric run id",
			text: "https://github.com/example-org/my-app/actions/runs/123abc",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotURL := firstBlacksmithActionsURL(tt.text)
			if gotURL != tt.wantURL {
				t.Fatalf("url=%q, want %q", gotURL, tt.wantURL)
			}
			gotID := blacksmithActionsRunID(gotURL)
			if gotID != tt.wantID {
				t.Fatalf("runID=%q, want %q", gotID, tt.wantID)
			}
		})
	}
}

func TestBlacksmithReusedRunWritesLeaseOutput(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	runner := &blacksmithFuncRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		if len(req.Args) >= 3 && req.Args[0] == "testbox" && req.Args[1] == "run" {
			return LocalCommandResult{}, nil
		}
		return LocalCommandResult{}, nil
	}}

	cfg := baseConfig()
	backend := newTestBlacksmithBackend(cfg, runner)
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Root: "/repo"},
		ID:      "tbx_reuse123",
		Command: []string{"npm", "run", "smoke"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("blacksmith calls=%d, want run only: %#v", len(runner.calls), runner.calls)
	}
	if result.Session == nil {
		t.Fatal("missing session handle")
	}
	got := result.Session
	if got.Provider != blacksmithTestboxProvider || got.LeaseID != "tbx_reuse123" || !got.Reused || !got.Kept {
		t.Fatalf("lease output=%#v", got)
	}
}

func TestBlacksmithRunTimingJSONIncludesCommandPhases(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	runner := &blacksmithFuncRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		if len(req.Args) >= 3 && req.Args[0] == "testbox" && req.Args[1] == "warmup" {
			return LocalCommandResult{Stdout: "ready tbx_phase123\n"}, nil
		}
		if len(req.Args) >= 3 && req.Args[0] == "testbox" && req.Args[1] == "run" {
			if req.Stdout != nil {
				_, _ = req.Stdout.Write([]byte("CRABBOX_PHASE:delegated\nok\n"))
			}
			return LocalCommandResult{}, nil
		}
		return LocalCommandResult{}, nil
	}}
	var stderr bytes.Buffer
	cfg := baseConfig()
	cfg.Blacksmith.Workflow = ".github/workflows/testbox.yml"
	backend := &blacksmithBackend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		rt:   Runtime{Stdout: io.Discard, Stderr: &stderr, Clock: testClock{}, Exec: runner},
	}

	_, err := backend.Run(context.Background(), RunRequest{
		Repo:       Repo{Root: "/repo"},
		Command:    []string{"true"},
		Label:      "update flow smoke",
		TimingJSON: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var report timingReport
	for _, line := range strings.Split(stderr.String(), "\n") {
		if strings.HasPrefix(line, "{") {
			if err := json.Unmarshal([]byte(line), &report); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(report.CommandPhases) != 2 {
		t.Fatalf("command phases=%#v, want user-command and delegated", report.CommandPhases)
	}
	if report.CommandPhases[1].Name != "delegated" {
		t.Fatalf("command phases=%#v, want delegated marker", report.CommandPhases)
	}
	if report.Label != "update flow smoke" {
		t.Fatalf("label=%q", report.Label)
	}
}

func TestBlacksmithRunProofArtifactsPersistSuccessStreams(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	runner := &blacksmithFuncRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		if len(req.Args) >= 3 && req.Args[0] == "testbox" && req.Args[1] == "warmup" {
			return LocalCommandResult{Stdout: "ready tbx_proof123\n"}, nil
		}
		if len(req.Args) >= 3 && req.Args[0] == "testbox" && req.Args[1] == "run" {
			if req.Stdout != nil {
				_, _ = req.Stdout.Write([]byte("https://github.com/example-org/my-app/actions/runs/123456789\n"))
				_, _ = req.Stdout.Write([]byte(strings.Repeat("verbose setup line\n", blacksmithProofStreamCaptureBytes/19+8)))
				_, _ = req.Stdout.Write([]byte("scenario pass delegated-proof\n"))
			}
			if req.Stderr != nil {
				_, _ = req.Stderr.Write([]byte("stderr detail\n"))
			}
			return LocalCommandResult{}, nil
		}
		return LocalCommandResult{}, nil
	}}
	cfg := baseConfig()
	cfg.Blacksmith.Workflow = ".github/workflows/testbox.yml"
	backend := newTestBlacksmithBackend(cfg, runner)
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:      Repo{Root: repo},
		Command:   []string{"pnpm", "test"},
		EmitProof: filepath.Join(repo, "proof.md"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.LeaseID != "tbx_proof123" || result.ActionsURL != "https://github.com/example-org/my-app/actions/runs/123456789" {
		t.Fatalf("result=%#v", result)
	}
	if !strings.Contains(result.LogExcerpt, "scenario pass delegated-proof") || !strings.Contains(result.LogExcerpt, "stderr detail") {
		t.Fatalf("log excerpt=%q", result.LogExcerpt)
	}
	if len(result.Artifacts) != 4 {
		t.Fatalf("artifacts=%#v", result.Artifacts)
	}
	for _, artifact := range result.Artifacts {
		data, err := os.ReadFile(artifact.Path)
		if err != nil {
			t.Fatalf("read artifact %#v: %v", artifact, err)
		}
		if artifact.Bytes != len(data) {
			t.Fatalf("artifact bytes mismatch for %#v got=%d", artifact, len(data))
		}
	}
	stdoutPath := core.LocalRunArtifactPath(repo, "", "tbx_proof123", "blacksmith.stdout.log")
	stdoutData, err := os.ReadFile(stdoutPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stdoutData), "scenario pass delegated-proof") {
		t.Fatalf("stdout artifact missing run output:\n%s", stdoutData)
	}
	if strings.Contains(string(stdoutData), "https://github.com/example-org/my-app/actions/runs/123456789") {
		t.Fatalf("stdout artifact should be tail-limited, got early URL:\n%s", stdoutData)
	}
}

func TestBlacksmithCollectRunArtifactsWritesBoundedArchive(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	archive := makeTarGz(t, map[string]string{"reports/manifest.json": `{"ok":true}`})
	runner := &blacksmithFuncRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		if len(req.Args) >= 3 && req.Args[0] == "testbox" && req.Args[1] == "run" {
			if req.Stdout != nil {
				_, _ = req.Stdout.Write([]byte("required artifact reports/manifest.json matched=1\n"))
				_, _ = req.Stdout.Write([]byte(core.DelegatedRunArtifactBeginMarker + "\n"))
				_, _ = req.Stdout.Write([]byte(base64.StdEncoding.EncodeToString(archive)))
				_, _ = req.Stdout.Write([]byte("\n" + core.DelegatedRunArtifactEndMarker + "\n"))
			}
			return LocalCommandResult{}, nil
		}
		return LocalCommandResult{}, nil
	}}
	cfg := baseConfig()
	cfg.Blacksmith.Workflow = ".github/workflows/testbox.yml"
	backend := newTestBlacksmithBackend(cfg, runner)
	result, err := backend.CollectRunArtifacts(context.Background(), core.DelegatedRunArtifactRequest{
		RunReq: core.RunRequest{
			Repo:                  core.Repo{Root: repo},
			ArtifactGlobs:         []string{"reports/**"},
			RequiredArtifactGlobs: []string{"reports/manifest.json"},
		},
		Result:   core.RunResult{LeaseID: "tbx_artifacts"},
		MaxFiles: 16,
		MaxBytes: int64(len(archive) + 128),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Output, "required artifact reports/manifest.json matched=1") {
		t.Fatalf("output=%q", result.Output)
	}
	if len(result.Artifacts) != 1 {
		t.Fatalf("artifacts=%#v", result.Artifacts)
	}
	artifact := result.Artifacts[0]
	if artifact.Kind != "artifact-glob" || artifact.Bytes != len(archive) {
		t.Fatalf("artifact=%#v", artifact)
	}
	got, err := os.ReadFile(artifact.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, archive) {
		t.Fatalf("archive mismatch bytes=%d want=%d", len(got), len(archive))
	}
	if len(runner.calls) != 1 || !strings.Contains(runner.calls[0][len(runner.calls[0])-1], core.DelegatedRunArtifactBeginMarker) {
		t.Fatalf("runner calls=%#v", runner.calls)
	}
}

func TestBlacksmithCollectRunArtifactsBoundsCommandOutput(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	const maxBytes int64 = 64
	oversized := strings.Repeat("x", int(blacksmithArtifactOutputCaptureLimit(maxBytes)+1024))
	runner := &blacksmithFuncRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		if len(req.Args) >= 3 && req.Args[0] == "testbox" && req.Args[1] == "run" && req.Stdout != nil {
			if !req.DisableOutputCapture {
				t.Fatalf("artifact collection should disable command-runner output capture")
			}
			_, _ = req.Stdout.Write([]byte(oversized))
		}
		return LocalCommandResult{}, nil
	}}
	cfg := baseConfig()
	cfg.Blacksmith.Workflow = ".github/workflows/testbox.yml"
	backend := newTestBlacksmithBackend(cfg, runner)
	_, err := backend.CollectRunArtifacts(context.Background(), core.DelegatedRunArtifactRequest{
		RunReq: core.RunRequest{
			Repo:          core.Repo{Root: repo},
			ArtifactGlobs: []string{"reports/**"},
		},
		Result:   core.RunResult{LeaseID: "tbx_artifacts"},
		MaxFiles: 16,
		MaxBytes: maxBytes,
	})
	if err == nil || !strings.Contains(err.Error(), "artifact output too large before archive validation") {
		t.Fatalf("err=%v, want bounded output error", err)
	}
}

func TestBlacksmithRunCollectsArtifactsBeforeOneShotCleanup(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	archive := makeTarGz(t, map[string]string{"reports/manifest.json": `{"ok":true}`})
	runCalls := 0
	runner := &blacksmithFuncRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		if len(req.Args) >= 2 && req.Args[0] == "testbox" && req.Args[1] == "warmup" {
			return LocalCommandResult{Stdout: "ready tbx_artifacts\n"}, nil
		}
		if len(req.Args) >= 2 && req.Args[0] == "testbox" && req.Args[1] == "run" {
			runCalls++
			if runCalls == 2 && req.Stdout != nil {
				_, _ = req.Stdout.Write([]byte("required artifact reports/manifest.json matched=1\n"))
				_, _ = req.Stdout.Write([]byte(core.DelegatedRunArtifactBeginMarker + "\n"))
				_, _ = req.Stdout.Write([]byte(base64.StdEncoding.EncodeToString(archive)))
				_, _ = req.Stdout.Write([]byte("\n" + core.DelegatedRunArtifactEndMarker + "\n"))
			}
			return LocalCommandResult{}, nil
		}
		if len(req.Args) >= 2 && req.Args[0] == "testbox" && req.Args[1] == "stop" {
			return LocalCommandResult{}, nil
		}
		return LocalCommandResult{}, nil
	}}
	cfg := baseConfig()
	cfg.Blacksmith.Workflow = ".github/workflows/testbox.yml"
	backend := newTestBlacksmithBackend(cfg, runner)
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:                  Repo{Root: repo},
		Command:               []string{"true"},
		ArtifactGlobs:         []string{"reports/**"},
		RequiredArtifactGlobs: []string{"reports/manifest.json"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Artifacts) != 1 {
		t.Fatalf("artifacts=%#v", result.Artifacts)
	}
	if len(runner.calls) != 5 {
		t.Fatalf("calls=%#v", runner.calls)
	}
	if runner.calls[0][1] != "list" || runner.calls[1][1] != "warmup" || runner.calls[2][1] != "run" || runner.calls[3][1] != "run" || runner.calls[4][1] != "stop" {
		t.Fatalf("unexpected call order: %#v", runner.calls)
	}
	if !strings.Contains(runner.calls[3][len(runner.calls[3])-1], core.DelegatedRunArtifactBeginMarker) {
		t.Fatalf("second run was not artifact collection: %#v", runner.calls[3])
	}
}

func TestBlacksmithRunArtifactFailureKeepsOneShotOnKeepOnFailure(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Chdir(repo)
	runCalls := 0
	runner := &blacksmithFuncRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		if len(req.Args) >= 2 && req.Args[0] == "testbox" && req.Args[1] == "warmup" {
			return LocalCommandResult{Stdout: "ready tbx_artifactfail\n"}, nil
		}
		if len(req.Args) >= 2 && req.Args[0] == "testbox" && req.Args[1] == "run" {
			runCalls++
			if runCalls == 2 {
				if req.Stderr != nil {
					_, _ = req.Stderr.Write([]byte("missing required artifact reports/manifest.json\n"))
				}
				return LocalCommandResult{ExitCode: 8}, errors.New("artifact missing")
			}
			return LocalCommandResult{}, nil
		}
		if len(req.Args) >= 2 && req.Args[0] == "testbox" && req.Args[1] == "stop" {
			t.Fatalf("stop should not run after artifact failure with keep-on-failure: %#v", req.Args)
		}
		return LocalCommandResult{}, nil
	}}
	var stderr bytes.Buffer
	cfg := baseConfig()
	cfg.Blacksmith.Workflow = ".github/workflows/testbox.yml"
	backend := &blacksmithBackend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		rt:   Runtime{Stdout: io.Discard, Stderr: &stderr, Clock: testClock{}, Exec: runner},
	}
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:                  Repo{Root: repo},
		Command:               []string{"true"},
		RequiredArtifactGlobs: []string{"reports/manifest.json"},
		KeepOnFailure:         true,
	})
	var exitErr ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 7 {
		t.Fatalf("err=%v want artifact exit 7", err)
	}
	if len(runner.calls) != 4 {
		t.Fatalf("blacksmith calls=%d want list/warmup/run/artifact-run without stop: %#v", len(runner.calls), runner.calls)
	}
	if result.Session == nil || !result.Session.Kept {
		t.Fatalf("session=%#v, want kept after artifact failure", result.Session)
	}
	got := stderr.String()
	for _, want := range []string{"blacksmith artifact retrieval failed", "missing required artifact reports/manifest.json", "blacksmith run summary", "exit=7", "failure-bundle local=", "keep-on-failure: kept lease=tbx_artifactfail"} {
		if !strings.Contains(got, want) {
			t.Fatalf("stderr missing %q in:\n%s", want, got)
		}
	}
}

func TestBlacksmithExtractArtifactArchiveRejectsMissingEnvelope(t *testing.T) {
	_, _, err := blacksmithExtractArtifactArchive("no marker", core.DelegatedRunArtifactDefaultMaxBytes)
	if err == nil || !strings.Contains(err.Error(), "did not return a bounded artifact archive") {
		t.Fatalf("err=%v, want missing envelope", err)
	}
}

func TestBlacksmithExtractArtifactArchiveIgnoresPreambleMarkerText(t *testing.T) {
	archive := makeTarGz(t, map[string]string{"reports/manifest.json": `{"ok":true}`})
	output := strings.Join([]string{
		"required artifact " + core.DelegatedRunArtifactBeginMarker + " matched=1",
		"required artifact " + core.DelegatedRunArtifactEndMarker + " matched=1",
		core.DelegatedRunArtifactBeginMarker,
		base64.StdEncoding.EncodeToString(archive),
		core.DelegatedRunArtifactEndMarker,
	}, "\n")
	got, clean, err := blacksmithExtractArtifactArchive(output, core.DelegatedRunArtifactDefaultMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, archive) {
		t.Fatalf("archive mismatch bytes=%d want=%d", len(got), len(archive))
	}
	for _, want := range []string{
		"required artifact " + core.DelegatedRunArtifactBeginMarker + " matched=1",
		"required artifact " + core.DelegatedRunArtifactEndMarker + " matched=1",
	} {
		if !strings.Contains(clean, want) {
			t.Fatalf("clean output missing %q:\n%s", want, clean)
		}
	}
}

func TestBlacksmithExtractArtifactArchiveAllowsExactMaxWithPadding(t *testing.T) {
	archive := bytes.Repeat([]byte("x"), 64)
	output := strings.Join([]string{
		core.DelegatedRunArtifactBeginMarker,
		base64.StdEncoding.EncodeToString(archive),
		core.DelegatedRunArtifactEndMarker,
	}, "\n")
	got, _, err := blacksmithExtractArtifactArchive(output, int64(len(archive)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, archive) {
		t.Fatalf("archive mismatch bytes=%d want=%d", len(got), len(archive))
	}
}

func makeTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		data := []byte(content)
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestBlacksmithKeepOnFailureKeepsTestboxAndWritesBundle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Chdir(t.TempDir())
	runner := &blacksmithFuncRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		if len(req.Args) >= 3 && req.Args[0] == "testbox" && req.Args[1] == "warmup" {
			return LocalCommandResult{Stdout: "ready tbx_keepfail\n"}, nil
		}
		if len(req.Args) >= 3 && req.Args[0] == "testbox" && req.Args[1] == "run" {
			if req.Stdout != nil {
				_, _ = req.Stdout.Write([]byte("delegated stdout\n"))
			}
			if req.Stderr != nil {
				_, _ = req.Stderr.Write([]byte("delegated stderr\n"))
			}
			return LocalCommandResult{ExitCode: 7}, errors.New("exit status 7")
		}
		return LocalCommandResult{}, nil
	}}
	var stderr bytes.Buffer
	cfg := baseConfig()
	cfg.Blacksmith.Workflow = ".github/workflows/testbox.yml"
	backend := &blacksmithBackend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		rt:   Runtime{Stdout: io.Discard, Stderr: &stderr, Clock: testClock{}, Exec: runner},
	}
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:          Repo{Root: "/repo"},
		Command:       []string{"false"},
		KeepOnFailure: true,
	})
	var exitErr ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 7 {
		t.Fatalf("err=%v want exit 7", err)
	}
	if len(runner.calls) != 3 {
		t.Fatalf("blacksmith calls=%d want list/warmup/run without stop: %#v", len(runner.calls), runner.calls)
	}
	if result.Session == nil || !result.Session.Kept {
		t.Fatalf("session=%#v, want kept after keep-on-failure", result.Session)
	}
	got := stderr.String()
	if !strings.Contains(got, "failure-bundle local=") || !strings.Contains(got, "keep-on-failure: kept lease=tbx_keepfail") {
		t.Fatalf("stderr missing bundle/keep hint: %s", got)
	}
	bundle := ""
	for _, field := range strings.Fields(got) {
		if strings.HasPrefix(field, "local=.crabbox/captures/") {
			bundle = strings.TrimPrefix(field, "local=")
			break
		}
	}
	if bundle == "" {
		t.Fatalf("missing bundle path in stderr: %s", got)
	}
	entries := readBlacksmithTarEntries(t, bundle)
	for _, want := range []string{"crabbox-artifacts/stdout.log", "crabbox-artifacts/stderr.log", "crabbox-artifacts/timings.json"} {
		if !entries[want] {
			t.Fatalf("bundle missing %q: %#v", want, entries)
		}
	}
}

func readBlacksmithTarEntries(t *testing.T, path string) map[string]bool {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	entries := make(map[string]bool)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		entries[header.Name] = true
	}
	return entries
}

func TestBlacksmithRunTerminatesSyncStall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_BLACKSMITH_SYNC_TIMEOUT_MS", "1")
	if _, _, err := ensureTestboxKey("tbx_syncstall"); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	backend := &blacksmithBackend{
		spec: Provider{}.Spec(),
		cfg:  baseConfig(),
		rt: Runtime{
			Stdout: io.Discard,
			Stderr: &stderr,
			Clock:  testClock{},
			Exec:   blockingSyncRunner{},
		},
	}
	code := backend.runTestbox(context.Background(), "tbx_syncstall", []string{"pnpm", "test"}, false, false, nil, nil, nil)
	if code != 124 {
		t.Fatalf("exit=%d want 124", code)
	}
	if !strings.Contains(stderr.String(), "Blacksmith Testbox sync did not print a completion marker") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestBlacksmithCollectRunArtifactsTerminatesSyncStall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_BLACKSMITH_SYNC_TIMEOUT_MS", "1")
	if _, _, err := ensureTestboxKey("tbx_artifactstall"); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	backend := &blacksmithBackend{
		spec: Provider{}.Spec(),
		cfg:  baseConfig(),
		rt: Runtime{
			Stdout: io.Discard,
			Stderr: &stderr,
			Clock:  testClock{},
			Exec:   blockingSyncRunner{},
		},
	}
	_, err := backend.CollectRunArtifacts(context.Background(), core.DelegatedRunArtifactRequest{
		RunReq: core.RunRequest{
			ArtifactGlobs: []string{"reports/**"},
		},
		Result: core.RunResult{LeaseID: "tbx_artifactstall"},
	})
	var exitErr ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 124 {
		t.Fatalf("err=%v want exit 124", err)
	}
	if !strings.Contains(stderr.String(), "during artifact retrieval") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestBlacksmithSyncTrackerMatchesCurrentMarkers(t *testing.T) {
	start := time.Unix(100, 0)
	tracker := &blacksmithSyncTracker{}

	tracker.observe("Syncing from repo root: /repo\n", start)
	if !tracker.syncStalled(time.Second, start.Add(2*time.Second)) {
		t.Fatal("sync start marker did not arm stall guard")
	}

	tracker.observe("Changes synced in 2.4s\n", start.Add(500*time.Millisecond))
	if tracker.syncStalled(time.Second, start.Add(3*time.Second)) {
		t.Fatal("sync completion marker did not clear stall guard")
	}
}

func TestBlacksmithSyncTrackerHandlesSplitMarkers(t *testing.T) {
	start := time.Unix(100, 0)
	tracker := &blacksmithSyncTracker{}

	tracker.observe("Syncing from repo", start)
	tracker.observe(" root: /repo\n", start)
	if !tracker.syncStalled(time.Second, start.Add(2*time.Second)) {
		t.Fatal("split sync start marker did not arm stall guard")
	}

	tracker.observe("Changes synced", start.Add(500*time.Millisecond))
	tracker.observe(" in 2.4s\n", start.Add(500*time.Millisecond))
	if tracker.syncStalled(time.Second, start.Add(3*time.Second)) {
		t.Fatal("split sync completion marker did not clear stall guard")
	}
}

func TestBlacksmithBackendUsesInjectedCommandRunnerForListAndStatus(t *testing.T) {
	runner := &blacksmithFuncRunner{fn: func(LocalCommandRequest) (LocalCommandResult, error) {
		return LocalCommandResult{
			Stdout: "tbx_123 ready my-app .github/workflows/testbox.yml test main 2026-05-06T00:00:00Z\n",
		}, nil
	}}
	cfg := baseConfig()
	cfg.Blacksmith.Workflow = ".github/workflows/testbox.yml"
	cfg.Blacksmith.Job = "test"
	cfg.Blacksmith.Ref = "main"
	backend := newTestBlacksmithBackend(cfg, runner)
	servers, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(servers) != 1 || servers[0].CloudID != "tbx_123" {
		t.Fatalf("servers=%#v", servers)
	}
	state, err := backend.Status(context.Background(), StatusRequest{ID: "tbx_123"})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !state.Ready || state.ID != "tbx_123" {
		t.Fatalf("state=%#v", state)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("runner calls=%d, want 2", len(runner.calls))
	}
}

func TestBlacksmithStatusWaitTimeoutMentionsQueuedState(t *testing.T) {
	runner := &blacksmithFuncRunner{fn: func(LocalCommandRequest) (LocalCommandResult, error) {
		return LocalCommandResult{
			Stdout: "tbx_123 queued openclaw .github/workflows/testbox.yml test main 2026-05-06T00:00:00Z\n",
		}, nil
	}}
	backend := newTestBlacksmithBackend(baseConfig(), runner)
	_, err := backend.Status(context.Background(), StatusRequest{ID: "tbx_123", Wait: true, WaitTimeout: -time.Second})
	if err == nil {
		t.Fatal("expected queued timeout")
	}
	for _, want := range []string{"last state queued", "Blacksmith queue may be stalled"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error=%q, want %q", err.Error(), want)
		}
	}
}

func TestBlacksmithStatusWaitReturnsOnContextCancellation(t *testing.T) {
	originalDelay := blacksmithStatusPollDelay
	blacksmithStatusPollDelay = 500 * time.Millisecond
	t.Cleanup(func() { blacksmithStatusPollDelay = originalDelay })

	ctx, cancel := context.WithCancel(context.Background())
	runner := &blacksmithFuncRunner{fn: func(LocalCommandRequest) (LocalCommandResult, error) {
		cancel()
		return LocalCommandResult{
			Stdout: "tbx_123 queued openclaw .github/workflows/testbox.yml test main 2026-05-06T00:00:00Z\n",
		}, nil
	}}
	backend := newTestBlacksmithBackend(baseConfig(), runner)
	started := time.Now()
	_, err := backend.Status(ctx, StatusRequest{ID: "tbx_123", Wait: true, WaitTimeout: time.Minute})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Status err=%v, want context.Canceled", err)
	}
	if elapsed := time.Since(started); elapsed >= 200*time.Millisecond {
		t.Fatalf("Status returned after %s, want prompt cancellation", elapsed)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("runner calls=%d, want one status poll before cancellation", len(runner.calls))
	}
}

func TestBlacksmithBackendListJSONKeepsParsedTableShape(t *testing.T) {
	runner := &blacksmithFuncRunner{fn: func(LocalCommandRequest) (LocalCommandResult, error) {
		return LocalCommandResult{
			Stdout: "tbx_123 ready my-app .github/workflows/testbox.yml test main 2026-05-06T00:00:00Z\n",
		}, nil
	}}
	backend := newTestBlacksmithBackend(baseConfig(), runner)
	view, err := backend.ListJSON(context.Background(), ListRequest{})
	if err != nil {
		t.Fatalf("list json: %v", err)
	}
	items, ok := view.([]blacksmithListItem)
	if !ok {
		t.Fatalf("view=%T, want []blacksmithListItem", view)
	}
	if len(items) != 1 || items[0].ID != "tbx_123" || items[0].Repo != "my-app" {
		t.Fatalf("items=%#v", items)
	}
}

func TestBlacksmithBackendListJSONCanIncludeAllStates(t *testing.T) {
	runner := &blacksmithFuncRunner{fn: func(LocalCommandRequest) (LocalCommandResult, error) {
		return LocalCommandResult{
			Stdout: "tbx_123 hydrating openclaw .github/workflows/testbox.yml test main 2026-05-06T00:00:00Z\n",
		}, nil
	}}
	backend := newTestBlacksmithBackend(baseConfig(), runner)
	view, err := backend.ListJSON(context.Background(), ListRequest{All: true})
	if err != nil {
		t.Fatalf("list json: %v", err)
	}
	items, ok := view.([]blacksmithListItem)
	if !ok {
		t.Fatalf("view=%T, want []blacksmithListItem", view)
	}
	if len(items) != 1 || items[0].Status != "hydrating" {
		t.Fatalf("items=%#v", items)
	}
	if len(runner.calls) != 1 || !containsString(runner.calls[0], "--all") {
		t.Fatalf("calls=%#v, want --all", runner.calls)
	}
}

func TestBlacksmithDoctorListsInventoryOnly(t *testing.T) {
	runner := &blacksmithFuncRunner{fn: func(LocalCommandRequest) (LocalCommandResult, error) {
		return LocalCommandResult{
			Stdout: "tbx_123 ready my-app .github/workflows/testbox.yml test main 2026-05-06T00:00:00Z\n",
		}, nil
	}}
	backend := newTestBlacksmithBackend(baseConfig(), runner)
	result, err := backend.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider != blacksmithTestboxProvider || !strings.Contains(result.Message, "cli=ready control_plane=ready inventory=ready api=list mutation=false leases=1 runtime=ci_hydrated_by_provider") {
		t.Fatalf("result=%#v", result)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls=%#v, want one list", runner.calls)
	}
	want := []string{"testbox", "list"}
	if !reflect.DeepEqual(runner.calls[0], want) {
		t.Fatalf("call=%#v, want %#v", runner.calls[0], want)
	}
}

func TestApplyBlacksmithFlagOverrides(t *testing.T) {
	defaults := baseConfig()
	defaults.Blacksmith = BlacksmithConfig{
		Org:      "default-org",
		Workflow: "default.yml",
		Job:      "default-job",
		Ref:      "main",
	}
	cfg := Config{}
	fs := newFlagSet("test", io.Discard)
	values := registerBlacksmithFlags(fs, defaults)
	if err := parseFlags(fs, []string{
		"--blacksmith-org", "openclaw",
		"--blacksmith-workflow", ".github/workflows/testbox.yml",
		"--blacksmith-job", "test",
		"--blacksmith-ref", "feature",
	}); err != nil {
		t.Fatal(err)
	}
	applyBlacksmithFlagOverrides(&cfg, fs, values)
	if cfg.Blacksmith.Org != "openclaw" || cfg.Blacksmith.Workflow != ".github/workflows/testbox.yml" || cfg.Blacksmith.Job != "test" || cfg.Blacksmith.Ref != "feature" {
		t.Fatalf("blacksmith flags not applied: %#v", cfg.Blacksmith)
	}
}

func TestParseBlacksmithList(t *testing.T) {
	got := parseBlacksmithList(`ID                              STATUS  REPO      WORKFLOW                                JOB    REF   CREATED
tbx_01kqk105g69sp8kcx31h5bgn0e  ready   openclaw  .github/workflows/ci-check-testbox.yml  check  main  2026-05-02T00:22:25.000000Z
`)
	if len(got) != 1 {
		t.Fatalf("items=%d want 1", len(got))
	}
	if got[0].ID != "tbx_01kqk105g69sp8kcx31h5bgn0e" || got[0].Workflow != ".github/workflows/ci-check-testbox.yml" || got[0].Job != "check" {
		t.Fatalf("unexpected item: %#v", got[0])
	}
}

func TestParseBlacksmithListIgnoresEmptyMessage(t *testing.T) {
	got := parseBlacksmithList("No active testboxes (use --all to show all org testboxes)")
	if len(got) != 0 {
		t.Fatalf("items=%d want 0: %#v", len(got), got)
	}
	if got == nil {
		t.Fatal("items=nil want empty slice for JSON []")
	}
}

func TestBlacksmithRunArgs(t *testing.T) {
	cfg := baseConfig()
	cfg.Blacksmith.Org = "openclaw"
	got := blacksmithRunArgs(cfg, "tbx_abc123", "/tmp/key", []string{"OPENCLAW_TESTBOX=1", "pnpm", "check:changed"}, true, false)
	want := []string{
		"--org", "openclaw",
		"testbox", "run",
		"--id", "tbx_abc123",
		"--ssh-private-key", "/tmp/key",
		"--debug",
		"OPENCLAW_TESTBOX='1' 'pnpm' 'check:changed'",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args=%#v want %#v", got, want)
	}
}

func TestBlacksmithCommandString(t *testing.T) {
	tests := []struct {
		name      string
		command   []string
		shellMode bool
		want      string
	}{
		{
			name:    "argv",
			command: []string{"pnpm", "test", "has space"},
			want:    "'pnpm' 'test' 'has space'",
		},
		{
			name:    "env assignment",
			command: []string{"OPENCLAW_TESTBOX=1", "NODE_OPTIONS=--max-old-space-size=4096", "pnpm", "check"},
			want:    "OPENCLAW_TESTBOX='1' NODE_OPTIONS='--max-old-space-size=4096' 'pnpm' 'check'",
		},
		{
			name:    "operator uses shell",
			command: []string{"pnpm", "install", "&&", "pnpm", "test"},
			want:    "'pnpm' 'install' && 'pnpm' 'test'",
		},
		{
			name:    "operator preserves spaced arg",
			command: []string{"printf", "%s\n", "a b", "&&", "echo", "ok"},
			want:    "'printf' '%s\n' 'a b' && 'echo' 'ok'",
		},
		{
			name:      "explicit shell",
			command:   []string{"echo", "hello"},
			shellMode: true,
			want:      "echo hello",
		},
		{
			name:      "explicit multiline shell trims trailing blank suffix",
			command:   []string{"set -e\nrun_case() {\n  printf '%s\\n' \"$1\"\n}\nrun_case ok\n \n"},
			shellMode: true,
			want:      "set -e\nrun_case() {\n  printf '%s\\n' \"$1\"\n}\nrun_case ok",
		},
		{
			name:    "single shell string trims trailing blank suffix",
			command: []string{"pnpm test\n"},
			want:    "pnpm test",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := blacksmithCommandString(tt.command, tt.shellMode); got != tt.want {
				t.Fatalf("command=%q want %q", got, tt.want)
			}
		})
	}
}

func TestParseBlacksmithID(t *testing.T) {
	if got := parseBlacksmithID("ready: tbx_abc-123_more"); got != "tbx_abc-123_more" {
		t.Fatalf("id=%q", got)
	}
	if got := parseBlacksmithID("ready: cbx_abc"); got != "" {
		t.Fatalf("id=%q", got)
	}
}

func TestResolveBlacksmithLeaseID(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if got, err := resolveBlacksmithLeaseID("tbx_raw123", "/repo", false); err != nil || got != "tbx_raw123" {
		t.Fatalf("raw id got=%q err=%v", got, err)
	}
	if err := claimLeaseForRepoProvider("tbx_abc123", "Blue Lobster", blacksmithTestboxProvider, "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	got, err := resolveBlacksmithLeaseID("blue-lobster", "/repo", false)
	if err != nil {
		t.Fatal(err)
	}
	if got != "tbx_abc123" {
		t.Fatalf("id=%q", got)
	}
	if _, err := resolveBlacksmithLeaseID("blue-lobster", "/other", false); err == nil || !strings.Contains(err.Error(), "use --reclaim") {
		t.Fatalf("expected repo claim error, got %v", err)
	}
	if got, err := resolveBlacksmithLeaseID("blue-lobster", "/other", true); err != nil || got != "tbx_abc123" {
		t.Fatalf("reclaim got=%q err=%v", got, err)
	}
}

func TestBlacksmithClaimSlugPreservesExistingSlug(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := claimLeaseForRepoProvider("tbx_abc123", "Blue Lobster", blacksmithTestboxProvider, "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	got, err := blacksmithClaimSlug("tbx_abc123", "tbx_abc123")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Blue Lobster" {
		t.Fatalf("slug=%q", got)
	}
}
