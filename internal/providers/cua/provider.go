package cua

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string      { return providerName }
func (Provider) Aliases() []string { return nil }

func (Provider) ServerTypeForConfig(core.Config) string { return "" }
func (Provider) ServerTypeForClass(string) string       { return "" }

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:   providerName,
		Family: providerName,
		Kind:   core.ProviderKindServiceControl,
		Targets: []core.TargetSpec{
			{OS: core.TargetLinux},
			{OS: core.TargetMacOS},
			{OS: core.TargetWindows, WindowsMode: core.WindowsModeNormal},
		},
		Features:         core.FeatureSet{},
		Coordinator:      core.CoordinatorNever,
		ClassDisposition: core.ProviderClassDispositionUnmapped,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return RegisterProviderFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return ApplyProviderFlags(cfg, fs, values)
}

func (Provider) ValidateConfig(cfg core.Config) error {
	return validateProviderConfig(cfg)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if err := p.ValidateConfig(cfg); err != nil {
		return nil, err
	}
	cfg.Provider = providerName
	return backend{spec: p.Spec(), cfg: cfg, rt: rt}, nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	return shared.ConfigureDoctor(providerName, func() (core.Backend, error) { return p.Configure(cfg, rt) })
}
