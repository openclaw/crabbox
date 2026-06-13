package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
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
	if got := webVNCAgentURLWithTicket("https://broker.example.com", "cbx_abcdef123456", "wvnc_abc"); got != "wss://broker.example.com/v1/leases/cbx_abcdef123456/webvnc/agent?ticket=wvnc_abc" {
		t.Fatalf("agent fallback URL=%q", got)
	}
	if got := webVNCAgentURLWithTicketAndCapabilities("https://broker.example.com", "cbx_abcdef123456", "wvnc_abc", "desktop_theme"); got != "wss://broker.example.com/v1/leases/cbx_abcdef123456/webvnc/agent?capabilities=desktop_theme&ticket=wvnc_abc" {
		t.Fatalf("agent capability fallback URL=%q", got)
	}
	if got := webVNCPortalURL("https://broker.example.com/", "cbx_abcdef123456", "", "secret value"); got != "https://broker.example.com/portal/leases/cbx_abcdef123456/vnc#password=secret+value" {
		t.Fatalf("portal URL=%q", got)
	}
	if got := webVNCPortalURL("https://broker.example.com/", "cbx_abcdef123456", "ec2-user", "secret value"); got != "https://broker.example.com/portal/leases/cbx_abcdef123456/vnc#password=secret+value&username=ec2-user" {
		t.Fatalf("portal URL=%q", got)
	}
	if got := webVNCPortalURL("https://broker.example.com/", "cbx_abcdef123456", "", "Cb1!abc"); got != "https://broker.example.com/portal/leases/cbx_abcdef123456/vnc#password=Cb1%21abc" {
		t.Fatalf("portal URL=%q", got)
	}
	if got := webVNCPortalURL("https://broker.example.com/#stale", "cbx_abcdef123456", "", ""); got != "https://broker.example.com/portal/leases/cbx_abcdef123456/vnc" {
		t.Fatalf("portal URL=%q", got)
	}
	if got := webVNCPortalURL("https://broker.example.com/", "cbx_abcdef123456", "", "", webVNCPortalOptions{TakeControl: true}); got != "https://broker.example.com/portal/leases/cbx_abcdef123456/vnc#control=take" {
		t.Fatalf("portal URL with control=%q", got)
	}
	if got := webVNCPortalURL("https://broker.example.com/", "cbx_abcdef123456", "", "secret value", webVNCPortalOptions{TakeControl: true}); got != "https://broker.example.com/portal/leases/cbx_abcdef123456/vnc#control=take&password=secret+value" {
		t.Fatalf("portal URL with password and control=%q", got)
	}
	got := webVNCPortalURL("https://broker.example.com/", "cbx_abcdef123456", "", "JVS/yMb%2B")
	if got != "https://broker.example.com/portal/leases/cbx_abcdef123456/vnc#password=JVS%2FyMb%252B" {
		t.Fatalf("portal URL with escaped password=%q", got)
	}
	fragment, ok := strings.CutPrefix(got, "https://broker.example.com/portal/leases/cbx_abcdef123456/vnc#")
	if !ok {
		t.Fatalf("portal URL missing expected fragment: %q", got)
	}
	values, err := url.ParseQuery(fragment)
	if err != nil {
		t.Fatal(err)
	}
	if values.Get("password") != "JVS/yMb%2B" {
		t.Fatalf("decoded portal password=%q", values.Get("password"))
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

func TestGuardMacOSDirectWebVNC(t *testing.T) {
	// macOS desktop leases (e.g. tart) must be rejected from the Linux-only
	// noVNC bridge with native-client guidance, not silently enter it.
	err := guardMacOSDirectWebVNC(Config{Provider: "tart", TargetOS: targetMacOS, SSHUser: "admin"})
	if err == nil {
		t.Fatal("macOS lease should be guarded out of the direct WebVNC browser path")
	}
	if !strings.Contains(err.Error(), "native VNC client") || !strings.Contains(err.Error(), "ssh -L 5900") {
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

func TestIsMacOSDesktopProviderOnlyDedicatedMacOS(t *testing.T) {
	// tart's only target is macOS -> uses the host-side Screen Sharing bridge.
	if !isMacOSDesktopProvider(Config{Provider: "tart"}) {
		t.Error("tart (dedicated macOS provider) should route to the macOS bridge")
	}
	// parallels is multi-target (macOS + Linux + Windows); it must NOT be diverted
	// into the tart bridge, even for a macOS lease — regression guard.
	if isMacOSDesktopProvider(Config{Provider: "parallels"}) {
		t.Error("parallels (multi-target) must not route to the macOS bridge")
	}
	if isMacOSDesktopProvider(Config{Provider: "parallels", TargetOS: targetMacOS}) {
		t.Error("a macOS parallels lease must still use the existing WebVNC path")
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
func (directWebVNCTestProvider) CommandRoutingArgs(_ Config, leaseID string) []string {
	return []string{"--direct-webvnc-routing", "route-" + leaseID}
}

func TestWebVNCResetRemoteCommandHandlesWaylandAndX11(t *testing.T) {
	got := webVNCResetRemoteCommand(SSHTarget{TargetOS: targetLinux})
	for _, want := range []string{
		"/var/lib/crabbox/desktop.env",
		`CRABBOX_DESKTOP_ENV:-xfce`,
		"crabbox-desktop.service crabbox-wayvnc.service",
		"crabbox-desktop-session.service crabbox-x11vnc.service",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("reset command missing %q:\n%s", want, got)
		}
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
			if got := r.Header.Get("Authorization"); got != "Bearer wvnc_abcdef1234567890abcdef1234567890" {
				t.Errorf("bridge authorization=%q", got)
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
	if strings.Contains(webVNCDesktopThemeCommand("neon", ""), "neon") {
		t.Fatal("invalid theme should fall back to dark")
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

func TestRetryBridgeTicketInQuery(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusUnauthorized,
		Body:       io.NopCloser(strings.NewReader("old broker needs query ticket")),
	}
	if !retryBridgeTicketInQuery(resp, errors.New("websocket rejected")) {
		t.Fatal("expected unauthorized websocket response to retry with query ticket")
	}
	if !retryBridgeTicketInQuery(&http.Response{StatusCode: http.StatusForbidden}, errors.New("forbidden")) {
		t.Fatal("expected upstream auth rejection to retry with query ticket")
	}
	if retryBridgeTicketInQuery(resp, nil) {
		t.Fatal("successful dial should not retry with query ticket")
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
	got := webVNCDaemonSupervisorScript("/tmp/crabbox", []string{
		"webvnc",
		"--provider",
		"hetzner",
		"--id",
		"pearl-krill",
		"--open",
	})
	if !strings.Contains(got, "/tmp/crabbox' 'webvnc' '--provider' 'hetzner' '--id' 'pearl-krill' '--open' '--reclaim'") {
		t.Fatalf("first daemon command missing --open: %s", got)
	}
	if !strings.Contains(got, "/tmp/crabbox' 'webvnc' '--provider' 'hetzner' '--id' 'pearl-krill' '--reclaim'\n") {
		t.Fatalf("restart daemon command should strip --open: %s", got)
	}
	if strings.Count(got, "--open") != 1 {
		t.Fatalf("daemon supervisor should only open portal once: %s", got)
	}
	if !strings.Contains(got, "webvnc daemon supervisor: child exited code=$code; restarting in 1s") {
		t.Fatalf("daemon supervisor missing restart log: %s", got)
	}
}

func TestWebVNCDaemonSupervisorKeepsExistingReclaim(t *testing.T) {
	got := webVNCDaemonSupervisorScript("/tmp/crabbox", []string{
		"webvnc",
		"--provider",
		"aws",
		"--id",
		"pearl-krill",
		"--reclaim",
	})
	if strings.Count(got, "--reclaim") != 2 {
		t.Fatalf("daemon supervisor should keep one reclaim flag per command: %s", got)
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

func TestIsWebVNCDaemonCommand(t *testing.T) {
	if !isWebVNCDaemonCommand("/usr/local/bin/crabbox webvnc --id pearl-krill") {
		t.Fatal("expected crabbox webvnc command")
	}
	if isWebVNCDaemonCommand("/bin/sleep 999") {
		t.Fatal("sleep must not be treated as WebVNC daemon")
	}
}
