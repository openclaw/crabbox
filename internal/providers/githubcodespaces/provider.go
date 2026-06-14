package githubcodespaces

import (
	"context"
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
	return &BackendSkeleton{spec: p.Spec(), cfg: cfg, rt: rt}, nil
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

type BackendSkeleton struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func (b *BackendSkeleton) Spec() ProviderSpec { return b.spec }

func (b *BackendSkeleton) Doctor(context.Context, DoctorRequest) (DoctorResult, error) {
	return DoctorResult{
		Provider: providerName,
		Message:  "auth=gh control_plane=unimplemented inventory=unimplemented mutation=false",
		Status:   "failed",
	}, exit(2, "provider=github-codespaces doctor is not implemented yet")
}

func (b *BackendSkeleton) Acquire(context.Context, AcquireRequest) (LeaseTarget, error) {
	return LeaseTarget{}, exit(2, "provider=github-codespaces acquire is not implemented yet")
}

func (b *BackendSkeleton) Resolve(context.Context, ResolveRequest) (LeaseTarget, error) {
	return LeaseTarget{}, exit(2, "provider=github-codespaces resolve is not implemented yet")
}

func (b *BackendSkeleton) List(context.Context, ListRequest) ([]LeaseView, error) {
	return nil, exit(2, "provider=github-codespaces list is not implemented yet")
}

func (b *BackendSkeleton) Touch(context.Context, TouchRequest) (Server, error) {
	return Server{}, exit(2, "provider=github-codespaces touch is not implemented yet")
}

func (b *BackendSkeleton) ReleaseLease(context.Context, ReleaseLeaseRequest) error {
	return exit(2, "provider=github-codespaces release is not implemented yet")
}
