package shared

import (
	"errors"
	"net/http"
	"net/url"

	core "github.com/openclaw/crabbox/internal/cli"
)

// SecureHTTPClient returns a copy of source whose CheckRedirect refuses
// redirects leaving the trusted origin, preserves source's CheckRedirect, and
// applies net/http's default 10-redirect cap when source has no redirect hook.
// newError builds the provider's refusal error for a rejected destination.
func SecureHTTPClient(source *http.Client, trusted *url.URL, newError func(dest *url.URL) error) *http.Client {
	client := *source
	originalCheckRedirect := source.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !core.SameHTTPOrigin(trusted, req.URL) {
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
