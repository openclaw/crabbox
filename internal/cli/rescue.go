package cli

import (
	"fmt"
	"io"
	"strings"
)

const (
	rescueBrowserNotLaunched        = "browser not launched"
	rescueClipboardDeliveryFailed   = "clipboard delivery failed"
	rescueClipboardUnavailable      = "clipboard unavailable"
	rescueDesktopCommandNotLaunched = "desktop command not launched"
	rescueDesktopSessionMissing     = "desktop session missing"
	rescueInputStackDead            = "input stack dead"
	rescueVNCBridgeDisconnected     = "VNC bridge disconnected"
	rescueVNCBridgeNotRunning       = "WebVNC daemon not running"
	rescueVNCObserverSlotsFull      = "WebVNC observer slots exhausted"
	rescueVNCStaleViewer            = "WebVNC viewer already active"
	rescueVNCTargetUnreachable      = "VNC target unreachable"
	rescueWindowManagerMissing      = "window manager missing"
	rescueScreenshotCaptureBroken   = "screenshot capture broken"
	rescueArtifactCaptureFailed     = "artifact capture failed"
)

type rescueContext struct {
	Cfg     Config
	Target  SSHTarget
	LeaseID string
}

func printRescue(w io.Writer, problem, detail string, commands ...string) {
	fmt.Fprintf(w, "problem: %s\n", problem)
	if strings.TrimSpace(detail) != "" {
		fmt.Fprintf(w, "detail: %s\n", strings.TrimSpace(detail))
	}
	for _, command := range commands {
		if strings.TrimSpace(command) != "" {
			fmt.Fprintf(w, "rescue: %s\n", command)
		}
	}
}

func printRescueWithFallback(w io.Writer, problem, detail, fallback string, commands ...string) {
	printRescue(w, problem, detail, commands...)
	if strings.TrimSpace(fallback) != "" {
		fmt.Fprintf(w, "fallback: %s\n", fallback)
	}
}

func desktopDoctorCommand(ctx rescueContext) string {
	return crabboxLeaseCommand(ctx, "desktop", "doctor")
}

func webVNCStatusRescueCommand(ctx rescueContext) string {
	return crabboxLeaseCommand(ctx, "webvnc", "status")
}

func webVNCResetRescueCommand(ctx rescueContext) string {
	routing := crabboxLeaseCommandRouting(ctx, "webvnc", "reset")
	return routing.ShellCommand(append(routing.Args, "--open"))
}

func webVNCDaemonStartRescueCommand(ctx rescueContext) string {
	routing := crabboxLeaseCommandRouting(ctx, "webvnc", "daemon", "start")
	return routing.ShellCommand(append(routing.Args, "--open"))
}

func desktopLaunchRetryCommand(ctx rescueContext, command []string) string {
	routing := crabboxLeaseCommandRouting(ctx, "desktop", "launch")
	return routing.ShellCommand(append(append(routing.Args, "--"), command...))
}

func crabboxLeaseCommand(ctx rescueContext, command ...string) string {
	routing := crabboxLeaseCommandRouting(ctx, command...)
	return routing.ShellCommand(routing.Args)
}

func leaseCommandRouting(cfg Config, target SSHTarget, leaseID string, purpose CommandRoutingPurpose) CommandRouting {
	cfg.TargetOS = firstNonBlank(target.TargetOS, cfg.TargetOS)
	cfg.WindowsMode = firstNonBlank(target.WindowsMode, cfg.WindowsMode)
	routing := CommandRoutingFor(cfg, leaseID, purpose, target)
	if cfg.Network != "" && cfg.Network != NetworkAuto {
		routing.Args = append(routing.Args, "--network", string(cfg.Network))
	}
	routing.Args = append(routing.Args, "--id", leaseID)
	return routing
}

func crabboxLeaseCommandRouting(ctx rescueContext, command ...string) CommandRouting {
	routing := leaseCommandRouting(ctx.Cfg, ctx.Target, ctx.LeaseID, CommandRoutingRescue)
	routing.Args = append(append([]string{"crabbox"}, command...), routing.Args...)
	return routing
}

// AppendExternalDesktopRoutingArgs excludes repository-provided credential
// selectors so a generated CLI flag cannot promote them to trusted input.
func AppendExternalDesktopRoutingArgs(args []string, cfg Config) []string {
	if username := strings.TrimSpace(cfg.External.Connection.Desktop.Username); username != "" || IsExternalDesktopUsernameExplicit(&cfg) {
		if cfg.credentialProvenance.externalDesktopUser != credentialSourceRepository {
			args = append(args, "--external-desktop-username", username)
		}
	}
	if passwordEnv := strings.TrimSpace(cfg.External.Connection.Desktop.PasswordEnv); passwordEnv != "" || IsExternalDesktopPasswordEnvExplicit(&cfg) {
		if cfg.credentialProvenance.externalDesktopEnv != credentialSourceRepository {
			args = append(args, "--external-desktop-password-env", passwordEnv)
		}
	}
	return args
}

func classifyDesktopFailure(output string) string {
	text := strings.ToLower(output)
	switch {
	case strings.Contains(text, "missing xdotool"), strings.Contains(text, "xdotool: not found"):
		return rescueInputStackDead
	case strings.Contains(text, "missing clipboard tool"), strings.Contains(text, "xclip: not found"), strings.Contains(text, "xsel: not found"):
		return rescueClipboardUnavailable
	case strings.Contains(text, "clipboard helper exited"), strings.Contains(text, "clipboard helper failed"):
		return rescueClipboardDeliveryFailed
	case strings.Contains(text, "desktop command exited during launch"), strings.Contains(text, "desktop window not visible"):
		return rescueDesktopCommandNotLaunched
	case strings.Contains(text, "browser window not visible"), strings.Contains(text, "browser process not found"):
		return rescueBrowserNotLaunched
	case strings.Contains(text, "can't open display"), strings.Contains(text, "unable to open display"), strings.Contains(text, "display"):
		return rescueDesktopSessionMissing
	case strings.Contains(text, "xfwm4"), strings.Contains(text, "window manager"):
		return rescueWindowManagerMissing
	case strings.Contains(text, "scrot"), strings.Contains(text, "screenshot"):
		return rescueScreenshotCaptureBroken
	case strings.Contains(text, "browser=true requested"), strings.Contains(text, "no such file"), strings.Contains(text, "not found"):
		return rescueBrowserNotLaunched
	default:
		return rescueInputStackDead
	}
}

func trimFailureDetail(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			if len(line) > 240 {
				return line[:240] + "..."
			}
			return line
		}
	}
	return ""
}
