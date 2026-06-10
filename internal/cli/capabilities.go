package cli

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

const (
	desktopDisplay         = ":99"
	desktopEnvPath         = "/var/lib/crabbox/desktop.env"
	desktopEnvXFCE         = "xfce"
	desktopEnvWayland      = "wayland"
	desktopEnvGnome        = "gnome"
	managedVNCPort         = "5900"
	managedCodePort        = "8080"
	codeServerBinary       = "/usr/local/bin/code-server"
	vncPasswordPath        = "/var/lib/crabbox/vnc.password"
	windowsVNCPasswordPath = `C:\ProgramData\crabbox\vnc.password`
	macOSVNCPasswordPath   = "/var/db/crabbox/vnc.password"
	browserEnvPath         = "/var/lib/crabbox/browser.env"
)

type vncEndpoint struct {
	Direct  bool
	Host    string
	Port    string
	Managed bool
}

func applyCapabilityFlags(cfg *Config, desktop, browser, code bool) {
	cfg.Desktop = desktop
	cfg.Browser = browser
	cfg.Code = code
}

func validateRequestedCapabilities(cfg Config) error {
	provider, err := ProviderFor(cfg.Provider)
	if err != nil {
		return err
	}
	spec := provider.Spec()
	if cfg.Desktop && !featureSetHas(spec.Features, FeatureDesktop) {
		return exit(2, "desktop/VNC is not supported for provider=%s", provider.Name())
	}
	if err := validateDesktopEnv(cfg); err != nil {
		return err
	}
	if cfg.Browser && !featureSetHas(spec.Features, FeatureBrowser) {
		return exit(2, "browser provisioning is not supported for provider=%s", provider.Name())
	}
	if cfg.Code && !featureSetHas(spec.Features, FeatureCode) {
		return exit(2, "web code is not supported for provider=%s", provider.Name())
	}
	if cfg.TargetOS == targetWindows && cfg.WindowsMode == windowsModeWSL2 && cfg.Desktop {
		return exit(2, "target=windows --windows-mode wsl2 does not support desktop/VNC; use --windows-mode normal for desktop/VNC or omit --desktop for WSL2")
	}
	if cfg.Provider == "azure" && cfg.TargetOS == targetWindows && (cfg.Browser || cfg.Code || cfg.Tailscale.Enabled) {
		return exit(2, "provider=azure target=windows currently supports SSH, sync, run, and desktop/VNC; browser/code/tailscale require Linux or AWS Windows where supported")
	}
	if cfg.Code && cfg.TargetOS != targetLinux {
		return exit(2, "web code currently supports managed Linux leases only")
	}
	return nil
}

func normalizedDesktopEnv(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return desktopEnvXFCE
	}
	return value
}

func NormalizedDesktopEnv(value string) string {
	return normalizedDesktopEnv(value)
}

func validateDesktopEnv(cfg Config) error {
	switch normalizedDesktopEnv(cfg.DesktopEnv) {
	case desktopEnvXFCE:
		return nil
	case desktopEnvWayland, desktopEnvGnome:
		if cfg.Desktop && cfg.TargetOS != targetLinux {
			return exit(2, "desktopEnv=%s requires target=linux", normalizedDesktopEnv(cfg.DesktopEnv))
		}
		return nil
	default:
		return exit(2, "desktopEnv must be xfce, wayland, or gnome")
	}
}

func isWaylandDesktopEnv(value string) bool {
	switch normalizedDesktopEnv(value) {
	case desktopEnvWayland, desktopEnvGnome:
		return true
	default:
		return false
	}
}

func enforceManagedLeaseCapabilities(cfg Config, server Server, leaseID string) error {
	if isStaticProvider(cfg.Provider) || server.Provider == staticProvider {
		return nil
	}
	if cfg.Desktop && !labelBool(server.Labels["desktop"]) && !macOSScreenSharingLease(cfg, server) {
		return exit(2, "lease %s was not created with desktop=true; warm a new lease with --desktop", leaseID)
	}
	if cfg.Desktop {
		requestedDesktopEnv := normalizedDesktopEnv(cfg.DesktopEnv)
		if requestedDesktopEnv != desktopEnvXFCE && normalizedDesktopEnv(server.Labels["desktop_env"]) != requestedDesktopEnv {
			return exit(2, "lease %s was not created with desktopEnv=%s; warm a new lease with --desktop-env %s", leaseID, requestedDesktopEnv, requestedDesktopEnv)
		}
	}
	if cfg.Browser && !labelBool(server.Labels["browser"]) {
		return exit(2, "lease %s was not created with browser=true; warm a new lease with --browser", leaseID)
	}
	if cfg.Code && !labelBool(server.Labels["code"]) {
		return exit(2, "lease %s was not created with code=true; warm a new lease with --code", leaseID)
	}
	return nil
}

func macOSScreenSharingLease(cfg Config, server Server) bool {
	if cfg.TargetOS != targetMacOS && !strings.EqualFold(server.Labels["target"], targetMacOS) {
		return false
	}
	providerName := firstNonBlank(server.Provider, cfg.Provider)
	if providerName == "" {
		return true
	}
	provider, err := ProviderFor(providerName)
	return err != nil || provider.Spec().Coordinator != CoordinatorNever
}

func labelBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func requestedCapabilityEnv(ctx context.Context, cfg Config, target SSHTarget) (map[string]string, error) {
	env := map[string]string{}
	if cfg.Desktop {
		desktopEnv, _ := probeDesktopEnv(ctx, target)
		if isStaticProvider(cfg.Provider) {
			if err := ensureStaticDesktop(ctx, cfg, target); err != nil {
				return nil, err
			}
		}
		env["CRABBOX_DESKTOP"] = "1"
		for key, value := range desktopEnv {
			env[key] = value
		}
		if env["WAYLAND_DISPLAY"] == "" {
			env["DISPLAY"] = desktopDisplay
		}
	}
	if cfg.Browser {
		browserEnv, err := probeBrowserEnv(ctx, cfg, target)
		if err != nil {
			return nil, err
		}
		env["CRABBOX_BROWSER"] = "1"
		for key, value := range browserEnv {
			env[key] = value
		}
	}
	return env, nil
}

func mergeEnv(base map[string]string, extra map[string]string) map[string]string {
	if len(extra) == 0 {
		return base
	}
	out := make(map[string]string, len(base)+len(extra))
	for key, value := range base {
		out[key] = value
	}
	for key, value := range extra {
		out[key] = value
	}
	return out
}

func ensureStaticDesktop(ctx context.Context, cfg Config, target SSHTarget) error {
	return probeStaticDesktop(ctx, cfg, target)
}

func probeStaticDesktop(ctx context.Context, cfg Config, target SSHTarget) error {
	if isWindowsNativeTarget(target) {
		if err := probeLoopbackVNC(ctx, target, "10", "3"); err != nil {
			return exit(2, "target=windows does not expose a localhost VNC service; install a VNC server bound to 127.0.0.1:5900 or expose static VNC on host:5900")
		}
		return nil
	}
	if target.TargetOS == targetMacOS {
		if err := probeLoopbackVNC(ctx, target, "10", "3"); err != nil {
			return exit(2, "target=macos does not expose a localhost VNC service; enable Screen Sharing or use a preconfigured VNC server")
		}
		return nil
	}
	check := staticDesktopProbeCommand(cfg, target)
	if err := runSSHQuiet(ctx, target, check); err != nil {
		if isWaylandDesktopEnv(cfg.DesktopEnv) {
			return exit(2, "target=linux does not expose a Crabbox Wayland desktop; create %s with CRABBOX_DESKTOP_ENV=wayland or gnome, XDG_RUNTIME_DIR, and WAYLAND_DISPLAY, then start the compositor and WayVNC on 127.0.0.1:5900", desktopEnvPath)
		}
		return exit(2, "target=linux does not expose a loopback X11 VNC desktop; start Xvfb/x11vnc on 127.0.0.1:5900 or request --desktop-env wayland or gnome for a configured Wayland target")
	}
	return nil
}

func staticDesktopProbeCommand(cfg Config, target SSHTarget) string {
	if isWaylandDesktopEnv(cfg.DesktopEnv) {
		envCheck := `case "${CRABBOX_DESKTOP_ENV:-}" in wayland|gnome) ;; *) exit 1 ;; esac`
		compositorCheck := `pgrep -x labwc >/dev/null`
		if normalizedDesktopEnv(cfg.DesktopEnv) == desktopEnvGnome {
			envCheck = `test "${CRABBOX_DESKTOP_ENV:-}" = "gnome"`
		}
		return `test -f ` + shellQuote(desktopEnvPath) + ` && . ` + shellQuote(desktopEnvPath) + ` && ` +
			envCheck + ` && ` +
			`test -n "${XDG_RUNTIME_DIR:-}" && test -n "${WAYLAND_DISPLAY:-}" && ` +
			`test -S "$XDG_RUNTIME_DIR/$WAYLAND_DISPLAY" && ` +
			compositorCheck + ` && pgrep -x wayvnc >/dev/null && ` + vncLoopbackCheckCommand(target)
	}
	return `pgrep -f 'Xvfb :99' >/dev/null && pgrep -f x11vnc >/dev/null && ` + vncLoopbackCheckCommand(target)
}

func probeDesktopEnv(ctx context.Context, target SSHTarget) (map[string]string, error) {
	if isWindowsNativeTarget(target) || target.TargetOS == targetMacOS {
		return map[string]string{}, nil
	}
	out, err := runSSHOutput(ctx, target, probeDesktopEnvCommand())
	if err != nil {
		return map[string]string{}, err
	}
	return parseEnvLines(out), nil
}

func probeDesktopEnvCommand() string {
	return `if [ -f ` + shellQuote(desktopEnvPath) + ` ]; then . ` + shellQuote(desktopEnvPath) + `; fi
for key in CRABBOX_DESKTOP_ENV DISPLAY XAUTHORITY XDG_RUNTIME_DIR WAYLAND_DISPLAY GDK_BACKEND MOZ_ENABLE_WAYLAND; do
  eval "value=\${$key:-}"
  [ -n "$value" ] && printf '%s=%s\n' "$key" "$value"
done`
}

func probeBrowserEnv(ctx context.Context, cfg Config, target SSHTarget) (map[string]string, error) {
	var script string
	if isWindowsNativeTarget(target) {
		script = windowsBrowserProbeScript()
	} else if cfg.TargetOS == targetMacOS || target.TargetOS == targetMacOS {
		script = `path="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"; test -x "$path" || exit 1; printf 'BROWSER=%s\nCHROME_BIN=%s\n' "$path" "$path"`
	} else {
		script = `if [ -f ` + shellQuote(browserEnvPath) + ` ]; then . ` + shellQuote(browserEnvPath) + `; fi
for candidate in "${BROWSER:-}" "${CHROME_BIN:-}" google-chrome chromium chromium-browser; do
  [ -n "$candidate" ] || continue
  if [ -x "$candidate" ]; then path="$candidate"; break; fi
  if path="$(command -v "$candidate" 2>/dev/null)"; then break; fi
done
[ -n "${path:-}" ] || exit 1
"$path" --version >/dev/null
printf 'BROWSER=%s\nCHROME_BIN=%s\n' "$path" "$path"`
	}
	out, err := runSSHOutput(ctx, target, script)
	if err != nil {
		return nil, exit(2, "browser=true requested but no supported browser was found on target")
	}
	env := parseEnvLines(out)
	if env["BROWSER"] == "" {
		return nil, exit(2, "browser=true requested but target did not report BROWSER")
	}
	if env["CHROME_BIN"] == "" {
		env["CHROME_BIN"] = env["BROWSER"]
	}
	return env, nil
}

func windowsBrowserProbeScript() string {
	return `$ErrorActionPreference = "SilentlyContinue"
$paths = @()
$cmd = Get-Command chrome.exe -ErrorAction SilentlyContinue
if ($cmd) { $paths += $cmd.Source }
$cmd = Get-Command msedge.exe -ErrorAction SilentlyContinue
if ($cmd) { $paths += $cmd.Source }
$paths += @(
  "$Env:ProgramFiles\Google\Chrome\Application\chrome.exe",
  "${Env:ProgramFiles(x86)}\Google\Chrome\Application\chrome.exe",
  "$Env:ProgramFiles\Microsoft\Edge\Application\msedge.exe",
  "${Env:ProgramFiles(x86)}\Microsoft\Edge\Application\msedge.exe"
)
$path = $paths | Where-Object { $_ -and (Test-Path -LiteralPath $_) } | Select-Object -First 1
if (-not $path) { exit 1 }
Write-Output ("BROWSER=" + $path)
Write-Output ("CHROME_BIN=" + $path)`
}

func parseEnvLines(input string) map[string]string {
	env := map[string]string{}
	for _, line := range strings.Split(input, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		env[key] = strings.TrimSpace(value)
	}
	return env
}

func availableLocalVNCPort() string {
	return availableLocalVNCPortExcept("")
}

// availableLocalVNCPortExcept returns a free loopback port in the VNC range,
// skipping `except` so two cooperating listeners (e.g. the WebVNC server and its
// SSH tunnel) never land on the same port.
func availableLocalVNCPortExcept(except string) string {
	for port := 5901; port <= 5999; port++ {
		p := fmt.Sprint(port)
		if p == except {
			continue
		}
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue
		}
		_ = ln.Close()
		return p
	}
	if except != "5901" {
		return "5901"
	}
	return "5902"
}

func resolveVNCEndpoint(ctx context.Context, cfg Config, target *SSHTarget) (vncEndpoint, error) {
	if isStaticProvider(cfg.Provider) {
		if err := waitForLoopbackVNC(ctx, target); err == nil {
			return vncEndpoint{Host: "127.0.0.1", Port: managedVNCPort}, nil
		}
		if tcpReachable(ctx, target.Host, managedVNCPort, 2*time.Second) {
			return vncEndpoint{Direct: true, Host: target.Host, Port: managedVNCPort}, nil
		}
		return vncEndpoint{}, exit(5, "target does not expose VNC through SSH loopback 127.0.0.1:5900 or direct %s:%s", target.Host, managedVNCPort)
	}
	if err := waitForLoopbackVNC(ctx, target); err != nil {
		return vncEndpoint{}, err
	}
	return vncEndpoint{Host: "127.0.0.1", Port: managedVNCPort, Managed: true}, nil
}

func waitForLoopbackVNC(ctx context.Context, target *SSHTarget) error {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		for _, port := range sshPortCandidates(target.Port, target.FallbackPorts) {
			probe := *target
			probe.Port = port
			probe.FallbackPorts = []string{}
			if err := probeLoopbackVNC(ctx, probe, "2", "1"); err == nil {
				target.Port = port
				return nil
			}
		}
		time.Sleep(2 * time.Second)
	}
	return exit(5, "target does not expose VNC on 127.0.0.1:5900")
}

func probeLoopbackVNC(ctx context.Context, target SSHTarget, connectTimeout, connectionAttempts string) error {
	return runSSHQuietWithOptions(ctx, target, vncLoopbackCheckCommand(target), connectTimeout, connectionAttempts)
}

func vncLoopbackCheckCommand(target SSHTarget) string {
	if isWindowsNativeTarget(target) {
		return powershellCommand(`$result = Test-NetConnection -ComputerName 127.0.0.1 -Port 5900 -WarningAction SilentlyContinue
if (-not $result.TcpTestSucceeded) { exit 1 }`)
	}
	if target.TargetOS == targetMacOS {
		return "nc -z 127.0.0.1 5900"
	}
	return "ss -ltn | grep -q '127.0.0.1:5900'"
}

func vncPasswordCommand(target SSHTarget) string {
	if isWindowsNativeTarget(target) {
		return powershellCommand("Get-Content -Raw -LiteralPath " + psQuote(windowsVNCPasswordPath))
	}
	if target.TargetOS == targetMacOS {
		return "sudo cat " + shellQuote(macOSVNCPasswordPath)
	}
	return "cat " + shellQuote(vncPasswordPath)
}

func tcpReachable(ctx context.Context, host, port string, timeout time.Duration) bool {
	if host == "" || port == "" {
		return false
	}
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
