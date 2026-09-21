package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type absenceTestBackend struct {
	testDelegatedBackend
	verify   AbsenceVerifierFunc
	ordinary bool
}

func (b absenceTestBackend) VerifyResourceAbsent(ctx context.Context, claim LeaseClaim) (AbsenceEvidence, error) {
	return b.verify(ctx, claim)
}
func (b absenceTestBackend) ReconcileAbsenceOnOrdinaryStop() bool { return b.ordinary }

func TestStopAbsenceRecovery(t *testing.T) {
	for _, name := range []string{"absent", "partial inventory", "auth error", "mismatched ID", "mismatched scope", "mismatched revision", "mismatched immutable ID", "fixed", "checkpoint", "checkpoint journal", "coordinator", "adapter", "pending adapter", "ordinary opt in", "ordinary fail closed", "live", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			isolateTestUserDirs(t)
			claim := LeaseClaim{LeaseID: "cbx_123456abcdef", Provider: "absence-test", ProviderScope: "account-a", CloudID: "resource-a", CloudImmutableID: "immutable-a", Revision: "revision-a", Labels: map[string]string{"owner": "original"}}
			excluded := true
			switch name {
			case "fixed":
				claim.FixedCreateIntent = &FixedCreateIntent{}
			case "checkpoint":
				claim.CheckpointCapture = &CheckpointCaptureBinding{ID: "checkpoint-a"}
			case "checkpoint journal":
				store, err := defaultCheckpointStore()
				if err != nil {
					t.Fatal(err)
				}
				record := checkpointRecord{ID: "chk_absence_hold", Kind: checkpointKindMachine0, Provider: "machine0", LeaseID: claim.LeaseID, Capture: &NativeCheckpointCapture{SourceDisposition: "retire", Phase: "prepared"}}
				if _, _, err := store.Reserve(record); err != nil {
					t.Fatal(err)
				}
			case "coordinator":
				claim.CoordinatorRegistrationURL = "https://coordinator.example.test"
			case "adapter":
				claim.RuntimeAdapterRegistrationID = "adapter-a"
			case "pending adapter":
				claim.RuntimeAdapterPendingRegistrationID = "adapter-a"
			default:
				excluded = false
			}
			path, _ := leaseClaimPath(claim.LeaseID)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := writeLeaseClaimAtomic(path, claim); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			backend := absenceTestBackend{testDelegatedBackend: testDelegatedBackend{spec: ProviderSpec{Name: claim.Provider}}, ordinary: name == "ordinary opt in"}
			backend.verify = func(_ context.Context, observed LeaseClaim) (AbsenceEvidence, error) {
				calls++
				evidence := AbsenceEvidence{Claim: observed, ExactNotFound: true, InventoryComplete: true}
				switch name {
				case "partial inventory":
					evidence.InventoryComplete = false
				case "auth error":
					return AbsenceEvidence{}, errors.New("HTTP 403")
				case "mismatched ID":
					evidence.Claim.CloudID = "resource-b"
				case "mismatched scope":
					evidence.Claim.ProviderScope = "account-b"
				case "mismatched revision":
					evidence.Claim.Revision = "revision-b"
				case "mismatched immutable ID":
					evidence.Claim.CloudImmutableID = "immutable-b"
				case "live":
					return AbsenceEvidence{}, nil
				case "cancelled":
					cancel()
				}
				return evidence, nil
			}
			var output bytes.Buffer
			force := !strings.HasPrefix(name, "ordinary")
			handled, _, err := (App{Stderr: &output}).recoverAbsentStopClaim(ctx, backend, claim.LeaseID, force)
			wantForgotten := name == "absent" || name == "ordinary opt in"
			if handled != wantForgotten || wantForgotten && err != nil {
				t.Fatalf("handled=%t err=%v output=%s", handled, err, output.String())
			}
			if !wantForgotten && name != "ordinary fail closed" && name != "live" && err == nil {
				t.Fatal("unverified absence did not fail closed")
			}
			got, exists, readErr := ReadLeaseClaimWithPresence(claim.LeaseID)
			if readErr != nil || exists == wantForgotten || exists && !reflect.DeepEqual(got, claim) {
				t.Fatalf("claim changed: exists=%t err=%v got=%+v", exists, readErr, got)
			}
			if (excluded || name == "ordinary fail closed") && calls != 0 {
				t.Fatal("ineligible claim reached provider")
			}
			if strings.Contains(output.String(), "released") || strings.Contains(output.String(), "forgotten locally (resource absent)") != wantForgotten {
				t.Fatalf("incorrect outcome: %s", output.String())
			}
		})
	}
}

func TestAbsenceRecoveryClaimFence(t *testing.T) {
	isolateTestUserDirs(t)
	const id = "cbx_123456abcdef"
	if err := ClaimLeaseForRepoProviderScopePondEndpoint(id, "fixture", "absence-test", "scope", "", t.TempDir(), time.Minute, false, Server{CloudID: "resource"}, SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	claim, _ := ReadLeaseClaim(id)
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := ForgetAbsentLeaseClaim(t.Context(), AbsenceVerifierFunc(func(_ context.Context, observed LeaseClaim) (AbsenceEvidence, error) {
			close(entered)
			<-release
			return AbsenceEvidence{Claim: observed, ExactNotFound: true, InventoryComplete: true}, nil
		}), claim)
		done <- err
	}()
	<-entered
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	err := WithDurableLeaseClaimLockContext(ctx, id, func(*LeaseClaim, bool, func() error) error {
		t.Error("claim lock escaped verification")
		return nil
	})
	close(release)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("concurrent writer was not fenced: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := ForgetAbsentLeaseClaim(t.Context(), AbsenceVerifierFunc(func(context.Context, LeaseClaim) (AbsenceEvidence, error) {
		t.Fatal("stale claim reached verifier")
		return AbsenceEvidence{}, nil
	}), claim); err == nil {
		t.Fatal("stale claim accepted")
	}
}
