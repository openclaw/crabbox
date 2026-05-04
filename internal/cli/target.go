package cli

import (
	"path"
	"strings"
)

const (
	targetLinux   = "linux"
	targetMacOS   = "macos"
	targetWindows = "windows"

	windowsModeNormal = "normal"
	windowsModeWSL2   = "wsl2"
)

func normalizeTargetConfig(cfg *Config) {
	cfg.TargetOS = normalizeTargetOS(cfg.TargetOS)
	cfg.WindowsMode = normalizeWindowsMode(cfg.WindowsMode)
	if cfg.TargetOS == targetWindows && cfg.WorkRoot == "/work/crabbox" {
		if cfg.WindowsMode == windowsModeWSL2 {
			cfg.WorkRoot = "/work/crabbox"
		} else {
			cfg.WorkRoot = `C:\crabbox`
		}
	}
	if cfg.Provider == "aws" && cfg.TargetOS == targetMacOS && cfg.SSHUser == baseConfig().SSHUser {
		cfg.SSHUser = "ec2-user"
	}
	if cfg.Provider == "aws" && cfg.TargetOS == targetWindows && cfg.WindowsMode == windowsModeWSL2 && cfg.SSHUser == baseConfig().SSHUser {
		cfg.SSHUser = "Administrator"
	}
	if cfg.Static.User != "" && cfg.SSHUser == baseConfig().SSHUser {
		cfg.SSHUser = cfg.Static.User
	}
	if cfg.Static.Port != "" && cfg.SSHPort == baseConfig().SSHPort {
		cfg.SSHPort = cfg.Static.Port
	}
	if cfg.Static.WorkRoot != "" {
		cfg.WorkRoot = cfg.Static.WorkRoot
	}
}

func normalizeTargetOS(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "linux", "ubuntu":
		return targetLinux
	case "mac", "macos", "darwin", "osx":
		return targetMacOS
	case "win", "windows":
		return targetWindows
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func normalizeWindowsMode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "normal", "native", "powershell", "ps":
		return windowsModeNormal
	case "wsl", "wsl2":
		return windowsModeWSL2
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func validateTargetConfig(cfg Config) error {
	switch cfg.TargetOS {
	case targetLinux, targetMacOS, targetWindows:
	default:
		return exit(2, "target must be linux, macos, or windows")
	}
	if cfg.TargetOS != targetWindows && cfg.WindowsMode != windowsModeNormal {
		return exit(2, "windows.mode is only valid with target=windows")
	}
	if cfg.TargetOS == targetWindows {
		switch cfg.WindowsMode {
		case windowsModeNormal, windowsModeWSL2:
		default:
			return exit(2, "windows.mode must be normal or wsl2")
		}
	}
	return nil
}

func validateProviderTarget(cfg Config) error {
	if isStaticProvider(cfg.Provider) || isBlacksmithProvider(cfg.Provider) {
		return nil
	}
	if cfg.Provider == "aws" && cfg.TargetOS == targetWindows && cfg.WindowsMode == windowsModeNormal {
		return nil
	}
	if cfg.Provider == "aws" && cfg.TargetOS == targetWindows && cfg.WindowsMode == windowsModeWSL2 {
		return nil
	}
	if cfg.Provider == "aws" && cfg.TargetOS == targetMacOS {
		if cfg.AWSMacHostID == "" && cfg.Coordinator == "" {
			return exit(2, "provider=aws target=macos requires CRABBOX_AWS_MAC_HOST_ID or aws.macHostId for an allocated EC2 Mac Dedicated Host")
		}
		if cfg.Capacity.Market != "on-demand" {
			return exit(2, "provider=aws target=macos requires --market on-demand; EC2 Mac instances are not Spot")
		}
		return nil
	}
	if cfg.TargetOS != targetLinux {
		return exit(2, "%s", unsupportedManagedTargetMessage(cfg.Provider, cfg.TargetOS))
	}
	return nil
}

func unsupportedManagedTargetMessage(provider, target string) string {
	switch target {
	case targetWindows:
		return sprintf("provider=%s managed provisioning supports target=linux only; use provider=aws for managed Windows or provider=ssh for existing Windows hosts", provider)
	case targetMacOS:
		return sprintf("provider=%s managed provisioning supports target=linux only; use provider=aws with an EC2 Mac Dedicated Host or provider=ssh for existing macOS hosts", provider)
	default:
		return sprintf("provider=%s managed provisioning supports target=linux only", provider)
	}
}

func newTargetCoordinatorClient(cfg Config) (*CoordinatorClient, bool, error) {
	if isStaticProvider(cfg.Provider) {
		return nil, false, nil
	}
	return newCoordinatorClient(cfg)
}

func isStaticProvider(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "ssh", "static", "static-ssh":
		return true
	default:
		return false
	}
}

func isWindowsNativeTarget(target SSHTarget) bool {
	return target.TargetOS == targetWindows && target.WindowsMode == windowsModeNormal
}

func isWindowsWSL2Target(target SSHTarget) bool {
	return target.TargetOS == targetWindows && target.WindowsMode == windowsModeWSL2
}

func isPOSIXTarget(target SSHTarget) bool {
	return !isWindowsNativeTarget(target)
}

func remoteJoin(cfg Config, parts ...string) string {
	values := make([]string, 0, len(parts)+1)
	if cfg.WorkRoot != "" {
		values = append(values, cfg.WorkRoot)
	}
	values = append(values, parts...)
	if cfg.TargetOS == targetWindows && cfg.WindowsMode == windowsModeNormal {
		return windowsPathJoin(values...)
	}
	return path.Join(values...)
}

func windowsPathJoin(parts ...string) string {
	out := ""
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		part = strings.ReplaceAll(part, "/", `\`)
		if out == "" {
			out = strings.TrimRight(part, `\`)
			continue
		}
		out = strings.TrimRight(out, `\`) + `\` + strings.Trim(part, `\`)
	}
	return out
}
