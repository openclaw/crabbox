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
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	instance "github.com/scaleway/scaleway-sdk-go/api/instance/v1"
	scwlogger "github.com/scaleway/scaleway-sdk-go/logger"
	"github.com/scaleway/scaleway-sdk-go/scw"

	core "github.com/openclaw/crabbox/internal/cli"
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
	if !sameScalewayOrigin(base, same) {
		t.Fatal("default HTTPS port should share origin")
	}
	if sameScalewayOrigin(base, otherPort) {
		t.Fatal("different effective port should be refused")
	}
	if sameScalewayOrigin(base, otherScheme) {
		t.Fatal("different scheme should be refused")
	}
}

func TestApplyScalewayOverridesPreservesSDKLocationWithoutExplicitCrabboxValue(t *testing.T) {
	profile := &scw.Profile{DefaultRegion: scw.StringPtr("nl-ams"), DefaultZone: scw.StringPtr("nl-ams-1")}
	cfg := core.Config{Scaleway: core.ScalewayConfig{Region: defaultRegion, Zone: defaultZone}}
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
	cfg := core.Config{Scaleway: core.ScalewayConfig{Region: defaultRegion, Zone: defaultZone}}
	core.SetScalewayRegionExplicit(&cfg)
	core.SetScalewayZoneExplicit(&cfg)
	applyCrabboxScalewayOverrides(profile, cfg)
	if got := stringPtrValue(profile.DefaultRegion); got != defaultRegion {
		t.Fatalf("region=%q", got)
	}
	if got := stringPtrValue(profile.DefaultZone); got != defaultZone {
		t.Fatalf("zone=%q", got)
	}
}

func TestApplyScalewayLocationDefaultsOnlyFillsMissingSDKValues(t *testing.T) {
	profile := &scw.Profile{DefaultRegion: scw.StringPtr("nl-ams")}
	applyScalewayLocationDefaults(profile)
	if got := stringPtrValue(profile.DefaultRegion); got != "nl-ams" {
		t.Fatalf("region=%q", got)
	}
	if got := stringPtrValue(profile.DefaultZone); got != defaultZone {
		t.Fatalf("zone=%q", got)
	}
}

func TestNewClientReportsMissingAuthWithoutSecrets(t *testing.T) {
	clearScalewayEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, err := newClient(core.Config{}, core.Runtime{})
	if err == nil || !strings.Contains(err.Error(), "SCW_ACCESS_KEY and SCW_SECRET_KEY") {
		t.Fatalf("newClient err=%v", err)
	}
}

func TestNewClientReportsPartialAuthWithoutSecretValue(t *testing.T) {
	clearScalewayEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
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
	clearScalewayEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("SCW_ACCESS_KEY", "invalid-access-key")
	t.Setenv("SCW_SECRET_KEY", "invalid-secret-key")
	t.Setenv("CRABBOX_SCALEWAY_PROJECT_ID", "project-1")
	_, err := newClient(core.Config{Scaleway: core.ScalewayConfig{ProjectID: "project-1"}}, core.Runtime{})
	if err == nil {
		t.Fatal("newClient unexpectedly succeeded")
	}
	text := err.Error()
	for _, secret := range []string{"invalid-access-key", "invalid-secret-key"} {
		if strings.Contains(text, secret) {
			t.Fatalf("SDK error leaked %q: %v", secret, err)
		}
	}
	if !strings.Contains(text, "<redacted>") {
		t.Fatalf("SDK error did not include redaction marker: %v", err)
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
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
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

func newTestScalewaySDKClientWithEnv(t *testing.T, apiURL string, httpClient *http.Client, env map[string]string) Client {
	t.Helper()
	clearScalewayEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
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
