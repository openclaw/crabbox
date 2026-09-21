package scaleway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	instance "github.com/scaleway/scaleway-sdk-go/api/instance/v1"
	sdkerrors "github.com/scaleway/scaleway-sdk-go/errors"
	scwlogger "github.com/scaleway/scaleway-sdk-go/logger"
	"github.com/scaleway/scaleway-sdk-go/scw"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func TestScalewayClientRefusesCrossOriginRedirectBeforeTokenReplay(t *testing.T) {
	for _, test := range []struct {
		name       string
		useRuntime bool
	}{
		{name: "SDK default client"},
		{name: "runtime client", useRuntime: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var sinkRequests atomic.Int32
			sink := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				sinkRequests.Add(1)
			}))
			defer sink.Close()

			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("X-Auth-Token"); got != testScalewaySecretKey {
					t.Errorf("origin X-Auth-Token=%q", got)
				}
				http.Redirect(w, r, sink.URL+"/stolen?location-secret=value#fragment-secret", http.StatusTemporaryRedirect)
			}))
			defer origin.Close()

			var runtimeHTTP *http.Client
			if test.useRuntime {
				runtimeHTTP = origin.Client()
			}
			client := newTestScalewaySDKClient(t, origin.URL, runtimeHTTP)
			_, err := client.Instance().ListServers(testScalewayListRequest(), scw.WithContext(context.Background()))
			if !errors.Is(err, errScalewayCrossOriginRedirect) {
				t.Fatalf("error=%v want cross-origin redirect refusal", err)
			}
			if got := sinkRequests.Load(); got != 0 {
				t.Fatalf("redirect sink received %d requests", got)
			}
			for _, leaked := range []string{"location-secret", "fragment-secret", "/stolen"} {
				if strings.Contains(err.Error(), leaked) {
					t.Fatalf("redirect error leaked %q: %v", leaked, err)
				}
			}
		})
	}
}

func TestScalewayClientFollowsSameOriginRedirectWithToken(t *testing.T) {
	var redirected atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/instance/v1/zones/fr-par-1/servers":
			http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
		case "/redirected":
			redirected.Store(true)
			if got := r.Header.Get("X-Auth-Token"); got != testScalewaySecretKey {
				t.Errorf("redirected X-Auth-Token=%q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"servers": []any{}, "total_count": 0})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newTestScalewaySDKClient(t, server.URL, nil)
	if _, err := client.Instance().ListServers(testScalewayListRequest(), scw.WithContext(context.Background())); err != nil {
		t.Fatal(err)
	}
	if !redirected.Load() {
		t.Fatal("same-origin redirect was not followed")
	}
}

func TestScalewayClientIgnoresServerSuppliedRedirectMarker(t *testing.T) {
	var redirected atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/instance/v1/zones/fr-par-1/servers":
			w.Header().Set(scalewayRedirectMarkerHeader, scalewayRedirectCrossOrigin)
			http.Redirect(w, r, "/redirected?forged-location-secret=value", http.StatusTemporaryRedirect)
		case "/redirected":
			redirected.Store(true)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"servers": []any{}, "total_count": 0})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newTestScalewaySDKClient(t, server.URL, nil)
	if _, err := client.Instance().ListServers(testScalewayListRequest(), scw.WithContext(context.Background())); err != nil {
		t.Fatal(err)
	}
	if !redirected.Load() {
		t.Fatal("same-origin redirect with forged marker was not followed")
	}
}

func TestScalewayClientPreservesInsecureSDKProfile(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Auth-Token"); got != testScalewaySecretKey {
			t.Errorf("X-Auth-Token=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"servers": []any{}, "total_count": 0})
	}))
	defer server.Close()

	client := newTestScalewaySDKClientWithEnv(t, server.URL, nil, map[string]string{"SCW_INSECURE": "true"})
	if _, err := client.Instance().ListServers(testScalewayListRequest(), scw.WithContext(context.Background())); err != nil {
		t.Fatal(err)
	}
}

func TestScalewayClientPreservesSDKDebugLoggingWithoutTokenLeak(t *testing.T) {
	logs := &captureScalewayLogger{}
	scwlogger.SetLogger(logs)
	t.Cleanup(func() { scwlogger.SetLogger(scwlogger.DefaultLogger) })

	var sinkRequests atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		sinkRequests.Add(1)
	}))
	defer sink.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL+"/debug-stolen?debug-location-secret=value", http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	client := newTestScalewaySDKClient(t, server.URL, nil)
	_, err := client.Instance().ListServers(testScalewayListRequest(), scw.WithContext(context.Background()))
	if !errors.Is(err, errScalewayCrossOriginRedirect) {
		t.Fatalf("error=%v want cross-origin redirect refusal", err)
	}
	if got := sinkRequests.Load(); got != 0 {
		t.Fatalf("redirect sink received %d requests", got)
	}
	text := logs.String()
	for _, want := range []string{"Scaleway SDK REQUEST", "Scaleway SDK RESPONSE"} {
		if !strings.Contains(text, want) {
			t.Fatalf("SDK debug log missing %q: %s", want, text)
		}
	}
	for _, leaked := range []string{testScalewaySecretKey, "debug-location-secret", "debug-stolen", sink.URL} {
		if strings.Contains(text, leaked) {
			t.Fatalf("SDK debug log leaked %q: %s", leaked, text)
		}
	}
}

func TestScalewayClientPreservesCallerRedirectPolicy(t *testing.T) {
	wantErr := errors.New("caller stopped redirect")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	source := server.Client()
	source.CheckRedirect = func(*http.Request, []*http.Request) error { return wantErr }

	client := newTestScalewaySDKClient(t, server.URL, source)
	_, err := client.Instance().ListServers(testScalewayListRequest(), scw.WithContext(context.Background()))
	if !errors.Is(err, wantErr) {
		t.Fatalf("error=%v want caller redirect policy", err)
	}
}

func TestScalewayClientSanitizesRedirectLimit(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hop := requests.Add(1)
		http.Redirect(w, r, fmt.Sprintf("/redirect/%d?limit-secret=value#limit-fragment", hop), http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	client := newTestScalewaySDKClient(t, server.URL, nil)
	_, err := client.Instance().ListServers(testScalewayListRequest(), scw.WithContext(context.Background()))
	if !errors.Is(err, errScalewayRedirectLimit) {
		t.Fatalf("error=%v want redirect limit", err)
	}
	for _, leaked := range []string{"limit-secret", "limit-fragment", "/redirect/"} {
		if strings.Contains(err.Error(), leaked) {
			t.Fatalf("redirect limit error leaked %q: %v", leaked, err)
		}
	}
}

func TestScalewayClientSanitizesMalformedRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "http://redirect.example.test/%zz?location-secret=value")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	client := newTestScalewaySDKClient(t, server.URL, nil)
	_, err := client.Instance().ListServers(testScalewayListRequest(), scw.WithContext(context.Background()))
	if !errors.Is(err, errScalewayInvalidRedirect) {
		t.Fatalf("error=%v want invalid redirect refusal", err)
	}
	if strings.Contains(err.Error(), "location-secret") || strings.Contains(err.Error(), "%zz") {
		t.Fatalf("invalid redirect error leaked Location details: %v", err)
	}
}

func TestScalewayRedirectGuardUsesEffectiveOrigin(t *testing.T) {
	base, _ := url.Parse("https://api.scaleway.example.test")
	same, _ := url.Parse("https://api.scaleway.example.test:443/redirected")
	otherPort, _ := url.Parse("https://api.scaleway.example.test:444/redirected")
	otherScheme, _ := url.Parse("http://api.scaleway.example.test:443/redirected")
	if !core.SameHTTPOrigin(base, same) {
		t.Fatal("default HTTPS port should share origin")
	}
	if core.SameHTTPOrigin(base, otherPort) {
		t.Fatal("different effective port should be refused")
	}
	if core.SameHTTPOrigin(base, otherScheme) {
		t.Fatal("different scheme should be refused")
	}
}

func TestApplyScalewayOverridesPreservesSDKLocationWithoutExplicitCrabboxValue(t *testing.T) {
	profile := &scw.Profile{DefaultRegion: scw.StringPtr("nl-ams"), DefaultZone: scw.StringPtr("nl-ams-1")}
	cfg := core.Config{Scaleway: core.ScalewayConfig{Region: "fr-par", Zone: "fr-par-1"}}
	applyCrabboxScalewayOverrides(profile, cfg)
	if got := stringPtrValue(profile.DefaultRegion); got != "nl-ams" {
		t.Fatalf("region=%q", got)
	}
	if got := stringPtrValue(profile.DefaultZone); got != "nl-ams-1" {
		t.Fatalf("zone=%q", got)
	}
}

func TestApplyScalewayOverridesUsesExplicitCrabboxLocation(t *testing.T) {
	profile := &scw.Profile{DefaultRegion: scw.StringPtr("nl-ams"), DefaultZone: scw.StringPtr("nl-ams-1")}
	cfg := core.Config{Scaleway: core.ScalewayConfig{Region: "fr-par", Zone: "fr-par-1"}}
	core.SetScalewayRegionExplicit(&cfg)
	core.SetScalewayZoneExplicit(&cfg)
	applyCrabboxScalewayOverrides(profile, cfg)
	if got := stringPtrValue(profile.DefaultRegion); got != "fr-par" {
		t.Fatalf("region=%q", got)
	}
	if got := stringPtrValue(profile.DefaultZone); got != "fr-par-1" {
		t.Fatalf("zone=%q", got)
	}
}

func TestApplyScalewayLocationDefaultsOnlyFillsMissingSDKValues(t *testing.T) {
	profile := &scw.Profile{DefaultRegion: scw.StringPtr("nl-ams")}
	applyScalewayLocationDefaults(profile)
	if got := stringPtrValue(profile.DefaultRegion); got != "nl-ams" {
		t.Fatalf("region=%q", got)
	}
	if got := stringPtrValue(profile.DefaultZone); got != "fr-par-1" {
		t.Fatalf("zone=%q", got)
	}
}

func TestNewClientReportsMissingAuthWithoutSecrets(t *testing.T) {
	clearScalewayEnv(t)
	_, err := newClient(core.Config{}, core.Runtime{})
	if err == nil || !strings.Contains(err.Error(), "SCW_ACCESS_KEY and SCW_SECRET_KEY") {
		t.Fatalf("newClient err=%v", err)
	}
}

func TestNewClientReportsPartialAuthWithoutSecretValue(t *testing.T) {
	clearScalewayEnv(t)
	t.Setenv("SCW_ACCESS_KEY", "SCW11111111111111111")
	_, err := newClient(core.Config{}, core.Runtime{})
	if err == nil || !strings.Contains(err.Error(), "SCW_SECRET_KEY") {
		t.Fatalf("newClient err=%v", err)
	}
	if strings.Contains(err.Error(), "SCW11111111111111111") {
		t.Fatalf("partial auth error leaked access key: %v", err)
	}
}

func TestNewClientSanitizesSDKValidationError(t *testing.T) {
	for _, tc := range []struct {
		name, accessKey, secretKey, want string
	}{
		{"access key", "invalid-access-key", "invalid-secret-key", "Scaleway SDK client configuration failed: scaleway-sdk-go: invalid access key format '<redacted>', expected SCWXXXXXXXXXXXXXXXXX format"},
		{"secret key", testScalewayAccessKey, "invalid-secret-key", "Scaleway SDK client configuration failed: scaleway-sdk-go: invalid secret key format '<redacted>', expected a UUID: xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearScalewayEnv(t)
			t.Setenv("SCW_ACCESS_KEY", tc.accessKey)
			t.Setenv("SCW_SECRET_KEY", tc.secretKey)
			_, err := newClient(core.Config{Scaleway: core.ScalewayConfig{ProjectID: "project-1"}}, core.Runtime{})
			var cause *scw.InvalidClientOptionError
			if err == nil || err.Error() != tc.want || core.ExitCodeForError(err, 1) != 3 || !errors.As(err, &cause) {
				t.Fatalf("err=%v code=%d retained SDK cause=%v", err, core.ExitCodeForError(err, 1), cause != nil)
			}
			for _, secret := range []string{tc.accessKey, tc.secretKey} {
				if strings.Contains(err.Error(), secret) {
					t.Fatal("SDK error leaked a fixture credential")
				}
			}
		})
	}
}

func TestNewClientRetainsSanitizedSDKConfigCauses(t *testing.T) {
	for _, tc := range []struct {
		name, config string
		parseError   bool
	}{
		{name: "config load", config: "insecure: synthkey\n", parseError: true},
		{name: "active profile", config: "active_profile: synthkey\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearScalewayEnv(t)
			t.Setenv("SCW_ACCESS_KEY", "synthkey")
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.config), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("SCW_CONFIG_PATH", path)
			want := "Scaleway SDK active profile load failed: scaleway-sdk-go: given profile <redacted> does not exist"
			if tc.parseError {
				want = fmt.Sprintf("Scaleway SDK config load failed: scaleway-sdk-go: content of config file %s is invalid: yaml: unmarshal errors:\n  line 1: cannot unmarshal !!str `<redacted>` into bool", path)
			}
			_, err := newClient(core.Config{}, core.Runtime{})
			var cause *sdkerrors.Error
			if err == nil || err.Error() != want || core.ExitCodeForError(err, 1) != 3 || !errors.As(err, &cause) {
				t.Fatalf("err=%v code=%d retained SDK cause=%v", err, core.ExitCodeForError(err, 1), cause != nil)
			}
			if tc.parseError && (cause.Err == nil || !errors.Is(err, cause.Err)) {
				t.Fatal("underlying YAML error identity lost")
			}
			if !tc.parseError && cause.Str != "given profile synthkey does not exist" {
				t.Fatal("active profile cause changed")
			}
			if strings.Contains(err.Error(), "synthkey") {
				t.Fatal("public config error leaked fixture credential")
			}
		})
	}
}

func TestNewClientRejectsUnsupportedDefaultTransport(t *testing.T) {
	clearScalewayEnv(t)
	t.Setenv("SCW_ACCESS_KEY", testScalewayAccessKey)
	t.Setenv("SCW_SECRET_KEY", testScalewaySecretKey)
	t.Setenv("SCW_DEFAULT_PROJECT_ID", testScalewayProjectID)
	t.Setenv("SCW_DEFAULT_ORGANIZATION_ID", testScalewayOrganizationID)
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	calls := 0
	http.DefaultTransport = scalewayRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("deny all")
	})

	client, err := newClient(core.Config{}, core.Runtime{})
	if client != nil || err == nil || !strings.Contains(err.Error(), "non-nil *http.Transport") {
		t.Fatalf("client=%#v err=%v, want transport setup error", client, err)
	}
	if calls != 0 {
		t.Fatalf("custom default invoked %d times, want 0", calls)
	}
}

func TestNewClientAcceptsExplicitHTTPClientWithUnsupportedDefault(t *testing.T) {
	clearScalewayEnv(t)
	t.Setenv("SCW_ACCESS_KEY", testScalewayAccessKey)
	t.Setenv("SCW_SECRET_KEY", testScalewaySecretKey)
	t.Setenv("SCW_DEFAULT_PROJECT_ID", testScalewayProjectID)
	t.Setenv("SCW_DEFAULT_ORGANIZATION_ID", testScalewayOrganizationID)
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = scalewayRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("deny all")
	})
	injected := &http.Client{Transport: scalewayRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("injected")
	})}

	client, err := newClient(core.Config{}, core.Runtime{HTTP: injected})
	if err != nil {
		t.Fatal(err)
	}
	if client == nil {
		t.Fatal("newClient returned nil client")
	}
}

func TestSanitizeSDKErrorRedactsProfileValues(t *testing.T) {
	const accessKey = "invalid-profile-access"
	const secretKey = "invalid-profile-secret"
	text := sanitizeSDKError(errors.New("invalid access "+accessKey+" and secret "+secretKey), accessKey, secretKey)
	for _, secret := range []string{accessKey, secretKey} {
		if strings.Contains(text, secret) {
			t.Fatalf("SDK profile error leaked %q: %s", secret, text)
		}
	}
	if !strings.Contains(text, "<redacted>") {
		t.Fatalf("SDK profile error did not include redaction marker: %s", text)
	}
}

type scalewayRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn scalewayRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestScalewayListPropagatesContextToSDKRequest(t *testing.T) {
	clearScalewayEnv(t)
	t.Setenv("SCW_ACCESS_KEY", "SCW11111111111111111")
	t.Setenv("SCW_SECRET_KEY", "11111111-1111-1111-1111-111111111111")
	t.Setenv("SCW_DEFAULT_PROJECT_ID", "11111111-1111-1111-1111-111111111111")
	t.Setenv("SCW_DEFAULT_ORGANIZATION_ID", "22222222-2222-2222-2222-222222222222")

	type contextKey struct{}
	ctx := context.WithValue(context.Background(), contextKey{}, "scaleway-context")
	var got any
	sentinel := errors.New("stop after context inspection")
	rt := core.Runtime{HTTP: &http.Client{Transport: scalewayRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		got = req.Context().Value(contextKey{})
		return nil, sentinel
	})}}
	backend := &Backend{spec: Provider{}.Spec(), cfg: core.Config{}, rt: rt, newClient: newClient}
	_, err := backend.List(ctx, core.ListRequest{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("List err=%v, want sentinel", err)
	}
	if got != "scaleway-context" {
		t.Fatalf("SDK request context value=%v", got)
	}
}

func clearScalewayEnv(t *testing.T) {
	t.Helper()
	testutil.IsolateUserDirs(t)
	unsetScalewayEnv(t, "SCW_API_URL")
	unsetScalewayEnv(t, "SCW_INSECURE")
	for _, key := range []string{
		"SCW_ACCESS_KEY",
		"SCW_SECRET_KEY",
		"SCW_DEFAULT_ORGANIZATION_ID",
		"SCW_DEFAULT_PROJECT_ID",
		"SCW_DEFAULT_REGION",
		"SCW_DEFAULT_ZONE",
		"SCW_PROFILE",
		"SCW_CONFIG_PATH",
		"CRABBOX_SCALEWAY_PROJECT_ID",
		"CRABBOX_SCALEWAY_ORGANIZATION_ID",
		"CRABBOX_SCALEWAY_REGION",
		"CRABBOX_SCALEWAY_ZONE",
	} {
		t.Setenv(key, "")
	}
}

func unsetScalewayEnv(t *testing.T, key string) {
	t.Helper()
	original, existed := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(key, original)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

const (
	testScalewayAccessKey      = "SCW11111111111111111"
	testScalewaySecretKey      = "11111111-1111-1111-1111-111111111111"
	testScalewayProjectID      = "22222222-2222-2222-2222-222222222222"
	testScalewayOrganizationID = "33333333-3333-3333-3333-333333333333"
)

func newTestScalewaySDKClient(t *testing.T, apiURL string, httpClient *http.Client) Client {
	return newTestScalewaySDKClientWithEnv(t, apiURL, httpClient, nil)
}

func TestScalewayResponseErrorUsesCompletedSDKResponses(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		want       bool
	}{
		{"standard", `{"type":"permissions_denied","message":"fixture denied"}`, http.StatusForbidden, true},
		{"legacy", `{"type":"unknown_resource","message":"fixture missing"}`, http.StatusNotFound, true},
		{"fallback", `{"type":"fixture_unknown","message":"fixture response"}`, http.StatusTeapot, true},
		{"malformed", `{`, http.StatusForbidden, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client := newTestScalewaySDKClient(t, server.URL, server.Client())
			_, err := client.Instance().GetServer(&instance.GetServerRequest{Zone: scw.Zone(client.Zone()), ServerID: "srv-1"}, scw.WithContext(t.Context()))
			if err == nil {
				t.Fatal("expected SDK error")
			}
			// A generic SDK wrapper must not hide a completed response beneath it.
			wrapped := errors.Join(sdkerrors.Wrap(err, "observation"), context.Canceled)
			if got := isScalewayResponseError(wrapped); got != tc.want {
				t.Fatalf("response=%t want=%t error=%T %v", got, tc.want, err, err)
			}
		})
	}
	if isScalewayResponseError(sdkerrors.Wrap(context.Canceled, "transport interrupted")) {
		t.Fatal("SDK transport wrapper is not a completed response")
	}
}

func newTestScalewaySDKClientWithEnv(t *testing.T, apiURL string, httpClient *http.Client, env map[string]string) Client {
	t.Helper()
	clearScalewayEnv(t)
	t.Setenv("SCW_ACCESS_KEY", testScalewayAccessKey)
	t.Setenv("SCW_SECRET_KEY", testScalewaySecretKey)
	t.Setenv("SCW_API_URL", apiURL)
	t.Setenv("SCW_DEFAULT_PROJECT_ID", testScalewayProjectID)
	t.Setenv("SCW_DEFAULT_ORGANIZATION_ID", testScalewayOrganizationID)
	for key, value := range env {
		t.Setenv(key, value)
	}
	client, err := newClient(core.Config{}, core.Runtime{HTTP: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func testScalewayListRequest() *instance.ListServersRequest {
	return &instance.ListServersRequest{
		Zone:    scw.Zone("fr-par-1"),
		Project: scw.StringPtr(testScalewayProjectID),
	}
}

type captureScalewayLogger struct {
	mu   sync.Mutex
	text strings.Builder
}

func (l *captureScalewayLogger) Debugf(format string, args ...any)   { l.write(format, args...) }
func (l *captureScalewayLogger) Infof(format string, args ...any)    { l.write(format, args...) }
func (l *captureScalewayLogger) Warningf(format string, args ...any) { l.write(format, args...) }
func (l *captureScalewayLogger) Errorf(format string, args ...any)   { l.write(format, args...) }
func (*captureScalewayLogger) ShouldLog(scwlogger.LogLevel) bool     { return true }

func (l *captureScalewayLogger) write(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = fmt.Fprintf(&l.text, format, args...)
}

func (l *captureScalewayLogger) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.text.String()
}

func TestScalewayBindingProfilePrecedence(t *testing.T) {
	for _, tc := range []struct {
		region, zone         string
		explicit             bool
		wantRegion, wantZone string
	}{{"fr-par", "fr-par-1", false, "nl-ams", "nl-ams-1"}, {"fr-par", "fr-par-1", true, "fr-par", "fr-par-1"}, {"  ", "  ", true, "fr-par", "fr-par-1"}, {"", "", true, "fr-par", "fr-par-1"}, {"  ", "  ", false, "nl-ams", "nl-ams-1"}, {" pl-waw ", " pl-waw-2 ", true, "pl-waw", "pl-waw-2"}} {
		cfg := core.Config{Scaleway: core.ScalewayConfig{Region: tc.region, Zone: tc.zone, Image: "  ", Type: "  ", ProjectID: " project-fixture ", OrganizationID: " org-fixture "}, Class: "standard"}
		if tc.explicit {
			core.SetScalewayRegionExplicit(&cfg)
			core.SetScalewayZoneExplicit(&cfg)
		}
		b := Backend{cfg: cfg}
		effective := b.cfgForRun()
		if effective.Scaleway.Image != "ubuntu_noble" || effective.Scaleway.Type != "DEV1-S" || core.ScalewayRegionWasExplicit(effective) != tc.explicit || core.ScalewayZoneWasExplicit(effective) != tc.explicit || b.cfg.Scaleway.Region != tc.region {
			t.Fatal("effective fallback/marker/copy phase changed")
		}
		profile := &scw.Profile{DefaultRegion: scw.StringPtr("nl-ams"), DefaultZone: scw.StringPtr("nl-ams-1"), DefaultProjectID: scw.StringPtr("project-prior"), DefaultOrganizationID: scw.StringPtr("org-prior")}
		applyCrabboxScalewayOverrides(profile, effective)
		applyScalewayLocationDefaults(profile)
		if stringPtrValue(profile.DefaultRegion) != tc.wantRegion || stringPtrValue(profile.DefaultZone) != tc.wantZone || stringPtrValue(profile.DefaultProjectID) != "project-fixture" || stringPtrValue(profile.DefaultOrganizationID) != "org-fixture" {
			t.Fatalf("profile region=%q zone=%q", stringPtrValue(profile.DefaultRegion), stringPtrValue(profile.DefaultZone))
		}
		if tc.explicit && strings.TrimSpace(tc.region) == "" {
			direct := &scw.Profile{DefaultRegion: scw.StringPtr("nl-ams"), DefaultZone: scw.StringPtr("nl-ams-1")}
			applyCrabboxScalewayOverrides(direct, cfg)
			if stringPtrValue(direct.DefaultRegion) != "nl-ams" || stringPtrValue(direct.DefaultZone) != "nl-ams-1" {
				t.Fatal("raw blank override should defer before effective fallback")
			}
		}
	}
	for _, tc := range []struct {
		region, zone         *string
		wantRegion, wantZone string
	}{{nil, nil, "fr-par", "fr-par-1"}, {nil, scw.StringPtr("nl-ams-2"), "fr-par", "nl-ams-2"}, {scw.StringPtr("nl-ams"), nil, "nl-ams", "fr-par-1"}, {scw.StringPtr("  "), scw.StringPtr("nl-ams-2"), "fr-par", "nl-ams-2"}} {
		profile := &scw.Profile{DefaultRegion: tc.region, DefaultZone: tc.zone}
		applyScalewayLocationDefaults(profile)
		if stringPtrValue(profile.DefaultRegion) != tc.wantRegion || stringPtrValue(profile.DefaultZone) != tc.wantZone {
			t.Fatal("independent profile defaults changed")
		}
	}
	p := Provider{}
	if p.ServerTypeForConfig(core.Config{Class: "standard"}) != "DEV1-S" || p.ServerTypeForConfig(core.Config{Class: "unknown"}) != "DEV1-S" {
		t.Fatal("fixed class fallback changed")
	}
	for _, raw := range []string{"", "  "} {
		cfg := core.Config{Scaleway: core.ScalewayConfig{Region: raw, Zone: raw, Image: raw}}
		if regionForConfig(cfg) != "fr-par" || zoneForConfig(cfg) != "fr-par-1" || imageForConfig(cfg) != "ubuntu_noble" {
			t.Fatal("trimmed config fallback changed")
		}
	}
}
