package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/openclaw/crabbox/internal/runner"
	"github.com/openclaw/crabbox/internal/runner/runnerfs"
	"github.com/openclaw/crabbox/internal/runtimeartifact"
)

// Each filesystem operation owns a distinct capability scope. Callers finalize
// its lease after release, or finish it after standalone work; the executable is
// temporary, while runnerfs journal state lives in the guest UserConfigDir.
// Development compilation is an explicit caller choice and never a pack fallback.
func newFilesystemRuntimeScope(development runtimeartifact.Source) (*nativeRuntimeScope, error) {
	buildID, err := runner.SourceID()
	if err != nil {
		return nil, err
	}
	scope := newNativeRuntimeScope()
	scope.required = runtimeartifact.Requirement{Capability: runtimeartifact.Filesystem, ProtocolVersion: "1", BuildID: buildID}
	scope.development = development
	required := scope.required
	scope.install = func(ctx context.Context, target SSHTarget, source runtimeartifact.Source) (*remoteNativeRuntime, error) {
		if isWindowsNativeTarget(target) {
			return prepareWindowsFilesystemRuntime(ctx, target, source, required)
		}
		return preparePOSIXRuntime(ctx, target, source, required)
	}
	return scope, nil
}

// filesystemClient binds cp/results consumers to the operation's installation.
// Local source selection completes before platform probing or any guest access.
// runner.Client validates the framed hello and terminal response on every call.
func (scope *nativeRuntimeScope) filesystemClient(ctx context.Context, target SSHTarget, stderr io.Writer) (*runner.Client, error) {
	if scope.required.Capability != runtimeartifact.Filesystem {
		return nil, errors.New("filesystem client requires its own capability scope")
	}
	installed, err := scope.ensure(ctx, target)
	if err != nil {
		return nil, err
	}
	identity := installed.identity
	serve := "serve"
	textOnly := isWindowsWSL2Target(target) || isWindowsNativeTarget(target)
	if textOnly {
		serve = "serve-base64"
	}
	command := remotePOSIXControlCommand("exec " + shellQuote(installed.path) + " " + shellQuote(runner.Command) + " " + serve)
	transport := runner.Transport(func(callCtx context.Context, input io.Reader, output io.Writer) error {
		// A retained client cannot reopen an installation after lease finalization.
		if _, err := scope.prepared(callCtx, target); err != nil {
			return err
		}
		buildCommand := func(size int64) string {
			if isWindowsNativeTarget(target) {
				return windowsFilesystemInvokeCommand(installed, size)
			}
			return command
		}
		retirementUnconfirmed, err := runFilesystemCommand(callCtx, target, buildCommand, input, output, stderr)
		if retirementUnconfirmed {
			scope.retain(installed, fmt.Errorf("filesystem transport retirement unconfirmed: %w", err))
		}
		return err
	})
	if textOnly {
		transport = runner.Base64Transport(transport)
	}
	return &runner.Client{Identity: runner.Identity{BuildID: identity.BuildID, OS: identity.Target.OS, Arch: identity.Target.Arch, Protocol: 1}, Transport: transport}, nil
}

// Spooling gives the existing finite SSH/WSL transport an exact input length.
// The generic transport owns workspace witnesses, cancellation, and command exit.
func runFilesystemCommand(ctx context.Context, target SSHTarget, command func(int64) string, input io.Reader, output, stderr io.Writer) (retirementUnconfirmed bool, err error) {
	spool, err := os.CreateTemp("", "crabbox-filesystem-input-*")
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, spool.Close(), os.Remove(spool.Name())) }()
	limit := runner.MaxRequestBytes()
	limited := &io.LimitedReader{R: input, N: limit + 1}
	if err := runnerfs.Copy(ctx, spool, limited); err != nil {
		return false, err
	}
	if limited.N == 0 {
		return false, errors.New("filesystem request exceeds input limit")
	}
	size := limit + 1 - limited.N
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	// Helpers do not have user workload stderr. Preserve the resolved transport's
	// bounded diagnostic redaction instead of streaming authentication fields.
	diagnostic := newSynchronizedTailBuffer(failureTailLines)
	retirementUnconfirmed, err = executeSSHWithRetirement(ctx, &target, command(size), spool, size, 0, "2", "1", output, diagnostic)
	writeSSHTransportDiagnostic(stderr, target, diagnostic.String())
	// Local spool errors above or during deferred close cannot make a command
	// active remotely. Dispatch retirement is recorded by the transport owner.
	return retirementUnconfirmed, err
}
