package shared

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// DelegatedSandbox is returned only after the adapter has authorized and bound
// the resource. Unlock, when set, is held through cleanup and final reporting.
// On acquisition failure the adapter owns partial-resource rollback; Unlock is
// still called, but no session or permission to delete is inferred from an ID.
type DelegatedSandbox struct {
	LeaseID        string
	Slug           string
	CleanupCommand string
	Unlock         func()
}

// DelegatedSandboxCommand keeps command preparation (including credential
// staging) outside the execution timer. Close receives a bounded, uncanceled
// context before sandbox cleanup, including for retained/reused sandboxes.
// Best-effort transport cleanup and its diagnostics remain adapter-owned.
type DelegatedSandboxCommand struct {
	Text  string
	Run   func(context.Context) (int, error)
	Close func(context.Context)
}

// DelegatedSandboxLifecycle describes sandbox operations, not a provider API.
// Adapters retain claim authorization, scope checks, locks, credential filtering,
// archive transport and execution. Jobs and SSH leases have different lifecycles.
type DelegatedSandboxLifecycle struct {
	Provider       string
	Runtime        core.Runtime
	Workdir        string
	IdleTimeout    time.Duration
	TTL            time.Duration
	CleanupTimeout time.Duration

	Preflight      func(context.Context) error
	PrepareArchive func(context.Context) (*core.PreparedArchive, error)
	Acquire        func(context.Context) (DelegatedSandbox, error)
	Resolve        func(context.Context) (DelegatedSandbox, error)
	// AdmitReuse checks run readiness after Resolve binds an authorized session.
	// Failure retains that session without activity refresh or rerun hints.
	AdmitReuse func(context.Context) error
	Setup      func(context.Context) error
	Sync       func(context.Context, *core.PreparedArchive) ([]core.TimingPhase, time.Duration, error)
	NoSync     func(context.Context) error
	Command    func(context.Context) (DelegatedSandboxCommand, error)
	Retained   func(context.Context) error
	Cleanup    func(context.Context) error
}

// RunDelegatedSandbox owns the single sandbox run sequence and finalization.
// The first failure determines the exit code/status; later cleanup failures are
// joined as diagnostics. Cleanup alone fails with code 1. A failed deletion
// leaves the session kept (and its claim intact in the adapter) for recovery.
func RunDelegatedSandbox(ctx context.Context, req core.RunRequest, lifecycle DelegatedSandboxLifecycle) (result core.RunResult, retErr error) {
	now := time.Now
	if lifecycle.Runtime.Clock != nil {
		now = lifecycle.Runtime.Clock.Now
	}
	stdout, stderr := lifecycle.Runtime.Stdout, lifecycle.Runtime.Stderr
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	cleanupTimeout := lifecycle.CleanupTimeout
	if cleanupTimeout <= 0 {
		cleanupTimeout = 30 * time.Second
	}
	started := now()
	result.Provider = lifecycle.Provider
	result.SyncDelegated = true
	var sandbox DelegatedSandbox
	var prepared *core.PreparedArchive
	var command DelegatedSandboxCommand
	var syncDuration time.Duration
	var syncPhases []core.TimingPhase
	if req.NoSync {
		syncPhases = []core.TimingPhase{{Name: "sync", Skipped: true, Reason: "--no-sync"}}
	}
	acquired := req.ID == ""
	reuseAdmitted := acquired || lifecycle.AdmitReuse == nil
	commandRan := false

	defer func() {
		if sandbox.Unlock != nil {
			defer sandbox.Unlock()
		}
		if prepared != nil {
			defer prepared.Close()
		}
		if command.Close != nil {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
			command.Close(cleanupCtx)
			cancel()
		}
		// Classify before secondary cleanup errors can obscure command/cancel
		// outcomes. Setup exit codes are CLI failures, not user command exits.
		if retErr != nil && result.Status == "" {
			outcome := core.FinalizeRunResult(core.RunResult{}, retErr)
			result.Status, result.ErrorKind = outcome.Status, outcome.ErrorKind
			var ee core.ExitError
			result.ExitCode = 1
			if errors.As(retErr, &ee) && ee.Code != 0 {
				result.ExitCode = ee.Code
			}
			// Pin the primary exit before joining cleanup errors, which may
			// themselves contain an ExitError with a different code.
			retErr = sandboxLifecycleError(result.ExitCode, retErr.Error(), retErr)
		}
		appendFailure := func(err error) {
			if err == nil {
				return
			}
			if retErr == nil {
				result.ExitCode = 1
				result.Status, result.ErrorKind = core.RunStatusFailed, core.RunErrorProvider
				retErr = sandboxLifecycleError(1, err.Error(), err)
			} else {
				retErr = errors.Join(retErr, err)
			}
		}
		if result.Session != nil {
			shouldStop := acquired && !req.Keep
			if retErr != nil && reuseAdmitted {
				core.HandleDelegatedRunFailure(stderr, req, lifecycle.Provider, sandbox.LeaseID, sandbox.Slug, lifecycle.IdleTimeout, lifecycle.TTL, acquired, &shouldStop)
			}
			result.Session.Kept = true
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
			if shouldStop {
				if err := lifecycle.Cleanup(cleanupCtx); err != nil {
					appendFailure(fmt.Errorf("%s cleanup failed: %w", lifecycle.Provider, err))
				} else {
					result.Session.Kept = false
				}
			} else if reuseAdmitted && lifecycle.Retained != nil {
				appendFailure(lifecycle.Retained(cleanupCtx))
			}
			cancel()
		}
		result.Total = now().Sub(started)
		result = core.FinalizeRunResult(result, retErr)
		if commandRan {
			if req.NoSync {
				fmt.Fprintf(stderr, "%s run summary sync_skipped=true command=%s total=%s exit=%d\n", lifecycle.Provider, result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), result.ExitCode)
			} else {
				fmt.Fprintf(stderr, "%s run summary sync=%s command=%s total=%s exit=%d\n", lifecycle.Provider, syncDuration.Round(time.Millisecond), result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), result.ExitCode)
			}
		}
		if req.TimingJSON {
			err := core.WriteTimingJSON(stderr, core.TimingReportWithRunResult(core.TimingReport{
				Provider: lifecycle.Provider, LeaseID: result.LeaseID, Slug: result.Slug,
				SyncDelegated: true, SyncSkipped: req.NoSync, SyncMs: syncDuration.Milliseconds(), SyncPhases: syncPhases,
				CommandMs: result.Command.Milliseconds(), TotalMs: result.Total.Milliseconds(),
				ExitCode: result.ExitCode, Label: strings.TrimSpace(req.Label),
			}, result, retErr))
			// A failed writer cannot emit an agreed record. Still report the I/O
			// failure without replacing an existing command/cleanup failure.
			appendFailure(err)
		}
	}()

	if err := ctx.Err(); err != nil {
		return result, err
	}
	if lifecycle.Preflight != nil {
		if err := lifecycle.Preflight(ctx); err != nil {
			return result, err
		}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	var err error
	if acquired && !req.NoSync {
		prepared, err = lifecycle.PrepareArchive(ctx)
		if err != nil {
			return result, err
		}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if acquired {
		sandbox, err = lifecycle.Acquire(ctx)
	} else {
		sandbox, err = lifecycle.Resolve(ctx)
	}
	if err != nil {
		return result, err
	}
	result.LeaseID, result.Slug = sandbox.LeaseID, sandbox.Slug
	result.Session = &core.RunSessionHandle{
		Provider: lifecycle.Provider, LeaseID: sandbox.LeaseID, Slug: sandbox.Slug,
		Reused: !acquired, CleanupCommand: sandbox.CleanupCommand,
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if !acquired && lifecycle.AdmitReuse != nil {
		if err := lifecycle.AdmitReuse(ctx); err != nil {
			return result, err
		}
		reuseAdmitted = true
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if lifecycle.Setup != nil {
		if err := lifecycle.Setup(ctx); err != nil {
			return result, err
		}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if req.NoSync {
		if err := lifecycle.NoSync(ctx); err != nil {
			return result, err
		}
	} else {
		syncPhases, syncDuration, err = lifecycle.Sync(ctx, prepared)
		if err != nil {
			return result, err
		}
		fmt.Fprintf(stderr, "sync complete in %s\n", syncDuration.Round(time.Millisecond))
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if req.SyncOnly {
		fmt.Fprintf(stdout, "synced %s\n", lifecycle.Workdir)
		return result, nil
	}
	command, err = lifecycle.Command(ctx)
	if err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	result.CommandText = command.Text
	commandStarted := now()
	result.ExitCode, err = command.Run(ctx)
	result.Command = now().Sub(commandStarted)
	commandRan = true
	if err != nil {
		// Transport errors are not command exits, even if the API also reports
		// a nonzero process code. Preserve context causes for normalization.
		outcome := core.FinalizeRunResult(core.RunResult{}, err)
		result.ExitCode = 1
		result.Status, result.ErrorKind = outcome.Status, outcome.ErrorKind
		return result, sandboxLifecycleError(1, fmt.Sprintf("%s run failed: %v", lifecycle.Provider, RedactErrorSecrets(err.Error())), err)
	}
	if result.ExitCode != 0 {
		result = core.FinalizeRunResult(result, nil)
		return result, core.ExitError{Code: result.ExitCode, Message: fmt.Sprintf("%s run exited %d", lifecycle.Provider, result.ExitCode)}
	}
	return result, nil
}

// Keep the public exit contract and the cause without printing raw provider
// diagnostics a second time (which can undo adapter redaction).
type sandboxRunError struct {
	core.ExitError
	cause error
}

func (e sandboxRunError) Unwrap() []error { return []error{e.ExitError, e.cause} }

func sandboxLifecycleError(code int, message string, cause error) error {
	return sandboxRunError{ExitError: core.ExitError{Code: code, Message: message}, cause: cause}
}
