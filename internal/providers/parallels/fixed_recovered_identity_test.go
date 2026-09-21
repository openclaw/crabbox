package parallels

import (
	"errors"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestParallelsFixedRenamedRecoveryRetainsUUID(t *testing.T) {
	b, runner, req := fixedParallelsFixture(t)
	runner.cloneErr, runner.cloneCommit = errors.New("lost clone reply"), true
	if _, err := b.Acquire(t.Context(), req); err == nil {
		t.Fatal("lost reply returned a usable lease")
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.CloudID != "" || parallelsFixedIncarnation(claim.FixedCreateIntent) != "" {
		t.Fatal("fixture already bound the unobserved clone")
	}
	name := claim.FixedCreateIntent.Attempt["name"]
	vm, found := runner.find(name)
	if !found {
		t.Fatal("fixture did not create a clone")
	}
	runner.rename(name, "operator-renamed")
	runner.cloneErr, runner.cloneCommit = nil, false
	if _, err := b.Acquire(t.Context(), req); err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Fatalf("renamed clone was admitted: %v", err)
	}
	recovered, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.CloudID != vm.ID || recovered.CloudImmutableID != vm.ID || parallelsFixedIncarnation(recovered.FixedCreateIntent) != vm.ID {
		t.Fatal("rejected name validation lost the recovered UUID")
	}
	// The known incarnation, once removed, permits authoritative absence cleanup.
	runner.remove(vm.ID)
	outcome, err := b.ReleaseLeaseWithOutcome(t.Context(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: req.RequestedLeaseID}})
	if err != nil || !outcome.Terminal {
		t.Fatalf("removed recovered clone remained stranded: outcome=%+v err=%v", outcome, err)
	}
	terminal, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || terminal.FixedCreateIntent.State != "released" {
		t.Fatal("cleanup did not retain the terminal receipt")
	}
	if clones, deletes := runner.counts(); clones != 1 || deletes != 0 {
		t.Fatalf("unexpected provider mutations: clones=%d deletes=%d", clones, deletes)
	}
}
