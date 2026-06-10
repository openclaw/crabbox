package incus

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

const providerName = "incus"

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string      { return providerName }
func (Provider) Aliases() []string { return nil }

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:        providerName,
		Family:      "local-vm",
		Kind:        core.ProviderKindSSHLease,
		Targets:     []core.TargetSpec{{OS: core.TargetLinux}},
		Features:    core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup},
		Coordinator: core.CoordinatorNever,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return registerFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return applyFlags(cfg, fs, values)
}

func (Provider) ServerTypeForConfig(cfg core.Config) string {
	return core.IncusServerTypeForConfig(cfg)
}

func (Provider) ServerTypeForClass(class string) string {
	_ = class
	return "container"
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if cfg.TargetOS != "" && cfg.TargetOS != core.TargetLinux {
		return nil, core.Exit(2, "provider=%s supports target=linux only", providerName)
	}
	if cfg.Tailscale.Enabled || string(cfg.Network) == "tailscale" {
		return nil, core.Exit(2, "--tailscale is not supported for provider=%s; use a remote SSH provider when tailnet reachability is required", providerName)
	}
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	return newBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		return nil, core.Exit(2, "%s doctor backend unavailable", providerName)
	}
	return doctor, nil
}

func validateConfig(cfg core.Config) error {
	instanceType := normalizeInstanceType(cfg.Incus.InstanceType)
	if instanceType == "" {
		return core.Exit(2, "provider=%s requires incus.instanceType to be container or vm", providerName)
	}
	if strings.TrimSpace(cfg.Incus.Image) == "" {
		return core.Exit(2, "provider=%s requires incus.image", providerName)
	}
	if instanceType == "virtual-machine" && strings.TrimSpace(cfg.Incus.ProxyListenPort) != "" {
		return core.Exit(2, "provider=%s does not support incus.proxyListenPort with virtual-machine instances; Incus VM proxy devices require a preconfigured static NIC, so omit the proxy or use a container", providerName)
	}
	return nil
}
