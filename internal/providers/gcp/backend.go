package gcp

import (
	"context"
	"fmt"
	"io"
	"os"
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

type gcpLeaseBackend struct{ shared.DirectSSHBackend }

type gcpClient interface {
	ListCrabboxServers(context.Context) ([]Server, error)
	ListCrabboxServersComplete(context.Context) ([]Server, error)
	CreateServerWithFallback(context.Context, Config, string, string, string, bool, func(string, ...any)) (Server, Config, error)
	WaitForServerIP(context.Context, string) (Server, error)
	GetServer(context.Context, string) (Server, error)
	DeleteServer(context.Context, string) error
	SetLabels(context.Context, string, map[string]string) error
}

func NewGCPLeaseBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = "gcp"
	return &gcpLeaseBackend{DirectSSHBackend: shared.DirectSSHBackend{SpecValue: spec, Cfg: cfg, RT: rt, StoredLeaseKeys: true}}
}

func (b *gcpLeaseBackend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	return acquireAttemptsRetry(b.RT, req.Keep, func() (LeaseTarget, error) {
		return b.acquireOnce(ctx, req.Keep, req.RequestedSlug)
	})
}

func (b *gcpLeaseBackend) acquireOnce(ctx context.Context, keep bool, requestedSlug string) (LeaseTarget, error) {
	if b.Cfg.Tailscale.Enabled && b.Cfg.Tailscale.AuthKey == "" {
		return LeaseTarget{}, exit(2, "direct --tailscale requires %s to contain a Tailscale auth key; brokered mode uses coordinator OAuth secrets", b.Cfg.Tailscale.AuthKeyEnv)
	}
	client, err := newGCPClient(ctx, b.Cfg)
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
	fmt.Fprintf(b.RT.Stderr, "provisioning provider=gcp lease=%s slug=%s class=%s preferred_type=%s project=%s zone=%s keep=%v market=%s\n",
		leaseID, slug, cfg.Class, cfg.ServerType, cfg.GCPProject, cfg.GCPZone, keep, cfg.Capacity.Market)
	server, cfg, err := client.CreateServerWithFallback(ctx, cfg, publicKey, leaseID, slug, keep, func(format string, args ...any) {
		fmt.Fprintf(b.RT.Stderr, format, args...)
	})
	if err != nil {
		return LeaseTarget{}, err
	}
	rollback := true
	rollbackCloudID := server.CloudID
	rollbackClient := client
	defer func() {
		if !rollback || strings.TrimSpace(rollbackCloudID) == "" {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cleanupClient, cleanupClientErr := newGCPClient(cleanupCtx, cfg)
		if cleanupClientErr != nil {
			fmt.Fprintf(b.RT.Stderr, "warning: create gcp cleanup client for %s: %v\n", rollbackCloudID, cleanupClientErr)
			cleanupClient = rollbackClient
		}
		if err := cleanupClient.DeleteServer(cleanupCtx, rollbackCloudID); err != nil {
			fmt.Fprintf(b.RT.Stderr, "warning: cleanup gcp server %s after acquire failure: %v\n", rollbackCloudID, err)
		}
	}()
	client, err = newGCPClient(ctx, cfg)
	if err != nil {
		return LeaseTarget{}, err
	}
	rollbackClient = client
	fmt.Fprintf(b.RT.Stderr, "provisioned lease=%s server=%s type=%s zone=%s\n", leaseID, server.DisplayID(), cfg.ServerType, cfg.GCPZone)
	server, err = client.WaitForServerIP(ctx, server.CloudID)
	if err != nil {
		return LeaseTarget{}, err
	}
	target := sshTargetFromConfig(cfg, server.PublicNet.IPv4.IP)
	if err := waitForSSHReady(ctx, &target, b.RT.Stderr, "bootstrap", bootstrapWaitTimeout(cfg)); err != nil {
		return LeaseTarget{}, err
	}
	server.Labels["state"] = "ready"
	rollback = false
	if err := client.SetLabels(ctx, server.CloudID, server.Labels); err != nil {
		fmt.Fprintf(b.RT.Stderr, "warning: set labels: %v\n", err)
	}
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *gcpLeaseBackend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	client, err := newGCPClient(ctx, b.Cfg)
	if err != nil {
		return LeaseTarget{}, err
	}
	if strings.HasPrefix(req.ID, "crabbox-") {
		server, err := client.GetServer(ctx, req.ID)
		if err != nil {
			if !core.IsGCPNotFound(err) {
				return LeaseTarget{}, err
			}
		} else {
			if !isCrabboxGCPLease(server) {
				return LeaseTarget{}, exit(4, "lease/server not found: %s (instance exists but is not Crabbox-managed)", req.ID)
			}
			leaseID := blank(server.Labels["lease"], req.ID)
			target := sshTargetFromConfig(b.Cfg, server.PublicNet.IPv4.IP)
			useStoredTestboxKey(&target, leaseID)
			return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
		}
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

func isCrabboxGCPLease(server Server) bool {
	if server.Labels == nil {
		return false
	}
	if server.Labels["crabbox"] != "true" {
		return false
	}
	if provider := server.Labels["provider"]; provider != "" && provider != "gcp" {
		return false
	}
	return true
}

func (b *gcpLeaseBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	_ = req
	client, err := newGCPClient(ctx, b.Cfg)
	if err != nil {
		return nil, err
	}
	return client.ListCrabboxServers(ctx)
}

func (b *gcpLeaseBackend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	servers, err := b.List(ctx, ListRequest{})
	if err != nil {
		return core.DoctorResult{}, err
	}
	result := core.InventoryDoctorResult("gcp", len(servers))
	result.Message += fmt.Sprintf(" project=%s zone=aggregated", b.Cfg.GCPProject)
	return result, nil
}

func (b *gcpLeaseBackend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	client, err := newGCPClient(ctx, b.Cfg)
	if err != nil {
		return err
	}
	if zone := req.Lease.Server.Labels["zone"]; zone != "" {
		cfg := b.Cfg
		cfg.GCPZone = zone
		client, err = newGCPClient(ctx, cfg)
		if err != nil {
			return err
		}
	}
	if err := client.DeleteServer(ctx, req.Lease.Server.CloudID); err != nil {
		return err
	}
	removeLeaseClaim(req.Lease.LeaseID)
	return nil
}

func (b *gcpLeaseBackend) ReleaseLeaseMessage(lease LeaseTarget) string {
	return fmt.Sprintf("deleted lease=%s server=%s name=%s", lease.LeaseID, lease.Server.DisplayID(), lease.Server.Name)
}

func (b *gcpLeaseBackend) Touch(ctx context.Context, req TouchRequest) (Server, error) {
	client, err := newGCPClient(ctx, b.Cfg)
	if err != nil {
		return Server{}, err
	}
	if zone := req.Lease.Server.Labels["zone"]; zone != "" {
		cfg := b.Cfg
		cfg.GCPZone = zone
		client, err = newGCPClient(ctx, cfg)
		if err != nil {
			return Server{}, err
		}
	}
	server := req.Lease.Server
	server.Labels = core.TouchDirectLeaseLabels(server.Labels, b.Cfg, req.State, time.Now().UTC())
	if err := client.SetLabels(ctx, server.CloudID, server.Labels); err != nil {
		return Server{}, err
	}
	return server, nil
}

func (b *gcpLeaseBackend) Cleanup(ctx context.Context, req CleanupRequest) error {
	servers, err := b.List(ctx, ListRequest{Options: req.Options})
	if err != nil {
		return err
	}
	client, err := newGCPClient(ctx, b.Cfg)
	if err != nil {
		return err
	}
	completeServers, err := client.ListCrabboxServersComplete(ctx)
	if err != nil {
		return err
	}
	liveLeaseIDs := make(map[string]struct{}, len(completeServers))
	for _, server := range completeServers {
		if leaseID := strings.TrimSpace(server.Labels["lease"]); leaseID != "" {
			liveLeaseIDs[leaseID] = struct{}{}
		}
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
		cfg := b.Cfg
		if zone := server.Labels["zone"]; zone != "" {
			cfg.GCPZone = zone
		}
		client, err := newGCPClient(ctx, cfg)
		if err != nil {
			return err
		}
		if err := client.DeleteServer(ctx, server.CloudID); err != nil {
			return err
		}
		if leaseID := strings.TrimSpace(server.Labels["lease"]); leaseID != "" {
			removeLeaseClaim(leaseID)
		}
	}
	if err := b.pruneStaleClaims(liveLeaseIDs, req.DryRun); err != nil {
		return err
	}
	return nil
}

func (b *gcpLeaseBackend) pruneStaleClaims(liveLeaseIDs map[string]struct{}, dryRun bool) error {
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return err
	}
	scope := gcpClaimScope(b.Cfg)
	for _, claim := range claims {
		if strings.TrimSpace(claim.Provider) != "gcp" {
			continue
		}
		if strings.TrimSpace(claim.ProviderScope) != scope {
			continue
		}
		if _, ok := liveLeaseIDs[claim.LeaseID]; ok {
			continue
		}
		fmt.Fprintf(b.RT.Stderr, "remove stale claim lease=%s slug=%s provider=gcp\n", claim.LeaseID, blank(claim.Slug, "-"))
		if !dryRun {
			removeLeaseClaim(claim.LeaseID)
		}
	}
	return nil
}

func gcpClaimScope(cfg Config) string {
	if cfg.GCPProject == "" {
		return ""
	}
	return "project:" + cfg.GCPProject
}

func acquireAttemptsRetry(rt Runtime, keep bool, acquire func() (LeaseTarget, error)) (LeaseTarget, error) {
	return shared.AcquireAttemptsRetry(rt, keep, acquire)
}

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}

var newGCPClient = func(ctx context.Context, cfg Config) (gcpClient, error) {
	return core.NewGCPClient(ctx, cfg)
}

func newLeaseID() string { return core.NewLeaseID() }
func allocateDirectLeaseSlug(id, requested string, servers []Server) (string, error) {
	return core.AllocateDirectLeaseSlug(id, requested, servers)
}
func ensureTestboxKeyForConfig(cfg Config, leaseID string) (string, string, error) {
	return core.EnsureTestboxKeyForConfig(cfg, leaseID)
}
func providerKeyForLease(leaseID string) string { return core.ProviderKeyForLease(leaseID) }
func sshTargetFromConfig(cfg Config, host string) SSHTarget {
	return core.SSHTargetFromConfig(cfg, host)
}
func waitForSSHReady(ctx context.Context, target *SSHTarget, stderr io.Writer, phase string, timeout time.Duration) error {
	return core.WaitForSSHReady(ctx, target, stderr, phase, timeout)
}
func bootstrapWaitTimeout(cfg Config) time.Duration { return core.BootstrapWaitTimeout(cfg) }
func blank(value, fallback string) string           { return core.Blank(value, fallback) }
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
