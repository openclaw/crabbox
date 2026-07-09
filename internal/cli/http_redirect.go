package cli

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// redirectCheckedHTTPClient clones source so callers can constrain redirects
// without mutating a shared client or discarding its transport and timeouts.
func redirectCheckedHTTPClient(source *http.Client, check func(*http.Request) error) *http.Client {
	if source == nil {
		source = http.DefaultClient
	}
	client := *source
	originalCheckRedirect := source.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := check(req); err != nil {
			return err
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

func sameHTTPOrigin(a, b *url.URL) bool {
	return a != nil && b != nil &&
		strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectiveHTTPPort(a) == effectiveHTTPPort(b)
}

func effectiveHTTPPort(value *url.URL) string {
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
