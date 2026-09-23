package cli

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func startTestEgressUpstreamHost(t *testing.T, proxyURL, allow string) (context.Context, context.CancelFunc, *websocket.Conn) {
	t.Helper()
	clearConfigEnv(t)
	isolateRunTestUserDirs(t, t.TempDir())
	t.Setenv("CRABBOX_TEST_UPSTREAM_PROXY", proxyURL)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	accepted := make(chan *websocket.Conn, 1)
	coordinator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept egress host: %v", err)
			return
		}
		accepted <- ws
		<-ctx.Done()
		_ = ws.CloseNow()
	}))
	result := make(chan error, 1)
	go func() {
		result <- (App{Stdout: io.Discard, Stderr: io.Discard}).egressHost(ctx, []string{
			"--id", "cbx_abcdef123456", "--coordinator", coordinator.URL,
			"--ticket", "egress_abcdef1234567890abcdef1234567890", "--session", "egress_upstream",
			"--allow", allow, "--upstream-proxy-env", "CRABBOX_TEST_UPSTREAM_PROXY",
		})
	}()
	t.Cleanup(func() {
		cancel()
		coordinator.Close()
		select {
		case <-result:
		case <-time.After(5 * time.Second):
			t.Error("egress host did not stop")
		}
	})
	select {
	case ws := <-accepted:
		return ctx, cancel, ws
	case err := <-result:
		result <- err
		t.Fatalf("egress host exited before connecting: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	return nil, nil, nil
}

func writeTestEgressFrame(t *testing.T, ctx context.Context, ws *websocket.Conn, msg egressProxyMessage) {
	t.Helper()
	body, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.Write(ctx, websocket.MessageText, body); err != nil {
		t.Fatal(err)
	}
}

func readTestEgressFrame(t *testing.T, ctx context.Context, ws *websocket.Conn) egressProxyMessage {
	t.Helper()
	_, body, err := ws.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var msg egressProxyMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Fatal(err)
	}
	return msg
}

func TestEgressHostUpstreamProxyTunnel(t *testing.T) {
	const credentials = "bridge:synthetic-proxy-password"
	const banner = "server-first"
	const payload = "remote-request"
	received := make(chan string, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "8.8.8.8:443" {
			t.Errorf("unexpected proxy request %s %s", r.Method, r.Host)
		}
		if r.Header.Get("Proxy-Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte(credentials)) {
			t.Error("upstream did not receive expected proxy credentials")
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = io.WriteString(rw, "HTTP/1.1 200 Connection Established\r\n\r\n"+banner)
		_ = rw.Flush()
		body := make([]byte, len(payload))
		_, err = io.ReadFull(rw, body)
		if err != nil {
			t.Errorf("read tunnel payload: %v", err)
		}
		received <- string(body)
	}))
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)
	proxyURL.User = url.UserPassword("bridge", "synthetic-proxy-password")
	ctx, _, ws := startTestEgressUpstreamHost(t, proxyURL.String(), "8.8.8.8")
	writeTestEgressFrame(t, ctx, ws, egressProxyMessage{Type: "open", ID: "tunnel", Host: "8.8.8.8", Port: "443"})
	if msg := readTestEgressFrame(t, ctx, ws); msg.Type != "open_ok" || msg.ID != "tunnel" {
		t.Fatalf("open result = %+v", msg)
	}
	msg := readTestEgressFrame(t, ctx, ws)
	data, _ := base64.StdEncoding.DecodeString(msg.Body)
	if msg.Type != "data" || string(data) != banner {
		t.Fatalf("tunnel banner = %+v, decoded %q", msg, data)
	}
	writeTestEgressFrame(t, ctx, ws, egressProxyMessage{Type: "data", ID: "tunnel", Body: base64.StdEncoding.EncodeToString([]byte(payload))})
	select {
	case got := <-received:
		if got != payload {
			t.Fatalf("proxy received %q", got)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestEgressHostUpstreamProxyRefusal(t *testing.T) {
	const secret = "synthetic-reflected-proxy-password"
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = io.WriteString(rw, "HTTP/1.1 407 "+secret+"\r\nContent-Length: 0\r\n\r\n")
		_ = rw.Flush()
	}))
	defer proxy.Close()
	ctx, _, ws := startTestEgressUpstreamHost(t, proxy.URL, "8.8.8.8")
	writeTestEgressFrame(t, ctx, ws, egressProxyMessage{Type: "open", ID: "refused", Host: "8.8.8.8", Port: "443"})
	msg := readTestEgressFrame(t, ctx, ws)
	if msg.Type != "error" || !strings.Contains(msg.Error, "407") || strings.Contains(msg.Error, secret) {
		t.Fatalf("refused upstream result = %+v", msg)
	}
}

func TestEgressHostUpstreamProxyRejectsDestinations(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("rejected destination reached upstream proxy")
		http.Error(w, "unexpected", http.StatusBadGateway)
	}))
	defer proxy.Close()
	ctx, _, ws := startTestEgressUpstreamHost(t, proxy.URL, "8.8.8.8,127.0.0.1")
	for _, tc := range []struct{ host, port, want string }{
		{"1.1.1.1", "443", "host not allowed"},
		{"127.0.0.1", "443", "public address"},
		{"8.8.8.8", "65536", "invalid destination port"},
	} {
		writeTestEgressFrame(t, ctx, ws, egressProxyMessage{Type: "open", ID: tc.host + tc.port, Host: tc.host, Port: tc.port})
		msg := readTestEgressFrame(t, ctx, ws)
		if msg.Type != "error" || !strings.Contains(msg.Error, tc.want) {
			t.Fatalf("rejected destination result = %+v, want %q", msg, tc.want)
		}
	}
}

func TestEgressHostUpstreamProxyCancellation(t *testing.T) {
	for _, phase := range []string{"connecting", "connected"} {
		for _, shutdown := range []string{"stream", "host"} {
			t.Run(phase+"/"+shutdown, func(t *testing.T) {
				connected, closed := make(chan struct{}), make(chan struct{})
				proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					conn, rw, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.Close()
					if phase == "connected" {
						_, _ = io.WriteString(rw, "HTTP/1.1 200 Connection Established\r\n\r\n")
						_ = rw.Flush()
					}
					close(connected)
					_, _ = io.Copy(io.Discard, bufio.NewReader(conn))
					close(closed)
				}))
				defer proxy.Close()
				ctx, cancel, ws := startTestEgressUpstreamHost(t, proxy.URL, "8.8.8.8")
				writeTestEgressFrame(t, ctx, ws, egressProxyMessage{Type: "open", ID: "cancel", Host: "8.8.8.8", Port: "443"})
				select {
				case <-connected:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if phase == "connected" {
					if msg := readTestEgressFrame(t, ctx, ws); msg.Type != "open_ok" {
						t.Fatalf("open result = %+v", msg)
					}
				}
				if shutdown == "host" {
					cancel()
				} else {
					writeTestEgressFrame(t, ctx, ws, egressProxyMessage{Type: "close", ID: "cancel"})
				}
				select {
				case <-closed:
					if shutdown == "stream" && ctx.Err() != nil {
						t.Fatal("upstream closed because the host fixture expired, not because its stream was canceled")
					}
				case <-time.After(5 * time.Second):
					t.Fatal("upstream connection survived cancellation")
				}
			})
		}
	}
}

func TestEgressUpstreamProxyPreservesCONNECTAuthority(t *testing.T) {
	const authority = "api.example.test:443"
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != authority || r.RequestURI != authority {
			t.Errorf("CONNECT authority changed: method=%s host=%s URI=%s", r.Method, r.Host, r.RequestURI)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()
	t.Setenv("CRABBOX_TEST_UPSTREAM_PROXY", proxy.URL)
	upstream, err := egressUpstreamProxyFromEnv("CRABBOX_TEST_UPSTREAM_PROXY")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := upstream.dialTunnel(t.Context(), authority)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
}

func TestEgressUpstreamProxyConfigurationFailsBeforeLeaseChanges(t *testing.T) {
	const secret = "synthetic-proxy-password"
	for _, subcommand := range []string{"start", "host", "run"} {
		for _, value := range []string{"", "socks5://proxy.example.test", "http://user:" + secret + "@proxy.example.test:bad", "http://user:" + secret + "@proxy.example.test/path", "https://proxy.example.test?token=" + secret} {
			t.Run(subcommand+"/"+strconv.Itoa(len(value)), func(t *testing.T) {
				t.Setenv("CRABBOX_TEST_UPSTREAM_PROXY", value)
				err := (App{Stdout: io.Discard, Stderr: io.Discard}).egress(t.Context(), []string{
					subcommand, "--id", "cbx_abcdef123456", "--allow", "example.com",
					"--upstream-proxy-env", "CRABBOX_TEST_UPSTREAM_PROXY",
					"--", "true",
				})
				if err == nil || !strings.Contains(err.Error(), "upstream proxy environment variable CRABBOX_TEST_UPSTREAM_PROXY") || strings.Contains(err.Error(), secret) {
					t.Fatalf("invalid proxy configuration returned %v", err)
				}
			})
		}
	}
	for _, subcommand := range []string{"start", "host", "run"} {
		err := (App{Stdout: io.Discard, Stderr: io.Discard}).egress(t.Context(), []string{
			subcommand, "--id", "cbx_abcdef123456", "--allow", "example.com", "--upstream-proxy-env", "",
			"--", "true",
		})
		if err == nil || !strings.Contains(err.Error(), "--upstream-proxy-env requires an environment variable name") {
			t.Fatalf("explicitly empty upstream option returned %v", err)
		}
	}
}

func TestEgressUpstreamProxyRefusesUntrustedTLS(t *testing.T) {
	proxy := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("untrusted TLS connection reached CONNECT")
	}))
	proxy.Config.ErrorLog = log.New(io.Discard, "", 0)
	proxy.StartTLS()
	defer proxy.Close()
	t.Setenv("CRABBOX_TEST_UPSTREAM_PROXY", proxy.URL)
	upstream, err := egressUpstreamProxyFromEnv("CRABBOX_TEST_UPSTREAM_PROXY")
	if err != nil {
		t.Fatal(err)
	}
	if conn, err := upstream.dialTunnel(t.Context(), "api.example.test:443"); err == nil || !strings.Contains(err.Error(), "TLS handshake failed") {
		if conn != nil {
			_ = conn.Close()
		}
		t.Fatalf("untrusted HTTPS proxy returned %v", err)
	}
}

func TestEgressHostUpstreamProxyDoesNotFallBackWhenUnavailable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxyURL := "http://" + listener.Addr().String()
	_ = listener.Close()
	ctx, _, ws := startTestEgressUpstreamHost(t, proxyURL, "8.8.8.8")
	writeTestEgressFrame(t, ctx, ws, egressProxyMessage{Type: "open", ID: "unavailable", Host: "8.8.8.8", Port: "443"})
	msg := readTestEgressFrame(t, ctx, ws)
	if msg.Type != "error" || msg.Error != "connect upstream proxy failed" {
		t.Fatalf("unavailable proxy result = %+v", msg)
	}
}
