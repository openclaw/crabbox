package shared

import (
	"net/url"
	"testing"
)

func TestCanonicalHostPort(t *testing.T) {
	for _, tt := range []struct{ scheme, host, want string }{
		{"https", "EXAMPLE.com:443", "example.com"},
		{"http", "EXAMPLE.com:80", "example.com"},
		{"https", "EXAMPLE.com:8443", "example.com:8443"},
		{"https", "[2001:DB8::1]:443", "[2001:db8::1]"},
		{"https", "[2001:DB8::1]:8443", "[2001:db8::1]:8443"},
		{"custom", "EXAMPLE.com:443", "example.com:443"},
		{"HTTPS", "EXAMPLE.com:443", "example.com:443"},
	} {
		parsed := &url.URL{Scheme: tt.scheme, Host: tt.host, Path: "/keep/"}
		original := *parsed
		if got := CanonicalHostPort(parsed); got != tt.want || *parsed != original {
			t.Errorf("authority(%q,%q)=%q, want %q with unchanged URL", tt.scheme, tt.host, got, tt.want)
		}
	}
}

func TestEndpointURLForError(t *testing.T) {
	for _, tt := range []struct{ input, want string }{
		{"https://alice@example.com:8443/api?q=value#fragment", "https://example.com:8443/api"},
		{"https://EXAMPLE.com/api?", "https://EXAMPLE.com/api"},
		{"https://example.com/a%2Fb", "https://example.com/a%2Fb"},
		{"mailto:alice@example.com", "<redacted>"},
		{"relative/path", "<redacted>"},
		{"https://example.com/%zz", "<redacted>"},
		{"", "<redacted>"},
	} {
		if got := EndpointURLForError(tt.input); got != tt.want {
			t.Errorf("display(%q)=%q, want %q", tt.input, got, tt.want)
		}
	}
}
