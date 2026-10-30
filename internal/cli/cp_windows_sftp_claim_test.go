package cli

import (
	"context"
	"errors"
	"testing"
	"time"
)

func windowsCopyClaimTestLease(t *testing.T, leaseID string) LeaseTarget {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	server := Server{Provider: "aws", CloudID: "vm-a"}
	if err := ClaimLeaseTargetForRepoConfig(leaseID, "windows-copy", Config{Provider: "aws"}, server, SSHTarget{}, "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	return LeaseTarget{LeaseID: leaseID, Server: server}
}

func TestWindowsCopyHoldsTheClaimFenceThroughTheTransfer(t *testing.T) {
	lease := windowsCopyClaimTestLease(t, "cbx_wincopyfence1")
	ran := false
	err := copyWindowsSFTPUnderClaim(t.Context(), lease, "/repo", func() error {
		ran = true
		claim, err := ReadLeaseClaim(lease.LeaseID)
		if err != nil {
			return err
		}
		waitCtx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		if err := withLeaseClaimUnchangedContext(waitCtx, lease.LeaseID, claim, false, func() error { return nil }); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("claim writer was admitted during the copy: %v", err)
		}
		return nil
	})
	if err != nil || !ran {
		t.Fatalf("copy under an unchanged claim: ran=%v err=%v", ran, err)
	}
	if err := UpdateLeaseClaimEndpoint(lease.LeaseID, Server{Provider: "aws", CloudID: "vm-a"}, SSHTarget{Host: "192.0.2.10"}); err != nil {
		t.Fatalf("claim writer still blocked after the copy: %v", err)
	}
}

func TestWindowsCopyRefusesAChangedClaimBeforeTransfer(t *testing.T) {
	cases := []struct {
		name   string
		change func(t *testing.T, leaseID string)
	}{
		{"revoked", func(_ *testing.T, leaseID string) { RemoveLeaseClaim(leaseID) }},
		{"reclaimed-by-another-repository", func(t *testing.T, leaseID string) {
			if err := ClaimLeaseTargetForRepoConfig(leaseID, "windows-copy", Config{Provider: "aws"}, Server{Provider: "aws", CloudID: "vm-a"}, SSHTarget{}, "/other-repo", time.Minute, true); err != nil {
				t.Fatal(err)
			}
		}},
		{"reassigned-to-another-resource", func(t *testing.T, leaseID string) {
			current, err := ReadLeaseClaim(leaseID)
			if err != nil {
				t.Fatal(err)
			}
			replacement := cloneLeaseClaim(current)
			replacement.CloudID = "vm-b"
			if err := ReplaceLeaseClaimIfUnchanged(leaseID, current, replacement); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lease := windowsCopyClaimTestLease(t, "cbx_wincopychange")
			tc.change(t, lease.LeaseID)
			ran := false
			err := copyWindowsSFTPUnderClaim(t.Context(), lease, "/repo", func() error { ran = true; return nil })
			var exit ExitError
			if ran || !AsExitError(err, &exit) || exit.Code != 4 {
				t.Fatalf("copy after the claim was %s: ran=%v err=%v", tc.name, ran, err)
			}
		})
	}
}
