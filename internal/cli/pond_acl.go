package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/tailscale/hujson"
)

// pondACLEnsureTimeout bounds each request made by pondACLEnsure so the auth-
// key mint path stays responsive even when Tailscale is slow. The total time
// budget is roughly twice this (one GET + at most one PUT).
const pondACLEnsureTimeout = 6 * time.Second

// Tailnet API base URL resolution. CRABBOX_TS_API_URL wins over TS_API_URL so
// operators can point the CLI at a self-hosted control plane (e.g. Headscale)
// without leaking that override into other Tailscale tooling running in the
// same shell. The default is the Tailscale-hosted REST API.
const (
	tailnetAPIURLEnvVar        = "TS_API_URL"
	crabboxTailnetAPIURLEnvVar = "CRABBOX_TS_API_URL"
	defaultTailnetAPIURL       = "https://api.tailscale.com"
)

// resolveTailnetAPIURL returns the base URL of the tailnet REST API the CLI
// should talk to for pond ACL bootstrap. Trailing slashes are stripped so the
// callers can append paths without thinking about it.
func resolveTailnetAPIURL() string {
	for _, v := range []string{os.Getenv(crabboxTailnetAPIURLEnvVar), os.Getenv(tailnetAPIURLEnvVar)} {
		if v = strings.TrimRight(strings.TrimSpace(v), "/"); v != "" {
			return v
		}
	}
	return defaultTailnetAPIURL
}

// ErrPondACLAutoBootstrapUnavailable is returned when the configured tailnet
// control plane (e.g. Headscale) does not expose a Tailscale-compatible
// policy REST API. Callers fall back to the manual snippet in
// docs/features/pond.md instead of failing the lease creation.
var ErrPondACLAutoBootstrapUnavailable = errors.New("pond acl: auto-bootstrap unavailable on this control plane")

// pondACLMaxAttempts caps the GET → merge → PUT loop. The first 412 from PUT
// almost always reflects a benign concurrent edit (another operator running
// the same bootstrap path), so re-reading the policy and retrying once is
// safe. Two attempts is the smallest value that lets us tolerate a single
// race; persistent 412s are surfaced as a clear error.
const pondACLMaxAttempts = 2

// errPondACLPreconditionFailed is returned by PutPolicy when the server
// rejected the write with HTTP 412 (ETag mismatch). It is wrapped so callers
// can detect the race via errors.Is and decide whether to retry.
var errPondACLPreconditionFailed = errors.New("pond acl: tailnet policy changed during update (ETag mismatch)")

// pondTailnetACLClient is satisfied by anything that can read and update the
// tailnet policy file. The real implementation hits the Tailscale API; tests
// inject a stub so unit tests never reach the network.
type pondTailnetACLClient interface {
	GetPolicy(ctx context.Context, tailnet string) (body string, etag string, err error)
	PutPolicy(ctx context.Context, tailnet string, body string, etag string) error
}

// pondTailnetACLClientFactory is overridden in tests. It returns nil when no
// API key is available so callers can fall through to the manual-setup path.
var pondTailnetACLClientFactory = newPondTailnetACLClient

// pondACLEnsure makes sure the concrete pond tag is declared in tagOwners and
// covered by a self-peering grant on the operator's tailnet. It is a no-op
// when the rows are already present. When changes are needed the function
// reads the current HuJSON policy with an ETag, applies only the missing
// entries, and PUTs it back with If-Match so concurrent edits fail-fast.
func pondACLEnsure(ctx context.Context, client pondTailnetACLClient, tailnet, owner, pond string) error {
	if client == nil {
		return fmt.Errorf("pond acl client unavailable")
	}
	tag := pondTailscaleTag(owner, pond)
	if tag == "" {
		return fmt.Errorf("pond acl: empty tag (owner=%q pond=%q)", owner, pond)
	}
	if tailnet == "" {
		tailnet = "-"
	}
	var lastPutErr error
	for attempt := 1; attempt <= pondACLMaxAttempts; attempt++ {
		getCtx, cancelGet := context.WithTimeout(ctx, pondACLEnsureTimeout)
		body, etag, err := client.GetPolicy(getCtx, tailnet)
		cancelGet()
		if err != nil {
			return fmt.Errorf("pond acl: read policy: %w", err)
		}
		if pondACLRowPresent(body, tag) {
			return nil
		}
		merged, err := pondACLMergePolicy(body, tag)
		if err != nil {
			return err
		}
		putCtx, cancelPut := context.WithTimeout(ctx, pondACLEnsureTimeout)
		err = client.PutPolicy(putCtx, tailnet, merged, etag)
		cancelPut()
		if err == nil {
			return nil
		}
		// A 412 means another writer raced us; re-read the policy with a
		// fresh ETag and retry. Once the retry budget is exhausted, surface
		// the race with a distinct error so operators know rerunning is
		// safe (vs. a hard auth failure). Any non-412 error is fatal — the
		// caller falls back to the manual snippet.
		if errors.Is(err, errPondACLPreconditionFailed) {
			lastPutErr = err
			if attempt < pondACLMaxAttempts {
				continue
			}
			return fmt.Errorf("pond acl: ETag race persisted after %d attempts: %w", pondACLMaxAttempts, lastPutErr)
		}
		return fmt.Errorf("pond acl: update policy: %w", err)
	}
	// Defensive: the loop body always returns or continues; reaching here
	// would indicate a bug in the loop bounds.
	return fmt.Errorf("pond acl: ETag race persisted after %d attempts: %w", pondACLMaxAttempts, lastPutErr)
}

type pondACLRule struct {
	Action string   `json:"action"`
	Src    []string `json:"src"`
	Dst    []string `json:"dst"`
}

type pondGrantRule struct {
	Src []string `json:"src"`
	Dst []string `json:"dst"`
	IP  []string `json:"ip"`
}

type pondACLPolicyState struct {
	value              hujson.Value
	rootMembers        int
	tagOwnerMembers    int
	grantEntries       int
	aclEntries         int
	rootTrailingComma  bool
	ownerTrailingComma bool
	grantTrailingComma bool
	aclTrailingComma   bool
	hasTagOwners       bool
	hasGrants          bool
	hasACLs            bool
	ownerPresent       bool
	grantPresent       bool
	aclPresent         bool
}

func (s pondACLPolicyState) accessPresent() bool {
	return s.grantPresent || s.aclPresent
}

// pondACLMergePolicy uses Tailscale's HuJSON syntax tree and JSON Patch
// implementation so untouched policy bytes remain byte-for-byte stable.
// Unsupported or ambiguous section shapes fail closed to the manual setup path.
func pondACLMergePolicy(body, tag string) (string, error) {
	state, err := parsePondACLPolicy(body, tag)
	if err != nil {
		return "", err
	}
	if state.ownerPresent && state.accessPresent() {
		return body, nil
	}

	type patchOperation struct {
		Op    string `json:"op"`
		Path  string `json:"path"`
		Value any    `json:"value"`
	}
	var patch []patchOperation
	if !state.ownerPresent {
		if state.hasTagOwners {
			patch = append(patch, patchOperation{
				Op:    "add",
				Path:  "/tagOwners/" + hujsonPointerToken(tag),
				Value: []string{"autogroup:admin"},
			})
		} else {
			patch = append(patch, patchOperation{
				Op:    "add",
				Path:  "/tagOwners",
				Value: map[string][]string{tag: {"autogroup:admin"}},
			})
		}
	}
	if !state.accessPresent() {
		switch {
		case state.hasGrants:
			patch = append(patch, patchOperation{
				Op:   "add",
				Path: "/grants/-",
				Value: pondGrantRule{
					Src: []string{tag},
					Dst: []string{tag},
					IP:  []string{"*"},
				},
			})
		case state.hasACLs:
			patch = append(patch, patchOperation{
				Op:   "add",
				Path: "/acls/-",
				Value: pondACLRule{
					Action: "accept",
					Src:    []string{tag},
					Dst:    []string{tag + ":*"},
				},
			})
		default:
			patch = append(patch, patchOperation{
				Op:   "add",
				Path: "/acls",
				Value: []pondACLRule{{
					Action: "accept",
					Src:    []string{tag},
					Dst:    []string{tag + ":*"},
				}},
			})
		}
	}

	patchBody, err := json.Marshal(patch)
	if err != nil {
		return "", fmt.Errorf("pond acl: build HuJSON patch: %w", err)
	}
	candidate := state.value
	if err := candidate.Patch(patchBody); err != nil {
		return "", fmt.Errorf("pond acl: safely apply HuJSON patch (apply the pond rows manually): %w", err)
	}
	formatPondACLInsertions(&candidate, state)
	restorePondACLTrailingCommas(&candidate, state)
	merged := string(candidate.Pack())
	verified, err := parsePondACLPolicy(merged, tag)
	if err != nil {
		return "", fmt.Errorf("pond acl: verify patched HuJSON policy: %w", err)
	}
	if !verified.ownerPresent || !verified.accessPresent() {
		return "", fmt.Errorf("pond acl: patched HuJSON policy is missing the required pond rows")
	}
	return merged, nil
}

func parsePondACLPolicy(body, tag string) (pondACLPolicyState, error) {
	if strings.TrimSpace(body) == "" {
		return pondACLPolicyState{}, fmt.Errorf("pond acl: empty policy body")
	}
	value, err := hujson.Parse([]byte(body))
	if err != nil {
		return pondACLPolicyState{}, fmt.Errorf("pond acl: cannot safely parse HuJSON policy (apply the pond rows manually): %w", err)
	}
	root, ok := value.Value.(*hujson.Object)
	if !ok {
		return pondACLPolicyState{}, fmt.Errorf("pond acl: policy root must be an object")
	}

	state := pondACLPolicyState{
		value:             value,
		rootMembers:       len(root.Members),
		rootTrailingComma: hujsonTrailingComma(&value),
	}
	seen := make(map[string]bool, len(root.Members))
	for _, member := range root.Members {
		name := member.Name.Value.(hujson.Literal).String()
		if seen[name] {
			return pondACLPolicyState{}, fmt.Errorf("pond acl: duplicate top-level policy field %q", name)
		}
		seen[name] = true
		switch name {
		case "tagOwners":
			owners, ok := member.Value.Value.(*hujson.Object)
			if !ok {
				return pondACLPolicyState{}, fmt.Errorf("pond acl: tagOwners must be an object")
			}
			state.hasTagOwners = true
			state.tagOwnerMembers = len(owners.Members)
			state.ownerTrailingComma = hujsonTrailingComma(&member.Value)
			ownerNames := make(map[string]bool, len(owners.Members))
			for _, owner := range owners.Members {
				ownerName := owner.Name.Value.(hujson.Literal).String()
				if ownerNames[ownerName] {
					return pondACLPolicyState{}, fmt.Errorf("pond acl: duplicate tagOwners entry %q", ownerName)
				}
				ownerNames[ownerName] = true
				state.ownerPresent = state.ownerPresent || ownerName == tag
			}
		case "grants":
			grants, ok := member.Value.Value.(*hujson.Array)
			if !ok {
				return pondACLPolicyState{}, fmt.Errorf("pond acl: grants must be an array")
			}
			state.hasGrants = true
			state.grantEntries = len(grants.Elements)
			state.grantTrailingComma = hujsonTrailingComma(&member.Value)
		case "acls":
			acls, ok := member.Value.Value.(*hujson.Array)
			if !ok {
				return pondACLPolicyState{}, fmt.Errorf("pond acl: acls must be an array")
			}
			state.hasACLs = true
			state.aclEntries = len(acls.Elements)
			state.aclTrailingComma = hujsonTrailingComma(&member.Value)
		}
	}

	standardized, err := hujson.Standardize([]byte(body))
	if err != nil {
		return pondACLPolicyState{}, fmt.Errorf("pond acl: cannot standardize HuJSON policy: %w", err)
	}
	var semantic struct {
		Grants []pondGrantRule `json:"grants"`
		ACLs   []pondACLRule   `json:"acls"`
	}
	if err := json.Unmarshal(standardized, &semantic); err != nil {
		return pondACLPolicyState{}, fmt.Errorf("pond acl: cannot safely decode policy sections: %w", err)
	}
	for _, grant := range semantic.Grants {
		state.grantPresent = state.grantPresent ||
			pondStringSliceContains(grant.Src, tag) &&
				pondStringSliceContains(grant.Dst, tag) &&
				len(grant.IP) > 0
	}
	for _, acl := range semantic.ACLs {
		state.aclPresent = state.aclPresent ||
			pondStringSliceContains(acl.Src, tag) &&
				(pondStringSliceContains(acl.Dst, tag) || pondStringSliceContains(acl.Dst, tag+":*"))
	}
	return state, nil
}

func pondACLRowPresent(policy, tag string) bool {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return false
	}
	state, err := parsePondACLPolicy(policy, tag)
	return err == nil && state.ownerPresent && state.accessPresent()
}

func pondStringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func hujsonPointerToken(value string) string {
	value = strings.ReplaceAll(value, "~", "~0")
	return strings.ReplaceAll(value, "/", "~1")
}

func formatPondACLInsertions(value *hujson.Value, state pondACLPolicyState) {
	if root, ok := value.Value.(*hujson.Object); ok {
		formatAppendedHuJSONMembers(root, state.rootMembers)
	}
	if !state.ownerPresent && state.hasTagOwners {
		if owners := value.Find("/tagOwners"); owners != nil {
			if object, ok := owners.Value.(*hujson.Object); ok {
				formatAppendedHuJSONMembers(object, state.tagOwnerMembers)
			}
		}
	}
	if !state.accessPresent() {
		switch {
		case state.hasGrants:
			if grants := value.Find("/grants"); grants != nil {
				if array, ok := grants.Value.(*hujson.Array); ok {
					formatAppendedHuJSONElements(array, state.grantEntries)
				}
			}
		case state.hasACLs:
			if acls := value.Find("/acls"); acls != nil {
				if array, ok := acls.Value.(*hujson.Array); ok {
					formatAppendedHuJSONElements(array, state.aclEntries)
				}
			}
		}
	}
}

func formatAppendedHuJSONMembers(object *hujson.Object, start int) {
	for i := start; i < len(object.Members); i++ {
		if i > 0 && object.Members[i].Name.BeforeExtra.IsStandard() {
			object.Members[i].Name.BeforeExtra = pondHuJSONSiblingExtra(object.Members[i-1].Name.BeforeExtra)
		}
		object.Members[i].Value.BeforeExtra = hujson.Extra(" ")
	}
}

func formatAppendedHuJSONElements(array *hujson.Array, start int) {
	for i := start; i < len(array.Elements); i++ {
		if i > 0 && array.Elements[i].BeforeExtra.IsStandard() {
			array.Elements[i].BeforeExtra = pondHuJSONSiblingExtra(array.Elements[i-1].BeforeExtra)
		}
	}
}

func pondHuJSONSiblingExtra(existing hujson.Extra) hujson.Extra {
	if newline := bytes.LastIndexByte(existing, '\n'); newline >= 0 {
		indent := existing[newline+1:]
		if len(bytes.Trim(indent, " \t")) == 0 {
			return append(hujson.Extra{'\n'}, indent...)
		}
	}
	if len(existing) > 0 && len(bytes.Trim(existing, " \t")) == 0 {
		return hujson.Extra(" ")
	}
	return nil
}

func restorePondACLTrailingCommas(value *hujson.Value, state pondACLPolicyState) {
	// HuJSON represents a trailing comma with a non-nil AfterExtra, including
	// an empty slice. Appending changes the last value, so carry that marker
	// to the new last value without formatting the surrounding container.
	if state.rootTrailingComma {
		setHuJSONTrailingComma(value)
	}
	if state.ownerTrailingComma {
		setHuJSONTrailingComma(value.Find("/tagOwners"))
	}
	if state.grantTrailingComma {
		setHuJSONTrailingComma(value.Find("/grants"))
	}
	if state.aclTrailingComma {
		setHuJSONTrailingComma(value.Find("/acls"))
	}
}

func hujsonTrailingComma(value *hujson.Value) bool {
	if value == nil {
		return false
	}
	switch composite := value.Value.(type) {
	case *hujson.Object:
		return len(composite.Members) > 0 && composite.Members[len(composite.Members)-1].Value.AfterExtra != nil
	case *hujson.Array:
		return len(composite.Elements) > 0 && composite.Elements[len(composite.Elements)-1].AfterExtra != nil
	default:
		return false
	}
}

func setHuJSONTrailingComma(value *hujson.Value) {
	if value == nil {
		return
	}
	switch composite := value.Value.(type) {
	case *hujson.Object:
		if len(composite.Members) > 0 && composite.Members[len(composite.Members)-1].Value.AfterExtra == nil {
			composite.Members[len(composite.Members)-1].Value.AfterExtra = hujson.Extra{}
		}
	case *hujson.Array:
		if len(composite.Elements) > 0 && composite.Elements[len(composite.Elements)-1].AfterExtra == nil {
			composite.Elements[len(composite.Elements)-1].AfterExtra = hujson.Extra{}
		}
	}
}

// livePondTailnetACLClient is the production implementation. It targets the
// documented "Get tailnet policy file" and "Set tailnet policy file"
// endpoints and threads ETag through so concurrent edits fail-fast.
type livePondTailnetACLClient struct {
	apiKey string
	http   *http.Client
}

func newPondTailnetACLClient(apiKey string) pondTailnetACLClient {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil
	}
	return &livePondTailnetACLClient{apiKey: apiKey, http: &http.Client{Timeout: pondACLEnsureTimeout}}
}

func (c *livePondTailnetACLClient) GetPolicy(ctx context.Context, tailnet string) (string, string, error) {
	if tailnet == "" {
		tailnet = "-"
	}
	url := resolveTailnetAPIURL() + "/api/v2/tailnet/" + tailnet + "/acl"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", err
	}
	req.SetBasicAuth(c.apiKey, "")
	req.Header.Set("Accept", "application/hujson")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if readErr != nil {
		return "", "", readErr
	}
	// 404/501 mean the configured control plane (e.g. Headscale) does not
	// expose Tailscale's `/api/v2/tailnet/.../acl` route. Surface a distinct
	// sentinel so callers fall back to the manual snippet rather than
	// erroring the lease creation path.
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNotImplemented {
		return "", "", fmt.Errorf("%w: GET %s returned %d", ErrPondACLAutoBootstrapUnavailable, url, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", tailscaleAPIError(resp.StatusCode, body)
	}
	// Missing ETag on a 2xx response means concurrent-edit safety via
	// If-Match isn't possible — we never PUT without a CAS token, so treat
	// this as "auto-bootstrap unavailable" and let the manual path handle it.
	etag := resp.Header.Get("ETag")
	if etag == "" {
		return "", "", fmt.Errorf("%w: GET %s returned no ETag header", ErrPondACLAutoBootstrapUnavailable, url)
	}
	return string(body), etag, nil
}

func (c *livePondTailnetACLClient) PutPolicy(ctx context.Context, tailnet, body, etag string) error {
	if tailnet == "" {
		tailnet = "-"
	}
	url := resolveTailnetAPIURL() + "/api/v2/tailnet/" + tailnet + "/acl"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte(body)))
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.apiKey, "")
	req.Header.Set("Content-Type", "application/hujson")
	req.Header.Set("Accept", "application/json")
	if etag != "" {
		req.Header.Set("If-Match", etag)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if resp.StatusCode == http.StatusPreconditionFailed {
		return errPondACLPreconditionFailed
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return tailscaleAPIError(resp.StatusCode, respBody)
	}
	return nil
}

func tailscaleAPIError(status int, body []byte) error {
	var envelope struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &envelope) == nil && envelope.Message != "" {
		return fmt.Errorf("tailscale api %d: %s", status, envelope.Message)
	}
	return fmt.Errorf("tailscale api %d", status)
}
