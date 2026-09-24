package boxd

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"google.golang.org/grpc/codes"
)

func TestPendingCreateAbsenceRecovery(t *testing.T) {
	for _, name := range []string{"absent", "org", "same name", "inventory failure", "late inventory failure", "late machine", "malformed inventory", "wrong account", "wrong scope", "wrong name", "bound ID", "cancelled", "stale claim"} {
		t.Run(name, func(t *testing.T) {
			b, f := fixtureBackend(t)
			if name == "org" {
				b.cfg.Boxd.Org = "example-org"
				f.listOrg = "example-org"
			}
			f.createUnavailable = true
			if _, err := b.Acquire(t.Context(), core.AcquireRequest{}); err == nil {
				t.Fatal("expected ambiguous create")
			}
			claim := onlyClaim(t)
			before := claim
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch name {
			case "same name":
				f.mutate(func() { f.rows = []fakeVM{{ID: "external", Name: claim.Labels["machine"]}} })
			case "inventory failure":
				f.mutate(func() { f.listStatus = codes.Unavailable })
			case "late inventory failure", "late machine":
				f.beforeList = func() {
					if f.count("ListVms") > 1 {
						f.mutate(func() {
							if name == "late machine" {
								f.rows = []fakeVM{{ID: "late", Name: claim.Labels["machine"]}}
							} else {
								f.listStatus = codes.PermissionDenied
							}
						})
					}
				}
			case "malformed inventory":
				f.mutate(func() { f.rows = []fakeVM{{ID: "external"}} })
			case "wrong account":
				f.mutate(func() { f.user = "different-user" })
			case "wrong scope":
				b.cfg.Boxd.Org = "other-org"
			case "wrong name", "bound ID":
				changed := map[string]string{}
				for key, value := range claim.Labels {
					changed[key] = value
				}
				if name == "wrong name" {
					changed["machine"] = "unrelated"
				} else {
					changed["vm_id"] = "vm-1"
				}
				var err error
				claim, err = core.UpdateLeaseClaimLabelsIfUnchanged(claim.LeaseID, claim, changed)
				if err != nil {
					t.Fatal(err)
				}
				before = claim
			case "cancelled":
				f.beforeList = cancel
			case "stale claim":
				changed := map[string]string{}
				for key, value := range claim.Labels {
					changed[key] = value
				}
				changed["extra"] = "concurrent writer"
				var err error
				before, err = core.UpdateLeaseClaimLabelsIfUnchanged(claim.LeaseID, claim, changed)
				if err != nil {
					t.Fatal(err)
				}
			}
			forgotten, err := core.ForgetAbsentLeaseClaim(ctx, b, claim)
			want := name == "absent" || name == "org"
			if forgotten != want || (err == nil) != want {
				t.Fatalf("forgotten=%t err=%v", forgotten, err)
			}
			if want {
				assertNoClaims(t)
			} else if got := onlyClaim(t); !reflect.DeepEqual(got, before) {
				t.Fatal("retained claim changed")
			}
			if f.count("DestroyVm vm-1") != 0 || f.count("DestroyVm external") != 0 || f.count("StopVm vm-1") != 0 {
				t.Fatal("recovery mutated a remote machine")
			}
			if name == "stale claim" && f.count("ListVms") != 0 {
				t.Fatal("stale claim reached inventory")
			}
		})
	}
}

func TestPendingCreateAbsenceRecoveryHoldsClaimFence(t *testing.T) {
	b, f := fixtureBackend(t)
	f.createUnavailable = true
	_, _ = b.Acquire(t.Context(), core.AcquireRequest{})
	claim := onlyClaim(t)
	f.beforeList = func() {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
		defer cancel()
		err := core.WithDurableLeaseClaimLockContext(ctx, claim.LeaseID, func(*core.LeaseClaim, bool, func() error) error {
			t.Error("claim writer escaped absence proof fence")
			return nil
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("lock error: %v", err)
		}
	}
	if forgotten, err := core.ForgetAbsentLeaseClaim(t.Context(), b, claim); !forgotten || err != nil {
		t.Fatalf("forgotten=%t err=%v", forgotten, err)
	}
}
