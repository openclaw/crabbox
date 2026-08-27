package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/openclaw/crabbox/internal/runner"
	"github.com/openclaw/crabbox/internal/runner/runnerfs"
)

func newResolvedRunnerClient(ctx context.Context, session *sshTransportSession, target SSHTarget, stderr io.Writer) (*runner.Client, error) {
	var probe bytes.Buffer
	bounded := &runnerProbeWriter{writer: &probe, remaining: 4096}
	if err := runResolvedRunnerCommand(ctx, session, target, runner.ProbeCommand(isWindowsNativeTarget(target)), nil, bounded, stderr, nil); err != nil {
		return nil, fmt.Errorf("probe remote runner platform: %w", err)
	}
	platform, err := runner.ParseProbe(probe.String())
	if err != nil {
		return nil, err
	}
	artifact, err := runner.ArtifactFor(ctx, platform.Target)
	if err != nil {
		return nil, err
	}
	install, err := runner.PrepareInstallation(platform, artifact)
	if err != nil {
		return nil, err
	}
	if err := runResolvedRunnerCommand(ctx, session, target, install.Command, bytes.NewReader(install.Input), io.Discard, stderr, nil); err != nil {
		return nil, fmt.Errorf("install verified remote runner: %w", err)
	}
	textOnly := isWindowsNativeTarget(target) || isWindowsWSL2Target(target)
	command, err := runner.InvokeCommand(platform, artifact, textOnly)
	if err != nil {
		return nil, err
	}
	transport := runner.Transport(func(ctx context.Context, input io.Reader, output io.Writer) error {
		var boundedCommand func(int64) (string, error)
		if isWindowsNativeTarget(target) {
			boundedCommand = func(size int64) (string, error) {
				return runner.InvokeCommandWithInputSize(platform, artifact, true, size)
			}
		}
		return runResolvedRunnerCommand(ctx, session, target, command, input, output, stderr, boundedCommand)
	})
	if textOnly {
		transport = runner.Base64Transport(transport)
	}
	return &runner.Client{Identity: artifact.Identity, Transport: transport}, nil
}

type runnerProbeWriter struct {
	writer    io.Writer
	remaining int
}

func (w *runnerProbeWriter) Write(data []byte) (int, error) {
	if len(data) > w.remaining {
		return 0, errors.New("runner platform probe exceeds output limit")
	}
	n, err := w.writer.Write(data)
	w.remaining -= n
	return n, err
}

// Keep the existing workspace witness around the helper until its process
// ownership protocol has migrated. Private resolved SSH config also keeps
// provider token usernames and proxy policy out of process arguments.
func runResolvedRunnerCommand(ctx context.Context, session *sshTransportSession, target SSHTarget, remote string, input io.Reader, output, stderr io.Writer, commandForInput func(int64) (string, error)) (err error) {
	var size int64
	var sizePointer *int64
	if input != nil {
		sizePointer = &size
	}
	var spool *os.File
	if input != nil && (isWindowsNativeTarget(target) || isWindowsWSL2Target(target)) {
		spool, err = os.CreateTemp("", "crabbox-runner-input-*")
		if err != nil {
			return err
		}
		defer func() { _ = spool.Close(); _ = os.Remove(spool.Name()) }()
		limit := runner.MaxRequestBytes()
		limited := &io.LimitedReader{R: input, N: limit + 1}
		if err := runnerfs.Copy(ctx, spool, limited); err != nil {
			return err
		}
		if limited.N == 0 {
			return errors.New("runner request exceeds input limit")
		}
		size = limit + 1 - limited.N
		if _, err := spool.Seek(0, io.SeekStart); err != nil {
			return err
		}
		input = spool
	}
	if commandForInput != nil {
		remote, err = commandForInput(size)
		if err != nil {
			return err
		}
	}
	prepared, err := prepareWorkspaceOwnerRemote(ctx, target, remote, sizePointer)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, prepared.close(ctx, target))
		}
	}()
	command := wrapRemoteForTarget(target, prepared.command)
	if spool != nil {
		transport, err := prepareSSHTransport(target, prepared, nil, spool, size, true, 0)
		if err != nil {
			return err
		}
		defer transport.close()
		command = transport.command
		input, err = transport.reset()
		if err != nil {
			return err
		}
	}
	args := append(session.commandPrefix(), "-T", "--", session.host(), command)
	handle := pondMeshExecCommand(ctx, target.ChildEnvDenylist, directSSHExecutable(), args...)
	execHandle, ok := handle.(*pondMeshExecHandle)
	if !ok {
		return errors.New("resolved SSH runner transport does not expose process streams")
	}
	tail := newSynchronizedTailBuffer(failureTailLines)
	execHandle.cmd.Stdin, execHandle.cmd.Stdout, execHandle.cmd.Stderr = input, output, tail
	if err := handle.Start(); err != nil {
		return fmt.Errorf("start remote runner transport: %w", err)
	}
	err = handle.Wait()
	if stderr != nil {
		writeSSHTransportDiagnostic(stderr, target, tail.String())
	}
	if err != nil && context.Cause(ctx) != nil && handle.WasTerminatedByOurCancel() {
		return context.Cause(ctx)
	}
	if err != nil {
		if detail := strings.TrimSpace(redactSSHTransportDiagnostic(target, tail.String())); detail != "" {
			return fmt.Errorf("remote runner transport: %w: %s", err, detail)
		}
	}
	return err
}
