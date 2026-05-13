package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
		{name: "daytona script", provider: "daytona", args: []string{"--script", "testdata/missing.sh"}, want: "daytona delegates run execution; --script is not supported"},
		{name: "e2b fresh pr", provider: "e2b", args: []string{"--fresh-pr", "example-org/my-app#1"}, want: "e2b delegates sync; --fresh-pr is not supported"},
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
	got := remoteCapabilityPreflightCommand("/home/runner/work/repo/repo", map[string]string{"CI": "1"}, []string{"/home/runner/.crabbox/actions/cbx-123.env.sh"})
	for _, want := range []string{
		"cd '/home/runner/work/repo/repo'",
		". '/home/runner/.crabbox/actions/cbx-123.env.sh'",
		"CI='1'",
		"pwd -P",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("preflight command missing %q in %q", want, got)
		}
	}
}

func TestWindowsRemoteCapabilityPreflightCommandUsesCommandEnvironment(t *testing.T) {
	got := windowsRemoteCapabilityPreflightCommand(`C:\crabbox\repo`, map[string]string{"CI": "1"}, []string{`.crabbox\env\run.env`})
	decoded := decodePowerShellCommand(t, got)
	for _, want := range []string{
		`Set-Location -LiteralPath 'C:\crabbox\repo'`,
		`Get-Content -Encoding UTF8 -LiteralPath '.crabbox\env\run.env'`,
		`$env:CI = '1'`,
		`Test-Value "powershell"`,
		`Test-Value "node"`,
		`pwsh=`,
	} {
		if !strings.Contains(decoded, want) {
			t.Fatalf("windows preflight command missing %q in %q", want, decoded)
		}
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
	if commandNeedsHydrationHint([]string{"go", "test", "./..."}, false) {
		t.Fatal("go test should not need hydration hint")
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
