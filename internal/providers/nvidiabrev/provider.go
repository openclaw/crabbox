package nvidiabrev

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string { return providerName }

func (Provider) Aliases() []string {
	return []string{"brev", "nvidia"}
}

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Authentication:   core.DirectProviderAuthentication(core.ProviderAuthenticationCLI),
		Name:             providerName,
		Family:           "nvidia-brev",
		Kind:             core.ProviderKindSSHLease,
		Targets:          []core.TargetSpec{{OS: core.TargetLinux}},
		Features:         core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup},
		Coordinator:      core.CoordinatorNever,
		ClassDisposition: core.ProviderClassDispositionUnmapped,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return RegisterNvidiaBrevProviderFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return ApplyNvidiaBrevProviderFlags(cfg, fs, values)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	applyNvidiaBrevDefaults(&cfg)
	if err := p.ValidateConfig(cfg); err != nil {
		return nil, err
	}
	if cfg.TargetOS != "" && cfg.TargetOS != core.TargetLinux {
		return nil, core.Exit(2, "provider=%s supports target=linux only", providerName)
	}
	if cfg.Tailscale.Enabled || string(cfg.Network) == "tailscale" {
		return nil, core.Exit(2, "--tailscale is not supported for provider=%s; NVIDIA Brev uses CLI-managed SSH access", providerName)
	}
	return NewNvidiaBrevBackend(p.Spec(), cfg, rt), nil
}

func (Provider) ValidateConfig(cfg core.Config) error {
	releaseAction := strings.ToLower(strings.TrimSpace(cfg.NvidiaBrev.ReleaseAction))
	switch releaseAction {
	case "", "delete", "stop":
	default:
		return core.Exit(2, "nvidiaBrev.releaseAction must be delete or stop")
	}

	target := strings.ToLower(strings.TrimSpace(cfg.NvidiaBrev.Target))
	switch target {
	case "", "container", "host":
	default:
		return core.Exit(2, "nvidiaBrev.target must be container or host")
	}
	return nil
}
