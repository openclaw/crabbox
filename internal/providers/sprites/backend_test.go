package sprites

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

	client := newSpritesClient(Config{Sprites: SpritesConfig{Token: "test-token", APIURL: srv.URL}}, Runtime{HTTP: srv.Client()})
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
	calls []string
}

func (r *recordingRunner) Run(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	r.calls = append(r.calls, strings.Join(append([]string{req.Name}, req.Args...), " "))
	return LocalCommandResult{}, nil
}

type fakeSpritesAPI struct {
	get     spritesInfo
	deleted string
}

func (f *fakeSpritesAPI) CreateSprite(context.Context, string, []string) (spritesInfo, error) {
	return spritesInfo{}, nil
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
