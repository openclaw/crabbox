package shared

import (
	"net/url"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

// NormalizedSandboxClaimEndpoint preserves the existing E2B-compatible claim key.
func NormalizedSandboxClaimEndpoint(raw string) string {
	endpoint := strings.TrimSpace(core.ClaimScopeURL(raw))
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return strings.TrimRight(endpoint, "/")
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	return canonicalEndpointAddress(parsed)
}
