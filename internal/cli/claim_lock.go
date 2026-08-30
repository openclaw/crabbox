package cli

import (
	"context"
	"time"

	"github.com/gofrs/flock"
)

const claimLockRetryDelay = 10 * time.Millisecond

func withLeaseClaimLockContext(ctx context.Context, path string, shared bool, action func() error) error {
	lockPath, err := leaseClaimLockPath(path)
	if err != nil {
		return err
	}
	if !shared {
		mu := claimMutationMutex(lockPath)
		for !mu.TryLock() {
			timer := time.NewTimer(claimLockRetryDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		defer mu.Unlock()
	}

	// Every reader owns an independent file handle. Writers use nonblocking
	// retries so a queued writer cannot prevent a cancellation reader from
	// joining the active command's shared fence (as sync.RWMutex would).
	lock := flock.New(lockPath, flock.SetPermissions(0o600))
	acquire := lock.TryLockContext
	if shared {
		acquire = lock.TryRLockContext
	}
	if _, err := acquire(ctx, claimLockRetryDelay); err != nil {
		return err
	}
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return action()
}
