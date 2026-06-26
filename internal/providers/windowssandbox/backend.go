package windowssandbox

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

type backend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

var windowsSandboxHostOS = runtime.GOOS

const windowsSandboxProcessNames = "WindowsSandbox,WindowsSandboxClient,WindowsSandboxServer,WindowsSandboxRemoteSession"

func newBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	applyDefaults(&cfg)
	return &backend{spec: spec, cfg: cfg, rt: rt}
}

func applyDefaults(cfg *Config) {
	cfg.Provider = providerName
	if cfg.TargetOS == "" {
		cfg.TargetOS = targetWindows
	}
	if cfg.WindowsMode == "" {
		cfg.WindowsMode = windowsModeNormal
	}
	if cfg.WindowsSandbox.Workdir == "" {
		cfg.WindowsSandbox.Workdir = `C:\crabbox-work`
	}
	if cfg.WindowsSandbox.Networking == "" {
		cfg.WindowsSandbox.Networking = "Enable"
	}
	if cfg.WindowsSandbox.VGPU == "" {
		cfg.WindowsSandbox.VGPU = "Disable"
	}
	if cfg.WindowsSandbox.Clipboard == "" {
		cfg.WindowsSandbox.Clipboard = "Disable"
	}
	if cfg.WindowsSandbox.ProtectedClient == "" {
		cfg.WindowsSandbox.ProtectedClient = "Default"
	}
	if cfg.WindowsSandbox.AudioInput == "" {
		cfg.WindowsSandbox.AudioInput = "Disable"
	}
	if cfg.WindowsSandbox.VideoInput == "" {
		cfg.WindowsSandbox.VideoInput = "Disable"
	}
	if cfg.WindowsSandbox.PrinterRedirection == "" {
		cfg.WindowsSandbox.PrinterRedirection = "Disable"
	}
	cfg.WindowsSandbox.Networking = normalizeWSBStateBestEffort(cfg.WindowsSandbox.Networking)
	cfg.WindowsSandbox.VGPU = normalizeWSBStateBestEffort(cfg.WindowsSandbox.VGPU)
	cfg.WindowsSandbox.Clipboard = normalizeWSBStateBestEffort(cfg.WindowsSandbox.Clipboard)
	cfg.WindowsSandbox.ProtectedClient = normalizeWSBStateBestEffort(cfg.WindowsSandbox.ProtectedClient)
	cfg.WindowsSandbox.AudioInput = normalizeWSBStateBestEffort(cfg.WindowsSandbox.AudioInput)
	cfg.WindowsSandbox.VideoInput = normalizeWSBStateBestEffort(cfg.WindowsSandbox.VideoInput)
	cfg.WindowsSandbox.PrinterRedirection = normalizeWSBStateBestEffort(cfg.WindowsSandbox.PrinterRedirection)
	cfg.ServerType = providerName
	cfg.WorkRoot = cfg.WindowsSandbox.Workdir
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) configForRun() Config {
	cfg := b.cfg
	applyDefaults(&cfg)
	return cfg
}

func (b *backend) Warmup(ctx context.Context, req WarmupRequest) error {
	_ = ctx
	_ = req
	return exit(2, "provider=%s does not support warmup; Windows Sandbox is launched per run", providerName)
}

func (b *backend) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if err := rejectWindowsSandboxRunOptions(b.spec, req); err != nil {
		return RunResult{}, err
	}
	if len(req.Command) == 0 {
		return RunResult{}, exit(2, "missing command")
	}
	if err := requireWindowsHost(); err != nil {
		return RunResult{}, err
	}

	started := b.now()
	cfg := b.configForRun()
	run, syncPhases, syncDuration, err := b.prepareRun(ctx, cfg, req)
	if err != nil {
		return RunResult{Total: b.now().Sub(started), SyncDelegated: true, Provider: providerName}, err
	}
	keepWorkspace := req.Keep
	defer func() {
		if keepWorkspace {
			return
		}
		if err := os.RemoveAll(run.root); err != nil {
			fmt.Fprintf(b.rt.Stderr, "warning: remove windows-sandbox temp dir %s: %v\n", run.root, err)
		}
	}()

	if req.EnvSummary {
		printEnvForwardingSummary(b.rt.Stderr, providerName, "forwarded", req.Options.EnvAllow, req.Env)
	}
	fmt.Fprintf(b.rt.Stderr, "provider=%s workdir=%s host_workspace=%s networking=%s vgpu=%s\n", providerName, cfg.WindowsSandbox.Workdir, run.hostWorkspace, cfg.WindowsSandbox.Networking, cfg.WindowsSandbox.VGPU)

	commandStarted := b.now()
	execResult, execErr := b.runHostRunner(ctx, LocalCommandRequest{
		Name:                 "powershell.exe",
		Args:                 hostRunnerArgs(run, cfg, req),
		Stdout:               b.rt.Stdout,
		Stderr:               b.rt.Stderr,
		DisableOutputCapture: true,
	}, filepath.Join(run.hostControl, "cancel.txt"), req.Keep || req.KeepOnFailure)
	commandDuration := b.now().Sub(commandStarted)
	exitCode := execResult.ExitCode
	if execErr != nil && exitCode == 0 {
		exitCode = 1
	}
	keepWorkspace = req.Keep || (req.KeepOnFailure && exitCode != 0)
	if keepWorkspace {
		fmt.Fprintf(b.rt.Stderr, "windows-sandbox temp preserved path=%s policy=%s\n", run.root, keepPolicy(req, exitCode))
	}

	result := RunResult{
		ExitCode:      exitCode,
		Command:       commandDuration,
		Total:         b.now().Sub(started),
		SyncDelegated: true,
		Provider:      providerName,
		Slug:          filepath.Base(run.root),
		CommandText:   windowsSandboxCommandText(req),
	}
	result = finalizeRunResult(result, execErr)
	if req.NoSync {
		fmt.Fprintf(b.rt.Stderr, "windows-sandbox run summary sync_skipped=true command=%s total=%s exit=%d\n", result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), result.ExitCode)
	} else {
		fmt.Fprintf(b.rt.Stderr, "windows-sandbox run summary sync=%s command=%s total=%s exit=%d\n", syncDuration.Round(time.Millisecond), result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), result.ExitCode)
	}
	if req.TimingJSON {
		report := timingReportWithRunResult(timingReport{
			Provider:      providerName,
			Slug:          result.Slug,
			SyncDelegated: true,
			SyncMs:        syncDuration.Milliseconds(),
			SyncPhases:    syncPhases,
			SyncSkipped:   req.NoSync,
			CommandMs:     commandDuration.Milliseconds(),
			TotalMs:       result.Total.Milliseconds(),
			ExitCode:      result.ExitCode,
			Label:         strings.TrimSpace(req.Label),
		}, result, execErr)
		if err := writeTimingJSON(b.rt.Stderr, report); err != nil {
			return result, err
		}
	}
	if execErr != nil || result.ExitCode != 0 {
		return result, ExitError{Code: result.ExitCode, Message: fmt.Sprintf("%s run exited %d", providerName, result.ExitCode)}
	}
	return result, nil
}

func (b *backend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	_ = ctx
	_ = req
	return nil, exit(2, "provider=%s does not expose persistent inventory; Windows Sandbox supports one disposable session at a time", providerName)
}

func (b *backend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	_ = ctx
	_ = req
	return StatusView{}, exit(2, "provider=%s does not expose persistent status; close the Windows Sandbox window or rerun the command", providerName)
}

func (b *backend) Stop(ctx context.Context, req StopRequest) error {
	_ = ctx
	_ = req
	return exit(2, "provider=%s stop is not supported; close the Windows Sandbox window", providerName)
}

func (b *backend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	if err := requireWindowsHost(); err != nil {
		return DoctorResult{}, err
	}
	result, err := b.rt.Exec.Run(ctx, LocalCommandRequest{
		Name: "powershell.exe",
		Args: []string{
			"-NoProfile",
			"-ExecutionPolicy",
			"Bypass",
			"-Command",
			`if (Get-Command WindowsSandbox.exe -ErrorAction SilentlyContinue) { Write-Output "WindowsSandbox.exe" } else { Write-Error "Windows Sandbox is not available. Enable the Windows Sandbox optional feature on Windows Pro, Enterprise, or Education."; exit 2 }`,
		},
	})
	if err != nil {
		code := result.ExitCode
		if code == 0 {
			code = 2
		}
		return DoctorResult{}, exit(code, "provider=%s doctor failed: %s", providerName, commandDetail(result, err))
	}
	cfg := b.configForRun()
	msg := fmt.Sprintf("cli=ready control_plane=local sandbox=ready mutation=false runtime=%s networking=%s vgpu=%s workdir=%s", strings.TrimSpace(result.Stdout), cfg.WindowsSandbox.Networking, cfg.WindowsSandbox.VGPU, cfg.WindowsSandbox.Workdir)
	return DoctorResult{Provider: providerName, Message: msg}, nil
}

func (b *backend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now()
	}
	return time.Now()
}

type preparedRun struct {
	root          string
	hostWorkspace string
	hostControl   string
	wsbPath       string
	hostRunner    string
}

func (b *backend) prepareRun(ctx context.Context, cfg Config, req RunRequest) (preparedRun, []timingPhase, time.Duration, error) {
	root, err := os.MkdirTemp(strings.TrimSpace(cfg.WindowsSandbox.TempRoot), "crabbox-wsb-*")
	if err != nil {
		return preparedRun{}, nil, 0, fmt.Errorf("create windows-sandbox temp dir: %w", err)
	}
	if absRoot, err := filepath.Abs(root); err == nil {
		root = absRoot
	}
	run := preparedRun{
		root:          root,
		hostWorkspace: filepath.Join(root, "workspace"),
		hostControl:   filepath.Join(root, "control"),
		wsbPath:       filepath.Join(root, "crabbox.wsb"),
		hostRunner:    filepath.Join(root, "host-runner.ps1"),
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(root)
		}
	}()
	for _, dir := range []string{run.hostWorkspace, run.hostControl} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return preparedRun{}, nil, 0, fmt.Errorf("create windows-sandbox dir %s: %w", dir, err)
		}
	}
	syncPhases, syncDuration, err := b.syncWorkspace(ctx, cfg, req, run.hostWorkspace)
	if err != nil {
		return preparedRun{}, nil, 0, err
	}
	sandboxScript, err := sandboxRunScript(cfg, req)
	if err != nil {
		return preparedRun{}, nil, 0, err
	}
	if err := os.WriteFile(filepath.Join(run.hostControl, "run.ps1"), []byte(sandboxScript), 0o600); err != nil {
		return preparedRun{}, nil, 0, fmt.Errorf("write windows-sandbox run script: %w", err)
	}
	if err := os.WriteFile(run.hostRunner, []byte(hostRunnerScript()), 0o600); err != nil {
		return preparedRun{}, nil, 0, fmt.Errorf("write windows-sandbox host runner: %w", err)
	}
	wsb, err := windowsSandboxConfigXML(cfg, run)
	if err != nil {
		return preparedRun{}, nil, 0, err
	}
	if err := os.WriteFile(run.wsbPath, wsb, 0o600); err != nil {
		return preparedRun{}, nil, 0, fmt.Errorf("write windows-sandbox config: %w", err)
	}
	keep = true
	return run, syncPhases, syncDuration, nil
}

func (b *backend) syncWorkspace(ctx context.Context, cfg Config, req RunRequest, hostWorkspace string) ([]timingPhase, time.Duration, error) {
	started := b.now()
	if req.NoSync {
		if err := os.MkdirAll(hostWorkspace, 0o700); err != nil {
			return nil, 0, fmt.Errorf("create windows-sandbox workspace: %w", err)
		}
		return []timingPhase{{Name: "sync", Skipped: true, Reason: "--no-sync"}}, 0, nil
	}
	syncCtx := ctx
	cancel := func() {}
	if cfg.Sync.Timeout > 0 {
		syncCtx, cancel = context.WithTimeout(ctx, cfg.Sync.Timeout)
	}
	defer cancel()

	excludes, err := syncExcludes(req.Repo.Root, cfg)
	if err != nil {
		return nil, 0, err
	}
	manifestStarted := b.now()
	manifest, err := syncManifest(req.Repo.Root, excludes, cfg.Sync.Includes)
	if err != nil {
		return nil, 0, exit(6, "build sync file list: %v", err)
	}
	manifestDuration := b.now().Sub(manifestStarted)
	preflightStarted := b.now()
	if err := checkSyncPreflight(manifest, cfg, req.ForceSyncLarge, b.rt.Stderr); err != nil {
		return nil, 0, err
	}
	preflightDuration := b.now().Sub(preflightStarted)
	copyStarted := b.now()
	if cfg.Sync.Delete {
		if err := os.RemoveAll(hostWorkspace); err != nil {
			return nil, 0, fmt.Errorf("reset windows-sandbox workspace: %w", err)
		}
	}
	if err := os.MkdirAll(hostWorkspace, 0o700); err != nil {
		return nil, 0, fmt.Errorf("create windows-sandbox workspace: %w", err)
	}
	if err := copyManifest(syncCtx, req.Repo.Root, hostWorkspace, manifest); err != nil {
		return nil, 0, err
	}
	copyDuration := b.now().Sub(copyStarted)
	total := b.now().Sub(started)
	return []timingPhase{
		{Name: "manifest", Ms: manifestDuration.Milliseconds()},
		{Name: "preflight", Ms: preflightDuration.Milliseconds()},
		{Name: "copy", Ms: copyDuration.Milliseconds()},
		{Name: "windows_sandbox_sync", Ms: total.Milliseconds()},
	}, total, nil
}

func copyManifest(ctx context.Context, root, dstRoot string, manifest SyncManifest) error {
	for _, rel := range manifest.Files {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		clean := path.Clean(filepath.ToSlash(rel))
		if clean == "." || path.IsAbs(clean) || strings.HasPrefix(clean, "../") || clean != filepath.ToSlash(rel) {
			return exit(6, "unsafe sync path %q", rel)
		}
		src := filepath.Join(root, filepath.FromSlash(clean))
		dst := filepath.Join(dstRoot, filepath.FromSlash(clean))
		if err := copyWorkspaceEntry(src, dst, clean); err != nil {
			return err
		}
	}
	return nil
}

func copyWorkspaceEntry(src, dst, rel string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return fmt.Errorf("stat sync path %s: %w", src, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("create sync parent %s: %w", filepath.Dir(dst), err)
	}
	mode := info.Mode()
	if mode&os.ModeSymlink != 0 {
		return exit(6, "provider=%s does not support syncing symlink %q; exclude it or replace it with a regular file before using Windows Sandbox", providerName, filepath.ToSlash(rel))
	}
	if !mode.IsRegular() {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open sync path %s: %w", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode.Perm())
	if err != nil {
		return fmt.Errorf("create sync path %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy sync path %s: %w", src, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close sync path %s: %w", dst, err)
	}
	return nil
}

func hostRunnerArgs(run preparedRun, cfg Config, req RunRequest) []string {
	timeout := durationSecondsCeil(cfg.TTL)
	if timeout <= 0 {
		timeout = 5400
	}
	args := []string{
		"-NoProfile",
		"-ExecutionPolicy",
		"Bypass",
		"-File",
		run.hostRunner,
		"-WsbPath",
		run.wsbPath,
		"-ControlDir",
		run.hostControl,
		"-TimeoutSeconds",
		fmt.Sprintf("%d", timeout),
	}
	if req.Keep {
		args = append(args, "-Keep")
	}
	if req.KeepOnFailure {
		args = append(args, "-KeepOnFailure")
	}
	return args
}

type localCommandOutcome struct {
	result LocalCommandResult
	err    error
}

func (b *backend) runHostRunner(ctx context.Context, command LocalCommandRequest, cancelPath string, keepOnCancel bool) (LocalCommandResult, error) {
	runCtx, cancelRun := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRun()
	done := make(chan localCommandOutcome, 1)
	go func() {
		result, err := b.rt.Exec.Run(runCtx, command)
		done <- localCommandOutcome{result: result, err: err}
	}()

	select {
	case outcome := <-done:
		return outcome.result, outcome.err
	case <-ctx.Done():
		if err := os.WriteFile(cancelPath, []byte("cancel\r\n"), 0o600); err != nil {
			cancelRun()
			outcome := <-done
			if !keepOnCancel {
				b.stopCanceledSandbox(ctx)
			}
			outcome.result.ExitCode = 130
			return outcome.result, fmt.Errorf("signal windows-sandbox cancellation: %w", err)
		}

		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		select {
		case outcome := <-done:
			outcome.result.ExitCode = 130
			if outcome.err == nil {
				outcome.err = ctx.Err()
			}
			return outcome.result, outcome.err
		case <-timer.C:
			cancelRun()
			outcome := <-done
			if !keepOnCancel {
				b.stopCanceledSandbox(ctx)
			}
			outcome.result.ExitCode = 130
			if outcome.err == nil {
				outcome.err = ctx.Err()
			}
			return outcome.result, outcome.err
		}
	}
}

func (b *backend) stopCanceledSandbox(ctx context.Context) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	result, err := b.rt.Exec.Run(cleanupCtx, LocalCommandRequest{
		Name: "powershell.exe",
		Args: []string{
			"-NoProfile",
			"-ExecutionPolicy",
			"Bypass",
			"-Command",
			`Get-Process -Name ` + windowsSandboxProcessNames + ` -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue`,
		},
		DisableOutputCapture: true,
	})
	if err != nil {
		fmt.Fprintf(b.rt.Stderr, "warning: stop canceled windows-sandbox session: %s\n", commandDetail(result, err))
	}
}

func windowsSandboxConfigXML(cfg Config, run preparedRun) ([]byte, error) {
	workdir, err := cleanWindowsSandboxPath(cfg.WindowsSandbox.Workdir)
	if err != nil {
		return nil, err
	}
	doc := wsbConfiguration{
		VGPU:               cfg.WindowsSandbox.VGPU,
		Networking:         cfg.WindowsSandbox.Networking,
		AudioInput:         cfg.WindowsSandbox.AudioInput,
		VideoInput:         cfg.WindowsSandbox.VideoInput,
		ProtectedClient:    cfg.WindowsSandbox.ProtectedClient,
		PrinterRedirection: cfg.WindowsSandbox.PrinterRedirection,
		Clipboard:          cfg.WindowsSandbox.Clipboard,
		MappedFolders: wsbMappedFolders{Folders: []wsbMappedFolder{
			{HostFolder: run.hostWorkspace, SandboxFolder: workdir, ReadOnly: false},
			{HostFolder: run.hostControl, SandboxFolder: `C:\crabbox-control`, ReadOnly: false},
		}},
		LogonCommand: wsbLogonCommand{Command: `powershell.exe -NoProfile -ExecutionPolicy Bypass -File C:\crabbox-control\run.ps1`},
		MemoryInMB:   cfg.WindowsSandbox.MemoryMB,
	}
	data, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal windows-sandbox config: %w", err)
	}
	return append([]byte(xml.Header), data...), nil
}

type wsbConfiguration struct {
	XMLName            xml.Name         `xml:"Configuration"`
	VGPU               string           `xml:"vGPU,omitempty"`
	Networking         string           `xml:"Networking,omitempty"`
	MappedFolders      wsbMappedFolders `xml:"MappedFolders"`
	LogonCommand       wsbLogonCommand  `xml:"LogonCommand"`
	AudioInput         string           `xml:"AudioInput,omitempty"`
	VideoInput         string           `xml:"VideoInput,omitempty"`
	ProtectedClient    string           `xml:"ProtectedClient,omitempty"`
	PrinterRedirection string           `xml:"PrinterRedirection,omitempty"`
	Clipboard          string           `xml:"ClipboardRedirection,omitempty"`
	MemoryInMB         int              `xml:"MemoryInMB,omitempty"`
}

type wsbMappedFolders struct {
	Folders []wsbMappedFolder `xml:"MappedFolder"`
}

type wsbMappedFolder struct {
	HostFolder    string `xml:"HostFolder"`
	SandboxFolder string `xml:"SandboxFolder"`
	ReadOnly      bool   `xml:"ReadOnly"`
}

type wsbLogonCommand struct {
	Command string `xml:"Command"`
}

func sandboxRunScript(cfg Config, req RunRequest) (string, error) {
	workdir, err := cleanWindowsSandboxPath(cfg.WindowsSandbox.Workdir)
	if err != nil {
		return "", err
	}
	envLines, err := powershellEnvLines(req.Env)
	if err != nil {
		return "", err
	}
	command, err := sandboxCommand(req)
	if err != nil {
		return "", err
	}
	return strings.Join([]string{
		"$ErrorActionPreference = 'Continue'",
		"$PSDefaultParameterValues['Out-File:Encoding'] = 'utf8'",
		"$stdout = 'C:\\crabbox-control\\stdout.log'",
		"$stderr = 'C:\\crabbox-control\\stderr.log'",
		"$exitFile = 'C:\\crabbox-control\\exit-code.txt'",
		"Remove-Item -LiteralPath $stdout,$stderr,$exitFile -Force -ErrorAction SilentlyContinue",
		"New-Item -ItemType Directory -Force -Path 'C:\\crabbox-control' | Out-Null",
		"Set-Location -LiteralPath " + psSingleQuote(workdir),
		envLines,
		"$exitCode = 0",
		"try {",
		"  $global:LASTEXITCODE = $null",
		"  " + command + " 1>> $stdout 2>> $stderr",
		"  if ($null -ne $global:LASTEXITCODE) { $exitCode = [int]$global:LASTEXITCODE } elseif ($?) { $exitCode = 0 } else { $exitCode = 1 }",
		"} catch {",
		"  $_ | Out-File -FilePath $stderr -Append",
		"  $exitCode = 1",
		"}",
		"$keep = " + psBool(req.Keep),
		"$keepOnFailure = " + psBool(req.KeepOnFailure),
		"$canceled = Test-Path -LiteralPath 'C:\\crabbox-control\\cancel.txt'",
		"if ($canceled -and $exitCode -eq 0) { $exitCode = 130 }",
		"Set-Content -LiteralPath $exitFile -Value $exitCode -Encoding ASCII",
		"if (-not $keep -and -not ($keepOnFailure -and $exitCode -ne 0)) {",
		"  Start-Process -FilePath \"$env:WINDIR\\System32\\shutdown.exe\" -ArgumentList '/s','/t','0','/f' -WindowStyle Hidden",
		"}",
		"",
	}, "\r\n"), nil
}

func sandboxCommand(req RunRequest) (string, error) {
	if len(req.Command) == 0 {
		return "", exit(2, "missing command")
	}
	if req.ShellMode {
		return "& powershell.exe -NoProfile -ExecutionPolicy Bypass -Command " + psSingleQuote(strings.Join(req.Command, " ")), nil
	}
	parts := make([]string, 0, len(req.Command))
	for _, part := range req.Command {
		parts = append(parts, psSingleQuote(part))
	}
	return "& " + strings.Join(parts, " "), nil
}

func powershellEnvLines(env map[string]string) (string, error) {
	if len(env) == 0 {
		return "", nil
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		if !validEnvName(key) {
			return "", exit(2, "invalid environment variable name %q for provider=%s", key, providerName)
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, key := range keys {
		fmt.Fprintf(&b, "$env:%s = %s\r\n", key, psSingleQuote(env[key]))
	}
	return strings.TrimRight(b.String(), "\r\n"), nil
}

func hostRunnerScript() string {
	return strings.ReplaceAll(strings.ReplaceAll(`param(
  [Parameter(Mandatory=$true)][string]$WsbPath,
  [Parameter(Mandatory=$true)][string]$ControlDir,
  [int]$TimeoutSeconds = 5400,
  [switch]$Keep,
  [switch]$KeepOnFailure
)
$ErrorActionPreference = 'Stop'
$stdout = Join-Path $ControlDir 'stdout.log'
$stderr = Join-Path $ControlDir 'stderr.log'
$exitFile = Join-Path $ControlDir 'exit-code.txt'
$cancelFile = Join-Path $ControlDir 'cancel.txt'
Remove-Item -LiteralPath $stdout,$stderr,$exitFile -Force -ErrorAction SilentlyContinue

function Get-SandboxProcesses {
  Get-Process -Name __SANDBOX_PROCESS_NAMES__ -ErrorAction SilentlyContinue
}

$running = Get-SandboxProcesses
if ($running) {
  [Console]::Error.WriteLine('Windows Sandbox is already running. Microsoft Windows Sandbox allows one instance at a time; close it before running Crabbox with provider=windows-sandbox.')
  exit 2
}

$sandboxExe = (Get-Command WindowsSandbox.exe -ErrorAction Stop).Source
$process = Start-Process -FilePath $sandboxExe -ArgumentList ('"{0}"' -f $WsbPath) -PassThru
$started = Get-Date
$deadline = $started.AddSeconds($TimeoutSeconds)
$startupGraceSeconds = [Math]::Min($TimeoutSeconds, 120)
$startupDeadline = $started.AddSeconds($startupGraceSeconds)
$outOffset = 0
$errOffset = 0
$sandboxSeen = $false

function Stop-SandboxSession {
  if ($process -and -not $process.HasExited) {
    Stop-Process -Id $process.Id -Force -ErrorAction SilentlyContinue
  }
  Get-SandboxProcesses | Stop-Process -Force -ErrorAction SilentlyContinue
}

function Wait-SandboxSession([int]$TimeoutSeconds) {
  $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
  while (Get-SandboxProcesses) {
    if ((Get-Date) -gt $deadline) {
      Stop-SandboxSession
      return
    }
    Start-Sleep -Milliseconds 500
  }
}

function Flush-Log([string]$Path, [ref]$Offset, [bool]$IsError) {
  if (-not (Test-Path -LiteralPath $Path)) { return }
  try {
    $stream = [System.IO.File]::Open($Path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
    try {
      if ($stream.Length -lt $Offset.Value) { $Offset.Value = 0 }
      if ($stream.Length -le $Offset.Value) { return }
      [void]$stream.Seek($Offset.Value, [System.IO.SeekOrigin]::Begin)
      $reader = [System.IO.StreamReader]::new($stream, [System.Text.Encoding]::UTF8, $true, 4096, $true)
      try {
        $chunk = $reader.ReadToEnd()
        $Offset.Value = $stream.Position
      } finally {
        $reader.Dispose()
      }
    } finally {
      $stream.Dispose()
    }
  } catch [System.IO.IOException] {
    return
  } catch [System.UnauthorizedAccessException] {
    return
  }
  if ($IsError) {
    [Console]::Error.Write($chunk)
  } else {
    [Console]::Out.Write($chunk)
  }
}

while ($true) {
  Flush-Log $stdout ([ref]$outOffset) $false
  Flush-Log $stderr ([ref]$errOffset) $true
  $sandboxRunning = Get-SandboxProcesses
  if ($sandboxRunning) {
    $sandboxSeen = $true
  }
  if (Test-Path -LiteralPath $cancelFile) {
    $keepOpen = $Keep.IsPresent -or $KeepOnFailure.IsPresent
    if (-not $keepOpen) {
      Stop-SandboxSession
      Wait-SandboxSession 20
    }
    [Console]::Error.WriteLine('Windows Sandbox run canceled.')
    exit 130
  }
  if (Test-Path -LiteralPath $exitFile) {
    Start-Sleep -Milliseconds 300
    Flush-Log $stdout ([ref]$outOffset) $false
    Flush-Log $stderr ([ref]$errOffset) $true
    $raw = (Get-Content -LiteralPath $exitFile -Raw).Trim()
    [int]$exitCode = 1
    [void][int]::TryParse($raw, [ref]$exitCode)
    $keepOpen = $Keep.IsPresent -or ($KeepOnFailure.IsPresent -and $exitCode -ne 0)
    if (-not $keepOpen) {
      Wait-SandboxSession 20
    }
    exit $exitCode
  }
  if ((Get-Date) -gt $deadline) {
    if (-not ($Keep.IsPresent -or $KeepOnFailure.IsPresent)) {
      Stop-SandboxSession
    }
    [Console]::Error.WriteLine("Windows Sandbox timed out after ${TimeoutSeconds}s")
    exit 124
  }
  if ($process -and $process.HasExited) {
    if ($sandboxRunning) {
      Start-Sleep -Milliseconds 500
      continue
    }
    if (-not $sandboxSeen) {
      if ((Get-Date) -gt $startupDeadline) {
        Flush-Log $stdout ([ref]$outOffset) $false
        Flush-Log $stderr ([ref]$errOffset) $true
        [Console]::Error.WriteLine("Windows Sandbox launcher exited before any sandbox process was observed after ${startupGraceSeconds}s.")
        exit 1
      }
      Start-Sleep -Milliseconds 500
      continue
    }
    Start-Sleep -Seconds 2
    Flush-Log $stdout ([ref]$outOffset) $false
    Flush-Log $stderr ([ref]$errOffset) $true
    if (Get-SandboxProcesses) {
      continue
    }
    if (-not (Test-Path -LiteralPath $exitFile)) {
      [Console]::Error.WriteLine('Windows Sandbox closed before Crabbox received an exit code.')
      exit 1
    }
  }
  Start-Sleep -Milliseconds 500
}
`, "__SANDBOX_PROCESS_NAMES__", windowsSandboxProcessNames), "\n", "\r\n")
}

func rejectWindowsSandboxRunOptions(spec ProviderSpec, req RunRequest) error {
	if err := rejectDelegatedSyncOptionsForSpec(spec, req); err != nil {
		return err
	}
	if req.ID != "" {
		return exit(2, "provider=%s does not support --id; Windows Sandbox sessions are disposable", providerName)
	}
	if req.SyncOnly {
		return exit(2, "provider=%s does not support --sync-only; Windows Sandbox workspaces are created per run", providerName)
	}
	if req.Reclaim {
		return exit(2, "provider=%s does not support --reclaim; Windows Sandbox sessions are disposable", providerName)
	}
	return nil
}

func requireWindowsHost() error {
	if windowsSandboxHostOS != "windows" {
		return exit(2, "provider=%s requires a Windows host with the Windows Sandbox optional feature enabled", providerName)
	}
	return nil
}

func normalizeWSBState(value, flagName string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "default":
		return "Default", nil
	case "enable", "enabled", "true", "yes", "on":
		return "Enable", nil
	case "disable", "disabled", "false", "no", "off":
		return "Disable", nil
	default:
		return "", exit(2, "%s must be enable, disable, or default", flagName)
	}
}

func normalizeWSBStateBestEffort(value string) string {
	normalized, err := normalizeWSBState(value, "")
	if err != nil {
		return value
	}
	return normalized
}

func validateWindowsSandboxConfig(cfg Config) error {
	checks := map[string]string{
		"windows-sandbox.networking":         cfg.WindowsSandbox.Networking,
		"windows-sandbox.vgpu":               cfg.WindowsSandbox.VGPU,
		"windows-sandbox.clipboard":          cfg.WindowsSandbox.Clipboard,
		"windows-sandbox.protectedClient":    cfg.WindowsSandbox.ProtectedClient,
		"windows-sandbox.audioInput":         cfg.WindowsSandbox.AudioInput,
		"windows-sandbox.videoInput":         cfg.WindowsSandbox.VideoInput,
		"windows-sandbox.printerRedirection": cfg.WindowsSandbox.PrinterRedirection,
	}
	for name, value := range checks {
		if _, err := normalizeWSBState(value, name); err != nil {
			return err
		}
	}
	if cfg.WindowsSandbox.MemoryMB < 0 {
		return exit(2, "windows-sandbox.memoryMB must be non-negative")
	}
	_, err := cleanWindowsSandboxPath(cfg.WindowsSandbox.Workdir)
	return err
}

func cleanWindowsSandboxPath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", exit(2, "windows-sandbox workdir must not be empty")
	}
	if strings.Contains(value, "/") {
		value = strings.ReplaceAll(value, "/", `\`)
	}
	if len(value) < 3 || value[1] != ':' || value[2] != '\\' {
		return "", exit(2, "windows-sandbox workdir %q must be an absolute Windows path like C:\\crabbox-work", value)
	}
	drive := value[0]
	if !((drive >= 'A' && drive <= 'Z') || (drive >= 'a' && drive <= 'z')) {
		return "", exit(2, "windows-sandbox workdir %q must start with a Windows drive letter like C:\\crabbox-work", value)
	}
	clean := cleanWindowsPath(value)
	switch strings.ToUpper(clean) {
	case `C:\`, `C:\WINDOWS`, `C:\USERS`, `C:\PROGRAM FILES`, `C:\PROGRAM FILES (X86)`:
		return "", exit(2, "windows-sandbox workdir %q is too broad; choose a dedicated directory", clean)
	}
	return clean, nil
}

func cleanWindowsPath(value string) string {
	drive := strings.ToUpper(value[:1])
	parts := make([]string, 0)
	for _, part := range strings.Split(value[3:], `\`) {
		switch part {
		case "", ".":
			continue
		case "..":
			if len(parts) > 0 {
				parts = parts[:len(parts)-1]
			}
		default:
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return drive + `:\`
	}
	return drive + `:\` + strings.Join(parts, `\`)
}

func psSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func psBool(value bool) string {
	if value {
		return "$true"
	}
	return "$false"
}

func validEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		ok := r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || i > 0 && r >= '0' && r <= '9'
		if !ok {
			return false
		}
	}
	return true
}

func windowsSandboxCommandText(req RunRequest) string {
	return strings.Join(req.Command, " ")
}

func keepPolicy(req RunRequest, exitCode int) string {
	if req.Keep {
		return "keep"
	}
	if req.KeepOnFailure && exitCode != 0 {
		return "keep-on-failure"
	}
	return "none"
}

func commandDetail(result LocalCommandResult, err error) string {
	text := strings.TrimSpace(result.Stderr)
	if text == "" {
		text = strings.TrimSpace(result.Stdout)
	}
	if text == "" && err != nil {
		text = err.Error()
	}
	return blank(text, "unknown error")
}
