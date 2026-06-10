package e2b

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type fakeBridgeAPI struct {
	fakeE2BSyncClient
	listed  []e2bSandbox
	listErr error
}

func (f *fakeBridgeAPI) ListSandboxes(_ context.Context, _ map[string]string) ([]e2bSandbox, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listed, nil
}

func newBridgeBackend(t *testing.T, fake *fakeBridgeAPI) *e2bBackend {
	t.Helper()
	prev := newE2BClient
	newE2BClient = func(Config, Runtime) (e2bAPI, error) { return fake, nil }
	t.Cleanup(func() { newE2BClient = prev })
	return &e2bBackend{
		cfg: Config{Provider: e2bProvider, E2B: E2BConfig{APIKey: "key", Domain: "e2b.app"}},
		rt:  Runtime{},
	}
}

func TestE2BPublishPeerReturnsCanonicalURL(t *testing.T) {
	fake := &fakeBridgeAPI{
		listed: []e2bSandbox{{
			SandboxID: "sbx-abc123",
			Domain:    "e2b.app",
			Metadata: map[string]string{
				"crabbox":  "true",
				"provider": e2bProvider,
				"lease":    "cbx_lease",
				"slug":     "web",
			},
		}},
	}
	backend := newBridgeBackend(t, fake)
	target, err := backend.PublishPeer(context.Background(), "cbx_lease", 8080, time.Hour)
	if err != nil {
		t.Fatalf("PublishPeer: %v", err)
	}
	if target.Port != 8080 {
		t.Fatalf("expected port 8080, got %d", target.Port)
	}
	want := "https://8080-sbx-abc123.e2b.app"
	if target.URL != want {
		t.Fatalf("expected URL %q, got %q", want, target.URL)
	}
}

func TestE2BPublishPeerAcceptsSyntheticLeaseID(t *testing.T) {
	fake := &fakeBridgeAPI{
		fakeE2BSyncClient: fakeE2BSyncClient{
			sandbox: e2bSandbox{
				SandboxID: "sbx_1",
				Domain:    "e2b.app",
				Metadata: map[string]string{
					"crabbox":  "true",
					"provider": e2bProvider,
				},
			},
		},
	}
	backend := newBridgeBackend(t, fake)
	target, err := backend.PublishPeer(context.Background(), "e2b_sbx_1", 3000, time.Hour)
	if err != nil {
		t.Fatalf("PublishPeer: %v", err)
	}
	if target.URL != "https://3000-sbx_1.e2b.app" {
		t.Fatalf("URL=%q, want synthetic sandbox preview URL", target.URL)
	}
}

func TestE2BPublishPeerFallsBackToConfigDomain(t *testing.T) {
	fake := &fakeBridgeAPI{
		listed: []e2bSandbox{{
			SandboxID: "sbx-z",
			Domain:    "",
			Metadata: map[string]string{
				"crabbox":  "true",
				"provider": e2bProvider,
				"lease":    "cbx_l",
			},
		}},
	}
	backend := newBridgeBackend(t, fake)
	target, err := backend.PublishPeer(context.Background(), "cbx_l", 443, time.Hour)
	if err != nil {
		t.Fatalf("PublishPeer: %v", err)
	}
	if !strings.HasSuffix(target.URL, ".e2b.app") {
		t.Fatalf("expected fallback domain e2b.app, got %q", target.URL)
	}
}

func TestE2BPublishPeerRejectsBadInput(t *testing.T) {
	fake := &fakeBridgeAPI{}
	backend := newBridgeBackend(t, fake)
	if _, err := backend.PublishPeer(context.Background(), "", 8080, time.Hour); err == nil {
		t.Fatalf("expected rejection of empty lease id")
	}
	if _, err := backend.PublishPeer(context.Background(), "isb_islo", 8080, time.Hour); err == nil {
		t.Fatalf("expected rejection of non-e2b lease prefix")
	}
	if _, err := backend.PublishPeer(context.Background(), "cbx_x", 0, time.Hour); err == nil {
		t.Fatalf("expected rejection of port 0")
	}
	if _, err := backend.PublishPeer(context.Background(), "cbx_x", 70000, time.Hour); err == nil {
		t.Fatalf("expected rejection of port 70000")
	}
}

func TestE2BPublishPeerSurfacesAPIError(t *testing.T) {
	fake := &fakeBridgeAPI{listErr: errors.New("e2b down")}
	backend := newBridgeBackend(t, fake)
	if _, err := backend.PublishPeer(context.Background(), "cbx_lease", 8080, time.Hour); err == nil {
		t.Fatalf("expected list error to surface")
	}
}

func TestE2BListPeerTargetsIsEmpty(t *testing.T) {
	fake := &fakeBridgeAPI{
		listed: []e2bSandbox{{
			SandboxID: "sbx-y",
			Domain:    "e2b.app",
			Metadata: map[string]string{
				"crabbox":  "true",
				"provider": e2bProvider,
				"lease":    "cbx_lease",
			},
		}},
	}
	backend := newBridgeBackend(t, fake)
	targets, err := backend.ListPeerTargets(context.Background(), "cbx_lease")
	if err != nil {
		t.Fatalf("ListPeerTargets: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("expected empty targets (no list API on E2B), got %d", len(targets))
	}
}

// Static check: ensure e2bBackend satisfies core.BridgeProvider.
var _ core.BridgeProvider = (*e2bBackend)(nil)
