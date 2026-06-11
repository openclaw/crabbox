//go:build smoke

package morph

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// TestLiveMorphAcquireResolveTouchReleaseLease exercises the real Morph Cloud
// REST API through the same morphLeaseBackend path a user would hit with
// `crabbox warmup --provider morph`. The test boots a real instance from a
// developer-owned snapshot, drives the full lifecycle (Acquire → Resolve →
// Touch → List → ReleaseLease), and unconditionally releases the lease on
// exit so a panic or failure cannot leak a billable instance.
//
// The test is skipped unless CRABBOX_MORPH_API_KEY (or MORPH_API_KEY) and
// CRABBOX_LIVE_MORPH_SNAPSHOT are set, and is skipped under -short. It never
// embeds the key, the response body, or the per-instance SSH private key in
// any log line; the value of $CRABBOX_MORPH_API_KEY is only read from the
// process environment.
//
//	go test -tags smoke -run TestLiveMorphAcquireResolveTouchReleaseLease \
//	  -count=1 -v ./internal/providers/morph/
func TestLiveMorphAcquireResolveTouchReleaseLease(t *testing.T) {
	if testing.Short() {
		t.Skip("live morph skipped in -short mode")
	}
	key := firstNonBlank(os.Getenv("CRABBOX_MORPH_API_KEY"), os.Getenv("MORPH_API_KEY"))
	if key == "" {
		t.Skip("set CRABBOX_MORPH_API_KEY or MORPH_API_KEY to run live morph")
	}
	snapshotID := strings.TrimSpace(os.Getenv("CRABBOX_LIVE_MORPH_SNAPSHOT"))
	if snapshotID == "" {
		t.Skip("set CRABBOX_LIVE_MORPH_SNAPSHOT to run live morph")
	}

	cfg := testMorphConfig()
	cfg.Morph.APIKey = key
	cfg.Morph.Snapshot = snapshotID
	cfg.Morph.DeleteOnRelease = true
	cfg.TTL = 10 * time.Minute
	cfg.IdleTimeout = 5 * time.Minute

	rt := Runtime{HTTP: &http.Client{Timeout: 60 * time.Second}}
	backend, err := NewMorphBackend(Provider{}.Spec(), cfg, rt)
	if err != nil {
		t.Fatalf("new morph backend: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		t.Fatalf("backend does not implement core.DoctorBackend")
	}
	doctorCtx, doctorCancel := context.WithTimeout(ctx, 60*time.Second)
	defer doctorCancel()
	if _, err := doctor.Doctor(doctorCtx, DoctorRequest{}); err != nil {
		t.Fatalf("doctor: %v", err)
	}

	leaseBackend, ok := backend.(core.SSHLeaseBackend)
	if !ok {
		t.Fatalf("backend does not implement core.SSHLeaseBackend")
	}

	slug := "live-morph-smoke"
	acquireStart := time.Now()
	lease, err := leaseBackend.Acquire(ctx, AcquireRequest{RequestedSlug: slug})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if lease.LeaseID == "" {
		t.Fatalf("acquire returned empty lease id")
	}
	if lease.Server.CloudID == "" {
		t.Fatalf("acquire returned empty instance id")
	}
	instanceID := lease.Server.CloudID
	t.Cleanup(func() {
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer releaseCancel()
		if relErr := leaseBackend.ReleaseLease(releaseCtx, ReleaseLeaseRequest{Lease: lease}); relErr != nil {
			t.Logf("cleanup release lease failed (ignored): %v", relErr)
		} else {
			t.Logf("cleanup released instance_id=%s", instanceID)
		}
	})

	resolveCtx, resolveCancel := context.WithTimeout(ctx, 60*time.Second)
	defer resolveCancel()
	resolved, err := leaseBackend.Resolve(resolveCtx, ResolveRequest{ID: lease.LeaseID})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.Server.CloudID != instanceID {
		t.Fatalf("resolve cloudID=%q want %q", resolved.Server.CloudID, instanceID)
	}

	touchCtx, touchCancel := context.WithTimeout(ctx, 60*time.Second)
	defer touchCancel()
	touched, err := leaseBackend.Touch(touchCtx, TouchRequest{
		Lease:       resolved,
		IdleTimeout: 60 * time.Second,
		State:       "running",
	})
	if err != nil {
		t.Fatalf("touch: %v", err)
	}
	if touched.Labels["state"] == "" {
		t.Fatalf("touch left state label empty: %#v", touched.Labels)
	}
	if got := touched.Labels["idle_timeout_secs"]; got != "60" {
		t.Fatalf("touch idle_timeout_secs=%q want %q", got, "60")
	}
	if touched.Labels["last_touched_at"] == "" {
		t.Fatalf("touch did not set last_touched_at: %#v", touched.Labels)
	}

	listCtx, listCancel := context.WithTimeout(ctx, 60*time.Second)
	defer listCancel()
	views, err := leaseBackend.List(listCtx, ListRequest{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var matched bool
	for _, v := range views {
		if v.Labels["lease"] == lease.LeaseID {
			matched = true
			break
		}
	}
	if !matched {
		t.Fatalf("list did not return lease_id=%s in %d views", lease.LeaseID, len(views))
	}

	t.Logf("live morph ok instance_id=%s slug=%s state=%s idle_timeout_secs=%s readiness_wait=%s",
		instanceID, touched.Labels["slug"], touched.Labels["state"], touched.Labels["idle_timeout_secs"], time.Since(acquireStart).Round(time.Second))
}

func firstNonBlank(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
