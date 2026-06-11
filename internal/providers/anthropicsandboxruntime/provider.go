package anthropicsandboxruntime

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string { return providerName }

func (Provider) Aliases() []string { return []string{"srt"} }

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:   providerName,
		Family: providerFamily,
		Kind:   core.ProviderKindDelegatedRun,
		Targets: []core.TargetSpec{
			{OS: core.TargetLinux},
			{OS: core.TargetMacOS},
		},
		Features:    core.FeatureSet{},
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
	return validateConfig(cfg)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if err := p.ValidateConfig(cfg); err != nil {
		return nil, err
	}
	return newBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		return nil, exit(2, "anthropic-sandbox-runtime doctor backend unavailable")
	}
	return doctor, nil
}
