package cua

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type backend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func (b backend) Spec() ProviderSpec { return b.spec }

func (b backend) client() *bridgeClient {
	return newBridgeClient(b.cfg, b.rt)
}

func (b backend) Warmup(ctx context.Context, req WarmupRequest) error {
	return provisioningUnsupported()
}

func (b backend) Run(_ context.Context, req RunRequest) (RunResult, error) {
	if strings.TrimSpace(req.ID) == "" {
		return RunResult{}, provisioningUnsupported()
	}
	return RunResult{}, mutationUnsupported()
}

func (b backend) List(ctx context.Context, _ ListRequest) ([]LeaseView, error) {
	client := b.client()
	sandboxes, err := client.ListSandboxes(ctx)
	if err != nil {
		return nil, err
	}
	claims, err := listCUALeaseClaims()
	if err != nil {
		return nil, err
	}
	claimsBySandbox := make(map[string]LeaseClaim, len(claims))
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
	views := make([]LeaseView, 0, len(sandboxes)+len(claimsBySandbox))
	seen := make(map[string]bool, len(sandboxes))
	for _, sb := range sandboxes {
		sandboxName := strings.TrimSpace(blank(sb.Name, sb.ID))
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
				claim = LeaseClaim{}
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

func (b backend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	identifier := strings.TrimSpace(req.ID)
	if identifier == "" {
		return StatusView{}, exit(2, "provider=cua status requires a sandbox id, Crabbox lease id, or slug")
	}
	claim, claimed, err := resolveCUALeaseClaim(identifier, b.cfg)
	if err != nil {
		return StatusView{}, err
	}
	sandboxID := identifier
	if claimed {
		sandboxID = claimSandboxName(claim)
		if sandboxID == "" {
			return StatusView{}, exit(4, "CUA lease %q is missing its claimed sandbox name", claim.LeaseID)
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
	deadline := b.now().Add(waitTimeout)
	for {
		sb, getErr := b.client().GetSandbox(pollCtx, sandboxID)
		if getErr == nil && claimed {
			getErr = validateSandboxOwnership(claim, sb, claim.ProviderScope)
		}
		if getErr != nil {
			if req.Wait && ctx.Err() == nil && pollCtx.Err() != nil {
				return StatusView{}, exit(5, "timed out waiting for CUA sandbox %s to become ready", sandboxID)
			}
			return StatusView{}, getErr
		}
		state := normalizedSandboxState(sb)
		leaseID, slug, pond := sandboxID, "", ""
		if claimed {
			leaseID = claim.LeaseID
			slug = blank(claim.Slug, newLeaseSlug(claim.LeaseID))
			pond = claim.Pond
		}
		view := StatusView{
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
			return StatusView{}, exit(5, "CUA sandbox %s entered terminal state %q before becoming ready", sandboxID, state)
		}
		if b.now().After(deadline) {
			return StatusView{}, exit(5, "timed out waiting for CUA sandbox %s to become ready", sandboxID)
		}
		select {
		case <-pollCtx.Done():
			if ctx.Err() == nil {
				return StatusView{}, exit(5, "timed out waiting for CUA sandbox %s to become ready", sandboxID)
			}
			return StatusView{}, pollCtx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (b backend) Stop(context.Context, StopRequest) error {
	return mutationUnsupported()
}

func (b backend) Cleanup(context.Context, CleanupRequest) error {
	return mutationUnsupported()
}

func (b backend) serverFromSandbox(claim LeaseClaim, sb bridgeSandboxSummary) Server {
	state := normalizedSandboxState(sb)
	sandboxName := strings.TrimSpace(blank(sb.Name, sb.ID))
	if sandboxName == "" {
		sandboxName = claimSandboxName(claim)
	}
	return Server{
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

func (b backend) claimMatchesActiveScope(claim LeaseClaim) bool {
	scope, err := cuaScope(b.cfg)
	return err == nil && claim.ProviderScope == scope
}

func (b backend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now()
	}
	return time.Now()
}

func normalizedSandboxState(sb bridgeSandboxSummary) string {
	return strings.ToLower(blank(strings.TrimSpace(blank(sb.Status, sb.State)), "unknown"))
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

func sandboxTargetOS(claim LeaseClaim, sb bridgeSandboxSummary) string {
	value := strings.ToLower(strings.TrimSpace(blank(sb.OSType, sb.Metadata["osType"])))
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
