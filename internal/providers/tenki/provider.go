package tenki

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string      { return tenkiProvider }
func (Provider) Aliases() []string { return nil }

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:             tenkiProvider,
		Family:           tenkiProvider,
		Kind:             core.ProviderKindSSHLease,
		Targets:          []core.TargetSpec{{OS: core.TargetLinux}},
		Features:         core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync},
		Coordinator:      core.CoordinatorNever,
		ClassDisposition: core.ProviderClassDispositionUnmapped,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return RegisterTenkiProviderFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return ApplyTenkiProviderFlags(cfg, fs, values)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return NewTenkiBackend(p.Spec(), cfg, rt)
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	return shared.ConfigureDoctor("tenki", func() (core.Backend, error) { return p.Configure(cfg, rt) })
}

func (Provider) ServerTypeForConfig(cfg core.Config) string {
	if cfg.Tenki.Image != "" {
		return cfg.Tenki.Image
	}
	if cfg.Tenki.Snapshot != "" {
		return "snapshot"
	}
	return "sandbox"
}

func (Provider) ServerTypeForClass(string) string {
	return "sandbox"
}
