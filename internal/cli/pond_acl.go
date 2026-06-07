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
// reads the current policy with an ETag, parses it as JSON, merges in the
// missing entries, and PUTs the result back with If-Match so concurrent
// edits fail-fast.
//
// The function is intentionally strict: if the live policy uses HuJSON
// constructs (line comments, trailing commas) that the standard JSON parser
// cannot decode, it returns a clear error so the caller falls back to the
// existing manual snippet instead of risking a destructive overwrite.
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

// pondACLMergePolicy parses the policy as JSON, ensures both the tagOwners
// entry and a self-peering grant for the tag, and returns the re-serialized
// document. Returns a clear error when the policy is not valid JSON (e.g.
// contains HuJSON comments) so the caller falls back to manual setup.
func pondACLMergePolicy(body, tag string) (string, error) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return "", fmt.Errorf("pond acl: empty policy body")
	}
	// Tailscale policies are HuJSON; standardize them before using stdlib JSON.
	standardized, hjErr := hujsonStandardize(trimmed)
	if hjErr != nil {
		standardized = trimmed
	}
	var policy map[string]json.RawMessage
	if err := json.Unmarshal([]byte(standardized), &policy); err != nil {
		return "", fmt.Errorf("pond acl: cannot merge non-JSON policy (add the tag:cbx-pond-... rows manually): %w", err)
	}
	if policy == nil {
		policy = map[string]json.RawMessage{}
	}

	// Merge tagOwners.
	tagOwners := map[string]json.RawMessage{}
	if raw, ok := policy["tagOwners"]; ok && len(raw) > 0 {
		if err := json.Unmarshal(raw, &tagOwners); err != nil {
			return "", fmt.Errorf("pond acl: cannot parse tagOwners: %w", err)
		}
	}
	if _, ok := tagOwners[tag]; !ok {
		owners, err := json.Marshal([]string{"autogroup:admin"})
		if err != nil {
			return "", err
		}
		tagOwners[tag] = owners
	}
	updatedOwners, err := json.Marshal(tagOwners)
	if err != nil {
		return "", err
	}
	policy["tagOwners"] = updatedOwners

	// Prefer grants when the policy already uses that shape, otherwise
	// append a legacy acls row. We never down-convert grants→acls.
	if raw, ok := policy["grants"]; ok && len(raw) > 0 {
		var grants []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &grants); err != nil {
			return "", fmt.Errorf("pond acl: cannot parse grants: %w", err)
		}
		grants = append(grants, pondGrantEntry(tag))
		updatedGrants, err := json.Marshal(grants)
		if err != nil {
			return "", err
		}
		policy["grants"] = updatedGrants
	} else {
		var acls []map[string]json.RawMessage
		if raw, ok := policy["acls"]; ok && len(raw) > 0 {
			if err := json.Unmarshal(raw, &acls); err != nil {
				return "", fmt.Errorf("pond acl: cannot parse acls: %w", err)
			}
		}
		acls = append(acls, pondACLEntry(tag))
		updatedACLs, err := json.Marshal(acls)
		if err != nil {
			return "", err
		}
		policy["acls"] = updatedACLs
	}

	out, err := json.MarshalIndent(policy, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func pondGrantEntry(tag string) map[string]json.RawMessage {
	src, _ := json.Marshal([]string{tag})
	dst, _ := json.Marshal([]string{tag})
	ip, _ := json.Marshal([]string{"*"})
	return map[string]json.RawMessage{
		"src": src,
		"dst": dst,
		"ip":  ip,
	}
}

func pondACLEntry(tag string) map[string]json.RawMessage {
	action, _ := json.Marshal("accept")
	src, _ := json.Marshal([]string{tag})
	dst, _ := json.Marshal([]string{tag + ":*"})
	return map[string]json.RawMessage{
		"action": action,
		"src":    src,
		"dst":    dst,
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
	req.Header.Set("Accept", "application/json")
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
	req.Header.Set("Content-Type", "application/json")
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

// hujsonStandardize strips HuJSON-only syntax (// and /* */ comments,
// trailing commas) so the result parses as standard JSON. Minimal shim to
// avoid pulling in tailscale.com/util/hujson as a new dependency — the
// trade is correctness on the policy shapes Tailscale actually emits via
// the API. Real-world policies use // line comments and trailing commas;
// neither survives stdlib encoding/json. This shim handles both without
// blocking string/escape-aware parsing.
func hujsonStandardize(body string) (string, error) {
	var b strings.Builder
	b.Grow(len(body))
	inString := false
	escaped := false
	i := 0
	for i < len(body) {
		ch := body[i]
		if inString {
			b.WriteByte(ch)
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inString = false
			}
			i++
			continue
		}
		if ch == '"' {
			inString = true
			b.WriteByte(ch)
			i++
			continue
		}
		// Line comment: //...\n
		if ch == '/' && i+1 < len(body) && body[i+1] == '/' {
			i += 2
			for i < len(body) && body[i] != '\n' {
				i++
			}
			continue
		}
		// Block comment: /* ... */
		if ch == '/' && i+1 < len(body) && body[i+1] == '*' {
			i += 2
			for i+1 < len(body) && !(body[i] == '*' && body[i+1] == '/') {
				i++
			}
			if i+1 < len(body) {
				i += 2
			} else {
				return body, fmt.Errorf("hujson: unterminated block comment")
			}
			continue
		}
		b.WriteByte(ch)
		i++
	}
	// Trailing-comma stripping: walk once, drop any `,` that is followed by
	// optional whitespace and then `]` or `}`. String/escape aware so commas
	// inside string literals are kept.
	out := b.String()
	var b2 strings.Builder
	b2.Grow(len(out))
	inStr := false
	esc := false
	for i := 0; i < len(out); i++ {
		ch := out[i]
		if inStr {
			b2.WriteByte(ch)
			if esc {
				esc = false
			} else if ch == '\\' {
				esc = true
			} else if ch == '"' {
				inStr = false
			}
			continue
		}
		if ch == '"' {
			inStr = true
			b2.WriteByte(ch)
			continue
		}
		if ch == ',' {
			j := i + 1
			for j < len(out) && (out[j] == ' ' || out[j] == '\t' || out[j] == '\n' || out[j] == '\r') {
				j++
			}
			if j < len(out) && (out[j] == ']' || out[j] == '}') {
				continue // drop trailing comma
			}
		}
		b2.WriteByte(ch)
	}
	return b2.String(), nil
}
