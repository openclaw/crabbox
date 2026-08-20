package railway

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
func (Provider) Aliases() []string { return []string{"rail", "railwayapp"} }

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:             providerName,
		Family:           "railway",
		Kind:             core.ProviderKindServiceControl,
		Targets:          []core.TargetSpec{{OS: core.TargetLinux}},
		Features:         core.FeatureSet{core.FeatureURLBridge},
		Coordinator:      core.CoordinatorNever,
		ClassDisposition: core.ProviderClassDispositionUnmapped,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return RegisterRailwayProviderFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return ApplyRailwayProviderFlags(cfg, fs, values)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return NewRailwayBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	return shared.ConfigureDoctor("railway", func() (core.Backend, error) { return p.Configure(cfg, rt) })
}
