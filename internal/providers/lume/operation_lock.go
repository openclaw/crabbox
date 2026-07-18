package lume

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"
	core "github.com/openclaw/crabbox/internal/cli"
)

var lumeCapacityLocks sync.Map

type lumeCapacitySemaphore struct {
	token chan struct{}
}

func newLumeCapacitySemaphore() *lumeCapacitySemaphore {
	semaphore := &lumeCapacitySemaphore{token: make(chan struct{}, 1)}
	semaphore.token <- struct{}{}
	return semaphore
}

func lockLumeCapacity(ctx context.Context) (func(), error) {
	stateDir, err := core.CrabboxStateDir()
	if err != nil {
		return nil, err
	}
	lockDir := filepath.Join(stateDir, "claim-locks")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, exit(2, "create Lume capacity lock directory: %v", err)
	}
	lockPath := filepath.Join(lockDir, "lume-macos-capacity.lock")
	value, _ := lumeCapacityLocks.LoadOrStore(lockPath, newLumeCapacitySemaphore())
	semaphore := value.(*lumeCapacitySemaphore)
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
		return nil, exit(2, "Lume macOS capacity lock was not acquired")
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			_ = fileLock.Unlock()
			semaphore.token <- struct{}{}
		})
	}, nil
}
