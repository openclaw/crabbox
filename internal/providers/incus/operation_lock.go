package incus

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

var incusOperationLocks sync.Map

type incusOperationSemaphore struct {
	token chan struct{}
}

func newIncusOperationSemaphore() *incusOperationSemaphore {
	semaphore := &incusOperationSemaphore{token: make(chan struct{}, 1)}
	semaphore.token <- struct{}{}
	return semaphore
}

func lockIncusLeaseOperation(ctx context.Context, leaseID, instanceName string) (func(), error) {
	identity := strings.TrimSpace(leaseID)
	if identity == "" {
		identity = strings.TrimSpace(instanceName)
	}
	if identity == "" || strings.ContainsAny(identity, `/\`) || filepath.Base(identity) != identity || identity == "." {
		return nil, core.Exit(2, "invalid Incus cleanup identity %q", identity)
	}
	stateDir, err := core.CrabboxStateDir()
	if err != nil {
		return nil, err
	}
	lockDir := filepath.Join(stateDir, "claim-locks")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, core.Exit(2, "create Incus operation lock directory: %v", err)
	}
	lockPath := filepath.Join(lockDir, identity+".incus-operation.lock")
	value, _ := incusOperationLocks.LoadOrStore(lockPath, newIncusOperationSemaphore())
	semaphore := value.(*incusOperationSemaphore)
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
		return nil, core.Exit(2, "Incus operation lock was not acquired for %s", identity)
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			_ = fileLock.Unlock()
			semaphore.token <- struct{}{}
		})
	}, nil
}
