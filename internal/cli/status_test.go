package cli

import "testing"

func TestStatusWaitDoneTreatsTerminalStatesAsDone(t *testing.T) {
	for _, state := range []string{"expired", "failed", "released", "stopped", "stopped_with_code", "terminated"} {
		if !statusWaitDone(statusView{State: state}) {
			t.Fatalf("statusWaitDone(%q) = false, want true", state)
		}
	}
	if statusWaitDone(statusView{State: "provisioning"}) {
		t.Fatal("statusWaitDone(provisioning) = true, want false")
	}
	if !statusWaitDone(statusView{State: "provisioning", Ready: true}) {
		t.Fatal("statusWaitDone(ready provisioning) = false, want true")
	}
}

func TestStatusWaitTerminalErrorFailsNonReadyTerminalState(t *testing.T) {
	err := statusWaitTerminalError("cbx_123", statusView{State: "stopped"})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 5 {
		t.Fatalf("statusWaitTerminalError = %#v, want exit 5", err)
	}
	if err := statusWaitTerminalError("cbx_123", statusView{State: "stopped", Ready: true}); err != nil {
		t.Fatalf("ready terminal state returned error: %v", err)
	}
	if err := statusWaitTerminalError("cbx_123", statusView{State: "provisioning"}); err != nil {
		t.Fatalf("non-terminal state returned error: %v", err)
	}
}

func TestLeaseStatusStateCanBeReadyRejectsTerminalStates(t *testing.T) {
	for _, state := range []string{"stopped", "released", "terminated"} {
		if leaseStatusStateCanBeReady(LeaseTarget{}, state) {
			t.Fatalf("leaseStatusStateCanBeReady(%q) = true, want false", state)
		}
	}
	if leaseStatusStateCanBeReady(LeaseTarget{}, "provisioning") {
		t.Fatal("leaseStatusStateCanBeReady(provisioning) = true, want false")
	}
	if !leaseStatusStateCanBeReady(LeaseTarget{}, "ready") {
		t.Fatal("leaseStatusStateCanBeReady(ready) = false, want true")
	}
}
