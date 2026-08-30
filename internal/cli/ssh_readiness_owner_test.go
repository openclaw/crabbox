package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"testing/synctest"
	"time"
)

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
	if target.Port != "22" || len(target.FallbackPorts) != 0 {
		t.Fatalf("target.Port=%q FallbackPorts=%v, want pinned port 22 with no fallbacks", target.Port, target.FallbackPorts)
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
