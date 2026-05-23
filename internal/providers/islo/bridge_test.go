package islo

import (
	"context"
	"errors"
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
