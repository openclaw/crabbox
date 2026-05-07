package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestEgressHostAllowedMatchesExactAndWildcards(t *testing.T) {
	allow := []string{"discord.com", "*.discordcdn.com"}
	for _, host := range []string{"discord.com", "cdn.discordcdn.com", "media.cdn.discordcdn.com"} {
		if !egressHostAllowed(host, allow) {
			t.Fatalf("expected %s to be allowed", host)
		}
	}
	for _, host := range []string{"example.com", "discord.com.evil.test"} {
		if egressHostAllowed(host, allow) {
			t.Fatalf("expected %s to be rejected", host)
		}
	}
}

func TestEgressAllowlistRejectsBareWildcard(t *testing.T) {
	allow := egressAllowlist("", []string{"*"})
	if len(allow) != 0 {
		t.Fatalf("bare wildcard allowlist=%v, want empty", allow)
	}
	if egressHostAllowed("example.com", []string{"*"}) {
		t.Fatal("bare wildcard should not allow every host")
	}
}

func TestValidateEgressListenRequiresLoopback(t *testing.T) {
	for _, listen := range []string{"127.0.0.1:3128", "localhost:3128", "[::1]:3128"} {
		if err := validateEgressListen(listen); err != nil {
			t.Fatalf("expected %s to be valid: %v", listen, err)
		}
	}
	for _, listen := range []string{"0.0.0.0:3128", ":3128", "192.168.1.10:3128", "[::]:3128"} {
		if err := validateEgressListen(listen); err == nil {
			t.Fatalf("expected %s to be rejected", listen)
		}
	}
}

func TestEgressCoordinatorNeedsAccess(t *testing.T) {
	if egressCoordinatorNeedsAccess(AccessConfig{}) {
		t.Fatal("empty access config should not block egress start")
	}
	for _, access := range []AccessConfig{
		{ClientID: "client"},
		{ClientSecret: "secret"},
		{Token: "jwt"},
	} {
		if !egressCoordinatorNeedsAccess(access) {
			t.Fatalf("access config should block egress start: %#v", access)
		}
	}
}

func TestEgressStartCoordinatorOverrideUsesPublicRoute(t *testing.T) {
	cfg := Config{
		Coordinator: "https://crabbox-access.openclaw.ai",
		Access:      AccessConfig{ClientID: "client", ClientSecret: "secret", Token: "jwt"},
	}
	got, err := egressStartCoordinatorConfig(cfg, "https://crabbox.openclaw.ai/")
	if err != nil {
		t.Fatal(err)
	}
	if got.Coordinator != "https://crabbox.openclaw.ai" {
		t.Fatalf("coordinator=%q", got.Coordinator)
	}
	if egressCoordinatorNeedsAccess(got.Access) {
		t.Fatalf("override should clear access headers for remote-safe start: %#v", got.Access)
	}
	if _, err := egressStartCoordinatorConfig(cfg, ""); err == nil {
		t.Fatal("expected access-protected coordinator without override to be rejected")
	}
}

func TestEgressClientBinaryRejectsNonLinuxTargets(t *testing.T) {
	_, cleanup, err := egressClientBinaryForTarget(context.Background(), SSHTarget{TargetOS: targetWindows})
	defer cleanup()
	if err == nil {
		t.Fatal("expected non-Linux egress target to be rejected")
	}
	if !strings.Contains(err.Error(), "only supports Linux lease targets") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestManualEgressTicketCreationReusesActiveSession(t *testing.T) {
	var ticketBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/leases/cbx_abcdef123456/egress/status":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"leaseID":   "cbx_abcdef123456",
				"sessionID": "egress_shared123",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/leases/cbx_abcdef123456/egress/ticket":
			if err := json.NewDecoder(r.Body).Decode(&ticketBody); err != nil {
				t.Fatalf("decode ticket body: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ticket":    "egress_ticket",
				"leaseID":   "cbx_abcdef123456",
				"role":      "client",
				"sessionID": ticketBody["sessionID"],
				"expiresAt": "2026-05-07T00:00:00Z",
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	coord := &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	sessionID, err := reusableEgressSessionID(context.Background(), coord, "cbx_abcdef123456", "")
	if err != nil {
		t.Fatal(err)
	}
	if sessionID != "egress_shared123" {
		t.Fatalf("sessionID=%q", sessionID)
	}
	if _, err := coord.CreateEgressTicket(context.Background(), "cbx_abcdef123456", "client", sessionID, "", nil); err != nil {
		t.Fatal(err)
	}
	if ticketBody["sessionID"] != "egress_shared123" {
		t.Fatalf("ticket sessionID=%v", ticketBody["sessionID"])
	}
}

func TestFatalEgressBridgeSetupError(t *testing.T) {
	fatalStatuses := []int{http.StatusForbidden, http.StatusNotFound, http.StatusGone, http.StatusConflict}
	for _, status := range fatalStatuses {
		err := CoordinatorHTTPError{StatusCode: status}
		if !fatalEgressBridgeSetupError(err) {
			t.Fatalf("status %d should stop stale egress bridge retries", status)
		}
	}
	if fatalEgressBridgeSetupError(CoordinatorHTTPError{StatusCode: http.StatusTooManyRequests}) {
		t.Fatal("transient coordinator errors should stay retryable")
	}
}

func TestEgressDaemonSupervisorStopsOnFatalExit(t *testing.T) {
	script := egressDaemonSupervisorScript("crabbox", []string{"egress", "host"})
	for _, want := range []string{
		`if [ "$code" = 4 ]; then`,
		`egress daemon supervisor: child exited fatal code=$code; stopping`,
		`exit "$code"`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("supervisor script missing %q:\n%s", want, script)
		}
	}
}

func TestEgressRequestHostPort(t *testing.T) {
	connect := &http.Request{Method: http.MethodConnect, Host: "discord.com:443"}
	host, port, err := egressRequestHostPort(connect)
	if err != nil {
		t.Fatal(err)
	}
	if host != "discord.com" || port != "443" {
		t.Fatalf("CONNECT host/port=%s/%s", host, port)
	}

	absolute := &http.Request{
		Method: http.MethodGet,
		Host:   "proxy.local",
		URL:    &url.URL{Scheme: "http", Host: "example.com", Path: "/"},
	}
	host, port, err = egressRequestHostPort(absolute)
	if err != nil {
		t.Fatal(err)
	}
	if host != "example.com" || port != "80" {
		t.Fatalf("absolute URL host/port=%s/%s", host, port)
	}
}

func TestEgressAgentURL(t *testing.T) {
	got := egressAgentURL("https://crabbox.openclaw.ai", "cbx_abcdef123456", "host", "egress_abc")
	want := "wss://crabbox.openclaw.ai/v1/leases/cbx_abcdef123456/egress/host?ticket=egress_abc"
	if got != want {
		t.Fatalf("egressAgentURL=%q want %q", got, want)
	}
}

func TestRemoteEgressClientCommandRedactsThroughShellQuoting(t *testing.T) {
	got := remoteEgressClientCommand("https://crabbox.openclaw.ai", "cbx_abcdef123456", "egress_ticket", "egress_session", "127.0.0.1:3128")
	for _, want := range []string{
		"pkill -f '[c]rabbox-egress-client egress client'",
		"'/tmp/crabbox-egress-client' 'egress' 'client'",
		"'--coordinator' 'https://crabbox.openclaw.ai'",
		"'--ticket' 'egress_ticket'",
		">'/tmp/crabbox-egress-client.log' 2>&1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("remote command missing %q:\n%s", want, got)
		}
	}
}
