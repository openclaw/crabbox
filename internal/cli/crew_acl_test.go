package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// stubCrewTailnetACLClient lets unit tests exercise crewACLEnsure without
// touching the network. Each method records call counts and the last input
// body so assertions can be made against the merge logic.
type stubCrewTailnetACLClient struct {
	policy   string
	etag     string
	getErr   error
	putErr   error
	gets     int32
	puts     int32
	lastBody string
	lastEtag string
}

func (s *stubCrewTailnetACLClient) GetPolicy(_ context.Context, _ string) (string, string, error) {
	atomic.AddInt32(&s.gets, 1)
	return s.policy, s.etag, s.getErr
}

func (s *stubCrewTailnetACLClient) PutPolicy(_ context.Context, _, body, etag string) error {
	atomic.AddInt32(&s.puts, 1)
	s.lastBody = body
	s.lastEtag = etag
	return s.putErr
}

func TestCrewACLEnsureNoopWhenRowPresent(t *testing.T) {
	tag := crewTailscaleTag("user", "alpha")
	stub := &stubCrewTailnetACLClient{
		policy: crewPolicyFixture(tag),
		etag:   `"v1"`,
	}
	if err := crewACLEnsure(context.Background(), stub, "-", "user", "alpha"); err != nil {
		t.Fatalf("crewACLEnsure: %v", err)
	}
	if atomic.LoadInt32(&stub.gets) != 1 {
		t.Fatalf("expected 1 GET, got %d", stub.gets)
	}
	if atomic.LoadInt32(&stub.puts) != 0 {
		t.Fatalf("expected no PUT when row present, got %d", stub.puts)
	}
}

func TestCrewACLEnsureUpsertsMissingRowAndPropagatesETag(t *testing.T) {
	stub := &stubCrewTailnetACLClient{
		policy: `{"tagOwners":{"tag:crabbox":["autogroup:admin"]},"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}`,
		etag:   `"v7"`,
	}
	if err := crewACLEnsure(context.Background(), stub, "-", "user", "alpha"); err != nil {
		t.Fatalf("crewACLEnsure: %v", err)
	}
	if atomic.LoadInt32(&stub.puts) != 1 {
		t.Fatalf("expected 1 PUT, got %d", stub.puts)
	}
	if stub.lastEtag != `"v7"` {
		t.Fatalf("expected If-Match etag to be propagated, got %q", stub.lastEtag)
	}
	wantTag := crewTailscaleTag("user", "alpha")
	if !strings.Contains(stub.lastBody, wantTag) {
		t.Fatalf("expected new policy body to mention %q, got:\n%s", wantTag, stub.lastBody)
	}
	// The merged body must still parse as JSON and carry the expected entries.
	var policy map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stub.lastBody), &policy); err != nil {
		t.Fatalf("merged policy is not valid JSON: %v\n%s", err, stub.lastBody)
	}
	if !crewACLRowPresent(stub.lastBody, wantTag) {
		t.Fatalf("merged policy should pass crewACLRowPresent, got:\n%s", stub.lastBody)
	}
}

func TestCrewACLEnsurePrefersGrantsWhenPolicyUsesGrants(t *testing.T) {
	stub := &stubCrewTailnetACLClient{
		policy: `{"tagOwners":{"tag:crabbox":["autogroup:admin"]},"grants":[{"src":["tag:crabbox"],"dst":["tag:crabbox"],"ip":["*"]}]}`,
		etag:   `"v1"`,
	}
	if err := crewACLEnsure(context.Background(), stub, "-", "user", "alpha"); err != nil {
		t.Fatalf("crewACLEnsure: %v", err)
	}
	if !strings.Contains(stub.lastBody, `"grants"`) {
		t.Fatalf("expected grants stanza preserved, got:\n%s", stub.lastBody)
	}
	if strings.Contains(stub.lastBody, `"acls"`) {
		t.Fatalf("must not down-convert grants policy to acls, got:\n%s", stub.lastBody)
	}
}

func TestCrewACLEnsureETagMismatchReturnsClearError(t *testing.T) {
	// A persistent 412 from the server must surface a clear, actionable
	// error after the retry budget is exhausted. The stub returns the
	// sentinel so crewACLEnsure goes through the full retry loop.
	stub := &stubCrewTailnetACLClient{
		policy: `{"tagOwners":{"tag:crabbox":["autogroup:admin"]}}`,
		etag:   `"v1"`,
		putErr: errCrewACLPreconditionFailed,
	}
	err := crewACLEnsure(context.Background(), stub, "-", "user", "alpha")
	if err == nil {
		t.Fatal("expected error on ETag mismatch")
	}
	if !strings.Contains(err.Error(), "ETag race persisted") {
		t.Fatalf("expected ETag race persisted error after retry, got %v", err)
	}
	if !errors.Is(err, errCrewACLPreconditionFailed) {
		t.Fatalf("expected wrapped sentinel, got %v", err)
	}
}

func TestCrewACLEnsureRefusesMalformedPolicy(t *testing.T) {
	// HuJSON line comments make the body non-JSON. We must refuse rather
	// than overwrite the operator's policy.
	stub := &stubCrewTailnetACLClient{
		policy: `// my tailnet policy
{
  "tagOwners": { "tag:crabbox": ["autogroup:admin"] }
}`,
		etag: `"v1"`,
	}
	err := crewACLEnsure(context.Background(), stub, "-", "user", "alpha")
	if err == nil {
		t.Fatal("expected error on non-JSON policy")
	}
	if !strings.Contains(err.Error(), "non-JSON") {
		t.Fatalf("expected non-JSON error, got %v", err)
	}
	if atomic.LoadInt32(&stub.puts) != 0 {
		t.Fatalf("must not PUT when merge fails, got %d puts", stub.puts)
	}
}

func TestCrewACLEnsurePropagatesGetError(t *testing.T) {
	stub := &stubCrewTailnetACLClient{getErr: fmt.Errorf("tailscale api 401: invalid api key")}
	err := crewACLEnsure(context.Background(), stub, "-", "user", "alpha")
	if err == nil {
		t.Fatal("expected error when GET fails")
	}
	if !strings.Contains(err.Error(), "read policy") {
		t.Fatalf("expected read policy error, got %v", err)
	}
}

// TestCrewACLEnsureLiveClientETagFlow uses an httptest server to validate the
// production HTTP client's ETag handling end-to-end. It covers (a) ETag is
// echoed from GET to PUT's If-Match header and (b) a 412 from the server is
// surfaced as a clear error.
func TestCrewACLEnsureLiveClientETagFlow(t *testing.T) {
	t.Run("happy path threads ETag", func(t *testing.T) {
		var lastIfMatch string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				w.Header().Set("ETag", `"v42"`)
				_, _ = io.WriteString(w, `{"tagOwners":{"tag:crabbox":["autogroup:admin"]}}`)
			case http.MethodPost:
				lastIfMatch = r.Header.Get("If-Match")
				w.WriteHeader(http.StatusOK)
			default:
				w.WriteHeader(http.StatusMethodNotAllowed)
			}
		}))
		defer srv.Close()
		client := newCrewTailnetTestClient(t, srv.URL, "stub-key")
		if err := crewACLEnsure(context.Background(), client, "-", "user", "alpha"); err != nil {
			t.Fatalf("crewACLEnsure: %v", err)
		}
		if lastIfMatch != `"v42"` {
			t.Fatalf("expected If-Match propagated, got %q", lastIfMatch)
		}
	})

	t.Run("server 412 surfaces clear error after retry", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				w.Header().Set("ETag", `"v1"`)
				_, _ = io.WriteString(w, `{"tagOwners":{"tag:crabbox":["autogroup:admin"]}}`)
			case http.MethodPost:
				w.WriteHeader(http.StatusPreconditionFailed)
			}
		}))
		defer srv.Close()
		client := newCrewTailnetTestClient(t, srv.URL, "stub-key")
		err := crewACLEnsure(context.Background(), client, "-", "user", "alpha")
		if err == nil {
			t.Fatal("expected error on 412")
		}
		if !strings.Contains(err.Error(), "ETag race persisted") {
			t.Fatalf("expected ETag race persisted error, got %v", err)
		}
	})
}

// newCrewTailnetTestClient returns a client wired against an httptest server.
// It mirrors the live client's request shape but rewrites the base URL so the
// test stays hermetic.
func newCrewTailnetTestClient(t *testing.T, baseURL, apiKey string) crewTailnetACLClient {
	t.Helper()
	return &testCrewTailnetClient{base: baseURL, apiKey: apiKey, http: http.DefaultClient}
}

type testCrewTailnetClient struct {
	base   string
	apiKey string
	http   *http.Client
}

func (c *testCrewTailnetClient) GetPolicy(ctx context.Context, _ string) (string, string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/v2/tailnet/-/acl", nil)
	req.SetBasicAuth(c.apiKey, "")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("tailscale api %d", resp.StatusCode)
	}
	return string(body), resp.Header.Get("ETag"), nil
}

func (c *testCrewTailnetClient) PutPolicy(ctx context.Context, _, body, etag string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/v2/tailnet/-/acl", strings.NewReader(body))
	req.SetBasicAuth(c.apiKey, "")
	req.Header.Set("Content-Type", "application/json")
	if etag != "" {
		req.Header.Set("If-Match", etag)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusPreconditionFailed {
		return errCrewACLPreconditionFailed
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("tailscale api %d", resp.StatusCode)
	}
	return nil
}

// TestCrewACLEnsureRetriesOnce412ThenSucceeds wires an httptest server that
// rejects the first PUT with 412 and accepts the second. crewACLEnsure must
// observe the ETag race, re-read the policy, and complete in two attempts
// without bubbling the transient failure to the caller.
func TestCrewACLEnsureRetriesOnce412ThenSucceeds(t *testing.T) {
	var gets, puts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			atomic.AddInt32(&gets, 1)
			w.Header().Set("ETag", `"v1"`)
			_, _ = io.WriteString(w, `{"tagOwners":{"tag:crabbox":["autogroup:admin"]}}`)
		case http.MethodPost:
			n := atomic.AddInt32(&puts, 1)
			if n == 1 {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()
	client := newCrewTailnetTestClient(t, srv.URL, "stub-key")
	if err := crewACLEnsure(context.Background(), client, "-", "user", "alpha"); err != nil {
		t.Fatalf("crewACLEnsure: %v", err)
	}
	if atomic.LoadInt32(&gets) != 2 {
		t.Fatalf("expected 2 GETs (initial + retry), got %d", gets)
	}
	if atomic.LoadInt32(&puts) != 2 {
		t.Fatalf("expected 2 PUTs (first 412, second 200), got %d", puts)
	}
}

// TestCrewACLEnsureSurfacesAfterPersistent412 ensures the retry budget is
// bounded: a server that always returns 412 must not be hammered indefinitely.
// The function should fail after exactly crewACLMaxAttempts PUTs with a
// clear, actionable error.
func TestCrewACLEnsureSurfacesAfterPersistent412(t *testing.T) {
	var puts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("ETag", `"v1"`)
			_, _ = io.WriteString(w, `{"tagOwners":{"tag:crabbox":["autogroup:admin"]}}`)
		case http.MethodPost:
			atomic.AddInt32(&puts, 1)
			w.WriteHeader(http.StatusPreconditionFailed)
		}
	}))
	defer srv.Close()
	client := newCrewTailnetTestClient(t, srv.URL, "stub-key")
	err := crewACLEnsure(context.Background(), client, "-", "user", "alpha")
	if err == nil {
		t.Fatal("expected error after persistent 412")
	}
	if !strings.Contains(err.Error(), "ETag race persisted") {
		t.Fatalf("expected ETag race persisted error, got %v", err)
	}
	if got := atomic.LoadInt32(&puts); got != int32(crewACLMaxAttempts) {
		t.Fatalf("expected exactly %d PUTs, got %d", crewACLMaxAttempts, got)
	}
}

func TestMaybeBootstrapCrewACLNoopWithoutAPIKey(t *testing.T) {
	t.Setenv("TS_API_KEY", "")
	cfg := Config{Provider: "hetzner", Crew: "alpha"}
	cfg.Tailscale.Enabled = true
	if err := maybeBootstrapCrewACL(context.Background(), cfg); err != nil {
		t.Fatalf("expected silent noop without TS_API_KEY, got %v", err)
	}
}

func TestMaybeBootstrapCrewACLCallsFactoryWhenKeyPresent(t *testing.T) {
	t.Setenv("TS_API_KEY", "tskey-api-stub")
	tag := crewTailscaleTag(localCoordinatorOwner(), "alpha")
	stub := &stubCrewTailnetACLClient{policy: crewPolicyFixture(tag), etag: `"v1"`}
	prev := crewTailnetACLClientFactory
	defer func() { crewTailnetACLClientFactory = prev }()
	crewTailnetACLClientFactory = func(_ string) crewTailnetACLClient { return stub }
	cfg := Config{Provider: "hetzner", Crew: "alpha"}
	cfg.Tailscale.Enabled = true
	if err := maybeBootstrapCrewACL(context.Background(), cfg); err != nil {
		t.Fatalf("maybeBootstrapCrewACL: %v", err)
	}
	if atomic.LoadInt32(&stub.gets) != 1 {
		t.Fatalf("expected 1 GET when key is set, got %d", stub.gets)
	}
}

func TestResolveTailnetAPIURLDefaults(t *testing.T) {
	t.Setenv("TS_API_URL", "")
	t.Setenv("CRABBOX_TS_API_URL", "")
	if got := resolveTailnetAPIURL(); got != defaultTailnetAPIURL {
		t.Fatalf("resolveTailnetAPIURL default mismatch: got %q want %q", got, defaultTailnetAPIURL)
	}
}

func TestResolveTailnetAPIURLOverrideViaTSAPIURL(t *testing.T) {
	t.Setenv("CRABBOX_TS_API_URL", "")
	t.Setenv("TS_API_URL", "https://headscale.example.com/")
	if got := resolveTailnetAPIURL(); got != "https://headscale.example.com" {
		t.Fatalf("resolveTailnetAPIURL TS_API_URL override mismatch: got %q", got)
	}
}

func TestResolveTailnetAPIURLCrabboxOverrideWins(t *testing.T) {
	t.Setenv("TS_API_URL", "https://headscale.example.com")
	t.Setenv("CRABBOX_TS_API_URL", "https://primary.example.com")
	if got := resolveTailnetAPIURL(); got != "https://primary.example.com" {
		t.Fatalf("resolveTailnetAPIURL CRABBOX_TS_API_URL precedence mismatch: got %q", got)
	}
}

// TestCrewACLEnsureReturnsUnavailableOn404 wires the live client at an httptest
// server that always replies 404 (the shape of a Headscale control plane,
// which exposes /api/v1/policy instead of Tailscale's /api/v2/tailnet/.../acl
// route). The live client must surface ErrCrewACLAutoBootstrapUnavailable so
// the lease creation path falls back to the manual snippet without erroring.
func TestCrewACLEnsureReturnsUnavailableOn404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()
	t.Setenv("CRABBOX_TS_API_URL", srv.URL)
	t.Setenv("TS_API_URL", "")
	client := newCrewTailnetACLClient("stub-key")
	if client == nil {
		t.Fatal("expected live client when api key is non-empty")
	}
	err := crewACLEnsure(context.Background(), client, "-", "user", "alpha")
	if !errors.Is(err, ErrCrewACLAutoBootstrapUnavailable) {
		t.Fatalf("expected ErrCrewACLAutoBootstrapUnavailable on 404, got %v", err)
	}
}

// TestCrewACLEnsureReturnsUnavailableWhenMissingETag asserts that a 2xx
// response without an ETag header (the way Headscale's policy GET responds)
// is treated as auto-bootstrap unavailable — we cannot safely PUT without
// concurrent-edit protection.
func TestCrewACLEnsureReturnsUnavailableWhenMissingETag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"tagOwners":{},"acls":[]}`)
	}))
	defer srv.Close()
	t.Setenv("CRABBOX_TS_API_URL", srv.URL)
	t.Setenv("TS_API_URL", "")
	client := newCrewTailnetACLClient("stub-key")
	if client == nil {
		t.Fatal("expected live client when api key is non-empty")
	}
	err := crewACLEnsure(context.Background(), client, "-", "user", "alpha")
	if !errors.Is(err, ErrCrewACLAutoBootstrapUnavailable) {
		t.Fatalf("expected ErrCrewACLAutoBootstrapUnavailable when ETag missing, got %v", err)
	}
}

// TestMaybeBootstrapCrewACLSilentlySkipsWhenControlPlaneUnavailable covers the
// integration path: when crewACLEnsure surfaces ErrCrewACLAutoBootstrapUnavailable
// (e.g. against Headscale), the lease creation must not fail. Doctor surfaces
// the same condition with a manual-snippet pointer.
func TestMaybeBootstrapCrewACLSilentlySkipsWhenControlPlaneUnavailable(t *testing.T) {
	t.Setenv("TS_API_KEY", "tskey-api-stub")
	prev := crewTailnetACLClientFactory
	defer func() { crewTailnetACLClientFactory = prev }()
	crewTailnetACLClientFactory = func(_ string) crewTailnetACLClient {
		return &stubCrewTailnetACLClient{getErr: fmt.Errorf("%w: GET / returned 404", ErrCrewACLAutoBootstrapUnavailable)}
	}
	cfg := Config{Provider: "hetzner", Crew: "alpha"}
	cfg.Tailscale.Enabled = true
	if err := maybeBootstrapCrewACL(context.Background(), cfg); err != nil {
		t.Fatalf("expected silent skip on unavailable control plane, got %v", err)
	}
}

func TestMaybeBootstrapCrewACLSkipsNonTailscaleProvider(t *testing.T) {
	t.Setenv("TS_API_KEY", "tskey-api-stub")
	stub := &stubCrewTailnetACLClient{}
	prev := crewTailnetACLClientFactory
	defer func() { crewTailnetACLClientFactory = prev }()
	crewTailnetACLClientFactory = func(_ string) crewTailnetACLClient { return stub }
	cfg := Config{Provider: "e2b", Crew: "alpha"}
	cfg.Tailscale.Enabled = true
	if err := maybeBootstrapCrewACL(context.Background(), cfg); err != nil {
		t.Fatalf("maybeBootstrapCrewACL: %v", err)
	}
	if atomic.LoadInt32(&stub.gets) != 0 {
		t.Fatalf("expected no API call for non-Tailscale provider, got %d gets", stub.gets)
	}
}
