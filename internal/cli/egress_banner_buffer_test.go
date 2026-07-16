package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func newTestEgressClientBridge() *egressBridge {
	return &egressBridge{
		conns:       map[string]net.Conn{},
		clientConns: map[string]*egressClientConn{},
		pending:     map[string]chan egressOpenResult{},
	}
}

func egressDataFrame(id string, data []byte) egressProxyMessage {
	return egressProxyMessage{Type: "data", ID: id, Body: base64.StdEncoding.EncodeToString(data)}
}

func readEgressBytes(t *testing.T, conn net.Conn, size int) []byte {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(conn, data); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestEgressServerFirstBannerFollowsProxyHandshake(t *testing.T) {
	const id = "conn_banner"
	const response = "HTTP/1.1 200 Connection Established\r\nProxy-Agent: crabbox\r\n\r\n"
	const banner = "SSH-2.0-OpenSSH_9.6\r\n"
	b := newTestEgressClientBridge()
	client, proxy := net.Pipe()
	defer client.Close()
	b.registerClientConn(id, proxy)

	b.enqueueClientConn(egressDataFrame(id, []byte(banner)))
	if err := client.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("server-first banner arrived before proxy handshake")
	}

	handshakeDone := make(chan error, 1)
	go func() {
		_, err := io.WriteString(proxy, response)
		if err == nil {
			b.markClientConnReady(id)
		}
		handshakeDone <- err
	}()
	if got := string(readEgressBytes(t, client, len(response)+len(banner))); got != response+banner {
		t.Fatalf("proxy bytes = %q, want handshake then banner", got)
	}
	if err := <-handshakeDone; err != nil {
		t.Fatal(err)
	}
	b.closeConn(id)
}

func TestEgressSlowClientStreamDoesNotBlockSharedReadLoop(t *testing.T) {
	serverConn := make(chan *websocket.Conn, 1)
	serverDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		serverConn <- conn
		<-serverDone
	}))
	defer func() {
		close(serverDone)
		server.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := newTestEgressClientBridge()
	b.ws = ws
	defer b.close()

	slowClient, slowProxy := net.Pipe()
	defer slowClient.Close()
	b.registerClientConn("slow", slowProxy)
	b.markClientConnReady("slow")
	fastClient, fastProxy := net.Pipe()
	defer fastClient.Close()
	b.registerClientConn("fast", fastProxy)
	b.markClientConnReady("fast")

	loopDone := make(chan error, 1)
	go func() { loopDone <- b.clientReadLoop(ctx) }()
	host := <-serverConn
	writeFrame := func(msg egressProxyMessage) {
		data, marshalErr := json.Marshal(msg)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if writeErr := host.Write(ctx, websocket.MessageText, data); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	writeFrame(egressDataFrame("slow", []byte("blocked")))
	writeFrame(egressDataFrame("fast", []byte("delivered")))
	if got := string(readEgressBytes(t, fastClient, len("delivered"))); got != "delivered" {
		t.Fatalf("fast stream = %q", got)
	}

	_ = host.CloseNow()
	b.close()
	select {
	case <-loopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("client read loop did not stop")
	}
}

func TestEgressClientQueuePreservesFrameOrder(t *testing.T) {
	const id = "conn_order"
	b := newTestEgressClientBridge()
	client, proxy := net.Pipe()
	defer client.Close()
	b.registerClientConn(id, proxy)
	for _, part := range []string{"first", "-second", "-third"} {
		b.enqueueClientConn(egressDataFrame(id, []byte(part)))
	}
	b.markClientConnReady(id)
	if got := string(readEgressBytes(t, client, len("first-second-third"))); got != "first-second-third" {
		t.Fatalf("ordered stream = %q", got)
	}
	b.closeConn(id)
}

func TestEgressClientCloseDrainsEarlierFrames(t *testing.T) {
	const id = "conn_terminal_order"
	b := newTestEgressClientBridge()
	client, proxy := net.Pipe()
	defer client.Close()
	b.registerClientConn(id, proxy)
	b.markClientConnReady(id)
	b.enqueueClientConn(egressDataFrame(id, []byte("final-")))
	b.enqueueClientConn(egressDataFrame(id, []byte("bytes")))
	if !b.closeClientConnAfterDrain(id) {
		t.Fatal("client stream was not registered")
	}
	if got := string(readEgressBytes(t, client, len("final-bytes"))); got != "final-bytes" {
		t.Fatalf("terminal stream = %q", got)
	}
	if _, err := client.Read(make([]byte, 1)); !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("terminal read error = %v, want closed stream", err)
	}
}

func TestEgressClientQueueOverflowClosesOnlyThatStream(t *testing.T) {
	b := newTestEgressClientBridge()
	overflowClient, overflowProxy := net.Pipe()
	defer overflowClient.Close()
	b.registerClientConn("overflow", overflowProxy)
	otherClient, otherProxy := net.Pipe()
	defer otherClient.Close()
	b.registerClientConn("other", otherProxy)

	chunk := make([]byte, egressCopyChunkBytes)
	for range egressClientQueueBytes/len(chunk) + 1 {
		b.enqueueClientConn(egressDataFrame("overflow", chunk))
	}
	b.mu.Lock()
	_, overflowExists := b.clientConns["overflow"]
	_, otherExists := b.clientConns["other"]
	b.mu.Unlock()
	if overflowExists || !otherExists {
		t.Fatalf("queue states overflow=%t other=%t", overflowExists, otherExists)
	}
	if _, err := overflowClient.Read(make([]byte, 1)); !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("overflow stream read error = %v, want closed stream", err)
	}
	b.closeConn("other")
}

func TestEgressRejectedOpenPreservesProxyResponseSocket(t *testing.T) {
	const id = "conn_rejected"
	b := newTestEgressClientBridge()
	client, proxy := net.Pipe()
	defer client.Close()
	defer proxy.Close()
	b.registerClientConn(id, proxy)
	result := make(chan egressOpenResult, 1)
	b.mu.Lock()
	b.pending[id] = result
	b.mu.Unlock()

	b.rejectOpen(id, errors.New("host not allowed"))
	if got := <-result; got.err == nil || got.err.Error() != "host not allowed" {
		t.Fatalf("open result = %v", got.err)
	}
	const response = "HTTP/1.1 502 Bad Gateway\r\n\r\n"
	writeDone := make(chan error, 1)
	go func() {
		_, err := io.WriteString(proxy, response)
		writeDone <- err
	}()
	if got := string(readEgressBytes(t, client, len(response))); got != response {
		t.Fatalf("proxy response = %q", got)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
}

func TestEgressRejectedOpenReturnsBadGatewayThroughReadLoop(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		defer conn.CloseNow()
		_, data, err := conn.Read(context.Background())
		if err != nil {
			t.Errorf("read open: %v", err)
			return
		}
		var open egressProxyMessage
		if err := json.Unmarshal(data, &open); err != nil {
			t.Errorf("decode open: %v", err)
			return
		}
		if open.Type != "open" {
			t.Errorf("message type = %q, want open", open.Type)
			return
		}
		encoded, err := json.Marshal(egressProxyMessage{Type: "error", ID: open.ID, Error: "host not allowed"})
		if err != nil {
			t.Errorf("encode error: %v", err)
			return
		}
		if err := conn.Write(context.Background(), websocket.MessageText, encoded); err != nil {
			t.Errorf("write error: %v", err)
			return
		}
		_, _, _ = conn.Read(context.Background())
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := newTestEgressClientBridge()
	b.ws = ws
	loopDone := make(chan error, 1)
	go func() { loopDone <- b.clientReadLoop(ctx) }()

	client, proxy := net.Pipe()
	defer client.Close()
	proxyDone := make(chan struct{})
	go func() {
		b.handleProxyConn(ctx, proxy)
		close(proxyDone)
	}()
	const request = "CONNECT denied.example:443 HTTP/1.1\r\nHost: denied.example:443\r\n\r\n"
	writeDone := make(chan error, 1)
	go func() {
		_, err := io.WriteString(client, request)
		writeDone <- err
	}()
	const response = "HTTP/1.1 502 Bad Gateway\r\n\r\n"
	if got := string(readEgressBytes(t, client, len(response))); got != response {
		t.Fatalf("proxy response = %q", got)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-proxyDone:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	b.close()
	select {
	case <-loopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("client read loop did not stop")
	}
}

func TestEgressHandshakeWriteFailureClosesHostConnection(t *testing.T) {
	hostMessages := make(chan egressProxyMessage, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		defer conn.CloseNow()
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			var msg egressProxyMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				t.Errorf("decode message: %v", err)
				return
			}
			hostMessages <- msg
			if msg.Type == "open" {
				encoded, err := json.Marshal(egressProxyMessage{Type: "open_ok", ID: msg.ID})
				if err != nil {
					t.Errorf("encode open_ok: %v", err)
					return
				}
				if err := conn.Write(context.Background(), websocket.MessageText, encoded); err != nil {
					t.Errorf("write open_ok: %v", err)
					return
				}
			}
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := newTestEgressClientBridge()
	b.ws = ws
	loopDone := make(chan error, 1)
	go func() { loopDone <- b.clientReadLoop(ctx) }()

	client, proxy := net.Pipe()
	proxyDone := make(chan struct{})
	go func() {
		b.handleProxyConn(ctx, proxy)
		close(proxyDone)
	}()
	requestDone := make(chan error, 1)
	go func() {
		_, err := io.WriteString(client, "CONNECT banner.example:22 HTTP/1.1\r\nHost: banner.example:22\r\n\r\n")
		_ = client.Close()
		requestDone <- err
	}()
	if err := <-requestDone; err != nil {
		t.Fatal(err)
	}

	open := <-hostMessages
	if open.Type != "open" {
		t.Fatalf("first host message = %q, want open", open.Type)
	}
	select {
	case msg := <-hostMessages:
		if msg.Type != "close" || msg.ID != open.ID {
			t.Fatalf("cleanup message = %+v, want close for %s", msg, open.ID)
		}
	case <-ctx.Done():
		t.Fatal("host connection was not closed after handshake write failure")
	}
	select {
	case <-proxyDone:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	_ = b.ws.CloseNow()
	b.close()
	select {
	case <-loopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("client read loop did not stop")
	}
}
