package smolvm

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string { return providerName }

func (Provider) Aliases() []string {
	return []string{"smol", "smolmachines", "smolfleet"}
}

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:        providerName,
		Family:      "smolvm",
		Kind:        core.ProviderKindDelegatedRun,
		Targets:     []core.TargetSpec{{OS: core.TargetLinux}},
		Features:    core.FeatureSet{core.FeatureArchiveSync, core.FeatureRunSession},
		Coordinator: core.CoordinatorNever,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return RegisterSmolvmProviderFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return ApplySmolvmProviderFlags(cfg, fs, values)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return NewBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		return nil, core.Exit(2, "smolvm doctor backend unavailable")
	}
	return doctor, nil
}
