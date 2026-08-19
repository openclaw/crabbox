package machine0

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() { core.RegisterProvider(Provider{}) }

type Provider struct{}

func (Provider) Name() string      { return providerName }
func (Provider) Aliases() []string { return nil }

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:    providerName,
		Family:  providerName,
		Kind:    core.ProviderKindSSHLease,
		Targets: []core.TargetSpec{{OS: core.TargetLinux}},
		Features: core.FeatureSet{
			core.FeatureSSH,
			core.FeatureCrabboxSync,
			core.FeatureCleanup,
			core.FeatureDesktop,
			core.FeatureBrowser,
			core.FeatureCode,
			core.FeaturePauseResume,
			core.FeatureCheckpoint,
			core.FeatureFork,
			core.FeatureSnapshot,
		},
		Coordinator: core.CoordinatorNever,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return registerFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return applyFlags(cfg, fs, values)
}

func (p Provider) ApplyConfigDefaults(cfg *core.Config) error {
	applyDefaults(cfg)
	return nil
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	applyDefaults(&cfg)
	if err := p.ValidateConfig(cfg); err != nil {
		return nil, err
	}
	if cfg.TargetOS != targetLinux {
		return nil, exit(2, "provider=%s supports target=linux only", providerName)
	}
	if cfg.Tailscale.Enabled || string(cfg.Network) == "tailscale" {
		return nil, exit(2, "--tailscale is not supported for provider=%s; use the Machine0 public IP or authenticated HTTPS URL", providerName)
	}
	return newBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	return backend.(core.DoctorBackend), nil
}

func (Provider) ValidateConfig(cfg core.Config) error {
	m := cfg.Machine0
	for name, value := range map[string]string{
		"machine0.cliPath": m.CLIPath,
		"machine0.image":   m.Image,
		"machine0.size":    m.Size,
		"machine0.region":  m.Region,
	} {
		if strings.TrimSpace(value) == "" {
			return exit(2, "%s is required", name)
		}
	}
	switch strings.ToLower(strings.TrimSpace(m.ReleasePolicy)) {
	case "", "destroy", "suspend":
	default:
		return exit(2, "machine0.releasePolicy must be destroy or suspend")
	}
	if m.CreateTimeout <= 0 {
		return exit(2, "machine0.createTimeout must be positive")
	}
	if m.ImageVersion < 0 {
		return exit(2, "machine0.imageVersion must not be negative")
	}
	if m.PollInterval <= 0 {
		return exit(2, "machine0.pollInterval must be positive")
	}
	return nil
}

func (Provider) ServerTypeForConfig(cfg core.Config) string { return cfg.Machine0.Size }
func (Provider) ServerTypeForClass(string) string           { return "large" }
