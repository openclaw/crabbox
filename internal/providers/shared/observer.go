package shared

import (
	"context"
	"errors"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type pollTerminationError struct {
	public         core.ExitError
	terminal       error
	diagnostic     error
	classification error
}

func (e *pollTerminationError) Error() string { return e.diagnostic.Error() }
func (e *pollTerminationError) Unwrap() []error {
	return []error{e.public, e.diagnostic, e.terminal, e.classification}
}
func (e *pollTerminationError) RunClassificationCause() error { return e.classification }

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
	return &pollTerminationError{
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

type ReadinessStop struct {
	BudgetExpired bool
	Err           error
	Cause         error
}

type ReadinessOptions[T any] struct {
	Timeout, Interval time.Duration
	Sleep             func(context.Context, time.Duration) error
	IsResponseError   func(error) bool
	Check             func(T, error) (bool, error)
	Diagnostic        func(ReadinessStop) error
}

// PollReadiness owns the elapsed-time budget and context-stop classification.
// Adapters own completed-response recognition, readiness/retry policy and public
// diagnostics. Check and Diagnostic are required; Sleep defaults to SleepContext.
func PollReadiness[T any](ctx context.Context, options ReadinessOptions[T], fetch func(context.Context) (T, error)) (T, error) {
	budgetExpired := errors.New("readiness budget expired")
	waitCtx, cancel := context.WithTimeoutCause(ctx, options.Timeout, budgetExpired)
	defer cancel()
	sleep := options.Sleep
	if sleep == nil {
		sleep = SleepContext
	}
	var observationError error
	result, err := Poll(waitCtx, 0, options.Interval, sleep, fetch,
		func(_ context.Context, value T, fetchErr error) (bool, error) {
			if fetchErr != nil {
				response := options.IsResponseError != nil && options.IsResponseError(fetchErr)
				if cause := context.Cause(waitCtx); cause != nil && !response &&
					(errors.Is(fetchErr, cause) || errors.Is(fetchErr, waitCtx.Err())) {
					return false, errors.Join(cause, fetchErr)
				}
			}
			ready, checkErr := options.Check(value, fetchErr)
			observationError = checkErr
			return ready, checkErr
		}, nil)
	if err != nil {
		var zero T
		if observationError != nil {
			return zero, observationError
		}
		if cause := context.Cause(waitCtx); cause != nil && errors.Is(err, cause) {
			diagnostic := options.Diagnostic(ReadinessStop{
				BudgetExpired: errors.Is(cause, budgetExpired), Err: waitCtx.Err(), Cause: cause,
			})
			return zero, PollTerminationError(waitCtx, err, diagnostic)
		}
		return zero, err
	}
	return result.Value, nil
}

// PollReady bounds acquisition observations and waits, stops on the first fetch
// error, and returns a value only when ready. The adapter owns the readiness
// predicate and timeout diagnostic; client deadlines and caller causes survive.
func PollReady[T any](ctx context.Context, timeout, interval time.Duration, fetch func(context.Context) (T, error), ready func(T) bool, timeoutError error) (T, error) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result, err := Poll(waitCtx, 0, interval, SleepContext, fetch,
		func(_ context.Context, value T, fetchErr error) (bool, error) {
			if fetchErr != nil {
				return false, fetchErr
			}
			return ready(value), nil
		}, nil)
	if err != nil {
		var zero T
		if context.Cause(ctx) == nil && errors.Is(context.Cause(waitCtx), context.DeadlineExceeded) && errors.Is(err, context.DeadlineExceeded) {
			return zero, timeoutError
		}
		return zero, err
	}
	return result.Value, nil
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
