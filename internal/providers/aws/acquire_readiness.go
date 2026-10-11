package aws

import (
	"context"
	"time"
)

// Monitor the provider while SSH/Windows bootstrap owns the foreground wait.
// Join the monitor before returning so no API call outlives the acquisition.
func waitAWSAcquireReady(ctx context.Context, check func(context.Context) error, wait func(context.Context) error) error {
	return waitAWSAcquireReadyEvery(ctx, check, wait, 15*time.Second)
}

func waitAWSAcquireReadyEvery(ctx context.Context, check func(context.Context) error, wait func(context.Context) error, interval time.Duration) error {
	if err := check(ctx); err != nil {
		return err
	}
	waitCtx, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-waitCtx.Done():
				return
			case <-ticker.C:
				if err := check(waitCtx); err != nil {
					cancel(err)
					return
				}
			}
		}
	}()
	err := wait(waitCtx)
	cause := context.Cause(waitCtx)
	cancel(context.Canceled)
	<-done
	if cause != nil {
		return cause
	}
	if err != nil {
		return err
	}
	return check(ctx)
}
