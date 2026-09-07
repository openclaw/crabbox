package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func preparedEndpointFixture(t *testing.T) (SSHTarget, string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX SSH process fixture")
	}
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	ports := make([]string, 2)
	for i := range ports {
		listener, err := net.Listen("tcp4", "0.0.0.0:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { listener.Close() })
		_, ports[i], _ = net.SplitHostPort(listener.Addr().String())
	}
	script := `#!/bin/sh
port="";host="";remote=""
while [ "$#" -gt 0 ]; do
 case "$1" in -p) shift;port="$1";; *@*) host="$1";; esac
 remote="$1";shift
done
printf '%s:%s:%s\n' "$host" "$port" "$remote" >> "$CRABBOX_ENDPOINT_CALLS"
case "$host" in
 *@::ffff:127.0.0.1) [ "$port" = "$CRABBOX_ENDPOINT_FALLBACK" ] || exit 255;;
 *@127.0.0.1) [ -z "$CRABBOX_ENDPOINT_PUBLIC" ] || [ "$port" = "$CRABBOX_ENDPOINT_PUBLIC" ] || exit 255;;
esac
[ "$remote" != fail-workload ] || exit 255
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_ENDPOINT_CALLS", calls)
	t.Setenv("CRABBOX_ENDPOINT_FALLBACK", ports[1])
	t.Setenv("CRABBOX_ENDPOINT_PUBLIC", "")
	return SSHTarget{User: "runner", Host: "127.0.0.1", Port: ports[0], FallbackPorts: []string{ports[1]}, ReadyCheck: "fixture-ready"}, calls, ports[1]
}

// These tests count requests, not subprocess startup time. Virtual time keeps
// host scheduling from consuming the unrelated readiness deadline.
func TestSSHPreparedEndpointAvoidsRepeatedTransportProbes(t *testing.T) {
	for _, producer := range []string{"wait", "ready probe", "transport probe"} {
		t.Run(producer, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				target, calls, _ := preparedEndpointFixture(t)
				var ready bool
				switch producer {
				case "wait":
					ready = waitForSSHReady(t.Context(), &target, io.Discard, "test", time.Second) == nil
				case "ready probe":
					ready = probeSSHReady(t.Context(), &target, time.Second)
				case "transport probe":
					ready = probeSSHTransport(t.Context(), &target, time.Second)
				}
				if !ready {
					t.Fatal("fixture readiness failed")
				}
				for _, command := range []string{"workload-one", "workload-two"} {
					if _, err := runSSHOutput(t.Context(), target, command); err != nil {
						t.Fatal(err)
					}
				}
				data, err := os.ReadFile(calls)
				if err != nil {
					t.Fatal(err)
				}
				lines := splitNonEmptyLines(string(data))
				if len(lines) != 3 {
					t.Fatalf("ready plus two commands used %d SSH requests, want3: %s", len(lines), data)
				}
			})
		})
	}
}

func TestSSHPreparedEndpointRetargetPreservesAdvertisedFallback(t *testing.T) {
	for _, swap := range []bool{false, true} {
		t.Run(fmt.Sprint("initial fallback=", swap), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				target, calls, fallback := preparedEndpointFixture(t)
				primary := target.Port
				publicPort, tailnetPort := primary, fallback
				expected := fmt.Sprintf("runner@127.0.0.1:%s:fixture-ready\n", primary)
				if swap {
					publicPort, tailnetPort = fallback, primary
					t.Setenv("CRABBOX_ENDPOINT_PUBLIC", fallback)
					t.Setenv("CRABBOX_ENDPOINT_FALLBACK", primary)
					expected = fmt.Sprintf("runner@127.0.0.1:%s:fixture-ready\nrunner@127.0.0.1:%s:exit 0\nrunner@127.0.0.1:%s:fixture-ready\n", primary, primary, fallback)
				}
				if err := waitForSSHReady(t.Context(), &target, io.Discard, "test", time.Second); err != nil {
					t.Fatal(err)
				}
				server := Server{Labels: map[string]string{"tailscale": "true", "tailscale_fqdn": "::ffff:127.0.0.1"}}
				resolved, err := resolveNetworkTarget(t.Context(), Config{Network: NetworkTailscale}, server, target)
				if err != nil {
					t.Fatal(err)
				}
				if resolved.Target.Host != "::ffff:127.0.0.1" || resolved.Target.Port != tailnetPort {
					t.Fatalf("retarget lost advertised candidate: %+v", resolved.Target)
				}
				if _, err := runSSHOutput(t.Context(), resolved.Target, "tailnet-workload"); err != nil {
					t.Fatal(err)
				}
				expected += fmt.Sprintf("runner@::ffff:127.0.0.1:%s:exit 0\nrunner@::ffff:127.0.0.1:%s:exit 0\nrunner@::ffff:127.0.0.1:%s:tailnet-workload\n", publicPort, tailnetPort, tailnetPort)
				data, _ := os.ReadFile(calls)
				if string(data) != expected {
					t.Fatalf("retarget request sequence=%s, want %s", data, expected)
				}
			})
		})
	}
}

func TestSSHPreparedEndpointDoesNotReuseChangedPortOrFreshDescriptor(t *testing.T) {
	for _, change := range []string{"port", "fresh descriptor"} {
		t.Run(change, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				target, calls, fallback := preparedEndpointFixture(t)
				if err := waitForSSHReady(t.Context(), &target, io.Discard, "test", time.Second); err != nil {
					t.Fatal(err)
				}
				switch change {
				case "port":
					target.FallbackPorts = []string{target.Port}
					target.Port = fallback
				case "fresh descriptor":
					target = SSHTarget{User: target.User, Host: target.Host, Port: target.Port, FallbackPorts: target.FallbackPorts}
				}
				if _, err := runSSHOutput(t.Context(), target, "workload"); err != nil {
					t.Fatal(err)
				}
				data, _ := os.ReadFile(calls)
				if len(splitNonEmptyLines(string(data))) != 3 {
					t.Fatalf("changed endpoint did not resolve transport anew: %s", data)
				}
			})
		})
	}
}

func TestSSHPreparedEndpointDoesNotReplayWorkloadFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		target, calls, _ := preparedEndpointFixture(t)
		if err := waitForSSHReady(t.Context(), &target, io.Discard, "test", time.Second); err != nil {
			t.Fatal(err)
		}
		if _, err := runSSHOutput(context.Background(), target, "fail-workload"); exitCode(err) != 255 {
			t.Fatalf("workload exit=%v, want255", err)
		}
		data, _ := os.ReadFile(calls)
		if strings.Count(string(data), ":fail-workload\n") != 1 {
			t.Fatalf("uncertain workload was replayed: %s", data)
		}
	})
}
