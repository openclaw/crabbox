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
	"time"
)

// stubPondTailnetACLClient lets unit tests exercise pondACLEnsure without
// touching the network. Each method records call counts and the last input
// body so assertions can be made against the merge logic.
type stubPondTailnetACLClient struct {
	policy   string
	etag     string
	getErr   error
	putErr   error
	gets     int32
	puts     int32
	lastBody string
	lastEtag string
}

func (s *stubPondTailnetACLClient) GetPolicy(_ context.Context, _ string) (string, string, error) {
	atomic.AddInt32(&s.gets, 1)
	return s.policy, s.etag, s.getErr
}

func (s *stubPondTailnetACLClient) PutPolicy(_ context.Context, _, body, etag string) error {
	atomic.AddInt32(&s.puts, 1)
	s.lastBody = body
	s.lastEtag = etag
	return s.putErr
}

func TestPondACLEnsureNoopWhenRowPresent(t *testing.T) {
	tag := pondTailscaleTag("user", "alpha")
	stub := &stubPondTailnetACLClient{
		policy: pondPolicyFixture(tag),
		etag:   `"v1"`,
	}
	if err := pondACLEnsure(context.Background(), stub, "-", "user", "alpha"); err != nil {
		t.Fatalf("pondACLEnsure: %v", err)
	}
	if atomic.LoadInt32(&stub.gets) != 1 {
		t.Fatalf("expected 1 GET, got %d", stub.gets)
	}
	if atomic.LoadInt32(&stub.puts) != 0 {
		t.Fatalf("expected no PUT when row present, got %d", stub.puts)
	}
}

func TestPondACLEnsureUpsertsMissingRowAndPropagatesETag(t *testing.T) {
	stub := &stubPondTailnetACLClient{
		policy: `{"tagOwners":{"tag:crabbox":["autogroup:admin"]},"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}`,
		etag:   `"v7"`,
	}
	if err := pondACLEnsure(context.Background(), stub, "-", "user", "alpha"); err != nil {
		t.Fatalf("pondACLEnsure: %v", err)
	}
	if atomic.LoadInt32(&stub.puts) != 1 {
		t.Fatalf("expected 1 PUT, got %d", stub.puts)
	}
	if stub.lastEtag != `"v7"` {
		t.Fatalf("expected If-Match etag to be propagated, got %q", stub.lastEtag)
	}
	wantTag := pondTailscaleTag("user", "alpha")
	if !strings.Contains(stub.lastBody, wantTag) {
		t.Fatalf("expected new policy body to mention %q, got:\n%s", wantTag, stub.lastBody)
	}
	// The merged body must still parse as JSON and carry the expected entries.
	var policy map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stub.lastBody), &policy); err != nil {
		t.Fatalf("merged policy is not valid JSON: %v\n%s", err, stub.lastBody)
	}
	if !pondACLRowPresent(stub.lastBody, wantTag) {
		t.Fatalf("merged policy should pass pondACLRowPresent, got:\n%s", stub.lastBody)
	}
}

func TestPondACLEnsurePrefersGrantsWhenPolicyUsesGrants(t *testing.T) {
	stub := &stubPondTailnetACLClient{
		policy: `{"tagOwners":{"tag:crabbox":["autogroup:admin"]},"grants":[{"src":["tag:crabbox"],"dst":["tag:crabbox"],"ip":["*"]}]}`,
		etag:   `"v1"`,
	}
	if err := pondACLEnsure(context.Background(), stub, "-", "user", "alpha"); err != nil {
		t.Fatalf("pondACLEnsure: %v", err)
	}
	if !strings.Contains(stub.lastBody, `"grants"`) {
		t.Fatalf("expected grants stanza preserved, got:\n%s", stub.lastBody)
	}
	if strings.Contains(stub.lastBody, `"acls"`) {
		t.Fatalf("must not down-convert grants policy to acls, got:\n%s", stub.lastBody)
	}
}

func TestPondACLEnsureETagMismatchReturnsClearError(t *testing.T) {
	// A persistent 412 from the server must surface a clear, actionable
	// error after the retry budget is exhausted. The stub returns the
	// sentinel so pondACLEnsure goes through the full retry loop.
	stub := &stubPondTailnetACLClient{
		policy: `{"tagOwners":{"tag:crabbox":["autogroup:admin"]}}`,
		etag:   `"v1"`,
		putErr: errPondACLPreconditionFailed,
	}
	err := pondACLEnsure(context.Background(), stub, "-", "user", "alpha")
	if err == nil {
		t.Fatal("expected error on ETag mismatch")
	}
	if !strings.Contains(err.Error(), "ETag race persisted") {
		t.Fatalf("expected ETag race persisted error after retry, got %v", err)
	}
	if !errors.Is(err, errPondACLPreconditionFailed) {
		t.Fatalf("expected wrapped sentinel, got %v", err)
	}
}

func TestPondACLEnsureAcceptsHuJSONPolicy(t *testing.T) {
	stub := &stubPondTailnetACLClient{
		policy: `// my tailnet policy
{
  "tagOwners": { "tag:crabbox": ["autogroup:admin"], },
}`,
		etag: `"v1"`,
	}
	err := pondACLEnsure(context.Background(), stub, "-", "user", "alpha")
	if err != nil {
		t.Fatalf("expected HuJSON to be accepted, got %v", err)
	}
}

func TestPondACLMergePolicyPreservesHuJSONStructure(t *testing.T) {
	tag := pondTailscaleTag("alice", "pr-42")
	input := `// policy header
{
  // owners stay commented
  "tagOwners": {
    "tag:crabbox": ["autogroup:admin"], // existing owner
  },
  "tests": [
    // unrelated test section must survive
    { "src": "alice@example.com", "accept": ["tag:crabbox:*"], },
  ],
  "grants": [
    { "src": ["tag:crabbox"], "dst": ["tag:crabbox"], "ip": ["*"], },
  ],
  "ssh": [
    { "action": "check", "src": ["autogroup:member"], "dst": ["autogroup:self"], "users": ["autogroup:nonroot"], },
  ],
  "unknownTopLevel": {
    "kept": true,
  },
}`

	merged, err := pondACLMergePolicy(input, tag)
	if err != nil {
		t.Fatalf("pondACLMergePolicy: %v", err)
	}
	for _, want := range []string{
		"// policy header",
		"// owners stay commented",
		"// existing owner",
		"// unrelated test section must survive",
		`"tests"`,
		`"ssh"`,
		`"unknownTopLevel"`,
		tag,
	} {
		if !strings.Contains(merged, want) {
			t.Fatalf("merged policy missing %q:\n%s", want, merged)
		}
	}
	if !pondACLRowPresent(merged, tag) {
		t.Fatalf("merged policy should pass pondACLRowPresent:\n%s", merged)
	}
	again, err := pondACLMergePolicy(merged, tag)
	if err != nil {
		t.Fatalf("second pondACLMergePolicy: %v", err)
	}
	if again != merged {
		t.Fatalf("merge should be idempotent\nfirst:\n%s\nsecond:\n%s", merged, again)
	}
}

func TestPondACLEnsureRefusesTrulyMalformedPolicy(t *testing.T) {
	// Genuinely malformed input (not JSON, not HuJSON) must still be
	// refused so we never overwrite an operator's policy on garbage.
	stub := &stubPondTailnetACLClient{
		policy: `this is not json or hujson at all {{{`,
		etag:   `"v1"`,
	}
	err := pondACLEnsure(context.Background(), stub, "-", "user", "alpha")
	if err == nil {
		t.Fatal("expected error on truly malformed policy")
	}
	if !strings.Contains(err.Error(), "non-HuJSON") {
		t.Fatalf("expected non-HuJSON error, got %v", err)
	}
	if atomic.LoadInt32(&stub.puts) != 0 {
		t.Fatalf("must not PUT when merge fails, got %d puts", stub.puts)
	}
}

func TestPondACLEnsurePropagatesGetError(t *testing.T) {
	stub := &stubPondTailnetACLClient{getErr: fmt.Errorf("tailscale api 401: invalid api key")}
	err := pondACLEnsure(context.Background(), stub, "-", "user", "alpha")
	if err == nil {
		t.Fatal("expected error when GET fails")
	}
	if !strings.Contains(err.Error(), "read policy") {
		t.Fatalf("expected read policy error, got %v", err)
	}
}

// TestPondACLEnsureLiveClientETagFlow uses an httptest server to validate the
// production HTTP client's ETag handling end-to-end. It covers (a) ETag is
// echoed from GET to PUT's If-Match header and (b) a 412 from the server is
// surfaced as a clear error.
func TestPondACLEnsureLiveClientETagFlow(t *testing.T) {
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
		client := newPondTailnetTestClient(t, srv.URL, "stub-key")
		if err := pondACLEnsure(context.Background(), client, "-", "user", "alpha"); err != nil {
			t.Fatalf("pondACLEnsure: %v", err)
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
		client := newPondTailnetTestClient(t, srv.URL, "stub-key")
		err := pondACLEnsure(context.Background(), client, "-", "user", "alpha")
		if err == nil {
			t.Fatal("expected error on 412")
		}
		if !strings.Contains(err.Error(), "ETag race persisted") {
			t.Fatalf("expected ETag race persisted error, got %v", err)
		}
	})
}

// newPondTailnetTestClient returns a client wired against an httptest server.
// It mirrors the live client's request shape but rewrites the base URL so the
// test stays hermetic.
func newPondTailnetTestClient(t *testing.T, baseURL, apiKey string) pondTailnetACLClient {
	t.Helper()
	return &testPondTailnetClient{base: baseURL, apiKey: apiKey, http: http.DefaultClient}
}

type testPondTailnetClient struct {
	base   string
	apiKey string
	http   *http.Client
}

func (c *testPondTailnetClient) GetPolicy(ctx context.Context, _ string) (string, string, error) {
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

func (c *testPondTailnetClient) PutPolicy(ctx context.Context, _, body, etag string) error {
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
		return errPondACLPreconditionFailed
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("tailscale api %d", resp.StatusCode)
	}
	return nil
}

// TestPondACLEnsureRetriesOnce412ThenSucceeds wires an httptest server that
// rejects the first PUT with 412 and accepts the second. pondACLEnsure must
// observe the ETag race, re-read the policy, and complete in two attempts
// without bubbling the transient failure to the caller.
func TestPondACLEnsureRetriesOnce412ThenSucceeds(t *testing.T) {
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
	client := newPondTailnetTestClient(t, srv.URL, "stub-key")
	if err := pondACLEnsure(context.Background(), client, "-", "user", "alpha"); err != nil {
		t.Fatalf("pondACLEnsure: %v", err)
	}
	if atomic.LoadInt32(&gets) != 2 {
		t.Fatalf("expected 2 GETs (initial + retry), got %d", gets)
	}
	if atomic.LoadInt32(&puts) != 2 {
		t.Fatalf("expected 2 PUTs (first 412, second 200), got %d", puts)
	}
}

// TestPondACLEnsureSurfacesAfterPersistent412 ensures the retry budget is
// bounded: a server that always returns 412 must not be hammered indefinitely.
// The function should fail after exactly pondACLMaxAttempts PUTs with a
// clear, actionable error.
func TestPondACLEnsureSurfacesAfterPersistent412(t *testing.T) {
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
	client := newPondTailnetTestClient(t, srv.URL, "stub-key")
	err := pondACLEnsure(context.Background(), client, "-", "user", "alpha")
	if err == nil {
		t.Fatal("expected error after persistent 412")
	}
	if !strings.Contains(err.Error(), "ETag race persisted") {
		t.Fatalf("expected ETag race persisted error, got %v", err)
	}
	if got := atomic.LoadInt32(&puts); got != int32(pondACLMaxAttempts) {
		t.Fatalf("expected exactly %d PUTs, got %d", pondACLMaxAttempts, got)
	}
}

func TestMaybeBootstrapPondACLNoopWithoutAPIKey(t *testing.T) {
	t.Setenv(pondACLAutoBootstrapEnvVar, "1")
	t.Setenv("TS_API_KEY", "")
	cfg := Config{Provider: "hetzner", Pond: "alpha"}
	cfg.Tailscale.Enabled = true
	if err := maybeBootstrapPondACL(context.Background(), cfg); err != nil {
		t.Fatalf("expected silent noop without TS_API_KEY, got %v", err)
	}
}

func TestMaybeBootstrapPondACLNoopWithoutExplicitOptIn(t *testing.T) {
	t.Setenv(pondACLAutoBootstrapEnvVar, "")
	t.Setenv("TS_API_KEY", "tskey-api-stub")
	stub := &stubPondTailnetACLClient{}
	prev := pondTailnetACLClientFactory
	defer func() { pondTailnetACLClientFactory = prev }()
	pondTailnetACLClientFactory = func(_ string) pondTailnetACLClient { return stub }
	cfg := Config{Provider: "hetzner", Pond: "alpha"}
	cfg.Tailscale.Enabled = true
	cfg.Tailscale.AuthKey = "tskey-auth-test"
	if err := maybeBootstrapPondACL(context.Background(), cfg); err != nil {
		t.Fatalf("expected silent noop without %s, got %v", pondACLAutoBootstrapEnvVar, err)
	}
	if atomic.LoadInt32(&stub.gets) != 0 {
		t.Fatalf("expected no API call without explicit opt-in, got %d gets", stub.gets)
	}
}

func TestMaybeBootstrapPondACLCallsFactoryWhenKeyPresent(t *testing.T) {
	t.Setenv(pondACLAutoBootstrapEnvVar, "1")
	t.Setenv("TS_API_KEY", "tskey-api-stub")
	tag := pondTailscaleTag(localCoordinatorOwner(), "alpha")
	stub := &stubPondTailnetACLClient{policy: pondPolicyFixture(tag), etag: `"v1"`}
	prev := pondTailnetACLClientFactory
	defer func() { pondTailnetACLClientFactory = prev }()
	pondTailnetACLClientFactory = func(_ string) pondTailnetACLClient { return stub }
	cfg := Config{Provider: "hetzner", Pond: "alpha"}
	cfg.Tailscale.Enabled = true
	cfg.Tailscale.AuthKey = "tskey-auth-test"
	if err := maybeBootstrapPondACL(context.Background(), cfg); err != nil {
		t.Fatalf("maybeBootstrapPondACL: %v", err)
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

// TestPondACLEnsureReturnsUnavailableOn404 wires the live client at an httptest
// server that always replies 404 (the shape of a Headscale control plane,
// which exposes /api/v1/policy instead of Tailscale's /api/v2/tailnet/.../acl
// route). The live client must surface ErrPondACLAutoBootstrapUnavailable so
// the lease creation path falls back to the manual snippet without erroring.
func TestPondACLEnsureReturnsUnavailableOn404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()
	t.Setenv("CRABBOX_TS_API_URL", srv.URL)
	t.Setenv("TS_API_URL", "")
	client := newPondTailnetACLClient("stub-key")
	if client == nil {
		t.Fatal("expected live client when api key is non-empty")
	}
	err := pondACLEnsure(context.Background(), client, "-", "user", "alpha")
	if !errors.Is(err, ErrPondACLAutoBootstrapUnavailable) {
		t.Fatalf("expected ErrPondACLAutoBootstrapUnavailable on 404, got %v", err)
	}
}

// TestPondACLEnsureReturnsUnavailableWhenMissingETag asserts that a 2xx
// response without an ETag header (the way Headscale's policy GET responds)
// is treated as auto-bootstrap unavailable — we cannot safely PUT without
// concurrent-edit protection.
func TestPondACLEnsureReturnsUnavailableWhenMissingETag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"tagOwners":{},"acls":[]}`)
	}))
	defer srv.Close()
	t.Setenv("CRABBOX_TS_API_URL", srv.URL)
	t.Setenv("TS_API_URL", "")
	client := newPondTailnetACLClient("stub-key")
	if client == nil {
		t.Fatal("expected live client when api key is non-empty")
	}
	err := pondACLEnsure(context.Background(), client, "-", "user", "alpha")
	if !errors.Is(err, ErrPondACLAutoBootstrapUnavailable) {
		t.Fatalf("expected ErrPondACLAutoBootstrapUnavailable when ETag missing, got %v", err)
	}
}

// TestMaybeBootstrapPondACLSilentlySkipsWhenControlPlaneUnavailable covers the
// integration path: when pondACLEnsure surfaces ErrPondACLAutoBootstrapUnavailable
// (e.g. against Headscale), the lease creation must not fail. Doctor surfaces
// the same condition with a manual-snippet pointer.
func TestMaybeBootstrapPondACLSilentlySkipsWhenControlPlaneUnavailable(t *testing.T) {
	t.Setenv(pondACLAutoBootstrapEnvVar, "1")
	t.Setenv("TS_API_KEY", "tskey-api-stub")
	prev := pondTailnetACLClientFactory
	defer func() { pondTailnetACLClientFactory = prev }()
	pondTailnetACLClientFactory = func(_ string) pondTailnetACLClient {
		return &stubPondTailnetACLClient{getErr: fmt.Errorf("%w: GET / returned 404", ErrPondACLAutoBootstrapUnavailable)}
	}
	cfg := Config{Provider: "hetzner", Pond: "alpha"}
	cfg.Tailscale.Enabled = true
	cfg.Tailscale.AuthKey = "tskey-auth-test"
	if err := maybeBootstrapPondACL(context.Background(), cfg); err != nil {
		t.Fatalf("expected silent skip on unavailable control plane, got %v", err)
	}
}

func TestMaybeBootstrapPondACLSkipsNonTailscaleProvider(t *testing.T) {
	t.Setenv(pondACLAutoBootstrapEnvVar, "1")
	t.Setenv("TS_API_KEY", "tskey-api-stub")
	stub := &stubPondTailnetACLClient{}
	prev := pondTailnetACLClientFactory
	defer func() { pondTailnetACLClientFactory = prev }()
	pondTailnetACLClientFactory = func(_ string) pondTailnetACLClient { return stub }
	cfg := Config{Provider: "e2b", Pond: "alpha"}
	cfg.Tailscale.Enabled = true
	if err := maybeBootstrapPondACL(context.Background(), cfg); err != nil {
		t.Fatalf("maybeBootstrapPondACL: %v", err)
	}
	if atomic.LoadInt32(&stub.gets) != 0 {
		t.Fatalf("expected no API call for non-Tailscale provider, got %d gets", stub.gets)
	}
}

func TestApplyLeaseCreateFlagsValidatesBeforePondACLBootstrap(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{
			name: "invalid expose",
			args: []string{"--pond", "alpha", "--expose", "not-a-port"},
		},
		{
			name: "invalid ttl",
			args: []string{"--pond", "alpha", "--ttl", "0s"},
		},
		{
			name: "invalid idle timeout",
			args: []string{"--pond", "alpha", "--idle-timeout", "0s"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(pondACLAutoBootstrapEnvVar, "1")
			t.Setenv("TS_API_KEY", "tskey-test")
			t.Setenv("CRABBOX_TAILSCALE_AUTH_KEY", "tskey-auth-test")
			stub := &stubPondTailnetACLClient{policy: `{"tagOwners":{}}`, etag: `"v1"`}
			var factories int32
			prev := pondTailnetACLClientFactory
			pondTailnetACLClientFactory = func(_ string) pondTailnetACLClient {
				atomic.AddInt32(&factories, 1)
				return stub
			}
			t.Cleanup(func() { pondTailnetACLClientFactory = prev })

			defaults := Config{
				Provider:    "hetzner",
				Profile:     "default",
				Class:       "standard",
				TargetOS:    targetLinux,
				TTL:         time.Hour,
				IdleTimeout: 15 * time.Minute,
				Network:     NetworkAuto,
				Capacity:    CapacityConfig{Market: "spot"},
				Tailscale: TailscaleConfig{
					Enabled: true,
					AuthKey: "tskey-auth-test",
				},
			}
			fs := newFlagSet("warmup", io.Discard)
			values := registerLeaseCreateFlags(fs, defaults)
			if err := parseFlags(fs, tc.args); err != nil {
				t.Fatal(err)
			}
			cfg := defaults
			if err := applyLeaseCreateFlags(&cfg, fs, values); err == nil {
				t.Fatalf("expected validation error for %v", tc.args)
			}
			if got := atomic.LoadInt32(&factories); got != 0 {
				t.Fatalf("ACL client factory called %d time(s) before validation failed", got)
			}
			if got := atomic.LoadInt32(&stub.gets); got != 0 {
				t.Fatalf("ACL policy read %d time(s) before validation failed", got)
			}
		})
	}
}
