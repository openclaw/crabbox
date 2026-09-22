package testutil

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

var pipeHTTPPort atomic.Uint32

// NewPipeHTTPServer keeps HTTP framing and transport cancellation real while
// making network waits visible to testing/synctest. Use its client's transport
// for every request; the server does not listen on a loopback socket.
func NewPipeHTTPServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	// SDK token caches often key on origin. Preserve httptest's distinct-server
	// identity even though these logical ports are never bound by the OS.
	port := 10000 + int(pipeHTTPPort.Add(1))
	if port > 65535 {
		t.Fatal("exhausted in-memory HTTP test origins")
	}
	listener := &pipeHTTPListener{connections: make(chan net.Conn), done: make(chan struct{}), port: port}
	server := &httptest.Server{Listener: listener, Config: &http.Server{Handler: handler}}
	server.Start()
	server.Client().Transport.(*http.Transport).DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		clientConn, serverConn := net.Pipe()
		select {
		case listener.connections <- serverConn:
			return clientConn, nil
		case <-ctx.Done():
			clientConn.Close()
			serverConn.Close()
			return nil, ctx.Err()
		case <-listener.done:
			clientConn.Close()
			serverConn.Close()
			return nil, net.ErrClosed
		}
	}
	t.Cleanup(server.Close)
	return server
}

type pipeHTTPListener struct {
	connections chan net.Conn
	done        chan struct{}
	closeOnce   sync.Once
	port        int
}

func (l *pipeHTTPListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.connections:
		return conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *pipeHTTPListener) Close() error {
	l.closeOnce.Do(func() { close(l.done) })
	return nil
}

func (l *pipeHTTPListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: l.port}
}
