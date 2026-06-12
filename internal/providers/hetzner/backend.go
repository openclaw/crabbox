package hetzner

import (
	"context"
	"errors"
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

const providerName = "hetzner"

type hetznerClient interface {
	ListCrabboxServers(context.Context) ([]Server, error)
	EnsureSSHKey(context.Context, string, string) (core.SSHKey, bool, error)
	CreateServerWithFallback(context.Context, Config, string, string, string, bool, func(string, ...any)) (Server, Config, error)
	GetServer(context.Context, int64) (Server, error)
	DeleteServer(context.Context, int64) error
	DeleteSSHKey(context.Context, string) error
	SetLabels(context.Context, int64, map[string]string) error
}

type hetznerLeaseBackend struct{ shared.DirectSSHBackend }

func NewHetznerLeaseBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = providerName
	return &hetznerLeaseBackend{DirectSSHBackend: shared.DirectSSHBackend{SpecValue: spec, Cfg: cfg, RT: rt, Delete: deleteServer}}
}

func (b *hetznerLeaseBackend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	return acquireAttemptsRetry(b.RT, req.Keep, func() (LeaseTarget, error) {
		return b.acquireOnce(ctx, req.Keep, req.RequestedSlug)
	})
}

func (b *hetznerLeaseBackend) acquireOnce(ctx context.Context, keep bool, requestedSlug string) (target LeaseTarget, err error) {
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
	rollbackKey := ""
	rollbackKeyCreated := false
	var rollbackServer Server
	rollbackServerCreated := false
	committed := false
	defer func() {
		if err == nil || committed {
			return
		}
		if cleanupErr := rollbackHetznerAcquire(client, rollbackServer, rollbackServerCreated, rollbackKey, rollbackKeyCreated); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("hetzner cleanup failed: %w", cleanupErr))
		}
	}()
	if cfg.ProviderKey != "" {
		providerKey, created, err := client.EnsureSSHKey(ctx, cfg.ProviderKey, publicKey)
		if err != nil {
			return LeaseTarget{}, err
		}
		cfg.ProviderKey = providerKey.Name
		rollbackKey = providerKey.Name
		rollbackKeyCreated = created
	}
	fmt.Fprintf(b.RT.Stderr, "provisioning provider=hetzner lease=%s slug=%s class=%s preferred_type=%s location=%s keep=%v\n", leaseID, slug, cfg.Class, cfg.ServerType, cfg.Location, keep)
	server, cfg, err := client.CreateServerWithFallback(ctx, cfg, publicKey, leaseID, slug, keep, func(format string, args ...any) {
		fmt.Fprintf(b.RT.Stderr, format, args...)
	})
	if err != nil {
		return LeaseTarget{}, err
	}
	rollbackServer = server
	rollbackServerCreated = true
	fmt.Fprintf(b.RT.Stderr, "provisioned lease=%s server=%d type=%s\n", leaseID, server.ID, cfg.ServerType)
	server, err = waitForServerIP(ctx, client, server.ID)
	if err != nil {
		return LeaseTarget{}, err
	}
	rollbackServer = server
	ssh := sshTargetFromConfig(cfg, server.PublicNet.IPv4.IP)
	if err := waitForSSHReady(ctx, &ssh, b.RT.Stderr, "bootstrap", bootstrapWaitTimeout(cfg)); err != nil {
		return LeaseTarget{}, err
	}
	server.Labels["state"] = "ready"
	if err := client.SetLabels(ctx, server.ID, server.Labels); err != nil {
		fmt.Fprintf(b.RT.Stderr, "warning: set labels: %v\n", err)
	}
	committed = true
	return LeaseTarget{Server: server, SSH: ssh, LeaseID: leaseID}, nil
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
		if err := validateHetznerServerOwnership(server); err != nil {
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
		if err := validateHetznerServerOwnership(server); err != nil {
			return LeaseTarget{}, err
		}
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
	serverGone, err := deleteServerForRelease(ctx, b.Cfg, req.Lease.Server)
	if err != nil {
		return err
	}
	if serverGone {
		removeLeaseClaim(req.Lease.LeaseID)
	}
	return nil
}

func (b *hetznerLeaseBackend) ReleaseLeaseMessage(lease LeaseTarget) string {
	return fmt.Sprintf("deleted lease=%s server=%s name=%s", lease.LeaseID, lease.Server.DisplayID(), lease.Server.Name)
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
func allocateDirectLeaseSlug(id, requested string, servers []Server) (string, error) {
	return core.AllocateDirectLeaseSlug(id, requested, servers)
}
func sshTargetFromConfig(cfg Config, host string) SSHTarget {
	return core.SSHTargetFromConfig(cfg, host)
}
func deleteServer(ctx context.Context, cfg Config, server Server) error {
	_, err := deleteServerForRelease(ctx, cfg, server)
	return err
}
func deleteServerForRelease(ctx context.Context, cfg Config, server Server) (bool, error) {
	if err := validateHetznerServerOwnership(server); err != nil {
		return false, err
	}
	client, err := newHetznerClient()
	if err != nil {
		return false, err
	}
	return deleteServerWithClient(ctx, client, server, true)
}
func deleteServerWithClient(ctx context.Context, client hetznerClient, server Server, deleteKey bool) (bool, error) {
	if err := validateHetznerServerOwnership(server); err != nil {
		return false, err
	}
	if err := client.DeleteServer(ctx, server.ID); err != nil {
		if !hetznerServerAlreadyAbsent(err, server.ID) {
			return false, err
		}
	}
	if keyName := core.ServerProviderKey(server); deleteKey && core.ValidCrabboxProviderKey(keyName) {
		return true, client.DeleteSSHKey(ctx, keyName)
	}
	return true, nil
}
func hetznerServerAlreadyAbsent(err error, serverID int64) bool {
	return strings.HasPrefix(err.Error(), fmt.Sprintf("hetzner DELETE /servers/%d: http 404:", serverID))
}
func validateHetznerServerOwnership(server Server) error {
	if server.Labels == nil ||
		server.Labels["crabbox"] != "true" ||
		server.Labels["created_by"] != "crabbox" ||
		(server.Labels["provider"] != "" && server.Labels["provider"] != providerName) ||
		server.Labels["lease"] == "" {
		return exit(2, "refusing to operate on non-Crabbox Hetzner server: %s", server.DisplayID())
	}
	return nil
}
func rollbackHetznerAcquire(client hetznerClient, server Server, serverCreated bool, keyName string, keyCreated bool) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if serverCreated {
		_, err := deleteServerWithClient(cleanupCtx, client, server, keyCreated)
		return err
	}
	if keyCreated && core.ValidCrabboxProviderKey(keyName) {
		return client.DeleteSSHKey(cleanupCtx, keyName)
	}
	return nil
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

var (
	newHetznerClient          = func() (hetznerClient, error) { return core.NewHetznerClient() }
	newLeaseID                = core.NewLeaseID
	ensureTestboxKeyForConfig = core.EnsureTestboxKeyForConfig
	providerKeyForLease       = core.ProviderKeyForLease
	waitForSSHReady           = core.WaitForSSHReady
	bootstrapWaitTimeout      = core.BootstrapWaitTimeout
	waitForServerIP           = func(ctx context.Context, client hetznerClient, id int64) (Server, error) {
		concrete, ok := client.(*core.HetznerClient)
		if !ok {
			return Server{}, exit(2, "hetzner IP wait requires a Hetzner client")
		}
		return core.WaitForServerIP(ctx, concrete, id)
	}
)
