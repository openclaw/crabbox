package cli

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestWaitForSSHReadyRetriesTransientFailureWithinBudget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell SSH fixture")
	}
	for _, proxy := range []bool{false, true} {
		name := "direct"
		if proxy {
			name = "proxy"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			calls := filepath.Join(dir, "calls")
			script := `#!/bin/sh
for remote; do :; done
printf '%s\n' "$remote" >> "$CRABBOX_RETRY_CALLS"
if [ "$remote" = fixture-ready ] && [ ! -f "$CRABBOX_RETRY_MARKER" ]; then
  touch "$CRABBOX_RETRY_MARKER"
  exit 1
fi
exit 0
`
			if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("CRABBOX_RETRY_CALLS", calls)
			t.Setenv("CRABBOX_RETRY_MARKER", filepath.Join(dir, "ready"))
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			host, port, err := net.SplitHostPort(listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			target := SSHTarget{
				User: "runner", Host: host, Port: port, FallbackPorts: []string{},
				ReadyCheck: "fixture-ready", SSHConfigProxy: proxy,
			}
			if err := waitForSSHReady(context.Background(), &target, io.Discard, "bootstrap", 3*time.Second); err != nil {
				t.Fatalf("transient readiness failure exhausted startup budget: %v", err)
			}
			got, err := os.ReadFile(calls)
			if err != nil || string(got) != "fixture-ready\nexit 0\nfixture-ready\n" {
				t.Fatalf("SSH calls=%q error=%v; want readiness, diagnostic transport, successful readiness", got, err)
			}
		})
	}
}
