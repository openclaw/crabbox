package cli

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A guest missing the readiness baseline reaches SSH and authenticates; only
// the readiness command fails. Reporting that as an unknown-authentication
// transport timeout sends operators after the wrong layer.
func TestWaitForSSHReadyTimeoutReportsProvenAuthentication(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell ssh fixture")
	}
	dir := t.TempDir()
	script := `#!/bin/sh
for remote; do :; done
if [ "$remote" = "exit 0" ]; then exit 0; fi
printf 'sh: node: command not found\n' >&2
exit 127
`
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	target := SSHTarget{User: "runner", Host: host, Port: port, TargetOS: targetMacOS, NoControlMaster: true}
	var progress bytes.Buffer
	err = waitForSSHReady(t.Context(), &target, &progress, "bootstrap", 200*time.Millisecond)
	if err == nil {
		t.Fatal("readiness wait unexpectedly succeeded")
	}
	message := err.Error()

	if !strings.Contains(message, "probe=readiness") {
		t.Fatalf("timeout does not blame readiness: %s", message)
	}
	// The transport probe returned 0 on this port, so authentication is proven,
	// not unknown.
	if strings.Contains(message, "authentication=unknown") {
		t.Fatalf("timeout still reports authentication as unknown after a successful transport probe: %s", message)
	}
	if !strings.Contains(message, "authentication=ok") {
		t.Fatalf("timeout does not record proven authentication: %s", message)
	}
	if !strings.Contains(message, port+":ready") {
		t.Fatalf("port status does not mark readiness as the failing stage: %s", message)
	}
}
