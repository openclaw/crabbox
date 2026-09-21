package shared

import (
	"context"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// ObserveSandboxStatus sequences a claimed sandbox read using the existing wait.
// Only transport and probe failures consult ContextError; ownership errors win
// over cancellation, and terminal states are rejected only when waiting.
// The adapter owns projection (including any probe) and terminal-state policy.
func ObserveSandboxStatus[T any](wait *StatusWait, id string, interval time.Duration,
	fetch func(context.Context, string) (T, error), validate func(T) error,
	project func(context.Context, T) (core.StatusView, error), terminal func(string) bool,
	terminalError func(string, string) error,
) (core.StatusView, error) {
	defer wait.Close()
	return wait.Poll(id, interval, func(ctx context.Context) (core.StatusView, bool, error) {
		sandbox, err := fetch(ctx, id)
		if err != nil {
			if ctxErr := wait.ContextError(id); ctxErr != nil {
				err = ctxErr
			}
			return core.StatusView{}, false, err
		}
		if err := validate(sandbox); err != nil {
			return core.StatusView{}, false, err
		}
		view, err := project(ctx, sandbox)
		if err != nil {
			return core.StatusView{}, false, err
		}
		if wait.wait && !view.Ready && terminal(view.State) {
			return core.StatusView{}, false, terminalError(id, view.State)
		}
		return view, false, nil
	})
}

// SandboxStatusView projects the common public Linux sandbox fields. Adapters
// retain optional labels and host fields as well as state/readiness policy.
func SandboxStatusView(provider, leaseID, slug, sandboxID, pond, state string, ready bool) core.StatusView {
	return core.StatusView{
		ID:       leaseID,
		Slug:     slug,
		Provider: provider,
		TargetOS: core.TargetLinux,
		State:    state,
		ServerID: sandboxID,
		Pond:     pond,
		Network:  core.NetworkPublic,
		Ready:    ready,
		Labels:   map[string]string{"provider": provider, "lease": leaseID, "pond": pond, "state": state},
	}
}

// VerifySandboxClaim preserves read, local scope, fetch, remote ownership order.
func VerifySandboxClaim[T any](ctx context.Context, leaseID, sandboxID string,
	validateScope func(core.LeaseClaim) error, fetch func(context.Context, string) (T, error),
	validateOwnership func(core.LeaseClaim, T) error,
) (T, error) {
	var zero T
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		return zero, err
	}
	if err := validateScope(claim); err != nil {
		return zero, err
	}
	sandbox, err := fetch(ctx, sandboxID)
	if err != nil {
		return zero, err
	}
	if err := validateOwnership(claim, sandbox); err != nil {
		return zero, err
	}
	return sandbox, nil
}
