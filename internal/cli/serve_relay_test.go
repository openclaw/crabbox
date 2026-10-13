package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"nhooyr.io/websocket"
)

func TestServeRelayStreamBackpressureAndClose(t *testing.T) {
	received := make(chan []byte, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		socket, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer socket.CloseNow()
		socket.SetReadLimit(serveRelayChunk + 1)
		_ = socket.Write(r.Context(), websocket.MessageBinary, []byte{2})
		for {
			kind, data, err := socket.Read(r.Context())
			if err != nil {
				return
			}
			if kind != websocket.MessageBinary || len(data) == 0 {
				return
			}
			if data[0] == 0 {
				received <- append([]byte(nil), data[1:]...)
				_ = socket.Write(r.Context(), websocket.MessageBinary, data)
				_, ack, err := socket.Read(r.Context())
				if err != nil || !bytes.Equal(ack, []byte{1}) {
					return
				}
				_ = socket.Write(r.Context(), websocket.MessageBinary, []byte{1})
			}
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, err := DialServeRelay(ctx, &CoordinatorClient{BaseURL: server.URL}, strings.Repeat("a", 32), "client", "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	payload := bytes.Repeat([]byte("x"), serveRelayChunk*2+1)
	done := make(chan error, 1)
	go func() { _, err := conn.Write(payload); done <- err }()
	output := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, output); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload, output) {
		t.Fatal("corrupted relay stream")
	}
	for i := 0; i < 3; i++ {
		if frame := <-received; len(frame) > serveRelayChunk {
			t.Fatal("unbounded frame")
		}
	}
	conn.Close()
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("closed stream readable")
	}
}
func TestServeRelayCandidateFallbackAndFreshDirectPreference(t *testing.T) {
	isolateTestUserDirs(t)
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(private)
	// A TEST-NET address is unavailable locally; fallback does not bypass either host-key pin.
	key := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	cfg := Config{Coordinator: "http://127.0.0.1:1"}
	target := SSHTarget{Host: "cbx_001122334455", Port: "22", Key: filepath.Join(t.TempDir(), "key ' %h $(bad)"), SSHHostKey: key, relayConfig: &cfg, SSHJump: &SSHJump{Host: "192.0.2.254", Port: 1, HostKey: key, Relay: true, Candidates: []SSHJumpCandidate{{"192.0.2.254", 1}}}}
	if err := prepareLeaseSSHTrust(&target, target.Host); err != nil {
		t.Fatal(err)
	}
	if !target.SSHJump.useRelay || !strings.Contains(target.ProxyCommand, "__serve-relay-proxy") || !strings.Contains(target.ProxyCommand, "HostKeyAlias=") {
		t.Fatal(target.ProxyCommand)
	}
	if strings.Contains(target.ProxyCommand, serveRelayAuthEnv) {
		t.Fatal("credentials in argv")
	}
	if target.ChildEnv[serveRelayAuthEnv] == "" || cachedSSHRouteDescription(target.Host) != "relay" {
		t.Fatal("missing relay route state")
	}
	// The route cache never treats relay as a direct endpoint; a fresh probe prefers a good direct route.
	cached := &sshJumpRoute{Fingerprint: ssh.FingerprintSHA256(signer.PublicKey()), Kind: "relay"}
	candidate := SSHJumpCandidate{"192.168.1.2", 8443}
	route, err := chooseJumpCandidate(t.Context(), []SSHJumpCandidate{candidate}, signer.PublicKey(), cached, nil, func(context.Context, SSHJumpCandidate, ssh.PublicKey) bool { return true })
	if err != nil || route.Kind != "lan" {
		t.Fatal(route, err)
	}
	var auth serveRelayAuth
	if err := json.Unmarshal([]byte(target.ChildEnv[serveRelayAuthEnv]), &auth); err != nil || auth.BaseURL != cfg.Coordinator {
		t.Fatal(err)
	}
}
func TestServeRelaySelfProxyQuotesExactArgv(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("POSIX shell unavailable")
	}
	executable := filepath.Join(t.TempDir(), "a ' %h $(printf unexpected)")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	command := serveRelayProxyCommand(executable, "cbx_001122334455", strings.Repeat("a", 64))
	if !strings.Contains(command, "%%h") {
		t.Fatal(command)
	}
	output, err := exec.Command("sh", "-c", strings.ReplaceAll(command, "%%", "%")).Output()
	if err != nil {
		t.Fatal(err)
	}
	want := "__serve-relay-proxy\n--lease\ncbx_001122334455\n--coordinator-binding\n" + strings.Repeat("a", 64) + "\n"
	if string(output) != want {
		t.Fatalf("argv = %q", output)
	}
	t.Setenv(serveRelayAuthEnv, "")
	if err := (App{Stderr: io.Discard, Stdout: io.Discard}).serveRelayProxy(t.Context(), []string{"--lease", "cbx_001122334455"}); err == nil {
		t.Fatal("self-proxy accepted missing auth")
	}
}

func TestServeRelayPrintedProxyLoadsBoundLogin(t *testing.T) {
	isolateTestUserDirs(t)
	for _, name := range []string{serveRelayAuthEnv, "CRABBOX_COORDINATOR", "CRABBOX_COORDINATOR_TOKEN", "CRABBOX_COORDINATOR_TOKEN_COMMAND", "CRABBOX_ACCESS_CLIENT_ID", "CRABBOX_ACCESS_CLIENT_SECRET", "CRABBOX_ACCESS_TOKEN", "CF_ACCESS_CLIENT_ID", "CF_ACCESS_CLIENT_SECRET", "CF_ACCESS_TOKEN"} {
		t.Setenv(name, "")
	}
	requests := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-relay-login" {
			w.WriteHeader(403)
			return
		}
		if r.Method == "POST" {
			requests <- r.URL.Path
			_ = json.NewEncoder(w).Encode(map[string]string{"sessionID": strings.Repeat("a", 32)})
			return
		}
		socket, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer socket.CloseNow()
		_ = socket.Write(r.Context(), websocket.MessageBinary, []byte{2})
		_, _, _ = socket.Read(r.Context())
	}))
	defer server.Close()
	config := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(config, []byte("broker:\n  url: "+server.URL+"\n  token: synthetic-relay-login\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", config)
	app := App{Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard}
	args := []string{"--lease", "cbx_001122334455", "--coordinator-binding", strings.Repeat("b", 64)}
	if err := app.serveRelayProxy(t.Context(), args); err == nil {
		t.Fatal("printed proxy accepted a different coordinator")
	}
	select {
	case <-requests:
		t.Fatal("sent credentials to a changed coordinator")
	default:
	}
	args[len(args)-1] = serveRelayCoordinatorBinding(server.URL)
	if err := app.serveRelayProxy(t.Context(), args); err != nil {
		t.Fatal(err)
	}
	if path := <-requests; path != "/v1/serve/leases/cbx_001122334455/relay" {
		t.Fatal(path)
	}
}

func TestServeRelayCoordinatorBindingExcludesCredentials(t *testing.T) {
	if serveRelayCoordinatorBinding("https://alice:password@example.com/api?token=secret") != serveRelayCoordinatorBinding("https://example.com/api") {
		t.Fatal("destination binding includes credentials")
	}
}
