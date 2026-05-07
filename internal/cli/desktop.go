package cli

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

func (a App) desktopLaunch(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("desktop launch", a.Stderr)
	provider := fs.String("provider", defaults.Provider, providerHelpSSH())
	id := fs.String("id", "", "lease id or slug")
	browser := fs.Bool("browser", false, "launch the target browser")
	url := fs.String("url", "", "URL to pass to the launched browser")
	webvnc := fs.Bool("webvnc", false, "bridge the launched desktop into the authenticated WebVNC portal")
	openPortal := fs.Bool("open", false, "open the WebVNC portal when --webvnc is set")
	fullscreen := fs.Bool("fullscreen", false, "leave launched browser fullscreen for capture/video workflows")
	egress := fs.String("egress", "", "egress profile; passes the active lease-local proxy to the browser")
	egressProxy := fs.String("egress-proxy", defaultEgressListen, "lease-local egress proxy for --egress")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *openPortal && !*webvnc {
		return exit(2, "desktop launch --open requires --webvnc")
	}
	if strings.TrimSpace(*egress) != "" && !*browser {
		return exit(2, "desktop launch --egress currently requires --browser")
	}
	positionalID := false
	if *id == "" && fs.NArg() > 0 {
		*id = fs.Arg(0)
		positionalID = true
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	cfg.Provider = *provider
	cfg.Desktop = true
	cfg.Browser = *browser
	if err := applyTargetFlagOverrides(&cfg, fs, targetFlags); err != nil {
		return err
	}
	if err := applyNetworkModeFlagOverride(&cfg, fs, networkFlags); err != nil {
		return err
	}
	if err := validateRequestedCapabilities(cfg); err != nil {
		return err
	}
	if *webvnc && (isBlacksmithProvider(cfg.Provider) || isStaticProvider(cfg.Provider)) {
		return exit(2, "desktop launch --webvnc currently supports coordinator-backed hetzner/aws/azure desktop leases")
	}
	if *id == "" && !isStaticProvider(cfg.Provider) {
		return exit(2, "usage: crabbox desktop launch --id <lease-id-or-slug> [--browser] [--url <url>] -- <command...>")
	}
	server, target, leaseID, err := a.resolveNetworkLeaseTarget(ctx, cfg, *id, false)
	if err != nil {
		return err
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
	a.touchLeaseTargetBestEffort(ctx, cfg, LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, "")
	if err := waitForLoopbackVNC(ctx, &target); err != nil {
		return err
	}
	env, err := requestedCapabilityEnv(ctx, cfg, target)
	if err != nil {
		return err
	}
	command := fs.Args()
	if positionalID && len(command) > 0 && command[0] == *id {
		command = command[1:]
	}
	expectBrowserLaunch := false
	if *browser {
		if len(command) == 0 {
			if env["BROWSER"] == "" {
				printRescue(a.Stdout, rescueBrowserNotLaunched, "browser=true requested but target did not report BROWSER", desktopDoctorCommand(rescueContext{Cfg: cfg, Target: target, LeaseID: leaseID}))
				return exit(2, "browser=true requested but target did not report BROWSER")
			}
			command = []string{env["BROWSER"]}
			expectBrowserLaunch = true
			if strings.TrimSpace(*egress) != "" {
				command = append(command, "--proxy-server=http://"+strings.TrimSpace(*egressProxy))
			}
			if strings.TrimSpace(*url) != "" {
				command = append(command, strings.TrimSpace(*url))
			}
		} else if strings.TrimSpace(*url) != "" {
			expectBrowserLaunch = desktopCommandLooksLikeBrowser(command, env["BROWSER"])
			if strings.TrimSpace(*egress) != "" {
				command = append(command, "--proxy-server=http://"+strings.TrimSpace(*egressProxy))
			}
			command = append(command, strings.TrimSpace(*url))
		} else if strings.TrimSpace(*egress) != "" {
			expectBrowserLaunch = desktopCommandLooksLikeBrowser(command, env["BROWSER"])
			command = append(command, "--proxy-server=http://"+strings.TrimSpace(*egressProxy))
		} else {
			expectBrowserLaunch = desktopCommandLooksLikeBrowser(command, env["BROWSER"])
		}
	}
	if len(command) == 0 {
		return exit(2, "usage: crabbox desktop launch --id <lease-id-or-slug> -- <command...>")
	}
	workdir := remoteJoin(cfg, leaseID, repo.Name)
	rescueCtx := rescueContext{Cfg: cfg, Target: target, LeaseID: leaseID}
	if out, err := runSSHCombinedOutput(ctx, target, desktopLaunchRemoteCommand(target, workdir, env, command, *browser && !*fullscreen)); err != nil {
		printRescue(a.Stdout, classifyDesktopFailure(out), trimFailureDetail(out), desktopDoctorCommand(rescueCtx), desktopLaunchRetryCommand(rescueCtx, command))
		return exit(5, "launch desktop command: %v", err)
	}
	if expectBrowserLaunch && target.TargetOS == targetLinux {
		if out, err := runSSHCombinedOutput(ctx, target, desktopBrowserLaunchCheckCommand()); err != nil {
			printRescue(a.Stdout, rescueBrowserNotLaunched, trimFailureDetail(out), desktopDoctorCommand(rescueCtx), desktopLaunchRetryCommand(rescueCtx, command))
			return exit(5, "browser not launched for %s: %v", leaseID, err)
		}
	}
	fmt.Fprintf(a.Stdout, "launched: %s\n", strings.Join(command, " "))
	if *webvnc {
		return a.webvnc(ctx, desktopLaunchWebVNCArgs(cfg, target, leaseID, *openPortal))
	}
	return nil
}

func desktopLaunchWebVNCArgs(cfg Config, target SSHTarget, leaseID string, openPortal bool) []string {
	targetOS := firstNonBlank(target.TargetOS, cfg.TargetOS)
	args := []string{"--provider", cfg.Provider, "--target", targetOS, "--id", leaseID}
	if cfg.Network != "" && cfg.Network != NetworkAuto {
		args = append(args, "--network", string(cfg.Network))
	}
	windowsMode := firstNonBlank(target.WindowsMode, cfg.WindowsMode)
	if targetOS == targetWindows && windowsMode != "" {
		args = append(args, "--windows-mode", windowsMode)
	}
	if openPortal {
		args = append(args, "--open")
	}
	return args
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func desktopLaunchRemoteCommand(target SSHTarget, workdir string, env map[string]string, command []string, windowedBrowser bool) string {
	if isWindowsNativeTarget(target) {
		return windowsDesktopLaunchRemoteCommand(workdir, env, command)
	}
	if target.TargetOS == targetMacOS {
		return posixDesktopLaunchRemoteCommand(workdir, env, command, windowedBrowser)
	}
	return posixDesktopLaunchRemoteCommand(workdir, env, command, windowedBrowser)
}

func posixDesktopLaunchRemoteCommand(workdir string, env map[string]string, command []string, windowedBrowser bool) string {
	var b bytes.Buffer
	b.WriteString("set -eu\n")
	if workdir != "" {
		b.WriteString("mkdir -p " + shellQuote(workdir) + "\n")
		b.WriteString("cd " + shellQuote(workdir) + "\n")
	}
	for key, value := range env {
		b.WriteString(key + "=" + shellQuote(value) + "\n")
		b.WriteString("export " + key + "\n")
	}
	b.WriteString("log=${TMPDIR:-/tmp}/crabbox-desktop-launch.log\n")
	b.WriteString("if command -v setsid >/dev/null 2>&1; then\n")
	b.WriteString("  setsid ")
	writeShellArgv(&b, command)
	b.WriteString(" >\"$log\" 2>&1 < /dev/null &\n")
	b.WriteString("else\n")
	b.WriteString("  nohup ")
	writeShellArgv(&b, command)
	b.WriteString(" >\"$log\" 2>&1 < /dev/null &\n")
	b.WriteString("fi\n")
	if windowedBrowser {
		b.WriteString(posixWindowBrowserCommand())
	}
	return b.String()
}

func posixWindowBrowserCommand() string {
	return `(
  sleep 2
  export DISPLAY="${DISPLAY:-:99}"
  if command -v wmctrl >/dev/null 2>&1; then
    wmctrl -r :ACTIVE: -b remove,fullscreen,maximized_vert,maximized_horz >/dev/null 2>&1 || true
  fi
  if command -v xdotool >/dev/null 2>&1; then
    window="$(xdotool search --onlyvisible --class google-chrome 2>/dev/null | tail -1 || true)"
    if [ -z "$window" ]; then
      window="$(xdotool search --onlyvisible --class chromium 2>/dev/null | tail -1 || true)"
    fi
    if [ -n "$window" ]; then
      xdotool windowactivate "$window" windowmove "$window" 80 80 windowsize "$window" 1500 900 >/dev/null 2>&1 || true
    fi
  fi
) >/dev/null 2>&1 &
`
}

func desktopBrowserLaunchCheckCommand() string {
	return `set +e
export DISPLAY="${DISPLAY:-:99}"
sleep 5
if command -v xdotool >/dev/null 2>&1; then
  window="$(xdotool search --onlyvisible --class google-chrome 2>/dev/null | tail -1 || true)"
  [ -n "$window" ] || window="$(xdotool search --onlyvisible --class chromium 2>/dev/null | tail -1 || true)"
  if [ -n "$window" ]; then
    exit 0
  fi
  echo "browser window not visible on DISPLAY=$DISPLAY" >&2
fi
if command -v pgrep >/dev/null 2>&1 && {
  pgrep -x google-chrome >/dev/null 2>&1 ||
  pgrep -x chrome >/dev/null 2>&1 ||
  pgrep -x chromium >/dev/null 2>&1 ||
  pgrep -x chromium-browser >/dev/null 2>&1
}; then
  exit 0
fi
echo "browser process not found" >&2
exit 1`
}

func desktopCommandLooksLikeBrowser(command []string, browserEnv string) bool {
	if len(command) == 0 {
		return false
	}
	first := strings.TrimSpace(command[0])
	if first == "" {
		return false
	}
	if strings.TrimSpace(browserEnv) != "" && first == strings.TrimSpace(browserEnv) {
		return true
	}
	lower := strings.ToLower(filepath.Base(first))
	return strings.Contains(lower, "chrome") || strings.Contains(lower, "chromium")
}

func writeShellArgv(b *bytes.Buffer, command []string) {
	for i, arg := range command {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(shellQuote(arg))
	}
}

func windowsDesktopLaunchRemoteCommand(workdir string, env map[string]string, command []string) string {
	inner := windowsDesktopLaunchScript(workdir, env, command)
	return `$ErrorActionPreference = "Stop"
$base = "C:\ProgramData\crabbox"
$usernamePath = Join-Path $base "windows.username"
$passwordPath = Join-Path $base "windows.password"
$username = if (Test-Path -LiteralPath $usernamePath) { Get-Content -Raw -LiteralPath $usernamePath } else { $env:USERNAME }
$username = $username.Trim()
$password = if (Test-Path -LiteralPath $passwordPath) { (Get-Content -Raw -LiteralPath $passwordPath).Trim() } else { "" }
$taskName = "CrabboxDesktopLaunch-" + [Guid]::NewGuid().ToString("N")
$script = Join-Path $base ($taskName + ".ps1")
Set-Content -Encoding UTF8 -LiteralPath $script -Value ` + psQuote(inner) + `
cmd.exe /c "schtasks.exe /Delete /TN $taskName /F 2>NUL" | Out-Null
$startTime = (Get-Date).AddMinutes(1).ToString("HH:mm")
$createArgs = @("/Create", "/TN", $taskName, "/SC", "ONCE", "/ST", $startTime, "/TR", "powershell.exe -NoProfile -WindowStyle Hidden -ExecutionPolicy Bypass -File $script", "/RU", $username, "/IT", "/F")
& schtasks.exe @createArgs | Out-Null
if ($LASTEXITCODE -ne 0 -and $password -ne "") {
  & schtasks.exe @($createArgs + @("/RP", $password)) | Out-Null
}
if ($LASTEXITCODE -ne 0) { throw "failed to create interactive desktop launch task" }
& schtasks.exe /Run /TN $taskName | Out-Null
Start-Sleep -Seconds 2
& schtasks.exe /Delete /TN $taskName /F | Out-Null
Remove-Item -Force -LiteralPath $script -ErrorAction SilentlyContinue
`
}

func windowsDesktopLaunchScript(workdir string, env map[string]string, command []string) string {
	var b bytes.Buffer
	b.WriteString("$ErrorActionPreference = \"Stop\"\n")
	if workdir != "" {
		b.WriteString("New-Item -ItemType Directory -Force -Path " + psQuote(workdir) + " | Out-Null\n")
		b.WriteString("Set-Location -LiteralPath " + psQuote(workdir) + "\n")
	}
	for key, value := range env {
		b.WriteString("$env:" + key + " = " + psQuote(value) + "\n")
	}
	b.WriteString("$file = " + psQuote(command[0]) + "\n")
	b.WriteString("$arguments = @(")
	for i, arg := range command[1:] {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(psQuote(arg))
	}
	b.WriteString(")\n")
	b.WriteString(`try {
  $shell = New-Object -ComObject Shell.Application
  $shell.MinimizeAll()
  Start-Sleep -Milliseconds 250
} catch {}
$process = Start-Process -FilePath $file -ArgumentList $arguments -WorkingDirectory (Get-Location).Path -WindowStyle Normal -PassThru
Start-Sleep -Seconds 2
try {
  $wshell = New-Object -ComObject WScript.Shell
  $names = @()
  if ($process -and $process.ProcessName) { $names += $process.ProcessName }
  $names += [IO.Path]::GetFileNameWithoutExtension($file)
  foreach ($name in ($names | Where-Object { $_ } | Select-Object -Unique)) {
    if ($wshell.AppActivate($name)) { break }
  }
} catch {}
`)
	return b.String()
}
