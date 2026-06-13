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

// pondACLMergePolicy parses the policy as HuJSON, ensures both the tagOwners
// entry and a self-peering grant for the tag, and returns a minimally patched
// document. Existing comments, ordering, and unrelated sections are preserved.
func pondACLMergePolicy(body, tag string) (string, error) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return "", fmt.Errorf("pond acl: empty policy body")
	}
	if pondACLRowPresent(trimmed, tag) {
		return trimmed, nil
	}
	value, err := hujson.Parse([]byte(trimmed))
	if err != nil {
		return "", fmt.Errorf("pond acl: cannot merge non-HuJSON policy (add the tag:cbx-pond-... rows manually): %w", err)
	}
	standardized := value.Clone()
	standardized.Standardize()
	var policy map[string]json.RawMessage
	if err := json.Unmarshal(standardized.Pack(), &policy); err != nil {
		return "", fmt.Errorf("pond acl: cannot merge non-JSON policy (add the tag:cbx-pond-... rows manually): %w", err)
	}
	if policy == nil {
		policy = map[string]json.RawMessage{}
	}

	var patch []map[string]any

	tagOwners := map[string]json.RawMessage{}
	if raw, ok := policy["tagOwners"]; ok && len(raw) > 0 {
		if err := json.Unmarshal(raw, &tagOwners); err != nil {
			return "", fmt.Errorf("pond acl: cannot parse tagOwners: %w", err)
		}
	}
	if _, ok := tagOwners[tag]; !ok {
		if _, ok := policy["tagOwners"]; ok {
			patch = append(patch, map[string]any{
				"op":    "add",
				"path":  "/tagOwners/" + jsonPointerEscape(tag),
				"value": []string{"autogroup:admin"},
			})
		} else {
			patch = append(patch, map[string]any{
				"op":    "add",
				"path":  "/tagOwners",
				"value": map[string][]string{tag: []string{"autogroup:admin"}},
			})
		}
	}

	if raw, ok := policy["grants"]; ok && len(raw) > 0 {
		var grants []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &grants); err != nil {
			return "", fmt.Errorf("pond acl: cannot parse grants: %w", err)
		}
		patch = append(patch, map[string]any{
			"op":    "add",
			"path":  "/grants/-",
			"value": pondGrantEntryValue(tag),
		})
	} else {
		var acls []map[string]json.RawMessage
		if raw, ok := policy["acls"]; ok && len(raw) > 0 {
			if err := json.Unmarshal(raw, &acls); err != nil {
				return "", fmt.Errorf("pond acl: cannot parse acls: %w", err)
			}
		}
		if _, ok := policy["acls"]; ok {
			patch = append(patch, map[string]any{
				"op":    "add",
				"path":  "/acls/-",
				"value": pondACLEntryValue(tag),
			})
		} else {
			patch = append(patch, map[string]any{
				"op":    "add",
				"path":  "/acls",
				"value": []map[string]any{pondACLEntryValue(tag)},
			})
		}
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return "", err
	}
	if err := value.Patch(patchBytes); err != nil {
		return "", fmt.Errorf("pond acl: patch policy: %w", err)
	}
	return string(value.Pack()), nil
}

func jsonPointerEscape(value string) string {
	value = strings.ReplaceAll(value, "~", "~0")
	value = strings.ReplaceAll(value, "/", "~1")
	return value
}

func pondGrantEntryValue(tag string) map[string]any {
	return map[string]any{
		"src": []string{tag},
		"dst": []string{tag},
		"ip":  []string{"*"},
	}
}

func pondACLEntryValue(tag string) map[string]any {
	return map[string]any{
		"action": "accept",
		"src":    []string{tag},
		"dst":    []string{tag + ":*"},
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
// trailing commas) so the result parses as standard JSON.
func hujsonStandardize(body string) (string, error) {
	out, err := hujson.Standardize([]byte(body))
	return string(out), err
}
