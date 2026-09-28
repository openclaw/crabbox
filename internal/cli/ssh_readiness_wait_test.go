package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func readinessWaitFixture(t *testing.T, mode string) (SSHTarget, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake SSH runner")
	}
	dir := t.TempDir()
	for name, script := range map[string]string{
		"ssh": `#!/bin/sh
for remote; do :; done
kind=ready
case "$remote" in
  'exit 0') kind=transport ;;
  *'sleep 0.25'*) kind=wait ;;
esac
printf '%s\n' "$kind" >> "$CRABBOX_WAIT_CALLS"
if [ "$CRABBOX_WAIT_MODE" = transport-failure ]; then exit 255; fi
if [ "$kind" = transport ]; then exit 0; fi
if [ "$kind" = ready ]; then
  if [ "$CRABBOX_WAIT_MODE" = timeout ] && [ -f "$CRABBOX_WAIT_POLLS" ]; then exit 0; fi
  exit 1
fi
case "$CRABBOX_WAIT_MODE" in
  cancel) exec sleep 30 ;;
  timeout) touch "$CRABBOX_WAIT_POLLS"; exit 124 ;;
  host-key) printf 'Host key verification failed.\n' >&2; exit 255 ;;
esac
exec sh -c "$remote"
`,
		"timeout": "#!/bin/sh\n[ \"$1\" = 30s ] || exit 99\nshift\nexec \"$@\"\n",
		"fixture-ready": `#!/bin/sh
n=0
if [ -f "$CRABBOX_WAIT_POLLS" ]; then read n < "$CRABBOX_WAIT_POLLS"; fi
n=$((n + 1))
printf '%s\n' "$n" > "$CRABBOX_WAIT_POLLS"
[ "$n" -ge 3 ]
`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_WAIT_CALLS", filepath.Join(dir, "calls"))
	t.Setenv("CRABBOX_WAIT_POLLS", filepath.Join(dir, "polls"))
	t.Setenv("CRABBOX_WAIT_MODE", mode)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return SSHTarget{User: "runner", Host: host, Port: port, FallbackPorts: []string{}, TargetOS: targetLinux, ReadyCheck: "fixture-ready", NoControlMaster: true}, dir
}

func TestWaitForSSHReadyLinuxGuestWait(t *testing.T) {
	for _, mode := range []string{"ready", "timeout", "host-key", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			target, dir := readinessWaitFixture(t, mode)
			observations := &creationObservations{}
			ctx := context.WithValue(t.Context(), creationObservationsKey{}, observations)
			ctx, cancel := context.WithCancelCause(ctx)
			defer cancel(nil)
			cause := errors.New("stop guest wait")
			if mode == "cancel" {
				go func() {
					for ctx.Err() == nil {
						calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
						if strings.Contains(string(calls), "wait\n") {
							cancel(cause)
							return
						}
						time.Sleep(10 * time.Millisecond)
					}
				}()
			}
			var progress bytes.Buffer
			start := time.Now()
			err := waitForSSHReady(ctx, &target, &progress, "bootstrap", 2*time.Second)
			switch mode {
			case "host-key":
				if !errors.Is(err, errSSHHostKeyVerification) {
					t.Fatalf("host trust rejection lost: %v", err)
				}
			case "cancel":
				if !errors.Is(err, cause) || time.Since(start) > time.Second {
					t.Fatalf("cancellation took %s: %v", time.Since(start), err)
				}
			default:
				if err != nil || target.preparedEndpoint == "" {
					t.Fatalf("guest wait failed: %v endpoint=%q", err, target.preparedEndpoint)
				}
			}
			calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
			want := "ready\ntransport\nwait\n"
			if mode == "timeout" {
				want += "ready\n"
			}
			if string(calls) != want {
				t.Fatalf("SSH calls=%q, want %q", calls, want)
			}
			if !strings.Contains(progress.String(), "bootstrap ready-check") {
				t.Fatalf("missing wait progress: %s", &progress)
			}
			if len(observations.events) != 2 || observations.events[0].Phase != "ssh_tcp_accept" || observations.events[1].Phase != "ssh_authenticated" {
				t.Fatalf("observations=%+v", observations.events)
			}
			if mode == "ready" {
				polls, _ := os.ReadFile(filepath.Join(dir, "polls"))
				if string(polls) != "3\n" || time.Since(start) < 500*time.Millisecond {
					t.Fatalf("guest did not poll the predicate with sleeps: %q", polls)
				}
			}
		})
	}
}

func TestWaitForSSHReadyNonLinuxKeepsPolling(t *testing.T) {
	for _, osName := range []string{targetMacOS, targetWindows, targetLinux} {
		t.Run(osName, func(t *testing.T) {
			target, dir := readinessWaitFixture(t, "ready")
			target.TargetOS = osName
			target.SSHConfigProxy = osName == targetLinux
			err := waitForSSHReady(t.Context(), &target, io.Discard, "bootstrap", 350*time.Millisecond)
			if err == nil {
				t.Fatal("unready target passed readiness")
			}
			calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
			if strings.Contains(string(calls), "wait") {
				t.Fatalf("unsupported target received guest wait: %s", calls)
			}
		})
	}
}

func TestLinuxReadinessWaitDeadlineBoundsSSH(t *testing.T) {
	target, _ := readinessWaitFixture(t, "cancel")
	start := time.Now()
	ctx := t.Context()
	err := waitForLinuxReadiness(ctx, target, sshReadinessProfileForTarget(target), 100*time.Millisecond, func() {})
	if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil || time.Since(start) > time.Second {
		t.Fatalf("bounded wait took %s: err=%v parent=%v", time.Since(start), err, ctx.Err())
	}
}

func TestLinuxReadinessWaitCommandBoundsHangingPredicate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX guest command")
	}
	if _, err := exec.LookPath("timeout"); err != nil {
		t.Skip("Linux timeout utility unavailable on this test host")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	for _, predicate := range []string{"sleep 30", "set -e; false; exit 0"} {
		command := linuxReadinessWaitCommand(SSHTarget{ReadyCheck: predicate}, 100*time.Millisecond)
		err := exec.CommandContext(ctx, "sh", "-c", command).Run()
		if exitCode(err) != 124 || ctx.Err() != nil {
			t.Fatalf("remote deadline or predicate semantics failed for %q: %v parent=%v", predicate, err, ctx.Err())
		}
	}
}
