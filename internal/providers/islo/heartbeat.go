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

// Heartbeat registers activity with a bounded no-op exec after observing a
// running sandbox. It reports the server's idle policy without rewriting it,
// changing absolute lifetime limits, updating the local claim, or calling resume.
func (b *isloBackend) Heartbeat(ctx context.Context, req core.LeaseHeartbeatRequest) (core.LeaseHeartbeatResult, error) {
	client, err := newIsloClient(b.cfg, b.rt)
	if err != nil {
		return core.LeaseHeartbeatResult{}, err
	}
	leaseID, name, slug, err := resolveIsloLeaseID(req.ID, "", false)
	if err != nil {
		return core.LeaseHeartbeatResult{}, err
	}
	claim, err := requireIsloLeaseClaim(leaseID, "heartbeat")
	if err != nil {
		return core.LeaseHeartbeatResult{}, err
	}
	if err := requireIsloClaimScope(claim, b.claimScope()); err != nil {
		return core.LeaseHeartbeatResult{}, err
	}
	getCtx, cancelGet := context.WithTimeout(ctx, isloHeartbeatTimeout)
	sandbox, err := client.GetSandbox(getCtx, name)
	cancelGet()
	if err != nil {
		return core.LeaseHeartbeatResult{}, isloError("get sandbox", err)
	}
	if sandbox == nil {
		return core.LeaseHeartbeatResult{}, core.Exit(4, "islo sandbox %s not found", name)
	}
	if err := requireIsloExecIdentity(claim, name, isloIdentityFromSandbox(sandbox), "heartbeat"); err != nil {
		return core.LeaseHeartbeatResult{}, err
	}
	state := sandbox.GetStatus()
	if isloStatusTerminal(state) {
		return core.LeaseHeartbeatResult{}, core.Exit(5, "islo sandbox %s is in terminal state=%s", name, state)
	}
	if !isloStatusReady(state) {
		// Do not turn an observed paused lease into a request to resume it.
		hint := ""
		if strings.EqualFold(strings.TrimSpace(state), "paused") {
			hint = "; resume it before heartbeat"
		}
		return core.LeaseHeartbeatResult{}, core.Exit(5, "islo sandbox %s is not running (state=%s)%s", name, core.Blank(state, "unknown"), hint)
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
		return core.LeaseHeartbeatResult{}, core.Exit(5, "islo heartbeat exec on sandbox %s exited %d", name, code)
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
