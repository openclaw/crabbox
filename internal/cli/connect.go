package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"
)

func (a App) connect(ctx context.Context, args []string) error {
	resolved, err := a.resolveSSHCommandTarget(ctx, "connect", args, false)
	if err != nil {
		return err
	}
	stopLeaseActivity := a.startInteractiveSSHLeaseActivity(ctx, resolved.Config, resolved.Lease)
	defer stopLeaseActivity()
	return runInteractiveSSH(ctx, resolved.Lease.SSH, a.input(), a.Stdout, a.Stderr)
}

func (a App) startInteractiveSSHLeaseActivity(ctx context.Context, cfg Config, lease LeaseTarget) func() {
	if lease.LeaseID == "" {
		return func() {}
	}
	var stopHeartbeat func()
	coord := lease.Coordinator
	if coord == nil && shouldRegisterCoordinatorLease(cfg) {
		var err error
		coord, _, err = newTargetCoordinatorClient(cfg)
		if err != nil {
			fmt.Fprintf(a.Stderr, "warning: coordinator heartbeat disabled for %s: %v\n", lease.LeaseID, err)
		}
	}
	if coord != nil {
		var telemetry leaseTelemetryCollector
		if !lease.SSH.AuthSecret {
			telemetry = leaseTelemetryCollectorForTarget(lease.SSH)
		}
		heartbeatIdleTimeout := cfg.IdleTimeout
		if resolved, ok := parseDurationSecondsLabel(lease.Server.Labels["idle_timeout_secs"]); ok {
			heartbeatIdleTimeout = resolved
		}
		stopHeartbeat = startCoordinatorHeartbeat(ctx, coord, lease.LeaseID, heartbeatIdleTimeout, nil, telemetry, a.Stderr)
	}
	backend, err := loadBackend(cfg, runtimeForApp(a))
	if err != nil {
		fmt.Fprintf(a.Stderr, "warning: direct touch state=running: %v\n", err)
		return stopInteractiveSSHHeartbeat(stopHeartbeat)
	}
	if backendCoordinator(backend) != nil {
		return stopInteractiveSSHHeartbeat(stopHeartbeat)
	}
	sshBackend, ok := backend.(LeaseTouchBackend)
	if !ok {
		fmt.Fprintf(a.Stderr, "warning: provider=%s does not support lease touch\n", backend.Spec().Name)
		return stopInteractiveSSHHeartbeat(stopHeartbeat)
	}
	if touched, touchErr := sshBackend.Touch(ctx, TouchRequest{Lease: lease, State: "running", IdleTimeout: cfg.IdleTimeout}); touchErr == nil {
		lease.Server = touched
	} else {
		fmt.Fprintf(a.Stderr, "warning: direct touch state=running: %v\n", touchErr)
	}
	return func() {
		if stopHeartbeat != nil {
			stopHeartbeat()
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if touched, touchErr := sshBackend.Touch(cleanupCtx, TouchRequest{Lease: lease, State: "ready", IdleTimeout: cfg.IdleTimeout}); touchErr == nil {
			lease.Server = touched
		} else {
			fmt.Fprintf(a.Stderr, "warning: direct touch state=ready: %v\n", touchErr)
		}
	}
}

func stopInteractiveSSHHeartbeat(stopHeartbeat func()) func() {
	return func() {
		if stopHeartbeat != nil {
			stopHeartbeat()
		}
	}
}

func runInteractiveSSH(ctx context.Context, target SSHTarget, stdin io.Reader, stdout, stderr io.Writer) error {
	target.FallbackPorts = nil
	return runInteractiveSSHOnce(ctx, target, stdin, stdout, stderr)
}

func probeConnectSSHTransport(ctx context.Context, target *SSHTarget, timeout time.Duration) bool {
	for _, port := range sshPortCandidates(target.Port, target.FallbackPorts) {
		probeCtx, cancel := context.WithTimeout(ctx, timeout)
		probe := *target
		probe.Port = port
		probe.FallbackPorts = []string{}
		err := runSSHQuietWithOptions(probeCtx, probe, sshTransportProbeCommand(probe), "2", "1")
		cancel()
		if err == nil {
			target.Port = port
			return true
		}
		if ctx.Err() != nil {
			return false
		}
	}
	return false
}

func runInteractiveSSHOnce(ctx context.Context, target SSHTarget, stdin io.Reader, stdout, stderr io.Writer) error {
	args := append(sshBaseArgs(target), target.User+"@"+target.Host)
	cmd := sshCommandContext(ctx, target, args...)
	cmd.Stdin = stdin
	err := runSSHCommand(cmd, stdout, stderr)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		var commandExit *exec.ExitError
		if errors.As(err, &commandExit) {
			code := exitCode(err)
			if code < 1 {
				code = 1
			}
			return ExitError{Code: code}
		}
		return fmt.Errorf("start ssh: %w", err)
	}
	return nil
}
