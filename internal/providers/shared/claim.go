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

// PreserveClaimIdentityLabels clones observations without granting new cleanup
// authority: protected values must come from the recorded claim. It returns the
// first conflicting key in caller order; adapters own the resulting diagnostic.
func PreserveClaimIdentityLabels(observed, recorded map[string]string, keys ...string) (map[string]string, string) {
	labels := CloneLabels(observed)
	for _, key := range keys {
		stored := recorded[key]
		if current := labels[key]; stored != "" && current != "" && current != stored {
			return nil, key
		}
		if stored != "" {
			labels[key] = stored
		} else {
			delete(labels, key)
		}
	}
	return labels, ""
}

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

// AdmitResolvedLease authorizes activity on the observed claim and conditionally
// commits repository admission. Callers own eligibility, provider identity checks,
// and projection of the returned committed claim; observations must not call this.
func AdmitResolvedLease(cfg core.Config, req core.ResolveRequest, target core.LeaseTarget, slug string, expected core.LeaseClaim, exists bool, idleOverride *time.Duration) (core.LeaseClaim, error) {
	if exists {
		if err := AuthorizeClaimActivity(expected); err != nil {
			return core.LeaseClaim{}, err
		}
	}
	return core.ClaimLeaseTargetForRepoConfigWithIdleTimeoutOverrideIfUnchanged(target.LeaseID, slug, cfg, target.Server, target.SSH, req.Repo.Root, cfg.IdleTimeout, idleOverride, req.Reclaim, expected, exists)
}

// RefreshRetainedLeaseActivity refreshes an existing claim after an admitted
// delegated run. The caller retains its provider operation lock; this does not
// perform admission or replace the core's recorded idle-timeout policy.
func RefreshRetainedLeaseActivity(leaseID, provider string, idleTimeout time.Duration) error {
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		return err
	}
	if claim.LeaseID == "" {
		return nil
	}
	if idleTimeout <= 0 {
		idleTimeout = time.Duration(claim.IdleTimeoutSeconds) * time.Second
	}
	return core.ClaimLeaseForRepoProviderScopePond(claim.LeaseID, claim.Slug, provider, claim.ProviderScope, claim.Pond, claim.RepoRoot, idleTimeout, false)
}

// ValidateSandboxOwnershipMetadata checks the common remote sandbox markers.
// Endpoint admission and binding the response ID to a requested resource remain
// caller-owned; missing metadata keys retain their existing empty-value semantics.
func ValidateSandboxOwnershipMetadata(provider, sandboxID string, metadata map[string]string, claim core.LeaseClaim) error {
	if sandboxID == "" {
		return core.Exit(5, "%s returned a sandbox without an id", provider)
	}
	if metadata["crabbox.provider"] != provider ||
		metadata["crabbox.scope"] != claim.ProviderScope ||
		metadata["crabbox.claim"] != claim.LeaseID {
		return core.Exit(4, "%s sandbox %q ownership metadata does not match its local claim", provider, sandboxID)
	}
	return nil
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

// RemoveExactClaimAfterContext validates the binding and waits for the claim fence with ctx.
// The action must honor ctx itself and must not reenter claim operations.
// Successful actions still complete durable claim removal after cancellation.
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

// LabelsWithDefaults copies labels and fills missing or empty values. Whitespace
// is a stored value, and an empty default still creates the corresponding key.
func LabelsWithDefaults(labels, defaults map[string]string) map[string]string {
	labels = CloneLabels(labels)
	for key, value := range defaults {
		if labels[key] == "" {
			labels[key] = value
		}
	}
	return labels
}

// IndexProviderClaims indexes stored snapshots using adapter-owned resource
// keys. Empty keys are skipped and later snapshots win duplicate keys. The
// index is a lookup aid; it does not grant ownership or mutation authority.
func IndexProviderClaims(provider string, key func(core.LeaseClaim) string) (map[string]core.LeaseClaim, error) {
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return nil, err
	}
	out := map[string]core.LeaseClaim{}
	for _, claim := range claims {
		if claim.Provider != provider {
			continue
		}
		if name := key(claim); name != "" {
			out[name] = claim
		}
	}
	return out, nil
}
