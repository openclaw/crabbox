package morph

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

func (Provider) Spec() ProviderSpec {
	return ProviderSpec{
		Name: providerName,
		Kind: core.ProviderKindSSHLease,
		Targets: []core.TargetSpec{{
			OS: targetLinux,
		}},
		Features:    core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync},
		Coordinator: core.CoordinatorNever,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults Config) any {
	return RegisterMorphProviderFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	return ApplyMorphProviderFlags(cfg, fs, values)
}

func (p Provider) Configure(cfg Config, rt Runtime) (Backend, error) {
	return NewMorphBackend(p.Spec(), cfg, rt)
}

func (p Provider) ConfigureDoctor(cfg Config, rt Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		return nil, exit(2, "provider=%s does not implement doctor", providerName)
	}
	return doctor, nil
}

func (Provider) ServerTypeForConfig(cfg Config) string {
	if snapshot := strings.TrimSpace(cfg.Morph.Snapshot); snapshot != "" {
		return snapshot
	}
	return "snapshot"
}

func (Provider) ServerTypeForClass(string) string {
	return "snapshot"
}
