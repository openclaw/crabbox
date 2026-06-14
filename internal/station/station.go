// Package station holds the first buildable slice of the Station profile
// primitive described in docs/features/station-profiles.md (issue #193).
//
// A Station is a durable supervised-workload record bound to a warm lease.
// This package intentionally ships only the config surface: a StationProfile
// struct, parsing and validation, the agent-profile boundary, and the security
// gating that keeps modelAccess separate from ordinary env.allow forwarding.
//
// Everything here is disabled by default and the lifecycle phase gates return
// clear "not yet enabled" errors until a future PR turns each phase on. No
// supervisor, lease, or credential delivery is wired up yet; that work lands in
// later, separately reviewed phases per the roadmap.
package station

import "time"

// Phase enumerates the staged rollout described in the roadmap. Each phase is
// reviewed and enabled separately.
type Phase int

const (
	// PhaseGenericStation is phase 1: a durable supervised workload with no
	// model credentials.
	PhaseGenericStation Phase = iota + 1
	// PhaseAgentProfile is phase 2: the `agent` station profile, still with no
	// model credentials by default.
	PhaseAgentProfile
	// PhaseModelAccess is phase 3: scoped modelAccess credential delivery,
	// gated behind all of the modelAccess security requirements.
	PhaseModelAccess
)

// String returns a stable identifier for the phase, used in error messages.
func (p Phase) String() string {
	switch p {
	case PhaseGenericStation:
		return "generic-station"
	case PhaseAgentProfile:
		return "agent-profile"
	case PhaseModelAccess:
		return "model-access"
	default:
		return "unknown"
	}
}

// RestartPolicy controls whether a stopped station attempt may be restarted.
// Agent loops are not safely replayable by default, so the zero value is
// "never".
type RestartPolicy string

const (
	// RestartNever never restarts a stopped attempt. This is the default.
	RestartNever RestartPolicy = "never"
	// RestartOnFailure restarts only on non-clean termination. Reserved for a
	// later product decision; rejected by validation today.
	RestartOnFailure RestartPolicy = "on-failure"
)

// StationProfile is a named station policy, for example `default` or `agent`.
// It is the config selector referenced by `stationProfile` in the roadmap.
//
// A profile is inert until Enabled is true, and even then only the lifecycle
// phases that have been turned on may run (see Gate).
type StationProfile struct {
	// Name is the profile selector, e.g. "default" or "agent".
	Name string
	// Enabled gates the profile. Profiles are disabled by default so that
	// merely declaring one never changes run behavior.
	Enabled bool
	// Agent marks this as an agent station (stationProfile: agent). The agent
	// loop is repo-owned; Crabbox supervises and records it but does not own
	// the prompt loop or model choice.
	Agent bool
	// Command is the repo-owned workload entrypoint, e.g. scripts/agent-loop.sh.
	Command string
	// TTL is the hard upper bound on station lifetime. Credentials, if any,
	// must expire at or before TTL.
	TTL time.Duration
	// IdleTimeout stops a station that has made no progress for this long.
	IdleTimeout time.Duration
	// RestartPolicy controls attempt restarts. Defaults to RestartNever.
	RestartPolicy RestartPolicy
	// ModelAccess is the security-gated credential-delivery policy. It is a
	// distinct field and is NEVER populated from env.allow. It stays empty
	// (disabled) until phase 3 and its own security gates are satisfied.
	ModelAccess ModelAccessPolicy
}

// ModelAccessPolicy is the explicit, audited credential-delivery policy for
// model/tool access. It is deliberately separate from env.allow forwarding:
// model/tool credentials must never travel through ordinary repo env.allow.
//
// This type only records policy/receipt metadata. It never holds a secret
// value; the secret delivery path lands in a later phase.
type ModelAccessPolicy struct {
	// Enabled turns on model access. It is rejected until the modelAccess
	// phase is enabled and all security gates pass.
	Enabled bool
	// Gateway names the approved model/tool gateway.
	Gateway string
	// AllowedModels lists the models the workload may reach. Empty means none.
	AllowedModels []string
	// AllowedTools lists the tools the workload may reach. Empty means none.
	AllowedTools []string
	// EgressProfile names an approved egress profile. Egress defaults to deny,
	// so an empty value means no egress is permitted.
	EgressProfile string
	// BudgetUSD caps spend for the station. Zero means unset (rejected when
	// model access is enabled).
	BudgetUSD float64
}

// IsZero reports whether the policy is the empty (disabled) policy.
func (m ModelAccessPolicy) IsZero() bool {
	return !m.Enabled &&
		m.Gateway == "" &&
		len(m.AllowedModels) == 0 &&
		len(m.AllowedTools) == 0 &&
		m.EgressProfile == "" &&
		m.BudgetUSD == 0
}
