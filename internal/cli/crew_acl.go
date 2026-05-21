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

// crewACLEnsureTimeout bounds each request made by crewACLEnsure so the auth-
// key mint path stays responsive even when Tailscale is slow. The total time
// budget is roughly twice this (one GET + at most one PUT).
const crewACLEnsureTimeout = 6 * time.Second

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
// should talk to for crew ACL bootstrap. Trailing slashes are stripped so the
// callers can append paths without thinking about it.
func resolveTailnetAPIURL() string {
	for _, v := range []string{os.Getenv(crabboxTailnetAPIURLEnvVar), os.Getenv(tailnetAPIURLEnvVar)} {
		if v = strings.TrimRight(strings.TrimSpace(v), "/"); v != "" {
			return v
		}
	}
	return defaultTailnetAPIURL
}

// ErrCrewACLAutoBootstrapUnavailable is returned when the configured tailnet
// control plane (e.g. Headscale) does not expose a Tailscale-compatible
// policy REST API. Callers fall back to the manual snippet in
// docs/features/crew.md instead of failing the lease creation.
var ErrCrewACLAutoBootstrapUnavailable = errors.New("crew acl: auto-bootstrap unavailable on this control plane")

// crewACLMaxAttempts caps the GET → merge → PUT loop. The first 412 from PUT
// almost always reflects a benign concurrent edit (another operator running
// the same bootstrap path), so re-reading the policy and retrying once is
// safe. Two attempts is the smallest value that lets us tolerate a single
// race; persistent 412s are surfaced as a clear error.
const crewACLMaxAttempts = 2

// errCrewACLPreconditionFailed is returned by PutPolicy when the server
// rejected the write with HTTP 412 (ETag mismatch). It is wrapped so callers
// can detect the race via errors.Is and decide whether to retry.
var errCrewACLPreconditionFailed = errors.New("crew acl: tailnet policy changed during update (ETag mismatch)")

// crewTailnetACLClient is satisfied by anything that can read and update the
// tailnet policy file. The real implementation hits the Tailscale API; tests
// inject a stub so unit tests never reach the network.
type crewTailnetACLClient interface {
	GetPolicy(ctx context.Context, tailnet string) (body string, etag string, err error)
	PutPolicy(ctx context.Context, tailnet string, body string, etag string) error
}

// crewTailnetACLClientFactory is overridden in tests. It returns nil when no
// API key is available so callers can fall through to the manual-setup path.
var crewTailnetACLClientFactory = newCrewTailnetACLClient

// crewACLEnsure makes sure the concrete crew tag is declared in tagOwners and
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
func crewACLEnsure(ctx context.Context, client crewTailnetACLClient, tailnet, owner, crew string) error {
	if client == nil {
		return fmt.Errorf("crew acl client unavailable")
	}
	tag := crewTailscaleTag(owner, crew)
	if tag == "" {
		return fmt.Errorf("crew acl: empty tag (owner=%q crew=%q)", owner, crew)
	}
	if tailnet == "" {
		tailnet = "-"
	}
	var lastPutErr error
	for attempt := 1; attempt <= crewACLMaxAttempts; attempt++ {
		getCtx, cancelGet := context.WithTimeout(ctx, crewACLEnsureTimeout)
		body, etag, err := client.GetPolicy(getCtx, tailnet)
		cancelGet()
		if err != nil {
			return fmt.Errorf("crew acl: read policy: %w", err)
		}
		if crewACLRowPresent(body, tag) {
			return nil
		}
		merged, err := crewACLMergePolicy(body, tag)
		if err != nil {
			return err
		}
		putCtx, cancelPut := context.WithTimeout(ctx, crewACLEnsureTimeout)
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
		if errors.Is(err, errCrewACLPreconditionFailed) {
			lastPutErr = err
			if attempt < crewACLMaxAttempts {
				continue
			}
			return fmt.Errorf("crew acl: ETag race persisted after %d attempts: %w", crewACLMaxAttempts, lastPutErr)
		}
		return fmt.Errorf("crew acl: update policy: %w", err)
	}
	// Defensive: the loop body always returns or continues; reaching here
	// would indicate a bug in the loop bounds.
	return fmt.Errorf("crew acl: ETag race persisted after %d attempts: %w", crewACLMaxAttempts, lastPutErr)
}

// crewACLMergePolicy parses the policy as JSON, ensures both the tagOwners
// entry and a self-peering grant for the tag, and returns the re-serialized
// document. Returns a clear error when the policy is not valid JSON (e.g.
// contains HuJSON comments) so the caller falls back to manual setup.
func crewACLMergePolicy(body, tag string) (string, error) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return "", fmt.Errorf("crew acl: empty policy body")
	}
	var policy map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &policy); err != nil {
		return "", fmt.Errorf("crew acl: cannot merge non-JSON policy (add the tag:cbx-crew-... rows manually): %w", err)
	}
	if policy == nil {
		policy = map[string]json.RawMessage{}
	}

	// Merge tagOwners.
	tagOwners := map[string]json.RawMessage{}
	if raw, ok := policy["tagOwners"]; ok && len(raw) > 0 {
		if err := json.Unmarshal(raw, &tagOwners); err != nil {
			return "", fmt.Errorf("crew acl: cannot parse tagOwners: %w", err)
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
			return "", fmt.Errorf("crew acl: cannot parse grants: %w", err)
		}
		grants = append(grants, crewGrantEntry(tag))
		updatedGrants, err := json.Marshal(grants)
		if err != nil {
			return "", err
		}
		policy["grants"] = updatedGrants
	} else {
		var acls []map[string]json.RawMessage
		if raw, ok := policy["acls"]; ok && len(raw) > 0 {
			if err := json.Unmarshal(raw, &acls); err != nil {
				return "", fmt.Errorf("crew acl: cannot parse acls: %w", err)
			}
		}
		acls = append(acls, crewACLEntry(tag))
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

func crewGrantEntry(tag string) map[string]json.RawMessage {
	src, _ := json.Marshal([]string{tag})
	dst, _ := json.Marshal([]string{tag})
	ip, _ := json.Marshal([]string{"*"})
	return map[string]json.RawMessage{
		"src": src,
		"dst": dst,
		"ip":  ip,
	}
}

func crewACLEntry(tag string) map[string]json.RawMessage {
	action, _ := json.Marshal("accept")
	src, _ := json.Marshal([]string{tag})
	dst, _ := json.Marshal([]string{tag + ":*"})
	return map[string]json.RawMessage{
		"action": action,
		"src":    src,
		"dst":    dst,
	}
}

// liveCrewTailnetACLClient is the production implementation. It targets the
// documented "Get tailnet policy file" and "Set tailnet policy file"
// endpoints and threads ETag through so concurrent edits fail-fast.
type liveCrewTailnetACLClient struct {
	apiKey string
	http   *http.Client
}

func newCrewTailnetACLClient(apiKey string) crewTailnetACLClient {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil
	}
	return &liveCrewTailnetACLClient{apiKey: apiKey, http: &http.Client{Timeout: crewACLEnsureTimeout}}
}

func (c *liveCrewTailnetACLClient) GetPolicy(ctx context.Context, tailnet string) (string, string, error) {
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
		return "", "", fmt.Errorf("%w: GET %s returned %d", ErrCrewACLAutoBootstrapUnavailable, url, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", tailscaleAPIError(resp.StatusCode, body)
	}
	// Missing ETag on a 2xx response means concurrent-edit safety via
	// If-Match isn't possible — we never PUT without a CAS token, so treat
	// this as "auto-bootstrap unavailable" and let the manual path handle it.
	etag := resp.Header.Get("ETag")
	if etag == "" {
		return "", "", fmt.Errorf("%w: GET %s returned no ETag header", ErrCrewACLAutoBootstrapUnavailable, url)
	}
	return string(body), etag, nil
}

func (c *liveCrewTailnetACLClient) PutPolicy(ctx context.Context, tailnet, body, etag string) error {
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
		return errCrewACLPreconditionFailed
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
