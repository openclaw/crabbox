package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

func (a App) desktopDoctor(ctx context.Context, args []string) error {
	target, cfg, leaseID, err := a.desktopCommandTarget(ctx, "desktop doctor", args, false)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "lease: %s provider=%s target=%s\n", leaseID, cfg.Provider, target.TargetOS)
	out, err := runSSHOutput(ctx, target, desktopDoctorRemoteCommand(target))
	if err != nil {
		return exit(5, "desktop doctor failed: %v", err)
	}
	fmt.Fprintln(a.Stdout, out)
	if isBlacksmithProvider(cfg.Provider) || isStaticProvider(cfg.Provider) {
		return nil
	}
	coord, useCoordinator, err := newTargetCoordinatorClient(cfg)
	if err == nil && useCoordinator && coord.hasConfiguredAuth() {
		rescueCtx := rescueContext{Cfg: cfg, Target: target, LeaseID: leaseID}
		status, err := coord.WebVNCStatus(ctx, leaseID)
		if err != nil {
			fmt.Fprintf(a.Stdout, "portal failed webvnc %v\n", err)
			printRescue(a.Stdout, rescueVNCBridgeDisconnected, err.Error(), webVNCStatusRescueCommand(rescueCtx), webVNCResetRescueCommand(rescueCtx))
		} else {
			fmt.Fprintf(a.Stdout, "portal ok webvnc bridge=%t viewers=%d observers=%d slots=%d\n", status.BridgeConnected, status.ViewerCount, status.ObserverCount, status.AvailableViewerSlots)
			if !status.BridgeConnected {
				printRescue(a.Stdout, rescueVNCBridgeNotRunning, "portal has no active WebVNC bridge for this lease", webVNCDaemonStartRescueCommand(rescueCtx), webVNCResetRescueCommand(rescueCtx))
			} else if webVNCObserverSlotsExhausted(status) {
				printRescue(a.Stdout, rescueVNCObserverSlotsFull, "all WebVNC observer slots are in use or stale", webVNCDaemonStartRescueCommand(rescueCtx), webVNCResetRescueCommand(rescueCtx))
			}
		}
	}
	return nil
}

func (a App) desktopClick(ctx context.Context, args []string) error {
	target, cfg, leaseID, err := a.desktopCommandTarget(ctx, "desktop click", args, false)
	if err != nil {
		return err
	}
	x, xOK := intFlagValue(args, "x")
	y, yOK := intFlagValue(args, "y")
	if !xOK || !yOK || x < 0 || y < 0 {
		return exit(2, "usage: crabbox desktop click --id <lease-id-or-slug> --x <n> --y <n>")
	}
	if !desktopClickSupportsTarget(target) {
		return exit(2, "desktop click supports target=linux, target=macos, or target=windows with windowsMode=normal")
	}
	if target.TargetOS == targetMacOS {
		if err := clickRemoteMacVNC(ctx, cfg, target, x, y); err != nil {
			return exit(5, "desktop click failed for %s: %v", leaseID, err)
		}
		fmt.Fprintf(a.Stdout, "clicked: lease=%s x=%d y=%d method=vnc\n", leaseID, x, y)
		return nil
	}
	if out, err := runSSHCombinedOutput(ctx, target, desktopClickRemoteCommand(target, x, y)); err != nil {
		a.printDesktopInputRescue(classifyDesktopFailure(out), out, cfg, target, leaseID)
		return exit(5, "desktop click failed for %s: %v", leaseID, err)
	}
	fmt.Fprintf(a.Stdout, "clicked: lease=%s x=%d y=%d\n", leaseID, x, y)
	return nil
}

func desktopClickSupportsTarget(target SSHTarget) bool {
	return target.TargetOS == targetLinux || target.TargetOS == targetMacOS || isWindowsNativeTarget(target)
}

func (a App) desktopPaste(ctx context.Context, args []string) error {
	target, cfg, leaseID, err := a.desktopCommandTarget(ctx, "desktop paste", args, false)
	if err != nil {
		return err
	}
	if !desktopTextSupportsTarget(target) {
		return exit(2, "desktop paste supports target=linux or target=macos")
	}
	text, err := desktopTextArgOrStdin(a.Stderr, args, "desktop paste")
	if err != nil {
		return err
	}
	if target.TargetOS == targetMacOS {
		if err := typeRemoteMacVNC(ctx, cfg, target, text); err != nil {
			return exit(5, "desktop paste failed for %s: %v", leaseID, err)
		}
		fmt.Fprintf(a.Stdout, "pasted: lease=%s bytes=%d method=vnc-key\n", leaseID, len(text))
		return nil
	}
	var stdout, stderr strings.Builder
	if err := runSSHInput(ctx, target, desktopPasteRemoteCommand(), strings.NewReader(text), &stdout, &stderr); err != nil {
		a.printDesktopInputRescue(classifyDesktopFailure(stderr.String()+"\n"+stdout.String()), stderr.String()+"\n"+stdout.String(), cfg, target, leaseID)
		return exit(5, "desktop paste failed for %s: %v", leaseID, err)
	}
	fmt.Fprintf(a.Stdout, "pasted: lease=%s bytes=%d\n", leaseID, len(text))
	return nil
}

func (a App) desktopType(ctx context.Context, args []string) error {
	target, cfg, leaseID, err := a.desktopCommandTarget(ctx, "desktop type", args, false)
	if err != nil {
		return err
	}
	if !desktopTextSupportsTarget(target) {
		return exit(2, "desktop type supports target=linux or target=macos")
	}
	text, err := desktopTextArgOrStdin(a.Stderr, args, "desktop type")
	if err != nil {
		return err
	}
	if target.TargetOS == targetMacOS {
		if err := typeRemoteMacVNC(ctx, cfg, target, text); err != nil {
			return exit(5, "desktop type failed for %s: %v", leaseID, err)
		}
		fmt.Fprintf(a.Stdout, "typed: lease=%s bytes=%d method=vnc-key\n", leaseID, len(text))
		return nil
	}
	if desktopShouldPasteForType(text) {
		var stdout, stderr strings.Builder
		if err := runSSHInput(ctx, target, desktopPasteRemoteCommand(), strings.NewReader(text), &stdout, &stderr); err != nil {
			pasteDetail := stderr.String() + "\n" + stdout.String()
			if !desktopPasteFailureSafeToRetry(pasteDetail) {
				a.printDesktopInputRescue(classifyDesktopFailure(pasteDetail), pasteDetail, cfg, target, leaseID)
				return exit(5, "desktop type paste failed for %s: %v", leaseID, err)
			}
			if out, typeErr := runSSHCombinedOutput(ctx, target, desktopTypeRemoteCommand(text)); typeErr == nil {
				fmt.Fprintf(a.Stdout, "typed: lease=%s method=key-fallback bytes=%d\n", leaseID, len(text))
				return nil
			} else {
				detail := pasteDetail + "\nkey fallback:\n" + out
				a.printDesktopInputRescue(classifyDesktopFailure(detail), detail, cfg, target, leaseID)
				return exit(5, "desktop type paste and key fallback failed for %s: %v", leaseID, typeErr)
			}
		}
		fmt.Fprintf(a.Stdout, "typed: lease=%s method=paste bytes=%d\n", leaseID, len(text))
		return nil
	}
	if out, err := runSSHCombinedOutput(ctx, target, desktopTypeRemoteCommand(text)); err != nil {
		a.printDesktopInputRescue(classifyDesktopFailure(out), out, cfg, target, leaseID)
		return exit(5, "desktop type failed for %s: %v", leaseID, err)
	}
	fmt.Fprintf(a.Stdout, "typed: lease=%s method=xdotool bytes=%d\n", leaseID, len(text))
	return nil
}

func desktopTextSupportsTarget(target SSHTarget) bool {
	return target.TargetOS == targetLinux || target.TargetOS == targetMacOS
}

// desktopPasteFailureSafeToRetry only permits a key-by-key retry when the
// remote paste helper failed before it could send input to the active window.
// Retrying after a post-paste clipboard-owner failure would duplicate text.
func desktopPasteFailureSafeToRetry(detail string) bool {
	for _, marker := range []string{
		"missing clipboard tool",
		"missing or unavailable xdotool",
		"missing xdotool",
		"missing wtype",
		"clipboard helper exited before paste",
		"clipboard helper failed to provide requested contents",
	} {
		if strings.Contains(detail, marker) {
			return true
		}
	}
	return false
}

func (a App) desktopKey(ctx context.Context, args []string) error {
	target, cfg, leaseID, err := a.desktopCommandTarget(ctx, "desktop key", args, true)
	if err != nil {
		return err
	}
	keys, err := desktopKeySequenceArg(args)
	if err != nil {
		return err
	}
	if strings.TrimSpace(keys) == "" {
		return exit(2, "usage: crabbox desktop key --id <lease-id-or-slug> <keys>")
	}
	if out, err := runSSHCombinedOutput(ctx, target, desktopKeyRemoteCommand(keys)); err != nil {
		a.printDesktopInputRescue(classifyDesktopFailure(out), out, cfg, target, leaseID)
		return exit(5, "desktop key failed for %s: %v", leaseID, err)
	}
	fmt.Fprintf(a.Stdout, "key: lease=%s keys=%s\n", leaseID, strings.TrimSpace(keys))
	return nil
}

func (a App) printDesktopInputRescue(problem, output string, cfg Config, target SSHTarget, leaseID string) {
	ctx := rescueContext{Cfg: cfg, Target: target, LeaseID: leaseID}
	printRescue(a.Stdout, problem, trimFailureDetail(output), desktopDoctorCommand(ctx))
}

func (a App) desktopCommandTarget(ctx context.Context, name string, args []string, requireLinux bool) (SSHTarget, Config, string, error) {
	defaults := defaultConfig()
	fs := newFlagSet(name, a.Stderr)
	provider := fs.String("provider", defaults.Provider, providerHelpSSH())
	id := fs.String("id", "", "lease id or slug")
	providerFlags := registerProviderFlags(fs, defaults)
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	if strings.HasSuffix(name, "click") {
		fs.Int("x", -1, "x coordinate")
		fs.Int("y", -1, "y coordinate")
	}
	if strings.HasSuffix(name, "paste") || strings.HasSuffix(name, "type") {
		fs.String("text", "", "text to enter")
	}
	if strings.HasSuffix(name, "key") {
		fs.String("keys", "", "xdotool key sequence")
	}
	if name == "artifacts video" {
		fs.String("output", "", "local MP4 output path")
		fs.Duration("duration", 10*time.Second, "video capture duration")
		fs.Float64("fps", 15, "video frames per second")
		fs.Bool("contact-sheet", true, "create a sampled contact sheet PNG next to recorded video")
		fs.String("contact-sheet-output", "", "contact sheet PNG output path")
		fs.Bool("no-contact-sheet", false, "skip contact sheet generation")
		fs.Int("contact-sheet-frames", 5, "number of sampled frames in the contact sheet")
		fs.Int("contact-sheet-cols", 5, "contact sheet columns")
		fs.Int("contact-sheet-width", 320, "width of each contact sheet tile")
	}
	if err := parseFlags(fs, args); err != nil {
		return SSHTarget{}, Config{}, "", err
	}
	setIDFromFirstArg(fs, id)
	cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{LeaseID: *id, Desktop: true})
	if err != nil {
		return SSHTarget{}, Config{}, "", err
	}
	if err := applyProviderFlags(&cfg, fs, providerFlags); err != nil {
		return SSHTarget{}, Config{}, "", err
	}
	if isBlacksmithProvider(cfg.Provider) {
		return SSHTarget{}, Config{}, "", exit(2, "desktop helpers are not supported for provider=%s; Blacksmith owns machine connectivity", cfg.Provider)
	}
	if err := requireLeaseID(*id, "crabbox "+name+" --id <lease-id-or-slug>", cfg); err != nil {
		return SSHTarget{}, Config{}, "", err
	}
	server, target, leaseID, err := a.resolveNetworkLeaseTarget(ctx, cfg, *id, false)
	if err != nil {
		return SSHTarget{}, Config{}, "", err
	}
	if err := enforceManagedLeaseCapabilities(cfg, server, leaseID); err != nil {
		return SSHTarget{}, Config{}, "", err
	}
	if requireLinux && target.TargetOS != targetLinux {
		return SSHTarget{}, Config{}, "", exit(2, "desktop input helpers currently require target=linux with xdotool")
	}
	a.touchLeaseTargetBestEffort(ctx, cfg, LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, "")
	return target, cfg, leaseID, nil
}

func desktopKeySequenceArg(args []string) (string, error) {
	defaults := defaultConfig()
	fs := newFlagSet("desktop key", io.Discard)
	fs.String("provider", defaults.Provider, providerHelpSSH())
	id := fs.String("id", "", "lease id or slug")
	registerProviderFlags(fs, defaults)
	registerTargetFlags(fs, defaults)
	registerNetworkModeFlag(fs, defaults)
	keys := fs.String("keys", "", "xdotool key sequence")
	if err := parseFlags(fs, args); err != nil {
		return "", err
	}
	if strings.TrimSpace(*keys) != "" {
		return *keys, nil
	}
	remaining := fs.Args()
	if *id == "" && len(remaining) > 0 {
		remaining = remaining[1:]
	}
	if len(remaining) == 0 {
		return "", nil
	}
	return remaining[0], nil
}

func desktopTextArgOrStdin(stderr io.Writer, args []string, name string) (string, error) {
	_ = stderr
	if text, ok := stringFlagValue(args, "text"); ok {
		return text, nil
	}
	info, err := os.Stdin.Stat()
	if err == nil && info.Mode()&os.ModeCharDevice != 0 {
		return "", exit(2, "usage: crabbox %s --id <lease-id-or-slug> --text <text>", name)
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", exit(2, "read stdin: %v", err)
	}
	return string(data), nil
}

func stringFlagValue(args []string, name string) (string, bool) {
	prefixes := []string{"--" + name + "=", "-" + name + "="}
	names := map[string]bool{"--" + name: true, "-" + name: true}
	for i, arg := range args {
		for _, prefix := range prefixes {
			if strings.HasPrefix(arg, prefix) {
				return strings.TrimPrefix(arg, prefix), true
			}
		}
		if names[arg] && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func intFlagValue(args []string, name string) (int, bool) {
	value, ok := stringFlagValue(args, name)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(value)
	return n, err == nil
}

func intFlagValueOr(args []string, name string, fallback int) int {
	value, ok := intFlagValue(args, name)
	if !ok {
		return fallback
	}
	return value
}

func boolFlagValueOr(args []string, name string, fallback bool) bool {
	prefixes := []string{"--" + name + "=", "-" + name + "="}
	names := map[string]bool{"--" + name: true, "-" + name: true}
	for _, arg := range args {
		for _, prefix := range prefixes {
			if strings.HasPrefix(arg, prefix) {
				return parseBoolFlagValue(strings.TrimPrefix(arg, prefix), fallback)
			}
		}
		if names[arg] {
			return true
		}
	}
	return fallback
}

func parseBoolFlagValue(value string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "t", "true", "y", "yes", "on":
		return true
	case "0", "f", "false", "n", "no", "off":
		return false
	default:
		return fallback
	}
}

func boolFlagPresent(args []string, name string) bool {
	prefixes := []string{"--" + name + "=", "-" + name + "="}
	names := map[string]bool{"--" + name: true, "-" + name: true}
	for _, arg := range args {
		for _, prefix := range prefixes {
			if strings.HasPrefix(arg, prefix) {
				value := strings.TrimPrefix(arg, prefix)
				return !strings.EqualFold(value, "false") && value != "0"
			}
		}
		if names[arg] {
			return true
		}
	}
	return false
}

func floatFlagValue(args []string, name string, fallback float64) float64 {
	value, ok := stringFlagValue(args, name)
	if !ok {
		return fallback
	}
	n, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fallback
	}
	return n
}

func durationFlagValue(args []string, name string, fallback time.Duration) time.Duration {
	value, ok := stringFlagValue(args, name)
	if !ok {
		return fallback
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return duration
}

func desktopShouldPasteForType(text string) bool {
	if text == "" {
		return false
	}
	if strings.ContainsAny(text, "\n\r\t @+:/\\'\"`$&|;<>[]{}()!*?=") {
		return true
	}
	if len(text) > 64 {
		return true
	}
	return false
}

func desktopClickRemoteCommand(target SSHTarget, x, y int) string {
	if isWindowsNativeTarget(target) {
		return fmt.Sprintf(`$ErrorActionPreference = "Stop"
$base = "C:\ProgramData\crabbox"
$passwordPath = Join-Path $base "windows.password"
$password = ""
if (Test-Path -LiteralPath $passwordPath) { $password = Get-Content -Raw -LiteralPath $passwordPath }
$taskName = "CrabboxClick-" + [Guid]::NewGuid().ToString("N")
$script = Join-Path $base ($taskName + ".ps1")
@'
Add-Type -TypeDefinition @"
using System;
using System.Runtime.InteropServices;
public class MouseInput {
  [DllImport("user32.dll")] public static extern bool SetCursorPos(int x, int y);
  [DllImport("user32.dll")] public static extern void mouse_event(uint flags, uint dx, uint dy, uint data, UIntPtr extraInfo);
}
"@
[MouseInput]::SetCursorPos(%d, %d) | Out-Null
Start-Sleep -Milliseconds 80
[MouseInput]::mouse_event(0x0002, 0, 0, 0, [UIntPtr]::Zero)
Start-Sleep -Milliseconds 80
[MouseInput]::mouse_event(0x0004, 0, 0, 0, [UIntPtr]::Zero)
'@ | Set-Content -Encoding ASCII -LiteralPath $script
cmd.exe /c "schtasks.exe /Delete /TN $taskName /F 2>NUL" | Out-Null
$startTime = (Get-Date).AddMinutes(1).ToString("HH:mm")
$createArgs = @("/Create", "/TN", $taskName, "/SC", "ONCE", "/ST", $startTime, "/TR", "powershell.exe -NoProfile -WindowStyle Hidden -ExecutionPolicy Bypass -File $script", "/RU", $env:USERNAME, "/IT", "/F")
& schtasks.exe @createArgs | Out-Null
if ($LASTEXITCODE -ne 0 -and $password -ne "") {
  & schtasks.exe @($createArgs + @("/RP", $password)) | Out-Null
}
if ($LASTEXITCODE -ne 0) { throw "failed to create interactive click task" }
schtasks.exe /Run /TN $taskName | Out-Null
Start-Sleep -Seconds 2
schtasks.exe /Delete /TN $taskName /F | Out-Null
Remove-Item -Force -LiteralPath $script -ErrorAction SilentlyContinue`, x, y)
	}
	if target.TargetOS == targetMacOS {
		return fmt.Sprintf(`set -eu
if command -v cliclick >/dev/null 2>&1; then
  cliclick c:%d,%d
  exit 0
fi
if command -v swift >/dev/null 2>&1; then
  tmp="$(mktemp -t crabbox-click).swift"
  trap 'rm -f "$tmp"' EXIT
  cat > "$tmp" <<'SWIFT'
import CoreGraphics
import Foundation
let point = CGPoint(x: %d, y: %d)
CGEvent(mouseEventSource: nil, mouseType: .mouseMoved, mouseCursorPosition: point, mouseButton: .left)?.post(tap: .cghidEventTap)
Thread.sleep(forTimeInterval: 0.08)
CGEvent(mouseEventSource: nil, mouseType: .leftMouseDown, mouseCursorPosition: point, mouseButton: .left)?.post(tap: .cghidEventTap)
Thread.sleep(forTimeInterval: 0.08)
CGEvent(mouseEventSource: nil, mouseType: .leftMouseUp, mouseCursorPosition: point, mouseButton: .left)?.post(tap: .cghidEventTap)
SWIFT
  swift "$tmp"
  exit 0
fi
echo "missing macOS click tool; install cliclick or Xcode swift in the template" >&2
exit 127`, x, y, x, y)
	}
	return fmt.Sprintf(`set -eu
if [ -f /var/lib/crabbox/desktop.env ]; then . /var/lib/crabbox/desktop.env; fi
export DISPLAY="${DISPLAY:-:99}"
if ! command -v xdotool >/dev/null 2>&1 || ! xdotool getactivewindow >/dev/null 2>&1; then
  if [ "${CRABBOX_DESKTOP_ENV:-xfce}" = "xfce" ]; then
    echo "missing or unavailable xdotool; warm a new --desktop lease or install xdotool" >&2
    exit 127
  fi
  echo "desktop click is not supported on Wayland desktop envs yet; use WebVNC pointer input" >&2
  exit 2
fi
xdotool mousemove %d %d click 1`, x, y)
}

func desktopKeyRemoteCommand(keys string) string {
	waylandArgs, waylandOK := desktopWaylandWtypeKeyArgs(keys)
	var wayland bytes.Buffer
	if waylandOK {
		wayland.WriteString("exec ")
		writeShellArgv(&wayland, append([]string{"wtype"}, waylandArgs...))
		wayland.WriteByte('\n')
	} else {
		wayland.WriteString("echo \"desktop key on --desktop-env wayland supports a single key or modifier+key sequence\" >&2\nexit 2\n")
	}
	return `set -eu
if [ -f /var/lib/crabbox/desktop.env ]; then . /var/lib/crabbox/desktop.env; fi
export DISPLAY="${DISPLAY:-:99}"
x11_input=0
if command -v xdotool >/dev/null 2>&1 && xdotool getactivewindow >/dev/null 2>&1; then x11_input=1; fi
if [ "${CRABBOX_DESKTOP_ENV:-xfce}" != "xfce" ] && [ "$x11_input" -ne 1 ]; then
  export XDG_RUNTIME_DIR WAYLAND_DISPLAY
  command -v wtype >/dev/null 2>&1 || { echo "missing wtype; warm a new Wayland desktop lease or install wtype" >&2; exit 127; }
  ` + wayland.String() + `fi
command -v xdotool >/dev/null 2>&1 || { echo "missing xdotool; warm a new --desktop lease or install xdotool" >&2; exit 127; }
xdotool key --clearmodifiers ` + shellQuote(strings.TrimSpace(keys))
}

func desktopTypeRemoteCommand(text string) string {
	return `set -eu
if [ -f /var/lib/crabbox/desktop.env ]; then . /var/lib/crabbox/desktop.env; fi
export DISPLAY="${DISPLAY:-:99}"
x11_input=0
if command -v xdotool >/dev/null 2>&1 && xdotool getactivewindow >/dev/null 2>&1; then x11_input=1; fi
if [ "${CRABBOX_DESKTOP_ENV:-xfce}" != "xfce" ] && [ "$x11_input" -ne 1 ]; then
  export XDG_RUNTIME_DIR WAYLAND_DISPLAY
  command -v wtype >/dev/null 2>&1 || { echo "missing wtype; warm a new Wayland desktop lease or install wtype" >&2; exit 127; }
  exec wtype -d 1 -- ` + shellQuote(text) + `
fi
command -v xdotool >/dev/null 2>&1 || { echo "missing xdotool; warm a new --desktop lease or install xdotool" >&2; exit 127; }
xdotool type --clearmodifiers --delay 1 -- ` + shellQuote(text)
}

func desktopPasteRemoteCommand() string {
	return `set -eu
tmp="$(mktemp)"
clip_pid=""
clip_backend=""
trap 'if [ -n "$clip_pid" ]; then kill "$clip_pid" 2>/dev/null || true; wait "$clip_pid" 2>/dev/null || true; fi; rm -f "$tmp"' EXIT
cat > "$tmp"
if [ -f /var/lib/crabbox/desktop.env ]; then . /var/lib/crabbox/desktop.env; fi
export DISPLAY="${DISPLAY:-:99}"
x11_input=0
if command -v xdotool >/dev/null 2>&1 && xdotool getactivewindow >/dev/null 2>&1; then x11_input=1; fi
if [ "${CRABBOX_DESKTOP_ENV:-xfce}" != "xfce" ] && [ "$x11_input" -ne 1 ]; then
  export XDG_RUNTIME_DIR WAYLAND_DISPLAY
  command -v wtype >/dev/null 2>&1 || { echo "missing wtype; warm a new Wayland desktop lease or install wtype" >&2; exit 127; }
  wtype -d 1 - < "$tmp"
  exit 0
fi
command -v xdotool >/dev/null 2>&1 || { echo "missing xdotool; warm a new --desktop lease or install xdotool" >&2; exit 127; }
active_class="$(xdotool getactivewindow getwindowclassname 2>/dev/null | tr '[:upper:]' '[:lower:]' || true)"
active_name="$(xdotool getactivewindow getwindowname 2>/dev/null | tr '[:upper:]' '[:lower:]' || true)"
active_pid="$(xdotool getactivewindow getwindowpid 2>/dev/null || true)"
active_proc=""
if [ -n "$active_pid" ] && command -v ps >/dev/null 2>&1; then
  active_proc="$(ps -p "$active_pid" -o comm= 2>/dev/null | tr '[:upper:]' '[:lower:]' || true)"
fi
case "$active_class $active_name $active_proc" in
  *xterm*|*terminal*|*konsole*|*alacritty*|*kitty*|*wezterm*)
    xdotool type --clearmodifiers --delay 1 --file "$tmp"
    exit 0
    ;;
esac
# Keep foreground-capable clipboard servers supervised; xsel is verified by
# exact readback because it has no paste-once mode.
if command -v xclip >/dev/null 2>&1; then
  timeout 5s xclip -quiet -selection clipboard -loops 1 "$tmp" &
  clip_pid=$!
  clip_backend=xclip
elif command -v xsel >/dev/null 2>&1; then
  xsel --nodetach --selectionTimeout 5000 --clipboard --input < "$tmp" &
  clip_pid=$!
  clip_verified=0
  clip_attempt=0
  while [ "$clip_attempt" -lt 10 ]; do
    if ! kill -0 "$clip_pid" >/dev/null 2>&1; then
      set +e
      wait "$clip_pid"
      clip_status=$?
      set -e
      [ "$clip_status" -ne 0 ] || clip_status=1
      clip_pid=""
      echo "clipboard helper exited before paste (xsel status=$clip_status)" >&2
      exit "$clip_status"
    fi
    if timeout 1s xsel --clipboard --output | cmp -s - "$tmp"; then
      clip_verified=1
      break
    fi
    clip_attempt=$((clip_attempt + 1))
    sleep 0.05
  done
  if [ "$clip_verified" -ne 1 ]; then
    echo "clipboard helper failed to provide requested contents (xsel)" >&2
    exit 1
  fi
  xdotool key --clearmodifiers ctrl+v
  # xsel has no paste-once mode. Give the target time to request the selection,
  # then stop the supervised owner so pasted contents do not remain exposed.
  sleep 0.2
  kill "$clip_pid" 2>/dev/null || true
  wait "$clip_pid" 2>/dev/null || true
  clip_pid=""
  exit 0
elif command -v wl-copy >/dev/null 2>&1; then
  wl-copy --foreground --paste-once < "$tmp" &
  clip_pid=$!
  clip_backend=wl-copy
else
  echo "missing clipboard tool; warm a new --desktop lease or install xclip/xsel" >&2
  exit 127
fi
sleep 0.2
if ! kill -0 "$clip_pid" >/dev/null 2>&1; then
  set +e
  wait "$clip_pid"
  clip_status=$?
  set -e
  if [ "$clip_status" -eq 0 ] && [ "$clip_backend" = xclip ] && timeout 1s xclip -selection clipboard -o | cmp -s - "$tmp"; then
    clip_pid=""
    xdotool key --clearmodifiers ctrl+v
    exit 0
  fi
  [ "$clip_status" -ne 0 ] || clip_status=1
  echo "clipboard helper exited before paste (status=$clip_status)" >&2
  exit "$clip_status"
fi
xdotool key --clearmodifiers ctrl+v
set +e
wait "$clip_pid"
clip_status=$?
set -e
if [ "$clip_status" -ne 0 ]; then
  echo "clipboard helper failed while serving paste (status=$clip_status)" >&2
  exit "$clip_status"
fi`
}

func desktopWaylandWtypeKeyArgs(keys string) ([]string, bool) {
	fields := strings.Fields(strings.TrimSpace(keys))
	if len(fields) != 1 || fields[0] == "" {
		return nil, false
	}
	parts := strings.Split(fields[0], "+")
	if len(parts) == 0 {
		return nil, false
	}
	var args []string
	var mods []string
	for _, raw := range parts[:len(parts)-1] {
		mod := desktopWaylandModifier(raw)
		if mod == "" {
			return nil, false
		}
		mods = append(mods, mod)
		args = append(args, "-M", mod)
	}
	key := strings.TrimSpace(parts[len(parts)-1])
	if key == "" {
		return nil, false
	}
	args = append(args, "-k", key)
	for i := len(mods) - 1; i >= 0; i-- {
		args = append(args, "-m", mods[i])
	}
	return args, true
}

func desktopWaylandModifier(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "ctrl", "control":
		return "ctrl"
	case "shift":
		return "shift"
	case "alt", "option":
		return "alt"
	case "logo", "win", "super", "cmd", "command", "meta":
		return "logo"
	case "altgr":
		return "altgr"
	default:
		return ""
	}
}

func desktopDoctorRemoteCommand(target SSHTarget) string {
	if target.TargetOS != targetLinux {
		return `echo "session warn target unsupported repair=desktop doctor has full checks for linux/xvfb leases"`
	}
	return `set +e
if [ -f /var/lib/crabbox/desktop.env ]; then . /var/lib/crabbox/desktop.env; fi
if [ "${CRABBOX_DESKTOP_ENV:-xfce}" != "xfce" ]; then
  export XDG_RUNTIME_DIR WAYLAND_DISPLAY
  check() {
    layer="$1"; item="$2"; shift 2
    if "$@" >/dev/null 2>&1; then
      echo "$layer ok $item"
    else
      echo "$layer failed $item repair=$CRABBOX_REPAIR"
    fi
  }
  [ -n "${XDG_RUNTIME_DIR:-}" ] && [ -n "${WAYLAND_DISPLAY:-}" ] && echo "session ok wayland=$XDG_RUNTIME_DIR/$WAYLAND_DISPLAY" || echo "session failed wayland repair=restart crabbox-desktop.service"
  CRABBOX_REPAIR="restart crabbox-desktop.service"; check session labwc pgrep -x labwc
  CRABBOX_REPAIR="restart crabbox-wayvnc.service"; check vm vnc ss -ltn sport = :5900
  CRABBOX_REPAIR="warm a new Wayland desktop lease or install wtype"; check input wtype command -v wtype
  CRABBOX_REPAIR="warm a new Wayland desktop lease or install wl-clipboard"; check input clipboard command -v wl-copy
  CRABBOX_REPAIR="warm a new Wayland desktop lease or install grim"; check capture screenshot command -v grim
  CRABBOX_REPAIR="warm with --browser or install Chrome/Chromium"; if [ -f /var/lib/crabbox/browser.env ]; then . /var/lib/crabbox/browser.env; fi; if [ -n "${BROWSER:-}" ] && [ -x "$BROWSER" ]; then echo "session ok browser=$BROWSER"; elif command -v google-chrome >/dev/null 2>&1 || command -v chromium >/dev/null 2>&1 || command -v chromium-browser >/dev/null 2>&1; then echo "session ok browser"; else echo "session failed browser repair=$CRABBOX_REPAIR"; fi
  exit 0
fi
export DISPLAY="${DISPLAY:-:99}"
check() {
  layer="$1"; item="$2"; shift 2
  if "$@" >/dev/null 2>&1; then
    echo "$layer ok $item"
  else
    echo "$layer failed $item repair=$CRABBOX_REPAIR"
  fi
}
CRABBOX_REPAIR="ensure DISPLAY=:99 is exported"; [ -n "$DISPLAY" ] && echo "session ok DISPLAY=$DISPLAY" || echo "session failed DISPLAY repair=export DISPLAY=:99"
CRABBOX_REPAIR="restart crabbox-xvfb.service"; check session display sh -c 'pgrep -f "Xtigervnc :99" >/dev/null || pgrep -f "Xvfb :99" >/dev/null'
CRABBOX_REPAIR="restart crabbox-desktop.service"; check session xfwm4 pgrep -x xfwm4
CRABBOX_REPAIR="restart crabbox-desktop.service"; check session panel pgrep -x xfce4-panel
CRABBOX_REPAIR="restart crabbox-xvfb.service"; check vm vnc ss -ltn sport = :5900
CRABBOX_REPAIR="warm a new --desktop lease or install xdotool"; check input xdotool command -v xdotool
CRABBOX_REPAIR="warm a new --desktop lease or install xclip"; if command -v xclip >/dev/null 2>&1 || command -v xsel >/dev/null 2>&1 || command -v wl-copy >/dev/null 2>&1; then echo "input ok clipboard"; else echo "input failed clipboard repair=$CRABBOX_REPAIR"; fi
CRABBOX_REPAIR="warm with --browser or install Chrome/Chromium"; if [ -f /var/lib/crabbox/browser.env ]; then . /var/lib/crabbox/browser.env; fi; if [ -n "${BROWSER:-}" ] && [ -x "$BROWSER" ]; then echo "session ok browser=$BROWSER"; elif command -v google-chrome >/dev/null 2>&1 || command -v chromium >/dev/null 2>&1 || command -v chromium-browser >/dev/null 2>&1; then echo "session ok browser"; else echo "session failed browser repair=$CRABBOX_REPAIR"; fi
CRABBOX_REPAIR="warm a new --desktop lease or install ffmpeg"; check capture ffmpeg command -v ffmpeg
CRABBOX_REPAIR="restart crabbox-xvfb.service"; if command -v xrandr >/dev/null 2>&1; then size="$(xrandr 2>/dev/null | awk '/ connected/{getline; print $1; exit}')"; [ -n "$size" ] && echo "session ok screen=$size" || echo "session failed screen repair=$CRABBOX_REPAIR"; else echo "session failed screen repair=install x11-xserver-utils"; fi
CRABBOX_REPAIR="restart desktop services or install scrot"; if command -v scrot >/dev/null 2>&1; then tmp="$(mktemp --suffix=.png)" && scrot -z -o "$tmp" >/dev/null 2>&1 && test -s "$tmp"; ok=$?; rm -f "$tmp"; [ "$ok" -eq 0 ] && echo "capture ok screenshot" || echo "capture failed screenshot repair=$CRABBOX_REPAIR"; else echo "capture failed screenshot repair=$CRABBOX_REPAIR"; fi`
}
