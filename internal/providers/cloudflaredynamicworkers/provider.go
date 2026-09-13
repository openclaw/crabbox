package cloudflaredynamicworkers

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string { return providerName }

func (Provider) Aliases() []string {
	return []string{"cf-dynamic", "cfdw"}
}

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Authentication:   core.DirectProviderAuthentication(core.ProviderAuthenticationAPIToken),
		Name:             providerName,
		Family:           "cloudflare",
		Kind:             core.ProviderKindDelegatedRun,
		Targets:          []core.TargetSpec{{OS: core.TargetWorkerRuntime}},
		Features:         core.FeatureSet{core.FeatureCleanup, core.FeatureModuleRun, core.FeatureRunSession},
		Coordinator:      core.CoordinatorNever,
		ClassDisposition: core.ProviderClassDispositionUnmapped,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return RegisterProviderFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return ApplyProviderFlags(cfg, fs, values)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if cfg.TargetOS != "" && cfg.TargetOS != core.TargetWorkerRuntime {
		if cfg.TargetOS == core.TargetLinux && !core.IsTargetExplicit(&cfg) {
			cfg.TargetOS = core.TargetWorkerRuntime
		} else {
			return nil, core.Exit(2, "%s supports target=worker-runtime only", providerName)
		}
	}
	if err := validateProviderConfig(cfg); err != nil {
		return nil, err
	}
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetWorkerRuntime
	return NewBackend(p.Spec(), cfg, rt), nil
}
