package shared

import (
	"context"
	"io"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// PrepareSSHWithBootstrap probes authentication before installing tools, then
// checks the original readiness command using the port resolved by the probe.
func PrepareSSHWithBootstrap(ctx context.Context, cfg core.Config, target *core.SSHTarget, stderr io.Writer, name, command string) error {
	probe := *target
	probe.ReadyCheck = "true"
	phase := strings.ToLower(name)
	if err := core.WaitForSSHReady(ctx, &probe, stderr, phase+" ssh", core.BootstrapWaitTimeout(cfg)); err != nil {
		return err
	}
	target.Port = probe.Port
	if err := core.RunSSHQuiet(ctx, *target, command); err != nil {
		return core.Exit(1, "%s tool bootstrap failed: %v", name, err)
	}
	return core.WaitForSSHReady(ctx, target, stderr, phase+" tools", core.BootstrapWaitTimeout(cfg))
}

// RetryLeaseLookup retries transient lookup errors at the recovery cadence.
// The first lookup precedes cancellation checks; a deadline returns its last error.
func RetryLeaseLookup[T any](ctx context.Context, cfg core.Config, leaseID string, timeout time.Duration, find func(context.Context, core.Config, string) (T, error)) (T, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var zero T
	for {
		item, err := find(ctx, cfg, leaseID)
		if err == nil {
			return item, nil
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			return zero, err
		case <-ctx.Done():
			return zero, ctx.Err()
		}
	}
}
