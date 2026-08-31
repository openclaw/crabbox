package islo

import (
	"strconv"
	"time"

	gosdk "github.com/islo-labs/go-sdk"
)

// isloLifecycleForConfig maps the generic Crabbox lease lifecycle onto Islo's
// create-time lifecycle policy. Only the non-destructive knob is mapped: the
// configured idle timeout becomes pause_after_idle, asking Islo to pause a
// sandbox that has been idle that long instead of leaving it billing.
//
// This is on by default, not opt-in. The lease idle timeout defaults to 30m and
// internal/cli/lease_flags.go rejects a non-positive one, so every CLI-driven
// islo create now carries an idle pause where it previously sent no lifecycle
// at all; the seconds <= 0 guard below only fires for a Config built in-process.
//
// delete_after is deliberately never sent, even though --ttl would fit its
// shape. Handing a deletion deadline to the provider lets Islo destroy a sandbox
// that Crabbox still holds a lease claim on, possibly mid-run, leaving the lease
// pointing at something that has simply vanished. The closest in-tree precedent
// makes the same call in the other direction:
// internal/providers/daytona/lifecycle.go pins the auto-delete interval off so
// Crabbox stays the only thing that deletes a Crabbox lease. --ttl is therefore
// not communicated to Islo at all and has no provider-side effect here.
//
// auto_resume is pinned to "never" so that resuming stays Crabbox's decision: an
// explicit `crabbox pause` is not undone behind the user's back, and Crabbox
// resumes a paused sandbox itself before it drives one (see Run's reuse path and
// resolveRunningSandbox). Resuming is also billable - an exec against a paused
// sandbox on a tenant with no credit is refused with 402 BILLING_NOT_ALLOWED,
// "Insufficient credit balance to resume a sandbox" - so it is a decision worth
// making deliberately rather than inheriting as a side effect of some request.
//
// pause_after_idle is enforced by Islo, not merely echoed back: a sandbox
// created with pause_after_idle=60 still reported "running" at 75s and reported
// "paused" at 90s, so the policy does fire, with some scheduler lag past the
// nominal window. That same observation polled GET /sandboxes every 15s
// throughout without holding the pause off, so control-plane reads are not
// activity.
//
// UNKNOWN, and not a safe unknown: what else Islo counts as activity is not
// documented. If an exec that is still running, or in-VM traffic to a published
// share or a tailnet peer, does not hold the idle clock off, then a `crabbox
// run` longer than --idle-timeout can be paused mid-exec, and a warm lease
// serving a share can be paused after 30m without a Crabbox call. Paths that
// resolve the lease first recover, because they resume before driving the
// sandbox; PublishPeer and fetchRunFileAs do not resolve, so they surface
// Islo's error and `crabbox resume` is the fix. Crabbox cannot detect the case
// on its own - nothing in the API reports why a sandbox paused - so raising
// --idle-timeout past the longest expected run is the only mitigation.
//
// pause_after (an absolute pause deadline regardless of activity) is left unset:
// Crabbox has no generic config for it, so Islo's tenant default applies rather
// than a Crabbox-invented flag.
func isloLifecycleForConfig(cfg Config) *gosdk.LifecyclePolicy {
	seconds := durationSecondsCeil(cfg.IdleTimeout)
	if seconds <= 0 {
		return nil
	}
	return &gosdk.LifecyclePolicy{
		PauseAfterIdle: &seconds,
		AutoResume:     gosdk.AutoResumePolicyNever.Ptr(),
	}
}

// isloLifecycleConflict refuses to adopt a sandbox whose immutable idle-pause
// policy does not match what the current config asks for.
//
// Islo fixes the lifecycle policy at create time: neither plane exposes a
// lifecycle update, so a changed --idle-timeout can never be pushed onto a live
// sandbox. Adopting the lease anyway would leave the caller believing an idle
// timeout that is not in force, so the mismatch is surfaced as a conflict.
//
// Only pause_after_idle is compared. auto_resume is not: Crabbox does not derive
// it from config and does not depend on it, since every Crabbox path that
// resolves a lease resumes the sandbox explicitly first.
func isloLifecycleConflict(name string, sandbox *gosdk.SandboxResponse, cfg Config) error {
	actual := sandbox.GetLifecycle()
	if actual == nil {
		// Sandboxes created before Crabbox sent a lifecycle (or by another tool)
		// report none, so there is nothing authoritative to compare against.
		return nil
	}
	var want *int64
	if desired := isloLifecycleForConfig(cfg); desired != nil {
		want = desired.PauseAfterIdle
	}
	got := actual.PauseAfterIdle
	if isloLifecycleSecondsEqual(want, got) {
		return nil
	}
	return exit(2, "islo sandbox %q has immutable lifecycle pause_after_idle=%s but this run asks for %s; islo cannot change lifecycle after create, so reuse it with a matching --idle-timeout or create a new lease",
		name, isloLifecycleSecondsText(got), isloLifecycleSecondsText(want))
}

func isloLifecycleSecondsEqual(want, got *int64) bool {
	if want == nil || got == nil {
		return want == nil && got == nil
	}
	return *want == *got
}

func isloLifecycleSecondsText(value *int64) string {
	if value == nil {
		return "unset"
	}
	return strconv.FormatInt(*value, 10)
}

// durationSecondsCeil rounds up on purpose. Truncating (as the share TTL path in
// client.go does) would turn a sub-second idle timeout into pause_after_idle=0,
// and 0 is not a value to guess the meaning of.
func durationSecondsCeil(duration time.Duration) int64 {
	if duration <= 0 {
		return 0
	}
	return int64((duration + time.Second - 1) / time.Second)
}
