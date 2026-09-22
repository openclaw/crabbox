package cli

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFixedEngineKeyedRetryAdmission(t *testing.T) {
	for _, test := range []struct {
		name    string
		elapsed time.Duration
		tamper  string
		calls   int
	}{
		{"one-retry", time.Hour, "", 2}, {"boundary", 24 * time.Hour, "", 1}, {"past-window", 25 * time.Hour, "", 1},
		{"clock-backwards", -time.Second, "", 1}, {"changed-key", time.Hour, "key", 1}, {"missing-journal", time.Hour, "journal", 1}, {"bound-before-error", time.Hour, "bound", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			isolateTestUserDirs(t)
			clock := time.Now().UTC()
			kind := FixedLeaseKind{ClaimProvider: "fixture-keyed", IntentVersion: 1, Label: "fixture"}
			opts := FixedAcquireOptions{Kind: kind, LeaseID: "cbx_abcdef123410", Now: func() time.Time { return clock }}
			calls := 0
			lost := errors.New("lost reply")
			ops := FixedLeaseOperations[string]{Admission: &FixedAdmission{FreshOnly: true, KeyedRetry: &FixedKeyedRetry{AttemptKey: "key", Window: 24 * time.Hour}},
				DescribeIntent: func(context.Context, *LeaseClaim, bool) (FixedLeaseBinding, error) {
					return FixedLeaseBinding{ProviderScope: "scope", Fingerprint: "fingerprint", Slug: "fixture"}, nil
				},
				Plan: func(context.Context, LeaseClaim) (FixedAttemptPlan, error) {
					return FixedAttemptPlan{Values: map[string]string{"key": "stable-key"}}, nil
				},
				ObserveExact: func(context.Context, *FixedTransaction, FixedObserveMode) (FixedObservation[string], error) {
					return FixedObservation[string]{CanSubmit: true}, nil
				},
				Submit: func(ctx context.Context, tx *FixedTransaction) (string, error) {
					calls++
					saved, err := ReadLeaseClaim(opts.LeaseID)
					if err != nil || saved.FixedCreateIntent.Journal.Submission.Count != calls {
						t.Fatalf("unjournaled submission %d: %v", calls, err)
					}
					if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 24*time.Hour {
						t.Fatal("native call outlives key window")
					}
					return "", lost
				},
				PrepareAccess: func(context.Context, *FixedTransaction, string) (LeaseTarget, error) {
					t.Fatal("prepared access after lost reply")
					return LeaseTarget{}, nil
				},
			}
			if _, err := AcquireFixedResource(t.Context(), opts, ops); !errors.Is(err, lost) {
				t.Fatal(err)
			}
			if test.tamper != "" {
				err := WithDurableLeaseClaimLock(opts.LeaseID, func(c *LeaseClaim, _ bool, persist func() error) error {
					switch test.tamper {
					case "key":
						c.FixedCreateIntent.Attempt["key"] = "different"
					case "journal":
						c.FixedCreateIntent.Journal.Submission = nil
					case "bound":
						c.CloudID = "native-id"
					}
					return persist()
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			clock = clock.Add(test.elapsed)
			for i := 0; i < 2; i++ {
				if _, err := AcquireFixedResource(t.Context(), opts, ops); err == nil {
					t.Fatal("uncertain attempt succeeded")
				}
			}
			if calls != test.calls {
				t.Fatalf("submissions=%d want=%d", calls, test.calls)
			}
			before, _ := ReadLeaseClaim(opts.LeaseID)
			if _, err := AcquireFixedResource(t.Context(), opts, ops); err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
				t.Fatalf("third submission admitted: %v", err)
			}
			after, _ := ReadLeaseClaim(opts.LeaseID)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("rejected replay changed durable state")
			}
		})
	}
}

func TestFixedEngineResolveRejectsDeletion(t *testing.T) {
	isolateTestUserDirs(t)
	kind := FixedLeaseKind{ClaimProvider: "fixture-keyed", IntentVersion: 1, Label: "fixture", DeletionState: "deleting"}
	claim := LeaseClaim{LeaseID: "cbx_abcdef123411", Provider: kind.ClaimProvider, FixedCreateIntent: &FixedCreateIntent{Version: 1, State: "deleting", Journal: &FixedLeaseJournal{Version: 1, Phase: "deleting", Revision: 1}}}
	if err := WithDurableLeaseClaimLock(claim.LeaseID, func(c *LeaseClaim, _ bool, persist func() error) error { *c = claim; return persist() }); err != nil {
		t.Fatal(err)
	}
	claim, _ = ReadLeaseClaim(claim.LeaseID)
	_, err := ResolveFixedLeaseTarget(t.Context(), FixedResolveOptions{Kind: kind, Expected: claim}, func(context.Context, *LeaseClaim, func() error) (LeaseTarget, error) {
		t.Fatal("deleting resource reused")
		return LeaseTarget{}, nil
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Fatalf("deleting claim accepted: %v", err)
	}
}
