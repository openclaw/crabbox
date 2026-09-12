package cua

import (
	"context"
	"fmt"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type backend struct {
	spec core.ProviderSpec
	cfg  core.Config
	rt   core.Runtime
}

func (b backend) Spec() core.ProviderSpec { return b.spec }

func (b backend) client() *bridgeClient {
	return newBridgeClient(b.cfg, b.rt)
}

func (b backend) Warmup(ctx context.Context, req core.WarmupRequest) error {
	return provisioningUnsupported()
}

func (b backend) Run(_ context.Context, req core.RunRequest) (core.RunResult, error) {
	if strings.TrimSpace(req.ID) == "" {
		return core.RunResult{}, provisioningUnsupported()
	}
	return core.RunResult{}, mutationUnsupported()
}

func (b backend) List(ctx context.Context, _ core.ListRequest) ([]core.LeaseView, error) {
	client := b.client()
	sandboxes, err := client.ListSandboxes(ctx)
	if err != nil {
		return nil, err
	}
	claims, err := listCUALeaseClaims()
	if err != nil {
		return nil, err
	}
	claimsBySandbox := make(map[string]core.LeaseClaim, len(claims))
	for _, claim := range claims {
		if claim.Provider != providerName || !b.claimMatchesActiveScope(claim) {
			continue
		}
		sandboxName := claimSandboxName(claim)
		if sandboxName == "" {
			continue
		}
		claimsBySandbox[sandboxName] = claim
	}
	views := make([]core.LeaseView, 0, len(sandboxes)+len(claimsBySandbox))
	seen := make(map[string]bool, len(sandboxes))
	for _, sb := range sandboxes {
		sandboxName := strings.TrimSpace(core.Blank(sb.Name, sb.ID))
		if sandboxName == "" {
			continue
		}
		seen[sandboxName] = true
		claim, hasClaim := claimsBySandbox[sandboxName]
		claimed := false
		claimState := "unclaimed"
		if hasClaim {
			if err := validateSandboxOwnership(claim, sb, claim.ProviderScope); err == nil {
				claimed = true
				claimState = "claimed"
			} else {
				claim = core.LeaseClaim{}
				claimState = "identity-mismatch"
			}
		}
		view := b.serverFromSandbox(claim, sb)
		view.Labels["claimed"] = fmt.Sprintf("%t", claimed)
		view.Labels["claim_state"] = claimState
		view.Labels["experimental"] = "true"
		views = append(views, view)
	}
	for sandboxName, claim := range claimsBySandbox {
		if seen[sandboxName] {
			continue
		}
		view := b.serverFromSandbox(claim, bridgeSandboxSummary{ID: sandboxName, Name: sandboxName, Status: "missing-or-inaccessible"})
		view.Labels["claimed"] = "true"
		view.Labels["experimental"] = "true"
		views = append(views, view)
	}
	return views, nil
}

func (b backend) Status(ctx context.Context, req core.StatusRequest) (core.StatusView, error) {
	identifier := strings.TrimSpace(req.ID)
	if identifier == "" {
		return core.StatusView{}, core.Exit(2, "provider=cua status requires a sandbox id, Crabbox lease id, or slug")
	}
	claim, claimed, err := resolveCUALeaseClaim(identifier, b.cfg)
	if err != nil {
		return core.StatusView{}, err
	}
	sandboxID := identifier
	if claimed {
		sandboxID = claimSandboxName(claim)
		if sandboxID == "" {
			return core.StatusView{}, core.Exit(4, "CUA lease %q is missing its claimed sandbox name", claim.LeaseID)
		}
	}
	waitTimeout := req.WaitTimeout
	if waitTimeout <= 0 {
		waitTimeout = 5 * time.Minute
	}
	pollCtx := ctx
	cancel := func() {}
	if req.Wait {
		pollCtx, cancel = context.WithTimeout(ctx, waitTimeout)
	}
	defer cancel()
	deadline := core.ClockNow(b.rt.Clock).Add(waitTimeout)
	for {
		sb, getErr := b.client().GetSandbox(pollCtx, sandboxID)
		if getErr == nil && claimed {
			getErr = validateSandboxOwnership(claim, sb, claim.ProviderScope)
		}
		if getErr != nil {
			if req.Wait && ctx.Err() == nil && pollCtx.Err() != nil {
				return core.StatusView{}, core.Exit(5, "timed out waiting for CUA sandbox %s to become ready", sandboxID)
			}
			return core.StatusView{}, getErr
		}
		state := normalizedSandboxState(sb)
		leaseID, slug, pond := sandboxID, "", ""
		if claimed {
			leaseID = claim.LeaseID
			slug = core.Blank(claim.Slug, core.NewLeaseSlug(claim.LeaseID))
			pond = claim.Pond
		}
		view := core.StatusView{
			ID:       leaseID,
			Slug:     slug,
			Provider: providerName,
			TargetOS: sandboxTargetOS(claim, sb),
			State:    state,
			ServerID: sandboxID,
			Pond:     pond,
			Network:  "public",
			Ready:    isReadyState(state),
			Labels: map[string]string{
				"provider":     providerName,
				"lease":        leaseID,
				"slug":         slug,
				"pond":         pond,
				"state":        state,
				"claimed":      fmt.Sprintf("%t", claimed),
				"experimental": "true",
			},
		}
		if !req.Wait || view.Ready {
			return view, nil
		}
		if isTerminalState(state) {
			return core.StatusView{}, core.Exit(5, "CUA sandbox %s entered terminal state %q before becoming ready", sandboxID, state)
		}
		if core.ClockNow(b.rt.Clock).After(deadline) {
			return core.StatusView{}, core.Exit(5, "timed out waiting for CUA sandbox %s to become ready", sandboxID)
		}
		select {
		case <-pollCtx.Done():
			if ctx.Err() == nil {
				return core.StatusView{}, core.Exit(5, "timed out waiting for CUA sandbox %s to become ready", sandboxID)
			}
			return core.StatusView{}, pollCtx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (b backend) Stop(context.Context, core.StopRequest) error {
	return mutationUnsupported()
}

func (b backend) Cleanup(context.Context, core.CleanupRequest) error {
	return mutationUnsupported()
}

func (b backend) serverFromSandbox(claim core.LeaseClaim, sb bridgeSandboxSummary) core.Server {
	state := normalizedSandboxState(sb)
	sandboxName := strings.TrimSpace(core.Blank(sb.Name, sb.ID))
	if sandboxName == "" {
		sandboxName = claimSandboxName(claim)
	}
	return core.Server{
		Provider: providerName,
		CloudID:  sandboxName,
		Name:     sandboxName,
		Status:   state,
		Labels: map[string]string{
			"provider": providerName,
			"lease":    claim.LeaseID,
			"slug":     claim.Slug,
			"pond":     claim.Pond,
			"target":   sandboxTargetOS(claim, sb),
			"state":    state,
		},
	}
}

func (b backend) claimMatchesActiveScope(claim core.LeaseClaim) bool {
	scope, err := cuaScope(b.cfg)
	return err == nil && claim.ProviderScope == scope
}

func normalizedSandboxState(sb bridgeSandboxSummary) string {
	return strings.ToLower(core.Blank(strings.TrimSpace(core.Blank(sb.Status, sb.State)), "unknown"))
}

func isReadyState(state string) bool {
	switch strings.TrimSpace(strings.ToLower(state)) {
	case "running", "ready", "started", "active":
		return true
	default:
		return false
	}
}

func isTerminalState(state string) bool {
	switch strings.TrimSpace(strings.ToLower(state)) {
	case "terminated", "stopped", "failed", "error", "aborted", "killed", "deleted", "destroyed":
		return true
	default:
		return false
	}
}

func sandboxTargetOS(claim core.LeaseClaim, sb bridgeSandboxSummary) string {
	value := strings.ToLower(strings.TrimSpace(core.Blank(sb.OSType, sb.Metadata["osType"])))
	if value == "" {
		value = strings.ToLower(strings.TrimSpace(claim.TargetOS))
	}
	switch value {
	case "ubuntu", "debian", "linux":
		return targetLinux
	case "darwin", "mac", "macos":
		return "macos"
	case "win", "windows":
		return "windows"
	case "":
		return "unknown"
	default:
		return value
	}
}
