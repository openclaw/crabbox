package shared

import (
	"context"
	"fmt"

	core "github.com/openclaw/crabbox/internal/cli"
)

type DirectCleanupAction uint8

const (
	DeleteCleanupServer DirectCleanupAction = iota + 1
	ResumeCleanupServer
	ForgetMissingCleanupServer
)

// DirectCleanupDecision follows adapter-owned, read-only eligibility checks.
// Mutation includes provider preparation, recovery writes, and key cleanup;
// none of those may run while a cleanup decision is only being previewed.
type DirectCleanupDecision struct {
	Action DirectCleanupAction
	Server core.Server
	Claim  core.LeaseClaim
	Mutate func(context.Context) error
}

func (d DirectCleanupDecision) Apply(ctx context.Context, req core.CleanupRequest, rt core.Runtime) error {
	switch d.Action {
	case DeleteCleanupServer, ResumeCleanupServer:
		if d.Mutate == nil {
			return core.Exit(2, "server cleanup decision has no mutation capability")
		}
		action := "delete"
		if d.Action == ResumeCleanupServer {
			action = "resume cleanup"
		}
		fmt.Fprintf(rt.Stderr, "%s server id=%s name=%s\n", action, d.Server.DisplayID(), d.Server.Name)
	case ForgetMissingCleanupServer:
		if d.Claim.LeaseID == "" || d.Mutate != nil {
			return core.Exit(2, "missing-server cleanup decision requires an exact claim and no provider mutation")
		}
		fmt.Fprintf(rt.Stderr, "remove claim lease=%s slug=%s reason=live instance no longer exists\n", d.Claim.LeaseID, core.Blank(d.Claim.Slug, "-"))
	default:
		return core.Exit(2, "invalid server cleanup decision")
	}
	if req.DryRun {
		return nil
	}
	if d.Action == ForgetMissingCleanupServer {
		return RemoveSSHLeaseClaimAfter(ctx, d.Claim, nil)
	}
	return d.Mutate(ctx)
}

// RemoveSSHLeaseClaimAfter waits for the unchanged-claim lock with ctx, then holds
// it through provider cleanup, SSH cleanup and claim removal. The action must
// honor ctx itself; successful deletion still finalizes artifacts after cancellation.
// A nil action requires confirmed absence.
func RemoveSSHLeaseClaimAfter(ctx context.Context, claim core.LeaseClaim, action func() error) error {
	return core.CleanupLeaseClaimIfUnchangedAfterContext(ctx, claim.LeaseID, claim, true, func() error {
		if action != nil {
			if err := action(); err != nil {
				return err
			}
		}
		if err := core.RemoveStoredTestboxConnectionArtifacts(claim.LeaseID); err != nil {
			return fmt.Errorf("remove SSH connection artifacts for lease %s: %w", claim.LeaseID, err)
		}
		return nil
	})
}
