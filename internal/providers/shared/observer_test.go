package shared

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestPollTerminationErrorPreservesDiagnosticAndTerminalClassification(t *testing.T) {
	observation := errors.New("not ready")
	customTerminal := core.ExitError{Code: 7, Message: "private terminal detail"}
	for _, tc := range []struct {
		name       string
		diagnostic error
		deadline   bool
		custom     bool
		code       int
	}{
		{name: "stale attempt deadline", diagnostic: errors.Join(observation, context.DeadlineExceeded), code: 1},
		{name: "public code", diagnostic: ExitErrorWithCause(23, "safe readiness", observation), code: 23},
		{name: "nested public diagnostic", diagnostic: fmt.Errorf("readiness timed out: %w", ExitErrorWithCause(23, "safe probe detail", observation)), code: 23},
		{name: "signed code", diagnostic: ExitErrorWithCause(-1, "safe readiness", observation), code: -1},
		{name: "zero code", diagnostic: ExitErrorWithCause(0, "safe readiness", observation), code: 1},
		{name: "custom cancellation ordinary diagnostic", diagnostic: fmt.Errorf("safe readiness: %w", observation), custom: true, code: 1},
		{name: "custom cancellation typed diagnostic", diagnostic: ExitErrorWithCause(23, "safe readiness", observation), custom: true, code: 23},
		{name: "custom deadline", diagnostic: fmt.Errorf("safe readiness: %w", observation), deadline: true, custom: true, code: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ctx context.Context
			var cancel context.CancelFunc
			if tc.deadline {
				ctx, cancel = context.WithDeadlineCause(context.Background(), time.Now().Add(-time.Second), customTerminal)
			} else {
				var stop context.CancelCauseFunc
				ctx, stop = context.WithCancelCause(context.Background())
				cancel = func() { stop(nil) }
				if tc.custom {
					stop(customTerminal)
				} else {
					stop(nil)
				}
			}
			defer cancel()
			terminal := context.Cause(ctx)
			err := PollTerminationError(ctx, terminal, tc.diagnostic)
			var oldPublic, public core.ExitError
			if !core.AsExitError(tc.diagnostic, &oldPublic) {
				oldPublic = core.ExitError{Code: 1, Message: tc.diagnostic.Error()}
			}
			if !core.AsExitError(err, &public) || public != oldPublic {
				t.Fatalf("public CLI error=%#v want original=%#v", public, oldPublic)
			}
			if err.Error() != tc.diagnostic.Error() || !errors.Is(err, observation) || !errors.Is(err, terminal) {
				t.Fatalf("diagnostic or ordinary causes lost: %v", err)
			}
			if !errors.Is(err, ctx.Err()) {
				t.Fatalf("canonical context identity %v lost: %v", ctx.Err(), err)
			}
			if code := core.ExitCodeForError(err, 1); code != tc.code {
				t.Fatalf("code=%d want=%d", code, tc.code)
			}
			if !errors.Is(err, tc.diagnostic) {
				t.Fatal("original diagnostic is not discoverable")
			}
			want := core.FinalizeRunResult(core.RunResult{}, ctx.Err())
			for _, wrapped := range []error{err, fmt.Errorf("resume: %w", err), ExitErrorWithCause(42, "safe outer", err), errors.Join(err, context.DeadlineExceeded)} {
				got := core.FinalizeRunResult(core.RunResult{}, wrapped)
				if got.Status != want.Status || got.ErrorKind != want.ErrorKind {
					t.Fatalf("outcome=%s/%s want=%s/%s", got.Status, got.ErrorKind, want.Status, want.ErrorKind)
				}
			}
			if tc.name == "stale attempt deadline" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("classification hid the observation deadline from ordinary errors.Is")
			}
		})
	}
}

func TestPollImmediateSuccessSkipsSleepAndProgress(t *testing.T) {
	sleeps, progress := 0, 0
	result, err := Poll(context.Background(), 0, time.Second,
		func(context.Context, time.Duration) error { sleeps++; return nil },
		func(context.Context) (int, error) { return 7, nil },
		func(_ context.Context, value int, _ error) (bool, error) { return value == 7, nil },
		func(PollResult[int]) { progress++ })
	if err != nil || result.Value != 7 || !result.HasValue || result.Attempt != 1 || sleeps != 0 || progress != 0 {
		t.Fatalf("result=%#v err=%v sleeps=%d progress=%d", result, err, sleeps, progress)
	}
}

func TestPollPendingToSuccess(t *testing.T) {
	values := []int{1, 2}
	sleeps, progress := 0, 0
	result, err := Poll(context.Background(), 0, time.Second,
		func(context.Context, time.Duration) error { sleeps++; return nil },
		func(context.Context) (int, error) {
			value := values[0]
			values = values[1:]
			return value, nil
		},
		func(_ context.Context, value int, _ error) (bool, error) { return value == 2, nil },
		func(PollResult[int]) { progress++ })
	if err != nil || result.Value != 2 || result.Attempt != 2 || sleeps != 1 || progress != 1 {
		t.Fatalf("result=%#v err=%v sleeps=%d progress=%d", result, err, sleeps, progress)
	}
}

func TestPollTerminalErrorRetainsValue(t *testing.T) {
	wantErr := errors.New("terminal")
	result, err := Poll(context.Background(), 0, 0, nil,
		func(context.Context) (string, error) { return "failed", nil },
		func(context.Context, string, error) (bool, error) { return false, wantErr }, nil)
	if !errors.Is(err, wantErr) || result.Value != "failed" || !result.HasValue {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestPollRetryableFetchErrorRetainsLastValue(t *testing.T) {
	fetchErr := errors.New("temporary")
	attempt := 0
	var failed PollResult[int]
	result, err := Poll(context.Background(), 0, 0, nil,
		func(context.Context) (int, error) {
			attempt++
			switch attempt {
			case 1:
				return 10, nil
			case 2:
				return 0, fetchErr
			default:
				return 20, nil
			}
		},
		func(_ context.Context, value int, fetchErr error) (bool, error) {
			if fetchErr != nil {
				failed = PollResult[int]{Value: value, HasValue: true, Err: fetchErr}
			}
			return value == 20, nil
		}, nil)
	if err != nil || result.Value != 20 || result.Attempt != 3 || failed.Value != 10 || !failed.HasValue || !errors.Is(failed.Err, fetchErr) {
		t.Fatalf("result=%#v failed=%#v err=%v", result, failed, err)
	}
}

func TestPollNonretryableFetchError(t *testing.T) {
	wantErr := errors.New("denied")
	result, err := Poll(context.Background(), 0, 0, nil,
		func(context.Context) (int, error) { return 0, wantErr },
		func(_ context.Context, _ int, fetchErr error) (bool, error) { return false, fetchErr }, nil)
	if !errors.Is(err, wantErr) || result.Attempt != 1 || result.HasValue {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestPollCancellationDuringSleepPreventsFetch(t *testing.T) {
	wantErr := errors.New("stop")
	ctx, cancel := context.WithCancelCause(context.Background())
	fetches := 0
	result, err := Poll(ctx, 0, time.Second,
		func(context.Context, time.Duration) error { cancel(wantErr); return context.Canceled },
		func(context.Context) (int, error) { fetches++; return fetches, nil },
		func(context.Context, int, error) (bool, error) { return false, nil }, nil)
	if !errors.Is(err, wantErr) || fetches != 1 || result.Attempt != 1 {
		t.Fatalf("result=%#v err=%v fetches=%d", result, err, fetches)
	}
}

func TestPollMaxAttemptsIsExact(t *testing.T) {
	fetches, sleeps, progress := 0, 0, 0
	result, err := Poll(context.Background(), 3, time.Second,
		func(context.Context, time.Duration) error { sleeps++; return nil },
		func(context.Context) (int, error) { fetches++; return fetches, nil },
		func(context.Context, int, error) (bool, error) { return false, nil },
		func(PollResult[int]) { progress++ })
	if err != nil || result.Attempt != 3 || fetches != 3 || sleeps != 2 || progress != 2 {
		t.Fatalf("result=%#v err=%v fetches=%d sleeps=%d progress=%d", result, err, fetches, sleeps, progress)
	}
}

func TestPollProgressIncludesTiming(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var got PollResult[int]
	result, err := Poll(ctx, 0, time.Second,
		func(context.Context, time.Duration) error { return nil },
		func(context.Context) (int, error) { return 1, nil },
		func(_ context.Context, _ int, _ error) (bool, error) { return got.Attempt == 1, nil },
		func(result PollResult[int]) { got = result })
	if err != nil || result.Attempt != 2 || got.Attempt != 1 || got.Remaining <= 0 || got.Remaining > 10*time.Second {
		t.Fatalf("result=%#v progress=%#v err=%v", result, got, err)
	}
}

func TestPollCanceledContextSkipsFetch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	result, err := Poll(ctx, 0, 0, nil,
		func(context.Context) (int, error) { called = true; return 0, nil },
		func(context.Context, int, error) (bool, error) { return true, nil }, nil)
	if !errors.Is(err, context.Canceled) || called || result.Attempt != 0 {
		t.Fatalf("result=%#v err=%v called=%v", result, err, called)
	}
}

func TestPollCompletedObservationWinsLateCancellation(t *testing.T) {
	for _, cancelDuring := range []string{"fetch", "check"} {
		t.Run(cancelDuring, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			result, err := Poll(ctx, 0, 0, nil,
				func(context.Context) (int, error) {
					if cancelDuring == "fetch" {
						cancel()
					}
					return 7, nil
				},
				func(context.Context, int, error) (bool, error) {
					if cancelDuring == "check" {
						cancel()
					}
					return true, nil
				}, nil)
			if err != nil || result.Value != 7 || result.Attempt != 1 {
				t.Fatalf("result=%#v err=%v", result, err)
			}
		})
	}
}
