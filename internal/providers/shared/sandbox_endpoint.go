package shared

import (
	"net"
	"net/url"
	"strings"
)

// NormalizeSandboxAPIURL admits HTTPS or loopback HTTP, preserving IPv6 zone
// identity. The adapter strips its API suffix from the decoded, trimmed path.
func NormalizeSandboxAPIURL(raw string, errs EndpointURLErrors, path func(string) string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.Opaque != "" {
		return "", errs.Invalid
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", errs.Components
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && IsLoopbackHost(parsed.Hostname())) {
		return "", errs.Insecure
	}
	host := LowercaseHostname(parsed.Hostname())
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
	parsed.Path = path(strings.TrimRight(parsed.Path, "/"))
	parsed.RawPath = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}
