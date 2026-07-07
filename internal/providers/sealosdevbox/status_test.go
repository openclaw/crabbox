package sealosdevbox

import "testing"

func TestDevboxReadyIgnoresDesiredState(t *testing.T) {
	item := devboxItem{Spec: devboxSpec{State: "Running"}}
	if got := normalizeDevboxState(item); got != "Pending" {
		t.Fatalf("normalizeDevboxState()=%q, want Pending without observed status", got)
	}
	if devboxReady(item) {
		t.Fatal("devboxReady()=true from desired spec.state without observed status")
	}

	item.Status.State = "Running"
	if !devboxReady(item) {
		t.Fatal("devboxReady()=false after status.state reports Running")
	}
}
