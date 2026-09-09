package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

// Older coordinators bootstrap stock images without Node. Complete that same
// managed baseline from the client before publishing a ready lease.
func bootstrapManagedMacOS(ctx context.Context, cfg Config, target *SSHTarget, stderr io.Writer) error {
	initial := *target
	initial.ReadyCheck = sshReadyCommand(SSHTarget{})
	if err := waitForSSHReady(ctx, &initial, stderr, "macOS bootstrap", bootstrapWaitTimeout(cfg)); err != nil {
		return err
	}
	fmt.Fprintln(stderr, "checking macOS Node baseline over SSH")
	started := time.Now()
	if err := runSSHInput(ctx, initial, "sudo -n /bin/bash -s", strings.NewReader(sharedMacOSNodeInstall()), stderr, stderr); err != nil {
		return fmt.Errorf("macOS Node baseline: %w", err)
	}
	fmt.Fprintf(stderr, "macOS Node baseline complete in %s\n", time.Since(started).Round(time.Millisecond))
	return waitForSSHReady(ctx, target, stderr, "bootstrap", bootstrapWaitTimeout(cfg))
}
