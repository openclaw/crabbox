package cli

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
)

const cloneDefaultTransportError = "http.DefaultTransport must be a non-nil *http.Transport; inject an explicit HTTP client where supported"

// CloneDefaultTransport copies http.DefaultTransport when it is a non-nil
// *http.Transport. Callers that need to mutate transport settings must not
// bypass a process-wide custom RoundTripper.
func CloneDefaultTransport() (*http.Transport, error) {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok || transport == nil {
		return nil, errors.New(cloneDefaultTransportError)
	}
	return transport.Clone(), nil
}

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

// SameHTTPOrigin compares scheme, hostname, and effective port. Callers retain
// URL admission and any redirect restrictions beyond origin equality.
func SameHTTPOrigin(a, b *url.URL) bool {
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
