package cli

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func TestCoordinatorNegotiatesSupportedHTTPProtocol(t *testing.T) {
	for _, http2 := range []bool{true, false} {
		name, want := "http1", 1
		if http2 {
			name, want = "http2", 2
		}
		t.Run(name, func(t *testing.T) {
			protocol := make(chan int, 1)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				protocol <- r.ProtoMajor
				io.WriteString(w, `{"ok":true}`)
			}))
			server.EnableHTTP2 = http2
			server.StartTLS()
			defer server.Close()
			client := coordinatorTLSFixture(t, server)
			var reply struct{ OK bool }
			if err := client.doHTTP(t.Context(), http.MethodGet, "/probe", nil, false, &reply); err != nil {
				t.Fatal(err)
			}
			if got := <-protocol; got != want || !reply.OK {
				t.Fatalf("protocol=%d reply=%v, want HTTP/%d and a successful response", got, reply, want)
			}
		})
	}
}

func TestCoordinatorCanceledEventDoesNotBlockOtherStreams(t *testing.T) {
	started := make(chan struct{})
	var connections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			close(started)
			<-r.Context().Done()
			return
		}
		io.WriteString(w, `{"ok":true}`)
	}))
	server.EnableHTTP2 = true
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.StartTLS()
	defer server.Close()
	client := coordinatorTLSFixture(t, server)
	client.Client.Transport.(*http.Transport).MaxConnsPerHost = 1
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	var slowErr error
	go func() { slowErr = client.doHTTP(ctx, http.MethodPost, "/slow", nil, false, nil); close(done) }()
	defer func() { cancel(); <-done }()
	<-started
	probeCtx, stopProbe := context.WithTimeout(t.Context(), time.Second)
	defer stopProbe()
	var reply struct{ OK bool }
	if err := client.doHTTP(probeCtx, http.MethodGet, "/probe", nil, false, &reply); err != nil || !reply.OK {
		t.Fatalf("a pending event blocked an independent request: %v", err)
	}
	cancel()
	<-done
	if !errors.Is(slowErr, context.Canceled) {
		t.Fatalf("event cancellation=%v", slowErr)
	}
	if err := client.doHTTP(t.Context(), http.MethodGet, "/after", nil, false, &reply); err != nil {
		t.Fatal(err)
	}
	if got := connections.Load(); got != 1 {
		t.Fatalf("canceled event discarded the shared connection: connections=%d", got)
	}
}

func coordinatorTLSFixture(t *testing.T, server *httptest.Server) *CoordinatorClient {
	t.Helper()
	client, configured, err := newCoordinatorClient(Config{Coordinator: server.URL})
	if err != nil || !configured {
		t.Fatalf("newCoordinatorClient configured=%t err=%v", configured, err)
	}
	transport := client.Client.Transport.(*http.Transport)
	transport.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	t.Cleanup(transport.CloseIdleConnections)
	return client
}

func TestCoordinatorControlCloseDoesNotAwaitIdlePeer(t *testing.T) {
	peerRelease := make(chan struct{})
	peerDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(peerDone)
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		<-peerRelease
	}))
	defer server.Close()
	control, err := dialCoordinatorControl(t.Context(), &CoordinatorClient{BaseURL: server.URL, Client: server.Client()})
	if err != nil {
		close(peerRelease)
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { control.close(); close(done) }()
	defer func() { close(peerRelease); <-peerDone; <-done }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ending the control owner waited for the idle peer's close handshake")
	}
	if err := control.write(t.Context(), map[string]string{"type": "ping"}); err == nil {
		t.Fatal("closed control owner still accepted a write")
	}
}

func TestCoordinatorHTTP2ClientRetainsWebSocketUpgrade(t *testing.T) {
	protocols := make(chan int, 3)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protocols <- r.ProtoMajor
		if r.URL.Path == "/v1/control" {
			http.Redirect(w, r, "/control-target", http.StatusFound)
			return
		}
		if r.URL.Path != "/control-target" {
			io.WriteString(w, `{"ok":true}`)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		if _, _, err := conn.Read(r.Context()); err != nil {
			t.Error(err)
			return
		}
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"pong"}`))
		_, _, _ = conn.Read(r.Context())
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	client := coordinatorTLSFixture(t, server)
	var reply struct{ OK bool }
	if err := client.doHTTP(t.Context(), http.MethodGet, "/probe", nil, false, &reply); err != nil {
		t.Fatal(err)
	}
	if protocol := <-protocols; protocol != 2 {
		t.Fatalf("API protocol=%d", protocol)
	}
	control, err := dialCoordinatorControl(t.Context(), client)
	if err != nil {
		t.Fatal(err)
	}
	defer control.close()
	if protocol := <-protocols; protocol != 1 {
		t.Fatalf("WebSocket upgrade protocol=%d", protocol)
	}
	if protocol := <-protocols; protocol != 1 {
		t.Fatalf("same-origin WebSocket redirect protocol=%d", protocol)
	}
	if err := control.write(t.Context(), map[string]string{"type": "ping"}); err != nil {
		t.Fatal(err)
	}
	if response, err := control.read(t.Context()); err != nil || response.Type != "pong" {
		t.Fatalf("control response=%v err=%v", response, err)
	}
}
