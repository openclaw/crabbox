package cli

import "context"

type runLeaseHandoff struct {
	lease     LeaseTarget
	published bool
}

// runCommandWithLeaseObserver reports only authority carried out of this run.
// The observer runs after the entire pipeline returns, including deferred ready
// touches, activity/heartbeat shutdown and runtime finalization, even on failure.
func (a App) runCommandWithLeaseObserver(ctx context.Context, args []string, observe func(LeaseTarget)) error {
	if observe == nil {
		return a.runCommand(ctx, args)
	}
	var final runLeaseHandoff
	a.runLeaseTarget = &final
	err := a.runCommand(ctx, args)
	if final.published {
		observe(final.lease)
	}
	return err
}

func (a App) recordRunLeaseTarget(lease LeaseTarget) {
	// Resolved/prepared state is not publication authority. In particular, a
	// failed admission must not turn a read-only lookup into a cleanup handoff.
	if a.runLeaseTarget == nil || !a.runLeaseTarget.published || lease.LeaseID != a.runLeaseTarget.lease.LeaseID {
		return
	}
	a.captureRunLeaseTarget(lease)
}

// recordRunLeasePublication is called only at successful core commit boundaries,
// before post-publication registration or setup can fail. Snapshot presence or a
// difference between current-store reads is not such a receipt.
func (a App) recordRunLeasePublication(lease LeaseTarget) {
	if a.runLeaseTarget == nil {
		return
	}
	if a.captureRunLeaseTarget(lease) {
		a.runLeaseTarget.published = true
	}
}

func (a App) captureRunLeaseTarget(lease LeaseTarget) bool {
	if lease.LeaseID == "" {
		return false
	}
	claim, exists, set := ServerLeaseClaimSnapshot(lease.Server)
	if !set || !exists || claim.LeaseID != lease.LeaseID {
		return false
	}
	a.runLeaseTarget.lease = cloneRunLeaseTarget(lease)
	return true
}

func cloneRunLeaseTarget(lease LeaseTarget) LeaseTarget {
	lease.Server.Labels = cloneStringMap(lease.Server.Labels)
	claim, exists, set := ServerLeaseClaimSnapshot(lease.Server)
	if set {
		SetServerLeaseClaimSnapshot(&lease.Server, claim, exists)
	}
	return lease
}
