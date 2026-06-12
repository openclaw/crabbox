package azure

import (
	"context"
	"fmt"
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

type azureLeaseBackend struct{ shared.DirectSSHBackend }

const azureAcquireRollbackTimeout = 20 * time.Minute

type azureClient interface {
	ListCrabboxServers(context.Context) ([]Server, error)
	CreateServerWithFallback(context.Context, Config, string, string, string, bool, func(string, ...any)) (Server, Config, error)
	WaitForServerIP(context.Context, string) (Server, error)
	GetServer(context.Context, string) (Server, error)
	DeleteServer(context.Context, string) error
	SetTags(context.Context, string, map[string]string) error
}

func NewAzureLeaseBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = "azure"
	return &azureLeaseBackend{DirectSSHBackend: shared.DirectSSHBackend{SpecValue: spec, Cfg: cfg, RT: rt, Delete: deleteServer, StoredLeaseKeys: true}}
}

func (b *azureLeaseBackend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	return acquireAttemptsRetry(b.RT, req.Keep, func() (LeaseTarget, error) {
		return b.acquireOnce(ctx, req.Keep, req.RequestedSlug)
	})
}

func (b *azureLeaseBackend) acquireOnce(ctx context.Context, keep bool, requestedSlug string) (LeaseTarget, error) {
	cfg := b.Cfg
	if cfg.Tailscale.Enabled && cfg.Tailscale.AuthKey == "" {
		return LeaseTarget{}, exit(2, "direct --tailscale requires %s to contain a Tailscale auth key; brokered mode uses coordinator OAuth secrets", cfg.Tailscale.AuthKeyEnv)
	}
	if err := validateAzureSSHCIDRsForAcquire(ctx, cfg); err != nil {
		return LeaseTarget{}, err
	}
	client, err := newAzureClient(ctx, cfg)
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
	keyPath, publicKey, err := ensureTestboxKeyForConfig(cfg, leaseID)
	if err != nil {
		return LeaseTarget{}, err
	}
	cfg.SSHKey = keyPath
	cfg.ProviderKey = providerKeyForLease(leaseID)
	fmt.Fprintf(b.RT.Stderr, "provisioning provider=azure lease=%s slug=%s class=%s preferred_type=%s location=%s rg=%s keep=%v\n",
		leaseID, slug, cfg.Class, cfg.ServerType, cfg.AzureLocation, cfg.AzureResourceGroup, keep)
	server, cfg, err := client.CreateServerWithFallback(ctx, cfg, publicKey, leaseID, slug, keep, func(format string, args ...any) {
		fmt.Fprintf(b.RT.Stderr, format, args...)
	})
	if err != nil {
		return LeaseTarget{}, err
	}
	rollback := true
	rollbackCloudID := server.CloudID
	defer func() {
		if !rollback || strings.TrimSpace(rollbackCloudID) == "" {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), azureAcquireRollbackTimeout)
		defer cancel()
		if err := client.DeleteServer(cleanupCtx, rollbackCloudID); err != nil {
			fmt.Fprintf(b.RT.Stderr, "warning: cleanup azure server %s after acquire failure: %v\n", rollbackCloudID, err)
		}
	}()
	fmt.Fprintf(b.RT.Stderr, "provisioned lease=%s server=%s type=%s\n", leaseID, server.DisplayID(), cfg.ServerType)
	server, err = client.WaitForServerIP(ctx, server.CloudID)
	if err != nil {
		return LeaseTarget{}, err
	}
	target := sshTargetFromConfig(cfg, azureServerHost(server, cfg.AzureNetwork))
	if err := bootstrapManagedWindowsDesktop(ctx, cfg, &target, publicKey, b.RT.Stderr); err != nil {
		return LeaseTarget{}, err
	}
	server.Labels["state"] = "ready"
	rollback = false
	if err := client.SetTags(ctx, server.CloudID, server.Labels); err != nil {
		fmt.Fprintf(b.RT.Stderr, "warning: set tags: %v\n", err)
	}
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *azureLeaseBackend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	client, err := newAzureClient(ctx, b.Cfg)
	if err != nil {
		return LeaseTarget{}, err
	}
	if strings.HasPrefix(req.ID, "crabbox-") {
		server, err := client.GetServer(ctx, req.ID)
		if err != nil {
			return LeaseTarget{}, err
		}
		if !isCrabboxAzureLease(server) {
			return LeaseTarget{}, exit(4, "lease/server not found: %s (vm exists but is not Crabbox-managed)", req.ID)
		}
		leaseID := blank(server.Labels["lease"], req.ID)
		target := sshTargetFromConfig(b.Cfg, azureServerHost(server, b.Cfg.AzureNetwork))
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
		target := sshTargetFromConfig(b.Cfg, azureServerHost(server, b.Cfg.AzureNetwork))
		useStoredTestboxKey(&target, leaseID)
		return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
	}
	return LeaseTarget{}, exit(4, "lease/server not found: %s", req.ID)
}

func isCrabboxAzureLease(server Server) bool {
	if server.Labels == nil {
		return false
	}
	if server.Labels["crabbox"] != "true" {
		return false
	}
	if provider := server.Labels["provider"]; provider != "" && provider != "azure" {
		return false
	}
	return true
}

func (b *azureLeaseBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	_ = req
	client, err := newAzureClient(ctx, b.Cfg)
	if err != nil {
		return nil, err
	}
	return client.ListCrabboxServers(ctx)
}

func (b *azureLeaseBackend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	servers, err := b.List(ctx, ListRequest{})
	if err != nil {
		return core.DoctorResult{}, err
	}
	result := core.InventoryDoctorResult("azure", len(servers))
	result.Message += fmt.Sprintf(" location=%s default_type=%s", b.Cfg.AzureLocation, b.Cfg.ServerType)
	return result, nil
}

func (b *azureLeaseBackend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	if err := deleteServer(ctx, b.Cfg, req.Lease.Server); err != nil {
		return err
	}
	removeLeaseClaim(req.Lease.LeaseID)
	return nil
}

func (b *azureLeaseBackend) ReleaseLeaseMessage(lease LeaseTarget) string {
	return fmt.Sprintf("deleted lease=%s server=%s name=%s", lease.LeaseID, lease.Server.DisplayID(), lease.Server.Name)
}

func (b *azureLeaseBackend) Touch(ctx context.Context, req TouchRequest) (Server, error) {
	return b.DirectSSHBackend.Touch(ctx, req.Lease.Server, req.State), nil
}

func (b *azureLeaseBackend) Cleanup(ctx context.Context, req CleanupRequest) error {
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

var newAzureClient = func(ctx context.Context, cfg Config) (azureClient, error) {
	return core.NewAzureClient(ctx, cfg)
}

var validateAzureSSHCIDRsForAcquire = core.ValidateAzureSSHCIDRsForAcquire

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

var bootstrapManagedWindowsDesktop = core.BootstrapManagedWindowsDesktop

func deleteServer(ctx context.Context, cfg Config, server Server) error {
	client, err := newAzureClient(ctx, cfg)
	if err != nil {
		return err
	}
	name := server.CloudID
	if name == "" {
		name = server.Name
	}
	return client.DeleteServer(ctx, name)
}
func blank(value, fallback string) string { return core.Blank(value, fallback) }
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
func azureServerHost(server Server, network string) string {
	return core.AzureServerHost(server, network)
}
