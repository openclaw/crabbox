package sprites

import (
	"context"
	"fmt"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

var fixedSpriteKind = core.FixedLeaseKind{ClaimProvider: spritesProvider, IntentVersion: 1, Label: "Sprites", DeletionState: "deleting"}

const (
	fixedSpriteNonceKey       = "nonce"
	fixedSpriteNonceLabel     = "fixed_attempt"
	fixedSpriteAttemptPrefix  = "attempt-"
	fixedSpriteOrganizationKV = "sprites_organization"
)

func (*spritesBackend) SupportsRequestedLeaseID() bool { return true }

func (b *spritesBackend) now() time.Time { return core.ClockNow(b.rt.Clock) }

// Sprite names are unique per organization and rejected on reuse, so a fixed
// lease resubmits the same deterministic name. The per-attempt nonce label,
// written atomically with the create, attests a lost-reply or racing create.
func (b *spritesBackend) acquireFixed(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	// The Sprite name is derived from the lease ID, so only canonical IDs map
	// one-to-one onto names.
	if !core.IsCanonicalLeaseID(req.RequestedLeaseID) {
		return core.LeaseTarget{}, core.Exit(2, "provider=sprites requires a canonical lease ID, got %q", req.RequestedLeaseID)
	}
	if err := b.ensureCLI(ctx); err != nil {
		return core.LeaseTarget{}, err
	}
	cfg := b.configForRun()
	scope := core.ProviderClaimScope(spritesProvider, cfg)
	organization, err := b.client.GetOrganization(ctx)
	if err != nil {
		return core.LeaseTarget{}, spritesError("get organization", err)
	}
	var publicKey string
	lease, err := core.AcquireFixedResource(ctx, core.FixedAcquireOptions{
		Kind: fixedSpriteKind, LeaseID: req.RequestedLeaseID, RepoRoot: req.Repo.Root, Reclaim: req.Reclaim,
		TargetOS: targetLinux, TTL: cfg.TTL, IdleTimeout: cfg.IdleTimeout, Now: b.now,
	}, core.FixedLeaseOperations[spritesInfo]{
		Admission: &core.FixedAdmission{RepeatSameIdentity: true},
		DescribeIntent: func(ctx context.Context, claim *core.LeaseClaim, exists bool) (core.FixedLeaseBinding, error) {
			if exists && (claim.ProviderScope != scope || claim.Labels[fixedSpriteOrganizationKV] != "" && claim.Labels[fixedSpriteOrganizationKV] != organization) {
				return core.FixedLeaseBinding{}, core.Exit(4, "lease_id_conflict: Sprites endpoint or organization changed")
			}
			var err error
			publicKey, err = core.PrepareFixedSSHKey(&cfg, req.RequestedLeaseID, core.FixedKeyPolicy{RequireExisting: exists && claim.FixedCreateIntent.Attempt != nil, UseStored: true})
			if err != nil {
				return core.FixedLeaseBinding{}, err
			}
			fingerprint, err := core.FixedIntentFingerprint("sprites/v1\x00", struct {
				Scope, Organization, WorkRoot, Slug, Pond string
				TTL, Idle                                 time.Duration
				Keep                                      bool
			}{scope, organization, cfg.WorkRoot, core.NormalizeLeaseSlug(req.RequestedSlug), core.NormalizePondName(cfg.Pond), cfg.TTL, cfg.IdleTimeout, req.Keep})
			if err != nil {
				return core.FixedLeaseBinding{}, err
			}
			binding := core.FixedLeaseBinding{ProviderScope: scope, Fingerprint: fingerprint, InitialLabels: map[string]string{fixedSpriteOrganizationKV: organization}}
			if exists {
				return binding, nil
			}
			sprites, err := b.client.ListSprites(ctx, "crabbox-")
			if err != nil {
				return binding, spritesError("list sprites", err)
			}
			servers := make([]core.Server, 0, len(sprites))
			for _, sprite := range sprites {
				if isCrabboxSprite(sprite) {
					servers = append(servers, b.spriteToServer(sprite, nil))
				}
			}
			binding.AllocateSlug, binding.RequestedSlug, binding.Inventory = true, req.RequestedSlug, servers
			return binding, nil
		},
		Plan: func(_ context.Context, claim core.LeaseClaim) (core.FixedAttemptPlan, error) {
			name := core.LeaseProviderName(claim.LeaseID, claim.Slug)
			return core.FixedAttemptPlan{
				Values: map[string]string{"name": name}, NonceKey: fixedSpriteNonceKey, NonceBytes: 16,
				Labels:           map[string]string{"name": name, "work_root": cfg.WorkRoot},
				PrivateLabels:    map[string]string{fixedSpriteOrganizationKV: organization},
				DirectLabels:     &core.FixedDirectLabels{Config: cfg, Provider: spritesProvider, Keep: req.Keep},
				FingerprintLabel: "fixed_intent_sha256", NonceLabel: fixedSpriteNonceLabel,
			}, nil
		},
		ObserveExact: b.observeFixedSprite,
		Submit: func(ctx context.Context, tx *core.FixedTransaction) (spritesInfo, error) {
			claim := *tx.Claim
			name := claim.FixedCreateIntent.Attempt["name"]
			fmt.Fprintf(b.rt.Stderr, "provisioning provider=sprites lease=%s slug=%s sprite=%s keep=%v\n", claim.LeaseID, claim.Slug, name, req.Keep)
			_, createErr := b.client.CreateSprite(ctx, name, fixedSpriteLabels(claim))
			// The create reply is not identity evidence; a name conflict may be our
			// own earlier attempt. Only the attested live resource is accepted.
			sprite, err := b.client.GetSprite(ctx, name)
			if err == nil {
				err = validateFixedSprite(claim, sprite)
			}
			if err != nil {
				return spritesInfo{}, fmt.Errorf("Sprites create outcome unresolved for lease=%s sprite=%s; claim and key retained, retry the same lease ID or stop it: %w", claim.LeaseID, name, joinCreateError(createErr, err))
			}
			return sprite, nil
		},
		PrepareAccess: func(ctx context.Context, tx *core.FixedTransaction, sprite spritesInfo) (core.LeaseTarget, error) {
			if err := validateFixedSprite(*tx.Claim, sprite); err != nil {
				return core.LeaseTarget{}, err
			}
			if err := tx.Bind(core.FixedResourceBinding{CloudID: sprite.Name, ImmutableID: sprite.ID}); err != nil {
				return core.LeaseTarget{}, err
			}
			lease, err := b.prepareFixedLease(ctx, *tx.Claim, sprite, cfg.SSHKey, publicKey)
			if err == nil {
				fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s sprite=%s state=ready\n", lease.LeaseID, sprite.Name)
			}
			return lease, err
		},
	})
	return core.CompleteFixedAcquisition(lease, err, req)
}

func joinCreateError(createErr, observeErr error) error {
	if createErr == nil {
		return observeErr
	}
	return fmt.Errorf("%w (create: %v)", observeErr, spritesError("create sprite", createErr))
}

func fixedSpriteLabels(claim core.LeaseClaim) []string {
	return append(spritesAPILabels(claim.LeaseID, claim.Slug), fixedSpriteAttemptPrefix+claim.FixedCreateIntent.Attempt[fixedSpriteNonceKey])
}

func validateFixedSpriteClaim(cfg core.Config, claim core.LeaseClaim) error {
	scope := core.ProviderClaimScope(spritesProvider, cfg)
	if !fixedSpriteKind.IsFixedClaim(claim) || claim.ProviderScope != scope {
		return core.Exit(4, "lease_id_conflict: Sprites fixed lease %s belongs to another endpoint", claim.LeaseID)
	}
	if claim.FixedCreateIntent.Attempt == nil {
		return nil
	}
	_, err := core.ReadFixedAttempt[map[string]string](claim.FixedCreateIntent, core.FixedAttemptFormat{
		RejectEmptyObject: true, ExactKeys: 2, Required: []string{fixedSpriteNonceKey},
		Equal: map[string]string{"name": core.LeaseProviderName(claim.LeaseID, claim.Slug)},
	})
	if err == nil && claim.Labels[fixedSpriteOrganizationKV] == "" {
		err = core.Exit(4, "lease_id_conflict: Sprites fixed lease %s has no recorded organization", claim.LeaseID)
	}
	return err
}

// The live Sprite must carry this attempt's exact name, organization, labels
// and nonce. After binding, the immutable provider ID must also match.
func validateFixedSprite(claim core.LeaseClaim, sprite spritesInfo) error {
	attempt := claim.FixedCreateIntent.Attempt
	want := append(spritesAPILabels(claim.LeaseID, claim.Slug), fixedSpriteAttemptPrefix+attempt[fixedSpriteNonceKey])
	switch {
	case attempt[fixedSpriteNonceKey] == "" || sprite.Name != attempt["name"]:
	case strings.TrimSpace(sprite.ID) == "" || claim.CloudImmutableID != "" && sprite.ID != claim.CloudImmutableID:
	case claim.CloudID != "" && sprite.Name != claim.CloudID:
	case claim.Labels[fixedSpriteOrganizationKV] == "" || sprite.Organization != claim.Labels[fixedSpriteOrganizationKV]:
	case !spriteHasLabels(sprite, want):
	default:
		return nil
	}
	return core.Exit(4, "lease_id_conflict: sprite %q does not match the fixed create intent of lease %s", sprite.Name, claim.LeaseID)
}

func spriteHasLabels(sprite spritesInfo, want []string) bool {
	have := make(map[string]bool, len(sprite.Labels))
	for _, label := range sprite.Labels {
		have[label] = true
	}
	for _, label := range want {
		if !have[label] {
			return false
		}
	}
	return true
}

// A 404 alone cannot identify the account: a token for another organization
// sees every foreign Sprite as missing. Confirm the recorded organization first.
func (b *spritesBackend) observeFixedSprite(ctx context.Context, tx *core.FixedTransaction, mode core.FixedObserveMode) (core.FixedObservation[spritesInfo], error) {
	claim := *tx.Claim
	var result core.FixedObservation[spritesInfo]
	if err := validateFixedSpriteClaim(b.configForRun(), claim); err != nil {
		return result, err
	}
	intent := claim.FixedCreateIntent
	if intent.Attempt == nil {
		// No attempt was ever published, so nothing was ever submitted.
		result.CanSubmit, result.AbsenceProven = mode == core.FixedObserveAcquire, mode == core.FixedObserveDelete
		return result, nil
	}
	organization, err := b.client.GetOrganization(ctx)
	if err != nil {
		return result, spritesError("confirm organization", err)
	}
	if organization != claim.Labels[fixedSpriteOrganizationKV] {
		return result, core.Exit(4, "lease_id_conflict: Sprites token organization does not match fixed lease %s", claim.LeaseID)
	}
	name := intent.Attempt["name"]
	sprite, err := b.client.GetSprite(ctx, name)
	if isSpritesNotFound(err) {
		switch {
		case mode == core.FixedObserveDelete && (intent.State == "acquired" || intent.State == fixedSpriteKind.DeletionState || claim.CloudImmutableID != ""):
			result.AbsenceProven = true
		case mode == core.FixedObserveAcquire && claim.CloudImmutableID == "":
			result.CanSubmit = true
		case claim.CloudImmutableID != "":
			result.Conflict = fmt.Sprintf("bound sprite %q is gone; stop lease %s", name, claim.LeaseID)
		default:
			return result, core.Exit(4, "Sprites create for lease %s is unresolved; retry warmup with the same lease ID before stopping it", claim.LeaseID)
		}
		return result, nil
	}
	if err != nil {
		return result, spritesError("get sprite", err)
	}
	if err := validateFixedSprite(claim, sprite); err != nil {
		result.Conflict = fmt.Sprintf("sprite %q does not match the fixed create intent", name)
		return result, nil
	}
	result.Candidates = []spritesInfo{sprite}
	result.Binding = &core.FixedResourceBinding{CloudID: sprite.Name, ImmutableID: sprite.ID}
	return result, nil
}

func (b *spritesBackend) inspectFixedSprite(ctx context.Context, claim core.LeaseClaim) (spritesInfo, error) {
	observed, err := core.InspectFixedResource(ctx, fixedSpriteKind, claim, core.FixedLeaseOperations[spritesInfo]{ObserveExact: b.observeFixedSprite})
	if err != nil {
		return spritesInfo{}, err
	}
	if len(observed.Candidates) != 1 {
		return spritesInfo{}, core.FixedUncertainCustody(claim.LeaseID)
	}
	return observed.Candidates[0], nil
}

func (b *spritesBackend) prepareFixedLease(ctx context.Context, claim core.LeaseClaim, sprite spritesInfo, keyPath, publicKey string) (core.LeaseTarget, error) {
	lease, err := b.prepareLease(ctx, sprite, claim.LeaseID, claim.Slug, claim.Labels, keyPath, publicKey)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	lease.Server.ImmutableID = sprite.ID
	return lease, nil
}

func (b *spritesBackend) fixedSpriteServer(claim core.LeaseClaim, sprite spritesInfo) core.Server {
	// Like generic status, project recorded claim policy (TTL, idle timeout).
	server := b.spriteToServer(sprite, &claim)
	server.Labels = shared.LabelsWithDefaults(server.Labels, claim.Labels)
	server.ImmutableID = claim.CloudImmutableID
	server.Labels["lease"], server.Labels["slug"] = claim.LeaseID, claim.Slug
	return server
}

func (b *spritesBackend) resolveFixed(ctx context.Context, claim core.LeaseClaim, req core.ResolveRequest) (core.LeaseTarget, error) {
	cfg := b.configForRun()
	if err := validateFixedSpriteClaim(cfg, claim); err != nil {
		return core.LeaseTarget{}, err
	}
	name := core.Blank(claim.CloudID, claim.FixedCreateIntent.Attempt["name"])
	return core.ResolveFixedLeaseTarget(ctx, core.FixedResolveOptions{
		Kind: fixedSpriteKind, Request: req, Expected: claim, Provider: spritesProvider,
		ResourceName: name, TerminalState: "released", Now: b.now,
	}, func(ctx context.Context, claim *core.LeaseClaim, persist func() error) (core.LeaseTarget, error) {
		if err := core.CheckFixedAttemptActive(*claim, fixedSpriteNonceKey); err != nil {
			return core.LeaseTarget{}, err
		}
		if err := b.ensureCLI(ctx); err != nil {
			return core.LeaseTarget{}, err
		}
		sprite, err := b.inspectFixedSprite(ctx, *claim)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		if err := core.BindFixedClaim(claim, core.FixedResourceBinding{CloudID: sprite.Name, ImmutableID: sprite.ID}, persist); err != nil {
			return core.LeaseTarget{}, err
		}
		keyCfg := cfg
		publicKey, err := core.PrepareFixedSSHKey(&keyCfg, claim.LeaseID, core.FixedKeyPolicy{RequireExisting: true, UseStored: true})
		if err != nil {
			return core.LeaseTarget{}, err
		}
		lease, err := b.prepareFixedLease(ctx, *claim, sprite, keyCfg.SSHKey, publicKey)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		return lease, core.ValidateLeaseTargetProviderIdentity(lease, req.ExpectedProviderIdentity)
	}, func(ctx context.Context, claim core.LeaseClaim) (core.LeaseTarget, error) {
		if req.ReleaseOnly {
			// Deletion re-attests native identity under the claim fence.
			sprite := spritesInfo{ID: claim.CloudImmutableID, Name: name, Organization: claim.Labels[fixedSpriteOrganizationKV], Labels: fixedSpriteLabels(claim)}
			return core.LeaseTarget{LeaseID: claim.LeaseID, Server: b.fixedSpriteServer(claim, sprite)}, nil
		}
		sprite, err := b.inspectFixedSprite(ctx, claim)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		lease := core.LeaseTarget{LeaseID: claim.LeaseID, Server: b.fixedSpriteServer(claim, sprite)}
		if lease.SSH, err = b.sshTarget(sprite.Name, ""); err != nil {
			return core.LeaseTarget{}, err
		}
		if err := core.UseStoredTestboxKey(&lease.SSH, claim.LeaseID); err != nil {
			return core.LeaseTarget{}, err
		}
		return lease, core.ValidateLeaseTargetProviderIdentity(lease, req.ExpectedProviderIdentity)
	})
}

func (b *spritesBackend) releaseFixed(ctx context.Context, claim core.LeaseClaim) error {
	if err := validateFixedSpriteClaim(b.configForRun(), claim); err != nil {
		return err
	}
	alreadyReleased := claim.FixedCreateIntent.State == "released"
	err := core.DeleteFixedResource(ctx, fixedSpriteKind, claim, core.FixedLeaseOperations[spritesInfo]{
		Release:      &core.FixedReleasePolicy{SkipTerminalObservation: true},
		ObserveExact: b.observeFixedSprite,
		DeleteExact: func(ctx context.Context, tx *core.FixedTransaction, sprite spritesInfo) error {
			// Sprites deletes by name; re-attest the exact resource immediately before.
			current, err := b.client.GetSprite(ctx, sprite.Name)
			if isSpritesNotFound(err) {
				return nil
			}
			if err != nil {
				return spritesError("get sprite", err)
			}
			if err := validateFixedSprite(*tx.Claim, current); err != nil {
				return err
			}
			if err := b.client.DeleteSprite(ctx, current.Name); err != nil && !isSpritesNotFound(err) {
				return spritesError("delete sprite", err)
			}
			if _, err := b.client.GetSprite(ctx, current.Name); !isSpritesNotFound(err) {
				if err == nil {
					err = fmt.Errorf("sprite %q still exists after deletion", current.Name)
				}
				return spritesError("confirm sprite deletion", err)
			}
			return nil
		},
	}, b.now)
	if err != nil {
		return err
	}
	core.RemoveStoredTestboxKey(claim.LeaseID)
	if alreadyReleased {
		return nil
	}
	fmt.Fprintf(b.rt.Stderr, "released lease=%s sprite=%s\n", claim.LeaseID, core.Blank(claim.CloudID, claim.FixedCreateIntent.Attempt["name"]))
	return nil
}

func (*spritesBackend) RetainLeaseClaimAfterReleaseWithClaim(lease core.LeaseTarget, previous core.LeaseClaim) (bool, error) {
	return fixedSpriteKind.RetainClaimAfterRelease(lease.LeaseID, previous, fixedSpriteKind.IsFixedClaim(previous), nil, nil)
}
