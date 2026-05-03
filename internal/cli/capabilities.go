package cli

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

const (
	desktopDisplay  = ":99"
	managedVNCPort  = "5900"
	vncPasswordPath = "/var/lib/crabbox/vnc.password"
	browserEnvPath  = "/var/lib/crabbox/browser.env"
)

type vncEndpoint struct {
	Direct  bool
	Host    string
	Port    string
	Managed bool
}

func applyCapabilityFlags(cfg *Config, desktop, browser bool) {
	cfg.Desktop = desktop
	cfg.Browser = browser
}

func validateRequestedCapabilities(cfg Config) error {
	if cfg.Desktop && isBlacksmithProvider(cfg.Provider) {
		return exit(2, "desktop/VNC is not supported for provider=%s; Blacksmith owns machine connectivity", cfg.Provider)
	}
	if cfg.Browser && isBlacksmithProvider(cfg.Provider) {
		return exit(2, "browser provisioning is not supported for provider=%s; use Blacksmith workflow setup for headless browser automation", cfg.Provider)
	}
	return nil
}

func enforceManagedLeaseCapabilities(cfg Config, server Server, leaseID string) error {
	if isStaticProvider(cfg.Provider) || server.Provider == staticProvider {
		return nil
	}
	if cfg.Desktop && !labelBool(server.Labels["desktop"]) {
		return exit(2, "lease %s was not created with desktop=true; warm a new lease with --desktop", leaseID)
	}
	if cfg.Browser && !labelBool(server.Labels["browser"]) {
		return exit(2, "lease %s was not created with browser=true; warm a new lease with --browser", leaseID)
	}
	return nil
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
		if isStaticProvider(cfg.Provider) {
			if err := ensureStaticDesktop(ctx, cfg, target); err != nil {
				return nil, err
			}
		}
		env["DISPLAY"] = desktopDisplay
		env["CRABBOX_DESKTOP"] = "1"
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

func ensureStaticDesktop(ctx context.Context, _ Config, target SSHTarget) error {
	return probeStaticDesktop(ctx, target)
}

func probeStaticDesktop(ctx context.Context, target SSHTarget) error {
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
	check := "pgrep -f 'Xvfb :99' >/dev/null && pgrep -f x11vnc >/dev/null && " + vncLoopbackCheckCommand(target)
	if err := runSSHQuiet(ctx, target, check); err != nil {
		return exit(2, "target=linux does not expose a loopback VNC desktop; start Xvfb :99 and x11vnc on 127.0.0.1:5900")
	}
	return nil
}

func probeBrowserEnv(ctx context.Context, cfg Config, target SSHTarget) (map[string]string, error) {
	var script string
	if isWindowsNativeTarget(target) {
		script = powershellCommand(`$ErrorActionPreference = "SilentlyContinue"
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
Write-Output ("CHROME_BIN=" + $path)`)
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
	for port := 5901; port <= 5999; port++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue
		}
		_ = ln.Close()
		return fmt.Sprint(port)
	}
	return "5901"
}

func resolveVNCEndpoint(ctx context.Context, cfg Config, target SSHTarget) (vncEndpoint, error) {
	if isStaticProvider(cfg.Provider) {
		if err := probeLoopbackVNC(ctx, target, "2", "1"); err == nil {
			return vncEndpoint{Host: "127.0.0.1", Port: managedVNCPort, Managed: cfg.Static.ManagedLogin}, nil
		}
		if tcpReachable(ctx, target.Host, managedVNCPort, 2*time.Second) {
			return vncEndpoint{Direct: true, Host: target.Host, Port: managedVNCPort, Managed: cfg.Static.ManagedLogin}, nil
		}
		return vncEndpoint{}, exit(5, "target does not expose VNC through SSH loopback 127.0.0.1:5900 or direct %s:%s", target.Host, managedVNCPort)
	}
	if err := waitForLoopbackVNC(ctx, target); err != nil {
		return vncEndpoint{}, err
	}
	return vncEndpoint{Host: "127.0.0.1", Port: managedVNCPort, Managed: true}, nil
}

func waitForLoopbackVNC(ctx context.Context, target SSHTarget) error {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if err := probeLoopbackVNC(ctx, target, "2", "1"); err == nil {
			return nil
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
