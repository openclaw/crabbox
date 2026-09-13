package blaxel

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string      { return providerName }
func (Provider) Aliases() []string { return nil }

func (Provider) ServerTypeForConfig(core.Config) string { return "" }
func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Authentication:             core.DirectProviderAuthentication(core.ProviderAuthenticationAPIKey),
		SyncGuardrailFullCandidate: true,
		Name:                       providerName,
		Family:                     providerName,
		Kind:                       core.ProviderKindDelegatedRun,
		Targets:                    []core.TargetSpec{{OS: core.TargetLinux}},
		Features:                   core.FeatureSet{core.FeatureArchiveSync, core.FeatureCleanup, core.FeatureRunSession},
		Coordinator:                core.CoordinatorNever,
		ClassDisposition:           core.ProviderClassDispositionUnmapped,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return RegisterBlaxelProviderFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return ApplyBlaxelProviderFlags(cfg, fs, values)
}

func (Provider) ValidateConfig(cfg core.Config) error {
	return validateBlaxelConfig(cfg)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if err := p.ValidateConfig(cfg); err != nil {
		return nil, err
	}
	cfg.Provider = providerName
	return &backend{spec: p.Spec(), cfg: cfg, rt: rt, clientFactory: newBlaxelClient}, nil
}

type backend struct {
	spec          core.ProviderSpec
	cfg           core.Config
	rt            core.Runtime
	clientFactory func(core.Config, core.Runtime) (Client, error)
}

func (b *backend) Spec() core.ProviderSpec { return b.spec }
