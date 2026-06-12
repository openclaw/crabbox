package islo

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type fakeBridgeClient struct {
	fakeIsloSyncClient
	created     []IsloShare
	preexisting []IsloShare
	listErr     error
}

func (f *fakeBridgeClient) CreateShare(_ context.Context, _ string, port int, ttl time.Duration) (IsloShare, error) {
	share := IsloShare{
		ShareID:   "shr_new",
		URL:       "https://new.share.islo.dev",
		Port:      port,
		ExpiresAt: time.Now().Add(ttl),
	}
	f.created = append(f.created, share)
	return share, nil
}

func (f *fakeBridgeClient) ListShares(context.Context, string) ([]IsloShare, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.preexisting, nil
}

func newBridgeBackend(t *testing.T, fake *fakeBridgeClient) *isloBackend {
	t.Helper()
	prev := newIsloClient
	newIsloClient = func(Config, Runtime) (isloAPI, error) { return fake, nil }
	t.Cleanup(func() { newIsloClient = prev })
	return &isloBackend{
		cfg: Config{Provider: isloProvider, Islo: IsloConfig{APIKey: "key"}},
		rt:  Runtime{},
	}
}

func TestPublishPeerReusesExistingShare(t *testing.T) {
	fake := &fakeBridgeClient{
		preexisting: []IsloShare{{ShareID: "shr_existing", URL: "https://existing.share.islo.dev", Port: 8080}},
	}
	backend := newBridgeBackend(t, fake)
	target, err := backend.PublishPeer(context.Background(), "isb_crabbox-x-abc123", 8080, time.Hour)
	if err != nil {
		t.Fatalf("PublishPeer: %v", err)
	}
	if target.ShareID != "shr_existing" || target.URL == "" {
		t.Fatalf("expected reuse of existing share, got %#v", target)
	}
	if len(fake.created) != 0 {
		t.Fatalf("expected no new share, got %d created", len(fake.created))
	}
}

func TestPublishPeerSkipsExpiredExistingShares(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name         string
		expiresAt    time.Time
		expiresAtSet bool
		wantCreate   bool
	}{
		{name: "non expiring", wantCreate: false},
		{name: "future", expiresAt: now.Add(time.Hour), wantCreate: false},
		{name: "expired", expiresAt: now.Add(-time.Second), wantCreate: true},
		{name: "imminent", expiresAt: now.Add(isloShareReuseSkew / 2), wantCreate: true},
		{name: "invalid expiry parse", expiresAtSet: true, wantCreate: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeBridgeClient{
				preexisting: []IsloShare{{
					ShareID:      "shr_existing",
					URL:          "https://existing.share.islo.dev",
					Port:         8080,
					ExpiresAt:    tt.expiresAt,
					ExpiresAtSet: tt.expiresAtSet,
				}},
			}
			backend := newBridgeBackend(t, fake)
			backend.rt.Clock = fixedClock{now: now}

			target, err := backend.PublishPeer(context.Background(), "isb_crabbox-x-abc123", 8080, time.Hour)
			if err != nil {
				t.Fatalf("PublishPeer: %v", err)
			}
			if tt.wantCreate {
				if target.ShareID != "shr_new" {
					t.Fatalf("target=%#v want new share", target)
				}
				if len(fake.created) != 1 {
					t.Fatalf("created=%d want 1", len(fake.created))
				}
				return
			}
			if target.ShareID != "shr_existing" {
				t.Fatalf("target=%#v want existing share", target)
			}
			if len(fake.created) != 0 {
				t.Fatalf("created=%d want 0", len(fake.created))
			}
		})
	}
}

func TestPublishPeerCreatesWhenAbsent(t *testing.T) {
	fake := &fakeBridgeClient{}
	backend := newBridgeBackend(t, fake)
	target, err := backend.PublishPeer(context.Background(), "isb_crabbox-x-abc123", 9090, time.Hour)
	if err != nil {
		t.Fatalf("PublishPeer: %v", err)
	}
	if target.ShareID != "shr_new" || target.Port != 9090 {
		t.Fatalf("expected new share, got %#v", target)
	}
	if len(fake.created) != 1 {
		t.Fatalf("expected 1 new share, got %d", len(fake.created))
	}
}

func TestPublishPeerDoesNotCreateWhenListFails(t *testing.T) {
	fake := &fakeBridgeClient{listErr: errors.New("islo down")}
	backend := newBridgeBackend(t, fake)
	_, err := backend.PublishPeer(context.Background(), "isb_crabbox-x-abc123", 9090, time.Hour)
	if err == nil || !strings.Contains(err.Error(), "islo down") {
		t.Fatalf("PublishPeer err=%v, want list error", err)
	}
	if len(fake.created) != 0 {
		t.Fatalf("created=%d want 0", len(fake.created))
	}
}

func TestPublishPeerRejectsNonCrabboxLease(t *testing.T) {
	fake := &fakeBridgeClient{}
	backend := newBridgeBackend(t, fake)
	if _, err := backend.PublishPeer(context.Background(), "isb_random-stranger", 8080, time.Hour); err == nil {
		t.Fatalf("expected rejection of non-Crabbox sandbox")
	}
	if _, err := backend.PublishPeer(context.Background(), "", 8080, time.Hour); err == nil {
		t.Fatalf("expected rejection of empty lease id")
	}
	if _, err := backend.PublishPeer(context.Background(), "isb_crabbox-x-abc123", 0, time.Hour); err == nil {
		t.Fatalf("expected rejection of port 0")
	}
	if _, err := backend.PublishPeer(context.Background(), "isb_crabbox-x-abc123", 70000, time.Hour); err == nil {
		t.Fatalf("expected rejection of port 70000")
	}
}

func TestListPeerTargetsFiltersBlankURLs(t *testing.T) {
	fake := &fakeBridgeClient{
		preexisting: []IsloShare{
			{ShareID: "a", URL: "https://a.share.islo.dev", Port: 8080},
			{ShareID: "b", URL: "", Port: 9090},
			{ShareID: "c", URL: "https://c.share.islo.dev", Port: 5432},
		},
	}
	backend := newBridgeBackend(t, fake)
	targets, err := backend.ListPeerTargets(context.Background(), "isb_crabbox-x-abc123")
	if err != nil {
		t.Fatalf("ListPeerTargets: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("expected 2 targets with URLs, got %d: %#v", len(targets), targets)
	}
}

func TestListPeerTargetsSurfacesErrors(t *testing.T) {
	fake := &fakeBridgeClient{listErr: errors.New("islo down")}
	backend := newBridgeBackend(t, fake)
	if _, err := backend.ListPeerTargets(context.Background(), "isb_crabbox-x-abc123"); err == nil {
		t.Fatalf("expected listErr to surface")
	}
}

// Static check: ensure isloBackend satisfies core.BridgeProvider.
var _ core.BridgeProvider = (*isloBackend)(nil)

type fixedClock struct {
	now time.Time
}

func (c fixedClock) Now() time.Time {
	return c.now
}
