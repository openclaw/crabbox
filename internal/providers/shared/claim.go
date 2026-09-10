package shared

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

var ErrStrictClaimMismatch = errors.New("strict claim identifier mismatch")

type ClaimBinding struct {
	Provider, ProviderScope, LeaseID, Slug, CloudID string
	RequiredLabels                                  map[string]string
	ExactProviderScope                              bool
}

type ScopedLeaseResolver struct {
	Provider, LeasePrefix    string
	ReadClaim                func(string) (core.LeaseClaim, error)
	ListClaims               func() ([]core.LeaseClaim, error)
	ValidateClaim            func(core.LeaseClaim) error
	FinishClaim              func(core.LeaseClaim) (string, string, string, error)
	EmptyIdentifierError     func() error
	UnclaimedIdentifierError func(string) error
}

type ScopedLeaseFinishOptions struct {
	Provider, LeasePrefix, RepoRoot string
	Reclaim                         bool
	IdleTimeout                     time.Duration
	ValidateClaim                   func(core.LeaseClaim) error
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
		if (field.want != "" || field.name == "provider scope" && want.ExactProviderScope) && field.got != field.want {
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

// RequireExactClaim resolves durable local ownership; provider inventory and
// resource names alone never authorize a destructive lifecycle operation.
func RequireExactClaim(want ClaimBinding) (core.LeaseClaim, error) {
	if strings.TrimSpace(want.Provider) == "" || strings.TrimSpace(want.LeaseID) == "" || strings.TrimSpace(want.CloudID) == "" {
		return core.LeaseClaim{}, core.Exit(2, "destructive provider operation requires an exact provider, lease, and resource identity")
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(want.LeaseID)
	if err != nil {
		return core.LeaseClaim{}, err
	}
	if !exists {
		return core.LeaseClaim{}, core.Exit(2, "%s lease=%s has no exact local ownership claim for resource=%s", want.Provider, want.LeaseID, want.CloudID)
	}
	if err := ValidateClaimBinding(claim, want); err != nil {
		return core.LeaseClaim{}, core.Exit(2, "%s lease=%s has a missing or stale exact local ownership claim for resource=%s: %v", want.Provider, want.LeaseID, want.CloudID, err)
	}
	return claim, nil
}

// RemoveExactClaimAfter keeps the exact claim fenced until the provider action
// succeeds and its durable ownership record has been removed.
func RemoveExactClaimAfter(claim core.LeaseClaim, want ClaimBinding, action func() error) error {
	return RemoveExactClaimAfterContext(context.Background(), claim, want, action)
}

// RemoveExactClaimAfterContext also bounds waiting for the exact claim fence.
// The action must honor ctx itself and must not reenter claim operations.
func RemoveExactClaimAfterContext(ctx context.Context, claim core.LeaseClaim, want ClaimBinding, action func() error) error {
	if err := ValidateClaimBinding(claim, want); err != nil {
		return core.Exit(2, "%s lease=%s has a stale exact local ownership claim: %v", want.Provider, want.LeaseID, err)
	}
	return core.CleanupLeaseClaimIfUnchangedAfterContext(ctx, want.LeaseID, claim, true, action)
}

// UpdateExactClaimLabelsAfter fences provider mutations that retain their
// resource, such as stopping rather than deleting a workspace or instance.
func UpdateExactClaimLabelsAfter(claim core.LeaseClaim, want ClaimBinding, labels map[string]string, action func() error) (core.LeaseClaim, error) {
	if err := ValidateClaimBinding(claim, want); err != nil {
		return core.LeaseClaim{}, core.Exit(2, "%s lease=%s has a stale exact local ownership claim: %v", want.Provider, want.LeaseID, err)
	}
	return core.UpdateLeaseClaimLabelsIfUnchangedAfter(want.LeaseID, claim, labels, action)
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

func ResolveScopedLeaseID(identifier string, resolver ScopedLeaseResolver) (string, string, string, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return "", "", "", resolver.EmptyIdentifierError()
	}
	exactLeaseID := identifier
	if !strings.HasPrefix(exactLeaseID, resolver.LeasePrefix) {
		exactLeaseID = resolver.LeasePrefix + exactLeaseID
	}
	if claim, err := resolver.ReadClaim(exactLeaseID); err != nil {
		return "", "", "", err
	} else if claim.LeaseID == exactLeaseID && claim.Provider == resolver.Provider {
		return resolver.FinishClaim(claim)
	}
	claim, ok, err := ResolveScopedLeaseClaim(identifier, resolver.Provider, resolver.ListClaims, resolver.ValidateClaim)
	if err != nil {
		return "", "", "", err
	}
	if ok {
		return resolver.FinishClaim(claim)
	}
	return "", "", "", resolver.UnclaimedIdentifierError(identifier)
}

func ResolveScopedLeaseClaim(identifier, provider string, listClaims func() ([]core.LeaseClaim, error), validate func(core.LeaseClaim) error) (core.LeaseClaim, bool, error) {
	claims, err := listClaims()
	if err != nil {
		return core.LeaseClaim{}, false, err
	}
	for _, claim := range claims {
		if claim.Provider == provider && claim.LeaseID == identifier {
			if err := validate(claim); err != nil {
				return core.LeaseClaim{}, false, err
			}
			return claim, true, nil
		}
	}
	slug := core.NormalizeLeaseSlug(identifier)
	if slug != "" {
		for _, claim := range claims {
			if claim.Provider == provider && core.NormalizeLeaseSlug(claim.Slug) == slug {
				if err := validate(claim); err != nil {
					return core.LeaseClaim{}, false, err
				}
				return claim, true, nil
			}
		}
	}
	return core.LeaseClaim{}, false, nil
}

func FinishScopedLease(claim core.LeaseClaim, opts ScopedLeaseFinishOptions) (string, string, string, error) {
	if err := opts.ValidateClaim(claim); err != nil {
		return "", "", "", err
	}
	if opts.RepoRoot != "" {
		idleTimeout := opts.IdleTimeout
		if idleTimeout <= 0 {
			idleTimeout = time.Duration(claim.IdleTimeoutSeconds) * time.Second
		}
		if err := core.ClaimLeaseForRepoProviderScopePond(claim.LeaseID, claim.Slug, opts.Provider, claim.ProviderScope, claim.Pond, opts.RepoRoot, idleTimeout, opts.Reclaim); err != nil {
			return "", "", "", err
		}
	}
	slug := claim.Slug
	if strings.TrimSpace(slug) == "" {
		slug = core.NewLeaseSlug(claim.LeaseID)
	}
	return claim.LeaseID, strings.TrimPrefix(claim.LeaseID, opts.LeasePrefix), slug, nil
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
