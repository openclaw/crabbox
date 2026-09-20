package cli

import (
	"errors"
	"fmt"
)

type noProviderFlags struct{}

func NoProviderFlags() any { return noProviderFlags{} }

func acquireAttemptsRetry(rt Runtime, keep bool, acquire func() (LeaseTarget, error)) (LeaseTarget, error) {
	for attempt := 1; ; attempt++ {
		lease, err := acquire()
		if err == nil {
			return lease, nil
		}
		if !isRetryableAcquireError(err) || attempt >= acquireAttemptsForError(keep, err) {
			return LeaseTarget{}, err
		}
		if isCoordinatorStaleInstanceCleanedError(err) {
			fmt.Fprintf(rt.Stderr, "warning: coordinator returned stale instance; retrying with fresh lease: %v\n", err)
		} else {
			fmt.Fprintf(rt.Stderr, "warning: bootstrap failed; retrying with fresh lease: %v\n", err)
		}
	}
}

func acquireAttemptsForError(keep bool, err error) int {
	if isCoordinatorStaleInstanceCleanedError(err) {
		return 5
	}
	return acquireAttempts(keep)
}

func isRetryableAcquireError(err error) bool {
	return isBootstrapWaitError(err) || isCoordinatorStaleInstanceCleanedError(err)
}

type coordinatorStaleInstanceCleanedError struct {
	err error
}

func (e coordinatorStaleInstanceCleanedError) Error() string {
	return e.err.Error()
}

func (e coordinatorStaleInstanceCleanedError) Unwrap() error {
	return e.err
}

func isCoordinatorStaleInstanceCleanedError(err error) bool {
	var cleaned coordinatorStaleInstanceCleanedError
	return errors.As(err, &cleaned)
}
