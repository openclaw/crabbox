package digitalocean

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (f *fakeDigitalOceanAPI) CreateFixedDroplet(ctx context.Context, cfg core.Config, publicKey, leaseID, slug string, keep bool, now time.Time, labels map[string]string) (droplet, error) {
	item, err := f.CreateDroplet(ctx, cfg, publicKey, leaseID, slug, keep, now)
	if err == nil {
		item.Tags = tagsFromLabels(labels)
		f.created[len(f.created)-1] = item
		f.droplets = append(f.droplets, item)
		if f.fixedReplyErr != nil {
			err = &ambiguousDropletCreateError{err: f.fixedReplyErr, keyID: item.SSHKeyID, keyCreated: item.SSHKeyCreated, keyOwnershipKnown: true}
		}
	}
	return item, err
}

func TestFixedDropletLifecycle(t *testing.T) {
	api := &fakeDigitalOceanAPI{}
	b := newTestBackend(t, api)
	req := core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123456", RequestedSlug: "fixed", Repo: core.Repo{Root: t.TempDir()}, Keep: true}
	first, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if first.Server.ID != replay.Server.ID || len(api.createRequests) != 1 {
		t.Fatalf("duplicate allocation: %v", api.createRequests)
	}
	changed := req
	changed.Keep = false
	if _, err := b.Acquire(t.Context(), changed); err == nil {
		t.Fatal("changed intent accepted")
	}
	lease, err := b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || claim.FixedCreateIntent.State != "released" {
		t.Fatalf("missing tombstone: %+v %v", claim, err)
	}
	if _, err := b.Acquire(t.Context(), req); err == nil {
		t.Fatal("released ID recreated")
	}
	lease, err = b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 1 || len(api.deletedKeyIDs) != 1 {
		t.Fatalf("cleanup: droplets=%v keys=%v", api.deleted, api.deletedKeyIDs)
	}
}

func TestFixedDropletReadinessRecoveryAndIdentity(t *testing.T) {
	api := &fakeDigitalOceanAPI{}
	b := newTestBackend(t, api)
	req := core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123457", RequestedSlug: "recover", Repo: core.Repo{Root: t.TempDir()}}
	b.waitSSH = func(context.Context, *core.SSHTarget, string, time.Duration) error {
		return errors.New("connection lost")
	}
	if _, err := b.Acquire(t.Context(), req); err == nil {
		t.Fatal("expected readiness failure")
	}
	if len(api.deleted) != 0 {
		t.Fatal("ambiguous resource rolled back")
	}
	b.waitSSH = func(context.Context, *core.SSHTarget, string, time.Duration) error { return nil }
	if _, err := b.Acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if len(api.createRequests) != 1 {
		t.Fatal("duplicate create")
	}
	api.accountID = "team:other"
	if _, err := b.Acquire(t.Context(), req); err == nil {
		t.Fatal("account change accepted")
	}
	api.accountID = ""
	api.getFn = func(context.Context, int64) (droplet, error) { item := api.created[0]; item.ID++; return item, nil }
	if _, err := b.Acquire(t.Context(), req); err == nil {
		t.Fatal("replacement adopted")
	}
}

func TestFixedDropletAmbiguousCreateNeverResubmits(t *testing.T) {
	api := &fakeDigitalOceanAPI{createErr: errors.New("reply lost")}
	b := newTestBackend(t, api)
	req := core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123458", RequestedSlug: "ambiguous", Repo: core.Repo{Root: t.TempDir()}}
	if _, err := b.Acquire(t.Context(), req); err == nil {
		t.Fatal("expected create failure")
	}
	api.createErr = nil
	if _, err := b.Acquire(t.Context(), req); err == nil || !strings.Contains(err.Error(), "no replacement allocated") {
		t.Fatalf("replay: %v", err)
	}
	if len(api.createRequests) != 1 {
		t.Fatal("ambiguous POST resubmitted")
	}
}

func TestFixedDropletLostCreateReplyAndCleanupRetry(t *testing.T) {
	api := &fakeDigitalOceanAPI{fixedReplyErr: errors.New("reply lost")}
	b := newTestBackend(t, api)
	req := core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123459", RequestedSlug: "lost-reply", Repo: core.Repo{Root: t.TempDir()}}
	if _, err := b.Acquire(t.Context(), req); err == nil {
		t.Fatal("expected lost response")
	}
	lease, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.ID != 100 || len(api.createRequests) != 1 {
		t.Fatal("lost reply created replacement")
	}
	api.keyDeleteErr = errors.New("key deletion failed")
	if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err == nil {
		t.Fatal("expected cleanup failure")
	}
	api.keyDeleteErr = nil
	lease, err = b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || claim.FixedCreateIntent.State != "released" {
		t.Fatalf("missing tombstone: %+v %v", claim, err)
	}
}

func TestFixedDropletBindsLeaseMetadata(t *testing.T) {
	api := &fakeDigitalOceanAPI{}
	b := newTestBackend(t, api)
	b.Cfg.Tailscale.Enabled = true
	b.Cfg.Tailscale.AuthKey = "test-only-key"
	req := core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123460", RequestedSlug: "metadata", Repo: core.Repo{Root: t.TempDir()}}
	lease, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.Labels["tailscale_hostname"] == "" {
		t.Fatal("generated Tailscale hostname missing from lease labels")
	}
	b.Cfg.Pond = "different-network"
	if _, err := b.Acquire(t.Context(), req); err == nil {
		t.Fatal("changed pond accepted")
	}
	if len(api.createRequests) != 1 {
		t.Fatal("duplicate create")
	}
}
