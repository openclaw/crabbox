package proxmox

import (
	"net/url"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (Provider) ClaimScope(cfg core.Config) string {
	endpoint := normalizedProxmoxClaimEndpoint(cfg.Proxmox.APIURL)
	node := strings.TrimSpace(cfg.Proxmox.Node)
	if endpoint != "" && node != "" {
		return "endpoint:" + endpoint + "|node:" + node
	}
	return ""
}

func normalizedProxmoxClaimEndpoint(raw string) string {
	endpoint := strings.TrimSpace(raw)
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" {
		endpoint = strings.TrimRight(endpoint, "/")
		endpoint = strings.TrimSuffix(endpoint, "/api2/json")
		return endpoint
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.Path = strings.TrimSuffix(parsed.Path, "/api2/json")
	parsed.RawPath = ""
	return parsed.String()
}
