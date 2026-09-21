package cli

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestFixedCleanupAdmissionPreservesChangedClaim(t *testing.T) {
	for _, state := range []string{"acquired", "deleting"} {
		for _, change := range []string{"revision", "renewal", "owner"} {
			t.Run(state+"/"+change, func(t *testing.T) {
				isolateTestUserDirs(t)
				expected := fixedCleanupClaim(t, state)
				err := WithDurableLeaseClaimLock(expected.LeaseID, func(claim *LeaseClaim, _ bool, persist func() error) error {
					switch change {
					case "renewal":
						claim.LastUsedAt = "2026-09-21T01:00:00Z"
						claim.Labels = map[string]string{"state": "ready", "reservation": "active"}
					case "owner":
						claim.RepoRoot = "/new-owner"
					}
					return persist()
				})
				if err != nil {
					t.Fatal(err)
				}
				current, err := ReadLeaseClaim(expected.LeaseID)
				if err != nil {
					t.Fatal(err)
				}
				var started bool
				outcome := ReleaseLeaseOutcome{}
				err = DeleteFixedResource(t.Context(), fixedCleanupKind(), expected, FixedLeaseOperations[string]{
					Release: &FixedReleasePolicy{Started: &started, Outcome: &outcome},
					ObserveExact: func(context.Context, *FixedTransaction, FixedObserveMode) (FixedObservation[string], error) {
						t.Fatal("stale cleanup crossed the ownership fence")
						return FixedObservation[string]{}, nil
					},
					DeleteExact: func(context.Context, *FixedTransaction, string) error { t.Fatal("changed lease deleted"); return nil },
				})
				if err == nil || started != (state == "deleting") || outcome.Terminal {
					t.Fatalf("started=%v terminal=%v error=%v", started, outcome.Terminal, err)
				}
				after, err := ReadLeaseClaim(expected.LeaseID)
				if err != nil || !reflect.DeepEqual(current, after) {
					t.Fatalf("cleanup changed fresh ownership: %v", err)
				}
			})
		}
	}
}

func TestFixedCleanupAdmissionSeparatesObservationAndDeletionFailure(t *testing.T) {
	for _, failObservation := range []bool{false, true} {
		t.Run(map[bool]string{true: "observation", false: "deletion"}[failObservation], func(t *testing.T) {
			isolateTestUserDirs(t)
			expected := fixedCleanupClaim(t, "acquired")
			failure := errors.New("native failure")
			var started bool
			err := DeleteFixedResource(t.Context(), fixedCleanupKind(), expected, FixedLeaseOperations[string]{
				Release: &FixedReleasePolicy{Started: &started},
				ObserveExact: func(context.Context, *FixedTransaction, FixedObserveMode) (FixedObservation[string], error) {
					if failObservation {
						return FixedObservation[string]{}, failure
					}
					return FixedObservation[string]{Candidates: []string{"resource"}}, nil
				},
				DeleteExact: func(context.Context, *FixedTransaction, string) error {
					durable, err := ReadLeaseClaim(expected.LeaseID)
					if err != nil || durable.FixedCreateIntent.State != "deleting" || !started {
						t.Fatalf("native effect preceded admission: %v", err)
					}
					return failure
				},
			})
			if !errors.Is(err, failure) || started == failObservation {
				t.Fatalf("started=%v error=%v", started, err)
			}
			after, err := ReadLeaseClaim(expected.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			if failObservation && !reflect.DeepEqual(expected, after) {
				t.Fatal("failed observation changed claim")
			}
			if !failObservation && after.FixedCreateIntent.State != "deleting" {
				t.Fatal("failed deletion lost admission")
			}
		})
	}
}

func fixedCleanupKind() FixedLeaseKind {
	return FixedLeaseKind{ClaimProvider: "fixture-fixed", IntentVersion: 1, Label: "fixture", DeletionState: "deleting"}
}

func fixedCleanupClaim(t *testing.T, state string) LeaseClaim {
	t.Helper()
	const id = "cbx_abcdef123453"
	err := WithDurableLeaseClaimLock(id, func(claim *LeaseClaim, _ bool, persist func() error) error {
		*claim = LeaseClaim{LeaseID: id, Slug: "fixture", Provider: fixedCleanupKind().ClaimProvider, ProviderScope: "scope", RepoRoot: "/owner", CloudID: "resource", CloudImmutableID: "generation",
			FixedCreateIntent: &FixedCreateIntent{Version: 1, Fingerprint: "hash", Slug: "fixture", ProviderScope: "scope", State: state, CreatedAt: "2026-09-21T00:00:00Z", Attempt: map[string]string{"name": "resource"}}}
		return persist()
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := ReadLeaseClaim(id)
	if err != nil {
		t.Fatal(err)
	}
	return claim
}
