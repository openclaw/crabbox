//go:build darwin || linux

package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"
	"nhooyr.io/websocket"
)

func TestServeRelayControlReusesConnectionsAndCleansUp(t *testing.T) {
	for _, onRelease := range []bool{true, false} {
		t.Run(strconv.FormatBool(onRelease), func(t *testing.T) {
			testServeRelayControlReusesConnectionsAndCleansUp(t, onRelease)
		})
	}
}

func testServeRelayControlReusesConnectionsAndCleansUp(t *testing.T, onRelease bool) {
	isolateTestUserDirs(t)
	inner := newForwardSSHServer(t, "fixture")
	close(inner.release)
	inner.enableReadinessSessions(t, t.TempDir(), true)
	const leaseID = "cbx_001122334455"
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(signer)
	var admissions, frontConnections atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			admissions.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]string{"sessionID": strings.Repeat("a", 32)})
			return
		}
		socket, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer socket.CloseNow()
		_ = socket.Write(r.Context(), websocket.MessageBinary, []byte{2})
		stream := newServeRelayStream(r.Context(), socket, nil)
		defer stream.Close()
		conn, channels, requests, err := ssh.NewServerConn(stream, config)
		if err != nil {
			return
		}
		defer conn.Close()
		frontConnections.Add(1)
		go ssh.DiscardRequests(requests)
		for channel := range channels {
			var dest struct {
				Host       string
				Port       uint32
				Origin     string
				OriginPort uint32
			}
			if channel.ChannelType() != "direct-tcpip" || ssh.Unmarshal(channel.ExtraData(), &dest) != nil || dest.Host != leaseID || dest.Port != 22 {
				_ = channel.Reject(ssh.Prohibited, "wrong lease destination")
				continue
			}
			upstream, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(inner.port())))
			if err != nil {
				_ = channel.Reject(ssh.ConnectionFailed, "fixture unavailable")
				continue
			}
			ch, requests, err := channel.Accept()
			if err != nil {
				upstream.Close()
				continue
			}
			go ssh.DiscardRequests(requests)
			go func() {
				defer upstream.Close()
				defer ch.Close()
				done := make(chan struct{})
				go func() { _, _ = io.Copy(upstream, ch); _ = upstream.(*net.TCPConn).CloseWrite(); close(done) }()
				_, _ = io.Copy(ch, upstream)
				_ = ch.Close()
				_ = upstream.Close()
				<-done
			}()
		}
	}))
	defer server.Close()
	keyPath, _, err := EnsureTestboxKey(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	target := SSHTarget{
		Host: leaseID, Port: "22", User: "fixture", Key: keyPath,
		SSHHostKey: inner.hostKey, TargetOS: targetLinux, FallbackPorts: []string{},
		relayCoordinator: &CoordinatorClient{BaseURL: server.URL},
		ChildEnv:         map[string]string{"CRABBOX_TEST_SERVE_RELAY_PROXY": "1"},
		SSHJump: &SSHJump{Host: "127.0.0.1", Port: 2222, useRelay: true,
			HostKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))},
	}
	if err := prepareLeaseSSHTrust(&target, leaseID); err != nil {
		t.Fatal(err)
	}
	controls := &serveRelayControls{}
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), serveRelayControlsKey{}, controls))
	defer cancel()
	t.Cleanup(func() { _ = controls.closeLease("") })
	prepared, err := prepareServeRelayControl(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		// Fresh target copies from the same lease must find the existing master.
		if err := runSSHQuiet(ctx, target, "exit 0"); err != nil {
			t.Fatal(err)
		}
	}
	copySession, err := newSSHTransportSession(ctx, target, false)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(copySession.configPath)
	if err != nil || !strings.Contains(string(data), prepared.ownerControlPath) {
		t.Fatalf("copy transport did not reuse the lease master: %v", err)
	}
	_ = copySession.Close()
	owner, closeOwner, err := startWorkspaceOwnerControl(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeOwner() })
	if owner.ownerControlPath == "" || owner.ownerControlPath == prepared.ownerControlPath {
		t.Fatal("owner must have a separate inner connection")
	}
	if err := runSSHQuiet(ctx, owner, "exit 0"); err != nil {
		t.Fatal(err)
	}
	if admissions.Load() != 1 || frontConnections.Load() != 1 {
		t.Fatalf("admissions=%d front-door connections=%d, want one", admissions.Load(), frontConnections.Load())
	}
	inner.mu.Lock()
	handshakes := len(inner.users)
	inner.mu.Unlock()
	if handshakes != 2 {
		t.Fatalf("inner handshakes=%d, want workload + owner", handshakes)
	}
	var dirs []string
	outerPath := regexp.MustCompile(`/tmp/cbx-owner-[^/' ]+/control`).FindString(prepared.ProxyCommand)
	if outerPath == "" {
		t.Fatal("missing private front-door control socket")
	}
	for _, path := range []string{prepared.ownerControlPath, owner.ownerControlPath, outerPath} {
		dir := filepath.Dir(path)
		dirs = append(dirs, dir)
		if err := inspectSSHControlPath(dir, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := closeOwner(); err != nil {
		t.Fatal(err)
	}
	cancel()
	closing := ""
	if onRelease {
		closing = leaseID
	}
	if err := controls.closeLease(closing); err != nil {
		t.Fatal(err)
	}
	for _, dir := range dirs {
		if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("control directory survived cleanup: %v", err)
		}
	}
	if _, err := prepareServeRelayControl(context.WithoutCancel(ctx), target); err == nil {
		t.Fatal("released lease connection was recreated")
	}
}

func TestServeRelayControlIdentityIsolation(t *testing.T) {
	base := SSHTarget{Host: "cbx_001122334455", Key: "/lease/key", SSHHostKey: "container-key",
		SSHJump: &SSHJump{HostKey: "front-door-key"}, ChildEnv: map[string]string{serveRelayAuthEnv: "owner-a"}}
	for _, mutate := range []func(*SSHTarget){
		func(v *SSHTarget) { v.Host = "cbx_112233445566" },
		func(v *SSHTarget) { v.Key = "/other/key" },
		func(v *SSHTarget) { v.SSHHostKey = "other-container-key" },
		func(v *SSHTarget) { v.SSHJump = &SSHJump{HostKey: "other-front-door-key"} },
		func(v *SSHTarget) { v.ChildEnv = map[string]string{serveRelayAuthEnv: "owner-b"} },
		func(v *SSHTarget) { v.ProxyCommand = "other-coordinator" },
	} {
		other := base
		mutate(&other)
		if serveRelayControlKey(base) == serveRelayControlKey(other) {
			t.Fatal("changed lease, owner, coordinator or pinned identity shared a control connection")
		}
	}
}

func TestServeRelayControlLeavesDirectRoutesUnchanged(t *testing.T) {
	ctx := context.WithValue(t.Context(), serveRelayControlsKey{}, &serveRelayControls{})
	for _, route := range []struct{ name, host string }{
		{"lan", "192.168.1.2"}, {"tailnet", "100.64.1.2"}, {"front-door", "192.0.2.1"},
	} {
		t.Run(route.name, func(t *testing.T) {
			target := SSHTarget{Host: "cbx_001122334455", ProxyCommand: "pinned-front-door", NoControlMaster: true,
				SSHJump: &SSHJump{Host: route.host, Port: 2222}}
			got, err := prepareServeRelayControl(ctx, target)
			if err != nil || got.ownerControlPath != "" || got.ProxyCommand != target.ProxyCommand || !got.NoControlMaster {
				t.Fatalf("direct route changed: %v", err)
			}
		})
	}
}
