package fal

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	falPollInterval             = 3 * time.Second
	falPollTimeout              = 10 * time.Minute
	falCreateReconcileAttempts  = 3
	falCreateReconcileRetryWait = time.Second
	falCreateRecoveryWindow     = 9 * time.Minute
	falCredentialBindingLabel   = "fal_credential_binding"
)

var (
	errFalProviderAbsenceNotAccountBound = errors.New("fal provider absence is not account-bound")
	errFalRecoveryClaimRemoved           = errors.New("fal recovery claim was removed by concurrent cleanup")
)

func (b *backend) Acquire(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	cfg := b.configForRun()
	if InstanceType(cfg.Fal.InstanceType) != InstanceTypeH100x8 {
		cfg.Fal.Sector = ""
	}
	client, err := b.api()
	if err != nil {
		return core.LeaseTarget{}, err
	}
	leaseID := core.NewLeaseID()
	slug, err := core.AllocateClaimLeaseSlug(leaseID, req.RequestedSlug)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	keyPath, publicKey, err := core.EnsureTestboxKeyForConfig(cfg, leaseID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	cfg.SSHKey = keyPath
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s instance=%s sector=%s keep=%v\n",
		providerName, leaseID, slug, cfg.Fal.InstanceType, cfg.Fal.Sector, req.Keep)

	createRequest := CreateInstanceRequest{
		InstanceType: InstanceType(cfg.Fal.InstanceType),
		SSHKey:       publicKey,
		Sector:       Sector(cfg.Fal.Sector),
	}
	createStarted := b.now()
	created, createErr := client.CreateInstance(ctx, createRequest, leaseID)
	if createErr != nil || strings.TrimSpace(created.ID) == "" {
		if createErr != nil && !isAmbiguousFalMutationError(createErr) {
			core.RemoveStoredTestboxKey(leaseID)
			return core.LeaseTarget{}, createErr
		}
		if createErr == nil {
			createErr = exit(5, "fal create instance returned an empty id")
		}
		reconciled, reconcileErr := b.reconcileAmbiguousCreate(ctx, client, createRequest, leaseID, createStarted, createErr)
		if reconcileErr != nil {
			claimErr := b.persistRecoveryClaimAt(leaseID, slug, cfg, req.Repo.Root, "", "ambiguous-create", false, createStarted)
			if claimErr != nil {
				return core.LeaseTarget{}, errors.Join(reconcileErr, fmt.Errorf("persist fal ambiguous-create recovery claim: %w", claimErr))
			}
			return core.LeaseTarget{}, reconcileErr
		}
		created = reconciled
	}
	instanceID := strings.TrimSpace(created.ID)
	initialClaim, claimErr := b.persistRecoveryClaimAtIfUnchanged(leaseID, slug, cfg, req.Repo.Root, instanceID, "provisioning", req.Keep, createStarted, core.LeaseClaim{}, false)
	if claimErr != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		cleanupErr := client.DeleteInstance(cleanupCtx, instanceID)
		if cleanupErr == nil {
			core.RemoveStoredTestboxKey(leaseID)
			return core.LeaseTarget{}, fmt.Errorf("persist fal provisioning claim after creating instance %s: %w", instanceID, claimErr)
		}
		return core.LeaseTarget{}, errors.Join(
			fmt.Errorf("persist fal provisioning claim after creating instance %s: %w", instanceID, claimErr),
			fmt.Errorf("fal cleanup failed for unclaimed instance %s: %w", instanceID, cleanupErr),
		)
	}
	if req.OnAcquired != nil {
		rawTarget := core.LeaseTarget{
			Server:  falServer(created, cfg, leaseID, slug, req.Keep, createStarted),
			LeaseID: leaseID,
		}
		if rawSSH, sshErr := falSSHTarget(cfg, created); sshErr == nil {
			rawTarget.SSH = rawSSH
		}
		if err := req.OnAcquired(rawTarget); err != nil {
			return core.LeaseTarget{}, b.rollbackClaimedAcquire(instanceID, leaseID, slug, cfg, req.Repo.Root, "rollback-cleanup", err)
		}
	}

	ready, err := b.waitForInstanceReady(ctx, client, instanceID)
	if err != nil {
		return core.LeaseTarget{}, b.handleFailedAcquire(instanceID, leaseID, slug, cfg, req.Repo.Root, req.Keep, err)
	}
	server := falServer(ready, cfg, leaseID, slug, req.Keep, createStarted)
	ssh, err := falSSHTarget(cfg, ready)
	if err != nil {
		return core.LeaseTarget{}, b.handleFailedAcquire(instanceID, leaseID, slug, cfg, req.Repo.Root, req.Keep, err)
	}
	if err := b.waitForSSH(ctx, &ssh, "fal bootstrap", core.BootstrapWaitTimeout(cfg)); err != nil {
		return core.LeaseTarget{}, b.handleFailedAcquire(instanceID, leaseID, slug, cfg, req.Repo.Root, req.Keep, err)
	}
	if req.Repo.Root != "" {
		claimServer, err := falClaimServer(server, cfg)
		if err != nil {
			return core.LeaseTarget{}, b.handleFailedAcquire(instanceID, leaseID, slug, cfg, req.Repo.Root, req.Keep, err)
		}
		if _, err := core.ClaimLeaseTargetForRepoConfigScopeIfUnchanged(leaseID, slug, cfg, falClaimScope(cfg), claimServer, ssh, req.Repo.Root, cfg.IdleTimeout, req.Reclaim, initialClaim, true); err != nil {
			return core.LeaseTarget{}, b.handleFailedAcquire(instanceID, leaseID, slug, cfg, req.Repo.Root, req.Keep, err)
		}
	} else {
		claimServer, err := falClaimServer(server, cfg)
		if err != nil {
			return core.LeaseTarget{}, b.handleFailedAcquire(instanceID, leaseID, slug, cfg, req.Repo.Root, req.Keep, err)
		}
		if _, err := core.ClaimLeaseTargetForConfigScopeIfUnchanged(leaseID, slug, cfg, falClaimScope(cfg), claimServer, ssh, cfg.IdleTimeout, initialClaim, true); err != nil {
			return core.LeaseTarget{}, b.handleFailedAcquire(instanceID, leaseID, slug, cfg, req.Repo.Root, req.Keep, err)
		}
	}
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s fal=%s state=ready\n", leaseID, instanceID)
	target := core.LeaseTarget{Server: server, SSH: ssh, LeaseID: leaseID}
	return target, nil
}

func (b *backend) reconcileAmbiguousCreate(ctx context.Context, client computeAPI, req CreateInstanceRequest, leaseID string, createStarted time.Time, cause error) (ComputeInstance, error) {
	var lastErr error
	for attempt := 1; attempt <= falCreateReconcileAttempts; attempt++ {
		if !b.now().Before(createStarted.Add(falCreateRecoveryWindow)) {
			return ComputeInstance{}, errors.Join(
				fmt.Errorf("fal instance creation remains indeterminate after the provider idempotency replay window expired; no provider id was returned"),
				cause,
				lastErr,
			)
		}
		instance, err := client.CreateInstance(ctx, req, leaseID)
		if err == nil {
			if strings.TrimSpace(instance.ID) != "" {
				return instance, nil
			}
			err = exit(5, "fal idempotent create retry returned an empty id")
		}
		lastErr = err
		if !isAmbiguousFalMutationError(err) {
			return ComputeInstance{}, errors.Join(cause, fmt.Errorf("fal idempotent create retry failed: %w", err))
		}
		if attempt == falCreateReconcileAttempts {
			break
		}
		timer := time.NewTimer(falCreateReconcileRetryWait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ComputeInstance{}, errors.Join(cause, ctx.Err())
		case <-timer.C:
		}
	}
	return ComputeInstance{}, errors.Join(
		fmt.Errorf("fal instance creation remains indeterminate after idempotent retry; no provider id was returned"),
		cause,
		lastErr,
	)
}

func (b *backend) Resolve(ctx context.Context, req core.ResolveRequest) (core.LeaseTarget, error) {
	cfg := b.configForRun()
	client, err := b.api()
	if err != nil {
		return core.LeaseTarget{}, err
	}
	claim, ok, err := resolveFalClaim(req.ID, falClaimScope(cfg))
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if !ok {
		return core.LeaseTarget{}, exit(4, "lease/fal instance not found or not locally claimed: %s", strings.TrimSpace(req.ID))
	}
	if err := verifyFalClaimCredential(claim, cfg); err != nil {
		return core.LeaseTarget{}, err
	}
	if claim.Labels["recovery"] == "provisioning" && !req.ReleaseOnly {
		return core.LeaseTarget{}, exit(4, "fal lease %s is still provisioning; retry after acquisition completes", claim.LeaseID)
	}
	if claim.CloudID == "" {
		if !req.ReleaseOnly {
			return core.LeaseTarget{}, exit(4, "fal recovery is still pending for lease=%s; local recovery state retained", claim.LeaseID)
		}
		claim, err = b.recoverAmbiguousCreateForRelease(ctx, client, claim, cfg)
		if err != nil {
			return core.LeaseTarget{}, err
		}
	}
	instance, err := client.GetInstance(ctx, claim.CloudID)
	if err != nil {
		if req.ReleaseOnly && isFalNotFound(err) {
			return leaseTargetFromClaim(claim, cfg, false)
		}
		return core.LeaseTarget{}, err
	}
	includeSSH := !req.ReleaseOnly && (!req.StatusOnly || (req.ReadyProbe && strings.TrimSpace(instance.IP) != ""))
	target, err := leaseTargetFromClaimedInstance(instance, claim, cfg, includeSSH)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if req.ReleaseOnly {
		target.SSH = core.SSHTarget{}
		return target, nil
	}
	if req.Repo.Root != "" && !req.NoLocalStateMutations {
		claimServer, err := falClaimServer(target.Server, cfg)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		if _, err := core.ClaimLeaseTargetForRepoConfigScopeIfUnchanged(target.LeaseID, claim.Slug, cfg, falClaimScope(cfg), claimServer, target.SSH, req.Repo.Root, cfg.IdleTimeout, req.Reclaim, claim, true); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	return target, nil
}

func (b *backend) List(ctx context.Context, _ core.ListRequest) ([]core.LeaseView, error) {
	cfg := b.configForRun()
	claims, err := falClaims(falClaimScope(cfg))
	if err != nil {
		return nil, err
	}
	views := make([]core.LeaseView, 0, len(claims))
	needsProvider := false
	for _, claim := range claims {
		if err := verifyFalClaimCredential(claim, cfg); err != nil {
			server, viewErr := falClaimView(claim, cfg, "credential-binding-mismatch")
			if viewErr != nil {
				return nil, viewErr
			}
			views = append(views, server)
			continue
		}
		if claim.CloudID == "" {
			server, err := falClaimView(claim, cfg, firstNonBlank(claim.Labels["recovery"], "recovery-pending"))
			if err != nil {
				return nil, err
			}
			views = append(views, server)
			continue
		}
		needsProvider = true
	}
	if !needsProvider {
		return views, nil
	}
	client, apiErr := b.api()
	for _, claim := range claims {
		if claim.CloudID == "" {
			continue
		}
		if verifyFalClaimCredential(claim, cfg) != nil {
			continue
		}
		if apiErr != nil {
			server, viewErr := falClaimView(claim, cfg, "provider-verification-unavailable")
			if viewErr != nil {
				return nil, viewErr
			}
			views = append(views, server)
			continue
		}
		instance, err := client.GetInstance(ctx, claim.CloudID)
		if isFalNotFound(err) {
			server, viewErr := falClaimView(claim, cfg, "provider-absence-unverified")
			if viewErr != nil {
				return nil, viewErr
			}
			views = append(views, server)
			continue
		}
		if err != nil {
			server, viewErr := falClaimView(claim, cfg, "provider-verification-unavailable")
			if viewErr != nil {
				return nil, viewErr
			}
			views = append(views, server)
			continue
		}
		target, err := leaseTargetFromClaimedInstance(instance, claim, cfg, false)
		if err != nil {
			return nil, err
		}
		views = append(views, target.Server)
	}
	return views, nil
}

func falClaimView(claim core.LeaseClaim, cfg Config, status string) (core.LeaseView, error) {
	server, err := serverFromClaim(claim, cfg)
	if err != nil {
		return core.LeaseView{}, err
	}
	server.Status = status
	server.Labels = cloneLabels(server.Labels)
	server.Labels["state"] = status
	return server, nil
}

func (b *backend) Touch(_ context.Context, req core.TouchRequest) (core.Server, error) {
	server := req.Lease.Server
	if req.Lease.LeaseID == "" {
		return core.Server{}, exit(2, "provider=%s touch requires a lease id", providerName)
	}
	claim, ok, err := core.ReadLeaseClaimWithPresence(req.Lease.LeaseID)
	if err != nil {
		return core.Server{}, err
	}
	if !ok || claim.Provider != providerName {
		return core.Server{}, exit(2, "no local claim for fal lease %s", req.Lease.LeaseID)
	}
	cfg := b.configForRun()
	if claim.ProviderScope != falClaimScope(cfg) {
		return core.Server{}, exit(2, "fal lease %s belongs to a different API endpoint; refusing to touch it", req.Lease.LeaseID)
	}
	if err := verifyFalClaimCredential(claim, cfg); err != nil {
		return core.Server{}, err
	}
	if claim.Labels["recovery"] == "provisioning" {
		return core.Server{}, exit(4, "fal lease %s is still provisioning; refusing to update it", claim.LeaseID)
	}
	if req.IdleTimeout > 0 {
		cfg.IdleTimeout = req.IdleTimeout
	}
	labels := cloneLabels(server.Labels)
	if len(labels) == 0 {
		labels = cloneLabels(claim.Labels)
	}
	server.Labels = core.TouchDirectLeaseLabels(labels, cfg, req.State, b.now())
	claimLabels := cloneLabels(server.Labels)
	claimLabels[falCredentialBindingLabel] = claim.Labels[falCredentialBindingLabel]
	if _, err := core.UpdateLeaseClaimLabelsIfUnchanged(req.Lease.LeaseID, claim, claimLabels); err != nil {
		return core.Server{}, err
	}
	delete(server.Labels, falCredentialBindingLabel)
	return server, nil
}

func (b *backend) ReleaseLease(ctx context.Context, req core.ReleaseLeaseRequest) error {
	if err := core.ValidateLeaseTargetProviderIdentity(req.Lease, req.ExpectedProviderIdentity); err != nil {
		return err
	}
	leaseID := strings.TrimSpace(req.Lease.LeaseID)
	if leaseID == "" {
		leaseID = req.Lease.Server.Labels["lease"]
	}
	if leaseID == "" {
		return exit(2, "provider=%s release requires a lease id", providerName)
	}
	claim, ok, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		return err
	}
	if !ok || claim.Provider != providerName {
		return exit(2, "no local claim for fal lease %s; refusing to delete provider resources", leaseID)
	}
	cfg := b.configForRun()
	if claim.ProviderScope != falClaimScope(cfg) {
		return exit(2, "fal lease %s belongs to a different API endpoint; refusing to delete provider resources", leaseID)
	}
	if err := verifyFalClaimCredential(claim, cfg); err != nil {
		return err
	}
	if claim.CloudID == "" {
		return exit(4, "fal recovery is still pending for lease=%s; local recovery state retained", leaseID)
	}
	if req.Lease.Server.CloudID != "" && claim.CloudID != req.Lease.Server.CloudID {
		return exit(2, "refusing to release fal instance %s from stale local claim", req.Lease.Server.CloudID)
	}
	instanceID := claim.CloudID
	client, err := b.api()
	if err != nil {
		return err
	}
	err = core.RemoveLeaseClaimIfUnchangedAfter(leaseID, claim, func() error {
		if live, getErr := client.GetInstance(ctx, instanceID); getErr == nil {
			if strings.TrimSpace(live.ID) != instanceID {
				return exit(2, "refusing to release fal instance %s after provider identity changed", instanceID)
			}
		} else if isFalNotFound(getErr) {
			return exit(5, "fal instance %s is not visible to the current credentials; local claim retained because provider absence is not account-bound", instanceID)
		} else {
			return getErr
		}
		if deleteErr := client.DeleteInstance(ctx, instanceID); deleteErr != nil && !isFalNotFound(deleteErr) {
			return deleteErr
		}
		return nil
	})
	if err != nil {
		return err
	}
	core.RemoveStoredTestboxKey(leaseID)
	return nil
}

func (b *backend) Cleanup(ctx context.Context, req core.CleanupRequest) error {
	cfg := b.configForRun()
	claims, err := falClaims(falClaimScope(cfg))
	if err != nil {
		return err
	}
	matchingClaims := make([]core.LeaseClaim, 0, len(claims))
	for _, claim := range claims {
		if err := verifyFalClaimCredential(claim, cfg); err != nil {
			fmt.Fprintf(b.rt.Stderr, "skip server id=%s name=%s reason=credential_binding_mismatch\n", firstNonBlank(claim.CloudID, claim.LeaseID), firstNonBlank(claim.Slug, claim.LeaseID))
			continue
		}
		matchingClaims = append(matchingClaims, claim)
	}
	claims = matchingClaims
	if len(claims) == 0 {
		return nil
	}
	client, err := b.api()
	if err != nil {
		return err
	}
	for _, claim := range claims {
		server, err := serverFromClaim(claim, cfg)
		if err != nil {
			return err
		}
		remove := claim.Labels["recovery"] == "rollback-cleanup"
		reason := "rollback-cleanup"
		if !remove {
			remove, reason = core.ShouldCleanupServer(server, b.now())
		}
		if !remove {
			fmt.Fprintf(b.rt.Stderr, "skip server id=%s name=%s reason=%s\n", server.DisplayID(), server.Name, reason)
			continue
		}
		if claim.CloudID == "" {
			fmt.Fprintf(b.rt.Stderr, "skip server id=%s name=%s reason=recovery_pending\n", server.DisplayID(), server.Name)
			continue
		}
		verifyLive := func() error {
			live, getErr := client.GetInstance(ctx, claim.CloudID)
			if isFalNotFound(getErr) {
				return fmt.Errorf("%w: %s", errFalProviderAbsenceNotAccountBound, claim.CloudID)
			}
			if getErr != nil {
				return getErr
			}
			if strings.TrimSpace(live.ID) != claim.CloudID {
				return exit(2, "refusing cleanup for fal lease %s after provider identity changed", claim.LeaseID)
			}
			return nil
		}
		if req.DryRun {
			if err := verifyLive(); err != nil {
				if errors.Is(err, errFalProviderAbsenceNotAccountBound) {
					fmt.Fprintf(b.rt.Stderr, "skip server id=%s name=%s reason=provider_absence_not_account_bound\n", server.DisplayID(), server.Name)
					continue
				}
				return err
			}
			fmt.Fprintf(b.rt.Stderr, "delete server id=%s name=%s\n", claim.CloudID, server.Name)
			continue
		}
		err = core.RemoveLeaseClaimIfUnchangedAfter(claim.LeaseID, claim, func() error {
			if err := verifyLive(); err != nil {
				return err
			}
			fmt.Fprintf(b.rt.Stderr, "delete server id=%s name=%s\n", claim.CloudID, server.Name)
			if deleteErr := client.DeleteInstance(ctx, claim.CloudID); deleteErr != nil && !isFalNotFound(deleteErr) {
				return deleteErr
			}
			return nil
		})
		if errors.Is(err, errFalProviderAbsenceNotAccountBound) {
			fmt.Fprintf(b.rt.Stderr, "skip server id=%s name=%s reason=provider_absence_not_account_bound\n", server.DisplayID(), server.Name)
			continue
		}
		if err != nil {
			return err
		}
		core.RemoveStoredTestboxKey(claim.LeaseID)
	}
	return nil
}

func (b *backend) api() (computeAPI, error) {
	if b.clientFactory == nil {
		b.clientFactory = newClient
	}
	return b.clientFactory(b.configForRun(), b.rt)
}

func (b *backend) configForRun() Config {
	cfg := b.cfg
	applyFalDefaults(&cfg)
	return cfg
}

func (b *backend) waitForSSH(ctx context.Context, target *core.SSHTarget, phase string, timeout time.Duration) error {
	if b.waitSSH != nil {
		return b.waitSSH(ctx, target, phase, timeout)
	}
	return core.WaitForSSHReady(ctx, target, b.rt.Stderr, phase, timeout)
}

func (b *backend) waitForInstanceReady(ctx context.Context, client computeAPI, id string) (ComputeInstance, error) {
	timeout := b.pollTimeout
	if timeout <= 0 {
		timeout = falPollTimeout
	}
	deadline := b.now().Add(timeout)
	for {
		item, err := client.GetInstance(ctx, id)
		if err != nil {
			return ComputeInstance{}, err
		}
		if !item.Status.Known() {
			return ComputeInstance{}, exit(5, "fal instance %s reported unknown status %s", id, item.Status)
		}
		if item.Status == InstanceStatusReady {
			if strings.TrimSpace(item.IP) == "" {
				return ComputeInstance{}, exit(5, "fal instance %s is ready without an SSH host", id)
			}
			return item, nil
		}
		if item.Status == InstanceStatusStopped {
			return ComputeInstance{}, exit(5, "fal instance %s reached terminal status %s", id, item.Status)
		}
		if !b.now().Before(deadline) {
			return ComputeInstance{}, exit(5, "timed out waiting for fal instance %s to become ready", id)
		}
		timer := time.NewTimer(b.effectivePollInterval())
		select {
		case <-ctx.Done():
			timer.Stop()
			return ComputeInstance{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (b *backend) effectivePollInterval() time.Duration {
	if b.pollInterval > 0 {
		return b.pollInterval
	}
	return falPollInterval
}

func (b *backend) recoverAmbiguousCreateForRelease(ctx context.Context, client computeAPI, claim core.LeaseClaim, cfg Config) (core.LeaseClaim, error) {
	if claim.Labels["recovery"] != "ambiguous-create" {
		return core.LeaseClaim{}, exit(4, "fal recovery is still pending for lease=%s; local recovery state retained", claim.LeaseID)
	}
	createdAt, err := strconv.ParseInt(strings.TrimSpace(claim.Labels["create_started_at"]), 10, 64)
	if err != nil || createdAt <= 0 || !b.now().Before(time.Unix(createdAt, 0).Add(falCreateRecoveryWindow)) {
		return core.LeaseClaim{}, exit(5, "fal create recovery window expired for lease=%s; local recovery claim retained for manual provider reconciliation", claim.LeaseID)
	}
	keyPath, err := core.TestboxKeyPath(claim.LeaseID)
	if err != nil {
		return core.LeaseClaim{}, err
	}
	if _, err := os.Stat(keyPath); err != nil {
		return core.LeaseClaim{}, exit(5, "fal create recovery key is unavailable for lease=%s; local recovery claim retained", claim.LeaseID)
	}
	_, publicKey, err := core.EnsureTestboxKeyForConfig(cfg, claim.LeaseID)
	if err != nil {
		return core.LeaseClaim{}, err
	}
	instanceType := firstNonBlank(claim.Labels["server_type"], cfg.Fal.InstanceType)
	sector := claim.Labels["sector"]
	if InstanceType(instanceType) != InstanceTypeH100x8 {
		sector = ""
	}
	created, err := client.CreateInstance(ctx, CreateInstanceRequest{
		InstanceType: InstanceType(instanceType),
		SSHKey:       publicKey,
		Sector:       Sector(sector),
	}, claim.LeaseID)
	if err != nil {
		return core.LeaseClaim{}, exit(5, "fal create recovery retry failed for lease=%s; local recovery claim retained: %v", claim.LeaseID, err)
	}
	instanceID := strings.TrimSpace(created.ID)
	if instanceID == "" {
		return core.LeaseClaim{}, exit(5, "fal create recovery retry returned an empty id for lease=%s; local recovery claim retained", claim.LeaseID)
	}
	cfg.Fal.InstanceType = instanceType
	cfg.ServerType = instanceType
	cfg.Fal.Sector = sector
	updated, err := b.persistRecoveredInstanceClaim(claim, cfg, instanceID)
	if err != nil {
		persistErr := fmt.Errorf("persist recovered fal instance %s claim: %w", instanceID, err)
		current, exists, readErr := core.ReadLeaseClaimWithPresence(claim.LeaseID)
		if readErr == nil && exists && current.Provider == providerName &&
			current.ProviderScope == falClaimScope(cfg) && current.CloudID == instanceID &&
			verifyFalClaimCredential(current, cfg) == nil {
			return current, nil
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cleanupErr := core.CleanupLeaseClaimIfUnchangedAfter(claim.LeaseID, claim, true, func() error {
			return client.DeleteInstance(cleanupCtx, instanceID)
		})
		if cleanupErr == nil {
			core.RemoveStoredTestboxKey(claim.LeaseID)
			return core.LeaseClaim{}, persistErr
		}
		current, exists, rereadErr := core.ReadLeaseClaimWithPresence(claim.LeaseID)
		if rereadErr == nil && exists && current.Provider == providerName &&
			current.ProviderScope == falClaimScope(cfg) && current.CloudID == instanceID &&
			verifyFalClaimCredential(current, cfg) == nil {
			return current, nil
		}
		return core.LeaseClaim{}, errors.Join(persistErr, readErr, cleanupErr, rereadErr)
	}
	if updated.CloudID != instanceID {
		return core.LeaseClaim{}, exit(5, "fal recovered instance claim is unavailable for lease=%s", claim.LeaseID)
	}
	return updated, nil
}

func (b *backend) persistRecoveredInstanceClaim(claim core.LeaseClaim, cfg Config, instanceID string) (core.LeaseClaim, error) {
	if b.persistRecoveredClaim != nil {
		return b.persistRecoveredClaim(claim, cfg, instanceID)
	}
	return b.persistRecoveryClaimIfUnchanged(claim.LeaseID, claim.Slug, cfg, claim.RepoRoot, instanceID, "rollback-cleanup", false, claim, true)
}

func (b *backend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func (b *backend) rollbackAcquire(instanceID, leaseID, slug string, cfg Config, repoRoot, reason string, cause error) error {
	return b.rollbackAcquireWithClaimState(instanceID, leaseID, slug, cfg, repoRoot, reason, cause, false)
}

func (b *backend) rollbackClaimedAcquire(instanceID, leaseID, slug string, cfg Config, repoRoot, reason string, cause error) error {
	return b.rollbackAcquireWithClaimState(instanceID, leaseID, slug, cfg, repoRoot, reason, cause, true)
}

func (b *backend) rollbackAcquireWithClaimState(instanceID, leaseID, slug string, cfg Config, repoRoot, reason string, cause error, expectedClaim bool) error {
	claim, claimErr := b.transitionRecoveryClaim(leaseID, slug, cfg, repoRoot, instanceID, reason, false, expectedClaim)
	if errors.Is(claimErr, errFalRecoveryClaimRemoved) {
		core.RemoveStoredTestboxKey(leaseID)
		return cause
	}
	client, err := b.api()
	if err != nil {
		return rollbackAcquireError(cause, instanceID, claimErr, err)
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cleanupAction := func() error { return client.DeleteInstance(cleanupCtx, instanceID) }
	var cleanupErr error
	if claim.LeaseID != "" {
		cleanupErr = core.RemoveLeaseClaimIfUnchangedAfter(leaseID, claim, cleanupAction)
	} else {
		cleanupErr = cleanupAction()
	}
	if cleanupErr != nil {
		return rollbackAcquireError(cause, instanceID, claimErr, cleanupErr)
	}
	core.RemoveStoredTestboxKey(leaseID)
	if claimErr != nil {
		return errors.Join(cause, fmt.Errorf("persist fal recovery claim: %w", claimErr))
	}
	return cause
}

func rollbackAcquireError(cause error, instanceID string, claimErr error, cleanupErr error) error {
	errs := []error{cause}
	if claimErr != nil {
		errs = append(errs, fmt.Errorf("persist fal recovery claim: %w", claimErr))
	}
	errs = append(errs, fmt.Errorf("fal cleanup failed for instance %s: %w", instanceID, cleanupErr))
	return errors.Join(errs...)
}

func (b *backend) handleFailedAcquire(instanceID, leaseID, slug string, cfg Config, repoRoot string, keep bool, cause error) error {
	if keep {
		if _, claimErr := b.transitionRecoveryClaim(leaseID, slug, cfg, repoRoot, instanceID, "keep-failed-acquire", true, true); claimErr != nil {
			return b.rollbackClaimedAcquire(instanceID, leaseID, slug, cfg, repoRoot, "rollback-cleanup", errors.Join(
				cause,
				fmt.Errorf("persist fal keep recovery claim: %w", claimErr),
				fmt.Errorf("deleting fal instance %s because --keep recovery state could not be persisted", instanceID),
			))
		}
		return cause
	}
	return b.rollbackClaimedAcquire(instanceID, leaseID, slug, cfg, repoRoot, "rollback-cleanup", cause)
}

func (b *backend) transitionRecoveryClaim(leaseID, slug string, cfg Config, repoRoot, instanceID, reason string, keep, expectedClaim bool) (core.LeaseClaim, error) {
	current, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		return core.LeaseClaim{}, err
	}
	if !exists {
		if expectedClaim {
			return core.LeaseClaim{}, errFalRecoveryClaimRemoved
		}
		return b.persistRecoveryClaimAtIfUnchanged(leaseID, slug, cfg, repoRoot, instanceID, reason, keep, time.Time{}, core.LeaseClaim{}, false)
	}
	if current.Provider != providerName || current.ProviderScope != falClaimScope(cfg) || (current.CloudID != "" && current.CloudID != instanceID) {
		return core.LeaseClaim{}, exit(2, "fal lease %s recovery identity changed; refusing claim transition", leaseID)
	}
	if err := verifyFalClaimCredential(current, cfg); err != nil {
		return core.LeaseClaim{}, err
	}
	updated, err := b.persistRecoveryClaimAtIfUnchanged(leaseID, slug, cfg, repoRoot, instanceID, reason, keep, time.Time{}, current, true)
	if err != nil {
		return current, err
	}
	return updated, nil
}

func (b *backend) persistRecoveryClaim(leaseID, slug string, cfg Config, repoRoot, instanceID, reason string, keep bool) error {
	return b.persistRecoveryClaimAt(leaseID, slug, cfg, repoRoot, instanceID, reason, keep, time.Time{})
}

func (b *backend) persistRecoveryClaimAt(leaseID, slug string, cfg Config, repoRoot, instanceID, reason string, keep bool, createStarted time.Time) error {
	_, err := b.persistRecoveryClaimAtIfUnchanged(leaseID, slug, cfg, repoRoot, instanceID, reason, keep, createStarted, core.LeaseClaim{}, false)
	return err
}

func (b *backend) persistRecoveryClaimIfUnchanged(leaseID, slug string, cfg Config, repoRoot, instanceID, reason string, keep bool, expected core.LeaseClaim, expectedExists bool) (core.LeaseClaim, error) {
	return b.persistRecoveryClaimAtIfUnchanged(leaseID, slug, cfg, repoRoot, instanceID, reason, keep, time.Time{}, expected, expectedExists)
}

func (b *backend) persistRecoveryClaimAtIfUnchanged(leaseID, slug string, cfg Config, repoRoot, instanceID, reason string, keep bool, createStarted time.Time, expected core.LeaseClaim, expectedExists bool) (core.LeaseClaim, error) {
	createStarted = falClaimStartedAt(expected, createStarted)
	if createStarted.IsZero() {
		createStarted = b.now()
	}
	labels := falLabels(cfg, leaseID, slug, keep, createStarted)
	binding := falCredentialBinding(cfg)
	if binding == "" {
		return core.LeaseClaim{}, exit(2, "provider=%s requires fal credentials to persist recovery ownership", providerName)
	}
	labels[falCredentialBindingLabel] = binding
	labels["create_started_at"] = strconv.FormatInt(createStarted.UTC().Unix(), 10)
	labels["state"] = reason
	labels["recovery"] = reason
	server := core.Server{
		CloudID:  strings.TrimSpace(instanceID),
		Provider: providerName,
		Name:     firstNonBlank(slug, leaseID),
		Status:   reason,
		Labels:   labels,
	}
	server.ServerType.Name = cfg.Fal.InstanceType
	target := core.SSHTargetFromConfig(cfg, "")
	if repoRoot != "" {
		return core.ClaimLeaseTargetForRepoConfigScopeIfUnchanged(leaseID, slug, cfg, falClaimScope(cfg), server, target, repoRoot, cfg.IdleTimeout, false, expected, expectedExists)
	}
	return core.ClaimLeaseTargetForConfigScopeIfUnchanged(leaseID, slug, cfg, falClaimScope(cfg), server, target, cfg.IdleTimeout, expected, expectedExists)
}

func falClaimStartedAt(claim core.LeaseClaim, fallback time.Time) time.Time {
	if raw := strings.TrimSpace(claim.Labels["create_started_at"]); raw != "" {
		if unixSeconds, err := strconv.ParseInt(raw, 10, 64); err == nil && unixSeconds > 0 {
			return time.Unix(unixSeconds, 0).UTC()
		}
	}
	if raw := strings.TrimSpace(claim.Labels["created_at"]); raw != "" {
		if unixSeconds, err := strconv.ParseInt(raw, 10, 64); err == nil && unixSeconds > 0 {
			return time.Unix(unixSeconds, 0).UTC()
		}
		if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
			return parsed.UTC()
		}
	}
	return fallback.UTC()
}

func resolveFalClaim(identifier, providerScope string) (core.LeaseClaim, bool, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return core.LeaseClaim{}, false, exit(2, "provider=%s requires --id <lease-or-instance>", providerName)
	}
	claim, ok, exact, err := core.ResolveLeaseClaimForProviderScopeWithExact(identifier, providerName, providerScope)
	if err != nil || ok || exact {
		return claim, ok, err
	}
	return core.ResolveLeaseClaimForProviderCloudIDScope(identifier, providerName, providerScope)
}

func falClaims(providerScope string) ([]core.LeaseClaim, error) {
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return nil, err
	}
	out := make([]core.LeaseClaim, 0, len(claims))
	for _, claim := range claims {
		if claim.Provider == providerName && claim.ProviderScope == providerScope {
			out = append(out, claim)
		}
	}
	return out, nil
}

func falClaimScope(cfg Config) string {
	return core.ProviderClaimScope(providerName, cfg)
}

func falCredentialBinding(cfg Config) string {
	key := strings.TrimSpace(cfg.Fal.APIKey)
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("crabbox/fal/credential-binding/v1\x00" + key))
	return fmt.Sprintf("%x", sum)
}

func verifyFalClaimCredential(claim core.LeaseClaim, cfg Config) error {
	expected := strings.TrimSpace(claim.Labels[falCredentialBindingLabel])
	actual := falCredentialBinding(cfg)
	if expected == "" {
		return exit(2, "fal lease %s has no credential binding; refusing provider access", claim.LeaseID)
	}
	if actual == "" {
		return exit(2, "fal lease %s requires the credential that created it; refusing provider access", claim.LeaseID)
	}
	if expected != actual {
		return exit(2, "fal lease %s belongs to a different credential identity; refusing provider access", claim.LeaseID)
	}
	return nil
}

func falClaimServer(server core.Server, cfg Config) (core.Server, error) {
	binding := falCredentialBinding(cfg)
	if binding == "" {
		return core.Server{}, exit(2, "provider=%s requires fal credentials to persist lease ownership", providerName)
	}
	server.Labels = cloneLabels(server.Labels)
	server.Labels[falCredentialBindingLabel] = binding
	return server, nil
}

func leaseTargetFromClaimedInstance(item ComputeInstance, claim core.LeaseClaim, cfg Config, includeSSH bool) (core.LeaseTarget, error) {
	if claim.Provider != providerName {
		return core.LeaseTarget{}, exit(2, "lease %s is claimed by provider=%s; refusing fal resolve", claim.LeaseID, claim.Provider)
	}
	if claim.CloudID != "" && strings.TrimSpace(item.ID) != claim.CloudID {
		return core.LeaseTarget{}, exit(2, "refusing to resolve changed fal instance %s", claim.CloudID)
	}
	server := falServer(item, cfg, claim.LeaseID, claim.Slug, claim.Labels["keep"] == "true", falClaimStartedAt(claim, time.Now().UTC()))
	server.Labels = mergeFalClaimLabels(server.Labels, claim.Labels)
	server, err := mergeClaimEndpoint(server, claim)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	target := core.LeaseTarget{Server: server, LeaseID: claim.LeaseID}
	if includeSSH {
		ssh, err := falSSHTarget(cfg, item)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		applyClaimSSHEndpoint(&ssh, claim)
		core.UseStoredTestboxKey(&ssh, claim.LeaseID)
		target.SSH = ssh
	}
	return target, nil
}

func leaseTargetFromClaim(claim core.LeaseClaim, cfg Config, includeSSH bool) (core.LeaseTarget, error) {
	server, err := serverFromClaim(claim, cfg)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	target := core.LeaseTarget{Server: server, LeaseID: claim.LeaseID}
	if includeSSH && claim.SSHHost != "" {
		ssh := core.SSHTargetFromConfig(cfg, claim.SSHHost)
		if claim.SSHPort > 0 {
			ssh.Port = strconv.Itoa(claim.SSHPort)
		}
		core.UseStoredTestboxKey(&ssh, claim.LeaseID)
		target.SSH = ssh
	}
	return target, nil
}

func applyClaimSSHEndpoint(ssh *core.SSHTarget, claim core.LeaseClaim) {
	if claim.SSHHost != "" {
		ssh.Host = claim.SSHHost
	}
	if claim.SSHPort > 0 {
		ssh.Port = strconv.Itoa(claim.SSHPort)
	}
	if user := firstNonBlank(claim.Labels["ssh_user"], claim.StaticUser); user != "" {
		ssh.User = user
	}
}

func serverFromClaim(claim core.LeaseClaim, cfg Config) (core.Server, error) {
	if claim.Provider != providerName {
		return core.Server{}, exit(2, "lease %s is claimed by provider=%s; refusing fal cleanup", claim.LeaseID, claim.Provider)
	}
	labels := cloneLabels(claim.Labels)
	if len(labels) == 0 {
		labels = falLabels(cfg, claim.LeaseID, claim.Slug, false, time.Now().UTC())
	}
	delete(labels, falCredentialBindingLabel)
	server := core.Server{
		CloudID:  claim.CloudID,
		Provider: providerName,
		Name:     firstNonBlank(labels["name"], claim.Slug, claim.LeaseID),
		Status:   firstNonBlank(labels["state"], "unknown"),
		Labels:   labels,
	}
	server.PublicNet.IPv4.IP = claim.SSHHost
	server.ServerType.Name = firstNonBlank(labels["server_type"], cfg.Fal.InstanceType, cfg.ServerType)
	return server, nil
}

func mergeClaimEndpoint(server core.Server, claim core.LeaseClaim) (core.Server, error) {
	if claim.CloudID != "" && server.CloudID != "" && claim.CloudID != server.CloudID {
		return core.Server{}, exit(2, "refusing to list fal instance %s from stale local claim", server.CloudID)
	}
	if claim.SSHHost != "" {
		server.PublicNet.IPv4.IP = claim.SSHHost
	}
	return server, nil
}

func falServer(item ComputeInstance, cfg Config, leaseID, slug string, keep bool, createdAt time.Time) core.Server {
	labels := falLabels(cfg, leaseID, slug, keep, createdAt)
	labels["fal_instance_id"] = strings.TrimSpace(item.ID)
	labels["server_type"] = firstNonBlank(string(item.InstanceType), cfg.Fal.InstanceType, cfg.ServerType)
	labels["name"] = firstNonBlank(slug, item.ID)
	if item.Region != "" {
		labels["region"] = item.Region
	}
	if item.Sector != "" {
		labels["sector"] = string(item.Sector)
	}
	if item.CreatorUserNickname != "" {
		labels["creator_user_nickname"] = item.CreatorUserNickname
	}
	if item.IP != "" {
		labels["ssh_host"] = item.IP
	}
	labels["ssh_port"] = firstNonBlank(cfg.SSHPort, "22")
	labels["ssh_user"] = firstNonBlank(cfg.SSHUser, cfg.Fal.User, defaultUser)
	status := normalizeFalStatus(item.Status)
	labels["state"] = status
	server := core.Server{
		CloudID:  strings.TrimSpace(item.ID),
		Provider: providerName,
		Name:     firstNonBlank(slug, item.ID),
		Status:   status,
		Labels:   labels,
	}
	server.PublicNet.IPv4.IP = strings.TrimSpace(item.IP)
	server.ServerType.Name = firstNonBlank(string(item.InstanceType), cfg.Fal.InstanceType, cfg.ServerType)
	return server
}

func falLabels(cfg Config, leaseID, slug string, keep bool, now time.Time) map[string]string {
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", keep, now)
	labels["work_root"] = cfg.WorkRoot
	labels["server_type"] = firstNonBlank(cfg.Fal.InstanceType, cfg.ServerType)
	labels["sector"] = cfg.Fal.Sector
	return labels
}

func falSSHTarget(cfg Config, item ComputeInstance) (core.SSHTarget, error) {
	host := strings.TrimSpace(item.IP)
	if host == "" {
		return core.SSHTarget{}, exit(5, "fal instance %s has no SSH host", item.ID)
	}
	target := core.SSHTargetFromConfig(cfg, host)
	target.TargetOS = core.TargetLinux
	target.NetworkKind = core.NetworkPublic
	target.ReadyCheck = "command -v git >/dev/null && command -v rsync >/dev/null && command -v tar >/dev/null"
	return target, nil
}

func normalizeFalStatus(status InstanceStatus) string {
	value := strings.ToLower(strings.TrimSpace(string(status)))
	if value == "" {
		return string(InstanceStatusUnknown)
	}
	return value
}

func isFalNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == 404
}

func isAmbiguousFalMutationError(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode >= 500 || apiErr.StatusCode == 408 || apiErr.StatusCode == 409
	}
	return err != nil
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func cloneLabels(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels))
	for key, value := range labels {
		out[key] = value
	}
	return out
}

func mergeLabels(base, overlay map[string]string) map[string]string {
	out := cloneLabels(base)
	for key, value := range overlay {
		if strings.TrimSpace(value) != "" {
			out[key] = value
		}
	}
	return out
}

func mergeFalClaimLabels(live, claim map[string]string) map[string]string {
	out := mergeLabels(live, claim)
	delete(out, falCredentialBindingLabel)
	for _, key := range []string{"creator_user_nickname", "fal_instance_id", "region", "sector", "server_type", "state"} {
		if value := strings.TrimSpace(live[key]); value != "" {
			out[key] = value
		} else {
			delete(out, key)
		}
	}
	return out
}
