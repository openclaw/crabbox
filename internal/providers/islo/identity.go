package islo

import (
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

// isloIdentity is the provider-side identity of one sandbox generation.
//
// A sandbox has two identifiers and they are not interchangeable:
//
//   - `id` is an immutable UUIDv7 the API assigns. `GET /sandboxes/-/by-id/{id}`
//     keeps answering 200 after the sandbox is deleted, with status "deleted"
//     and deleted_at set, so the id is the only handle that can prove a
//     specific resource generation is gone.
//   - `name` is a caller-supplied namespace key. It stops resolving the instant
//     a delete returns (`GET /sandboxes/{name}` answers 404 authoritatively),
//     and it can be occupied by a record that is not the resource the caller
//     believes it created: a create that fails on billing leaves a "failed"
//     sandbox holding the name.
//
// What the name cannot do is identify a resource *generation*, which is why
// teardown proofs are anchored on the id.
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

// bindIsloClaimIdentity persists the immutable provider id (plus the creator
// attribution the API reports and the sandbox name) on an existing claim so the
// binding survives a restart and later teardowns can address the exact resource
// generation. It is a no-op when there is no claim to bind or when the claim
// already carries the same identity.
//
// claim.ProviderScope is deliberately not written here: it is a core field with
// core semantics, so it is supplied through the claim helper that writes it for
// every provider (Provider.ClaimScope).
func bindIsloClaimIdentity(leaseID string, identity isloIdentity) error {
	if identity.ID == "" && identity.Name == "" {
		return nil
	}
	return withDurableLeaseClaimLock(leaseID, func(claim *core.LeaseClaim, exists bool, persist func() error) error {
		if !exists || claim.Provider != isloProvider {
			return nil
		}
		changed := false
		set := func(target *string, value string) {
			if value != "" && *target != value {
				*target = value
				changed = true
			}
		}
		// CloudID and CloudImmutableID both carry the sandbox id: core treats
		// claim.CloudID and Server.CloudID as the same value, and
		// isloSandboxToServer publishes the id in both Server fields.
		set(&claim.CloudID, identity.ID)
		set(&claim.CloudImmutableID, identity.ID)
		for label, value := range map[string]string{
			isloSandboxNameLabel:     identity.Name,
			isloCreatedByLabel:       identity.CreatedBy,
			isloCreatedByEntityLabel: identity.CreatedByEntity,
		} {
			if value == "" || claim.Labels[label] == value {
				continue
			}
			if claim.Labels == nil {
				claim.Labels = map[string]string{}
			}
			claim.Labels[label] = value
			changed = true
		}
		if !changed {
			return nil
		}
		return persist()
	})
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

// isloSandboxDeleted reports whether a by-id response is a tombstone. The
// probed API answered 200 for a deleted sandbox with status "deleted" and
// deleted_at set; either signal is accepted.
//
// ASSUMPTION: deleted_at is not populated while a delete is still in flight. If
// the API ever sets deleted_at on a "stopping"/"deleting" sandbox, this would
// confirm a delete that has not finished. The consequence is bounded: the local
// claim is dropped while the platform finishes a delete it has already accepted,
// which is the same window the previous unconditional teardown always had.
func isloSandboxDeleted(sandbox *gosdk.SandboxResponse) bool {
	if sandbox == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(sandbox.GetStatus()), "deleted") {
		return true
	}
	return strings.TrimSpace(stringPointerValue(sandbox.GetDeletedAt())) != ""
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
