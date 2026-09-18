package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"

	"github.com/openclaw/crabbox/internal/runner"
	"github.com/openclaw/crabbox/internal/runner/runnerfs"
)

func recoverLocalArchive(ctx context.Context, destination string, choice runnerfs.ArchiveRecoveryChoice, stdout io.Writer) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	retained, err := runnerfs.AdoptArchiveRecovery(destination, choice)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Recovery complete; retained data: %q\n", retained)
	return err
}

func recoverRemoteArchive(ctx context.Context, target SSHTarget, destination string, choice runnerfs.ArchiveRecoveryChoice, stdout, stderr io.Writer) (err error) {
	if isWindowsNativeTarget(target) {
		return Exit(2, "archive recovery requires a POSIX lease")
	}
	scope, err := newFilesystemRuntimeScope(runner.DevelopmentSource())
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, scope.finish(context.WithoutCancel(ctx))) }()
	client, err := scope.filesystemClient(ctx, target, stderr)
	if err != nil {
		return err
	}
	retained, err := client.RecoverArchive(ctx, destination, choice)
	if err != nil {
		return err
	}
	if err := scope.finish(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("recovery applied; retained data: %q; runtime cleanup: %w", retained, err)
	}
	_, err = fmt.Fprintf(stdout, "Recovery complete on lease; retained data: %q\n", retained)
	return err
}

func archiveRecoveryGuidance(err error, lease string) error {
	if err == nil {
		return nil
	}
	var local *runnerfs.ArchiveAdoptionRequiredError
	var remote runner.RemoteError
	target, remoteTarget := "", false
	if errors.As(err, &local) {
		target = local.Target
	} else if errors.As(err, &remote) && remote.Code == "recovery-required" {
		target, remoteTarget = "SANDBOX:"+remote.Target, true
	} else {
		return err
	}
	quote := shellQuote
	if runtime.GOOS == "windows" {
		quote = psQuote
	}
	command := func(choice string) string {
		cmd := "crabbox cp --recover " + choice
		if remoteTarget {
			cmd += " --id " + quote(lease)
		}
		return cmd + " -- " + quote(target)
	}
	return fmt.Errorf("%w\nChoose the intended outcome; both commands retain unselected legacy data:\n  %s\n  %s", err, command("keep-destination"), command("restore-backup"))
}
