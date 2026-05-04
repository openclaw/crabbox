package cli

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func TestWebVNCURLs(t *testing.T) {
	if got := webVNCAgentURL("https://crabbox.openclaw.ai", "cbx_abcdef123456", "wvnc_abc"); got != "wss://crabbox.openclaw.ai/v1/leases/cbx_abcdef123456/webvnc/agent?ticket=wvnc_abc" {
		t.Fatalf("agent URL=%q", got)
	}
	if got := webVNCPortalURL("https://crabbox.openclaw.ai/", "cbx_abcdef123456", "", "secret value"); got != "https://crabbox.openclaw.ai/portal/leases/cbx_abcdef123456/vnc#password=secret+value" {
		t.Fatalf("portal URL=%q", got)
	}
	if got := webVNCPortalURL("https://crabbox.openclaw.ai/", "cbx_abcdef123456", "ec2-user", "secret value"); got != "https://crabbox.openclaw.ai/portal/leases/cbx_abcdef123456/vnc#password=secret+value&username=ec2-user" {
		t.Fatalf("portal URL=%q", got)
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
			if got := r.URL.Query().Get("ticket"); got != "wvnc_abcdef1234567890abcdef1234567890" {
				t.Errorf("ticket=%q", got)
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
