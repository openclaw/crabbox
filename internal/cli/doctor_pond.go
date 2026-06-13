package cli

import (
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

// doctorPondTimeout bounds the live Tailscale API call. Kept short so the
// doctor command stays responsive even when the user's tailnet is degraded.
const doctorPondTimeout = 4 * time.Second

// doctorTailscaleACLClient is satisfied by anything that can return the raw
// policy document for the user's tailnet. The real implementation hits the
// Tailscale API; tests inject a stub so unit tests never reach the network.
type doctorTailscaleACLClient interface {
	PolicyHuJSON(ctx context.Context, tailnet string) (string, error)
}

// doctorTailscaleACLClientFactory is overridden in tests. It returns nil when
// the live client is not available so the check can degrade gracefully.
var doctorTailscaleACLClientFactory = newDoctorTailscaleACLClient

// doctorPondSummary is the doctor entry point invoked from finish(). It
// always returns either ("","",nil) — meaning "no pond check applies" — or a
// triplet ready to feed into the existing record(...) helper. The check is
// intentionally bounded to a few seconds so doctor stays fast even on
// degraded tailnets.
func doctorPondSummary(ctx context.Context, cfg Config) (string, string, map[string]string) {
	pond := normalizePondName(cfg.Pond)
	if pond == "" {
		return "", "", nil
	}
	tag := pondTailscaleTag(localCoordinatorOwner(), pond)
	if tag == "" {
		return "", "", nil
	}
	hasClaims, hasTailscaleProvider := pondClaimProviderSummary(pond)
	if !hasClaims {
		return "skip", fmt.Sprintf("pond %q: no local lease claims found; create or claim a pond member before checking Tailscale policy", pond), map[string]string{"pond": pond, "tag": tag, "plane": "tailscale", "reason": "no_pond_claims"}
	}
	if !hasTailscaleProvider {
		return "skip", fmt.Sprintf("pond %q: no Tailscale-capable provider found; pond networking unavailable", pond), map[string]string{"provider": cfg.Provider, "pond": pond, "tag": tag, "plane": "tailscale", "reason": "no_tailscale_capable_provider"}
	}
	apiKey := strings.TrimSpace(os.Getenv("TS_API_KEY"))
	if apiKey == "" {
		return "skip", "TS_API_KEY missing; skipped ACL verification", map[string]string{"pond": pond, "tag": tag, "reason": "ts_api_key_missing"}
	}
	tailnet := strings.TrimSpace(os.Getenv("TS_TAILNET"))
	if tailnet == "" {
		tailnet = "-"
	}
	client := doctorTailscaleACLClientFactory(apiKey)
	if client == nil {
		return "skip", "tailscale api client unavailable", map[string]string{"pond": pond, "tag": tag, "reason": "client_unavailable"}
	}
	checkCtx, cancel := context.WithTimeout(ctx, doctorPondTimeout)
	defer cancel()
	body, err := client.PolicyHuJSON(checkCtx, tailnet)
	if err != nil {
		// Self-hosted control planes (e.g. Headscale) do not expose the
		// Tailscale `/api/v2/tailnet/.../acl` route. Skip with a helpful
		// message pointing operators at the manual snippet rather than
		// failing — the network plane still works against the client-side
		// Tailscale binary regardless of which control plane runs it.
		if errors.Is(err, ErrPondACLAutoBootstrapUnavailable) {
			return "skip", fmt.Sprintf("pond %q: control plane at %s does not expose a Tailscale-compatible policy API; apply the snippet from docs/features/pond.md (e.g. Headscale: `headscale policy set --file ./policy.hujson`)", pond, resolveTailnetAPIURL()), map[string]string{"pond": pond, "tag": tag, "tailnet": tailnet, "api_url": resolveTailnetAPIURL(), "reason": "control_plane_incompatible"}
		}
		return "failed", fmt.Sprintf("tailscale policy lookup failed: %v", err), map[string]string{"pond": pond, "tag": tag, "tailnet": tailnet, "error": err.Error()}
	}
	if pondACLRowPresent(body, tag) {
		return "ok", fmt.Sprintf("pond %q: Tailscale plane auto-managed (%s)", pond, tag), map[string]string{"pond": pond, "tag": tag, "tailnet": tailnet, "mode": "auto-managed"}
	}
	return "failed", fmt.Sprintf("pond %q: tailnet policy row missing for %s. Set TS_API_KEY plus %s=1 to auto-install, or apply the snippet from docs/features/pond.md", pond, tag, pondACLAutoBootstrapEnvVar), map[string]string{"pond": pond, "tag": tag, "tailnet": tailnet, "remedy": "see_docs_features_pond_md"}
}

// liveDoctorTailscaleACLClient is the production implementation. It targets
// the documented "Get tailnet policy file" endpoint and returns the response
// body verbatim so the shared HuJSON policy parser sees the operator's policy
// exactly as written.
type liveDoctorTailscaleACLClient struct {
	apiKey string
	http   *http.Client
}

func newDoctorTailscaleACLClient(apiKey string) doctorTailscaleACLClient {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil
	}
	return &liveDoctorTailscaleACLClient{apiKey: apiKey, http: &http.Client{Timeout: doctorPondTimeout}}
}

func (c *liveDoctorTailscaleACLClient) PolicyHuJSON(ctx context.Context, tailnet string) (string, error) {
	if tailnet == "" {
		tailnet = "-"
	}
	url := resolveTailnetAPIURL() + "/api/v2/tailnet/" + tailnet + "/acl"
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
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNotImplemented {
		return "", fmt.Errorf("%w: GET %s returned %d", ErrPondACLAutoBootstrapUnavailable, url, resp.StatusCode)
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
