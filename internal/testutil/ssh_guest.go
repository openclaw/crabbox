package testutil

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
)

// RecordingSSHGuest authenticates only the supplied public key and records exec
// requests without executing them on the test host.
func RecordingSSHGuest(t *testing.T, publicKeyPath string) (string, string, func() []string) {
	t.Helper()
	data, err := os.ReadFile(publicKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, _, _, _, err := ssh.ParseAuthorizedKey(data)
	if err != nil {
		t.Fatal(err)
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if bytes.Equal(key.Marshal(), publicKey.Marshal()) {
			return nil, nil
		}
		return nil, os.ErrPermission
	}}
	cfg.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var commands []string
	var conns []net.Conn
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer conn.Close()
				_, channels, requests, err := ssh.NewServerConn(conn, cfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(requests)
				for channel := range channels {
					if channel.ChannelType() != "session" {
						_ = channel.Reject(ssh.UnknownChannelType, "session only")
						continue
					}
					session, requests, err := channel.Accept()
					if err != nil {
						continue
					}
					for req := range requests {
						if req.Type != "exec" {
							_ = req.Reply(false, nil)
							continue
						}
						var payload struct{ Command string }
						if ssh.Unmarshal(req.Payload, &payload) != nil {
							_ = req.Reply(false, nil)
							continue
						}
						mu.Lock()
						commands = append(commands, payload.Command)
						mu.Unlock()
						_ = req.Reply(true, nil)
						_, _ = session.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
						_ = session.Close()
						break
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		for _, conn := range conns {
			_ = conn.Close()
		}
		mu.Unlock()
		workers.Wait()
	})
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	return host, port, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), commands...) }
}
