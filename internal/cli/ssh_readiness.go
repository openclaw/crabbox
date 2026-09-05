package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

var errSSHHostKeyVerification = errors.New("SSH host-key verification failed; verify the lease identity and its SSH host trust before reconnecting")

func runSSHReadinessProbe(ctx context.Context, target SSHTarget, remote, connectTimeout, attempts string) error {
	diagnostic := synchronizedBuffer{limit: 64 * 1024}
	err := executeSSH(ctx, &target, remote, nil, 0, 0, connectTimeout, attempts, io.Discard, &diagnostic)
	return sshReadinessProbeError(ctx, err, diagnostic.String())
}

func sshReadinessProbeError(ctx context.Context, err error, diagnostic string) error {
	// A host-key rejection cannot recover while waiting for guest bootstrap.
	// Retain the exit cause, but never expose captured remote stderr or key data.
	if ctx.Err() == nil && exitCode(err) == 255 && strings.Contains(diagnostic, "Host key verification failed.") {
		return errors.Join(errSSHHostKeyVerification, err)
	}
	return err
}

func sshReadinessError(err error, phase string) error {
	if errors.Is(err, errSSHHostKeyVerification) {
		return fmt.Errorf("%s: %w", phase, err)
	}
	return workspaceOwnerReadinessError(err, phase)
}
