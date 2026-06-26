package windowssandbox

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderSpec(t *testing.T) {
	spec := Provider{}.Spec()
	if spec.Name != providerName {
		t.Fatalf("Name=%q", spec.Name)
	}
	if spec.Kind != core.ProviderKindDelegatedRun {
		t.Fatalf("Kind=%q, want delegated-run", spec.Kind)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetWindows || spec.Targets[0].WindowsMode != core.WindowsModeNormal {
		t.Fatalf("Targets=%#v, want native Windows only", spec.Targets)
	}
	if spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("Coordinator=%q, want never", spec.Coordinator)
	}
	if !spec.Features.Has(core.FeatureArchiveSync) {
		t.Fatalf("Features=%#v, want archive sync support", spec.Features)
	}
}

func TestApplyDefaultsSelectsNativeWindowsAndSecureWSBDefaults(t *testing.T) {
	cfg := Config{}
	applyDefaults(&cfg)
	if cfg.Provider != providerName {
		t.Fatalf("Provider=%q", cfg.Provider)
	}
	if cfg.TargetOS != core.TargetWindows || cfg.WindowsMode != core.WindowsModeNormal {
		t.Fatalf("target=%s mode=%s, want native Windows", cfg.TargetOS, cfg.WindowsMode)
	}
	if cfg.WindowsSandbox.Networking != "Enable" {
		t.Fatalf("Networking=%q", cfg.WindowsSandbox.Networking)
	}
	for name, got := range map[string]string{
		"vgpu":               cfg.WindowsSandbox.VGPU,
		"clipboard":          cfg.WindowsSandbox.Clipboard,
		"audioInput":         cfg.WindowsSandbox.AudioInput,
		"videoInput":         cfg.WindowsSandbox.VideoInput,
		"printerRedirection": cfg.WindowsSandbox.PrinterRedirection,
	} {
		if got != "Disable" {
			t.Fatalf("%s=%q, want Disable", name, got)
		}
	}
}

func TestWindowsSandboxConfigXML(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.WindowsSandbox.Workdir = `C:\work\repo`
	cfg.WindowsSandbox.Networking = "Disable"
	cfg.WindowsSandbox.VGPU = "Disable"
	cfg.WindowsSandbox.Clipboard = "Disable"
	cfg.WindowsSandbox.ProtectedClient = "Default"
	cfg.WindowsSandbox.AudioInput = "Disable"
	cfg.WindowsSandbox.VideoInput = "Disable"
	cfg.WindowsSandbox.PrinterRedirection = "Disable"
	cfg.WindowsSandbox.MemoryMB = 4096
	run := preparedRun{
		hostWorkspace: `C:\host\workspace`,
		hostControl:   `C:\host\control`,
	}
	data, err := windowsSandboxConfigXML(cfg, run)
	if err != nil {
		t.Fatal(err)
	}
	xml := string(data)
	for _, want := range []string{
		"<Configuration>",
		"<Networking>Disable</Networking>",
		"<vGPU>Disable</vGPU>",
		"<ClipboardRedirection>Disable</ClipboardRedirection>",
		"<MemoryInMB>4096</MemoryInMB>",
		"<HostFolder>C:\\host\\workspace</HostFolder>",
		"<SandboxFolder>C:\\work\\repo</SandboxFolder>",
		"<HostFolder>C:\\host\\control</HostFolder>",
		"<SandboxFolder>C:\\crabbox-control</SandboxFolder>",
		"<Command>powershell.exe -NoProfile -ExecutionPolicy Bypass -File C:\\crabbox-control\\run.ps1</Command>",
	} {
		if !strings.Contains(xml, want) {
			t.Fatalf("wsb xml missing %q:\n%s", want, xml)
		}
	}
}

func TestSandboxRunScriptQuotesCommandEnvAndKeepOnFailure(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.WindowsSandbox.Workdir = `C:\work\repo`
	script, err := sandboxRunScript(cfg, RunRequest{
		Command:       []string{"pwsh", "-NoProfile", "-Command", "Write-Output 'hi'"},
		KeepOnFailure: true,
		Env: map[string]string{
			"CI":     "true",
			"SECRET": "can't leak",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Set-Location -LiteralPath 'C:\\work\\repo'",
		"$env:CI = 'true'",
		"$env:SECRET = 'can''t leak'",
		"& 'pwsh' '-NoProfile' '-Command' 'Write-Output ''hi''' 1>> $stdout 2>> $stderr",
		"$keepOnFailure = $true",
		"$canceled = Test-Path -LiteralPath 'C:\\crabbox-control\\cancel.txt'",
		"if ($canceled -and $exitCode -eq 0) { $exitCode = 130 }",
		"Set-Content -LiteralPath $exitFile -Value $exitCode -Encoding ASCII",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q:\n%s", want, script)
		}
	}
}

func TestHostRunnerScriptStopsActualSandboxProcessOnTimeout(t *testing.T) {
	script := hostRunnerScript()
	for _, want := range []string{
		"function Get-SandboxProcesses",
		"function Stop-SandboxSession",
		"function Wait-SandboxSession",
		"Get-Process -Name " + windowsSandboxProcessNames + " -ErrorAction SilentlyContinue",
		"$sandboxExe = (Get-Command WindowsSandbox.exe -ErrorAction Stop).Source",
		"Start-Process -FilePath $sandboxExe",
		"$startupGraceSeconds = [Math]::Min($TimeoutSeconds, 120)",
		"$sandboxSeen = $false",
		"Wait-SandboxSession 20",
		"Get-SandboxProcesses | Stop-Process -Force",
		"Test-Path -LiteralPath $cancelFile",
		"Windows Sandbox run canceled.",
		"exit 130",
		"if (-not $sandboxSeen)",
		"Windows Sandbox launcher exited before any sandbox process was observed",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("host runner missing %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "Write-Error") {
		t.Fatalf("host runner must not terminate before explicit lifecycle exit codes:\n%s", script)
	}
	for _, processName := range strings.Split(windowsSandboxProcessNames, ",") {
		if !strings.Contains(script, processName) {
			t.Fatalf("host runner missing sandbox process name %q:\n%s", processName, script)
		}
	}
}

func TestStopCanceledSandboxStopsLegacyAnd24H2Processes(t *testing.T) {
	runner := &recordingRunner{}
	be := &backend{rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	be.stopCanceledSandbox(context.Background())

	if runner.name != "powershell.exe" {
		t.Fatalf("cleanup runner name=%q", runner.name)
	}
	if !runner.disableOutputCapture {
		t.Fatal("cleanup runner must stream without retaining output")
	}
	joined := strings.Join(runner.args, "\n")
	wantCommand := "Get-Process -Name " + windowsSandboxProcessNames + " -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue"
	if !strings.Contains(joined, wantCommand) {
		t.Fatalf("cleanup command missing %q: %#v", wantCommand, runner.args)
	}
	for _, processName := range strings.Split(windowsSandboxProcessNames, ",") {
		if !strings.Contains(joined, processName) {
			t.Fatalf("cleanup command missing sandbox process name %q: %#v", processName, runner.args)
		}
	}
}

func TestHostRunnerScriptReadsOnlyAppendedLogBytes(t *testing.T) {
	script := hostRunnerScript()
	for _, want := range []string{
		"[System.IO.File]::Open",
		"$stream.Seek($Offset.Value",
		"[System.IO.StreamReader]::new",
		"catch [System.IO.IOException]",
		"catch [System.UnauthorizedAccessException]",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("host runner missing %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "Get-Content -LiteralPath $Path -Raw") {
		t.Fatalf("host runner still rereads complete logs:\n%s", script)
	}
}

func TestHostRunnerScriptHonorsKeepFlagsOnTimeout(t *testing.T) {
	script := hostRunnerScript()
	timeoutBranch := "if ((Get-Date) -gt $deadline) {\r\n    if (-not ($Keep.IsPresent -or $KeepOnFailure.IsPresent)) {\r\n      Stop-SandboxSession\r\n    }\r\n    [Console]::Error.WriteLine(\"Windows Sandbox timed out after ${TimeoutSeconds}s\")\r\n    exit 124\r\n  }"
	if !strings.Contains(script, timeoutBranch) {
		t.Fatalf("host runner timeout branch must gate Stop-SandboxSession on keep flags:\n%s", script)
	}
}

func TestGeneratedPowerShellScriptsParse(t *testing.T) {
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("powershell.exe not available")
	}
	cfg := core.BaseConfig()
	sandboxScript, err := sandboxRunScript(cfg, RunRequest{Command: []string{"cmd.exe", "/c", "echo ok"}})
	if err != nil {
		t.Fatal(err)
	}
	for name, script := range map[string]string{
		"host-runner.ps1": hostRunnerScript(),
		"run.ps1":         sandboxScript,
	} {
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}
		parse := `$tokens = $null; $errors = $null; [System.Management.Automation.Language.Parser]::ParseFile(` +
			psSingleQuote(path) +
			`, [ref]$tokens, [ref]$errors) | Out-Null; if ($errors.Count -gt 0) { $errors | ForEach-Object { [Console]::Error.WriteLine($_.ToString()) }; exit 1 }`
		if output, err := exec.Command(powershell, "-NoProfile", "-NonInteractive", "-Command", parse).CombinedOutput(); err != nil {
			t.Fatalf("%s parse failed: %v\n%s", name, err, output)
		}
	}
}

func TestCleanWindowsSandboxPathUsesWindowsSemantics(t *testing.T) {
	got, err := cleanWindowsSandboxPath(`c:/repo/../work/./project`)
	if err != nil {
		t.Fatal(err)
	}
	if got != `C:\work\project` {
		t.Fatalf("clean path=%q, want C:\\work\\project", got)
	}

	if _, err := cleanWindowsSandboxPath(`C:/work/../Windows`); err == nil || !strings.Contains(err.Error(), "too broad") {
		t.Fatalf("err=%v, want broad path rejection", err)
	}
}

func TestRejectWindowsSandboxSyncOnly(t *testing.T) {
	err := rejectWindowsSandboxRunOptions(Provider{}.Spec(), RunRequest{SyncOnly: true})
	if err == nil || !strings.Contains(err.Error(), "--sync-only") {
		t.Fatalf("err=%v, want --sync-only rejection", err)
	}
}

func TestCopyManifestRejectsSymlinks(t *testing.T) {
	repo := t.TempDir()
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "target.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.txt", filepath.Join(repo, "link.txt")); err != nil {
		t.Skipf("symlink creation unavailable on this host: %v", err)
	}
	err := copyManifest(context.Background(), repo, dst, SyncManifest{Files: []string{"link.txt"}})
	if err == nil || !strings.Contains(err.Error(), "does not support syncing symlink") {
		t.Fatalf("err=%v, want symlink rejection", err)
	}
}

func TestSyncManifestHonorsIncludes(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	for name, data := range map[string]string{
		"keep.txt":       "keep",
		"skip.txt":       "skip",
		"nested/keep.go": "package nested",
	} {
		path := filepath.Join(repo, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := syncManifest(repo, nil, []string{"keep.txt", "nested/"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(manifest.Files, ","); got != "keep.txt,nested/keep.go" {
		t.Fatalf("manifest files=%q", got)
	}
}

func TestRunInvokesHostRunnerWithNoSync(t *testing.T) {
	oldOS := windowsSandboxHostOS
	windowsSandboxHostOS = "windows"
	defer func() { windowsSandboxHostOS = oldOS }()

	runner := &recordingRunner{}
	cfg := core.BaseConfig()
	cfg.WindowsSandbox.TempRoot = t.TempDir()
	cfg.TTL = 2 * time.Minute
	be := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner})
	result, err := be.(*backend).Run(context.Background(), RunRequest{
		NoSync:  true,
		Command: []string{"cmd.exe", "/c", "echo ok"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || !result.SyncDelegated {
		t.Fatalf("result=%#v", result)
	}
	if result.Status != core.RunStatusSucceeded || result.ErrorKind != core.RunErrorNone {
		t.Fatalf("status/error=%q/%q", result.Status, result.ErrorKind)
	}
	if runner.name != "powershell.exe" {
		t.Fatalf("runner name=%q", runner.name)
	}
	if !runner.disableOutputCapture {
		t.Fatal("host runner must stream without retaining output")
	}
	joined := strings.Join(runner.args, "\n")
	for _, want := range []string{"-File", "host-runner.ps1", "-TimeoutSeconds\n120"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args missing %q: %#v", want, runner.args)
		}
	}
}

func TestRunSignalsCancellationAndWaitsForHostCleanup(t *testing.T) {
	oldOS := windowsSandboxHostOS
	windowsSandboxHostOS = "windows"
	defer func() { windowsSandboxHostOS = oldOS }()

	runner := &cancelAwareRunner{
		started:        make(chan struct{}),
		observedCancel: make(chan struct{}),
	}
	cfg := core.BaseConfig()
	cfg.WindowsSandbox.TempRoot = t.TempDir()
	be := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := be.(*backend).Run(ctx, RunRequest{
			NoSync:  true,
			Command: []string{"cmd.exe", "/c", "timeout /t 30"},
		})
		done <- err
	}()

	select {
	case <-runner.started:
	case <-time.After(5 * time.Second):
		t.Fatal("host runner did not start")
	}
	cancel()
	select {
	case <-runner.observedCancel:
	case <-time.After(5 * time.Second):
		t.Fatal("host runner did not observe cancellation sentinel")
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "exited 130") {
			t.Fatalf("err=%v, want cancellation exit 130", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not wait for canceled host runner")
	}
	entries, err := os.ReadDir(cfg.WindowsSandbox.TempRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temp entries=%d, want canceled workspace removed", len(entries))
	}
}

func TestRunHostRunnerNormalizesCancellationFallbackExitCode(t *testing.T) {
	runner := &cancelOnContextRunner{started: make(chan struct{})}
	be := &backend{rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	ctx, cancel := context.WithCancel(context.Background())
	cancelPath := filepath.Join(t.TempDir(), "missing", "cancel.txt")
	done := make(chan localCommandOutcome, 1)
	go func() {
		result, err := be.runHostRunner(ctx, LocalCommandRequest{Name: "powershell.exe"}, cancelPath, true)
		done <- localCommandOutcome{result: result, err: err}
	}()
	<-runner.started
	cancel()
	outcome := <-done
	if outcome.err == nil || !strings.Contains(outcome.err.Error(), "signal windows-sandbox cancellation") {
		t.Fatalf("err=%v, want cancellation sentinel failure", outcome.err)
	}
	if outcome.result.ExitCode != 130 {
		t.Fatalf("ExitCode=%d, want 130", outcome.result.ExitCode)
	}
}

func TestRunKeepsWorkspaceOnFailureWithKeepOnFailure(t *testing.T) {
	oldOS := windowsSandboxHostOS
	windowsSandboxHostOS = "windows"
	defer func() { windowsSandboxHostOS = oldOS }()

	runner := &recordingRunner{result: LocalCommandResult{ExitCode: 7}, err: errors.New("exit status 7")}
	cfg := core.BaseConfig()
	cfg.WindowsSandbox.TempRoot = t.TempDir()
	var stderr strings.Builder
	be := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: &stderr, Exec: runner})
	result, err := be.(*backend).Run(context.Background(), RunRequest{
		NoSync:        true,
		KeepOnFailure: true,
		Command:       []string{"cmd.exe", "/c", "exit 7"},
		TimingJSON:    true,
	})
	if err == nil {
		t.Fatal("expected run error")
	}
	if result.ExitCode != 7 {
		t.Fatalf("ExitCode=%d", result.ExitCode)
	}
	if result.Status != core.RunStatusFailed || result.ErrorKind != core.RunErrorCommandExit {
		t.Fatalf("status/error=%q/%q", result.Status, result.ErrorKind)
	}
	if !strings.Contains(stderr.String(), `"runStatus":"failed"`) || !strings.Contains(stderr.String(), `"errorKind":"command-exit"`) {
		t.Fatalf("stderr = %q, want failed command-exit timing", stderr.String())
	}
	entries, readErr := os.ReadDir(cfg.WindowsSandbox.TempRoot)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 1 {
		t.Fatalf("temp entries=%d, want preserved workspace", len(entries))
	}
	if _, statErr := os.Stat(filepath.Join(cfg.WindowsSandbox.TempRoot, entries[0].Name(), "control", "run.ps1")); statErr != nil {
		t.Fatalf("preserved run script missing: %v", statErr)
	}
}

type recordingRunner struct {
	name                 string
	args                 []string
	disableOutputCapture bool
	result               LocalCommandResult
	err                  error
}

func (r *recordingRunner) Run(ctx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	_ = ctx
	r.name = req.Name
	r.args = append([]string(nil), req.Args...)
	r.disableOutputCapture = req.DisableOutputCapture
	return r.result, r.err
}

type cancelAwareRunner struct {
	started        chan struct{}
	observedCancel chan struct{}
}

type cancelOnContextRunner struct {
	started chan struct{}
}

func (r *cancelOnContextRunner) Run(ctx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	_ = req
	close(r.started)
	<-ctx.Done()
	return LocalCommandResult{ExitCode: 1}, ctx.Err()
}

func (r *cancelAwareRunner) Run(ctx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	if req.Name != "powershell.exe" {
		return LocalCommandResult{}, nil
	}
	close(r.started)
	var controlDir string
	for i, arg := range req.Args {
		if arg == "-ControlDir" && i+1 < len(req.Args) {
			controlDir = req.Args[i+1]
			break
		}
	}
	cancelPath := filepath.Join(controlDir, "cancel.txt")
	for {
		if _, err := os.Stat(cancelPath); err == nil {
			close(r.observedCancel)
			return LocalCommandResult{ExitCode: 130}, errors.New("exit status 130")
		}
		select {
		case <-ctx.Done():
			return LocalCommandResult{ExitCode: 1}, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}
