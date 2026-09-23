package cli

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestFixedEngineObservedBindingPrecedesAccess(t *testing.T) {
	for _, scenario := range []string{"recover", "identity-conflict", "already-bound"} {
		t.Run(scenario, func(t *testing.T) {
			conflict := scenario == "identity-conflict"
			onlyUnbound := scenario == "already-bound"
			isolateTestUserDirs(t)
			claim := fixedCleanupClaim(t, "prepared")
			binding := FixedResourceBinding{OnlyUnbound: onlyUnbound, CloudID: claim.CloudID, ImmutableID: claim.CloudImmutableID, Labels: map[string]string{"child": "original"}}
			if conflict {
				binding.ImmutableID = "replacement"
			}
			failure := errors.New("access unavailable")
			ops := FixedLeaseOperations[string]{
				Admission: &FixedAdmission{},
				DescribeIntent: func(context.Context, *LeaseClaim, bool) (FixedLeaseBinding, error) {
					return FixedLeaseBinding{ProviderScope: claim.ProviderScope, Fingerprint: claim.FixedCreateIntent.Fingerprint}, nil
				},
				Plan: func(context.Context, LeaseClaim) (FixedAttemptPlan, error) {
					t.Fatal("recovery planned a replacement")
					return FixedAttemptPlan{}, nil
				},
				ObserveExact: func(context.Context, *FixedTransaction, FixedObserveMode) (FixedObservation[string], error) {
					return FixedObservation[string]{Candidates: []string{claim.CloudID}, Binding: &binding}, nil
				},
				Submit: func(context.Context, *FixedTransaction) (string, error) {
					t.Fatal("recovery submitted a replacement")
					return "", nil
				},
				PrepareAccess: func(context.Context, *FixedTransaction, string) (LeaseTarget, error) {
					if conflict {
						t.Fatal("conflicting binding reached access preparation")
					}
					durable, err := ReadLeaseClaim(claim.LeaseID)
					if err != nil {
						t.Fatal(err)
					}
					if onlyUnbound {
						if !reflect.DeepEqual(durable, claim) {
							t.Fatal("already-bound observation rewrote custody")
						}
					} else if durable.Labels["child"] != "original" || durable.FixedCreateIntent.Journal == nil || durable.FixedCreateIntent.Journal.Phase != "bound" {
						t.Fatal("access preceded durable recovery evidence")
					}
					return LeaseTarget{}, failure
				},
			}
			// Inspection returns facts without publishing the adapter's binding.
			if _, err := InspectFixedResource(t.Context(), fixedCleanupKind(), claim, ops); err != nil {
				t.Fatal(err)
			}
			before, _ := ReadLeaseClaim(claim.LeaseID)
			if !reflect.DeepEqual(before, claim) {
				t.Fatal("inspection published binding evidence")
			}
			_, err := AcquireFixedResource(t.Context(), FixedAcquireOptions{Kind: fixedCleanupKind(), LeaseID: claim.LeaseID, RepoRoot: claim.RepoRoot}, ops)
			if err == nil || (!conflict && !errors.Is(err, failure)) {
				t.Fatalf("recovery error: %v", err)
			}
			after, readErr := ReadLeaseClaim(claim.LeaseID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if (conflict || onlyUnbound) && !reflect.DeepEqual(after, claim) || scenario == "recover" && after.Labels["child"] != "original" {
				t.Fatal("failed recovery lost or changed custody")
			}
		})
	}
}

func TestFixedEngineAbsenceRecoveryUsesReleaseFence(t *testing.T) {
	for _, scenario := range []string{"absent", "unproven", "lookup-error", "owner", "checkpoint", "stale"} {
		t.Run(scenario, func(t *testing.T) {
			isolateTestUserDirs(t)
			claim := fixedCleanupClaim(t, "acquired")
			if scenario == "checkpoint" {
				if err := WithDurableLeaseClaimLock(claim.LeaseID, func(c *LeaseClaim, _ bool, persist func() error) error {
					c.CheckpointCapture = &CheckpointCaptureBinding{ID: "capture", Revision: "capture-revision"}
					return persist()
				}); err != nil {
					t.Fatal(err)
				}
				claim, _ = ReadLeaseClaim(claim.LeaseID)
			}
			current := CloneLeaseClaim(claim)
			if scenario == "stale" {
				claim.Revision = "stale"
			}
			checkpointID := ""
			outcome := ReleaseLeaseOutcome{}
			policy := &FixedReleasePolicy{Outcome: &outcome, RepoRoot: claim.RepoRoot, CheckpointID: &checkpointID}
			if scenario == "owner" {
				policy.RepoRoot = "/another-owner"
			}
			err := DeleteFixedResource(t.Context(), fixedCleanupKind(), claim, FixedLeaseOperations[string]{
				Release: policy,
				ObserveExact: func(context.Context, *FixedTransaction, FixedObserveMode) (FixedObservation[string], error) {
					if scenario == "owner" || scenario == "checkpoint" || scenario == "stale" {
						t.Fatal("absence lookup crossed a rejected ownership fence")
					}
					if scenario == "lookup-error" {
						return FixedObservation[string]{AbsenceProven: true}, errors.New("inventory unavailable")
					}
					return FixedObservation[string]{AbsenceProven: scenario == "absent"}, nil
				},
				DeleteExact: func(context.Context, *FixedTransaction, string) error {
					t.Fatal("absence-only recovery issued deletion")
					return nil
				},
			})
			after, readErr := ReadLeaseClaim(claim.LeaseID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if scenario == "absent" {
				if err != nil || !outcome.Terminal {
					t.Fatalf("absence recovery failed: %+v %v", outcome, err)
				}
				if err := fixedCleanupKind().ValidateTerminalClaim(after, current, claim.LeaseID, nil); err != nil {
					t.Fatal(err)
				}
			} else if err == nil || outcome.Terminal || !reflect.DeepEqual(current, after) {
				t.Fatalf("unproven absence changed custody: %+v %v", outcome, err)
			}
		})
	}
}

func TestFixedEngineTerminalRecoveryRetainsReceipt(t *testing.T) {
	for _, scenario := range []string{"cleanup-error", "observation-error", "invalid-receipt", "stale"} {
		t.Run(scenario, func(t *testing.T) {
			isolateTestUserDirs(t)
			claim := fixedCleanupClaim(t, "acquired")
			kind := fixedCleanupKind()
			if err := WithDurableLeaseClaimLock(claim.LeaseID, func(c *LeaseClaim, _ bool, persist func() error) error {
				*c = kind.TerminalClaim(*c, time.Now().UTC())
				if scenario == "invalid-receipt" {
					c.FixedCreateIntent.Attempt = map[string]string{"unresolved": "attempt"}
				}
				return persist()
			}); err != nil {
				t.Fatal(err)
			}
			current, _ := ReadLeaseClaim(claim.LeaseID)
			claim = CloneLeaseClaim(current)
			if scenario == "stale" {
				claim.Revision = "stale"
			}
			outcome := ReleaseLeaseOutcome{}
			cleanupCalls := 0
			cleanupErr := errors.New("local artifact cleanup interrupted")
			kind.AfterTerminal = func(receipt LeaseClaim) error {
				cleanupCalls++
				durable, err := ReadLeaseClaim(receipt.LeaseID)
				if err != nil || !reflect.DeepEqual(current, durable) || !outcome.Terminal {
					t.Fatalf("artifact cleanup preceded terminal receipt: %+v %v", outcome, err)
				}
				return cleanupErr
			}
			err := DeleteFixedResource(t.Context(), kind, claim, FixedLeaseOperations[string]{
				Release: &FixedReleasePolicy{Outcome: &outcome},
				ObserveExact: func(context.Context, *FixedTransaction, FixedObserveMode) (FixedObservation[string], error) {
					if scenario == "stale" {
						t.Fatal("stale receipt reached provider")
					}
					if scenario == "observation-error" {
						return FixedObservation[string]{}, errors.New("inventory conflict")
					}
					return FixedObservation[string]{AbsenceProven: true}, nil
				},
				DeleteExact: func(context.Context, *FixedTransaction, string) error {
					t.Fatal("receipt replay deleted a resource")
					return nil
				},
			})
			if err == nil {
				t.Fatal("lost recovery failure")
			}
			if scenario == "cleanup-error" {
				if !errors.Is(err, cleanupErr) || cleanupCalls != 1 || !outcome.Terminal {
					t.Fatalf("lost terminal outcome: %+v %v", outcome, err)
				}
			} else if cleanupCalls != 0 || outcome.Terminal {
				t.Fatal("unattested receipt authorized artifact cleanup")
			}
			after, _ := ReadLeaseClaim(claim.LeaseID)
			if !reflect.DeepEqual(current, after) {
				t.Fatal("receipt replay rewrote terminal custody")
			}
		})
	}
}

func TestFixedEngineAllocatesSlugAndPreservesReplayKey(t *testing.T) {
	isolateTestUserDirs(t)
	const id = "cbx_abcdef123454"
	cfg := Config{}
	kind := fixedCleanupKind()
	var created, slug, publicKey string
	ops := FixedLeaseOperations[string]{
		Admission: &FixedAdmission{},
		DescribeIntent: func(_ context.Context, _ *LeaseClaim, exists bool) (FixedLeaseBinding, error) {
			key, err := PrepareFixedSSHKey(&cfg, id, FixedKeyPolicy{RequireExisting: exists})
			if err != nil {
				return FixedLeaseBinding{}, err
			}
			if publicKey != "" && key != publicKey {
				t.Fatal("replay replaced the SSH key")
			}
			publicKey = key
			return FixedLeaseBinding{ProviderScope: "scope", Fingerprint: "hash", AllocateSlug: true, RequestedSlug: "worker", Inventory: []Server{{Labels: map[string]string{"slug": "worker"}}}}, nil
		},
		Plan: func(_ context.Context, claim LeaseClaim) (FixedAttemptPlan, error) {
			if claim.Slug == "" || claim.Slug == "worker" {
				t.Fatal("engine did not resolve inventory slug collision")
			}
			slug = claim.Slug
			return FixedAttemptPlan{Values: map[string]string{"name": claim.Slug}}, nil
		},
		ObserveExact: func(context.Context, *FixedTransaction, FixedObserveMode) (FixedObservation[string], error) {
			if created == "" {
				return FixedObservation[string]{CanSubmit: true}, nil
			}
			return FixedObservation[string]{Candidates: []string{created}}, nil
		},
		Submit: func(context.Context, *FixedTransaction) (string, error) {
			if created != "" {
				t.Fatal("replay submitted another resource")
			}
			created = "resource"
			return created, nil
		},
		PrepareAccess: func(_ context.Context, tx *FixedTransaction, resource string) (LeaseTarget, error) {
			if tx.Claim.Slug != slug {
				t.Fatal("replay changed allocated slug")
			}
			return LeaseTarget{LeaseID: id, Server: Server{CloudID: resource}}, nil
		},
	}
	opts := FixedAcquireOptions{Kind: kind, LeaseID: id}
	for range 2 {
		if _, err := AcquireFixedResource(t.Context(), opts, ops); err != nil {
			t.Fatal(err)
		}
	}
}
