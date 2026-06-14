package islo

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string { return "islo" }
func (Provider) Aliases() []string {
	return nil
}
func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:                "islo",
		Family:              "islo",
		Kind:                core.ProviderKindDelegatedRun,
		Targets:             []core.TargetSpec{{OS: core.TargetLinux}},
		Features:            core.FeatureSet{core.FeatureSSH, core.FeatureURLBridge, core.FeatureRunSession, core.FeatureTailscale, core.FeaturePauseResume, core.FeatureRunDownloads},
		Coordinator:         core.CoordinatorNever,
		TailscaleEgressOnly: true,
	}
}
func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return RegisterIsloProviderFlags(fs, defaults)
}
func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return ApplyIsloProviderFlags(cfg, fs, values)
}
func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return NewIsloBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		return nil, core.Exit(2, "islo doctor backend unavailable")
	}
	return doctor, nil
}
