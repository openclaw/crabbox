package namespaceinstance

import (
	"net/url"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (Provider) ClaimScope(cfg core.Config) string {
	parts := make([]string, 0, 3)
	if endpoint := normalizedNamespaceClaimEndpoint(cfg.NamespaceInstance.Endpoint); endpoint != "" {
		parts = append(parts, "endpoint:"+endpoint)
	}
	if region := strings.TrimSpace(cfg.NamespaceInstance.Region); region != "" {
		parts = append(parts, "region:"+region)
	}
	if keychain := strings.TrimSpace(cfg.NamespaceInstance.Keychain); keychain != "" {
		parts = append(parts, "keychain:"+keychain)
	}
	return strings.Join(parts, "|")
}

func normalizedNamespaceClaimEndpoint(raw string) string {
	endpoint := strings.TrimSpace(core.ClaimScopeURL(raw))
	addedScheme := false
	parseValue := endpoint
	if endpoint != "" && !strings.Contains(endpoint, "://") {
		parseValue = "https://" + endpoint
		addedScheme = true
	}
	parsed, err := url.Parse(parseValue)
	if err != nil || parsed.Host == "" {
		return strings.TrimRight(endpoint, "/")
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	out := parsed.String()
	if addedScheme {
		out = strings.TrimPrefix(out, "https://")
	}
	return out
}
