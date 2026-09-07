package cli

import (
	"bytes"
	"context"
	"errors"
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

func TestWaitForSSHReadyOnlyDiagnosesFailedReadiness(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell ssh fixture")
	}
	for _, test := range []struct {
		name, readyExit, transportExit, wantCalls, wantProgress string
	}{
		{name: "ready", readyExit: "0", transportExit: "0", wantCalls: "fixture-ready\n"},
		{name: "toolchain pending", readyExit: "1", transportExit: "0", wantCalls: "fixture-ready\nexit 0\n", wantProgress: "test ready-check"},
		{name: "transport pending", readyExit: "255", transportExit: "255", wantCalls: "fixture-ready\nexit 0\n", wantProgress: "test ssh-transport"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			callsPath := filepath.Join(dir, "calls")
			script := `#!/bin/sh
for remote; do :; done
printf '%s\n' "$remote" >> "$CRABBOX_FAKE_SSH_OWNER_CALLS"
if [ "$remote" = fixture-ready ]; then exit "$CRABBOX_FAKE_READY_EXIT"; fi
exit "$CRABBOX_FAKE_TRANSPORT_EXIT"
`
			if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("CRABBOX_FAKE_SSH_OWNER_CALLS", callsPath)
			t.Setenv("CRABBOX_FAKE_READY_EXIT", test.readyExit)
			t.Setenv("CRABBOX_FAKE_TRANSPORT_EXIT", test.transportExit)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			host, port, err := net.SplitHostPort(listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			target := SSHTarget{User: "runner", Host: host, Port: port, FallbackPorts: []string{}, ReadyCheck: "fixture-ready"}
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			var progress bytes.Buffer
			signal := &sshWaitProgressSignal{ready: make(chan struct{})}
			done := make(chan error, 1)
			go func() {
				done <- waitForSSHReady(ctx, &target, io.MultiWriter(&progress, signal), "test", 5*time.Second)
			}()
			if test.wantProgress != "" {
				select {
				case <-signal.ready:
				case err := <-done:
					t.Fatalf("readiness returned before reporting failure: %v", err)
				}
				cancel(context.Canceled)
			}
			err = <-done
			if test.wantProgress == "" && err != nil || test.wantProgress != "" && !errors.Is(err, context.Canceled) {
				t.Fatalf("readiness: %v", err)
			}
			if !strings.Contains(progress.String(), test.wantProgress) {
				t.Fatalf("progress=%q, want %q", progress.String(), test.wantProgress)
			}
			calls, err := os.ReadFile(callsPath)
			if err != nil || string(calls) != test.wantCalls {
				t.Fatalf("SSH calls=%q error=%v, want %q", calls, err, test.wantCalls)
			}
		})
	}
}

func TestWaitForSSHReadyDeadlineStopsAtActiveProbe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell ssh fixture")
	}
	for _, test := range []struct {
		name, route, blocked, wantProbe, wantCalls string
	}{
		{"direct readiness", "direct", "readiness", "readiness", "readiness\n"},
		{"direct transport", "direct", "transport", "transport", "readiness\ntransport\n"},
		{"proxy readiness", "proxy", "readiness", "readiness", "readiness\n"},
		{"WSL transport", "wsl", "transport", "transport", "transport\n"},
		{"WSL SFTP", "wsl", "sftp", "transport", "transport\n"},
		{"WSL readiness", "wsl", "readiness", "readiness", "transport\nreadiness\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			callsPath := filepath.Join(dir, "calls")
			script := `#!/bin/sh
for remote; do :; done
kind=transport
if [ "$remote" = "$CRABBOX_FAKE_READY_COMMAND" ]; then kind=readiness; fi
printf '%s\n' "$kind" >> "$CRABBOX_FAKE_SSH_OWNER_CALLS"
printf 'private-readiness-stderr\n' >&2
if [ "$kind" = "$CRABBOX_FAKE_BLOCKED_PROBE" ]; then exec sleep 30; fi
if [ "$kind" = readiness ]; then exit 1; fi
exit 0
`
			if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("CRABBOX_FAKE_SSH_OWNER_CALLS", callsPath)
			t.Setenv("CRABBOX_FAKE_BLOCKED_PROBE", test.blocked)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			host, port, err := net.SplitHostPort(listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			fallback, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			_, fallbackPort, err := net.SplitHostPort(fallback.Addr().String())
			if err != nil {
				fallback.Close()
				t.Fatal(err)
			}
			dialed := make(chan bool, 1)
			go func() {
				conn, err := fallback.Accept()
				if err == nil {
					conn.Close()
				}
				dialed <- err == nil
			}()
			defer fallback.Close()
			target := SSHTarget{
				User: "runner", Host: host, Port: port, FallbackPorts: []string{fallbackPort},
				ReadyCheck: "fixture-ready", NoControlMaster: true,
			}
			readyCommand := target.ReadyCheck
			sftpCalls := 0
			switch test.route {
			case "proxy":
				target.SSHConfigProxy = true
			case "wsl":
				target.TargetOS, target.WindowsMode = targetWindows, windowsModeWSL2
				readyCommand = wsl2ReadinessCommand(target.ReadyCheck)
				original := probeWSLSFTPSubsystem
				probeWSLSFTPSubsystem = func(ctx context.Context, _ SSHTarget, _, _ string, _ io.Writer) error {
					sftpCalls++
					if test.blocked == "sftp" {
						<-ctx.Done()
						return ctx.Err()
					}
					return nil
				}
				t.Cleanup(func() { probeWSLSFTPSubsystem = original })
			}
			t.Setenv("CRABBOX_FAKE_READY_COMMAND", readyCommand)
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			var progress bytes.Buffer
			err = waitForSSHReady(ctx, &target, &progress, "before sync", 250*time.Millisecond)
			if deadlineErr := fallback.(*net.TCPListener).SetDeadline(time.Now().Add(10 * time.Millisecond)); deadlineErr != nil {
				t.Error(deadlineErr)
				fallback.Close()
			}
			if <-dialed {
				t.Error("dialed a fallback after the active probe exhausted the deadline")
			}
			fallback.Close()
			var exitErr ExitError
			if !AsExitError(err, &exitErr) || exitErr.Code != 5 || !isBootstrapWaitError(err) {
				t.Fatalf("readiness=%v, want retry-classified exit 5", err)
			}
			for _, want := range []string{
				"timed out waiting for SSH", "probe=" + test.wantProbe,
				"cause=deadline_exceeded", "authentication=unknown",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error=%q, missing %q", err, want)
				}
			}
			for _, forbidden := range []string{"ssh-auth", "private-readiness-stderr", "fixture-ready", port + ":tcp"} {
				if strings.Contains(err.Error()+progress.String(), forbidden) {
					t.Errorf("deadline misclassification or disclosure: %q", forbidden)
				}
			}
			calls, readErr := os.ReadFile(callsPath)
			if readErr != nil || string(calls) != test.wantCalls {
				t.Errorf("SSH calls=%q error=%v, want %q", calls, readErr, test.wantCalls)
			}
			wantSFTP := 0
			if test.route == "wsl" && test.blocked != "transport" {
				wantSFTP = 1
			}
			if sftpCalls != wantSFTP || target.Port != port || target.preparedEndpoint != "" || ctx.Err() != nil {
				t.Errorf("sftp=%d target=%+v parent=%v, want unchanged target and live parent", sftpCalls, target, ctx.Err())
			}
		})
	}
}

func TestWaitForSSHReadyLocalDeadlineDuringBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		target := SSHTarget{Host: "ssh-readiness.example", FallbackPorts: []string{}}
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(nil)
		var progress bytes.Buffer
		start := time.Now()
		err := waitForSSHReady(ctx, &target, &progress, "test", time.Second)
		var exitErr ExitError
		if !AsExitError(err, &exitErr) || exitErr.Code != 5 || !isBootstrapWaitError(err) || ctx.Err() != nil {
			t.Fatalf("readiness=%v parent=%v, want exit 5 with an uncancelled parent", err, ctx.Err())
		}
		for _, want := range []string{"probe=transport", "cause=deadline_exceeded", "authentication=unknown"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error=%q missing %q", err, want)
			}
		}
		if elapsed := time.Since(start); elapsed != time.Second {
			t.Errorf("backoff consumed %s, want the one-second local deadline", elapsed)
		}
	})
}

func TestWaitForSSHReadyCanceledParentWinsDeadline(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	cause := errors.New("fixture lease authority revoked")
	cancel(cause)
	target := SSHTarget{Host: "ssh-readiness.example", FallbackPorts: []string{}}
	var progress bytes.Buffer
	err := waitForSSHReady(ctx, &target, &progress, "test", 0)
	if err != cause || progress.Len() != 0 {
		t.Fatalf("error=%v progress=%q, want original cancellation and no probe progress", err, progress.String())
	}
}

func TestResolveSSHPortNoInputPreservesOrderedFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell ssh fixture")
	}
	dir := t.TempDir()
	callsPath := filepath.Join(dir, "calls")
	script := `#!/bin/sh
port=""
no_input=""
remote=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -p) shift; port="$1" ;;
    -n) no_input="$1" ;;
  esac
  remote="$1"
  shift
done
printf '%s:%s:%s\n' "$port" "$no_input" "$remote" >> "$CRABBOX_FAKE_SSH_OWNER_CALLS"
case "$port" in
  2222) exit 255 ;;
  22) exit 0 ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_FAKE_SSH_OWNER_CALLS", callsPath)

	target := SSHTarget{
		User:          "crabbox",
		Host:          "ssh-readiness.example",
		Port:          "2222",
		FallbackPorts: []string{"22"},
	}
	if err := resolveSSHPortNoInput(t.Context(), &target, "5", "1", io.Discard); err != nil {
		t.Fatalf("resolveSSHPortNoInput: %v", err)
	}
	calls, err := os.ReadFile(callsPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(calls), "2222:-n:exit 0\n22:-n:exit 0\n"; got != want {
		t.Fatalf("SSH calls=%q, want %q", got, want)
	}
	if target.Port != "22" {
		t.Fatalf("target.Port=%q FallbackPorts=%v, want resolved port 22", target.Port, target.FallbackPorts)
	}
	if err := resolveSSHPortNoInput(t.Context(), &target, "5", "1", io.Discard); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(callsPath)
	if err != nil || string(after) != string(calls) {
		t.Fatalf("prepared target was probed again: %s error=%v", after, err)
	}
}

func TestWaitForSSHReadyBackoffCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Exercise the wait/backoff owner with explicitly empty candidates:
		// unlike nil fallbacks, these reach progress without network or SSH calls.
		target := SSHTarget{
			Host:          "ssh-readiness.example",
			Port:          "",
			FallbackPorts: []string{},
		}
		cause := errors.New("readiness canceled during backoff")
		ctx, cancel := context.WithCancelCause(t.Context())
		progress := &sshWaitProgressSignal{ready: make(chan struct{})}
		errCh := make(chan error, 1)
		done := make(chan struct{})
		start := time.Now()
		go func() {
			defer close(done)
			errCh <- waitForSSHReady(ctx, &target, progress, "test", time.Minute)
		}()
		defer func() {
			cancel(nil)
			<-done
		}()

		synctest.Wait()
		select {
		case <-progress.ready:
		default:
			t.Fatal("waitForSSHReady did not report progress before backoff")
		}
		select {
		case err := <-errCh:
			t.Fatalf("waitForSSHReady returned before cancellation: %v", err)
		default:
		}

		cancel(cause)
		synctest.Wait()
		select {
		case err := <-errCh:
			if !errors.Is(err, cause) {
				t.Fatalf("waitForSSHReady returned %v, want cancellation cause %v", err, cause)
			}
		default:
			t.Fatal("waitForSSHReady remained blocked after cancellation")
		}
		if elapsed := time.Since(start); elapsed != 0 {
			t.Fatalf("fake clock advanced by %v during backoff cancellation, want 0", elapsed)
		}
	})
}
