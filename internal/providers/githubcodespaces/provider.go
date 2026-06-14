package githubcodespaces

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	coreRegisterProvider(Provider{})
}

var coreRegisterProvider = func(provider Provider) {
	core.RegisterProvider(provider)
}

type Provider struct{}

func (Provider) Name() string { return providerName }

func (Provider) Aliases() []string {
	return []string{"codespaces", "gh-codespaces"}
}

func (Provider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        providerName,
		Family:      providerFamily,
		Kind:        core.ProviderKindSSHLease,
		Targets:     []core.TargetSpec{{OS: targetLinux}},
		Features:    core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup},
		Coordinator: core.CoordinatorNever,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults Config) any {
	return RegisterGitHubCodespacesProviderFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	return ApplyGitHubCodespacesProviderFlags(cfg, fs, values)
}

func (Provider) ServerTypeForConfig(cfg Config) string {
	if cfg.ServerTypeExplicit && cfg.ServerType != "" {
		return cfg.ServerType
	}
	if cfg.GitHubCodespaces.Machine != "" {
		return cfg.GitHubCodespaces.Machine
	}
	return defaultCodespaceMachine
}

func (Provider) ServerTypeForClass(string) string {
	return defaultCodespaceMachine
}

func (p Provider) Configure(cfg Config, rt Runtime) (Backend, error) {
	cfg.Provider = providerName
	if err := ValidateGitHubCodespacesConfig(cfg); err != nil {
		return nil, err
	}
	return newBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg Config, rt Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		return nil, exit(2, "github-codespaces doctor backend unavailable")
	}
	return doctor, nil
}
