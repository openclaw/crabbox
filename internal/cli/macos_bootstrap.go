package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

// Complete older coordinators' missing user-session and Node setup before
// publishing a ready managed lease.
func bootstrapManagedMacOS(ctx context.Context, cfg Config, target *SSHTarget, stderr io.Writer) error {
	initial := *target
	// A pre-PAM control master retains the system launchd namespace. Bootstrap
	// uses disposable connections so readiness and commands get the user session.
	initial.NoControlMaster = true
	initial.ReadyCheck = sshReadyCommand(SSHTarget{})
	if err := waitForSSHReady(ctx, &initial, stderr, "macOS bootstrap", bootstrapWaitTimeout(cfg)); err != nil {
		return err
	}
	fmt.Fprintln(stderr, "checking macOS SSH session and Node baseline over SSH")
	started := time.Now()
	bootstrap := "set -eu\n(\n" + sharedMacOSSSHSession() + ")\n(\n" + sharedMacOSNodeInstall() + ")\n"
	if err := runSSHInput(ctx, initial, "sudo -n /bin/bash -s", strings.NewReader(bootstrap), stderr, stderr); err != nil {
		return fmt.Errorf("macOS session and Node baseline: %w", err)
	}
	fmt.Fprintf(stderr, "macOS session and Node baseline complete in %s\n", time.Since(started).Round(time.Millisecond))
	return waitForSSHReady(ctx, target, stderr, "bootstrap", bootstrapWaitTimeout(cfg))
}
