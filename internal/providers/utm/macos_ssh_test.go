package utm

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestPasswordBootstrapProvesIdentityBeforeInstallingContinuingKey(t *testing.T) {
	for _, identityOK := range []bool{true, false} {
		t.Run(strconv.FormatBool(identityOK), func(t *testing.T) {
			_, hostPrivate, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			signer, err := ssh.NewSignerFromKey(hostPrivate)
			if err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			password := "fixture-only-initial-password"
			public := "ssh-ed25519 fixture-public-key"
			config := &ssh.ServerConfig{PasswordCallback: func(_ ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) {
				if string(p) != password {
					return nil, io.EOF
				}
				return nil, nil
			}}
			config.AddHostKey(signer)
			var mu sync.Mutex
			var commands []string
			done := make(chan struct{})
			go func() {
				defer close(done)
				conn, e := listener.Accept()
				if e != nil {
					return
				}
				defer conn.Close()
				server, channels, requests, e := ssh.NewServerConn(conn, config)
				if e != nil {
					return
				}
				defer server.Close()
				go ssh.DiscardRequests(requests)
				for incoming := range channels {
					if incoming.ChannelType() != "session" {
						incoming.Reject(ssh.UnknownChannelType, "session required")
						continue
					}
					ch, reqs, e := incoming.Accept()
					if e != nil {
						return
					}
					go func() {
						defer ch.Close()
						for req := range reqs {
							if req.Type != "exec" {
								req.Reply(false, nil)
								continue
							}
							var payload struct{ Command string }
							if ssh.Unmarshal(req.Payload, &payload) != nil {
								return
							}
							mu.Lock()
							commands = append(commands, payload.Command)
							count := len(commands)
							mu.Unlock()
							req.Reply(true, nil)
							status := uint32(0)
							if count == 1 && !identityOK {
								status = 1
								ch.Stderr().Write([]byte(password))
							}
							ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
							return
						}
					}()
				}
			}()
			r := record{Address: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port, User: "fixture", ImageVersion: "27.0.1", ImageBuild: "fixture", MAC: "02:00:00:00:00:01"}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			key, authenticated, err := bootstrapMacOSAttempt(ctx, r, password, public)
			<-done
			mu.Lock()
			defer mu.Unlock()
			if !authenticated {
				t.Fatal("password SSH handshake failed")
			}
			if identityOK {
				if err != nil || key != strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) || len(commands) != 2 || !strings.Contains(commands[1], public) {
					t.Fatal("authenticated identity did not install continuing key and pin host")
				}
			} else if err == nil || key != "" || len(commands) != 1 {
				t.Fatal("rejected guest received continuing key")
			}
			for _, command := range commands {
				if strings.Contains(command, password) {
					t.Fatal("password entered remote command text")
				}
			}
			if err != nil && strings.Contains(err.Error(), password) {
				t.Fatal("remote credential echo reached diagnostics")
			}
		})
	}
}
func TestPasswordBootstrapCancellationClosesHandshake(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	started := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			started <- err
			return
		}
		defer conn.Close()
		accepted <- conn
		reader := bufio.NewReader(conn)
		_, err = reader.ReadString('\n')
		started <- err
		if err == nil {
			_, _ = io.Copy(io.Discard, reader)
		}
	}()
	// Exercise caller cancellation while SSH is waiting for the server banner.
	// A shared socket/context deadline can expire in the network poller before
	// the context timer publishes Err(), so wall-clock ordering cannot prove it.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type outcome struct {
		key           string
		authenticated bool
		err           error
	}
	returned := make(chan outcome, 1)
	go func() {
		key, authenticated, err := bootstrapMacOSAttempt(ctx, record{Address: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port, User: "fixture"}, "fixture-only", "fixture-public")
		returned <- outcome{key, authenticated, err}
	}()
	select {
	case conn := <-accepted:
		defer conn.Close()
	case <-time.After(time.Second):
		t.Fatal("SSH connection was not accepted")
	}
	select {
	case err := <-started:
		if err != nil {
			t.Fatalf("client SSH preamble failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("SSH handshake did not start")
	}
	cancel()
	select {
	case result := <-returned:
		if result.err == nil || result.authenticated || result.key != "" || ctx.Err() != context.Canceled {
			t.Fatalf("canceled handshake accepted: error=%v authenticated=%t host-key-present=%t context-error=%v", result.err, result.authenticated, result.key != "", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("canceled SSH handshake did not return")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled SSH connection remained open")
	}
}
