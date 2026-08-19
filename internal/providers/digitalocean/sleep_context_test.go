package digitalocean

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestWaitForDropletIPHonorsCancellation(t *testing.T) {
	t.Parallel()
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
	wantErr := errors.New("late read denied")
	api := &fakeDigitalOceanAPI{getFn: func(ctx context.Context, _ int64) (droplet, error) {
		<-ctx.Done()
		return droplet{}, wantErr
	}}
	_, err := new(digitalOceanLeaseBackend).waitForDropletIP(context.Background(), api, 42, 10*time.Millisecond)
	if !errors.Is(err, wantErr) || strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err=%v", err)
	}
}

func TestPendingRecoveryCancellationDuringDelayPreventsExtraPoll(t *testing.T) {
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
