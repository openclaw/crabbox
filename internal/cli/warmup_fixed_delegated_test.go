package cli

import (
	"bytes"
	"testing"
)

func TestWarmupDelegatedPreservesFixedLeaseID(t *testing.T) {
	p := setupProbeAdmission(t, nil)
	var stdout, stderr bytes.Buffer
	const leaseID = "cbx_012345abcdef"
	if err := (App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), []string{"warmup", "--lease-id", leaseID}); err != nil {
		t.Fatal(err)
	}
	if p.warmed != 1 || p.warmupRequest.RequestedLeaseID != leaseID {
		t.Fatalf("delegated warmup lost fixed ID: calls=%d id=%q", p.warmed, p.warmupRequest.RequestedLeaseID)
	}
}
