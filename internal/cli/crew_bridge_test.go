package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestFilterClaimsForCrew(t *testing.T) {
	claims := []leaseClaim{
		{LeaseID: "isb_1", Slug: "web", Provider: "islo", Crew: "alpha"},
		{LeaseID: "isb_2", Slug: "client", Provider: "islo", Crew: "Alpha"},
		{LeaseID: "cbx_3", Slug: "db", Provider: "hetzner", Crew: "alpha"},
		{LeaseID: "isb_4", Slug: "other", Provider: "islo", Crew: "bravo"},
		{LeaseID: "isb_5", Slug: "noCrew", Provider: "islo"},
	}
	out := filterClaimsForCrew(claims, "alpha", "islo")
	if len(out) != 2 {
		t.Fatalf("expected 2 islo+alpha claims, got %d: %#v", len(out), out)
	}
	if out[0].LeaseID != "isb_1" || out[1].LeaseID != "isb_2" {
		t.Fatalf("unexpected order: %#v", out)
	}
	if all := filterClaimsForCrew(claims, "alpha", ""); len(all) != 3 {
		t.Fatalf("expected 3 alpha claims across providers, got %d", len(all))
	}
	if empty := filterClaimsForCrew(claims, "", "islo"); len(empty) != 0 {
		t.Fatalf("expected 0 claims for empty crew, got %d", len(empty))
	}
	if none := filterClaimsForCrew(claims, "charlie", "islo"); len(none) != 0 {
		t.Fatalf("expected 0 matches for crew=charlie, got %d", len(none))
	}
}

type fakeBridgeProvider struct {
	listed    map[string][]BridgePeerTarget
	published map[string]BridgePeerTarget
	listErr   error
	pubErr    error
	calls     int
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
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listed[leaseID], nil
}

func withTempClaims(t *testing.T, claims []leaseClaim) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	for _, claim := range claims {
		if err := claimLeaseForRepoProviderScopeCrew(claim.LeaseID, claim.Slug, claim.Provider, claim.ProviderScope, claim.Crew, claim.RepoRoot, 30*time.Minute, false); err != nil {
			t.Fatalf("seed claim %s: %v", claim.LeaseID, err)
		}
	}
}

func TestResolveCrewPeersListsTargets(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "isb_web", Slug: "bridge-web", Provider: "islo", Crew: "demo", RepoRoot: "/r"},
		{LeaseID: "isb_client", Slug: "bridge-client", Provider: "islo", Crew: "demo", RepoRoot: "/r"},
		{LeaseID: "isb_other", Slug: "noise", Provider: "islo", Crew: "other", RepoRoot: "/r"},
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

	peers, err := resolveCrewPeers(context.Background(), Runtime{}, "demo", "islo", crewPeersFlags{})
	if err != nil {
		t.Fatalf("resolveCrewPeers: %v", err)
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
	if peers[0].Crew != "demo" || peers[1].Crew != "demo" {
		t.Fatalf("peers should carry crew=demo, got %#v", peers)
	}
}

func TestResolveCrewPeersPublishesShare(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "isb_w", Slug: "w", Provider: "islo", Crew: "demo", RepoRoot: "/r"},
	})
	fake := &fakeBridgeProvider{
		published: map[string]BridgePeerTarget{
			"isb_w": {URL: "https://abc.share.islo.dev", ShareID: "shr_w"},
		},
	}
	prev := loadBridgeProviderFunc
	loadBridgeProviderFunc = func(string, Runtime) (BridgeProvider, error) { return fake, nil }
	t.Cleanup(func() { loadBridgeProviderFunc = prev })

	peers, err := resolveCrewPeers(context.Background(), Runtime{}, "demo", "islo", crewPeersFlags{SharePort: 8080, ShareTTL: time.Hour})
	if err != nil {
		t.Fatalf("resolveCrewPeers: %v", err)
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

func TestResolveCrewPeersUnknownProvider(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "isb_w", Slug: "w", Provider: "islo", Crew: "demo", RepoRoot: "/r"},
	})
	prev := loadBridgeProviderFunc
	loadBridgeProviderFunc = func(string, Runtime) (BridgeProvider, error) {
		return nil, nil // backend exists but does not implement BridgeProvider
	}
	t.Cleanup(func() { loadBridgeProviderFunc = prev })
	peers, err := resolveCrewPeers(context.Background(), Runtime{}, "demo", "islo", crewPeersFlags{})
	if err != nil {
		t.Fatalf("resolveCrewPeers: %v", err)
	}
	if len(peers) != 1 || len(peers[0].Targets) != 0 {
		t.Fatalf("expected 1 peer with no targets, got %#v", peers)
	}
	if peers[0].BridgeState != "unsupported-provider" {
		t.Fatalf("expected BridgeState=unsupported-provider for backend without BridgeProvider, got %q", peers[0].BridgeState)
	}
}

func TestResolveCrewPeersExplicitlyUnsupportedAdapter(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "cbx_modal", Slug: "fn", Provider: "modal", Crew: "demo", RepoRoot: "/r"},
	})
	fake := &fakeBridgeProvider{listErr: ErrBridgeNotImplemented}
	prev := loadBridgeProviderFunc
	loadBridgeProviderFunc = func(string, Runtime) (BridgeProvider, error) { return fake, nil }
	t.Cleanup(func() { loadBridgeProviderFunc = prev })

	peers, err := resolveCrewPeers(context.Background(), Runtime{}, "demo", "modal", crewPeersFlags{})
	if err != nil {
		t.Fatalf("resolveCrewPeers: %v", err)
	}
	if len(peers) != 1 || peers[0].BridgeState != "unsupported" {
		t.Fatalf("expected ErrBridgeNotImplemented to surface as BridgeState=unsupported, got %#v", peers)
	}
	if len(peers[0].Targets) != 0 {
		t.Fatalf("expected no targets when adapter reports unsupported, got %#v", peers[0].Targets)
	}
}

func TestResolveCrewPeersExplicitlyUnsupportedPublish(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "cbx_cf", Slug: "edge", Provider: "cloudflare", Crew: "demo", RepoRoot: "/r"},
	})
	fake := &fakeBridgeProvider{pubErr: ErrBridgeNotImplemented}
	prev := loadBridgeProviderFunc
	loadBridgeProviderFunc = func(string, Runtime) (BridgeProvider, error) { return fake, nil }
	t.Cleanup(func() { loadBridgeProviderFunc = prev })

	peers, err := resolveCrewPeers(context.Background(), Runtime{}, "demo", "cloudflare", crewPeersFlags{SharePort: 8080, ShareTTL: time.Hour})
	if err != nil {
		t.Fatalf("resolveCrewPeers: %v", err)
	}
	if len(peers) != 1 || peers[0].BridgeState != "unsupported" {
		t.Fatalf("expected publish unsupported to surface as BridgeState=unsupported, got %#v", peers)
	}
}

func TestResolveCrewPeersMultiProviderFanOut(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "isb_islo1", Slug: "islo-a", Provider: "islo", Crew: "demo", RepoRoot: "/r"},
		{LeaseID: "cbx_e2b1", Slug: "e2b-a", Provider: "e2b", Crew: "demo", RepoRoot: "/r"},
		{LeaseID: "cbx_modal1", Slug: "modal-a", Provider: "modal", Crew: "demo", RepoRoot: "/r"},
		{LeaseID: "isb_other", Slug: "noise", Provider: "islo", Crew: "other", RepoRoot: "/r"},
	})
	// Each provider key maps to a distinct adapter so we can assert that
	// resolveCrewPeers picks the right backend per provider rather than
	// applying one backend uniformly.
	adapters := map[string]*fakeBridgeProvider{
		"islo":  {listed: map[string][]BridgePeerTarget{"isb_islo1": {{Port: 80, URL: "https://islo-a.share.islo.dev"}}}},
		"e2b":   {listed: map[string][]BridgePeerTarget{"cbx_e2b1": {{Port: 8080, URL: "https://8080-sbx.e2b.app"}}}},
		"modal": {listErr: ErrBridgeNotImplemented},
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
	peers, err := resolveCrewPeers(context.Background(), Runtime{}, "demo", "", crewPeersFlags{})
	if err != nil {
		t.Fatalf("resolveCrewPeers: %v", err)
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
	if state := byProvider["modal"].BridgeState; state != "unsupported" {
		t.Fatalf("modal peer should report unsupported, got %q", state)
	}
}

func TestResolveCrewPeersListError(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "isb_w", Slug: "w", Provider: "islo", Crew: "demo", RepoRoot: "/r"},
	})
	fake := &fakeBridgeProvider{listErr: errors.New("api down")}
	prev := loadBridgeProviderFunc
	loadBridgeProviderFunc = func(string, Runtime) (BridgeProvider, error) { return fake, nil }
	t.Cleanup(func() { loadBridgeProviderFunc = prev })
	if _, err := resolveCrewPeers(context.Background(), Runtime{}, "demo", "islo", crewPeersFlags{}); err == nil {
		t.Fatalf("expected ListPeerTargets error to surface")
	}
}

func TestCrewPeersCommandRendersJSON(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "isb_w", Slug: "web", Provider: "islo", Crew: "demo", RepoRoot: "/r"},
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
	if err := app.crewPeers(context.Background(), []string{"--crew", "demo", "--json"}); err != nil {
		t.Fatalf("crewPeers: %v", err)
	}
	var payload crewPeersJSON
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

func TestCrewPeersCommandRequiresCrew(t *testing.T) {
	var out, errBuf strings.Builder
	app := App{Stdout: &out, Stderr: &errBuf}
	if err := app.crewPeers(context.Background(), nil); err == nil {
		t.Fatalf("expected error when --crew is missing")
	}
}

func TestCrewPeersCommandRejectsBadPort(t *testing.T) {
	var out, errBuf strings.Builder
	app := App{Stdout: &out, Stderr: &errBuf}
	if err := app.crewPeers(context.Background(), []string{"--crew", "demo", "--share-port", "70000"}); err == nil {
		t.Fatalf("expected error for out-of-range port")
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

// TestCrewPeersIncludesManagedLinuxWithTailnetTransport asserts that a
// managed-Linux peer with a recorded Tailscale IPv4 is surfaced into the
// unified crew listing with transport=tailnet and the tailnet IP as the
// endpoint — even when no delegated-provider bridge backend is configured
// (the resolver should never consult a URL adapter for hetzner peers).
func TestCrewPeersIncludesManagedLinuxWithTailnetTransport(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "cbx_web", Slug: "web", Provider: "hetzner", Crew: "demo", RepoRoot: "/r"},
	})
	mutateClaim(t, "cbx_web", func(c *leaseClaim) { c.TailscaleIPv4 = "100.64.1.3" })
	prev := loadBridgeProviderFunc
	loadBridgeProviderFunc = func(provider string, _ Runtime) (BridgeProvider, error) {
		t.Fatalf("bridge provider lookup must not be invoked for tailnet peers; got provider=%q", provider)
		return nil, nil
	}
	t.Cleanup(func() { loadBridgeProviderFunc = prev })

	peers, err := resolveCrewPeers(context.Background(), Runtime{}, "demo", "", crewPeersFlags{})
	if err != nil {
		t.Fatalf("resolveCrewPeers: %v", err)
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

// TestCrewPeersIncludesSSHLeaseWithSSHTransport asserts that SSH-lease
// providers (RunPod here) surface their endpoint as `ssh://host:port` so
// downstream tooling can dial it without provider-specific knowledge.
func TestCrewPeersIncludesSSHLeaseWithSSHTransport(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "rp_db", Slug: "db", Provider: "runpod", Crew: "demo", RepoRoot: "/r"},
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

	peers, err := resolveCrewPeers(context.Background(), Runtime{}, "demo", "", crewPeersFlags{})
	if err != nil {
		t.Fatalf("resolveCrewPeers: %v", err)
	}
	if peers[0].Transport != TransportSSH {
		t.Fatalf("transport=%q want ssh", peers[0].Transport)
	}
	if peers[0].Endpoint != "ssh://1.2.3.4:2200" {
		t.Fatalf("endpoint=%q want ssh://1.2.3.4:2200", peers[0].Endpoint)
	}
}

// TestCrewPeersHandlesPendingTailscaleIP covers the case where the lease
// is tagged for a tailnet-capable provider but the IP has not been
// recorded yet (race between provision and tailnet join). The peer should
// be reported as transport=pending with an honest note rather than being
// dropped or pretending the endpoint exists.
func TestCrewPeersHandlesPendingTailscaleIP(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "cbx_pend", Slug: "pend", Provider: "aws", Crew: "demo", RepoRoot: "/r"},
	})
	prev := loadBridgeProviderFunc
	loadBridgeProviderFunc = func(string, Runtime) (BridgeProvider, error) { return nil, nil }
	t.Cleanup(func() { loadBridgeProviderFunc = prev })

	peers, err := resolveCrewPeers(context.Background(), Runtime{}, "demo", "", crewPeersFlags{})
	if err != nil {
		t.Fatalf("resolveCrewPeers: %v", err)
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

// TestCrewPeersHandlesBlacksmithAsNone asserts that Blacksmith — the
// provider that owns its own connectivity outside Crabbox's planes — is
// surfaced as transport=none with the exact note documented in the public
// API so client tooling can detect it.
func TestCrewPeersHandlesBlacksmithAsNone(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "bs_what", Slug: "what", Provider: "blacksmith", Crew: "demo", RepoRoot: "/r"},
	})
	prev := loadBridgeProviderFunc
	loadBridgeProviderFunc = func(string, Runtime) (BridgeProvider, error) { return nil, nil }
	t.Cleanup(func() { loadBridgeProviderFunc = prev })

	peers, err := resolveCrewPeers(context.Background(), Runtime{}, "demo", "", crewPeersFlags{})
	if err != nil {
		t.Fatalf("resolveCrewPeers: %v", err)
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

// TestDoctorCrewReachabilityMatrixAsymmetric pins the asymmetry that
// `crabbox doctor --crew` reports between the transport planes. The
// matrix must not pretend `url -> tailnet` is reachable, and it must
// flag `* -> ssh` and `ssh -> *` as warnings (operator-side bridge
// required) rather than ok.
func TestDoctorCrewReachabilityMatrixAsymmetric(t *testing.T) {
	peers := []BridgePeer{
		{Slug: "web", Provider: "hetzner", Transport: TransportTailnet, Endpoint: "100.64.1.3"},
		{Slug: "api", Provider: "islo", Transport: TransportURL, Endpoint: "https://api.share.islo.dev"},
		{Slug: "db", Provider: "runpod", Transport: TransportSSH, Endpoint: "ssh://1.2.3.4:22"},
		{Slug: "what", Provider: "blacksmith", Transport: TransportNone, Note: "blacksmith owns connectivity"},
	}
	matrix := buildCrewReachabilityMatrix("alpha", peers)
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

// TestRenderCrewReachabilityMatrixIncludesAsymmetricNotes checks the
// human renderer surfaces the per-cell notes verbatim so reviewers can
// audit the claims without parsing JSON.
func TestRenderCrewReachabilityMatrixIncludesAsymmetricNotes(t *testing.T) {
	peers := []BridgePeer{
		{Slug: "web", Provider: "hetzner", Transport: TransportTailnet, Endpoint: "100.64.1.3"},
		{Slug: "api", Provider: "islo", Transport: TransportURL, Endpoint: "https://x"},
	}
	matrix := buildCrewReachabilityMatrix("alpha", peers)
	var buf strings.Builder
	renderCrewReachabilityMatrix(&buf, matrix)
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
