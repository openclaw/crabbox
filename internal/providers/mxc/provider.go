package mxc

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() { core.RegisterProvider(Provider{}) }

type Provider struct{}

func (Provider) Name() string      { return providerName }
func (Provider) Aliases() []string { return []string{"execution-container"} }
func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:        providerName,
		Family:      "sandbox",
		Kind:        core.ProviderKindDelegatedRun,
		Targets:     []core.TargetSpec{{OS: core.TargetWindows, WindowsMode: core.WindowsModeNormal}},
		Features:    core.FeatureSet{},
		Coordinator: core.CoordinatorNever,
	}
}
func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return registerFlags(fs, defaults)
}
func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return applyFlags(cfg, fs, values)
}
func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if cfg.TargetOS != "" && cfg.TargetOS != core.TargetWindows {
		return nil, core.Exit(2, "provider=mxc supports target=windows only")
	}
	if cfg.WindowsMode != "" && cfg.WindowsMode != core.WindowsModeNormal {
		return nil, core.Exit(2, "provider=mxc supports native Windows mode only")
	}
	containment := strings.ToLower(strings.TrimSpace(cfg.MXC.Containment))
	if containment != "processcontainer" && !cfg.MXC.Experimental {
		return nil, core.Exit(2, "MXC containment %q is experimental; pass --mxc-experimental to enable it", containment)
	}
	cfg.MXC.Containment = containment
	return newBackend(p.Spec(), cfg, rt), nil
}
func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	return backend.(core.DoctorBackend), nil
}
