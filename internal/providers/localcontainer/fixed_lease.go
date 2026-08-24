package localcontainer

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

const fixedLocalContainerIntentVersion = 1

var fixedLocalContainerLeaseKind = core.FixedLeaseKind{
	ClaimProvider: core.FixedLocalContainerClaimProvider,
	IntentVersion: fixedLocalContainerIntentVersion,
	Label:         "local-container",
}

func isLocalContainerClaimProvider(provider string) bool {
	return provider == providerName || provider == core.FixedLocalContainerClaimProvider
}

func isReleasedFixedLocalContainerClaim(claim core.LeaseClaim) bool {
	return fixedLocalContainerLeaseKind.IsFixedClaim(claim) && claim.FixedCreateIntent.State == "released"
}

type fixedLocalContainerCreateIntent struct {
	Runtime         string          `json:"runtime"`
	RuntimeScope    checkpointScope `json:"runtimeScope"`
	Image           string          `json:"image"`
	ForkImageID     string          `json:"forkImageID,omitempty"`
	User            string          `json:"user"`
	WorkRoot        string          `json:"workRoot"`
	CPUs            int             `json:"cpus"`
	Memory          string          `json:"memory"`
	Network         string          `json:"network"`
	DockerSocket    bool            `json:"dockerSocket"`
	HostVolumes     []string        `json:"hostVolumes,omitempty"`
	CacheVolumes    []string        `json:"cacheVolumes,omitempty"`
	Desktop         bool            `json:"desktop"`
	DesktopEnv      string          `json:"desktopEnv"`
	Browser         bool            `json:"browser"`
	Architecture    string          `json:"architecture,omitempty"`
	RequestedSlug   string          `json:"requestedSlug,omitempty"`
	Pond            string          `json:"pond,omitempty"`
	Keep            bool            `json:"keep"`
	TTLNanoseconds  int64           `json:"ttlNanoseconds"`
	IdleNanoseconds int64           `json:"idleNanoseconds"`
	SSHPublicKey    string          `json:"sshPublicKey"`
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
	intent := fixedLocalContainerCreateIntent{
		Runtime:         strings.TrimSpace(cfg.LocalContainer.Runtime),
		RuntimeScope:    checkpointScopeFromMetadata(cfg.LocalContainer.CheckpointMetadata, cfg.LocalContainer.Runtime),
		Image:           strings.TrimSpace(cfg.LocalContainer.Image),
		ForkImageID:     strings.TrimSpace(cfg.LocalContainer.CheckpointMetadata[checkpointMetadataForkID]),
		User:            strings.TrimSpace(cfg.LocalContainer.User),
		WorkRoot:        strings.TrimSpace(cfg.LocalContainer.WorkRoot),
		CPUs:            cfg.LocalContainer.CPUs,
		Memory:          strings.TrimSpace(cfg.LocalContainer.Memory),
		Network:         strings.TrimSpace(cfg.LocalContainer.Network),
		DockerSocket:    cfg.LocalContainer.DockerSocket,
		HostVolumes:     volumes,
		CacheVolumes:    cacheVolumes,
		Desktop:         cfg.Desktop,
		DesktopEnv:      core.NormalizedDesktopEnv(cfg.DesktopEnv),
		Browser:         cfg.Browser,
		RequestedSlug:   core.NormalizeLeaseSlug(req.RequestedSlug),
		Pond:            core.NormalizePondName(cfg.Pond),
		Keep:            req.Keep,
		TTLNanoseconds:  cfg.TTL.Nanoseconds(),
		IdleNanoseconds: cfg.IdleTimeout.Nanoseconds(),
		SSHPublicKey:    strings.TrimSpace(publicKey),
	}
	if core.IsArchitectureExplicit(cfg) {
		intent.Architecture = cfg.Architecture
	}
	data, err := json.Marshal(intent)
	if err != nil {
		return "", fmt.Errorf("fingerprint fixed local-container create intent: %w", err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

func (b *backend) acquireFixed(ctx context.Context, req core.AcquireRequest, cfg core.Config) (core.LeaseTarget, error) {
	leaseID := strings.TrimSpace(req.RequestedLeaseID)
	var fingerprint, publicKey string
	var pendingClaim core.LeaseClaim
	var pendingLease core.LeaseTarget
	rememberPending := func(claim *core.LeaseClaim, lease core.LeaseTarget) {
		if !isPendingLocalContainerClaim(*claim) || claim.CloudID != lease.Server.CloudID {
			return
		}
		pendingClaim = *claim
		pendingClaim.Labels = cloneLabels(claim.Labels)
		if claim.FixedCreateIntent != nil {
			intent := *claim.FixedCreateIntent
			intent.Attempt = cloneLabels(claim.FixedCreateIntent.Attempt)
			intent.FailedAttempts = append([]string(nil), claim.FixedCreateIntent.FailedAttempts...)
			pendingClaim.FixedCreateIntent = &intent
		}
		pendingLease = lease
	}
	acquired, err := core.AcquireFixedLease(core.FixedAcquireOptions{
		Kind:        fixedLocalContainerLeaseKind,
		LeaseID:     leaseID,
		RepoRoot:    req.Repo.Root,
		Reclaim:     req.Reclaim,
		TargetOS:    cfg.TargetOS,
		WindowsMode: cfg.WindowsMode,
		TTL:         cfg.TTL,
		IdleTimeout: cfg.IdleTimeout,
	}, func(ctx context.Context, _ *core.LeaseClaim, exists bool) (core.FixedLeaseBinding, error) {
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
		binding.Slug, err = core.AllocateDirectLeaseSlug(leaseID, req.RequestedSlug, servers)
		return binding, err
	}, func(ctx context.Context, claim *core.LeaseClaim, intent *core.FixedCreateIntent, persist func() error) (core.LeaseTarget, error) {
		containers, err := b.listContainers(ctx)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		var matches []inspectContainer
		for _, container := range containers {
			if container.Config.Labels["lease"] == leaseID {
				matches = append(matches, container)
			}
		}
		if len(matches) > 1 {
			return core.LeaseTarget{}, core.Exit(4, "lease_id_conflict: multiple local containers match fixed lease %s", leaseID)
		}
		recoveringUnboundAttempt := len(matches) == 1 && claim.CloudID == ""
		name := core.LeaseProviderName(leaseID, intent.Slug)
		if intent.Attempt != nil && intent.Attempt["container_name"] != name {
			return core.LeaseTarget{}, core.Exit(4, "lease_id_conflict: fixed local-container lease %s has an invalid durable create attempt", leaseID)
		}
		var container inspectContainer
		if len(matches) == 1 {
			container = matches[0]
			if intent.Attempt == nil {
				return core.LeaseTarget{}, core.Exit(4, "lease_id_conflict: fixed local-container lease %s has no durable create attempt", leaseID)
			}
			if (intent.State == "acquired" || claim.CloudID != "") && claim.CloudID != container.ID {
				return core.LeaseTarget{}, core.Exit(4, "lease_id_conflict: fixed local-container lease %s does not match its bound container %s", leaseID, blank(claim.CloudID, "<empty>"))
			}
			if err := validateFixedLocalContainer(container, cfg, leaseID, intent.Slug, fingerprint); err != nil {
				return core.LeaseTarget{}, err
			}
		} else {
			if intent.State == "acquired" || claim.CloudID != "" {
				return core.LeaseTarget{}, core.Exit(4, "lease_id_conflict: acquired fixed local-container lease %s is missing its bound container", leaseID)
			}
			if intent.Attempt != nil {
				return core.LeaseTarget{}, core.Exit(4, "lease_id_conflict: fixed local-container lease %s has an unresolved create attempt", leaseID)
			}
			intent.Attempt = map[string]string{"container_name": name}
			if err := persist(); err != nil {
				return core.LeaseTarget{}, err
			}
			fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s runtime=%s image=%s keep=%v fixed=true\n", providerName, leaseID, intent.Slug, cfg.LocalContainer.Runtime, cfg.LocalContainer.Image, req.Keep)
			containerID, bootstrapDir, createErr := b.createContainerWithFixedIntent(ctx, cfg, name, leaseID, intent.Slug, publicKey, fingerprint, req.Keep)
			if containerID == "" {
				return core.LeaseTarget{}, createErr
			}
			pending := createdPendingLease(cfg, containerID, leaseID, intent.Slug, bootstrapDir, req.Keep)
			pending.Server.Labels["fixed_intent_sha256"] = fingerprint
			claim.CloudID = containerID
			claim.Labels = cloneLabels(pending.Server.Labels)
			intent.Attempt["container_id"] = containerID
			if err := persist(); err != nil {
				return core.LeaseTarget{}, err
			}
			rememberPending(claim, pending)
			if createErr != nil {
				return core.LeaseTarget{}, createErr
			}
			container, err = b.inspectContainer(ctx, containerID)
			if err != nil {
				return core.LeaseTarget{}, err
			}
			if err := validateFixedLocalContainer(container, cfg, leaseID, intent.Slug, fingerprint); err != nil {
				return core.LeaseTarget{}, err
			}
		}
		if containerID := intent.Attempt["container_id"]; containerID != "" && containerID != container.ID {
			return core.LeaseTarget{}, fmt.Errorf("%w: %w", errContainerIdentityMismatch,
				core.Exit(4, "lease_id_conflict: fixed local-container lease %s does not match its durable container identity", leaseID))
		}
		if claim.CloudID == "" {
			pending := createdPendingLease(cfg, container.ID, leaseID, intent.Slug, container.Config.Labels["bootstrap_dir"], req.Keep)
			pending.Server.Labels["fixed_intent_sha256"] = fingerprint
			claim.CloudID = container.ID
			claim.Labels = cloneLabels(pending.Server.Labels)
			intent.Attempt["container_id"] = container.ID
			if err := persist(); err != nil {
				return core.LeaseTarget{}, err
			}
		}
		if isPendingLocalContainerClaim(*claim) {
			rememberPending(claim, b.pendingLease(cfg, container, leaseID, intent.Slug))
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
		lease, err := b.waitForContainerEndpoint(ctx, cfg, containerID, leaseID, intent.Slug)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		if isPendingLocalContainerClaim(*claim) {
			claim.SSHHost = lease.SSH.Host
			if port, parseErr := strconv.Atoi(strings.TrimSpace(lease.SSH.Port)); parseErr == nil && port > 0 {
				claim.SSHPort = port
			}
			if err := persist(); err != nil {
				return core.LeaseTarget{}, err
			}
			rememberPending(claim, lease)
		}
		if err := b.waitForExactContainerSSHReady(ctx, &lease, core.BootstrapWaitTimeout(cfg)); err != nil {
			return core.LeaseTarget{}, err
		}
		lease.Server.Status = "ready"
		lease.Server.Labels = cloneLabels(lease.Server.Labels)
		lease.Server.Labels["state"] = "ready"
		delete(lease.Server.Labels, "recovery")
		for _, key := range checkpointScopeMetadataKeys {
			if value := strings.TrimSpace(cfg.LocalContainer.CheckpointMetadata[key]); value != "" {
				lease.Server.Labels[key] = value
			}
		}
		return lease, nil
	}, ctx)
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
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	acquired.Server.Labels = publicLocalContainerClaimLabels(acquired.Server.Labels)
	core.SetServerLeaseClaimSnapshot(&acquired.Server, claim, true)
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s container=%s state=ready\n", leaseID, shortID(acquired.Server.CloudID))
	if req.OnAcquired != nil {
		if err := req.OnAcquired(acquired); err != nil {
			return core.LeaseTarget{}, fmt.Errorf("acknowledge fixed local-container acquisition: %w", err)
		}
	}
	return acquired, nil
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
	fingerprint := strings.TrimSpace(lease.Server.Labels["fixed_intent_sha256"])
	return fixedLocalContainerLeaseKind.RetainClaimAfterRelease(lease.LeaseID, previous, fingerprint != "", nil, func(claim core.LeaseClaim) error {
		if fingerprint != "" && fingerprint != claim.FixedCreateIntent.Fingerprint {
			return core.Exit(4, "lease_id_conflict: fixed local-container lease %s container label differs from its terminal tombstone", lease.LeaseID)
		}
		return nil
	})
}
