package islo

import (
	"strconv"
	"time"

	gosdk "github.com/islo-labs/go-sdk"
	core "github.com/openclaw/crabbox/internal/cli"
)

// Idle pausing is opt-in because provider activity semantics may interrupt a
// long-running command. Never add a deletion deadline or automatic resume here.
func isloLifecycleForConfig(cfg core.Config) *gosdk.LifecyclePolicy {
	if !cfg.Islo.IdlePause {
		return nil
	}
	seconds := durationSecondsCeil(cfg.IdleTimeout)
	if seconds <= 0 {
		return nil
	}
	return &gosdk.LifecyclePolicy{
		PauseAfterIdle: &seconds,
		AutoResume:     gosdk.AutoResumePolicyNever.Ptr(),
	}
}

// This check applies only to explicit reclaim with idle pausing enabled; it
// neither rewrites an existing policy nor adds policy requirements to Stop.
func isloLifecycleConflict(name string, sandbox *gosdk.SandboxResponse, cfg core.Config) error {
	if !cfg.Islo.IdlePause {
		return nil
	}
	actual := sandbox.GetLifecycle()
	if actual == nil {
		// Legacy responses without policy metadata remain adoptable. This is
		// not confirmation that the configured idle timeout is enforced.
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
	return core.Exit(2, "islo sandbox %q has immutable lifecycle pause_after_idle=%s but this run asks for %s; islo cannot change lifecycle after create, so reuse it with a matching --idle-timeout or create a new lease",
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

func durationSecondsCeil(duration time.Duration) int64 {
	if duration <= 0 {
		return 0
	}
	// Subtract before rounding so the maximum positive duration cannot wrap.
	return int64((duration-1)/time.Second) + 1
}
