package shared

import (
	"context"
	"time"
)

type PollResult[T any] struct {
	Value     T
	HasValue  bool
	Err       error
	Attempt   int
	Remaining time.Duration
}

func Poll[T any](
	ctx context.Context,
	maxAttempts int,
	interval time.Duration,
	sleep func(context.Context, time.Duration) error,
	fetch func(context.Context) (T, error),
	check func(context.Context, T, error) (bool, error),
	progress func(PollResult[T]),
) (PollResult[T], error) {
	deadline, bounded := ctx.Deadline()
	var result PollResult[T]

	for {
		if err := context.Cause(ctx); err != nil {
			return result, err
		}
		result.Attempt++
		value, err := fetch(ctx)
		result.Err = err
		if err == nil {
			result.Value = value
			result.HasValue = true
		}
		done, err := check(ctx, result.Value, result.Err)
		if err != nil {
			return result, err
		}
		if done || maxAttempts > 0 && result.Attempt >= maxAttempts {
			return result, nil
		}
		if err := context.Cause(ctx); err != nil {
			return result, err
		}
		if progress != nil {
			if bounded {
				result.Remaining = max(0, time.Until(deadline))
			}
			progress(result)
		}
		if interval <= 0 {
			continue
		}
		if err := sleep(ctx, interval); err != nil {
			if cause := context.Cause(ctx); cause != nil {
				err = cause
			}
			return result, err
		}
	}
}

func SleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return nil
	}
}
