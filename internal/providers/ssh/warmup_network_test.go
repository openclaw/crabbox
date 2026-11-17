package ssh

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func TestWarmupStaticNetworkReporting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX SSH executable")
	}
	// Loopback only supplies transport readiness. This does not test tailnet
	// membership or change the resolver's network-mode semantics.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	t.Cleanup(func() { listener.Close(); <-done })
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{"ssh", "static-ssh"} {
		for _, mode := range []string{"public", "auto", "tailscale"} {
			t.Run(provider+"/"+mode, func(t *testing.T) {
				testutil.IsolateUserDirs(t)
				for _, entry := range os.Environ() {
					name, _, _ := strings.Cut(entry, "=")
					if strings.HasPrefix(name, "CRABBOX_") {
						t.Setenv(name, "")
					}
				}
				root := t.TempDir()
				t.Chdir(root)
				bin := filepath.Join(root, "bin")
				if err := os.Mkdir(bin, 0700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", bin)
				config := filepath.Join(root, "config.yaml")
				if err := os.WriteFile(config, []byte("provider: ssh\ntarget: macos\nstatic:\n  noControlMaster: true\n"), 0600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("CRABBOX_CONFIG", config)
				log := filepath.Join(root, "ssh.log")
				t.Setenv("TEST_SSH_LOG", log)
				t.Setenv("TEST_SSH_PORT", port)
				t.Setenv("TEST_ARCH_PROBE", "/bin/sh -c "+core.ShellQuote(macArchitectureProbe))
				// Never execute the received command; reject anything except
				// the three exact readiness/architecture probes used here.
				script := `#!/bin/sh
while [ "$#" -gt 2 ]; do
    case "$1" in
        -n) shift ;;
        -p) [ "$2" = "$TEST_SSH_PORT" ] || exit 99; shift 2 ;;
        -o|-i) shift 2 ;;
        *) exit 99 ;;
    esac
done
[ "$#" = 2 ] && [ "$1" = 'alice@127.0.0.1' ] || exit 99
case "$2" in
    'git --version >/dev/null && rsync --version >/dev/null && tar --version >/dev/null'|'exit 0') ;;
    "$TEST_ARCH_PROBE") printf 'v1|arm64|arm64|arm64|false\n' ;;
    *) exit 99 ;;
esac
printf '%s\n' "$1" >> "$TEST_SSH_LOG"
`
				if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nexit 128\n"), 0700); err != nil {
					t.Fatal(err)
				}
				flags := []string{"--provider", provider, "--target", "macos", "--static-host", "127.0.0.1", "--static-port", port, "--static-user", "alice", "--network", mode}
				run := func(args []string) string {
					t.Helper()
					ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
					defer cancel()
					var stdout, stderr bytes.Buffer
					if err := (core.App{Stdout: &stdout, Stderr: &stderr}).Run(ctx, args); err != nil {
						t.Fatalf("%v: %v\n%s", args, err, &stderr)
					}
					return stdout.String()
				}
				warm := run(append(append([]string{"warmup"}, flags...), "--keep"))
				status := run(append(append([]string{"status"}, flags...), "--id", "static_127-0-0-1", "--json"))
				var view struct {
					Network string
					Ready   bool
					Target  string
				}
				if err := json.Unmarshal([]byte(status), &view); err != nil {
					t.Fatal(err)
				}
				want := "public"
				if mode == "tailscale" {
					want = "tailscale"
				}
				if view.Network != want || !view.Ready || view.Target != "macos" {
					t.Fatalf("status=%s", status)
				}
				var ready string
				for _, line := range strings.Split(warm, "\n") {
					if strings.HasPrefix(line, "ready ") {
						ready = line
					}
				}
				if !strings.Contains(ready, " network="+view.Network+" ") {
					t.Errorf("warmup/status network mismatch: %q; status network=%q", ready, view.Network)
				}
				if !strings.Contains(ready, "ssh=alice@127.0.0.1:"+port+" ") {
					t.Errorf("warmup destination changed: %q", ready)
				}
				calls, err := os.ReadFile(log)
				if err != nil {
					t.Fatal(err)
				}
				wantCalls := 3
				if mode == "tailscale" {
					wantCalls = 5
				}
				if got := strings.Count(string(calls), "alice@127.0.0.1\n"); got != wantCalls {
					t.Fatalf("SSH calls=%d, want %d: %s", got, wantCalls, calls)
				}
			})
		}
	}
}
