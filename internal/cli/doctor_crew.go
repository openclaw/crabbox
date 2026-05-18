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
	if !providerCapableOfTailscale(cfg.Provider) {
		return "skip", fmt.Sprintf("provider=%s does not support the Tailscale plane; crew network is unavailable", cfg.Provider), map[string]string{"provider": cfg.Provider, "plane": "tailscale", "reason": "provider_not_tailscale_capable"}
	}
	owner := crewTagOwner(localCoordinatorOwner())
	if owner == "" {
		owner = "user"
	}
	tag := crewTailscaleTagPrefix + owner + "-*"
	apiKey := strings.TrimSpace(os.Getenv("TS_API_KEY"))
	if apiKey == "" {
		return "skip", "TS_API_KEY missing; skipped ACL verification", map[string]string{"tag": tag, "reason": "ts_api_key_missing"}
	}
	tailnet := strings.TrimSpace(os.Getenv("TS_TAILNET"))
	if tailnet == "" {
		tailnet = "-"
	}
	client := doctorTailscaleACLClientFactory(apiKey)
	if client == nil {
		return "skip", "tailscale api client unavailable", map[string]string{"tag": tag, "reason": "client_unavailable"}
	}
	checkCtx, cancel := context.WithTimeout(ctx, doctorCrewTimeout)
	defer cancel()
	body, err := client.PolicyHuJSON(checkCtx, tailnet)
	if err != nil {
		return "failed", fmt.Sprintf("tailscale policy lookup failed: %v", err), map[string]string{"tag": tag, "tailnet": tailnet, "error": err.Error()}
	}
	if crewACLRowPresent(body) {
		return "ok", fmt.Sprintf("acl row present for %s", tag), map[string]string{"tag": tag, "tailnet": tailnet}
	}
	return "failed", fmt.Sprintf("acl row missing for %s; add the one-time setup snippet from docs/features/crew.md", tag), map[string]string{"tag": tag, "tailnet": tailnet, "remedy": "see_docs_features_crew_md"}
}

// crewACLRowPresent does a deliberately lenient text scan for the two
// tag:cbx-crew-* mentions an operator-correct policy must contain (one in
// `acls`, one in `tagOwners`). The Tailscale policy file is HuJSON and not
// trivially JSON-parseable; a substring check is good enough to flag the
// common misconfiguration without dragging in a HuJSON dependency.
func crewACLRowPresent(policy string) bool {
	if !strings.Contains(policy, crewTailscaleTagPrefix) {
		return false
	}
	return strings.Contains(policy, "tagOwners") && strings.Contains(policy, "acls")
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
