package azuresandbox

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestStatusReportsOriginalAllocationExpiry(t *testing.T) {
	for _, state := range []string{"Running", "Stopped"} {
		for _, expired := range []bool{false, true} {
			t.Run(state+map[bool]string{false: "/future", true: "/expired"}[expired], func(t *testing.T) {
				f := &fixture{}
				b, req := fixtureBackend(t, f)
				if _, err := b.acquire(t.Context(), req); err != nil {
					t.Fatal(err)
				}
				deadline := time.Now().Add(time.Hour).UTC()
				if expired {
					deadline = time.Now().Add(-time.Hour).UTC()
				}
				expires := deadline.Format(time.RFC3339Nano)
				if err := core.WithDurableLeaseClaimLock(req.RequestedLeaseID, func(claim *core.LeaseClaim, exists bool, persist func() error) error {
					claim.FixedCreateIntent.Attempt["expires_at"] = expires
					return persist()
				}); err != nil {
					t.Fatal(err)
				}
				f.box.State = state
				before, err := b.claim(req.RequestedLeaseID)
				if err != nil {
					t.Fatal(err)
				}
				// A later configured TTL must not renew the original allocation.
				b.cfg.TTL = 24 * time.Hour
				view, err := b.Status(t.Context(), core.StatusRequest{ID: req.RequestedLeaseID})
				if err != nil {
					t.Fatal(err)
				}
				wantState := state
				if expired {
					wantState = "expired"
				}
				if view.State != wantState || view.Ready != (!expired && state == "Running") || view.ExpiresAt != expires || view.ServerID != f.box.ID || view.ID != req.RequestedLeaseID || !reflect.DeepEqual(view.Labels, f.box.Labels) {
					t.Fatalf("unexpected status: %+v", view)
				}
				wire, _ := json.Marshal(view)
				var fields map[string]any
				if err := json.Unmarshal(wire, &fields); err != nil || fields["expiresAt"] != expires {
					t.Fatal("canonical expiry missing from JSON")
				}
				after, err := b.claim(req.RequestedLeaseID)
				if err != nil || !reflect.DeepEqual(before, after) || f.creates != 1 || f.deletes != 0 {
					t.Fatal("status mutated lease or resource")
				}
				if expired {
					if _, err := b.Run(t.Context(), core.RunRequest{ID: req.RequestedLeaseID, Repo: req.Repo, Keep: true, NoSync: true, Command: []string{"true"}}); err == nil {
						t.Fatal("expired run admitted")
					}
					if _, err := b.Heartbeat(t.Context(), core.LeaseHeartbeatRequest{ID: req.RequestedLeaseID}); err == nil {
						t.Fatal("expired heartbeat admitted")
					}
				}
				if err := b.Stop(t.Context(), core.StopRequest{ID: req.RequestedLeaseID}); err != nil {
					t.Fatal(err)
				}
				view, err = b.Status(t.Context(), core.StatusRequest{ID: req.RequestedLeaseID})
				if err != nil || view.State != "deleted" || view.Ready || f.deletes != 1 {
					t.Fatalf("canonical cleanup failed: %+v %v", view, err)
				}
			})
		}
	}
}

func TestStatusInvalidExpiryAndForeignOwnershipFailClosed(t *testing.T) {
	for _, invalid := range []string{"", "not-a-deadline", "foreign"} {
		t.Run(invalid, func(t *testing.T) {
			f := &fixture{}
			b, req := fixtureBackend(t, f)
			if _, err := b.acquire(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			if invalid == "foreign" {
				f.box.Labels["crabbox_attempt"] = "different-attempt"
			} else if err := core.WithDurableLeaseClaimLock(req.RequestedLeaseID, func(claim *core.LeaseClaim, exists bool, persist func() error) error {
				claim.FixedCreateIntent.Attempt["expires_at"] = invalid
				return persist()
			}); err != nil {
				t.Fatal(err)
			}
			view, err := b.Status(t.Context(), core.StatusRequest{ID: req.RequestedLeaseID})
			if err == nil || view.Ready || f.deletes != 0 {
				t.Fatalf("unsafe status: %+v %v", view, err)
			}
		})
	}
}
