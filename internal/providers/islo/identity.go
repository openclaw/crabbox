package islo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	gosdk "github.com/islo-labs/go-sdk"
	sdkcore "github.com/islo-labs/go-sdk/core"
	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const (
	isloCreatedByLabel       = "islo_created_by"
	isloCreatedByEntityLabel = "islo_created_by_entity"
	// isloSandboxNameLabel records the sandbox name a claim is bound to. The
	// name lives in a label rather than in claim.CloudID because CloudID is the
	// provider's resource identifier everywhere else in the tree (see
	// isloSandboxToServer, which reports the sandbox id as Server.CloudID, and
	// core's claim/status code, which compares the two for equality).
	isloSandboxNameLabel = "islo_sandbox_name"
)

// isloIdentity describes one provider resource generation. The provider ID
// identifies that generation; its name is a mutable addressing handle. A
// matching by-ID response with terminal status "deleted" can prove completion,
// while a bare 404 does not. DELETE remains name-based, so identity reads cannot
// make the later deletion atomic with respect to name reuse.
type isloIdentity struct {
	ID              string
	Name            string
	CreatedBy       string
	CreatedByEntity string
}

func isloIdentityFromSandbox(sandbox *gosdk.SandboxResponse) isloIdentity {
	if sandbox == nil {
		return isloIdentity{}
	}
	return isloIdentity{
		ID:              strings.TrimSpace(sandbox.GetID()),
		Name:            strings.TrimSpace(sandbox.GetName()),
		CreatedBy:       strings.TrimSpace(stringPointerValue(sandbox.GetCreatedBy())),
		CreatedByEntity: isloExtraString(sandbox, "created_by_entity"),
	}
}

// A successful by-ID lookup is evidence only for the ID actually requested.
func requireIsloByIDResponse(resourceID string, sandbox *gosdk.SandboxResponse) error {
	observed := isloIdentityFromSandbox(sandbox).ID
	if resourceID == "" || observed != resourceID {
		return exit(5, "islo by-id response for resource %q reported id %q; refusing to use unverified resource identity", resourceID, observed)
	}
	return nil
}

// isloExtraString reads a field the generated SDK model does not carry yet.
// `created_by_entity` is returned by the live API but absent from the pinned
// SDK struct, so it only reaches us through the extra-properties bag.
func isloExtraString(sandbox *gosdk.SandboxResponse, field string) string {
	if sandbox == nil {
		return ""
	}
	value, ok := sandbox.GetExtraProperties()[field]
	if !ok {
		return ""
	}
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}

func stringPointerValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// isloClaimIdentity reads back the identity a previous run bound to the claim.
func isloClaimIdentity(claim core.LeaseClaim) isloIdentity {
	return isloIdentity{
		ID:              strings.TrimSpace(claim.CloudImmutableID),
		Name:            strings.TrimSpace(claim.Labels[isloSandboxNameLabel]),
		CreatedBy:       strings.TrimSpace(claim.Labels[isloCreatedByLabel]),
		CreatedByEntity: strings.TrimSpace(claim.Labels[isloCreatedByEntityLabel]),
	}
}

// Bookkeeping may change during a run, but it cannot grant cleanup authority
// over another resource or repository. ClaimedAt is existing reclaim metadata,
// not a unique incarnation token; the final cleanup still fences the full claim.
func sameIsloRunOwnership(acquired, current core.LeaseClaim) bool {
	return acquired.LeaseID == current.LeaseID &&
		acquired.Provider == current.Provider &&
		acquired.ProviderScope == current.ProviderScope &&
		acquired.CloudID == current.CloudID &&
		acquired.CloudImmutableID == current.CloudImmutableID &&
		acquired.Labels[isloSandboxNameLabel] == current.Labels[isloSandboxNameLabel] &&
		acquired.RepoRoot == current.RepoRoot &&
		acquired.ClaimedAt == current.ClaimedAt
}

// isloClaimScope records which Islo API endpoint a claim was created against,
// so a later teardown can refuse to interpret a 404 obtained from a different
// endpoint as proof that our resource is gone. It is the value core writes into
// claim.ProviderScope for this provider (see Provider.ClaimScope), so core's own
// scope comparisons and this guard read the same string.
//
// The value is cfg.Islo.BaseURL, which defaults to the control-plane host
// https://api.islo.dev; it is not a region identifier and it is not a credential
// fingerprint. Two different API keys pointed at the same endpoint therefore
// share a scope, so this guard separates endpoints only.
func isloClaimScope(cfg Config) string {
	endpoint := shared.NormalizedSandboxClaimEndpoint(blank(strings.TrimSpace(cfg.Islo.BaseURL), isloDefaultBaseURL))
	if endpoint == "" {
		return ""
	}
	return "endpoint:" + endpoint
}

func (b *isloBackend) claimScope() string { return isloClaimScope(b.cfg) }

func (identity isloIdentity) labels() map[string]string {
	labels := map[string]string{isloSandboxNameLabel: identity.Name}
	if identity.CreatedBy != "" {
		labels[isloCreatedByLabel] = identity.CreatedBy
	}
	if identity.CreatedByEntity != "" {
		labels[isloCreatedByEntityLabel] = identity.CreatedByEntity
	}
	return labels
}

// Publish the resource identity with its claim, never as a later overwrite of
// whichever claim happens to occupy the name after the provider lookup.
func (b *isloBackend) publishIsloClaim(ctx context.Context, leaseID, slug, repoRoot string, identity isloIdentity) (core.LeaseClaim, error) {
	if identity.ID == "" || identity.Name == "" {
		return core.LeaseClaim{}, exit(5, "islo sandbox %q returned incomplete resource identity; refusing to publish an unbound claim", identity.Name)
	}
	cfg := b.cfg
	cfg.Provider = isloProvider
	server := Server{
		Provider: isloProvider, CloudID: identity.ID, ImmutableID: identity.ID,
		Name: identity.Name, Labels: identity.labels(),
	}
	if repoRoot == "" {
		return core.ClaimLeaseTargetForConfigScopeIfUnchanged(leaseID, slug, cfg, b.claimScope(), server, core.SSHTarget{}, cfg.IdleTimeout, core.LeaseClaim{}, false)
	}
	return core.ClaimLeaseTargetForRepoConfigScopeIfUnchangedDurableAfterContext(ctx, leaseID, slug, cfg, b.claimScope(), server, core.SSHTarget{}, repoRoot, cfg.IdleTimeout, false, core.LeaseClaim{}, false, nil)
}

// requireIsloClaimScope refuses to act when the claim was bound to a different
// API endpoint than the one the current credentials talk to. Without this a
// 404 from an unrelated endpoint would look exactly like "our sandbox is gone".
func requireIsloClaimScope(claim core.LeaseClaim, scope string) error {
	bound := strings.TrimSpace(claim.ProviderScope)
	// Claims written before the identity binding existed carry no scope. They
	// are still usable, but they cannot contribute to a deletion proof, which
	// is why the teardown path grades its proofs by what the claim records.
	if bound == "" || bound == scope {
		return nil
	}
	return exit(4, "islo lease %q was claimed against %s but the current credentials target %s; refusing to act on a resource this scope cannot address", claim.LeaseID, bound, scope)
}

// requireIsloIdentityMatch checks that a live sandbox is the same resource the
// claim owns before any destructive call. It returns an advisory message for
// differences that must be reported but must not block, and an error only for a
// difference that positively proves this is not our resource.
//
// Only the immutable id is fatal. created_by is the API KEY NAME and
// created_by_entity its kind ("api_key"): attribution reported by the same
// tenant-scoped API we are already trusting, which any key that can create a
// sandbox reproduces. Making a difference fatal would therefore buy no security
// while permanently stranding a lease, and its billed sandbox, because there is
// no force override on `crabbox stop`. They are advisory only, and are never a
// security boundary.
func requireIsloIdentityMatch(claim core.LeaseClaim, observed isloIdentity) (string, error) {
	bound := isloClaimIdentity(claim)
	if bound.ID != "" && observed.ID != "" && bound.ID != observed.ID {
		// Guard against a name that resolves to a resource generation this
		// lease does not own. Whether the API ever re-issues a released name to
		// a new sandbox is UNCONFIRMED against the live service; if it never
		// does, this branch simply never fires.
		return "", exit(4, "islo sandbox %q now resolves to resource %s but lease %q owns %s; refusing to act on a resource this lease does not own", observed.Name, observed.ID, claim.LeaseID, bound.ID)
	}
	var advisories []string
	for _, field := range []struct{ what, bound, observed string }{
		{"created_by", bound.CreatedBy, observed.CreatedBy},
		{"created_by_entity", bound.CreatedByEntity, observed.CreatedByEntity},
	} {
		if field.bound != "" && field.observed != "" && field.bound != field.observed {
			advisories = append(advisories, fmt.Sprintf("%s is now %q but lease %q recorded %q", field.what, field.observed, claim.LeaseID, field.bound))
		}
	}
	if len(advisories) == 0 {
		return "", nil
	}
	return fmt.Sprintf("islo sandbox %q reports different creator attribution than the lease recorded: %s; attribution only corroborates ownership, so this is advisory only and does not block the operation", observed.Name, strings.Join(advisories, "; ")), nil
}

// A deletion timestamp alone does not prove the sandbox has reached a terminal
// state. Callers must also verify that the response identifies their resource.
func isloSandboxDeleted(sandbox *gosdk.SandboxResponse) bool {
	return sandbox != nil && strings.EqualFold(strings.TrimSpace(sandbox.GetStatus()), "deleted")
}

// isloNotFound reports whether an API call failed because the resource is not
// visible to these credentials. The SDK surfaces this as a typed
// *gosdk.NotFoundError; the raw endpoints surface it as an *sdkcore.APIError.
func isloNotFound(err error) bool {
	if err == nil {
		return false
	}
	var notFound *gosdk.NotFoundError
	if errors.As(err, &notFound) {
		return true
	}
	var apiErr *sdkcore.APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return true
	}
	return false
}

// isloSandboxAbsentByName normalizes the two shapes a missing sandbox takes on
// the by-name lookup: a 404 error, or a nil body with no error. A non-nil body
// is never treated as absence, however incomplete it looks: `name` is required
// on SandboxResponse, so a body with an empty name is a malformed response, not
// evidence of a deletion.
func isloSandboxAbsentByName(sandbox *gosdk.SandboxResponse, err error) bool {
	if err != nil {
		return isloNotFound(err)
	}
	return sandbox == nil
}

func isloIdentityString(identity isloIdentity) string {
	if identity.ID == "" {
		return fmt.Sprintf("name=%s", identity.Name)
	}
	return fmt.Sprintf("name=%s id=%s", identity.Name, identity.ID)
}

// isloReportedResourceID decides which resource id, if any, is safe to report as
// providerResourceId. When the read fell back to the sandbox name and landed on a
// resource the claim does not own, no id is reported: an id that automation keys
// off must never name a resource other than the one the lease owns. The
// islo_resource_id_mismatch labels carry the detail in that case.
func isloReportedResourceID(mismatched bool, resourceID, claimedID string) string {
	if mismatched {
		return ""
	}
	return blank(resourceID, claimedID)
}
