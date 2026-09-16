package cli

import (
	"context"
	"time"
)

// SleepContext waits for delay and preserves ctx.Err as the cancellation result.
func SleepContext(ctx context.Context, delay time.Duration) error {
	return sleepContext(ctx, delay)
}

// sleepContext waits for delay or returns early when ctx is cancelled.
// Matches the pattern used by GCP/Azure WaitForServerIP and provider backends.
func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
