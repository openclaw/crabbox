package proxmox

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
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

type leaseBackend struct{ shared.DirectSSHBackend }

func NewLeaseBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = "proxmox"
	if cfg.Proxmox.User != "" {
		cfg.SSHUser = cfg.Proxmox.User
	}
	if cfg.Proxmox.WorkRoot != "" {
		cfg.WorkRoot = cfg.Proxmox.WorkRoot
	}
	return &leaseBackend{DirectSSHBackend: shared.DirectSSHBackend{SpecValue: spec, Cfg: cfg, RT: rt}}
}

func (b *leaseBackend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	return shared.AcquireAttemptsRetry(b.RT, req.Keep, func() (LeaseTarget, error) {
		return b.acquireOnce(ctx, req.Keep)
	})
}

func (b *leaseBackend) acquireOnce(ctx context.Context, keep bool) (LeaseTarget, error) {
	client, err := newClient(b.Cfg)
	if err != nil {
		return LeaseTarget{}, err
	}
	leaseID := newLeaseID()
	servers, err := client.ListCrabboxServers(ctx)
	if err != nil {
		return LeaseTarget{}, err
	}
	slug := allocateDirectLeaseSlug(leaseID, servers)
	cfg := b.Cfg
	keyPath, publicKey, err := ensureTestboxKeyForConfig(cfg, leaseID)
	if err != nil {
		return LeaseTarget{}, err
	}
	cfg.SSHKey = keyPath
	cfg.ProviderKey = providerKeyForLease(leaseID)
	cfg.ServerType = proxmoxServerTypeForConfig(cfg)
	fmt.Fprintf(b.RT.Stderr, "provisioning provider=proxmox lease=%s slug=%s node=%s template=%d keep=%v\n",
		leaseID, slug, cfg.Proxmox.Node, cfg.Proxmox.TemplateID, keep)
	server, err := client.CreateServer(ctx, cfg, publicKey, leaseID, slug, keep)
	if err != nil {
		return LeaseTarget{}, err
	}
	if server.PublicNet.IPv4.IP == "" {
		cloudID := server.CloudID
		server, err = client.GetServer(ctx, server.CloudID)
		if err != nil {
			_ = client.DeleteServer(context.Background(), cloudID)
			return LeaseTarget{}, err
		}
	}
	target := sshTargetFromConfig(cfg, server.PublicNet.IPv4.IP)
	if err := waitForSSHReady(ctx, &target, b.RT.Stderr, "bootstrap", bootstrapWaitTimeout(cfg)); err != nil {
		_ = client.DeleteServer(context.Background(), server.CloudID)
		return LeaseTarget{}, err
	}
	server.Labels["state"] = "ready"
	if err := client.SetLabels(ctx, server.CloudID, server.Labels); err != nil {
		fmt.Fprintf(b.RT.Stderr, "warning: set proxmox labels: %v\n", err)
	}
	fmt.Fprintf(b.RT.Stderr, "provisioned lease=%s server=%s node=%s ip=%s\n", leaseID, server.DisplayID(), cfg.Proxmox.Node, server.PublicNet.IPv4.IP)
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *leaseBackend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	client, err := newClient(b.Cfg)
	if err != nil {
		return LeaseTarget{}, err
	}
	if req.ID != "" {
		if _, err := strconv.Atoi(req.ID); err == nil || strings.HasPrefix(req.ID, "crabbox-") {
			server, err := client.GetServer(ctx, req.ID)
			if err != nil {
				if !core.IsProxmoxNotFound(err) {
					return LeaseTarget{}, err
				}
			} else {
				if !isCrabboxLease(server) {
					return LeaseTarget{}, exit(4, "lease/server not found: %s (VM exists but is not Crabbox-managed)", req.ID)
				}
				return b.targetForServer(server), nil
			}
		}
	}
	servers, err := client.ListCrabboxServers(ctx)
	if err != nil {
		return LeaseTarget{}, err
	}
	if server, leaseID, err := findServerByAlias(servers, req.ID); err != nil {
		return LeaseTarget{}, err
	} else if leaseID != "" {
		target := b.targetForServer(server)
		target.LeaseID = leaseID
		return target, nil
	}
	return LeaseTarget{}, exit(4, "lease/server not found: %s", req.ID)
}

func (b *leaseBackend) targetForServer(server Server) LeaseTarget {
	cfg := b.Cfg
	target := sshTargetFromConfig(cfg, server.PublicNet.IPv4.IP)
	leaseID := core.Blank(server.Labels["lease"], server.CloudID)
	useStoredTestboxKey(&target, leaseID)
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}
}

func (b *leaseBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	_ = req
	client, err := newClient(b.Cfg)
	if err != nil {
		return nil, err
	}
	return client.ListCrabboxServers(ctx)
}

func (b *leaseBackend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	client, err := newClient(b.Cfg)
	if err != nil {
		return err
	}
	id := req.Lease.Server.CloudID
	if id == "" {
		id = req.Lease.LeaseID
	}
	if err := client.DeleteServer(ctx, id); err != nil {
		return err
	}
	removeLeaseClaim(req.Lease.LeaseID)
	return nil
}

func (b *leaseBackend) Touch(ctx context.Context, req TouchRequest) (Server, error) {
	client, err := newClient(b.Cfg)
	if err != nil {
		return Server{}, err
	}
	server := req.Lease.Server
	server.Labels = core.TouchDirectLeaseLabels(server.Labels, b.Cfg, req.State, time.Now().UTC())
	if err := client.SetLabels(ctx, server.CloudID, server.Labels); err != nil {
		return Server{}, err
	}
	return server, nil
}

func (b *leaseBackend) Cleanup(ctx context.Context, req CleanupRequest) error {
	servers, err := b.List(ctx, ListRequest{Options: req.Options})
	if err != nil {
		return err
	}
	client, err := newClient(b.Cfg)
	if err != nil {
		return err
	}
	for _, server := range servers {
		shouldDelete, reason := core.ShouldCleanupServer(server, time.Now().UTC())
		if !shouldDelete {
			fmt.Fprintf(b.RT.Stderr, "skip server id=%s name=%s reason=%s\n", server.DisplayID(), server.Name, reason)
			continue
		}
		fmt.Fprintf(b.RT.Stderr, "delete server id=%s name=%s\n", server.DisplayID(), server.Name)
		if req.DryRun {
			continue
		}
		if err := client.DeleteServer(ctx, server.CloudID); err != nil {
			return err
		}
	}
	return nil
}

func newClient(cfg Config) (*core.ProxmoxClient, error) { return core.NewProxmoxClient(cfg) }
func newLeaseID() string                                { return core.NewLeaseID() }
func allocateDirectLeaseSlug(id string, servers []Server) string {
	return core.AllocateDirectLeaseSlug(id, servers)
}
func ensureTestboxKeyForConfig(cfg Config, leaseID string) (string, string, error) {
	return core.EnsureTestboxKeyForConfig(cfg, leaseID)
}
func providerKeyForLease(leaseID string) string { return core.ProviderKeyForLease(leaseID) }
func proxmoxServerTypeForConfig(cfg Config) string {
	return core.ProxmoxServerTypeForConfig(cfg)
}
func sshTargetFromConfig(cfg Config, host string) SSHTarget {
	return core.SSHTargetFromConfig(cfg, host)
}
func waitForSSHReady(ctx context.Context, target *SSHTarget, stderr io.Writer, phase string, timeout time.Duration) error {
	return core.WaitForSSHReady(ctx, target, stderr, phase, timeout)
}
func bootstrapWaitTimeout(cfg Config) time.Duration { return core.BootstrapWaitTimeout(cfg) }
func findServerByAlias(servers []Server, id string) (Server, string, error) {
	return core.FindServerByAlias(servers, id)
}
func isCrabboxLease(server Server) bool { return core.IsCrabboxProxmoxLease(server) }
func removeLeaseClaim(leaseID string)   { core.RemoveLeaseClaim(leaseID) }
func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}

func useStoredTestboxKey(target *SSHTarget, leaseID string) {
	if keyPath, err := core.TestboxKeyPath(leaseID); err == nil {
		if _, statErr := os.Stat(keyPath); statErr == nil {
			target.Key = keyPath
		}
	}
}
