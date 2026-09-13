package freestyle

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string { return "freestyle" }
func (Provider) Aliases() []string {
	return nil
}

// ServerTypeForConfig implements ProviderServerTypeProvider
// so core needs no provider == "freestyle" special-case. Delegated-run Freestyle
// VMs have no server-type concept, so both return "".
func (Provider) ServerTypeForConfig(core.Config) string { return "" }
func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Authentication:             core.DirectProviderAuthentication(core.ProviderAuthenticationAPIKey),
		Name:                       "freestyle",
		SyncGuardrailFullCandidate: true,
		Kind:                       core.ProviderKindDelegatedRun,
		Targets:                    []core.TargetSpec{{OS: core.TargetLinux}},
		Features:                   core.FeatureSet{core.FeatureArchiveSync, core.FeatureRunSession},
		Coordinator:                core.CoordinatorNever,
		ClassDisposition:           core.ProviderClassDispositionUnmapped,
	}
}
func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return RegisterFreestyleProviderFlags(fs, defaults)
}
func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return ApplyFreestyleProviderFlags(cfg, fs, values)
}
func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return NewFreestyleBackend(p.Spec(), cfg, rt), nil
}
