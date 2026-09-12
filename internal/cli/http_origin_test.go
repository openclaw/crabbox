package cli

import (
	"net/url"
	"testing"
)

func TestSameHTTPOrigin(t *testing.T) {
	t.Parallel()

	base := parseOriginTestURL(t, "https://Example.COM/api")
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
			candidate := parseOriginTestURL(t, test.candidate)
			if got := SameHTTPOrigin(base, candidate); got != test.want {
				t.Fatalf("SameHTTPOrigin(%q, %q) = %v, want %v", base, candidate, got, test.want)
			}
		})
	}
	if SameHTTPOrigin(nil, base) || SameHTTPOrigin(base, nil) || SameHTTPOrigin(nil, nil) {
		t.Fatal("SameHTTPOrigin accepted a nil URL")
	}
}

func TestSameHTTPOriginEdgeCases(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		a, b string
		want bool
	}{
		{name: "http default port", a: "http://example.com", b: "HTTP://EXAMPLE.COM:80", want: true},
		{name: "empty URLs", want: true},
		{name: "unknown scheme", a: "custom://example.com", b: "CUSTOM://EXAMPLE.COM", want: true},
		{name: "unknown scheme explicit port", a: "custom://example.com", b: "custom://example.com:443"},
		{name: "zero padded port", a: "https://example.com:0443", b: "https://example.com:443"},
		{name: "IPv6 textual difference", a: "https://[2001:db8::1]", b: "https://[2001:0db8::1]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			a, b := parseOriginTestURL(t, test.a), parseOriginTestURL(t, test.b)
			if got := SameHTTPOrigin(a, b); got != test.want {
				t.Fatalf("SameHTTPOrigin(%q, %q) = %v, want %v", a, b, got, test.want)
			}
		})
	}
}

func parseOriginTestURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
