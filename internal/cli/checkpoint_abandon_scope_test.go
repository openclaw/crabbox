package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRestoreCheckpointAbandonSourceScope(t *testing.T) {
	const account, region = "111111111111", "us-west-2"
	const receiptError = "cleanup scope no longer matches its exact source receipt"
	for _, tc := range []struct {
		name         string
		mutate       func(*checkpointRecord, *leaseClaim)
		missingClaim bool
		wantError    string
	}{
		{name: "matching terminal receipt restores missing scope"},
		{name: "ordinary removed claim preserves attested scope", missingClaim: true, mutate: func(r *checkpointRecord, c *leaseClaim) {
			r.Capture.SourceIntent = ""
			c.FixedCreateIntent = nil
			r.Native.AccountID, r.Native.Region = account, region
		}},
		{name: "matching existing scope is idempotent", mutate: func(r *checkpointRecord, _ *leaseClaim) {
			r.Native.AccountID, r.Native.Region = account, region
		}},

		{name: "claim scope changed", wantError: receiptError, mutate: func(_ *checkpointRecord, c *leaseClaim) {
			c.ProviderScope = "account:222222222222"
		}},

		{name: "fixed intent changed", wantError: receiptError, mutate: func(_ *checkpointRecord, c *leaseClaim) {
			c.FixedCreateIntent.Fingerprint = "replacement-intent"
		}},
		{name: "missing fixed intent receipt", wantError: receiptError, mutate: func(_ *checkpointRecord, c *leaseClaim) {
			c.FixedCreateIntent = nil
		}},
		{name: "missing terminal claim", missingClaim: true, wantError: receiptError},
		{name: "source changed", wantError: receiptError, mutate: func(_ *checkpointRecord, c *leaseClaim) {
			c.CloudID = "i-replacement"
		}},
		{name: "owner changed", wantError: receiptError, mutate: func(_ *checkpointRecord, c *leaseClaim) {
			c.RepoRoot = filepath.Join(c.RepoRoot, "other-repo")
		}},
		{name: "claim generation changed", wantError: receiptError, mutate: func(_ *checkpointRecord, c *leaseClaim) {
			c.ClaimedAt = "2026-01-02T00:00:00Z"
		}},
		{name: "provider changed", wantError: receiptError, mutate: func(_ *checkpointRecord, c *leaseClaim) {
			c.Provider = "machine0"
		}},
		{name: "missing capture receipt", wantError: receiptError, mutate: func(r *checkpointRecord, _ *leaseClaim) {
			r.Capture = nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			repo := Repo{Root: t.TempDir()}
			claim := leaseClaim{
				LeaseID: "cbx_abcdef123456", Revision: "5", Provider: FixedAWSClaimProvider,
				CloudID: "i-fixture", ProviderScope: "account:" + account, RepoRoot: repo.Root,
				ClaimedAt:         "2026-01-01T00:00:00Z",
				Labels:            map[string]string{"aws_account_id": account, "aws_region": region},
				FixedCreateIntent: &FixedCreateIntent{Version: 1, Fingerprint: "original-intent", State: "released"},
			}
			record := checkpointRecord{
				ID: "chk_abandon_scope", Kind: checkpointKindAWSAMI, Provider: "aws", LeaseID: claim.LeaseID,
				Capture: &NativeCheckpointCapture{
					SourceDisposition: "abandon", Phase: "abandoned", SourceID: claim.CloudID,
					SourceScope: claim.ProviderScope, SourceClaimedAt: claim.ClaimedAt,
					SourceRevision: "3", SourceIntent: claim.FixedCreateIntent.Fingerprint,
					Error: "source disposed; original image submission remains unresolved; retain this checkpoint record",
				},
			}
			record.Repo.Root = repo.Root
			record.Native.Metadata = map[string]string{checkpointPreSubmitEvidenceMetadata: strings.Repeat("a", 64)}
			if tc.mutate != nil {
				tc.mutate(&record, &claim)
			}
			claimPath, err := leaseClaimPath(claim.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			var claimBefore []byte
			if !tc.missingClaim {
				if err := os.MkdirAll(filepath.Dir(claimPath), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := writeLeaseClaimAtomic(claimPath, claim); err != nil {
					t.Fatal(err)
				}
				claimBefore, err = os.ReadFile(claimPath)
				if err != nil {
					t.Fatal(err)
				}
			}
			store, err := defaultCheckpointStore()
			if err != nil {
				t.Fatal(err)
			}
			record, paths, err := store.Reserve(record)
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(paths.Meta)
			if err != nil {
				t.Fatal(err)
			}
			// Decode an independent expected value before invoking restoration:
			// shared capture pointers or metadata maps must not hide mutations.
			var want checkpointRecord
			if err := json.Unmarshal(before, &want); err != nil {
				t.Fatal(err)
			}
			want.artifactDir = paths.Dir
			want.Native.AccountID, want.Native.Region = account, region
			err = store.WithLock(record.ID, func() error {
				return (App{}).restoreCheckpointAbandonSourceScope(store, &record, repo, fixtureAbandonScope{})
			})
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("want rejection %q, got %v", tc.wantError, err)
				}
				after, readErr := os.ReadFile(paths.Meta)
				if readErr != nil || !bytes.Equal(before, after) {
					t.Fatalf("rejected restoration changed durable journal: %v", readErr)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				// Only source account/region may change; preserve all image
				// identity fields, evidence and the unresolved obligation.
				got, _, err := store.Read(record.ID)
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("unexpected restored journal: got=%+v want=%+v err=%v", got, want, err)
				}
				if got.Native.ImageID != "" || got.Native.Resource != "" || got.nativeDeleteID() != "" || !unresolvedCheckpoint(got) {
					t.Fatal("scope restoration resolved the original unknown image obligation")
				}
			}
			claimAfter, err := os.ReadFile(claimPath)
			if tc.missingClaim {
				if !os.IsNotExist(err) {
					t.Fatalf("restoration invented a missing terminal claim: %v", err)
				}
			} else if err != nil || !bytes.Equal(claimBefore, claimAfter) {
				t.Fatalf("restoration changed terminal claim: %v", err)
			}
		})
	}
}

// Scope interpretation is adapter-owned; these cases test only shared journal
// preservation and receipt fencing. AWS policy is covered in its package.
type fixtureAbandonScope struct{}

func (fixtureAbandonScope) NativeCheckpointAbandonScope(_ *LeaseClaim, _ NativeCheckpointCapture, scope NativeCheckpointImage) (NativeCheckpointImage, error) {
	scope.AccountID, scope.Region = "111111111111", "us-west-2"
	return scope, nil
}
