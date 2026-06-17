package cloudflaresandbox

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

var cloudflareSandboxOperationLocks sync.Map

type cloudflareSandboxOperationSemaphore struct {
	token chan struct{}
}

func newCloudflareSandboxOperationSemaphore() *cloudflareSandboxOperationSemaphore {
	semaphore := &cloudflareSandboxOperationSemaphore{token: make(chan struct{}, 1)}
	semaphore.token <- struct{}{}
	return semaphore
}

func lockCloudflareSandboxLeaseOperation(ctx context.Context, leaseID string) (func(), error) {
	if !strings.HasPrefix(leaseID, leasePrefix) || strings.TrimPrefix(leaseID, leasePrefix) == "" || filepath.Base(leaseID) != leaseID || leaseID == "." {
		return nil, exit(2, "invalid cloudflare-sandbox lease id %q", leaseID)
	}
	stateDir, err := core.CrabboxStateDir()
	if err != nil {
		return nil, err
	}
	lockDir := filepath.Join(stateDir, "claim-locks")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, exit(2, "create cloudflare-sandbox lock directory: %v", err)
	}
	lockPath := filepath.Join(lockDir, leaseID+".cloudflare-sandbox-operation.lock")
	value, _ := cloudflareSandboxOperationLocks.LoadOrStore(lockPath, newCloudflareSandboxOperationSemaphore())
	semaphore := value.(*cloudflareSandboxOperationSemaphore)
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
		return nil, exit(2, "lock cloudflare-sandbox lease %s was not acquired", leaseID)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = fileLock.Unlock()
			semaphore.token <- struct{}{}
		})
	}, nil
}
