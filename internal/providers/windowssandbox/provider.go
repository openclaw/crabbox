package windowssandbox

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
		Aliases:          []string{"wsb", "windows-sandbox-provider"},
		Authentication:   core.DirectProviderAuthentication(core.ProviderAuthenticationLocalContext),
		Name:             providerName,
		Family:           "local-sandbox",
		Kind:             core.ProviderKindDelegatedRun,
		Targets:          []core.TargetSpec{{OS: core.TargetWindows, WindowsMode: core.WindowsModeNormal}},
		Features:         core.FeatureSet{core.FeatureArchiveSync},
		Coordinator:      core.CoordinatorNever,
		ClassDisposition: core.ProviderClassDispositionUnmapped,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return registerFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return applyFlags(cfg, fs, values)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	applyDefaults(&cfg)
	if err := validateWindowsSandboxConfig(cfg); err != nil {
		return nil, err
	}
	if cfg.TargetOS != core.TargetWindows || cfg.WindowsMode != core.WindowsModeNormal {
		return nil, core.Exit(2, "provider=%s supports target=windows windows.mode=normal only", providerName)
	}
	if cfg.Tailscale.Enabled || string(cfg.Network) == "tailscale" {
		return nil, core.Exit(2, "--tailscale is not supported for provider=%s; Windows Sandbox networking is controlled by --windows-sandbox-networking", providerName)
	}
	return newBackend(p.Spec(), cfg, rt), nil
}

func (Provider) ConfigDefaultsTargetFinalization() core.ProviderConfigDefaultsTargetFinalization {
	return core.ProviderConfigDefaultsCallerFinalizes
}

func (Provider) ApplyConfigDefaults(cfg *core.Config) error {
	if core.IsTargetExplicit(cfg) && core.NormalizeTargetOS(cfg.TargetOS) != core.TargetWindows {
		return core.Exit(2, "provider=windows-sandbox supports target=windows only")
	}
	if cfg.TargetOS == "" || (!core.IsTargetExplicit(cfg) && cfg.TargetOS == core.TargetLinux) {
		cfg.TargetOS = core.TargetWindows
	}
	if core.ExplicitWindowsModeValue(*cfg) != "" && core.NormalizeWindowsMode(core.ExplicitWindowsModeValue(*cfg)) != core.WindowsModeNormal {
		return core.Exit(2, "provider=windows-sandbox supports windows.mode=normal only")
	}
	cfg.WindowsMode = core.WindowsModeNormal
	if cfg.WindowsSandbox.Workdir == "" {
		cfg.WindowsSandbox.Workdir = `C:\crabbox-work`
	}
	cfg.WorkRoot = cfg.WindowsSandbox.Workdir
	return nil
}
