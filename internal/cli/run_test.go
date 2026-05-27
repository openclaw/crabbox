package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func init() {
	RegisterProvider(windowsEnvHelperTestProvider{})
	RegisterProvider(runEnvProfileTestProvider{})
}

type windowsEnvHelperTestProvider struct{}

func (windowsEnvHelperTestProvider) Name() string { return "windows-env-helper-test" }
func (windowsEnvHelperTestProvider) Aliases() []string {
	return nil
}
func (windowsEnvHelperTestProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name: "windows-env-helper-test",
		Kind: ProviderKindSSHLease,
		Targets: []TargetSpec{
			{OS: targetWindows, WindowsMode: windowsModeNormal},
		},
		Features:    FeatureSet{FeatureSSH, FeatureCrabboxSync},
		Coordinator: CoordinatorNever,
	}
}
func (windowsEnvHelperTestProvider) RegisterFlags(*flag.FlagSet, Config) any {
	return noProviderFlags{}
}
func (windowsEnvHelperTestProvider) ApplyFlags(*Config, *flag.FlagSet, any) error {
	return nil
}
func (p windowsEnvHelperTestProvider) Configure(Config, Runtime) (Backend, error) {
	return windowsEnvHelperTestBackend{spec: p.Spec()}, nil
}

type windowsEnvHelperTestBackend struct {
	spec ProviderSpec
}

var windowsEnvHelperTestTouchCount int

func (b windowsEnvHelperTestBackend) Spec() ProviderSpec { return b.spec }
func (b windowsEnvHelperTestBackend) Acquire(context.Context, AcquireRequest) (LeaseTarget, error) {
	return LeaseTarget{
		Server: Server{Provider: b.spec.Name},
		SSH: SSHTarget{
			User:        "crabbox",
			Host:        "203.0.113.10",
			Port:        "22",
			TargetOS:    targetWindows,
			WindowsMode: windowsModeNormal,
		},
		LeaseID: "cbx_win",
	}, nil
}
func (b windowsEnvHelperTestBackend) Resolve(context.Context, ResolveRequest) (LeaseTarget, error) {
	return b.Acquire(context.Background(), AcquireRequest{})
}
func (b windowsEnvHelperTestBackend) List(context.Context, ListRequest) ([]LeaseView, error) {
	return nil, nil
}
func (b windowsEnvHelperTestBackend) ReleaseLease(context.Context, ReleaseLeaseRequest) error {
	return nil
}
func (b windowsEnvHelperTestBackend) Touch(context.Context, TouchRequest) (Server, error) {
	windowsEnvHelperTestTouchCount++
	return Server{Provider: b.spec.Name}, nil
}

type runEnvProfileTestProvider struct{}

func (runEnvProfileTestProvider) Name() string { return "run-env-profile-test" }
func (runEnvProfileTestProvider) Aliases() []string {
	return nil
}
func (runEnvProfileTestProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        "run-env-profile-test",
		Kind:        ProviderKindSSHLease,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Features:    FeatureSet{FeatureSSH, FeatureCrabboxSync},
		Coordinator: CoordinatorNever,
	}
}
func (runEnvProfileTestProvider) RegisterFlags(*flag.FlagSet, Config) any {
	return noProviderFlags{}
}
func (runEnvProfileTestProvider) ApplyFlags(*Config, *flag.FlagSet, any) error {
	return nil
}
func (p runEnvProfileTestProvider) Configure(Config, Runtime) (Backend, error) {
	return runEnvProfileTestBackend{spec: p.Spec()}, nil
}

type runEnvProfileTestBackend struct {
	spec ProviderSpec
}

var runEnvProfileTestReleaseErr error

func (b runEnvProfileTestBackend) Spec() ProviderSpec { return b.spec }
func (b runEnvProfileTestBackend) Acquire(context.Context, AcquireRequest) (LeaseTarget, error) {
	return LeaseTarget{
		Server: Server{Provider: b.spec.Name},
		SSH: SSHTarget{
			User:           "crabbox",
			Host:           "127.0.0.1",
			Port:           os.Getenv("CRABBOX_FAKE_SSH_PORT"),
			TargetOS:       targetLinux,
			SSHConfigProxy: os.Getenv("CRABBOX_FAKE_SSH_PROXY") == "1",
		},
		LeaseID: "cbx_env_profile_test",
	}, nil
}
func (b runEnvProfileTestBackend) Resolve(context.Context, ResolveRequest) (LeaseTarget, error) {
	return b.Acquire(context.Background(), AcquireRequest{})
}
func (b runEnvProfileTestBackend) List(context.Context, ListRequest) ([]LeaseView, error) {
	return nil, nil
}
func (b runEnvProfileTestBackend) ReleaseLease(context.Context, ReleaseLeaseRequest) error {
	return runEnvProfileTestReleaseErr
}
func (b runEnvProfileTestBackend) Touch(context.Context, TouchRequest) (Server, error) {
	return Server{Provider: b.spec.Name}, nil
}

func TestFormatRunSummary(t *testing.T) {
	got := formatRunSummary(runTimings{
		sync:    1200 * time.Millisecond,
		command: 3400 * time.Millisecond,
		syncSteps: syncStepTimings{
			manifest: 20 * time.Millisecond,
			rsync:    900 * time.Millisecond,
		},
		syncSkipped: true,
	}, 5*time.Second, 7)
	for _, want := range []string{
		"run summary",
		"sync=1.2s",
		"command=3.4s",
		"total=5s",
		"sync_skipped=true",
		"exit=7",
		"sync_steps=manifest:20ms,rsync:900ms",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary missing %q in %q", want, got)
		}
	}
}

func TestFormatRunSummaryIncludesGitHydrateSkipReason(t *testing.T) {
	got := formatRunSummary(runTimings{
		sync: 2 * time.Second,
		syncSteps: syncStepTimings{
			gitHydrateSkipped:    true,
			gitHydrateSkipReason: "remote base current",
		},
	}, 3*time.Second, 0)
	if !strings.Contains(got, "git_hydrate:skipped_remote_base_current") {
		t.Fatalf("summary missing git hydrate skip reason: %q", got)
	}
}

func TestFormatRunSummaryNoSync(t *testing.T) {
	got := formatRunSummary(runTimings{
		syncSkipped: true,
	}, 500*time.Millisecond, 0)
	for _, want := range []string{
		"sync=0s",
		"sync_skipped=true",
		"exit=0",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary missing %q in %q", want, got)
		}
	}
}

func TestShouldReplaceLeaseAfterBeforeCommandSSHFailure(t *testing.T) {
	waitErr := exit(5, "timed out waiting for SSH on 203.0.113.10 during before command")
	otherErr := exit(6, "rsync failed")
	tests := []struct {
		name            string
		err             error
		acquired        bool
		useCoordinator  bool
		explicitLeaseID bool
		keep            bool
		keepOnFailure   bool
		noSync          bool
		syncOnly        bool
		stopAfter       string
		requestedSlug   string
		want            bool
	}{
		{name: "fresh coordinator one shot", err: waitErr, acquired: true, useCoordinator: true, want: true},
		{name: "wrong error", err: otherErr, acquired: true, useCoordinator: true},
		{name: "direct backend", err: waitErr, acquired: true},
		{name: "existing lease", err: waitErr, acquired: true, useCoordinator: true, explicitLeaseID: true},
		{name: "kept lease", err: waitErr, acquired: true, useCoordinator: true, keep: true},
		{name: "keep on failure", err: waitErr, acquired: true, useCoordinator: true, keepOnFailure: true},
		{name: "no sync", err: waitErr, acquired: true, useCoordinator: true, noSync: true},
		{name: "sync only", err: waitErr, acquired: true, useCoordinator: true, syncOnly: true},
		{name: "custom slug", err: waitErr, acquired: true, useCoordinator: true, requestedSlug: "qa-smoke"},
		{name: "stop after failure", err: waitErr, acquired: true, useCoordinator: true, stopAfter: "failure", want: true},
		{name: "stop after success", err: waitErr, acquired: true, useCoordinator: true, stopAfter: "success"},
		{name: "stop after never", err: waitErr, acquired: true, useCoordinator: true, stopAfter: "never"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldReplaceLeaseAfterBeforeCommandSSHFailure(tt.err, tt.acquired, tt.useCoordinator, tt.explicitLeaseID, tt.keep, tt.keepOnFailure, tt.noSync, tt.syncOnly, tt.stopAfter, tt.requestedSlug)
			if got != tt.want {
				t.Fatalf("shouldReplaceLeaseAfterBeforeCommandSSHFailure()=%t, want %t", got, tt.want)
			}
		})
	}
}

func TestTimingJSONShape(t *testing.T) {
	var buf bytes.Buffer
	err := writeTimingJSON(&buf, timingReportFromRun("aws", "cbx_123", "blue-crab", runTimings{
		sync:    1200 * time.Millisecond,
		command: 3400 * time.Millisecond,
		syncSteps: syncStepTimings{
			rsync:                900 * time.Millisecond,
			gitHydrateSkipped:    true,
			gitHydrateSkipReason: "marker base current",
		},
		syncSkipped: true,
	}, 5*time.Second, 7))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Provider    string `json:"provider"`
		LeaseID     string `json:"leaseId"`
		SyncMs      int64  `json:"syncMs"`
		CommandMs   int64  `json:"commandMs"`
		TotalMs     int64  `json:"totalMs"`
		ExitCode    int    `json:"exitCode"`
		SyncSkipped bool   `json:"syncSkipped"`
		SyncPhases  []struct {
			Name    string `json:"name"`
			Ms      int64  `json:"ms"`
			Skipped bool   `json:"skipped"`
			Reason  string `json:"reason"`
		} `json:"syncPhases"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Provider != "aws" || got.LeaseID != "cbx_123" || got.SyncMs != 1200 || got.CommandMs != 3400 || got.TotalMs != 5000 || got.ExitCode != 7 || !got.SyncSkipped {
		t.Fatalf("unexpected report: %#v", got)
	}
	if len(got.SyncPhases) != 2 || got.SyncPhases[1].Name != "git_hydrate" || !got.SyncPhases[1].Skipped || got.SyncPhases[1].Reason != "marker base current" {
		t.Fatalf("unexpected phases: %#v", got.SyncPhases)
	}
}

func TestTimingJSONIncludesActionsRunURLWhenAvailable(t *testing.T) {
	var buf bytes.Buffer
	err := writeTimingJSON(&buf, timingReportFromRunWithActionsURL("aws", "cbx_123", "blue-crab", runTimings{
		sync:    1200 * time.Millisecond,
		command: 3400 * time.Millisecond,
	}, 5*time.Second, 0, "https://github.com/openclaw/openclaw/actions/runs/123"))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		ActionsRunURL string `json:"actionsRunUrl"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ActionsRunURL != "https://github.com/openclaw/openclaw/actions/runs/123" {
		t.Fatalf("actionsRunUrl=%q", got.ActionsRunURL)
	}
}

func TestTimingJSONIncludesLabelWhenAvailable(t *testing.T) {
	var buf bytes.Buffer
	report := timingReportFromRun("aws", "cbx_123", "blue-crab", runTimings{}, time.Second, 0)
	report.Label = "update flow smoke"
	if err := writeTimingJSON(&buf, report); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Label string `json:"label"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Label != "update flow smoke" {
		t.Fatalf("label=%q", got.Label)
	}
}

func TestRunCommandRejectsUnsupportedDelegatedCaptureOptions(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		args     []string
		want     string
	}{
		{name: "daytona capture stdout", provider: "daytona", args: []string{"--capture-stdout", "stdout.bin"}, want: "daytona delegates run execution; --capture-stdout is not supported"},
		{name: "islo capture stdout", provider: "islo", args: []string{"--capture-stdout", "stdout.bin"}, want: "islo delegates run execution; --capture-stdout is not supported"},
		{name: "e2b capture stdout", provider: "e2b", args: []string{"--capture-stdout", "stdout.bin"}, want: "e2b delegates run execution; --capture-stdout is not supported"},
		{name: "daytona capture stderr", provider: "daytona", args: []string{"--capture-stderr", "stderr.bin"}, want: "daytona delegates run execution; --capture-stderr is not supported"},
		{name: "islo capture on fail", provider: "islo", args: []string{"--capture-on-fail"}, want: "islo delegates run execution; --capture-on-fail is not supported"},
		{name: "daytona download", provider: "daytona", args: []string{"--download", "/tmp/proof=proof.bin"}, want: "daytona delegates run execution; --download is not supported"},
		{name: "islo download", provider: "islo", args: []string{"--download", "/tmp/proof=proof.bin"}, want: "islo delegates run execution; --download is not supported"},
		{name: "e2b download", provider: "e2b", args: []string{"--download", "/tmp/proof=proof.bin"}, want: "e2b delegates run execution; --download is not supported"},
		{name: "e2b stop after", provider: "e2b", args: []string{"--stop-after", "never"}, want: "e2b delegates run execution; --stop-after is not supported"},
		{name: "daytona script", provider: "daytona", args: []string{"--script", "testdata/missing.sh"}, want: "daytona delegates run execution; --script is not supported"},
		{name: "e2b fresh pr", provider: "e2b", args: []string{"--fresh-pr", "example-org/my-app#1"}, want: "e2b delegates sync; --fresh-pr is not supported"},
		{name: "e2b full resync", provider: "e2b", args: []string{"--full-resync"}, want: "e2b delegates sync; --full-resync is not supported"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			app := App{Stdout: &stdout, Stderr: &stderr}
			args := append([]string{"--provider", tt.provider}, tt.args...)
			args = append(args, "--", "true")
			err := app.runCommand(context.Background(), args)
			var exitErr ExitError
			if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
				t.Fatalf("error=%v, want exit 2", err)
			}
			if !strings.Contains(exitErr.Message, tt.want) {
				t.Fatalf("message=%q want %q", exitErr.Message, tt.want)
			}
		})
	}
}

func TestRunCommandRejectsSlugWithExistingLease(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--id", "blue-lobster",
		"--slug", "update-flow-smoke",
		"--", "true",
	})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("err=%v, want exit 2", err)
	}
	if !strings.Contains(exitErr.Message, "--slug only applies when creating a new lease") {
		t.Fatalf("message=%q", exitErr.Message)
	}
}

func TestRunCommandRejectsDelegatedScriptStdinBeforeReading(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{
		Stdout: &stdout,
		Stderr: &stderr,
		Stdin:  strings.NewReader("should-not-be-consumed"),
	}).runCommand(context.Background(), []string{"--provider", "e2b", "--script-stdin"})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("error=%v, want exit 2", err)
	}
	if !strings.Contains(exitErr.Message, "e2b delegates run execution; --script is not supported") {
		t.Fatalf("message=%q", exitErr.Message)
	}
}

func TestRunCommandRejectsDelegatedEnvHelper(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "env.profile")
	if err := os.WriteFile(profile, []byte("API_TOKEN=secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--provider", "e2b",
		"--allow-env", "API_TOKEN",
		"--env-from-profile", profile,
		"--env-helper", "live",
		"--", "true",
	})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("error=%v, want exit 2", err)
	}
	if !strings.Contains(exitErr.Message, "e2b delegates run execution; --env-helper is not supported") {
		t.Fatalf("message=%q", exitErr.Message)
	}
}

func TestRunCommandRejectsDelegatedProfileDoctor(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, ".crabbox.yaml")
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	if err := os.WriteFile(cfgPath, []byte(`
profiles:
  qa:
    doctor:
      enabled: true
      tools: [node]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--provider", "e2b",
		"--profile", "qa",
		"--", "true",
	})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("error=%v, want exit 2", err)
	}
	if !strings.Contains(exitErr.Message, "e2b delegates run execution; profile doctor is not supported") {
		t.Fatalf("message=%q", exitErr.Message)
	}
}

func TestRunCommandRejectsSyncOnlyScriptStdinBeforeReading(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{
		Stdout: &stdout,
		Stderr: &stderr,
		Stdin:  strings.NewReader("should-not-be-consumed"),
	}).runCommand(context.Background(), []string{"--sync-only", "--script-stdin"})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("error=%v, want exit 2", err)
	}
	if !strings.Contains(exitErr.Message, "--script cannot be combined with --sync-only") {
		t.Fatalf("message=%q", exitErr.Message)
	}
}

func TestRunCommandRejectsEnvHelperWithSyncOnly(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "env.profile")
	if err := os.WriteFile(profile, []byte("API_TOKEN=secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--sync-only",
		"--allow-env", "API_TOKEN",
		"--env-from-profile", profile,
		"--env-helper", "live",
	})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("error=%v, want exit 2", err)
	}
	if !strings.Contains(exitErr.Message, "--env-helper cannot be combined with --sync-only") {
		t.Fatalf("message=%q", exitErr.Message)
	}
}

func TestRunCommandRejectsProofAndArtifactsWithSyncOnly(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "artifact glob",
			args: []string{"--sync-only", "--artifact-glob", ".artifacts/**"},
			want: "--artifact-glob cannot be combined with --sync-only",
		},
		{
			name: "emit proof",
			args: []string{"--sync-only", "--emit-proof", filepath.Join(t.TempDir(), "proof.md")},
			want: "--emit-proof cannot be combined with --sync-only",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			clearConfigEnv(t)
			dir := t.TempDir()
			isolateRunTestUserDirs(t, dir)
			t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, ".crabbox.yaml"))
			var stdout, stderr bytes.Buffer
			err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), tt.args)
			var exitErr ExitError
			if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
				t.Fatalf("error=%v, want exit 2", err)
			}
			if !strings.Contains(exitErr.Message, tt.want) {
				t.Fatalf("message=%q want %q", exitErr.Message, tt.want)
			}
		})
	}
}

func TestRunCommandRejectsTargetOnlyProfileOutputsBeforeLease(t *testing.T) {
	for _, tt := range []struct {
		name   string
		config string
		args   []string
		want   string
	}{
		{
			name: "macos artifacts",
			args: []string{"--provider", "ssh", "--target", "macos", "--artifact-glob", ".artifacts/**", "--", "true"},
			want: "--artifact-glob is not supported for macOS targets",
		},
		{
			name: "native windows doctor",
			config: `
profiles:
  qa:
    doctor:
      enabled: true
      tools: [node]
`,
			args: []string{"--provider", "windows-env-helper-test", "--target", "windows", "--windows-mode", "normal", "--profile", "qa", "--", "true"},
			want: "profile doctor is not supported for native Windows targets",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			clearConfigEnv(t)
			dir := t.TempDir()
			isolateRunTestUserDirs(t, dir)
			cfgPath := filepath.Join(dir, ".crabbox.yaml")
			t.Setenv("CRABBOX_CONFIG", cfgPath)
			if strings.TrimSpace(tt.config) != "" {
				if err := os.WriteFile(cfgPath, []byte(tt.config), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var stdout, stderr bytes.Buffer
			err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), tt.args)
			var exitErr ExitError
			if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
				t.Fatalf("error=%v, want exit 2", err)
			}
			if !strings.Contains(exitErr.Message, tt.want) {
				t.Fatalf("message=%q want %q", exitErr.Message, tt.want)
			}
			if strings.Contains(stderr.String(), "leased ") || strings.Contains(stderr.String(), "claim") {
				t.Fatalf("lease work happened before target rejection: %q", stderr.String())
			}
		})
	}
}

func TestRunCommandRejectsExistingLeaseTargetBeforeTouch(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, ".crabbox.yaml"))
	windowsEnvHelperTestTouchCount = 0
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--provider", "windows-env-helper-test",
		"--target", "windows",
		"--windows-mode", "normal",
		"--id", "cbx_win",
		"--artifact-glob", ".artifacts/**",
		"--", "true",
	})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("error=%v, want exit 2", err)
	}
	if !strings.Contains(exitErr.Message, "--artifact-glob is not supported for native Windows targets") {
		t.Fatalf("message=%q", exitErr.Message)
	}
	if windowsEnvHelperTestTouchCount != 0 {
		t.Fatalf("touch count=%d, want 0", windowsEnvHelperTestTouchCount)
	}
}

func TestRunCommandTimingJSONRemainsFinalLineWithCleanup(t *testing.T) {
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	sshPath := filepath.Join(dir, "ssh")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	_, sshPort, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sshPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_FAKE_SSH_PORT", sshPort)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, ".crabbox.yaml"))

	var stdout, stderr bytes.Buffer
	err = (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--provider", "run-env-profile-test",
		"--no-sync",
		"--timing-json",
		"--stop-after", "success",
		"--", "true",
	})
	if err != nil {
		t.Fatalf("runCommand error=%v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	if len(lines) == 0 {
		t.Fatal("stderr was empty")
	}
	last := lines[len(lines)-1]
	var report TimingReport
	if err := json.Unmarshal([]byte(last), &report); err != nil {
		t.Fatalf("last stderr line is not timing JSON: %q\nfull stderr:\n%s", last, stderr.String())
	}
	if strings.Contains(last, "lease cleanup") {
		t.Fatalf("cleanup log appended to timing JSON: %q", last)
	}
	if report.LeaseStopped == nil || !*report.LeaseStopped {
		t.Fatalf("leaseStopped=%v, want true", report.LeaseStopped)
	}
}

func TestRunCommandTimingJSONSurfacesCleanupFailure(t *testing.T) {
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	sshPath := filepath.Join(dir, "ssh")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	_, sshPort, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sshPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_FAKE_SSH_PORT", sshPort)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, ".crabbox.yaml"))
	runEnvProfileTestReleaseErr = errors.New("release API unavailable")
	t.Cleanup(func() { runEnvProfileTestReleaseErr = nil })

	var stdout, stderr bytes.Buffer
	err = (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--provider", "run-env-profile-test",
		"--no-sync",
		"--timing-json",
		"--stop-after", "success",
		"--", "true",
	})
	if err != nil {
		t.Fatalf("runCommand error=%v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	last := lines[len(lines)-1]
	var report TimingReport
	if err := json.Unmarshal([]byte(last), &report); err != nil {
		t.Fatalf("last stderr line is not timing JSON: %q\nfull stderr:\n%s", last, stderr.String())
	}
	if report.LeaseStopped == nil || *report.LeaseStopped {
		t.Fatalf("leaseStopped=%v, want false", report.LeaseStopped)
	}
	if !strings.Contains(report.LeaseStopErr, "release API unavailable") {
		t.Fatalf("leaseStopError=%q", report.LeaseStopErr)
	}
}

func TestRunCommandCleansEnvProfileWhenProbeFails(t *testing.T) {
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	sshPath := filepath.Join(dir, "ssh")
	logPath := filepath.Join(dir, "ssh.log")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	_, sshPort, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
cmd=""
for arg do
  cmd="$arg"
done
printf '%s\n---\n' "$cmd" >> "$CRABBOX_FAKE_SSH_LOG"
case "$cmd" in
  *"secret=true"*) exit 9 ;;
esac
exit 0
`
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_FAKE_SSH_LOG", logPath)
	t.Setenv("CRABBOX_FAKE_SSH_PORT", sshPort)
	profile := filepath.Join(dir, "env.profile")
	if err := os.WriteFile(profile, []byte("API_TOKEN=secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err = (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--provider", "run-env-profile-test",
		"--no-sync",
		"--allow-env", "API_TOKEN",
		"--env-from-profile", profile,
		"--", "true",
	})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 7 {
		t.Fatalf("error=%v, want exit 7", err)
	}
	if !strings.Contains(exitErr.Message, "probe env profile") {
		t.Fatalf("message=%q", exitErr.Message)
	}
	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(logData), "rm -f --") || !strings.Contains(string(logData), ".crabbox/env/cbx_env_profile_test.env") {
		t.Fatalf("cleanup command missing from ssh log:\n%s", logData)
	}
}

func TestRunCommandHardFailsMissingJSRuntimeBeforeCommand(t *testing.T) {
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	sshPath := filepath.Join(dir, "ssh")
	logPath := filepath.Join(dir, "ssh.log")
	script := `#!/bin/sh
cmd=""
for arg do
  cmd="$arg"
done
printf '%s\n---\n' "$cmd" >> "$CRABBOX_FAKE_SSH_LOG"
case "$cmd" in
  *"command -v"*) printf '` + missingRemoteToolPrefix + `pnpm\n' ;;
esac
exit 0
`
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_FAKE_SSH_LOG", logPath)
	t.Setenv("CRABBOX_FAKE_SSH_PORT", "22")
	t.Setenv("CRABBOX_FAKE_SSH_PROXY", "1")
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--provider", "run-env-profile-test",
		"--no-sync",
		"--keep-on-failure",
		"--", "pnpm", "test:docs",
	})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 5 {
		t.Fatalf("error=%v, want exit 5", err)
	}
	for _, want := range []string{
		"remote raw workspace missing JS runtime tool(s): pnpm",
		"command starts with \"pnpm\"",
		"would fail before project code runs",
	} {
		if !strings.Contains(exitErr.Message, want) {
			t.Fatalf("message missing %q in %q", want, exitErr.Message)
		}
	}
	if strings.Contains(stderr.String(), "running on ") {
		t.Fatalf("remote command should not start after JS preflight failure:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "keep-on-failure: kept lease=cbx_env_profile_test") {
		t.Fatalf("keep-on-failure hint missing:\n%s", stderr.String())
	}
	if strings.Contains(stderr.String(), "releasing cbx_env_profile_test") {
		t.Fatalf("preflight failure should keep lease:\n%s", stderr.String())
	}
	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(logData), "pnpm test:docs") {
		t.Fatalf("user command reached ssh log:\n%s", logData)
	}
}

func TestRunCommandSyncOnlyIgnoresJSCommandRuntime(t *testing.T) {
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	sshPath := filepath.Join(dir, "ssh")
	logPath := filepath.Join(dir, "ssh.log")
	script := `#!/bin/sh
cmd=""
for arg do
  cmd="$arg"
done
printf '%s\n---\n' "$cmd" >> "$CRABBOX_FAKE_SSH_LOG"
case "$cmd" in
  *"command -v"*) printf 'pnpm\n' ;;
esac
exit 0
`
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_FAKE_SSH_LOG", logPath)
	t.Setenv("CRABBOX_FAKE_SSH_PORT", "22")
	t.Setenv("CRABBOX_FAKE_SSH_PROXY", "1")
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--provider", "run-env-profile-test",
		"--no-sync",
		"--sync-only",
		"--", "pnpm", "test",
	})
	if err != nil {
		t.Fatalf("sync-only should ignore command runtime: %v", err)
	}
	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(logData), "command -v") {
		t.Fatalf("sync-only should not probe command runtime:\n%s", logData)
	}
}

func TestRunCommandSkipsJSRuntimePreflightWithForwardedPATH(t *testing.T) {
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	sshPath := filepath.Join(dir, "ssh")
	logPath := filepath.Join(dir, "ssh.log")
	script := `#!/bin/sh
cmd=""
for arg do
  cmd="$arg"
done
printf '%s\n---\n' "$cmd" >> "$CRABBOX_FAKE_SSH_LOG"
case "$cmd" in
  *"command -v"*) printf '` + missingRemoteToolPrefix + `pnpm\n' ;;
esac
exit 0
`
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_FAKE_SSH_LOG", logPath)
	t.Setenv("CRABBOX_FAKE_SSH_PORT", "22")
	t.Setenv("CRABBOX_FAKE_SSH_PROXY", "1")
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--provider", "run-env-profile-test",
		"--no-sync",
		"--allow-env", "PATH",
		"--", "pnpm", "test",
	})
	if err != nil {
		t.Fatalf("forwarded PATH should skip hard runtime preflight: %v", err)
	}
	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(logData), "command -v") {
		t.Fatalf("forwarded PATH should skip command runtime probe:\n%s", logData)
	}
	if !strings.Contains(string(logData), "pnpm") {
		t.Fatalf("user command missing from ssh log:\n%s", logData)
	}
}

func TestValidateRunEnvHelperTargetRejectsNativeWindows(t *testing.T) {
	err := validateRunEnvHelperTarget(SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}, ".crabbox/env/live")
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("error=%v, want exit 2", err)
	}
	if !strings.Contains(exitErr.Message, "--env-helper is not supported for native Windows targets yet") {
		t.Fatalf("message=%q", exitErr.Message)
	}
	if err := validateRunEnvHelperTarget(SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}, ".crabbox/env/live"); err != nil {
		t.Fatalf("wsl2 helper rejected: %v", err)
	}
}

func TestRunCommandRejectsWindowsEnvHelperBeforeRemoteCommands(t *testing.T) {
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	sshPath := filepath.Join(dir, "ssh")
	logPath := filepath.Join(dir, "ssh.log")
	script := `#!/bin/sh
printf 'ssh called\n' >> "$CRABBOX_FAKE_SSH_LOG"
exit 0
`
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_FAKE_SSH_LOG", logPath)
	profile := filepath.Join(dir, "env.profile")
	if err := os.WriteFile(profile, []byte("API_TOKEN=secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--provider", "windows-env-helper-test",
		"--target", "windows",
		"--windows-mode", "normal",
		"--static-host", "203.0.113.10",
		"--static-user", "crabbox",
		"--static-work-root", `C:\crabbox-test`,
		"--no-sync",
		"--allow-env", "API_TOKEN",
		"--env-from-profile", profile,
		"--env-helper", "live",
		"--", "true",
	})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("error=%v, want exit 2", err)
	}
	if !strings.Contains(exitErr.Message, "--env-helper is not supported for native Windows targets yet") {
		t.Fatalf("message=%q", exitErr.Message)
	}
	if _, readErr := os.ReadFile(logPath); !os.IsNotExist(readErr) {
		t.Fatalf("ssh should not run before Windows env-helper rejection, readErr=%v", readErr)
	}
}

func isolateRunTestUserDirs(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", filepath.Join(dir, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "xdg-config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "xdg-state"))
}

func TestFullResyncPrunesEvenWhenDeleteDisabled(t *testing.T) {
	for _, tt := range []struct {
		name       string
		delete     bool
		fullResync bool
		want       bool
	}{
		{name: "normal delete off", want: false},
		{name: "normal delete on", delete: true, want: true},
		{name: "full resync delete off", fullResync: true, want: true},
		{name: "full resync delete on", delete: true, fullResync: true, want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldPruneRemoteSync(tt.delete, tt.fullResync); got != tt.want {
				t.Fatalf("shouldPruneRemoteSync=%t want %t", got, tt.want)
			}
		})
	}
}

func TestFullResyncSeedsPruneManifestFromGit(t *testing.T) {
	if !shouldSeedRemotePruneManifest(false, true) {
		t.Fatal("full-resync should seed old manifest from git before pruning")
	}
	if !shouldSeedRemotePruneManifest(true, false) {
		t.Fatal("hydrated actions workspace should seed old manifest from git before pruning")
	}
	if shouldSeedRemotePruneManifest(false, false) {
		t.Fatal("normal non-hydrated sync should not seed old manifest from git")
	}
}

func TestRunCommandRejectsApplyLocalPatchWithoutFreshPR(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{"--apply-local-patch", "--", "true"})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("error=%v, want exit 2", err)
	}
	if !strings.Contains(exitErr.Message, "--apply-local-patch requires --fresh-pr") {
		t.Fatalf("message=%q", exitErr.Message)
	}
}

func TestRunCommandRejectsFullResyncWithNoSync(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{"--full-resync", "--no-sync", "--", "true"})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("error=%v, want exit 2", err)
	}
	if !strings.Contains(exitErr.Message, "--full-resync cannot be combined with --no-sync") {
		t.Fatalf("message=%q", exitErr.Message)
	}
}

func TestRunCommandPreflightsLocalOutputOptions(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "malformed download", args: []string{"--download", "out.bin", "--", "true"}, want: "--download expects remote=local"},
		{name: "missing capture directory", args: []string{"--capture-stdout", filepath.Join(t.TempDir(), "missing", "stdout.bin"), "--", "true"}, want: "capture stdout:"},
		{name: "missing stderr capture directory", args: []string{"--capture-stderr", filepath.Join(t.TempDir(), "missing", "stderr.bin"), "--", "true"}, want: "capture stderr:"},
		{name: "same capture path", args: []string{"--capture-stdout", "run.log", "--capture-stderr", "run.log", "--", "true"}, want: "paths must be different"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), tt.args)
			var exitErr ExitError
			if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
				t.Fatalf("error=%v, want exit 2", err)
			}
			if !strings.Contains(exitErr.Message, tt.want) {
				t.Fatalf("message=%q want %q", exitErr.Message, tt.want)
			}
		})
	}
}

func TestEnvForwardingSummaryRedactsSecretValues(t *testing.T) {
	t.Setenv("CRABBOX_ENV_ALLOW", "OPENAI_API_KEY,CI")
	var buf bytes.Buffer
	printEnvForwardingSummary(&buf, "aws", "forwarded", []string{"OPENAI_API_KEY", "CI"}, map[string]string{
		"OPENAI_API_KEY": "sk-live-secret",
		"CI":             "1",
	})
	got := buf.String()
	if strings.Contains(got, "sk-live-secret") {
		t.Fatalf("summary leaked value: %s", got)
	}
	for _, want := range []string{
		"provider=aws",
		"behavior=forwarded",
		"OPENAI_API_KEY=set len=14 secret=true",
		"CI=set",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary missing %q in %q", want, got)
		}
	}
}

func TestPhaseMarkerParsingAndTimingJSON(t *testing.T) {
	if name, ok := phaseNameFromLine("CRABBOX_PHASE: install deps"); !ok || name != "install-deps" {
		t.Fatalf("phase=%q ok=%t", name, ok)
	}
	if _, ok := phaseNameFromLine("not a marker"); ok {
		t.Fatal("unexpected phase marker")
	}

	var buf bytes.Buffer
	err := writeTimingJSON(&buf, timingReportFromRun("aws", "cbx_123", "blue-crab", runTimings{
		command: 1500 * time.Millisecond,
		commandPhases: []timingPhase{
			{Name: "install", Ms: 500},
			{Name: "build", Ms: 1000},
		},
	}, 2*time.Second, 1))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		CommandPhases []struct {
			Name string `json:"name"`
			Ms   int64  `json:"ms"`
		} `json:"commandPhases"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.CommandPhases) != 2 || got.CommandPhases[0].Name != "install" || got.CommandPhases[1].Ms != 1000 {
		t.Fatalf("unexpected command phases: %#v", got.CommandPhases)
	}
}

func TestPhaseMarkerWritersKeepStreamBuffersSeparate(t *testing.T) {
	tracker := newCommandPhaseTracker(time.Now())
	stdoutWriter := &phaseMarkerWriter{tracker: tracker}
	stderrWriter := &phaseMarkerWriter{tracker: tracker}

	if _, err := stdoutWriter.Write([]byte("CRABBOX_PHASE:build")); err != nil {
		t.Fatal(err)
	}
	if _, err := stderrWriter.Write([]byte("CRABBOX_PHASE:test\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := stdoutWriter.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}

	phases := tracker.Finish(time.Now())
	names := make(map[string]bool)
	for _, phase := range phases {
		names[phase.Name] = true
	}
	if !names["build"] || !names["test"] {
		t.Fatalf("phases=%#v want independent stdout/stderr markers", phases)
	}
}

func TestPhaseMarkerWriterParsesCompleteLinesBeforePendingTruncation(t *testing.T) {
	tracker := newCommandPhaseTracker(time.Now())
	writer := &phaseMarkerWriter{tracker: tracker}

	if _, err := writer.Write([]byte("CRABBOX_PHASE:build\n" + strings.Repeat("x", phaseMarkerPendingBytes*2))); err != nil {
		t.Fatal(err)
	}
	if len(writer.pending) > phaseMarkerPendingBytes {
		t.Fatalf("pending=%d want <=%d", len(writer.pending), phaseMarkerPendingBytes)
	}

	phases := tracker.Finish(time.Now())
	names := make(map[string]bool)
	for _, phase := range phases {
		names[phase.Name] = true
	}
	if !names["build"] {
		t.Fatalf("phases=%#v want build marker from large chunk", phases)
	}
}

func TestRemotePreflightRawWorkspaceHydrateWarning(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider = "aws"
	cfg.Actions.Workflow = ".github/workflows/hydrate.yml"
	lines := remotePreflightWorkspaceLines(cfg, SSHTarget{TargetOS: targetLinux}, "cbx_123", "/work/crabbox/cbx_123/repo", false, "", true)
	got := strings.Join(lines, "\n")
	for _, want := range []string{
		"workspace=raw",
		"workdir=/work/crabbox/cbx_123/repo",
		"hydrate_supported=true",
		"hydrate_suggestion=crabbox actions hydrate --id cbx_123 --provider aws --target linux --workflow .github/workflows/hydrate.yml",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("preflight text missing %q in %q", want, got)
		}
	}
}

func TestRemotePreflightNativeWindowsHydrateSuggestionUsesGitHubRunner(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider = "aws"
	cfg.TargetOS = targetWindows
	cfg.WindowsMode = windowsModeNormal
	cfg.Actions.Workflow = ".github/workflows/hydrate.yml"
	lines := remotePreflightWorkspaceLines(cfg, SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}, "cbx_123", `C:\crabbox\cbx_123\repo`, false, "", true)
	got := strings.Join(lines, "\n")
	for _, want := range []string{
		"hydrate_supported=true",
		"--target windows",
		"--windows-mode normal",
		"--github-runner",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("preflight text missing %q in %q", want, got)
		}
	}
}

func TestRemotePreflightRawWorkspaceSkipsHydrateSuggestionWithoutWorkflow(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider = "aws"
	lines := remotePreflightWorkspaceLines(cfg, SSHTarget{TargetOS: targetLinux}, "cbx_123", "/work/crabbox/cbx_123/repo", false, "", true)
	got := strings.Join(lines, "\n")
	if strings.Contains(got, "hydrate_suggestion=") {
		t.Fatalf("unexpected hydrate suggestion without workflow: %q", got)
	}
	if !strings.Contains(got, "workspace=raw") {
		t.Fatalf("preflight text missing workspace: %q", got)
	}
}

func TestRemoteCapabilityPreflightCommandUsesCommandEnvironment(t *testing.T) {
	got := remoteCapabilityPreflightCommand("/home/runner/work/repo/repo", map[string]string{"CI": "1"}, []string{"/home/runner/.crabbox/actions/cbx-123.env.sh"}, []string{"node", "bun"})
	for _, want := range []string{
		"cd '/home/runner/work/repo/repo'",
		". '/home/runner/.crabbox/actions/cbx-123.env.sh'",
		"CI='1'",
		"pwd -P",
		`exe="$1"; shift`,
		"preflight_cmd '\\''node'\\'' '\\''node'\\'' node --version",
		"preflight_cmd '\\''bun'\\'' '\\''bun'\\'' bun --version",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("preflight command missing %q in %q", want, got)
		}
	}
}

func TestWindowsRemoteCapabilityPreflightCommandUsesCommandEnvironment(t *testing.T) {
	got := windowsRemoteCapabilityPreflightCommand(`C:\crabbox\repo`, map[string]string{"CI": "1"}, []string{`.crabbox\env\run.env`}, []string{"powershell", "node", "bun", "pwsh"})
	decoded := decodePowerShellCommand(t, got)
	for _, want := range []string{
		`Set-Location -LiteralPath 'C:\crabbox\repo'`,
		`Import-CrabboxEnvFile '.crabbox\env\run.env'`,
		`$env:CI = '1'`,
		`Test-Value "user" { whoami }`,
		`Test-Value "cwd" { (Get-Location).Path }`,
		`Test-Value "powershell"`,
		`Test-Tool 'node' 'node' @('--version')`,
		`Test-Tool 'bun' 'bun' @('--version')`,
		`Test-Tool 'pwsh' 'pwsh' @('--version')`,
	} {
		if !strings.Contains(decoded, want) {
			t.Fatalf("windows preflight command missing %q in %q", want, decoded)
		}
	}
}

func TestPreflightToolsForTargetFiltersByOS(t *testing.T) {
	got := preflightToolsForTarget(SSHTarget{TargetOS: targetMacOS}, []string{"node", "apt", "powershell", "bun"})
	if strings.Join(got, ",") != "node,bun" {
		t.Fatalf("mac tools=%v", got)
	}
	got = preflightToolsForTarget(SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}, []string{"node", "apt", "powershell", "bun"})
	if strings.Join(got, ",") != "node,powershell,bun" {
		t.Fatalf("windows tools=%v", got)
	}
	got = preflightToolsForTarget(SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}, []string{"node", "apt", "powershell", "bun"})
	if strings.Join(got, ",") != "node,apt,bun" {
		t.Fatalf("wsl2 tools=%v", got)
	}
	got = preflightToolsForTarget(SSHTarget{TargetOS: targetLinux}, []string{"none"})
	if len(got) != 0 {
		t.Fatalf("none tools=%v", got)
	}
	got = preflightToolsForTarget(SSHTarget{TargetOS: targetMacOS}, []string{"apt", "powershell"})
	if len(got) != 0 {
		t.Fatalf("unsupported mac tools=%v", got)
	}
}

func TestRemotePreflightNonePrintsWorkspaceOnly(t *testing.T) {
	cfg := defaultConfig()
	cfg.Run.PreflightTools = []string{"none"}
	var out bytes.Buffer
	printRemoteCapabilityPreflight(context.Background(), &out, cfg, SSHTarget{TargetOS: targetLinux}, "cbx_123", "/work/repo", nil, false, "", false, nil)
	got := out.String()
	if !strings.Contains(got, "remote preflight workspace=raw workdir=/work/repo hydrate_supported=false") {
		t.Fatalf("missing workspace summary: %q", got)
	}
	if strings.Contains(got, "remote preflight failed") || strings.Contains(got, "remote preflight user=") || strings.Contains(got, "remote preflight cwd=") {
		t.Fatalf("none should skip remote probes, got %q", got)
	}
}

func TestValidatePreflightToolsRejectsUnknown(t *testing.T) {
	if err := validatePreflightTools([]string{"node", "bogus"}); err == nil {
		t.Fatal("expected unknown preflight tool error")
	}
	if err := validatePreflightTools([]string{"default", "bun"}); err != nil {
		t.Fatalf("default tools should validate: %v", err)
	}
}

func TestDelegatedPreflightPrintsUnsupportedMessage(t *testing.T) {
	var stderr bytes.Buffer
	printDelegatedPreflightUnsupported(&stderr, "e2b")
	got := stderr.String()
	for _, want := range []string{"provider=e2b", "delegated unsupported", "provider owns workspace and command transport"} {
		if !strings.Contains(got, want) {
			t.Fatalf("message missing %q in %q", want, got)
		}
	}
}

func TestRemoteFailureCaptureCommandAvoidsDuplicateDirectoryChildren(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is required for POSIX capture command test")
	}
	workdir := t.TempDir()
	if err := os.Mkdir(filepath.Join(workdir, "test-results"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "test-results", "failure.log"), []byte("failure"), 0o666); err != nil {
		t.Fatal(err)
	}

	command := remoteFailureCaptureCommand(workdir, ".crabbox/capture.tar.gz")
	if out, err := exec.Command("bash", "-lc", command).CombinedOutput(); err != nil {
		t.Fatalf("capture command failed: %v\n%s", err, out)
	}

	file, err := os.Open(filepath.Join(workdir, ".crabbox", "capture.tar.gz"))
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
	counts := make(map[string]int)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		counts[header.Name]++
	}
	if counts["test-results/failure.log"] != 1 {
		t.Fatalf("test-results/failure.log count=%d entries=%#v", counts["test-results/failure.log"], counts)
	}
}

func TestRemoteRemoveFailureCaptureCommandRemovesBundle(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is required for POSIX cleanup command test")
	}
	workdir := t.TempDir()
	captureDir := filepath.Join(workdir, ".crabbox")
	if err := os.Mkdir(captureDir, 0o777); err != nil {
		t.Fatal(err)
	}
	capturePath := filepath.Join(captureDir, "capture.tar.gz")
	if err := os.WriteFile(capturePath, []byte("secret logs"), 0o666); err != nil {
		t.Fatal(err)
	}

	command := remoteRemoveFailureCaptureCommand(workdir, ".crabbox/capture.tar.gz")
	if out, err := exec.Command("bash", "-lc", command).CombinedOutput(); err != nil {
		t.Fatalf("cleanup command failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(capturePath); !os.IsNotExist(err) {
		t.Fatalf("capture bundle should be removed, stat err=%v", err)
	}
}

func TestFailureEnvSummaryRedactsSecretValues(t *testing.T) {
	got := failureEnvSummary([]string{"API_TOKEN", "CI", "MISSING"}, map[string]string{
		"API_TOKEN": "secret-value",
		"CI":        "1",
	})
	if strings.Contains(got, "secret-value") {
		t.Fatalf("summary leaked secret: %s", got)
	}
	for _, want := range []string{
		"API_TOKEN=present len=12 secret=true",
		"CI=present",
		"MISSING=missing",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary missing %q in %q", want, got)
		}
	}
}

func TestWriteLocalFailureBundleIncludesMetadataStreamsAndRemoteFiles(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is required for POSIX capture command test")
	}
	dir := t.TempDir()
	t.Chdir(dir)
	stdoutPath := filepath.Join(dir, "stdout.log")
	stderrPath := filepath.Join(dir, "stderr.log")
	if err := os.WriteFile(stdoutPath, []byte("remote stdout\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stderrPath, []byte("remote stderr\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	remoteWorkdir := filepath.Join(dir, "remote")
	if err := os.MkdirAll(filepath.Join(remoteWorkdir, "test-results"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remoteWorkdir, "test-results", "failure.log"), []byte("failure"), 0o666); err != nil {
		t.Fatal(err)
	}
	command := remoteFailureCaptureCommand(remoteWorkdir, ".crabbox/remote.tar.gz")
	if out, err := exec.Command("bash", "-lc", command).CombinedOutput(); err != nil {
		t.Fatalf("remote capture command failed: %v\n%s", err, out)
	}

	local, _, err := writeLocalFailureBundle("bundle.tar.gz", filepath.Join(remoteWorkdir, ".crabbox", "remote.tar.gz"), FailureCaptureMetadata{
		Provider:   "aws",
		LeaseID:    "cbx_123",
		Slug:       "blue-crab",
		RunID:      "run_123",
		Workdir:    "/work/crabbox/cbx_123/repo",
		ExitCode:   7,
		Timing:     timingReport{Provider: "aws", LeaseID: "cbx_123", ExitCode: 7},
		EnvAllow:   []string{"API_TOKEN"},
		Env:        map[string]string{"API_TOKEN": "secret-value"},
		Config:     Config{Provider: "aws", TargetOS: targetLinux, Class: "standard", IdleTimeout: time.Minute, TTL: time.Hour, WorkRoot: "/work/crabbox"},
		StdoutPath: stdoutPath,
		StderrPath: stderrPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	contents := readTarGzContents(t, local)
	for _, want := range []string{
		"crabbox-artifacts/crabbox-run.json",
		"crabbox-artifacts/timings.json",
		"crabbox-artifacts/env.redacted.txt",
		"crabbox-artifacts/config.redacted.txt",
		"crabbox-artifacts/stdout.log",
		"crabbox-artifacts/stderr.log",
		"crabbox-artifacts/remote/test-results/failure.log",
	} {
		if _, ok := contents[want]; !ok {
			t.Fatalf("bundle missing %q; entries=%#v", want, contents)
		}
	}
	for name, data := range contents {
		if bytes.Contains(data, []byte("secret-value")) {
			t.Fatalf("bundle entry %s leaked secret value", name)
		}
	}
}

func TestNativeWindowsFailureBundleUsesLocalStreams(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	stdoutPath := filepath.Join(dir, "stdout.log")
	stderrPath := filepath.Join(dir, "stderr.log")
	if err := os.WriteFile(stdoutPath, []byte("native stdout\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stderrPath, []byte("native stderr\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	local, _, err := captureFailureBundle(context.Background(), SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}, "C:\\crabbox\\repo", "cbx_win", "run_win", FailureCaptureMetadata{
		Provider:   "aws",
		LeaseID:    "cbx_win",
		RunID:      "run_win",
		Workdir:    "C:\\crabbox\\repo",
		ExitCode:   9,
		Timing:     timingReport{Provider: "aws", LeaseID: "cbx_win", ExitCode: 9},
		Config:     Config{Provider: "aws", TargetOS: targetWindows, WindowsMode: windowsModeNormal},
		StdoutPath: stdoutPath,
		StderrPath: stderrPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	contents := readTarGzContents(t, local)
	if !bytes.Contains(contents["crabbox-artifacts/stdout.log"], []byte("native stdout")) {
		t.Fatalf("stdout missing: %#v", contents["crabbox-artifacts/stdout.log"])
	}
	if !bytes.Contains(contents["crabbox-artifacts/stderr.log"], []byte("native stderr")) {
		t.Fatalf("stderr missing: %#v", contents["crabbox-artifacts/stderr.log"])
	}
	if _, ok := contents["crabbox-artifacts/remote/.crabbox/capture-manifest.txt"]; ok {
		t.Fatalf("native Windows bundle should be local-only: %#v", contents)
	}
}

func TestFailureBundleStreamsLargeStreamFiles(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	stdoutPath := filepath.Join(dir, "stdout.log")
	stderrPath := filepath.Join(dir, "stderr.log")
	stdoutData := bytes.Repeat([]byte("stdout0123456789\n"), 128*1024)
	stderrData := []byte("stderr\n")
	if err := os.WriteFile(stdoutPath, stdoutData, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stderrPath, stderrData, 0o666); err != nil {
		t.Fatal(err)
	}
	local, _, err := writeLocalFailureBundle("large-streams.tar.gz", "", FailureCaptureMetadata{
		Provider:   "aws",
		LeaseID:    "cbx_large",
		RunID:      "run_large",
		ExitCode:   1,
		Timing:     timingReport{Provider: "aws", LeaseID: "cbx_large", ExitCode: 1},
		Config:     Config{Provider: "aws", TargetOS: targetLinux},
		StdoutPath: stdoutPath,
		StderrPath: stderrPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	contents := readTarGzContents(t, local)
	if !bytes.Equal(contents["crabbox-artifacts/stdout.log"], stdoutData) {
		t.Fatalf("stdout data mismatch: got=%d want=%d", len(contents["crabbox-artifacts/stdout.log"]), len(stdoutData))
	}
	if !bytes.Equal(contents["crabbox-artifacts/stderr.log"], stderrData) {
		t.Fatalf("stderr data mismatch: got=%q want=%q", contents["crabbox-artifacts/stderr.log"], stderrData)
	}
}

func TestCappedFailureBundleStreamBoundsImplicitCapture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stream.log")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := NewCappedFailureBundleStream(file)
	chunk := bytes.Repeat([]byte("x"), 1024*1024)
	for i := 0; i < 24; i++ {
		if _, err := writer.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > failureBundleStreamCaptureBytes+256 {
		t.Fatalf("implicit capture grew too large: %d", info.Size())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("failure-bundle stream truncated")) {
		t.Fatalf("missing truncation marker")
	}
}

func readTarGzContents(t *testing.T, path string) map[string][]byte {
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
	entries := make(map[string][]byte)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeRegA {
			data, err := io.ReadAll(tarReader)
			if err != nil {
				t.Fatal(err)
			}
			entries[header.Name] = data
		} else {
			entries[header.Name] = nil
		}
	}
	return entries
}

func TestPrintKeepOnFailureSSHHint(t *testing.T) {
	var buf bytes.Buffer
	cfg := Config{Provider: "aws", IdleTimeout: 30 * time.Minute, TTL: 90 * time.Minute}
	server := Server{Labels: map[string]string{"slug": "blue-crab", "expires_at": "1777777777"}}
	target := SSHTarget{User: "crabbox", Host: "203.0.113.10", Port: "22", Key: "/tmp/key"}
	printKeepOnFailureSSHHint(&buf, cfg, "cbx_123", server, target)
	got := buf.String()
	for _, want := range []string{
		"keep-on-failure: kept lease=cbx_123",
		"ssh: crabbox ssh --provider aws --id blue-crab",
		"ssh-direct:",
		"stop: crabbox stop --provider aws blue-crab",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("hint missing %q in %q", want, got)
		}
	}
}

func TestStreamTailBufferBoundsLongPendingLine(t *testing.T) {
	tail := newStreamTailBuffer(2)
	chunk := strings.Repeat("a", failureTailLineBytes*3)
	if _, err := tail.Write([]byte(chunk)); err != nil {
		t.Fatal(err)
	}
	if len(tail.pending) > failureTailLineBytes+len("[truncated] ") {
		t.Fatalf("pending length=%d", len(tail.pending))
	}
	lines := tail.Lines()
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "[truncated] ") {
		t.Fatalf("unexpected lines: %#v", lines)
	}
	if _, err := tail.Write([]byte("\n" + strings.Repeat("b", failureTailLineBytes*2) + "\n")); err != nil {
		t.Fatal(err)
	}
	lines = tail.Lines()
	if len(lines) != 2 {
		t.Fatalf("lines=%d want 2", len(lines))
	}
	for _, line := range lines {
		if len(line) > failureTailLineBytes+len("[truncated] ") {
			t.Fatalf("line length=%d", len(line))
		}
	}
}

func TestApplyCapacityMarketFlag(t *testing.T) {
	fs := newFlagSet("test", io.Discard)
	market := fs.String("market", "spot", "")
	if err := parseFlags(fs, []string{"--market", "on-demand"}); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	if err := applyCapacityMarketFlag(&cfg, fs, *market); err != nil {
		t.Fatal(err)
	}
	if cfg.Capacity.Market != "on-demand" {
		t.Fatalf("market=%s want on-demand", cfg.Capacity.Market)
	}

	fs = newFlagSet("test", io.Discard)
	market = fs.String("market", "spot", "")
	if err := parseFlags(fs, []string{"--market", "reserved"}); err != nil {
		t.Fatal(err)
	}
	if err := applyCapacityMarketFlag(&cfg, fs, *market); err == nil {
		t.Fatal("expected invalid market failure")
	}
}

func TestApplyLeaseCreateFlagsForExistingAWSMacOSLeaseDefaultsOnDemand(t *testing.T) {
	fs := newFlagSet("test", io.Discard)
	values := registerLeaseCreateFlags(fs, defaultConfig())
	if err := parseFlags(fs, []string{"--provider", "aws", "--target", "macos"}); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.Coordinator = "https://broker.example.test"
	if err := applyLeaseCreateFlagsForLease(&cfg, fs, values, "cbx_123"); err != nil {
		t.Fatal(err)
	}
	if cfg.Capacity.Market != "on-demand" {
		t.Fatalf("market=%s want on-demand", cfg.Capacity.Market)
	}
}

func TestApplyLeaseCreateFlagsForExistingAWSMacOSLeaseRejectsExplicitSpot(t *testing.T) {
	fs := newFlagSet("test", io.Discard)
	values := registerLeaseCreateFlags(fs, defaultConfig())
	if err := parseFlags(fs, []string{"--provider", "aws", "--target", "macos", "--market", "spot"}); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.Coordinator = "https://broker.example.test"
	err := applyLeaseCreateFlagsForLease(&cfg, fs, values, "cbx_123")
	if err == nil || !strings.Contains(err.Error(), "requires --market on-demand") {
		t.Fatalf("err=%v, want explicit spot rejection", err)
	}
}

func TestApplyServerTypeFlagOverridesUsesTargetAwareAWSDefaults(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "macos",
			args: []string{"--provider", "aws", "--target", "macos", "--class", "standard"},
			want: "mac2.metal",
		},
		{
			name: "windows",
			args: []string{"--provider", "aws", "--target", "windows", "--class", "standard"},
			want: "m7i.large",
		},
		{
			name: "windows wsl2",
			args: []string{"--provider", "aws", "--target", "windows", "--windows-mode", "wsl2", "--class", "standard"},
			want: "m8i.large",
		},
		{
			name: "windows mode only",
			args: []string{"--windows-mode", "wsl2"},
			want: "m8i.4xlarge",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				Provider:    "aws",
				TargetOS:    targetWindows,
				WindowsMode: windowsModeNormal,
				Class:       "beast",
				ServerType:  "c7a.48xlarge",
				WorkRoot:    defaultWindowsWorkRoot,
			}
			fs := newFlagSet("test", io.Discard)
			provider := fs.String("provider", cfg.Provider, "")
			class := fs.String("class", cfg.Class, "")
			serverType := fs.String("type", "", "")
			targetFlags := registerTargetFlags(fs, cfg)
			if err := parseFlags(fs, tt.args); err != nil {
				t.Fatal(err)
			}
			cfg.Provider = *provider
			cfg.Class = *class
			if err := applyTargetFlagOverrides(&cfg, fs, targetFlags); err != nil {
				t.Fatal(err)
			}
			applyServerTypeFlagOverrides(&cfg, fs, *serverType)
			if cfg.ServerType != tt.want {
				t.Fatalf("serverType=%q want %q", cfg.ServerType, tt.want)
			}
			if cfg.WindowsMode == windowsModeWSL2 && cfg.WorkRoot != defaultPOSIXWorkRoot {
				t.Fatalf("workRoot=%q want %q", cfg.WorkRoot, defaultPOSIXWorkRoot)
			}
			if cfg.ServerTypeExplicit {
				t.Fatal("ServerTypeExplicit=true, want false")
			}
		})
	}
}

func TestApplyTargetFlagOverridesRefreshesDefaultWorkRoot(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		args []string
		want string
	}{
		{
			name: "native windows to wsl2",
			cfg: Config{
				TargetOS:    targetWindows,
				WindowsMode: windowsModeNormal,
				WorkRoot:    defaultWindowsWorkRoot,
			},
			args: []string{"--windows-mode", "wsl2"},
			want: defaultPOSIXWorkRoot,
		},
		{
			name: "wsl2 to native windows",
			cfg: Config{
				TargetOS:    targetWindows,
				WindowsMode: windowsModeWSL2,
				WorkRoot:    defaultPOSIXWorkRoot,
			},
			args: []string{"--windows-mode", "normal"},
			want: defaultWindowsWorkRoot,
		},
		{
			name: "custom root is preserved",
			cfg: Config{
				TargetOS:    targetWindows,
				WindowsMode: windowsModeNormal,
				WorkRoot:    `/custom/root`,
			},
			args: []string{"--windows-mode", "wsl2"},
			want: `/custom/root`,
		},
		{
			name: "linux to macos",
			cfg: Config{
				Provider:    "aws",
				TargetOS:    targetLinux,
				WindowsMode: windowsModeNormal,
				SSHUser:     baseConfig().SSHUser,
				WorkRoot:    defaultPOSIXWorkRoot,
			},
			args: []string{"--target", "macos"},
			want: defaultMacOSWorkRoot,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newFlagSet("test", io.Discard)
			targetFlags := registerTargetFlags(fs, tt.cfg)
			if err := parseFlags(fs, tt.args); err != nil {
				t.Fatal(err)
			}
			cfg := tt.cfg
			if err := applyTargetFlagOverrides(&cfg, fs, targetFlags); err != nil {
				t.Fatal(err)
			}
			if cfg.WorkRoot != tt.want {
				t.Fatalf("workRoot=%q want %q", cfg.WorkRoot, tt.want)
			}
		})
	}
}

func TestApplyServerTypeFlagOverridesPreservesExplicitType(t *testing.T) {
	cfg := Config{
		Provider:    "aws",
		TargetOS:    targetLinux,
		WindowsMode: windowsModeNormal,
		Class:       "beast",
		ServerType:  "c7a.48xlarge",
	}
	fs := newFlagSet("test", io.Discard)
	provider := fs.String("provider", cfg.Provider, "")
	class := fs.String("class", cfg.Class, "")
	serverType := fs.String("type", "", "")
	targetFlags := registerTargetFlags(fs, cfg)
	if err := parseFlags(fs, []string{"--provider", "aws", "--target", "macos", "--class", "standard", "--type", "mac1.metal"}); err != nil {
		t.Fatal(err)
	}
	cfg.Provider = *provider
	cfg.Class = *class
	if err := applyTargetFlagOverrides(&cfg, fs, targetFlags); err != nil {
		t.Fatal(err)
	}
	applyServerTypeFlagOverrides(&cfg, fs, *serverType)
	if cfg.ServerType != "mac1.metal" {
		t.Fatalf("serverType=%q want mac1.metal", cfg.ServerType)
	}
	if !cfg.ServerTypeExplicit {
		t.Fatal("ServerTypeExplicit=false, want true")
	}
}

func TestCommandNeedsHydrationHint(t *testing.T) {
	if !commandNeedsHydrationHint([]string{"env NODE_OPTIONS=--max-old-space-size=4096 pnpm test"}, true) {
		t.Fatal("expected shell pnpm command to need hydration hint")
	}
	if !commandNeedsHydrationHint([]string{"pnpm", "test:docs"}, false) {
		t.Fatal("expected pnpm docs command to need hydration hint")
	}
	if !commandNeedsHydrationHint([]string{"node", "scripts/check.mjs"}, false) {
		t.Fatal("expected node script command to need hydration hint")
	}
	if commandNeedsHydrationHint([]string{"go", "test", "./..."}, false) {
		t.Fatal("go test should not need hydration hint")
	}
}

func TestShouldAutoHydrateActions(t *testing.T) {
	cfg := defaultConfig()
	cfg.Actions.Workflow = ".github/workflows/hydrate.yml"
	if !shouldAutoHydrateActions(cfg, false, false, FreshPRSpec{}, false) {
		t.Fatal("configured workflow should auto-hydrate normal runs")
	}
	if shouldAutoHydrateActions(cfg, true, false, FreshPRSpec{}, false) {
		t.Fatal("--no-hydrate should disable auto hydration")
	}
	if shouldAutoHydrateActions(cfg, false, true, FreshPRSpec{}, false) {
		t.Fatal("--no-sync should disable auto hydration")
	}
	if shouldAutoHydrateActions(cfg, false, false, FreshPRSpec{Owner: "example-org", Repo: "my-app", Number: 1}, false) {
		t.Fatal("--fresh-pr should disable auto hydration")
	}
	if shouldAutoHydrateActions(cfg, false, false, FreshPRSpec{}, true) {
		t.Fatal("--sync-only should disable auto hydration")
	}
	cfg.Actions.Workflow = ""
	if shouldAutoHydrateActions(cfg, false, false, FreshPRSpec{}, false) {
		t.Fatal("missing workflow should disable auto hydration")
	}
}

func TestCommandRuntimePreflightToolsFocusesEntrypoint(t *testing.T) {
	if got := strings.Join(commandRuntimePreflightTools([]string{"env CI=1 pnpm test"}, true), ","); got != "pnpm" {
		t.Fatalf("tools=%q want pnpm", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"env -u NODE_OPTIONS pnpm test"}, true), ","); got != "pnpm" {
		t.Fatalf("tools=%q want pnpm through env -u", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"env", "--unset=NODE_OPTIONS", "pnpm", "test"}, false), ","); got != "pnpm" {
		t.Fatalf("tools=%q want pnpm through env --unset", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"/usr/bin/env", "pnpm", "test"}, false), ","); got != "pnpm" {
		t.Fatalf("tools=%q want pnpm through /usr/bin/env", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"/opt/node/bin/pnpm", "test"}, false), ","); got != "/opt/node/bin/pnpm" {
		t.Fatalf("tools=%q want explicit pnpm path", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"./scripts/pnpm", "test"}, false), ","); got != "" {
		t.Fatalf("repo-relative wrapper should not be preflight-blocked, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"env PATH=/opt/node/bin:$PATH pnpm test"}, true), ","); got != "" {
		t.Fatalf("custom PATH command should not be preflight-blocked, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"export PATH=/opt/node/bin:$PATH; pnpm test"}, true), ","); got != "" {
		t.Fatalf("export PATH setup should not be preflight-blocked, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"bash", "-lc", "source ~/.nvm/nvm.sh && pnpm test"}, false), ","); got != "" {
		t.Fatalf("bash setup wrapper should not be preflight-blocked, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"sudo apt-get update && sudo apt-get install -y nodejs npm && npm test"}, true), ","); got != "" {
		t.Fatalf("sudo runtime setup command should not be preflight-blocked, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"corepack enable && pnpm install"}, true), ","); got != "" {
		t.Fatalf("corepack setup command should not be preflight-blocked, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"npm install -g pnpm && pnpm test"}, true), ","); got != "" {
		t.Fatalf("npm global setup command should not be preflight-blocked, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"cd web && pnpm test"}, true), ","); got != "pnpm" {
		t.Fatalf("shell cd prefix should still preflight pnpm, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"echo starting; pnpm install"}, true), ","); got != "pnpm" {
		t.Fatalf("shell echo prefix should still preflight pnpm, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{`echo "ok&&pnpm"`}, true), ","); got != "" {
		t.Fatalf("quoted shell separator should not expose pnpm preflight, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"echo 'ok; pnpm'"}, true), ","); got != "" {
		t.Fatalf("quoted semicolon should not expose pnpm preflight, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"node --version && pnpm test"}, true), ","); got != "node,pnpm" {
		t.Fatalf("multi-segment JS command should preflight node and pnpm, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"pnpm test && bash scripts/post.sh"}, true), ","); got != "pnpm" {
		t.Fatalf("later setup wrapper should not erase earlier pnpm preflight, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"pnpm test && curl -fsSL https://example.invalid/setup.sh | bash"}, true), ","); got != "pnpm" {
		t.Fatalf("later installer command should not erase earlier pnpm preflight, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"sudo", "-E", "pnpm", "test"}, false), ","); got != "pnpm" {
		t.Fatalf("sudo JS command should preflight pnpm, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"sudo", "env", "CI=1", "pnpm", "test"}, false), ","); got != "pnpm" {
		t.Fatalf("sudo env JS command should preflight pnpm, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"sudo", "CI=1", "pnpm", "test"}, false), ","); got != "pnpm" {
		t.Fatalf("sudo assignment JS command should preflight pnpm, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"apt-get update && apt-get install -y nodejs npm && npm test"}, true), ","); got != "" {
		t.Fatalf("runtime setup command should not be preflight-blocked, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"pnpm --version || npm --version"}, true), ","); got != "" {
		t.Fatalf("shell fallback command should not be preflight-blocked, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"pnpm --version||npm --version"}, true), ","); got != "" {
		t.Fatalf("compact shell fallback command should not be preflight-blocked, got %q", got)
	}
	if got := strings.Join(commandRuntimePreflightTools([]string{"pnpm", "--version", "||", "npm", "--version"}, false), ","); got != "" {
		t.Fatalf("argv fallback command should not be preflight-blocked, got %q", got)
	}
}

func TestRunEnvProvidesPathHandlesWindowsCasing(t *testing.T) {
	if !runEnvProvidesPath(map[string]string{"PATH": "/opt/node/bin"}, SSHTarget{TargetOS: targetLinux}) {
		t.Fatal("POSIX PATH should skip runtime preflight")
	}
	if runEnvProvidesPath(map[string]string{"Path": "/opt/node/bin"}, SSHTarget{TargetOS: targetLinux}) {
		t.Fatal("POSIX Path should not skip runtime preflight")
	}
	if !runEnvProvidesPath(map[string]string{"Path": `C:\node`}, SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}) {
		t.Fatal("native Windows Path should skip runtime preflight")
	}
}

func TestRemoteMissingToolsCommandUsesLoginShell(t *testing.T) {
	got := remoteMissingToolsCommand([]string{"pnpm"})
	if !strings.HasPrefix(got, "bash -lc ") {
		t.Fatalf("command=%q want bash -lc wrapper", got)
	}
	if !strings.Contains(got, "command -v") {
		t.Fatalf("command=%q want command -v probe", got)
	}
	if !strings.Contains(got, missingRemoteToolPrefix) {
		t.Fatalf("command=%q want missing tool sentinel", got)
	}
}

func TestParseMissingRemoteToolsOutputIgnoresShellNoise(t *testing.T) {
	got := strings.Join(parseMissingRemoteToolsOutput("Welcome\n"+missingRemoteToolPrefix+"pnpm\nwarning\n"+missingRemoteToolPrefix+"pnpm\n"), ",")
	if got != "pnpm" {
		t.Fatalf("missing=%q want pnpm", got)
	}
}

func TestRawJSRuntimeMissingErrorMessage(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider = "aws"
	cfg.Actions.Workflow = ".github/workflows/hydrate.yml"
	err := rawJSRuntimeMissingError(cfg, []string{"pnpm"}, []string{"pnpm", "test:docs"}, false, "crabbox actions hydrate --id cbx_123 --provider aws")
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 5 {
		t.Fatalf("error=%v, want exit 5", err)
	}
	for _, want := range []string{
		"remote raw workspace missing JS runtime tool(s): pnpm",
		"command starts with \"pnpm\"",
		"hydrate first: crabbox actions hydrate --id cbx_123 --provider aws",
		"include Node/Corepack/package-manager setup",
		"provider/image with the JS toolchain",
	} {
		if !strings.Contains(exitErr.Message, want) {
			t.Fatalf("message missing %q in %q", want, exitErr.Message)
		}
	}
}

func TestRawJSRuntimeHydrateSuggestionAvoidsReleasedLease(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider = "aws"
	cfg.Actions.Workflow = ".github/workflows/hydrate.yml"
	target := SSHTarget{TargetOS: targetLinux}
	got := rawJSRuntimeHydrateSuggestion(cfg, target, "cbx_released", true, false, false)
	if strings.Contains(got, "cbx_released") || !strings.Contains(got, "--keep") {
		t.Fatalf("suggestion=%q should not target released lease", got)
	}
	got = rawJSRuntimeHydrateSuggestion(cfg, target, "cbx_kept", true, true, false)
	if !strings.Contains(got, "cbx_kept") {
		t.Fatalf("suggestion=%q should target kept lease", got)
	}
}

func TestPrintCommandNotFoundHint(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider = "aws"
	cfg.Actions.Workflow = ".github/workflows/hydrate.yml"
	var out bytes.Buffer
	printCommandNotFoundHint(&out, cfg, SSHTarget{TargetOS: targetLinux}, "cbx_123", []string{"pnpm", "test"}, false, 127, false, "crabbox actions hydrate --id cbx_123 --provider aws")
	got := out.String()
	for _, want := range []string{"exit 127", "pnpm", "crabbox actions hydrate --id cbx_123"} {
		if !strings.Contains(got, want) {
			t.Fatalf("hint missing %q in %q", want, got)
		}
	}
}

func TestPrintCommandNotFoundHintAvoidsReleasedLease(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider = "aws"
	cfg.Actions.Workflow = ".github/workflows/hydrate.yml"
	var out bytes.Buffer
	suggestion := rawJSRuntimeHydrateSuggestion(cfg, SSHTarget{TargetOS: targetLinux}, "cbx_released", true, false, false)
	printCommandNotFoundHint(&out, cfg, SSHTarget{TargetOS: targetLinux}, "cbx_released", []string{"pnpm", "test"}, false, 127, false, suggestion)
	got := out.String()
	if strings.Contains(got, "cbx_released") || !strings.Contains(got, "--keep") {
		t.Fatalf("hint should avoid released lease and suggest --keep, got %q", got)
	}
}

func TestRecordRunFailureCapturesShadowedReturnErrors(t *testing.T) {
	var recorded error
	func() {
		if err := errors.New("sync failed"); err != nil {
			_ = recordRunFailure(&recorded, err)
			return
		}
	}()
	if recorded == nil || recorded.Error() != "sync failed" {
		t.Fatalf("recorded=%v", recorded)
	}
	_ = recordRunFailure(&recorded, nil)
	if recorded == nil || recorded.Error() != "sync failed" {
		t.Fatalf("nil failure should not clear recorded error, got %v", recorded)
	}
}

func TestLocalContainerDockerSocketSyncUsesResolvedLabels(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider = "local-container"
	server := Server{
		Provider: "local-container",
		Labels:   map[string]string{"docker_socket": "1"},
	}
	if !localContainerDockerSocketSync(cfg, server) {
		t.Fatal("socket-enabled resolved lease should sync without preserving mtimes")
	}
	server.Labels["docker_socket"] = "0"
	cfg.LocalContainer.DockerSocket = true
	if !localContainerDockerSocketSync(cfg, server) {
		t.Fatal("socket-enabled config should sync without preserving mtimes")
	}
	cfg.Provider = "aws"
	server.Provider = "aws"
	if localContainerDockerSocketSync(cfg, server) {
		t.Fatal("non-local-container provider should preserve normal rsync defaults")
	}
}

func TestApplyResolvedServerConfigRestoresLocalContainerSocketConfig(t *testing.T) {
	cfg := defaultConfig()
	server := Server{
		Provider: "local-container",
		Labels: map[string]string{
			"docker_socket": "1",
			"work_root":     "/tmp/crabbox-local-container-work",
		},
	}
	applyResolvedServerConfig(&cfg, server)
	if cfg.Provider != "local-container" || !cfg.LocalContainer.DockerSocket {
		t.Fatalf("local-container socket labels not restored: provider=%s local=%#v", cfg.Provider, cfg.LocalContainer)
	}
	if cfg.WorkRoot != server.Labels["work_root"] || cfg.LocalContainer.WorkRoot != server.Labels["work_root"] {
		t.Fatalf("work roots not restored: workRoot=%q local=%q", cfg.WorkRoot, cfg.LocalContainer.WorkRoot)
	}
	if !localContainerDockerSocketConfig(cfg) {
		t.Fatal("restored socket config should use no-times sync")
	}
}
