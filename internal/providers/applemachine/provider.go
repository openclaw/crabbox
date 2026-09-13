package applemachine

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() { core.RegisterProvider(Provider{}) }

type Provider struct{}

func (Provider) Name() string      { return providerName }
func (Provider) Aliases() []string { return []string{"applemachine"} }

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Authentication:   core.DirectProviderAuthentication(core.ProviderAuthenticationLocalContext),
		Name:             providerName,
		Family:           "container",
		Kind:             core.ProviderKindDelegatedRun,
		Targets:          []core.TargetSpec{{OS: core.TargetLinux}},
		Features:         core.FeatureSet{core.FeatureRunSession},
		Coordinator:      core.CoordinatorNever,
		ClassDisposition: core.ProviderClassDispositionUnmapped,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterAppleMachineConfigFlags(fs, defaults.AppleContainer)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if cfg.TargetOS != "" && cfg.TargetOS != core.TargetLinux {
		return nil, core.Exit(2, "provider=%s supports target=linux only", providerName)
	}
	if cfg.Tailscale.Enabled || string(cfg.Network) == "tailscale" {
		return nil, core.Exit(2, "--tailscale is not supported for provider=%s", providerName)
	}
	if !core.AppleContainerImageExplicit(cfg) {
		cfg.AppleContainer.Image = "alpine:latest"
	}
	return newBackend(p.Spec(), cfg, rt), nil
}
