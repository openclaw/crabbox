package blacksmith

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

var _ core.RunOptionsValidator = Provider{}

func (p Provider) ValidateRunOptions(req core.RunRequest) error {
	return validateBlacksmithRunOptions(p.Spec(), req)
}

func (Provider) Name() string { return "blacksmith-testbox" }
func (Provider) Aliases() []string {
	return []string{"blacksmith"}
}
func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:             "blacksmith-testbox",
		Family:           "blacksmith",
		Kind:             core.ProviderKindDelegatedRun,
		Targets:          []core.TargetSpec{{OS: core.TargetLinux}},
		Features:         core.FeatureSet{core.FeatureCacheVolume, core.FeatureRunProof, core.FeatureRunSession, core.FeatureRunArtifacts},
		Coordinator:      core.CoordinatorNever,
		ClassDisposition: core.ProviderClassDispositionUnmapped,
	}
}
func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return RegisterBlacksmithProviderFlags(fs, defaults)
}
func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return ApplyBlacksmithProviderFlags(cfg, fs, values)
}
func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return NewBlacksmithBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	return shared.ConfigureDoctor("blacksmith-testbox", func() (core.Backend, error) { return p.Configure(cfg, rt) })
}
