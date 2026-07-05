package cli

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"
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

func TestPublicEgressAddressRejectsPrivateAndReservedRanges(t *testing.T) {
	tests := map[string]bool{
		"8.8.8.8":                true,
		"2606:4700:4700::1111":   true,
		"0.0.0.0":                false,
		"10.0.0.1":               false,
		"100.64.0.1":             false,
		"127.0.0.1":              false,
		"169.254.169.254":        false,
		"192.0.2.1":              false,
		"192.168.1.1":            false,
		"198.18.0.1":             false,
		"224.0.0.1":              false,
		"::1":                    false,
		"64:ff9b::808:808":       true,
		"64:ff9b::a00:1":         false,
		"64:ff9b::a9fe:a9fe":     false,
		"64:ff9b:1::1":           false,
		"2001:db8::1":            false,
		"fc00::1":                false,
		"fe80::1":                false,
		"::ffff:127.0.0.1":       false,
		"::ffff:169.254.169.254": false,
	}
	for raw, want := range tests {
		t.Run(raw, func(t *testing.T) {
			if got := publicEgressAddress(netip.MustParseAddr(raw)); got != want {
				t.Fatalf("publicEgressAddress(%s)=%t, want %t", raw, got, want)
			}
		})
	}
}

func TestDialPublicEgressHostPinsValidatedAddress(t *testing.T) {
	var dialed string
	lookup := func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{
			netip.MustParseAddr("127.0.0.1"),
			netip.MustParseAddr("8.8.8.8"),
			netip.MustParseAddr("8.8.8.8"),
		}, nil
	}
	dial := func(_ context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" {
			t.Fatalf("network=%q, want tcp", network)
		}
		dialed = address
		conn, peer := net.Pipe()
		_ = peer.Close()
		return conn, nil
	}

	conn, err := dialPublicEgressHostWith(context.Background(), "allowed.example", "443", lookup, dial)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if dialed != "8.8.8.8:443" {
		t.Fatalf("dialed=%q, want pinned public address", dialed)
	}
}

func TestDialPublicEgressHostRejectsPrivateResolutionBeforeDial(t *testing.T) {
	dialed := false
	lookup := func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
	}
	dial := func(context.Context, string, string) (net.Conn, error) {
		dialed = true
		return nil, nil
	}

	_, err := dialPublicEgressHostWith(context.Background(), "allowed.example", "80", lookup, dial)
	if err == nil || !strings.Contains(err.Error(), "public address") {
		t.Fatalf("err=%v, want public-address rejection", err)
	}
	if dialed {
		t.Fatal("private resolved address reached dialer")
	}
}

func TestDialPublicEgressHostRejectsPrivateIPLiteralWithoutDNS(t *testing.T) {
	lookedUp := false
	dialed := false
	lookup := func(context.Context, string, string) ([]netip.Addr, error) {
		lookedUp = true
		return nil, nil
	}
	dial := func(context.Context, string, string) (net.Conn, error) {
		dialed = true
		return nil, nil
	}

	_, err := dialPublicEgressHostWith(context.Background(), "127.0.0.1", "8080", lookup, dial)
	if err == nil || !strings.Contains(err.Error(), "public address") {
		t.Fatalf("err=%v, want public-address rejection", err)
	}
	if lookedUp || dialed {
		t.Fatalf("private IP literal lookedUp=%t dialed=%t", lookedUp, dialed)
	}
}

func TestLivePublicEgressDial(t *testing.T) {
	if os.Getenv("CRABBOX_LIVE") != "1" {
		t.Skip("set CRABBOX_LIVE=1 to exercise public DNS and TCP egress")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := dialPublicEgressHost(ctx, "example.com", "443")
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
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
		Coordinator: "https://broker-access.example.com",
		Access:      AccessConfig{ClientID: "client", ClientSecret: "secret", Token: "jwt"},
	}
	got, err := egressStartCoordinatorConfig(cfg, "https://broker.example.com/")
	if err != nil {
		t.Fatal(err)
	}
	if got.Coordinator != "https://broker.example.com" {
		t.Fatalf("coordinator=%q", got.Coordinator)
	}
	if egressCoordinatorNeedsAccess(got.Access) {
		t.Fatalf("override should clear access headers for remote-safe start: %#v", got.Access)
	}
	if _, err := egressStartCoordinatorConfig(cfg, ""); err == nil {
		t.Fatal("expected access-protected coordinator without override to be rejected")
	}
}

func TestExplicitEgressTicketDoesNotCreateFakeBearer(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CRABBOX_CONFIG", home+"/missing.yaml")

	coord, leaseID, err := (App{}).egressCoordinatorAndLease(
		context.Background(),
		"aws",
		"https://broker.example.com",
		"cbx_abcdef123456",
		"egress_ticket",
	)
	if err != nil {
		t.Fatal(err)
	}
	if leaseID != "cbx_abcdef123456" {
		t.Fatalf("leaseID=%q", leaseID)
	}
	if coord.hasConfiguredAuth() {
		t.Fatal("explicit-ticket mode should not synthesize coordinator credentials")
	}
	headers, err := coord.webVNCAccessHeaders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := headers.Get("Authorization"); got != "" {
		t.Fatalf("Authorization=%q, want empty", got)
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

func TestSCPBaseArgsUseLegacyProtocolForNativeWindows(t *testing.T) {
	native := scpBaseArgs(SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal})
	if !slices.Contains(native, "-O") {
		t.Fatalf("native Windows scp args should include -O for OpenSSH servers without SFTP subsystem: %v", native)
	}
	wsl2 := scpBaseArgs(SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2})
	if slices.Contains(wsl2, "-O") {
		t.Fatalf("WSL2 scp args should not force legacy protocol: %v", wsl2)
	}
	linux := scpBaseArgs(SSHTarget{TargetOS: targetLinux})
	if slices.Contains(linux, "-O") {
		t.Fatalf("Linux scp args should not force legacy protocol: %v", linux)
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
	got := egressAgentURL("https://broker.example.com", "cbx_abcdef123456", "host")
	want := "wss://broker.example.com/v1/leases/cbx_abcdef123456/egress/host"
	if got != want {
		t.Fatalf("egressAgentURL=%q want %q", got, want)
	}
}

func TestConnectEgressBridgeSendsTicketInDedicatedHeader(t *testing.T) {
	agentConnected := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/leases/cbx_abcdef123456/egress/host" {
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query().Get("ticket"); got != "" {
			t.Errorf("query ticket=%q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer coordinator-token" {
			t.Errorf("authorization=%q", got)
		}
		if got := r.Header.Get("X-Crabbox-Bridge-Ticket"); got != "egress_abcdef1234567890abcdef1234567890" {
			t.Errorf("bridge ticket=%q", got)
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("websocket accept: %v", err)
			return
		}
		close(agentConnected)
		_, _, _ = conn.Read(context.Background())
		_ = conn.Close(websocket.StatusNormalClosure, "test done")
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	coord := &CoordinatorClient{BaseURL: server.URL, Token: "coordinator-token", Client: server.Client()}
	bridge, err := connectEgressBridge(
		ctx,
		coord,
		"cbx_abcdef123456",
		"host",
		"egress_abcdef1234567890abcdef1234567890",
		"egress_session",
		"",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.close()

	select {
	case <-agentConnected:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestRemoteEgressClientCommandRedactsThroughShellQuoting(t *testing.T) {
	got := remoteEgressClientCommand("https://broker.example.com", "cbx_abcdef123456", "egress_ticket", "egress_session", "127.0.0.1:3128")
	for _, want := range []string{
		"pkill -f '[c]rabbox-egress-client egress client'",
		"'/tmp/crabbox-egress-client' 'egress' 'client'",
		"'--coordinator' 'https://broker.example.com'",
		"'--ticket' 'egress_ticket'",
		">'/tmp/crabbox-egress-client.log' 2>&1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("remote command missing %q:\n%s", want, got)
		}
	}
}

func TestEgressStopUsesSharedRemoteClientStopCommand(t *testing.T) {
	got := remoteEgressClientCommand("https://broker.example.com", "cbx_abcdef123456", "egress_ticket", "egress_session", "127.0.0.1:3128")
	want := remoteStopEgressClientCommand()
	if !strings.HasPrefix(got, want+"\n") {
		t.Fatalf("remote start command should stop existing client with shared command %q:\n%s", want, got)
	}
	if want != "pkill -f '[c]rabbox-egress-client egress client' >/dev/null 2>&1 || true" {
		t.Fatalf("unexpected remote stop command: %q", want)
	}
}
