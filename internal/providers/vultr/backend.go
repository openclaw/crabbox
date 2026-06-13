package vultr

import (
	"context"

	core "github.com/openclaw/crabbox/internal/cli"
)

type backend struct {
	spec core.ProviderSpec
	cfg  core.Config
	rt   core.Runtime
}

func NewBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) core.Backend {
	cfg.Provider = providerName
	if cfg.Vultr.Region == "" {
		cfg.Vultr.Region = "ewr"
	}
	if cfg.Vultr.UserScheme == "" {
		cfg.Vultr.UserScheme = "root"
	}
	if cfg.SSHUser == "" {
		cfg.SSHUser = "root"
	}
	if cfg.SSHPort == "" {
		cfg.SSHPort = "22"
	}
	cfg.SSHFallbackPorts = nil
	if cfg.TargetOS == "" {
		cfg.TargetOS = core.TargetLinux
	}
	if cfg.WorkRoot == "" {
		cfg.WorkRoot = "/work/crabbox"
	}
	if cfg.ServerType == "" {
		cfg.ServerType = vultrServerTypeForClass(cfg.Class)
	}
	return &backend{spec: spec, cfg: cfg, rt: rt}
}

func (b *backend) Spec() core.ProviderSpec { return b.spec }

func (b *backend) Acquire(context.Context, core.AcquireRequest) (core.LeaseTarget, error) {
	return core.LeaseTarget{}, vultrLifecycleNotImplemented("acquire")
}

func (b *backend) Resolve(context.Context, core.ResolveRequest) (core.LeaseTarget, error) {
	return core.LeaseTarget{}, vultrLifecycleNotImplemented("resolve")
}

func (b *backend) List(context.Context, core.ListRequest) ([]core.LeaseView, error) {
	return nil, vultrLifecycleNotImplemented("list")
}

func (b *backend) Touch(context.Context, core.TouchRequest) (core.Server, error) {
	return core.Server{}, vultrLifecycleNotImplemented("touch")
}

func (b *backend) ReleaseLease(context.Context, core.ReleaseLeaseRequest) error {
	return vultrLifecycleNotImplemented("release")
}

func (b *backend) Cleanup(context.Context, core.CleanupRequest) error {
	return vultrLifecycleNotImplemented("cleanup")
}

func (b *backend) Doctor(context.Context, core.DoctorRequest) (core.DoctorResult, error) {
	return core.DoctorResult{
		Provider: providerName,
		Message:  "auth=not-checked lifecycle=not-implemented mutation=false",
		Status:   "missing",
	}, nil
}

func vultrLifecycleNotImplemented(action string) error {
	return core.Exit(2, "provider=vultr %s lifecycle is not implemented yet; PLAN-02 owns Vultr API lifecycle", action)
}
