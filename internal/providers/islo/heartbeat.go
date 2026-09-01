package islo

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	gosdk "github.com/islo-labs/go-sdk"
	core "github.com/openclaw/crabbox/internal/cli"
)

// isloHeartbeatCommand is the smallest command the exec endpoint can run. Islo
// exposes no dedicated heartbeat endpoint, so a heartbeat has to be an exec
// (POST /sandboxes/{name}/exec/stream). `true` exits 0 immediately and mutates
// nothing, which also makes the call safe to replay.
var isloHeartbeatCommand = []string{"true"}

// isloHeartbeatTimeoutSecs is the server-side command-runtime hint. The client
// side is bounded separately by isloHeartbeatTimeout, since the hint alone does
// not stop a hung stream from hanging a heartbeat loop forever.
const isloHeartbeatTimeoutSecs int64 = 10

// isloHeartbeatTimeout bounds each client call this heartbeat makes.
const isloHeartbeatTimeout = 30 * time.Second

var _ core.LeaseHeartbeatBackend = (*isloBackend)(nil)

// Heartbeat runs one no-op exec against the sandbox and reports the sandbox's
// own idle policy as Islo echoes it back.
//
// An exec is what registers sandbox activity: observed against a live tenant, a
// sandbox with lifecycle.pause_after_idle=60s and auto_resume=on_activity
// paused once its idle window elapsed, and an exec against that paused sandbox
// was accepted and left it running again. So an exec both counts as activity
// and resumes a paused sandbox.
//
// The reported idle window is read from the live sandbox rather than from
// Crabbox config: GET /sandboxes/{name} echoes lifecycle verbatim, so
// `pause_after_idle` is the only idle number that describes this lease. When the
// sandbox carries no such policy the result reports no idle timeout at all and
// the user is warned, because then there is no observable idle deadline for a
// heartbeat to defer.
//
// This path performs exactly two calls, a GET and an exec: it issues no create
// and no lifecycle write of any kind, so it cannot move any deadline
// (pause_after or delete_after) in either direction. It persists nothing
// either - LastTouchedAt below is the observation time, not a claim touch.
func (b *isloBackend) Heartbeat(ctx context.Context, req core.LeaseHeartbeatRequest) (core.LeaseHeartbeatResult, error) {
	client, err := newIsloClient(b.cfg, b.rt)
	if err != nil {
		return core.LeaseHeartbeatResult{}, err
	}
	leaseID, name, slug, err := resolveIsloLeaseID(req.ID, "", false)
	if err != nil {
		return core.LeaseHeartbeatResult{}, err
	}
	if err := requireIsloLeaseClaim(leaseID, "heartbeat"); err != nil {
		return core.LeaseHeartbeatResult{}, err
	}
	getCtx, cancelGet := context.WithTimeout(ctx, isloHeartbeatTimeout)
	sandbox, err := client.GetSandbox(getCtx, name)
	cancelGet()
	if err != nil {
		return core.LeaseHeartbeatResult{}, isloError("get sandbox", err)
	}
	if sandbox == nil {
		return core.LeaseHeartbeatResult{}, exit(4, "islo sandbox %s not found", name)
	}
	state := sandbox.GetStatus()
	if isloStatusTerminal(state) {
		return core.LeaseHeartbeatResult{}, exit(5, "islo sandbox %s is in terminal state=%s", name, state)
	}
	if !isloStatusReady(state) {
		// A paused sandbox is refused rather than exec'd: an exec against a
		// paused sandbox resumes it, and the resume is billed - on a tenant
		// with no credit the same call is rejected with HTTP 402
		// BILLING_NOT_ALLOWED "Insufficient credit balance to resume a
		// sandbox". Refusing here means a heartbeat can never be the thing
		// that starts billing compute. Anything else that is not yet running
		// has nothing to resume, so do not advise resuming it.
		hint := ""
		if strings.EqualFold(strings.TrimSpace(state), "paused") {
			hint = "; resume it before heartbeat"
		}
		return core.LeaseHeartbeatResult{}, exit(5, "islo sandbox %s is not running (state=%s)%s", name, blank(state, "unknown"), hint)
	}
	idleTimeout := isloPauseAfterIdle(sandbox)
	if idleTimeout <= 0 {
		fmt.Fprintf(b.rt.Stderr, "warning: islo sandbox %s reports no lifecycle.pause_after_idle, so this heartbeat has no idle deadline to defer\n", name)
	}
	timeoutSecs := isloHeartbeatTimeoutSecs
	execCtx, cancel := context.WithTimeout(ctx, isloHeartbeatTimeout)
	defer cancel()
	// Both streams are discarded: a heartbeat must not leak sandbox output into
	// the caller's terminal, so success is silent.
	code, err := client.ExecStream(execCtx, name, &gosdk.ExecRequest{
		Command:     append([]string(nil), isloHeartbeatCommand...),
		User:        stringValue(isloWorkloadUser),
		TimeoutSecs: &timeoutSecs,
	}, io.Discard, io.Discard)
	if err != nil {
		return core.LeaseHeartbeatResult{}, isloError("heartbeat exec", err)
	}
	if code != 0 {
		return core.LeaseHeartbeatResult{}, exit(5, "islo heartbeat exec on sandbox %s exited %d", name, code)
	}
	return core.LeaseHeartbeatResult{
		LeaseID:       leaseID,
		Slug:          slug,
		State:         state,
		LastTouchedAt: b.now(),
		IdleTimeout:   idleTimeout,
	}, nil
}

// isloPauseAfterIdle reads the sandbox's echoed lifecycle.pause_after_idle.
// Returns 0 when the sandbox carries no idle policy, which Crabbox cannot
// distinguish from a tenant-side default it is never told about.
func isloPauseAfterIdle(sandbox *gosdk.SandboxResponse) time.Duration {
	seconds := sandbox.GetLifecycle().GetPauseAfterIdle()
	if seconds == nil || *seconds <= 0 {
		return 0
	}
	return time.Duration(*seconds) * time.Second
}
