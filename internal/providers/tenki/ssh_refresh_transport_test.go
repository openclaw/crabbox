//go:build darwin || linux

package tenki

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	core "github.com/openclaw/crabbox/internal/cli"
	tenkiproto "github.com/openclaw/crabbox/internal/providers/tenki/proto"
	"golang.org/x/crypto/ssh"
)

func TestTenkiSSHRefreshKeepsRunningCommandAlive(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("OpenSSH unavailable")
	}
	material := authorityTestSetup(t)
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	client, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(material.output.IdentityFile, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	clock := &credentialTestClock{}
	clock.unix.Store(time.Now().Unix())
	ca, host := authorityTestSigner(t), authorityTestSigner(t)
	if err := os.WriteFile(material.output.CertificateFile, credentialTestCert(t, ca, client.PublicKey(), material.output.SessionID, clock.Now()), 0o600); err != nil {
		t.Fatal(err)
	}
	var issued atomic.Int32
	authority := authorityTestServer(t, func(_ context.Context, req *connect.Request[tenkiproto.IssueSandboxSSHCertRequest]) (*connect.Response[tenkiproto.IssueSandboxSSHCertResponse], error) {
		issued.Add(1)
		return connect.NewResponse(&tenkiproto.IssueSandboxSSHCertResponse{SshCert: string(credentialTestCert(t, ca, client.PublicKey(), req.Msg.SessionId, clock.Now())), CaPub: string(ssh.MarshalAuthorizedKey(ca.PublicKey()))}), nil
	}, nil)
	checker := &ssh.CertChecker{Clock: clock.Now, IsUserAuthority: func(key ssh.PublicKey) bool { return bytes.Equal(key.Marshal(), ca.PublicKey().Marshal()) }}
	config := &ssh.ServerConfig{PublicKeyCallback: checker.Authenticate}
	config.AddHostKey(host)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	started, release := make(chan struct{}), make(chan struct{})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
				server, channels, requests, err := ssh.NewServerConn(conn, config)
				if err != nil {
					return
				}
				defer server.Close()
				go ssh.DiscardRequests(requests)
				for channel := range channels {
					ch, requests, err := channel.Accept()
					if err != nil {
						return
					}
					for req := range requests {
						if req.Type != "exec" {
							_ = req.Reply(false, nil)
							continue
						}
						var command struct{ Command string }
						_ = ssh.Unmarshal(req.Payload, &command)
						_ = req.Reply(true, nil)
						if command.Command == "long-command" {
							close(started)
							select {
							case <-release:
							case <-ctx.Done():
							}
						}
						_, _ = ch.Write([]byte("ok\n"))
						_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{0}))
						_ = ch.Close()
						break
					}
				}
			}()
		}
	}()
	backend := &tenkiBackend{cfg: core.Config{Tenki: core.TenkiConfig{Endpoint: authority.URL}}, rt: core.Runtime{Clock: clock, HTTP: authority.Client()}}
	trust := filepath.Join(t.TempDir(), "known_hosts")
	// OpenSSH's nonstandard-port host pattern is [host]:port.
	hostname, port, _ := net.SplitHostPort(listener.Addr().String())
	if err := os.WriteFile(trust, []byte(fmt.Sprintf("[%s]:%s %s", hostname, port, ssh.MarshalAuthorizedKey(host.PublicKey()))), 0o600); err != nil {
		t.Fatal(err)
	}
	target := backend.sshTarget(material.output, trust, "")
	target.Host, target.Port, target.ProxyCommand, target.SSHConfigProxy = hostname, port, "", false
	completed := make(chan error, 1)
	go func() { _, err := core.RunSSHOutput(ctx, target, "long-command"); completed <- err }()
	select {
	case <-started:
	case err := <-completed:
		t.Fatalf("start: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	clock.unix.Add(11 * 60)
	for _, command := range []string{"owner-renewal", "collection", "cleanup"} {
		if out, err := core.RunSSHOutput(ctx, target, command); err != nil || out != "ok" {
			t.Fatalf("%s: out=%q err=%v", command, out, err)
		}
	}
	if issued.Load() != 1 {
		t.Fatalf("issued=%d", issued.Load())
	}
	select {
	case err := <-completed:
		t.Fatalf("long command interrupted: %v", err)
	default:
	}
	close(release)
	if err := <-completed; err != nil {
		t.Fatal(err)
	}
}
