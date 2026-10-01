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

	"golang.org/x/net/dns/dnsmessage"
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
				if host, _, err := net.SplitHostPort(r.RemoteAddr); err != nil || host != "::1" {
					t.Errorf("heartbeat source=%s, want IPv6 loopback", r.RemoteAddr)
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
				endpoint = "http://dualstack.example.test.:" + port
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
				dialer := &net.Dialer{Timeout: 5 * time.Second, Resolver: coordinatorLoopbackResolver(t), ControlContext: func(ctx context.Context, network, address string, conn syscall.RawConn) error {
					if network == "tcp4" {
						<-ctx.Done()
						// The fallback's canceled IPv4 attempt must not erase the preferred deadline.
						if errors.Is(ctx.Err(), context.DeadlineExceeded) {
							preferredTimedOut.Store(true)
						}
						return ctx.Err()
					}
					return nil
				}}
				transport := client.Client.Transport.(*http.Transport)
				transport.Proxy = nil
				transport.DialContext = coordinatorIPv4FirstDialer(dialer)
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

// Supply both loopback families without consulting the host's localhost records or DNS.
func coordinatorLoopbackResolver(t *testing.T) *net.Resolver {
	t.Helper()
	server, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() {
		server.Close()
		<-done
	})
	go func() {
		defer close(done)
		buffer := make([]byte, 4096)
		for {
			n, peer, err := server.ReadFrom(buffer)
			if err != nil {
				if !errors.Is(err, net.ErrClosed) {
					t.Error(err)
				}
				return
			}
			var query dnsmessage.Message
			if err := query.Unpack(buffer[:n]); err != nil {
				t.Error(err)
				return
			}
			response := dnsmessage.Message{
				Header:    dnsmessage.Header{ID: query.ID, Response: true, RecursionDesired: query.RecursionDesired, RecursionAvailable: true},
				Questions: query.Questions,
			}
			for _, question := range query.Questions {
				if question.Name.String() != "dualstack.example.test." {
					t.Errorf("unexpected DNS question: %s", question.Name)
					response.RCode = dnsmessage.RCodeNameError
					continue
				}
				answer := dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: question.Name, Type: question.Type, Class: dnsmessage.ClassINET}}
				switch question.Type {
				case dnsmessage.TypeA:
					answer.Body = &dnsmessage.AResource{A: [4]byte{127, 0, 0, 1}}
				case dnsmessage.TypeAAAA:
					answer.Body = &dnsmessage.AAAAResource{AAAA: [16]byte{15: 1}}
				default:
					continue
				}
				response.Answers = append(response.Answers, answer)
			}
			packet, err := response.Pack()
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := server.WriteTo(packet, peer); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "udp4", server.LocalAddr().String())
	}}
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
