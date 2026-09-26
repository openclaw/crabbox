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
	Static      StaticConfigFlagValues
	Commands    staticCommandFlagValues
}

func registerTargetFlags(fs *flag.FlagSet, defaults Config) targetFlagValues {
	return targetFlagValues{
		Target:      fs.String("target", defaults.TargetOS, "target OS: linux, macos, or windows"),
		WindowsMode: fs.String("windows-mode", defaults.WindowsMode, "Windows mode: normal or wsl2"),
		Static:      RegisterStaticConfigFlags(fs, defaults.Static),
		Commands:    registerStaticCommandFlags(fs),
	}
}

func applyTargetFlagOverrides(cfg *Config, fs *flag.FlagSet, values targetFlagValues) error {
	if flagWasSet(fs, "target") {
		cfg.TargetOS = *values.Target
		cfg.targetExplicit = true
		cfg.targetFlagExplicit = true
		cfg.credentialProvenance.externalDesktopTarget = credentialSourceFlag
		recordConfigInput(cfg, configInputGeneric, configInputFlag, true)
		if normalizeTargetOS(cfg.TargetOS) != targetWindows && !flagWasSet(fs, "windows-mode") {
			cfg.WindowsMode = windowsModeNormal
			cfg.explicitWindowsMode = ""
			cfg.credentialProvenance.externalDesktopMode = credentialSourceFlag
		}
	}
	if flagWasSet(fs, "windows-mode") {
		cfg.WindowsMode = *values.WindowsMode
		cfg.explicitWindowsMode = *values.WindowsMode
		cfg.windowsModeFlagExplicit = true
		recordConfigInput(cfg, configInputGeneric, configInputFlag, true)
		cfg.credentialProvenance.externalDesktopMode = credentialSourceFlag
	}
	applied, err := values.Static.Apply(&cfg.Static, fs)
	recordConfigInput(cfg, "ssh", configInputFlag, applied.InputAccepted)
	// Target validation can fail after this explicit host approval has applied.
	if applied.Host {
		cfg.credentialProvenance.staticHost = credentialSourceFlag
	}
	if flagWasSet(fs, "static-user") {
		cfg.credentialProvenance.staticUser = credentialSourceFlag
	}
	if flagWasSet(fs, "static-port") {
		cfg.credentialProvenance.staticPort = credentialSourceFlag
	}
	if err != nil {
		return err
	}
	if err := applyStaticCommandFlags(cfg, fs, values.Commands); err != nil {
		return err
	}
	normalizeTargetConfig(cfg)
	return validateTargetConfig(*cfg)
}

func staticLease(cfg Config) (Server, SSHTarget, string, error) {
	if cfg.Static.Host == "" {
		return Server{}, SSHTarget{}, "", Exit(2, "provider=%s requires static.host or CRABBOX_STATIC_HOST", cfg.Provider)
	}
	leaseID := strings.TrimSpace(cfg.Static.ID)
	if leaseID == "" {
		leaseID = "static_" + NormalizeLeaseSlug(cfg.Static.Host)
	}
	slug := NormalizeLeaseSlug(cfg.Static.Name)
	if slug == "" {
		slug = NormalizeLeaseSlug(cfg.Static.Host)
	}
	name := cfg.Static.Name
	if name == "" {
		name = "crabbox-" + slug
	}
	now := time.Now().UTC()
	labelCfg := cfg
	labelCfg.ServerType = staticServerType(cfg)
	labels := DirectLeaseLabels(labelCfg, leaseID, slug, staticProvider, "", true, now)
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
