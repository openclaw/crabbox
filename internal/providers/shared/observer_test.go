package shared

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
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

func TestPollReadinessTerminationAndObservationPrecedence(t *testing.T) {
	for _, scenario := range []string{
		"ready", "pre-canceled", "read canceled Err", "read canceled Cause", "between observations",
		"budget during read Err", "budget during read Cause", "budget during sleep", "late ready",
		"completed response", "response with context error", "independent deadline", "terminal observation", "sleep failure",
	} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cause := errors.New("private caller cause")
				response := errors.New("completed provider response")
				terminal := errors.New("terminal native state")
				sleepErr := errors.New("sleep failed")
				ctx, cancel := context.WithCancelCause(context.Background())
				defer cancel(nil)
				if scenario == "pre-canceled" {
					cancel(cause)
				}
				reads, checks, diagnostics := 0, 0, 0
				var responseErr error
				var stop ReadinessStop
				options := ReadinessOptions[int]{
					Timeout: time.Second, Interval: 3 * time.Second,
					IsResponseError: func(err error) bool { return errors.Is(err, response) },
					Check: func(value int, err error) (bool, error) {
						checks++
						if value == -1 {
							return false, terminal
						}
						return value == 7, err
					},
					Diagnostic: func(s ReadinessStop) error {
						diagnostics++
						stop = s
						return core.Exit(5, "safe readiness stop")
					},
				}
				if scenario == "sleep failure" {
					options.Sleep = func(context.Context, time.Duration) error { return sleepErr }
				}
				got, err := PollReadiness(ctx, options, func(ctx context.Context) (int, error) {
					reads++
					if scenario == "pre-canceled" {
						t.Fatal("fetch after cancellation")
					}
					switch scenario {
					case "ready":
						return 7, nil
					case "read canceled Err":
						cancel(cause)
						return 0, ctx.Err()
					case "read canceled Cause":
						cancel(cause)
						return 0, context.Cause(ctx)
					case "between observations":
						cancel(cause)
						return 0, nil
					case "budget during read Err":
						<-ctx.Done()
						return 0, ctx.Err()
					case "budget during read Cause":
						<-ctx.Done()
						return 0, context.Cause(ctx)
					case "late ready":
						<-ctx.Done()
						return 7, nil
					case "completed response":
						cancel(cause)
						responseErr = response
						return 0, responseErr
					case "response with context error":
						cancel(cause)
						responseErr = errors.Join(response, ctx.Err())
						return 0, responseErr
					case "independent deadline":
						return 0, context.DeadlineExceeded
					case "terminal observation":
						cancel(cause)
						return -1, nil
					default:
						return 0, nil
					}
				})
				switch scenario {
				case "ready", "late ready":
					if err != nil || got != 7 || diagnostics != 0 {
						t.Fatalf("got=%d err=%v diagnostics=%d", got, err, diagnostics)
					}
				case "completed response", "response with context error":
					if err != responseErr || diagnostics != 0 {
						t.Fatalf("err=%v, want original response", err)
					}
				case "independent deadline":
					if err != context.DeadlineExceeded || diagnostics != 0 {
						t.Fatalf("err=%v, want independent deadline", err)
					}
				case "terminal observation":
					if err != terminal || diagnostics != 0 {
						t.Fatalf("err=%v, want terminal observation", err)
					}
				case "sleep failure":
					if err != sleepErr || diagnostics != 0 {
						t.Fatalf("err=%v, want sleeper failure", err)
					}
				default:
					budget := scenario == "budget during read Err" || scenario == "budget during read Cause" || scenario == "budget during sleep"
					wantState := error(context.Canceled)
					if budget {
						wantState = context.DeadlineExceeded
					}
					if err == nil || err.Error() != "safe readiness stop" || !errors.Is(err, wantState) || diagnostics != 1 || stop.BudgetExpired != budget || stop.Err != wantState || !errors.Is(err, stop.Cause) {
						t.Fatalf("err=%v stop=%+v diagnostics=%d", err, stop, diagnostics)
					}
					if !budget && !errors.Is(err, cause) {
						t.Fatal("lost custom caller cause")
					}
				}
				if scenario == "pre-canceled" && (reads != 0 || checks != 0) {
					t.Fatal("observed pre-canceled request")
				}
				if (scenario == "read canceled Err" || scenario == "read canceled Cause" || scenario == "budget during read Err" || scenario == "budget during read Cause") && checks != 0 {
					t.Fatal("interrupted fetch reached provider observation callback")
				}
			})
		})
	}
}

func TestPollReadinessRetainsOnlyCompletedRetryDiagnostics(t *testing.T) {
	for _, clear := range []bool{false, true} {
		t.Run(fmt.Sprintf("successful observation clears=%v", clear), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				transient := errors.New("transient response")
				var last error
				reads, checks := 0, 0
				_, err := PollReadiness(context.Background(), ReadinessOptions[int]{
					Timeout: time.Second, Interval: time.Millisecond,
					Check: func(_ int, err error) (bool, error) { checks++; last = err; return false, nil },
					Diagnostic: func(ReadinessStop) error {
						if clear && last != nil || !clear && last != transient {
							t.Fatalf("last=%v clear=%v", last, clear)
						}
						return core.Exit(5, "timed out")
					},
				}, func(ctx context.Context) (int, error) {
					reads++
					if reads == 1 {
						return 0, transient
					}
					if reads == 2 && clear {
						return 0, nil
					}
					<-ctx.Done()
					return 0, ctx.Err()
				})
				wantChecks := 1
				if clear {
					wantChecks = 2
				}
				if !errors.Is(err, context.DeadlineExceeded) || checks != wantChecks {
					t.Fatalf("err=%v checks=%d", err, checks)
				}
			})
		})
	}
}

func TestPollReadyAcquisitionLifecycle(t *testing.T) {
	readError := errors.New("read failed")
	callerCause := errors.New("caller stopped acquisition")
	timeoutError := core.Exit(5, "acquisition timed out")
	for _, tc := range []struct {
		name      string
		wantError error
		wantValue int
		wantCalls int
	}{
		{name: "ready", wantValue: 7, wantCalls: 1},
		{name: "owned timeout", wantError: timeoutError, wantCalls: 1},
		{name: "caller cancellation", wantError: callerCause, wantCalls: 1},
		{name: "client deadline", wantError: context.DeadlineExceeded, wantCalls: 1},
		{name: "read error", wantError: readError, wantCalls: 1},
		{name: "read error after deadline", wantError: readError, wantCalls: 1},
		{name: "pending then error", wantError: readError, wantCalls: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				parent, cancel := context.WithCancelCause(context.Background())
				defer cancel(nil)
				var child context.Context
				calls := 0
				got, err := PollReady(parent, time.Minute, time.Second,
					func(ctx context.Context) (int, error) {
						child = ctx
						calls++
						switch tc.name {
						case "owned timeout":
							<-ctx.Done()
							return 7, ctx.Err()
						case "caller cancellation":
							cancel(callerCause)
						case "client deadline":
							return 7, context.DeadlineExceeded
						case "read error":
							return 7, readError
						case "read error after deadline":
							<-ctx.Done()
							return 7, readError
						case "pending then error":
							if calls == 2 {
								return 8, readError
							}
						}
						return 7, nil
					}, func(value int) bool { return tc.name == "ready" && value == 7 }, timeoutError)
				if got != tc.wantValue || err != tc.wantError || calls != tc.wantCalls {
					t.Fatalf("value=%d err=%v calls=%d; want value=%d err=%v calls=%d", got, err, calls, tc.wantValue, tc.wantError, tc.wantCalls)
				}
				if _, bounded := child.Deadline(); !bounded || child.Err() == nil {
					t.Fatalf("acquisition child was not bounded and released: %v", child.Err())
				}
				if tc.name != "caller cancellation" && context.Cause(parent) != nil {
					t.Fatalf("acquisition changed parent: %v", context.Cause(parent))
				}
			})
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

func TestErrorOwnersSelfMatchWithNonComparableCause(t *testing.T) {
	cause := sliceMessageCause{"private", "terminal"}
	for _, tc := range []struct {
		name    string
		build   func(context.Context) error
		message string
		code    int
	}{
		{"poll terminal", func(ctx context.Context) error {
			return PollTerminationError(ctx, cause, core.ExitError{Code: 23, Message: "public readiness"})
		}, "public readiness", 23},
		{"poll diagnostic", func(ctx context.Context) error { return PollTerminationError(ctx, context.Canceled, cause) }, cause.Error(), 1},
		{"exit cause", func(context.Context) error { return ExitErrorWithCause(23, "public run", cause) }, "public run", 23},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			cancel(cause)
			wrapped := tc.build(ctx)
			var typed sliceMessageCause
			if !errors.As(wrapped, &typed) || len(typed) != 2 {
				t.Fatal("typed non-comparable cause lost")
			}
			var public core.ExitError
			if !core.AsExitError(wrapped, &public) || public.Code != tc.code || wrapped.Error() != tc.message {
				t.Fatalf("public contract changed: %#v %q", public, wrapped.Error())
			}
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("errors.Is(wrapper, wrapper) panicked: %v", p)
				}
			}()
			if !errors.Is(wrapped, wrapped) {
				t.Fatal("wrapper did not match itself")
			}
		})
	}
}

func TestErrorOwnersComparableCauseControls(t *testing.T) {
	cause := errors.New("private cause")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	poll := PollTerminationError(ctx, cause, core.ExitError{Code: 23, Message: "safe"})
	for _, wrapped := range []error{poll, ExitErrorWithCause(23, "safe", cause)} {
		if !errors.Is(wrapped, wrapped) || !errors.Is(wrapped, cause) || wrapped.Error() != "safe" {
			t.Fatal("ordinary wrapper identity/presentation lost")
		}
		var public core.ExitError
		if !errors.As(wrapped, &public) || public.Code != 23 {
			t.Fatal("public exit lost")
		}
	}
	if !errors.Is(poll, context.Canceled) {
		t.Fatal("poll cancellation cause lost")
	}
	got := core.FinalizeRunResult(core.RunResult{}, poll)
	want := core.FinalizeRunResult(core.RunResult{}, context.Canceled)
	if got.Status != want.Status || got.ErrorKind != want.ErrorKind {
		t.Fatal("poll classification changed")
	}
	observed := ObservedProcessEndError("safe observed process")
	if !errors.Is(observed, observed) {
		t.Fatal("string-only value wrapper not self-comparable")
	}
}
