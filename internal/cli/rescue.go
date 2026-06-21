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
	args := crabboxLeaseCommandArgs(ctx, "webvnc", "reset")
	args = append(args, "--open")
	return strings.Join(readableShellWords(args), " ")
}

func webVNCDaemonStartRescueCommand(ctx rescueContext) string {
	args := crabboxLeaseCommandArgs(ctx, "webvnc", "daemon", "start")
	args = append(args, "--open")
	return strings.Join(readableShellWords(args), " ")
}

func desktopLaunchRetryCommand(ctx rescueContext, command []string) string {
	args := crabboxLeaseCommandArgs(ctx, "desktop", "launch")
	args = append(args, "--")
	args = append(args, command...)
	return strings.Join(readableShellWords(args), " ")
}

func crabboxLeaseCommand(ctx rescueContext, command ...string) string {
	return strings.Join(readableShellWords(crabboxLeaseCommandArgs(ctx, command...)), " ")
}

func crabboxLeaseCommandArgs(ctx rescueContext, command ...string) []string {
	targetOS := firstNonBlank(ctx.Target.TargetOS, ctx.Cfg.TargetOS)
	args := append([]string{"crabbox"}, command...)
	if strings.TrimSpace(ctx.Cfg.Provider) != "" {
		args = append(args, "--provider", strings.TrimSpace(ctx.Cfg.Provider))
	}
	if targetOS != "" {
		args = append(args, "--target", targetOS)
	}
	if strings.TrimSpace(ctx.Cfg.Provider) == staticProvider {
		if staticHost := firstNonBlank(ctx.Cfg.Static.Host, ctx.Target.Host); staticHost != "" {
			args = append(args, "--static-host", staticHost)
		}
		if staticUser := firstNonBlank(ctx.Cfg.Static.User, ctx.Target.User); staticUser != "" {
			args = append(args, "--static-user", staticUser)
		}
		if staticPort := firstNonBlank(ctx.Cfg.Static.Port, ctx.Target.Port); staticPort != "" {
			args = append(args, "--static-port", staticPort)
		}
		if strings.TrimSpace(ctx.Cfg.Static.WorkRoot) != "" {
			args = append(args, "--static-work-root", strings.TrimSpace(ctx.Cfg.Static.WorkRoot))
		}
	}
	if ctx.Cfg.Network != "" && ctx.Cfg.Network != NetworkAuto {
		args = append(args, "--network", string(ctx.Cfg.Network))
	}
	windowsMode := firstNonBlank(ctx.Target.WindowsMode, ctx.Cfg.WindowsMode)
	if targetOS == targetWindows && windowsMode != "" {
		args = append(args, "--windows-mode", windowsMode)
	}
	args = append(args, "--id", ctx.LeaseID)
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
