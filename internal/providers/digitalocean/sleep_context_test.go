package digitalocean

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestWaitForDropletIPHonorsCancellation(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		start := time.Now()
		backend := &digitalOceanLeaseBackend{}
		api := &fakeDigitalOceanAPI{droplets: []droplet{{ID: 42}}}
		_, err := backend.waitForDropletIP(ctx, api, 42, 5*time.Minute)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waitForDropletIP returned %v, want context.Canceled", err)
		}
		if time.Since(start) > time.Second {
			t.Fatalf("waitForDropletIP took %v; expected immediate return on cancel", time.Since(start))
		}
	})
}

func TestWaitForDropletIPAcceptsPublicIPRegardlessOfStatus(t *testing.T) {
	item := droplet{ID: 42, Status: "off"}
	item.Networks.V4 = append(item.Networks.V4, struct {
		IPAddress string `json:"ip_address"`
		Type      string `json:"type"`
	}{IPAddress: "203.0.113.42", Type: "public"})
	api := &fakeDigitalOceanAPI{droplets: []droplet{item}}
	backend := &digitalOceanLeaseBackend{}
	got, err := backend.waitForDropletIP(context.Background(), api, item.ID, 5*time.Minute)
	if err != nil || got.ID != item.ID || api.getCalls != 1 {
		t.Fatalf("droplet=%#v err=%v getCalls=%d", got, err, api.getCalls)
	}
}

func TestWaitForDropletIPReturnsGetErrorImmediately(t *testing.T) {
	wantErr := errors.New("get denied")
	api := &fakeDigitalOceanAPI{getErr: wantErr}
	backend := &digitalOceanLeaseBackend{}
	_, err := backend.waitForDropletIP(context.Background(), api, 42, 5*time.Minute)
	if !errors.Is(err, wantErr) || api.getCalls != 1 {
		t.Fatalf("err=%v getCalls=%d", err, api.getCalls)
	}
}

func TestWaitForDropletIPPreservesClientDeadline(t *testing.T) {
	api := &fakeDigitalOceanAPI{getErr: context.DeadlineExceeded}
	_, err := new(digitalOceanLeaseBackend).waitForDropletIP(context.Background(), api, 42, time.Minute)
	if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "timed out waiting") {
		t.Fatalf("err=%v", err)
	}
}

func TestWaitForDropletIPPreservesReadErrorAtDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		wantErr := errors.New("late read denied")
		api := &fakeDigitalOceanAPI{getFn: func(ctx context.Context, _ int64) (droplet, error) {
			<-ctx.Done()
			return droplet{}, wantErr
		}}
		_, err := new(digitalOceanLeaseBackend).waitForDropletIP(context.Background(), api, 42, 10*time.Millisecond)
		if !errors.Is(err, wantErr) || strings.Contains(err.Error(), "timed out") {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestPendingRecoveryCancellationDuringDelayPreventsExtraPoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		listCalls := 0
		api := &fakeDigitalOceanAPI{listFn: func() ([]droplet, error) {
			listCalls++
			if listCalls == 1 {
				time.AfterFunc(time.Millisecond, cancel)
			}
			return nil, nil
		}}
		backend := &digitalOceanLeaseBackend{
			recoveryReconcilePolls:    3,
			recoveryReconcileInterval: time.Second,
		}
		_, found, err := backend.reconcilePendingRecovery(ctx, api, core.LeaseClaim{LeaseID: "cbx_abcdef123456", Slug: "late"}, "team:test")
		if !errors.Is(err, context.Canceled) || found || listCalls != 1 {
			t.Fatalf("found=%v err=%v listCalls=%d", found, err, listCalls)
		}
	})
}

func TestPendingRecoveryUsesExactPollCount(t *testing.T) {
	listCalls := 0
	api := &fakeDigitalOceanAPI{listFn: func() ([]droplet, error) {
		listCalls++
		return nil, nil
	}}
	backend := &digitalOceanLeaseBackend{
		recoveryReconcilePolls:    4,
		recoveryReconcileInterval: time.Nanosecond,
	}
	_, found, err := backend.reconcilePendingRecovery(context.Background(), api, core.LeaseClaim{LeaseID: "cbx_abcdef123456", Slug: "late"}, "team:test")
	if err != nil || found || listCalls != 4 {
		t.Fatalf("found=%v err=%v listCalls=%d", found, err, listCalls)
	}
}

func TestWaitForDropletIPPreservesCallerCauseDuringRead(t *testing.T) {
	for _, returnCause := range []bool{false, true} {
		t.Run(fmt.Sprint(returnCause), func(t *testing.T) {
			cause := core.Exit(7, "caller stopped acquisition")
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			api := &fakeDigitalOceanAPI{getFn: func(observeCtx context.Context, _ int64) (droplet, error) {
				cancel(cause)
				if returnCause {
					return droplet{}, context.Cause(observeCtx)
				}
				return droplet{}, observeCtx.Err()
			}}
			got, err := new(digitalOceanLeaseBackend).waitForDropletIP(ctx, api, 42, time.Minute)
			var exit core.ExitError
			if got.ID != 0 || !errors.Is(err, cause) || !errors.Is(err, context.Canceled) || !core.AsExitError(err, &exit) || exit.Code != 7 || err.Error() != cause.Error() || api.getCalls != 1 {
				t.Fatalf("droplet=%#v err=%v exit=%#v calls=%d", got, err, exit, api.getCalls)
			}
		})
	}
}

func TestWaitForDropletIPPreservesBudgetCause(t *testing.T) {
	for _, phase := range []string{"read", "sleep"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				api := &fakeDigitalOceanAPI{getFn: func(ctx context.Context, _ int64) (droplet, error) {
					if phase == "read" {
						<-ctx.Done()
						return droplet{}, ctx.Err()
					}
					return droplet{ID: 42}, nil
				}}
				parent := t.Context()
				got, err := new(digitalOceanLeaseBackend).waitForDropletIP(parent, api, 42, time.Second)
				var exit core.ExitError
				if got.ID != 0 || !errors.Is(err, context.DeadlineExceeded) || !core.AsExitError(err, &exit) || exit.Code != 5 || err.Error() != "timed out waiting for DigitalOcean Droplet IP" || api.getCalls != 1 || parent.Err() != nil {
					t.Fatalf("droplet=%#v err=%v exit=%#v calls=%d parent=%v", got, err, exit, api.getCalls, parent.Err())
				}
			})
		})
	}
}

func TestWaitForDropletIPCompletedResponseWinsCancellation(t *testing.T) {
	for _, ready := range []bool{false, true} {
		t.Run(fmt.Sprint(ready), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			responseErr := errors.Join(&digitalOceanAPIError{Status: 403}, context.Canceled)
			api := &fakeDigitalOceanAPI{getFn: func(context.Context, int64) (droplet, error) {
				cancel()
				if !ready {
					return droplet{}, responseErr
				}
				var item droplet
				item.ID = 42
				item.Networks.V4 = append(item.Networks.V4, struct {
					IPAddress string `json:"ip_address"`
					Type      string `json:"type"`
				}{IPAddress: "203.0.113.42", Type: "public"})
				return item, nil
			}}
			got, err := new(digitalOceanLeaseBackend).waitForDropletIP(ctx, api, 42, time.Minute)
			if ready && (err != nil || got.ID != 42) || !ready && err != responseErr || api.getCalls != 1 {
				t.Fatalf("droplet=%#v err=%v calls=%d", got, err, api.getCalls)
			}
		})
	}
}
