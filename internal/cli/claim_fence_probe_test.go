//go:build crabbox_claim_fence_probe

package cli

import (
	"strings"
	"testing"
	"time"
)

func resetClaimFenceProbe(t *testing.T) {
	t.Helper()
	claimFenceProbeMu.Lock()
	claimFenceProbeFired = false
	claimFenceProbeMu.Unlock()
	t.Cleanup(func() {
		claimFenceProbeMu.Lock()
		claimFenceProbeFired = false
		claimFenceProbeMu.Unlock()
	})
}

func claimFenceProbeTestLease(t *testing.T, leaseID string) leaseClaim {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := ClaimLeaseTargetForRepoConfig(leaseID, "probe", Config{Provider: "aws"}, Server{Provider: "aws", CloudID: "i-123"}, SSHTarget{}, "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	expected, err := ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	return expected
}

func TestClaimFenceProbeSwapIsRefusedByTheFence(t *testing.T) {
	for _, mode := range []string{"revoke", "reassign"} {
		t.Run(mode, func(t *testing.T) {
			resetClaimFenceProbe(t)
			leaseID := "cbx_fenceprobe" + mode
			expected := claimFenceProbeTestLease(t, leaseID)
			t.Setenv(claimFenceProbeEnv, "TestClaimFenceProbeSwapIsRefusedByTheFence:"+mode)
			called := false
			err := WithLeaseClaimUnchanged(leaseID, expected, func() error {
				called = true
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), "claim changed") || called {
				t.Fatalf("fence err=%v called=%v", err, called)
			}
			path, err := leaseClaimPath(leaseID)
			if err != nil {
				t.Fatal(err)
			}
			current, exists, readErr := readLeaseClaimPathWithPresence(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			switch mode {
			case "revoke":
				if exists {
					t.Fatalf("revoked claim still present: %#v", current)
				}
			case "reassign":
				if !exists || current.CloudID != "i-123-reassigned" {
					t.Fatalf("reassigned claim=%#v exists=%v", current, exists)
				}
			}
		})
	}
}

func TestClaimFenceProbeFiresOnceAndOnlyAtTheNamedSite(t *testing.T) {
	resetClaimFenceProbe(t)
	leaseID := "cbx_fenceprobesite"
	expected := claimFenceProbeTestLease(t, leaseID)
	t.Setenv(claimFenceProbeEnv, "someOtherSite:revoke")
	if err := WithLeaseClaimUnchanged(leaseID, expected, func() error { return nil }); err != nil {
		t.Fatalf("unnamed site was disturbed: %v", err)
	}
	t.Setenv(claimFenceProbeEnv, "TestClaimFenceProbeFiresOnceAndOnlyAtTheNamedSite:reassign")
	if err := WithLeaseClaimUnchanged(leaseID, expected, nil); err == nil {
		t.Fatal("named site was not swapped")
	}
	reassigned, err := ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	if err := WithLeaseClaimUnchanged(leaseID, reassigned, func() error { called = true; return nil }); err != nil || !called {
		t.Fatalf("probe fired twice: err=%v called=%v", err, called)
	}
}
