package nebius

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

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:        providerName,
		Family:      providerName,
		Kind:        core.ProviderKindSSHLease,
		Targets:     []core.TargetSpec{{OS: core.TargetLinux}},
		Features:    core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup},
		Coordinator: core.CoordinatorNever,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return RegisterNebiusProviderFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return ApplyNebiusProviderFlags(cfg, fs, values)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if cfg.TargetOS != "" && cfg.TargetOS != core.TargetLinux {
		return nil, exit(2, "provider=%s supports target=linux only", providerName)
	}
	if cfg.Tailscale.Enabled || string(cfg.Network) == "tailscale" {
		return nil, exit(2, "--tailscale is not supported for provider=%s in the Nebius provider foundation", providerName)
	}
	if err := p.ValidateConfig(cfg); err != nil {
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
		return nil, exit(2, "nebius doctor backend unavailable")
	}
	return doctor, nil
}

func (Provider) ValidateConfig(cfg core.Config) error {
	neb := cfg.Nebius
	if strings.TrimSpace(neb.CLI) == "" {
		return exit(2, "nebius.cli is required")
	}
	if err := validateNebiusUser(neb.User); err != nil {
		return err
	}
	if neb.DiskSizeGiB <= 0 {
		return exit(2, "nebius.diskSizeGiB must be positive")
	}
	switch strings.ToLower(strings.TrimSpace(neb.PublicIP)) {
	case "", "dynamic", "none":
	default:
		return exit(2, "nebius.publicIP must be dynamic or none")
	}
	switch strings.ToLower(strings.TrimSpace(neb.RecoveryPolicy)) {
	case "", "fail":
	default:
		return exit(2, "nebius.recoveryPolicy must be fail")
	}
	return nil
}
