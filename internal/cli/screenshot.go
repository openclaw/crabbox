package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func (a App) screenshot(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("screenshot", a.Stderr)
	provider := fs.String("provider", defaults.Provider, providerHelpSSH())
	id := fs.String("id", "", "lease id or slug")
	output := fs.String("output", "", "local PNG output path")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	setIDFromFirstArg(fs, id)
	cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{Desktop: true})
	if err != nil {
		return err
	}
	if isBlacksmithProvider(cfg.Provider) {
		return exit(2, "desktop screenshots are not supported for provider=%s; Blacksmith owns machine connectivity", cfg.Provider)
	}
	if err := requireLeaseID(*id, "crabbox screenshot --id <lease-id-or-slug> [--output <path>]", cfg); err != nil {
		return err
	}
	server, target, leaseID, err := a.resolveNetworkLeaseTarget(ctx, cfg, *id, false)
	if err != nil {
		return err
	}
	if isStaticProvider(cfg.Provider) && target.TargetOS != targetLinux {
		return exit(2, "desktop screenshots are not captured from static %s hosts because those are existing host machines, not Crabbox-created desktops", target.TargetOS)
	}
	if err := enforceManagedLeaseCapabilities(cfg, server, leaseID); err != nil {
		return err
	}
	if err := a.claimAndTouchLeaseTarget(ctx, cfg, server, leaseID, *reclaim); err != nil {
		return err
	}
	if err := waitForLoopbackVNC(ctx, &target); err != nil {
		return err
	}
	outPath := strings.TrimSpace(*output)
	if outPath == "" {
		outPath = defaultScreenshotPath(leaseID, serverSlug(server))
	}
	if err := captureDesktopScreenshot(ctx, target, outPath); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "screenshot: %s\n", outPath)
	return nil
}

func defaultScreenshotPath(leaseID, slug string) string {
	name := slug
	if strings.TrimSpace(name) == "" {
		name = leaseID
	}
	if strings.TrimSpace(name) == "" {
		name = "crabbox"
	}
	return "crabbox-" + normalizeLeaseSlug(name) + "-screenshot.png"
}

func captureDesktopScreenshot(ctx context.Context, target SSHTarget, outputPath string) error {
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return exit(2, "create screenshot directory: %v", err)
	}
	file, err := os.Create(outputPath)
	if err != nil {
		return exit(2, "create screenshot %s: %v", outputPath, err)
	}
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(outputPath)
		}
	}()
	if err := runSSHToWriter(ctx, target, screenshotRemoteCommand(target), file); err != nil {
		return exit(5, "capture screenshot: %v", err)
	}
	ok = true
	return nil
}

func runSSHToWriter(ctx context.Context, target SSHTarget, remote string, stdout io.Writer) error {
	remote = wrapRemoteForTarget(target, remote)
	var lastErr error
	var lastMessage string
	for _, port := range sshPortCandidates(target.Port, target.FallbackPorts) {
		probe := target
		probe.Port = port
		cmd := exec.CommandContext(ctx, "ssh", sshArgs(probe, remote)...)
		cmd.Stdout = stdout
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			lastErr = err
			lastMessage = strings.TrimSpace(stderr.String())
			if shouldRetrySSHPort(err) {
				continue
			}
			if lastMessage != "" {
				return fmt.Errorf("%w: %s", err, lastMessage)
			}
			return err
		}
		return nil
	}
	if lastMessage != "" {
		return fmt.Errorf("%w: %s", lastErr, lastMessage)
	}
	return lastErr
}

func screenshotRemoteCommand(target SSHTarget) string {
	if isWindowsNativeTarget(target) {
		return `$ErrorActionPreference = "Stop"
$base = "C:\ProgramData\crabbox"
$password = Get-Content -Raw -LiteralPath (Join-Path $base "windows.password")
$taskName = "CrabboxScreenshot-" + [Guid]::NewGuid().ToString("N")
$out = Join-Path $base ($taskName + ".png")
$script = Join-Path $base ($taskName + ".ps1")
@'
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
$bounds = [System.Windows.Forms.Screen]::PrimaryScreen.Bounds
$bitmap = New-Object System.Drawing.Bitmap $bounds.Width, $bounds.Height
$graphics = [System.Drawing.Graphics]::FromImage($bitmap)
$graphics.CopyFromScreen($bounds.Location, [System.Drawing.Point]::Empty, $bounds.Size)
$bitmap.Save("__CRABBOX_SCREENSHOT_OUT__", [System.Drawing.Imaging.ImageFormat]::Png)
$graphics.Dispose()
$bitmap.Dispose()
'@.Replace("__CRABBOX_SCREENSHOT_OUT__", $out.Replace("\", "\\")) | Set-Content -Encoding ASCII -LiteralPath $script
cmd.exe /c "schtasks.exe /Delete /TN $taskName /F 2>NUL" | Out-Null
$startTime = (Get-Date).AddMinutes(1).ToString("HH:mm")
$createArgs = @("/Create", "/TN", $taskName, "/SC", "ONCE", "/ST", $startTime, "/TR", "powershell.exe -NoProfile -WindowStyle Hidden -ExecutionPolicy Bypass -File $script", "/RU", $env:USERNAME, "/IT", "/F")
& schtasks.exe @createArgs | Out-Null
if ($LASTEXITCODE -ne 0 -and $password -ne "") {
  & schtasks.exe @($createArgs + @("/RP", $password)) | Out-Null
}
if ($LASTEXITCODE -ne 0) { throw "failed to create interactive screenshot task" }
schtasks.exe /Run /TN $taskName | Out-Null
for ($i = 0; $i -lt 30; $i++) {
  if (Test-Path -LiteralPath $out) {
    try {
      $stream = [IO.File]::Open($out, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::Read)
      try {
        $bytes = New-Object byte[] $stream.Length
        $read = $stream.Read($bytes, 0, $bytes.Length)
        [Console]::OpenStandardOutput().Write($bytes, 0, $read)
      } finally {
        $stream.Dispose()
      }
      schtasks.exe /Delete /TN $taskName /F | Out-Null
      Remove-Item -Force -LiteralPath $out, $script -ErrorAction SilentlyContinue
      exit 0
    } catch {
      Start-Sleep -Milliseconds 500
    }
  }
  Start-Sleep -Milliseconds 500
}
schtasks.exe /Delete /TN $taskName /F | Out-Null
Remove-Item -Force -LiteralPath $script -ErrorAction SilentlyContinue
throw "scheduled interactive screenshot did not produce output"`
	}
	if target.TargetOS == targetMacOS {
		return `set -eu
if command -v screencapture >/dev/null 2>&1; then
  screencapture -x -t png -
else
  echo "no screenshot tool found; EC2 macOS should provide screencapture" >&2
  exit 127
fi`
	}
	return `set -eu
export DISPLAY="${DISPLAY:-:99}"
if command -v scrot >/dev/null 2>&1; then
  tmp="$(mktemp --suffix=.png)"
  trap 'rm -f "$tmp"' EXIT
  scrot -z -o "$tmp"
  cat "$tmp"
elif command -v import >/dev/null 2>&1; then
  import -window root png:-
else
  echo "no screenshot tool found; warm a new --desktop lease or install scrot" >&2
  exit 127
fi`
}
