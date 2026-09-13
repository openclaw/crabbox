package proxmox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type leaseBackend struct{ shared.DirectSSHBackend }

const proxmoxReleaseAbsentMarker = "proxmox-release-absent"

type proxmoxClient interface {
	DoctorReadiness(context.Context, core.Config) ([]core.ProxmoxReadinessCheck, error)
	ListCrabboxServers(context.Context) ([]core.Server, error)
	ListCrabboxServersCluster(context.Context) ([]core.Server, error)
	CreateServer(context.Context, core.Config, string, string, string, bool) (core.Server, error)
	GetServer(context.Context, string) (core.Server, error)
	GetServerOnNode(context.Context, string, string) (core.Server, error)
	VMExistsInCluster(context.Context, string) (bool, error)
	DeleteServer(context.Context, string) error
	DeleteServerOnNode(context.Context, string, string) error
	DeleteServerOnNodeChecked(context.Context, string, string, func(core.Server) error) error
	SetLabels(context.Context, string, map[string]string) error
	SetLabelsOnNode(context.Context, string, string, map[string]string) error
}

func NewLeaseBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) core.Backend {
	cfg.Provider = "proxmox"
	if cfg.Proxmox.User != "" {
		cfg.SSHUser = cfg.Proxmox.User
	}
	if cfg.Proxmox.WorkRoot != "" {
		cfg.WorkRoot = cfg.Proxmox.WorkRoot
	}
	return &leaseBackend{DirectSSHBackend: shared.DirectSSHBackend{SpecValue: spec, Cfg: cfg, RT: rt, StoredLeaseKeys: true}}
}

func (b *leaseBackend) Acquire(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	return shared.AcquireAttemptsRetry(b.RT, req.Keep, func() (core.LeaseTarget, error) {
		return b.acquireOnce(ctx, req.Keep, req.RequestedSlug)
	})
}

func (b *leaseBackend) acquireOnce(ctx context.Context, keep bool, requestedSlug string) (core.LeaseTarget, error) {
	if b.Cfg.Proxmox.TemplateID <= 0 {
		return core.LeaseTarget{}, core.Exit(3, "proxmox templateId is required (set proxmox.templateId or CRABBOX_PROXMOX_TEMPLATE_ID)")
	}
	client, err := newClient(b.Cfg)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	leaseID := core.NewLeaseID()
	servers, err := client.ListCrabboxServersCluster(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	slug, err := core.AllocateDirectLeaseSlug(leaseID, requestedSlug, servers)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	cfg := b.Cfg
	keyPath, publicKey, err := core.EnsureTestboxKeyForConfig(cfg, leaseID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	cfg.SSHKey = keyPath
	cfg.ProviderKey = core.ProviderKeyForLease(leaseID)
	cfg.ServerType = core.ProxmoxServerTypeForConfig(cfg)
	fmt.Fprintf(b.RT.Stderr, "provisioning provider=proxmox lease=%s slug=%s node=%s template=%d keep=%v\n",
		leaseID, slug, cfg.Proxmox.Node, cfg.Proxmox.TemplateID, keep)
	server, err := client.CreateServer(ctx, cfg, publicKey, leaseID, slug, keep)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if server.PublicNet.IPv4.IP == "" {
		cloudID := server.CloudID
		hostID := server.HostID
		server, err = b.waitForServerIP(ctx, client, cloudID, core.BootstrapWaitTimeout(cfg))
		if err != nil {
			b.cleanupFailedAcquire(client, core.Server{CloudID: cloudID, HostID: hostID}, leaseID)
			return core.LeaseTarget{}, err
		}
	}
	target := core.SSHTargetFromConfig(cfg, server.PublicNet.IPv4.IP)
	if err := waitForSSHReadyFunc(ctx, &target, b.RT.Stderr, "bootstrap", core.BootstrapWaitTimeout(cfg)); err != nil {
		b.cleanupFailedAcquire(client, server, leaseID)
		return core.LeaseTarget{}, err
	}
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	server.Labels["state"] = "ready"
	if err := client.SetLabels(ctx, server.CloudID, server.Labels); err != nil {
		fmt.Fprintf(b.RT.Stderr, "warning: set proxmox labels: %v\n", err)
	}
	fmt.Fprintf(b.RT.Stderr, "provisioned lease=%s server=%s node=%s ip=%s\n", leaseID, server.DisplayID(), cfg.Proxmox.Node, server.PublicNet.IPv4.IP)
	return core.LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *leaseBackend) cleanupFailedAcquire(client proxmoxClient, server core.Server, leaseID string) {
	node := core.Blank(server.HostID, b.Cfg.Proxmox.Node)
	if err := client.DeleteServerOnNode(context.Background(), node, server.CloudID); err != nil && !core.IsProxmoxNotFound(err) {
		fmt.Fprintf(b.RT.Stderr, "warning: preserve failed Proxmox acquire residue lease=%s reason=delete_failed error=%v\n", leaseID, err)
		return
	}
	exists, err := client.VMExistsInCluster(context.Background(), server.CloudID)
	if err != nil {
		fmt.Fprintf(b.RT.Stderr, "warning: preserve failed Proxmox acquire residue lease=%s reason=cluster_verification_failed error=%v\n", leaseID, err)
		return
	}
	if exists {
		fmt.Fprintf(b.RT.Stderr, "warning: preserve failed Proxmox acquire residue lease=%s reason=vm_still_exists\n", leaseID)
		return
	}
	removeLocalLeaseResidue(leaseID)
}

func (b *leaseBackend) waitForServerIP(ctx context.Context, client proxmoxClient, cloudID string, timeout time.Duration) (core.Server, error) {
	deadlineCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(proxmoxIPPollInterval)
	defer ticker.Stop()
	result, err := shared.Poll(deadlineCtx, 0, proxmoxIPPollInterval,
		func(ctx context.Context, _ time.Duration) error {
			select {
			case <-ctx.Done():
				return context.Cause(ctx)
			case <-ticker.C:
				return nil
			}
		},
		func(ctx context.Context) (core.Server, error) { return client.GetServer(ctx, cloudID) },
		func(_ context.Context, server core.Server, fetchErr error) (bool, error) {
			return server.PublicNet.IPv4.IP != "", fetchErr
		}, nil)
	if err != nil {
		if result.Err == nil && context.Cause(deadlineCtx) != nil && errors.Is(err, context.Cause(deadlineCtx)) {
			return core.Server{}, deadlineCtx.Err()
		}
		return core.Server{}, err
	}
	return result.Value, nil
}

func (b *leaseBackend) Resolve(ctx context.Context, req core.ResolveRequest) (core.LeaseTarget, error) {
	client, err := newClient(b.Cfg)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if req.ID != "" {
		if _, err := strconv.Atoi(req.ID); err == nil || strings.HasPrefix(req.ID, "crabbox-") {
			server, err := client.GetServer(ctx, req.ID)
			if err != nil {
				if !core.IsProxmoxNotFound(err) && !req.ReleaseOnly {
					return core.LeaseTarget{}, err
				}
			} else {
				if !core.IsCrabboxProxmoxLease(server) {
					return core.LeaseTarget{}, core.Exit(4, "lease/server not found: %s (VM exists but is not Crabbox-managed)", req.ID)
				}
				return b.targetForServer(server, req.ReleaseOnly)
			}
		}
	}
	servers, err := client.ListCrabboxServersCluster(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if server, leaseID, err := core.FindServerByAlias(servers, req.ID); err != nil {
		return core.LeaseTarget{}, err
	} else if leaseID != "" {
		target, err := b.targetForServer(server, req.ReleaseOnly)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		target.LeaseID = leaseID
		return target, nil
	}
	if req.ReleaseOnly {
		return b.releaseTargetFromClaim(ctx, client, req.ID)
	}
	return core.LeaseTarget{}, core.Exit(4, "lease/server not found: %s", req.ID)
}

func (b *leaseBackend) releaseTargetFromClaim(ctx context.Context, client proxmoxClient, id string) (core.LeaseTarget, error) {
	var (
		claim core.LeaseClaim
		ok    bool
		err   error
	)
	if _, numeric := strconv.ParseInt(strings.TrimSpace(id), 10, 64); numeric == nil {
		claim, ok, err = b.resolveNumericClaim(id)
	} else {
		var exact bool
		claim, ok, exact, err = core.ResolveLeaseClaimForProviderWithExact(id, "proxmox")
		if err == nil && exact && (!ok || claim.LeaseID != id) {
			return core.LeaseTarget{}, core.Exit(2, "proxmox exact lease identifier %q does not match a valid Proxmox claim", id)
		}
	}
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if !ok || claim.LeaseID == "" || !core.LeaseClaimMatchesIdentifier(claim, id) {
		return core.LeaseTarget{}, core.Exit(4, "lease/server not found: %s", id)
	}
	cloudID := strings.TrimSpace(claim.CloudID)
	vmid, err := strconv.ParseInt(cloudID, 10, 64)
	if err != nil || vmid <= 0 {
		return core.LeaseTarget{}, core.Exit(2, "proxmox lease claim has invalid VM identity for lease=%s", claim.LeaseID)
	}
	if server, err := client.GetServer(ctx, cloudID); err == nil {
		if !core.IsCrabboxProxmoxLease(server) || strings.TrimSpace(server.Labels["lease"]) != claim.LeaseID {
			return core.LeaseTarget{}, core.Exit(2, "refusing to release Proxmox VM %s from stale local claim lease=%s", cloudID, claim.LeaseID)
		}
		return core.LeaseTarget{LeaseID: claim.LeaseID, Server: server}, nil
	}
	claimScope := strings.TrimSpace(claim.ProviderScope)
	currentScope := strings.TrimSpace(core.ProviderClaimScope("proxmox", b.Cfg))
	if claimScope == "" || currentScope == "" || claimScope != currentScope {
		return core.LeaseTarget{}, core.Exit(2, "refusing to accept missing Proxmox VM %s from lease=%s with unverified cluster scope", cloudID, claim.LeaseID)
	}
	clusterServers, err := client.ListCrabboxServersCluster(ctx)
	if err != nil {
		return core.LeaseTarget{}, fmt.Errorf("locate Proxmox VM %s across cluster: %w", cloudID, err)
	}
	for _, server := range clusterServers {
		if server.CloudID != cloudID {
			continue
		}
		if !core.IsCrabboxProxmoxLease(server) || strings.TrimSpace(server.Labels["lease"]) != claim.LeaseID {
			return core.LeaseTarget{}, core.Exit(2, "refusing to release Proxmox VM %s from stale local claim lease=%s", cloudID, claim.LeaseID)
		}
		return core.LeaseTarget{LeaseID: claim.LeaseID, Server: server}, nil
	}
	exists, err := client.VMExistsInCluster(ctx, cloudID)
	if err != nil {
		return core.LeaseTarget{}, fmt.Errorf("verify Proxmox VM %s cluster absence: %w", cloudID, err)
	}
	if exists {
		return core.LeaseTarget{}, core.Exit(2, "refusing to accept missing Proxmox VM %s from lease=%s because it still exists in the cluster", cloudID, claim.LeaseID)
	}
	labels := shared.CloneLabels(claim.Labels)
	if leaseLabel := strings.TrimSpace(labels["lease"]); leaseLabel != "" && leaseLabel != claim.LeaseID {
		return core.LeaseTarget{}, core.Exit(2, "proxmox lease claim label mismatch for lease=%s", claim.LeaseID)
	}
	if providerLabel := strings.TrimSpace(labels["provider"]); providerLabel != "" && providerLabel != "proxmox" {
		return core.LeaseTarget{}, core.Exit(2, "proxmox lease claim provider label mismatch for lease=%s", claim.LeaseID)
	}
	labels["lease"] = claim.LeaseID
	labels["provider"] = "proxmox"
	return core.LeaseTarget{
		LeaseID: claim.LeaseID,
		Server: core.Server{
			CloudID:  cloudID,
			Provider: "proxmox",
			HostID:   proxmoxReleaseAbsentMarker,
			ID:       vmid,
			Name:     claim.Slug,
			Labels:   labels,
		},
	}, nil
}

func (b *leaseBackend) resolveNumericClaim(cloudID string) (core.LeaseClaim, bool, error) {
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return core.LeaseClaim{}, false, err
	}
	currentScope := strings.TrimSpace(core.ProviderClaimScope("proxmox", b.Cfg))
	var scoped, legacy []core.LeaseClaim
	for _, claim := range claims {
		if claim.Provider != "proxmox" || strings.TrimSpace(claim.CloudID) != cloudID {
			continue
		}
		scope := strings.TrimSpace(claim.ProviderScope)
		switch {
		case scope != "" && scope == currentScope:
			scoped = append(scoped, claim)
		case scope == "":
			legacy = append(legacy, claim)
		}
	}
	if len(scoped) > 1 {
		return core.LeaseClaim{}, false, core.Exit(2, "multiple provider=proxmox claims in the current scope match cloud id %s", cloudID)
	}
	if len(scoped) == 1 {
		return scoped[0], true, nil
	}
	if len(legacy) > 1 {
		return core.LeaseClaim{}, false, core.Exit(2, "multiple unscoped provider=proxmox claims match cloud id %s", cloudID)
	}
	if len(legacy) == 1 {
		return legacy[0], true, nil
	}
	return core.LeaseClaim{}, false, nil
}

func (b *leaseBackend) targetForServer(server core.Server, releaseOnly bool) (core.LeaseTarget, error) {
	cfg := b.Cfg
	target := core.SSHTargetFromConfig(cfg, server.PublicNet.IPv4.IP)
	leaseID := core.Blank(server.Labels["lease"], server.CloudID)
	if !releaseOnly {
		if err := core.UseStoredTestboxKey(&target, leaseID); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	return core.LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *leaseBackend) List(ctx context.Context, req core.ListRequest) ([]core.LeaseView, error) {
	_ = req
	client, err := newClient(b.Cfg)
	if err != nil {
		return nil, err
	}
	return client.ListCrabboxServersCluster(ctx)
}

func (b *leaseBackend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	client, err := newClient(b.Cfg)
	if err != nil {
		return core.DoctorResult{}, err
	}
	checks, err := client.DoctorReadiness(ctx, b.Cfg)
	if err != nil {
		return core.DoctorResult{}, err
	}
	result := core.DoctorResult{Provider: "proxmox", Checks: make([]core.DoctorCheck, 0, len(checks))}
	for _, check := range checks {
		result.Checks = append(result.Checks, core.DoctorCheck{
			Status:  check.Status,
			Check:   check.Check,
			Message: check.Message,
			Details: check.Details,
		})
	}
	return result, nil
}

func (b *leaseBackend) ReleaseLease(ctx context.Context, req core.ReleaseLeaseRequest) error {
	client, err := newClient(b.Cfg)
	if err != nil {
		return err
	}
	id := req.Lease.Server.CloudID
	if id == "" {
		id = req.Lease.LeaseID
	}
	leaseID := proxmoxClaimLeaseID(req.Lease.Server, req.Lease.LeaseID)
	if req.Lease.Server.HostID != proxmoxReleaseAbsentMarker {
		if err := b.backfillReleaseClaimScope(leaseID, id, req.Lease.Server); err != nil {
			return err
		}
		node := core.Blank(req.Lease.Server.HostID, b.Cfg.Proxmox.Node)
		if err := client.DeleteServerOnNode(ctx, node, id); err != nil && !core.IsProxmoxNotFound(err) {
			return err
		}
	}
	remaining, err := client.ListCrabboxServersCluster(ctx)
	if err != nil {
		fmt.Fprintf(b.RT.Stderr, "warning: preserve local lease residue lease=%s reason=inventory_refresh_failed error=%v\n", leaseID, err)
		return fmt.Errorf("reconcile Proxmox lease after release: %w", err)
	}
	deleted := req.Lease.Server
	deleted.CloudID = id
	if deleted.Labels == nil {
		deleted.Labels = map[string]string{}
	}
	if deleted.Labels["lease"] == "" {
		deleted.Labels["lease"] = leaseID
	}
	return removeCleanupLeaseResidue(ctx, client, deleted, remaining, b.Cfg, b.RT.Stderr)
}

func (b *leaseBackend) backfillReleaseClaimScope(leaseID, cloudID string, server core.Server) error {
	if leaseID == "" || proxmoxClaimLabelLeaseID(server) != leaseID {
		return nil
	}
	claim, found, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		return err
	}
	if !found || strings.TrimSpace(claim.ProviderScope) != "" {
		return nil
	}
	if claim.Provider != "proxmox" || (claim.CloudID != "" && claim.CloudID != cloudID) {
		return nil
	}
	scope := strings.TrimSpace(core.ProviderClaimScope("proxmox", b.Cfg))
	if scope == "" {
		return core.Exit(2, "cannot safely release legacy Proxmox claim lease=%s without configured cluster scope", leaseID)
	}
	replacement := claim
	replacement.ProviderScope = scope
	if replacement.CloudID == "" {
		replacement.CloudID = cloudID
	}
	return core.ReplaceLeaseClaimIfUnchanged(leaseID, claim, replacement)
}

func (b *leaseBackend) Touch(ctx context.Context, req core.TouchRequest) (core.Server, error) {
	client, err := newClient(b.Cfg)
	if err != nil {
		return core.Server{}, err
	}
	server := req.Lease.Server
	server.Labels = core.TouchDirectLeaseLabels(server.Labels, b.Cfg, req.State, time.Now().UTC())
	node := core.Blank(server.HostID, b.Cfg.Proxmox.Node)
	if err := client.SetLabelsOnNode(ctx, node, server.CloudID, server.Labels); err != nil {
		return core.Server{}, err
	}
	return server, nil
}

func (b *leaseBackend) Cleanup(ctx context.Context, req core.CleanupRequest) error {
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return err
	}
	client, err := newClient(b.Cfg)
	if err != nil {
		return err
	}
	servers, err := client.ListCrabboxServersCluster(ctx)
	if err != nil {
		return err
	}
	for _, server := range servers {
		claim, binding, err := b.cleanupClaim(server, servers, claims)
		if err != nil {
			fmt.Fprintf(b.RT.Stderr, "skip server id=%s name=%s reason=%v\n", server.DisplayID(), server.Name, err)
			continue
		}
		if eligible, reason := core.ShouldCleanupServer(server, time.Now().UTC()); !eligible {
			fmt.Fprintf(b.RT.Stderr, "skip server id=%s name=%s reason=%s\n", server.DisplayID(), server.Name, reason)
			continue
		}
		if req.DryRun {
			fmt.Fprintf(b.RT.Stderr, "would delete server id=%s name=%s\n", server.DisplayID(), server.Name)
			continue
		}
		if err := b.cleanupClaimedServer(ctx, client, server, claim, binding); err != nil {
			return err
		}
		// Acquisition creates keys before publishing claims; this claim lock
		// cannot fence a concurrent key creator, so cleanup keeps local keys.
		fmt.Fprintf(b.RT.Stderr, "delete server id=%s name=%s key_retained=true\n", server.DisplayID(), server.Name)
	}
	return nil
}

func (b *leaseBackend) cleanupClaimedServer(ctx context.Context, client proxmoxClient, server core.Server, claim core.LeaseClaim, binding shared.ClaimBinding) error {
	var deleteErr error
	err := shared.RemoveExactClaimAfter(claim, binding, func() error {
		// Inventory is discovery only. Revalidate its node, lifecycle and native
		// generation while the same claim revision is fenced through removal.
		currentClaims, err := core.ListLeaseClaims()
		if err != nil {
			return err
		}
		current, err := client.ListCrabboxServersCluster(ctx)
		if err != nil {
			return err
		}
		var fresh core.Server
		for _, candidate := range current {
			if candidate.CloudID == server.CloudID {
				fresh = candidate
				break
			}
		}
		if fresh.CloudID == "" {
			return fmt.Errorf("Proxmox VM %s disappeared during cleanup; claim retained", server.CloudID)
		}
		if _, _, err := b.cleanupClaim(fresh, current, currentClaims); err != nil {
			return err
		}
		check := func(live core.Server) error {
			if live.HostID != fresh.HostID {
				return fmt.Errorf("Proxmox VM %s changed node during cleanup", server.CloudID)
			}
			if err := validateCleanupServer(live, claim, binding); err != nil {
				return err
			}
			if eligible, reason := core.ShouldCleanupServer(live, time.Now().UTC()); !eligible {
				return fmt.Errorf("Proxmox VM %s no longer eligible: %s", server.CloudID, reason)
			}
			return nil
		}
		if err := check(fresh); err != nil {
			return err
		}
		deleteErr = client.DeleteServerOnNodeChecked(ctx, fresh.HostID, fresh.CloudID, check)
		if deleteErr != nil {
			// Only an accepted/ambiguous purge may be reconciled as success;
			// failed authorization or stop must preserve the original claim.
			if !core.IsProxmoxDeleteTaskError(deleteErr) && !core.IsProxmoxDeleteRequestError(deleteErr) {
				return deleteErr
			}
			if err := waitForProxmoxCleanupAbsence(ctx, client, fresh.CloudID); err != nil {
				return err
			}
		}
		remaining, err := client.ListCrabboxServersCluster(ctx)
		if err != nil {
			return fmt.Errorf("verify Proxmox cleanup inventory: %w", err)
		}
		for _, survivor := range remaining {
			if survivor.CloudID == fresh.CloudID || proxmoxClaimLabelLeaseID(survivor) == claim.LeaseID {
				return fmt.Errorf("Proxmox lease %s still has a surviving VM; claim retained", claim.LeaseID)
			}
		}
		// The filtered Crabbox inventory cannot prove a VMID is absent.
		exists, err := client.VMExistsInCluster(ctx, fresh.CloudID)
		if err != nil {
			return fmt.Errorf("verify Proxmox cleanup absence: %w", err)
		}
		if exists {
			return fmt.Errorf("Proxmox VM %s still exists in cluster; claim retained", fresh.CloudID)
		}
		return nil
	})
	return errors.Join(deleteErr, err)
}

func waitForProxmoxCleanupAbsence(ctx context.Context, client proxmoxClient, cloudID string) error {
	verifyCtx, cancel := context.WithTimeout(ctx, proxmoxDeleteVerifyTimeout)
	defer cancel()
	_, err := shared.Poll(verifyCtx, 0, proxmoxDeleteVerifyPollInterval, shared.SleepContext,
		func(ctx context.Context) (bool, error) { return client.VMExistsInCluster(ctx, cloudID) },
		func(_ context.Context, exists bool, err error) (bool, error) { return !exists, err }, nil)
	return err
}

func removeCleanupLeaseResidue(ctx context.Context, client proxmoxClient, deleted core.Server, inventory []core.Server, cfg core.Config, stderr io.Writer) error {
	leaseID := proxmoxClaimLabelLeaseID(deleted)
	if leaseID == "" {
		return nil
	}
	missingCloudIDs := map[string]bool{deleted.CloudID: true}
	var survivors []core.Server
	for _, server := range inventory {
		if proxmoxClaimLabelLeaseID(server) == leaseID {
			survivors = append(survivors, server)
		}
	}
	claim, found, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		fmt.Fprintf(stderr, "warning: preserve local lease residue lease=%s reason=claim_read_failed error=%v\n", leaseID, err)
		return nil
	}
	if found && claim.Provider == "proxmox" {
		claimScope := strings.TrimSpace(claim.ProviderScope)
		currentScope := strings.TrimSpace(core.ProviderClaimScope("proxmox", cfg))
		if claimScope != currentScope && (claimScope != "" || currentScope != "") {
			fmt.Fprintf(stderr, "warning: preserve local lease residue lease=%s reason=claim_scope_mismatch\n", leaseID)
			return nil
		}
	}
	if len(survivors) == 1 {
		node := core.Blank(survivors[0].HostID, cfg.Proxmox.Node)
		verified, err := client.GetServerOnNode(ctx, node, survivors[0].CloudID)
		if err == nil {
			if proxmoxClaimLabelLeaseID(verified) != leaseID {
				fmt.Fprintf(stderr, "warning: preserve local lease residue lease=%s reason=survivor_ownership_unverified\n", leaseID)
				return fmt.Errorf("Proxmox lease %s has an unverified surviving VM", leaseID)
			}
			survivors[0] = verified
		} else if core.IsProxmoxNotFound(err) {
			missingCloudIDs[survivors[0].CloudID] = true
			survivors = nil
		} else {
			fmt.Fprintf(stderr, "warning: preserve local lease residue lease=%s reason=survivor_verification_failed error=%v\n", leaseID, err)
			return fmt.Errorf("verify surviving Proxmox VM for lease %s: %w", leaseID, err)
		}
	}
	if len(survivors) > 0 {
		if found && len(survivors) == 1 && claim.Provider == "proxmox" {
			canRetarget := claim.CloudID == "" || claim.CloudID == deleted.CloudID || claim.CloudID == survivors[0].CloudID
			if !canRetarget {
				if exists, err := client.VMExistsInCluster(ctx, claim.CloudID); err == nil && !exists {
					canRetarget = true
				} else if err != nil {
					fmt.Fprintf(stderr, "warning: preserve local lease residue lease=%s reason=claim_cloud_verification_failed error=%v\n", leaseID, err)
					return fmt.Errorf("verify claimed Proxmox VM for lease %s: %w", leaseID, err)
				}
			}
			if canRetarget {
				target := core.SSHTargetFromConfig(cfg, survivors[0].PublicNet.IPv4.IP)
				if target.Port == "" && claim.SSHPort > 0 {
					target.Port = strconv.Itoa(claim.SSHPort)
				}
				if _, err := core.ReplaceLeaseClaimEndpointIfUnchangedWithProviderMetadata(leaseID, claim, survivors[0], target); err != nil {
					fmt.Fprintf(stderr, "warning: preserve local lease residue lease=%s reason=claim_retarget_failed error=%v\n", leaseID, err)
					return fmt.Errorf("retarget Proxmox lease %s to surviving VM: %w", leaseID, err)
				}
			}
		}
		fmt.Fprintf(stderr, "warning: preserve local lease residue lease=%s reason=duplicate_remote_lease_label\n", leaseID)
		return fmt.Errorf("Proxmox lease %s still has %d surviving VM(s)", leaseID, len(survivors))
	}
	if !found {
		core.RemoveStoredTestboxKey(leaseID)
		return nil
	}
	if claim.Provider != "proxmox" {
		fmt.Fprintf(stderr, "warning: preserve local lease residue lease=%s reason=claim_cloud_mismatch\n", leaseID)
		return nil
	}
	if claim.CloudID != "" && !missingCloudIDs[claim.CloudID] {
		if exists, err := client.VMExistsInCluster(ctx, claim.CloudID); err == nil && exists {
			fmt.Fprintf(stderr, "warning: preserve local lease residue lease=%s reason=claim_cloud_still_exists\n", leaseID)
			return nil
		} else if err == nil {
			missingCloudIDs[claim.CloudID] = true
		} else {
			fmt.Fprintf(stderr, "warning: preserve local lease residue lease=%s reason=claim_cloud_verification_failed error=%v\n", leaseID, err)
			return nil
		}
	}
	if err := core.RemoveLeaseClaimIfUnchanged(leaseID, claim); err != nil {
		fmt.Fprintf(stderr, "warning: preserve local lease residue lease=%s reason=claim_changed error=%v\n", leaseID, err)
		return nil
	}
	core.RemoveStoredTestboxKey(leaseID)
	return nil
}

func proxmoxClaimLeaseID(server core.Server, fallback string) string {
	if leaseID := proxmoxClaimLabelLeaseID(server); leaseID != "" {
		return leaseID
	}
	return strings.TrimSpace(fallback)
}

func proxmoxClaimLabelLeaseID(server core.Server) string {
	if server.Labels != nil {
		if leaseID := strings.TrimSpace(server.Labels["lease"]); leaseID != "" {
			return leaseID
		}
	}
	return ""
}

var newClient = func(cfg core.Config) (proxmoxClient, error) { return core.NewProxmoxClient(cfg) }

var waitForSSHReadyFunc = core.WaitForSSHReady

var proxmoxIPPollInterval = 2 * time.Second
var proxmoxDeleteVerifyPollInterval = time.Second
var proxmoxDeleteVerifyTimeout = 30 * time.Second

func removeLocalLeaseResidue(leaseID string) {
	core.RemoveLeaseClaim(leaseID)
	core.RemoveStoredTestboxKey(leaseID)
}
