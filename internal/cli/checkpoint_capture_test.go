package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCheckpointForkRejectsDiscardedCapture(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	store, err := defaultCheckpointStore()
	if err != nil {
		t.Fatal(err)
	}
	record := checkpointRecord{ID: "chk_discarded", Kind: checkpointKindMachine0, Provider: "machine0", Capture: &NativeCheckpointCapture{SourceDisposition: "retire", Phase: "retired", DiscardFailed: true}}
	record.Native.ImageID = "deleted-image"
	if _, err := store.Create(record); err != nil {
		t.Fatal(err)
	}
	if err := (App{Stdout: io.Discard, Stderr: io.Discard}).checkpointFork(context.Background(), []string{record.ID, "--dry-run"}); err == nil || !strings.Contains(err.Error(), "discarded") {
		t.Fatalf("discarded capture reached fork: %v", err)
	}
	if err := deleteCheckpoint(context.Background(), store, record.ID, true); err != nil {
		t.Fatalf("discarded capture blocked local cleanup: %v", err)
	}
}

func TestCheckpointCaptureBindingSurvivesReopenAndRejectsReplacedGeneration(t *testing.T) {
	for _, mutation := range []string{"unchanged", "revision", "source", "scope", "owner", "binding", "missing"} {
		t.Run(mutation, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			const leaseID = "cbx_abcdef123456"
			err := withDurableLeaseClaimLock(leaseID, func(claim *leaseClaim, _ bool, persist func() error) error {
				*claim = leaseClaim{LeaseID: leaseID, Provider: "machine0", CloudID: "fixture-source", ProviderScope: "fixture-scope", RepoRoot: "/fixture-repo", ClaimedAt: "2026-08-01T00:00:00Z"}
				return persist()
			})
			if err != nil {
				t.Fatal(err)
			}
			original, err := readLeaseClaim(leaseID)
			if err != nil {
				t.Fatal(err)
			}
			store, err := defaultCheckpointStore()
			if err != nil {
				t.Fatal(err)
			}
			record := checkpointRecord{ID: "chk_capture_generation", Kind: CheckpointKindMachine0, LeaseID: leaseID, Provider: "machine0", Capture: &NativeCheckpointCapture{SourceDisposition: "retire", Phase: "prepared", SourceID: original.CloudID, SourceScope: original.ProviderScope, SourceRevision: original.Revision, SourceClaimedAt: original.ClaimedAt}}
			record.Repo.Root = original.RepoRoot
			record, _, err = store.Reserve(record)
			if err != nil {
				t.Fatal(err)
			}
			// Reservation survived but binding did not run before process death.
			reopened := checkpointStore{root: store.root}
			record, _, err = reopened.Read(record.ID)
			if err != nil {
				t.Fatal(err)
			}
			bound, err := bindCheckpointCapture(record)
			if err != nil || bound.CheckpointCapture == nil || bound.CheckpointCapture.BoundRevision != bound.Revision || bound.Revision == original.Revision {
				t.Fatalf("durable binding=%+v err=%v", bound, err)
			}
			if err := AuthorizeCheckpointRelease(bound, ""); err == nil {
				t.Fatal("ordinary release cut through a held capture")
			}
			if err := AuthorizeCheckpointRelease(bound, record.ID); err == nil {
				t.Fatal("prepared capture authorized premature release")
			}
			record.Capture.Phase = "retiring"
			if err := reopened.Write(record); err != nil {
				t.Fatal(err)
			}
			if err := AuthorizeCheckpointRelease(bound, record.ID); err != nil {
				t.Fatalf("exact ready operation could not retire: %v", err)
			}
			path, err := leaseClaimPath(leaseID)
			if err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "revision":
				bound.Revision = "replacement-generation"
			case "source":
				bound.CloudID = "replacement-source"
			case "scope":
				bound.ProviderScope = "replacement-scope"
			case "owner":
				bound.RepoRoot = "/replacement-owner"
			case "binding":
				bound.CheckpointCapture.ID = "chk_other"
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if mutation != "missing" {
				if err := writeLeaseClaimAtomic(path, bound); err != nil {
					t.Fatal(err)
				}
			}
			_, err = bindCheckpointCapture(record)
			if (err == nil) != (mutation == "unchanged") {
				t.Fatalf("replay mutation=%s err=%v", mutation, err)
			}
			if _, err := os.Stat(filepath.Join(store.root, record.ID, checkpointMetaFile)); err != nil {
				t.Fatalf("failed replay lost its journal: %v", err)
			}
		})
	}
}

func TestCheckpointReservationFencesPreviouslyAuthorizedTouch(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const leaseID = "cbx_abcdef123456"
	if err := withDurableLeaseClaimLock(leaseID, func(claim *leaseClaim, _ bool, persist func() error) error {
		*claim = leaseClaim{LeaseID: leaseID, Provider: "machine0", CloudID: "fixture-source", ProviderScope: "fixture-scope", RepoRoot: "/fixture-repo", ClaimedAt: "2026-08-01T00:00:00Z"}
		return persist()
	}); err != nil {
		t.Fatal(err)
	}
	original, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	// Status authorized the touch, then capture reserved before its publication.
	if err := AuthorizeCheckpointRelease(original, ""); err != nil {
		t.Fatal(err)
	}
	store, err := defaultCheckpointStore()
	if err != nil {
		t.Fatal(err)
	}
	record := checkpointRecord{ID: "chk_touch_reservation", Kind: checkpointKindMachine0, Provider: "machine0", LeaseID: leaseID, Capture: &NativeCheckpointCapture{SourceDisposition: "retire", Phase: "prepared", SourceID: original.CloudID, SourceScope: original.ProviderScope, SourceRevision: original.Revision, SourceClaimedAt: original.ClaimedAt}}
	record.Repo.Root = original.RepoRoot
	if err := store.WithLock(record.ID, func() error {
		var err error
		record, _, err = reserveSourceCheckpoint(store, record, original)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := updateLeaseClaimTouchIfUnchanged(leaseID, original, map[string]string{"state": "ready"}, time.Now(), nil); err == nil {
		t.Fatal("previously authorized touch rewrote the reserved source claim")
	}
	current, err := readLeaseClaim(leaseID)
	if err != nil || !reflect.DeepEqual(current, original) {
		t.Fatalf("rejected touch changed source ownership: current=%+v err=%v", current, err)
	}
	if _, err := bindCheckpointCapture(record); err != nil {
		t.Fatalf("touch prevented the reserved operation from binding: %v", err)
	}
}
