package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func TestAWSCoordinatorHeartbeatUsesIPv4(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		t.Run(fmt.Sprintf("automatic=%t", automatic), func(t *testing.T) {
			observed := make(chan string, 2)
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer fixture-token" {
					t.Error("lost owner authentication")
				}
				if r.URL.Path == "/v1/control" {
					conn, err := websocket.Accept(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.CloseNow()
					_, data, err := conn.Read(r.Context())
					if err != nil {
						t.Error(err)
						return
					}
					var message coordinatorControlMessage
					if err := json.Unmarshal(data, &message); err != nil || message.ExpectedProvider != "aws" || message.LeaseID != "cbx_fixture" {
						t.Errorf("invalid heartbeat: %s, %v", data, err)
					}
					observed <- r.RemoteAddr
					_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"heartbeat","ok":true}`))
					_, _, _ = conn.Read(r.Context())
					return
				}
				if r.Method != http.MethodPost || r.URL.Path != "/v1/leases/cbx_fixture/heartbeat" {
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
				}
				observed <- r.RemoteAddr
				fmt.Fprint(w, `{"lease":{"id":"cbx_fixture","provider":"aws","state":"active"}}`)
			})
			v4, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			_, port, _ := net.SplitHostPort(v4.Addr().String())
			v6, err := net.Listen("tcp6", net.JoinHostPort("::1", port))
			if err != nil {
				v4.Close()
				t.Skipf("IPv6 loopback unavailable: %v", err)
			}
			for _, listener := range []net.Listener{v4, v6} {
				server := httptest.NewUnstartedServer(handler)
				server.Listener.Close()
				server.Listener = listener
				server.Start()
				t.Cleanup(server.Close)
			}
			client, _, err := newCoordinatorClient(Config{Coordinator: "http://localhost:" + port, Provider: "aws", CoordToken: "fixture-token"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(client.Client.CloseIdleConnections)
			if automatic {
				stop, err := startCoordinatorHeartbeat(t.Context(), client, "cbx_fixture", "aws", time.Hour, nil, nil, io.Discard)
				if err != nil {
					t.Fatal(err)
				}
				defer stop()
			} else if _, err := client.TouchLeaseForProvider(t.Context(), "cbx_fixture", "aws"); err != nil {
				t.Fatal(err)
			}
			select {
			case address := <-observed:
				host, _, _ := net.SplitHostPort(address)
				if net.ParseIP(host).To4() == nil {
					t.Fatalf("heartbeat observed %s; IPv4 SSH needs a current IPv4 source", address)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("heartbeat did not arrive")
			}
		})
	}
}

func TestAWSCoordinatorHeartbeatPreservesIPv6AndProxy(t *testing.T) {
	for _, test := range []struct {
		name     string
		proxy    bool
		slowIPv4 bool
	}{
		{name: "IPv6-only coordinator"},
		{name: "IPv6-only proxy", proxy: true},
		{name: "slow IPv4", slowIPv4: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			listener, err := net.Listen("tcp6", "[::1]:0")
			if err != nil {
				t.Skipf("IPv6 loopback unavailable: %v", err)
			}
			var requests atomic.Int32
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Header.Get("Authorization") != "Bearer fixture-token" {
					t.Error("lost owner authentication")
				}
				if test.proxy && r.URL.Host != "coordinator.example.test" {
					t.Errorf("proxy target=%s", r.URL)
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["expectedProvider"] != "aws" {
					t.Errorf("heartbeat body=%v err=%v", body, err)
				}
				fmt.Fprint(w, `{"lease":{"id":"cbx_fixture","provider":"aws","state":"active"}}`)
			}))
			server.Listener.Close()
			server.Listener = listener
			server.Start()
			defer server.Close()
			endpoint := server.URL
			if test.proxy {
				endpoint = "http://coordinator.example.test"
			}
			if test.slowIPv4 {
				_, port, _ := net.SplitHostPort(listener.Addr().String())
				endpoint = "http://localhost:" + port
			}
			client, _, err := newCoordinatorClient(Config{Coordinator: endpoint, Provider: "aws", CoordToken: "fixture-token"})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Client.CloseIdleConnections()
			if test.proxy {
				proxyURL, err := url.Parse(server.URL)
				if err != nil {
					t.Fatal(err)
				}
				client.Client.Transport.(*http.Transport).Proxy = http.ProxyURL(proxyURL)
			}
			var preferredTimedOut atomic.Bool
			if test.slowIPv4 {
				dialer := &net.Dialer{Timeout: 5 * time.Second, ControlContext: func(ctx context.Context, network, address string, conn syscall.RawConn) error {
					if network == "tcp4" {
						<-ctx.Done()
						preferredTimedOut.Store(errors.Is(ctx.Err(), context.DeadlineExceeded))
						return ctx.Err()
					}
					return nil
				}}
				client.Client.Transport.(*http.Transport).DialContext = coordinatorIPv4FirstDialer(dialer)
			}
			started := time.Now()
			if _, err := client.TouchLeaseForProvider(t.Context(), "cbx_fixture", "aws"); err != nil {
				t.Fatal(err)
			}
			if test.slowIPv4 && !preferredTimedOut.Load() {
				t.Fatal("did not exercise the IPv4 connection deadline")
			}
			t.Logf("heartbeat completed in %s with %d HTTP request(s)", time.Since(started), requests.Load())
			if requests.Load() != 1 {
				t.Fatalf("requests=%d, want one heartbeat", requests.Load())
			}
		})
	}
}

func TestAWSCoordinatorDialHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	dial := coordinatorIPv4FirstDialer(&net.Dialer{Timeout: 5 * time.Second})
	conn, err := dial(ctx, "tcp", "127.0.0.1:1")
	if conn != nil {
		conn.Close()
		t.Fatal("canceled dial returned a connection")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("dial error=%v, want cancellation", err)
	}
}
