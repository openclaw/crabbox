package aws

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func TestAWSAcquireReadinessMonitor(t *testing.T) {
	for _, terminal := range []bool{true, false} {
		name := "healthy slow bootstrap"
		if terminal {
			name = "reclaim during bootstrap"
		}
		t.Run(name, func(t *testing.T) {
			var polls atomic.Int32
			stopped := &core.AWSAcquireStateError{InstanceID: "i-test", State: "terminated", Reason: "Server.SpotInstanceTermination"}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			err := waitAWSAcquireReadyEvery(ctx, func(context.Context) error {
				if polls.Add(1) >= 2 && terminal {
					return stopped
				}
				return nil
			}, func(ctx context.Context) error {
				if terminal {
					<-ctx.Done()
					return ctx.Err()
				}
				time.Sleep(10 * time.Millisecond)
				return nil
			}, time.Millisecond)
			if terminal && !errors.Is(err, stopped) {
				t.Fatalf("error=%v, want terminal cause", err)
			}
			if !terminal && err != nil {
				t.Fatal(err)
			}
			if polls.Load() < 2 {
				t.Fatalf("readiness did not poll: %d", polls.Load())
			}
			count := polls.Load()
			time.Sleep(5 * time.Millisecond)
			if polls.Load() != count {
				t.Fatal("monitor survived the wait")
			}
		})
	}
}

func TestAWSAcquireTerminalBootstrapRollsBack(t *testing.T) {
	for _, targetOS := range []string{"linux", "windows"} {
		t.Run(targetOS, func(t *testing.T) {
			testutil.IsolateUserDirs(t)
			stopped := &core.AWSAcquireStateError{InstanceID: "i-created", State: "terminated", Reason: "Server.SpotInstanceTermination"}
			fake := &fakeAWSClient{acquireStateErr: stopped}
			oldClient := newAWSClient
			newAWSClient = func(context.Context, core.Config) (awsClient, error) { return fake, nil }
			t.Cleanup(func() { newAWSClient = oldClient })
			backend := NewAWSLeaseBackend(core.ProviderSpec{}, core.Config{Provider: "aws", AWSRegion: "us-east-1", TargetOS: targetOS}, core.Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
			_, err := backend.acquireOnce(t.Context(), false, "")
			if !errors.Is(err, stopped) {
				t.Fatalf("error=%v, want reclaim", err)
			}
			if len(fake.deletedInstances) != 1 || fake.deletedInstances[0] != "i-created" || len(fake.deletedKeys) != 1 {
				t.Fatalf("cleanup instances=%v keys=%v", fake.deletedInstances, fake.deletedKeys)
			}
			if len(fake.tagged) != 0 {
				t.Fatal("terminated instance tagged ready")
			}
		})
	}
}

func TestAWSFixedCheckpointInterruptedBootstrapResumesSameInstance(t *testing.T) {
	for _, cancellation := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "cancellation"}[cancellation], func(t *testing.T) {
			testutil.IsolateUserDirs(t)
			fake := &fakeAWSClient{}
			t.Cleanup(installFixedAWSTestClient(t, fake))
			backend := NewAWSLeaseBackend(core.ProviderSpec{}, fixedAWSTestConfig(), core.Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
			req := core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, Keep: true, RequestedLeaseID: "cbx_abcdef123456", RequestedCheckpointID: "chk_resume", RequestedSlug: "resume-test"}
			interrupted := error(core.Exit(5, "checkpoint readiness state=pending"))
			if cancellation {
				interrupted = context.Canceled
			}
			bootstrapAWSWindowsDesktop = func(context.Context, core.Config, *core.SSHTarget, string, io.Writer) error { return interrupted }
			if _, err := backend.Acquire(t.Context(), req); !errors.Is(err, interrupted) {
				t.Fatalf("initial wait: %v", err)
			}
			if fake.createCalls != 1 || len(fake.deletedInstances) != 0 || len(fake.tagged) != 0 {
				t.Fatalf("unready allocation: creates=%d deletes=%v tags=%v", fake.createCalls, fake.deletedInstances, fake.tagged)
			}
			claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
			if err != nil || claim.CloudID == "" || claim.FixedCreateIntent.CheckpointID != req.RequestedCheckpointID {
				t.Fatalf("identity lost: %+v %v", claim, err)
			}
			bootstrapAWSWindowsDesktop = func(context.Context, core.Config, *core.SSHTarget, string, io.Writer) error { return nil }
			lease, err := backend.Acquire(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			if fake.createCalls != 1 || lease.Server.CloudID != claim.CloudID || lease.LeaseID != req.RequestedLeaseID || len(fake.deletedInstances) != 0 {
				t.Fatalf("replaced lease: %+v creates=%d", lease, fake.createCalls)
			}
		})
	}
}

func TestAWSAcquireReadinessPreservesStoppedWaitDiagnostic(t *testing.T) {
	for _, tc := range []struct {
		name              string
		parent, unrelated bool
	}{
		{name: "caller cancellation", parent: true},
		{name: "lifecycle cancellation"},
		{name: "lifecycle wins over unrelated wait error", unrelated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			cause := errors.New("authoritative stop")
			var polls atomic.Int32
			var diagnostic error
			err := waitAWSAcquireReadyEvery(ctx, func(context.Context) error {
				if polls.Add(1) >= 2 && !tc.parent {
					return cause
				}
				return nil
			}, func(ctx context.Context) error {
				if tc.parent {
					cancel(cause)
				}
				<-ctx.Done()
				diagnostic = fmt.Errorf("checkpoint readiness stopped lease=cbx_012345abcdef; fixed identity retained: %w", context.Cause(ctx))
				if tc.unrelated {
					return errors.New("independent readiness failure")
				}
				return diagnostic
			}, time.Millisecond)
			if !errors.Is(err, cause) {
				t.Fatalf("stop cause lost: %v", err)
			}
			if !tc.unrelated && err != diagnostic {
				t.Fatalf("wait diagnostic lost: %v", err)
			}
			if tc.unrelated && err != cause {
				t.Fatalf("live authority masked: %v", err)
			}
		})
	}
}
