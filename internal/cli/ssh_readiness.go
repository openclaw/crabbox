package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"time"
)

type sshReadinessProbeStopped struct {
	probe string
	cause error
}

func (e *sshReadinessProbeStopped) Error() string { return e.cause.Error() }
func (e *sshReadinessProbeStopped) Unwrap() error { return e.cause }

// The stage names the local invocation, not evidence that remote code started.
func sshReadinessProbeContextError(ctx context.Context, probe string) *sshReadinessProbeStopped {
	cause := context.Cause(ctx)
	if cause == nil {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 0 {
			return nil
		}
		cause = context.DeadlineExceeded
	}
	return &sshReadinessProbeStopped{probe: probe, cause: cause}
}

var errSSHHostKeyVerification = errors.New("SSH host-key verification failed; verify the lease identity and its SSH host trust before reconnecting")

const sshHostKeyVerificationDiagnostic = "Host key verification failed."

var sshHostKeyVerificationPrefixes = func() [len(sshHostKeyVerificationDiagnostic)]int {
	var prefixes [len(sshHostKeyVerificationDiagnostic)]int
	for i, matched := 1, 0; i < len(prefixes); i++ {
		for matched > 0 && sshHostKeyVerificationDiagnostic[i] != sshHostKeyVerificationDiagnostic[matched] {
			matched = prefixes[matched-1]
		}
		if sshHostKeyVerificationDiagnostic[i] == sshHostKeyVerificationDiagnostic[matched] {
			matched++
		}
		prefixes[i] = matched
	}
	return prefixes
}()

// Retain only matching state, never stderr bytes. Matching survives arbitrary
// write boundaries and output volume without weakening bounded-buffer semantics.
type sshReadinessPatternState struct {
	fragment []byte
	prefixes []int
	matched  int
	err      error
}

type sshReadinessTerminalError struct{ message string }

func (e *sshReadinessTerminalError) Error() string { return e.message }

type sshReadinessDiagnostic struct {
	mu       sync.Mutex
	matched  int
	rejected bool
	patterns []sshReadinessPatternState
}

func newSSHReadinessDiagnostic(target SSHTarget) *sshReadinessDiagnostic {
	diagnostic := &sshReadinessDiagnostic{}
	for _, pattern := range target.ReadinessErrorPatterns {
		if pattern.Fragment == "" || pattern.Message == "" {
			continue
		}
		fragment := []byte(pattern.Fragment)
		prefixes := make([]int, len(fragment))
		for i, matched := 1, 0; i < len(fragment); i++ {
			for matched > 0 && fragment[i] != fragment[matched] {
				matched = prefixes[matched-1]
			}
			if fragment[i] == fragment[matched] {
				matched++
			}
			prefixes[i] = matched
		}
		diagnostic.patterns = append(diagnostic.patterns, sshReadinessPatternState{
			fragment: fragment,
			prefixes: prefixes,
			err:      &sshReadinessTerminalError{message: pattern.Message},
		})
	}
	return diagnostic
}

func (d *sshReadinessDiagnostic) Write(data []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, b := range data {
		if !d.rejected {
			for d.matched > 0 && b != sshHostKeyVerificationDiagnostic[d.matched] {
				d.matched = sshHostKeyVerificationPrefixes[d.matched-1]
			}
			if b == sshHostKeyVerificationDiagnostic[d.matched] {
				d.matched++
			}
			if d.matched == len(sshHostKeyVerificationDiagnostic) {
				d.rejected = true
			}
		}
		for i := range d.patterns {
			pattern := &d.patterns[i]
			if pattern.err == nil || pattern.matched == len(pattern.fragment) {
				continue
			}
			for pattern.matched > 0 && b != pattern.fragment[pattern.matched] {
				pattern.matched = pattern.prefixes[pattern.matched-1]
			}
			if b == pattern.fragment[pattern.matched] {
				pattern.matched++
			}
		}
	}
	return len(data), nil
}

func (d *sshReadinessDiagnostic) hostKeyRejected() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.rejected
}

func (d *sshReadinessDiagnostic) terminalError() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, pattern := range d.patterns {
		if pattern.matched == len(pattern.fragment) {
			return pattern.err
		}
	}
	return nil
}

func runSSHReadinessProbe(ctx context.Context, target SSHTarget, remote, connectTimeout, attempts string) error {
	diagnostic := newSSHReadinessDiagnostic(target)
	var stderr io.Writer = diagnostic
	if usesServeRelay(target) && os.Getenv("CRABBOX_SERVE_RELAY_DEBUG") == "1" {
		stderr = io.MultiWriter(stderr, &serveRelayDiagnosticForwarder{out: os.Stderr})
	}
	err := executeSSH(ctx, &target, remote, nil, 0, 0, connectTimeout, attempts, io.Discard, stderr)
	if err == nil {
		recordCreationObservation(ctx, "ssh_authenticated")
	}
	return sshReadinessProbeError(ctx, err, diagnostic)
}

func canWaitForLinuxReadiness(target SSHTarget) bool {
	return target.TargetOS == targetLinux && !target.SSHConfigProxy && target.SSHConfigFile == "" && target.ProxyCommand == ""
}

func linuxReadinessWaitCommand(target SSHTarget, timeout time.Duration) string {
	// A separate shell preserves custom predicates' exit and errexit behavior;
	// a subshell used as an && condition would suppress their set -e failures.
	loop := "while :; do\nsh -c " + shellQuote(sshReadyCommand(target)) + " && exit 0\nsleep 0.25 || exit 1\ndone"
	return "timeout " + strconv.FormatFloat(timeout.Seconds(), 'f', -1, 64) + "s sh -c " + shellQuote(loop)
}

func waitForLinuxReadiness(ctx context.Context, target SSHTarget, profile sshReadinessProfile, timeout time.Duration, progress func()) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// Do not leave a new persistent master holding probe pipes on cancellation.
	// An existing workspace-owned master retains its explicit lifetime owner.
	target.NoControlMaster = true
	// The remote timeout also bounds the loop if the client disappears. Hosts
	// without timeout simply fail this optimization and retry the usual probe.
	done := make(chan error, 1)
	go func() {
		done <- runSSHReadinessProbe(waitCtx, target, linuxReadinessWaitCommand(target, timeout), profile.connectTimeout, profile.connectionAttempts)
	}()
	progress()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			if waitCtx.Err() != nil {
				return context.Cause(waitCtx)
			}
			return err
		case <-ticker.C:
			progress()
		}
	}
}

func runWSLReadinessTransport(ctx context.Context, target SSHTarget, command, connectTimeout, attempts string) error {
	transport := sshTransportPreparation{command: command}
	diagnostic := newSSHReadinessDiagnostic(target)
	_, err := transport.runOnce(ctx, target, connectTimeout, attempts, io.Discard, diagnostic, false)
	if err == nil {
		err = context.Cause(ctx)
	}
	return sshReadinessProbeError(ctx, err, diagnostic)
}

func sshReadinessProbeError(ctx context.Context, err error, diagnostic *sshReadinessDiagnostic) error {
	// Terminal diagnostics are matched while streaming and retain no stderr bytes.
	// Retain the exit cause without exposing remote output or key data.
	if ctx.Err() == nil && exitCode(err) == 255 && diagnostic.hostKeyRejected() {
		return errors.Join(errSSHHostKeyVerification, err)
	}
	if ctx.Err() == nil && err != nil {
		if terminalErr := diagnostic.terminalError(); terminalErr != nil {
			return errors.Join(terminalErr, err)
		}
	}
	return err
}

func sshReadinessError(err error, phase string) error {
	var terminalErr *sshReadinessTerminalError
	if errors.Is(err, errSSHHostKeyVerification) || errors.As(err, &terminalErr) {
		return fmt.Errorf("%s: %w", phase, err)
	}
	return workspaceOwnerReadinessError(err, phase)
}
