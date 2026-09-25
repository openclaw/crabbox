package azure

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

var fixedAzureLeaseKind = core.FixedLeaseKind{ClaimProvider: "azure", IntentVersion: 1, Label: "Azure"}

func (*azureLeaseBackend) SupportsRequestedLeaseID() bool { return true }

type fixedAzureCreator interface {
	CreateFixedServer(context.Context, core.Config, string, string, string, map[string]string) (core.Server, error)
}

func (b *azureLeaseBackend) acquireFixed(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	cfg := b.Cfg
	if cfg.Azure.Snapshot != "" || req.RequestedCheckpointID != "" {
		return core.LeaseTarget{}, core.Exit(2, "direct Azure fixed leases require a VM image; checkpoint forks are not supported")
	}
	if cfg.Tailscale.Enabled && cfg.Tailscale.AuthKey == "" {
		return core.LeaseTarget{}, core.Exit(2, "direct --tailscale requires %s", cfg.Tailscale.AuthKeyEnv)
	}
	if err := validateAzureSSHCIDRsForAcquire(ctx, cfg); err != nil {
		return core.LeaseTarget{}, err
	}
	client, err := newAzureClient(ctx, cfg)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	creator, ok := client.(fixedAzureCreator)
	if !ok {
		return core.LeaseTarget{}, core.Exit(2, "Azure client cannot create fixed leases")
	}
	scope := client.LeaseClaimScope()
	if scope == "" {
		return core.LeaseTarget{}, core.Exit(2, "Azure account scope is missing")
	}
	cfg.ServerType = (Provider{}).ServerTypeForConfig(cfg)
	var publicKey string
	lease, err := core.AcquireFixedResource(ctx, core.FixedAcquireOptions{
		Kind: fixedAzureLeaseKind, LeaseID: req.RequestedLeaseID, RepoRoot: req.Repo.Root, Reclaim: req.Reclaim,
		TargetOS: cfg.TargetOS, WindowsMode: cfg.WindowsMode, TTL: cfg.TTL, IdleTimeout: cfg.IdleTimeout,
	}, core.FixedLeaseOperations[core.Server]{Admission: &core.FixedAdmission{}, DescribeIntent: func(ctx context.Context, claim *core.LeaseClaim, exists bool) (core.FixedLeaseBinding, error) {
		if exists && (!fixedAzureLeaseKind.IsFixedClaim(*claim) || claim.ProviderScope != scope) {
			return core.FixedLeaseBinding{}, core.Exit(4, "lease_id_conflict: Azure owner or account scope changed")
		}
		var err error
		publicKey, err = core.PrepareFixedSSHKey(&cfg, req.RequestedLeaseID, core.FixedKeyPolicy{RequireExisting: exists && claim.FixedCreateIntent.Attempt != nil})
		if err != nil {
			return core.FixedLeaseBinding{}, err
		}
		bootstrap := core.CloudInitUserData(cfg, publicKey)
		if cfg.TargetOS == core.TargetWindows {
			bootstrap = core.WindowsBootstrapPowerShell(cfg, publicKey)
		}
		fingerprint, err := core.FixedIntentFingerprint("", struct {
			Labels                                                                                                                                                   map[string]string
			Location, Image, Disk, DiskSKU, VNet, Subnet, NSG, Network, Type, Architecture, Target, WindowsMode, Bootstrap, Slug, User, Port, WorkRoot, Pond, Market string
			CIDRs                                                                                                                                                    []string
			Keep                                                                                                                                                     bool
			TTL, Idle                                                                                                                                                time.Duration
		}{core.DirectLeaseLabels(cfg, req.RequestedLeaseID, req.RequestedSlug, "azure", cfg.Capacity.Market, req.Keep, time.Unix(0, 0)), cfg.Azure.Location, cfg.Azure.Image, cfg.Azure.OSDisk, cfg.Azure.OSDiskSKU, cfg.Azure.VNet, cfg.Azure.Subnet, cfg.Azure.NSG, cfg.Azure.Network, cfg.ServerType, cfg.Architecture, cfg.TargetOS, cfg.WindowsMode, bootstrap, core.NormalizeLeaseSlug(req.RequestedSlug), cfg.SSHUser, cfg.SSHPort, cfg.WorkRoot, cfg.Pond, cfg.Capacity.Market, cfg.Azure.SSHCIDRs, req.Keep, cfg.TTL, cfg.IdleTimeout})
		if err != nil {
			return core.FixedLeaseBinding{}, err
		}
		binding := core.FixedLeaseBinding{ProviderScope: scope, Fingerprint: fingerprint}
		if exists {
			return binding, nil
		}
		servers, err := client.ListCrabboxServers(ctx)
		if err != nil {
			return binding, err
		}
		binding.AllocateSlug, binding.RejectExistingLease, binding.RequestedSlug, binding.Inventory = true, true, req.RequestedSlug, servers
		return binding, nil
	}, ObserveExact: func(ctx context.Context, tx *core.FixedTransaction, _ core.FixedObserveMode) (core.FixedObservation[core.Server], error) {
		claim := tx.Claim
		var result core.FixedObservation[core.Server]
		if core.HasAzureCleanupBinding(claim.Labels) {
			return result, core.Exit(4, "Azure fixed lease has entered cleanup; retry stop")
		}
		name := core.LeaseProviderName(claim.LeaseID, claim.Slug)
		if cfg.Tailscale.Enabled && cfg.Tailscale.Hostname == "" {
			cfg.Tailscale.Hostname = core.RenderTailscaleHostname(cfg.Tailscale.HostnameTemplate, claim.LeaseID, claim.Slug, cfg.Provider)
		}
		server, err := client.GetServer(ctx, name)
		return core.FixedLookupObservation(fixedAzureLeaseKind, *claim, server, err, isAzureCleanupNotFound, validateFixedAzureServer)
	}, Plan: func(ctx context.Context, claim core.LeaseClaim) (core.FixedAttemptPlan, error) {
		return core.FixedAttemptPlan{Values: map[string]string{"name": core.LeaseProviderName(claim.LeaseID, claim.Slug)},
			NonceKey: "nonce", FingerprintLabel: "fixed_intent_sha256", NonceLabel: "fixed_attempt",
			DirectLabels: &core.FixedDirectLabels{Config: cfg, Provider: "azure", Market: cfg.Capacity.Market, Keep: req.Keep},
		}, nil
	}, Submit: func(ctx context.Context, tx *core.FixedTransaction) (core.Server, error) {
		claim := tx.Claim
		server, err := creator.CreateFixedServer(ctx, cfg, publicKey, claim.LeaseID, claim.Slug, maps.Clone(claim.Labels))
		if err != nil {
			return core.Server{}, fmt.Errorf("Azure fixed create unresolved; replay or stop lease %s: %w", claim.LeaseID, err)
		}
		return server, nil
	}, PrepareAccess: func(ctx context.Context, tx *core.FixedTransaction, server core.Server) (core.LeaseTarget, error) {
		claim := tx.Claim
		name := core.LeaseProviderName(claim.LeaseID, claim.Slug)
		if err := validateFixedAzureServer(*claim, server); err != nil {
			return core.LeaseTarget{}, err
		}
		if err := tx.Bind(core.FixedResourceBinding{CloudID: server.CloudID, ImmutableID: server.ImmutableID}); err != nil {
			return core.LeaseTarget{}, err
		}
		server, err := client.WaitForServerIP(ctx, name)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		if err := validateFixedAzureServer(*claim, server); err != nil {
			return core.LeaseTarget{}, err
		}
		target := core.SSHTargetFromConfig(cfg, core.AzureServerHost(server, cfg.Azure.Network))
		if err := bootstrapManagedWindowsDesktop(ctx, cfg, &target, publicKey, b.RT.Stderr); err != nil {
			return core.LeaseTarget{}, err
		}
		server.Labels["state"] = "ready"
		if err := client.SetTags(ctx, name, server.Labels); err != nil {
			return core.LeaseTarget{}, err
		}
		return core.LeaseTarget{Server: server, SSH: target, LeaseID: claim.LeaseID}, nil
	}})
	if err == nil && req.OnAcquired != nil {
		err = req.OnAcquired(lease)
	}
	return lease, err
}

func validateFixedAzureServer(claim core.LeaseClaim, server core.Server) error {
	intent := claim.FixedCreateIntent
	if !fixedAzureLeaseKind.IsFixedClaim(claim) || intent.State == "released" || intent.Attempt["nonce"] == "" ||
		!isCrabboxAzureLease(server) || server.ImmutableID == "" || server.CloudID != intent.Attempt["name"] ||
		server.Labels["fixed_attempt"] != intent.Attempt["nonce"] || server.Labels["fixed_intent_sha256"] != intent.Fingerprint ||
		(claim.CloudImmutableID != "" && claim.CloudImmutableID != server.ImmutableID) {
		return core.Exit(4, "lease_id_conflict: Azure VM does not match fixed create intent")
	}
	expected := core.Server{CloudID: intent.Attempt["name"], ImmutableID: server.ImmutableID, Labels: claim.Labels}
	return validateAzureAcquiredVM(expected, server)
}

func (b *azureLeaseBackend) resolveFixed(ctx context.Context, client azureClient, req core.ResolveRequest) (core.LeaseTarget, bool, error) {
	claim, exists, err := core.ResolveLeaseClaimForProvider(req.ID, "azure")
	if err != nil {
		return core.LeaseTarget{}, true, err
	}
	if !exists || claim.FixedCreateIntent == nil {
		return core.LeaseTarget{}, false, nil
	}
	if claim.ProviderScope != client.LeaseClaimScope() {
		return core.LeaseTarget{}, true, core.Exit(4, "Azure fixed lease account scope changed")
	}
	if lease, terminal, err := fixedAzureLeaseKind.ResolveTerminal(claim, req.ReleaseOnly); terminal {
		return lease, true, err
	}

	server, err := core.LookupFixedResource(ctx, fixedAzureLeaseKind, claim, func(ctx context.Context, claim core.LeaseClaim) (core.Server, error) {
		server, err := client.GetServer(ctx, claim.FixedCreateIntent.Attempt["name"])
		if err == nil {
			err = validateFixedAzureServer(claim, server)
		}
		return server, err
	})
	if err != nil {
		if req.ReleaseOnly && claim.CloudID != "" && isAzureCleanupNotFound(err) {
			lease, err := resolveMissingAzureReleaseClaim(claim.LeaseID, client.LeaseClaimScope())
			return lease, true, err
		}
		return core.LeaseTarget{}, true, err
	}
	if claim.CloudImmutableID == "" && req.ReleaseOnly {
		_, err := core.CompareAndBindFixedClaim(claim, func(next *core.LeaseClaim, persist func() error) error {
			return core.BindFixedClaim(next, core.FixedResourceBinding{CloudID: server.CloudID, ImmutableID: server.ImmutableID, ImageEvidence: claim.ImageEvidence}, persist)
		})
		if err != nil {
			return core.LeaseTarget{}, true, err
		}
	}
	lease, err := b.ResolvedLeaseTarget(server, core.SSHTargetFromConfig(b.Cfg, core.AzureServerHost(server, b.Cfg.Azure.Network)), claim.LeaseID, req.ReleaseOnly)
	return lease, true, err
}

func (b *azureLeaseBackend) releaseFixedTerminal(lease core.LeaseTarget) (bool, error) {
	return fixedAzureLeaseKind.ReadTerminal(lease.LeaseID)
}

func (b *azureLeaseBackend) RetainLeaseClaimAfterReleaseWithClaim(lease core.LeaseTarget, previous core.LeaseClaim) (bool, error) {
	return fixedAzureLeaseKind.RetainClaimAfterRelease(lease.LeaseID, previous, lease.Server.Labels["fixed_attempt"] != "", nil, nil)
}

func (b *azureLeaseBackend) resolvedAzureLease(server core.Server, target core.SSHTarget, leaseID string, releaseOnly bool) (core.LeaseTarget, error) {
	if server.Labels["fixed_attempt"] != "" {
		claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		if !exists {
			return core.LeaseTarget{}, core.Exit(4, "Azure fixed lease cannot be adopted without its create intent")
		}
		if err := validateFixedAzureServer(claim, server); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	return b.ResolvedLeaseTarget(server, target, leaseID, releaseOnly)
}

func (b *azureLeaseBackend) ReclaimAndStop(ctx context.Context, req core.StopRequest) error {
	leaseID := strings.TrimSpace(req.ID)
	if !core.IsCanonicalLeaseID(leaseID) {
		return core.Exit(2, "Azure recovery requires stop --force --provider azure --id <canonical-id>")
	}
	client, err := newAzureClient(ctx, b.Cfg)
	if err != nil {
		return err
	}
	// A prior recovery may have deleted the VM but retained companion cleanup
	// bindings, or already published its terminal receipt. Resolve that durable
	// state before requiring a live VM for adoption.
	lease, handled, err := b.resolveFixed(ctx, client, core.ResolveRequest{ID: leaseID, ReleaseOnly: true})
	if err != nil {
		return err
	}
	if !handled {
		servers, err := client.ListCrabboxServers(ctx)
		if err != nil {
			return err
		}
		providerKey := core.ProviderKeyForLease(leaseID)
		server, found, err := core.SelectFixedCandidate(fixedAzureLeaseKind, leaseID, servers, func(candidate core.Server) bool {
			return candidate.Labels["lease"] == leaseID || candidate.Labels["provider_key"] == providerKey
		})
		if err != nil {
			return err
		}
		if !found {
			return core.Exit(4, "Azure fixed lease %s was not found", leaseID)
		}
		claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
		if err != nil {
			return err
		}
		if !exists {
			claim, err = recoveredFixedAzureClaim(client.LeaseClaimScope(), leaseID, server)
			if err != nil {
				return err
			}
			if err := core.ValidateFixedLocalClaimUniqueness(fixedAzureLeaseKind, claim, "azure"); err != nil {
				return err
			}
			claim, err = core.PublishFixedRecoveryClaimIfAbsent(ctx, fixedAzureLeaseKind, claim)
			if err != nil {
				return err
			}
		}
		if err := validateExactAzureClaim(claim, server, leaseID, client.LeaseClaimScope()); err != nil {
			return err
		}
		lease = core.LeaseTarget{LeaseID: leaseID, Server: server}
	}
	if err := b.ReleaseLease(ctx, core.ReleaseLeaseRequest{Lease: lease, Force: true}); err != nil {
		return err
	}
	fmt.Fprintln(b.RT.Stderr, b.ReleaseLeaseMessage(lease))
	return nil
}

func recoveredFixedAzureClaim(providerScope, leaseID string, server core.Server) (core.LeaseClaim, error) {
	labels := server.Labels
	slug := strings.TrimSpace(labels["slug"])
	fingerprint := strings.TrimSpace(labels["fixed_intent_sha256"])
	nonce := strings.TrimSpace(labels["fixed_attempt"])
	createdUnix, err := strconv.ParseInt(strings.TrimSpace(labels["created_at"]), 10, 64)
	if strings.TrimSpace(providerScope) == "" || !isCrabboxAzureLease(server) ||
		labels["lease"] != leaseID || labels["provider_key"] != core.ProviderKeyForLease(leaseID) ||
		slug == "" || !core.FixedSHA256(fingerprint) || nonce == "" || err != nil || createdUnix <= 0 ||
		server.CloudID != core.LeaseProviderName(leaseID, slug) || server.Name != server.CloudID ||
		strings.TrimSpace(server.ImmutableID) == "" {
		return core.LeaseClaim{}, core.Exit(4, "lease_id_conflict: Azure VM does not contain a complete fixed create identity")
	}
	created := time.Unix(createdUnix, 0).UTC()
	claim := core.LeaseClaim{
		LeaseID: leaseID, Slug: slug, Provider: "azure", ProviderScope: providerScope,
		CloudID: server.CloudID, CloudImmutableID: server.ImmutableID,
		ClaimedAt:  created.Format(time.RFC3339),
		LastUsedAt: time.Now().UTC().Format(time.RFC3339), Labels: maps.Clone(labels),
		FixedCreateIntent: &core.FixedCreateIntent{
			Version: fixedAzureLeaseKind.IntentVersion, Fingerprint: fingerprint,
			ProviderScope: providerScope, Slug: slug, CreatedAt: created.Format(time.RFC3339Nano),
			State: "acquired", Attempt: map[string]string{"nonce": nonce, "name": server.CloudID},
		},
	}
	if err := validateFixedAzureServer(claim, server); err != nil {
		return core.LeaseClaim{}, err
	}
	return claim, nil
}
