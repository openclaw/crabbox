package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fixedAcquisitionTestBackend struct {
	*checkpointFixedForkBackend
	pause bool
}

func (b *fixedAcquisitionTestBackend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	lease, err := AcquireFixedLease(FixedAcquireOptions{
		Kind:    FixedLeaseKind{ClaimProvider: FixedAWSClaimProvider, IntentVersion: 1},
		LeaseID: req.RequestedLeaseID, CheckpointID: req.RequestedCheckpointID, RepoRoot: req.Repo.Root,
	}, func(context.Context, *LeaseClaim, bool) (FixedLeaseBinding, error) {
		return FixedLeaseBinding{ProviderScope: "test-account", Fingerprint: "test-intent", Slug: "fixed-acquisition"}, nil
	}, func(context.Context, *LeaseClaim, *FixedCreateIntent, func() error) (LeaseTarget, error) {
		return LeaseTarget{
			LeaseID: req.RequestedLeaseID,
			Server: Server{Provider: "aws", CloudID: "i-" + req.RequestedLeaseID, Labels: map[string]string{
				"lease": req.RequestedLeaseID, "slug": "fixed-acquisition", "state": "ready",
			}},
			SSH: SSHTarget{Host: "192.0.2.10", Port: "22", TargetOS: targetLinux},
		}, nil
	}, ctx)
	if err == nil && b.pause {
		// The provider claim is committed, but CLI registration has not run yet.
		fmt.Fprintln(os.Stdout, "provider-acquired")
		_, err = io.Copy(io.Discard, os.Stdin)
	}
	return lease, err
}

func TestFixedAcquisitionCommandHelper(t *testing.T) {
	command := os.Getenv("CRABBOX_TEST_FIXED_COMMAND")
	if command == "" {
		return
	}
	t.Setenv("XDG_STATE_HOME", os.Getenv("CRABBOX_TEST_FIXED_STATE"))
	backend := &fixedAcquisitionTestBackend{
		checkpointFixedForkBackend: &checkpointFixedForkBackend{checkpointForkReleaseBackend: &checkpointForkReleaseBackend{}},
		pause:                      os.Getenv("CRABBOX_TEST_FIXED_PAUSE") == "1",
	}
	testAWSBackendOverride = backend
	defer func() { testAWSBackendOverride = nil }()
	leaseID := os.Getenv("CRABBOX_TEST_FIXED_ID")
	args := []string{"warmup", "--provider", "aws", "--network", "public", "--lease-id", leaseID}
	if command == "fork" {
		args = []string{"checkpoint", "fork", "chk_command_lock", "--network", "public", "--lease-id", leaseID}
	}
	ctx := context.Background()
	waiter := os.Getenv("CRABBOX_TEST_FIXED_WAITER") == "1"
	if waiter {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 750*time.Millisecond)
		defer cancel()
	}
	err := (App{Stdout: os.Stdout, Stderr: os.Stderr}).Run(ctx, args)
	if waiter {
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("same-ID replay did not wait for CLI publication: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestFixedAcquisitionSerializesCLIReplayAcrossProcesses(t *testing.T) {
	for _, pair := range [][2]string{{"warmup", "warmup"}, {"fork", "fork"}, {"warmup", "fork"}} {
		t.Run(pair[0]+"-"+pair[1], func(t *testing.T) {
			clearConfigEnv(t)
			isolateRunTestUserDirs(t, t.TempDir())
			t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
			createCheckpointForkTestRecord(t, "chk_command_lock", "")
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			child := func(command, id, pause, waiter string) *exec.Cmd {
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestFixedAcquisitionCommandHelper$")
				cmd.Env = append(os.Environ(), "CRABBOX_TEST_FIXED_COMMAND="+command, "CRABBOX_TEST_FIXED_ID="+id,
					"CRABBOX_TEST_FIXED_PAUSE="+pause, "CRABBOX_TEST_FIXED_WAITER="+waiter,
					"CRABBOX_TEST_FIXED_STATE="+os.Getenv("XDG_STATE_HOME"))
				return cmd
			}
			const leaseID = "cbx_123456abcdef"
			first := child(pair[0], leaseID, "1", "0")
			input, err := first.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			output, err := first.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			first.Stderr = &stderr
			if err := first.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				_ = input.Close()
				_, _ = io.Copy(io.Discard, output)
				_ = first.Wait()
			}()
			reader := bufio.NewReader(output)
			line, err := reader.ReadString('\n')
			if err != nil || line != "provider-acquired\n" {
				t.Fatalf("first acquisition boundary: %q (%v)", line, err)
			}
			if output, err := child(pair[1], leaseID, "0", "1").CombinedOutput(); err != nil {
				t.Fatalf("overlapping replay: %v\n%s", err, output)
			}
			if output, err := child(pair[1], "cbx_654321abcdef", "0", "0").CombinedOutput(); err != nil {
				t.Fatalf("independent ID was blocked: %v\n%s", err, output)
			}
			_ = input.Close()
			remaining, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			if err := first.Wait(); err != nil {
				t.Fatalf("first acquisition: %v\n%s\n%s", err, remaining, stderr.String())
			}
			if output, err := child(pair[0], leaseID, "0", "0").CombinedOutput(); err != nil {
				t.Fatalf("replay after publication: %v\n%s", err, output)
			}
		})
	}
}

func TestWarmupFailedFixedReplayPreservesLease(t *testing.T) {
	clearConfigEnv(t)
	isolateRunTestUserDirs(t, t.TempDir())
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	backend := &checkpointFixedForkBackend{checkpointForkReleaseBackend: &checkpointForkReleaseBackend{}}
	testAWSBackendOverride = backend
	t.Cleanup(func() { testAWSBackendOverride = nil })
	const leaseID = "cbx_123456abcdef"
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	if err := app.Run(context.Background(), []string{"warmup", "--provider", "aws", "--network", "public", "--lease-id", leaseID}); err != nil {
		t.Fatal(err)
	}
	err := app.Run(context.Background(), []string{"warmup", "--provider", "aws", "--network", "tailscale", "--lease-id", leaseID})
	if err == nil || !strings.Contains(err.Error(), "no tailnet address") {
		t.Fatalf("expected post-acquisition network failure: %v", err)
	}
	if backend.creates != 1 || backend.releaseCount != 0 {
		t.Fatalf("fixed replay created=%d released=%d, want 1/0", backend.creates, backend.releaseCount)
	}
	if _, exists, err := readLeaseClaimWithPresence(leaseID); err != nil || !exists {
		t.Fatalf("fixed replay lost its claim: exists=%t err=%v", exists, err)
	}
}
