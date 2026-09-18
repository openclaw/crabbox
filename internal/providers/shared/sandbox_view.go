package shared

import (
	"strconv"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

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

// SandboxObservation contains provider facts, not defaults for a future lease.
// Adapters associate optional local history with the observed resource before
// projecting it; these labels do not authorize any lifecycle operation.
type SandboxObservation struct {
	Provider, Target, LeaseID, Slug, State string
	CreatedAt, UpdatedAt                   time.Time
}

func (o SandboxObservation) Labels(claim *core.LeaseClaim) map[string]string {
	labels := map[string]string{
		"provider": o.Provider, "target": o.Target, "lease": o.LeaseID,
		"slug": o.Slug, "state": o.State, "crabbox": "true", "created_by": "crabbox",
	}
	for key, value := range map[string]time.Time{"created_at": o.CreatedAt, "updated_at": o.UpdatedAt} {
		if !value.IsZero() && value.Unix() > 0 {
			labels[key] = core.LeaseLabelTime(value)
		}
	}
	if claim == nil {
		return labels
	}
	for _, key := range []string{"class", "profile", "provider_key"} {
		if value := claim.Labels[key]; value != "" && value != "unknown" {
			labels[key] = value
		}
	}
	if claim.Pond != "" {
		labels["pond"] = claim.Pond
	}
	if used, err := time.Parse(time.RFC3339Nano, claim.LastUsedAt); err == nil && used.Unix() > 0 {
		labels["last_touched_at"] = core.LeaseLabelTime(used)
	}
	if claim.IdleTimeoutSeconds > 0 {
		labels["idle_timeout"] = strconv.Itoa(claim.IdleTimeoutSeconds)
		labels["idle_timeout_secs"] = labels["idle_timeout"]
	}
	if ttl, err := strconv.ParseInt(claim.Labels["ttl_secs"], 10, 64); err == nil && ttl > 0 {
		labels["ttl_secs"] = strconv.FormatInt(ttl, 10)
	}
	if keep := claim.Labels["keep"]; keep == "true" || keep == "false" {
		labels["keep"] = keep
	}
	// Local policy and usage are not a provider-enforced deletion schedule.
	// In particular, a read must not compute a new expiry from today's config.
	return labels
}
