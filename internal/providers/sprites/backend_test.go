package sprites

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestSpritesSSHTargetUsesSpriteProxy(t *testing.T) {
	target := spritesSSHTarget("crabbox-blue-lobster-12345678", "/tmp/key")
	if target.User != "sprite" || target.Host != "crabbox-blue-lobster-12345678" || target.Port != "22" {
		t.Fatalf("target=%#v", target)
	}
	if !target.SSHConfigProxy || target.ProxyCommand != "sprite proxy -s %h -W 22" {
		t.Fatalf("proxy target=%#v", target)
	}
}

func TestSpritesLabelsRoundTripLeaseAndSlug(t *testing.T) {
	labels := spritesAPILabels("cbx_abcdef123456", "blue-lobster")
	sprite := spritesInfo{Name: "crabbox-blue-lobster-12345678", Labels: labels}
	if !isCrabboxSprite(sprite) {
		t.Fatal("expected crabbox sprite")
	}
	if got := spritesLeaseID(sprite); got != "cbx_abcdef123456" {
		t.Fatalf("lease=%q", got)
	}
	if got := spritesSlug("cbx_abcdef123456", sprite); got != "blue-lobster" {
		t.Fatalf("slug=%q", got)
	}
}

func TestCrabboxSpriteOwnershipRequiresLabels(t *testing.T) {
	sprite := spritesInfo{Name: "crabbox-handmade"}
	if isCrabboxSprite(sprite) {
		t.Fatal("prefix-only sprite should not be treated as Crabbox-owned")
	}
	if !isLegacyCrabboxSpriteName(sprite) {
		t.Fatal("expected legacy Crabbox name recognition")
	}
}

func TestCleanSpritesWorkRootRejectsBroadPaths(t *testing.T) {
	for _, path := range []string{"/", "/home", "/home/sprite", "/tmp", "relative"} {
		if err := cleanSpritesWorkRoot(path); err == nil {
			t.Fatalf("expected %q to be rejected", path)
		}
	}
	if err := cleanSpritesWorkRoot("/home/sprite/crabbox"); err != nil {
		t.Fatalf("work root rejected: %v", err)
	}
}

func TestResolveSpriteNameAcceptsSprPrefix(t *testing.T) {
	backend := &spritesBackend{client: &fakeSpritesAPI{
		get: spritesInfo{Name: "crabbox-blue-lobster-12345678", Labels: spritesAPILabels("cbx_abcdef123456", "blue-lobster")},
	}}
	name, leaseID, slug, err := backend.resolveSpriteName(context.Background(), "spr_crabbox-blue-lobster-12345678", false)
	if err != nil {
		t.Fatal(err)
	}
	if name != "crabbox-blue-lobster-12345678" || leaseID != "cbx_abcdef123456" || slug != "blue-lobster" {
		t.Fatalf("name=%q lease=%q slug=%q", name, leaseID, slug)
	}
}

func TestResolveSpriteNameRejectsPrefixOnlyWithoutReclaim(t *testing.T) {
	backend := &spritesBackend{client: &fakeSpritesAPI{
		get: spritesInfo{Name: "crabbox-handmade"},
	}}
	_, _, _, err := backend.resolveSpriteName(context.Background(), "crabbox-handmade", false)
	if err == nil || !strings.Contains(err.Error(), "has no Crabbox labels") {
		t.Fatalf("err=%v, want prefix-only reclaim error", err)
	}
}

func TestResolveSpriteNameAcceptsPrefixOnlyWithReclaim(t *testing.T) {
	backend := &spritesBackend{client: &fakeSpritesAPI{
		get: spritesInfo{Name: "crabbox-handmade"},
	}}
	name, leaseID, _, err := backend.resolveSpriteName(context.Background(), "crabbox-handmade", true)
	if err != nil {
		t.Fatal(err)
	}
	if name != "crabbox-handmade" || leaseID != "spr_crabbox-handmade" {
		t.Fatalf("name=%q lease=%q", name, leaseID)
	}
}

func TestResolveSpriteNameUsesAdoptedSpriteNameFromClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := claimLeaseForRepoProvider("spr_handmade-sprite", "adopted", spritesProvider, t.TempDir(), 0, true); err != nil {
		t.Fatal(err)
	}
	backend := &spritesBackend{client: &fakeSpritesAPI{}}
	name, leaseID, slug, err := backend.resolveSpriteName(context.Background(), "adopted", false)
	if err != nil {
		t.Fatal(err)
	}
	if name != "handmade-sprite" || leaseID != "spr_handmade-sprite" || slug != "adopted" {
		t.Fatalf("name=%q lease=%q slug=%q", name, leaseID, slug)
	}
}

func TestResolveSpriteNameAcceptsProviderlessCrabboxClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := claimLeaseForRepoProvider("cbx_abcdef123456", "blue-lobster", "", t.TempDir(), 0, true); err != nil {
		t.Fatal(err)
	}
	backend := &spritesBackend{client: &fakeSpritesAPI{}}
	name, leaseID, slug, err := backend.resolveSpriteName(context.Background(), "blue-lobster", false)
	if err != nil {
		t.Fatal(err)
	}
	if name != "crabbox-blue-lobster-c80c2195" || leaseID != "cbx_abcdef123456" || slug != "blue-lobster" {
		t.Fatalf("name=%q lease=%q slug=%q", name, leaseID, slug)
	}
}

func TestResolveSpriteNameRejectsOtherProviderClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := claimLeaseForRepoProvider("cbx_abcdef123456", "blue-lobster", "aws", t.TempDir(), 0, true); err != nil {
		t.Fatal(err)
	}
	backend := &spritesBackend{client: &fakeSpritesAPI{}}
	_, _, _, err := backend.resolveSpriteName(context.Background(), "blue-lobster", false)
	if err == nil || !strings.Contains(err.Error(), "provider=aws") {
		t.Fatalf("err=%v", err)
	}
}

func TestResolveReleaseOnlySkipsSpriteCLIAndBootstrap(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateDir)
	repoRoot := t.TempDir()
	if err := claimLeaseForRepoProvider("spr_unhealthy-sprite", "unhealthy", spritesProvider, repoRoot, 0, true); err != nil {
		t.Fatal(err)
	}
	api := &fakeSpritesAPI{}
	runner := &recordingRunner{}
	backend := &spritesBackend{
		cfg:    Config{Sprites: SpritesConfig{WorkRoot: "/home/sprite/crabbox"}},
		rt:     Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner},
		client: api,
	}
	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: "unhealthy", ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("release-only resolve should not call sprite CLI: %#v", runner.calls)
	}
	if lease.LeaseID != "spr_unhealthy-sprite" || lease.Server.Name != "unhealthy-sprite" {
		t.Fatalf("lease=%#v", lease)
	}
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease, Force: true}); err != nil {
		t.Fatal(err)
	}
	if api.deleted != "unhealthy-sprite" {
		t.Fatalf("deleted=%q", api.deleted)
	}
	if _, ok, err := resolveLeaseClaim("unhealthy"); err != nil || ok {
		t.Fatalf("claim still resolves ok=%t err=%v", ok, err)
	}
}

func TestReleaseLeaseRejectsUnclaimedPrefixOnlySprite(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeSpritesAPI{get: spritesInfo{Name: "crabbox-handmade"}}
	backend := &spritesBackend{client: api}
	err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{
		Lease: LeaseTarget{
			LeaseID: "spr_crabbox-handmade",
			Server:  Server{Name: "crabbox-handmade"},
		},
		Force: true,
	})
	if err == nil || !strings.Contains(err.Error(), "not Crabbox-managed") {
		t.Fatalf("ReleaseLease err=%v, want unmanaged sprite error", err)
	}
	if api.deleted != "" {
		t.Fatalf("deleted prefix-only sprite %q", api.deleted)
	}
}

func TestAcquireKeepFailurePreservesRetainedSpriteKey(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeSpritesAPI{}
	runner := &recordingRunner{failContains: "exec -s", err: errors.New("bootstrap failed")}
	backend := &spritesBackend{
		cfg:    Config{Sprites: SpritesConfig{WorkRoot: "/home/sprite/crabbox"}},
		rt:     Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner},
		client: api,
	}

	_, err := backend.Acquire(context.Background(), AcquireRequest{Keep: true, Repo: core.Repo{Root: t.TempDir()}})
	if err == nil || !strings.Contains(err.Error(), "bootstrap failed") {
		t.Fatalf("Acquire err=%v, want bootstrap failure", err)
	}
	if api.deleted != "" {
		t.Fatalf("kept failed acquire should not delete sprite, deleted=%q", api.deleted)
	}
	leaseID := spritesLeaseID(spritesInfo{Labels: api.createdLabels})
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("kept Sprite SSH key missing: %v", err)
	}
	t.Cleanup(func() { core.RemoveStoredTestboxKey(leaseID) })
}

func TestAcquireFailureDeletesReturnedSpriteName(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeSpritesAPI{create: spritesInfo{Name: "canonical-sprite"}}
	runner := &recordingRunner{failContains: "exec -s", err: errors.New("bootstrap failed")}
	backend := &spritesBackend{
		cfg:    Config{Sprites: SpritesConfig{WorkRoot: "/home/sprite/crabbox"}},
		rt:     Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner},
		client: api,
	}

	_, err := backend.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: t.TempDir()}})
	if err == nil || !strings.Contains(err.Error(), "bootstrap failed") {
		t.Fatalf("Acquire err=%v, want bootstrap failure", err)
	}
	if api.createdName == api.deleted {
		t.Fatalf("test did not exercise renamed sprite; created=%q deleted=%q", api.createdName, api.deleted)
	}
	if api.deleted != "canonical-sprite" {
		t.Fatalf("deleted=%q, want returned sprite name", api.deleted)
	}
}

func TestSpritesRejectsTailscale(t *testing.T) {
	cfg := Config{Sprites: SpritesConfig{Token: "test-token"}}
	cfg.Tailscale.Enabled = true
	_, err := NewSpritesBackend(Provider{}.Spec(), cfg, Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: &recordingRunner{}})
	if err == nil || !strings.Contains(err.Error(), "--tailscale is not supported for provider=sprites") {
		t.Fatalf("err=%v", err)
	}
}

func TestSpritesRejectsUnsafeWorkRootBeforeBackend(t *testing.T) {
	cfg := Config{Sprites: SpritesConfig{Token: "test-token", WorkRoot: "/tmp"}}
	_, err := NewSpritesBackend(Provider{}.Spec(), cfg, Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: &recordingRunner{}})
	if err == nil || !strings.Contains(err.Error(), "too broad") {
		t.Fatalf("err=%v", err)
	}
}

func TestSpritesRejectsUnsafeAPIURLBeforeBackend(t *testing.T) {
	cfg := Config{Sprites: SpritesConfig{
		Token:    "test-token",
		APIURL:   "http://api.sprites.dev",
		WorkRoot: "/home/sprite/crabbox",
	}}
	_, err := NewSpritesBackend(Provider{}.Spec(), cfg, Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: &recordingRunner{}})
	if err == nil || !strings.Contains(err.Error(), "must use HTTPS") {
		t.Fatalf("err=%v", err)
	}
}

func TestSpritesAPIURLValidation(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "canonical https", raw: "HTTPS://API.SPRITES.DEV:443/tenant/", want: "https://api.sprites.dev/tenant"},
		{name: "escaped path", raw: "https://api.sprites.dev/tenant%2F/", want: "https://api.sprites.dev/tenant%2F"},
		{name: "localhost", raw: "http://localhost:8080/", want: "http://localhost:8080"},
		{name: "ipv4 loopback", raw: "http://127.0.0.2:8080/api", want: "http://127.0.0.2:8080/api"},
		{name: "ipv6 loopback", raw: "http://[::1]:80/", want: "http://[::1]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, _, err := validateSpritesAPIURL(test.raw)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("url=%q want %q", got, test.want)
			}
		})
	}

	for _, test := range []struct {
		name string
		raw  string
	}{
		{name: "public http", raw: "http://api.sprites.dev"},
		{name: "relative", raw: "/v1/sprites"},
		{name: "schemeless", raw: "api.sprites.dev"},
		{name: "missing host", raw: "https:///v1/sprites"},
		{name: "opaque", raw: "https:api.sprites.dev"},
		{name: "other scheme", raw: "ftp://api.sprites.dev"},
		{name: "userinfo", raw: "https://token@api.sprites.dev"},
		{name: "query", raw: "https://api.sprites.dev?region=us"},
		{name: "bare query", raw: "https://api.sprites.dev?"},
		{name: "fragment", raw: "https://api.sprites.dev#fragment"},
		{name: "malformed port", raw: "https://api.sprites.dev:bad"},
		{name: "loopback lookalike", raw: "http://localhost.example.com"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := validateSpritesAPIURL(test.raw); err == nil {
				t.Fatalf("expected %q to be rejected", test.raw)
			}
		})
	}
}

func TestSpritesClientDefaultsAPIURL(t *testing.T) {
	client, err := newSpritesClient(Config{Sprites: SpritesConfig{Token: "test-token"}}, Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if got := client.(*spritesClient).apiURL; got != "https://api.sprites.dev" {
		t.Fatalf("apiURL=%q", got)
	}
}

func TestSpritesClientRejectsCrossOriginRedirect(t *testing.T) {
	targetRequests := 0
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetRequests++
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/capture", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	client, err := newSpritesClient(
		Config{Sprites: SpritesConfig{Token: "test-token", APIURL: origin.URL}},
		Runtime{HTTP: origin.Client()},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ListSprites(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "redirect changed API origin") {
		t.Fatalf("err=%v", err)
	}
	if targetRequests != 0 {
		t.Fatalf("target requests=%d", targetRequests)
	}
}

func TestSpritesClientPreservesAuthOnSameOriginRedirect(t *testing.T) {
	var redirected bool
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/sprites" {
			http.Redirect(w, r, srv.URL+"/redirected", http.StatusTemporaryRedirect)
			return
		}
		redirected = true
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("auth=%q", got)
		}
		_ = json.NewEncoder(w).Encode(spritesListResponse{})
	}))
	defer srv.Close()

	client, err := newSpritesClient(
		Config{Sprites: SpritesConfig{Token: "test-token", APIURL: srv.URL}},
		Runtime{HTTP: srv.Client()},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListSprites(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if !redirected {
		t.Fatal("same-origin redirect was not followed")
	}
}

func TestSpritesClientPreservesCustomRedirectPolicy(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	source := srv.Client()
	source.Timeout = 3 * time.Second
	source.CheckRedirect = func(*http.Request, []*http.Request) error {
		called = true
		return errors.New("custom redirect stop")
	}

	client, err := newSpritesClient(
		Config{Sprites: SpritesConfig{Token: "test-token", APIURL: srv.URL}},
		Runtime{HTTP: source},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ListSprites(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "custom redirect stop") {
		t.Fatalf("err=%v", err)
	}
	if !called {
		t.Fatal("custom redirect policy was not called")
	}
	secured := client.(*spritesClient).httpClient
	if secured == source || secured.Timeout != source.Timeout || secured.Transport != source.Transport {
		t.Fatal("HTTP client settings were not preserved in a clone")
	}
}

func TestSpritesSameOriginUsesEffectiveDefaultPorts(t *testing.T) {
	a, _ := url.Parse("https://api.sprites.dev/path")
	b, _ := url.Parse("https://API.SPRITES.DEV:443/other")
	c, _ := url.Parse("http://api.sprites.dev:443/other")
	if !sameSpritesOrigin(a, b) {
		t.Fatal("default HTTPS port should be the same origin")
	}
	if sameSpritesOrigin(a, c) {
		t.Fatal("scheme change should not be the same origin")
	}
}

func TestSpritesClientLifecycleRequests(t *testing.T) {
	var sawCreate bool
	var sawDelete bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("auth=%q", got)
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sprites":
			sawCreate = true
			var body struct {
				Name   string   `json:"name"`
				Labels []string `json:"labels"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Name != "crabbox-blue-lobster-12345678" || len(body.Labels) == 0 {
				t.Fatalf("create body=%#v", body)
			}
			_ = json.NewEncoder(w).Encode(spritesInfo{Name: body.Name, Status: "cold", Labels: body.Labels})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sprites":
			if r.URL.Query().Get("prefix") != "crabbox-" {
				t.Fatalf("prefix=%q", r.URL.Query().Get("prefix"))
			}
			_ = json.NewEncoder(w).Encode(spritesListResponse{Sprites: []spritesInfo{{Name: "crabbox-blue-lobster-12345678", Labels: []string{"crabbox"}}}})
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/sprites/crabbox-blue-lobster-12345678":
			sawDelete = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.String())
		}
	}))
	defer srv.Close()

	client, err := newSpritesClient(Config{Sprites: SpritesConfig{Token: "test-token", APIURL: srv.URL}}, Runtime{HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	sprite, err := client.CreateSprite(context.Background(), "crabbox-blue-lobster-12345678", []string{"crabbox"})
	if err != nil {
		t.Fatal(err)
	}
	if sprite.Name != "crabbox-blue-lobster-12345678" {
		t.Fatalf("sprite=%#v", sprite)
	}
	items, err := client.ListSprites(context.Background(), "crabbox-")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items=%#v", items)
	}
	if err := client.DeleteSprite(context.Background(), "crabbox-blue-lobster-12345678"); err != nil {
		t.Fatal(err)
	}
	if !sawCreate || !sawDelete {
		t.Fatalf("sawCreate=%t sawDelete=%t", sawCreate, sawDelete)
	}
}

func TestSpritesClientRedactsErrorResponseCredentials(t *testing.T) {
	const token = "sprites-provider-secret"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Fatalf("auth=%q", got)
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"request denied","authorization":"Bearer `+token+`","clientSecret":"secondary-secret","url":"https://user:pass@example.test/?token=query-secret"}`)
	}))
	defer srv.Close()

	client, err := newSpritesClient(Config{Sprites: SpritesConfig{Token: token, APIURL: srv.URL}}, Runtime{HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ListSprites(context.Background(), "")
	if err == nil {
		t.Fatal("expected Sprites API error")
	}
	text := err.Error()
	for _, leaked := range []string{token, "secondary-secret", "user", "pass", "query-secret"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("Sprites error leaked %q: %s", leaked, text)
		}
	}
	if !strings.Contains(text, "request denied") || !strings.Contains(text, "[redacted]") {
		t.Fatalf("Sprites error lost useful diagnostic context: %s", text)
	}
}

func TestSpritesClientRedactsErrorStatusText(t *testing.T) {
	const token = "sprites-provider-secret"
	httpClient := &http.Client{Transport: spritesRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Fatalf("auth=%q", got)
		}
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Status:     "401 Bearer " + token,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"message":"denied"}`)),
			Request:    r,
		}, nil
	})}
	client, err := newSpritesClient(Config{Sprites: SpritesConfig{Token: token}}, Runtime{HTTP: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ListSprites(context.Background(), "")
	if err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("Sprites status redaction err=%v", err)
	}
}

type spritesRoundTripFunc func(*http.Request) (*http.Response, error)

func (f spritesRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestSpritesClientRejectsBadPagination(t *testing.T) {
	for name, response := range map[string]spritesListResponse{
		"missing token": {HasMore: true},
		"repeated token": {
			HasMore:               true,
			NextContinuationToken: "same",
		},
	} {
		t.Run(name, func(t *testing.T) {
			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v1/sprites" {
					t.Fatalf("unexpected %s %s", r.Method, r.URL.String())
				}
				requests++
				_ = json.NewEncoder(w).Encode(response)
			}))
			defer srv.Close()

			client, err := newSpritesClient(Config{Sprites: SpritesConfig{Token: "test-token", APIURL: srv.URL}}, Runtime{HTTP: srv.Client()})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.ListSprites(context.Background(), "crabbox-")
			if err == nil {
				t.Fatal("expected pagination error")
			}
			if name == "repeated token" && requests != 2 {
				t.Fatalf("requests=%d want 2", requests)
			}
			if name == "missing token" && requests != 1 {
				t.Fatalf("requests=%d want 1", requests)
			}
		})
	}
}

func TestSpritesEnsureCLIUsesSpriteBinary(t *testing.T) {
	runner := &recordingRunner{}
	backend := &spritesBackend{rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	if err := backend.ensureCLI(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 || runner.calls[0] != "sprite --version" {
		t.Fatalf("calls=%#v", runner.calls)
	}
}

func TestSpritesBootstrapInstallsFullSyncToolchain(t *testing.T) {
	runner := &recordingRunner{}
	backend := &spritesBackend{rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	if err := backend.bootstrapSSH(context.Background(), "crabbox-blue-lobster-12345678", "ssh-ed25519 AAAAtest"); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls=%#v", runner.calls)
	}
	call := runner.calls[0]
	for _, want := range []string{"openssh-server", "git", "rsync", "tar", "python3", "command -v python3"} {
		if !strings.Contains(call, want) {
			t.Fatalf("bootstrap command missing %q: %s", want, call)
		}
	}
}

type recordingRunner struct {
	calls        []string
	failContains string
	err          error
}

func (r *recordingRunner) Run(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	call := strings.Join(append([]string{req.Name}, req.Args...), " ")
	r.calls = append(r.calls, call)
	if r.failContains != "" && strings.Contains(call, r.failContains) {
		err := r.err
		if err == nil {
			err = errors.New("command failed")
		}
		return LocalCommandResult{ExitCode: 1, Stderr: err.Error()}, err
	}
	return LocalCommandResult{}, nil
}

type fakeSpritesAPI struct {
	create        spritesInfo
	get           spritesInfo
	createdName   string
	createdLabels []string
	deleted       string
}

func (f *fakeSpritesAPI) CreateSprite(_ context.Context, name string, labels []string) (spritesInfo, error) {
	f.createdName = name
	f.createdLabels = labels
	if f.create.Name != "" || len(f.create.Labels) > 0 || f.create.ID != "" {
		sprite := f.create
		if len(sprite.Labels) == 0 {
			sprite.Labels = labels
		}
		return sprite, nil
	}
	return spritesInfo{Name: name, Labels: labels}, nil
}

func (f *fakeSpritesAPI) GetSprite(context.Context, string) (spritesInfo, error) {
	return f.get, nil
}

func (f *fakeSpritesAPI) ListSprites(context.Context, string) ([]spritesInfo, error) {
	return nil, nil
}

func (f *fakeSpritesAPI) DeleteSprite(_ context.Context, name string) error {
	f.deleted = name
	return nil
}
