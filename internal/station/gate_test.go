package station

import (
	"strings"
	"testing"
)

func TestGateDisabledByDefault(t *testing.T) {
	g := DefaultGate()
	for _, p := range []Phase{PhaseGenericStation, PhaseAgentProfile, PhaseModelAccess} {
		if g.Enabled(p) {
			t.Fatalf("phase %s should be disabled by default", p)
		}
		err := g.EnsurePhase(p)
		if err == nil {
			t.Fatalf("EnsurePhase(%s) should fail while disabled", p)
		}
		if !IsNotEnabled(err) {
			t.Fatalf("expected not-enabled error for %s, got %v", p, err)
		}
		if !strings.Contains(err.Error(), "not yet enabled") {
			t.Fatalf("error message should say not yet enabled: %v", err)
		}
	}
}

func TestGateWithPhaseDoesNotEnableOthers(t *testing.T) {
	g := DefaultGate().WithPhase(PhaseGenericStation)
	if !g.Enabled(PhaseGenericStation) {
		t.Fatalf("generic station should be enabled")
	}
	if g.Enabled(PhaseAgentProfile) || g.Enabled(PhaseModelAccess) {
		t.Fatalf("only the requested phase should be enabled: %#v", g)
	}
	// Original gate is unchanged (copy semantics).
	if DefaultGate().Enabled(PhaseGenericStation) {
		t.Fatalf("DefaultGate must remain fully disabled")
	}
}

func TestNewAgentProfileGated(t *testing.T) {
	profile := StationProfile{
		Name:          "agent",
		Enabled:       true,
		Agent:         true,
		Command:       "scripts/agent-loop.sh",
		RestartPolicy: RestartNever,
	}

	// Disabled gate refuses with a not-yet-enabled error.
	if _, err := NewAgentProfile(DefaultGate(), profile); err == nil || !IsNotEnabled(err) {
		t.Fatalf("expected not-enabled error, got %v", err)
	}

	// With the agent phase on, a valid agent profile is admitted.
	g := DefaultGate().WithPhase(PhaseAgentProfile)
	ap, err := NewAgentProfile(g, profile)
	if err != nil {
		t.Fatalf("NewAgentProfile: %v", err)
	}
	if ap.Command() != "scripts/agent-loop.sh" {
		t.Fatalf("command = %q", ap.Command())
	}

	// A non-agent profile is rejected even with the phase on.
	nonAgent := profile
	nonAgent.Agent = false
	if _, err := NewAgentProfile(g, nonAgent); err == nil || IsNotEnabled(err) {
		t.Fatalf("expected non-agent rejection, got %v", err)
	}
}

func TestAuthorizeModelAccessGatedAndSeparateFromEnvAllow(t *testing.T) {
	profile := StationProfile{
		Name:          "agent",
		Enabled:       true,
		Agent:         true,
		Command:       "scripts/agent-loop.sh",
		RestartPolicy: RestartNever,
		ModelAccess: ModelAccessPolicy{
			Enabled:       true,
			Gateway:       "scoped-gateway",
			AllowedModels: []string{"claude-opus-4-8"},
			EgressProfile: "approved-anthropic",
			BudgetUSD:     5,
		},
	}

	// Disabled gate: refuses with not-yet-enabled.
	if err := DefaultGate().AuthorizeModelAccess(profile, nil); err == nil || !IsNotEnabled(err) {
		t.Fatalf("expected not-enabled error, got %v", err)
	}

	g := DefaultGate().WithPhase(PhaseModelAccess)

	// Phase enabled and env.allow clean: authorized.
	if err := g.AuthorizeModelAccess(profile, []string{"CI", "NODE_OPTIONS"}); err != nil {
		t.Fatalf("AuthorizeModelAccess: %v", err)
	}

	// The gateway leaking through env.allow is rejected: credentials must use
	// the separate audited delivery path, never env.allow forwarding.
	err := g.AuthorizeModelAccess(profile, []string{"scoped-gateway"})
	if err == nil || IsNotEnabled(err) {
		t.Fatalf("expected env.allow separation error, got %v", err)
	}
	if !strings.Contains(err.Error(), "env.allow") {
		t.Fatalf("error should mention env.allow: %v", err)
	}

	// A profile with no model access is rejected even when the phase is on.
	noAccess := profile
	noAccess.ModelAccess = ModelAccessPolicy{}
	if err := g.AuthorizeModelAccess(noAccess, nil); err == nil || IsNotEnabled(err) {
		t.Fatalf("expected no-policy rejection, got %v", err)
	}
}
