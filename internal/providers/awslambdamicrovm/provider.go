package awslambdamicrovm

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() { core.RegisterProvider(Provider{}) }

type Provider struct{}

func (Provider) Name() string      { return providerName }
func (Provider) Aliases() []string { return nil }
func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:        providerName,
		Family:      "aws",
		Kind:        core.ProviderKindDelegatedRun,
		Targets:     []core.TargetSpec{{OS: core.TargetLinux}},
		Features:    core.FeatureSet{core.FeatureArchiveSync, core.FeatureCleanup, core.FeatureRunSession, core.FeaturePauseResume},
		Coordinator: core.CoordinatorNever,
	}
}
func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return registerFlags(fs, defaults)
}
func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return applyFlags(cfg, fs, values)
}
func (Provider) ValidateConfig(cfg core.Config) error {
	if cfg.Tailscale.Enabled || cfg.Network == core.NetworkTailscale {
		return core.Exit(2, "provider=%s does not support Tailscale options", providerName)
	}
	return validateConfig(cfg)
}
func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if err := p.ValidateConfig(cfg); err != nil {
		return nil, err
	}
	cfg.Provider = providerName
	return newBackend(p.Spec(), cfg, rt), nil
}
func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		return nil, core.Exit(2, "%s doctor backend unavailable", providerName)
	}
	return doctor, nil
}
