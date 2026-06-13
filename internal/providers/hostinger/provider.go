package hostinger

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string { return providerName }

func (Provider) Aliases() []string { return nil }

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:        providerName,
		Family:      providerName,
		Kind:        core.ProviderKindSSHLease,
		Targets:     []core.TargetSpec{{OS: core.TargetLinux}},
		Features:    core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup},
		Coordinator: core.CoordinatorNever,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return RegisterHostingerProviderFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return ApplyHostingerProviderFlags(cfg, fs, values)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if cfg.TargetOS != "" && cfg.TargetOS != core.TargetLinux {
		return nil, exit(2, "provider=%s managed provisioning supports target=linux only", providerName)
	}
	if cfg.Tailscale.Enabled || string(cfg.Network) == "tailscale" {
		return nil, exit(2, "--tailscale is not supported for provider=%s; hostinger leases expose public SSH only", providerName)
	}
	normalized := cfg
	applyDefaults(&normalized)
	if err := validateHostingerWorkRoot(normalized); err != nil {
		return nil, err
	}
	return NewLeaseBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		return nil, core.Exit(2, "hostinger doctor backend unavailable")
	}
	return doctor, nil
}
