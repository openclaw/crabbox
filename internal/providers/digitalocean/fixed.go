package digitalocean

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

var fixedLeaseKind = core.FixedLeaseKind{ClaimProvider: providerName, IntentVersion: 1, Label: "DigitalOcean"}

func (*digitalOceanLeaseBackend) SupportsRequestedLeaseID() bool { return true }

type fixedDropletCreator interface {
	CreateFixedDroplet(context.Context, core.Config, string, string, string, bool, time.Time, map[string]string) (droplet, error)
}

func (b *digitalOceanLeaseBackend) acquireFixed(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	if b.acquireConfigErr != nil {
		return core.LeaseTarget{}, b.acquireConfigErr
	}
	cfg := b.Cfg
	if cfg.Tailscale.Enabled && cfg.Tailscale.AuthKey == "" {
		return core.LeaseTarget{}, core.Exit(2, "direct --tailscale requires %s", cfg.Tailscale.AuthKeyEnv)
	}
	client, err := b.clientFactory(b.RT)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	creator, ok := client.(fixedDropletCreator)
	if !ok {
		return core.LeaseTarget{}, core.Exit(2, "DigitalOcean client cannot create fixed leases")
	}
	account, err := client.AccountID(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if account == "" {
		return core.LeaseTarget{}, core.Exit(2, "DigitalOcean account identity is missing")
	}
	var publicKey string
	var createLabels map[string]string
	lease, err := core.AcquireFixedResource(ctx, core.FixedAcquireOptions{
		Kind: fixedLeaseKind, LeaseID: req.RequestedLeaseID, RepoRoot: req.Repo.Root, Reclaim: req.Reclaim,
		TargetOS: core.TargetLinux, TTL: cfg.TTL, IdleTimeout: cfg.IdleTimeout, Now: func() time.Time { return core.ClockNow(b.RT.Clock) },
	}, core.FixedLeaseOperations[droplet]{DescribeIntent: func(ctx context.Context, claim *core.LeaseClaim, exists bool) (core.FixedLeaseBinding, error) {
		if exists && (!fixedLeaseKind.IsFixedClaim(*claim) || claim.ProviderScope != account) {
			return core.FixedLeaseBinding{}, core.Exit(4, "lease_id_conflict: DigitalOcean owner or account changed")
		}
		if exists && claim.FixedCreateIntent.Attempt != nil {
			target := core.SSHTarget{}
			if err := core.UseStoredTestboxKey(&target, req.RequestedLeaseID); err != nil {
				return core.FixedLeaseBinding{}, err
			}
			cfg.SSHKey = target.Key
		}
		var err error
		cfg.SSHKey, publicKey, err = core.EnsureTestboxKeyForConfig(cfg, req.RequestedLeaseID)
		if err != nil {
			return core.FixedLeaseBinding{}, err
		}
		cfg.ProviderKey = providerKeyForLease(req.RequestedLeaseID)
		fingerprint, err := core.FixedIntentFingerprint("", struct {
			Labels                                                    map[string]string
			Provider                                                  core.DigitalOceanConfig
			Type, Architecture, Bootstrap, Slug, User, Port, WorkRoot string
			Keep                                                      bool
			TTL, Idle                                                 time.Duration
		}{core.DirectLeaseLabels(cfg, req.RequestedLeaseID, req.RequestedSlug, providerName, "", req.Keep, time.Unix(0, 0)), cfg.DigitalOcean, cfg.ServerType, cfg.Architecture, core.CloudInitUserData(cfg, publicKey), core.NormalizeLeaseSlug(req.RequestedSlug), cfg.SSHUser, cfg.SSHPort, cfg.WorkRoot, req.Keep, cfg.TTL, cfg.IdleTimeout})
		if err != nil {
			return core.FixedLeaseBinding{}, err
		}
		binding := core.FixedLeaseBinding{ProviderScope: account, Fingerprint: fingerprint}
		if exists {
			return binding, nil
		}
		droplets, err := client.ListCrabboxDroplets(ctx)
		if err != nil {
			return binding, err
		}
		var servers []core.Server
		for _, item := range droplets {
			server := serverFromDroplet(item, cfg)
			if server.Labels["lease"] == req.RequestedLeaseID {
				return binding, core.Exit(4, "lease_id_conflict: Droplet exists without its create intent")
			}
			servers = append(servers, server)
		}
		binding.Slug, err = core.AllocateDirectLeaseSlug(req.RequestedLeaseID, req.RequestedSlug, servers)
		return binding, err
	}, ObserveExact: func(ctx context.Context, tx *core.FixedTransaction, _ core.FixedObserveMode) (core.FixedObservation[droplet], error) {
		claim := tx.Claim
		if cfg.Tailscale.Enabled && cfg.Tailscale.Hostname == "" {
			cfg.Tailscale.Hostname = core.RenderTailscaleHostname(cfg.Tailscale.HostnameTemplate, claim.LeaseID, claim.Slug, cfg.Provider)
		}
		if claim.FixedCreateIntent.Attempt == nil {
			return core.FixedObservation[droplet]{CanSubmit: true}, nil
		}
		item, err := loadFixedDroplet(ctx, client, *claim)
		if err != nil {
			return core.FixedObservation[droplet]{}, err
		}
		if err := validateFixedDroplet(*claim, item); err != nil {
			return core.FixedObservation[droplet]{}, err
		}
		return core.FixedObservation[droplet]{Candidates: []droplet{item}}, nil
	}, PlanAttempt: func(ctx context.Context, tx *core.FixedTransaction) error {
		claim, intent := tx.Claim, tx.Claim.FixedCreateIntent
		createdAt, _ := time.Parse(time.RFC3339Nano, intent.CreatedAt)
		intent.Attempt = map[string]string{"nonce": rand.Text()}
		labels := labelsFromTags(leaseTags(cfg, claim.LeaseID, claim.Slug, "provisioning", req.Keep, createdAt))
		labels["fixed_intent_sha256"], labels["fixed_attempt"] = intent.Fingerprint, intent.Attempt["nonce"]
		createLabels = labels
		claim.Labels = maps.Clone(labels)
		claim.Labels[digitalOceanAccountLabel], claim.Labels["recovery"] = account, "ambiguous-create"
		return nil
	}, Submit: func(ctx context.Context, tx *core.FixedTransaction) (droplet, error) {
		claim, intent := tx.Claim, tx.Claim.FixedCreateIntent
		createdAt, _ := time.Parse(time.RFC3339Nano, intent.CreatedAt)
		labels := maps.Clone(createLabels)
		if err := tx.Record("submitting"); err != nil {
			return droplet{}, err
		}
		item, err := creator.CreateFixedDroplet(ctx, cfg, publicKey, claim.LeaseID, claim.Slug, req.Keep, createdAt, labels)
		if err != nil {
			var ambiguous *ambiguousDropletCreateError
			if errors.As(err, &ambiguous) {
				setDigitalOceanKeyIdentity(claim.Labels, ambiguous.keyID, ambiguous.keyCreated, ambiguous.keyOwnershipKnown)
			}
			return droplet{}, errors.Join(fmt.Errorf("DigitalOcean fixed create unresolved; replay or stop lease %s: %w", claim.LeaseID, err), tx.Record("submitting"))
		}
		setDigitalOceanKeyIdentity(claim.Labels, item.SSHKeyID, item.SSHKeyCreated, true)
		return item, nil
	}, PrepareAccess: func(ctx context.Context, tx *core.FixedTransaction, item droplet) (core.LeaseTarget, error) {
		claim := tx.Claim
		if err := validateFixedDroplet(*claim, item); err != nil {
			return core.LeaseTarget{}, err
		}
		claim.CloudID, claim.CloudNumericID, claim.CloudImmutableID = dropletIDString(item.ID), item.ID, dropletIDString(item.ID)
		if err := tx.Record("bound"); err != nil {
			return core.LeaseTarget{}, err
		}
		item, err := b.waitForDropletIP(ctx, client, item.ID, 5*time.Minute)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		if err := validateFixedDroplet(*claim, item); err != nil {
			return core.LeaseTarget{}, err
		}
		server := serverFromDroplet(item, cfg)
		server.ImmutableID = claim.CloudImmutableID
		target := core.SSHTargetFromConfig(cfg, server.PublicNet.IPv4.IP)
		if err := b.waitSSH(ctx, &target, "digitalocean bootstrap", core.BootstrapWaitTimeout(cfg)); err != nil {
			return core.LeaseTarget{}, err
		}
		labels := maps.Clone(server.Labels)
		labels["state"] = "ready"
		if err := client.ReplaceDropletTags(ctx, item.ID, item.Tags, tagsFromLabels(labels)); err != nil {
			return core.LeaseTarget{}, err
		}
		labels[digitalOceanAccountLabel] = account
		preserveDigitalOceanKeyIdentity(labels, claim.Labels)
		server.Labels, server.Status = labels, "ready"
		return core.LeaseTarget{Server: server, SSH: target, LeaseID: claim.LeaseID}, nil
	}})
	if err == nil && req.OnAcquired != nil {
		err = req.OnAcquired(lease)
	}
	return lease, err
}

func validateFixedDroplet(claim core.LeaseClaim, item droplet) error {
	intent := claim.FixedCreateIntent
	labels := labelsFromTags(item.Tags)
	if !fixedLeaseKind.IsFixedClaim(claim) || intent.State == "released" || intent.Attempt["nonce"] == "" ||
		item.ID <= 0 || item.Name != core.LeaseProviderName(claim.LeaseID, claim.Slug) || !isOwnedDroplet(item) ||
		labels["lease"] != claim.LeaseID || labels["slug"] != claim.Slug || labels["provider_key"] != claim.Labels["provider_key"] ||
		labels["fixed_attempt"] != intent.Attempt["nonce"] || labels["fixed_intent_sha256"] != intent.Fingerprint ||
		(claim.CloudID != "" && claim.CloudID != dropletIDString(item.ID)) {
		return core.Exit(4, "lease_id_conflict: DigitalOcean Droplet does not match fixed create intent")
	}
	return nil
}

func loadFixedDroplet(ctx context.Context, client digitalOceanAPI, claim core.LeaseClaim) (droplet, error) {
	observed, err := core.InspectFixedResource(ctx, fixedLeaseKind, claim, core.FixedLeaseOperations[droplet]{ObserveExact: func(ctx context.Context, _ *core.FixedTransaction, _ core.FixedObserveMode) (core.FixedObservation[droplet], error) {
		if claim.CloudID != "" {
			id, ok := parseDropletID(claim.CloudID)
			if !ok {
				return core.FixedObservation[droplet]{}, core.Exit(4, "invalid fixed Droplet identity")
			}
			item, err := client.GetDroplet(ctx, id)
			if err != nil {
				return core.FixedObservation[droplet]{}, err
			}
			if err := validateFixedDroplet(claim, item); err != nil {
				return core.FixedObservation[droplet]{}, err
			}
			return core.FixedObservation[droplet]{Candidates: []droplet{item}}, nil
		}
		items, err := client.ListCrabboxDroplets(ctx)
		if err != nil {
			return core.FixedObservation[droplet]{}, err
		}
		var matches []droplet
		for _, item := range items {
			if validateFixedDroplet(claim, item) == nil {
				matches = append(matches, item)
			}
		}
		if len(matches) != 1 {
			return core.FixedObservation[droplet]{Conflict: fmt.Sprintf("DigitalOcean fixed create remains unresolved (%d matches); no replacement allocated", len(matches))}, nil
		}
		return core.FixedObservation[droplet]{Candidates: matches}, nil
	}})
	if err != nil {
		return droplet{}, err
	}
	return observed.Candidates[0], nil
}

func (b *digitalOceanLeaseBackend) RetainLeaseClaimAfterReleaseWithClaim(lease core.LeaseTarget, previous core.LeaseClaim) (bool, error) {
	return fixedLeaseKind.RetainClaimAfterRelease(lease.LeaseID, previous, lease.Server.Labels["fixed_attempt"] != "", nil, nil)
}

func (b *digitalOceanLeaseBackend) resolveFixed(ctx context.Context, client digitalOceanAPI, req core.ResolveRequest, account string) (core.LeaseTarget, bool, error) {
	var claim core.LeaseClaim
	var exists bool
	var err error
	if core.IsCanonicalLeaseID(req.ID) {
		claim, exists, err = core.ReadLeaseClaimWithPresence(req.ID)
		if exists && claim.FixedCreateIntent != nil && claim.Provider != providerName {
			return core.LeaseTarget{}, true, core.Exit(4, "lease_id_conflict: fixed lease belongs to another provider")
		}
	} else {
		claim, exists, err = core.ResolveLeaseClaimForProvider(req.ID, providerName)
	}
	if err != nil {
		return core.LeaseTarget{}, true, err
	}
	if !exists || claim.FixedCreateIntent == nil {
		return core.LeaseTarget{}, false, nil
	}
	if claim.ProviderScope != account {
		return core.LeaseTarget{}, true, core.Exit(4, "DigitalOcean fixed lease account changed")
	}
	if claim.FixedCreateIntent.State == "released" {
		if !req.ReleaseOnly {
			return core.LeaseTarget{}, true, core.Exit(4, "DigitalOcean fixed lease is terminal")
		}
		err := fixedLeaseKind.ValidateTerminalClaim(claim, claim, claim.LeaseID, nil)
		return core.LeaseTarget{LeaseID: claim.LeaseID}, true, err
	}
	item, err := loadFixedDroplet(ctx, client, claim)
	if err != nil {
		if req.ReleaseOnly && claim.CloudID != "" && isDigitalOceanNotFound(err) {
			id, _ := parseDropletID(claim.CloudID)
			server := core.Server{Provider: providerName, CloudID: claim.CloudID, ID: id, Name: core.LeaseProviderName(claim.LeaseID, claim.Slug), Labels: maps.Clone(claim.Labels)}
			core.SetServerLeaseClaimSnapshot(&server, claim, true)
			return core.LeaseTarget{LeaseID: claim.LeaseID, Server: server}, true, nil
		}
		return core.LeaseTarget{}, true, err
	}
	if err := validateFixedDroplet(claim, item); err != nil {
		return core.LeaseTarget{}, true, err
	}
	if claim.CloudID == "" && req.ReleaseOnly {
		next := claim
		next.CloudID, next.CloudNumericID, next.CloudImmutableID = dropletIDString(item.ID), item.ID, dropletIDString(item.ID)
		claim, err = core.ReplaceLeaseClaimIfUnchangedDurableReturning(claim.LeaseID, claim, next)
		if err != nil {
			return core.LeaseTarget{}, true, err
		}
	}
	lease, err := b.targetFromDroplet(item, req, []droplet{item}, account)
	return lease, true, err
}
