package serve

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/localcontainer"
	"golang.org/x/crypto/ssh"
	"nhooyr.io/websocket"
)

func pairedRelay(t *testing.T, a *agent, leaseID string) net.Conn {
	t.Helper()
	return pairedRelays(t, a, leaseID, 1)[0]
}

func pairedRelays(t *testing.T, a *agent, leaseID string, count int) []net.Conn {
	t.Helper()
	type session struct {
		sockets  map[string]*websocket.Conn
		ready    chan struct{}
		inFlight map[string]bool
	}
	var mu sync.Mutex
	sessions := map[string]*session{}
	pending := make([]relayAssignment, count)
	for i := range pending {
		id := fmt.Sprintf("%032x", i+1)
		sessions[id] = &session{sockets: map[string]*websocket.Conn{}, ready: make(chan struct{}), inFlight: map[string]bool{}}
		pending[i] = relayAssignment{SessionID: id, LeaseID: leaseID, AgentToken: strings.Repeat("b", 32)}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		state := sessions[parts[len(parts)-2]]
		if state == nil {
			w.WriteHeader(404)
			return
		}
		role := "client"
		if strings.HasSuffix(r.URL.Path, "/agent") {
			role = "agent"
			if r.Header.Get("X-Crabbox-Relay-Token") != strings.Repeat("b", 32) || r.Header.Get("X-Crabbox-Serve-Machine") != "test" {
				w.WriteHeader(403)
				return
			}
		}
		socket, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer socket.CloseNow()
		socket.SetReadLimit(32*1024 + 1)
		mu.Lock()
		state.sockets[role] = socket
		if len(state.sockets) == 2 {
			close(state.ready)
		}
		mu.Unlock()
		select {
		case <-state.ready:
		case <-r.Context().Done():
			return
		}
		mu.Lock()
		peerRole := "agent"
		if role == "agent" {
			peerRole = "client"
		}
		peer := state.sockets[peerRole]
		mu.Unlock()
		defer peer.CloseNow()
		if socket.Write(r.Context(), websocket.MessageBinary, []byte{2}) != nil {
			return
		}
		for {
			kind, data, err := socket.Read(r.Context())
			if err != nil {
				return
			}
			mu.Lock()
			valid := kind == websocket.MessageBinary && len(data) > 0
			if valid && data[0] == 0 && len(data) > 1 && !state.inFlight[role] {
				state.inFlight[role] = true
			} else if valid && data[0] == 1 && len(data) == 1 && state.inFlight[peerRole] {
				state.inFlight[peerRole] = false
			} else {
				valid = false
			}
			mu.Unlock()
			if !valid {
				t.Errorf("invalid relay frame/ACK window in %s", role)
				return
			}
			if peer.Write(r.Context(), kind, data) != nil {
				return
			}
		}
	}))
	t.Cleanup(func() {
		mu.Lock()
		for _, state := range sessions {
			for _, socket := range state.sockets {
				_ = socket.CloseNow()
			}
		}
		mu.Unlock()
		server.Close()
	})
	coord, err := core.NewCoordinatorClient(core.Config{Coordinator: server.URL, CoordToken: "synthetic-relay-test"})
	if err != nil {
		t.Fatal(err)
	}
	a.coord = coord
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	a.startRelays(ctx, pending)
	connections := make([]net.Conn, count)
	for i, assignment := range pending {
		conn, err := core.DialServeRelay(ctx, coord, assignment.SessionID, "client", "", "")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		connections[i] = conn
	}
	return connections
}
func TestServeRelayFrontDoorAuthChannelsAndRevocation(t *testing.T) {
	f, a := testFront(t)
	a.rt.Stderr = io.Discard
	leaseKey := testSigner(t)
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		conn, err := echo.Accept()
		if err == nil {
			defer conn.Close()
			_, _ = io.Copy(conn, conn)
		}
	}()
	item := assignFront(a, testLeaseA, leaseKey, echo.Addr().String())
	a.updateFrontDoor([]localcontainer.ServedContainer{item})
	raw := pairedRelay(t, a, testLeaseA)
	conn, channels, requests, err := ssh.NewClientConn(raw, "relay", &ssh.ClientConfig{User: "crabbox", Auth: []ssh.AuthMethod{ssh.PublicKeys(leaseKey)}, HostKeyCallback: ssh.FixedHostKey(f.signer.PublicKey())})
	if err != nil {
		t.Fatal(err)
	}
	client := ssh.NewClient(conn, channels, requests)
	defer client.Close()
	if session, err := client.NewSession(); err == nil {
		session.Close()
		t.Fatal("relay allowed session channel")
	}
	if stream, err := client.Dial("tcp", testLeaseB+":22"); err == nil {
		stream.Close()
		t.Fatal("relay crossed lease boundary")
	}
	stream, err := client.Dial("tcp", testLeaseA+":22")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if _, err := stream.Write([]byte("RELAY_OK")); err != nil {
		t.Fatal(err)
	}
	result := make([]byte, 8)
	if _, err := io.ReadFull(stream, result); err != nil || string(result) != "RELAY_OK" {
		t.Fatal(string(result), err)
	}
	a.state.Assignments = map[string]assignment{}
	a.updateFrontDoor(nil)
	requireClosed(t, client)
}
func TestServeRelayCannotAuthenticateAnotherLease(t *testing.T) {
	f, a := testFront(t)
	a.rt.Stderr = io.Discard
	keyA, keyB := testSigner(t), testSigner(t)
	itemA := assignFront(a, testLeaseA, keyA, "127.0.0.1:1234")
	itemB := assignFront(a, testLeaseB, keyB, "127.0.0.1:1235")
	a.updateFrontDoor([]localcontainer.ServedContainer{itemA, itemB})
	raw := pairedRelay(t, a, testLeaseA)
	defer raw.Close()
	if conn, _, _, err := ssh.NewClientConn(raw, "relay", &ssh.ClientConfig{User: "crabbox", Auth: []ssh.AuthMethod{ssh.PublicKeys(keyB)}, HostKeyCallback: ssh.FixedHostKey(f.signer.PublicKey())}); err == nil {
		conn.Close()
		t.Fatal("relay minted for A accepted B's key")
	}
}

func TestServeRelayConcurrentSessionsLargeTransfer(t *testing.T) {
	f, a := testFront(t)
	a.rt.Stderr = io.Discard
	key := testSigner(t)
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			conn, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); _, _ = io.Copy(conn, conn) }()
		}
	}()
	item := assignFront(a, testLeaseA, key, echo.Addr().String())
	a.updateFrontDoor([]localcontainer.ServedContainer{item})
	raws := pairedRelays(t, a, testLeaseA, 3)
	// A readiness-style connection closing immediately must not revoke peers.
	_ = raws[0].Close()
	clients := make([]*ssh.Client, 2)
	streams := make([]net.Conn, 2)
	for i, raw := range raws[1:] {
		conn, channels, requests, err := ssh.NewClientConn(raw, "relay", &ssh.ClientConfig{User: "crabbox", Auth: []ssh.AuthMethod{ssh.PublicKeys(key)}, HostKeyCallback: ssh.FixedHostKey(f.signer.PublicKey())})
		if err != nil {
			t.Fatal(err)
		}
		clients[i] = ssh.NewClient(conn, channels, requests)
		defer clients[i].Close()
		streams[i], err = clients[i].Dial("tcp", testLeaseA+":22")
		if err != nil {
			t.Fatal(err)
		}
		defer streams[i].Close()
	}
	var wg sync.WaitGroup
	for i, stream := range streams {
		wg.Go(func() {
			payload := bytes.Repeat([]byte{byte(i), 0, 255, 42}, 160*1024)
			written := make(chan error, 1)
			go func() { _, err := stream.Write(payload); written <- err }()
			output := make([]byte, len(payload))
			if _, err := io.ReadFull(stream, output); err != nil {
				t.Error(err)
				_ = stream.Close()
			}
			if err := <-written; err != nil {
				t.Error(err)
			}
			if !bytes.Equal(payload, output) {
				t.Error("large relay transfer corrupted or crossed sessions")
			}
		})
	}
	wg.Wait()
	_ = clients[0].Close()
	if _, err := streams[1].Write([]byte("still-live")); err != nil {
		t.Fatal(err)
	}
	output := make([]byte, len("still-live"))
	if _, err := io.ReadFull(streams[1], output); err != nil || string(output) != "still-live" {
		t.Fatal("peer close ended live session", err)
	}
}
