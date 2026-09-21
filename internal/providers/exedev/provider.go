package exedev

import (
	"flag"
	"os"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) ServerTypeForConfig(cfg core.Config) string {
	return core.Blank(cfg.ExeDev.Image, core.ExeDevDefaultImageLabel)
}

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Aliases:          []string{"exe", "exedev"},
		Authentication:   core.DirectProviderAuthentication(core.ProviderAuthenticationSSH),
		Name:             providerName,
		Family:           "exe-dev",
		Kind:             core.ProviderKindSSHLease,
		Targets:          []core.TargetSpec{{OS: core.TargetLinux}},
		Features:         core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync},
		Coordinator:      core.CoordinatorNever,
		ClassDisposition: core.ProviderClassDispositionUnmapped,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return RegisterExeDevProviderFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return ApplyExeDevProviderFlags(cfg, fs, values)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if cfg.TargetOS != "" && cfg.TargetOS != core.TargetLinux {
		return nil, core.Exit(2, "provider=%s managed provisioning supports target=linux only", providerName)
	}
	if cfg.Tailscale.Enabled || string(cfg.Network) == "tailscale" {
		return nil, core.Exit(2, "--tailscale is not supported for provider=%s; exe.dev VMs expose public SSH only", providerName)
	}
	return NewExeDevLeaseBackend(p.Spec(), cfg, rt), nil
}

func (Provider) ConfigDefaultsTargetFinalization() core.ProviderConfigDefaultsTargetFinalization {
	return core.ProviderConfigDefaultsCallerFinalizes
}

func (Provider) ApplyConfigDefaults(cfg *core.Config) error {
	if cfg.ExeDev.User != "" {
		cfg.SSHUser = cfg.ExeDev.User
	} else if cfg.SSHUser == core.BaseConfig().SSHUser {
		if user := os.Getenv("USER"); user != "" {
			cfg.SSHUser = user
		}
	}
	if cfg.SSHPort == "" || cfg.SSHPort == core.BaseConfig().SSHPort {
		cfg.SSHPort = "22"
	}
	cfg.SSHFallbackPorts = nil
	cfg.ExeDev.WorkRoot = core.ResolveInheritedWorkRoot(cfg.ExeDev.WorkRoot, cfg.WorkRoot, core.ExeDevWorkRootFallback)
	if cfg.ExeDev.WorkRoot != "" {
		cfg.WorkRoot = cfg.ExeDev.WorkRoot
	}
	if cfg.TargetOS == "" {
		cfg.TargetOS = core.TargetLinux
	}
	return nil
}
