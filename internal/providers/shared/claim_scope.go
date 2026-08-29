package shared

import (
	"net"
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
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if (parsed.Scheme == "https" && port == "443") || (parsed.Scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		parsed.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		parsed.Host = "[" + host + "]"
	} else {
		parsed.Host = host
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	return strings.TrimRight(parsed.String(), "/")
}
