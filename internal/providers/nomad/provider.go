package nomad

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
		Name:             providerName,
		Family:           providerName,
		Kind:             core.ProviderKindDelegatedRun,
		Targets:          []core.TargetSpec{{OS: core.TargetLinux}},
		Features:         core.FeatureSet{core.FeatureArchiveSync, core.FeatureCleanup, core.FeatureRunSession},
		Coordinator:      core.CoordinatorNever,
		ClassDisposition: core.ProviderClassDispositionUnmapped,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return RegisterNomadProviderFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return ApplyNomadProviderFlags(cfg, fs, values)
}

func (Provider) ValidateConfig(cfg core.Config) error {
	return validateConfig(cfg)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if err := p.ValidateConfig(cfg); err != nil {
		return nil, err
	}
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	return &backend{spec: p.Spec(), cfg: cfg, rt: rt, clientFactory: newNomadClient}, nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	return shared.ConfigureDoctor("nomad", func() (core.Backend, error) { return p.Configure(cfg, rt) })
}

type backend struct {
	spec          ProviderSpec
	cfg           Config
	rt            Runtime
	clientFactory func(Config, Runtime) (Client, error)
}

func (b *backend) Spec() ProviderSpec { return b.spec }
