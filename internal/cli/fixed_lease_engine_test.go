package cli

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestFixedEngineReadsLegacyProviderRecords(t *testing.T) {
	for _, provider := range []string{"parallels", "tenki", "proxmox", "daytona"} {
		t.Run(provider, func(t *testing.T) {
			isolateTestUserDirs(t)
			data, err := os.ReadFile(filepath.Join("testdata", "fixed-lease-v1", provider+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var legacy LeaseClaim
			if err := json.Unmarshal(data, &legacy); err != nil {
				t.Fatal(err)
			}
			path, err := leaseClaimPath(legacy.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			loaded, err := ReadLeaseClaim(legacy.LeaseID)
			if err != nil || !reflect.DeepEqual(loaded, legacy) || loaded.FixedCreateIntent.Journal != nil {
				t.Fatalf("legacy read: %v", err)
			}
			kind := FixedLeaseKind{ClaimProvider: legacy.Provider, IntentVersion: 1, Label: provider}
			ops := FixedLeaseOperations[string]{Admission: &FixedAdmission{FreshOnly: true},
				DescribeIntent: func(context.Context, *LeaseClaim, bool) (FixedLeaseBinding, error) {
					return FixedLeaseBinding{ProviderScope: legacy.FixedCreateIntent.ProviderScope, Fingerprint: legacy.FixedCreateIntent.Fingerprint, Slug: legacy.Slug}, nil
				},
				Plan: func(context.Context, LeaseClaim) (FixedAttemptPlan, error) {
					t.Fatal("legacy acquired claim planned another attempt")
					return FixedAttemptPlan{}, nil
				},
				ObserveExact: func(context.Context, *FixedTransaction, FixedObserveMode) (FixedObservation[string], error) {
					return FixedObservation[string]{Candidates: []string{legacy.CloudID}}, nil
				},
				Submit: func(context.Context, *FixedTransaction) (string, error) {
					t.Fatal("legacy acquired claim resubmitted")
					return "", nil
				},
				PrepareAccess: func(_ context.Context, tx *FixedTransaction, id string) (LeaseTarget, error) {
					return LeaseTarget{LeaseID: legacy.LeaseID, Server: Server{CloudID: id, ImmutableID: legacy.CloudImmutableID, Labels: maps.Clone(legacy.Labels)}, SSH: SSHTarget{Host: legacy.SSHHost, Port: strconv.Itoa(legacy.SSHPort)}}, nil
				},
			}
			_, err = AcquireFixedResource(t.Context(), FixedAcquireOptions{Kind: kind, LeaseID: legacy.LeaseID, RepoRoot: legacy.RepoRoot}, ops)
			if err != nil {
				t.Fatal(err)
			}
			current, err := ReadLeaseClaim(legacy.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			if current.FixedCreateIntent.Journal == nil || current.FixedCreateIntent.Journal.Phase != "acquired" {
				t.Fatal("new write has no acquired journal")
			}
			if !maps.Equal(current.FixedCreateIntent.Attempt, legacy.FixedCreateIntent.Attempt) || current.FixedCreateIntent.Fingerprint != legacy.FixedCreateIntent.Fingerprint || current.CloudID != legacy.CloudID || current.CloudImmutableID != legacy.CloudImmutableID {
				t.Fatal("migration changed native custody")
			}
			current.FixedCreateIntent.Journal = nil
			current.LastUsedAt = legacy.LastUsedAt
			current.Revision = legacy.Revision
			if !reflect.DeepEqual(current, legacy) {
				t.Fatalf("migration changed legacy envelope:\n%+v\n%+v", current, legacy)
			}
		})
	}
}

func TestFixedEngineRetainsUncertainAttemptAndRejectsDuplicates(t *testing.T) {
	isolateTestUserDirs(t)
	kind := FixedLeaseKind{ClaimProvider: "fixture-fixed", IntentVersion: 1, Label: "fixture", DeletionState: "deleting"}
	opts := FixedAcquireOptions{Kind: kind, LeaseID: "cbx_abcdef123405", RepoRoot: "/fixture"}
	lost := errors.New("lost create reply")
	calls := 0
	observed := []string(nil)
	ops := FixedLeaseOperations[string]{Admission: &FixedAdmission{FreshOnly: true},
		DescribeIntent: func(context.Context, *LeaseClaim, bool) (FixedLeaseBinding, error) {
			return FixedLeaseBinding{ProviderScope: "scope", Fingerprint: "hash", Slug: "fixture"}, nil
		},
		Plan: func(context.Context, LeaseClaim) (FixedAttemptPlan, error) {
			return FixedAttemptPlan{Values: map[string]string{"nonce": "original"}}, nil
		},
		ObserveExact: func(_ context.Context, tx *FixedTransaction, _ FixedObserveMode) (FixedObservation[string], error) {
			return FixedObservation[string]{Candidates: observed, CanSubmit: true}, nil
		},
		Submit: func(_ context.Context, tx *FixedTransaction) (string, error) {
			saved, err := ReadLeaseClaim(opts.LeaseID)
			if err != nil || saved.FixedCreateIntent.Attempt["nonce"] != "original" || saved.FixedCreateIntent.Journal.Phase != "submitting" {
				t.Fatal("mutation preceded durable admission")
			}
			calls++
			return "", lost
		},
		Identity: func(id string) FixedResourceBinding { return FixedResourceBinding{CloudID: id, ImmutableID: id} },
		PrepareAccess: func(_ context.Context, tx *FixedTransaction, id string) (LeaseTarget, error) {
			return LeaseTarget{LeaseID: opts.LeaseID, Server: Server{CloudID: id, ImmutableID: id}}, nil
		},
	}
	if _, err := AcquireFixedResource(t.Context(), opts, ops); !errors.Is(err, lost) {
		t.Fatal(err)
	}
	before, _ := ReadLeaseClaim(opts.LeaseID)
	if _, err := AcquireFixedResource(t.Context(), opts, ops); err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Fatal(err)
	}
	after, _ := ReadLeaseClaim(opts.LeaseID)
	if !reflect.DeepEqual(before, after) || calls != 1 {
		t.Fatal("uncertain replay changed custody or resubmitted")
	}
	observed = []string{"one", "two"}
	if _, err := AcquireFixedResource(t.Context(), opts, ops); err == nil {
		t.Fatal("multiple resources adopted")
	}
	observed = []string{"one"}
	if _, err := AcquireFixedResource(t.Context(), opts, ops); err != nil {
		t.Fatal(err)
	}
	acquired, _ := ReadLeaseClaim(opts.LeaseID)
	ops.DeleteExact = func(context.Context, *FixedTransaction, string) error { return lost }
	if err := DeleteFixedResource(t.Context(), kind, acquired, ops); !errors.Is(err, lost) {
		t.Fatal(err)
	}
	deleting, _ := ReadLeaseClaim(opts.LeaseID)
	if deleting.FixedCreateIntent.Journal.Phase != "deleting" || deleting.FixedCreateIntent.Attempt["nonce"] != "original" {
		t.Fatal("failed deletion lost custody")
	}
	if _, err := AcquireFixedResource(t.Context(), opts, ops); err == nil {
		t.Fatal("deleting lease replayed")
	}
	ops.DeleteExact = func(context.Context, *FixedTransaction, string) error { return nil }
	if err := DeleteFixedResource(t.Context(), kind, deleting, ops); err != nil {
		t.Fatal(err)
	}
	terminal, _ := ReadLeaseClaim(opts.LeaseID)
	if terminal.FixedCreateIntent.State != "released" || terminal.FixedCreateIntent.Journal.Phase != "released" || len(terminal.FixedCreateIntent.Attempt) != 0 {
		t.Fatal("missing terminal receipt")
	}
	if _, err := AcquireFixedResource(t.Context(), opts, ops); err == nil {
		t.Fatal("terminal ID reopened")
	}
}

func TestFixedEngineNeverResubmitsBoundClaim(t *testing.T) {
	isolateTestUserDirs(t)
	kind := FixedLeaseKind{ClaimProvider: "fixture-fixed", IntentVersion: 1, Label: "fixture"}
	const id = "cbx_abcdef123409"
	err := WithDurableLeaseClaimLock(id, func(c *LeaseClaim, _ bool, persist func() error) error {
		*c = LeaseClaim{LeaseID: id, Provider: kind.ClaimProvider, ProviderScope: "scope", Slug: "fixture", CloudID: "bound", CloudImmutableID: "generation", FixedCreateIntent: &FixedCreateIntent{Version: 1, Fingerprint: "hash", ProviderScope: "scope", Slug: "fixture", CreatedAt: "2026-09-20T00:00:00Z", State: "prepared"}}
		return persist()
	})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := ReadLeaseClaim(id)
	ops := FixedLeaseOperations[string]{Admission: &FixedAdmission{FreshOnly: true},
		DescribeIntent: func(context.Context, *LeaseClaim, bool) (FixedLeaseBinding, error) {
			return FixedLeaseBinding{ProviderScope: "scope", Fingerprint: "hash", Slug: "fixture"}, nil
		},
		// Even an adapter reporting eligible absence cannot replace a bound ID.
		ObserveExact: func(context.Context, *FixedTransaction, FixedObserveMode) (FixedObservation[string], error) {
			return FixedObservation[string]{CanSubmit: true}, nil
		},
		Plan: func(context.Context, LeaseClaim) (FixedAttemptPlan, error) {
			t.Fatal("planned a replacement for bound identity")
			return FixedAttemptPlan{}, nil
		},
		Submit: func(context.Context, *FixedTransaction) (string, error) {
			t.Fatal("resubmitted bound identity")
			return "", nil
		},
		PrepareAccess: func(context.Context, *FixedTransaction, string) (LeaseTarget, error) {
			t.Fatal("prepared access without observation")
			return LeaseTarget{}, nil
		},
	}
	if _, err := AcquireFixedResource(t.Context(), FixedAcquireOptions{Kind: kind, LeaseID: id}, ops); err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Fatal(err)
	}
	after, _ := ReadLeaseClaim(id)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("rejected replacement changed custody")
	}
}
