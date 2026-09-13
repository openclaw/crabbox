package freestyle

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

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
func (Provider) ValidateConfig(cfg core.Config) error {
	if cfg.Freestyle.VCPUs < 0 {
		return core.Exit(2, "freestyle vcpus must be non-negative")
	}
	if cfg.Freestyle.MemoryGB < 0 {
		return core.Exit(2, "freestyle memoryGB must be non-negative")
	}
	return nil
}
func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if err := p.ValidateConfig(cfg); err != nil {
		return nil, err
	}
	return NewFreestyleBackend(p.Spec(), cfg, rt), nil
}
