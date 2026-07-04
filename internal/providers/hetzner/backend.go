package hetzner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
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

type hetznerLeaseBackend struct {
	shared.DirectSSHBackend
	acquired sync.Map
}

type acquiredHetznerLease struct {
	LeaseID string
	CloudID string
	ID      int64
}

func NewHetznerLeaseBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = providerName
	return &hetznerLeaseBackend{DirectSSHBackend: shared.DirectSSHBackend{SpecValue: spec, Cfg: cfg, RT: rt, Delete: deleteServer, StoredLeaseKeys: true}}
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
	server = normalizeHetznerServer(server)
	rollbackServer = server
	rollbackServerCreated = true
	fmt.Fprintf(b.RT.Stderr, "provisioned lease=%s server=%d type=%s\n", leaseID, server.ID, cfg.ServerType)
	server, err = waitForServerIP(ctx, client, server.ID)
	if err != nil {
		return LeaseTarget{}, err
	}
	server = normalizeHetznerServer(server)
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
	target = LeaseTarget{Server: server, SSH: ssh, LeaseID: leaseID}
	b.acquired.Store(leaseID, acquiredHetznerLease{LeaseID: leaseID, CloudID: server.CloudID, ID: server.ID})
	return target, nil
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
		server = normalizeHetznerServer(server)
		if err := validateHetznerResolveOwnership(server, req); err != nil {
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
	servers = ownedHetznerServers(servers)
	if server, leaseID, err := findServerByAlias(servers, req.ID); err != nil {
		return LeaseTarget{}, err
	} else if leaseID != "" {
		if err := validateHetznerResolveOwnership(server, req); err != nil {
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
	servers, err := client.ListCrabboxServers(ctx)
	if err != nil {
		return nil, err
	}
	return ownedHetznerServers(servers), nil
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
	server := normalizeHetznerServer(req.Lease.Server)
	claim, err := requireExactHetznerClaim(server, req.Lease.LeaseID)
	if err != nil {
		if !b.matchesAcquiredLease(server, req.Lease.LeaseID) {
			return err
		}
		client, clientErr := newHetznerClient()
		if clientErr != nil {
			return clientErr
		}
		serverGone, deleteErr := deleteServerWithClient(ctx, client, server, true, req.Lease.LeaseID)
		if serverGone {
			b.acquired.Delete(req.Lease.LeaseID)
		}
		return deleteErr
	}
	client, err := newHetznerClient()
	if err != nil {
		return err
	}
	serverGone, err := deleteClaimedHetznerServer(ctx, client, server, claim)
	if serverGone {
		b.acquired.Delete(req.Lease.LeaseID)
	}
	return err
}

func (b *hetznerLeaseBackend) matchesAcquiredLease(server Server, leaseID string) bool {
	if validateHetznerServerOwnership(server, false) != nil || server.Labels["lease"] != leaseID {
		return false
	}
	value, ok := b.acquired.Load(leaseID)
	if !ok {
		return false
	}
	acquired, ok := value.(acquiredHetznerLease)
	return ok && acquired.LeaseID == leaseID && acquired.CloudID == server.CloudID && acquired.ID == server.ID
}

func (b *hetznerLeaseBackend) ReleaseLeaseMessage(lease LeaseTarget) string {
	return fmt.Sprintf("deleted lease=%s server=%s name=%s", lease.LeaseID, lease.Server.DisplayID(), lease.Server.Name)
}

func (b *hetznerLeaseBackend) Touch(ctx context.Context, req TouchRequest) (Server, error) {
	return b.DirectSSHBackend.Touch(ctx, req.Lease.Server, req.State), nil
}

func (b *hetznerLeaseBackend) Cleanup(ctx context.Context, req CleanupRequest) error {
	client, err := newHetznerClient()
	if err != nil {
		return err
	}
	servers, err := client.ListCrabboxServers(ctx)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if b.RT.Clock != nil {
		now = b.RT.Clock.Now().UTC()
	}
	for _, raw := range servers {
		server := normalizeHetznerServer(raw)
		if err := validateHetznerServerOwnership(server, false); err != nil {
			fmt.Fprintf(b.RT.Stderr, "skip server id=%s name=%s reason=canonical Crabbox ownership labels missing\n", server.DisplayID(), server.Name)
			continue
		}
		shouldDelete, reason := core.ShouldCleanupServer(server, now)
		if !shouldDelete {
			fmt.Fprintf(b.RT.Stderr, "skip server id=%s name=%s reason=%s\n", server.DisplayID(), server.Name, reason)
			continue
		}
		claim, claimErr := requireExactHetznerClaim(server, server.Labels["lease"])
		if claimErr != nil {
			fmt.Fprintf(b.RT.Stderr, "skip server id=%s name=%s reason=exact local claim missing or stale\n", server.DisplayID(), server.Name)
			continue
		}
		fmt.Fprintf(b.RT.Stderr, "delete server id=%s name=%s\n", server.DisplayID(), server.Name)
		if req.DryRun {
			continue
		}
		if _, err := deleteClaimedHetznerServer(ctx, client, server, claim); err != nil {
			return err
		}
	}
	return nil
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
	_ = cfg
	server = normalizeHetznerServer(server)
	if err := validateHetznerServerOwnership(server, true); err != nil {
		return false, err
	}
	claim, err := requireExactHetznerClaim(server, server.Labels["lease"])
	if err != nil {
		return false, err
	}
	client, err := newHetznerClient()
	if err != nil {
		return false, err
	}
	return deleteClaimedHetznerServer(ctx, client, server, claim)
}

func deleteClaimedHetznerServer(ctx context.Context, client hetznerClient, server Server, claim core.LeaseClaim) (bool, error) {
	serverGone := false
	updated, err := core.UpdateLeaseClaimLabelsIfUnchangedAfter(claim.LeaseID, claim, claim.Labels, func() error {
		var deleteErr error
		serverGone, deleteErr = deleteServerWithClient(ctx, client, server, true, claim.LeaseID)
		return deleteErr
	})
	if err != nil {
		return false, err
	}
	if serverGone {
		if err := core.RemoveLeaseClaimIfUnchanged(updated.LeaseID, updated); err != nil {
			return false, fmt.Errorf("finalize Hetzner cleanup claim: %w", err)
		}
	}
	return serverGone, nil
}

func deleteServerWithClient(ctx context.Context, client hetznerClient, server Server, deleteKey bool, expectedLeaseID string) (bool, error) {
	server = normalizeHetznerServer(server)
	if err := validateHetznerServerOwnership(server, true); err != nil {
		return false, err
	}
	if server.Labels["lease"] != expectedLeaseID {
		return false, exit(2, "refusing to delete Hetzner server %s for mismatched lease %s", server.DisplayID(), expectedLeaseID)
	}
	if !core.IsCanonicalLeaseID(expectedLeaseID) {
		return false, exit(2, "refusing to delete Hetzner server %s for non-canonical lease %s", server.DisplayID(), expectedLeaseID)
	}
	// Delete the auxiliary key first. If that fails, retaining the server keeps
	// the exact claim reachable through normal resolve-and-release retries.
	if keyName := core.ServerProviderKey(server); deleteKey && core.ValidCrabboxProviderKey(keyName) {
		if err := client.DeleteSSHKey(ctx, keyName); err != nil {
			return false, err
		}
	}
	if err := client.DeleteServer(ctx, server.ID); err != nil {
		if !hetznerServerAlreadyAbsent(err, server.ID) {
			return false, err
		}
	}
	return true, nil
}
func hetznerServerAlreadyAbsent(err error, serverID int64) bool {
	return strings.HasPrefix(err.Error(), fmt.Sprintf("hetzner DELETE /servers/%d: http 404:", serverID))
}
func validateHetznerServerOwnership(server Server, allowLegacyProvider bool) error {
	provider := strings.TrimSpace(server.Labels["provider"])
	if server.Labels == nil ||
		server.Labels["crabbox"] != "true" ||
		server.Labels["created_by"] != "crabbox" ||
		(provider != providerName && !(allowLegacyProvider && provider == "")) ||
		!core.IsCanonicalLeaseID(server.Labels["lease"]) ||
		strings.TrimSpace(server.Labels["slug"]) == "" {
		return exit(2, "refusing to operate on non-Crabbox Hetzner server: %s", server.DisplayID())
	}
	return nil
}

func validateHetznerResolveOwnership(server Server, req ResolveRequest) error {
	claim, claimExists, err := core.ReadLeaseClaimWithPresence(server.Labels["lease"])
	if err != nil {
		return err
	}
	if err := validateHetznerServerOwnership(server, claimExists); err != nil {
		return err
	}
	if claimExists {
		if upgradeableHetznerClaim(claim, server) && req.Reclaim && !req.ReleaseOnly && !req.NoLocalStateMutations {
			return nil
		}
		if err := validateHetznerClaim(claim, server, server.Labels["lease"]); err != nil {
			return err
		}
		return nil
	}
	if req.ReleaseOnly {
		return exit(2, "hetzner lease=%s has no exact local claim; refusing release", server.Labels["lease"])
	}
	if req.NoLocalStateMutations {
		return nil
	}
	if !req.Reclaim {
		return exit(2, "hetzner lease=%s is unclaimed; use --reclaim to adopt it explicitly", server.Labels["lease"])
	}
	if req.Repo.Root == "" {
		return exit(2, "hetzner lease=%s cannot be reclaimed without a repository root", server.Labels["lease"])
	}
	return nil
}

func upgradeableHetznerClaim(claim core.LeaseClaim, server Server) bool {
	return claim.LeaseID == server.Labels["lease"] &&
		(claim.Provider == "" || claim.Provider == providerName) &&
		claim.CloudID == "" &&
		(claim.Slug == "" || claim.Slug == server.Labels["slug"])
}

func requireExactHetznerClaim(server Server, expectedLeaseID string) (core.LeaseClaim, error) {
	claim, exists, err := core.ReadLeaseClaimWithPresence(expectedLeaseID)
	if err != nil {
		return core.LeaseClaim{}, err
	}
	if !exists {
		return core.LeaseClaim{}, exit(2, "hetzner lease=%s has no exact local claim; refusing destructive operation", expectedLeaseID)
	}
	if err := validateHetznerServerOwnership(server, true); err != nil {
		return core.LeaseClaim{}, err
	}
	if err := validateHetznerClaim(claim, server, expectedLeaseID); err != nil {
		return core.LeaseClaim{}, err
	}
	return claim, nil
}

func validateHetznerClaim(claim core.LeaseClaim, server Server, expectedLeaseID string) error {
	if claim.LeaseID != expectedLeaseID ||
		claim.Provider != providerName ||
		claim.CloudID == "" ||
		claim.CloudID != server.CloudID ||
		server.Labels["lease"] != expectedLeaseID ||
		(claim.Slug != "" && claim.Slug != server.Labels["slug"]) {
		return exit(2, "refusing to operate on Hetzner server %s from a missing or stale exact local claim", server.DisplayID())
	}
	return nil
}

func normalizeHetznerServer(server Server) Server {
	if server.CloudID == "" && server.ID > 0 {
		server.CloudID = strconv.FormatInt(server.ID, 10)
	}
	server.Provider = providerName
	return server
}

func ownedHetznerServers(servers []Server) []Server {
	owned := make([]Server, 0, len(servers))
	for _, raw := range servers {
		server := normalizeHetznerServer(raw)
		claim, claimExists, err := core.ReadLeaseClaimWithPresence(server.Labels["lease"])
		allowLegacyProvider := err == nil && claimExists &&
			(validateHetznerClaim(claim, server, server.Labels["lease"]) == nil || upgradeableHetznerClaim(claim, server))
		if validateHetznerServerOwnership(server, allowLegacyProvider) == nil {
			owned = append(owned, server)
		}
	}
	return owned
}
func rollbackHetznerAcquire(client hetznerClient, server Server, serverCreated bool, keyName string, keyCreated bool) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if serverCreated {
		_, err := deleteServerWithClient(cleanupCtx, client, server, keyCreated, server.Labels["lease"])
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
