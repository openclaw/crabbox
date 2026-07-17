package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

// TestCodeBridgeCloseDropsLateUpstreamWebSocket forces an upstream dial to
// complete after bridge shutdown. The late connection must close and must not
// be registered on the dead bridge.
func TestCodeBridgeCloseDropsLateUpstreamWebSocket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dialArrived := make(chan struct{})      // code-server saw the upstream dial
	releaseUpstream := make(chan struct{})  // let the upstream dial complete
	upstreamAccepted := make(chan struct{}) // upstream websocket established
	upstreamClosed := make(chan struct{})   // upstream websocket was closed by bridge
	testDone := make(chan struct{})

	var dialArrivedOnce sync.Once

	// Fake local code-server: hold the ws_open dial in flight until released,
	// then accept and keep the websocket open (as the real code-server would).
	codeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dialArrivedOnce.Do(func() { close(dialArrived) })
		select {
		case <-releaseUpstream:
		case <-testDone:
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer close(upstreamClosed)
		close(upstreamAccepted)
		// Keep the conn open like a real code-server session; exit with test.
		readCtx, readCancel := context.WithCancel(r.Context())
		defer readCancel()
		go func() {
			<-testDone
			readCancel()
		}()
		for {
			if _, _, err := conn.Read(readCtx); err != nil {
				return
			}
		}
	}))

	// Fake coordinator: send one ws_open, wait until the bridge's upstream
	// dial is in flight, then drop the connection so Serve returns and Close runs.
	coord := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		msg := `{"type":"ws_open","id":"leak-1","path":"/ws"}`
		if err := conn.Write(r.Context(), websocket.MessageText, []byte(msg)); err != nil {
			return
		}
		select {
		case <-dialArrived:
		case <-testDone:
			return
		}
		conn.CloseNow() // coordinator websocket drops mid-session
	}))
	t.Cleanup(func() {
		close(testDone)
		coord.Close()
		codeServer.Close()
	})

	ws, _, err := websocket.Dial(ctx, "ws"+coord.URL[len("http"):], nil)
	if err != nil {
		t.Fatalf("dial fake coordinator: %v", err)
	}

	// Mirror the production bridge construction without coordinator auth.
	b := &codeBridge{
		ws:             ws,
		baseURL:        codeServer.URL,
		client:         &http.Client{Timeout: 30 * time.Second},
		upstream:       map[string]*websocket.Conn{},
		pending:        map[string][]codeProxyMessage{},
		incomingFrames: map[string]codePendingWebSocketFrame{},
	}

	serveDone := make(chan error, 1)
	go func() { serveDone <- b.Serve(ctx) }() // deferred b.Close runs before this returns

	select {
	case <-serveDone:
		// Serve returned => Close (deferred in Serve) has fully completed.
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after coordinator drop")
	}

	b.mu.Lock()
	n := len(b.upstream)
	b.mu.Unlock()
	if n != 0 {
		t.Fatalf("sanity: upstream map not empty immediately after Close: %d", n)
	}

	// Bridge is closed. Now let the in-flight ws_open dial complete, exactly
	// as when the local code-server answers slowly during a bridge drop.
	close(releaseUpstream)

	select {
	case <-upstreamAccepted:
	case <-time.After(10 * time.Second):
		t.Fatal("upstream dial never completed after release")
	}

	// Invariant: the completed in-flight dial must be closed instead of being
	// registered on the already-closed bridge.
	select {
	case <-upstreamClosed:
	case <-time.After(5 * time.Second):
		t.Fatal("closed codeBridge left the completed upstream websocket open")
	}
	b.mu.Lock()
	n = len(b.upstream)
	b.mu.Unlock()
	if n != 0 {
		t.Fatalf("closed codeBridge re-registered an upstream websocket: len(b.upstream)=%d after Close", n)
	}
}
