package shared

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// SameOrigin reports whether two URLs share scheme, host, and effective port.
func SameOrigin(a, b *url.URL) bool {
	return a != nil && b != nil &&
		strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectivePort(a) == effectivePort(b)
}

func effectivePort(value *url.URL) string {
	if port := value.Port(); port != "" {
		return port
	}
	switch strings.ToLower(value.Scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	default:
		return ""
	}
}

// SecureHTTPClient returns a copy of source whose CheckRedirect refuses
// redirects leaving the trusted origin, preserves source's CheckRedirect, and
// applies net/http's default 10-redirect cap when source has no redirect hook.
// newError builds the provider's refusal error for a rejected destination.
func SecureHTTPClient(source *http.Client, trusted *url.URL, newError func(dest *url.URL) error) *http.Client {
	client := *source
	originalCheckRedirect := source.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !SameOrigin(trusted, req.URL) {
			return newError(req.URL)
		}
		if originalCheckRedirect != nil {
			return originalCheckRedirect(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &client
}
