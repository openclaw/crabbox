package parallels

import (
	"reflect"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestParallelsFixedReleaseRejectsInconsistentAbsenceIdentity(t *testing.T) {
	for _, field := range []string{"missing-incarnation", "changed-incarnation", "cloud-id", "immutable-id", "intent-scope", "attempt-host"} {
		t.Run(field, func(t *testing.T) {
			b, runner, req := fixedParallelsFixture(t)
			lease, err := b.Acquire(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			runner.rename(fixedLeaseVMName(t, req.RequestedLeaseID), "operator-renamed")
			err = core.WithDurableLeaseClaimLock(lease.LeaseID, func(c *core.LeaseClaim, _ bool, persist func() error) error {
				// Older records have no engine journal; they need the same fence.
				c.FixedCreateIntent.Journal = nil
				switch field {
				case "missing-incarnation":
					delete(c.FixedCreateIntent.Attempt, "vm_uuid")
				case "changed-incarnation":
					c.FixedCreateIntent.Attempt["vm_uuid"] = "{other-uuid}"
				case "cloud-id":
					c.CloudID = "{other-uuid}"
				case "immutable-id":
					c.CloudImmutableID = "{other-uuid}"
				case "intent-scope":
					c.FixedCreateIntent.ProviderScope = "other-host"
				case "attempt-host":
					c.FixedCreateIntent.Attempt["host"] = "other-host"
				}
				return persist()
			})
			if err != nil {
				t.Fatal(err)
			}
			// For inconsistent envelopes, even complete absence grants no authority.
			if field != "missing-incarnation" && field != "changed-incarnation" {
				runner.remove(lease.Server.CloudID)
			}
			before, _ := core.ReadLeaseClaim(lease.LeaseID)
			outcome, err := b.ReleaseLeaseWithOutcome(t.Context(), core.ReleaseLeaseRequest{Lease: lease})
			if err == nil || !strings.Contains(err.Error(), "lease_id_conflict") || outcome.Terminal {
				t.Errorf("invalid identity finalized: terminal=%v err=%v", outcome.Terminal, err)
			}
			after, readErr := core.ReadLeaseClaim(lease.LeaseID)
			if readErr != nil || !reflect.DeepEqual(before, after) {
				t.Errorf("invalid identity changed custody: %v", readErr)
			}
			if _, deletes := runner.counts(); deletes != 0 {
				t.Fatal("invalid record authorized deletion")
			}
		})
	}
}

func TestParallelsFixedReleaseValidatesTerminalReceipt(t *testing.T) {
	b, _, req := fixedParallelsFixture(t)
	lease, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if err := core.WithDurableLeaseClaimLock(lease.LeaseID, func(c *core.LeaseClaim, _ bool, persist func() error) error {
		c.FixedCreateIntent.Version++
		return persist()
	}); err != nil {
		t.Fatal(err)
	}
	outcome, err := b.ReleaseLeaseWithOutcome(t.Context(), core.ReleaseLeaseRequest{Lease: lease})
	if err == nil || !strings.Contains(err.Error(), "lease_id_conflict") || outcome.Terminal {
		t.Fatalf("invalid receipt reported release success: terminal=%v err=%v", outcome.Terminal, err)
	}
}
