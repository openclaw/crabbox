package namespaceinstance

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
	return []string{"namespace-compute", "nsc"}
}

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:        providerName,
		Family:      "namespace",
		Kind:        core.ProviderKindSSHLease,
		Targets:     []core.TargetSpec{{OS: core.TargetLinux}},
		Features:    core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup},
		Coordinator: core.CoordinatorNever,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return RegisterProviderFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return ApplyProviderFlags(cfg, fs, values)
}

func (p Provider) ServerTypeForConfig(cfg core.Config) string {
	return namespaceInstanceMachineTypeForConfig(cfg)
}

func (p Provider) ServerTypeForClass(class string) string {
	return namespaceInstanceMachineTypeForClass(class)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if cfg.TargetOS != "" && cfg.TargetOS != core.TargetLinux {
		return nil, core.Exit(2, "provider=%s supports target=linux only", providerName)
	}
	if cfg.Tailscale.Enabled || string(cfg.Network) == "tailscale" {
		return nil, core.Exit(2, "--tailscale is not supported for provider=%s; Namespace Compute instances expose SSH directly", providerName)
	}
	if err := validateNamespaceInstanceConfig(cfg); err != nil {
		return nil, err
	}
	return NewBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		return nil, core.Exit(2, "namespace-instance doctor backend unavailable")
	}
	return doctor, nil
}
