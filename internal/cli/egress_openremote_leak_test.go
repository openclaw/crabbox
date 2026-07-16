package cli

// Repro for candidate concurrency bug: egressBridge.openRemote abandons a
// timed-out open (egress.go line 669, `case <-timer.C:`) without deleting
// b.pending[id] and without notifying the host with a "close" message.
//
// Consequences on the base revision:
//  1. If the host never answers (dial hung), the client's pending map entry
//     for the abandoned id is never removed -> unbounded growth on the
//     long-lived client bridge.
//  2. If the host answers late with open_ok (slow dial > egressOpenTimeout),
//     the host has already registered conns[id] and spawned copyConnToBridge
//     (egress.go lines 447-454). The client silently discards the open_ok and
//     never sends "close" for that id, so the host keeps the destination TCP
//     connection and its copy goroutine alive for the life of the bridge.
//
// This test drives the REAL client production path (connectEgressBridge +
// clientReadLoop + openRemote) against a fake host over a real websocket and
// asserts the two cleanup behaviors a correct implementation must have:
//   (a) after openRemote returns "egress open timed out", pending[id] is gone;
//   (b) the host receives a "close" for the abandoned id (either at timeout
//       or in response to the late open_ok).
// Both assertions FAIL on the base revision (808d96fc7235, pre-fix); they pass on this PR. NOTE: the test takes ~20s because it
// waits out the real egressOpenTimeout to hit the exact timer.C branch.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func TestEgressOpenRemoteTimeoutLeaksPendingAndNeverClosesHostConn(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the real 20s egressOpenTimeout")
	}

	const leasePath = "/v1/leases/cbx_abcdef123456/egress/client"

	hostMsgs := make(chan egressProxyMessage, 64) // messages the fake host receives
	hostWS := make(chan *websocket.Conn, 1)       // host side of the bridge, for late replies
	hostDone := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != leasePath {
			http.NotFound(w, r)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("websocket accept: %v", err)
			return
		}
		conn.SetReadLimit(egressMaxMessageBytes)
		hostWS <- conn
		// Fake host read loop: record every message the client sends us.
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				close(hostDone)
				return
			}
			var msg egressProxyMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				t.Errorf("fake host unmarshal: %v", err)
				continue
			}
			hostMsgs <- msg
		}
	}))
	defer server.Close()

	dialCtx, cancelDial := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelDial()
	coord := &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	bridge, err := connectEgressBridge(
		dialCtx,
		coord,
		"cbx_abcdef123456",
		"client",
		"egress_abcdef1234567890abcdef1234567890",
		"egress_session",
		"",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.close()

	// Run the real client read loop, exactly as serveClient does.
	loopCtx, cancelLoop := context.WithCancel(context.Background())
	defer cancelLoop()
	go func() { _ = bridge.clientReadLoop(loopCtx) }()

	hostConn := <-hostWS

	// Client opens a remote conn; the fake host's "dial" is slow: it does not
	// answer before egressOpenTimeout (20s), so openRemote hits timer.C.
	const id = "conn_"
	openErr := make(chan error, 1)
	go func() {
		openErr <- bridge.openRemote(context.Background(), id, "discord.com", "443")
	}()

	// Fake host receives the open request.
	select {
	case msg := <-hostMsgs:
		if msg.Type != "open" || msg.ID != id {
			t.Fatalf("fake host expected open for %s, got %+v", id, msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fake host never received the open request")
	}

	// Wait out the real egressOpenTimeout: openRemote must fail via timer.C.
	var err669 error
	select {
	case err669 = <-openErr:
	case <-time.After(egressOpenTimeout + 10*time.Second):
		t.Fatal("openRemote did not return after egressOpenTimeout")
	}
	if err669 == nil || err669.Error() != "egress open timed out" {
		t.Fatalf("expected timer.C timeout error, got %v", err669)
	}

	// (a) An abandoned open must not leave client-side state behind: if the
	// host never replies (hung dial), this entry lives forever on the
	// long-running bridge. FAILS on the base revision: openRemote's timer.C branch
	// returns without delete(b.pending, id).
	bridge.mu.Lock()
	_, stillPending := bridge.pending[id]
	pendingLen := len(bridge.pending)
	bridge.mu.Unlock()
	if stillPending {
		t.Errorf("pending[%q] leaked after openRemote timeout (pending size=%d); timed-out open was abandoned without cleanup", id, pendingLen)
	}

	// The host's slow dial now completes: it registers conns[id], sends
	// open_ok, and spawns copyConnToBridge (hostOpen, egress.go:447-454).
	openOK, err := json.Marshal(egressProxyMessage{Type: "open_ok", ID: id})
	if err != nil {
		t.Fatal(err)
	}
	writeCtx, cancelWrite := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelWrite()
	if err := hostConn.Write(writeCtx, websocket.MessageText, openOK); err != nil {
		t.Fatalf("fake host write open_ok: %v", err)
	}

	// (b) The client abandoned this conn, so the host must be told to close
	// it — either proactively at timeout or in reaction to the late open_ok.
	// Otherwise the host keeps the destination TCP conn + copyConnToBridge
	// goroutine until the destination itself hangs up (forever for keepalive
	// destinations). FAILS on the base revision: no "close" is ever sent for id.
	deadline := time.After(3 * time.Second)
hostCloseReceived:
	for {
		select {
		case msg := <-hostMsgs:
			if msg.Type == "close" && msg.ID == id {
				break hostCloseReceived
			}
		case <-deadline:
			t.Fatalf("host never received a \"close\" for abandoned id %q after late open_ok; host-side destination conn and copyConnToBridge goroutine leak for the life of the bridge", id)
		}
	}

	// A failed initial WebSocket write abandons the pending open too. Close the
	// client transport to make that failure deterministic, then ensure the same
	// pending-state cleanup applies before openRemote returns.
	if err := bridge.ws.CloseNow(); err != nil {
		t.Fatal(err)
	}
	const writeFailureID = "conn_write_failure"
	if err := bridge.openRemote(context.Background(), writeFailureID, "example.com", "443"); err == nil {
		t.Fatal("openRemote unexpectedly succeeded on a closed WebSocket")
	}
	bridge.mu.Lock()
	_, writeFailurePending := bridge.pending[writeFailureID]
	bridge.mu.Unlock()
	if writeFailurePending {
		t.Fatalf("pending[%q] leaked after the initial WebSocket write failed", writeFailureID)
	}
}
