package station

import "fmt"

// Gate is the feature gate for the Station primitive. Its zero value is the
// safe default: every phase is disabled, so no station lifecycle, no agent
// loop supervision, and no model-access credential delivery can run.
//
// Phases are enabled independently and only by an explicit opt-in, mirroring
// the cautious skeleton style used elsewhere in the codebase. A future PR will
// flip these as each phase ships and is reviewed.
type Gate struct {
	enabled map[Phase]bool
}

// DefaultGate returns the disabled-by-default gate. Nothing is enabled.
func DefaultGate() Gate {
	return Gate{}
}

// WithPhase returns a copy of the gate with the given phase enabled. It is the
// single explicit opt-in seam; callers must not mutate the gate any other way.
func (g Gate) WithPhase(p Phase) Gate {
	enabled := make(map[Phase]bool, len(g.enabled)+1)
	for k, v := range g.enabled {
		enabled[k] = v
	}
	enabled[p] = true
	return Gate{enabled: enabled}
}

// Enabled reports whether a phase is turned on.
func (g Gate) Enabled(p Phase) bool {
	return g.enabled[p]
}

// notEnabledError is returned when a phase is exercised while disabled. It is a
// distinct type so callers can detect the gate condition.
type notEnabledError struct {
	phase Phase
}

func (e notEnabledError) Error() string {
	return fmt.Sprintf("station %s phase is not yet enabled", e.phase)
}

// IsNotEnabled reports whether err is a phase-gate "not yet enabled" error.
func IsNotEnabled(err error) bool {
	_, ok := err.(notEnabledError)
	return ok
}

// EnsurePhase returns a clear "not yet enabled" error unless the phase is on.
// It is the enforcement stub future lifecycle code calls before doing work.
func (g Gate) EnsurePhase(p Phase) error {
	if !g.Enabled(p) {
		return notEnabledError{phase: p}
	}
	return nil
}

// AgentProfile is the agent-profile boundary type. It wraps a StationProfile
// that has been admitted as an agent station and records what Crabbox owns
// versus what the repo-owned workload owns. Construct it only through
// NewAgentProfile so the boundary checks always run.
type AgentProfile struct {
	profile StationProfile
}

// Profile returns the underlying station profile.
func (a AgentProfile) Profile() StationProfile {
	return a.profile
}

// Command returns the repo-owned workload entrypoint. Crabbox supervises and
// records this command; it does not own the prompt loop or model choice.
func (a AgentProfile) Command() string {
	return a.profile.Command
}

// NewAgentProfile admits a profile as an agent station, enforcing the phase
// gate and the agent-profile boundary. It returns a "not yet enabled" error
// while the agent-profile phase is off, mirroring the roadmap's staged rollout.
func NewAgentProfile(g Gate, p StationProfile) (AgentProfile, error) {
	if err := g.EnsurePhase(PhaseAgentProfile); err != nil {
		return AgentProfile{}, err
	}
	if err := p.Validate(); err != nil {
		return AgentProfile{}, err
	}
	if !p.Agent {
		return AgentProfile{}, fmt.Errorf("station profile %q is not an agent profile", p.Name)
	}
	if !p.Enabled {
		return AgentProfile{}, fmt.Errorf("station profile %q is not enabled", p.Name)
	}
	if p.Command == "" {
		return AgentProfile{}, fmt.Errorf("station profile %q has no command", p.Name)
	}
	return AgentProfile{profile: p}, nil
}

// AuthorizeModelAccess is the credential-delivery gate. It refuses unless the
// modelAccess phase is enabled, and it always refuses to source credentials
// from env.allow forwarding. No secret is produced here; this is the seam a
// future phase replaces with the audited delivery path.
//
// envAllow is passed in only so the boundary can be asserted: any overlap
// between env.allow names and model-access intent is a configuration error.
func (g Gate) AuthorizeModelAccess(p StationProfile, envAllow []string) error {
	if err := g.EnsurePhase(PhaseModelAccess); err != nil {
		return err
	}
	if p.ModelAccess.IsZero() || !p.ModelAccess.Enabled {
		return fmt.Errorf("station profile %q has no enabled modelAccess policy", p.Name)
	}
	if err := p.ModelAccess.validate(p.Name); err != nil {
		return err
	}
	// Model/tool credentials must never travel through ordinary repo env.allow.
	// The gateway name doubles as the credential channel identifier, so its
	// appearance in env.allow signals an attempt to smuggle credentials through
	// the unaudited path.
	for _, name := range envAllow {
		if name == p.ModelAccess.Gateway {
			return fmt.Errorf("station profile %q modelAccess gateway %q must not be forwarded via env.allow; it uses the separate audited delivery path", p.Name, p.ModelAccess.Gateway)
		}
	}
	return nil
}
