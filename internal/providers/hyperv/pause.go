package hyperv

import (
	"context"
	"fmt"
	"github.com/openclaw/crabbox/internal/providers/shared"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	hypervStateRunning = 2
	hypervStateStopped = 3
	hypervStateSaved   = 6
	hypervStatePaused  = 9
)

var _ core.PausableBackend = (*backend)(nil)

func hypervLeaseState(state int) string {
	switch state {
	case hypervStateSaved, hypervStatePaused:
		return "paused"
	default:
		return hypervState(state)
	}
}

func (b *backend) Pause(ctx context.Context, req core.PauseRequest) error {
	inst, claim, err := b.resolveInstance(ctx, req.ID)
	if err != nil {
		return err
	}
	if err := requireHyperVLeaseState(inst, claim, "pause"); err != nil {
		return err
	}

	switch inst.State {
	case hypervStateSaved:
		fmt.Fprintf(b.rt.Stderr, "paused lease=%s instance=%s state=saved\n", claim.LeaseID, inst.Name)
		return nil
	case hypervStateRunning, hypervStatePaused:
		script := fmt.Sprintf(`Save-VM -Name '%s' -ErrorAction Stop`, escapePSString(inst.Name))
		if err := core.WithLeaseClaimUnchangedContext(ctx, claim.LeaseID, claim, func() error {
			result, saveErr := b.powershell(ctx, script)
			if saveErr != nil {
				return shared.LocalCommandError("Save-VM", result, saveErr)
			}
			return nil
		}); err != nil {
			return err
		}
		fmt.Fprintf(b.rt.Stderr, "paused lease=%s instance=%s state=saved\n", claim.LeaseID, inst.Name)
		return nil
	case hypervStateStopped:
		return core.Exit(4, "hyperv lease %q VM %q is stopped and cannot be paused", claim.LeaseID, inst.Name)
	default:
		return core.Exit(4, "hyperv lease %q VM %q is in unsupported state %s", claim.LeaseID, inst.Name, hypervState(inst.State))
	}
}

func (b *backend) Resume(ctx context.Context, req core.ResumeRequest) error {
	inst, claim, err := b.resolveInstance(ctx, req.ID)
	if err != nil {
		return err
	}
	if err := requireHyperVLeaseState(inst, claim, "resume"); err != nil {
		return err
	}

	switch inst.State {
	case hypervStateSaved:
		if err := core.WithLeaseClaimUnchangedContext(ctx, claim.LeaseID, claim, func() error { return b.startVM(ctx, inst.Name) }); err != nil {
			return err
		}
	case hypervStatePaused:
		if err := core.WithLeaseClaimUnchangedContext(ctx, claim.LeaseID, claim, func() error { return b.runVMStateCommand(ctx, "Resume-VM", inst.Name) }); err != nil {
			return err
		}
	case hypervStateRunning:
		// Already resumed. Refresh readiness and endpoint below in case DHCP changed.
	case hypervStateStopped:
		return core.Exit(4, "hyperv lease %q VM %q is stopped and cannot be resumed", claim.LeaseID, inst.Name)
	default:
		return core.Exit(4, "hyperv lease %q VM %q is in unsupported state %s", claim.LeaseID, inst.Name, hypervState(inst.State))
	}

	lease, err := b.waitForResumedLease(ctx, inst.Name, claim)
	if err != nil {
		return err
	}
	if err := updateLeaseClaimEndpointIfUnchanged(claim.LeaseID, claim, lease.Server, lease.SSH); err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stderr, "resumed lease=%s instance=%s host=%s\n", claim.LeaseID, inst.Name, lease.SSH.Host)
	return nil
}

func (b *backend) waitForResumedLease(ctx context.Context, name string, claim core.LeaseClaim) (core.LeaseTarget, error) {
	cfg := b.configForRun()
	timeout := core.BootstrapWaitTimeout(cfg)
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if ctx.Err() != nil {
			return core.LeaseTarget{}, context.Cause(ctx)
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return core.LeaseTarget{}, core.Exit(5, "hyperv VM %s did not reach a stable SSH endpoint within %s: %v", name, timeout, lastErr)
			}
			return core.LeaseTarget{}, core.Exit(5, "hyperv VM %s did not reach a stable SSH endpoint within %s", name, timeout)
		}

		ip := b.queryLiveIP(ctx, name)
		if ip == "" {
			if err := b.waitResumePoll(ctx); err != nil {
				return core.LeaseTarget{}, err
			}
			continue
		}
		lease, err := b.prepareLease(ctx, cfg, hypervVM{Name: name, State: hypervStateRunning}, ip, claim, false)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		probeTimeout := minDuration(b.resumeSSHProbeTimeout, time.Until(deadline))
		if err := core.WithLeaseClaimUnchangedContext(ctx, claim.LeaseID, claim, func() error {
			return b.sshReady(ctx, &lease.SSH, b.rt.Stderr, "hyperv resume", probeTimeout)
		}); err != nil {
			if strings.Contains(err.Error(), "claim changed") {
				return core.LeaseTarget{}, err
			}
			lastErr = err
			if err := b.waitResumePoll(ctx); err != nil {
				return core.LeaseTarget{}, err
			}
			continue
		}
		stable, err := b.resumeIPRemainsStable(ctx, name, ip, deadline)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		if !stable {
			lastErr = fmt.Errorf("IP changed after SSH became ready")
			continue
		}
		lease.Server.Status = "ready"
		lease.Server.Labels["state"] = "ready"
		return lease, nil
	}
}

func (b *backend) resumeIPRemainsStable(ctx context.Context, name, expectedIP string, deadline time.Time) (bool, error) {
	stableUntil := time.Now().Add(b.resumeIPStableWindow)
	if stableUntil.After(deadline) {
		stableUntil = deadline
	}
	for {
		if ctx.Err() != nil {
			return false, context.Cause(ctx)
		}
		if current := b.queryLiveIP(ctx, name); current != expectedIP {
			return false, nil
		}
		if !time.Now().Before(stableUntil) {
			return true, nil
		}
		if err := b.waitResumePoll(ctx); err != nil {
			return false, err
		}
	}
}

func (b *backend) waitResumePoll(ctx context.Context) error {
	if b.resumePollInterval <= 0 {
		return nil
	}
	timer := time.NewTimer(b.resumePollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return nil
	}
}

func minDuration(a, b time.Duration) time.Duration {
	if a <= 0 {
		return b
	}
	if b <= 0 || b < a {
		return b
	}
	return a
}

func (b *backend) runVMStateCommand(ctx context.Context, command, name string) error {
	script := fmt.Sprintf(`%s -Name '%s' -ErrorAction Stop`, command, escapePSString(name))
	result, err := b.powershell(ctx, script)
	if err != nil {
		return shared.LocalCommandError(command, result, err)
	}
	return nil
}

func requireHyperVLeaseState(inst hypervVM, claim core.LeaseClaim, action string) error {
	if inst.State == hypervMissingState {
		return core.Exit(4, "hyperv VM %q from lease %q no longer exists", inst.Name, claim.LeaseID)
	}
	if strings.TrimSpace(claim.LeaseID) == "" {
		return core.Exit(4, "hyperv instance %q has no Crabbox lease claim; refusing to %s it", inst.Name, action)
	}
	return requireExactHyperVClaimFor(claim.LeaseID, inst.Name, action)
}
