package azuresandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

// The OS lock is a transient cancellation signal, not deletion authority.
// Binding it to the entire claim snapshot prevents a stale stop from cancelling
// a new repository owner's activity. A crashed stop releases the signal; once
// deletion is admitted, the durable claim journal independently blocks activity.
func activityStopPath(claim core.LeaseClaim) (string, error) {
	fingerprint, err := core.FixedIntentFingerprint("aca-sandbox-stop-v1", claim)
	if err != nil {
		return "", err
	}
	root, err := core.CrabboxStateDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, "claim-locks")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	return filepath.Join(dir, claim.LeaseID+"."+fingerprint+".azure-sandbox-stop.lock"), nil
}

func stopRequested() error {
	return fmt.Errorf("ACA Sandbox stop requested; remote activity remains uncertain until cleanup: %w", context.Canceled)
}

func withActivity(ctx context.Context, claim core.LeaseClaim, action func(context.Context) error) error {
	return core.WithLeaseClaimUnchangedShared(ctx, claim.LeaseID, claim, func() error {
		path, err := activityStopPath(claim)
		if err != nil {
			return err
		}
		probe := flock.New(path, flock.SetPermissions(0600))
		defer probe.Close()
		check := func() error {
			locked, err := probe.TryRLock()
			if err != nil {
				return err
			}
			if !locked {
				return stopRequested()
			}
			return probe.Unlock()
		}
		if err := check(); err != nil {
			return err
		}
		ctx, cancel := context.WithCancelCause(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := check(); err != nil {
						cancel(err)
						return
					}
				}
			}
		}()
		defer func() { cancel(nil); <-done }()
		err = action(ctx)
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		return err
	})
}

func (b *backend) interruptActivity(ctx context.Context, claim core.LeaseClaim, repoRoot string) (func(), error) {
	// Validate before signalling, but never wait for another stop's signal lock
	// while holding a claim reader: that stop needs the exclusive claim fence.
	err := core.WithLeaseClaimUnchangedShared(ctx, claim.LeaseID, claim, func() error {
		if repoRoot != "" {
			if claim.RepoRoot == "" {
				return core.Exit(4, "ACA Sandbox fixed lease %s has no current repository owner", claim.LeaseID)
			}
			if err := core.CheckLeaseClaimRepositoryOwner(claim.LeaseID, claim, repoRoot, false); err != nil {
				return err
			}
		}
		_, err := core.InspectFixedResource(ctx, leaseKind, claim, core.FixedLeaseOperations[sandbox]{ObserveExact: b.observe})
		return err
	})
	if err != nil {
		return nil, err
	}
	path, err := activityStopPath(claim)
	if err != nil {
		return nil, err
	}
	return shared.LockOperationFile(ctx, path, "ACA Sandbox stop")
}
