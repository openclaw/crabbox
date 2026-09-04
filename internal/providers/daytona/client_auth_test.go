package daytona

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openclaw/crabbox/internal/testutil"
)

func TestDaytonaCLIAuthUsesOAuthProfileForAPIAndToolbox(t *testing.T) {
	testutil.IsolateUserDirs(t)
	dir := t.TempDir()
	t.Setenv("DAYTONA_CONFIG_DIR", dir)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests++
		if req.URL.Path != "/api/sandbox" {
			t.Errorf("request path=%s, want active profile endpoint", req.URL.Path)
		}
		if req.Header.Get("Authorization") != "Bearer synthetic-access" || req.Header.Get("X-Daytona-Organization-ID") != "org-browser" {
			t.Error("API request did not use the active profile's access token and organization")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[],"nextCursor":null}`)
	}))
	defer server.Close()
	profile := []byte(fmt.Sprintf(`{"activeProfile":"browser","profiles":[
		{"id":"unused","api":{"key":"wrong-key"}},
		{"id":"browser","activeOrganizationId":"org-browser","api":{
			"url":%q,"key":null,
			"token":{"accessToken":"synthetic-access","refreshToken":"synthetic-refresh","expiresAt":"2099-01-01T00:00:00Z"}
		}}
	]}`, server.URL+"/api"))
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, profile, 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := newDaytonaClient(Config{}, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListCrabboxSandboxes(t.Context()); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("requests=%d, want 1", requests)
	}
	headers, err := daytonaToolboxHeaders(Config{})
	if err != nil || headers["Authorization"] != "Bearer synthetic-access" || headers["X-Daytona-Organization-ID"] != "org-browser" {
		t.Fatalf("toolbox did not use the active OAuth profile: %v", err)
	}
	if after, err := os.ReadFile(path); err != nil || string(after) != string(profile) {
		t.Fatalf("Crabbox modified the CLI-owned profile: %v", err)
	}
}

func TestParseDaytonaCLIAuthConfigOAuthExpiryAndAPIKeyPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name, key, access, expiry, wantToken string
		wantError                            bool
	}{
		{name: "fresh OAuth", access: "access", expiry: "2099-01-01T00:00:00Z", wantToken: "access"},
		{name: "expired OAuth", access: "access", expiry: "2000-01-01T00:00:00Z", wantError: true},
		{name: "missing expiry", access: "access", wantError: true},
		{name: "invalid expiry", access: "access", expiry: "invalid-expiry", wantError: true},
		{name: "API key precedes expired OAuth", key: "api-key", access: "access", expiry: "2000-01-01T00:00:00Z", wantToken: "api-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte(fmt.Sprintf(`{"activeProfile":"browser","profiles":[{"id":"browser","activeOrganizationId":"org-browser","api":{"key":%q,"token":{"accessToken":%q,"refreshToken":"refresh-secret","expiresAt":%q}}}]}`, tc.key, tc.access, tc.expiry))
			auth, err := parseDaytonaCLIAuthConfig(data)
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), "daytona login") {
					t.Fatalf("error=%v, want actionable CLI reauthentication", err)
				}
				if auth.token() != "" || strings.Contains(err.Error(), "refresh-secret") || strings.Contains(err.Error(), "invalid-expiry") {
					t.Fatal("invalid OAuth profile exposed credentials or returned usable auth")
				}
				return
			}
			if err != nil || auth.token() != tc.wantToken || auth.OrganizationID != "org-browser" {
				t.Fatalf("profile auth did not preserve credential precedence: %v", err)
			}
		})
	}
}

func TestDaytonaCLIConfigDirectoryDoesNotFallBackToAnotherProfile(t *testing.T) {
	testutil.IsolateUserDirs(t)
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	defaultDir := filepath.Join(configDir, "daytona")
	if err := os.MkdirAll(defaultDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(defaultDir, "config.json"), []byte(`{"profiles":[{"api":{"key":"wrong-account-key"}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DAYTONA_CONFIG_DIR", t.TempDir())
	if _, err := daytonaAuthConfig(Config{}); err == nil {
		t.Fatal("missing selected config directory fell back to another account")
	}
	for _, tokenKind := range []string{"API key", "JWT"} {
		t.Run(tokenKind, func(t *testing.T) {
			cfg := Config{}
			cfg.Daytona.OrganizationID = "explicit-org"
			if tokenKind == "API key" {
				cfg.Daytona.APIKey = "explicit-token"
			} else {
				cfg.Daytona.JWTToken = "explicit-token"
			}
			auth, err := daytonaAuthConfig(cfg)
			if err != nil || auth.token() != "explicit-token" || auth.OrganizationID != "explicit-org" {
				t.Fatalf("explicit auth did not bypass the CLI profile: %v", err)
			}
		})
	}
}
