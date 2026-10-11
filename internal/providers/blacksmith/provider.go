package blacksmith

import (
	"flag"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

// GitHub cancels any workflow run after 35 days, so a Testbox claim unused for
// longer has necessarily settled and no stop retry can still need it.
const blacksmithClaimExpiryBound = 36 * 24 * time.Hour

type Provider struct{}

var _ core.RunOptionsValidator = Provider{}

func (p Provider) ValidateRunOptions(req core.RunRequest) error {
	return validateBlacksmithRunOptions(p.Spec(), req)
}

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Aliases:          []string{"blacksmith"},
		Authentication:   core.DirectProviderAuthentication(core.ProviderAuthenticationCLI),
		Name:             "blacksmith-testbox",
		Family:           "blacksmith",
		Kind:             core.ProviderKindDelegatedRun,
		Targets:          []core.TargetSpec{{OS: core.TargetLinux}},
		Features:         core.FeatureSet{core.FeatureCacheVolume, core.FeatureRunProof, core.FeatureRunSession, core.FeatureRunArtifacts, core.FeaturePreparedArtifactWorkspace},
		Coordinator:      core.CoordinatorNever,
		ClassDisposition: core.ProviderClassDispositionUnmapped,
		ClaimExpiryBound: blacksmithClaimExpiryBound,
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
