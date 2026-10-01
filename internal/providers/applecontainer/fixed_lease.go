package applecontainer

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

const fixedAppleContainerIntentVersion = 1

var fixedAppleContainerLeaseKind = core.FixedLeaseKind{
	ClaimProvider: providerName,
	IntentVersion: fixedAppleContainerIntentVersion,
	Label:         providerName,
}

func isReleasedFixedAppleContainerClaim(claim core.LeaseClaim) bool {
	return fixedAppleContainerLeaseKind.IsFixedClaim(claim) && claim.FixedCreateIntent.State == "released"
}

type fixedAppleContainerCreateIntent struct {
	CLIPath         string                   `json:"cliPath"`
	Image           string                   `json:"image"`
	User            string                   `json:"user"`
	WorkRoot        string                   `json:"workRoot"`
	CPUs            int                      `json:"cpus"`
	Memory          string                   `json:"memory"`
	ExtraRunArgs    []string                 `json:"extraRunArgs,omitempty"`
	CacheVolumes    []core.CacheVolumeConfig `json:"cacheVolumes,omitempty"`
	Architecture    string                   `json:"architecture,omitempty"`
	RequestedSlug   string                   `json:"requestedSlug,omitempty"`
	Pond            string                   `json:"pond,omitempty"`
	Keep            bool                     `json:"keep"`
	TTLNanoseconds  int64                    `json:"ttlNanoseconds"`
	IdleNanoseconds int64                    `json:"idleNanoseconds"`
	SSHPublicKey    string                   `json:"sshPublicKey"`
}

func fixedAppleContainerProviderScope(cfg core.Config) string {
	return providerName + ":cli:" + strings.TrimSpace(cfg.AppleContainer.CLIPath)
}

func fixedAppleContainerFingerprint(cfg core.Config, req core.AcquireRequest, publicKey string) (string, error) {
	intent := fixedAppleContainerCreateIntent{
		CLIPath:         strings.TrimSpace(cfg.AppleContainer.CLIPath),
		Image:           strings.TrimSpace(cfg.AppleContainer.Image),
		User:            strings.TrimSpace(cfg.AppleContainer.User),
		WorkRoot:        strings.TrimSpace(cfg.AppleContainer.WorkRoot),
		CPUs:            cfg.AppleContainer.CPUs,
		Memory:          strings.TrimSpace(cfg.AppleContainer.Memory),
		ExtraRunArgs:    append([]string(nil), cfg.AppleContainer.ExtraRunArgs...),
		CacheVolumes:    append([]core.CacheVolumeConfig(nil), cfg.Cache.Volumes...),
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
		return "", fmt.Errorf("fingerprint fixed apple-container create intent: %w", err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

func (b *backend) acquireFixed(ctx context.Context, req core.AcquireRequest, cfg core.Config) (core.LeaseTarget, error) {
	leaseID := strings.TrimSpace(req.RequestedLeaseID)
	var fingerprint, publicKey string
	acquired, err := core.AcquireFixedLease(core.FixedAcquireOptions{
		Kind:        fixedAppleContainerLeaseKind,
		LeaseID:     leaseID,
		RepoRoot:    req.Repo.Root,
		Reclaim:     req.Reclaim,
		TargetOS:    cfg.TargetOS,
		WindowsMode: cfg.WindowsMode,
		TTL:         cfg.TTL,
		IdleTimeout: cfg.IdleTimeout,
	}, func(ctx context.Context, _ *core.LeaseClaim, exists bool) (core.FixedLeaseBinding, error) {
		keyPath, key, err := core.EnsureTestboxKeyForConfig(cfg, leaseID)
		if err != nil {
			return core.FixedLeaseBinding{}, err
		}
		cfg.SSHKey, publicKey = keyPath, key
		fingerprint, err = fixedAppleContainerFingerprint(cfg, req, publicKey)
		if err != nil {
			return core.FixedLeaseBinding{}, err
		}
		binding := core.FixedLeaseBinding{
			ProviderScope: fixedAppleContainerProviderScope(cfg),
			Fingerprint:   fingerprint,
		}
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
			if container.labels()["lease"] == leaseID {
				matches = append(matches, container)
			}
		}
		if len(matches) > 1 {
			return core.LeaseTarget{}, core.Exit(4, "lease_id_conflict: multiple Apple containers match fixed lease %s", leaseID)
		}
		name := core.LeaseProviderName(leaseID, intent.Slug)
		if intent.Attempt != nil && intent.Attempt["container_name"] != name {
			return core.LeaseTarget{}, core.Exit(4, "lease_id_conflict: fixed apple-container lease %s has an invalid durable create attempt", leaseID)
		}
		var container inspectContainer
		if len(matches) == 0 {
			if intent.State == "acquired" || claim.CloudID != "" {
				return core.LeaseTarget{}, core.Exit(4, "lease_id_conflict: acquired fixed apple-container lease %s is missing its bound container", leaseID)
			}
			if intent.Attempt != nil {
				return core.LeaseTarget{}, core.Exit(4, "lease_id_conflict: fixed apple-container lease %s has an unresolved create attempt", leaseID)
			}
			intent.Attempt = map[string]string{"container_name": name}
			if err := persist(); err != nil {
				return core.LeaseTarget{}, err
			}
			fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s image=%s keep=%v fixed=true\n", providerName, leaseID, intent.Slug, cfg.AppleContainer.Image, req.Keep)
			containerID, err := b.createContainerWithFixedIntentUnderLeaseLock(ctx, cfg, name, leaseID, intent.Slug, publicKey, req.Keep, fingerprint)
			if err != nil {
				return core.LeaseTarget{}, err
			}
			container, err = b.inspectContainer(ctx, containerID)
			if err != nil {
				return core.LeaseTarget{}, err
			}
			claim.CloudID = container.id()
			intent.Attempt["container_id"] = container.id()
			claim.Labels = b.serverFromContainer(container, cfg).Labels
			if err := persist(); err != nil {
				return core.LeaseTarget{}, err
			}
		} else {
			container = matches[0]
			if intent.Attempt == nil {
				return core.LeaseTarget{}, core.Exit(4, "lease_id_conflict: fixed apple-container lease %s has no durable create attempt", leaseID)
			}
		}
		if claim.CloudID != "" && claim.CloudID != container.id() {
			return core.LeaseTarget{}, core.Exit(4, "lease_id_conflict: fixed apple-container lease %s does not match its bound container", leaseID)
		}
		if containerID := intent.Attempt["container_id"]; containerID != "" && containerID != container.id() {
			return core.LeaseTarget{}, core.Exit(4, "lease_id_conflict: fixed apple-container lease %s does not match its durable container identity", leaseID)
		}
		if err := validateFixedAppleContainer(container, cfg, leaseID, intent.Slug, fingerprint); err != nil {
			return core.LeaseTarget{}, err
		}
		if claim.CloudID == "" {
			claim.CloudID = container.id()
			intent.Attempt["container_id"] = container.id()
			claim.Labels = b.serverFromContainer(container, cfg).Labels
			if err := persist(); err != nil {
				return core.LeaseTarget{}, err
			}
		}
		container, err = b.waitForNetworkAddress(ctx, container.id(), container, core.BootstrapWaitTimeout(cfg))
		if err != nil {
			return core.LeaseTarget{}, err
		}
		return b.prepareLease(ctx, cfg, container, leaseID, intent.Slug, true)
	}, ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s container=%s state=ready\n", leaseID, acquired.Server.CloudID)
	if req.OnAcquired != nil {
		if err := req.OnAcquired(acquired); err != nil {
			return core.LeaseTarget{}, fmt.Errorf("acknowledge fixed apple-container acquisition: %w", err)
		}
	}
	return acquired, nil
}

func validateFixedAppleContainer(container inspectContainer, cfg core.Config, leaseID, slug, fingerprint string) error {
	labels := container.labels()
	if labels["crabbox"] != "true" || labels["provider"] != providerName ||
		labels["lease"] != leaseID || core.NormalizeLeaseSlug(labels["slug"]) != slug ||
		labels["pond"] != core.NormalizePondName(cfg.Pond) ||
		labels["fixed_intent_sha256"] != fingerprint ||
		labels["image"] != cfg.AppleContainer.Image ||
		container.image() != cfg.AppleContainer.Image ||
		container.id() != core.LeaseProviderName(leaseID, slug) {
		return core.Exit(4, "lease_id_conflict: Apple container for lease %s does not match its fixed create intent", leaseID)
	}
	return nil
}

func (b *backend) RetainLeaseClaimAfterReleaseWithClaim(lease core.LeaseTarget, previous core.LeaseClaim) (bool, error) {
	fingerprint := strings.TrimSpace(lease.Server.Labels["fixed_intent_sha256"])
	return fixedAppleContainerLeaseKind.RetainClaimAfterRelease(lease.LeaseID, previous, fingerprint != "", nil, func(claim core.LeaseClaim) error {
		if fingerprint != "" && fingerprint != claim.FixedCreateIntent.Fingerprint {
			return core.Exit(4, "lease_id_conflict: fixed apple-container lease %s container label differs from its terminal tombstone", lease.LeaseID)
		}
		return nil
	})
}
