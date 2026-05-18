package ssh

import (
	"context"
	"fmt"
	"io"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type Config = core.Config
type Runtime = core.Runtime
type ProviderSpec = core.ProviderSpec
type Backend = core.Backend
type AcquireRequest = core.AcquireRequest
type ResolveRequest = core.ResolveRequest
type ListRequest = core.ListRequest
type LeaseView = core.LeaseView
type ReleaseLeaseRequest = core.ReleaseLeaseRequest
type TouchRequest = core.TouchRequest
type CleanupRequest = core.CleanupRequest
type LeaseTarget = core.LeaseTarget
type Server = core.Server
type SSHTarget = core.SSHTarget

type staticLeaseBackend struct{ shared.DirectSSHBackend }

func NewStaticSSHLeaseBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = "ssh"
	return &staticLeaseBackend{DirectSSHBackend: shared.DirectSSHBackend{SpecValue: spec, Cfg: cfg, RT: rt}}
}

func (b *staticLeaseBackend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	cfg := b.Cfg
	if req.RequestedSlug != "" {
		cfg.Static.Name = req.RequestedSlug
	}
	server, target, leaseID, err := staticLease(cfg)
	if err != nil {
		return LeaseTarget{}, err
	}
	fmt.Fprintf(b.RT.Stderr, "using static target lease=%s slug=%s target=%s windows_mode=%s host=%s keep=%v\n", leaseID, serverSlug(server), b.Cfg.TargetOS, b.Cfg.WindowsMode, target.Host, req.Keep)
	if err := waitForSSH(ctx, &target, b.RT.Stderr); err != nil {
		return LeaseTarget{}, err
	}
	server.Labels["state"] = "ready"
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *staticLeaseBackend) Resolve(_ context.Context, req ResolveRequest) (LeaseTarget, error) {
	server, target, leaseID, err := staticLease(b.Cfg)
	if err != nil {
		return LeaseTarget{}, err
	}
	if req.ID == "" || req.ID == leaseID || req.ID == server.Name || req.ID == serverSlug(server) || req.ID == b.Cfg.Static.Host {
		return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
	}
	return LeaseTarget{}, exit(4, "static lease not found: %s", req.ID)
}

func (b *staticLeaseBackend) List(_ context.Context, req ListRequest) ([]LeaseView, error) {
	_ = req
	server, _, _, err := staticLease(b.Cfg)
	if err != nil {
		return nil, err
	}
	return []LeaseView{server}, nil
}

func (b *staticLeaseBackend) Doctor(ctx context.Context, req core.DoctorRequest) (core.DoctorResult, error) {
	if b.Cfg.Static.Host == "" {
		return core.DoctorResult{}, exit(3, "missing static.host")
	}
	runtime := "unchecked"
	api := "static_config"
	if req.ProbeSSH {
		_, target, _, err := staticLease(b.Cfg)
		if err != nil {
			return core.DoctorResult{}, err
		}
		if err := waitForSSHReady(ctx, &target, b.RT.Stderr, "doctor", 10*time.Second); err != nil {
			return core.DoctorResult{}, err
		}
		api = "ssh_probe"
		runtime = "ssh_reachable"
	}
	return core.DoctorResult{
		Provider: "ssh",
		Message:  fmt.Sprintf("target=%s windows_mode=%s host=%s api=%s mutation=false runtime=%s", b.Cfg.TargetOS, b.Cfg.WindowsMode, b.Cfg.Static.Host, api, runtime),
	}, nil
}

func (b *staticLeaseBackend) ReleaseLease(_ context.Context, req ReleaseLeaseRequest) error {
	removeLeaseClaim(req.Lease.LeaseID)
	return nil
}

func (b *staticLeaseBackend) Touch(_ context.Context, req TouchRequest) (Server, error) {
	server := req.Lease.Server
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	server.Labels = touchDirectLeaseLabels(server.Labels, b.Cfg, req.State, time.Now().UTC())
	return server, nil
}

func (b *staticLeaseBackend) Cleanup(context.Context, CleanupRequest) error {
	return exit(2, "machine cleanup is not supported for provider=%s", b.Cfg.Provider)
}

func staticLease(cfg Config) (Server, SSHTarget, string, error) { return core.StaticLease(cfg) }
func serverSlug(server Server) string                           { return core.ServerSlug(server) }
func waitForSSH(ctx context.Context, target *SSHTarget, stderr io.Writer) error {
	return core.WaitForSSH(ctx, target, stderr)
}
func waitForSSHReady(ctx context.Context, target *SSHTarget, stderr io.Writer, phase string, timeout time.Duration) error {
	return core.WaitForSSHReady(ctx, target, stderr, phase, timeout)
}
func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}
func removeLeaseClaim(leaseID string) { core.RemoveLeaseClaim(leaseID) }
func touchDirectLeaseLabels(labels map[string]string, cfg Config, state string, now time.Time) map[string]string {
	return core.TouchDirectLeaseLabels(labels, cfg, state, now)
}
