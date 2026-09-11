package daytona

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string { return "daytona" }
func (Provider) Aliases() []string {
	return nil
}
func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Authentication: core.ProviderAuthentication{
			{Route: "direct", Methods: []core.ProviderAuthenticationMethod{core.ProviderAuthenticationAPIKey, core.ProviderAuthenticationAPIToken}, Description: "Direct API-key or JWT/OAuth access-token credentials may also come from a saved Daytona profile."},
			{Route: "brokered", Methods: []core.ProviderAuthenticationMethod{core.ProviderAuthenticationCoordinator}, Description: "The client authenticates to the coordinator; Daytona credentials remain server-side."},
		},
		Name:             "daytona",
		Family:           "daytona",
		Kind:             core.ProviderKindSSHLease,
		Targets:          []core.TargetSpec{{OS: core.TargetLinux}},
		Features:         core.FeatureSet{core.FeatureSSH, core.FeatureSSHScriptRun, core.FeatureCrabboxSync, core.FeatureArchiveSync, core.FeatureCheckpoint, core.FeatureFork, core.FeatureSnapshot},
		Coordinator:      core.CoordinatorSupported,
		ClassDisposition: core.ProviderClassDispositionMapped,
	}
}
func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return RegisterDaytonaProviderFlags(fs, defaults)
}
func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return ApplyDaytonaProviderFlags(cfg, fs, values)
}
func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if cfg.Daytona.ForgetMissing && core.ShouldUseCoordinator(cfg, p.Spec()) {
		return nil, core.Exit(2, "--daytona-forget-missing requires direct provider=daytona; brokered leases require normal stop")
	}
	return NewDaytonaLeaseBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	return shared.ConfigureDoctor("daytona", func() (core.Backend, error) { return p.Configure(cfg, rt) })
}
