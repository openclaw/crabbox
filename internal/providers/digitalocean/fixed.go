package digitalocean

import (
	"context"
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
	lease, err := core.AcquireFixedResource(ctx, core.FixedAcquireOptions{
		Kind: fixedLeaseKind, LeaseID: req.RequestedLeaseID, RepoRoot: req.Repo.Root, Reclaim: req.Reclaim,
		TargetOS: core.TargetLinux, TTL: cfg.TTL, IdleTimeout: cfg.IdleTimeout, Now: func() time.Time { return core.ClockNow(b.RT.Clock) },
	}, core.FixedLeaseOperations[droplet]{Admission: &core.FixedAdmission{}, DescribeIntent: func(ctx context.Context, claim *core.LeaseClaim, exists bool) (core.FixedLeaseBinding, error) {
		if exists && (!fixedLeaseKind.IsFixedClaim(*claim) || claim.ProviderScope != account) {
			return core.FixedLeaseBinding{}, core.Exit(4, "lease_id_conflict: DigitalOcean owner or account changed")
		}
		var err error
		publicKey, err = core.PrepareFixedSSHKey(&cfg, req.RequestedLeaseID, core.FixedKeyPolicy{RequireExisting: exists && claim.FixedCreateIntent.Attempt != nil, UseStored: true})
		if err != nil {
			return core.FixedLeaseBinding{}, err
		}
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
			servers = append(servers, server)
		}
		binding.AllocateSlug, binding.RejectExistingLease, binding.RequestedSlug, binding.Inventory = true, true, req.RequestedSlug, servers
		return binding, nil
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
	}, Plan: func(ctx context.Context, claim core.LeaseClaim) (core.FixedAttemptPlan, error) {
		return core.FixedAttemptPlan{NonceKey: "nonce", FingerprintLabel: "fixed_intent_sha256", NonceLabel: "fixed_attempt",
			Labels:        labelsFromTags(leaseTags(cfg, claim.LeaseID, claim.Slug, "provisioning", req.Keep, core.FixedCreateTime(claim))),
			PrivateLabels: map[string]string{digitalOceanAccountLabel: account, "recovery": "ambiguous-create"},
		}, nil
	}, Submit: func(ctx context.Context, tx *core.FixedTransaction) (droplet, error) {
		claim := tx.Claim
		labels := maps.Clone(claim.Labels)
		item, err := creator.CreateFixedDroplet(ctx, cfg, publicKey, claim.LeaseID, claim.Slug, req.Keep, core.FixedCreateTime(*claim), tx.CreateLabels())
		if err != nil {
			var ambiguous *ambiguousDropletCreateError
			if errors.As(err, &ambiguous) {
				setDigitalOceanKeyIdentity(labels, ambiguous.keyID, ambiguous.keyCreated, ambiguous.keyOwnershipKnown)
			}
			return droplet{}, errors.Join(fmt.Errorf("DigitalOcean fixed create unresolved; replay or stop lease %s: %w", claim.LeaseID, err), tx.Observe(core.FixedResourceBinding{Labels: labels}))
		}
		setDigitalOceanKeyIdentity(labels, item.SSHKeyID, item.SSHKeyCreated, true)
		return item, tx.Observe(core.FixedResourceBinding{Labels: labels})
	}, PrepareAccess: func(ctx context.Context, tx *core.FixedTransaction, item droplet) (core.LeaseTarget, error) {
		claim := tx.Claim
		if err := validateFixedDroplet(*claim, item); err != nil {
			return core.LeaseTarget{}, err
		}
		if err := tx.Bind(core.FixedResourceBinding{CloudID: dropletIDString(item.ID), NumericID: item.ID, ImmutableID: dropletIDString(item.ID)}); err != nil {
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
	return core.LookupFixedResource(ctx, fixedLeaseKind, claim, func(ctx context.Context, claim core.LeaseClaim) (droplet, error) {
		if claim.CloudID != "" {
			id, ok := parseDropletID(claim.CloudID)
			if !ok {
				return droplet{}, core.Exit(4, "invalid fixed Droplet identity")
			}
			item, err := client.GetDroplet(ctx, id)
			if err == nil {
				err = validateFixedDroplet(claim, item)
			}
			return item, err
		}
		items, err := client.ListCrabboxDroplets(ctx)
		if err != nil {
			return droplet{}, err
		}
		item, found, err := core.SelectFixedCandidate(fixedLeaseKind, claim.LeaseID, items, func(item droplet) bool { return validateFixedDroplet(claim, item) == nil })
		if err == nil && !found {
			err = core.FixedUncertainCustody(claim.LeaseID)
		}
		return item, err
	})
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
	if lease, terminal, err := fixedLeaseKind.ResolveTerminal(claim, req.ReleaseOnly); terminal {
		return lease, true, err
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
		claim, err = core.CompareAndBindFixedClaim(claim, func(next *core.LeaseClaim, persist func() error) error {
			return core.BindFixedClaim(next, core.FixedResourceBinding{CloudID: dropletIDString(item.ID), NumericID: item.ID, ImmutableID: dropletIDString(item.ID), ImageEvidence: claim.ImageEvidence}, persist)
		})
		if err != nil {
			return core.LeaseTarget{}, true, err
		}
	}
	lease, err := b.targetFromDroplet(item, req, []droplet{item}, account)
	return lease, true, err
}
