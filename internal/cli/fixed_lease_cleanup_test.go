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

func TestFixedCleanupPersistsRecoveryBindingBeforeDeletion(t *testing.T) {
	for _, source := range []string{"observation", "observation-no-policy", "release"} {
		t.Run(source, func(t *testing.T) {
			isolateTestUserDirs(t)
			claim := fixedCleanupClaim(t, "acquired")
			kind := fixedCleanupKind()
			kind.DeletionState = ""
			binding := &FixedResourceBinding{Labels: map[string]string{"cleanup_child": "original-child"}}
			policy := &FixedReleasePolicy{}
			if source == "release" {
				policy.Binding = binding
			} else if source == "observation-no-policy" {
				policy = nil
			}
			interrupted := errors.New("parent deleted; companion cleanup interrupted")
			ops := FixedLeaseOperations[string]{
				Release: policy,
				ObserveExact: func(context.Context, *FixedTransaction, FixedObserveMode) (FixedObservation[string], error) {
					observed := FixedObservation[string]{Candidates: []string{"resource"}}
					if source != "release" {
						observed.Binding = binding
					}
					return observed, nil
				},
				DeleteExact: func(context.Context, *FixedTransaction, string) error {
					durable, err := ReadLeaseClaim(claim.LeaseID)
					if err != nil || durable.Labels["cleanup_child"] != "original-child" {
						t.Errorf("cleanup binding was not durable before deletion: %v", err)
					}
					return interrupted
				},
			}
			if err := DeleteFixedResource(t.Context(), kind, claim, ops); !errors.Is(err, interrupted) {
				t.Fatal(err)
			}
			retained, err := ReadLeaseClaim(claim.LeaseID)
			if err != nil || retained.Labels["cleanup_child"] != "original-child" || retained.FixedCreateIntent.Journal == nil || retained.FixedCreateIntent.Journal.Phase != "deleting" {
				t.Fatalf("failed cleanup lost recovery evidence: %v", err)
			}
			if retained.CloudID != claim.CloudID || retained.CloudImmutableID != claim.CloudImmutableID || retained.FixedCreateIntent.State != claim.FixedCreateIntent.State {
				t.Fatal("cleanup binding changed resource identity or legacy state")
			}
			// Once the parent is gone, retry must use the original durable binding.
			ops.ObserveExact = func(_ context.Context, tx *FixedTransaction, _ FixedObserveMode) (FixedObservation[string], error) {
				return FixedObservation[string]{Candidates: []string{tx.Claim.Labels["cleanup_child"]}}, nil
			}
			if ops.Release != nil {
				ops.Release.Binding = nil
			}
			ops.DeleteExact = func(_ context.Context, _ *FixedTransaction, child string) error {
				if child != "original-child" {
					t.Fatalf("cleanup retry retargeted companion: %q", child)
				}
				return nil
			}
			if err := DeleteFixedResource(t.Context(), kind, retained, ops); err != nil {
				t.Fatal(err)
			}
			terminal, err := ReadLeaseClaim(claim.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			if err := kind.ValidateTerminalClaim(terminal, retained, claim.LeaseID, nil); err != nil {
				t.Fatalf("retry did not finalize its receipt: %v", err)
			}
		})
	}
}

func TestFixedCleanupWithoutDeletionStateRetainsApplicableEvidence(t *testing.T) {
	for _, state := range []string{"prepared", "acquired"} {
		for _, bindingSource := range []string{"none", "observation", "release", "only-unbound"} {
			t.Run(state+"/"+bindingSource, func(t *testing.T) {
				isolateTestUserDirs(t)
				claim := fixedCleanupClaim(t, state)
				if err := WithDurableLeaseClaimLock(claim.LeaseID, func(c *LeaseClaim, _ bool, persist func() error) error {
					c.ImageEvidence = &ImageEvidence{ConfiguredReference: "image:v1", RuntimeImageID: "image-1", RepositoryDigests: []string{"image@sha256:fixture"}}
					c.CheckpointCapture = &CheckpointCaptureBinding{ID: "capture", Revision: "capture-revision"}
					c.FixedCreateIntent.Journal = &FixedLeaseJournal{Version: 1, Phase: state, Revision: 3}
					return persist()
				}); err != nil {
					t.Fatal(err)
				}
				var err error
				claim, err = ReadLeaseClaim(claim.LeaseID)
				if err != nil {
					t.Fatal(err)
				}
				kind := fixedCleanupKind()
				kind.DeletionState = ""
				failure := errors.New("native cleanup uncertain")
				started := false
				outcome := ReleaseLeaseOutcome{}
				policy := &FixedReleasePolicy{Started: &started, Outcome: &outcome}
				binding := &FixedResourceBinding{CloudID: claim.CloudID, Labels: map[string]string{"observed": "ready"}}
				if bindingSource == "release" {
					policy.Binding = binding
				}
				binding.OnlyUnbound = bindingSource == "only-unbound"
				assertEvidence := func(durable LeaseClaim) {
					t.Helper()
					want := CloneLeaseClaim(claim)
					if bindingSource == "observation" || bindingSource == "release" {
						want.Labels = binding.Labels
						want.FixedCreateIntent.Journal.Phase = "deleting"
						want.FixedCreateIntent.Journal.Revision++
						want.Revision = durable.Revision
					}
					if !reflect.DeepEqual(want, durable) {
						t.Fatalf("cleanup lost custody or binding evidence:\nwant=%+v\ngot=%+v", want, durable)
					}
				}
				ops := FixedLeaseOperations[string]{
					Release: policy,
					ObserveExact: func(context.Context, *FixedTransaction, FixedObserveMode) (FixedObservation[string], error) {
						observed := FixedObservation[string]{Candidates: []string{"resource"}}
						if bindingSource == "observation" || bindingSource == "only-unbound" {
							observed.Binding = binding
						}
						return observed, nil
					},
					DeleteExact: func(context.Context, *FixedTransaction, string) error {
						durable, err := ReadLeaseClaim(claim.LeaseID)
						if err != nil || !started {
							t.Fatalf("deletion preceded admission: started=%v err=%v", started, err)
						}
						assertEvidence(durable)
						return failure
					},
				}
				if err := DeleteFixedResource(t.Context(), kind, claim, ops); !errors.Is(err, failure) || !started || outcome.Terminal {
					t.Fatalf("failed cleanup: started=%v terminal=%v err=%v", started, outcome.Terminal, err)
				}
				after, err := ReadLeaseClaim(claim.LeaseID)
				if err != nil {
					t.Fatal(err)
				}
				assertEvidence(after)
				ops.DeleteExact = func(context.Context, *FixedTransaction, string) error { return nil }
				if err := DeleteFixedResource(t.Context(), kind, after, ops); err != nil || !outcome.Terminal {
					t.Fatalf("cleanup retry: terminal=%v err=%v", outcome.Terminal, err)
				}
				terminal, err := ReadLeaseClaim(claim.LeaseID)
				if err != nil {
					t.Fatal(err)
				}
				if err := kind.ValidateTerminalClaim(terminal, claim, claim.LeaseID, nil); err != nil {
					t.Fatalf("successful retry did not publish a valid receipt: %v", err)
				}
			})
		}
	}
}

func TestFixedCleanupExistingJournalAdmissionFencesReplay(t *testing.T) {
	isolateTestUserDirs(t)
	claim := fixedCleanupClaim(t, "acquired")
	kind := fixedCleanupKind()
	kind.DeletionState = ""
	// Previously persisted admission remains authoritative on retry, even
	// when this dialect no longer writes a deletion marker before completion.
	if err := WithDurableLeaseClaimLock(claim.LeaseID, func(c *LeaseClaim, _ bool, persist func() error) error {
		c.FixedCreateIntent.Journal = &FixedLeaseJournal{Version: 1, Phase: "deleting", Revision: 1}
		return persist()
	}); err != nil {
		t.Fatal(err)
	}
	var err error
	claim, err = ReadLeaseClaim(claim.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("native deletion interrupted")
	admitted := false
	ops := FixedLeaseOperations[string]{
		Admission: &FixedAdmission{FreshOnly: true},
		DescribeIntent: func(context.Context, *LeaseClaim, bool) (FixedLeaseBinding, error) {
			return FixedLeaseBinding{ProviderScope: "scope", Fingerprint: "hash", Slug: "fixture"}, nil
		},
		Plan: func(context.Context, LeaseClaim) (FixedAttemptPlan, error) {
			t.Fatal("planned another attempt")
			return FixedAttemptPlan{}, nil
		},
		ObserveExact: func(context.Context, *FixedTransaction, FixedObserveMode) (FixedObservation[string], error) {
			return FixedObservation[string]{Candidates: []string{"resource"}}, nil
		},
		Submit: func(context.Context, *FixedTransaction) (string, error) {
			t.Fatal("submitted another resource")
			return "", nil
		},
		PrepareAccess: func(context.Context, *FixedTransaction, string) (LeaseTarget, error) {
			return LeaseTarget{LeaseID: claim.LeaseID, Server: Server{CloudID: "resource", ImmutableID: "generation"}}, nil
		},
		DeleteExact: func(context.Context, *FixedTransaction, string) error {
			durable, err := ReadLeaseClaim(claim.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			admitted = durable.FixedCreateIntent.Journal != nil && durable.FixedCreateIntent.Journal.Phase == "deleting"
			return failure
		},
	}
	if err := DeleteFixedResource(t.Context(), kind, claim, ops); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if !admitted {
		t.Error("native deletion preceded durable admission")
	}
	deleting, err := ReadLeaseClaim(claim.LeaseID)
	if err != nil || !reflect.DeepEqual(claim, deleting) {
		t.Fatalf("failed cleanup changed durable admission: %v", err)
	}
	if _, err := AcquireFixedResource(t.Context(), FixedAcquireOptions{Kind: kind, LeaseID: claim.LeaseID, RepoRoot: claim.RepoRoot}, ops); err == nil {
		t.Error("replayed a resource after interrupted deletion")
	}
	// A stale retry must still report that cleanup was previously admitted.
	if err := WithDurableLeaseClaimLock(claim.LeaseID, func(c *LeaseClaim, _ bool, persist func() error) error {
		c.LastUsedAt = "2026-09-21T02:00:00Z"
		return persist()
	}); err != nil {
		t.Fatal(err)
	}
	started := false
	ops.Release = &FixedReleasePolicy{Started: &started}
	if err := DeleteFixedResource(t.Context(), kind, deleting, ops); err == nil || !started {
		t.Errorf("stale admitted cleanup: started=%v err=%v", started, err)
	}
	deleting, err = ReadLeaseClaim(claim.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	ops.DeleteExact = func(context.Context, *FixedTransaction, string) error { return nil }
	if err := DeleteFixedResource(t.Context(), kind, deleting, ops); err != nil {
		t.Fatal(err)
	}
	terminal, err := ReadLeaseClaim(claim.LeaseID)
	if err != nil || terminal.FixedCreateIntent.State != "released" {
		t.Fatalf("cleanup retry lost terminal receipt: %v", err)
	}
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
