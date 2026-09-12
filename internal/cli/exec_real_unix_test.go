//go:build darwin || linux

package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestExecCommandKeepsTokenOutOfRealSSHAuthenticationFailure(t *testing.T) {
	b, dir := setupExecCommand(t)
	// This loopback fixture rejects authentication and cannot execute commands.
	// Real OpenSSH normally includes the rejected username in this diagnostic.
	server := newForwardSSHServer(t, "different-synthetic-user")
	t.Setenv("PATH", "/usr/bin:"+os.Getenv("PATH"))
	knownHosts := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(knownHosts, []byte(fmt.Sprintf("[127.0.0.1]:%d %s\n", server.port(), server.hostKey)), 0o600); err != nil {
		t.Fatal(err)
	}
	b.lease.SSH.Host = "127.0.0.1"
	b.lease.SSH.Port = strconv.Itoa(server.port())
	b.lease.SSH.SSHHostKey = server.hostKey
	b.lease.SSH.KnownHostsFile = knownHosts
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var out, diagnostic bytes.Buffer
	err := (App{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &diagnostic}).Run(ctx, []string{"exec", "--id", b.lease.LeaseID, "--", "true"})
	var commandExit ExitError
	if !AsExitError(err, &commandExit) || commandExit.Code != 255 {
		t.Fatalf("authentication failure=%v", err)
	}
	server.mu.Lock()
	authReached := len(server.users) > 0 && server.users[0] == b.lease.SSH.User
	server.mu.Unlock()
	if !authReached {
		t.Fatal("connection failed before the synthetic authentication rejection")
	}
	if out.Len() != 0 || diagnostic.Len() != 0 {
		t.Fatalf("transport diagnostics escaped: stdout=%q stderr=%q", out.String(), diagnostic.String())
	}
}
