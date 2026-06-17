package nebius

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	nebiusProviderLabel = "crabbox_provider"
	nebiusLeaseLabel    = "crabbox_lease"
	nebiusSlugLabel     = "crabbox_slug"
	nebiusTargetLabel   = "crabbox_target"
	nebiusScopeLabel    = "crabbox_scope"
	nebiusParentLabel   = "crabbox_parent_id"
	nebiusProfileLabel  = "crabbox_profile"
)

func nebiusLeaseLabels(cfg Config, leaseID, slug, state string, keep bool, now time.Time) map[string]string {
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", keep, now)
	labels["state"] = state
	return addNebiusScopeLabels(labels, cfg)
}

func addNebiusScopeLabels(labels map[string]string, cfg Config) map[string]string {
	out := make(map[string]string, len(labels)+8)
	for key, value := range labels {
		out[normalizeNebiusLabelKey(key)] = normalizeNebiusLabelValue(value)
	}
	out["crabbox"] = "true"
	out["provider"] = providerName
	out["target"] = targetLinux
	out[nebiusProviderLabel] = providerName
	out[nebiusLeaseLabel] = out["lease"]
	out[nebiusSlugLabel] = out["slug"]
	out[nebiusTargetLabel] = targetLinux
	out[nebiusScopeLabel] = nebiusScopeHash(cfg)
	out[nebiusParentLabel] = normalizeNebiusLabelValue(cfg.Nebius.ParentID)
	if strings.TrimSpace(cfg.Nebius.Profile) != "" {
		out[nebiusProfileLabel] = normalizeNebiusLabelValue(cfg.Nebius.Profile)
	}
	return out
}

func validateNebiusOwnership(labels map[string]string, cfg Config) error {
	if labels == nil {
		return core.Exit(3, "nebius resource has no labels; refusing lifecycle action")
	}
	required := map[string]string{
		"crabbox":           "true",
		"provider":          providerName,
		"target":            targetLinux,
		nebiusProviderLabel: providerName,
		nebiusTargetLabel:   targetLinux,
		nebiusScopeLabel:    nebiusScopeHash(cfg),
		nebiusParentLabel:   normalizeNebiusLabelValue(cfg.Nebius.ParentID),
	}
	for key, want := range required {
		if got := strings.TrimSpace(labels[key]); got != want {
			return core.Exit(3, "nebius ownership mismatch for %s: expected %q, found %q", key, want, got)
		}
	}
	if strings.TrimSpace(labels["lease"]) == "" || labels["lease"] != labels[nebiusLeaseLabel] {
		return core.Exit(3, "nebius ownership requires matching lease labels")
	}
	if strings.TrimSpace(labels["slug"]) == "" || labels["slug"] != labels[nebiusSlugLabel] {
		return core.Exit(3, "nebius ownership requires matching slug labels")
	}
	if strings.TrimSpace(labels["expires_at"]) == "" {
		return core.Exit(3, "nebius ownership requires expires_at label")
	}
	if wantProfile := strings.TrimSpace(cfg.Nebius.Profile); wantProfile != "" {
		if got := strings.TrimSpace(labels[nebiusProfileLabel]); got != normalizeNebiusLabelValue(wantProfile) {
			return core.Exit(3, "nebius profile mismatch: expected %q, found %q", normalizeNebiusLabelValue(wantProfile), got)
		}
	}
	return nil
}

func nebiusScopeHash(cfg Config) string {
	parts := []string{
		"provider=" + providerName,
		"profile=" + strings.TrimSpace(cfg.Nebius.Profile),
		"parent=" + strings.TrimSpace(cfg.Nebius.ParentID),
		"subnet=" + strings.TrimSpace(cfg.Nebius.SubnetID),
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])[:24]
}

func normalizeNebiusLabelKey(key string) string {
	key = strings.TrimSpace(strings.ToLower(key))
	key = strings.NewReplacer(".", "_", "-", "_", "/", "_", " ", "_").Replace(key)
	if key == "" {
		return "label"
	}
	return key
}

func normalizeNebiusLabelValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 256 {
		value = value[:256]
	}
	return value
}
