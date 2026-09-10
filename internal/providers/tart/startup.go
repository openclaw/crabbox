package tart

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const startupDiagnosticLimit = 64 << 10

// startupProcess belongs to one acquisition, not the backend. Readiness runs
// synchronously on ctx; the sole reaper cancels it when the foreground VM exits.
type startupProcess struct {
	cmd    *exec.Cmd
	name   string
	keep   bool
	caller context.Context
	ctx    context.Context
	done   chan struct{}

	mu           sync.Mutex
	cancel       context.CancelCauseFunc
	log          *os.File
	detail       string
	captured     bool
	exited       bool
	failure      error
	stopping     bool
	transferred  bool
	stopLifetime func() bool
	lifetimeDone chan struct{}
}

func (b *backend) startVM(ctx context.Context, name string, keep bool) (*startupProcess, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, errors.Join(exit(2, "tart run %s: context already cancelled: %v", name, err), err)
	}
	env, err := tartEnvironment()
	if err != nil {
		return nil, err
	}
	log, err := os.CreateTemp("", "crabbox-tart-run-*.log")
	if err != nil {
		return nil, exit(2, "tart run %s: create startup log: %v", name, err)
	}
	// Files (including nil's /dev/null) avoid exec's copy pipes. A kept VM must
	// not depend on a reader in Crabbox after a successful acquisition.
	cmd := exec.Command("tart", "run", name, "--no-graphics", "--no-clipboard", "--no-audio")
	cmd.Env, cmd.Stderr = env, log
	if keep {
		detachCommand(cmd)
	}
	startupCtx, cancel := context.WithCancelCause(ctx)
	p := &startupProcess{cmd: cmd, name: name, keep: keep, caller: ctx, ctx: startupCtx, cancel: cancel, log: log, done: make(chan struct{})}
	if err := cmd.Start(); err != nil {
		cancel(nil)
		return nil, errors.Join(exit(2, "tart run %s: %v", name, err), p.closeLog())
	}
	go p.reap()
	if err := p.observe(b.startupObserveTimeout); err != nil {
		return nil, p.abort(err)
	}
	return p, nil
}

func (p *startupProcess) reap() {
	err := p.wait() // Exactly one Wait; returns with mu held.
	p.exited = true
	if !p.transferred {
		// A normal exit can finish writing after abort's pre-signal snapshot.
		// Forced termination must keep that snapshot, not cleanup diagnostics.
		if p.cmd.ProcessState != nil && p.cmd.ProcessState.Exited() {
			p.captured = false
		}
		p.capture()
		switch {
		case p.detail != "":
			p.failure = exit(2, "tart run %s failed during startup: %s", p.name, p.detail)
		case err != nil:
			p.failure = exit(2, "tart run %s failed during startup: %v", p.name, err)
		default:
			p.failure = exit(2, "tart run %s exited unexpectedly during startup", p.name)
		}
		if !p.stopping {
			p.cancel(p.failure)
		}
	}
	stop, lifetimeDone := p.stopLifetime, p.lifetimeDone
	p.mu.Unlock()
	if stop != nil && !stop() {
		<-lifetimeDone
	}
	close(p.done)
}

func (p *startupProcess) observe(timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.ctx.Done():
	case <-timer.C:
	}
	return context.Cause(p.ctx)
}

// capture and closeLog run with mu held (or before the reaper starts).
func (p *startupProcess) capture() {
	if p.captured || p.log == nil {
		return
	}
	p.captured = true
	// ReadAt does not change the shared file offset used by the child's writes.
	data := make([]byte, startupDiagnosticLimit)
	n, err := p.log.ReadAt(data, 0)
	p.detail = strings.TrimSpace(string(data[:n]))
	if err != nil && !errors.Is(err, io.EOF) {
		p.detail += fmt.Sprintf(" (read startup log: %v)", err)
		p.detail = p.detail[:min(len(p.detail), startupDiagnosticLimit)]
	}
}

func (p *startupProcess) closeLog() error {
	if p.log == nil {
		return nil
	}
	log := p.log
	p.log = nil
	// Unlinking does not reclaim the inode while Tart retains its writable fd.
	// The diagnostic read is bounded; whole-VM-lifetime disk usage is not.
	return errors.Join(log.Close(), os.Remove(log.Name()))
}

// abort must precede VM stop/delete. It snapshots diagnostics before sending
// any cleanup signal, cancels readiness, and joins the exact child in both modes.
func (p *startupProcess) abort(readinessErr error) error {
	p.mu.Lock()
	cause := context.Cause(p.ctx)
	if cause != nil {
		readinessErr = cause
	}
	p.capture()
	p.stopping = true
	p.cancel(readinessErr)
	exited := p.exited
	p.mu.Unlock()
	var killErr error
	if !exited {
		killErr = p.kill()
	}
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	// Wait may have won the race with Kill without yet publishing its result.
	// A normal exit (including zero) is not our cleanup-generated termination.
	naturalExit := p.cmd.ProcessState != nil && p.cmd.ProcessState.Exited()
	stoppedByCleanup := !exited && killErr == nil && !naturalExit
	if cause == nil && (exited || errors.Is(killErr, os.ErrProcessDone) || naturalExit) {
		readinessErr = p.failure
	}
	if errors.Is(killErr, os.ErrProcessDone) {
		killErr = nil
	}
	if callerErr := context.Cause(p.caller); callerErr != nil && errors.Is(cause, callerErr) {
		readinessErr = errors.Join(exit(2, "tart run %s: context cancelled during startup: %v", p.name, callerErr), callerErr)
	}
	if stoppedByCleanup && p.detail != "" {
		// The CLI displays ExitError.Message, so a joined diagnostic alone
		// would disappear there. Preserve both its exit code and original cause.
		display := exit(1, "%v", readinessErr)
		_ = errors.As(readinessErr, &display)
		readinessErr = startupStderrError{
			error: exit(display.Code, "%s\ntart run %s startup stderr: %s", display.Message, p.name, p.detail),
			cause: readinessErr,
		}
	}
	return errors.Join(readinessErr, killErr, p.closeLog())
}

type startupStderrError struct {
	error
	cause error
}

func (e startupStderrError) Unwrap() []error { return []error{e.error, e.cause} }

// handoff is the acquisition's commit point, after readiness AND claim success.
// Only the reaper survives for keep=true; no context-monitor goroutine remains.
func (p *startupProcess) handoff() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := context.Cause(p.ctx); err != nil {
		return err
	}
	if err := p.closeLog(); err != nil {
		return err
	}
	p.transferred = true
	p.cancel(nil) // Remove the acquisition context from its parent.
	if !p.keep {
		p.lifetimeDone = make(chan struct{})
		p.stopLifetime = context.AfterFunc(p.caller, func() {
			defer close(p.lifetimeDone)
			_ = p.kill()
		})
	}
	return nil
}

func (p *startupProcess) kill() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exited {
		return os.ErrProcessDone
	}
	return p.cmd.Process.Kill()
}
