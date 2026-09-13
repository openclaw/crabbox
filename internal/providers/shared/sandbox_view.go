package shared

import core "github.com/openclaw/crabbox/internal/cli"

// SandboxLeaseView projects an adapter's observed resource and local claim into
// the common view. It neither validates ownership nor copies private claim labels.
func SandboxLeaseView(provider, target string, claim core.LeaseClaim, id, name, state string) core.LeaseView {
	return core.LeaseView{
		Provider: provider,
		CloudID:  id,
		Name:     name,
		Status:   state,
		Labels: map[string]string{
			"provider": provider,
			"lease":    claim.LeaseID,
			"slug":     claim.Slug,
			"pond":     claim.Pond,
			"target":   target,
			"state":    state,
		},
	}
}
