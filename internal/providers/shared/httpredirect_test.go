package shared

import (
	"errors"
	"net/http"
	"net/url"
	"testing"
)

func TestSameOrigin(t *testing.T) {
	t.Parallel()

	base := mustParseURL(t, "https://Example.COM/api")
	tests := []struct {
		name      string
		candidate string
		want      bool
	}{
		{name: "identical", candidate: "https://example.com/api", want: true},
		{name: "default port", candidate: "HTTPS://EXAMPLE.COM:443/other", want: true},
		{name: "other scheme", candidate: "http://example.com/api", want: false},
		{name: "other host", candidate: "https://other.example.com/api", want: false},
		{name: "other port", candidate: "https://example.com:8443/api", want: false},
		{name: "userinfo ignored", candidate: "https://alice@example.com/api", want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := mustParseURL(t, test.candidate)
			if got := SameOrigin(base, candidate); got != test.want {
				t.Fatalf("SameOrigin(%q, %q) = %v, want %v", base, candidate, got, test.want)
			}
		})
	}
	if SameOrigin(nil, base) || SameOrigin(base, nil) {
		t.Fatal("SameOrigin accepted a nil URL")
	}
}

func TestSecureHTTPClient(t *testing.T) {
	t.Parallel()

	trusted := mustParseURL(t, "https://api.example.com")
	refusal := errors.New("provider redirect refusal")
	source := &http.Client{Timeout: 42, Transport: http.DefaultTransport}
	secured := SecureHTTPClient(source, trusted, func(*url.URL) error { return refusal })
	if secured == source {
		t.Fatal("SecureHTTPClient returned the source client")
	}
	if secured.Timeout != source.Timeout || secured.Transport != source.Transport {
		t.Fatal("SecureHTTPClient did not preserve source settings")
	}
	if source.CheckRedirect != nil {
		t.Fatal("SecureHTTPClient mutated the source client")
	}

	sameOrigin := &http.Request{URL: mustParseURL(t, "https://API.EXAMPLE.COM:443/next")}
	if err := secured.CheckRedirect(sameOrigin, make([]*http.Request, 9)); err != nil {
		t.Fatalf("same-origin redirect before limit: %v", err)
	}
	if err := secured.CheckRedirect(sameOrigin, make([]*http.Request, 10)); err == nil || err.Error() != "stopped after 10 redirects" {
		t.Fatalf("default redirect limit error = %v", err)
	}

	crossOrigin := &http.Request{URL: mustParseURL(t, "https://other.example.com/next")}
	if err := secured.CheckRedirect(crossOrigin, nil); !errors.Is(err, refusal) {
		t.Fatalf("cross-origin redirect error = %v, want refusal", err)
	}
}

func TestSecureHTTPClientPreservesOriginalCheckRedirect(t *testing.T) {
	t.Parallel()

	trusted := mustParseURL(t, "https://api.example.com")
	originalError := errors.New("original redirect policy")
	called := false
	source := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		called = true
		return originalError
	}}
	secured := SecureHTTPClient(source, trusted, func(*url.URL) error {
		t.Fatal("same-origin redirect called refusal builder")
		return nil
	})
	req := &http.Request{URL: mustParseURL(t, "https://api.example.com/next")}
	if err := secured.CheckRedirect(req, make([]*http.Request, 10)); !errors.Is(err, originalError) {
		t.Fatalf("redirect error = %v, want original policy", err)
	}
	if !called {
		t.Fatal("original redirect policy was not called")
	}
}

func TestSecureHTTPClientRejectsNilTrustedOrigin(t *testing.T) {
	t.Parallel()

	refusal := errors.New("provider redirect refusal")
	secured := SecureHTTPClient(http.DefaultClient, nil, func(*url.URL) error { return refusal })
	req := &http.Request{URL: mustParseURL(t, "https://api.example.com/next")}
	if err := secured.CheckRedirect(req, nil); !errors.Is(err, refusal) {
		t.Fatalf("redirect error = %v, want refusal", err)
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", raw, err)
	}
	return parsed
}
