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
	falCreateRequestTimeout     = 30 * time.Second
	falCredentialBindingLabel   = "fal_credential_binding"
	falCreateAttemptLabel       = "fal_create_attempt"
	falCreateRequestLabel       = "fal_create_request_binding"
)

var (
	errFalProviderAbsenceNotAccountBound = errors.New("fal provider absence is not account-bound")
	errFalRecoveryClaimRemoved           = errors.New("fal recovery claim was removed by concurrent cleanup")
)

func (b *backend) Acquire(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	cfg := b.configForRun()
	cfg.Fal.InstanceType = strings.TrimSpace(cfg.Fal.InstanceType)
	cfg.ServerType = cfg.Fal.InstanceType
	cfg.Fal.Sector = strings.TrimSpace(cfg.Fal.Sector)
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
	if err := b.syncFalCreateKey(leaseID); err != nil {
		return core.LeaseTarget{}, errors.Join(fmt.Errorf("sync fal create key before intent: %w", err), core.RemoveStoredTestboxKeyWithError(leaseID))
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
	intentClaim, claimErr := b.persistInitialFalCreateIntent(leaseID, slug, cfg, req.Repo.Root, req.Keep, createStarted, createRequest)
	if claimErr != nil {
		cleanupErr := b.cleanupRejectedFalCreateIntent(intentClaim)
		if cleanupErr == nil {
			cleanupErr = removeFalKeyIfClaimAbsent(leaseID)
		}
		return core.LeaseTarget{}, errors.Join(fmt.Errorf("persist fal create intent before provider mutation: %w", claimErr), cleanupErr)
	}
	created, initialClaim, ambiguous, createErr := b.replayFalCreateWithClaim(ctx, client, createRequest, intentClaim, cfg, "provisioning", req.Keep)
	needsBind := strings.TrimSpace(created.ID) != "" && createErr != nil
	if createErr != nil && !ambiguous && !needsBind {
		return core.LeaseTarget{}, createErr
	}
	if ambiguous {
		created, initialClaim, createErr = b.reconcileAmbiguousCreate(ctx, client, createRequest, initialClaim, cfg, req.Keep, createErr)
		needsBind = strings.TrimSpace(created.ID) != "" && createErr != nil
		if createErr != nil && !needsBind {
			return core.LeaseTarget{}, createErr
		}
	}
	instanceID := strings.TrimSpace(created.ID)
	var bindErr error
	if needsBind {
		base := initialClaim
		if base.LeaseID == "" {
			base = intentClaim
		}
		var bound bool
		initialClaim, bound, bindErr = b.adoptOrBindKnownFalInstance(base, cfg, instanceID, "provisioning", req.Keep)
		if bindErr == nil && !bound {
			bindErr = errFalRecoveryClaimRemoved
		}
	}
	if bindErr != nil {
		cause := fmt.Errorf("persist fal provisioning claim after creating instance %s: %w", instanceID, bindErr)
		return core.LeaseTarget{}, b.cleanupKnownFalCreateAfterBindFailure(ctx, client, intentClaim, cfg, instanceID, "provisioning", req.Keep, cause)
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

func (b *backend) reconcileAmbiguousCreate(ctx context.Context, client computeAPI, req CreateInstanceRequest, claim core.LeaseClaim, cfg Config, keep bool, cause error) (ComputeInstance, core.LeaseClaim, error) {
	var lastErr error
	for attempt := 1; attempt <= falCreateReconcileAttempts; attempt++ {
		instance, updated, ambiguous, err := b.replayFalCreateWithClaim(ctx, client, req, claim, cfg, "provisioning", keep)
		claim = updated
		if err == nil && strings.TrimSpace(instance.ID) != "" {
			return instance, claim, nil
		}
		if strings.TrimSpace(instance.ID) != "" {
			return instance, claim, errors.Join(cause, fmt.Errorf("persist fal idempotent create result: %w", err))
		}
		lastErr = err
		if !ambiguous {
			return ComputeInstance{}, claim, errors.Join(cause, fmt.Errorf("fal idempotent create retry failed: %w", err))
		}
		createStarted := falClaimStartedAt(claim, time.Time{})
		if createStarted.IsZero() || !b.now().Before(createStarted.Add(falCreateRecoveryWindow)) {
			return ComputeInstance{}, claim, errors.Join(
				fmt.Errorf("fal instance creation remains indeterminate after the provider idempotency replay window expired; no provider id was returned"),
				cause,
				lastErr,
			)
		}
		if attempt == falCreateReconcileAttempts {
			break
		}
		timer := time.NewTimer(falCreateReconcileRetryWait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ComputeInstance{}, claim, errors.Join(cause, ctx.Err())
		case <-timer.C:
		}
	}
	return ComputeInstance{}, claim, errors.Join(
		fmt.Errorf("fal instance creation remains indeterminate after idempotent retry; no provider id was returned"),
		cause,
		lastErr,
	)
}

func (b *backend) replayFalCreateWithClaim(ctx context.Context, client computeAPI, req CreateInstanceRequest, claim core.LeaseClaim, cfg Config, successReason string, keep bool) (ComputeInstance, core.LeaseClaim, bool, error) {
	if claim.LeaseID == "" || claim.CloudID != "" {
		return ComputeInstance{}, claim, false, exit(2, "fal create replay requires an identityless recovery claim")
	}
	if claim.Provider != providerName || claim.ProviderScope != falClaimScope(cfg) {
		return ComputeInstance{}, claim, false, exit(2, "fal lease %s create replay ownership changed", claim.LeaseID)
	}
	if err := verifyFalClaimCredential(claim, cfg); err != nil {
		return ComputeInstance{}, claim, false, err
	}
	if err := verifyFalCreateRequestBinding(claim, req); err != nil {
		return ComputeInstance{}, claim, false, err
	}
	recovery := strings.TrimSpace(claim.Labels["recovery"])
	if recovery != "create-intent" && recovery != "ambiguous-create" && recovery != "ambiguous-create-inflight" {
		return ComputeInstance{}, claim, false, exit(4, "fal recovery is still pending for lease=%s; local recovery state retained", claim.LeaseID)
	}
	createStarted := falClaimStartedAt(claim, time.Time{})
	createCtx, cancel, err := b.createReplayContext(ctx, createStarted)
	if err != nil {
		return ComputeInstance{}, claim, false, exit(5, "fal create recovery window expired for lease=%s; local recovery claim retained for manual provider reconciliation", claim.LeaseID)
	}
	defer cancel()
	hadPriorAttempt := recovery != "create-intent" || strings.TrimSpace(claim.Labels[falCreateAttemptLabel]) != ""

	inflight, err := b.newFalRecoveryClaim(claim, cfg, "", "ambiguous-create-inflight", keep)
	if err != nil {
		return ComputeInstance{}, claim, false, err
	}
	inflight.Labels[falCreateAttemptLabel] = core.NewLeaseID()
	if err := core.ReplaceLeaseClaimIfUnchangedDurable(claim.LeaseID, claim, inflight); err != nil {
		return ComputeInstance{}, claim, false, fmt.Errorf("claim fal create replay attempt: %w", err)
	}

	var created ComputeInstance
	var createErr error
	updated, exists, mutationErr := core.ReplaceOrRemoveLeaseClaimIfUnchangedAfter(claim.LeaseID, inflight, func(current core.LeaseClaim) (core.LeaseClaim, bool, error) {
		created, createErr = client.CreateInstance(createCtx, req, claim.LeaseID)
		instanceID := strings.TrimSpace(created.ID)
		if createErr == nil && instanceID == "" {
			createErr = exit(5, "fal idempotent create returned an empty id")
		}
		if instanceID == "" && createErr != nil && !isAmbiguousFalMutationError(createErr) && !hadPriorAttempt {
			return core.LeaseClaim{}, true, nil
		}
		reason := successReason
		if instanceID == "" {
			reason = "ambiguous-create"
		}
		replacement, err := b.newFalRecoveryClaim(current, cfg, instanceID, reason, keep)
		if err != nil {
			return core.LeaseClaim{}, false, err
		}
		replacement.Labels[falCreateAttemptLabel] = current.Labels[falCreateAttemptLabel]
		return replacement, false, nil
	})
	if mutationErr != nil {
		if current, currentExists, readErr := core.ReadLeaseClaimWithPresence(claim.LeaseID); readErr == nil && currentExists &&
			current.Provider == providerName && current.ProviderScope == falClaimScope(cfg) && verifyFalClaimCredential(current, cfg) == nil {
			updated = current
			exists = true
		}
		outcomeErr := fmt.Errorf("persist fal create replay outcome: %w", mutationErr)
		if createErr != nil {
			outcomeErr = errors.Join(createErr, outcomeErr)
		}
		if !exists && createErr != nil && !isAmbiguousFalMutationError(createErr) && !hadPriorAttempt {
			outcomeErr = errors.Join(outcomeErr, removeFalKeyIfClaimAbsent(claim.LeaseID))
		}
		return created, updated, createErr != nil && isAmbiguousFalMutationError(createErr), outcomeErr
	}
	if strings.TrimSpace(created.ID) == "" && createErr != nil && !isAmbiguousFalMutationError(createErr) {
		if !exists {
			if keyErr := removeFalKeyIfClaimAbsent(claim.LeaseID); keyErr != nil {
				return ComputeInstance{}, updated, false, errors.Join(createErr, keyErr)
			}
		}
		return ComputeInstance{}, updated, false, createErr
	}
	if strings.TrimSpace(created.ID) != "" && createErr != nil {
		return created, updated, false, createErr
	}
	if createErr != nil {
		return ComputeInstance{}, updated, true, createErr
	}
	return created, updated, false, nil
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
		if claim.Labels["recovery"] == "create-intent" {
			return leaseTargetFromClaim(claim, cfg, false)
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
		if claim.Labels["recovery"] == "create-intent" {
			if req.Lease.Server.CloudID != "" {
				return exit(2, "refusing to cancel fal create intent %s from stale provider identity", leaseID)
			}
			if err := core.RemoveLeaseClaimIfUnchanged(leaseID, claim); err != nil {
				return err
			}
			return removeFalKeyIfClaimAbsent(leaseID)
		}
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
	return removeFalKeyIfClaimAbsent(leaseID)
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
		if err := removeFalKeyIfClaimAbsent(claim.LeaseID); err != nil {
			return err
		}
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

func (b *backend) syncFalCreateKey(leaseID string) error {
	if b.syncCreateKey != nil {
		return b.syncCreateKey(leaseID)
	}
	return core.SyncStoredTestboxKey(leaseID)
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
	if claim.Labels["recovery"] != "ambiguous-create" && claim.Labels["recovery"] != "ambiguous-create-inflight" {
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
	publicKeyData, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		return core.LeaseClaim{}, exit(5, "fal create recovery public key is unavailable for lease=%s; local recovery claim retained", claim.LeaseID)
	}
	publicKey := strings.TrimSpace(string(publicKeyData))
	if publicKey == "" {
		return core.LeaseClaim{}, exit(5, "fal create recovery public key is empty for lease=%s; local recovery claim retained", claim.LeaseID)
	}
	instanceType := firstNonBlank(claim.Labels["server_type"], cfg.Fal.InstanceType)
	sector := claim.Labels["sector"]
	if InstanceType(instanceType) != InstanceTypeH100x8 {
		sector = ""
	}
	cfg.Fal.InstanceType = instanceType
	cfg.ServerType = instanceType
	cfg.Fal.Sector = sector
	created, updated, ambiguous, replayErr := b.replayFalCreateWithClaim(ctx, client, CreateInstanceRequest{
		InstanceType: InstanceType(instanceType),
		SSHKey:       publicKey,
		Sector:       Sector(sector),
	}, claim, cfg, "rollback-cleanup", false)
	instanceID := strings.TrimSpace(created.ID)
	if replayErr == nil {
		if instanceID == "" || updated.CloudID != instanceID {
			return core.LeaseClaim{}, exit(5, "fal recovered instance claim is unavailable for lease=%s", claim.LeaseID)
		}
		return updated, nil
	}
	if instanceID == "" {
		if ambiguous {
			return core.LeaseClaim{}, exit(5, "fal create recovery retry failed for lease=%s; local recovery claim retained: %v", claim.LeaseID, replayErr)
		}
		return core.LeaseClaim{}, replayErr
	}
	persistErr := fmt.Errorf("persist recovered fal instance %s claim: %w", instanceID, replayErr)
	base := updated
	if base.LeaseID == "" {
		base = claim
	}
	owned, exists, ownerErr := b.adoptOrBindKnownFalInstance(base, cfg, instanceID, "rollback-cleanup", false)
	if ownerErr == nil && exists {
		return owned, nil
	}
	if ownerErr == nil && !exists {
		return core.LeaseClaim{}, b.rollbackAcquireAfterClaimRemoval(instanceID, claim.LeaseID, claim.Slug, cfg, claim.RepoRoot, "rollback-cleanup", persistErr)
	}
	current, currentExists, readErr := core.ReadLeaseClaimWithPresence(claim.LeaseID)
	if readErr == nil && currentExists && current.Provider == providerName && current.ProviderScope == falClaimScope(cfg) &&
		current.CloudID == "" && verifyFalClaimCredential(current, cfg) == nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cleanupErr := core.CleanupLeaseClaimIfUnchangedAfter(claim.LeaseID, current, true, func() error {
			return client.DeleteInstance(cleanupCtx, instanceID)
		})
		if cleanupErr == nil {
			return core.LeaseClaim{}, errors.Join(persistErr, removeFalKeyIfClaimAbsent(claim.LeaseID))
		}
		owned, exists, retryOwnerErr := b.adoptOrBindKnownFalInstance(current, cfg, instanceID, "rollback-cleanup", false)
		if retryOwnerErr == nil && exists {
			return owned, nil
		}
		if retryOwnerErr == nil && !exists {
			return core.LeaseClaim{}, errors.Join(
				b.rollbackAcquireAfterClaimRemoval(instanceID, claim.LeaseID, claim.Slug, cfg, claim.RepoRoot, "rollback-cleanup", persistErr),
				cleanupErr,
			)
		}
		return core.LeaseClaim{}, errors.Join(persistErr, ownerErr, cleanupErr, retryOwnerErr)
	}
	return core.LeaseClaim{}, errors.Join(persistErr, ownerErr, readErr)
}

func (b *backend) persistInitialFalCreateIntent(leaseID, slug string, cfg Config, repoRoot string, keep bool, createStarted time.Time, req CreateInstanceRequest) (core.LeaseClaim, error) {
	if b.persistCreateIntent != nil {
		return b.persistCreateIntent(leaseID, slug, cfg, repoRoot, keep, createStarted, req)
	}
	claim, err := b.persistRecoveryClaimAtIfUnchanged(
		leaseID,
		slug,
		cfg,
		repoRoot,
		"",
		"create-intent",
		keep,
		createStarted,
		core.LeaseClaim{},
		false,
		req,
	)
	if err != nil {
		return claim, err
	}
	if err := core.ReplaceLeaseClaimIfUnchangedDurable(leaseID, claim, claim); err != nil {
		return claim, fmt.Errorf("sync fal create intent: %w", err)
	}
	return claim, nil
}

func (b *backend) cleanupRejectedFalCreateIntent(intent core.LeaseClaim) error {
	if intent.LeaseID == "" {
		return nil
	}
	_, exists, err := core.ReadLeaseClaimWithPresence(intent.LeaseID)
	if err != nil || !exists {
		return err
	}
	if err := core.RemoveLeaseClaimIfUnchanged(intent.LeaseID, intent); err != nil {
		return fmt.Errorf("clean up rejected fal create intent: %w", err)
	}
	return nil
}

func removeFalKeyIfClaimAbsent(leaseID string) error {
	return core.FinalizeAbsentLeaseClaimAfterSync(leaseID, func() error {
		return core.RemoveStoredTestboxKeyWithError(leaseID)
	})
}

func falRecoveryClaimReplacement(current core.LeaseClaim, cfg Config, instanceID, reason string, keep bool) (core.LeaseClaim, error) {
	createStarted := falClaimStartedAt(current, time.Time{})
	if createStarted.IsZero() {
		return core.LeaseClaim{}, exit(2, "fal lease %s recovery claim has no create timestamp", current.LeaseID)
	}
	labels := falLabels(cfg, current.LeaseID, current.Slug, keep, createStarted)
	binding := falCredentialBinding(cfg)
	if binding == "" {
		return core.LeaseClaim{}, exit(2, "provider=%s requires fal credentials to persist recovery ownership", providerName)
	}
	labels[falCredentialBindingLabel] = binding
	if requestBinding := strings.TrimSpace(current.Labels[falCreateRequestLabel]); requestBinding != "" {
		labels[falCreateRequestLabel] = requestBinding
	}
	labels["create_started_at"] = strconv.FormatInt(createStarted.UTC().Unix(), 10)
	labels["state"] = reason
	labels["recovery"] = reason
	replacement := current
	replacement.Provider = providerName
	replacement.ProviderScope = falClaimScope(cfg)
	replacement.CloudID = strings.TrimSpace(instanceID)
	replacement.Labels = labels
	return replacement, nil
}

func falCreateRequestBinding(req CreateInstanceRequest) string {
	value := string(req.InstanceType) + "\x00" + string(req.Sector) + "\x00" + req.SSHKey
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("sha256:%x", sum[:])
}

func verifyFalCreateRequestBinding(claim core.LeaseClaim, req CreateInstanceRequest) error {
	want := strings.TrimSpace(claim.Labels[falCreateRequestLabel])
	if want == "" {
		return exit(5, "fal create request binding is unavailable for lease=%s; local recovery claim retained", claim.LeaseID)
	}
	if got := falCreateRequestBinding(req); got != want {
		return exit(5, "fal create request changed for lease=%s; local recovery claim retained", claim.LeaseID)
	}
	return nil
}

func (b *backend) newFalRecoveryClaim(current core.LeaseClaim, cfg Config, instanceID, reason string, keep bool) (core.LeaseClaim, error) {
	if b.recoveryClaimReplacement != nil {
		return b.recoveryClaimReplacement(current, cfg, instanceID, reason, keep)
	}
	return falRecoveryClaimReplacement(current, cfg, instanceID, reason, keep)
}

func (b *backend) adoptOrBindKnownFalInstance(intent core.LeaseClaim, cfg Config, instanceID, reason string, keep bool) (core.LeaseClaim, bool, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		current, exists, err := core.ReadLeaseClaimWithPresence(intent.LeaseID)
		if err != nil {
			return core.LeaseClaim{}, false, err
		}
		if !exists {
			return core.LeaseClaim{}, false, nil
		}
		if current.Provider != providerName || current.ProviderScope != falClaimScope(cfg) {
			return core.LeaseClaim{}, true, exit(2, "fal lease %s recovery ownership changed", intent.LeaseID)
		}
		if err := verifyFalClaimCredential(current, cfg); err != nil {
			return core.LeaseClaim{}, true, err
		}
		if current.CloudID != "" {
			if current.CloudID != instanceID {
				return core.LeaseClaim{}, true, exit(2, "fal lease %s recovery instance changed from %s to %s", intent.LeaseID, instanceID, current.CloudID)
			}
		}
		replacement, replaceErr := b.newFalRecoveryClaim(current, cfg, instanceID, reason, keep)
		if replaceErr == nil {
			replaceErr = core.ReplaceLeaseClaimIfUnchangedDurable(intent.LeaseID, current, replacement)
		}
		if replaceErr == nil {
			return replacement, true, nil
		}
		lastErr = replaceErr
	}
	return core.LeaseClaim{}, true, fmt.Errorf("bind fal instance %s to recovery claim %s: %w", instanceID, intent.LeaseID, lastErr)
}

func (b *backend) cleanupKnownFalCreateAfterBindFailure(ctx context.Context, client computeAPI, intent core.LeaseClaim, cfg Config, instanceID, reason string, keep bool, cause error) error {
	current, exists, readErr := core.ReadLeaseClaimWithPresence(intent.LeaseID)
	if readErr != nil {
		return errors.Join(cause, readErr)
	}
	if !exists {
		return b.rollbackAcquireAfterClaimRemoval(instanceID, intent.LeaseID, intent.Slug, cfg, intent.RepoRoot, "rollback-cleanup", cause)
	}
	if current.Provider != providerName || current.ProviderScope != falClaimScope(cfg) ||
		(current.CloudID != "" && current.CloudID != instanceID) {
		return errors.Join(cause, exit(2, "fal lease %s recovery identity changed; refusing cleanup of instance %s", intent.LeaseID, instanceID))
	}
	if err := verifyFalClaimCredential(current, cfg); err != nil {
		return errors.Join(cause, err)
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	cleanupErr := core.CleanupLeaseClaimIfUnchangedAfter(intent.LeaseID, current, true, func() error {
		deleteErr := client.DeleteInstance(cleanupCtx, instanceID)
		if isFalNotFound(deleteErr) {
			return errFalProviderAbsenceNotAccountBound
		}
		return deleteErr
	})
	if cleanupErr == nil {
		return errors.Join(cause, removeFalKeyIfClaimAbsent(intent.LeaseID))
	}
	owned, ownedExists, ownerErr := b.adoptOrBindKnownFalInstance(current, cfg, instanceID, reason, keep)
	if ownerErr == nil && ownedExists {
		return errors.Join(cause, fmt.Errorf("fal cleanup failed for instance %s: %w", instanceID, cleanupErr), fmt.Errorf("fal instance retained by recovery claim %s", owned.LeaseID))
	}
	if ownerErr == nil && !ownedExists {
		return errors.Join(
			b.rollbackAcquireAfterClaimRemoval(instanceID, intent.LeaseID, intent.Slug, cfg, intent.RepoRoot, "rollback-cleanup", cause),
			cleanupErr,
		)
	}
	return errors.Join(cause, fmt.Errorf("fal cleanup failed for instance %s: %w", instanceID, cleanupErr), ownerErr)
}

func (b *backend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func (b *backend) createReplayContext(ctx context.Context, createStarted time.Time) (context.Context, context.CancelFunc, error) {
	wallNow := time.Now()
	remaining := createStarted.Add(falCreateRecoveryWindow).Sub(b.now())
	if remaining <= 0 {
		return nil, nil, context.DeadlineExceeded
	}
	if remaining > falCreateRequestTimeout {
		remaining = falCreateRequestTimeout
	}
	replayCtx, cancel := context.WithDeadline(ctx, wallNow.Add(remaining))
	return replayCtx, cancel, nil
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
		return b.rollbackAcquireAfterClaimRemoval(instanceID, leaseID, slug, cfg, repoRoot, reason, cause)
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
	if keyErr := removeFalKeyIfClaimAbsent(leaseID); keyErr != nil {
		return rollbackAcquireError(cause, instanceID, claimErr, keyErr)
	}
	if claimErr != nil {
		return errors.Join(cause, fmt.Errorf("persist fal recovery claim: %w", claimErr))
	}
	return cause
}

func (b *backend) rollbackAcquireAfterClaimRemoval(instanceID, leaseID, slug string, cfg Config, repoRoot, reason string, cause error) error {
	client, err := b.api()
	if err != nil {
		return b.retainKnownInstanceAfterClaimRemoval(instanceID, leaseID, slug, cfg, repoRoot, reason, cause, err)
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cleanupErr := core.CleanupLeaseClaimIfUnchangedAfter(leaseID, core.LeaseClaim{}, false, func() error {
		live, getErr := client.GetInstance(cleanupCtx, instanceID)
		if isFalNotFound(getErr) {
			ids, inventoryErr := falInventoryIDs(cleanupCtx, client)
			if inventoryErr != nil {
				return errors.Join(errFalProviderAbsenceNotAccountBound, inventoryErr)
			}
			for _, id := range ids {
				if id != instanceID {
					continue
				}
				deleteErr := client.DeleteInstance(cleanupCtx, instanceID)
				if isFalNotFound(deleteErr) {
					return errFalProviderAbsenceNotAccountBound
				}
				return deleteErr
			}
			return nil
		}
		if getErr != nil {
			return getErr
		}
		if strings.TrimSpace(live.ID) != instanceID {
			return exit(2, "refusing rollback for fal instance %s after provider identity changed", instanceID)
		}
		deleteErr := client.DeleteInstance(cleanupCtx, instanceID)
		if isFalNotFound(deleteErr) {
			return errFalProviderAbsenceNotAccountBound
		}
		return deleteErr
	})
	if cleanupErr == nil {
		return errors.Join(cause, removeFalKeyIfClaimAbsent(leaseID))
	}
	current, exists, readErr := core.ReadLeaseClaimWithPresence(leaseID)
	if readErr == nil && exists && current.Provider == providerName && current.ProviderScope == falClaimScope(cfg) &&
		(current.CloudID == "" || current.CloudID == instanceID) && verifyFalClaimCredential(current, cfg) == nil {
		return errors.Join(cause, fmt.Errorf("fal rollback was superseded by a concurrent recovery claim for instance %s", instanceID))
	}
	return b.retainKnownInstanceAfterClaimRemoval(instanceID, leaseID, slug, cfg, repoRoot, reason, cause, errors.Join(cleanupErr, readErr))
}

func (b *backend) retainKnownInstanceAfterClaimRemoval(instanceID, leaseID, slug string, cfg Config, repoRoot, reason string, cause, cleanupErr error) error {
	_, claimErr := b.persistRecoveryClaimAtIfUnchanged(
		leaseID,
		slug,
		cfg,
		repoRoot,
		instanceID,
		reason,
		false,
		time.Time{},
		core.LeaseClaim{},
		false,
	)
	return rollbackAcquireError(cause, instanceID, claimErr, cleanupErr)
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

func (b *backend) persistRecoveryClaimAtIfUnchanged(leaseID, slug string, cfg Config, repoRoot, instanceID, reason string, keep bool, createStarted time.Time, expected core.LeaseClaim, expectedExists bool, createRequest ...CreateInstanceRequest) (core.LeaseClaim, error) {
	if len(createRequest) > 1 {
		return core.LeaseClaim{}, exit(2, "fal recovery claim accepts at most one create request binding")
	}
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
	if len(createRequest) == 1 {
		labels[falCreateRequestLabel] = falCreateRequestBinding(createRequest[0])
	} else if requestBinding := strings.TrimSpace(expected.Labels[falCreateRequestLabel]); requestBinding != "" {
		labels[falCreateRequestLabel] = requestBinding
	}
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
