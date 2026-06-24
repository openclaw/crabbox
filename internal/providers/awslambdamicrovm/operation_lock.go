package awslambdamicrovm

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

const leasePrefix = "cbx_"

var awsLambdaMicroVMOperationLocks sync.Map

type awsLambdaMicroVMOperationSemaphore struct {
	token chan struct{}
}

func newAWSLambdaMicroVMOperationSemaphore() *awsLambdaMicroVMOperationSemaphore {
	semaphore := &awsLambdaMicroVMOperationSemaphore{token: make(chan struct{}, 1)}
	semaphore.token <- struct{}{}
	return semaphore
}

func lockAWSLambdaMicroVMLeaseOperation(ctx context.Context, leaseID string) (func(), error) {
	if !strings.HasPrefix(leaseID, leasePrefix) || strings.TrimPrefix(leaseID, leasePrefix) == "" ||
		filepath.Base(leaseID) != leaseID || leaseID == "." {
		return nil, exit(2, "invalid aws-lambda-microvm lease id %q", leaseID)
	}
	stateDir, err := core.CrabboxStateDir()
	if err != nil {
		return nil, err
	}
	lockDir := filepath.Join(stateDir, "claim-locks")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, exit(2, "create aws-lambda-microvm lock directory: %v", err)
	}
	lockPath := filepath.Join(lockDir, leaseID+".aws-lambda-microvm-operation.lock")
	value, _ := awsLambdaMicroVMOperationLocks.LoadOrStore(lockPath, newAWSLambdaMicroVMOperationSemaphore())
	semaphore := value.(*awsLambdaMicroVMOperationSemaphore)
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
		return nil, exit(2, "lock aws-lambda-microvm lease %s was not acquired", leaseID)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = fileLock.Unlock()
			semaphore.token <- struct{}{}
		})
	}, nil
}
