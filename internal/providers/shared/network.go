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
	return canonicalEndpointAddress(parsed), nil
}

// Callers own scheme admission and URL-component policy before canonicalizing
// the lowercased scheme's address. Claim keys and live endpoints differ there.
func canonicalEndpointAddress(parsed *url.URL) string {
	parsed.Host = CanonicalHostPort(parsed)
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	return strings.TrimRight(parsed.String(), "/")
}

// CanonicalHostPort formats an already-parsed endpoint authority. Callers own
// scheme validation and path policy; the complete hostname is lowercased.
func CanonicalHostPort(parsed *url.URL) string {
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if (parsed.Scheme == "https" && port == "443") || (parsed.Scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		return net.JoinHostPort(host, port)
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

// EndpointURLForError omits userinfo, query, and fragment components. It is an
// endpoint display formatter, not a general diagnostic secret scrubber.
func EndpointURLForError(raw string) string {
	parsed, err := url.Parse(raw)
	if err == nil {
		if parsed.Opaque != "" || parsed.Host == "" {
			return "<redacted>"
		}
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.ForceQuery = false
		parsed.Fragment = ""
		return parsed.String()
	}
	return "<redacted>"
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
