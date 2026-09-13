package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestSleepContextHonorsCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := sleepContext(ctx, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("sleepContext returned %v, want context.Canceled", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("sleepContext took %v; expected immediate return on cancel", time.Since(start))
	}
}

func TestSleepContextPreservesContextError(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errors.New("provider cancellation detail"))
	if err := SleepContext(ctx, time.Hour); err != context.Canceled {
		t.Fatalf("SleepContext returned %v, want context.Canceled", err)
	}
}

func TestSleepContextCompletesWhenContextStaysLive(t *testing.T) {
	t.Parallel()
	start := time.Now()
	err := sleepContext(context.Background(), 20*time.Millisecond)
	if err != nil {
		t.Fatalf("sleepContext returned %v, want nil", err)
	}
	if time.Since(start) < 15*time.Millisecond {
		t.Fatalf("sleepContext returned too early: %v", time.Since(start))
	}
}

func TestWaitForLoopbackVNCHonorsCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := waitForLoopbackVNC(ctx, &SSHTarget{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForLoopbackVNC returned %v, want context.Canceled", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("waitForLoopbackVNC took %v; expected immediate return on cancel", time.Since(start))
	}
}

func TestResolveVNCEndpointStaticHonorsCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := resolveVNCEndpoint(ctx, Config{Provider: staticProvider}, &SSHTarget{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("resolveVNCEndpoint returned %v, want context.Canceled", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("resolveVNCEndpoint took %v; expected immediate return on cancel", time.Since(start))
	}
}

func TestWaitForManagedWindowsLoopbackVNCHonorsCancellationDuringBackoff(t *testing.T) {
	// Prove cancel is observed during the inter-attempt backoff, not only at
	// the top of the loop. Fake ssh always fails so wait enters the sleep.
	if runtime.GOOS == "windows" {
		t.Skip("shell ssh fixture")
	}
	dir := t.TempDir()
	sshPath := filepath.Join(dir, "ssh")
	probesPath := filepath.Join(dir, "probes")
	script := `#!/bin/sh
printf 'probe\n' >> "$CRABBOX_FAKE_SSH_PROBES"
exit 255
`
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_FAKE_SSH_PROBES", probesPath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- waitForManagedWindowsLoopbackVNC(ctx, &SSHTarget{
			User: "crabbox",
			Host: "203.0.113.10",
			Port: "22",
		}, io.Discard, time.Minute)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(probesPath); err == nil {
			break
		}
		select {
		case err := <-errCh:
			t.Fatalf("waitForManagedWindowsLoopbackVNC returned before backoff: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("waitForManagedWindowsLoopbackVNC did not probe before backoff")
		}
	}
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waitForManagedWindowsLoopbackVNC returned %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waitForManagedWindowsLoopbackVNC did not return within 3s after cancel; still blocked on bare sleep")
	}
}
