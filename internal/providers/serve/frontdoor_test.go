package serve

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/localcontainer"
	"golang.org/x/crypto/ssh"
)

const testLeaseA = "cbx_000000000001"
const testLeaseB = "cbx_000000000002"

func testSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func testFront(t *testing.T) (*frontDoor, *agent) {
	t.Helper()
	f, err := startFrontDoor(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	return f, &agent{frontDoor: f, state: localState{Machine: machine{MachineID: "test"}, Assignments: map[string]assignment{}}}
}
func assignFront(a *agent, id string, key ssh.Signer, address string) localcontainer.ServedContainer {
	host, port, _ := net.SplitHostPort(address)
	a.state.Assignments[id] = assignment{ID: id, MachineID: "test", State: "active", SSHPublicKey: string(ssh.MarshalAuthorizedKey(key.PublicKey())), ExpiresAt: time.Now().Add(time.Hour), IdleDeadline: time.Now().Add(time.Hour).UnixMilli()}
	return localcontainer.ServedContainer{ID: id, LeaseID: id, Host: host, Port: port, Running: true, Labels: map[string]string{"crabbox.serve": "true", "crabbox.serve.machine": "test"}}
}
func dialFront(f *frontDoor, user string, keys ...ssh.Signer) (*ssh.Client, error) {
	return ssh.Dial("tcp", frontDoorTestAddress(f), &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{ssh.PublicKeys(keys...)}, HostKeyCallback: ssh.FixedHostKey(f.signer.PublicKey()), Timeout: 2 * time.Second})
}
func requireClosed(t *testing.T, c *ssh.Client) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("revoked SSH connection stayed open")
	}
}
func TestFrontDoorAuthAndChannelPolicy(t *testing.T) {
	f, a := testFront(t)
	key, other := testSigner(t), testSigner(t)
	ca := assignFront(a, testLeaseA, key, "127.0.0.1:12345")
	cb := assignFront(a, testLeaseB, other, "127.0.0.1:12346")
	a.updateFrontDoor([]localcontainer.ServedContainer{ca, cb})
	for _, tt := range []struct {
		name, user string
		key        ssh.Signer
	}{{"wrong key", "crabbox", testSigner(t)}, {"wrong user", "root", key}} {
		t.Run(tt.name, func(t *testing.T) {
			c, err := dialFront(f, tt.user, tt.key)
			if err == nil {
				c.Close()
				t.Fatal("authentication accepted")
			}
		})
	}
	c, err := dialFront(f, "crabbox", key)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, kind := range []string{"session", "exec", "shell", "subsystem", "x11", "auth-agent@openssh.com", "forwarded-tcpip"} {
		if ch, _, err := c.OpenChannel(kind, nil); err == nil {
			ch.Close()
			t.Fatalf("accepted channel %s", kind)
		}
	}
	for _, kind := range []string{"tcpip-forward", "cancel-tcpip-forward", "unknown"} {
		ok, _, err := c.SendRequest(kind, true, ssh.Marshal(struct {
			Host string
			Port uint32
		}{"127.0.0.1", 10000}))
		if err != nil || ok {
			t.Fatalf("global %s: %t %v", kind, ok, err)
		}
	}
	if ok, _, err := c.SendRequest("keepalive@openssh.com", true, nil); err != nil || !ok {
		t.Fatalf("keepalive: %t %v", ok, err)
	}
	for _, dest := range []string{testLeaseB + ":22", testLeaseA + ":23", "127.0.0.1:22"} {
		if stream, err := c.Dial("tcp", dest); err == nil {
			stream.Close()
			t.Fatalf("cross-lease/port accepted: %s", dest)
		}
	}
	// The key remains in stored assignments, but no longer belongs to a live container.
	a.updateFrontDoor([]localcontainer.ServedContainer{cb})
	requireClosed(t, c)
	if c, err := dialFront(f, "crabbox", key); err == nil {
		c.Close()
		t.Fatal("non-live key accepted")
	}
}

func TestFrontDoorRevokesConnectionsAndIdleStreams(t *testing.T) {
	for _, reason := range []string{"release", "expiry", "reconcile", "foreign", "drain"} {
		t.Run(reason, func(t *testing.T) {
			f, a := testFront(t)
			key := testSigner(t)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			closed := make(chan struct{})
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
				close(closed)
			}()
			item := assignFront(a, testLeaseA, key, listener.Addr().String())
			a.updateFrontDoor([]localcontainer.ServedContainer{item})
			c, err := dialFront(f, "crabbox", key)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			stream, err := c.Dial("tcp", testLeaseA+":22")
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			if _, err := stream.Write([]byte("ping")); err != nil {
				t.Fatal(err)
			}
			b := make([]byte, 4)
			if _, err := io.ReadFull(stream, b); err != nil || string(b) != "ping" {
				t.Fatalf("echo=%q %v", b, err)
			}
			switch reason {
			case "release":
				delete(a.state.Assignments, testLeaseA)
				a.updateFrontDoor([]localcontainer.ServedContainer{item})
			case "expiry":
				l := a.state.Assignments[testLeaseA]
				l.ExpiresAt = time.Now().Add(50 * time.Millisecond)
				a.state.Assignments[testLeaseA] = l
				a.updateFrontDoor([]localcontainer.ServedContainer{item})
			case "reconcile":
				a.updateFrontDoor(nil)
			case "foreign":
				l := a.state.Assignments[testLeaseA]
				l.MachineID = "foreign"
				a.state.Assignments[testLeaseA] = l
				a.updateFrontDoor([]localcontainer.ServedContainer{item})
			case "drain":
				f.Close()
			}
			requireClosed(t, c)
			select {
			case <-closed:
			case <-time.After(2 * time.Second):
				t.Fatal("idle loopback stream stayed open")
			}
			if c, err := dialFront(f, "crabbox", key); err == nil {
				c.Close()
				t.Fatal("revoked key accepted")
			}
		})
	}
}
func TestFrontDoorLimits(t *testing.T) {
	t.Run("handshake timeout", func(t *testing.T) {
		f, _ := testFront(t)
		// Measure before dialing: the server can start its deadline before Dial returns.
		start := time.Now()
		conn, err := net.Dial("tcp", frontDoorTestAddress(f))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(frontDoorHandshakeTimeout + 5*time.Second))
		_, err = io.ReadAll(conn)
		if err != nil || time.Since(start) < frontDoorHandshakeTimeout || time.Since(start) > frontDoorHandshakeTimeout+5*time.Second {
			t.Fatalf("timeout=%s err=%v", time.Since(start), err)
		}
	})
	t.Run("authentication attempts", func(t *testing.T) {
		f, a := testFront(t)
		key := testSigner(t)
		item := assignFront(a, testLeaseA, key, "127.0.0.1:12345")
		a.updateFrontDoor([]localcontainer.ServedContainer{item})
		c, err := dialFront(f, "crabbox", testSigner(t), testSigner(t), testSigner(t), key)
		if err == nil {
			c.Close()
			t.Fatal("fourth authentication attempt accepted")
		}
		c, err = dialFront(f, "crabbox", testSigner(t), testSigner(t), key)
		if err != nil {
			t.Fatalf("third attempt denied: %v", err)
		}
		c.Close()
	})
	t.Run("connections", func(t *testing.T) {
		f, a := testFront(t)
		key := testSigner(t)
		item := assignFront(a, testLeaseA, key, "127.0.0.1:12345")
		a.updateFrontDoor([]localcontainer.ServedContainer{item})
		for range frontDoorConnections {
			c, err := dialFront(f, "crabbox", key)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
		}
		if c, err := dialFront(f, "crabbox", key); err == nil {
			c.Close()
			t.Fatal("connection cap exceeded")
		}
	})
	t.Run("pending handshakes", func(t *testing.T) {
		f, _ := testFront(t)
		for range frontDoorHandshakes {
			c, err := net.Dial("tcp", frontDoorTestAddress(f))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
		}
		until := time.Now().Add(2 * time.Second)
		for len(f.handshakes) != frontDoorHandshakes && time.Now().Before(until) {
			time.Sleep(time.Millisecond)
		}
		if len(f.handshakes) != frontDoorHandshakes {
			t.Fatal("pending handshakes not registered")
		}
		c, err := net.Dial("tcp", frontDoorTestAddress(f))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
		b := make([]byte, 1)
		if _, err := c.Read(b); err != io.EOF {
			t.Fatalf("excess handshake not closed: %v", err)
		}
	})
}
func TestFrontDoorHostKeyPersists(t *testing.T) {
	dir := t.TempDir()
	f, err := startFrontDoor(dir, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	key := f.signer.PublicKey().Marshal()
	address := frontDoorTestAddress(f)
	f.Close()
	f, err = startFrontDoor(dir, address)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if !bytes.Equal(key, f.signer.PublicKey().Marshal()) {
		t.Fatal("host key changed across restart")
	}
	info, err := os.Stat(filepath.Join(dir, "ssh_host_ed25519_key"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("key permissions: %v %v", info, err)
	}
	if _, err := startFrontDoor(dir, address); err == nil || !strings.Contains(err.Error(), "--ssh-port") {
		t.Fatalf("bind failure not actionable: %v", err)
	}
}

func TestFrontDoorEndToEndInnerSSH(t *testing.T) {
	f, a := testFront(t)
	key, host := testSigner(t), testSigner(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	innerDone := make(chan struct{})
	go func() {
		defer close(innerDone)
		raw, err := listener.Accept()
		if err != nil {
			return
		}
		defer raw.Close()
		config := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, p ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(p.Marshal(), key.PublicKey().Marshal()) {
				return nil, nil
			}
			return nil, fmt.Errorf("wrong inner key")
		}}
		config.AddHostKey(host)
		conn, channels, requests, err := ssh.NewServerConn(raw, config)
		if err != nil {
			return
		}
		defer conn.Close()
		go ssh.DiscardRequests(requests)
		for ch := range channels {
			if ch.ChannelType() != "session" {
				_ = ch.Reject(ssh.Prohibited, "session required")
				continue
			}
			stream, requests, err := ch.Accept()
			if err != nil {
				return
			}
			for req := range requests {
				if req.Type == "exec" {
					_ = req.Reply(true, nil)
					_, _ = io.WriteString(stream, "FRONTDOOR_OK\n")
					_, _ = stream.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{0}))
					_ = stream.Close()
					break
				}
				_ = req.Reply(false, nil)
			}
		}
	}()
	item := assignFront(a, testLeaseA, key, listener.Addr().String())
	a.updateFrontDoor([]localcontainer.ServedContainer{item})
	outer, err := dialFront(f, "crabbox", key)
	if err != nil {
		t.Fatal(err)
	}
	defer outer.Close()
	if conn, err := outer.Dial("tcp", testLeaseB+":22"); err == nil {
		conn.Close()
		t.Fatal("cross-lease tunnel allowed")
	}
	tunnel, err := outer.Dial("tcp", testLeaseA+":22")
	if err != nil {
		t.Fatal(err)
	}
	client, chans, reqs, err := ssh.NewClientConn(tunnel, testLeaseA+":22", &ssh.ClientConfig{User: "crabbox", Auth: []ssh.AuthMethod{ssh.PublicKeys(key)}, HostKeyCallback: ssh.FixedHostKey(host.PublicKey())})
	if err != nil {
		t.Fatal(err)
	}
	inner := ssh.NewClient(client, chans, reqs)
	session, err := inner.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	output, err := session.Output("hostname; echo FRONTDOOR_OK")
	if err != nil || string(output) != "FRONTDOOR_OK\n" {
		t.Fatalf("inner output=%q error=%v", output, err)
	}
	_ = session.Close()
	_ = inner.Close()
	select {
	case <-innerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("inner server not closed")
	}
}

func TestServeSSHPortValidation(t *testing.T) {
	for _, port := range []int{0, -1, 65536} {
		err := (Provider{}).RunAgent(context.Background(), core.Config{}, core.Runtime{Stderr: io.Discard}, []string{"--ssh-port", strconv.Itoa(port), "--state-dir", t.TempDir()})
		if err == nil || !strings.Contains(err.Error(), "--ssh-port") {
			t.Fatalf("port=%d error=%v", port, err)
		}
	}
}

func TestFrontDoorStreamLimitAndDuplicateKeys(t *testing.T) {
	f, a := testFront(t)
	key := testSigner(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); _, _ = io.Copy(conn, conn) }()
		}
	}()
	item := assignFront(a, testLeaseA, key, listener.Addr().String())
	a.updateFrontDoor([]localcontainer.ServedContainer{item})
	c, err := dialFront(f, "crabbox", key)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for range frontDoorStreams {
		s, err := c.Dial("tcp", testLeaseA+":22")
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
	}
	if s, err := c.Dial("tcp", testLeaseA+":22"); err == nil {
		s.Close()
		t.Fatal("stream cap exceeded")
	}
	duplicate := assignFront(a, testLeaseB, key, listener.Addr().String())
	a.updateFrontDoor([]localcontainer.ServedContainer{item, duplicate})
	if c, err := dialFront(f, "crabbox", key); err == nil {
		c.Close()
		t.Fatal("ambiguous key selected a lease")
	}
}

func startFrontDoor(dir, address string) (*frontDoor, error) {
	signer, err := frontDoorHostKey(dir)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp4", address)
	if err != nil {
		return nil, fmt.Errorf("bind serve SSH front door: %w; select --ssh-port", err)
	}
	f := &frontDoor{listeners: map[string]net.Listener{address: listener}, signer: signer, leases: map[string]*frontLease{}, sockets: map[net.Conn]struct{}{}, handshakes: make(chan struct{}, frontDoorHandshakes)}
	f.wg.Add(1)
	go f.accept(listener)
	return f, nil
}
func frontDoorTestAddress(f *frontDoor) string {
	for _, listener := range f.listeners {
		return listener.Addr().String()
	}
	return ""
}
