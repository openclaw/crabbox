package shared

import (
	"errors"
	"fmt"
	"maps"

	core "github.com/openclaw/crabbox/internal/cli"
)

var ErrStrictClaimMismatch = errors.New("strict claim identifier mismatch")

type ClaimBinding struct {
	Provider, ProviderScope, LeaseID, Slug, CloudID string
	RequiredLabels                                  map[string]string
}

// ValidateClaimBinding checks non-empty structural fields and exact required labels, including empty label values.
func ValidateClaimBinding(claim core.LeaseClaim, want ClaimBinding) error {
	fields := []struct{ name, got, want string }{
		{"provider", claim.Provider, want.Provider},
		{"provider scope", claim.ProviderScope, want.ProviderScope},
		{"lease ID", claim.LeaseID, want.LeaseID},
		{"slug", claim.Slug, want.Slug},
		{"cloud ID", claim.CloudID, want.CloudID},
		{"label provider", claim.Labels["provider"], claim.Provider},
		{"label lease", claim.Labels["lease"], claim.LeaseID},
		{"label slug", claim.Labels["slug"], claim.Slug},
	}
	for _, field := range fields {
		if field.want != "" && field.got != field.want {
			return fmt.Errorf("claim %s mismatch: got %q, want %q", field.name, field.got, field.want)
		}
	}
	for key, expected := range want.RequiredLabels {
		if value, ok := claim.Labels[key]; !ok || value != expected {
			return fmt.Errorf("claim label %s mismatch: got %q, want %q", key, value, expected)
		}
	}
	return nil
}

// ResolveProviderClaimStrict resolves exact claims before slugs and never treats a canonical lease ID as a slug.
func ResolveProviderClaimStrict(identifier, provider, providerScope string) (core.LeaseClaim, bool, error) {
	claim, ok, exact, err := core.ResolveLeaseClaimForProviderScopeWithExact(identifier, provider, providerScope)
	if err != nil {
		return core.LeaseClaim{}, false, err
	}
	if (exact || core.IsCanonicalLeaseID(identifier)) && (!exact || !ok || claim.LeaseID != identifier) {
		return core.LeaseClaim{}, false, ErrStrictClaimMismatch
	}
	return claim, ok, nil
}

// RequireClaimSnapshot returns the exact revisioned claim carried by a provider result.
// It validates transport invariants only; provider cleanup policy remains adapter-owned.
func RequireClaimSnapshot(server core.Server, provider string) (core.LeaseClaim, error) {
	claim, exists, set := core.ServerLeaseClaimSnapshot(server)
	if !set {
		return core.LeaseClaim{}, core.Exit(2, "%s cleanup claim snapshot is missing", provider)
	}
	if !exists {
		return core.LeaseClaim{}, core.Exit(2, "%s cleanup claim snapshot records no exact claim", provider)
	}
	if claim.Provider != provider {
		return core.LeaseClaim{}, core.Exit(2, "%s cleanup claim provider mismatch: got %q", provider, claim.Provider)
	}
	leaseID := server.Labels["lease"]
	if leaseID == "" || claim.LeaseID != leaseID {
		return core.LeaseClaim{}, core.Exit(2, "%s cleanup claim lease mismatch: server=%q claim=%q", provider, leaseID, claim.LeaseID)
	}
	if claim.Revision == "" {
		return core.LeaseClaim{}, core.Exit(2, "%s cleanup claim snapshot has no revision for lease=%s", provider, leaseID)
	}
	return claim, nil
}

// CloneLabels returns a writable, non-nil copy, including for a nil input.
func CloneLabels(labels map[string]string) map[string]string {
	clone := maps.Clone(labels)
	if clone == nil {
		clone = map[string]string{}
	}
	return clone
}
