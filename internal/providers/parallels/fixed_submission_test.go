package parallels

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestParallelsFixedSubmittedAttemptDoesNotRecloneMissingVM(t *testing.T) {
	for _, failure := range []string{"lost_reply", "failed_readback", "missing_submission_evidence"} {
		t.Run(failure, func(t *testing.T) {
			backend, runner, req := fixedParallelsFixture(t)
			if failure == "lost_reply" {
				runner.cloneErr, runner.cloneCommit = errors.New("clone reply lost"), true
			} else {
				runner.afterClone = func() { runner.listAllErr = errors.New("inventory unavailable after clone") }
			}
			if _, err := backend.Acquire(context.Background(), req); err == nil {
				t.Fatal("interrupted acquisition returned a usable lease")
			}
			claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
			if err != nil {
				t.Fatal(err)
			}
			if claim.FixedCreateIntent == nil || claim.FixedCreateIntent.State != "prepared" || claim.CloudID != "" {
				t.Fatalf("expected an unbound prepared attempt: %+v", claim)
			}
			if failure == "missing_submission_evidence" {
				rewriteFixedClaim(t, req.RequestedLeaseID, func(claim *core.LeaseClaim) {
					delete(claim.FixedCreateIntent.Attempt, "submission")
				})
				delete(claim.FixedCreateIntent.Attempt, "submission")
			}
			vm, exists := runner.find(claim.FixedCreateIntent.Attempt["name"])
			if !exists {
				t.Fatal("fixture did not create the original VM")
			}
			runner.remove(vm.ID)
			runner.cloneErr, runner.listAllErr, runner.afterClone = nil, nil, nil

			_, err = backend.Acquire(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
				t.Fatalf("replay after loss of an unbound clone err=%v, want conflict", err)
			}
			if clones, deletes := runner.counts(); clones != 1 || deletes != 0 {
				t.Fatalf("clone/delete calls=%d/%d, want 1/0", clones, deletes)
			}
			after, err := core.ReadLeaseClaim(req.RequestedLeaseID)
			if err != nil {
				t.Fatal(err)
			}
			if after.FixedCreateIntent == nil || after.FixedCreateIntent.State != "prepared" || after.CloudID != "" || !maps.Equal(after.FixedCreateIntent.Attempt, claim.FixedCreateIntent.Attempt) {
				t.Fatalf("uncertain custody changed during rejected replay: %+v", after)
			}
		})
	}
}

func TestParallelsFixedSnapshotPreflightFailureCanRetry(t *testing.T) {
	backend, runner, req := fixedParallelsFixture(t)
	backend.Cfg.Parallels.CloneMode = "linked"
	backend.Cfg.Parallels.SourceSnapshotID = "snapshot-ready"
	runner.snapshotErr = errors.New("snapshot inventory unavailable")
	if _, err := backend.Acquire(context.Background(), req); err == nil {
		t.Fatal("failed snapshot preflight returned a usable lease")
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.FixedCreateIntent == nil || claim.FixedCreateIntent.Attempt["submission"] != "pending" {
		t.Fatalf("snapshot preflight burned the create attempt: %+v", claim)
	}
	if clones, _ := runner.counts(); clones != 0 {
		t.Fatalf("clone calls=%d after failed snapshot preflight, want 0", clones)
	}
	runner.snapshotErr = nil
	runner.beforeClone = func() {
		claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
		if err != nil {
			t.Fatal(err)
		}
		if claim.FixedCreateIntent == nil || claim.FixedCreateIntent.Attempt["submission"] != "submitted" {
			t.Fatalf("linked clone started without a durable submission record: %+v", claim)
		}
	}
	if _, err := backend.Acquire(context.Background(), req); err != nil {
		t.Fatalf("retry after snapshot preflight recovered: %v", err)
	}
	if clones, _ := runner.counts(); clones != 1 {
		t.Fatalf("clone calls=%d, want 1", clones)
	}
}
