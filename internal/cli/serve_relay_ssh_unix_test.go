//go:build darwin || linux

package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestServeRelaySSHAllowsPairingBeforeHandshake(t *testing.T) {
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		t.Fatal(err)
	}
	isolateTestUserDirs(t)
	root := t.TempDir()
	server := newForwardSSHServer(t, "relay-inner")
	server.enableReadinessSessions(t, root, true)
	close(server.release)
	jump := newForwardSSHServer(t, "relay-front", server.port())
	close(jump.release)
	knownHosts := filepath.Join(root, "known_hosts")
	pins := fmt.Sprintf("[127.0.0.1]:%d %s\n[127.0.0.1]:%d %s\n", server.port(), server.hostKey, jump.port(), jump.hostKey)
	if err := os.WriteFile(knownHosts, []byte(pins), 0600); err != nil {
		t.Fatal(err)
	}
	// OpenSSH prepends exec to ProxyCommand, so the delay needs its own shell.
	proxy := "/bin/sh -c " + ShellQuote("sleep 2; exec "+ShellQuote(sshPath)+" -F /dev/null -o BatchMode=yes -o StrictHostKeyChecking=yes -o GlobalKnownHostsFile=none -o "+ShellQuote("UserKnownHostsFile="+sshConfigFileValue(knownHosts))+" -p "+strconv.Itoa(jump.port())+" -W 127.0.0.1:"+strconv.Itoa(server.port())+" relay-front@127.0.0.1")
	for _, privateConfig := range []bool{false, true} {
		t.Run(fmt.Sprintf("private-config=%t", privateConfig), func(t *testing.T) {
			target := SSHTarget{Host: "127.0.0.1", Port: strconv.Itoa(server.port()), User: "relay-inner", KnownHostsFile: knownHosts, NoControlMaster: true, ProxyCommand: proxy, AuthSecret: privateConfig, FallbackPorts: []string{}}
			var diagnostic bytes.Buffer
			probe := func(ctx context.Context) error {
				diagnostic.Reset()
				return executeSSH(ctx, &target, "exit 0", nil, 0, 0, "1", "1", io.Discard, &diagnostic)
			}
			// The ordinary one-second probe cannot accommodate relay pairing.
			if err := probe(t.Context()); err == nil {
				t.Fatal("ordinary short timeout unexpectedly succeeded")
			}
			target.SSHJump = &SSHJump{useRelay: true}
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			if err := probe(ctx); err != nil {
				t.Fatalf("relay pairing exhausted SSH timeout: %v: %s", err, diagnostic.String())
			}
			ctx, cancelEarly := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancelEarly()
			if err := probe(ctx); err == nil || ctx.Err() == nil {
				t.Fatal("relay ignored caller deadline")
			}
		})
	}
}
