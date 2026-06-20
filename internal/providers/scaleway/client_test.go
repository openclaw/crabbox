package scaleway

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/scaleway/scaleway-sdk-go/scw"

	core "github.com/openclaw/crabbox/internal/cli"
)

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
