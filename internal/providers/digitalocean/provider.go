package digitalocean

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
func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:        providerName,
		Family:      providerName,
		Kind:        core.ProviderKindSSHLease,
		Targets:     []core.TargetSpec{{OS: core.TargetLinux}},
		Features:    core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup, core.FeatureTailscale},
		Coordinator: core.CoordinatorNever,
	}
}

func (Provider) RegisterFlags(*flag.FlagSet, core.Config) any { return core.NoProviderFlags() }
func (Provider) ApplyFlags(*core.Config, *flag.FlagSet, any) error {
	return nil
}

func (p Provider) ServerTypeForConfig(cfg core.Config) string {
	if cfg.ServerType != "" {
		return cfg.ServerType
	}
	return digitalOceanServerTypeForClass(cfg.Class)
}

func (Provider) ServerTypeForClass(class string) string {
	return digitalOceanServerTypeForClass(class)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return NewDigitalOceanLeaseBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		return nil, core.Exit(2, "digitalocean doctor backend unavailable")
	}
	return doctor, nil
}

func digitalOceanServerTypeForClass(class string) string {
	switch class {
	case "standard", "fast", "large", "beast":
		return "s-1vcpu-1gb"
	default:
		return "s-1vcpu-1gb"
	}
}
