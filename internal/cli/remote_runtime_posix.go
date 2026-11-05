package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/openclaw/crabbox/internal/remoteruntime"
)

func (r *remoteNativeRuntime) prepareCommand(command string, input io.ReadSeeker, size int64, limit sshCommandLimit) (sshTransportPreparation, error) {
	if len(command) > wslStageMaxCommand || size > wslStageMaxSize-int64(len(command)) {
		return sshTransportPreparation{}, errors.New("native command exceeds frame limits")
	}
	if input == nil {
		input = strings.NewReader("")
	}
	actual, err := input.Seek(0, io.SeekEnd)
	if err != nil || actual != size {
		return sshTransportPreparation{}, errors.Join(err, errors.New("native command input size changed"))
	}
	nonce, err := randomHex(16)
	if err != nil {
		return sshTransportPreparation{}, err
	}
	frame, err := newReplayableSSHInputStream([]byte(command), input, size)
	if err != nil {
		return sshTransportPreparation{}, err
	}
	// CBX-REMOTE-1's preflight purpose retains a nonce-bound completion record.
	// Use that existing receipt mode, but keep the actual command's exit status;
	// its diagnostic state names are not user-command outcomes. Auto-removing
	// command stages cannot distinguish retirement from a delayed launch.
	return sshTransportPreparation{
		command: r.runCommand(nonce, len(command), size, limit.execution, "preflight", "watched"),
		replay:  frame, native: r, nativeNonce: nonce,
	}, nil
}

// Keep the existing watched-frame lifetime open after the finite command/input.
// A real file descriptor avoids an exec stdin copier waiting forever for EOF.
// Closing the controller or SSH stream revokes the foreground worker lifetime.
func nativeCommandInput(cmd *exec.Cmd, frame io.Reader) (func() error, error) {
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdin = reader
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(writer, frame)
		if err != nil {
			_ = writer.Close()
		}
		done <- err
	}()
	return func() error {
		_ = reader.Close()
		_ = writer.Close()
		return <-done
	}, nil
}

func (p *sshTransportPreparation) runNative(ctx context.Context, target SSHTarget, connectTimeout, attempts string, stdout, stderr io.Writer) error {
	// No port or mux retry after dispatch: this nonce owns one invocation.
	if nativeRuntimeRouteKey(target) != nativeRuntimeRouteKey(p.native.target) {
		return errors.New("native runtime command target changed")
	}
	// The controller alone holds the watched pipe's writer. A mux master only
	// receives its reader, so controller exit still sends EOF on this channel.
	_, runErr := p.runOnce(ctx, target, connectTimeout, attempts, stdout, stderr, false)
	if !p.dispatched {
		return errors.Join(runErr, context.Cause(ctx))
	}
	// Even a normal SSH exit is not a retained-stage retirement receipt.
	p.retirementUnconfirmed = true
	// The existing full cleanup allowance covers TERM/KILL grace plus both
	// receipt collection and retirement, including when the caller cancels.
	cleanupCtx, cancel := wslStageCleanupContext(ctx, wslStageCleanupTimeout, wslStageCleanupTimeout)
	defer cancel()
	action := "observe"
	if runErr != nil || ctx.Err() != nil {
		action = "cancel"
	}
	record := newSynchronizedBuffer(remoteruntime.CompletionLimit + 1)
	// Each control validates the nonce-bound completion itself. A single SSH
	// exit acknowledges both operations; lost output or exit remains ambiguous.
	settle := p.native.command("control", remoteruntime.Protocol, p.nativeNonce, action) + " && " +
		p.native.command("control", remoteruntime.Protocol, p.nativeNonce, "retire")
	cleanupErr := p.native.runPOSIX(cleanupCtx, settle, &record)
	if cleanupErr == nil {
		_, cleanupErr = remoteruntime.ParseCompletion(record.Bytes(), p.nativeNonce)
	}
	if cleanupErr != nil {
		// Absence is not settlement: an accepted SSH wrapper may not have
		// started the runtime yet. Preserve its executable and exact locator.
		reason := fmt.Errorf("command stage /tmp/crabbox-command-%s retirement unconfirmed: %w", p.nativeNonce, cleanupErr)
		if scope, ok := ctx.Value(nativeRuntimeScopeKey{}).(*nativeRuntimeScope); ok {
			scope.retain(p.native, reason)
		}
		return errors.Join(runErr, context.Cause(ctx), reason)
	}
	p.retirementUnconfirmed = false
	return errors.Join(runErr, context.Cause(ctx))
}
