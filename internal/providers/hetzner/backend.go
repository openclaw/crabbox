package hetzner

import (
	"context"
	"fmt"
	"io"
	"os"
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

type hetznerLeaseBackend struct{ shared.DirectSSHBackend }

func NewHetznerLeaseBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = "hetzner"
	return &hetznerLeaseBackend{DirectSSHBackend: shared.DirectSSHBackend{SpecValue: spec, Cfg: cfg, RT: rt}}
}

func (b *hetznerLeaseBackend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	return acquireAttemptsRetry(b.RT, req.Keep, func() (LeaseTarget, error) {
		return b.acquireOnce(ctx, req.Keep, req.RequestedSlug)
	})
}

func (b *hetznerLeaseBackend) acquireOnce(ctx context.Context, keep bool, requestedSlug string) (LeaseTarget, error) {
	if b.Cfg.Tailscale.Enabled && b.Cfg.Tailscale.AuthKey == "" {
		return LeaseTarget{}, exit(2, "direct --tailscale requires %s to contain a Tailscale auth key; brokered mode uses coordinator OAuth secrets", b.Cfg.Tailscale.AuthKeyEnv)
	}
	client, err := newHetznerClient()
	if err != nil {
		return LeaseTarget{}, err
	}
	leaseID := newLeaseID()
	servers, err := client.ListCrabboxServers(ctx)
	if err != nil {
		return LeaseTarget{}, err
	}
	slug, err := allocateDirectLeaseSlug(leaseID, requestedSlug, servers)
	if err != nil {
		return LeaseTarget{}, err
	}
	cfg := b.Cfg
	keyPath, publicKey, err := ensureTestboxKeyForConfig(cfg, leaseID)
	if err != nil {
		return LeaseTarget{}, err
	}
	cfg.SSHKey = keyPath
	cfg.ProviderKey = providerKeyForLease(leaseID)
	if cfg.ProviderKey != "" {
		providerKey, err := client.EnsureSSHKey(ctx, cfg.ProviderKey, publicKey)
		if err != nil {
			return LeaseTarget{}, err
		}
		cfg.ProviderKey = providerKey.Name
	}
	fmt.Fprintf(b.RT.Stderr, "provisioning provider=hetzner lease=%s slug=%s class=%s preferred_type=%s location=%s keep=%v\n", leaseID, slug, cfg.Class, cfg.ServerType, cfg.Location, keep)
	server, cfg, err := client.CreateServerWithFallback(ctx, cfg, publicKey, leaseID, slug, keep, func(format string, args ...any) {
		fmt.Fprintf(b.RT.Stderr, format, args...)
	})
	if err != nil {
		return LeaseTarget{}, err
	}
	fmt.Fprintf(b.RT.Stderr, "provisioned lease=%s server=%d type=%s\n", leaseID, server.ID, cfg.ServerType)
	server, err = waitForServerIP(ctx, client, server.ID)
	if err != nil {
		return LeaseTarget{}, err
	}
	target := sshTargetFromConfig(cfg, server.PublicNet.IPv4.IP)
	if err := waitForSSHReady(ctx, &target, b.RT.Stderr, "bootstrap", bootstrapWaitTimeout(cfg)); err != nil {
		_ = deleteServer(context.Background(), cfg, server)
		return LeaseTarget{}, err
	}
	server.Labels["state"] = "ready"
	if err := client.SetLabels(ctx, server.ID, server.Labels); err != nil {
		fmt.Fprintf(b.RT.Stderr, "warning: set labels: %v\n", err)
	}
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *hetznerLeaseBackend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	client, err := newHetznerClient()
	if err != nil {
		return LeaseTarget{}, err
	}
	if serverID, ok := parseServerID(req.ID); ok {
		server, err := client.GetServer(ctx, serverID)
		if err != nil {
			return LeaseTarget{}, err
		}
		leaseID := blank(server.Labels["lease"], req.ID)
		target := sshTargetFromConfig(b.Cfg, server.PublicNet.IPv4.IP)
		useStoredTestboxKey(&target, leaseID)
		return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
	}
	servers, err := client.ListCrabboxServers(ctx)
	if err != nil {
		return LeaseTarget{}, err
	}
	if server, leaseID, err := findServerByAlias(servers, req.ID); err != nil {
		return LeaseTarget{}, err
	} else if leaseID != "" {
		target := sshTargetFromConfig(b.Cfg, server.PublicNet.IPv4.IP)
		useStoredTestboxKey(&target, leaseID)
		return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
	}
	return LeaseTarget{}, exit(4, "lease/server not found: %s", req.ID)
}

func (b *hetznerLeaseBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	_ = req
	client, err := newHetznerClient()
	if err != nil {
		return nil, err
	}
	return client.ListCrabboxServers(ctx)
}

func (b *hetznerLeaseBackend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	servers, err := b.List(ctx, ListRequest{})
	if err != nil {
		return core.DoctorResult{}, err
	}
	result := core.InventoryDoctorResult("hetzner", len(servers))
	result.Message += fmt.Sprintf(" default_type=%s", b.Cfg.ServerType)
	return result, nil
}

func (b *hetznerLeaseBackend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	if err := deleteServer(ctx, b.Cfg, req.Lease.Server); err != nil {
		return err
	}
	removeLeaseClaim(req.Lease.LeaseID)
	return nil
}

func (b *hetznerLeaseBackend) Touch(ctx context.Context, req TouchRequest) (Server, error) {
	return b.DirectSSHBackend.Touch(ctx, req.Lease.Server, req.State), nil
}

func (b *hetznerLeaseBackend) Cleanup(ctx context.Context, req CleanupRequest) error {
	servers, err := b.List(ctx, ListRequest{Options: req.Options})
	if err != nil {
		return err
	}
	return b.CleanupServers(ctx, req, servers)
}

func acquireAttemptsRetry(rt Runtime, keep bool, acquire func() (LeaseTarget, error)) (LeaseTarget, error) {
	return shared.AcquireAttemptsRetry(rt, keep, acquire)
}
func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}
func newHetznerClient() (*core.HetznerClient, error) { return core.NewHetznerClient() }
func newLeaseID() string                             { return core.NewLeaseID() }
func allocateDirectLeaseSlug(id, requested string, servers []Server) (string, error) {
	return core.AllocateDirectLeaseSlug(id, requested, servers)
}
func ensureTestboxKeyForConfig(cfg Config, leaseID string) (string, string, error) {
	return core.EnsureTestboxKeyForConfig(cfg, leaseID)
}
func providerKeyForLease(leaseID string) string { return core.ProviderKeyForLease(leaseID) }
func waitForServerIP(ctx context.Context, client *core.HetznerClient, id int64) (Server, error) {
	return core.WaitForServerIP(ctx, client, id)
}
func sshTargetFromConfig(cfg Config, host string) SSHTarget {
	return core.SSHTargetFromConfig(cfg, host)
}
func waitForSSHReady(ctx context.Context, target *SSHTarget, stderr io.Writer, phase string, timeout time.Duration) error {
	return core.WaitForSSHReady(ctx, target, stderr, phase, timeout)
}
func bootstrapWaitTimeout(cfg Config) time.Duration { return core.BootstrapWaitTimeout(cfg) }
func deleteServer(ctx context.Context, cfg Config, server Server) error {
	return core.DeleteServer(ctx, cfg, server)
}
func parseServerID(s string) (int64, bool) { return core.ParseServerID(s) }
func blank(value, fallback string) string  { return core.Blank(value, fallback) }
func useStoredTestboxKey(target *SSHTarget, leaseID string) {
	if keyPath, err := core.TestboxKeyPath(leaseID); err == nil {
		if _, statErr := os.Stat(keyPath); statErr == nil {
			target.Key = keyPath
		}
	}
}
func findServerByAlias(servers []Server, id string) (Server, string, error) {
	return core.FindServerByAlias(servers, id)
}
func removeLeaseClaim(leaseID string) { core.RemoveLeaseClaim(leaseID) }
