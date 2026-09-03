package islo

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
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
		ClassDisposition:    core.ProviderClassDispositionUnmapped,
		TailscaleEgressOnly: true,
	}
}
func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return RegisterIsloProviderFlags(fs, defaults)
}
func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return ApplyIsloProviderFlags(cfg, fs, values)
}

// ClaimScope contributes the API endpoint identity to every claim core writes
// for this provider, the same way the other sandbox providers do, so a claim
// created against one endpoint is never matched against another.
func (Provider) ClaimScope(cfg core.Config) string { return isloClaimScope(cfg) }

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return NewIsloBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	return shared.ConfigureDoctor("islo", func() (core.Backend, error) { return p.Configure(cfg, rt) })
}
