package opensandbox

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

var openSandboxOperationLocks sync.Map

type openSandboxOperationSemaphore struct {
	token chan struct{}
}

func newOpenSandboxOperationSemaphore() *openSandboxOperationSemaphore {
	semaphore := &openSandboxOperationSemaphore{token: make(chan struct{}, 1)}
	semaphore.token <- struct{}{}
	return semaphore
}

func lockOpenSandboxLeaseOperation(ctx context.Context, leaseID string) (func(), error) {
	validLeaseID := strings.HasPrefix(leaseID, leasePrefix) && strings.TrimPrefix(leaseID, leasePrefix) != ""
	validRecoveryID := strings.HasPrefix(leaseID, recoveryPrefix) && strings.TrimPrefix(leaseID, recoveryPrefix) != ""
	if (!validLeaseID && !validRecoveryID) || filepath.Base(leaseID) != leaseID || leaseID == "." {
		return nil, exit(2, "invalid opensandbox lease id %q", leaseID)
	}
	stateDir, err := core.CrabboxStateDir()
	if err != nil {
		return nil, err
	}
	lockDir := filepath.Join(stateDir, "claim-locks")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, exit(2, "create opensandbox lock directory: %v", err)
	}
	lockPath := filepath.Join(lockDir, leaseID+".opensandbox-operation.lock")
	value, _ := openSandboxOperationLocks.LoadOrStore(lockPath, newOpenSandboxOperationSemaphore())
	semaphore := value.(*openSandboxOperationSemaphore)
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
		return nil, exit(2, "lock opensandbox lease %s was not acquired", leaseID)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = fileLock.Unlock()
			semaphore.token <- struct{}{}
		})
	}, nil
}
