package e2b

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string      { return e2bProvider }
func (Provider) Aliases() []string { return nil }

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		SyncGuardrailFullCandidate: true,
		Name:                       e2bProvider,
		Family:                     "e2b",
		Kind:                       core.ProviderKindDelegatedRun,
		Targets:                    []core.TargetSpec{{OS: core.TargetLinux}},
		Features:                   core.FeatureSet{core.FeatureURLBridge, core.FeatureRunSession},
		Coordinator:                core.CoordinatorNever,
		ClassDisposition:           core.ProviderClassDispositionUnmapped,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return RegisterE2BProviderFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return ApplyE2BProviderFlags(cfg, fs, values)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return NewE2BBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	return shared.ConfigureDoctor("e2b", func() (core.Backend, error) { return p.Configure(cfg, rt) })
}
