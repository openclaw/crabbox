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
// Recovery explicitly reports a durable recovery claim after failed acquisition.
type DelegatedSandbox struct {
	LeaseID        string
	Slug           string
	CleanupCommand string
	Unlock         func()
	Recovery       *DelegatedSandboxRecovery
}

// DelegatedSandboxRecovery reports an authorized durable claim left after the
// adapter's acquisition rollback. It is used only on Acquire error and grants
// no permission for shared cleanup, retention, execution, or readiness.
type DelegatedSandboxRecovery struct {
	LeaseID        string
	Slug           string
	CleanupCommand string
}

// DelegatedSandboxCommand keeps command preparation (including credential
// staging) outside the execution timer. Close receives a bounded, uncanceled
// context before sandbox cleanup, including for retained/reused sandboxes.
// Adapters may return a mandatory cleanup failure, or warn and return nil for
// best-effort cleanup. An explicit ExitError preserves its cleanup-only code.
type DelegatedSandboxCommand struct {
	Text string
	// OutputScope names the adapter's actual stream boundary, not inferred workload purity.
	OutputScope core.RunOutputScope
	Run         func(context.Context, io.Writer, io.Writer) (int, error)
	Close       func(context.Context) error
}

type observedProcessEndError struct{ message string }

func (e observedProcessEndError) Error() string { return e.message }

// ObservedProcessEndError marks an abnormal end decoded for the submitted
// command. Its accompanying return code is the observed process exit, not a
// transport status. The message must already be safe to display. Return this
// error directly from Command.Run; wrapping or joining it loses this distinction.
func ObservedProcessEndError(message string) error {
	return observedProcessEndError{message: message}
}

// DelegatedSandboxLifecycle describes sandbox operations, not a provider API.
// Adapters retain claim authorization, scope checks, locks, credential filtering,
// archive transport and execution. Persistent shell allocations fit this owner;
// finite batch jobs and SSH leases have different lifecycles.
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

// FinalizeDelegatedCommandOutcome interprets a provider's command response.
// Transport errors do not establish command exits, regardless of numeric code.
func FinalizeDelegatedCommandOutcome(exitCode int, err error) core.RunResult {
	if err == nil {
		return core.FinalizeRunResult(core.RunResult{ExitCode: exitCode}, nil)
	}
	if _, observed := err.(observedProcessEndError); observed {
		if exitCode == 0 {
			exitCode = 1
		}
		return core.RunResult{ExitCode: exitCode, Status: core.RunStatusFailed, ErrorKind: core.RunErrorCommandExit}
	}
	outcome := core.FinalizeRunResult(core.RunResult{}, err)
	outcome.ExitCode = 1
	return outcome
}

// PinDelegatedRunFailure classifies an unclassified setup failure before cleanup.
// Nonzero public setup codes (including signed process exits) survive without
// becoming user command exits. Already classified outcomes and their error
// identities are left unchanged.
func PinDelegatedRunFailure(result core.RunResult, err error) (core.RunResult, error) {
	if err == nil || result.Status != "" {
		return result, err
	}
	outcome := core.FinalizeRunResult(core.RunResult{}, err)
	result.Status, result.ErrorKind = outcome.Status, outcome.ErrorKind
	result.ExitCode = core.ExitCodeForError(err, 1)
	return result, ExitErrorWithCause(result.ExitCode, err.Error(), err)
}

// AppendDelegatedRunFailure adds a terminal cleanup or reporting failure to an
// already classified primary outcome. A first failure selects firstCode and a
// provider-error result; later failures preserve the primary code/status and
// expose each safe error message without formatting hidden underlying causes.
func AppendDelegatedRunFailure(result core.RunResult, primary, secondary error, firstCode int) (core.RunResult, error) {
	if secondary == nil {
		return result, primary
	}
	if primary == nil {
		result.ExitCode = firstCode
		result.Status, result.ErrorKind = core.RunStatusFailed, core.RunErrorProvider
		return result, ExitErrorWithCause(firstCode, secondary.Error(), secondary)
	}
	joined := errors.Join(primary, secondary)
	// The CLI prints the selected ExitError message, not the joined error.
	return result, ExitErrorWithCause(result.ExitCode, joined.Error(), joined)
}

// RunDelegatedSandbox owns the single sandbox run sequence and finalization.
// The first failure determines the exit code/status; later cleanup failures are
// joined as diagnostics. Sandbox cleanup alone fails with code 1. A failed deletion
// leaves the session kept (and its claim intact in the adapter) for recovery.
func RunDelegatedSandbox(ctx context.Context, req core.RunRequest, lifecycle DelegatedSandboxLifecycle) (result core.RunResult, retErr error) {
	clock := lifecycle.Runtime.Clock
	now := func() time.Time { return core.ClockNow(clock) }
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
	acquireFailed := false
	reuseAdmitted := acquired || lifecycle.AdmitReuse == nil
	commandRan := false

	defer func() {
		if sandbox.Unlock != nil {
			defer sandbox.Unlock()
		}
		if prepared != nil {
			defer prepared.Close()
		}
		result, retErr = PinDelegatedRunFailure(result, retErr)
		appendFailure := func(err error, firstCode int) {
			result, retErr = AppendDelegatedRunFailure(result, retErr, err, firstCode)
		}
		if command.Close != nil {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
			closeErr := command.Close(cleanupCtx)
			cancel()
			appendFailure(closeErr, core.ExitCodeForError(closeErr, 1))
		}
		if result.Session != nil && !acquireFailed {
			shouldStop := acquired && !req.Keep
			if retErr != nil && reuseAdmitted {
				core.HandleDelegatedRunFailure(stderr, req, lifecycle.Provider, sandbox.LeaseID, sandbox.Slug, lifecycle.IdleTimeout, lifecycle.TTL, acquired, &shouldStop)
			}
			result.Session.Kept = true
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
			if shouldStop {
				req.Observation.Phase(core.RunPhaseCleanup)
				if err := lifecycle.Cleanup(cleanupCtx); err != nil {
					appendFailure(fmt.Errorf("%s cleanup failed: %w", lifecycle.Provider, err), 1)
				} else {
					result.Session.Kept = false
				}
			} else if reuseAdmitted && lifecycle.Retained != nil {
				appendFailure(lifecycle.Retained(cleanupCtx), 1)
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
				ExitCode: result.ExitCode, Label: strings.TrimSpace(req.Label), Workdir: lifecycle.Workdir,
			}, result, retErr))
			// A failed writer cannot emit an agreed record. Still report the I/O
			// failure without replacing an existing command/cleanup failure.
			appendFailure(err, 1)
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
		req.Observation.Phase(core.RunPhaseAcquire)
		sandbox, err = lifecycle.Acquire(ctx)
	} else {
		req.Observation.Phase(core.RunPhaseResolve)
		sandbox, err = lifecycle.Resolve(ctx)
	}
	if err != nil {
		acquireFailed = acquired
		if recovery := sandbox.Recovery; acquired && recovery != nil && recovery.LeaseID != "" {
			result.LeaseID, result.Slug = recovery.LeaseID, recovery.Slug
			req.Observation.BindLease(recovery.LeaseID, recovery.Slug)
			result.Session = &core.RunSessionHandle{
				Provider: lifecycle.Provider, LeaseID: recovery.LeaseID, Slug: recovery.Slug,
				Kept: true, CleanupCommand: recovery.CleanupCommand,
			}
		}
		return result, err
	}
	result.LeaseID, result.Slug = sandbox.LeaseID, sandbox.Slug
	req.Observation.BindLease(sandbox.LeaseID, sandbox.Slug)
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
		req.Observation.Phase(core.RunPhaseSetup)
		if err := lifecycle.Setup(ctx); err != nil {
			return result, err
		}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	req.Observation.Phase(core.RunPhaseSync)
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
	req.Observation.Phase(core.RunPhaseCommand)
	scope := command.OutputScope
	if scope == "" {
		scope = core.RunOutputWorkload
	}
	commandStdout, commandStderr := req.Observation.CommandWriters(stdout, stderr, scope)
	commandStarted := now()
	result.ExitCode, err = command.Run(ctx, commandStdout, commandStderr)
	result.Command = now().Sub(commandStarted)
	commandRan = true
	outcome := FinalizeDelegatedCommandOutcome(result.ExitCode, err)
	result.ExitCode, result.Status, result.ErrorKind = outcome.ExitCode, outcome.Status, outcome.ErrorKind
	if err != nil {
		return result, ExitErrorWithCause(result.ExitCode, fmt.Sprintf("%s run failed: %v", lifecycle.Provider, RedactErrorSecrets(err.Error())), err)
	}
	if result.ExitCode != 0 {
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

func (e sandboxRunError) RunClassificationCause() error {
	return core.PrimaryRunClassificationCause(e.cause)
}

// ExitErrorWithCause keeps the selected exit code and a display-safe message
// while retaining the cause for errors.Is/As without printing it again.
func ExitErrorWithCause(code int, message string, cause error) error {
	return sandboxRunError{ExitError: core.ExitError{Code: code, Message: message}, cause: cause}
}
