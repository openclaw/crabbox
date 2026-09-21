package azure

import (
	"context"
	"crypto/rand"
	"fmt"
	"maps"
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
	if cfg.AzureOSDisk == core.AzureOSDiskEphemeralPreview {
		return core.LeaseTarget{}, core.Exit(2, "direct Azure fixed leases do not support ephemeral-preview OS disks")
	}
	if cfg.AzureSnapshot != "" || req.RequestedCheckpointID != "" {
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
	}, core.FixedLeaseOperations[core.Server]{DescribeIntent: func(ctx context.Context, claim *core.LeaseClaim, exists bool) (core.FixedLeaseBinding, error) {
		if exists && (!fixedAzureLeaseKind.IsFixedClaim(*claim) || claim.ProviderScope != scope) {
			return core.FixedLeaseBinding{}, core.Exit(4, "lease_id_conflict: Azure owner or account scope changed")
		}
		if exists && claim.FixedCreateIntent.Attempt != nil {
			target := core.SSHTarget{}
			if err := core.UseStoredTestboxKey(&target, req.RequestedLeaseID); err != nil {
				return core.FixedLeaseBinding{}, err
			}
		}
		var err error
		cfg.SSHKey, publicKey, err = core.EnsureTestboxKeyForConfig(cfg, req.RequestedLeaseID)
		if err != nil {
			return core.FixedLeaseBinding{}, err
		}
		cfg.ProviderKey = core.ProviderKeyForLease(req.RequestedLeaseID)
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
		}{core.DirectLeaseLabels(cfg, req.RequestedLeaseID, req.RequestedSlug, "azure", cfg.Capacity.Market, req.Keep, time.Unix(0, 0)), cfg.AzureLocation, cfg.AzureImage, cfg.AzureOSDisk, cfg.AzureOSDiskSKU, cfg.AzureVNet, cfg.AzureSubnet, cfg.AzureNSG, cfg.AzureNetwork, cfg.ServerType, cfg.Architecture, cfg.TargetOS, cfg.WindowsMode, bootstrap, core.NormalizeLeaseSlug(req.RequestedSlug), cfg.SSHUser, cfg.SSHPort, cfg.WorkRoot, cfg.Pond, cfg.Capacity.Market, cfg.AzureSSHCIDRs, req.Keep, cfg.TTL, cfg.IdleTimeout})
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
		for _, server := range servers {
			if server.Labels["lease"] == req.RequestedLeaseID {
				return binding, core.Exit(4, "lease_id_conflict: Azure VM exists without its create intent")
			}
		}
		binding.Slug, err = core.AllocateDirectLeaseSlug(req.RequestedLeaseID, req.RequestedSlug, servers)
		return binding, err
	}, ObserveExact: func(ctx context.Context, tx *core.FixedTransaction, _ core.FixedObserveMode) (core.FixedObservation[core.Server], error) {
		claim, intent := tx.Claim, tx.Claim.FixedCreateIntent
		var result core.FixedObservation[core.Server]
		if core.HasAzureCleanupBinding(claim.Labels) {
			return result, core.Exit(4, "Azure fixed lease has entered cleanup; retry stop")
		}
		name := core.LeaseProviderName(claim.LeaseID, claim.Slug)
		if cfg.Tailscale.Enabled && cfg.Tailscale.Hostname == "" {
			cfg.Tailscale.Hostname = core.RenderTailscaleHostname(cfg.Tailscale.HostnameTemplate, claim.LeaseID, claim.Slug, cfg.Provider)
		}
		server, err := client.GetServer(ctx, name)
		if intent.Attempt == nil {
			if err == nil {
				return result, core.Exit(4, "lease_id_conflict: Azure VM name already exists")
			}
			if !isAzureCleanupNotFound(err) {
				return result, err
			}
			result.CanSubmit = true
			return result, nil
		}
		if err != nil {
			return result, fmt.Errorf("Azure fixed create unresolved; no replacement allocated: %w", err)
		}
		if err := validateFixedAzureServer(*claim, server); err != nil {
			return result, err
		}
		return core.FixedObservation[core.Server]{Candidates: []core.Server{server}}, nil
	}, PlanAttempt: func(ctx context.Context, tx *core.FixedTransaction) error {
		claim, intent := tx.Claim, tx.Claim.FixedCreateIntent
		createdAt, _ := time.Parse(time.RFC3339Nano, intent.CreatedAt)
		intent.Attempt = map[string]string{"name": core.LeaseProviderName(claim.LeaseID, claim.Slug), "nonce": rand.Text()}
		claim.Labels = core.DirectLeaseLabels(cfg, claim.LeaseID, claim.Slug, "azure", cfg.Capacity.Market, req.Keep, createdAt)
		claim.Labels["fixed_intent_sha256"], claim.Labels["fixed_attempt"] = intent.Fingerprint, intent.Attempt["nonce"]
		return nil
	}, Submit: func(ctx context.Context, tx *core.FixedTransaction) (core.Server, error) {
		claim := tx.Claim
		if err := tx.Record("submitting"); err != nil {
			return core.Server{}, err
		}
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
		claim.CloudID, claim.CloudImmutableID = server.CloudID, server.ImmutableID
		if err := tx.Record("bound"); err != nil {
			return core.LeaseTarget{}, err
		}
		server, err := client.WaitForServerIP(ctx, name)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		if err := validateFixedAzureServer(*claim, server); err != nil {
			return core.LeaseTarget{}, err
		}
		target := core.SSHTargetFromConfig(cfg, core.AzureServerHost(server, cfg.AzureNetwork))
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
	if claim.FixedCreateIntent.State == "released" {
		if !req.ReleaseOnly {
			return core.LeaseTarget{}, true, core.Exit(4, "Azure fixed lease is terminal")
		}
		err := fixedAzureLeaseKind.ValidateTerminalClaim(claim, claim, claim.LeaseID, nil)
		return core.LeaseTarget{LeaseID: claim.LeaseID}, true, err
	}
	observed, err := core.InspectFixedResource(ctx, fixedAzureLeaseKind, claim, core.FixedLeaseOperations[core.Server]{
		ObserveExact: func(ctx context.Context, tx *core.FixedTransaction, _ core.FixedObserveMode) (core.FixedObservation[core.Server], error) {
			server, err := client.GetServer(ctx, tx.Claim.FixedCreateIntent.Attempt["name"])
			if err != nil {
				return core.FixedObservation[core.Server]{}, err
			}
			if err := validateFixedAzureServer(*tx.Claim, server); err != nil {
				return core.FixedObservation[core.Server]{}, err
			}
			return core.FixedObservation[core.Server]{Candidates: []core.Server{server}}, nil
		},
	})
	if err != nil {
		if req.ReleaseOnly && claim.CloudID != "" && isAzureCleanupNotFound(err) {
			lease, err := resolveMissingAzureReleaseClaim(claim.LeaseID, client.LeaseClaimScope())
			return lease, true, err
		}
		return core.LeaseTarget{}, true, err
	}
	server := observed.Candidates[0]
	if claim.CloudImmutableID == "" && req.ReleaseOnly {
		next := claim
		next.CloudID, next.CloudImmutableID = server.CloudID, server.ImmutableID
		if _, err := core.ReplaceLeaseClaimIfUnchangedDurableReturning(claim.LeaseID, claim, next); err != nil {
			return core.LeaseTarget{}, true, err
		}
	}
	lease, err := b.ResolvedLeaseTarget(server, core.SSHTargetFromConfig(b.Cfg, core.AzureServerHost(server, b.Cfg.AzureNetwork)), claim.LeaseID, req.ReleaseOnly)
	return lease, true, err
}

func (b *azureLeaseBackend) releaseFixedTerminal(lease core.LeaseTarget) (bool, error) {
	claim, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
	if err != nil {
		return true, err
	}
	if !exists || !fixedAzureLeaseKind.IsFixedClaim(claim) || claim.FixedCreateIntent.State != "released" {
		return false, nil
	}
	return true, fixedAzureLeaseKind.ValidateTerminalClaim(claim, claim, lease.LeaseID, nil)
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
