//go:build darwin || linux

package aws

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"golang.org/x/crypto/ssh"
)

func TestAWSSSMProviderTargetReadinessUsesOpenSSHProxy(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Fatal("real OpenSSH is required:", err)
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Fatal("python3 is required for the byte proxy fixture:", err)
	}
	const user = "synthetic-ssm"
	serverPort := startSSMReadinessSSHServer(t, user)
	trap, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var directDials atomic.Int32
	var trapWG sync.WaitGroup
	trapWG.Add(1)
	go func() {
		defer trapWG.Done()
		for {
			conn, err := trap.Accept()
			if err != nil {
				return
			}
			directDials.Add(1)
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = trap.Close(); trapWG.Wait() })

	dir := t.TempDir()
	argsPath := filepath.Join(dir, "aws.args")
	awsPath := filepath.Join(dir, "aws")
	proxy := fmt.Sprintf(`#!/usr/bin/env python3
import os
import select
import socket
import sys
with open(os.environ["AWS_ARGS_PATH"], "a", encoding="utf-8") as log:
    log.write(" ".join(sys.argv[1:]) + "\n")
stream = socket.create_connection(("127.0.0.1", %d), timeout=5)
stdin_open = True
while True:
    readers = [stream]
    if stdin_open:
        readers.append(sys.stdin.buffer)
    ready, _, _ = select.select(readers, [], [])
    if stream in ready:
        data = stream.recv(65536)
        if not data:
            break
        sys.stdout.buffer.write(data)
        sys.stdout.buffer.flush()
    if stdin_open and sys.stdin.buffer in ready:
        data = os.read(sys.stdin.fileno(), 65536)
        if data:
            stream.sendall(data)
        else:
            stdin_open = False
            stream.shutdown(socket.SHUT_WR)
`, serverPort)
	if err := os.WriteFile(awsPath, []byte(proxy), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AWS_ARGS_PATH", argsPath)

	trapPort := strconv.Itoa(trap.Addr().(*net.TCPAddr).Port)
	cfg := core.Config{Provider: "aws", AWSSSHTransport: core.AWSSSHTransportSSM, AWSRegion: "us-west-2", SSHUser: user, SSHPort: trapPort, SSHFallbackPorts: []string{}, TargetOS: "linux"}
	server := core.Server{CloudID: "127.0.0.1"}
	target := awsSSHTarget(cfg, server, "203.0.113.10")
	target.DisableHostKeyChecking = true

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var progress bytes.Buffer
	if err := core.WaitForSSHReady(ctx, &target, &progress, "bootstrap", 5*time.Second); err != nil {
		t.Fatalf("provider target readiness: %v; progress=%s", err, progress.String())
	}
	output, err := core.RunSSHOutput(ctx, target, "printf provider-normal")
	if err != nil || output != "provider-normal" {
		t.Fatalf("provider target normal exec output=%q err=%v", output, err)
	}
	if got := directDials.Load(); got != 0 {
		t.Fatalf("readiness made %d direct TCP dial(s) before using ProxyCommand", got)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "ssm start-session --target 127.0.0.1 --document-name AWS-StartSSHSession --parameters portNumber=" + trapPort + " --region us-west-2\n"
	if got := strings.Count(string(args), want); got != 2 {
		t.Fatalf("documented ProxyCommand argv count=%d want=2; args=%q", got, args)
	}
}

func TestAWSSSMTargetNotConnectedReadinessRetries(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Fatal("real OpenSSH is required:", err)
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Fatal("python3 is required for the byte proxy fixture:", err)
	}
	const user = "synthetic-ssm"
	serverPort := startSSMReadinessSSHServer(t, user)
	for _, test := range []struct {
		name         string
		failures     int
		timeout      time.Duration
		wantTimeout  bool
		minimumCalls int
	}{
		{name: "transient rejection then ready", failures: 1, timeout: 5 * time.Second, minimumCalls: 2},
		{name: "repeated transient rejection reaches deadline", failures: -1, timeout: 900 * time.Millisecond, wantTimeout: true, minimumCalls: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			statePath := filepath.Join(dir, "calls")
			proxy := `#!/usr/bin/env python3
import os
import select
import socket
import sys

state_path = os.environ["AWS_STATE_PATH"]
try:
    with open(state_path, "r", encoding="utf-8") as state:
        calls = int(state.read()) + 1
except FileNotFoundError:
    calls = 1
with open(state_path, "w", encoding="utf-8") as state:
    state.write(str(calls))
failures = int(os.environ["AWS_TRANSIENT_FAILURES"])
if failures < 0 or calls <= failures:
    print("An error occurred (TargetNotConnected) when calling the StartSession operation", file=sys.stderr)
    sys.exit(254)
stream = socket.create_connection(("127.0.0.1", int(os.environ["AWS_PROXY_PORT"])), timeout=5)
stdin_open = True
while True:
    readers = [stream]
    if stdin_open:
        readers.append(sys.stdin.buffer)
    ready, _, _ = select.select(readers, [], [])
    if stream in ready:
        data = stream.recv(65536)
        if not data:
            break
        sys.stdout.buffer.write(data)
        sys.stdout.buffer.flush()
    if stdin_open and sys.stdin.buffer in ready:
        data = os.read(sys.stdin.fileno(), 65536)
        if data:
            stream.sendall(data)
        else:
            stdin_open = False
            stream.shutdown(socket.SHUT_WR)
`
			if err := os.WriteFile(filepath.Join(dir, "aws"), []byte(proxy), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("AWS_STATE_PATH", statePath)
			t.Setenv("AWS_TRANSIENT_FAILURES", strconv.Itoa(test.failures))
			t.Setenv("AWS_PROXY_PORT", strconv.Itoa(serverPort))

			trap, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer trap.Close()
			cfg := core.Config{Provider: "aws", AWSSSHTransport: core.AWSSSHTransportSSM, AWSRegion: "us-west-2", SSHUser: user, SSHPort: strconv.Itoa(trap.Addr().(*net.TCPAddr).Port), SSHFallbackPorts: []string{}, TargetOS: "linux"}
			target := awsSSHTarget(cfg, core.Server{CloudID: "127.0.0.1"}, "127.0.0.1")
			target.DisableHostKeyChecking = true
			var progress bytes.Buffer
			err = core.WaitForSSHReady(t.Context(), &target, &progress, "bootstrap", test.timeout)
			if test.wantTimeout {
				if err == nil || !strings.Contains(err.Error(), "timed out waiting for SSH") || strings.Contains(err.Error(), "TargetNotConnected") {
					t.Fatalf("readiness error=%v progress=%q, want sanitized overall timeout", err, progress.String())
				}
			} else if err != nil {
				t.Fatalf("transient readiness did not recover: %v; progress=%s", err, progress.String())
			}
			calls, readErr := os.ReadFile(statePath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			count, parseErr := strconv.Atoi(string(calls))
			if parseErr != nil || count < test.minimumCalls {
				t.Fatalf("StartSession calls=%q err=%v, want at least %d", calls, parseErr, test.minimumCalls)
			}
		})
	}
}

func TestAWSSSMReadinessFailuresNeverDialDirect(t *testing.T) {
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		t.Fatal("real OpenSSH is required:", err)
	}
	tests := []struct {
		name      string
		awsScript string
		wantError string
	}{
		{name: "missing AWS CLI", wantError: "requires the AWS CLI in PATH"},
		{name: "missing Session Manager plugin", awsScript: "#!/bin/sh\nprintf '%s\\n' 'SessionManagerPlugin is not found' >&2\nexit 255\n", wantError: "requires the Session Manager plugin"},
		{name: "StartSession access denied", awsScript: "#!/bin/sh\nprintf '%s\\n' 'An error occurred (AccessDeniedException) when calling the StartSession operation' >&2\nexit 254\n", wantError: "was denied by AWS"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			statePath := filepath.Join(dir, "calls")
			if err := os.Symlink(sshPath, filepath.Join(dir, "ssh")); err != nil {
				t.Fatal(err)
			}
			if test.awsScript != "" {
				script := strings.Replace(test.awsScript, "#!/bin/sh\n", "#!/bin/sh\nprintf x >>\"$AWS_STATE_PATH\"\n", 1)
				if err := os.WriteFile(filepath.Join(dir, "aws"), []byte(script), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", dir)
			t.Setenv("AWS_STATE_PATH", statePath)

			trap, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			var directDials atomic.Int32
			var trapWG sync.WaitGroup
			trapWG.Add(1)
			go func() {
				defer trapWG.Done()
				for {
					conn, err := trap.Accept()
					if err != nil {
						return
					}
					directDials.Add(1)
					_ = conn.Close()
				}
			}()
			defer func() {
				_ = trap.Close()
				trapWG.Wait()
			}()

			cfg := core.Config{
				Provider:         "aws",
				AWSSSHTransport:  core.AWSSSHTransportSSM,
				AWSRegion:        "us-west-2",
				SSHUser:          "crabbox",
				SSHPort:          strconv.Itoa(trap.Addr().(*net.TCPAddr).Port),
				SSHFallbackPorts: []string{},
				TargetOS:         "linux",
			}
			target := awsSSHTarget(cfg, core.Server{CloudID: "127.0.0.1"}, "127.0.0.1")
			target.DisableHostKeyChecking = true
			var progress bytes.Buffer
			err = core.WaitForSSHReady(t.Context(), &target, &progress, "bootstrap", 2*time.Second)
			if err == nil || !strings.Contains(err.Error(), test.wantError) || strings.Contains(err.Error(), "timed out waiting for SSH") {
				t.Fatalf("readiness error=%v progress=%q, want terminal diagnostic %q", err, progress.String(), test.wantError)
			}
			if got := directDials.Load(); got != 0 {
				t.Fatalf("readiness made %d direct TCP dial(s) after proxy failure", got)
			}
			calls, readErr := os.ReadFile(statePath)
			if test.awsScript == "" {
				if !os.IsNotExist(readErr) {
					t.Fatalf("missing-CLI calls=%q err=%v, want no executable invocation", calls, readErr)
				}
			} else if readErr != nil || string(calls) != "x" {
				t.Fatalf("terminal StartSession calls=%q err=%v, want exactly one", calls, readErr)
			}
		})
	}
}

func startSSMReadinessSSHServer(t *testing.T, user string) int {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{NoClientAuth: true, NoClientAuthCallback: func(meta ssh.ConnMetadata) (*ssh.Permissions, error) {
		if meta.User() != user {
			return nil, fmt.Errorf("unexpected SSH user %q", meta.User())
		}
		return nil, nil
	}}
	config.AddHostKey(signer)
	var connections sync.WaitGroup
	connections.Add(1)
	go func() {
		defer connections.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			go func() {
				defer connections.Done()
				defer conn.Close()
				transport, channels, requests, err := ssh.NewServerConn(conn, config)
				if err != nil {
					return
				}
				defer transport.Close()
				go ssh.DiscardRequests(requests)
				for incoming := range channels {
					if incoming.ChannelType() != "session" {
						_ = incoming.Reject(ssh.UnknownChannelType, "session required")
						continue
					}
					channel, sessionRequests, err := incoming.Accept()
					if err != nil {
						continue
					}
					for request := range sessionRequests {
						var payload struct{ Command string }
						if request.Type != "exec" || ssh.Unmarshal(request.Payload, &payload) != nil {
							_ = request.Reply(false, nil)
							continue
						}
						_ = request.Reply(true, nil)
						if payload.Command == "printf provider-normal" {
							_, _ = channel.Write([]byte("provider-normal"))
						}
						_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
						_ = channel.Close()
						break
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); connections.Wait() })
	return listener.Addr().(*net.TCPAddr).Port
}
