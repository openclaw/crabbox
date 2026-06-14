package agx

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestAGXSSHTargetUsesWorkspaceGateway(t *testing.T) {
	b := &agxBackend{cfg: Config{AGX: AGXConfig{Workspace: "workspace.agx.so", User: "root"}}}
	target := b.agxSSHTarget(agxInstance{ID: "inst-123"}, "/tmp/key")
	if target.User != "root+inst-123" {
		t.Fatalf("user=%q", target.User)
	}
	if target.Host != "workspace.agx.so" || target.Port != "22" {
		t.Fatalf("host/port=%q/%q", target.Host, target.Port)
	}
	if target.Key != "/tmp/key" || target.TargetOS != targetLinux {
		t.Fatalf("target=%#v", target)
	}
	if !target.DisableHostKeyChecking {
		t.Fatal("ephemeral gateway leases should disable host key checking")
	}
}

func TestAGXSSHTargetPrefersControlPlaneEndpoint(t *testing.T) {
	b := &agxBackend{cfg: Config{AGX: AGXConfig{Workspace: "workspace.agx.so", User: "root"}}}
	target := b.agxSSHTarget(agxInstance{ID: "inst-123", SSHUser: "agent+inst-123", SSHHost: "eu.agx.so", SSHPort: 2222}, "/tmp/key")
	if target.User != "agent+inst-123" || target.Host != "eu.agx.so" || target.Port != "2222" {
		t.Fatalf("target=%#v", target)
	}
}

func TestAGXLabelsRoundTripLeaseAndSlug(t *testing.T) {
	labels := agxAPILabels("cbx_abcdef123456", "blue-lobster")
	instance := agxInstance{ID: "inst-1", Name: "crabbox-blue-lobster-12345678", Labels: labels}
	if !isCrabboxInstance(instance) {
		t.Fatal("expected crabbox instance")
	}
	if got := agxLeaseID(instance); got != "cbx_abcdef123456" {
		t.Fatalf("lease=%q", got)
	}
	if got := agxSlug("cbx_abcdef123456", instance); got != "blue-lobster" {
		t.Fatalf("slug=%q", got)
	}
}

func TestCrabboxInstanceOwnershipRequiresLabels(t *testing.T) {
	if isCrabboxInstance(agxInstance{Name: "crabbox-handmade"}) {
		t.Fatal("name-only instance should not be treated as Crabbox-owned")
	}
}

func TestCleanAGXWorkRootRejectsBroadPaths(t *testing.T) {
	for _, p := range []string{"/", "/home", "/root", "/tmp", "relative", ""} {
		if err := cleanAGXWorkRoot(p); err == nil {
			t.Fatalf("expected %q to be rejected", p)
		}
	}
	if err := cleanAGXWorkRoot("/root/crabbox"); err != nil {
		t.Fatalf("work root rejected: %v", err)
	}
}

func TestResolveInstanceIDAcceptsRawInstance(t *testing.T) {
	backend := &agxBackend{client: &fakeAGXAPI{
		get: agxInstance{ID: "inst-1", Name: "crabbox-blue-lobster-12345678", Labels: agxAPILabels("cbx_abcdef123456", "blue-lobster")},
	}}
	id, leaseID, slug, err := backend.resolveInstanceID(context.Background(), "inst-1", false)
	if err != nil {
		t.Fatal(err)
	}
	if id != "inst-1" || leaseID != "cbx_abcdef123456" || slug != "blue-lobster" {
		t.Fatalf("id=%q lease=%q slug=%q", id, leaseID, slug)
	}
}

func TestResolveInstanceIDRejectsUnmanagedWithoutReclaim(t *testing.T) {
	backend := &agxBackend{client: &fakeAGXAPI{get: agxInstance{ID: "inst-2", Name: "handmade"}}}
	_, _, _, err := backend.resolveInstanceID(context.Background(), "inst-2", false)
	if err == nil || !strings.Contains(err.Error(), "not Crabbox-managed") {
		t.Fatalf("err=%v, want reclaim error", err)
	}
}

func TestResolveInstanceIDAcceptsUnmanagedWithReclaim(t *testing.T) {
	backend := &agxBackend{client: &fakeAGXAPI{get: agxInstance{ID: "inst-2", Name: "handmade"}}}
	id, leaseID, _, err := backend.resolveInstanceID(context.Background(), "inst-2", true)
	if err != nil {
		t.Fatal(err)
	}
	if id != "inst-2" || leaseID != "agx_handmade" {
		t.Fatalf("id=%q lease=%q", id, leaseID)
	}
}

func TestResolveInstanceIDRejectsOtherProviderClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := claimLeaseForRepoProvider("cbx_abcdef123456", "blue-lobster", "aws", t.TempDir(), 0, true); err != nil {
		t.Fatal(err)
	}
	backend := &agxBackend{client: &fakeAGXAPI{}}
	_, _, _, err := backend.resolveInstanceID(context.Background(), "blue-lobster", false)
	if err == nil || !strings.Contains(err.Error(), "provider=aws") {
		t.Fatalf("err=%v", err)
	}
}

func TestResolveReleaseOnlySkipsControlPlaneGet(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repoRoot := t.TempDir()
	if err := claimLeaseForRepoProvider("agx_inst-9", "swift-crab", agxProvider, repoRoot, 0, true); err != nil {
		t.Fatal(err)
	}
	api := &fakeAGXAPI{}
	backend := &agxBackend{
		cfg:    Config{AGX: AGXConfig{Workspace: "workspace.agx.so", User: "root", WorkRoot: "/root/crabbox"}},
		rt:     Runtime{Stdout: io.Discard, Stderr: io.Discard},
		client: api,
	}
	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: "swift-crab", ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if api.gets != 0 {
		t.Fatalf("release-only resolve should not call GetInstance: %d", api.gets)
	}
	if lease.LeaseID != "agx_inst-9" || lease.Server.CloudID != "inst-9" {
		t.Fatalf("lease=%#v", lease)
	}
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease, Force: true}); err != nil {
		t.Fatal(err)
	}
	if api.deleted != "inst-9" {
		t.Fatalf("deleted=%q", api.deleted)
	}
	if _, ok, err := resolveLeaseClaim("swift-crab"); err != nil || ok {
		t.Fatalf("claim still resolves ok=%t err=%v", ok, err)
	}
}

func TestReleaseLeaseRejectsUnclaimedUnmanagedInstance(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeAGXAPI{get: agxInstance{ID: "inst-3", Name: "handmade"}}
	backend := &agxBackend{rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}, client: api}
	err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{
		Lease: LeaseTarget{LeaseID: "agx_inst-3", Server: Server{CloudID: "inst-3"}},
		Force: true,
	})
	if err == nil || !strings.Contains(err.Error(), "not Crabbox-managed") {
		t.Fatalf("ReleaseLease err=%v, want unmanaged error", err)
	}
	if api.deleted != "" {
		t.Fatalf("deleted unmanaged instance %q", api.deleted)
	}
}

func TestAcquireFailureDeletesInstanceAndKey(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	// No SSH server is reachable, so waitForSSHReady fails fast once the context
	// is cancelled; use an already-cancelled context to avoid a long wait.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	api := &fakeAGXAPI{create: agxInstance{ID: "inst-created"}}
	backend := &agxBackend{
		cfg:    Config{AGX: AGXConfig{Workspace: "workspace.agx.so", User: "root", WorkRoot: "/root/crabbox"}},
		rt:     Runtime{Stdout: io.Discard, Stderr: io.Discard},
		client: api,
	}
	_, err := backend.Acquire(ctx, AcquireRequest{Repo: core.Repo{Root: t.TempDir()}})
	if err == nil {
		t.Fatal("expected acquire to fail when SSH never becomes ready")
	}
	if api.deleted != "inst-created" {
		t.Fatalf("deleted=%q, want returned instance id", api.deleted)
	}
}

func TestAGXRejectsTailscale(t *testing.T) {
	cfg := Config{AGX: AGXConfig{Token: "test-key", WorkRoot: "/root/crabbox"}}
	cfg.Tailscale.Enabled = true
	_, err := NewAGXBackend(Provider{}.Spec(), cfg, Runtime{Stdout: io.Discard, Stderr: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "--tailscale is not supported for provider=agx") {
		t.Fatalf("err=%v", err)
	}
}

func TestAGXRequiresToken(t *testing.T) {
	cfg := Config{AGX: AGXConfig{WorkRoot: "/root/crabbox"}}
	_, err := NewAGXBackend(Provider{}.Spec(), cfg, Runtime{Stdout: io.Discard, Stderr: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "requires an API key") {
		t.Fatalf("err=%v", err)
	}
}

func TestAGXRejectsUnsafeWorkRootBeforeBackend(t *testing.T) {
	cfg := Config{AGX: AGXConfig{Token: "test-key", WorkRoot: "/tmp"}}
	_, err := NewAGXBackend(Provider{}.Spec(), cfg, Runtime{Stdout: io.Discard, Stderr: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "too broad") {
		t.Fatalf("err=%v", err)
	}
}

func TestAGXClientLifecycleRequests(t *testing.T) {
	var sawCreate, sawDelete bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("auth=%q", got)
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/instances":
			sawCreate = true
			var body agxCreateRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Name != "crabbox-blue-lobster-12345678" || body.PublicKey == "" || body.Labels["crabbox"] != "true" {
				t.Fatalf("create body=%#v", body)
			}
			_ = json.NewEncoder(w).Encode(agxInstance{ID: "inst-1", Name: body.Name, Status: "booting", Labels: body.Labels})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/instances":
			if r.URL.Query().Get("prefix") != "crabbox-" {
				t.Fatalf("prefix=%q", r.URL.Query().Get("prefix"))
			}
			_ = json.NewEncoder(w).Encode(agxListResponse{Instances: []agxInstance{{ID: "inst-1", Name: "crabbox-blue-lobster-12345678", Labels: map[string]string{"crabbox": "true"}}}})
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/instances/inst-1":
			sawDelete = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.String())
		}
	}))
	defer srv.Close()

	client := newAGXClient(Config{AGX: AGXConfig{Token: "test-key", APIURL: srv.URL}}, Runtime{HTTP: srv.Client()})
	instance, err := client.CreateInstance(context.Background(), agxCreateRequest{Name: "crabbox-blue-lobster-12345678", PublicKey: "ssh-ed25519 AAAAtest", Labels: agxAPILabels("cbx_abcdef123456", "blue-lobster")})
	if err != nil {
		t.Fatal(err)
	}
	if instance.ID != "inst-1" {
		t.Fatalf("instance=%#v", instance)
	}
	items, err := client.ListInstances(context.Background(), "crabbox-")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items=%#v", items)
	}
	if err := client.DeleteInstance(context.Background(), "inst-1"); err != nil {
		t.Fatal(err)
	}
	if !sawCreate || !sawDelete {
		t.Fatalf("sawCreate=%t sawDelete=%t", sawCreate, sawDelete)
	}
}

func TestAGXClientRejectsRepeatedPageToken(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_ = json.NewEncoder(w).Encode(agxListResponse{NextPageToken: "same"})
	}))
	defer srv.Close()

	client := newAGXClient(Config{AGX: AGXConfig{Token: "test-key", APIURL: srv.URL}}, Runtime{HTTP: srv.Client()})
	if _, err := client.ListInstances(context.Background(), "crabbox-"); err == nil {
		t.Fatal("expected repeated page token error")
	}
	if requests != 2 {
		t.Fatalf("requests=%d want 2", requests)
	}
}

func TestAGXProviderRegistration(t *testing.T) {
	spec := Provider{}.Spec()
	if spec.Name != "agx" || spec.Kind != core.ProviderKindSSHLease {
		t.Fatalf("spec=%#v", spec)
	}
	if !spec.Features.Has(core.FeatureSSH) || !spec.Features.Has(core.FeatureCrabboxSync) || !spec.Features.Has(core.FeatureCleanup) {
		t.Fatalf("features=%#v", spec.Features)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("targets=%#v", spec.Targets)
	}
	if spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("coordinator=%v", spec.Coordinator)
	}
}

type fakeAGXAPI struct {
	create  agxInstance
	get     agxInstance
	list    []agxInstance
	gets    int
	deleted string
}

func (f *fakeAGXAPI) CreateInstance(_ context.Context, req agxCreateRequest) (agxInstance, error) {
	instance := f.create
	if instance.ID == "" {
		instance.ID = req.Name
	}
	if instance.Name == "" {
		instance.Name = req.Name
	}
	if len(instance.Labels) == 0 {
		instance.Labels = req.Labels
	}
	return instance, nil
}

func (f *fakeAGXAPI) GetInstance(context.Context, string) (agxInstance, error) {
	f.gets++
	return f.get, nil
}

func (f *fakeAGXAPI) ListInstances(context.Context, string) ([]agxInstance, error) {
	return f.list, nil
}

func (f *fakeAGXAPI) DeleteInstance(_ context.Context, id string) error {
	f.deleted = id
	return nil
}
