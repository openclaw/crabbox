package azure

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
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
	LeaseClaimScope() string
	ListCrabboxServers(context.Context) ([]Server, error)
	CreateServerWithFallback(context.Context, Config, string, string, string, bool, func(string, ...any)) (Server, Config, error)
	WaitForServerIP(context.Context, string) (Server, error)
	GetServer(context.Context, string) (Server, error)
	DeleteServer(context.Context, string) error
	PrepareOwnedServer(context.Context, Server) (Server, error)
	PrepareCleanupServer(context.Context, Server, time.Time) (Server, error)
	DeleteOwnedServer(context.Context, Server) error
	DeleteCleanupServer(context.Context, Server, time.Time) error
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
	if err := client.SetTags(ctx, server.CloudID, server.Labels); err != nil {
		fmt.Fprintf(b.RT.Stderr, "warning: set tags: %v\n", err)
	}
	if err := core.ClaimLeaseTargetForConfig(leaseID, slug, cfg, server, target, cfg.IdleTimeout); err != nil {
		return LeaseTarget{}, err
	}
	b.Cfg.AzureSubscription = cfg.AzureSubscription
	b.Cfg.AzureResourceGroup = cfg.AzureResourceGroup
	rollback = false
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
			if req.ReleaseOnly && isAzureCleanupNotFound(err) {
				return resolveMissingAzureReleaseClaim(req.ID, client.LeaseClaimScope())
			}
			return LeaseTarget{}, err
		}
		if !isCrabboxAzureLease(server) {
			return LeaseTarget{}, exit(4, "lease/server not found: %s (vm exists but is not Crabbox-managed)", req.ID)
		}
		leaseID := server.Labels["lease"]
		target := sshTargetFromConfig(b.Cfg, azureServerHost(server, b.Cfg.AzureNetwork))
		useStoredTestboxKey(&target, leaseID)
		return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
	}
	servers, err := listOwnedAzureServers(ctx, client)
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
	if req.ReleaseOnly {
		return resolveMissingAzureReleaseClaim(req.ID, client.LeaseClaimScope())
	}
	return LeaseTarget{}, exit(4, "lease/server not found: %s", req.ID)
}

func resolveMissingAzureReleaseClaim(identifier, providerScope string) (LeaseTarget, error) {
	claim, exists, err := core.ResolveLeaseClaimForProvider(identifier, "azure")
	if err != nil {
		return LeaseTarget{}, err
	}
	if !exists {
		claim, exists, err = core.ResolveLeaseClaimForProviderCloudID(identifier, "azure")
		if err != nil {
			return LeaseTarget{}, err
		}
	}
	if !exists || !core.LeaseClaimMatchesIdentifier(claim, identifier) {
		return LeaseTarget{}, exit(4, "lease/server not found: %s", identifier)
	}
	server := azureServerFromClaim(claim)
	if err := validateExactAzureClaim(claim, server, claim.LeaseID, providerScope); err != nil {
		return LeaseTarget{}, err
	}
	return LeaseTarget{Server: server, LeaseID: claim.LeaseID}, nil
}

func isCrabboxAzureLease(server Server) bool {
	labels := server.Labels
	return labels != nil &&
		labels["crabbox"] == "true" &&
		labels["created_by"] == "crabbox" &&
		labels["provider"] == "azure" &&
		core.IsCanonicalLeaseID(labels["lease"]) &&
		strings.TrimSpace(labels["slug"]) != ""
}

func (b *azureLeaseBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	_ = req
	client, err := newAzureClient(ctx, b.Cfg)
	if err != nil {
		return nil, err
	}
	return listOwnedAzureServers(ctx, client)
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
	client, err := newAzureClient(ctx, b.Cfg)
	if err != nil {
		return err
	}
	claim, err := requireExactAzureClaim(req.Lease.Server, req.Lease.LeaseID, client.LeaseClaimScope())
	if err != nil {
		return err
	}
	prepared, err := client.PrepareOwnedServer(ctx, req.Lease.Server)
	if err != nil {
		return err
	}
	if err := validateExactAzureClaim(claim, prepared, req.Lease.LeaseID, client.LeaseClaimScope()); err != nil {
		return err
	}
	claim, err = core.UpdateLeaseClaimLabelsIfUnchanged(claim.LeaseID, claim, prepared.Labels)
	if err != nil {
		return err
	}
	if err := core.RemoveLeaseClaimIfUnchangedAfter(claim.LeaseID, claim, func() error {
		return client.DeleteOwnedServer(ctx, prepared)
	}); err != nil {
		return err
	}
	core.RemoveStoredTestboxKey(req.Lease.LeaseID)
	return nil
}

func (b *azureLeaseBackend) ReleaseLeaseMessage(lease LeaseTarget) string {
	return fmt.Sprintf("deleted lease=%s server=%s name=%s", lease.LeaseID, lease.Server.DisplayID(), lease.Server.Name)
}

func (b *azureLeaseBackend) Touch(ctx context.Context, req TouchRequest) (Server, error) {
	return b.DirectSSHBackend.Touch(ctx, req.Lease.Server, req.State), nil
}

func (b *azureLeaseBackend) Cleanup(ctx context.Context, req CleanupRequest) error {
	client, err := newAzureClient(ctx, b.Cfg)
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
	seen := make(map[string]bool, len(servers))
	for _, server := range servers {
		seen[server.CloudID] = true
		if !isCrabboxAzureLease(server) {
			fmt.Fprintf(b.RT.Stderr, "skip server id=%s name=%s reason=canonical Crabbox ownership tags missing\n", server.DisplayID(), server.Name)
			continue
		}
		shouldDelete, reason := core.ShouldCleanupServer(server, now)
		if !shouldDelete {
			fmt.Fprintf(b.RT.Stderr, "skip server id=%s name=%s reason=%s\n", server.DisplayID(), server.Name, reason)
			continue
		}
		claim, claimErr := requireExactAzureClaim(server, server.Labels["lease"], client.LeaseClaimScope())
		if claimErr != nil {
			fmt.Fprintf(b.RT.Stderr, "skip server id=%s name=%s reason=exact local claim missing or stale\n", server.DisplayID(), server.Name)
			continue
		}
		live, err := client.GetServer(ctx, server.CloudID)
		if err != nil {
			if isAzureCleanupNotFound(err) {
				fmt.Fprintf(b.RT.Stderr, "skip server id=%s name=%s reason=live VM no longer exists\n", server.DisplayID(), server.Name)
				// Keep local recovery state: neither deterministically named companion resources nor the claim can be cleared without a live VM proving ownership.
				continue
			}
			return fmt.Errorf("re-read Azure cleanup candidate %s: %w", server.DisplayID(), err)
		}
		if err := validateAzureCleanupLiveServer(server, live); err != nil {
			fmt.Fprintf(b.RT.Stderr, "skip server id=%s name=%s reason=%v\n", server.DisplayID(), server.Name, err)
			continue
		}
		if err := validateExactAzureClaim(claim, live, server.Labels["lease"], client.LeaseClaimScope()); err != nil {
			fmt.Fprintf(b.RT.Stderr, "skip server id=%s name=%s reason=exact local claim missing or stale\n", server.DisplayID(), server.Name)
			continue
		}
		if shouldDelete, reason := core.ShouldCleanupServer(live, now); !shouldDelete {
			fmt.Fprintf(b.RT.Stderr, "skip server id=%s name=%s reason=live VM %s\n", server.DisplayID(), server.Name, reason)
			continue
		}
		fmt.Fprintf(b.RT.Stderr, "delete server id=%s name=%s\n", live.DisplayID(), live.Name)
		if req.DryRun {
			continue
		}
		prepared, err := client.PrepareCleanupServer(ctx, live, now)
		if err != nil {
			if core.IsAzureCleanupSkipError(err) {
				fmt.Fprintf(b.RT.Stderr, "skip server id=%s name=%s reason=%v\n", live.DisplayID(), live.Name, err)
				continue
			}
			return err
		}
		claim, err = core.UpdateLeaseClaimLabelsIfUnchanged(claim.LeaseID, claim, prepared.Labels)
		if err != nil {
			return err
		}
		if err := core.RemoveLeaseClaimIfUnchangedAfter(claim.LeaseID, claim, func() error {
			return client.DeleteCleanupServer(ctx, prepared, now)
		}); err != nil {
			if core.IsAzureCleanupSkipError(err) {
				fmt.Fprintf(b.RT.Stderr, "skip server id=%s name=%s reason=%v\n", live.DisplayID(), live.Name, err)
				continue
			}
			if isAzureCleanupNotFound(err) {
				fmt.Fprintf(b.RT.Stderr, "skip server id=%s name=%s reason=live VM no longer exists at delete boundary\n", live.DisplayID(), live.Name)
				continue
			}
			return err
		}
		core.RemoveStoredTestboxKey(claim.LeaseID)
	}
	return b.resumeAzureCleanupClaims(ctx, client, seen, now, req.DryRun)
}

func (b *azureLeaseBackend) resumeAzureCleanupClaims(ctx context.Context, client azureClient, seen map[string]bool, now time.Time, dryRun bool) error {
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return err
	}
	for _, claim := range claims {
		if claim.Provider != "azure" || claim.ProviderScope != client.LeaseClaimScope() || seen[claim.CloudID] || !core.HasAzureCleanupBinding(claim.Labels) {
			continue
		}
		server := azureServerFromClaim(claim)
		if err := validateExactAzureClaim(claim, server, claim.LeaseID, client.LeaseClaimScope()); err != nil {
			fmt.Fprintf(b.RT.Stderr, "skip recovery server id=%s reason=exact local claim missing or stale\n", claim.CloudID)
			continue
		}
		fmt.Fprintf(b.RT.Stderr, "resume cleanup server id=%s name=%s\n", server.DisplayID(), server.Name)
		if dryRun {
			continue
		}
		prepared, err := client.PrepareCleanupServer(ctx, server, now)
		if err != nil {
			if core.IsAzureCleanupSkipError(err) {
				fmt.Fprintf(b.RT.Stderr, "skip recovery server id=%s reason=%v\n", server.DisplayID(), err)
				continue
			}
			return err
		}
		claim, err = core.UpdateLeaseClaimLabelsIfUnchanged(claim.LeaseID, claim, prepared.Labels)
		if err != nil {
			return err
		}
		if err := core.RemoveLeaseClaimIfUnchangedAfter(claim.LeaseID, claim, func() error {
			return client.DeleteCleanupServer(ctx, prepared, now)
		}); err != nil {
			if core.IsAzureCleanupSkipError(err) {
				fmt.Fprintf(b.RT.Stderr, "skip recovery server id=%s reason=%v\n", server.DisplayID(), err)
				continue
			}
			return err
		}
		core.RemoveStoredTestboxKey(claim.LeaseID)
	}
	return nil
}

func azureServerFromClaim(claim core.LeaseClaim) Server {
	return Server{
		CloudID:     claim.CloudID,
		Name:        claim.CloudID,
		Provider:    "azure",
		ImmutableID: claim.CloudImmutableID,
		Labels:      maps.Clone(claim.Labels),
	}
}

func listOwnedAzureServers(ctx context.Context, client azureClient) ([]Server, error) {
	servers, err := client.ListCrabboxServers(ctx)
	if err != nil {
		return nil, err
	}
	owned := make([]Server, 0, len(servers))
	for _, server := range servers {
		if isCrabboxAzureLease(server) {
			owned = append(owned, server)
		}
	}
	return owned, nil
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
	if !isCrabboxAzureLease(server) {
		return exit(4, "refusing to delete Azure VM %s without canonical Crabbox ownership tags", server.DisplayID())
	}
	client, err := newAzureClient(ctx, cfg)
	if err != nil {
		return err
	}
	return deleteAzureServerWithClient(ctx, client, server)
}

func deleteAzureServerWithClient(ctx context.Context, client azureClient, server Server) error {
	name := server.CloudID
	if name == "" {
		name = server.Name
	}
	return client.DeleteServer(ctx, name)
}

func requireExactAzureClaim(server Server, expectedLeaseID, providerScope string) (core.LeaseClaim, error) {
	claim, exists, err := core.ReadLeaseClaimWithPresence(expectedLeaseID)
	if err != nil {
		return core.LeaseClaim{}, err
	}
	if !exists {
		return core.LeaseClaim{}, exit(2, "azure lease=%s has no exact local claim; refusing destructive operation", expectedLeaseID)
	}
	if err := validateExactAzureClaim(claim, server, expectedLeaseID, providerScope); err != nil {
		return core.LeaseClaim{}, err
	}
	return claim, nil
}

func validateExactAzureClaim(claim core.LeaseClaim, server Server, expectedLeaseID, providerScope string) error {
	serverSlug := strings.TrimSpace(server.Labels["slug"])
	serverProviderKey := strings.TrimSpace(server.Labels["provider_key"])
	if strings.TrimSpace(providerScope) == "" ||
		!isCrabboxAzureLease(server) ||
		claim.LeaseID != expectedLeaseID ||
		claim.Provider != "azure" ||
		claim.ProviderScope != providerScope ||
		claim.CloudID == "" ||
		claim.CloudID != strings.TrimSpace(server.CloudID) ||
		claim.CloudImmutableID == "" ||
		claim.CloudImmutableID != strings.TrimSpace(server.ImmutableID) ||
		claim.Slug == "" ||
		claim.Slug != serverSlug ||
		server.Labels["lease"] != expectedLeaseID ||
		strings.TrimSpace(claim.Labels["lease"]) != expectedLeaseID ||
		strings.TrimSpace(claim.Labels["slug"]) != serverSlug ||
		strings.TrimSpace(claim.Labels["provider"]) != "azure" ||
		serverProviderKey == "" ||
		strings.TrimSpace(claim.Labels["provider_key"]) != serverProviderKey {
		return exit(2, "refusing to operate on Azure VM %s from a missing or stale exact local claim", server.DisplayID())
	}
	return nil
}

func validateAzureCleanupLiveServer(expected, live Server) error {
	cloudID := strings.TrimSpace(expected.CloudID)
	if cloudID == "" || strings.TrimSpace(live.CloudID) != cloudID {
		return fmt.Errorf("live cloud id %q does not match cleanup candidate %q", live.CloudID, expected.CloudID)
	}
	if strings.TrimSpace(expected.ImmutableID) == "" || strings.TrimSpace(live.ImmutableID) != strings.TrimSpace(expected.ImmutableID) {
		return fmt.Errorf("live VM identity %q does not match cleanup candidate identity %q", live.ImmutableID, expected.ImmutableID)
	}
	if !isCrabboxAzureLease(live) {
		return fmt.Errorf("live VM no longer has canonical Crabbox ownership tags")
	}
	expectedLeaseID := strings.TrimSpace(expected.Labels["lease"])
	if liveLeaseID := strings.TrimSpace(live.Labels["lease"]); liveLeaseID != expectedLeaseID {
		return fmt.Errorf("live VM lease %q does not match cleanup candidate lease %q", liveLeaseID, expectedLeaseID)
	}
	return nil
}

func isAzureCleanupNotFound(err error) bool {
	var exitErr core.ExitError
	if core.AsExitError(err, &exitErr) && exitErr.Code == 4 {
		return true
	}
	var responseErr *azcore.ResponseError
	if errors.As(err, &responseErr) && responseErr.StatusCode == 404 {
		return true
	}
	message := err.Error()
	return strings.Contains(message, "ResourceNotFound") || strings.Contains(message, "NotFound")
}

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
func azureServerHost(server Server, network string) string {
	return core.AzureServerHost(server, network)
}
