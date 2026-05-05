package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	recordWhileRecorderReadyTimeout = 10 * time.Second
	recordWhileRemoteStopHeadroom   = 15 * time.Second
)

func (a App) recordDesktop(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("record", a.Stderr)
	provider := fs.String("provider", defaults.Provider, "provider: hetzner, aws, or ssh")
	id := fs.String("id", "", "lease id or slug")
	output := fs.String("output", "", "local MP4 output path")
	duration := fs.Duration("duration", 10*time.Second, "recording duration")
	fps := fs.Int("fps", 15, "recording frame rate")
	size := fs.String("size", "1024x768", "recording size, or auto")
	while := fs.Bool("while", false, "record while a local command runs after --")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	idFromPositional := false
	if *id == "" && fs.NArg() > 0 {
		*id = fs.Arg(0)
		idFromPositional = true
	}
	whileCommand := recordWhileCommandArgs(fs.Args(), *id, idFromPositional)
	if *while && len(whileCommand) == 0 {
		return exit(2, "usage: crabbox record --id <lease-id-or-slug> --while -- <local-command...>")
	}
	if *duration <= 0 {
		return exit(2, "--duration must be greater than zero")
	}
	if *duration > 10*time.Minute {
		return exit(2, "--duration must be 10m or less")
	}
	if *fps <= 0 || *fps > 60 {
		return exit(2, "--fps must be between 1 and 60")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	cfg.Provider = *provider
	cfg.Desktop = true
	if err := applyTargetFlagOverrides(&cfg, fs, targetFlags); err != nil {
		return err
	}
	if err := applyNetworkModeFlagOverride(&cfg, fs, networkFlags); err != nil {
		return err
	}
	if isBlacksmithProvider(cfg.Provider) {
		return exit(2, "desktop recording is not supported for provider=%s; Blacksmith owns machine connectivity", cfg.Provider)
	}
	if *id == "" && !isStaticProvider(cfg.Provider) {
		return exit(2, "usage: crabbox record --id <lease-id-or-slug> [--duration 10s] [--output <path>]")
	}
	server, target, leaseID, err := a.resolveLeaseTarget(ctx, cfg, *id)
	if err != nil {
		return err
	}
	if resolved, err := resolveNetworkTarget(ctx, cfg, server, target); err != nil {
		return err
	} else {
		target = resolved.Target
	}
	if isStaticProvider(cfg.Provider) && target.TargetOS != targetLinux {
		return exit(2, "desktop recordings are not captured from static %s hosts because those are existing host machines, not Crabbox-created desktops", target.TargetOS)
	}
	if target.TargetOS == targetMacOS {
		return exit(2, "desktop recording is not supported for macOS targets yet")
	}
	if err := enforceManagedLeaseCapabilities(cfg, server, leaseID); err != nil {
		return err
	}
	repo, err := findRepo()
	if err != nil {
		return err
	}
	if err := claimLeaseForRepoConfig(leaseID, serverSlug(server), cfg, repo.Root, cfg.IdleTimeout, *reclaim); err != nil {
		return err
	}
	a.touchActiveLeaseBestEffort(ctx, cfg, server, leaseID)
	recordTarget := recordDesktopControlTarget(target)
	if err := waitForLoopbackVNC(ctx, &recordTarget); err != nil {
		return err
	}
	outPath := strings.TrimSpace(*output)
	if outPath == "" {
		outPath = defaultRecordingPath(leaseID, serverSlug(server))
	}
	recordOpts := recordDesktopOptions{
		Duration: *duration,
		FPS:      *fps,
		Size:     strings.TrimSpace(*size),
	}
	if *while {
		if recordTarget.TargetOS == targetWindows {
			return exit(2, "record --while is not supported on Windows targets yet")
		}
		if err := captureDesktopRecordingWhile(ctx, recordTarget, outPath, recordOpts, recordWhileCommandOptions{
			Command:  whileCommand,
			LeaseID:  leaseID,
			Provider: cfg.Provider,
			Timeout:  recordOpts.Duration,
		}); err != nil {
			return err
		}
	} else if err := captureDesktopRecording(ctx, recordTarget, outPath, recordOpts); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "recording: %s\n", outPath)
	return nil
}

func recordWhileCommandArgs(args []string, id string, idFromPositional bool) []string {
	if idFromPositional && id != "" && len(args) > 0 && args[0] == id {
		return args[1:]
	}
	return args
}

func recordDesktopControlTarget(target SSHTarget) SSHTarget {
	if target.TargetOS == targetWindows {
		target.WindowsMode = windowsModeNormal
	}
	return target
}

type recordDesktopOptions struct {
	Duration time.Duration
	FPS      int
	Size     string
}

type recordWhileCommandOptions struct {
	Command  []string
	LeaseID  string
	Provider string
	Timeout  time.Duration
}

func defaultRecordingPath(leaseID, slug string) string {
	name := slug
	if strings.TrimSpace(name) == "" {
		name = leaseID
	}
	if strings.TrimSpace(name) == "" {
		name = "crabbox"
	}
	return "crabbox-" + normalizeLeaseSlug(name) + "-recording.mp4"
}

func captureDesktopRecordingWhile(ctx context.Context, target SSHTarget, outputPath string, opts recordDesktopOptions, command recordWhileCommandOptions) error {
	if target.TargetOS == targetWindows {
		return exit(2, "record --while is not supported on Windows targets yet")
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return exit(2, "create recording directory: %v", err)
	}
	file, err := os.Create(outputPath)
	if err != nil {
		return exit(2, "create recording %s: %v", outputPath, err)
	}
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(outputPath)
		}
	}()
	token := fmt.Sprintf("%d", time.Now().UnixNano())
	remoteStopPath := "/tmp/crabbox-record-" + token + ".stop"
	remoteReadyPath := "/tmp/crabbox-record-" + token + ".ready"
	remoteOutputPath := "/tmp/crabbox-record-" + token + ".mp4"
	recorderCtx, cancelRecorder := context.WithCancel(ctx)
	defer cancelRecorder()
	recordDone := make(chan error, 1)
	go func() {
		recordDone <- runSSHToWriter(recorderCtx, target, recordRemoteUntilStopCommand(target, opts, remoteOutputPath, remoteStopPath, remoteReadyPath), file)
	}()
	if err := waitForRecordWhileRecorderReady(ctx, target, remoteReadyPath, recordDone); err != nil {
		_ = runSSHQuiet(ctx, target, "touch "+shellQuote(remoteStopPath))
		cancelRecorder()
		recordErr := <-recordDone
		if recordErr != nil {
			return exit(5, "start recording: %v; recorder: %v", err, recordErr)
		}
		return exit(5, "start recording: %v", err)
	}
	driverErr := runRecordWhileLocalCommand(ctx, command)
	stopErr := runSSHQuiet(ctx, target, "touch "+shellQuote(remoteStopPath))
	recordErr := <-recordDone
	if recordErr == nil {
		ok = true
	}
	if driverErr != nil {
		return exit(5, "record driver command: %v", driverErr)
	}
	if stopErr != nil {
		return exit(5, "stop recording: %v", stopErr)
	}
	if recordErr != nil {
		return exit(5, "capture recording: %v", recordErr)
	}
	return nil
}

func waitForRecordWhileRecorderReady(ctx context.Context, target SSHTarget, remoteReadyPath string, recordDone chan error) error {
	waitCtx, cancel := context.WithTimeout(ctx, recordWhileRecorderReadyTimeout)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-recordDone:
			recordDone <- err
			if err != nil {
				return fmt.Errorf("recorder exited before ready: %w", err)
			}
			return fmt.Errorf("recorder exited before ready")
		default:
		}
		probeCtx, probeCancel := context.WithTimeout(waitCtx, 2*time.Second)
		err := runSSHQuietWithOptions(probeCtx, target, "test -f "+shellQuote(remoteReadyPath), "2", "1")
		probeCancel()
		if err == nil {
			return nil
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("recorder did not become ready within %s", recordWhileRecorderReadyTimeout)
		case <-ticker.C:
		}
	}
}

func runRecordWhileLocalCommand(ctx context.Context, opts recordWhileCommandOptions) error {
	driverCtx := ctx
	cancel := func() {}
	if opts.Timeout > 0 {
		driverCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
	}
	defer cancel()
	cmd := exec.CommandContext(driverCtx, opts.Command[0], opts.Command[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(),
		"CRABBOX_RECORD_LEASE_ID="+opts.LeaseID,
		"CRABBOX_RECORD_PROVIDER="+opts.Provider,
	)
	err := cmd.Run()
	if err != nil && errors.Is(driverCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("timed out after %s", opts.Timeout)
	}
	return err
}

func captureDesktopRecording(ctx context.Context, target SSHTarget, outputPath string, opts recordDesktopOptions) error {
	target = recordDesktopControlTarget(target)
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return exit(2, "create recording directory: %v", err)
	}
	file, err := os.Create(outputPath)
	if err != nil {
		return exit(2, "create recording %s: %v", outputPath, err)
	}
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(outputPath)
		}
	}()
	if err := runSSHToWriter(ctx, target, recordRemoteCommand(target, opts), file); err != nil {
		return exit(5, "capture recording: %v", err)
	}
	ok = true
	return nil
}

func recordRemoteUntilStopCommand(target SSHTarget, opts recordDesktopOptions, remoteOutputPath, remoteStopPath, remoteReadyPath string) string {
	durationSeconds := int(opts.Duration.Round(time.Second).Seconds())
	if durationSeconds < 1 {
		durationSeconds = 1
	}
	remoteMaxSeconds := int((opts.Duration + recordWhileRemoteStopHeadroom).Round(time.Second).Seconds())
	if remoteMaxSeconds < durationSeconds {
		remoteMaxSeconds = durationSeconds
	}
	size := strings.TrimSpace(opts.Size)
	if size == "" {
		size = "1024x768"
	}
	return posixRecordUntilStopRemoteCommand(remoteMaxSeconds, opts.FPS, size, remoteOutputPath, remoteStopPath, remoteReadyPath)
}

func recordRemoteCommand(target SSHTarget, opts recordDesktopOptions) string {
	durationSeconds := int(opts.Duration.Round(time.Second).Seconds())
	if durationSeconds < 1 {
		durationSeconds = 1
	}
	size := strings.TrimSpace(opts.Size)
	if size == "" {
		size = "1024x768"
	}
	if target.TargetOS == targetWindows {
		return windowsRecordRemoteCommand(durationSeconds, opts.FPS, size)
	}
	return posixRecordRemoteCommand(durationSeconds, opts.FPS, size)
}

func posixRecordUntilStopRemoteCommand(durationSeconds, fps int, size, remoteOutputPath, remoteStopPath, remoteReadyPath string) string {
	sizeArg := ""
	if size != "auto" {
		sizeArg = " -video_size " + shellQuote(size)
	}
	return fmt.Sprintf(`set -eu
export DISPLAY="${DISPLAY:-:99}"
out=%s
stop=%s
ready=%s
rm -f "$out" "$stop" "$ready"
if ! command -v ffmpeg >/dev/null 2>&1; then
  sudo apt-get update -y >/tmp/crabbox-record-apt.log 2>&1 || true
  sudo DEBIAN_FRONTEND=noninteractive apt-get install -y ffmpeg >>/tmp/crabbox-record-apt.log 2>&1 || true
fi
if ! command -v ffmpeg >/dev/null 2>&1; then
  echo "no video recording tool found; warm a new --desktop lease or install ffmpeg; apt log: /tmp/crabbox-record-apt.log" >&2
  exit 127
fi
input="$DISPLAY"
case "$input" in
  *.*) ;;
  *) input="${input}.0" ;;
esac
ffmpeg -hide_banner -loglevel error -y -f x11grab%s -framerate %d -i "$input" -t %d -pix_fmt yuv420p "$out" >/tmp/crabbox-record-ffmpeg.log 2>&1 &
pid=$!
printf ready > "$ready"
while kill -0 "$pid" >/dev/null 2>&1; do
  if [ -f "$stop" ]; then
    kill -INT "$pid" >/dev/null 2>&1 || true
    break
  fi
  sleep 0.2
done
wait "$pid" || true
test -s "$out"
cat "$out"
rm -f "$out" "$stop" "$ready"
`, shellQuote(remoteOutputPath), shellQuote(remoteStopPath), shellQuote(remoteReadyPath), sizeArg, fps, durationSeconds)
}

func posixRecordRemoteCommand(durationSeconds, fps int, size string) string {
	sizeArg := ""
	if size != "auto" {
		sizeArg = " -video_size " + shellQuote(size)
	}
	return fmt.Sprintf(`set -eu
export DISPLAY="${DISPLAY:-:99}"
if ! command -v ffmpeg >/dev/null 2>&1; then
  sudo apt-get update -y >/tmp/crabbox-record-apt.log 2>&1 || true
  sudo DEBIAN_FRONTEND=noninteractive apt-get install -y ffmpeg >>/tmp/crabbox-record-apt.log 2>&1 || true
fi
if ! command -v ffmpeg >/dev/null 2>&1; then
  echo "no video recording tool found; warm a new --desktop lease or install ffmpeg; apt log: /tmp/crabbox-record-apt.log" >&2
  exit 127
fi
input="$DISPLAY"
case "$input" in
  *.*) ;;
  *) input="${input}.0" ;;
esac
ffmpeg -hide_banner -loglevel error -y -f x11grab%s -framerate %d -i "$input" -t %d -pix_fmt yuv420p -movflags frag_keyframe+empty_moov -f mp4 pipe:1
`, sizeArg, fps, durationSeconds)
}

func windowsRecordRemoteCommand(durationSeconds, fps int, size string) string {
	sizeArgs := ""
	if size != "auto" {
		sizeArgs = `, "-video_size", ` + psQuote(size)
	}
	inner := `$ErrorActionPreference = "Stop"
$ffmpegCommand = Get-Command ffmpeg.exe -ErrorAction SilentlyContinue
if (-not $ffmpegCommand) { throw "no video recording tool found; install ffmpeg.exe on the Windows desktop lease" }
$args = @("-hide_banner", "-loglevel", "error", "-y", "-f", "gdigrab"` + sizeArgs + `, "-framerate", "` + fmt.Sprint(fps) + `", "-i", "desktop", "-t", "` + fmt.Sprint(durationSeconds) + `", "-pix_fmt", "yuv420p", "__CRABBOX_RECORDING_OUT__")
$proc = Start-Process -FilePath $ffmpegCommand.Source -ArgumentList $args -WindowStyle Hidden -Wait -PassThru
if ($proc.ExitCode -ne 0) { throw "ffmpeg.exe failed with exit code $($proc.ExitCode)" }
if (-not (Test-Path -LiteralPath "__CRABBOX_RECORDING_OUT__") -or ((Get-Item -LiteralPath "__CRABBOX_RECORDING_OUT__").Length -le 0)) { throw "ffmpeg.exe produced no recording" }
Set-Content -Encoding ASCII -LiteralPath "__CRABBOX_RECORDING_DONE__" -Value "done"
`
	return `$ErrorActionPreference = "Stop"
$base = "C:\ProgramData\crabbox"
$passwordPath = Join-Path $base "windows.password"
$password = if (Test-Path -LiteralPath $passwordPath) { (Get-Content -Raw -LiteralPath $passwordPath).Trim() } else { "" }
$taskName = "CrabboxRecord-" + [Guid]::NewGuid().ToString("N")
$out = Join-Path $base ($taskName + ".mp4")
$done = Join-Path $base ($taskName + ".done")
$script = Join-Path $base ($taskName + ".ps1")
try {
  Set-Content -Encoding UTF8 -LiteralPath $script -Value (` + psQuote(inner) + `.Replace("__CRABBOX_RECORDING_OUT__", $out).Replace("__CRABBOX_RECORDING_DONE__", $done))
  cmd.exe /c "schtasks.exe /Delete /TN $taskName /F 2>NUL" | Out-Null
  $startTime = (Get-Date).AddMinutes(1).ToString("HH:mm")
  $createArgs = @("/Create", "/TN", $taskName, "/SC", "ONCE", "/ST", $startTime, "/TR", "powershell.exe -NoProfile -WindowStyle Hidden -ExecutionPolicy Bypass -File $script", "/RU", $env:USERNAME, "/IT", "/F")
  & schtasks.exe @createArgs | Out-Null
  if ($LASTEXITCODE -ne 0 -and $password -ne "") {
    & schtasks.exe @($createArgs + @("/RP", $password)) | Out-Null
  }
  if ($LASTEXITCODE -ne 0) { throw "failed to create interactive recording task" }
  schtasks.exe /Run /TN $taskName | Out-Null
  $deadline = (Get-Date).AddSeconds(` + fmt.Sprint(durationSeconds+45) + `)
  while ((Get-Date) -lt $deadline) {
    if (Test-Path -LiteralPath $done) { break }
    Start-Sleep -Milliseconds 500
  }
  if (-not (Test-Path -LiteralPath $done)) { throw "scheduled interactive recording did not finish" }
  $bytes = [IO.File]::ReadAllBytes($out)
  [Console]::OpenStandardOutput().Write($bytes, 0, $bytes.Length)
} finally {
  cmd.exe /c "schtasks.exe /Delete /TN $taskName /F 2>NUL" | Out-Null
  Remove-Item -Force -LiteralPath $out, $done, $script -ErrorAction SilentlyContinue
}
`
}
