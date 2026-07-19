package githubcodespaces

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"
	core "github.com/openclaw/crabbox/internal/cli"
)

var githubCodespacesOperationLocks sync.Map

var ensureGitHubCodespacesClaimNamespace = func(string) error {
	return core.EnsureCrabboxClaimNamespaceDurable()
}

type githubCodespacesOperationSemaphore struct {
	token chan struct{}
}

func newGitHubCodespacesOperationSemaphore() *githubCodespacesOperationSemaphore {
	semaphore := &githubCodespacesOperationSemaphore{token: make(chan struct{}, 1)}
	semaphore.token <- struct{}{}
	return semaphore
}

func lockGitHubCodespacesLeaseOperation(ctx context.Context, leaseID string) (func(), error) {
	if !core.IsCanonicalLeaseID(leaseID) {
		return nil, exit(2, "invalid github-codespaces lease id %q", leaseID)
	}
	return lockGitHubCodespacesOperation(
		ctx,
		leaseID+".github-codespaces-operation.lock",
		"github-codespaces lease "+leaseID,
	)
}

func lockGitHubCodespacesSlugAllocation(ctx context.Context) (func(), error) {
	return lockGitHubCodespacesOperation(
		ctx,
		"github-codespaces-slug-allocation.lock",
		"github-codespaces slug allocation",
	)
}

func lockGitHubCodespacesOperation(ctx context.Context, lockName, description string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stateDir, err := core.CrabboxStateDir()
	if err != nil {
		return nil, err
	}
	stateDir = filepath.Clean(stateDir)
	if !filepath.IsAbs(stateDir) {
		return nil, exit(2, "github-codespaces state directory must be absolute: %s", stateDir)
	}
	if err := ensureGitHubCodespacesClaimNamespace(stateDir); err != nil {
		return nil, exit(2, "create github-codespaces claim namespace: %v", err)
	}
	lockDir := filepath.Join(stateDir, "claim-locks")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, exit(2, "create github-codespaces lock directory: %v", err)
	}
	lockPath := filepath.Join(lockDir, lockName)
	value, _ := githubCodespacesOperationLocks.LoadOrStore(lockPath, newGitHubCodespacesOperationSemaphore())
	semaphore := value.(*githubCodespacesOperationSemaphore)
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
