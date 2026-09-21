package shared

import (
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// LocalInstanceViews applies a display filter, not an ownership predicate.
// Inventory order is retained; claim lookup and projection do not mutate inputs.
func LocalInstanceViews[T any](instances []T, claims map[string]core.LeaseClaim, cfg core.Config, name func(T) string, project func(T, core.LeaseClaim, core.Config) core.Server) []core.LeaseView {
	views := make([]core.LeaseView, 0, len(instances))
	for _, inst := range instances {
		instanceName := name(inst)
		claim := claims[instanceName]
		if claim.LeaseID == "" && !strings.HasPrefix(instanceName, "crabbox-") {
			continue
		}
		views = append(views, project(inst, claim, cfg))
	}
	return views
}

// LocalInstanceServer promotes ready observations only while the adapter says
// the instance is running. Label precedence and identity remain adapter-owned.
func LocalInstanceServer(provider, name, state string, running bool, labels map[string]string) core.Server {
	if running && labels["state"] == "ready" {
		state = "ready"
	}
	return core.Server{
		CloudID:  name,
		Provider: provider,
		Name:     name,
		Status:   state,
		Labels:   labels,
	}
}

// ClaimIdleExpiredAfterGrace preserves strict expiry and active-claim reasons.
// Callers own timestamp whitespace normalization and all deletion policy.
func ClaimIdleExpiredAfterGrace(claim core.LeaseClaim, now time.Time, grace time.Duration) (bool, string) {
	lastUsed, err := time.Parse(time.RFC3339, claim.LastUsedAt)
	if err != nil || lastUsed.IsZero() {
		return false, "claim active"
	}
	idle := time.Duration(claim.IdleTimeoutSeconds) * time.Second
	if idle > 0 && now.After(lastUsed.Add(idle).Add(grace)) {
		return true, "claim expired"
	}
	return false, "claim active"
}
