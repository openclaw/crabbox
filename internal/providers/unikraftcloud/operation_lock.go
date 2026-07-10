package unikraftcloud

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

var unikraftCloudOperationLocks sync.Map

type unikraftCloudOperationSemaphore struct {
	token chan struct{}
}

func newUnikraftCloudOperationSemaphore() *unikraftCloudOperationSemaphore {
	semaphore := &unikraftCloudOperationSemaphore{token: make(chan struct{}, 1)}
	semaphore.token <- struct{}{}
	return semaphore
}

func lockUnikraftCloudLeaseOperation(ctx context.Context, leaseID string) (func(), error) {
	if !strings.HasPrefix(leaseID, leasePrefix) || strings.TrimPrefix(leaseID, leasePrefix) == "" ||
		strings.ContainsAny(leaseID, `/\`) || filepath.Base(leaseID) != leaseID || leaseID == "." {
		return nil, exit(2, "invalid unikraft-cloud lease id %q", leaseID)
	}
	return lockUnikraftCloudOperation(ctx, leaseID+".unikraft-cloud-operation.lock", "unikraft-cloud lease "+leaseID)
}

func lockUnikraftCloudSlugAllocation(ctx context.Context) (func(), error) {
	return lockUnikraftCloudOperation(ctx, "unikraft-cloud-slug-allocation.lock", "unikraft-cloud slug allocation")
}

func lockUnikraftCloudOperation(ctx context.Context, lockName, description string) (func(), error) {
	stateDir, err := core.CrabboxStateDir()
	if err != nil {
		return nil, err
	}
	lockDir := filepath.Join(stateDir, "claim-locks")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, exit(2, "create unikraft-cloud lock directory: %v", err)
	}
	lockPath := filepath.Join(lockDir, lockName)
	value, _ := unikraftCloudOperationLocks.LoadOrStore(lockPath, newUnikraftCloudOperationSemaphore())
	semaphore := value.(*unikraftCloudOperationSemaphore)
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
