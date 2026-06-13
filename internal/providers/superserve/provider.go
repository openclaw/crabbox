package superserve

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
		Family:      "superserve",
		Kind:        core.ProviderKindDelegatedRun,
		Targets:     []core.TargetSpec{{OS: core.TargetLinux}},
		Features:    core.FeatureSet{core.FeatureArchiveSync, core.FeatureCleanup},
		Coordinator: core.CoordinatorNever,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return RegisterSuperserveProviderFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return ApplySuperserveProviderFlags(cfg, fs, values)
}

func (Provider) ValidateConfig(cfg core.Config) error {
	return validateSuperserveConfig(cfg)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if err := p.ValidateConfig(cfg); err != nil {
		return nil, err
	}
	cfg.Provider = providerName
	return &backend{spec: p.Spec(), cfg: cfg, rt: rt}, nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		return nil, core.Exit(2, "superserve doctor backend unavailable")
	}
	return doctor, nil
}

type backend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) Doctor(ctx context.Context, req DoctorRequest) (DoctorResult, error) {
	if err := ctx.Err(); err != nil {
		return DoctorResult{}, err
	}
	result := inventoryDoctorResult(providerName, 0)
	result.Status = "ok"
	result.Message = "config=ready lifecycle=not_implemented control_plane=not_contacted"
	return result, nil
}

func (b *backend) Warmup(ctx context.Context, req WarmupRequest) error {
	return notImplemented(ctx, "warmup")
}

func (b *backend) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	return RunResult{}, notImplemented(ctx, "run")
}

func (b *backend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	return nil, notImplemented(ctx, "list")
}

func (b *backend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	return StatusView{}, notImplemented(ctx, "status")
}

func (b *backend) Stop(ctx context.Context, req StopRequest) error {
	return notImplemented(ctx, "stop")
}

func (b *backend) Cleanup(ctx context.Context, req CleanupRequest) error {
	return notImplemented(ctx, "cleanup")
}
