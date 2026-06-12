package blaxel

import (
	"context"
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string      { return providerName }
func (Provider) Aliases() []string { return nil }

func (Provider) ServerTypeForConfig(core.Config) string { return "" }
func (Provider) ServerTypeForClass(string) string       { return "" }

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:        providerName,
		Family:      providerName,
		Kind:        core.ProviderKindDelegatedRun,
		Targets:     []core.TargetSpec{{OS: core.TargetLinux}},
		Features:    core.FeatureSet{core.FeatureArchiveSync, core.FeatureCleanup},
		Coordinator: core.CoordinatorNever,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return RegisterBlaxelProviderFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return ApplyBlaxelProviderFlags(cfg, fs, values)
}

func (Provider) ValidateConfig(cfg core.Config) error {
	return validateBlaxelConfig(cfg)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if err := p.ValidateConfig(cfg); err != nil {
		return nil, err
	}
	cfg.Provider = providerName
	return &backend{spec: p.Spec(), cfg: cfg, rt: rt, clientFactory: newBlaxelClient}, nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		return nil, core.Exit(2, "blaxel doctor backend unavailable")
	}
	return doctor, nil
}

type backend struct {
	spec          ProviderSpec
	cfg           Config
	rt            Runtime
	clientFactory func(Config, Runtime) (Client, error)
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) Warmup(context.Context, WarmupRequest) error {
	return exit(2, "provider=blaxel lifecycle is not implemented yet")
}

func (b *backend) Run(context.Context, RunRequest) (RunResult, error) {
	return RunResult{}, exit(2, "provider=blaxel run is not implemented yet")
}

func (b *backend) List(context.Context, ListRequest) ([]LeaseView, error) {
	return nil, exit(2, "provider=blaxel list is not implemented yet")
}

func (b *backend) Status(context.Context, StatusRequest) (StatusView, error) {
	return StatusView{}, exit(2, "provider=blaxel status is not implemented yet")
}

func (b *backend) Stop(context.Context, StopRequest) error {
	return exit(2, "provider=blaxel stop is not implemented yet")
}
