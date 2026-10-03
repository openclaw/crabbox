package linode

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

var fixedLeaseKind = core.FixedLeaseKind{ClaimProvider: providerName, IntentVersion: 1, Label: "Linode"}

func (*linodeLeaseBackend) SupportsRequestedLeaseID() bool { return true }

func (b *linodeLeaseBackend) acquireFixed(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	if b.acquireConfigErr != nil {
		return core.LeaseTarget{}, b.acquireConfigErr
	}
	cfg := b.Cfg
	cfg.ServerType = linodeServerTypeForConfig(cfg)
	if cfg.TargetOS != core.TargetLinux {
		return core.LeaseTarget{}, core.Exit(2, "provider=linode only supports target=linux")
	}
	if cfg.Tailscale.Enabled && cfg.Tailscale.AuthKey == "" {
		return core.LeaseTarget{}, core.Exit(2, "direct --tailscale requires %s", cfg.Tailscale.AuthKeyEnv)
	}
	client, err := b.clientFactory(b.RT)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	account, err := client.AccountID(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if account == "" {
		return core.LeaseTarget{}, core.Exit(2, "Linode account identity is missing")
	}
	createReq := createLinodeRequest{Region: linodeRegionForConfig(cfg), Type: cfg.ServerType, Image: linodeImageForConfig(cfg)}
	firewallID, err := parseLinodeFirewallID(cfg.Linode.FirewallID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if firewallID > 0 {
		settings, err := client.AccountSettings(ctx)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		if err := configureLinodeFirewall(&createReq, firewallID, settings.InterfacesForNewLinodes); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	var publicKey string
	lease, err := core.AcquireFixedResource(ctx, core.FixedAcquireOptions{
		Kind: fixedLeaseKind, LeaseID: req.RequestedLeaseID, RepoRoot: req.Repo.Root, Reclaim: req.Reclaim,
		TargetOS: core.TargetLinux, TTL: cfg.TTL, IdleTimeout: cfg.IdleTimeout, Now: func() time.Time { return core.ClockNow(b.RT.Clock) },
	}, core.FixedLeaseOperations[linodeInstance]{Admission: &core.FixedAdmission{}, DescribeIntent: func(ctx context.Context, claim *core.LeaseClaim, exists bool) (core.FixedLeaseBinding, error) {
		if exists && (!fixedLeaseKind.IsFixedClaim(*claim) || claim.ProviderScope != account) {
			return core.FixedLeaseBinding{}, core.Exit(4, "lease_id_conflict: Linode owner or account changed")
		}
		if exists && claim.FixedCreateIntent.Attempt != nil {
			// Optional key lookup may succeed without a key; fixed replay must not regenerate it.
			stored := core.SSHTarget{}
			if err := core.UseStoredTestboxKey(&stored, claim.LeaseID); err != nil || stored.Key == "" {
				return core.FixedLeaseBinding{}, core.Exit(4, "lease_id_conflict: Linode fixed lease requires its stored SSH key")
			}
		}
		var err error
		publicKey, err = core.PrepareFixedSSHKey(&cfg, req.RequestedLeaseID, core.FixedKeyPolicy{RequireExisting: exists && claim.FixedCreateIntent.Attempt != nil, UseStored: true})
		if err != nil {
			return core.FixedLeaseBinding{}, err
		}
		fingerprint, err := core.FixedIntentFingerprint("", struct {
			Labels                                                    map[string]string
			Provider                                                  createLinodeRequest
			Type, Architecture, Bootstrap, Slug, User, Port, WorkRoot string
			Keep                                                      bool
			TTL, Idle                                                 time.Duration
		}{core.DirectLeaseLabels(cfg, req.RequestedLeaseID, req.RequestedSlug, providerName, "", req.Keep, time.Unix(0, 0)), createReq, cfg.ServerType, cfg.Architecture, core.CloudInitUserData(cfg, publicKey), core.NormalizeLeaseSlug(req.RequestedSlug), cfg.SSHUser, cfg.SSHPort, cfg.WorkRoot, req.Keep, cfg.TTL, cfg.IdleTimeout})
		if err != nil {
			return core.FixedLeaseBinding{}, err
		}
		binding := core.FixedLeaseBinding{ProviderScope: account, Fingerprint: fingerprint}
		if exists {
			return binding, nil
		}
		linodeInstances, err := client.ListLinodes(ctx)
		if err != nil {
			return binding, err
		}
		var servers []core.Server
		for _, item := range linodeInstances {
			server := serverFromLinode(item, cfg)
			if isOwnedLinode(item) {
				servers = append(servers, server)
			}
		}
		binding.AllocateSlug, binding.RejectExistingLease, binding.RequestedSlug, binding.Inventory = true, true, req.RequestedSlug, servers
		return binding, nil
	}, ObserveExact: func(ctx context.Context, tx *core.FixedTransaction, _ core.FixedObserveMode) (core.FixedObservation[linodeInstance], error) {
		claim := tx.Claim
		if cfg.Tailscale.Enabled && cfg.Tailscale.Hostname == "" {
			cfg.Tailscale.Hostname = core.RenderTailscaleHostname(cfg.Tailscale.HostnameTemplate, claim.LeaseID, claim.Slug, cfg.Provider)
		}
		if claim.FixedCreateIntent.Attempt == nil {
			return core.FixedObservation[linodeInstance]{CanSubmit: true}, nil
		}
		item, err := loadFixedLinode(ctx, client, *claim)
		if err != nil {
			return core.FixedObservation[linodeInstance]{}, err
		}
		if err := validateFixedLinode(*claim, item); err != nil {
			return core.FixedObservation[linodeInstance]{}, err
		}
		return core.FixedObservation[linodeInstance]{Candidates: []linodeInstance{item}}, nil
	}, Plan: func(ctx context.Context, claim core.LeaseClaim) (core.FixedAttemptPlan, error) {
		return core.FixedAttemptPlan{NonceKey: "nonce", FingerprintLabel: "fixed_intent_sha256", NonceLabel: "fixed_attempt",
			Labels:        labelsFromTags(leaseTags(cfg, claim.LeaseID, claim.Slug, "provisioning", req.Keep, core.FixedCreateTime(claim))),
			PrivateLabels: map[string]string{linodeAccountLabel: account, "recovery": "ambiguous-create"},
		}, nil
	}, Submit: func(ctx context.Context, tx *core.FixedTransaction) (linodeInstance, error) {
		claim := tx.Claim
		createReq.Label, createReq.Tags = core.LeaseProviderName(claim.LeaseID, claim.Slug), tagsFromLabels(tx.CreateLabels())
		createReq.AuthorizedKeys = []string{publicKey}
		createReq.Metadata = &linodeMetadata{UserData: linodeUserData(cfg, publicKey)}
		createReq.RootPass, err = generateLinodeRootPass()
		if err != nil {
			return linodeInstance{}, err
		}
		item, err := client.CreateLinode(ctx, createReq)
		if err != nil {
			return linodeInstance{}, fmt.Errorf("Linode fixed create unresolved; replay or stop lease %s: %w", claim.LeaseID, err)
		}
		return item, nil
	}, PrepareAccess: func(ctx context.Context, tx *core.FixedTransaction, item linodeInstance) (core.LeaseTarget, error) {
		claim := tx.Claim
		if err := validateFixedLinode(*claim, item); err != nil {
			return core.LeaseTarget{}, err
		}
		if err := tx.Bind(core.FixedResourceBinding{CloudID: strconv.FormatInt(item.ID, 10), NumericID: item.ID, ImmutableID: strconv.FormatInt(item.ID, 10)}); err != nil {
			return core.LeaseTarget{}, err
		}
		item, err := b.waitForLinodeIP(ctx, client, item.ID, 5*time.Minute)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		if err := validateFixedLinode(*claim, item); err != nil {
			return core.LeaseTarget{}, err
		}
		server := serverFromLinode(item, cfg)
		server.ImmutableID = claim.CloudImmutableID
		target := core.SSHTargetFromConfig(cfg, server.PublicNet.IPv4.IP)
		if err := b.waitSSH(ctx, &target, "linode bootstrap", core.BootstrapWaitTimeout(cfg)); err != nil {
			return core.LeaseTarget{}, err
		}
		labels := maps.Clone(server.Labels)
		labels["state"] = "ready"
		if err := client.UpdateLinodeTags(ctx, item.ID, shared.ReplaceCrabboxTags(item.Tags, tagsFromLabels(labels))); err != nil {
			return core.LeaseTarget{}, err
		}
		labels[linodeAccountLabel] = account
		server.Labels, server.Status = labels, "ready"
		return core.LeaseTarget{Server: server, SSH: target, LeaseID: claim.LeaseID}, nil
	}})
	if err == nil && req.OnAcquired != nil {
		err = req.OnAcquired(lease)
	}
	return lease, err
}

func validateFixedLinode(claim core.LeaseClaim, item linodeInstance) error {
	intent := claim.FixedCreateIntent
	labels := labelsFromTags(item.Tags)
	if !fixedLeaseKind.IsFixedClaim(claim) || intent.State == "released" || intent.Attempt["nonce"] == "" ||
		item.ID <= 0 || item.Label != core.LeaseProviderName(claim.LeaseID, claim.Slug) || !isOwnedLinode(item) ||
		labels["lease"] != claim.LeaseID || labels["slug"] != claim.Slug || labels["provider_key"] != claim.Labels["provider_key"] ||
		labels["fixed_attempt"] != intent.Attempt["nonce"] || labels["fixed_intent_sha256"] != intent.Fingerprint ||
		(claim.CloudID != "" && claim.CloudID != strconv.FormatInt(item.ID, 10)) {
		return core.Exit(4, "lease_id_conflict: Linode instance does not match fixed create intent")
	}
	return nil
}

func loadFixedLinode(ctx context.Context, client linodeAPI, claim core.LeaseClaim) (linodeInstance, error) {
	return core.LookupFixedResource(ctx, fixedLeaseKind, claim, func(ctx context.Context, claim core.LeaseClaim) (linodeInstance, error) {
		if claim.CloudID != "" {
			id, ok := parseLinodeID(claim.CloudID)
			if !ok {
				return linodeInstance{}, core.Exit(4, "invalid fixed Linode identity")
			}
			item, err := client.GetLinode(ctx, id)
			if err == nil {
				err = validateFixedLinode(claim, item)
			}
			return item, err
		}
		items, err := client.ListLinodes(ctx)
		if err != nil {
			return linodeInstance{}, err
		}
		item, found, err := core.SelectFixedCandidate(fixedLeaseKind, claim.LeaseID, items, func(item linodeInstance) bool { return validateFixedLinode(claim, item) == nil })
		if err == nil && !found {
			err = core.FixedUncertainCustody(claim.LeaseID)
		}
		return item, err
	})
}

func (b *linodeLeaseBackend) RetainLeaseClaimAfterReleaseWithClaim(lease core.LeaseTarget, previous core.LeaseClaim) (bool, error) {
	return fixedLeaseKind.RetainClaimAfterRelease(lease.LeaseID, previous, lease.Server.Labels["fixed_attempt"] != "", nil, nil)
}

func (b *linodeLeaseBackend) resolveFixed(ctx context.Context, client linodeAPI, req core.ResolveRequest, account string) (core.LeaseTarget, bool, error) {
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
		return core.LeaseTarget{}, true, core.Exit(4, "lease_id_conflict: Linode fixed lease account changed")
	}
	if lease, terminal, err := fixedLeaseKind.ResolveTerminal(claim, req.ReleaseOnly); terminal {
		return lease, true, err
	}

	item, err := loadFixedLinode(ctx, client, claim)
	if err != nil {
		if req.ReleaseOnly && claim.CloudID != "" && isLinodeNotFound(err) {
			id, _ := parseLinodeID(claim.CloudID)
			server := core.Server{Provider: providerName, CloudID: claim.CloudID, ID: id, Name: core.LeaseProviderName(claim.LeaseID, claim.Slug), Labels: maps.Clone(claim.Labels)}
			core.SetServerLeaseClaimSnapshot(&server, claim, true)
			return core.LeaseTarget{LeaseID: claim.LeaseID, Server: server}, true, nil
		}
		return core.LeaseTarget{}, true, err
	}
	if err := validateFixedLinode(claim, item); err != nil {
		return core.LeaseTarget{}, true, err
	}
	if claim.CloudID == "" && req.ReleaseOnly {
		claim, err = core.CompareAndBindFixedClaim(claim, func(next *core.LeaseClaim, persist func() error) error {
			return core.BindFixedClaim(next, core.FixedResourceBinding{CloudID: strconv.FormatInt(item.ID, 10), NumericID: item.ID, ImmutableID: strconv.FormatInt(item.ID, 10), ImageEvidence: claim.ImageEvidence}, persist)
		})
		if err != nil {
			return core.LeaseTarget{}, true, err
		}
	}
	lease, err := b.targetFromLinode(item, req, []linodeInstance{item}, account)
	return lease, true, err
}
