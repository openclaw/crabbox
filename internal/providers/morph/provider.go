package morph

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string      { return providerName }
func (Provider) Aliases() []string { return nil }

func (Provider) ClaimScope(cfg core.Config) string {
	endpoint, err := normalizeMorphAPIURL(cfg.Morph.APIURL)
	if err != nil {
		return ""
	}
	return "endpoint:" + endpoint
}

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Authentication: core.DirectProviderAuthentication(core.ProviderAuthenticationAPIKey),
		Name:           providerName,
		Kind:           core.ProviderKindSSHLease,
		Targets: []core.TargetSpec{{
			OS: targetLinux,
		}},
		Features:         core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync},
		Coordinator:      core.CoordinatorNever,
		ClassDisposition: core.ProviderClassDispositionUnmapped,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return RegisterMorphProviderFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return ApplyMorphProviderFlags(cfg, fs, values)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return NewMorphBackend(p.Spec(), cfg, rt)
}

func (Provider) ServerTypeForConfig(cfg core.Config) string {
	if snapshot := strings.TrimSpace(cfg.Morph.Snapshot); snapshot != "" {
		return snapshot
	}
	return "snapshot"
}
