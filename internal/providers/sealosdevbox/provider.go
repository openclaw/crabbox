package sealosdevbox

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName = "sealos-devbox"
	familyName   = "sealos"

	// AutomationSurfaceDecision records the PLAN-01 evidence gate. Current
	// Sealos source exposes devbox.sealos.io/v1alpha2 Devbox CRDs and SSHGate
	// routing; public docs emphasize dashboard/plugin workflows. Later mutating
	// plans must continue from this CRD-first gate instead of guessing another
	// lifecycle surface.
	AutomationSurfaceDecision = "crd_first"
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
		Family:      familyName,
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

func (Provider) ValidateConfig(cfg core.Config) error {
	return validateBaseConfig(cfg)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if cfg.TargetOS != "" && cfg.TargetOS != core.TargetLinux {
		return nil, core.Exit(2, "provider=%s supports target=linux only", providerName)
	}
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	cfg = prepareBackendConfig(cfg)
	return &backend{spec: p.Spec(), cfg: cfg, rt: rt}, nil
}

func prepareBackendConfig(cfg core.Config) core.Config {
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.SSHUser = cfg.SealosDevbox.SSHUser
	cfg.SSHPort = cfg.SealosDevbox.SSHGatewayPort
	cfg.SSHFallbackPorts = nil
	cfg.WorkRoot = sealosWorkRoot(cfg)
	if strings.EqualFold(cfg.SealosDevbox.Network, networkNodePort) {
		cfg.SSHPort = "22"
	}
	return cfg
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	if cfg.TargetOS != "" && cfg.TargetOS != core.TargetLinux {
		return nil, core.Exit(2, "provider=%s supports target=linux only", providerName)
	}
	if err := validateBaseConfig(cfg); err != nil {
		return nil, err
	}
	cfg = prepareBackendConfig(cfg)
	return &backend{spec: p.Spec(), cfg: cfg, rt: rt}, nil
}
