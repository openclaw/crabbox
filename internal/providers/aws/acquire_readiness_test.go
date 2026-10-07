package aws

import (
	"context"
	"errors"
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
