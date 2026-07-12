package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func init() {
	RegisterProvider(directWebVNCTestProvider{})
}

func TestWebVNCURLs(t *testing.T) {
	if got := webVNCAgentURL("https://broker.example.com", "cbx_abcdef123456"); got != "wss://broker.example.com/v1/leases/cbx_abcdef123456/webvnc/agent" {
		t.Fatalf("agent URL=%q", got)
	}
	if got := webVNCAgentURLWithCapabilities("https://broker.example.com", "cbx_abcdef123456", "desktop_theme"); got != "wss://broker.example.com/v1/leases/cbx_abcdef123456/webvnc/agent?capabilities=desktop_theme" {
		t.Fatalf("agent capability URL=%q", got)
	}
	if got := webVNCPortalURL("https://broker.example.com/", "cbx_abcdef123456", "vnc_handoff_deadbeef"); got != "https://broker.example.com/portal/leases/cbx_abcdef123456/vnc#handoff=vnc_handoff_deadbeef" {
		t.Fatalf("portal URL=%q", got)
	}
	if got := webVNCPortalURL("https://broker.example.com/#stale", "cbx_abcdef123456", ""); got != "https://broker.example.com/portal/leases/cbx_abcdef123456/vnc" {
		t.Fatalf("portal URL=%q", got)
	}
	if got := webVNCPortalURL("https://broker.example.com/", "cbx_abcdef123456", "", webVNCPortalOptions{TakeControl: true}); got != "https://broker.example.com/portal/leases/cbx_abcdef123456/vnc#control=take" {
		t.Fatalf("portal URL with control=%q", got)
	}
	if got := webVNCPortalURL("https://broker.example.com/", "cbx_abcdef123456", "vnc_handoff_deadbeef", webVNCPortalOptions{TakeControl: true}); got != "https://broker.example.com/portal/leases/cbx_abcdef123456/vnc#control=take&handoff=vnc_handoff_deadbeef" {
		t.Fatalf("portal URL with handoff and control=%q", got)
	}
	for value, valid := range map[string]bool{
		"vnc_handoff_0123456789abcdef0123456789abcdef": true,
		"vnc_handoff_0123456789ABCDEF0123456789ABCDEF": false,
		"vnc_handoff_short":                            false,
		"password_0123456789abcdef0123456789abcdef":    false,
	} {
		if got := validWebVNCCredentialHandoffTicket(value); got != valid {
			t.Fatalf("validWebVNCCredentialHandoffTicket(%q) = %t, want %t", value, got, valid)
		}
	}
	if got := directSSHWebVNCURL("5901", "p+a ss"); got != "http://127.0.0.1:5901/vnc.html?autoconnect=1&compression=0&host=127.0.0.1&password=p%2Ba+ss&path=websockify&port=5901&quality=6&resize=scale" {
		t.Fatalf("local container WebVNC URL=%q", got)
	}
	if !isLocalContainerProvider("docker") || !isLocalContainerProvider("local-container") {
		t.Fatal("local-container aliases should use local WebVNC")
	}
	for _, provider := range []string{"local-container", "direct-webvnc-test"} {
		if !supportsDirectSSHWebVNC(provider) {
			t.Fatalf("provider %s should support direct SSH WebVNC", provider)
		}
	}
	if supportsDirectSSHWebVNC("static") || supportsDirectSSHWebVNC("aws") {
		t.Fatal("static and coordinator-backed providers should not use direct SSH WebVNC")
	}
}

func TestCreateWebVNCPortalURLUsesCredentialHandoff(t *testing.T) {
	var received map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/leases/cbx_abcdef123456/webvnc/handoff" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(CoordinatorWebVNCCredentialHandoff{
			Ticket:    "vnc_handoff_0123456789abcdef0123456789abcdef",
			ExpiresAt: "2026-07-09T12:00:00Z",
		})
	}))
	defer server.Close()
	coord := &CoordinatorClient{BaseURL: server.URL, Token: "test-token", Client: server.Client()}
	got, err := createWebVNCPortalURL(
		context.Background(),
		coord,
		"cbx_abcdef123456",
		"vnc-user",
		"generated-vnc-password",
		webVNCPortalOptions{TakeControl: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if received["username"] != "vnc-user" || received["password"] != "generated-vnc-password" {
		t.Fatalf("credential handoff body = %#v", received)
	}
	if strings.Contains(got, "vnc-user") || strings.Contains(got, "generated-vnc-password") || strings.Contains(got, "password=") || strings.Contains(got, "username=") {
		t.Fatalf("portal URL exposed credentials: %s", got)
	}
	want := server.URL + "/portal/leases/cbx_abcdef123456/vnc#control=take&handoff=vnc_handoff_0123456789abcdef0123456789abcdef"
	if got != want {
		t.Fatalf("portal URL = %q, want %q", got, want)
	}
}

func TestWebVNCAgentBaseURL(t *testing.T) {
	const base = "https://portal.example.test"
	t.Setenv("CRABBOX_WEBVNC_AGENT_BASE_URL", "")
	if got, err := webVNCAgentBaseURL(base); err != nil || got != base {
		t.Fatalf("default agent base URL=(%q, %v)", got, err)
	}

	tests := []struct {
		name    string
		value   string
		want    string
		wantErr bool
	}{
		{name: "https", value: "https://agent.example.test", want: "https://agent.example.test"},
		{name: "trailing slash", value: "https://agent.example.test/", want: "https://agent.example.test"},
		{name: "loopback http", value: "http://127.0.0.1:8787", want: "http://127.0.0.1:8787"},
		{name: "maximum port", value: "https://agent.example.test:65535", want: "https://agent.example.test:65535"},
		{name: "zero port", value: "https://agent.example.test:0", wantErr: true},
		{name: "zero loopback port", value: "http://127.0.0.1:0", wantErr: true},
		{name: "empty port", value: "https://agent.example.test:", wantErr: true},
		{name: "leading zero port", value: "https://agent.example.test:08443", wantErr: true},
		{name: "padded port", value: "https://agent.example.test:0000000000000000000008443", wantErr: true},
		{name: "out of range port", value: "https://agent.example.test:65536", wantErr: true},
		{name: "external http", value: "http://agent.example.test", wantErr: true},
		{name: "path", value: "https://agent.example.test/base", wantErr: true},
		{name: "query", value: "https://agent.example.test?x=1", wantErr: true},
		{name: "userinfo", value: "https://user@agent.example.test", wantErr: true},
		{name: "empty host", value: "https://:443", wantErr: true},
		{name: "whitespace", value: " https://agent.example.test", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("CRABBOX_WEBVNC_AGENT_BASE_URL", test.value)
			got, err := webVNCAgentBaseURL(base)
			if test.wantErr {
				if err == nil {
					t.Fatalf("agent base URL %q unexpectedly resolved to %q", test.value, got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("agent base URL %q=(%q, %v), want %q", test.value, got, err, test.want)
			}
		})
	}
}

func TestSameWebVNCOrigin(t *testing.T) {
	tests := []struct {
		name        string
		left, right string
		want        bool
	}{
		{name: "paths ignored", left: "https://agent.example.test/api", right: "https://agent.example.test", want: true},
		{name: "default HTTPS port", left: "https://agent.example.test", right: "https://AGENT.EXAMPLE.TEST:443/", want: true},
		{name: "numeric port normalization", left: "https://agent.example.test:0443", right: "https://agent.example.test:443/", want: true},
		{name: "default HTTP port", left: "http://127.0.0.1", right: "http://127.0.0.1:80/", want: true},
		{name: "different port", left: "https://agent.example.test", right: "https://agent.example.test:8443"},
		{name: "different scheme", left: "https://agent.example.test", right: "http://agent.example.test"},
		{name: "different host", left: "https://agent.example.test", right: "https://portal.example.test"},
		{name: "invalid", left: "not a URL", right: "https://agent.example.test"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := sameWebVNCOrigin(test.left, test.right); got != test.want {
				t.Fatalf("sameWebVNCOrigin(%q, %q)=%v, want %v", test.left, test.right, got, test.want)
			}
		})
	}
}

func TestWebVNCWebSocketDialRejectsCrossOriginDowngradeRedirect(t *testing.T) {
	redirected := make(chan http.Header, 1)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected <- r.Header.Clone()
		http.Error(w, "unexpected redirect", http.StatusBadRequest)
	}))
	defer sink.Close()

	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()

	options := webVNCWebSocketDialOptions(http.Header{
		"X-Crabbox-Bridge-Ticket": {"bridge-ticket-must-not-leak"},
	})
	options.HTTPClient.Transport = redirect.Client().Transport
	wsURL := "wss" + strings.TrimPrefix(redirect.URL, "https")
	conn, response, err := websocket.Dial(context.Background(), wsURL, options)
	if conn != nil {
		conn.CloseNow()
		t.Fatal("WebVNC WebSocket followed a cross-origin HTTPS-to-HTTP redirect")
	}
	if err == nil {
		t.Fatal("WebVNC WebSocket redirect unexpectedly succeeded")
	}
	if response == nil || response.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("WebVNC WebSocket redirect response=%v, want HTTP %d", response, http.StatusTemporaryRedirect)
	}
	response.Body.Close()
	select {
	case headers := <-redirected:
		t.Fatalf("WebVNC WebSocket redirect reached sink with ticket=%q", headers.Get("X-Crabbox-Bridge-Ticket"))
	default:
	}
}

func TestWebVNCRedactingWriterKeepsCredentialsOutOfDaemonLogs(t *testing.T) {
	var output bytes.Buffer
	writer := webVNCRedactingWriter{Writer: &output}
	input := "bridge: connected\nwebvnc: https://portal.example/vnc#password=secret\npassword: secret\nusername: crabbox\nwebvnc: https://broker-user:broker-secret@portal.example/vnc\nopened: https://other-user:other-secret@portal.example/vnc\nopened: https://portal.example/vnc#handoff=vnc_handoff_secret\nwebvnc: run crabbox webvnc --id demo\nwebvnc: https://portal.example/vnc\n"
	if _, err := writer.Write([]byte(input)); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if strings.Contains(got, "secret") || strings.Contains(got, "broker-user") || strings.Contains(got, "other-user") || strings.Contains(got, "#password=") {
		t.Fatalf("credential leaked: %q", got)
	}
	if !strings.Contains(got, "bridge: connected") || strings.Count(got, "[redacted]") != 6 {
		t.Fatalf("unexpected redacted output: %q", got)
	}
	if !strings.Contains(got, "webvnc: run crabbox webvnc --id demo") || !strings.Contains(got, "webvnc: https://portal.example/vnc") {
		t.Fatalf("non-secret WebVNC output was redacted: %q", got)
	}
}

func TestWebVNCCredentialOutputDefaultsToRedacted(t *testing.T) {
	fs := newFlagSet("webvnc-test", io.Discard)
	redact := registerWebVNCCredentialOutputFlag(fs)
	if err := parseFlags(fs, nil); err != nil {
		t.Fatal(err)
	}
	if !*redact {
		t.Fatal("WebVNC credential output must default to redacted")
	}

	fs = newFlagSet("webvnc-test", io.Discard)
	redact = registerWebVNCCredentialOutputFlag(fs)
	if err := parseFlags(fs, []string{"--redact-credentials=false"}); err != nil {
		t.Fatal(err)
	}
	if *redact {
		t.Fatal("explicit private-terminal reveal was ignored")
	}
}

func TestPrepareWebVNCDaemonArgsGatesProviderSideEffectsByOwner(t *testing.T) {
	args := prepareWebVNCDaemonArgs([]string{"--id", "cbx_abcdef123456", "--reclaim"}, false)
	joined := strings.Join(args, " ")
	if len(args) < 1 || args[0] != "--redact-credentials=true" || strings.Contains(joined, "--no-provider-side-effects") || strings.Contains(joined, "--local-port") || strings.Contains(joined, "--reclaim") {
		t.Fatalf("ordinary daemon args lost heartbeat or safety flags: %q", args)
	}
	args = prepareWebVNCDaemonArgs([]string{"--id", "cbx_abcdef123456", "--redact-credentials=false", "--no-provider-side-effects=false", "--local-port=5942"}, true)
	joined = strings.Join(args, " ")
	if strings.Count(joined, "--redact-credentials") != 1 || !strings.Contains(joined, "--redact-credentials=true") || strings.Contains(joined, "--redact-credentials=false") || strings.Count(joined, "--no-provider-side-effects") != 1 || !strings.Contains(joined, "--no-provider-side-effects=true") || strings.Count(joined, "--local-port") != 1 || !strings.Contains(joined, "--local-port=5942") {
		t.Fatalf("controller daemon args duplicated or lost ownership flags: %q", args)
	}
	args = prepareWebVNCDaemonArgs([]string{"pearl-krill", "-reclaim=false"}, true)
	if args[0] != "--redact-credentials=true" || args[1] != "--no-provider-side-effects=true" || strings.Contains(strings.Join(args, " "), "reclaim") {
		t.Fatalf("positional daemon args bypassed safety flags: %q", args)
	}
}

func TestReserveWebVNCDaemonPortRejectsInvalidPort(t *testing.T) {
	for _, port := range []string{"0", "65536", "05901", "not-a-port"} {
		if reservation, err := reserveWebVNCDaemonPort(port); err == nil {
			reservation.release()
			t.Fatalf("port %q unexpectedly accepted", port)
		}
	}
}

func TestWebVNCDaemonPortReservationEnvironmentReplacesAmbientValue(t *testing.T) {
	env := webVNCDaemonPortReservationEnvironment([]string{
		"KEEP=value",
		webVNCDaemonPortReservationEnv + "=5999",
		webVNCDaemonPortReservationFDEnv + "=99",
	}, "5901", "3")
	joined := strings.Join(env, "\n")
	if strings.Count(joined, webVNCDaemonPortReservationEnv+"=") != 1 ||
		!strings.Contains(joined, webVNCDaemonPortReservationEnv+"=5901") ||
		!strings.Contains(joined, webVNCDaemonPortReservationFDEnv+"=3") ||
		!strings.Contains(joined, "KEEP=value") {
		t.Fatalf("reservation environment=%q", env)
	}
}

func TestInheritedWebVNCDaemonPortReservationRequiresExactPort(t *testing.T) {
	t.Setenv(webVNCDaemonPortReservationEnv, "5901")
	t.Setenv(webVNCDaemonPortReservationFDEnv, "3")
	if !inheritedWebVNCDaemonPortReservation("5901") {
		t.Fatal("matching inherited reservation was not recognized")
	}
	if inheritedWebVNCDaemonPortReservation("5902") || inheritedWebVNCDaemonPortReservation("") {
		t.Fatal("mismatched inherited reservation was accepted")
	}
}

func TestWebVNCRejectsReclaimInNoProviderSideEffectMode(t *testing.T) {
	err := (App{Stdout: io.Discard, Stderr: io.Discard}).webvnc(context.Background(), []string{
		"--id", "cbx_abcdef123456", "--no-provider-side-effects=true", "--reclaim",
	})
	if err == nil || !strings.Contains(err.Error(), "cannot be combined with --reclaim") {
		t.Fatalf("error=%v", err)
	}
}

func TestWebVNCExpectedProviderIdentityRequiresCompleteFlags(t *testing.T) {
	complete := []string{
		"--expected-provider-lease-id", "cbx_identity123",
		"--expected-provider-attempt-lease-id", "cbx_identity123",
		"--expected-provider-slug", "identity-slug",
		"--expected-provider-resource-id", "provider/identity",
		"--expected-provider-scope", "test-external:provider-a",
	}
	fs := newFlagSet("webvnc-expected-identity", io.Discard)
	flags := registerWebVNCExpectedProviderIdentityFlags(fs)
	if err := parseFlags(fs, complete); err != nil {
		t.Fatal(err)
	}
	expected, err := flags.value(fs)
	if err != nil || !expected.set || expected.Identity.ResourceID != "provider/identity" || expected.Scope != "test-external:provider-a" {
		t.Fatalf("expected identity=%#v err=%v", expected, err)
	}

	fs = newFlagSet("webvnc-partial-identity", io.Discard)
	flags = registerWebVNCExpectedProviderIdentityFlags(fs)
	if err := parseFlags(fs, complete[:2]); err != nil {
		t.Fatal(err)
	}
	if _, err := flags.value(fs); err == nil || !strings.Contains(err.Error(), "complete expected provider identity") {
		t.Fatalf("partial identity error=%v", err)
	}
}

func TestWebVNCResolvedProviderIdentityValidatesEveryPersistedField(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider = "external"
	cfg.External.Command = "provider-a"
	server := Server{CloudID: "provider/identity", Provider: "external", Labels: map[string]string{"slug": "identity-slug"}}
	target := SSHTarget{Host: "192.0.2.10", User: "crabbox", Port: "22", TargetOS: targetLinux}
	expected := webVNCExpectedProviderIdentity{
		Identity: ProviderIdentityExpectation{
			LeaseID: "cbx_identity123", AttemptLeaseID: "cbx_identity123",
			Slug: "identity-slug", ResourceID: "provider/identity",
		},
		Scope: "test-external:provider-a",
		set:   true,
	}
	if err := validateWebVNCResolvedProviderIdentity(cfg, server, target, "cbx_identity123", expected); err != nil {
		t.Fatalf("matching identity rejected: %v", err)
	}
	for name, mutate := range map[string]func(*webVNCExpectedProviderIdentity){
		"lease":    func(value *webVNCExpectedProviderIdentity) { value.Identity.LeaseID = "cbx_other123" },
		"attempt":  func(value *webVNCExpectedProviderIdentity) { value.Identity.AttemptLeaseID = "cbx_other123" },
		"slug":     func(value *webVNCExpectedProviderIdentity) { value.Identity.Slug = "other-slug" },
		"resource": func(value *webVNCExpectedProviderIdentity) { value.Identity.ResourceID = "provider/other" },
		"scope":    func(value *webVNCExpectedProviderIdentity) { value.Scope = "test-external:provider-b" },
	} {
		t.Run(name, func(t *testing.T) {
			mismatched := expected
			mutate(&mismatched)
			if err := validateWebVNCResolvedProviderIdentity(cfg, server, target, "cbx_identity123", mismatched); err == nil {
				t.Fatalf("%s mismatch was accepted", name)
			}
		})
	}
}

func TestGuardMacOSDirectWebVNC(t *testing.T) {
	// macOS desktop leases (e.g. tart) must be rejected from the Linux-only
	// noVNC bridge with native-client guidance, not silently enter it.
	err := guardMacOSDirectWebVNC(Config{Provider: "tart", TargetOS: targetMacOS, SSHUser: "admin"})
	if err == nil {
		t.Fatal("macOS lease should be guarded out of the direct WebVNC browser path")
	}
	if !strings.Contains(err.Error(), "native VNC client") ||
		!strings.Contains(err.Error(), "GatewayPorts=no") ||
		!strings.Contains(err.Error(), "-L 127.0.0.1:5900:127.0.0.1:5900") {
		t.Fatalf("guard error should give native-client guidance, got: %v", err)
	}
	// Even with TargetOS unresolved (as the webvnc subcommands leave it), tart is
	// guarded via its provider spec's macOS target.
	if err := guardMacOSDirectWebVNC(Config{Provider: "tart"}); err == nil {
		t.Fatal("tart should be guarded via provider spec even when TargetOS is unset")
	}
	// Linux desktop leases keep using the browser bridge.
	if err := guardMacOSDirectWebVNC(Config{Provider: "local-container", TargetOS: targetLinux}); err != nil {
		t.Fatalf("linux lease should not be guarded: %v", err)
	}
}

func TestRegisteredLeaseUsesCoordinatorWebVNC(t *testing.T) {
	cfg := Config{
		Provider:    "direct-webvnc-test",
		Coordinator: "https://broker.example.test",
		BrokerMode:  BrokerModeRegistered,
	}
	if useDirectSSHWebVNC(cfg) {
		t.Fatal("registered lease should use the coordinator WebVNC bridge")
	}
	if !useDirectSSHWebVNC(Config{Provider: "direct-webvnc-test"}) {
		t.Fatal("unregistered direct provider should keep its local WebVNC bridge")
	}
}

func TestMacOSWebVNCPortalConfigUsesAuthenticatedCoordinator(t *testing.T) {
	cfg := Config{
		Provider:    "direct-webvnc-test",
		TargetOS:    targetMacOS,
		Coordinator: "https://broker.example.test",
		CoordToken:  "token",
	}
	got, temporary, err := macOSPortalWebVNCConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !temporary || got.BrokerMode != BrokerModeRegistered || useDirectSSHWebVNC(got) {
		t.Fatalf("portal config=%#v temporary=%t", got, temporary)
	}

	withoutAuth := cfg
	withoutAuth.CoordToken = ""
	withoutAuth.Coordinator = "not-a-url"
	got, temporary, err = macOSPortalWebVNCConfig(withoutAuth)
	if err != nil {
		t.Fatal(err)
	}
	if temporary || got.BrokerMode == BrokerModeRegistered || !useDirectSSHWebVNC(got) {
		t.Fatalf("unauthenticated config=%#v temporary=%t", got, temporary)
	}

	registered := cfg
	registered.BrokerMode = BrokerModeRegistered
	got, temporary, err = macOSPortalWebVNCConfig(registered)
	if err != nil {
		t.Fatal(err)
	}
	if temporary || got.BrokerMode != BrokerModeRegistered {
		t.Fatalf("explicit registered config=%#v temporary=%t", got, temporary)
	}

	invalid := cfg
	invalid.Coordinator = "not-a-url"
	if _, _, err := macOSPortalWebVNCConfig(invalid); err == nil {
		t.Fatal("invalid authenticated coordinator URL should fail instead of silently using the local viewer")
	}
}

func TestMacOSWebVNCPortalConfigUsesStoredMultiTargetLease(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_macosportal123"
	claimCfg := baseConfig()
	claimCfg.Provider = "direct-webvnc-test"
	claimCfg.TargetOS = targetMacOS
	if err := claimLeaseTargetForRepoConfig(
		leaseID,
		"macos-portal",
		claimCfg,
		Server{Provider: "direct-webvnc-test", Labels: map[string]string{"state": "ready", "target": targetMacOS}},
		SSHTarget{Host: "192.0.2.40", TargetOS: targetMacOS},
		"/repo",
		time.Hour,
		true,
	); err != nil {
		t.Fatal(err)
	}
	stored, exists, err := readLeaseClaimWithPresence(leaseID)
	if err != nil || !exists || stored.Labels["target"] != targetMacOS {
		t.Fatalf("stored claim=%#v exists=%t err=%v", stored, exists, err)
	}
	foreignCfg := baseConfig()
	foreignCfg.Provider = "local-container"
	foreignCfg.TargetOS = targetLinux
	if err := claimLeaseTargetForRepoConfig(
		"cbx_aaa_foreign",
		"macos-portal",
		foreignCfg,
		Server{Provider: "local-container", Labels: map[string]string{"state": "ready", "target": targetLinux}},
		SSHTarget{Host: "192.0.2.41", TargetOS: targetLinux},
		"/repo",
		time.Hour,
		true,
	); err != nil {
		t.Fatal(err)
	}

	got, automatic, err := macOSPortalWebVNCConfigForLease(Config{
		Provider:    "direct-webvnc-test",
		Coordinator: "https://broker.example.test",
		CoordToken:  "token",
	}, "macos-portal")
	if err != nil {
		t.Fatal(err)
	}
	if !automatic || got.TargetOS != targetMacOS || got.BrokerMode != BrokerModeRegistered {
		t.Fatalf("stored target config=%#v automatic=%t", got, automatic)
	}
	if err := persistAutomaticCoordinatorRegistrationBinding(leaseID, got, "https://broker.example.test"); err != nil {
		t.Fatal(err)
	}
	stored, exists, err = readLeaseClaimWithPresence(leaseID)
	if err != nil || !exists || stored.CoordinatorRegistrationURL != "https://broker.example.test" {
		t.Fatalf("persisted coordinator claim=%#v exists=%t err=%v", stored, exists, err)
	}
	if _, _, err := macOSPortalWebVNCConfigForLease(Config{
		Provider:    "direct-webvnc-test",
		Coordinator: "https://other-broker.example.test",
		CoordToken:  "token",
	}, "macos-portal"); err == nil || !strings.Contains(err.Error(), "persisted registration binding") {
		t.Fatalf("coordinator drift error=%v", err)
	}
	if _, _, err := macOSPortalWebVNCConfigForLease(Config{
		Provider:    "direct-webvnc-test",
		Coordinator: "https://other-broker.example.test",
		CoordToken:  "token",
		BrokerMode:  BrokerModeRegistered,
	}, "macos-portal"); err == nil || !strings.Contains(err.Error(), "persisted registration binding") {
		t.Fatalf("explicit registered coordinator drift error=%v", err)
	}

	explicitLinux := got
	explicitLinux.BrokerMode = BrokerModeManaged
	explicitLinux.TargetOS = targetLinux
	explicitLinux.targetFlagExplicit = true
	got, automatic, err = macOSPortalWebVNCConfigForLease(explicitLinux, leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if automatic || got.TargetOS != targetLinux {
		t.Fatalf("explicit target config=%#v automatic=%t", got, automatic)
	}
}

func TestIsMacOSDesktopProviderUsesExplicitTargetOrDedicatedProvider(t *testing.T) {
	// tart's only target is macOS -> uses the host-side Screen Sharing bridge.
	if !isMacOSDesktopProvider(Config{Provider: "tart"}) {
		t.Error("tart (dedicated macOS provider) should route to the macOS bridge")
	}
	// Parallels needs an explicit resolved target because it also serves Linux
	// and Windows leases.
	if isMacOSDesktopProvider(Config{Provider: "parallels"}) {
		t.Error("unresolved parallels target must not route to the macOS bridge")
	}
	if !isMacOSDesktopProvider(Config{Provider: "parallels", TargetOS: targetMacOS}) {
		t.Error("a macOS parallels lease should route to the native Screen Sharing bridge")
	}
	if isMacOSDesktopProvider(Config{Provider: "parallels", TargetOS: targetLinux}) {
		t.Error("a Linux parallels lease must keep the guest WebVNC path")
	}
}

func TestWebVNCBridgeArgsPreserveProviderRouting(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	args := webVNCBridgeArgs(
		Config{Provider: "direct-webvnc-test", TargetOS: targetLinux},
		SSHTarget{TargetOS: targetLinux},
		"cbx_abcdef123456",
		false,
		false,
	)
	got := strings.Join(args, " ")
	if !strings.Contains(got, "--direct-webvnc-routing route-cbx_abcdef123456") {
		t.Fatalf("args=%#v", args)
	}
}

type directWebVNCTestProvider struct{}

func (directWebVNCTestProvider) Name() string      { return "direct-webvnc-test" }
func (directWebVNCTestProvider) Aliases() []string { return nil }
func (directWebVNCTestProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        "direct-webvnc-test",
		Kind:        ProviderKindSSHLease,
		Features:    FeatureSet{FeatureSSH, FeatureDesktop},
		Coordinator: CoordinatorNever,
	}
}
func (directWebVNCTestProvider) RegisterFlags(fs *flag.FlagSet, _ Config) any {
	return fs.String("direct-webvnc-routing", "", "test routing value")
}
func (directWebVNCTestProvider) ApplyFlags(*Config, *flag.FlagSet, any) error {
	return nil
}
func (directWebVNCTestProvider) Configure(Config, Runtime) (Backend, error) { return nil, nil }
func (directWebVNCTestProvider) DesktopCredentials(Config, SSHTarget) (DesktopCredentials, bool) {
	return DesktopCredentials{Username: "admin", Password: "provider-secret"}, true
}
func (directWebVNCTestProvider) CommandRoutingArgs(_ Config, leaseID string) []string {
	return []string{"--direct-webvnc-routing", "route-" + leaseID}
}

func TestWebVNCPortalCredentialsUseMacOSProviderAccount(t *testing.T) {
	read := false
	credentials, err := resolveWebVNCPortalCredentials(
		context.Background(),
		Config{Provider: "direct-webvnc-test"},
		SSHTarget{TargetOS: targetMacOS, User: "lease-user"},
		vncEndpoint{Managed: true},
		func(context.Context, SSHTarget, string) (string, error) {
			read = true
			return "", errors.New("managed password file is absent")
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if read {
		t.Fatal("provider account credentials should avoid the managed password file")
	}
	if credentials.Username != "admin" || credentials.Password != "provider-secret" {
		t.Fatalf("credentials=%#v", credentials)
	}
}

func TestWebVNCResetRemoteCommandHandlesWaylandAndX11(t *testing.T) {
	got := webVNCResetRemoteCommand(SSHTarget{TargetOS: targetLinux})
	for _, want := range []string{
		"/var/lib/crabbox/desktop.env",
		"/usr/local/bin/crabbox-start-desktop",
		`CRABBOX_DESKTOP_ENV:-xfce`,
		"crabbox-desktop.service crabbox-wayvnc.service",
		"crabbox-xvfb.service crabbox-desktop.service crabbox-desktop-session.service",
		"crabbox-desktop.service crabbox-x11vnc.service",
		"crabbox-desktop-session.service crabbox-x11vnc.service",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("reset command missing %q:\n%s", want, got)
		}
	}
}

func TestDirectSSHWebVNCRemoteReadinessRequiresExactOwnedSocket(t *testing.T) {
	owner, err := directSSHWebVNCRemoteOwnerFromID(strings.Repeat("01", sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	start := directSSHNoVNCRemoteCommand(owner)
	status := directSSHWebVNCRemoteStatusCommand(owner)
	reset := directSSHWebVNCResetRemoteCommand(owner)
	for name, command := range map[string]string{
		"start": start, "status": status, "reset": reset,
	} {
		if output, err := exec.Command("sh", "-n", "-c", command).CombinedOutput(); err != nil {
			t.Fatalf("%s remote command syntax: %v: %s", name, err, output)
		}
	}
	for _, want := range []string{
		`preferred_remote_port='` + owner.PreferredPort + `'`,
		`remote_port=""`,
		`expected_owner_id='` + owner.ID + `'`,
		`state_dir="$state_root/crabbox/direct-webvnc"`,
		`identity="$state_dir/$expected_owner_id.identity"`,
		`allocation_lock="$state_dir/allocation.lock"`,
		`flock -x 9`,
		`direct_webvnc_allocate_port`,
		`while [ "$launch_attempt" -lt 32 ]`,
		`127.0.0.1:5900 9>&- >"$log"`,
		`ss -H -4 -ltnp "sport = :$remote_port"`,
		`awk -v expected="127.0.0.1:$remote_port"`,
		`printf '%s %s %s %s %s %s\n' "$pid" "$started" "$boot_id" "$expected_owner_id" "$remote_port" "$process_nonce"`,
		`/proc/sys/kernel/random/boot_id`,
		`[ "$boot_id" = "$(direct_webvnc_current_boot_id)" ]`,
		directSSHWebVNCRemotePortPrefix,
		"CRABBOX_DIRECT_WEBVNC_PROCESS_NONCE",
		`grep -Eq '^[0-9a-f]{32}$'`,
		`/proc/$pid/stat`,
		`[ "$socket_pids" = "$pid" ]`,
		"refusing live direct WebVNC process without its exact owner socket",
	} {
		if !strings.Contains(start, want) {
			t.Fatalf("direct SSH startup missing %q:\n%s", want, start)
		}
	}
	if strings.Contains(start, "CRABBOX_DIRECT_WEBVNC_OWNER") {
		t.Fatalf("direct SSH startup exposed its owner identity as a long-lived process credential:\n%s", start)
	}
	if strings.Contains(start, "ss -ltn | grep -q '127.0.0.1:6080'") {
		t.Fatal("direct SSH startup retained substring listener acceptance")
	}
	if !strings.Contains(status, "direct_webvnc_identity_valid") || !strings.Contains(status, "direct_webvnc_prepare_state") || !strings.Contains(status, "direct_webvnc_acquire_lock") || !strings.Contains(status, "direct_webvnc_print_port") {
		t.Fatalf("status does not verify controller-started bridge identity:\n%s", status)
	}
	if strings.Contains(reset, "pkill") || strings.Contains(reset, "killall") {
		t.Fatalf("reset retained broad process termination:\n%s", reset)
	}
	for _, want := range []string{
		"direct_webvnc_process_identity_valid",
		"direct_webvnc_process_alive_same",
		`owned_pid="$pid"`,
		`owned_boot_id="$boot_id"`,
		`kill "$owned_pid"`,
		`kill -KILL "$owned_pid"`,
		"owned direct WebVNC identity changed before stop",
		"owned direct WebVNC identity changed before forced stop",
		"owned direct WebVNC identity changed during stop",
		"refusing unrelated listener on owner socket 127.0.0.1:$remote_port",
		"direct_webvnc_acquire_lock",
		"direct_webvnc_release_lock",
	} {
		if !strings.Contains(reset, want) {
			t.Fatalf("direct SSH reset missing %q:\n%s", want, reset)
		}
	}
	if strings.Count(reset, `[ "$boot_id" != "$owned_boot_id" ]`) != 2 {
		t.Fatalf("direct SSH reset does not revalidate boot identity before both TERM and KILL:\n%s", reset)
	}
	if !strings.Contains(reset, `expected_identity="$owned_pid $owned_started $owned_boot_id $owned_owner_id $owned_port $owned_process_nonce"`) {
		t.Fatalf("direct SSH reset does not bind final cleanup to the boot identity:\n%s", reset)
	}
	if stop, start := strings.Index(reset, `kill "$owned_pid"`), strings.LastIndex(reset, "nohup env CRABBOX_DIRECT_WEBVNC_PROCESS_NONCE="); stop < 0 || start < 0 || stop >= start {
		t.Fatalf("reset did not terminate its verified process before starting a replacement:\n%s", reset)
	}
}

func TestDirectSSHWebVNCWSL2StagesLargeCommandOverStdin(t *testing.T) {
	dir := t.TempDir()
	argvPath := filepath.Join(dir, "argv")
	stdinPath := filepath.Join(dir, "stdin")
	sshPath := filepath.Join(dir, "ssh")
	script := "#!/bin/sh\nprintf '%s' \"$*\" > " + shellQuote(argvPath) + "\ncat > " + shellQuote(stdinPath) + "\nprintf running\n"
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	remote := strings.Repeat("large-webvnc-command\n", 2000)
	target := SSHTarget{User: "crabbox", Host: "windows.test", Port: "22", TargetOS: targetWindows, WindowsMode: windowsModeWSL2}
	out, err := runDirectSSHWebVNCRemoteCombinedOutput(context.Background(), target, remote)
	if err != nil {
		t.Fatal(err)
	}
	if out != "running" {
		t.Fatalf("output=%q want running", out)
	}
	stdin, err := os.ReadFile(stdinPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(stdin) != remote {
		t.Fatal("WSL2 WebVNC command was not staged intact over stdin")
	}
	argv, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(argv) >= 8191 || bytes.Contains(argv, []byte("large-webvnc-command")) {
		t.Fatalf("WSL2 WebVNC wrapper still embeds the remote payload: argv bytes=%d", len(argv))
	}
}

func TestDirectSSHWebVNCNativeWindowsUsesLocalBridge(t *testing.T) {
	native := SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}
	if !directSSHWebVNCUsesLocalBridge(native) {
		t.Fatal("native Windows WebVNC must use the host-side bridge")
	}
	for _, target := range []SSHTarget{
		{TargetOS: targetWindows, WindowsMode: windowsModeWSL2},
		{TargetOS: targetLinux},
		{TargetOS: targetMacOS},
	} {
		if directSSHWebVNCUsesLocalBridge(target) {
			t.Fatalf("target unexpectedly selected native Windows bridge: %#v", target)
		}
	}
	command := powershellCommand(directSSHNoVNCRemoteCommand(directSSHWebVNCRemoteOwner{
		ID: strings.Repeat("01", sha256.Size), PreferredPort: "20001",
	}))
	if len(command) <= 8191 {
		t.Fatalf("regression fixture no longer exceeds Windows command-line limit: %d", len(command))
	}
}

func TestDirectSSHWebVNCRemoteOwnersForcePreferredPortCollision(t *testing.T) {
	ownerIDs := []string{
		strings.Repeat("01", sha256.Size),
		"01010101" + strings.Repeat("02", sha256.Size-4),
	}
	type result struct {
		owner   directSSHWebVNCRemoteOwner
		command string
		err     error
	}
	results := make(chan result, len(ownerIDs))
	for _, ownerID := range ownerIDs {
		ownerID := ownerID
		go func() {
			owner, err := directSSHWebVNCRemoteOwnerFromID(ownerID)
			command := ""
			if err == nil {
				command = directSSHNoVNCRemoteCommand(owner)
			}
			results <- result{owner: owner, command: command, err: err}
		}()
	}
	got := make([]result, 0, len(ownerIDs))
	for range ownerIDs {
		entry := <-results
		if entry.err != nil {
			t.Fatal(entry.err)
		}
		if output, err := exec.Command("sh", "-n", "-c", entry.command).CombinedOutput(); err != nil {
			t.Fatalf("owner %s remote command syntax: %v: %s", entry.owner.ID, err, output)
		}
		got = append(got, entry)
	}
	if got[0].owner.PreferredPort != got[1].owner.PreferredPort {
		t.Fatalf("test owners did not force a preferred-port collision: %s != %s", got[0].owner.PreferredPort, got[1].owner.PreferredPort)
	}
	for index, entry := range got {
		other := got[1-index]
		for _, want := range []string{
			`expected_owner_id='` + entry.owner.ID + `'`,
			`preferred_remote_port='` + entry.owner.PreferredPort + `'`,
			`identity="$state_dir/$expected_owner_id.identity"`,
			`direct_webvnc_allocate_port`,
		} {
			if !strings.Contains(entry.command, want) {
				t.Fatalf("owner command missing %q:\n%s", want, entry.command)
			}
		}
		if strings.Contains(entry.command, other.owner.ID) {
			t.Fatalf("owner %s command crossed into owner %s namespace", entry.owner.ID, other.owner.ID)
		}
	}
}

func TestDirectSSHWebVNCRemotePortOutputValidation(t *testing.T) {
	if got, err := directSSHWebVNCRemotePortFromOutput("running\n" + directSSHWebVNCRemotePortPrefix + "23456\n"); err != nil || got != "23456" {
		t.Fatalf("remote port got=%q err=%v", got, err)
	}
	for _, output := range []string{
		"running",
		directSSHWebVNCRemotePortPrefix + "19999",
		directSSHWebVNCRemotePortPrefix + "60000",
		directSSHWebVNCRemotePortPrefix + "abc",
		directSSHWebVNCRemotePortPrefix + "23456\n" + directSSHWebVNCRemotePortPrefix + "23456",
		directSSHWebVNCRemotePortPrefix + "23456\n" + directSSHWebVNCRemotePortPrefix + "23457",
	} {
		if _, err := directSSHWebVNCRemotePortFromOutput(output); err == nil {
			t.Fatalf("accepted invalid remote port output %q", output)
		}
	}
}

func TestDirectSSHWebVNCRemoteIdentityLoadsPersistedSelectedPort(t *testing.T) {
	owner, err := directSSHWebVNCRemoteOwnerFromID(strings.Repeat("01", sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	preferred, err := strconv.Atoi(owner.PreferredPort)
	if err != nil {
		t.Fatal(err)
	}
	selected := directSSHWebVNCRemotePortBase + (preferred-directSSHWebVNCRemotePortBase+7)%directSSHWebVNCRemotePortSpan
	stateRoot := t.TempDir()
	stateDir := filepath.Join(stateRoot, "crabbox", "direct-webvnc")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	identity := fmt.Sprintf("123 456 12345678-1234-1234-1234-123456789abc %s %d %s\n", owner.ID, selected, strings.Repeat("ab", 16))
	if err := os.WriteFile(filepath.Join(stateDir, owner.ID+".identity"), []byte(identity), 0o600); err != nil {
		t.Fatal(err)
	}
	command := directSSHWebVNCRemoteIdentityFunctions(owner) + `
direct_webvnc_load_identity
direct_webvnc_print_port`
	cmd := exec.Command("sh", "-c", command)
	cmd.Env = append(os.Environ(), "XDG_STATE_HOME="+stateRoot)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("load persisted remote port: %v: %s", err, output)
	}
	if got, err := directSSHWebVNCRemotePortFromOutput(string(output)); err != nil || got != strconv.Itoa(selected) {
		t.Fatalf("persisted remote port got=%q want=%d err=%v", got, selected, err)
	}
}

func TestDirectSSHWebVNCRemoteAllocationSerializesForcedCollision(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("remote allocator uses Linux flock/stat semantics")
	}
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock is unavailable")
	}
	ownerIDs := []string{
		strings.Repeat("01", sha256.Size),
		"01010101" + strings.Repeat("02", sha256.Size-4),
	}
	stateRoot := t.TempDir()
	type result struct {
		port string
		err  error
	}
	results := make(chan result, len(ownerIDs))
	start := make(chan struct{})
	for _, ownerID := range ownerIDs {
		owner, err := directSSHWebVNCRemoteOwnerFromID(ownerID)
		if err != nil {
			t.Fatal(err)
		}
		command := directSSHWebVNCRemoteIdentityFunctions(owner) + `
direct_webvnc_port_in_use() {
  [ -e "$state_dir/test-reserved-$1" ]
}
direct_webvnc_prepare_state
direct_webvnc_acquire_lock
trap direct_webvnc_release_lock EXIT HUP INT TERM
if ! mkdir "$state_dir/test-critical" 2>/dev/null; then
  echo "allocator critical sections overlapped" >&2
  exit 91
fi
sleep 0.2
port_cursor="$preferred_remote_port"
direct_webvnc_allocate_port
: >"$state_dir/test-reserved-$remote_port"
rmdir "$state_dir/test-critical"
direct_webvnc_print_port
direct_webvnc_release_lock
trap - EXIT HUP INT TERM`
		go func() {
			<-start
			cmd := exec.Command("sh", "-c", command)
			cmd.Env = append(os.Environ(), "XDG_STATE_HOME="+stateRoot)
			output, err := cmd.CombinedOutput()
			if err != nil {
				results <- result{err: fmt.Errorf("allocator command: %w: %s", err, output)}
				return
			}
			port, err := directSSHWebVNCRemotePortFromOutput(string(output))
			results <- result{port: port, err: err}
		}()
	}
	close(start)
	ports := map[string]struct{}{}
	for range ownerIDs {
		entry := <-results
		if entry.err != nil {
			t.Fatal(entry.err)
		}
		ports[entry.port] = struct{}{}
	}
	if len(ports) != len(ownerIDs) {
		t.Fatalf("concurrent colliding owners received ports %#v", ports)
	}
}

func TestProbeDirectSSHWebVNCAuthenticatesPasswordOverWebSocket(t *testing.T) {
	const password = "s3cret!!"
	serverErr := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/websockify" {
			serverErr <- fmt.Errorf("path=%s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"binary"}})
		if err != nil {
			serverErr <- err
			return
		}
		defer ws.Close(websocket.StatusNormalClosure, "done")
		conn := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
		defer conn.Close()
		if _, err := conn.Write([]byte("RFB 003.008\n")); err != nil {
			serverErr <- err
			return
		}
		version := make([]byte, 12)
		if _, err := io.ReadFull(conn, version); err != nil {
			serverErr <- err
			return
		}
		if string(version) != "RFB 003.008\n" {
			serverErr <- fmt.Errorf("version=%q", version)
			return
		}
		if _, err := conn.Write([]byte{1, 2}); err != nil {
			serverErr <- err
			return
		}
		selected := []byte{0}
		if _, err := io.ReadFull(conn, selected); err != nil || selected[0] != 2 {
			serverErr <- fmt.Errorf("selected=%v err=%v", selected, err)
			return
		}
		challenge := []byte("0123456789abcdef")
		if _, err := conn.Write(challenge); err != nil {
			serverErr <- err
			return
		}
		response := make([]byte, 16)
		if _, err := io.ReadFull(conn, response); err != nil {
			serverErr <- err
			return
		}
		expected, err := directSSHWebVNCChallengeResponse(password, challenge)
		if err != nil || !bytes.Equal(response, expected) {
			serverErr <- fmt.Errorf("password response mismatch err=%v", err)
			return
		}
		result := make([]byte, 4)
		binary.BigEndian.PutUint32(result, 0)
		if _, err := conn.Write(result); err != nil {
			serverErr <- err
			return
		}
		serverErr <- nil
	}))
	defer server.Close()
	port := strconv.Itoa(server.Listener.Addr().(*net.TCPAddr).Port)
	if err := probeDirectSSHWebVNC(context.Background(), port, password); err != nil {
		t.Fatal(err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestDirectSSHWebVNCAcceptsNoneOnlyForManagedLoopbackWayland(t *testing.T) {
	endpoint := vncEndpoint{Host: "127.0.0.1", Port: managedVNCPort, Managed: true}
	server := Server{Labels: map[string]string{"desktop_env": desktopEnvWayland}}
	if !directSSHWebVNCAllowsNone(server, endpoint) {
		t.Fatal("generated loopback WayVNC endpoint did not allow None security")
	}
	for name, mutate := range map[string]func(*Server, *vncEndpoint){
		"xfce": func(server *Server, _ *vncEndpoint) {
			server.Labels = map[string]string{"desktop_env": desktopEnvXFCE}
		},
		"missing-label": func(server *Server, _ *vncEndpoint) { server.Labels = nil },
		"direct":        func(_ *Server, endpoint *vncEndpoint) { endpoint.Direct = true },
		"public":        func(_ *Server, endpoint *vncEndpoint) { endpoint.Host = "0.0.0.0" },
		"foreign": func(_ *Server, endpoint *vncEndpoint) {
			endpoint.Managed = false
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidateServer, candidateEndpoint := server, endpoint
			mutate(&candidateServer, &candidateEndpoint)
			if directSSHWebVNCAllowsNone(candidateServer, candidateEndpoint) {
				t.Fatal("None security escaped the managed loopback Wayland boundary")
			}
		})
	}

	authenticate := func(allowNone bool) error {
		client, server := net.Pipe()
		defer client.Close()
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer server.Close()
			_, _ = server.Write([]byte("RFB 003.008\n"))
			version := make([]byte, 12)
			if _, err := io.ReadFull(server, version); err != nil {
				return
			}
			_, _ = server.Write([]byte{1, 1})
			selected := []byte{0}
			if _, err := io.ReadFull(server, selected); err != nil || selected[0] != 1 {
				return
			}
			_, _ = server.Write([]byte{0, 0, 0, 0})
		}()
		err := authenticateDirectSSHWebVNCRFBWithSecurity(client, "", allowNone)
		_ = client.Close()
		<-done
		return err
	}
	if err := authenticate(false); err == nil || !strings.Contains(err.Error(), "did not offer password authentication") {
		t.Fatalf("ordinary endpoint accepted None security: %v", err)
	}
	if err := authenticate(true); err != nil {
		t.Fatalf("managed loopback WayVNC None security failed: %v", err)
	}
}

func TestDirectSSHWebVNCStatusUsesRecordedDaemonListenerIdentity(t *testing.T) {
	daemon := localWebVNCDaemon{PID: 321, LocalPort: "5942", Alive: true}
	port, pid := directSSHWebVNCStatusListenerIdentity("", 0, daemon)
	if port != "5942" || pid != 321 {
		t.Fatalf("listener identity port=%q pid=%d", port, pid)
	}
	port, pid = directSSHWebVNCStatusListenerIdentity("5999", 123, daemon)
	if port != "5999" || pid != 123 {
		t.Fatalf("explicit listener identity was overwritten: port=%q pid=%d", port, pid)
	}
	daemon.Stale = true
	port, pid = directSSHWebVNCStatusListenerIdentity("", 0, daemon)
	if port != "" || pid != 0 {
		t.Fatalf("stale listener identity was trusted: port=%q pid=%d", port, pid)
	}
}

func TestDirectSSHWebVNCCredentialProbeRequiresListenerOwnerPID(t *testing.T) {
	err := verifyDirectSSHWebVNCListenerOwner("6080", 0)
	if err == nil || !strings.Contains(err.Error(), "owner PID is required") {
		t.Fatalf("zero owner PID error=%v", err)
	}
}

func TestConnectWebVNCBridgeRegistersAgentBeforeServe(t *testing.T) {
	tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcpListener.Close()
	go func() {
		conn, err := tcpListener.Accept()
		if err == nil {
			_ = conn.Close()
		}
	}()

	agentConnected := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/leases/cbx_abcdef123456/webvnc/ticket":
			if r.Method != http.MethodPost {
				t.Errorf("ticket method=%s", r.Method)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
				t.Errorf("authorization=%q", got)
			}
			_ = json.NewEncoder(w).Encode(CoordinatorWebVNCTicket{
				Ticket:  "wvnc_abcdef1234567890abcdef1234567890",
				LeaseID: "cbx_abcdef123456",
			})
		case "/v1/leases/cbx_abcdef123456/webvnc/agent":
			if got := r.URL.Query().Get("ticket"); got != "" {
				t.Errorf("query ticket=%q", got)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
				t.Errorf("bridge authorization=%q", got)
			}
			if got := r.Header.Get("X-Crabbox-Bridge-Ticket"); got != "wvnc_abcdef1234567890abcdef1234567890" {
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
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, port, err := net.SplitHostPort(tcpListener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	coord := &CoordinatorClient{BaseURL: server.URL, Token: "test-token", Client: server.Client()}
	bridge, err := connectWebVNCBridge(ctx, coord, "cbx_abcdef123456", "127.0.0.1", port, SSHTarget{TargetOS: targetLinux}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()

	select {
	case <-agentConnected:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestConnectWebVNCBridgeSplitOriginSendsOnlyBridgeTicket(t *testing.T) {
	tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcpListener.Close()
	go func() {
		conn, err := tcpListener.Accept()
		if err == nil {
			_ = conn.Close()
		}
	}()

	agentConnected := make(chan struct{})
	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, name := range []string{
			"Authorization",
			"CF-Access-Client-Id",
			"CF-Access-Client-Secret",
			"CF-Access-Token",
		} {
			if got := r.Header.Get(name); got != "" {
				t.Errorf("split agent header %s=%q", name, got)
			}
		}
		if got := r.Header.Get("X-Crabbox-Bridge-Ticket"); got != "wvnc_abcdef1234567890abcdef1234567890" {
			t.Errorf("split bridge ticket=%q", got)
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
	defer agentServer.Close()
	t.Setenv("CRABBOX_WEBVNC_AGENT_BASE_URL", agentServer.URL)

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/leases/cbx_abcdef123456/webvnc/ticket" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer coordinator-token" {
			t.Errorf("ticket authorization=%q", got)
		}
		for name, want := range map[string]string{
			"CF-Access-Client-Id":     "access-client-id",
			"CF-Access-Client-Secret": "access-client-secret",
			"CF-Access-Token":         "access-jwt",
		} {
			if got := r.Header.Get(name); got != want {
				t.Errorf("ticket header %s=%q, want %q", name, got, want)
			}
		}
		_ = json.NewEncoder(w).Encode(CoordinatorWebVNCTicket{
			Ticket:  "wvnc_abcdef1234567890abcdef1234567890",
			LeaseID: "cbx_abcdef123456",
		})
	}))
	defer apiServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, port, err := net.SplitHostPort(tcpListener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	coord := &CoordinatorClient{
		BaseURL: apiServer.URL,
		Token:   "coordinator-token",
		Access: AccessConfig{
			ClientID:     "access-client-id",
			ClientSecret: "access-client-secret",
			Token:        "access-jwt",
		},
		Client: apiServer.Client(),
	}
	bridge, err := connectWebVNCBridge(ctx, coord, "cbx_abcdef123456", "127.0.0.1", port, SSHTarget{TargetOS: targetMacOS}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()

	select {
	case <-agentConnected:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestWebVNCBridgeThemeControlDoesNotBlockRFB(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	themeStarted := make(chan string, 1)
	serverResult := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			serverResult <- err
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "test done")
		go func() {
			for {
				if _, _, err := conn.Read(ctx); err != nil {
					return
				}
			}
		}()
		if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"desktop_theme","theme":"light"}`)); err != nil {
			serverResult <- err
			return
		}
		if err := conn.Write(ctx, websocket.MessageBinary, []byte("rfb-frame")); err != nil {
			serverResult <- err
			return
		}
		pingCtx, cancelPing := context.WithTimeout(ctx, time.Second)
		err = conn.Ping(pingCtx)
		cancelPing()
		serverResult <- err
		<-ctx.Done()
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	bridgeTCP, peerTCP := net.Pipe()
	defer peerTCP.Close()
	bridge := &webVNCBridge{
		tcp:                 bridgeTCP,
		ws:                  ws,
		target:              SSHTarget{TargetOS: targetLinux},
		desktopThemeUpdates: make(chan string, 1),
		applyDesktopThemeFunc: func(ctx context.Context, theme string) error {
			themeStarted <- theme
			<-ctx.Done()
			return context.Cause(ctx)
		},
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- bridge.Serve(ctx) }()

	select {
	case theme := <-themeStarted:
		if theme != "light" {
			t.Fatalf("theme=%q", theme)
		}
	case <-ctx.Done():
		t.Fatal("theme worker did not start")
	}
	if err := peerTCP.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, len("rfb-frame"))
	if _, err := io.ReadFull(peerTCP, frame); err != nil {
		t.Fatalf("RFB frame was blocked by desktop theme update: %v", err)
	}
	if string(frame) != "rfb-frame" {
		t.Fatalf("RFB frame=%q", frame)
	}
	select {
	case err := <-serverResult:
		if err != nil {
			t.Fatalf("WebSocket ping failed while desktop theme update was blocked: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("WebSocket ping was blocked by desktop theme update")
	}
	cancel()
	select {
	case <-serveDone:
	case <-time.After(time.Second):
		t.Fatal("bridge did not stop after cancellation")
	}
}

func TestWebVNCBridgeThemeUpdatesKeepLatestPendingValue(t *testing.T) {
	bridge := &webVNCBridge{desktopThemeUpdates: make(chan string, 1)}
	bridge.queueDesktopThemeUpdate("light")
	bridge.queueDesktopThemeUpdate("dark")
	select {
	case theme := <-bridge.desktopThemeUpdates:
		if theme != "dark" {
			t.Fatalf("pending theme=%q", theme)
		}
	default:
		t.Fatal("latest desktop theme update was dropped")
	}
}

func TestWebVNCDesktopThemeApplyTimeoutCoversSSHFallbackCandidates(t *testing.T) {
	target := SSHTarget{Port: "2222", FallbackPorts: []string{"22", "2200", "2222"}}
	want := 3 * webVNCDesktopThemeSSHAttemptTimeout
	if got := webVNCDesktopThemeApplyTimeout(target); got != want {
		t.Fatalf("theme apply timeout=%s, want %s for all SSH port candidates", got, want)
	}
}

func TestWebVNCDesktopThemeCommand(t *testing.T) {
	got := webVNCDesktopThemeCommand("light", "demo user")
	for _, want := range []string{
		"/usr/local/bin/crabbox-configure-desktop-theme",
		"grep -q 'desktop-theme' /usr/local/bin/crabbox-configure-desktop-theme",
		"/usr/local/bin/crabbox-start-desktop",
		"grep -q 'desktop-theme' /usr/local/bin/crabbox-start-desktop",
		"/var/lib/crabbox/desktop.env",
		"CRABBOX_DESKTOP_ENV=gnome",
		"CRABBOX_DESKTOP_USER='demo user'",
		"CRABBOX_SSH_USER='demo user'",
		"theme='light'",
		"prefer-light",
		"org.gnome.Terminal.ProfilesList",
		"background-color",
		"#f8fafc",
		"$config_dir/labwc/themerc-override",
		"window.active.title.bg.color",
		"window.active.button.unpressed.image.color",
		`LABWC_PID="$labwc_pid"`,
		"labwc --reconfigure",
		`kill -HUP "$labwc_pid"`,
		"$config_dir/gtk-3.0/gtk.css",
		"menubar menuitem",
		"desktop-background-$theme.svg",
		`swaybg -i "$wallpaper_file" -m fill`,
		`status=$?`,
		`[ "$status" -lt 128 ] || exit "$status"`,
		`exec env XDG_RUNTIME_DIR="$runtime"`,
		`) </dev/null >/tmp/crabbox-swaybg.log 2>&1 &`,
		"gnome-panel",
		"gnome-terminal",
		"gnome-terminal-theme",
		"/gnome-terminal-server",
		"NO_AT_BRIDGE=1",
		"light",
		"DISPLAY=:99",
		"exit 127",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("theme command missing %q in %s", want, got)
		}
	}
	if strings.Contains(got, "2>&1) &") {
		t.Fatalf("theme command leaves the swaybg wrapper attached to SSH: %s", got)
	}
	if strings.Contains(got, `|| XDG_RUNTIME_DIR="$runtime"`) {
		t.Fatalf("theme command can launch a stale fallback swaybg after termination: %s", got)
	}
	if got := webVNCDesktopThemeCommand("light; touch /tmp/pwned", ""); strings.Contains(got, "touch") || strings.Contains(got, "pwned") {
		t.Fatalf("invalid theme reached remote command: %s", got)
	}
}

func TestWebVNCDesktopThemeCapabilityCommandAllowsLegacyGnome(t *testing.T) {
	got := webVNCDesktopThemeCapabilityCommand()
	for _, want := range []string{
		"/usr/local/bin/crabbox-configure-desktop-theme",
		"/usr/local/bin/crabbox-start-desktop",
		"/var/lib/crabbox/desktop.env",
		"CRABBOX_DESKTOP_ENV=gnome",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("capability command missing %q in %s", want, got)
		}
	}
}

func TestRetryableWebVNCBridgeErrors(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		retryable bool
	}{
		{
			name:      "viewer disconnected",
			err:       errors.New(`failed to get reader: received close frame: status = StatusInternalError and reason = "WebVNC viewer disconnected"`),
			retryable: true,
		},
		{
			name:      "newer viewer",
			err:       errors.New(`received close frame: status = StatusServiceRestart and reason = "replaced by a newer WebVNC viewer"`),
			retryable: true,
		},
		{
			name:      "websocket eof",
			err:       errors.New(`failed to get reader: failed to read frame header: EOF`),
			retryable: true,
		},
		{
			name:      "normal close",
			err:       errors.New(`received close frame: status = StatusNormalClosure and reason = "test done"`),
			retryable: true,
		},
		{
			name:      "nil",
			err:       nil,
			retryable: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryableWebVNCBridgeError(tc.err); got != tc.retryable {
				t.Fatalf("retryable=%v, want %v", got, tc.retryable)
			}
		})
	}
}

func TestClassifyWebVNCBridgeProblem(t *testing.T) {
	if got := classifyWebVNCBridgeProblem(errors.New(`received close frame: replaced by a newer WebVNC viewer`)); got != rescueVNCStaleViewer {
		t.Fatalf("problem=%q, want %q", got, rescueVNCStaleViewer)
	}
	if got := classifyWebVNCBridgeProblem(errors.New(`failed to read frame header: EOF`)); got != rescueVNCBridgeDisconnected {
		t.Fatalf("problem=%q, want %q", got, rescueVNCBridgeDisconnected)
	}
}

func TestNextWebVNCBridgeFailureBacksOffInitialFailures(t *testing.T) {
	attempt, kind := nextWebVNCBridgeFailure(false, 0)
	if attempt != 1 || kind != "initial-error" {
		t.Fatalf("first failure attempt=%d kind=%q", attempt, kind)
	}
	attempt, kind = nextWebVNCBridgeFailure(false, attempt)
	if attempt != 2 || kind != "retry" {
		t.Fatalf("second initial failure attempt=%d kind=%q", attempt, kind)
	}
	if got := webVNCReconnectDelay(attempt); got != time.Second {
		t.Fatalf("second initial failure delay=%s, want 1s", got)
	}
	attempt, kind = nextWebVNCBridgeFailure(true, 0)
	if attempt != 1 || kind != "retry" {
		t.Fatalf("post-connect failure attempt=%d kind=%q", attempt, kind)
	}
}

func TestWebVNCObserverSlotsExhausted(t *testing.T) {
	if !webVNCObserverSlotsExhausted(CoordinatorWebVNCStatus{
		BridgeConnected:      true,
		ViewerCount:          4,
		AvailableViewerSlots: 0,
	}) {
		t.Fatal("expected full viewer pool to be exhausted")
	}
	if !webVNCObserverSlotsExhausted(CoordinatorWebVNCStatus{
		BridgeConnected:      true,
		AvailableViewerSlots: 0,
		Message:              "waiting for an available WebVNC observer slot",
	}) {
		t.Fatal("expected exhausted status message to be exhausted")
	}
	if webVNCObserverSlotsExhausted(CoordinatorWebVNCStatus{
		BridgeConnected: true,
	}) {
		t.Fatal("old bridge-only status must not be treated as exhausted")
	}
	if webVNCObserverSlotsExhausted(CoordinatorWebVNCStatus{
		BridgeConnected:      true,
		ViewerCount:          1,
		AvailableViewerSlots: 2,
	}) {
		t.Fatal("available slots must not be exhausted")
	}
}

func TestRetryBridgeTicketInAuthorization(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusUnauthorized,
		Body:       io.NopCloser(strings.NewReader("old broker needs query ticket")),
	}
	if !retryBridgeTicketInAuthorization(resp, errors.New("websocket rejected")) {
		t.Fatal("expected unauthorized websocket response to retry with bearer ticket")
	}
	if !retryBridgeTicketInAuthorization(&http.Response{StatusCode: http.StatusForbidden}, errors.New("forbidden")) {
		t.Fatal("expected upstream auth rejection to retry with bearer ticket")
	}
	if retryBridgeTicketInAuthorization(resp, nil) {
		t.Fatal("successful dial should not retry with bearer ticket")
	}
}

func TestWebVNCDaemonStatusSubcommandStaysLocalDaemonCheck(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	if err := app.webvnc(context.Background(), []string{"daemon", "status", "--id", "pearl-krill"}); err != nil {
		t.Fatal(err)
	}
	got := stdout.String()
	if !strings.Contains(got, "webvnc daemon: no pid file for pearl-krill") {
		t.Fatalf("status output=%q", got)
	}
	if strings.Contains(got, "requires a configured coordinator") {
		t.Fatalf("daemon status must not require coordinator: %q", got)
	}
}

func TestWebVNCLegacyStatusAndStopFlagsStayLocalDaemonChecks(t *testing.T) {
	for _, args := range [][]string{
		{"--id", "pearl-krill", "--status"},
		{"--id", "pearl-krill", "--stop"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			var stdout bytes.Buffer
			app := App{Stdout: &stdout, Stderr: io.Discard}
			if err := app.webvnc(context.Background(), args); err != nil {
				t.Fatal(err)
			}
			got := stdout.String()
			if !strings.Contains(got, "webvnc daemon: no pid file for pearl-krill") {
				t.Fatalf("legacy daemon output=%q", got)
			}
			if strings.Contains(got, "requires a configured coordinator") {
				t.Fatalf("legacy daemon flag must not require coordinator: %q", got)
			}
		})
	}
}

func TestNativeVNCFallbackCommand(t *testing.T) {
	got := nativeVNCOpenCommand(
		Config{Provider: "aws", TargetOS: targetWindows, WindowsMode: windowsModeWSL2},
		SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2},
		"cbx_1",
	)
	if got != "crabbox vnc --provider aws --target windows --windows-mode wsl2 --id cbx_1 --open" {
		t.Fatalf("fallback=%q", got)
	}
}

func TestNativeVNCFallbackCommandCarriesNetworkOverride(t *testing.T) {
	got := nativeVNCOpenCommand(
		Config{Provider: "aws", TargetOS: targetLinux, Network: NetworkTailscale},
		SSHTarget{TargetOS: targetLinux},
		"cbx_1",
	)
	if got != "crabbox vnc --provider aws --target linux --network tailscale --id cbx_1 --open" {
		t.Fatalf("fallback=%q", got)
	}
}

func TestResolvedWebVNCCommandConfigPrefersResolvedLeaseProvider(t *testing.T) {
	cfg := resolvedWebVNCCommandConfig(
		Config{Provider: "azure", TargetOS: targetLinux},
		Server{Provider: "aws"},
		SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2},
	)
	got := nativeVNCOpenCommand(cfg, SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}, "cbx_1")
	if got != "crabbox vnc --provider aws --target windows --windows-mode wsl2 --id cbx_1 --open" {
		t.Fatalf("fallback=%q", got)
	}

	bridge := strings.Join(
		webVNCBridgeArgs(cfg, SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}, "cbx_1", true, false),
		" ",
	)
	if bridge != "--provider aws --target windows --windows-mode wsl2 --id cbx_1 --open" {
		t.Fatalf("bridge args=%q", bridge)
	}
}

func TestWebVNCBridgeArgsCarriesNetworkOverride(t *testing.T) {
	got := strings.Join(webVNCBridgeArgs(
		Config{Provider: "aws", TargetOS: targetLinux, Network: NetworkTailscale},
		SSHTarget{TargetOS: targetLinux},
		"cbx_1",
		true,
		true,
	), " ")
	if got != "--provider aws --target linux --network tailscale --id cbx_1 --open --take-control" {
		t.Fatalf("bridge args=%q", got)
	}
}

func TestWebVNCBridgePoolSizeForTarget(t *testing.T) {
	if got := webVNCBridgePoolSizeForTarget(SSHTarget{TargetOS: targetMacOS}); got != 2 {
		t.Fatalf("macOS pool size=%d, want 2", got)
	}
	if got := webVNCBridgePoolSizeForTarget(SSHTarget{TargetOS: targetLinux}); got != defaultWebVNCBridgePoolSize {
		t.Fatalf("linux pool size=%d, want default", got)
	}
}

func TestEnsureOpenWebVNCPortalAccessSharesOrgUse(t *testing.T) {
	var putBody CoordinatorShare
	var gotPut bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/leases/cbx_abcdef123456/share":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"share": CoordinatorShare{
					Users: map[string]CoordinatorShareRole{"friend@example.com": CoordinatorShareUse},
				},
			})
		case r.Method == http.MethodPut && r.URL.Path == "/v1/leases/cbx_abcdef123456/share":
			gotPut = true
			if err := json.NewDecoder(r.Body).Decode(&putBody); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"share": putBody})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var stdout bytes.Buffer
	coord := &CoordinatorClient{BaseURL: server.URL, Token: "test-token", Client: server.Client()}
	if err := ensureOpenWebVNCPortalAccess(context.Background(), coord, "cbx_abcdef123456", true, &stdout); err != nil {
		t.Fatal(err)
	}
	if !gotPut {
		t.Fatal("expected org share update")
	}
	if putBody.Org != CoordinatorShareUse {
		t.Fatalf("org role=%q", putBody.Org)
	}
	if putBody.Users["friend@example.com"] != CoordinatorShareUse {
		t.Fatalf("existing user share not preserved: %#v", putBody.Users)
	}
	if !strings.Contains(stdout.String(), "portal share: org=use") {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestEnsureOpenWebVNCPortalAccessSkipsWhenNotOpening(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		http.NotFound(w, r)
	}))
	defer server.Close()

	coord := &CoordinatorClient{BaseURL: server.URL, Token: "test-token", Client: server.Client()}
	if err := ensureOpenWebVNCPortalAccess(context.Background(), coord, "cbx_abcdef123456", false, io.Discard); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("closed portal flow should not touch sharing")
	}
}

func TestEnsureOpenWebVNCPortalAccessAllowsUseOnlyCallers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/leases/cbx_abcdef123456/share":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"share": CoordinatorShare{
					Users: map[string]CoordinatorShareRole{"operator@example.com": CoordinatorShareUse},
				},
			})
		case r.Method == http.MethodPut && r.URL.Path == "/v1/leases/cbx_abcdef123456/share":
			http.Error(w, `{"error":"forbidden","message":"lease manage access required"}`, http.StatusForbidden)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var stdout bytes.Buffer
	coord := &CoordinatorClient{BaseURL: server.URL, Token: "test-token", Client: server.Client()}
	if err := ensureOpenWebVNCPortalAccess(context.Background(), coord, "cbx_abcdef123456", true, &stdout); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "portal share: skipped") {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestStripLegacyWebVNCDaemonFlags(t *testing.T) {
	got := strings.Join(stripLegacyWebVNCDaemonFlags([]string{
		"--provider",
		"aws",
		"--daemon",
		"--target",
		"linux",
		"--background=true",
		"--id",
		"pearl-krill",
		"--open",
	}), " ")
	if got != "--provider aws --target linux --id pearl-krill --open" {
		t.Fatalf("stripped args=%q", got)
	}
}

func TestWebVNCDaemonSupervisorRestartsWithoutReopeningPortal(t *testing.T) {
	prepared := prepareWebVNCDaemonArgs([]string{
		"--provider",
		"hetzner",
		"--id",
		"pearl-krill",
		"--open",
	}, true)
	args := append([]string{"webvnc"}, prepared...)
	got := webVNCDaemonSupervisorScript("/tmp/crabbox", args)
	var firstCommand, restartCommand string
	for _, line := range strings.Split(got, "\n") {
		if !strings.Contains(line, "'/tmp/crabbox' 'webvnc'") {
			continue
		}
		if strings.Contains(line, "'--open'") {
			firstCommand = line
		} else {
			restartCommand = line
		}
	}
	for name, command := range map[string]string{"first": firstCommand, "restart": restartCommand} {
		for _, want := range []string{"'--no-provider-side-effects=true'", "'--provider' 'hetzner'", "'--id' 'pearl-krill'"} {
			if !strings.Contains(command, want) {
				t.Fatalf("%s daemon command missing %q: %s", name, want, got)
			}
		}
	}
	if strings.Count(got, "--open") != 1 {
		t.Fatalf("daemon supervisor should only open portal once: %s", got)
	}
	if !strings.Contains(got, "webvnc daemon supervisor: child exited code=$code; restarting in 1s") {
		t.Fatalf("daemon supervisor missing restart log: %s", got)
	}
}

func TestWebVNCDaemonSupervisorPreservesOwnerHeartbeatPolicy(t *testing.T) {
	controllerPrepared := prepareWebVNCDaemonArgs([]string{
		"--provider",
		"aws",
		"--id",
		"pearl-krill",
		"-reclaim=true",
		"--no-provider-side-effects=false",
	}, true)
	controllerArgs := append([]string{"webvnc"}, controllerPrepared...)
	got := webVNCDaemonSupervisorScript("/tmp/crabbox", controllerArgs)
	if strings.Contains(got, "reclaim") || strings.Contains(got, "--no-provider-side-effects=false") || strings.Count(got, "--no-provider-side-effects=true") != 2 {
		t.Fatalf("daemon supervisor can mutate provider state: %s", got)
	}
	ordinaryPrepared := prepareWebVNCDaemonArgs([]string{"--id", "pearl-krill"}, false)
	ordinaryArgs := append([]string{"webvnc"}, ordinaryPrepared...)
	ordinary := webVNCDaemonSupervisorScript("/tmp/crabbox", ordinaryArgs)
	if strings.Contains(ordinary, "--no-provider-side-effects") {
		t.Fatalf("ordinary registered daemon lost coordinator heartbeat: %s", ordinary)
	}
}

func TestWebVNCDaemonSupervisorExcludesRawControllerOwnerToken(t *testing.T) {
	rawOwnerToken := strings.Repeat("ab", sha256.Size)
	ownerIDHash := sha256.Sum256([]byte("crabbox:webvnc-owner-id:v1\x00" + rawOwnerToken))
	ownerID := fmt.Sprintf("%x", ownerIDHash[:])
	prepared := prepareWebVNCDaemonArgs([]string{"--id", "pearl-krill"}, true)
	args := append([]string{"webvnc"}, prepared...)
	args = append(args, "--controller-owner-id", ownerID)
	script := webVNCDaemonSupervisorScript("/tmp/crabbox", args)
	if strings.Contains(script, rawOwnerToken) {
		t.Fatalf("raw controller owner token leaked into daemon argv: %s", script)
	}
	if !strings.Contains(script, ownerID) {
		t.Fatalf("daemon argv lost its public owner identity: %s", script)
	}
}

func TestWebVNCDaemonStatusRedactsControllerOwnerIdentity(t *testing.T) {
	ownerID := strings.Repeat("cd", sha256.Size)
	status := localWebVNCDaemon{
		LeaseID:               "workspace-a",
		PID:                   123,
		LogPath:               "/tmp/webvnc.log",
		Command:               "/tmp/crabbox webvnc --controller-owner-id " + ownerID,
		ControllerOwned:       true,
		NoProviderSideEffects: true,
		ControllerOwnerID:     ownerID,
	}
	var output bytes.Buffer
	printLocalWebVNCDaemonStatus(&output, status, ownerID)
	got := output.String()
	if strings.Contains(got, ownerID) || strings.Contains(got, "owner-token") {
		t.Fatalf("controller owner identity leaked into daemon status: %q", got)
	}
	if !strings.Contains(got, "owner-match=true") || !strings.Contains(got, "--controller-owner-id [redacted]") {
		t.Fatalf("daemon status lost redacted ownership proof: %q", got)
	}
	identityJSON, err := json.Marshal(webVNCDaemonIdentity{ControllerOwnerID: ownerID})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(identityJSON), "controllerOwnerToken") {
		t.Fatalf("new daemon identity retained the legacy owner-token field: %s", identityJSON)
	}
}

func TestWebVNCDaemonSupervisorWaitsForIdentityHandshake(t *testing.T) {
	got := webVNCDaemonGatedSupervisorScript("/tmp/crabbox", []string{"webvnc", "--id", "pearl-krill"})
	gate := "IFS= read -r gate || exit 125\n[ \"$gate\" = run ] || exit 125\n"
	if !strings.HasPrefix(got, gate) {
		t.Fatalf("daemon supervisor can launch before identity handshake: %s", got)
	}
	if strings.Index(got, "webvnc daemon supervisor: starting") < len(gate) {
		t.Fatalf("daemon supervisor starts before launch gate: %s", got)
	}
}

func TestWebVNCDaemonLogReady(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.log")
	if webVNCDaemonLogReady(path, 0) {
		t.Fatal("missing log must not be ready")
	}
	if err := os.WriteFile(path, []byte("bridge: probing VNC\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if webVNCDaemonLogReady(path, 0) {
		t.Fatal("probe-only log must not be ready")
	}
	if err := os.WriteFile(path, []byte("bridge: connected; keep this process running while using WebVNC\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !webVNCDaemonLogReady(path, 0) {
		t.Fatal("connected log must be ready")
	}
}

func TestSafeWebVNCDaemonName(t *testing.T) {
	if got := safeWebVNCDaemonName("pearl/krill :99"); got != "pearl_krill__99" {
		t.Fatalf("safe daemon name=%q", got)
	}
}

func TestWebVNCDaemonLockSerializesWorkspaceState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	firstUnlock, err := acquireWebVNCDaemonLock("workspace-lock")
	if err != nil {
		t.Fatal(err)
	}
	secondStarted := make(chan struct{})
	secondAcquired := make(chan error, 1)
	releaseSecond := make(chan struct{})
	go func() {
		close(secondStarted)
		unlock, err := acquireWebVNCDaemonLock("workspace-lock")
		secondAcquired <- err
		if err != nil {
			return
		}
		<-releaseSecond
		unlock()
	}()
	<-secondStarted
	select {
	case err := <-secondAcquired:
		firstUnlock()
		t.Fatalf("second workspace lock bypassed the first: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	firstUnlock()
	select {
	case err := <-secondAcquired:
		if err != nil {
			t.Fatal(err)
		}
		close(releaseSecond)
	case <-time.After(2 * time.Second):
		t.Fatal("second workspace lock did not acquire after release")
	}
}

func TestReadWebVNCDaemonPID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.pid")
	if err := os.WriteFile(path, []byte("12345\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readWebVNCDaemonPID(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != 12345 {
		t.Fatalf("pid=%d", got)
	}
}

func TestWebVNCDaemonStatusRequiresExactWorkspaceAndProcessIdentity(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	nonce := "0123456789abcdef0123456789abcdef"
	cmd := startTestWebVNCDaemonProcess(t, nonce)
	started, err := webVNCDaemonProcessStartIdentity(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	_, pidPath, err := webVNCDaemonPaths("workspace-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeWebVNCDaemonIdentity(pidPath, webVNCDaemonIdentity{
		Version: webVNCDaemonIdentityVersion, WorkspaceID: "workspace-b", PID: cmd.Process.Pid,
		ProcessStarted: started, BootID: currentProcessBootIdentityForTest(t), Nonce: nonce,
	}); err != nil {
		t.Fatal(err)
	}
	status, err := localWebVNCDaemonStatus("workspace-a")
	if err != nil {
		t.Fatal(err)
	}
	if !status.Stale {
		t.Fatalf("cross-workspace daemon reported reusable: %#v", status)
	}
	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	if _, err := app.stopWebVNCDaemonIfRunning("workspace-a"); err == nil {
		t.Fatal("cross-workspace daemon stop was not refused")
	}
	if _, alive := webVNCDaemonProcessCommand(cmd.Process.Pid); !alive {
		t.Fatal("cross-workspace daemon was killed")
	}
}

func TestWebVNCDaemonStopDoesNotSignalRecycledPID(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	nonce := "fedcba9876543210fedcba9876543210"
	cmd := startTestWebVNCDaemonProcess(t, nonce)
	started, err := webVNCDaemonProcessStartIdentity(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	_, pidPath, err := webVNCDaemonPaths("workspace-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeWebVNCDaemonIdentity(pidPath, webVNCDaemonIdentity{
		Version: webVNCDaemonIdentityVersion, WorkspaceID: "workspace-a", PID: cmd.Process.Pid,
		ProcessStarted: started + "-stale", BootID: currentProcessBootIdentityForTest(t), Nonce: nonce,
	}); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	stopped, err := app.stopWebVNCDaemonIfRunning("workspace-a")
	if err == nil || stopped || !strings.Contains(err.Error(), "refusing to drop unverified") {
		t.Fatalf("stale identity cleanup stopped=%t output=%q err=%v", stopped, stdout.String(), err)
	}
	if _, alive := webVNCDaemonProcessCommand(cmd.Process.Pid); !alive {
		t.Fatal("recycled pid target was killed")
	}
	if _, err := os.Stat(pidPath); err != nil {
		t.Fatalf("stale identity handle was removed: %v", err)
	}
}

func TestWebVNCDaemonStopSignalsOnlyVerifiedIdentity(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	nonce := "00112233445566778899aabbccddeeff"
	cmd := startTestWebVNCDaemonProcess(t, nonce)
	started, err := webVNCDaemonProcessStartIdentity(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	_, pidPath, err := webVNCDaemonPaths("workspace-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeWebVNCDaemonIdentity(pidPath, webVNCDaemonIdentity{
		Version: webVNCDaemonIdentityVersion, WorkspaceID: "workspace-a", PID: cmd.Process.Pid,
		LocalPort: "5942", ProcessStarted: started, BootID: currentProcessBootIdentityForTest(t), Nonce: nonce,
	}); err != nil {
		t.Fatal(err)
	}
	status, err := localWebVNCDaemonStatus("workspace-a")
	if err != nil {
		t.Fatal(err)
	}
	if status.Stale || !status.Alive {
		t.Fatalf("verified daemon not reusable: %#v", status)
	}
	if status.PID != cmd.Process.Pid || status.LocalPort != "5942" {
		t.Fatalf("recorded daemon listener identity lost: %#v", status)
	}
	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	stopped, err := app.stopWebVNCDaemonIfRunning("workspace-a")
	if err != nil {
		t.Fatal(err)
	}
	if !stopped || !strings.Contains(stdout.String(), "stopped pid=") {
		t.Fatalf("verified daemon stop stopped=%t output=%q", stopped, stdout.String())
	}
	_ = cmd.Wait()
}

func TestLegacyControllerOwnerTokenIdentityIsStaleButStoppable(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	nonce := "aabbccddeeff00112233445566778899"
	cmd := startTestWebVNCDaemonProcess(t, nonce)
	started, err := webVNCDaemonProcessStartIdentity(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	_, pidPath, err := webVNCDaemonPaths("workspace-legacy")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o700); err != nil {
		t.Fatal(err)
	}
	legacyToken := strings.Repeat("ef", sha256.Size)
	if err := writeWebVNCDaemonIdentity(pidPath, webVNCDaemonIdentity{
		Version: webVNCDaemonIdentityVersion, WorkspaceID: "workspace-legacy", PID: cmd.Process.Pid,
		ProcessStarted: started, BootID: currentProcessBootIdentityForTest(t), Nonce: nonce, ControllerOwned: true, NoProviderSideEffects: true,
		LegacyOwnerToken: legacyToken,
	}); err != nil {
		t.Fatal(err)
	}
	status, err := localWebVNCDaemonStatus("workspace-legacy")
	if err != nil {
		t.Fatal(err)
	}
	if !status.Stale {
		t.Fatalf("legacy owner-token identity remained reusable: %#v", status)
	}
	var output bytes.Buffer
	printLocalWebVNCDaemonStatus(&output, status, "")
	if strings.Contains(output.String(), legacyToken) {
		t.Fatalf("legacy owner token leaked into stale status: %q", output.String())
	}
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	stopped, err := app.stopWebVNCDaemonIfRunning("workspace-legacy")
	if err != nil || !stopped {
		t.Fatalf("legacy verified daemon stop stopped=%t err=%v", stopped, err)
	}
	_ = cmd.Wait()
}

func startTestWebVNCDaemonProcess(t *testing.T, nonce string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sh", "-c", "while :; do sleep 1; done", "crabbox-webvnc-test", nonce)
	configureDaemonCommand(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stopDaemonProcess(cmd.Process, cmd.Process.Pid)
		_ = cmd.Wait()
	})
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if command, alive := webVNCDaemonProcessCommand(cmd.Process.Pid); alive && strings.Contains(command, nonce) {
			return cmd
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("test WebVNC daemon did not become observable")
	return nil
}

func currentProcessBootIdentityForTest(t *testing.T) string {
	t.Helper()
	bootID, err := processBootIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return bootID
}

func TestIsWebVNCDaemonCommand(t *testing.T) {
	if !isWebVNCDaemonCommand("/usr/local/bin/crabbox webvnc --id pearl-krill") {
		t.Fatal("expected crabbox webvnc command")
	}
	if isWebVNCDaemonCommand("/bin/sleep 999") {
		t.Fatal("sleep must not be treated as WebVNC daemon")
	}
}

func TestControllerWebVNCResolveIsIdentityBoundAndReadOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	leaseID := "cbx_abcdef123456"
	expected := webVNCExpectedProviderIdentity{
		Identity: ProviderIdentityExpectation{
			LeaseID:        leaseID,
			AttemptLeaseID: leaseID,
			Slug:           "mac-lab",
			ResourceID:     "provider/workspace-123",
		},
		Scope: "test-external:provider-command",
		set:   true,
	}
	var request ResolveRequest
	testExternalResolveHook = func(req ResolveRequest) (LeaseTarget, error) {
		request = req
		return LeaseTarget{
			LeaseID: leaseID,
			Server: Server{
				CloudID: "provider/workspace-123",
				Labels:  map[string]string{"slug": "mac-lab"},
			},
		}, nil
	}
	t.Cleanup(func() { testExternalResolveHook = nil })

	cfg := BaseConfig()
	cfg.Provider = "external"
	cfg.External.Command = "provider-command"
	cfg.External.Capabilities.IdempotentLeaseID = true
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	server, _, gotLeaseID, err := app.resolveWebVNCLeaseTarget(context.Background(), cfg, leaseID, false, true, expected)
	if err != nil {
		t.Fatal(err)
	}
	if gotLeaseID != leaseID || server.DisplayID() != expected.Identity.ResourceID {
		t.Fatalf("resolved lease=%q server=%#v", gotLeaseID, server)
	}
	if !request.NoLocalStateMutations || request.ExpectedProviderIdentity != expected.Identity {
		t.Fatalf("resolve request=%#v", request)
	}
}

func TestVNCForegroundTunnelReportsSSHExit(t *testing.T) {
	if runtime.GOOS == "windows" || !controllerListenerOwnershipSupported() {
		t.Skip("process listener ownership fixture")
	}
	dir := t.TempDir()
	sshPath := filepath.Join(dir, "ssh")
	if err := os.WriteFile(sshPath, []byte("#!/bin/sh\nexec \"$CRABBOX_TEST_BINARY\" -test.run=TestVNCForegroundTunnelHelperProcess -- \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_TEST_BINARY", executable)
	t.Setenv("CRABBOX_VNC_TUNNEL_HELPER", "1")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	port := availableControllerListenerTestPort(t)
	tunnel, err := startVNCForegroundTunnel(context.Background(), SSHTarget{Host: "example.invalid", User: "tester", Port: "22"}, port, "127.0.0.1", "5900")
	if err != nil {
		t.Fatal(err)
	}
	defer stopProcess(tunnel)
	select {
	case <-tunnel.Done():
		if err := tunnel.ExitError(); err == nil || !strings.Contains(err.Error(), "intentional tunnel exit") {
			t.Fatalf("tunnel exit error=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("foreground tunnel did not report SSH process exit")
	}
}

func TestVNCForegroundTunnelHelperProcess(t *testing.T) {
	if os.Getenv("CRABBOX_VNC_TUNNEL_HELPER") != "1" {
		return
	}
	forward := ""
	for i, arg := range os.Args {
		if arg == "-L" && i+1 < len(os.Args) {
			forward = os.Args[i+1]
			break
		}
	}
	parts := strings.Split(forward, ":")
	if len(parts) < 4 {
		fmt.Fprintln(os.Stderr, "missing SSH forwarding argument")
		os.Exit(22)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", parts[1]))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(22)
	}
	// Coverage and race instrumentation can make the parent-side process/socket
	// ownership scan noticeably slower than an ordinary run. Keep the helper
	// listener alive long enough for that exact-ownership proof to complete.
	time.Sleep(3 * time.Second)
	_ = listener.Close()
	fmt.Fprintln(os.Stderr, "intentional tunnel exit")
	os.Exit(23)
}
