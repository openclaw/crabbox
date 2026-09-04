package shared

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type sandboxTestClock struct{ current time.Time }

func (c *sandboxTestClock) Now() time.Time        { return c.current }
func (c *sandboxTestClock) Sleep(d time.Duration) { c.current = c.current.Add(d) }

func TestExitErrorWithCausePreservesSelectedCodeAndMessage(t *testing.T) {
	inner := core.ExitError{Code: 7, Message: "raw provider diagnostic"}
	cause := errors.Join(inner, context.Canceled)
	err := ExitErrorWithCause(1, "safe provider error", cause)
	var exitErr core.ExitError
	if err.Error() != "safe provider error" || !errors.Is(err, cause) || !errors.Is(err, context.Canceled) {
		t.Fatalf("cause or safe message lost: %v", err)
	}
	if !errors.As(err, &exitErr) || exitErr.Code != 1 {
		t.Fatalf("selected exit code lost: %#v", exitErr)
	}
}

func TestDelegatedSandboxLifecycle(t *testing.T) {
	failure := errors.New("phase failed")
	cleanupFailure := errors.New("delete unavailable")
	for _, tc := range []struct {
		name, phase                                  string
		err                                          error
		reuse, keep, keepOnFailure, noSync, syncOnly bool
		commandCode                                  int
		cleanupErr                                   error
		wantCode                                     int
		wantStatus                                   core.RunStatus
		wantKind                                     core.RunErrorKind
		wantSession, wantKept, wantCleanup           bool
	}{
		{name: "new", wantSession: true, wantCleanup: true},
		{name: "reused", reuse: true, wantSession: true, wantKept: true},
		{name: "keep", keep: true, wantSession: true, wantKept: true},
		{name: "keep success only on failure", keepOnFailure: true, wantSession: true, wantCleanup: true},
		{name: "preflight", phase: "preflight", err: failure, wantCode: 1, wantKind: core.RunErrorProvider},
		{name: "archive guardrail", phase: "archive", err: core.ExitError{Code: 6, Message: "archive too large"}, wantCode: 6, wantKind: core.RunErrorProvider},
		{name: "acquire", phase: "acquire", err: failure, keepOnFailure: true, wantCode: 1, wantKind: core.RunErrorProvider},
		{name: "resolve ownership", reuse: true, phase: "resolve", err: core.ExitError{Code: 4, Message: "not owned"}, wantCode: 4, wantKind: core.RunErrorProvider},
		{name: "setup", phase: "setup", err: failure, wantCode: 1, wantKind: core.RunErrorProvider, wantSession: true, wantCleanup: true},
		{name: "kept setup", phase: "setup", err: failure, keepOnFailure: true, wantCode: 1, wantKind: core.RunErrorProvider, wantSession: true, wantKept: true},
		{name: "sync", phase: "sync", err: core.ExitError{Code: 6, Message: "sync failed"}, wantCode: 6, wantKind: core.RunErrorProvider, wantSession: true, wantCleanup: true},
		{name: "kept sync", phase: "sync", err: failure, keepOnFailure: true, wantCode: 1, wantKind: core.RunErrorProvider, wantSession: true, wantKept: true},
		{name: "reused sync", phase: "sync", err: failure, reuse: true, wantCode: 1, wantKind: core.RunErrorProvider, wantSession: true, wantKept: true},
		{name: "no sync", noSync: true, wantSession: true, wantCleanup: true},
		{name: "no sync setup", noSync: true, phase: "workspace", err: failure, wantCode: 1, wantKind: core.RunErrorProvider, wantSession: true, wantCleanup: true},
		{name: "kept no sync setup", noSync: true, phase: "workspace", err: failure, keepOnFailure: true, wantCode: 1, wantKind: core.RunErrorProvider, wantSession: true, wantKept: true},
		{name: "sync only", syncOnly: true, wantSession: true, wantCleanup: true},
		{name: "sync only kept", syncOnly: true, keep: true, wantSession: true, wantKept: true},
		{name: "sync only no sync", syncOnly: true, noSync: true, wantSession: true, wantCleanup: true},
		{name: "prepare command", phase: "command", err: failure, keepOnFailure: true, wantCode: 1, wantKind: core.RunErrorProvider, wantSession: true, wantKept: true},
		{name: "command failure", commandCode: 7, wantCode: 7, wantKind: core.RunErrorCommandExit, wantSession: true, wantCleanup: true},
		{name: "kept command failure", commandCode: 7, keepOnFailure: true, wantCode: 7, wantKind: core.RunErrorCommandExit, wantSession: true, wantKept: true},
		{name: "transport failure", phase: "exec", err: failure, commandCode: 125, wantCode: 1, wantKind: core.RunErrorProvider, wantSession: true, wantCleanup: true},
		{name: "cancel", phase: "exec", err: context.Canceled, wantCode: 1, wantStatus: core.RunStatusCanceled, wantKind: core.RunErrorCanceled, wantSession: true, wantCleanup: true},
		{name: "keep canceled", phase: "exec", err: context.Canceled, keepOnFailure: true, wantCode: 1, wantStatus: core.RunStatusCanceled, wantKind: core.RunErrorCanceled, wantSession: true, wantKept: true},
		{name: "timeout", phase: "exec", err: context.DeadlineExceeded, wantCode: 1, wantStatus: core.RunStatusTimedOut, wantKind: core.RunErrorTimeout, wantSession: true, wantCleanup: true},
		{name: "cleanup", cleanupErr: cleanupFailure, wantCode: 1, wantKind: core.RunErrorProvider, wantSession: true, wantKept: true, wantCleanup: true},
		{name: "sync only cleanup", syncOnly: true, cleanupErr: cleanupFailure, wantCode: 1, wantKind: core.RunErrorProvider, wantSession: true, wantKept: true, wantCleanup: true},
		{name: "command and cleanup", commandCode: 7, cleanupErr: cleanupFailure, wantCode: 7, wantKind: core.RunErrorCommandExit, wantSession: true, wantKept: true, wantCleanup: true},
		{name: "setup and typed cleanup", phase: "setup", err: failure, cleanupErr: core.ExitError{Code: 4, Message: "claim changed"}, wantCode: 1, wantKind: core.RunErrorProvider, wantSession: true, wantKept: true, wantCleanup: true},
		{name: "setup and cleanup", phase: "setup", err: failure, cleanupErr: cleanupFailure, wantCode: 1, wantKind: core.RunErrorProvider, wantSession: true, wantKept: true, wantCleanup: true},
		{name: "cancel and cleanup", phase: "exec", err: context.Canceled, cleanupErr: context.DeadlineExceeded, wantCode: 1, wantStatus: core.RunStatusCanceled, wantKind: core.RunErrorCanceled, wantSession: true, wantKept: true, wantCleanup: true},
		{name: "retained activity", reuse: true, phase: "retained", err: failure, wantCode: 1, wantKind: core.RunErrorProvider, wantSession: true, wantKept: true},
		{name: "command and retained activity", reuse: true, phase: "retained", err: failure, commandCode: 7, wantCode: 7, wantKind: core.RunErrorCommandExit, wantSession: true, wantKept: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := &sandboxTestClock{current: time.Unix(0, 0)}
			var stdout, stderr bytes.Buffer
			var calls []string
			var archivePath string
			var locked bool
			ctx, cancel := context.WithCancel(context.WithValue(t.Context(), sandboxContextKey{}, "operation"))
			defer cancel()
			step := func(name string) error {
				calls = append(calls, name)
				clock.Sleep(time.Millisecond)
				if tc.phase == name {
					return tc.err
				}
				return nil
			}
			detached := func(ctx context.Context) {
				t.Helper()
				if ctx.Err() != nil || ctx.Value(sandboxContextKey{}) != "operation" {
					t.Fatalf("cleanup lost live context/value: %v", ctx.Err())
				}
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 2*time.Second {
					t.Fatal("cleanup must be bounded")
				}
			}
			sandbox := func(name string) (DelegatedSandbox, error) {
				locked = true
				return DelegatedSandbox{LeaseID: "lease", Slug: "slug", CleanupCommand: "stop lease", Unlock: func() {
					if !locked {
						t.Fatal("duplicate unlock")
					}
					locked = false
					calls = append(calls, "unlock")
				}}, step(name)
			}
			lifecycle := DelegatedSandboxLifecycle{
				Provider: "test", Runtime: core.Runtime{Stdout: &stdout, Stderr: &stderr, Clock: clock}, Workdir: "/workspace/repo", CleanupTimeout: 2 * time.Second,
				Preflight: func(context.Context) error { return step("preflight") },
				PrepareArchive: func(context.Context) (*core.PreparedArchive, error) {
					if err := step("archive"); err != nil {
						return nil, err
					}
					file, err := os.CreateTemp(t.TempDir(), "archive-")
					if err != nil {
						t.Fatal(err)
					}
					archivePath = file.Name()
					return &core.PreparedArchive{File: file}, nil
				},
				Acquire: func(context.Context) (DelegatedSandbox, error) { return sandbox("acquire") },
				Resolve: func(context.Context) (DelegatedSandbox, error) { return sandbox("resolve") },
				Setup:   func(context.Context) error { return step("setup") },
				Sync: func(_ context.Context, archive *core.PreparedArchive) ([]core.TimingPhase, time.Duration, error) {
					if (archive == nil) != tc.reuse {
						t.Fatalf("prepared archive=%v reused=%t", archive, tc.reuse)
					}
					return []core.TimingPhase{{Name: "test_sync", Ms: 1}}, time.Millisecond, step("sync")
				},
				NoSync: func(context.Context) error { return step("workspace") },
				Command: func(context.Context) (DelegatedSandboxCommand, error) {
					return DelegatedSandboxCommand{Text: "true", Run: func(context.Context) (int, error) {
						if !locked {
							t.Fatal("command without lock")
						}
						if errors.Is(tc.err, context.Canceled) {
							cancel()
						}
						return tc.commandCode, step("exec")
					}, Close: func(ctx context.Context) { detached(ctx); _ = step("close-command") }}, step("command")
				},
				Retained: func(ctx context.Context) error {
					detached(ctx)
					if !locked {
						t.Fatal("retained without lock")
					}
					return step("retained")
				},
				Cleanup: func(ctx context.Context) error {
					detached(ctx)
					if !locked {
						t.Fatal("cleanup without lock")
					}
					_ = step("cleanup")
					return tc.cleanupErr
				},
			}
			req := core.RunRequest{Keep: tc.keep, KeepOnFailure: tc.keepOnFailure, NoSync: tc.noSync, SyncOnly: tc.syncOnly, TimingJSON: true, Label: " label "}
			if tc.reuse {
				req.ID = "lease"
			}
			result, err := RunDelegatedSandbox(ctx, req, lifecycle)
			if tc.wantStatus == "" {
				tc.wantStatus = core.RunStatusSucceeded
				if tc.wantCode != 0 {
					tc.wantStatus = core.RunStatusFailed
				}
			}
			if result.ExitCode != tc.wantCode || result.Status != tc.wantStatus || result.ErrorKind != tc.wantKind || (err != nil) != (tc.wantCode != 0) {
				t.Fatalf("result=%#v err=%v want code=%d status=%s kind=%s", result, err, tc.wantCode, tc.wantStatus, tc.wantKind)
			}
			if tc.err != nil && !errors.Is(err, tc.err) {
				t.Fatalf("lost primary cause: %v", err)
			}
			if tc.cleanupErr != nil && !errors.Is(err, tc.cleanupErr) {
				t.Fatalf("lost cleanup cause: %v", err)
			}
			if (result.Session != nil) != tc.wantSession {
				t.Fatalf("session=%#v", result.Session)
			}
			if tc.wantSession && (result.Session.Kept != tc.wantKept || result.Session.Reused != tc.reuse || result.Session.LeaseID != "lease" || result.Provider != "test" || result.LeaseID != "lease") {
				t.Fatalf("result=%#v session=%#v", result, result.Session)
			}
			if err != nil {
				var ee core.ExitError
				if !errors.As(err, &ee) || ee.Code != tc.wantCode {
					t.Fatalf("exit error=%v", err)
				}
			}
			count := 0
			for _, call := range calls {
				if call == "cleanup" {
					count++
				}
			}
			wantCount := 0
			if tc.wantCleanup {
				wantCount = 1
			}
			if count != wantCount {
				t.Fatalf("cleanup count=%d calls=%v", count, calls)
			}
			if tc.noSync && (slices.Contains(calls, "archive") || slices.Contains(calls, "sync")) {
				t.Fatalf("no-sync calls=%v", calls)
			}
			if tc.syncOnly && slices.Contains(calls, "command") {
				t.Fatalf("sync-only calls=%v", calls)
			}
			if tc.reuse && slices.Contains(calls, "archive") {
				t.Fatalf("reused archive prepared before ownership: %v", calls)
			}
			if locked {
				t.Fatal("operation lock leaked")
			}
			if archivePath != "" {
				if _, err := os.Stat(archivePath); !os.IsNotExist(err) {
					t.Fatalf("archive leaked: %v", err)
				}
			}
			var report core.TimingReport
			reports := 0
			for _, line := range strings.Split(stderr.String(), "\n") {
				if strings.HasPrefix(line, "{") {
					if err := json.Unmarshal([]byte(line), &report); err != nil {
						t.Fatal(err)
					}
					reports++
				}
			}
			if reports != 1 || report.ExitCode != result.ExitCode || report.RunStatus != result.Status || report.ErrorKind != result.ErrorKind || report.TotalMs != result.Total.Milliseconds() || report.CommandMs != result.Command.Milliseconds() || report.LeaseID != result.LeaseID || report.Label != "label" {
				t.Fatalf("timing=%#v result=%#v stderr=%s", report, result, stderr.String())
			}
			if report.Workdir != "/workspace/repo" {
				t.Fatalf("timing workdir=%q", report.Workdir)
			}
			if result.Total != clock.current.Sub(time.Unix(0, 0)) {
				t.Fatalf("finalization missing from total: %v calls=%v", result.Total, calls)
			}
			wantCommand := time.Duration(0)
			if slices.Contains(calls, "exec") {
				wantCommand = time.Millisecond
			}
			if result.Command != wantCommand {
				t.Fatalf("command timer includes preparation/cleanup: %v calls=%v", result.Command, calls)
			}
		})
	}
}

type sandboxContextKey struct{}

func TestDelegatedSandboxSequence(t *testing.T) {
	var calls []string
	add := func(name string) { calls = append(calls, name) }
	lifecycle := DelegatedSandboxLifecycle{
		Provider: "test", Runtime: core.Runtime{}, Workdir: "/repo",
		Preflight:      func(context.Context) error { add("preflight"); return nil },
		PrepareArchive: func(context.Context) (*core.PreparedArchive, error) { add("archive"); return nil, nil },
		Acquire: func(context.Context) (DelegatedSandbox, error) {
			add("acquire")
			return DelegatedSandbox{Unlock: func() { add("unlock") }}, nil
		},
		Setup: func(context.Context) error { add("setup"); return nil },
		Sync: func(context.Context, *core.PreparedArchive) ([]core.TimingPhase, time.Duration, error) {
			add("sync")
			return nil, 0, nil
		},
		Command: func(context.Context) (DelegatedSandboxCommand, error) {
			add("command")
			return DelegatedSandboxCommand{Run: func(context.Context) (int, error) { add("exec"); return 0, nil }, Close: func(context.Context) { add("close-command") }}, nil
		},
		Cleanup: func(context.Context) error { add("cleanup"); return nil },
	}
	_, err := RunDelegatedSandbox(t.Context(), core.RunRequest{}, lifecycle)
	want := []string{"preflight", "archive", "acquire", "setup", "sync", "command", "exec", "close-command", "cleanup", "unlock"}
	if err != nil || !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls=%v err=%v", calls, err)
	}
}

func TestDelegatedSandboxCanceledBeforeAcquisition(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result, err := RunDelegatedSandbox(ctx, core.RunRequest{}, DelegatedSandboxLifecycle{Provider: "test"})
	if !errors.Is(err, context.Canceled) || result.Session != nil || result.Status != core.RunStatusCanceled {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestDelegatedSandboxCleanupDeadline(t *testing.T) {
	var calls int
	result, err := RunDelegatedSandbox(t.Context(), core.RunRequest{NoSync: true, SyncOnly: true}, DelegatedSandboxLifecycle{
		Provider: "test", CleanupTimeout: time.Millisecond,
		Acquire: func(context.Context) (DelegatedSandbox, error) { return DelegatedSandbox{LeaseID: "lease"}, nil },
		NoSync:  func(context.Context) error { return nil },
		Cleanup: func(ctx context.Context) error { calls++; <-ctx.Done(); return ctx.Err() },
	})
	if calls != 1 || !errors.Is(err, context.DeadlineExceeded) || result.ExitCode != 1 || result.ErrorKind != core.RunErrorProvider || !result.Session.Kept {
		t.Fatalf("calls=%d result=%#v err=%v", calls, result, err)
	}
}

type sandboxFailingTimingWriter struct{}

func (sandboxFailingTimingWriter) Write(p []byte) (int, error)               { return len(p), nil }
func (sandboxFailingTimingWriter) WriteTimingReport(core.TimingReport) error { return io.ErrClosedPipe }

func TestDelegatedSandboxTimingWriterFailureDoesNotSkipCleanupOrMaskExit(t *testing.T) {
	for _, code := range []int{0, 7} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			calls := 0
			result, err := RunDelegatedSandbox(t.Context(), core.RunRequest{NoSync: true, TimingJSON: true, KeepOnFailure: true}, DelegatedSandboxLifecycle{
				Provider: "test", Runtime: core.Runtime{Stderr: sandboxFailingTimingWriter{}},
				Acquire: func(context.Context) (DelegatedSandbox, error) { return DelegatedSandbox{LeaseID: "lease"}, nil },
				NoSync:  func(context.Context) error { return nil },
				Command: func(context.Context) (DelegatedSandboxCommand, error) {
					return DelegatedSandboxCommand{Run: func(context.Context) (int, error) { return code, nil }}, nil
				},
				Cleanup: func(context.Context) error { calls++; return nil },
			})
			wantCode, wantCleanup := 1, 1
			if code != 0 {
				wantCode, wantCleanup = code, 0
			}
			var ee core.ExitError
			if !errors.Is(err, io.ErrClosedPipe) || !errors.As(err, &ee) || ee.Code != wantCode || result.ExitCode != wantCode || calls != wantCleanup {
				t.Fatalf("calls=%d result=%#v err=%v", calls, result, err)
			}
		})
	}
}

func TestDelegatedSandboxCancellationBetweenPhases(t *testing.T) {
	for _, phase := range []string{"preflight", "acquire", "setup", "sync", "command"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var calls []string
			step := func(name string) {
				if ctx.Err() != nil {
					t.Fatalf("started %s after cancellation: %v", name, calls)
				}
				calls = append(calls, name)
				if phase == name {
					cancel()
				}
			}
			cleanups := 0
			result, err := RunDelegatedSandbox(ctx, core.RunRequest{}, DelegatedSandboxLifecycle{
				Provider:       "test",
				Preflight:      func(context.Context) error { step("preflight"); return nil },
				PrepareArchive: func(context.Context) (*core.PreparedArchive, error) { step("archive"); return nil, nil },
				Acquire: func(context.Context) (DelegatedSandbox, error) {
					step("acquire")
					return DelegatedSandbox{LeaseID: "lease"}, nil
				},
				Setup: func(context.Context) error { step("setup"); return nil },
				Sync: func(context.Context, *core.PreparedArchive) ([]core.TimingPhase, time.Duration, error) {
					step("sync")
					return nil, 0, nil
				},
				Command: func(context.Context) (DelegatedSandboxCommand, error) {
					step("command")
					return DelegatedSandboxCommand{Run: func(context.Context) (int, error) { t.Fatal("command ran after cancellation"); return 0, nil }}, nil
				},
				Cleanup: func(ctx context.Context) error {
					if ctx.Err() != nil {
						t.Fatal("cleanup context canceled")
					}
					cleanups++
					return nil
				},
			})
			wantCleanups := 1
			if phase == "preflight" {
				wantCleanups = 0
			}
			if !errors.Is(err, context.Canceled) || result.Status != core.RunStatusCanceled || cleanups != wantCleanups {
				t.Fatalf("result=%#v err=%v cleanups=%d", result, err, cleanups)
			}
		})
	}
}

func TestDelegatedSandboxReuseAdmission(t *testing.T) {
	failure := errors.New("reuse is not ready")
	for _, stage := range []string{"before admission", "admission error", "admission cancellation", "after admission", "setup error"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var stderr bytes.Buffer
			var calls []string
			result, err := RunDelegatedSandbox(ctx, core.RunRequest{ID: "lease", KeepOnFailure: true, TimingJSON: true}, DelegatedSandboxLifecycle{
				Provider: "test", Runtime: core.Runtime{Stderr: &stderr},
				Resolve: func(context.Context) (DelegatedSandbox, error) {
					if stage == "before admission" {
						cancel()
					}
					return DelegatedSandbox{LeaseID: "lease", Slug: "slug", Unlock: func() { calls = append(calls, "unlock") }}, nil
				},
				AdmitReuse: func(context.Context) error {
					calls = append(calls, "admit")
					if stage == "admission error" {
						return failure
					}
					if stage == "admission cancellation" {
						cancel()
						return ctx.Err()
					}
					if stage == "after admission" {
						cancel()
					}
					return nil
				},
				Setup:    func(context.Context) error { calls = append(calls, "setup"); return failure },
				Retained: func(context.Context) error { calls = append(calls, "retained"); return nil },
				Cleanup:  func(context.Context) error { t.Fatal("reused session must not be deleted"); return nil },
			})
			if err == nil || result.Session == nil || !result.Session.Kept || !result.Session.Reused {
				t.Fatalf("result=%#v session=%#v err=%v", result, result.Session, err)
			}
			want := []string{"admit", "unlock"}
			switch stage {
			case "before admission":
				want = []string{"unlock"}
			case "after admission":
				want = []string{"admit", "retained", "unlock"}
			case "setup error":
				want = []string{"admit", "setup", "retained", "unlock"}
			}
			if !slices.Equal(calls, want) {
				t.Fatalf("calls=%v want=%v", calls, want)
			}
			if !slices.Contains(want, "retained") && strings.Contains(stderr.String(), "rerun:") {
				t.Fatalf("unusable session got a rerun hint: %s", stderr.String())
			}
			if strings.Contains(stage, "admission") && stage != "admission error" && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation cause: %v", err)
			}
		})
	}
}
