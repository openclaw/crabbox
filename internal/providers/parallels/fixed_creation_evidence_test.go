package parallels

import (
	"context"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

// R7 — the clone's identity comes from the creation operation, not from a later
// lookup of its mutable name.
//
// prlctl clone reports no UUID, so ParallelsClient.Clone resolves the new VM by
// name afterwards. If another management operation moves the clone and puts a
// different VM at that name before the lookup, binding that lookup's UUID would
// hand the replacement the per-lease key and, later, deletion authority. The
// attempt's own --dst directory is the only create-time attribute Crabbox
// controls, so it is what identifies the incarnation.
func TestParallelsFixedBindsCloneIdentityNotNameLookup(t *testing.T) {
	backend, runner, req := fixedParallelsFixture(t)

	// Between `prlctl clone` and the lookup that follows it, the clone is moved
	// aside and an unrelated VM takes its name.
	runner.afterClone = func() {
		name := core.ParallelsLeaseVMName(req.RequestedLeaseID, "quiet-lobster")
		for i := range runner.vms {
			if runner.vms[i].Name == name {
				runner.vms[i].Name = name + "-moved"
			}
		}
		runner.addAtLocked(name, runner.vmHome)
	}

	if _, err := backend.Acquire(context.Background(), req); err == nil {
		t.Fatal("a replaced name must not produce a usable lease")
	}

	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	dst := strings.TrimSpace(claim.FixedCreateIntent.Attempt["dst"])
	if dst == "" {
		t.Fatal("the attempt recorded no creation directory, so nothing attests which VM was created")
	}
	var created, impostor core.ParallelsVM
	runner.mu.Lock()
	for _, vm := range runner.vms {
		if strings.HasPrefix(vm.Home, strings.TrimRight(dst, "/")+"/") {
			created = vm
		} else if vm.Name == core.ParallelsLeaseVMName(req.RequestedLeaseID, "quiet-lobster") {
			impostor = vm
		}
	}
	runner.mu.Unlock()
	if created.ID == "" || impostor.ID == "" {
		t.Fatalf("fixture did not produce both a clone and a replacement: created=%q impostor=%q", created.ID, impostor.ID)
	}

	bound := claim.FixedCreateIntent.Attempt["vm_uuid"]
	if bound == impostor.ID {
		t.Fatalf("the attempt bound the replacement VM %q found by name lookup", impostor.ID)
	}
	if bound != "" && bound != created.ID {
		t.Fatalf("attempt vm_uuid=%q, want the created clone %q or nothing", bound, created.ID)
	}
	if claim.CloudID == impostor.ID || claim.CloudImmutableID == impostor.ID {
		t.Fatalf("the claim bound the replacement VM %q", impostor.ID)
	}
	runner.mu.Lock()
	execs := len(runner.execCalls)
	runner.mu.Unlock()
	if execs != 0 {
		t.Fatalf("guest exec calls=%d, want 0: the replacement received guest mutation", execs)
	}
	if _, deletes := runner.counts(); deletes != 0 {
		t.Fatalf("delete calls=%d, want 0", deletes)
	}
}

// R7b — the created VM is discovered by its creation directory, so a clone that
// is renamed before Crabbox reads it back is still bound to this lease rather
// than lost.
func TestParallelsFixedCloneIdentitySurvivesRename(t *testing.T) {
	backend, runner, req := fixedParallelsFixture(t)
	name := core.ParallelsLeaseVMName(req.RequestedLeaseID, "quiet-lobster")
	runner.afterClone = func() {
		for i := range runner.vms {
			if runner.vms[i].Name == name {
				runner.vms[i].Name = name + "-moved"
			}
		}
	}
	if _, err := backend.Acquire(context.Background(), req); err == nil {
		t.Fatal("a renamed clone must not silently acquire under its old name")
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	var created core.ParallelsVM
	runner.mu.Lock()
	for _, vm := range runner.vms {
		if vm.Name == name+"-moved" {
			created = vm
		}
	}
	runner.mu.Unlock()
	if created.ID == "" {
		t.Fatal("fixture did not rename the clone")
	}
	if got := claim.FixedCreateIntent.Attempt["vm_uuid"]; got != created.ID {
		t.Fatalf("attempt vm_uuid=%q, want the created clone %q bound by its creation directory", got, created.ID)
	}
}
