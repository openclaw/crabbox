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
		Name:        providerName,
		Family:      "nvidia-brev",
		Kind:        core.ProviderKindSSHLease,
		Targets:     []core.TargetSpec{{OS: core.TargetLinux}},
		Features:    core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup},
		Coordinator: core.CoordinatorNever,
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
		return nil, exit(2, "provider=%s supports target=linux only", providerName)
	}
	if cfg.Tailscale.Enabled || string(cfg.Network) == "tailscale" {
		return nil, exit(2, "--tailscale is not supported for provider=%s; NVIDIA Brev uses CLI-managed SSH access", providerName)
	}
	return NewNvidiaBrevBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		return nil, core.Exit(2, "nvidia-brev doctor backend unavailable")
	}
	return doctor, nil
}

func (Provider) ValidateConfig(cfg core.Config) error {
	releaseAction := strings.ToLower(strings.TrimSpace(cfg.NvidiaBrev.ReleaseAction))
	switch releaseAction {
	case "", "delete", "stop":
	default:
		return exit(2, "nvidiaBrev.releaseAction must be delete or stop")
	}

	target := strings.ToLower(strings.TrimSpace(cfg.NvidiaBrev.Target))
	switch target {
	case "", "container", "host":
	default:
		return exit(2, "nvidiaBrev.target must be container or host")
	}
	return nil
}
