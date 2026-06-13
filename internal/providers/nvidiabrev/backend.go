package nvidiabrev

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	brevAcquirePollInterval = 2 * time.Second
	brevAcquirePollTimeout  = 12 * time.Minute
	brevCreateRecoveryGrace = 30 * time.Minute
	brevDeletePollInterval  = 2 * time.Second
	brevDeletePollTimeout   = 12 * time.Minute
	brevWorkspaceNameMaxLen = 63
)

var normalizeBrevSlugPattern = regexp.MustCompile(`[^a-z0-9]+`)

type brevOrgChangedError struct {
	beforeID string
	afterID  string
}

func (e *brevOrgChangedError) Error() string {
	return "active Brev organization changed while resolving workspace"
}

type nvidiaBrevBackend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func NewNvidiaBrevBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	applyNvidiaBrevDefaults(&cfg)
	cfg.Provider = providerName
	return &nvidiaBrevBackend{spec: spec, cfg: cfg, rt: rt}
}

func (b *nvidiaBrevBackend) Spec() ProviderSpec { return b.spec }

func (b *nvidiaBrevBackend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	client, err := b.client()
	if err != nil {
		return LeaseTarget{}, err
	}
	cfg := b.configForRun()
	leaseID := newLeaseID()
	existing, err := b.listServers(ctx, client, true)
	if err != nil {
		return LeaseTarget{}, err
	}
	claims, err := listLeaseClaims()
	if err != nil {
		return LeaseTarget{}, err
	}
	createOrg, err := client.activeOrg(ctx)
	if err != nil {
		return LeaseTarget{}, err
	}
	slug := allocateBrevLeaseSlug(leaseID, req.RequestedSlug, existing, claims)
	name := brevProviderName(leaseID, slug)
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s name=%s gpu=%s keep=%v\n", providerName, leaseID, slug, name, cfg.NvidiaBrev.GPUName, req.Keep)

	if err := client.rejectOrgScopedMutation("create"); err != nil {
		return LeaseTarget{}, err
	}
	if err := client.create(ctx, name); err != nil {
		return LeaseTarget{}, b.reconcileCreateFailure(client, brevWorkspace{Name: name}, createOrg.ID, leaseID, slug, cfg, req, err)
	}
	workspace, workspaceOrgID, err := b.waitForWorkspaceReady(ctx, client, name)
	if err != nil {
		var orgChanged *brevOrgChangedError
		if errors.As(err, &orgChanged) {
			err = b.retainAmbiguousCreateOrganizationClaim(workspace, createOrg.ID, orgChanged.beforeID, orgChanged.afterID, leaseID, slug, cfg, req, err)
		} else if req.Keep {
			if workspaceIdentifier(workspace) == "" {
				workspace.Name = name
			}
			err = b.retainFailedCreatedWorkspace(workspace, firstNonEmpty(workspaceOrgID, createOrg.ID), leaseID, slug, cfg, req, "kept_acquire_failed", err)
		} else {
			if workspaceIdentifier(workspace) == "" {
				workspace.Name = name
			}
			err = b.rollbackOrRetainCreatedWorkspace(workspace, createOrg.ID, leaseID, slug, cfg, req, err)
		}
		return LeaseTarget{}, err
	}
	if workspaceOrgID != createOrg.ID {
		var err error = exit(2, "active Brev organization changed while creating workspace %s", safeWorkspaceRef(workspace))
		err = b.retainAmbiguousCreateOrganizationClaim(workspace, createOrg.ID, workspaceOrgID, workspaceOrgID, leaseID, slug, cfg, req, err)
		return LeaseTarget{}, err
	}
	lease, err := b.prepareLease(ctx, client, cfg, workspace, leaseID, slug, req.Keep, true)
	if err != nil {
		if req.Keep {
			err = b.retainFailedCreatedWorkspace(workspace, createOrg.ID, leaseID, slug, cfg, req, "kept_acquire_failed", err)
		} else {
			err = b.rollbackOrRetainCreatedWorkspace(workspace, createOrg.ID, leaseID, slug, cfg, req, err)
		}
		return LeaseTarget{}, err
	}
	lease.Server.Labels["brev_org_id"] = workspaceOrgID
	if err := persistLeaseTargetForRepoConfig(leaseID, slug, cfg, lease.Server, lease.SSH, req.Repo.Root, req.Reclaim); err != nil {
		if req.Keep {
			err = b.retainFailedCreatedWorkspace(workspace, createOrg.ID, leaseID, slug, cfg, req, "kept_acquire_failed", err)
		} else {
			err = b.rollbackOrRetainCreatedWorkspace(workspace, createOrg.ID, leaseID, slug, cfg, req, err)
		}
		return LeaseTarget{}, err
	}
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s workspace=%s state=ready\n", leaseID, safeWorkspaceRef(workspace))
	return lease, nil
}

func (b *nvidiaBrevBackend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	client, err := b.client()
	if err != nil {
		return LeaseTarget{}, err
	}
	cfg := b.configForRun()
	claim, claimed, claimErr := resolveNvidiaBrevClaim(req.ID)
	if claimErr != nil {
		return LeaseTarget{}, claimErr
	}
	if claimed && strings.EqualFold(strings.TrimSpace(claim.Labels["state"]), "deleting") {
		if req.ReleaseOnly || req.StatusOnly {
			return deletingLeaseTarget(claim), nil
		}
		return LeaseTarget{}, exit(4, "nvidia-brev lease=%s is deleting", claim.LeaseID)
	}
	if claimed && req.StatusOnly && createRecoveryClaim(claim) {
		workspaces, err := client.list(ctx, true)
		if err != nil {
			return LeaseTarget{}, err
		}
		_, found, err := findBrevWorkspace(workspaces, firstNonEmpty(claim.CloudID, claim.Labels["brev_workspace_name"]))
		if err != nil {
			return LeaseTarget{}, err
		}
		if !found {
			return claimStateLeaseTarget(claim), nil
		}
	}
	if claimed && req.ReleaseOnly && createRecoveryClaim(claim) {
		return claimStateLeaseTarget(claim), nil
	}
	if claimed && strings.TrimSpace(cfg.NvidiaBrev.Org) == "" && strings.TrimSpace(claim.Labels["brev_org_id"]) != "" {
		if _, err := verifyActiveBrevOrgScope(ctx, client, claim); err != nil {
			return LeaseTarget{}, err
		}
	}
	workspace, leaseID, slug, claim, err := b.resolveWorkspace(ctx, client, req.ID)
	if err != nil {
		return LeaseTarget{}, err
	}
	lease := LeaseTarget{Server: workspaceToClaimedServer(cfg, workspace, leaseID, slug, claim), LeaseID: leaseID}
	activeOrgID := ""
	if strings.TrimSpace(cfg.NvidiaBrev.Org) == "" && !req.ReleaseOnly {
		activeOrgID, err = verifyActiveBrevOrgScope(ctx, client, claim)
		if err != nil {
			return LeaseTarget{}, err
		}
		lease.Server.Labels["brev_org_id"] = activeOrgID
	}
	if req.ReleaseOnly || req.StatusOnly {
		if req.ReadyProbe && brevWorkspaceReady(workspace) {
			target, targetErr := b.resolveSSHTarget(ctx, client, cfg, workspace)
			if targetErr != nil {
				return LeaseTarget{}, targetErr
			}
			lease.SSH = target
		}
		return lease, nil
	}
	if brevWorkspaceStopped(workspace) {
		if claim.LeaseID != "" {
			starting := workspace
			starting.Status = "STARTING"
			startingServer := workspaceToClaimedServer(cfg, starting, leaseID, slug, claim)
			startingServer.Labels["brev_org_id"] = activeOrgID
			updatedClaim, updateErr := updateLeaseClaimEndpointIfUnchangedAfter(leaseID, claim, startingServer, SSHTarget{}, func() error {
				if err := requireActiveBrevOrg(ctx, client, activeOrgID); err != nil {
					return err
				}
				fmt.Fprintf(b.rt.Stderr, "starting provider=%s workspace=%s\n", providerName, safeWorkspaceRef(workspace))
				return client.start(ctx, workspaceIdentifier(workspace))
			})
			if updateErr != nil {
				return LeaseTarget{}, updateErr
			}
			claim = updatedClaim
			workspace, _, err = b.waitForWorkspaceReady(ctx, client, workspaceIdentifier(workspace))
			if err != nil {
				return LeaseTarget{}, err
			}
		} else {
			workspace, err = b.startStoppedWorkspace(ctx, client, workspace, activeOrgID)
			if err != nil {
				return LeaseTarget{}, err
			}
		}
		lease.Server = workspaceToClaimedServer(cfg, workspace, leaseID, slug, claim)
		lease.Server.Labels["brev_org_id"] = activeOrgID
	}
	target, err := b.resolveSSHTarget(ctx, client, cfg, workspace)
	if err != nil {
		return LeaseTarget{}, err
	}
	lease.SSH = target
	if req.Repo.Root != "" && isCrabboxBrevWorkspace(workspace) {
		updatedClaim, claimErr := claimLeaseTargetForRepoConfigIfUnchanged(leaseID, slug, cfg, lease.Server, lease.SSH, req.Repo.Root, req.Reclaim, claim, claim.LeaseID != "")
		if claimErr != nil {
			return LeaseTarget{}, claimErr
		}
		claim = updatedClaim
	} else if claim.LeaseID != "" {
		updatedClaim, claimErr := updateLeaseClaimEndpointIfUnchanged(leaseID, claim, lease.Server, lease.SSH)
		if claimErr != nil {
			return LeaseTarget{}, claimErr
		}
		claim = updatedClaim
	}
	return lease, nil
}

func (b *nvidiaBrevBackend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	client, err := b.client()
	if err != nil {
		return nil, err
	}
	return b.listServers(ctx, client, req.All)
}

func (b *nvidiaBrevBackend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	client, err := b.client()
	if err != nil {
		return err
	}
	if err := client.rejectOrgScopedMutation("release"); err != nil {
		return err
	}
	identifier := firstNonEmpty(req.Lease.LeaseID, req.Lease.Server.CloudID, req.Lease.Server.Name)
	if claim, claimed, claimErr := resolveNvidiaBrevClaim(identifier); claimErr != nil {
		return claimErr
	} else if claimed && strings.EqualFold(strings.TrimSpace(claim.Labels["state"]), "deleting") {
		return b.reconcileDeletingClaim(ctx, client, claim)
	} else if claimed && createRecoveryClaim(claim) {
		return b.reconcileCreateRecoveryClaim(ctx, client, claim)
	} else if claimed && strings.TrimSpace(claim.Labels["brev_org_id"]) != "" {
		if _, err := verifyActiveBrevOrgScope(ctx, client, claim); err != nil {
			return err
		}
	}
	workspace, _, _, claim, err := b.resolveWorkspace(ctx, client, identifier)
	if err != nil {
		return err
	}
	if claim.LeaseID == "" {
		return exit(2, "refusing to release nvidia-brev workspace %s without a local Crabbox claim", safeWorkspaceRef(workspace))
	}
	if claim.Provider != providerName {
		return exit(2, "lease=%s is claimed by provider=%s; refusing nvidia-brev release", claim.LeaseID, claim.Provider)
	}
	if !claimMatchesWorkspace(claim, workspace) {
		return exit(2, "lease=%s claim does not match nvidia-brev workspace %s", claim.LeaseID, safeWorkspaceRef(workspace))
	}
	action := b.releaseAction(claim.Labels)
	if action == "stop" {
		return b.stopWorkspaceAndPersistClaim(ctx, client, workspace, claim)
	}
	return b.deleteWorkspaceAndRemoveClaim(ctx, client, workspace, claim)
}

func (b *nvidiaBrevBackend) RetainLeaseClaimAfterRelease(lease LeaseTarget) bool {
	if b.releaseResultAction(lease.Server.Labels) != "stop" {
		return false
	}
	if createRecoveryLabels(lease.Server.Labels) && lease.LeaseID != "" {
		if _, retained, err := resolveLeaseClaimForProvider(lease.LeaseID); err == nil && !retained {
			return false
		}
	}
	return true
}

func (b *nvidiaBrevBackend) ReleaseLeaseMessage(lease LeaseTarget) string {
	workspace := firstNonEmpty(lease.Server.CloudID, lease.Server.Name, "-")
	if b.releaseResultAction(lease.Server.Labels) == "stop" {
		if createRecoveryLabels(lease.Server.Labels) && lease.LeaseID != "" {
			if _, retained, err := resolveLeaseClaimForProvider(lease.LeaseID); err == nil && !retained {
				return fmt.Sprintf("removed stale lease=%s workspace=%s absent=true", lease.LeaseID, workspace)
			}
		}
		return fmt.Sprintf("stopped lease=%s workspace=%s retained=true", lease.LeaseID, workspace)
	}
	return fmt.Sprintf("deleted lease=%s workspace=%s", lease.LeaseID, workspace)
}

func (b *nvidiaBrevBackend) Touch(_ context.Context, req TouchRequest) (Server, error) {
	server := req.Lease.Server
	cfg := b.configForRun()
	if req.IdleTimeout > 0 {
		cfg.IdleTimeout = req.IdleTimeout
	}
	var claim LeaseClaim
	var claimed bool
	var err error
	if req.Lease.LeaseID != "" {
		claim, claimed, err = resolveLeaseClaimForProvider(req.Lease.LeaseID)
		if err != nil {
			return server, err
		}
		if claimed {
			switch state := strings.ToLower(strings.TrimSpace(claim.Labels["state"])); state {
			case "stopped", "deleting":
				return server, exit(4, "nvidia-brev lease=%s is %s", claim.LeaseID, state)
			}
			server = serverWithClaimLabels(server, claim)
		}
	}
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	server.Labels = touchDirectLeaseLabels(server.Labels, cfg, req.State)
	if strings.TrimSpace(req.State) != "" {
		server.Status = strings.TrimSpace(req.State)
	} else if state := strings.TrimSpace(server.Labels["state"]); state != "" {
		server.Status = state
	}
	if claimed && claim.RepoRoot != "" {
		if _, err := claimLeaseTargetForRepoConfigIfUnchanged(claim.LeaseID, claim.Slug, cfg, server, req.Lease.SSH, claim.RepoRoot, false, claim, true); err != nil {
			return server, err
		}
	}
	return server, nil
}

func (b *nvidiaBrevBackend) Cleanup(ctx context.Context, req CleanupRequest) error {
	client, err := b.client()
	if err != nil {
		return err
	}
	if !req.DryRun {
		if err := client.rejectOrgScopedMutation("cleanup"); err != nil {
			return err
		}
	}
	workspaces, err := client.list(ctx, true)
	if err != nil {
		return err
	}
	var errs []error
	handledDeletingWorkspaces := map[string]struct{}{}
	claims, err := listLeaseClaims()
	if err != nil {
		return err
	}
	for _, claim := range claims {
		if claim.Provider != providerName || !strings.EqualFold(strings.TrimSpace(claim.Labels["state"]), "deleting") {
			continue
		}
		workspace := brevWorkspace{
			ID:     claim.CloudID,
			Name:   claim.Labels["brev_workspace_name"],
			Status: "DELETING",
		}
		handledDeletingWorkspaces[brevWorkspaceKey(workspace)] = struct{}{}
		identifier := workspaceIdentifier(workspace)
		if identifier == "" {
			errs = append(errs, exit(2, "nvidia-brev deleting lease=%s has no workspace id or name", claim.LeaseID))
			continue
		}
		if req.DryRun {
			var current brevWorkspace
			var found bool
			var findErr error
			if strings.TrimSpace(b.configForRun().NvidiaBrev.Org) != "" {
				current, found, findErr = findBrevWorkspace(workspaces, identifier)
			} else {
				current, found, findErr = b.deletingClaimWorkspace(ctx, client, claim)
			}
			if findErr != nil {
				errs = append(errs, findErr)
				continue
			}
			if found {
				workspace = current
			}
			fmt.Fprintf(b.rt.Stderr, "would reconcile provider=%s lease=%s slug=%s workspace=%s state=deleting present=%v\n", providerName, claim.LeaseID, claim.Slug, safeWorkspaceRef(workspace), found)
			continue
		}
		if err := b.reconcileDeletingClaim(ctx, client, claim); err != nil {
			errs = append(errs, err)
		}
	}
	for _, workspace := range workspaces {
		if _, handled := handledDeletingWorkspaces[brevWorkspaceKey(workspace)]; handled {
			continue
		}
		claim, claimed, err := resolveLeaseClaimForProviderCloudID(workspace.ID)
		if err != nil {
			return err
		}
		nameOwned := isCrabboxBrevWorkspace(workspace)
		if !claimed && !nameOwned {
			fmt.Fprintf(b.rt.Stderr, "skip provider=%s workspace=%s reason=not-crabbox-owned\n", providerName, safeWorkspaceRef(workspace))
			continue
		}
		leaseID, slug := brevLeaseIdentity(workspace, claim)
		action := b.releaseAction(claim.Labels)
		if claimed && claim.Provider == providerName && claimMatchesWorkspace(claim, workspace) && action == "stop" && brevWorkspaceStopped(workspace) {
			if strings.EqualFold(strings.TrimSpace(claim.Labels["state"]), "stopped") && claim.SSHHost == "" && claim.SSHPort == 0 {
				fmt.Fprintf(b.rt.Stderr, "skip provider=%s lease=%s workspace=%s reason=stopped\n", providerName, leaseID, safeWorkspaceRef(workspace))
				continue
			}
			if req.DryRun {
				fmt.Fprintf(b.rt.Stderr, "would reconcile provider=%s lease=%s slug=%s workspace=%s state=stopped\n", providerName, leaseID, slug, safeWorkspaceRef(workspace))
				continue
			}
			if err := b.persistStoppedClaim(workspace, claim, nil); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		eligible, reason := b.cleanupEligible(workspace, claim, claimed)
		if !eligible {
			fmt.Fprintf(b.rt.Stderr, "skip provider=%s lease=%s workspace=%s reason=%s\n", providerName, leaseID, safeWorkspaceRef(workspace), reason)
			continue
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stderr, "would release provider=%s lease=%s slug=%s workspace=%s action=%s\n", providerName, leaseID, slug, safeWorkspaceRef(workspace), action)
			continue
		}
		if action == "stop" {
			if err := b.stopWorkspaceAndPersistClaim(ctx, client, workspace, claim); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if err := b.deleteWorkspaceAndRemoveClaim(ctx, client, workspace, claim); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (b *nvidiaBrevBackend) cleanupEligible(workspace brevWorkspace, claim LeaseClaim, claimed bool) (bool, string) {
	if !claimed {
		return false, "no-local-cleanup-claim"
	}
	if claim.Provider != providerName || !claimMatchesWorkspace(claim, workspace) {
		return false, "claim-mismatch"
	}
	if strings.EqualFold(strings.TrimSpace(claim.Labels["keep"]), "true") {
		return false, "keep"
	}
	if expiresAt := strings.TrimSpace(claim.Labels["expires_at"]); expiresAt != "" {
		if expires, ok := parseBrevClaimTime(expiresAt); ok && time.Now().After(expires) {
			return true, "expired-ttl"
		}
	}
	if claim.IdleTimeoutSeconds <= 0 || strings.TrimSpace(claim.LastUsedAt) == "" {
		return false, "active-claim"
	}
	lastUsed, ok := parseBrevClaimTime(claim.LastUsedAt)
	if !ok {
		return false, "active-claim"
	}
	if time.Since(lastUsed) <= time.Duration(claim.IdleTimeoutSeconds)*time.Second {
		return false, "active-claim"
	}
	return true, "expired"
}

func parseBrevClaimTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 {
		return time.Unix(seconds, 0).UTC(), true
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t.UTC(), true
	}
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}

func (b *nvidiaBrevBackend) stopWorkspaceAndPersistClaim(ctx context.Context, client *brevClient, workspace brevWorkspace, claim LeaseClaim) error {
	updated, err := b.ensureClaimActiveBrevOrgScope(ctx, client, workspace, claim)
	if err != nil {
		return err
	}
	claim = updated
	return b.persistStoppedClaim(workspace, claim, func() error {
		if err := requireActiveBrevOrg(ctx, client, claim.Labels["brev_org_id"]); err != nil {
			return err
		}
		return b.releaseWorkspace(ctx, client, workspace, "stop")
	})
}

func (b *nvidiaBrevBackend) deleteWorkspaceAndRemoveClaim(ctx context.Context, client *brevClient, workspace brevWorkspace, claim LeaseClaim) error {
	updatedClaim, err := b.ensureClaimActiveBrevOrgScope(ctx, client, workspace, claim)
	if err != nil {
		return err
	}
	claim = updatedClaim
	deleting := workspaceToClaimedServer(b.configForRun(), workspace, claim.LeaseID, claim.Slug, claim)
	deleting.Status = "deleting"
	deleting.Labels["state"] = "deleting"
	deleting.Labels["release"] = "delete"
	updated, err := updateLeaseClaimEndpointIfUnchanged(claim.LeaseID, claim, deleting, SSHTarget{})
	if err != nil {
		return err
	}
	if err := requireActiveBrevOrg(ctx, client, updated.Labels["brev_org_id"]); err != nil {
		return err
	}
	if err := b.releaseWorkspace(ctx, client, workspace, "delete"); err != nil {
		return err
	}
	return b.finishDeletingClaim(ctx, client, workspace, updated)
}

func (b *nvidiaBrevBackend) ensureClaimActiveBrevOrgScope(ctx context.Context, client *brevClient, workspace brevWorkspace, claim LeaseClaim) (LeaseClaim, error) {
	if strings.TrimSpace(claim.Labels["brev_org_id"]) != "" {
		_, err := verifyActiveBrevOrgScope(ctx, client, claim)
		if err != nil {
			return LeaseClaim{}, err
		}
		return claim, nil
	}
	active, err := client.activeOrg(ctx)
	if err != nil {
		return LeaseClaim{}, err
	}
	workspaces, err := client.list(ctx, true)
	if err != nil {
		return LeaseClaim{}, err
	}
	current, found, err := findBrevWorkspace(workspaces, workspaceIdentifier(workspace))
	if err != nil {
		return LeaseClaim{}, err
	}
	if !found || !claimMatchesWorkspace(claim, current) {
		return LeaseClaim{}, exit(2, "cannot safely bind nvidia-brev lease=%s to the active organization; local claim retained", claim.LeaseID)
	}
	after, err := client.activeOrg(ctx)
	if err != nil {
		return LeaseClaim{}, err
	}
	if active.ID != after.ID {
		return LeaseClaim{}, exit(2, "active Brev organization changed while validating lease scope; local claim retained")
	}
	server := workspaceToClaimedServer(b.configForRun(), current, claim.LeaseID, claim.Slug, claim)
	server.Labels["brev_org_id"] = active.ID
	updated, err := updateLeaseClaimEndpointIfUnchanged(claim.LeaseID, claim, server, SSHTarget{})
	if err != nil {
		return LeaseClaim{}, err
	}
	return updated, nil
}

func verifyActiveBrevOrgScope(ctx context.Context, client *brevClient, claim LeaseClaim) (string, error) {
	active, err := client.activeOrg(ctx)
	if err != nil {
		return "", err
	}
	stored := strings.TrimSpace(claim.Labels["brev_org_id"])
	if stored != "" && stored != active.ID {
		return "", exit(2, "active Brev organization changed; run `brev set` for the lease organization before lifecycle operations")
	}
	return active.ID, nil
}

func requireActiveBrevOrg(ctx context.Context, client *brevClient, orgID string) error {
	active, err := client.activeOrg(ctx)
	if err != nil {
		return err
	}
	if strings.TrimSpace(orgID) == "" || active.ID != strings.TrimSpace(orgID) {
		return exit(2, "active Brev organization changed; local deletion claim retained. Run `brev set` for the lease organization before retrying")
	}
	return nil
}

func (b *nvidiaBrevBackend) deletingClaimWorkspace(ctx context.Context, client *brevClient, claim LeaseClaim) (brevWorkspace, bool, error) {
	orgID := strings.TrimSpace(claim.Labels["brev_org_id"])
	if orgID == "" {
		return brevWorkspace{}, false, exit(2, "nvidia-brev deleting lease=%s has no organization scope; local claim retained", claim.LeaseID)
	}
	if err := requireActiveBrevOrg(ctx, client, orgID); err != nil {
		return brevWorkspace{}, false, err
	}
	workspaces, err := client.list(ctx, true)
	if err != nil {
		return brevWorkspace{}, false, err
	}
	workspace := brevWorkspace{ID: claim.CloudID, Name: claim.Labels["brev_workspace_name"]}
	current, found, err := findBrevWorkspace(workspaces, workspaceIdentifier(workspace))
	if err != nil {
		return brevWorkspace{}, false, err
	}
	if err := requireActiveBrevOrg(ctx, client, orgID); err != nil {
		return brevWorkspace{}, false, err
	}
	return current, found, nil
}

func (b *nvidiaBrevBackend) reconcileDeletingClaim(ctx context.Context, client *brevClient, claim LeaseClaim) error {
	if strings.EqualFold(strings.TrimSpace(claim.Labels["brev_recovery"]), "org_changed") {
		return exit(2, "nvidia-brev active organization changed during workspace creation; automatic deletion is unsafe and recovery claim for lease=%s was retained for manual reconciliation", claim.LeaseID)
	}
	workspace := brevWorkspace{
		ID:     claim.CloudID,
		Name:   claim.Labels["brev_workspace_name"],
		Status: claim.Labels["brev_status"],
	}
	if workspaceIdentifier(workspace) == "" {
		return exit(2, "nvidia-brev deleting lease=%s has no workspace id or name", claim.LeaseID)
	}
	current, found, err := b.deletingClaimWorkspace(ctx, client, claim)
	if err != nil {
		return err
	}
	if !found {
		if strings.EqualFold(strings.TrimSpace(claim.Labels["brev_recovery"]), "create_unknown") && strings.TrimSpace(claim.CloudID) == "" {
			createdAt, ok := parseBrevClaimTime(claim.Labels["created_at"])
			if !ok || time.Since(createdAt) < brevCreateRecoveryGrace {
				return exit(5, "nvidia-brev workspace for lease=%s has not appeared; ambiguous create recovery claim retained", claim.LeaseID)
			}
		}
		return removeLeaseClaimIfUnchanged(claim.LeaseID, claim)
	}
	if !strings.EqualFold(strings.TrimSpace(current.Status), "deleting") {
		if err := requireActiveBrevOrg(ctx, client, claim.Labels["brev_org_id"]); err != nil {
			return err
		}
		if err := b.releaseWorkspace(ctx, client, current, "delete"); err != nil {
			return err
		}
	}
	return b.finishDeletingClaim(ctx, client, current, claim)
}

func (b *nvidiaBrevBackend) finishDeletingClaim(ctx context.Context, client *brevClient, workspace brevWorkspace, claim LeaseClaim) error {
	if err := b.waitForWorkspaceDeleted(ctx, client, workspace, strings.TrimSpace(claim.Labels["brev_org_id"])); err != nil {
		return err
	}
	return removeLeaseClaimIfUnchanged(claim.LeaseID, claim)
}

func deletingLeaseTarget(claim LeaseClaim) LeaseTarget {
	return claimLeaseTargetWithStatus(claim, "deleting")
}

func claimStateLeaseTarget(claim LeaseClaim) LeaseTarget {
	return claimLeaseTargetWithStatus(claim, firstNonEmpty(claim.Labels["state"], "failed"))
}

func claimLeaseTargetWithStatus(claim LeaseClaim, status string) LeaseTarget {
	labels := make(map[string]string, len(claim.Labels))
	for key, value := range claim.Labels {
		labels[key] = value
	}
	server := Server{
		CloudID:  claim.CloudID,
		Provider: providerName,
		Name:     labels["brev_workspace_name"],
		Status:   status,
		Labels:   labels,
	}
	server.ServerType.Name = labels["server_type"]
	return LeaseTarget{LeaseID: claim.LeaseID, Server: server}
}

func createRecoveryClaim(claim LeaseClaim) bool {
	return createRecoveryLabels(claim.Labels)
}

func createRecoveryLabels(labels map[string]string) bool {
	switch strings.ToLower(strings.TrimSpace(labels["brev_recovery"])) {
	case "create_unknown", "kept_acquire_failed", "kept_create_failed":
		return true
	default:
		return false
	}
}

func (b *nvidiaBrevBackend) reconcileCreateRecoveryClaim(ctx context.Context, client *brevClient, claim LeaseClaim) error {
	current, found, err := b.deletingClaimWorkspace(ctx, client, claim)
	if err != nil {
		return err
	}
	if !found {
		createdAt, ok := parseBrevClaimTime(claim.Labels["created_at"])
		if !ok || time.Since(createdAt) < brevCreateRecoveryGrace {
			return exit(5, "nvidia-brev workspace for lease=%s has not appeared; create recovery claim retained", claim.LeaseID)
		}
		return removeLeaseClaimIfUnchanged(claim.LeaseID, claim)
	}
	if b.releaseAction(claim.Labels) == "stop" {
		return b.stopWorkspaceAndPersistClaim(ctx, client, current, claim)
	}
	return b.deleteWorkspaceAndRemoveClaim(ctx, client, current, claim)
}

func brevWorkspaceKey(workspace brevWorkspace) string {
	if workspace.ID != "" {
		return "id:" + workspace.ID
	}
	return "name:" + workspace.Name
}

func (b *nvidiaBrevBackend) waitForWorkspaceDeleted(ctx context.Context, client *brevClient, workspace brevWorkspace, orgID string) error {
	if orgID == "" {
		return exit(2, "nvidia-brev deleting lease has no organization scope; local claim retained")
	}
	id := workspaceIdentifier(workspace)
	deadline := time.Now().Add(brevDeletePollTimeout)
	if err := requireActiveBrevOrg(ctx, client, orgID); err != nil {
		return err
	}
	for {
		workspaces, err := client.list(ctx, true)
		if err != nil {
			return err
		}
		_, found, err := findBrevWorkspace(workspaces, id)
		if err != nil {
			return err
		}
		if !found {
			if err := requireActiveBrevOrg(ctx, client, orgID); err != nil {
				return err
			}
			return nil
		}
		if time.Now().After(deadline) {
			return exit(5, "timed out waiting for nvidia-brev workspace %s deletion; local claim retained", safeWorkspaceRef(workspace))
		}
		timer := time.NewTimer(brevDeletePollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (b *nvidiaBrevBackend) persistStoppedClaim(workspace brevWorkspace, claim LeaseClaim, action func() error) error {
	stopped := workspaceToClaimedServer(b.configForRun(), workspace, claim.LeaseID, claim.Slug, claim)
	stopped.Status = "stopped"
	stopped.Labels["state"] = "stopped"
	stopped.Labels["release"] = "stop"
	if _, err := updateLeaseClaimEndpointIfUnchangedAfter(claim.LeaseID, claim, stopped, SSHTarget{}, action); err != nil {
		return fmt.Errorf("persist stopped nvidia-brev lease claim: %w", err)
	}
	return nil
}

func (b *nvidiaBrevBackend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	client, err := b.client()
	if err != nil {
		return DoctorResult{}, err
	}
	if _, err := client.version(ctx); err != nil {
		return DoctorResult{}, exit(2, "nvidia-brev CLI check failed: %v", err)
	}
	workspaces, err := client.list(ctx, false)
	if err != nil {
		return DoctorResult{}, exit(1, "nvidia-brev auth/list check failed: %v", err)
	}
	return cliDoctorResult(providerName, len(workspaces), "unchecked"), nil
}

func (b *nvidiaBrevBackend) client() (*brevClient, error) {
	return newBrevClient(b.configForRun(), b.rt)
}

func (b *nvidiaBrevBackend) configForRun() Config {
	cfg := b.cfg
	applyNvidiaBrevDefaults(&cfg)
	return cfg
}

func applyNvidiaBrevDefaults(cfg *Config) {
	cfg.Provider = providerName
	if cfg.TargetOS == "" {
		cfg.TargetOS = targetLinux
	}
	if cfg.NvidiaBrev.CLI == "" {
		cfg.NvidiaBrev.CLI = "brev"
	}
	if cfg.NvidiaBrev.GPUName == "" {
		cfg.NvidiaBrev.GPUName = "A100"
	}
	if cfg.NvidiaBrev.Mode == "" {
		cfg.NvidiaBrev.Mode = "vm"
	}
	if cfg.NvidiaBrev.ReleaseAction == "" {
		cfg.NvidiaBrev.ReleaseAction = "delete"
	}
	if cfg.NvidiaBrev.Target == "" {
		cfg.NvidiaBrev.Target = "container"
	}
	cfg.NvidiaBrev.WorkRoot = effectiveNvidiaBrevWorkRoot(*cfg)
	if cfg.NvidiaBrev.User != "" {
		cfg.SSHUser = cfg.NvidiaBrev.User
	}
	if cfg.NvidiaBrev.WorkRoot != "" {
		cfg.WorkRoot = cfg.NvidiaBrev.WorkRoot
	}
	cfg.SSHPort = ""
	cfg.SSHFallbackPorts = nil
}

func (b *nvidiaBrevBackend) waitForWorkspaceReady(ctx context.Context, client *brevClient, name string) (brevWorkspace, string, error) {
	deadline := time.Now().Add(brevAcquirePollTimeout)
	activeBefore, err := client.activeOrg(ctx)
	if err != nil {
		return brevWorkspace{}, "", err
	}
	var observed brevWorkspace
	observedOrgID := ""
	for {
		workspaces, err := client.list(ctx, true)
		if err != nil {
			return brevWorkspace{}, "", err
		}
		workspace, found, err := findBrevWorkspace(workspaces, name)
		if err != nil {
			return brevWorkspace{}, "", err
		}
		if found {
			observed = workspace
		}
		if found && observedOrgID == "" {
			activeAfter, err := client.activeOrg(ctx)
			if err != nil {
				return brevWorkspace{}, "", err
			}
			if activeBefore.ID != activeAfter.ID {
				return workspace, activeBefore.ID, &brevOrgChangedError{beforeID: activeBefore.ID, afterID: activeAfter.ID}
			}
			observedOrgID = activeAfter.ID
		}
		if found && brevWorkspaceReady(workspace) {
			activeAfter, err := client.activeOrg(ctx)
			if err != nil {
				return brevWorkspace{}, "", err
			}
			if observedOrgID != activeAfter.ID {
				return workspace, observedOrgID, &brevOrgChangedError{beforeID: observedOrgID, afterID: activeAfter.ID}
			}
			return workspace, activeAfter.ID, nil
		}
		if time.Now().After(deadline) {
			if found {
				activeAfter, orgErr := client.activeOrg(ctx)
				if orgErr != nil {
					return brevWorkspace{}, "", orgErr
				}
				if observedOrgID != "" && observedOrgID != activeAfter.ID {
					return workspace, observedOrgID, &brevOrgChangedError{beforeID: observedOrgID, afterID: activeAfter.ID}
				}
				return workspace, observedOrgID, exit(5, "timed out waiting for nvidia-brev workspace %s to become ready (status=%s build=%s shell=%s health=%s)", safeWorkspaceRef(workspace), workspace.Status, workspace.BuildStatus, workspace.ShellStatus, workspace.HealthStatus)
			}
			return observed, observedOrgID, exit(5, "timed out waiting for nvidia-brev workspace %q to appear", name)
		}
		timer := time.NewTimer(brevAcquirePollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return brevWorkspace{}, "", ctx.Err()
		case <-timer.C:
		}
	}
}

func (b *nvidiaBrevBackend) prepareLease(ctx context.Context, client *brevClient, cfg Config, workspace brevWorkspace, leaseID, slug string, keep, probeSSH bool) (LeaseTarget, error) {
	target, err := b.resolveSSHTarget(ctx, client, cfg, workspace)
	if err != nil {
		return LeaseTarget{}, err
	}
	server := workspaceToServer(cfg, workspace, leaseID, slug, keep)
	if probeSSH {
		if err := waitForSSH(ctx, &target, b.rt.Stderr); err != nil {
			return LeaseTarget{}, err
		}
	}
	return LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, nil
}

func (b *nvidiaBrevBackend) startStoppedWorkspace(ctx context.Context, client *brevClient, workspace brevWorkspace, orgID string) (brevWorkspace, error) {
	id := workspaceIdentifier(workspace)
	if id == "" {
		return brevWorkspace{}, exit(2, "nvidia-brev start requires workspace id or name")
	}
	if err := requireActiveBrevOrg(ctx, client, orgID); err != nil {
		return brevWorkspace{}, err
	}
	fmt.Fprintf(b.rt.Stderr, "starting provider=%s workspace=%s\n", providerName, safeWorkspaceRef(workspace))
	if err := client.start(ctx, id); err != nil {
		return brevWorkspace{}, err
	}
	workspace, _, err := b.waitForWorkspaceReady(ctx, client, id)
	return workspace, err
}

func (b *nvidiaBrevBackend) resolveSSHTarget(ctx context.Context, client *brevClient, cfg Config, workspace brevWorkspace) (SSHTarget, error) {
	if strings.TrimSpace(cfg.NvidiaBrev.Org) != "" {
		return SSHTarget{}, exit(2, "nvidiaBrev.org scopes read-only Brev inventory only; brev refresh does not support --org, so SSH lifecycle resolution is unsafe. Run `brev set` for the desired active org or remove nvidiaBrev.org before using nvidia-brev SSH lifecycle commands")
	}
	if err := client.refresh(ctx); err != nil {
		return SSHTarget{}, err
	}
	path := defaultBrevSSHConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return SSHTarget{}, exit(2, "read nvidia-brev SSH config %s: %v", path, err)
	}
	alias := brevSSHConfigAlias(workspace.Name, cfg.NvidiaBrev.Target)
	target, err := selectBrevSSHTarget(cfg, string(data), alias)
	if err != nil {
		return SSHTarget{}, err
	}
	return target, nil
}

func (b *nvidiaBrevBackend) rollbackOrRetainCreatedWorkspace(workspace brevWorkspace, orgID, leaseID, slug string, cfg Config, req AcquireRequest, cause error) error {
	server := workspaceToServer(cfg, workspace, leaseID, slug, false)
	server.Status = "deleting"
	server.Labels["state"] = "deleting"
	server.Labels["release"] = "delete"
	server.Labels["brev_org_id"] = orgID
	if workspace.ID == "" {
		server.Labels["brev_recovery"] = "create_unknown"
	}
	persistErr := persistLeaseTargetForRepoConfig(leaseID, slug, cfg, server, SSHTarget{}, req.Repo.Root, req.Reclaim)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := b.client()
	if err != nil {
		return errors.Join(cause, recoveryClaimError(persistErr), fmt.Errorf("nvidia-brev rollback client unavailable; recovery claim retained: %w", err))
	}
	if err := requireActiveBrevOrg(ctx, client, orgID); err != nil {
		return errors.Join(cause, recoveryClaimError(persistErr), fmt.Errorf("nvidia-brev rollback organization check failed; recovery claim retained: %w", err))
	}
	if persistErr != nil && strings.TrimSpace(workspace.ID) == "" {
		return errors.Join(cause, recoveryClaimError(persistErr), fmt.Errorf("nvidia-brev rollback requires a known workspace id when recovery claim persistence fails"))
	}
	if err := client.delete(ctx, workspaceIdentifier(workspace)); err != nil {
		return errors.Join(cause, recoveryClaimError(persistErr), fmt.Errorf("nvidia-brev rollback cleanup failed for workspace %s; recovery claim retained: %w", workspaceIdentifier(workspace), err))
	}
	if workspace.ID == "" {
		return errors.Join(cause, fmt.Errorf("nvidia-brev rollback requested; ambiguous create recovery claim retained for lease=%s", leaseID))
	}
	if persistErr != nil {
		if err := b.waitForWorkspaceDeleted(ctx, client, workspace, orgID); err != nil {
			return errors.Join(cause, recoveryClaimError(persistErr), fmt.Errorf("nvidia-brev rollback deletion pending without a recovery claim: %w", err))
		}
		return errors.Join(cause, recoveryClaimError(persistErr))
	}
	claim, claimed, err := resolveLeaseClaimForProvider(leaseID)
	if err != nil {
		return errors.Join(cause, fmt.Errorf("load nvidia-brev recovery claim: %w", err))
	}
	if !claimed {
		return errors.Join(cause, fmt.Errorf("load nvidia-brev recovery claim: lease=%s not found", leaseID))
	}
	if err := b.finishDeletingClaim(ctx, client, workspace, claim); err != nil {
		return errors.Join(cause, fmt.Errorf("nvidia-brev rollback deletion pending; recovery claim retained: %w", err))
	}
	return cause
}

func (b *nvidiaBrevBackend) retainAmbiguousCreateOrganizationClaim(workspace brevWorkspace, createOrgID, observedOrgID, currentOrgID, leaseID, slug string, cfg Config, req AcquireRequest, cause error) error {
	server := workspaceToServer(cfg, workspace, leaseID, slug, req.Keep)
	server.Status = "deleting"
	server.Labels["state"] = "deleting"
	server.Labels["release"] = "delete"
	server.Labels["brev_recovery"] = "org_changed"
	server.Labels["brev_create_org_id"] = createOrgID
	server.Labels["brev_observed_org_id"] = observedOrgID
	server.Labels["brev_current_org_id"] = currentOrgID
	delete(server.Labels, "brev_org_id")
	if err := persistLeaseTargetForRepoConfig(leaseID, slug, cfg, server, SSHTarget{}, req.Repo.Root, req.Reclaim); err != nil {
		return errors.Join(cause, fmt.Errorf("persist nvidia-brev organization-change recovery claim: %w", err))
	}
	return errors.Join(cause, exit(2, "automatic rollback is unsafe because workspace ownership is ambiguous; recovery claim retained for lease=%s", leaseID))
}

func (b *nvidiaBrevBackend) reconcileCreateFailure(client *brevClient, workspace brevWorkspace, createOrgID, leaseID, slug string, cfg Config, req AcquireRequest, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	activeBefore, err := client.activeOrg(ctx)
	if err != nil {
		return b.retainCreateUnknownClaim(workspace, createOrgID, leaseID, slug, cfg, req, errors.Join(cause, fmt.Errorf("reconcile failed nvidia-brev create organization: %w", err)))
	}
	if activeBefore.ID != createOrgID {
		return b.retainAmbiguousCreateOrganizationClaim(workspace, createOrgID, activeBefore.ID, activeBefore.ID, leaseID, slug, cfg, req, cause)
	}
	workspaces, err := client.list(ctx, true)
	if err != nil {
		return b.retainCreateUnknownClaim(workspace, createOrgID, leaseID, slug, cfg, req, errors.Join(cause, fmt.Errorf("reconcile failed nvidia-brev create inventory: %w", err)))
	}
	current, found, findErr := findBrevWorkspace(workspaces, workspace.Name)
	if findErr != nil {
		return b.retainCreateUnknownClaim(workspace, createOrgID, leaseID, slug, cfg, req, errors.Join(cause, findErr))
	}
	activeAfter, err := client.activeOrg(ctx)
	if err != nil {
		return b.retainCreateUnknownClaim(workspace, createOrgID, leaseID, slug, cfg, req, errors.Join(cause, fmt.Errorf("reconcile failed nvidia-brev create organization: %w", err)))
	}
	if activeBefore.ID != activeAfter.ID {
		if found {
			workspace = current
		}
		return b.retainAmbiguousCreateOrganizationClaim(workspace, createOrgID, activeBefore.ID, activeAfter.ID, leaseID, slug, cfg, req, cause)
	}
	if !found {
		return b.retainCreateUnknownClaim(workspace, createOrgID, leaseID, slug, cfg, req, cause)
	}
	if req.Keep {
		return b.retainFailedCreatedWorkspace(current, createOrgID, leaseID, slug, cfg, req, "kept_create_failed", cause)
	}
	return b.rollbackOrRetainCreatedWorkspace(current, createOrgID, leaseID, slug, cfg, req, cause)
}

func (b *nvidiaBrevBackend) retainCreateUnknownClaim(workspace brevWorkspace, orgID, leaseID, slug string, cfg Config, req AcquireRequest, cause error) error {
	if req.Keep {
		return b.retainFailedCreatedWorkspace(workspace, orgID, leaseID, slug, cfg, req, "create_unknown", cause)
	}
	server := workspaceToServer(cfg, workspace, leaseID, slug, false)
	server.Status = "deleting"
	server.Labels["state"] = "deleting"
	server.Labels["release"] = "delete"
	server.Labels["brev_org_id"] = orgID
	server.Labels["brev_recovery"] = "create_unknown"
	if err := persistLeaseTargetForRepoConfig(leaseID, slug, cfg, server, SSHTarget{}, req.Repo.Root, req.Reclaim); err != nil {
		return errors.Join(cause, fmt.Errorf("persist nvidia-brev ambiguous create recovery claim: %w", err))
	}
	return errors.Join(cause, fmt.Errorf("nvidia-brev create outcome is ambiguous; recovery claim retained for lease=%s", leaseID))
}

func (b *nvidiaBrevBackend) retainFailedCreatedWorkspace(workspace brevWorkspace, orgID, leaseID, slug string, cfg Config, req AcquireRequest, recovery string, cause error) error {
	server := workspaceToServer(cfg, workspace, leaseID, slug, true)
	server.Status = "failed"
	server.Labels["state"] = "failed"
	server.Labels["keep"] = "true"
	server.Labels["brev_org_id"] = orgID
	server.Labels["brev_recovery"] = recovery
	if err := persistLeaseTargetForRepoConfig(leaseID, slug, cfg, server, SSHTarget{}, req.Repo.Root, req.Reclaim); err != nil {
		return errors.Join(cause, fmt.Errorf("persist retained nvidia-brev recovery claim: %w", err))
	}
	return errors.Join(cause, fmt.Errorf("nvidia-brev workspace retained after acquisition failure; recovery claim stored for lease=%s", leaseID))
}

func recoveryClaimError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("persist nvidia-brev recovery claim before rollback: %w", err)
}

func (b *nvidiaBrevBackend) resolveWorkspace(ctx context.Context, client *brevClient, id string) (brevWorkspace, string, string, LeaseClaim, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return brevWorkspace{}, "", "", LeaseClaim{}, exit(2, "nvidia-brev resolve requires lease id, slug, workspace id, or workspace name")
	}
	claim, claimed, err := resolveNvidiaBrevClaim(id)
	if err != nil {
		return brevWorkspace{}, "", "", LeaseClaim{}, err
	}
	workspaces, err := client.list(ctx, true)
	if err != nil {
		return brevWorkspace{}, "", "", LeaseClaim{}, err
	}
	if claimed {
		workspaceRef := firstNonEmpty(claim.CloudID, claim.Labels["brev_workspace_name"])
		if workspace, found, err := findBrevWorkspace(workspaces, workspaceRef); err != nil {
			return brevWorkspace{}, "", "", LeaseClaim{}, err
		} else if found {
			if !claimMatchesWorkspace(claim, workspace) {
				return brevWorkspace{}, "", "", LeaseClaim{}, exit(2, "lease=%s claim does not match nvidia-brev workspace %s", claim.LeaseID, safeWorkspaceRef(workspace))
			}
			leaseID, slug := brevLeaseIdentity(workspace, claim)
			return workspace, leaseID, slug, claim, nil
		}
		if workspaceRef != "" {
			return brevWorkspace{}, "", "", LeaseClaim{}, exit(4, "nvidia-brev workspace for lease=%s not found", claim.LeaseID)
		}
	}
	workspace, found, err := findBrevWorkspace(workspaces, id)
	if err != nil {
		return brevWorkspace{}, "", "", LeaseClaim{}, err
	}
	if !found {
		return brevWorkspace{}, "", "", LeaseClaim{}, exit(4, "nvidia-brev workspace not found: %s", id)
	}
	if !claimed {
		claim, claimed, err = resolveLeaseClaimForProviderCloudID(workspace.ID)
		if err != nil {
			return brevWorkspace{}, "", "", LeaseClaim{}, err
		}
	}
	leaseID, slug := brevLeaseIdentity(workspace, claim)
	return workspace, leaseID, slug, claim, nil
}

func resolveNvidiaBrevClaim(identifier string) (LeaseClaim, bool, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return LeaseClaim{}, false, nil
	}
	if claim, ok, err := resolveLeaseClaimForProvider(identifier); err != nil || ok {
		return claim, ok, err
	}
	if claim, ok, err := resolveLeaseClaimForProviderCloudID(identifier); err != nil || ok {
		return claim, ok, err
	}
	claims, err := listLeaseClaims()
	if err != nil {
		return LeaseClaim{}, false, err
	}
	var match LeaseClaim
	for _, claim := range claims {
		if claim.Provider != providerName || strings.TrimSpace(claim.Labels["brev_workspace_name"]) != identifier {
			continue
		}
		if match.LeaseID != "" {
			return LeaseClaim{}, false, exit(2, "multiple provider=%s claims match workspace name %s", providerName, identifier)
		}
		match = claim
	}
	return match, match.LeaseID != "", nil
}

func (b *nvidiaBrevBackend) listServers(ctx context.Context, client *brevClient, all bool) ([]LeaseView, error) {
	workspaces, err := client.list(ctx, all)
	if err != nil {
		return nil, err
	}
	claims, err := listLeaseClaims()
	if err != nil {
		return nil, err
	}
	claimsByCloudID := map[string]LeaseClaim{}
	claimsByWorkspaceName := map[string]LeaseClaim{}
	for _, claim := range claims {
		if claim.Provider != providerName {
			continue
		}
		if claim.CloudID != "" {
			claimsByCloudID[claim.CloudID] = claim
			continue
		}
		if !createRecoveryClaim(claim) {
			continue
		}
		name := strings.TrimSpace(claim.Labels["brev_workspace_name"])
		if name == "" {
			continue
		}
		if existing := claimsByWorkspaceName[name]; existing.LeaseID != "" && existing.LeaseID != claim.LeaseID {
			return nil, exit(2, "multiple provider=%s recovery claims match workspace name %s", providerName, name)
		}
		claimsByWorkspaceName[name] = claim
	}
	servers := make([]LeaseView, 0, len(workspaces))
	for _, workspace := range workspaces {
		claim := claimsByCloudID[workspace.ID]
		if claim.LeaseID == "" {
			claim = claimsByWorkspaceName[workspace.Name]
		}
		if !all && claim.LeaseID == "" && !isCrabboxBrevWorkspace(workspace) {
			continue
		}
		leaseID, slug := brevLeaseIdentity(workspace, claim)
		servers = append(servers, workspaceToClaimedServer(b.configForRun(), workspace, leaseID, slug, claim))
	}
	return servers, nil
}

func (b *nvidiaBrevBackend) releaseWorkspace(ctx context.Context, client *brevClient, workspace brevWorkspace, action string) error {
	id := workspaceIdentifier(workspace)
	if id == "" {
		return exit(2, "nvidia-brev release requires workspace id or name")
	}
	switch normalizeNvidiaBrevReleaseAction(action) {
	case "stop":
		if err := client.stop(ctx, id); err != nil {
			return err
		}
	default:
		if err := client.delete(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func (b *nvidiaBrevBackend) releaseAction(labels map[string]string) string {
	cfg := b.configForRun()
	if releaseActionExplicit(cfg) {
		return normalizeNvidiaBrevReleaseAction(cfg.NvidiaBrev.ReleaseAction)
	}
	if labels != nil {
		switch strings.ToLower(strings.TrimSpace(labels["release"])) {
		case "stop":
			return "stop"
		case "delete":
			return "delete"
		}
	}
	return normalizeNvidiaBrevReleaseAction(cfg.NvidiaBrev.ReleaseAction)
}

func (b *nvidiaBrevBackend) releaseResultAction(labels map[string]string) string {
	if labels != nil && strings.EqualFold(strings.TrimSpace(labels["state"]), "deleting") {
		return "delete"
	}
	return b.releaseAction(labels)
}

func normalizeNvidiaBrevReleaseAction(action string) string {
	action = strings.ToLower(strings.TrimSpace(action))
	if action == "stop" {
		return "stop"
	}
	return "delete"
}

func findBrevWorkspace(workspaces []brevWorkspace, idOrName string) (brevWorkspace, bool, error) {
	idOrName = strings.TrimSpace(idOrName)
	if idOrName == "" {
		return brevWorkspace{}, false, nil
	}
	var match brevWorkspace
	for _, workspace := range workspaces {
		if workspace.ID != idOrName && workspace.Name != idOrName {
			continue
		}
		if match.ID != "" || match.Name != "" {
			return brevWorkspace{}, false, exit(2, "nvidia-brev workspace %q is ambiguous", idOrName)
		}
		match = workspace
	}
	return match, match.ID != "" || match.Name != "", nil
}

func workspaceToServer(cfg Config, workspace brevWorkspace, leaseID, slug string, keep bool) Server {
	labels := directLeaseLabels(cfg, leaseID, slug, providerName, "", keep)
	labels["state"] = normalizeBrevState(workspace)
	labels["release"] = normalizeNvidiaBrevReleaseAction(cfg.NvidiaBrev.ReleaseAction)
	labels["brev_workspace_id"] = workspace.ID
	labels["brev_workspace_name"] = workspace.Name
	labels["brev_status"] = workspace.Status
	labels["brev_build_status"] = workspace.BuildStatus
	labels["brev_shell_status"] = workspace.ShellStatus
	labels["brev_health_status"] = workspace.HealthStatus
	labels["brev_target"] = strings.TrimSpace(cfg.NvidiaBrev.Target)
	if workspace.GPU != "" {
		labels["gpu"] = workspace.GPU
	}
	if workspace.InstanceKind != "" {
		labels["instance_kind"] = workspace.InstanceKind
	}
	server := Server{
		CloudID:  workspace.ID,
		Provider: providerName,
		Name:     workspace.Name,
		Status:   labels["state"],
		Labels:   labels,
	}
	server.ServerType.Name = firstNonEmpty(workspace.InstanceType, workspace.WorkspaceClass, cfg.NvidiaBrev.Type, cfg.NvidiaBrev.GPUName)
	server.Labels["server_type"] = server.ServerType.Name
	return server
}

func workspaceToClaimedServer(cfg Config, workspace brevWorkspace, leaseID, slug string, claim LeaseClaim) Server {
	server := workspaceToServer(cfg, workspace, leaseID, slug, false)
	if workspace.InstanceType == "" && workspace.WorkspaceClass == "" {
		if storedType := strings.TrimSpace(claim.Labels["server_type"]); storedType != "" {
			server.ServerType.Name = storedType
			server.Labels["server_type"] = storedType
		}
	}
	return serverWithClaimLabels(server, claim)
}

func serverWithClaimLabels(server Server, claim LeaseClaim) Server {
	if len(claim.Labels) == 0 {
		return server
	}
	labels := make(map[string]string, len(claim.Labels)+len(server.Labels))
	for key, value := range claim.Labels {
		labels[key] = value
	}
	for key, value := range server.Labels {
		if _, exists := labels[key]; !exists {
			labels[key] = value
		}
	}
	for _, key := range []string{
		"state",
		"brev_workspace_id",
		"brev_workspace_name",
		"brev_status",
		"brev_target",
	} {
		delete(labels, key)
		if value := server.Labels[key]; value != "" {
			labels[key] = value
		}
	}
	for _, key := range []string{
		"server_type",
		"brev_build_status",
		"brev_shell_status",
		"brev_health_status",
		"gpu",
		"instance_kind",
	} {
		if value := server.Labels[key]; value != "" {
			labels[key] = value
		}
	}
	server.Labels = labels
	server.Status = labels["state"]
	return server
}

func normalizeBrevState(workspace brevWorkspace) string {
	if brevWorkspaceReady(workspace) {
		return "ready"
	}
	status := strings.ToLower(strings.TrimSpace(workspace.Status))
	switch status {
	case "running", "starting", "building", "queued", "pending":
		return "booting"
	case "paused", "off":
		return "stopped"
	case "deleted":
		return "released"
	case "error":
		return "failed"
	case "stopped", "failed":
		return status
	case "":
		return "unknown"
	default:
		return status
	}
}

func brevWorkspaceStopped(workspace brevWorkspace) bool {
	return oneOf(strings.ToLower(strings.TrimSpace(workspace.Status)), "stopped", "paused", "off")
}

func brevWorkspaceReady(workspace brevWorkspace) bool {
	status := strings.ToLower(strings.TrimSpace(workspace.Status))
	build := strings.ToLower(strings.TrimSpace(workspace.BuildStatus))
	shell := strings.ToLower(strings.TrimSpace(workspace.ShellStatus))
	health := strings.ToLower(strings.TrimSpace(workspace.HealthStatus))
	return oneOf(status, "running", "ready") &&
		(build == "" || oneOf(build, "ready", "complete", "completed", "success", "succeeded")) &&
		(shell == "" || oneOf(shell, "ready", "running", "connected")) &&
		(health == "" || oneOf(health, "healthy", "ready", "ok"))
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func workspaceIdentifier(workspace brevWorkspace) string {
	return firstNonEmpty(workspace.ID, workspace.Name)
}

func safeWorkspaceRef(workspace brevWorkspace) string {
	if workspace.ID != "" {
		return workspace.ID
	}
	return workspace.Name
}

func claimMatchesWorkspace(claim LeaseClaim, workspace brevWorkspace) bool {
	if claim.CloudID != "" {
		return claim.CloudID == workspace.ID
	}
	if claim.Labels != nil {
		if id := strings.TrimSpace(claim.Labels["brev_workspace_id"]); id != "" {
			return id == workspace.ID
		}
		if name := strings.TrimSpace(claim.Labels["brev_workspace_name"]); name != "" {
			return name == workspace.Name
		}
	}
	return false
}

func brevLeaseIdentity(workspace brevWorkspace, claim LeaseClaim) (string, string) {
	if claim.LeaseID != "" {
		return claim.LeaseID, claim.Slug
	}
	leaseID, slug := parseBrevProviderName(workspace.Name)
	if leaseID != "" {
		return leaseID, slug
	}
	if workspace.ID != "" {
		return "nbrev_" + normalizeBrevSlug(workspace.ID), normalizeBrevSlug(workspace.Name)
	}
	return "", normalizeBrevSlug(workspace.Name)
}

func isCrabboxBrevWorkspace(workspace brevWorkspace) bool {
	leaseID, _ := parseBrevProviderName(workspace.Name)
	return leaseID != ""
}

func brevProviderName(leaseID, slug string) string {
	slug = fitBrevSlugForName(slug, leaseID)
	if slug == "" {
		slug = "lease"
	}
	return fmt.Sprintf("crabbox-%s-%s", slug, strings.TrimPrefix(leaseID, "cbx_"))
}

func parseBrevProviderName(name string) (string, string) {
	name = strings.TrimSpace(name)
	if !strings.HasPrefix(name, "crabbox-") {
		return "", ""
	}
	rest := strings.TrimPrefix(name, "crabbox-")
	lastDash := strings.LastIndex(rest, "-")
	if lastDash < 0 || lastDash == len(rest)-1 {
		return "", ""
	}
	slug := rest[:lastDash]
	id := rest[lastDash+1:]
	if len(id) < 8 {
		return "", ""
	}
	return "cbx_" + id, slug
}

func allocateBrevLeaseSlug(leaseID, requested string, servers []LeaseView, claims []LeaseClaim) string {
	base := fitBrevSlugForName(requested, leaseID)
	if base == "" {
		base = fitBrevSlugForName(strings.TrimPrefix(leaseID, "cbx_"), leaseID)
	}
	if base == "" {
		base = "lease"
	}
	slug := base
	for attempt := 0; attempt < 20; attempt++ {
		if !brevSlugInUse(slug, leaseID, servers, claims) {
			return slug
		}
		slug = brevSlugWithCollisionSuffix(base, fmt.Sprintf("%02d", attempt+1), leaseID)
	}
	return brevSlugWithCollisionSuffix(base, strings.TrimPrefix(leaseID, "cbx_"), leaseID)
}

func brevSlugInUse(slug, leaseID string, servers []LeaseView, claims []LeaseClaim) bool {
	for _, server := range servers {
		if normalizeBrevSlug(server.Labels["slug"]) == slug {
			return true
		}
	}
	for _, claim := range claims {
		if claim.Provider == providerName && claim.LeaseID != leaseID && normalizeBrevSlug(claim.Slug) == slug {
			return true
		}
	}
	return false
}

func brevSlugWithCollisionSuffix(base, suffix, leaseID string) string {
	suffix = "-" + normalizeBrevSlug(suffix)
	maxLen := maxBrevSlugLength(leaseID)
	if len(suffix) >= maxLen {
		return strings.Trim(suffix[len(suffix)-maxLen:], "-")
	}
	base = fitBrevSlugForName(base, leaseID)
	if len(base) > maxLen-len(suffix) {
		base = strings.Trim(base[:maxLen-len(suffix)], "-")
	}
	return strings.Trim(base+suffix, "-")
}

func fitBrevSlugForName(value, leaseID string) string {
	value = normalizeBrevSlug(value)
	maxLen := maxBrevSlugLength(leaseID)
	if len(value) > maxLen {
		value = strings.Trim(value[:maxLen], "-")
	}
	return value
}

func maxBrevSlugLength(leaseID string) int {
	maxLen := brevWorkspaceNameMaxLen - len("crabbox-") - 1 - len(strings.TrimPrefix(leaseID, "cbx_"))
	if maxLen < 1 {
		return 1
	}
	return maxLen
}

func normalizeBrevSlug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = normalizeBrevSlugPattern.ReplaceAllString(value, "-")
	value = strings.Trim(value, "-")
	if len(value) > 48 {
		value = strings.Trim(value[:48], "-")
	}
	return value
}
