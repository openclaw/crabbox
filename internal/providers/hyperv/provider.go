package hyperv

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Authentication:   core.DirectProviderAuthentication(core.ProviderAuthenticationLocalContext),
		Name:             providerName,
		Family:           "local-vm",
		Kind:             core.ProviderKindSSHLease,
		Targets:          providerTargets(),
		Features:         providerFeatures(),
		Coordinator:      core.CoordinatorNever,
		ClassDisposition: core.ProviderClassDispositionUnmapped,
		TargetFeatures: map[string]core.FeatureSet{
			core.TargetLinux: append(providerFeatures(), core.FeatureCode),
			core.TargetWindows + "/" + core.WindowsModeNormal: providerFeatures(),
		},
	}
}

func providerTargets() []core.TargetSpec {
	return []core.TargetSpec{
		{OS: core.TargetLinux},
		{OS: core.TargetWindows, WindowsMode: core.WindowsModeNormal},
	}
}

func providerCommonFeatures() core.FeatureSet {
	return core.FeatureSet{
		core.FeatureSSH,
		core.FeatureCrabboxSync,
		core.FeatureDesktop,
		core.FeatureBrowser,
		core.FeatureCleanup,
		core.FeaturePauseResume,
		core.FeatureTailscale,
		core.FeatureCacheVolume,
	}
}

func providerFeatures() core.FeatureSet {
	return append(providerCommonFeatures(),
		core.FeatureCheckpoint,
		core.FeatureFork,
		core.FeatureRestore,
		core.FeatureSnapshot,
	)
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return registerFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return applyFlags(cfg, fs, values)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	targetOS := cfg.TargetOS
	if targetOS == "" {
		targetOS = core.TargetWindows
	}
	if targetOS != core.TargetLinux && targetOS != core.TargetWindows {
		return nil, core.Exit(2, "provider=%s supports target=linux or target=windows", providerName)
	}
	if targetOS == core.TargetWindows && cfg.WindowsMode != "" && cfg.WindowsMode != core.WindowsModeNormal {
		return nil, core.Exit(2, "provider=%s supports windows.mode=normal only", providerName)
	}
	if targetOS == core.TargetLinux && cfg.HyperV.InitPassword {
		return nil, core.Exit(2, "--hyperv-init-password is supported only for target=windows")
	}
	if _, err := secureBootSettings(targetOS, cfg.HyperV.SecureBoot); err != nil {
		return nil, err
	}
	return newBackend(p.Spec(), cfg, rt), nil
}

func (Provider) ConfigDefaultsTargetFinalization() core.ProviderConfigDefaultsTargetFinalization {
	return core.ProviderConfigDefaultsCallerFinalizes
}

func (Provider) ApplyConfigDefaults(cfg *core.Config) error {
	if !core.IsTargetExplicit(cfg) {
		cfg.TargetOS = core.TargetWindows
	}
	cfg.SSHFallbackPorts = nil
	if cfg.HyperV.User != "" {
		cfg.SSHUser = cfg.HyperV.User
	}
	if cfg.HyperV.WorkRoot != "" && !(cfg.TargetOS == core.TargetLinux && cfg.HyperV.WorkRoot == `C:\crabbox`) {
		cfg.WorkRoot = cfg.HyperV.WorkRoot
	}
	cfg.SSHPort = "22"
	return nil
}
