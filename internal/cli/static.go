package cli

import (
	"flag"
	"strings"
	"time"
)

const staticProvider = "ssh"

type targetFlagValues struct {
	Target      *string
	WindowsMode *string
	StaticHost  *string
	StaticUser  *string
	StaticPort  *string
	StaticRoot  *string
}

func registerTargetFlags(fs *flag.FlagSet, defaults Config) targetFlagValues {
	return targetFlagValues{
		Target:      fs.String("target", defaults.TargetOS, "target OS: linux, macos, or windows"),
		WindowsMode: fs.String("windows-mode", defaults.WindowsMode, "Windows mode: normal or wsl2"),
		StaticHost:  fs.String("static-host", defaults.Static.Host, "static SSH host"),
		StaticUser:  fs.String("static-user", defaults.Static.User, "static SSH user"),
		StaticPort:  fs.String("static-port", defaults.Static.Port, "static SSH port"),
		StaticRoot:  fs.String("static-work-root", defaults.Static.WorkRoot, "static target work root"),
	}
}

func applyTargetFlagOverrides(cfg *Config, fs *flag.FlagSet, values targetFlagValues) error {
	if flagWasSet(fs, "target") {
		cfg.TargetOS = *values.Target
		cfg.targetExplicit = true
		cfg.targetFlagExplicit = true
		if normalizeTargetOS(cfg.TargetOS) != targetWindows && !flagWasSet(fs, "windows-mode") {
			cfg.WindowsMode = windowsModeNormal
			cfg.explicitWindowsMode = ""
		}
	}
	if flagWasSet(fs, "windows-mode") {
		cfg.WindowsMode = *values.WindowsMode
		cfg.explicitWindowsMode = *values.WindowsMode
		cfg.windowsModeFlagExplicit = true
	}
	if flagWasSet(fs, "static-host") {
		cfg.Static.Host = *values.StaticHost
	}
	if flagWasSet(fs, "static-user") {
		cfg.Static.User = *values.StaticUser
	}
	if flagWasSet(fs, "static-port") {
		cfg.Static.Port = *values.StaticPort
	}
	if flagWasSet(fs, "static-work-root") {
		cfg.Static.WorkRoot = *values.StaticRoot
	}
	normalizeTargetConfig(cfg)
	return validateTargetConfig(*cfg)
}

func staticLease(cfg Config) (Server, SSHTarget, string, error) {
	if cfg.Static.Host == "" {
		return Server{}, SSHTarget{}, "", exit(2, "provider=%s requires static.host or CRABBOX_STATIC_HOST", cfg.Provider)
	}
	leaseID := strings.TrimSpace(cfg.Static.ID)
	if leaseID == "" {
		leaseID = "static_" + normalizeLeaseSlug(cfg.Static.Host)
	}
	slug := normalizeLeaseSlug(cfg.Static.Name)
	if slug == "" {
		slug = normalizeLeaseSlug(cfg.Static.Host)
	}
	name := cfg.Static.Name
	if name == "" {
		name = "crabbox-" + slug
	}
	now := time.Now().UTC()
	labelCfg := cfg
	labelCfg.ServerType = staticServerType(cfg)
	labels := directLeaseLabels(labelCfg, leaseID, slug, staticProvider, "", true, now)
	labels["target"] = cfg.TargetOS
	if cfg.TargetOS == targetWindows {
		labels["windows_mode"] = cfg.WindowsMode
	}
	server := Server{
		CloudID:  leaseID,
		Provider: staticProvider,
		ID:       0,
		Name:     name,
		Status:   "active",
		Labels:   labels,
	}
	server.PublicNet.IPv4.IP = cfg.Static.Host
	server.ServerType.Name = staticServerType(cfg)
	target := sshTargetForLease(cfg, cfg.Static.Host, firstNonEmpty(cfg.Static.User, cfg.SSHUser), firstNonEmpty(cfg.Static.Port, cfg.SSHPort))
	target.ReadyCheck = staticReadyCommand(target)
	return server, target, leaseID, nil
}

func StaticLease(cfg Config) (Server, SSHTarget, string, error) {
	return staticLease(cfg)
}

func staticReadyCommand(target SSHTarget) string {
	if isWindowsNativeTarget(target) {
		return windowsRemoteDoctor()
	}
	return "git --version >/dev/null && rsync --version >/dev/null && tar --version >/dev/null"
}

func staticServerType(cfg Config) string {
	if cfg.ServerType != "" && cfg.ServerTypeExplicit {
		return cfg.ServerType
	}
	if cfg.TargetOS == targetWindows {
		return "windows-" + cfg.WindowsMode
	}
	if cfg.TargetOS == targetMacOS {
		return "macos"
	}
	return "ssh"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
