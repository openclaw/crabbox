package fal

import (
	"context"
	"flag"
	"io"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string { return providerName }

func (Provider) Aliases() []string { return []string{"fal-ai"} }

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
	return RegisterFalProviderFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return ApplyFalProviderFlags(cfg, fs, values)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if cfg.TargetOS != "" && cfg.TargetOS != core.TargetLinux {
		return nil, exit(2, "provider=%s managed provisioning supports target=linux only", providerName)
	}
	if cfg.Tailscale.Enabled || string(cfg.Network) == "tailscale" {
		return nil, exit(2, "--tailscale is not supported for provider=%s; fal Compute exposes public SSH only", providerName)
	}
	applyFalDefaults(&cfg)
	return &backend{spec: p.Spec(), cfg: cfg, rt: rt, clientFactory: newClient}, nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		return nil, core.Exit(2, "fal doctor backend unavailable")
	}
	return doctor, nil
}

type backend struct {
	spec          ProviderSpec
	cfg           Config
	rt            Runtime
	clientFactory func(Config, Runtime) (computeAPI, error)
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) Acquire(context.Context, core.AcquireRequest) (core.LeaseTarget, error) {
	return core.LeaseTarget{}, lifecycleDeferredError("acquire")
}

func (b *backend) Resolve(context.Context, core.ResolveRequest) (core.LeaseTarget, error) {
	return core.LeaseTarget{}, lifecycleDeferredError("resolve")
}

func (b *backend) List(context.Context, core.ListRequest) ([]core.LeaseView, error) {
	return nil, lifecycleDeferredError("list")
}

func (b *backend) Touch(context.Context, core.TouchRequest) (core.Server, error) {
	return core.Server{}, lifecycleDeferredError("touch")
}

func (b *backend) ReleaseLease(context.Context, core.ReleaseLeaseRequest) error {
	return lifecycleDeferredError("release")
}

func (b *backend) Cleanup(context.Context, core.CleanupRequest) error {
	return lifecycleDeferredError("cleanup")
}

func (b *backend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	if strings.TrimSpace(b.cfg.Fal.APIKey) == "" {
		return DoctorResult{}, exit(2, "provider=%s requires fal credentials in environment", providerName)
	}
	client, err := b.clientFactory(b.cfg, b.rt)
	if err != nil {
		return DoctorResult{}, err
	}
	if _, err := client.ListInstances(ctx, 1, ""); err != nil {
		return DoctorResult{}, exit(1, "fal auth check failed: %v", err)
	}
	return DoctorResult{
		Provider: providerName,
		Message:  "auth=ready control_plane=ready inventory=ready api=list mutation=false runtime=unchecked",
	}, nil
}

func lifecycleDeferredError(operation string) error {
	return exit(2, "provider=%s %s lifecycle is deferred until PLAN-02 implements fal Compute leases", providerName, operation)
}

func newDiscardRuntime() Runtime {
	return Runtime{Stdout: io.Discard, Stderr: io.Discard}
}
