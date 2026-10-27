package ssh

import core "github.com/openclaw/crabbox/internal/cli"

// staticClaimCollisionHint explains how to share one static host between
// repositories without adopting another repository's lease.
const staticClaimCollisionHint = "hint: to share this static host concurrently, set a distinct static.id (CRABBOX_STATIC_ID); each id gets its own claim and workdir <workRoot>/<id>/<repo>. --reclaim takes over the other repository's lease and workdir"

// checkStaticLeaseClaimOwner applies the core repository owner policy and adds
// static-host guidance to a refusal, preserving its exit code and message.
func checkStaticLeaseClaimOwner(leaseID string, existing core.LeaseClaim, repoRoot string, reclaim bool) error {
	err := core.CheckLeaseClaimRepositoryOwner(leaseID, existing, repoRoot, reclaim)
	var exitErr core.ExitError
	if err == nil || !core.AsExitError(err, &exitErr) {
		return err
	}
	return core.ExitError{Code: exitErr.Code, Message: exitErr.Message + "\n" + staticClaimCollisionHint}
}
