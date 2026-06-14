package agentsandbox

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

var agentSandboxOperationLocks sync.Map

type agentSandboxOperationSemaphore struct {
	token chan struct{}
}

func newAgentSandboxOperationSemaphore() *agentSandboxOperationSemaphore {
	semaphore := &agentSandboxOperationSemaphore{token: make(chan struct{}, 1)}
	semaphore.token <- struct{}{}
	return semaphore
}

func lockAgentSandboxLeaseOperation(ctx context.Context, leaseID string) (func(), error) {
	if !strings.HasPrefix(leaseID, leasePrefix) || strings.TrimPrefix(leaseID, leasePrefix) == "" || filepath.Base(leaseID) != leaseID || leaseID == "." {
		return nil, exit(2, "invalid agent-sandbox lease id %q", leaseID)
	}
	return lockAgentSandboxOperationPath(ctx, leaseID+".agent-sandbox-operation.lock")
}

func lockAgentSandboxSlugAllocation(ctx context.Context, _ string) (func(), error) {
	return lockAgentSandboxOperationPath(ctx, "agent-sandbox-slug-allocation.lock")
}

func lockAgentSandboxOperationPath(ctx context.Context, name string) (func(), error) {
	stateDir, err := core.CrabboxStateDir()
	if err != nil {
		return nil, err
	}
	lockDir := filepath.Join(stateDir, "claim-locks")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, exit(2, "create agent-sandbox lock directory: %v", err)
	}
	lockPath := filepath.Join(lockDir, name)
	value, _ := agentSandboxOperationLocks.LoadOrStore(lockPath, newAgentSandboxOperationSemaphore())
	semaphore := value.(*agentSandboxOperationSemaphore)
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
		return nil, exit(2, "lock agent-sandbox operation %s was not acquired", name)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = fileLock.Unlock()
			semaphore.token <- struct{}{}
		})
	}, nil
}
