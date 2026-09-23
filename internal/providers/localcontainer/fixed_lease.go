package localcontainer

import (
	"context"
	"errors"
	"fmt"
	"github.com/openclaw/crabbox/internal/providers/shared"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

const fixedLocalContainerIntentVersion = 1

var fixedLocalContainerLeaseKind = core.FixedLeaseKind{
	ClaimProvider:  core.FixedLocalContainerClaimProvider,
	IntentVersion:  fixedLocalContainerIntentVersion,
	Label:          "local-container",
	ResourcePlural: "local containers",
}

func isLocalContainerClaimProvider(provider string) bool {
	return provider == providerName || provider == core.FixedLocalContainerClaimProvider
}

func isReleasedFixedLocalContainerClaim(claim core.LeaseClaim) bool {
	return fixedLocalContainerLeaseKind.IsFixedClaim(claim) && claim.FixedCreateIntent.State == "released"
}

func fixedLocalContainerFingerprint(cfg core.Config, req core.AcquireRequest, publicKey string) (string, error) {
	cacheVolumes, err := localContainerCacheVolumeMounts(cfg.Cache.Volumes)
	if err != nil {
		return "", err
	}
	volumes := make([]string, len(cfg.LocalContainer.Volumes))
	for i, volume := range cfg.LocalContainer.Volumes {
		volumes[i] = strings.TrimSpace(volume)
	}
	architecture := ""
	if core.IsArchitectureExplicit(cfg) {
		architecture = cfg.Architecture
	}
	intent := core.FixedIntentFields{
		{Name: "runtime", Value: strings.TrimSpace(cfg.LocalContainer.Runtime)},
		{Name: "runtimeScope", Value: checkpointScopeFromMetadata(cfg.LocalContainer.CheckpointMetadata, cfg.LocalContainer.Runtime)},
		{Name: "image", Value: strings.TrimSpace(cfg.LocalContainer.Image)},
		{Name: "forkImageID", Value: strings.TrimSpace(cfg.LocalContainer.CheckpointMetadata[checkpointMetadataForkID]), OmitEmpty: true},
		{Name: "user", Value: strings.TrimSpace(cfg.LocalContainer.User)},
		{Name: "workRoot", Value: strings.TrimSpace(cfg.LocalContainer.WorkRoot)},
		{Name: "cpus", Value: cfg.LocalContainer.CPUs},
		{Name: "memory", Value: strings.TrimSpace(cfg.LocalContainer.Memory)},
		{Name: "network", Value: strings.TrimSpace(cfg.LocalContainer.Network)},
		{Name: "dockerSocket", Value: cfg.LocalContainer.DockerSocket},
		{Name: "noHostname", Value: cfg.LocalContainer.NoHostname, OmitEmpty: true},
		{Name: "hostVolumes", Value: volumes, OmitEmpty: true},
		{Name: "cacheVolumes", Value: cacheVolumes, OmitEmpty: true},
		{Name: "desktop", Value: cfg.Desktop},
		{Name: "desktopEnv", Value: core.NormalizedDesktopEnv(cfg.DesktopEnv)},
		{Name: "browser", Value: cfg.Browser},
		{Name: "architecture", Value: architecture, OmitEmpty: true},
		{Name: "requestedSlug", Value: core.NormalizeLeaseSlug(req.RequestedSlug), OmitEmpty: true},
		{Name: "pond", Value: core.NormalizePondName(cfg.Pond), OmitEmpty: true},
		{Name: "keep", Value: req.Keep},
		{Name: "ttlNanoseconds", Value: cfg.TTL.Nanoseconds()},
		{Name: "idleNanoseconds", Value: cfg.IdleTimeout.Nanoseconds()},
		{Name: "sshPublicKey", Value: strings.TrimSpace(publicKey)},
	}
	fingerprint, err := core.FixedIntentFingerprint("", intent)
	if err != nil {
		return "", fmt.Errorf("fingerprint fixed local-container create intent: %w", err)
	}
	return fingerprint, nil
}

func (b *backend) acquireFixed(ctx context.Context, req core.AcquireRequest, cfg core.Config) (core.LeaseTarget, error) {
	leaseID := strings.TrimSpace(req.RequestedLeaseID)
	var fingerprint, publicKey string
	var recoveringUnboundAttempt bool
	var pendingClaim core.LeaseClaim
	var pendingLease core.LeaseTarget
	rememberPending := func(claim *core.LeaseClaim, lease core.LeaseTarget) {
		if !isPendingLocalContainerClaim(*claim) || claim.CloudID != lease.Server.CloudID {
			return
		}
		pendingClaim = core.CloneLeaseClaim(*claim)
		pendingLease = lease
	}
	acquired, err := core.AcquireFixedResource(ctx, core.FixedAcquireOptions{
		Kind:         fixedLocalContainerLeaseKind,
		LeaseID:      leaseID,
		CheckpointID: req.RequestedCheckpointID,
		RepoRoot:     req.Repo.Root,
		Reclaim:      req.Reclaim,
		TargetOS:     cfg.TargetOS,
		WindowsMode:  cfg.WindowsMode,
		TTL:          cfg.TTL,
		IdleTimeout:  cfg.IdleTimeout,
	}, core.FixedLeaseOperations[inspectContainer]{Admission: &core.FixedAdmission{}, DescribeIntent: func(ctx context.Context, _ *core.LeaseClaim, exists bool) (core.FixedLeaseBinding, error) {
		providerScope := strings.TrimSpace(b.claimScope(ctx))
		if providerScope == "" {
			return core.FixedLeaseBinding{}, core.Exit(2, "local-container runtime scope is unavailable; refusing to create an unscoped lease")
		}
		keyPath, key, err := core.EnsureTestboxKeyForConfig(cfg, leaseID)
		if err != nil {
			return core.FixedLeaseBinding{}, err
		}
		cfg.SSHKey, publicKey = keyPath, key
		fingerprint, err = fixedLocalContainerFingerprint(cfg, req, publicKey)
		if err != nil {
			return core.FixedLeaseBinding{}, err
		}
		binding := core.FixedLeaseBinding{ProviderScope: providerScope, Fingerprint: fingerprint}
		if exists {
			return binding, nil
		}
		containers, err := b.listContainers(ctx)
		if err != nil {
			return core.FixedLeaseBinding{}, err
		}
		servers := make([]core.Server, 0, len(containers))
		for _, container := range containers {
			servers = append(servers, b.serverFromContainer(container, cfg))
		}
		binding.AllocateSlug, binding.RequestedSlug, binding.Inventory = true, req.RequestedSlug, servers
		return binding, nil
	}, ObserveExact: func(ctx context.Context, tx *core.FixedTransaction, _ core.FixedObserveMode) (core.FixedObservation[inspectContainer], error) {
		claim, intent := tx.Claim, tx.Claim.FixedCreateIntent
		var result core.FixedObservation[inspectContainer]
		containers, err := b.listContainers(ctx)
		if err != nil {
			return result, err
		}
		for _, container := range containers {
			if container.Config.Labels["lease"] == leaseID {
				result.Candidates = append(result.Candidates, container)
			}
		}
		if len(result.Candidates) > 1 {
			return result, nil
		}
		recoveringUnboundAttempt = len(result.Candidates) == 1 && claim.CloudID == ""
		name := core.LeaseProviderName(leaseID, intent.Slug)
		if intent.Attempt != nil && intent.Attempt["container_name"] != name {
			return result, core.Exit(4, "lease_id_conflict: fixed local-container lease %s has an invalid durable create attempt", leaseID)
		}
		if len(result.Candidates) == 1 {
			container := result.Candidates[0]
			if intent.Attempt == nil {
				return result, core.Exit(4, "lease_id_conflict: fixed local-container lease %s has no durable create attempt", leaseID)
			}
			if (intent.State == "acquired" || claim.CloudID != "") && claim.CloudID != container.ID {
				return result, core.Exit(4, "lease_id_conflict: fixed local-container lease %s does not match its bound container %s", leaseID, blank(claim.CloudID, "<empty>"))
			}
			if err := validateFixedLocalContainer(container, cfg, leaseID, intent.Slug, fingerprint); err != nil {
				return result, err
			}
			if container.State.Running {
				if err := validateLocalContainerInspectedMounts(container); err != nil {
					return result, err
				}
			}
			return result, nil
		}
		if intent.State == "acquired" || claim.CloudID != "" {
			return result, core.Exit(4, "lease_id_conflict: acquired fixed local-container lease %s is missing its bound container", leaseID)
		}
		if intent.Attempt != nil {
			return result, core.Exit(4, "lease_id_conflict: fixed local-container lease %s has an unresolved create attempt", leaseID)
		}
		result.CanSubmit = true
		return result, nil
	}, Plan: func(ctx context.Context, claim core.LeaseClaim) (core.FixedAttemptPlan, error) {
		return core.FixedAttemptPlan{Values: map[string]string{"container_name": core.LeaseProviderName(leaseID, claim.Slug)}}, nil
	}, Submit: func(ctx context.Context, tx *core.FixedTransaction) (inspectContainer, error) {
		claim, intent := tx.Claim, tx.Claim.FixedCreateIntent
		name := intent.Attempt["container_name"]
		fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s runtime=%s image=%s keep=%v fixed=true\n", providerName, leaseID, intent.Slug, cfg.LocalContainer.Runtime, cfg.LocalContainer.Image, req.Keep)
		containerID, bootstrapDir, createErr := b.createContainerWithFixedIntent(ctx, cfg, name, leaseID, intent.Slug, publicKey, fingerprint, req.Keep)
		if containerID == "" {
			return inspectContainer{}, createErr
		}
		pending := createdPendingLease(cfg, containerID, leaseID, intent.Slug, bootstrapDir, req.Keep)
		pending.Server.Labels["fixed_intent_sha256"] = fingerprint
		if err := tx.Observe(core.FixedResourceBinding{CloudID: containerID, AttemptIdentityKey: "container_id", Labels: pending.Server.Labels}); err != nil {
			return inspectContainer{}, err
		}

		rememberPending(claim, pending)
		if createErr != nil {
			return inspectContainer{}, createErr
		}
		container, err := b.inspectContainer(ctx, containerID)
		if err != nil {
			return inspectContainer{}, err
		}
		if err := validateFixedLocalContainer(container, cfg, leaseID, intent.Slug, fingerprint); err != nil {
			return inspectContainer{}, err
		}
		return container, nil
	}, PrepareAccess: func(ctx context.Context, tx *core.FixedTransaction, container inspectContainer) (core.LeaseTarget, error) {
		claim, intent := tx.Claim, tx.Claim.FixedCreateIntent
		if containerID := intent.Attempt["container_id"]; containerID != "" && containerID != container.ID {
			return core.LeaseTarget{}, fmt.Errorf("%w: %w", errContainerIdentityMismatch,
				core.Exit(4, "lease_id_conflict: fixed local-container lease %s does not match its durable container identity", leaseID))
		}
		if claim.CloudID == "" {
			pending := createdPendingLease(cfg, container.ID, leaseID, intent.Slug, container.Config.Labels["bootstrap_dir"], req.Keep)
			pending.Server.Labels["fixed_intent_sha256"] = fingerprint
			if err := tx.Bind(core.FixedResourceBinding{CloudID: container.ID, AttemptIdentityKey: "container_id", Labels: pending.Server.Labels}); err != nil {
				return core.LeaseTarget{}, err
			}
		}
		if isPendingLocalContainerClaim(*claim) {
			pending, err := b.pendingLease(cfg, container, leaseID, intent.Slug)
			// Keep the observed identity for reconciliation even when key admission fails.
			rememberPending(claim, pending)
			if err != nil {
				return core.LeaseTarget{}, err
			}
		}
		containerID := strings.TrimSpace(claim.CloudID)
		if !container.State.Running {
			notRunning := core.Exit(4, "lease_id_conflict: fixed local-container lease %s is bound to a non-running container", leaseID)
			if observedErr := validateObservedContainer(container, containerID, leaseID); observedErr != nil {
				if recoveringUnboundAttempt {
					return core.LeaseTarget{}, errors.Join(notRunning, observedErr)
				}
				return core.LeaseTarget{}, observedErr
			}
			return core.LeaseTarget{}, notRunning
		}
		imageEvidence, err := b.observeImageEvidence(ctx, cfg, container, *claim)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		lease, err := b.waitForContainerEndpoint(ctx, cfg, containerID, leaseID, intent.Slug)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		lease.Server.ImageEvidence = imageEvidence
		if isPendingLocalContainerClaim(*claim) {
			if err := tx.Bind(core.FixedResourceBinding{ImageEvidence: imageEvidence, SSH: &lease.SSH}); err != nil {
				return core.LeaseTarget{}, err
			}

			rememberPending(claim, lease)
		}
		if err := b.waitForExactContainerSSHReady(ctx, &lease, core.BootstrapWaitTimeout(cfg)); err != nil {
			return core.LeaseTarget{}, err
		}
		lease.Server.Status = "ready"
		lease.Server.Labels = shared.CloneLabels(lease.Server.Labels)
		lease.Server.Labels["state"] = "ready"
		delete(lease.Server.Labels, "recovery")
		for _, key := range checkpointScopeMetadataKeys {
			if value := strings.TrimSpace(cfg.LocalContainer.CheckpointMetadata[key]); value != "" {
				lease.Server.Labels[key] = value
			}
		}
		return lease, nil
	}})
	if err != nil {
		if pendingClaim.LeaseID == leaseID && pendingLease.Server.CloudID == pendingClaim.CloudID {
			bootstrapDir := strings.TrimSpace(pendingClaim.Labels["bootstrap_dir"])
			retained, reconcileErr := b.reconcileReadinessFailure(req.Keep, pendingClaim, pendingLease, bootstrapDir, err)
			if retained {
				b.printPendingRecovery(leaseID, pendingClaim.Slug, pendingClaim, err)
			}
			return core.LeaseTarget{}, errors.Join(err, reconcileErr)
		}
		return core.LeaseTarget{}, err
	}
	acquired.Server.Labels = publicLocalContainerClaimLabels(acquired.Server.Labels)
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s container=%s state=ready\n", leaseID, shortID(acquired.Server.CloudID))
	return core.CompleteFixedAcquisition(acquired, nil, req)
}

func validateFixedLocalContainer(container inspectContainer, cfg core.Config, leaseID, slug, fingerprint string) error {
	labels := container.Config.Labels
	if labels["crabbox"] != "true" || labels["provider"] != providerName ||
		labels["lease"] != leaseID || core.NormalizeLeaseSlug(labels["slug"]) != slug ||
		labels["pond"] != core.NormalizePondName(cfg.Pond) ||
		labels["fixed_intent_sha256"] != fingerprint ||
		labels["runtime"] != cfg.LocalContainer.Runtime ||
		labels["image"] != localContainerDisplayImage(cfg) ||
		container.Config.Image != cfg.LocalContainer.Image ||
		labels[checkpointMetadataDaemonID] != cfg.LocalContainer.CheckpointMetadata[checkpointMetadataDaemonID] ||
		strings.TrimPrefix(container.Name, "/") != core.LeaseProviderName(leaseID, slug) {
		return core.Exit(4, "lease_id_conflict: local container for lease %s does not match its fixed create intent", leaseID)
	}
	return nil
}

func (b *backend) RetainLeaseClaimAfterReleaseWithClaim(lease core.LeaseTarget, previous core.LeaseClaim) (bool, error) {
	return fixedLocalContainerLeaseKind.RetainMatchingFingerprint(lease.LeaseID, previous, strings.TrimSpace(lease.Server.Labels["fixed_intent_sha256"]))
}
