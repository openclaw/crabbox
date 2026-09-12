package shared

import (
	"net"
	"net/url"
	"strings"
)

type EndpointURLErrors struct {
	Invalid    error
	Components error
	Insecure   error
}

// LowercaseHostname preserves an IPv6 zone identifier's case. It does not
// normalize IP spelling, strip a trailing dot, or trim whitespace.
func LowercaseHostname(host string) string {
	if zoneAt := strings.Index(host, "%"); zoneAt > 0 && strings.Contains(host[:zoneAt], ":") {
		return strings.ToLower(host[:zoneAt]) + host[zoneAt:]
	}
	return strings.ToLower(host)
}

func NormalizeHTTPSURL(raw string, errs EndpointURLErrors) (string, error) {
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
	return strings.TrimRight(parsed.String(), "/"), nil
}

func IsLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func IsLoopbackHTTPURL(parsed *url.URL) bool {
	if parsed.Scheme != "http" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
