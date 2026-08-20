package shared

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"
	core "github.com/openclaw/crabbox/internal/cli"
)

var operationLocks sync.Map

type operationSemaphore struct {
	token chan struct{}
}

func newOperationSemaphore() *operationSemaphore {
	semaphore := &operationSemaphore{token: make(chan struct{}, 1)}
	semaphore.token <- struct{}{}
	return semaphore
}

// LockLeaseOperation serializes provider lease operations cross-process.
// providerName selects the same on-disk lock location the provider used
// before consolidation.
func LockLeaseOperation(ctx context.Context, providerName, leaseID string) (func(), error) {
	return LockOperation(
		ctx,
		providerName,
		leaseID+"."+providerName+"-operation.lock",
		providerName+" lease "+leaseID,
	)
}

// LockOperation serializes a provider operation using a named lock in the
// Crabbox claim-lock directory.
func LockOperation(ctx context.Context, providerName, lockName, description string) (func(), error) {
	stateDir, err := core.CrabboxStateDir()
	if err != nil {
		return nil, err
	}
	lockDir := filepath.Join(stateDir, "claim-locks")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, core.Exit(2, "create %s lock directory: %v", providerName, err)
	}
	return LockOperationFile(ctx, filepath.Join(lockDir, lockName), description)
}

// LockOperationFile serializes an operation using an already-prepared lock
// path. The caller owns creation and validation of the containing directory.
func LockOperationFile(ctx context.Context, lockPath, description string) (func(), error) {
	value, _ := operationLocks.LoadOrStore(lockPath, newOperationSemaphore())
	semaphore := value.(*operationSemaphore)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-semaphore.token:
	}

	fileLock := flock.New(lockPath, flock.SetPermissions(0o600))
	locked, err := fileLock.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil {
		semaphore.token <- struct{}{}
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		return nil, err
	}
	if !locked {
		semaphore.token <- struct{}{}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, core.Exit(2, "lock %s was not acquired", description)
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			_ = fileLock.Unlock()
			semaphore.token <- struct{}{}
		})
	}, nil
}
