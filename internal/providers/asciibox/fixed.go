package asciibox

import (
	"context"
	"errors"
	"fmt"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

var fixedBoxKind = core.FixedLeaseKind{ClaimProvider: providerName, IntentVersion: 1, Label: providerName, DeletionState: "deleting"}

func (*backend) SupportsRequestedLeaseID() bool { return true }

func fixedBoxKey(claim core.LeaseClaim) string {
	return "crabbox-" + claim.LeaseID + "-" + claim.FixedCreateIntent.Fingerprint
}

func (b *backend) acquireFixed(ctx context.Context, cfg core.Config, client api, req core.AcquireRequest) (core.LeaseTarget, error) {
	ttl := req.Options.TTL
	if ttl <= 0 {
		ttl = cfg.TTL
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	ttl = ttl.Round(time.Second)
	cfg.TTL = ttl
	lease, err := core.AcquireFixedResource(ctx, core.FixedAcquireOptions{
		Kind: fixedBoxKind, LeaseID: req.RequestedLeaseID, RepoRoot: req.Repo.Root, Reclaim: req.Reclaim,
		TargetOS: targetLinux, TTL: ttl, IdleTimeout: cfg.IdleTimeout, Now: b.now,
	}, core.FixedLeaseOperations[boxData]{
		Admission: &core.FixedAdmission{FreshOnly: true, KeyedRetry: &core.FixedKeyedRetry{AttemptKey: "idempotency_key", Window: asciiBoxIdempotencyWindow}},
		DescribeIntent: func(_ context.Context, _ *core.LeaseClaim, _ bool) (core.FixedLeaseBinding, error) {
			scope := (Provider{}).ClaimScope(cfg)
			fingerprint, err := core.FixedIntentFingerprint("ascii-box/v1\x00", struct {
				Scope, Workdir, Slug, Pond string
				TTL, Idle                  time.Duration
				Keep                       bool
			}{scope, cfg.WorkRoot, core.NormalizeLeaseSlug(req.RequestedSlug), core.NormalizePondName(cfg.Pond), ttl, cfg.IdleTimeout, req.Keep})
			return core.FixedLeaseBinding{ProviderScope: scope, Fingerprint: fingerprint, AllocateSlug: true, RequestedSlug: req.RequestedSlug}, err
		},
		Plan: func(_ context.Context, claim core.LeaseClaim) (core.FixedAttemptPlan, error) {
			return core.FixedAttemptPlan{
				Values:       map[string]string{"idempotency_key": fixedBoxKey(claim)},
				DirectLabels: &core.FixedDirectLabels{Config: cfg, Provider: providerName, Keep: req.Keep},
			}, nil
		},
		ObserveExact: fixedBoxObserver(cfg, client),
		Submit: func(ctx context.Context, tx *core.FixedTransaction) (boxData, error) {
			box, createErr := client.CreateBox(ctx, createRequest{TTL: ttl, IdempotencyKey: tx.Claim.FixedCreateIntent.Attempt["idempotency_key"]})
			if concreteBoxID(box.createdID) && box.ID == box.createdID {
				server := fixedBoxServer(cfg, box, *tx.Claim)
				if err := tx.Observe(core.FixedResourceBinding{CloudID: box.ID, ImmutableID: server.ImmutableID, Labels: server.Labels}); err != nil {
					return boxData{}, err
				}
			}
			if createErr != nil {
				return boxData{}, createErr
			}
			observed, err := core.InspectFixedResource(ctx, fixedBoxKind, *tx.Claim, core.FixedLeaseOperations[boxData]{ObserveExact: fixedBoxObserver(cfg, client)})
			if err != nil {
				return boxData{}, err
			}
			if len(observed.Candidates) != 1 {
				return boxData{}, core.FixedUncertainCustody(tx.Claim.LeaseID)
			}
			return observed.Candidates[0], nil
		},
		PrepareAccess: func(ctx context.Context, tx *core.FixedTransaction, box boxData) (core.LeaseTarget, error) {
			server := fixedBoxServer(cfg, box, *tx.Claim)
			if err := tx.Bind(core.FixedResourceBinding{CloudID: box.ID, ImmutableID: server.ImmutableID, Labels: server.Labels}); err != nil {
				return core.LeaseTarget{}, err
			}
			return b.prepareFixedBox(ctx, cfg, client, *tx.Claim, box)
		},
	})
	return core.CompleteFixedAcquisition(lease, err, req)
}

func validateFixedBoxClaim(cfg core.Config, claim core.LeaseClaim) error {
	scope := (Provider{}).ClaimScope(cfg)
	if scope == "" {
		return fmt.Errorf("ascii-box endpoint scope is unavailable")
	}
	if err := core.ValidateFixedClaim(claim, core.FixedClaimRules{
		Kind: fixedBoxKind, Scope: scope, IntentScope: scope,
		States: []string{"prepared", "acquired", "deleting", "released"}, RequireCanonicalID: true, RequireSlug: true,
		RequireTimestamp: true, RequireSHA256: true, NoCheckpoint: true, NoFailedAttempts: true, NoNumericID: true,
	}); err != nil {
		return err
	}
	_, err := core.ReadFixedAttempt[map[string]string](claim.FixedCreateIntent, core.FixedAttemptFormat{
		RejectEmptyObject: true, Equal: map[string]string{"idempotency_key": fixedBoxKey(claim)},
		OptionalEqual: map[string]string{"deletion_completed": "true"},
	})
	return err
}

func fixedBoxObserver(cfg core.Config, client api) func(context.Context, *core.FixedTransaction, core.FixedObserveMode) (core.FixedObservation[boxData], error) {
	return func(ctx context.Context, tx *core.FixedTransaction, mode core.FixedObserveMode) (core.FixedObservation[boxData], error) {
		claim := *tx.Claim
		var result core.FixedObservation[boxData]
		if err := validateFixedBoxClaim(cfg, claim); err != nil {
			return result, err
		}
		if claim.CloudID == "" {
			result.CanSubmit = true
			return result, nil
		}
		if !concreteBoxID(claim.CloudID) {
			return result, fmt.Errorf("ascii-box fixed claim has no concrete Box ID")
		}
		box, err := client.GetBox(ctx, claim.CloudID)
		if err != nil {
			if mode == core.FixedObserveAcquire && isNotFound(err) {
				return result, nil
			}
			if mode != core.FixedObserveDelete || !isNotFound(err) {
				return result, err
			}
			evidence, err := boxAbsenceVerifier(client).VerifyResourceAbsent(ctx, claim)
			result.AbsenceProven = evidence.ExactNotFound && evidence.InventoryComplete
			return result, err
		}
		if box.ID != claim.CloudID || boxCreationTime(box) == "" || claim.CloudImmutableID != "" && boxCreationTime(box) != claim.CloudImmutableID {
			result.Conflict = "ascii-box native identity is missing or changed"
			return result, nil
		}
		if mode == core.FixedObserveDelete {
			box.deletionOperationID = claim.FixedCreateIntent.Attempt["deletion_operation_id"]
			box.deletionCompleted = claim.FixedCreateIntent.Attempt["deletion_completed"] == "true"
			box, err = exactBoxForRelease(ctx, client, box)
			if err != nil {
				return result, err
			}
		}
		result.Candidates = []boxData{box}
		return result, nil
	}
}

func fixedBoxServer(cfg core.Config, box boxData, claim core.LeaseClaim) core.Server {
	server := recordedBoxServer(cfg, box, claim)
	server.ImmutableID = boxCreationTime(box)
	return server
}

func (b *backend) prepareFixedBox(ctx context.Context, cfg core.Config, client api, claim core.LeaseClaim, box boxData) (core.LeaseTarget, error) {
	ready, err := client.waitForBoxReady(ctx, box)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if err := validateBoxIdentity(ready, box); err != nil {
		return core.LeaseTarget{}, err
	}
	if err := client.PrepareSSH(ctx, box.ID); err != nil {
		return core.LeaseTarget{}, err
	}
	lease, err := b.leaseFromBox(ctx, cfg, ready, claim)
	lease.Server.ImmutableID = boxCreationTime(box)
	return lease, err
}

func (b *backend) resolveFixed(ctx context.Context, cfg core.Config, client api, claim core.LeaseClaim, req core.ResolveRequest) (core.LeaseTarget, error) {
	if err := validateFixedBoxClaim(cfg, claim); err != nil {
		return core.LeaseTarget{}, err
	}
	observe := func(ctx context.Context, claim core.LeaseClaim) (boxData, error) {
		result, err := core.InspectFixedResource(ctx, fixedBoxKind, claim, core.FixedLeaseOperations[boxData]{ObserveExact: fixedBoxObserver(cfg, client)})
		if err != nil {
			return boxData{}, err
		}
		if len(result.Candidates) != 1 {
			return boxData{}, core.FixedUncertainCustody(claim.LeaseID)
		}
		return result.Candidates[0], nil
	}
	return core.ResolveFixedLeaseTarget(ctx, core.FixedResolveOptions{Kind: fixedBoxKind, Request: req, Expected: claim, Provider: providerName, ResourceName: claim.CloudID, TerminalState: "released", Now: b.now},
		func(ctx context.Context, claim *core.LeaseClaim, persist func() error) (core.LeaseTarget, error) {
			if err := core.CheckFixedAttemptActive(*claim, "idempotency_key", "deletion_operation_id", "deletion_completed"); err != nil {
				return core.LeaseTarget{}, err
			}
			box, err := observe(ctx, *claim)
			if err != nil {
				return core.LeaseTarget{}, err
			}
			if err := core.BindFixedClaim(claim, core.FixedResourceBinding{CloudID: box.ID, ImmutableID: boxCreationTime(box), Labels: fixedBoxServer(cfg, box, *claim).Labels}, persist); err != nil {
				return core.LeaseTarget{}, err
			}
			return b.prepareFixedBox(ctx, cfg, client, *claim, box)
		},
		func(ctx context.Context, claim core.LeaseClaim) (core.LeaseTarget, error) {
			if req.ReleaseOnly {
				// The engine re-attests native evidence inside the deletion fence.
				return core.LeaseTarget{LeaseID: claim.LeaseID, Server: fixedBoxServer(cfg, boxFromClaim(claim), claim)}, nil
			}
			box, err := observe(ctx, claim)
			server := observedBoxServer(cfg, box, claim.LeaseID, claim.Slug, &claim)
			server.ImmutableID = boxCreationTime(box)
			return core.LeaseTarget{LeaseID: claim.LeaseID, Server: server}, err
		})
}

func (b *backend) releaseFixed(ctx context.Context, cfg core.Config, client api, claim core.LeaseClaim, beforeRelease func(boxData)) (core.ReleaseLeaseOutcome, error) {
	var outcome core.ReleaseLeaseOutcome
	if err := validateFixedBoxClaim(cfg, claim); err != nil {
		return outcome, err
	}
	err := core.DeleteFixedResource(ctx, fixedBoxKind, claim, core.FixedLeaseOperations[boxData]{
		Release:      &core.FixedReleasePolicy{Outcome: &outcome, SkipTerminalObservation: true},
		ObserveExact: fixedBoxObserver(cfg, client),
		DeleteExact: func(ctx context.Context, tx *core.FixedTransaction, box boxData) error {
			var witnessErr error
			err := releaseExactBox(ctx, client, box, beforeRelease, func() {
				witnessErr = core.RecordFixedWitness(tx.Claim, "deletion_completed", "true", tx.PersistDeletionEvidence)
			})
			var incomplete *boxDeletionIncompleteError
			if errors.As(err, &incomplete) && validateBoxDeletionOperation(incomplete.operation, box.ID, "") == nil {
				return errors.Join(err, core.RecordFixedWitness(tx.Claim, "deletion_operation_id", incomplete.operation.ID, tx.PersistDeletionEvidence))
			}
			return errors.Join(err, witnessErr)
		},
	})
	return outcome, err
}

func (*backend) RetainLeaseClaimAfterReleaseWithClaim(lease core.LeaseTarget, previous core.LeaseClaim) (bool, error) {
	return fixedBoxKind.RetainClaimAfterRelease(lease.LeaseID, previous, false, nil, nil)
}

func (b *backend) statusFixed(ctx context.Context, cfg core.Config, client api, claim core.LeaseClaim, req core.StatusRequest) (core.StatusView, error) {
	if err := validateFixedBoxClaim(cfg, claim); err != nil {
		return core.StatusView{}, err
	}
	return shared.PollStatus(ctx, req, b.now, func(ctx context.Context) (core.StatusView, bool, error) {
		if _, terminal, err := fixedBoxKind.ResolveTerminal(claim, true); terminal {
			return core.StatusView{ID: claim.LeaseID, Slug: claim.Slug, Provider: providerName, TargetOS: targetLinux, State: "released"}, true, err
		}
		observed, err := core.InspectFixedResource(ctx, fixedBoxKind, claim, core.FixedLeaseOperations[boxData]{ObserveExact: fixedBoxObserver(cfg, client)})
		if err != nil {
			return core.StatusView{}, false, err
		}
		if len(observed.Candidates) != 1 {
			return core.StatusView{}, false, core.FixedUncertainCustody(claim.LeaseID)
		}
		view := statusFromBox(cfg, observed.Candidates[0], claim.LeaseID, claim.Slug, &claim)
		return view, boxStateFailed(view.State), nil
	}, func() error { return core.Exit(5, "timed out waiting for ascii-box %s", claim.CloudID) })
}
