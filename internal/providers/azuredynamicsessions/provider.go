package azuredynamicsessions

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

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		SyncGuardrailFullCandidate: true,
		Name:                       providerName,
		Family:                     "azure",
		Kind:                       core.ProviderKindDelegatedRun,
		Targets:                    []core.TargetSpec{{OS: core.TargetLinux}},
		Features:                   core.FeatureSet{core.FeatureArchiveSync, core.FeatureRunSession},
		Coordinator:                core.CoordinatorNever,
		ClassDisposition:           core.ProviderClassDispositionUnmapped,
	}
}

func (Provider) RouteConfig(cfg *core.Config, _ *flag.FlagSet, _ any) error {
	cfg.AzureBackend = core.AzureBackendDynamicSessions
	return nil
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return RegisterAzureDynamicSessionsProviderFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return ApplyAzureDynamicSessionsProviderFlags(cfg, fs, values)
}

func (Provider) ServerTypeForConfig(core.Config) string { return "" }

func (Provider) ServerTypeForClass(string) string { return "" }

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if cfg.TargetOS != "" && cfg.TargetOS != core.TargetLinux {
		return nil, core.Exit(2, "%s supports target=linux only", providerName)
	}
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	return NewAzureDynamicSessionsBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	return shared.ConfigureDoctor(providerName, func() (core.Backend, error) { return p.Configure(cfg, rt) })
}
