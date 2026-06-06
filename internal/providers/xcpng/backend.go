package xcpng

import (
	"context"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type leaseBackend struct{ shared.DirectSSHBackend }

func NewLeaseBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) core.Backend {
	cfg.Provider = "xcp-ng"
	if cfg.XCPNg.User != "" {
		cfg.SSHUser = cfg.XCPNg.User
	}
	if cfg.XCPNg.WorkRoot != "" {
		cfg.WorkRoot = cfg.XCPNg.WorkRoot
	}
	if cfg.ServerType == "" {
		cfg.ServerType = xcpNgServerTypeForConfig(cfg)
	}
	return &leaseBackend{DirectSSHBackend: shared.DirectSSHBackend{SpecValue: spec, Cfg: cfg, RT: rt}}
}

func (b *leaseBackend) Acquire(context.Context, core.AcquireRequest) (core.LeaseTarget, error) {
	return core.LeaseTarget{}, notImplemented()
}

func (b *leaseBackend) Resolve(context.Context, core.ResolveRequest) (core.LeaseTarget, error) {
	return core.LeaseTarget{}, notImplemented()
}

func (b *leaseBackend) List(context.Context, core.ListRequest) ([]core.LeaseView, error) {
	return nil, notImplemented()
}

func (b *leaseBackend) ReleaseLease(context.Context, core.ReleaseLeaseRequest) error {
	return notImplemented()
}

func (b *leaseBackend) Touch(context.Context, core.TouchRequest) (core.Server, error) {
	return core.Server{}, notImplemented()
}

func (b *leaseBackend) Cleanup(context.Context, core.CleanupRequest) error {
	return notImplemented()
}

func (b *leaseBackend) Doctor(context.Context, core.DoctorRequest) (core.DoctorResult, error) {
	cfg := b.Cfg.XCPNg
	status := "configured"
	if strings.TrimSpace(cfg.APIURL) == "" || strings.TrimSpace(cfg.Username) == "" || strings.TrimSpace(cfg.Password) == "" {
		status = "configuration-incomplete"
	}
	return core.DoctorResult{
		Provider: "xcp-ng",
		Message:  "auth=" + status + " control_plane=unchecked inventory=unchecked mutation=false runtime=unchecked",
	}, nil
}

func xcpNgServerTypeForConfig(cfg core.Config) string {
	if value := strings.TrimSpace(cfg.XCPNg.TemplateUUID); value != "" {
		return "template-" + value
	}
	if value := strings.TrimSpace(cfg.XCPNg.Template); value != "" {
		return "template-" + core.NormalizeLeaseSlug(value)
	}
	return "template"
}

func notImplemented() error {
	return core.Exit(2, "xcp-ng lifecycle is not implemented yet")
}
