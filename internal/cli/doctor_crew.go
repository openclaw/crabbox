package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// doctorCrewTimeout bounds the live Tailscale API call. Kept short so the
// doctor command stays responsive even when the user's tailnet is degraded.
const doctorCrewTimeout = 4 * time.Second

// doctorTailscaleACLClient is satisfied by anything that can return the raw
// policy document for the user's tailnet. The real implementation hits the
// Tailscale API; tests inject a stub so unit tests never reach the network.
type doctorTailscaleACLClient interface {
	PolicyHuJSON(ctx context.Context, tailnet string) (string, error)
}

// doctorTailscaleACLClientFactory is overridden in tests. It returns nil when
// the live client is not available so the check can degrade gracefully.
var doctorTailscaleACLClientFactory = newDoctorTailscaleACLClient

// doctorCrewSummary is the doctor entry point invoked from finish(). It
// always returns either ("","",nil) — meaning "no crew check applies" — or a
// triplet ready to feed into the existing record(...) helper. The check is
// intentionally bounded to a few seconds so doctor stays fast even on
// degraded tailnets.
func doctorCrewSummary(ctx context.Context, cfg Config) (string, string, map[string]string) {
	crew := normalizeCrewName(cfg.Crew)
	if crew == "" {
		return "", "", nil
	}
	tag := crewTailscaleTag(localCoordinatorOwner(), crew)
	if tag == "" {
		return "", "", nil
	}
	if !providerCapableOfTailscale(cfg.Provider) {
		return "skip", fmt.Sprintf("crew %q: provider %s does not support the Tailscale plane; crew networking unavailable", crew, cfg.Provider), map[string]string{"provider": cfg.Provider, "crew": crew, "tag": tag, "plane": "tailscale", "reason": "provider_not_tailscale_capable"}
	}
	apiKey := strings.TrimSpace(os.Getenv("TS_API_KEY"))
	if apiKey == "" {
		return "skip", "TS_API_KEY missing; skipped ACL verification", map[string]string{"crew": crew, "tag": tag, "reason": "ts_api_key_missing"}
	}
	tailnet := strings.TrimSpace(os.Getenv("TS_TAILNET"))
	if tailnet == "" {
		tailnet = "-"
	}
	client := doctorTailscaleACLClientFactory(apiKey)
	if client == nil {
		return "skip", "tailscale api client unavailable", map[string]string{"crew": crew, "tag": tag, "reason": "client_unavailable"}
	}
	checkCtx, cancel := context.WithTimeout(ctx, doctorCrewTimeout)
	defer cancel()
	body, err := client.PolicyHuJSON(checkCtx, tailnet)
	if err != nil {
		return "failed", fmt.Sprintf("tailscale policy lookup failed: %v", err), map[string]string{"crew": crew, "tag": tag, "tailnet": tailnet, "error": err.Error()}
	}
	if crewACLRowPresent(body, tag) {
		return "ok", fmt.Sprintf("crew %q: Tailscale plane auto-managed (%s)", crew, tag), map[string]string{"crew": crew, "tag": tag, "tailnet": tailnet, "mode": "auto-managed"}
	}
	return "failed", fmt.Sprintf("crew %q: tailnet policy row missing for %s. Run with $TS_API_KEY exported to auto-install, or apply the snippet from docs/features/crew.md", crew, tag), map[string]string{"crew": crew, "tag": tag, "tailnet": tailnet, "remedy": "see_docs_features_crew_md"}
}

// crewACLRowPresent checks for the concrete tag declaration and access row
// needed by a crew. The Tailscale policy file is HuJSON and not trivially
// JSON-parseable without an extra dependency, so keep the scan textual but
// exact enough: the tag must appear under tagOwners and either a legacy ACL row
// (`tag` -> `tag:*`) or a grants row (`tag` -> `tag`) must be present.
func crewACLRowPresent(policy, tag string) bool {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return false
	}
	quotedTag := `"` + tag + `"`
	quotedDst := `"` + tag + `:*"`
	if !policySectionContains(policy, "tagOwners", quotedTag) {
		return false
	}
	if policySectionContains(policy, "acls", quotedTag) &&
		policySectionContains(policy, "acls", quotedDst) {
		return true
	}
	if grants, ok := policySection(policy, "grants"); ok {
		return strings.Count(grants, quotedTag) >= 2 && strings.Contains(grants, `"ip"`)
	}
	return false
}

func policySectionContains(policy, section, token string) bool {
	body, ok := policySection(policy, section)
	return ok && strings.Contains(body, token)
}

func policySection(policy, section string) (string, bool) {
	start := activePolicySectionStart(policy, section)
	if start < 0 {
		return "", false
	}
	rest := policy[start+len(section)+2:]
	colon := strings.IndexByte(rest, ':')
	if colon < 0 {
		return "", false
	}
	rest = rest[colon+1:]
	trimmed := strings.TrimLeft(rest, " \t\r\n")
	if trimmed == "" {
		return "", false
	}
	open := trimmed[0]
	close := byte(0)
	switch open {
	case '{':
		close = '}'
	case '[':
		close = ']'
	default:
		return "", false
	}
	depth := 0
	inString := false
	escaped := false
	for i := 0; i < len(trimmed); i++ {
		ch := trimmed[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == '"' {
				inString = false
			}
			continue
		}
		if ch == '"' {
			inString = true
			continue
		}
		switch ch {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return trimmed[:i+1], true
			}
		}
	}
	return "", false
}

func activePolicySectionStart(policy, section string) int {
	offset := 0
	prefix := `"` + section + `"`
	for _, line := range strings.SplitAfter(policy, "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, prefix) {
			return offset + strings.Index(line, prefix)
		}
		offset += len(line)
	}
	return -1
}

// liveDoctorTailscaleACLClient is the production implementation. It targets
// the documented "Get tailnet policy file" endpoint and returns the response
// body verbatim so the substring scan above sees the user's HuJSON exactly
// as they wrote it.
type liveDoctorTailscaleACLClient struct {
	apiKey string
	http   *http.Client
}

func newDoctorTailscaleACLClient(apiKey string) doctorTailscaleACLClient {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil
	}
	return &liveDoctorTailscaleACLClient{apiKey: apiKey, http: &http.Client{Timeout: doctorCrewTimeout}}
}

func (c *liveDoctorTailscaleACLClient) PolicyHuJSON(ctx context.Context, tailnet string) (string, error) {
	if tailnet == "" {
		tailnet = "-"
	}
	url := "https://api.tailscale.com/api/v2/tailnet/" + tailnet + "/acl"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(c.apiKey, "")
	req.Header.Set("Accept", "application/hujson")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if readErr != nil {
		return "", readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Return body as error detail when the response is a JSON error
		// envelope so the caller surfaces actionable text.
		var envelope struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(body, &envelope) == nil && envelope.Message != "" {
			return "", fmt.Errorf("tailscale api %d: %s", resp.StatusCode, envelope.Message)
		}
		return "", fmt.Errorf("tailscale api %d", resp.StatusCode)
	}
	return string(body), nil
}
