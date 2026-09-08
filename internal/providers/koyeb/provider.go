package koyeb

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

const providerName = "koyeb"

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

var (
	_ core.Provider                       = Provider{}
	_ core.DoctorProvider                 = Provider{}
	_ core.ProviderArchitectureCapability = Provider{}
	_ core.ProviderConfigDefaulter        = Provider{}
	_ core.ProviderSSHTargetConfigurer    = Provider{}
	_ core.ProviderServerTypeProvider     = Provider{}
)

func (Provider) Name() string      { return providerName }
func (Provider) Aliases() []string { return nil }

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:             providerName,
		Family:           providerName,
		Kind:             core.ProviderKindSSHLease,
		Targets:          []core.TargetSpec{{OS: core.TargetLinux}},
		Features:         core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup, core.FeatureDesktop, core.FeatureBrowser, core.FeatureCode, core.FeatureTailscale},
		Coordinator:      core.CoordinatorSupported,
		ClassDisposition: core.ProviderClassDispositionUnmapped,
	}
}

func (Provider) RegisterFlags(*flag.FlagSet, core.Config) any { return core.NoProviderFlags() }

func (Provider) ApplyFlags(*core.Config, *flag.FlagSet, any) error { return nil }

func (Provider) ApplyConfigDefaults(cfg *core.Config) error {
	cfg.Provider = providerName
	// Koyeb allocation is coordinator-owned. Do not leak the compiled Hetzner
	// location and image defaults into the coordinator request.
	cfg.Location = ""
	cfg.Image = ""
	return nil
}

func (Provider) SupportsArchitecture(_ core.Config, architecture string) bool {
	return architecture == core.ArchitectureAMD64
}

func (Provider) ConfigureSSHTarget(target *core.SSHTarget, _ string) {
	if target.TargetOS != core.TargetLinux {
		return
	}
	target.ProxyCommand = "tailscale nc %h %p"
	target.SSHConfigProxy = true
	target.ReadyCheck = "crabbox-ready"
}

func (Provider) ServerTypeForConfig(cfg core.Config) string { return cfg.ServerType }

func (Provider) ServerTypeForClass(string) string { return "" }

func (p Provider) Configure(core.Config, core.Runtime) (core.Backend, error) {
	return newBackend(p.Spec()), nil
}

func (p Provider) ConfigureDoctor(core.Config, core.Runtime) (core.DoctorBackend, error) {
	return newBackend(p.Spec()), nil
}
