package fal

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
	core "github.com/openclaw/crabbox/internal/cli"
)

var falOperationLocks sync.Map

type falOperationSemaphore struct {
	token chan struct{}
}

func newFalOperationSemaphore() *falOperationSemaphore {
	semaphore := &falOperationSemaphore{token: make(chan struct{}, 1)}
	semaphore.token <- struct{}{}
	return semaphore
}

func lockFalLeaseOperation(ctx context.Context, leaseID string) (func(), error) {
	if !strings.HasPrefix(leaseID, "cbx_") || strings.TrimPrefix(leaseID, "cbx_") == "" || filepath.Base(leaseID) != leaseID || leaseID == "." {
		return nil, exit(2, "invalid fal lease id %q", leaseID)
	}
	return lockFalOperation(ctx, leaseID+".fal-operation.lock", "fal lease "+leaseID)
}

func lockFalSlugAllocation(ctx context.Context) (func(), error) {
	return lockFalOperation(ctx, "fal-slug-allocation.lock", "fal slug allocation")
}

func lockFalOperation(ctx context.Context, lockName, description string) (func(), error) {
	stateDir, err := core.CrabboxStateDir()
	if err != nil {
		return nil, err
	}
	lockDir := filepath.Join(stateDir, "claim-locks")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, exit(2, "create fal lock directory: %v", err)
	}
	lockPath := filepath.Join(lockDir, lockName)
	value, _ := falOperationLocks.LoadOrStore(lockPath, newFalOperationSemaphore())
	semaphore := value.(*falOperationSemaphore)
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
		return nil, exit(2, "lock %s was not acquired", description)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = fileLock.Unlock()
			semaphore.token <- struct{}{}
		})
	}, nil
}
