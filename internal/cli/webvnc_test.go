package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

func TestWebVNCURLs(t *testing.T) {
	if got := webVNCAgentURL("https://crabbox.openclaw.ai", "cbx_abcdef123456"); got != "wss://crabbox.openclaw.ai/v1/leases/cbx_abcdef123456/webvnc/agent" {
		t.Fatalf("agent URL=%q", got)
	}
	if got := webVNCAgentURLWithTicket("https://crabbox.openclaw.ai", "cbx_abcdef123456", "wvnc_abc"); got != "wss://crabbox.openclaw.ai/v1/leases/cbx_abcdef123456/webvnc/agent?ticket=wvnc_abc" {
		t.Fatalf("agent fallback URL=%q", got)
	}
	if got := webVNCPortalURL("https://crabbox.openclaw.ai/", "cbx_abcdef123456", "", "secret value"); got != "https://crabbox.openclaw.ai/portal/leases/cbx_abcdef123456/vnc#password=secret+value" {
		t.Fatalf("portal URL=%q", got)
	}
	if got := webVNCPortalURL("https://crabbox.openclaw.ai/", "cbx_abcdef123456", "ec2-user", "secret value"); got != "https://crabbox.openclaw.ai/portal/leases/cbx_abcdef123456/vnc#password=secret+value&username=ec2-user" {
		t.Fatalf("portal URL=%q", got)
	}
	if got := webVNCPortalURL("https://crabbox.openclaw.ai/", "cbx_abcdef123456", "", "Cb1!abc"); got != "https://crabbox.openclaw.ai/portal/leases/cbx_abcdef123456/vnc#password=Cb1%21abc" {
		t.Fatalf("portal URL=%q", got)
	}
	if got := webVNCPortalURL("https://crabbox.openclaw.ai/#stale", "cbx_abcdef123456", "", ""); got != "https://crabbox.openclaw.ai/portal/leases/cbx_abcdef123456/vnc" {
		t.Fatalf("portal URL=%q", got)
	}
	got := webVNCPortalURL("https://crabbox.openclaw.ai/", "cbx_abcdef123456", "", "JVS/yMb%2B")
	if got != "https://crabbox.openclaw.ai/portal/leases/cbx_abcdef123456/vnc#password=JVS%2FyMb%252B" {
		t.Fatalf("portal URL with escaped password=%q", got)
	}
	fragment, ok := strings.CutPrefix(got, "https://crabbox.openclaw.ai/portal/leases/cbx_abcdef123456/vnc#")
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
	bridge, err := connectWebVNCBridge(ctx, coord, "cbx_abcdef123456", "127.0.0.1", port)
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
	if retryBridgeTicketInQuery(&http.Response{StatusCode: http.StatusForbidden}, errors.New("forbidden")) {
		t.Fatal("forbidden response should not retry with query ticket")
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
	), " ")
	if got != "--provider aws --target linux --network tailscale --id cbx_1 --open" {
		t.Fatalf("bridge args=%q", got)
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
	if !strings.Contains(got, "/tmp/crabbox' 'webvnc' '--provider' 'hetzner' '--id' 'pearl-krill' '--open'") {
		t.Fatalf("first daemon command missing --open: %s", got)
	}
	if !strings.Contains(got, "/tmp/crabbox' 'webvnc' '--provider' 'hetzner' '--id' 'pearl-krill'\n") {
		t.Fatalf("restart daemon command should strip --open: %s", got)
	}
	if strings.Count(got, "--open") != 1 {
		t.Fatalf("daemon supervisor should only open portal once: %s", got)
	}
	if !strings.Contains(got, "webvnc daemon supervisor: child exited code=$code; restarting in 1s") {
		t.Fatalf("daemon supervisor missing restart log: %s", got)
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
