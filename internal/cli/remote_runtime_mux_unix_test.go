//go:build darwin || linux

package cli

import (
	"context"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestNativeWatchedInputClosesOnMultiplexedCancellation(t *testing.T) {
	server := newForwardSSHServer(t, "fixture")
	close(server.release)
	ready, lost := make(chan struct{}), make(chan struct{})
	handler := func(incoming ssh.NewChannel, _ string) {
		ch, requests, err := incoming.Accept()
		if err != nil {
			return
		}
		defer ch.Close()
		for request := range requests {
			var payload struct{ Command string }
			if request.Type != "exec" || ssh.Unmarshal(request.Payload, &payload) != nil {
				_ = request.Reply(false, nil)
				continue
			}
			if payload.Command != "watched" && payload.Command != "complete" && payload.Command != "exit 0" {
				_ = request.Reply(false, nil)
				return
			}
			_ = request.Reply(true, nil)
			if payload.Command == "watched" || payload.Command == "complete" {
				frame := make([]byte, 5)
				if _, err := io.ReadFull(ch, frame); err != nil || string(frame) != "frame" {
					t.Errorf("frame: %q %v", frame, err)
					return
				}
				if payload.Command == "complete" {
					_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
					return
				}
				close(ready)
				var one [1]byte
				if _, err := ch.Read(one[:]); err != io.EOF {
					t.Errorf("lifetime: %v", err)
					return
				}
				close(lost)
			}
			_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
			return
		}
	}
	server.mu.Lock()
	server.sessionHandler = handler
	server.mu.Unlock()
	target := SSHTarget{Host: "127.0.0.1", Port: strconv.Itoa(server.port()), User: "fixture", TargetOS: targetLinux, DisableHostKeyChecking: true, FallbackPorts: []string{}}
	target, closeMaster, err := startWorkspaceOwnerControl(t.Context(), target)
	if err != nil {
		t.Fatal(err)
	}
	defer closeMaster()
	if target.ownerControlPath == "" {
		t.Fatal("fixture did not establish a real multiplexed connection")
	}
	frame, err := newReplayableSSHInputStream([]byte("frame"), strings.NewReader(""), 0)
	if err != nil {
		t.Fatal(err)
	}
	transport := sshTransportPreparation{command: "watched", replay: frame, native: &remoteNativeRuntime{}}
	defer transport.close()
	started := time.Now()
	for range 2 {
		transport.command = "complete"
		if _, err := transport.runOnce(t.Context(), target, "2", "1", io.Discard, io.Discard, false); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Fatalf("completed channels incurred a fixed wait: %s", elapsed)
	} else {
		t.Logf("two completed channels: %s", elapsed)
	}
	server.mu.Lock()
	beforeCancel := len(server.users)
	server.mu.Unlock()
	if beforeCancel != 1 {
		t.Fatalf("normal commands did not reuse master: %d handshakes", beforeCancel)
	}
	transport.command = "watched"
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := transport.runOnce(ctx, target, "2", "1", io.Discard, io.Discard, false); done <- err }()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("frame never reached SSH channel")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("canceled SSH did not return")
	}
	select {
	case <-lost:
	case <-time.After(10 * time.Second):
		t.Fatal("surviving master kept watched input alive")
	}
	if err := runSSHQuiet(t.Context(), target, "exit 0"); err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	handshakes := len(server.users)
	server.mu.Unlock()
	if handshakes != 1 {
		t.Fatalf("unexpected reconnects after cancellation: %d handshakes", handshakes)
	}
	t.Logf("two completed commands: %d handshake; cancellation/recovery: %d total", beforeCancel, handshakes)
}
