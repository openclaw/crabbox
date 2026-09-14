package shared

import (
	"context"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type pollTerminationError struct {
	public         core.ExitError
	terminal       error
	diagnostic     error
	classification error
}

func (e pollTerminationError) Error() string { return e.diagnostic.Error() }
func (e pollTerminationError) Unwrap() []error {
	return []error{e.public, e.diagnostic, e.terminal, e.classification}
}
func (e pollTerminationError) RunClassificationCause() error { return e.classification }

// PollTerminationError preserves a nonnil, display-safe diagnostic and its
// public ExitError while classifying a confirmed context-stop by the context's
// final state. Call only after establishing that terminal came from ctx stopping;
// immediate observation failures and successful polls retain their own policy.
// Both original errors and ctx.Err() remain discoverable without exposing
// terminal's text. Existing public codes and messages, including zero, survive.
func PollTerminationError(ctx context.Context, terminal, diagnostic error) error {
	var public core.ExitError
	if !core.AsExitError(diagnostic, &public) {
		public = core.ExitError{Code: 1, Message: diagnostic.Error()}
	}
	return pollTerminationError{
		public: public, terminal: terminal, diagnostic: diagnostic, classification: ctx.Err(),
	}
}

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
