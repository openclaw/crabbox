package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFilterClaimsForPond(t *testing.T) {
	claims := []leaseClaim{
		{LeaseID: "isb_1", Slug: "web", Provider: "islo", Pond: "alpha"},
		{LeaseID: "isb_2", Slug: "client", Provider: "islo", Pond: "Alpha"},
		{LeaseID: "cbx_3", Slug: "db", Provider: "hetzner", Pond: "alpha"},
		{LeaseID: "isb_4", Slug: "other", Provider: "islo", Pond: "bravo"},
		{LeaseID: "isb_5", Slug: "noPond", Provider: "islo"},
	}
	out := filterClaimsForPond(claims, "alpha", "islo")
	if len(out) != 2 {
		t.Fatalf("expected 2 islo+alpha claims, got %d: %#v", len(out), out)
	}
	if out[0].LeaseID != "isb_1" || out[1].LeaseID != "isb_2" {
		t.Fatalf("unexpected order: %#v", out)
	}
	if all := filterClaimsForPond(claims, "alpha", ""); len(all) != 3 {
		t.Fatalf("expected 3 alpha claims across providers, got %d", len(all))
	}
	if empty := filterClaimsForPond(claims, "", "islo"); len(empty) != 0 {
		t.Fatalf("expected 0 claims for empty pond, got %d", len(empty))
	}
	if none := filterClaimsForPond(claims, "charlie", "islo"); len(none) != 0 {
		t.Fatalf("expected 0 matches for pond=charlie, got %d", len(none))
	}
}

type fakeBridgeProvider struct {
	listed    map[string][]BridgePeerTarget
	published map[string]BridgePeerTarget
	listErr   error
	listErrs  map[string]error
	pubErr    error
	calls     int
}

type fakeTailnetBridgeProvider struct {
	*fakeBridgeProvider
	meta        TailscaleMetadata
	validateErr error
}

type pondReleaseRetentionBackend struct {
	retain bool
}

func (b pondReleaseRetentionBackend) Spec() ProviderSpec {
	return ProviderSpec{Name: "test"}
}

func (b pondReleaseRetentionBackend) RetainLeaseClaimAfterRelease(LeaseTarget) bool {
	return b.retain
}

func (f *fakeTailnetBridgeProvider) ValidateTailnetPeer(context.Context, string) (TailscaleMetadata, error) {
	return f.meta, f.validateErr
}

func (f *fakeBridgeProvider) PublishPeer(_ context.Context, leaseID string, port int, _ time.Duration) (BridgePeerTarget, error) {
	f.calls++
	if f.pubErr != nil {
		return BridgePeerTarget{}, f.pubErr
	}
	t := f.published[leaseID]
	t.Port = port
	return t, nil
}

func (f *fakeBridgeProvider) ListPeerTargets(_ context.Context, leaseID string) ([]BridgePeerTarget, error) {
	f.calls++
	if err := f.listErrs[leaseID]; err != nil {
		return nil, err
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listed[leaseID], nil
}

func withTempClaims(t *testing.T, claims []leaseClaim) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, "missing.yaml"))
	t.Setenv("CRABBOX_COORDINATOR", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")
	t.Setenv("CRABBOX_COORDINATOR_ADMIN_TOKEN", "")
	t.Setenv("CRABBOX_PROVIDER", "")
	for _, claim := range claims {
		if err := claimLeaseForRepoProviderScopePond(claim.LeaseID, claim.Slug, claim.Provider, claim.ProviderScope, claim.Pond, claim.RepoRoot, 30*time.Minute, false); err != nil {
			t.Fatalf("seed claim %s: %v", claim.LeaseID, err)
		}
	}
}

func TestResolvePondPeersListsTargets(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "isb_web", Slug: "bridge-web", Provider: "islo", Pond: "demo", RepoRoot: "/r"},
		{LeaseID: "isb_client", Slug: "bridge-client", Provider: "islo", Pond: "demo", RepoRoot: "/r"},
		{LeaseID: "isb_other", Slug: "noise", Provider: "islo", Pond: "other", RepoRoot: "/r"},
	})
	fake := &fakeBridgeProvider{
		listed: map[string][]BridgePeerTarget{
			"isb_web": {{Port: 8080, URL: "https://web.share.islo.dev", ShareID: "shr_web"}},
		},
	}
	prev := loadBridgeProviderFunc
	loadBridgeProviderFunc = func(provider string, _ Runtime) (BridgeProvider, error) {
		if provider != "islo" {
			t.Fatalf("unexpected provider %q", provider)
		}
		return fake, nil
	}
	t.Cleanup(func() { loadBridgeProviderFunc = prev })

	peers, err := resolvePondPeers(context.Background(), Runtime{}, "demo", "islo", pondPeersFlags{})
	if err != nil {
		t.Fatalf("resolvePondPeers: %v", err)
	}
	if len(peers) != 2 {
		t.Fatalf("expected 2 peers, got %d: %#v", len(peers), peers)
	}
	if peers[0].Slug != "bridge-client" || peers[1].Slug != "bridge-web" {
		t.Fatalf("peers not sorted by slug: %#v", peers)
	}
	if len(peers[1].Targets) != 1 || peers[1].Targets[0].URL == "" {
		t.Fatalf("expected web peer to have 1 target with URL, got %#v", peers[1].Targets)
	}
	if len(peers[0].Targets) != 0 {
		t.Fatalf("client peer should have no targets in this fake, got %#v", peers[0].Targets)
	}
	if peers[0].Pond != "demo" || peers[1].Pond != "demo" {
		t.Fatalf("peers should carry pond=demo, got %#v", peers)
	}
}

func TestResolvePondPeersPublishesShare(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "isb_w", Slug: "w", Provider: "islo", Pond: "demo", RepoRoot: "/r"},
	})
	fake := &fakeBridgeProvider{
		published: map[string]BridgePeerTarget{
			"isb_w": {URL: "https://abc.share.islo.dev", ShareID: "shr_w"},
		},
	}
	prev := loadBridgeProviderFunc
	loadBridgeProviderFunc = func(string, Runtime) (BridgeProvider, error) { return fake, nil }
	t.Cleanup(func() { loadBridgeProviderFunc = prev })

	peers, err := resolvePondPeers(context.Background(), Runtime{}, "demo", "islo", pondPeersFlags{SharePort: 8080, ShareTTL: time.Hour})
	if err != nil {
		t.Fatalf("resolvePondPeers: %v", err)
	}
	if len(peers) != 1 || len(peers[0].Targets) != 1 {
		t.Fatalf("expected 1 peer with 1 target, got %#v", peers)
	}
	if peers[0].Targets[0].Port != 8080 || peers[0].Targets[0].URL == "" {
		t.Fatalf("publish should have set port=8080 and URL: %#v", peers[0].Targets[0])
	}
	if fake.calls != 1 {
		t.Fatalf("expected 1 fake call, got %d", fake.calls)
	}
}

func TestResolvePondPeersPublishesShareForOutboundTailnet(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "isb_w", Slug: "w", Provider: "islo", Pond: "demo", RepoRoot: "/r"},
	})
	mutateClaim(t, "isb_w", func(c *leaseClaim) { setLeaseClaimTailscale(c, "100.64.7.7", "") })
	fake := &fakeBridgeProvider{
		published: map[string]BridgePeerTarget{
			"isb_w": {URL: "https://abc.share.islo.dev", ShareID: "shr_w"},
		},
	}
	prev := loadBridgeProviderFunc
	loadBridgeProviderFunc = func(string, Runtime) (BridgeProvider, error) { return fake, nil }
	t.Cleanup(func() { loadBridgeProviderFunc = prev })

	peers, err := resolvePondPeers(context.Background(), Runtime{}, "demo", "islo", pondPeersFlags{SharePort: 8080, ShareTTL: time.Hour})
	if err != nil {
		t.Fatalf("resolvePondPeers: %v", err)
	}
	if len(peers) != 1 || len(peers[0].Targets) != 1 {
		t.Fatalf("expected 1 URL peer with 1 target, got %#v", peers)
	}
	if peers[0].Transport != TransportURL || peers[0].Endpoint != "https://abc.share.islo.dev" {
		t.Fatalf("outbound tailnet must not replace URL primary: %#v", peers[0])
	}
	if len(peers[0].Transports) != 1 || peers[0].Transports[0] != TransportURL || !strings.Contains(peers[0].Note, "outbound proxy") {
		t.Fatalf("unexpected dialable transport surface: %#v", peers[0])
	}
	if peers[0].Targets[0].Port != 8080 || peers[0].Targets[0].URL == "" {
		t.Fatalf("publish should have set port=8080 and URL: %#v", peers[0].Targets[0])
	}
	if fake.calls != 1 {
		t.Fatalf("expected 1 fake call, got %d", fake.calls)
	}
}

func TestResolvePondPeersListsSharesForOutboundTailnet(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "isb_w", Slug: "w", Provider: "islo", Pond: "demo", RepoRoot: "/r"},
	})
	mutateClaim(t, "isb_w", func(c *leaseClaim) { c.TailscaleIPv4 = "100.64.7.7" })
	fake := &fakeBridgeProvider{
		listed: map[string][]BridgePeerTarget{
			"isb_w": {{Port: 8080, URL: "https://abc.share.islo.dev", ShareID: "shr_w"}},
		},
	}
	prev := loadBridgeProviderFunc
	loadBridgeProviderFunc = func(string, Runtime) (BridgeProvider, error) { return fake, nil }
	t.Cleanup(func() { loadBridgeProviderFunc = prev })

	peers, err := resolvePondPeers(context.Background(), Runtime{}, "demo", "islo", pondPeersFlags{})
	if err != nil {
		t.Fatalf("resolvePondPeers: %v", err)
	}
	if len(peers) != 1 || len(peers[0].Targets) != 1 {
		t.Fatalf("expected 1 URL peer with its existing target, got %#v", peers)
	}
	if peers[0].Transport != TransportURL || peers[0].Endpoint != "https://abc.share.islo.dev" {
		t.Fatalf("outbound tailnet must not replace URL primary: %#v", peers[0])
	}
	if peers[0].Targets[0].URL != "https://abc.share.islo.dev" {
		t.Fatalf("existing target missing: %#v", peers[0].Targets)
	}
	if fake.calls != 1 {
		t.Fatalf("expected 1 fake call, got %d", fake.calls)
	}
}

func TestResolvePondPeersReturnsBridgeLoadErrorForOutboundTailnet(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "isb_w", Slug: "w", Provider: "islo", Pond: "demo", RepoRoot: "/r"},
	})
	mutateClaim(t, "isb_w", func(c *leaseClaim) { c.TailscaleIPv4 = "100.64.7.7" })
	prev := loadBridgeProviderFunc
	loadBridgeProviderFunc = func(string, Runtime) (BridgeProvider, error) {
		return nil, errors.New("missing Islo API key")
	}
	t.Cleanup(func() { loadBridgeProviderFunc = prev })

	peers, err := resolvePondPeers(context.Background(), Runtime{}, "demo", "islo", pondPeersFlags{})
	if err == nil || !strings.Contains(err.Error(), "missing Islo API key") {
		t.Fatalf("expected primary URL bridge load error, got peers=%#v err=%v", peers, err)
	}
	if peers != nil {
		t.Fatalf("single-provider resolution should fail without partial output: %#v", peers)
	}
}

func TestResolvePondPeersReturnsErrorWhenAllOutboundTailnetURLsFail(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "isb_tailnet", Slug: "tailnet", Provider: "islo", Pond: "demo", RepoRoot: "/r"},
		{LeaseID: "isb_url", Slug: "url", Provider: "islo", Pond: "demo", RepoRoot: "/r"},
	})
	mutateClaim(t, "isb_tailnet", func(c *leaseClaim) { c.TailscaleIPv4 = "100.64.7.7" })
	fake := &fakeBridgeProvider{listErr: errors.New("Islo API unavailable")}
	prev := loadBridgeProviderFunc
	loadBridgeProviderFunc = func(string, Runtime) (BridgeProvider, error) { return fake, nil }
	t.Cleanup(func() { loadBridgeProviderFunc = prev })

	peers, err := resolvePondPeers(context.Background(), Runtime{}, "demo", "islo", pondPeersFlags{})
	if err == nil || !strings.Contains(err.Error(), "Islo API unavailable") {
		t.Fatalf("expected primary URL failure, got peers=%#v err=%v", peers, err)
	}
	if peers != nil {
		t.Fatalf("single-provider resolution should fail without partial output: %#v", peers)
	}
}

func TestResolvePondPeersKeepsHealthyURLPeerWhenURLMemberFails(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "isb_healthy", Slug: "healthy", Provider: "islo", Pond: "demo", RepoRoot: "/r"},
		{LeaseID: "isb_failed", Slug: "failed", Provider: "islo", Pond: "demo", RepoRoot: "/r"},
	})
	fake := &fakeBridgeProvider{
		listed: map[string][]BridgePeerTarget{
			"isb_healthy": {{Port: 8080, URL: "https://healthy.share.islo.dev"}},
		},
		listErrs: map[string]error{
			"isb_failed": errors.New("Islo API unavailable"),
		},
	}
	prev := loadBridgeProviderFunc
	loadBridgeProviderFunc = func(string, Runtime) (BridgeProvider, error) { return fake, nil }
	t.Cleanup(func() { loadBridgeProviderFunc = prev })

	peers, err := resolvePondPeers(context.Background(), Runtime{}, "demo", "islo", pondPeersFlags{})
	if err != nil {
		t.Fatalf("failed URL member must not discard healthy URL peer: %v", err)
	}
	if len(peers) != 2 {
		t.Fatalf("expected healthy and degraded members, got %#v", peers)
	}
	bySlug := map[string]BridgePeer{}
	for _, peer := range peers {
		bySlug[peer.Slug] = peer
	}
	if got := bySlug["healthy"]; got.Transport != TransportURL || got.Endpoint != "https://healthy.share.islo.dev" {
		t.Fatalf("healthy URL peer missing: %#v", got)
	}
	if got := bySlug["failed"]; got.Transport != TransportNone || got.BridgeState != "error" {
		t.Fatalf("failed URL peer should degrade in place: %#v", got)
	}
}

func TestResolvePondPeersFallsBackWhenTailnetValidationFails(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "isb_w", Slug: "w", Provider: "islo", Pond: "demo", RepoRoot: "/r"},
	})
	mutateClaim(t, "isb_w", func(c *leaseClaim) { setLeaseClaimTailscale(c, "100.64.7.7", "") })
	fake := &fakeTailnetBridgeProvider{
		fakeBridgeProvider: &fakeBridgeProvider{listed: map[string][]BridgePeerTarget{
			"isb_w": {{Port: 8080, URL: "https://abc.share.islo.dev"}},
		}},
		validateErr: errors.New("daemon unavailable"),
	}
	prev := loadBridgeProviderFunc
	loadBridgeProviderFunc = func(string, Runtime) (BridgeProvider, error) { return fake, nil }
	t.Cleanup(func() { loadBridgeProviderFunc = prev })

	peers, err := resolvePondPeers(context.Background(), Runtime{}, "demo", "islo", pondPeersFlags{})
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 || peers[0].Transport != TransportURL || peers[0].Endpoint != "https://abc.share.islo.dev" {
		t.Fatalf("expected URL fallback after failed tailnet validation, got %#v", peers)
	}
	for _, key := range []string{"tailscale", "tailscale_state", "tailscale_ipv4", "tailscale_fqdn"} {
		if peers[0].Labels[key] != "" {
			t.Fatalf("URL fallback retained stale %s label: %#v", key, peers[0])
		}
	}
}

func TestResolvePondPeersReturnsBridgeErrorWhenAllTailnetClaimsAreStale(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "isb_w", Slug: "w", Provider: "islo", Pond: "demo", RepoRoot: "/r"},
	})
	mutateClaim(t, "isb_w", func(c *leaseClaim) { setLeaseClaimTailscale(c, "100.64.7.7", "") })
	fake := &fakeTailnetBridgeProvider{
		fakeBridgeProvider: &fakeBridgeProvider{listErr: errors.New("Islo API unavailable")},
		validateErr:        errors.New("daemon unavailable"),
	}
	prev := loadBridgeProviderFunc
	loadBridgeProviderFunc = func(string, Runtime) (BridgeProvider, error) { return fake, nil }
	t.Cleanup(func() { loadBridgeProviderFunc = prev })

	if _, err := resolvePondPeers(context.Background(), Runtime{}, "demo", "islo", pondPeersFlags{}); err == nil {
		t.Fatal("expected bridge error after the only tailnet claim failed validation")
	}
}

func TestResolvePondPeersRevalidatesPersistedTailnetEnrollment(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "isb_w", Slug: "w", Provider: "islo", Pond: "demo", RepoRoot: "/r"},
	})
	mutateClaim(t, "isb_w", func(c *leaseClaim) { c.TailscaleHostname = "node-a" })
	fake := &fakeTailnetBridgeProvider{
		fakeBridgeProvider: &fakeBridgeProvider{listed: map[string][]BridgePeerTarget{
			"isb_w": {{Port: 8080, URL: "https://abc.share.islo.dev"}},
		}},
		meta: TailscaleMetadata{Enabled: true, IPv4: "100.64.7.8", State: "ready"},
	}
	prev := loadBridgeProviderFunc
	loadBridgeProviderFunc = func(string, Runtime) (BridgeProvider, error) { return fake, nil }
	t.Cleanup(func() { loadBridgeProviderFunc = prev })

	peers, err := resolvePondPeers(context.Background(), Runtime{}, "demo", "islo", pondPeersFlags{})
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 || peers[0].Transport != TransportURL || peers[0].Endpoint != "https://abc.share.islo.dev" || peers[0].Labels["tailscale_ipv4"] != "100.64.7.8" {
		t.Fatalf("validated endpoint and labels disagree: %#v", peers)
	}
}

func TestResolvePondPeersUnknownProvider(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "isb_w", Slug: "w", Provider: "islo", Pond: "demo", RepoRoot: "/r"},
	})
	prev := loadBridgeProviderFunc
	loadBridgeProviderFunc = func(string, Runtime) (BridgeProvider, error) {
		return nil, nil // backend exists but does not implement BridgeProvider
	}
	t.Cleanup(func() { loadBridgeProviderFunc = prev })
	peers, err := resolvePondPeers(context.Background(), Runtime{}, "demo", "islo", pondPeersFlags{})
	if err != nil {
		t.Fatalf("resolvePondPeers: %v", err)
	}
	if len(peers) != 1 || len(peers[0].Targets) != 0 {
		t.Fatalf("expected 1 peer with no targets, got %#v", peers)
	}
	if peers[0].BridgeState != "unsupported-provider" {
		t.Fatalf("expected BridgeState=unsupported-provider for backend without BridgeProvider, got %q", peers[0].BridgeState)
	}
}

func TestResolvePondPeersExplicitlyUnsupportedAdapter(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "isb_islo", Slug: "fn", Provider: "islo", Pond: "demo", RepoRoot: "/r"},
	})
	fake := &fakeBridgeProvider{listErr: ErrBridgeNotImplemented}
	prev := loadBridgeProviderFunc
	loadBridgeProviderFunc = func(string, Runtime) (BridgeProvider, error) { return fake, nil }
	t.Cleanup(func() { loadBridgeProviderFunc = prev })

	peers, err := resolvePondPeers(context.Background(), Runtime{}, "demo", "islo", pondPeersFlags{})
	if err != nil {
		t.Fatalf("resolvePondPeers: %v", err)
	}
	if len(peers) != 1 || peers[0].BridgeState != "unsupported" {
		t.Fatalf("expected ErrBridgeNotImplemented to surface as BridgeState=unsupported, got %#v", peers)
	}
	if peers[0].Transport != TransportNone {
		t.Fatalf("transport=%q want none", peers[0].Transport)
	}
	if len(peers[0].Targets) != 0 {
		t.Fatalf("expected no targets when adapter reports unsupported, got %#v", peers[0].Targets)
	}
}

func TestResolvePondPeersExplicitlyUnsupportedPublish(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "isb_islo", Slug: "edge", Provider: "islo", Pond: "demo", RepoRoot: "/r"},
	})
	fake := &fakeBridgeProvider{pubErr: ErrBridgeNotImplemented}
	prev := loadBridgeProviderFunc
	loadBridgeProviderFunc = func(string, Runtime) (BridgeProvider, error) { return fake, nil }
	t.Cleanup(func() { loadBridgeProviderFunc = prev })

	peers, err := resolvePondPeers(context.Background(), Runtime{}, "demo", "islo", pondPeersFlags{SharePort: 8080, ShareTTL: time.Hour})
	if err != nil {
		t.Fatalf("resolvePondPeers: %v", err)
	}
	if len(peers) != 1 || peers[0].BridgeState != "unsupported" {
		t.Fatalf("expected publish unsupported to surface as BridgeState=unsupported, got %#v", peers)
	}
}

func TestResolvePondPeersMultiProviderFanOut(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "isb_islo1", Slug: "islo-a", Provider: "islo", Pond: "demo", RepoRoot: "/r"},
		{LeaseID: "cbx_e2b1", Slug: "e2b-a", Provider: "e2b", Pond: "demo", RepoRoot: "/r"},
		{LeaseID: "cbx_modal1", Slug: "modal-a", Provider: "modal", Pond: "demo", RepoRoot: "/r"},
		{LeaseID: "isb_other", Slug: "noise", Provider: "islo", Pond: "other", RepoRoot: "/r"},
	})
	// Each provider key maps to a distinct adapter so we can assert that
	// resolvePondPeers picks the right backend per provider rather than
	// applying one backend uniformly.
	adapters := map[string]*fakeBridgeProvider{
		"islo": {listed: map[string][]BridgePeerTarget{"isb_islo1": {{Port: 80, URL: "https://islo-a.share.islo.dev"}}}},
		"e2b":  {listed: map[string][]BridgePeerTarget{"cbx_e2b1": {{Port: 8080, URL: "https://8080-sbx.e2b.app"}}}},
	}
	prev := loadBridgeProviderFunc
	loadBridgeProviderFunc = func(provider string, _ Runtime) (BridgeProvider, error) {
		if adapter, ok := adapters[provider]; ok {
			return adapter, nil
		}
		t.Fatalf("unexpected provider in fan-out: %q", provider)
		return nil, nil
	}
	t.Cleanup(func() { loadBridgeProviderFunc = prev })

	// Empty provider triggers the fan-out path.
	peers, err := resolvePondPeers(context.Background(), Runtime{}, "demo", "", pondPeersFlags{})
	if err != nil {
		t.Fatalf("resolvePondPeers: %v", err)
	}
	if len(peers) != 3 {
		t.Fatalf("expected 3 peers across providers, got %d: %#v", len(peers), peers)
	}
	byProvider := map[string]BridgePeer{}
	for _, p := range peers {
		byProvider[p.Provider] = p
	}
	if got := byProvider["islo"].Targets; len(got) != 1 || got[0].URL == "" {
		t.Fatalf("islo peer should have its target, got %#v", got)
	}
	if got := byProvider["e2b"].Targets; len(got) != 1 || got[0].URL == "" {
		t.Fatalf("e2b peer should have its target, got %#v", got)
	}
	modal := byProvider["modal"]
	if modal.Transport != TransportNone {
		t.Fatalf("modal transport=%q want none", modal.Transport)
	}
	if modal.BridgeState != "" {
		t.Fatalf("modal peer should not enter bridge path, got state %q", modal.BridgeState)
	}
	if modal.Note != "no advertised pond transport for provider modal" {
		t.Fatalf("modal note=%q", modal.Note)
	}
}

func TestResolvePondPeersMultiProviderFanOutKeepsFailedProviderRow(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "cbx_e2b1", Slug: "e2b-a", Provider: "e2b", Pond: "demo", RepoRoot: "/r"},
		{LeaseID: "isb_islo1", Slug: "islo-a", Provider: "islo", Pond: "demo", RepoRoot: "/r"},
	})
	adapters := map[string]*fakeBridgeProvider{
		"e2b":  {listed: map[string][]BridgePeerTarget{"cbx_e2b1": {{Port: 8080, URL: "https://8080-sbx.e2b.app"}}}},
		"islo": {listErr: errors.New("islo api down")},
	}
	prev := loadBridgeProviderFunc
	loadBridgeProviderFunc = func(provider string, _ Runtime) (BridgeProvider, error) {
		if adapter, ok := adapters[provider]; ok {
			return adapter, nil
		}
		t.Fatalf("unexpected provider in fan-out: %q", provider)
		return nil, nil
	}
	t.Cleanup(func() { loadBridgeProviderFunc = prev })

	peers, err := resolvePondPeers(context.Background(), Runtime{}, "demo", "", pondPeersFlags{})
	if err != nil {
		t.Fatalf("resolvePondPeers should keep healthy providers when one fails: %v", err)
	}
	if len(peers) != 2 {
		t.Fatalf("expected healthy and degraded peers, got %d: %#v", len(peers), peers)
	}
	byProvider := map[string]BridgePeer{}
	for _, peer := range peers {
		byProvider[peer.Provider] = peer
	}
	if got := byProvider["e2b"]; len(got.Targets) != 1 || got.Targets[0].URL == "" {
		t.Fatalf("expected healthy e2b peer with target, got %#v", got)
	}
	if got := byProvider["islo"]; got.Transport != TransportNone || got.BridgeState != "error" {
		t.Fatalf("expected degraded islo peer, got %#v", got)
	}
}

func TestResolvePondPeersMultiProviderFanOutFailsWhenEveryProviderFails(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "cbx_e2b1", Slug: "e2b-a", Provider: "e2b", Pond: "demo", RepoRoot: "/r"},
		{LeaseID: "isb_islo1", Slug: "islo-a", Provider: "islo", Pond: "demo", RepoRoot: "/r"},
	})
	prev := loadBridgeProviderFunc
	loadBridgeProviderFunc = func(provider string, _ Runtime) (BridgeProvider, error) {
		return &fakeBridgeProvider{listErr: errors.New(provider + " api down")}, nil
	}
	t.Cleanup(func() { loadBridgeProviderFunc = prev })

	_, err := resolvePondPeers(context.Background(), Runtime{}, "demo", "", pondPeersFlags{})
	if err == nil {
		t.Fatalf("expected error when every provider fails")
	}
	if !strings.Contains(err.Error(), "e2b") {
		t.Fatalf("expected first provider error to identify provider, got %v", err)
	}
}

func TestResolvePondPeersListError(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "isb_w", Slug: "w", Provider: "islo", Pond: "demo", RepoRoot: "/r"},
	})
	fake := &fakeBridgeProvider{listErr: errors.New("api down")}
	prev := loadBridgeProviderFunc
	loadBridgeProviderFunc = func(string, Runtime) (BridgeProvider, error) { return fake, nil }
	t.Cleanup(func() { loadBridgeProviderFunc = prev })
	if _, err := resolvePondPeers(context.Background(), Runtime{}, "demo", "islo", pondPeersFlags{}); err == nil {
		t.Fatalf("expected ListPeerTargets error to surface")
	}
}

func TestPondPeersCommandRendersJSON(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "isb_w", Slug: "web", Provider: "islo", Pond: "demo", RepoRoot: "/r"},
	})
	fake := &fakeBridgeProvider{
		listed: map[string][]BridgePeerTarget{
			"isb_w": {{Port: 8080, URL: "https://x.share.islo.dev"}},
		},
	}
	prev := loadBridgeProviderFunc
	loadBridgeProviderFunc = func(string, Runtime) (BridgeProvider, error) { return fake, nil }
	t.Cleanup(func() { loadBridgeProviderFunc = prev })

	var out, errBuf strings.Builder
	app := App{Stdout: &out, Stderr: &errBuf}
	if err := app.pondPeers(context.Background(), []string{"--pond", "demo", "--json"}); err != nil {
		t.Fatalf("pondPeers: %v", err)
	}
	var payload pondPeersJSON
	if err := json.Unmarshal([]byte(out.String()), &payload); err != nil {
		t.Fatalf("decode json: %v (raw=%q)", err, out.String())
	}
	peers := payload.Members
	if len(peers) != 1 || peers[0].Targets[0].URL == "" {
		t.Fatalf("unexpected peers JSON: %#v", peers)
	}
	if peers[0].Transport != TransportURL {
		t.Fatalf("expected transport=url for islo peer, got %q", peers[0].Transport)
	}
}

func TestPondPeersCommandRequiresPond(t *testing.T) {
	var out, errBuf strings.Builder
	app := App{Stdout: &out, Stderr: &errBuf}
	if err := app.pondPeers(context.Background(), nil); err == nil {
		t.Fatalf("expected error when --pond is missing")
	}
}

func TestPondPeersCommandRejectsBadPort(t *testing.T) {
	var out, errBuf strings.Builder
	app := App{Stdout: &out, Stderr: &errBuf}
	if err := app.pondPeers(context.Background(), []string{"--pond", "demo", "--share-port", "70000"}); err == nil {
		t.Fatalf("expected error for out-of-range port")
	}
}

func TestPondConnectKongStripsCommandPath(t *testing.T) {
	withTempClaims(t, nil)
	t.Setenv("HOME", t.TempDir())
	var out, errBuf strings.Builder
	app := App{Stdout: &out, Stderr: &errBuf}
	if err := app.Run(context.Background(), []string{"pond", "connect", "alpha", "--export"}); err != nil {
		t.Fatalf("pond connect through Kong: %v", err)
	}
	if !strings.Contains(errBuf.String(), `pond "alpha" has no SSH-mesh-capable members`) {
		t.Fatalf("expected stripped pond name alpha, stdout=%q stderr=%q", out.String(), errBuf.String())
	}
	if strings.Contains(errBuf.String(), `pond "pond"`) {
		t.Fatalf("Kong command path leaked into pond name: %q", errBuf.String())
	}
}

func TestPondReleaseKongStripsCommandPath(t *testing.T) {
	withTempClaims(t, nil)
	var out, errBuf strings.Builder
	app := App{Stdout: &out, Stderr: &errBuf}
	if err := app.Run(context.Background(), []string{"pond", "release", "alpha"}); err != nil {
		t.Fatalf("pond release through Kong: %v", err)
	}
	if !strings.Contains(out.String(), `pond "alpha" has no active leases`) {
		t.Fatalf("expected stripped pond name alpha, stdout=%q stderr=%q", out.String(), errBuf.String())
	}
	if strings.Contains(out.String(), `pond "pond-release-alpha"`) {
		t.Fatalf("Kong command path leaked into pond name: %q", out.String())
	}
}

func TestFinalizePondReleaseClaimUsesProviderPolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		retain bool
	}{
		{name: "remove", retain: false},
		{name: "retain", retain: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claim := leaseClaim{LeaseID: "cbx_retained", Slug: "retained", Provider: "test", Pond: "demo", RepoRoot: "/repo"}
			withTempClaims(t, []leaseClaim{claim})
			lease := LeaseTarget{LeaseID: claim.LeaseID}

			if got := finalizePondReleaseClaim(pondReleaseRetentionBackend{retain: tc.retain}, lease, claim); got != tc.retain {
				t.Fatalf("retained=%v want %v", got, tc.retain)
			}
			claims, err := listLeaseClaims()
			if err != nil {
				t.Fatal(err)
			}
			if tc.retain && len(claims) != 1 {
				t.Fatalf("retained claim missing: %#v", claims)
			}
			if !tc.retain && len(claims) != 0 {
				t.Fatalf("released claim remains: %#v", claims)
			}
		})
	}
}

func TestProbeBridgePeersReachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	peers := []BridgePeer{{
		Slug:    "web",
		Targets: []BridgePeerTarget{{Port: 80, URL: srv.URL}},
	}, {
		Slug: "missing",
	}}
	results := ProbeBridgePeers(context.Background(), srv.Client(), peers, 2*time.Second)
	if len(results) != 2 {
		t.Fatalf("expected 2 probe results, got %d", len(results))
	}
	if results[0].State != "reachable" || results[0].StatusCode != 200 {
		t.Fatalf("first probe should be reachable+200: %#v", results[0])
	}
	if results[1].State != "no-targets" {
		t.Fatalf("second probe should be no-targets: %#v", results[1])
	}
}

func TestProbeBridgePeersUnreachable(t *testing.T) {
	peers := []BridgePeer{{
		Slug:    "broken",
		Targets: []BridgePeerTarget{{Port: 80, URL: "https://127.0.0.1:1/" + strings.Repeat("x", 4)}},
	}}
	results := ProbeBridgePeers(context.Background(), &http.Client{Timeout: 500 * time.Millisecond}, peers, 500*time.Millisecond)
	if results[0].State != "unreachable" {
		t.Fatalf("expected unreachable, got %#v", results[0])
	}
}

// TestPondPeersIncludesManagedLinuxWithTailnetTransport asserts that a
// managed-Linux peer with a recorded Tailscale IPv4 is surfaced into the
// unified pond listing with transport=tailnet and the tailnet IP as the
// endpoint — even when no delegated-provider bridge backend is configured
// (the resolver should never consult a URL adapter for hetzner peers).
func TestPondPeersIncludesManagedLinuxWithTailnetTransport(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "cbx_web", Slug: "web", Provider: "hetzner", Pond: "demo", RepoRoot: "/r"},
	})
	mutateClaim(t, "cbx_web", func(c *leaseClaim) { c.TailscaleIPv4 = "100.64.1.3" })
	prev := loadBridgeProviderFunc
	loadBridgeProviderFunc = func(provider string, _ Runtime) (BridgeProvider, error) {
		t.Fatalf("bridge provider lookup must not be invoked for tailnet peers; got provider=%q", provider)
		return nil, nil
	}
	t.Cleanup(func() { loadBridgeProviderFunc = prev })

	peers, err := resolvePondPeers(context.Background(), Runtime{}, "demo", "", pondPeersFlags{})
	if err != nil {
		t.Fatalf("resolvePondPeers: %v", err)
	}
	if len(peers) != 1 {
		t.Fatalf("expected 1 peer, got %d", len(peers))
	}
	if peers[0].Transport != TransportTailnet {
		t.Fatalf("transport=%q want tailnet", peers[0].Transport)
	}
	if peers[0].Endpoint != "100.64.1.3" {
		t.Fatalf("endpoint=%q want 100.64.1.3", peers[0].Endpoint)
	}
}

// TestPondPeersMixesIsloBridgeAndTailnetMembers proves the headline pond
// promise: a single pond can hold an Islo URL-bridge member and a Tailscale
// mesh member at the same time, and one `pond peers` fan-out renders both with
// the correct plane each — the Islo member as a URL-bridge peer (its share URL
// resolved through the bridge adapter) and the Hetzner member as a tailnet peer
// (its 100.x IPv4, with the bridge adapter never consulted for it). This is the
// "does islo as a pond work with tailscale" question stated as a regression.
func TestPondPeersMixesIsloBridgeAndTailnetMembers(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "isb_islo1", Slug: "sandbox", Provider: "islo", Pond: "demo", RepoRoot: "/r"},
		{LeaseID: "cbx_web", Slug: "web", Provider: "hetzner", Pond: "demo", RepoRoot: "/r"},
	})
	mutateClaim(t, "cbx_web", func(c *leaseClaim) { c.TailscaleIPv4 = "100.64.1.3" })

	fake := &fakeBridgeProvider{
		listed: map[string][]BridgePeerTarget{
			"isb_islo1": {{Port: 8080, URL: "https://sandbox.share.islo.dev", ShareID: "shr_islo"}},
		},
	}
	prev := loadBridgeProviderFunc
	loadBridgeProviderFunc = func(provider string, _ Runtime) (BridgeProvider, error) {
		if provider != "islo" {
			// The tailnet member must never reach the bridge adapter.
			t.Fatalf("bridge adapter consulted for non-islo provider %q", provider)
		}
		return fake, nil
	}
	t.Cleanup(func() { loadBridgeProviderFunc = prev })

	peers, err := resolvePondPeers(context.Background(), Runtime{}, "demo", "", pondPeersFlags{})
	if err != nil {
		t.Fatalf("resolvePondPeers: %v", err)
	}
	if len(peers) != 2 {
		t.Fatalf("expected 2 peers (islo + hetzner), got %d: %#v", len(peers), peers)
	}
	byProvider := map[string]BridgePeer{}
	for _, p := range peers {
		byProvider[p.Provider] = p
	}

	islo, ok := byProvider["islo"]
	if !ok {
		t.Fatalf("islo member missing: %#v", peers)
	}
	if islo.Transport != TransportURL {
		t.Fatalf("islo transport=%q want %q", islo.Transport, TransportURL)
	}
	if len(islo.Targets) != 1 || islo.Targets[0].URL != "https://sandbox.share.islo.dev" {
		t.Fatalf("islo member should carry its share URL, got %#v", islo.Targets)
	}

	hetzner, ok := byProvider["hetzner"]
	if !ok {
		t.Fatalf("hetzner member missing: %#v", peers)
	}
	if hetzner.Transport != TransportTailnet {
		t.Fatalf("hetzner transport=%q want %q", hetzner.Transport, TransportTailnet)
	}
	if hetzner.Endpoint != "100.64.1.3" {
		t.Fatalf("hetzner endpoint=%q want 100.64.1.3", hetzner.Endpoint)
	}
}

// TestPondPeersIsloKeepsURLPrimaryForOutboundTailnet proves that Islo's
// userspace Tailscale capability does not advertise an inbound peer endpoint.
func TestPondPeersIsloKeepsURLPrimaryForOutboundTailnet(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "isb_meshed", Slug: "node-a", Provider: "islo", Pond: "demo", RepoRoot: "/r"},
		{LeaseID: "isb_plain", Slug: "node-b", Provider: "islo", Pond: "demo", RepoRoot: "/r"},
	})
	mutateClaim(t, "isb_meshed", func(c *leaseClaim) { c.TailscaleIPv4 = "100.64.7.7" })

	fake := &fakeBridgeProvider{listed: map[string][]BridgePeerTarget{
		"isb_meshed": {{Port: 8080, URL: "https://node-a.share.islo.dev"}},
		"isb_plain":  {{Port: 8080, URL: "https://node-b.share.islo.dev"}},
	}}
	prev := loadBridgeProviderFunc
	loadBridgeProviderFunc = func(provider string, _ Runtime) (BridgeProvider, error) {
		if provider != "islo" {
			t.Fatalf("unexpected provider %q", provider)
		}
		return fake, nil
	}
	t.Cleanup(func() { loadBridgeProviderFunc = prev })

	peers, err := resolvePondPeers(context.Background(), Runtime{}, "demo", "", pondPeersFlags{})
	if err != nil {
		t.Fatalf("resolvePondPeers: %v", err)
	}
	by := map[string]BridgePeer{}
	for _, p := range peers {
		by[p.Slug] = p
	}
	if got := by["node-a"]; got.Transport != TransportURL || got.Endpoint != "https://node-a.share.islo.dev" {
		t.Fatalf("joined islo member should retain URL primary transport, got %#v", got)
	}
	if got := by["node-b"]; got.Transport != TransportURL || got.Endpoint != "https://node-b.share.islo.dev" {
		t.Fatalf("plain islo member should fall back to url, got transport=%q endpoint=%q", got.Transport, got.Endpoint)
	}
	if got := by["node-a"]; len(got.Transports) != 1 || got.Transports[0] != TransportURL || !strings.Contains(got.Note, "outbound proxy") {
		t.Fatalf("islo should advertise only its dialable URL transport: %#v", got)
	}
}

// TestPondPeersIncludesSSHLeaseWithSSHTransport asserts that SSH-lease
// providers (RunPod here) surface their endpoint as `ssh://host:port` so
// downstream tooling can dial it without provider-specific knowledge.
func TestPondPeersIncludesSSHLeaseWithSSHTransport(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "rp_db", Slug: "db", Provider: "runpod", Pond: "demo", RepoRoot: "/r"},
	})
	mutateClaim(t, "rp_db", func(c *leaseClaim) {
		c.SSHHost = "1.2.3.4"
		c.SSHPort = 2200
	})
	prev := loadBridgeProviderFunc
	loadBridgeProviderFunc = func(string, Runtime) (BridgeProvider, error) {
		t.Fatalf("bridge provider lookup must not be invoked for ssh peers")
		return nil, nil
	}
	t.Cleanup(func() { loadBridgeProviderFunc = prev })

	peers, err := resolvePondPeers(context.Background(), Runtime{}, "demo", "", pondPeersFlags{})
	if err != nil {
		t.Fatalf("resolvePondPeers: %v", err)
	}
	if peers[0].Transport != TransportSSH {
		t.Fatalf("transport=%q want ssh", peers[0].Transport)
	}
	if peers[0].Endpoint != "ssh://1.2.3.4:2200" {
		t.Fatalf("endpoint=%q want ssh://1.2.3.4:2200", peers[0].Endpoint)
	}
}

// TestPondPeersHandlesPendingTailscaleIP covers the case where the lease
// is tagged for a tailnet-capable provider but the IP has not been
// recorded yet (race between provision and tailnet join). The peer should
// be reported as transport=pending with an honest note rather than being
// dropped or pretending the endpoint exists.
func TestPondPeersHandlesPendingTailscaleIP(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "cbx_pend", Slug: "pend", Provider: "aws", Pond: "demo", RepoRoot: "/r"},
	})
	prev := loadBridgeProviderFunc
	loadBridgeProviderFunc = func(string, Runtime) (BridgeProvider, error) { return nil, nil }
	t.Cleanup(func() { loadBridgeProviderFunc = prev })

	peers, err := resolvePondPeers(context.Background(), Runtime{}, "demo", "", pondPeersFlags{})
	if err != nil {
		t.Fatalf("resolvePondPeers: %v", err)
	}
	if peers[0].Transport != TransportPending {
		t.Fatalf("transport=%q want pending", peers[0].Transport)
	}
	if peers[0].Endpoint != "" {
		t.Fatalf("pending peer must not report a fake endpoint; got %q", peers[0].Endpoint)
	}
	if peers[0].Note == "" {
		t.Fatalf("pending peer should carry an honest note")
	}
}

// TestPondPeersHandlesBlacksmithAsNone asserts that Blacksmith — the
// provider that owns its own connectivity outside Crabbox's planes — is
// surfaced as transport=none with the exact note documented in the public
// API so client tooling can detect it.
func TestPondPeersHandlesBlacksmithAsNone(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "bs_what", Slug: "what", Provider: "blacksmith", Pond: "demo", RepoRoot: "/r"},
	})
	prev := loadBridgeProviderFunc
	loadBridgeProviderFunc = func(string, Runtime) (BridgeProvider, error) { return nil, nil }
	t.Cleanup(func() { loadBridgeProviderFunc = prev })

	peers, err := resolvePondPeers(context.Background(), Runtime{}, "demo", "", pondPeersFlags{})
	if err != nil {
		t.Fatalf("resolvePondPeers: %v", err)
	}
	if peers[0].Transport != TransportNone {
		t.Fatalf("transport=%q want none", peers[0].Transport)
	}
	if peers[0].Note != "blacksmith owns connectivity" {
		t.Fatalf("note=%q want \"blacksmith owns connectivity\"", peers[0].Note)
	}
	if peers[0].Endpoint != "" {
		t.Fatalf("blacksmith peer must not report an endpoint; got %q", peers[0].Endpoint)
	}
}

// TestDoctorPondReachabilityMatrixAsymmetric pins the asymmetry that
// `crabbox doctor --pond` reports between the transport planes. The
// matrix must not pretend `url -> tailnet` is reachable, and it must
// flag `* -> ssh` and `ssh -> *` as warnings (operator-side bridge
// required) rather than ok.
func TestDoctorPondReachabilityMatrixAsymmetric(t *testing.T) {
	peers := []BridgePeer{
		{Slug: "web", Provider: "hetzner", Transport: TransportTailnet, Endpoint: "100.64.1.3"},
		{Slug: "api", Provider: "islo", Transport: TransportURL, Endpoint: "https://api.share.islo.dev"},
		{Slug: "db", Provider: "runpod", Transport: TransportSSH, Endpoint: "ssh://1.2.3.4:22"},
		{Slug: "what", Provider: "blacksmith", Transport: TransportNone, Note: "blacksmith owns connectivity"},
	}
	matrix := buildPondReachabilityMatrix("alpha", peers)
	if got, want := len(matrix.Transports), 4; got != want {
		t.Fatalf("transports=%d want %d (%v)", got, want, matrix.Transports)
	}
	cellState := func(from, to string) string {
		for _, cell := range matrix.Cells {
			if cell.From == from && cell.To == to {
				return cell.State
			}
		}
		t.Fatalf("cell %s -> %s missing", from, to)
		return ""
	}
	if got := cellState(TransportURL, TransportTailnet); got != reachNo {
		t.Fatalf("url -> tailnet should be NO (no public endpoint on tailnet), got %q", got)
	}
	if got := cellState(TransportTailnet, TransportURL); got != reachOK {
		t.Fatalf("tailnet -> url should be OK (outbound HTTPS), got %q", got)
	}
	if got := cellState(TransportTailnet, TransportSSH); got != reachWarn {
		t.Fatalf("tailnet -> ssh should be WARN (operator-side bridge), got %q", got)
	}
	if got := cellState(TransportSSH, TransportSSH); got != reachWarn {
		t.Fatalf("ssh -> ssh should be WARN (no shared mesh), got %q", got)
	}
	if got := cellState(TransportSSH, TransportURL); got != reachOK {
		t.Fatalf("ssh -> url should be OK (outbound HTTPS), got %q", got)
	}
	if got := cellState(TransportNone, TransportTailnet); got != reachNo {
		t.Fatalf("none -> tailnet should be NO (provider owns its own connectivity), got %q", got)
	}
	for _, want := range []string{TransportTailnet, TransportURL, TransportSSH, TransportNone} {
		found := false
		for _, got := range matrix.Transports {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("matrix transports missing %q (have %v)", want, matrix.Transports)
		}
	}
	if matrix.Breakdown[TransportTailnet] != 1 || matrix.Breakdown[TransportURL] != 1 || matrix.Breakdown[TransportSSH] != 1 || matrix.Breakdown[TransportNone] != 1 {
		t.Fatalf("unexpected breakdown: %#v", matrix.Breakdown)
	}
}

// TestRenderPondReachabilityMatrixIncludesAsymmetricNotes checks the
// human renderer surfaces the per-cell notes verbatim so reviewers can
// audit the claims without parsing JSON.
func TestRenderPondReachabilityMatrixIncludesAsymmetricNotes(t *testing.T) {
	peers := []BridgePeer{
		{Slug: "web", Provider: "hetzner", Transport: TransportTailnet, Endpoint: "100.64.1.3"},
		{Slug: "api", Provider: "islo", Transport: TransportURL, Endpoint: "https://x"},
	}
	matrix := buildPondReachabilityMatrix("alpha", peers)
	var buf strings.Builder
	renderPondReachabilityMatrix(&buf, matrix)
	out := buf.String()
	if !strings.Contains(out, "tailnet -> url") {
		t.Fatalf("renderer should label rows by transport pair, got:\n%s", out)
	}
	if !strings.Contains(out, "no public endpoint on tailnet members") {
		t.Fatalf("renderer should surface the url -> tailnet asymmetry note, got:\n%s", out)
	}
}

// mutateClaim is a helper for tests that need to overlay endpoint
// metadata onto a seeded lease claim. It re-reads the claim sidecar
// produced by withTempClaims, applies the mutation, and writes it back
// through the same path the production claim writer uses so JSON-shape
// changes stay covered by the on-disk format.
func mutateClaim(t *testing.T, leaseID string, fn func(*leaseClaim)) {
	t.Helper()
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatalf("readLeaseClaim %s: %v", leaseID, err)
	}
	if claim.LeaseID == "" {
		t.Fatalf("no claim seeded for lease %s", leaseID)
	}
	fn(&claim)
	path, err := leaseClaimPath(leaseID)
	if err != nil {
		t.Fatalf("leaseClaimPath %s: %v", leaseID, err)
	}
	data, err := json.MarshalIndent(claim, "", "  ")
	if err != nil {
		t.Fatalf("marshal claim %s: %v", leaseID, err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatalf("write claim %s: %v", leaseID, err)
	}
}
