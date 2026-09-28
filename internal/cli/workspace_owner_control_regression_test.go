//go:build darwin || linux

package cli

import (
	"context"
	"io"
	"strconv"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestWorkspaceOwnerAuthenticatesOnce(t *testing.T) {
	server := newForwardSSHServer(t, "fixture")
	close(server.release)
	var releasing atomic.Bool
	server.mu.Lock()
	server.sessionHandler = func(incoming ssh.NewChannel, _ string) {
		channel, requests, err := incoming.Accept()
		if err != nil {
			return
		}
		defer channel.Close()
		for request := range requests {
			if request.Type != "exec" {
				_ = request.Reply(false, nil)
				continue
			}
			_ = request.Reply(true, nil)
			_, _ = io.Copy(io.Discard, channel)
			response := "ACQUIRED"
			if releasing.Load() {
				response = "RELEASED"
			}
			_, _ = io.WriteString(channel, response+"\n")
			_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
			return
		}
	}
	server.mu.Unlock()
	target := SSHTarget{Host: "127.0.0.1", Port: strconv.Itoa(server.port()), User: "fixture", TargetOS: targetLinux,
		FallbackPorts: []string{}, DisableHostKeyChecking: true}
	owner, err := acquireWorkspaceOwner(t.Context(), target, "fixture", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		releasing.Store(true)
		if err := owner.Close(context.WithoutCancel(t.Context())); err != nil {
			t.Error(err)
		}
	})
	for range 3 {
		_, err := owner.transport.Do(t.Context(), workspaceOwnerRemoteRequest{Action: workspaceOwnerInspect, Key: owner.key, Token: owner.token, TTL: owner.ttl})
		if err != nil {
			t.Fatal(err)
		}
	}
	server.mu.Lock()
	handshakes := len(server.users)
	server.mu.Unlock()
	if handshakes != 1 {
		t.Fatalf("owner acquire and three inspections authenticated %d times, want 1", handshakes)
	}
}
