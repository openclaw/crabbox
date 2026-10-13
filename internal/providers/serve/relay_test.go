package serve

import (
	"context"
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
	var mu sync.Mutex
	sockets := map[string]*websocket.Conn{}
	ready := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		socket.SetReadLimit(64 * 1024)
		mu.Lock()
		sockets[role] = socket
		if len(sockets) == 2 {
			once.Do(func() { close(ready) })
		}
		mu.Unlock()
		select {
		case <-ready:
		case <-r.Context().Done():
			return
		}
		mu.Lock()
		peer := sockets["agent"]
		if role == "agent" {
			peer = sockets["client"]
		}
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
			if peer.Write(r.Context(), kind, data) != nil {
				return
			}
		}
	}))
	t.Cleanup(func() {
		mu.Lock()
		for _, socket := range sockets {
			_ = socket.CloseNow()
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
	a.startRelays(ctx, []relayAssignment{{SessionID: strings.Repeat("a", 32), LeaseID: leaseID, AgentToken: strings.Repeat("b", 32)}})
	conn, err := core.DialServeRelay(ctx, coord, strings.Repeat("a", 32), "client", "", "")
	if err != nil {
		t.Fatal(err)
	}
	return conn
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
