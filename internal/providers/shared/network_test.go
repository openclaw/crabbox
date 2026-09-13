package shared

import (
	"errors"
	"testing"
)

func TestEndpointAddressCanonicalization(t *testing.T) {
	for _, tt := range []struct{ input, want string }{
		{" https://EXAMPLE.com:443/api/// ", "https://example.com/api"},
		{"http://LOCALHOST:80/work/", "http://localhost/work"},
		{"http://[::1]:80/", "http://[::1]"},
		{"https://[2001:DB8::1]:443/api/", "https://[2001:db8::1]/api"},
		{"https://EXAMPLE.com:8443/a%2Fb/", "https://example.com:8443/a/b"},
	} {
		t.Run(tt.input, func(t *testing.T) {
			got, err := NormalizeHTTPSURL(tt.input, endpointTestErrors())
			if err != nil || got != tt.want {
				t.Fatalf("endpoint=(%q, %v), want %q", got, err, tt.want)
			}
			if got := NormalizedSandboxClaimEndpoint(tt.input); got != tt.want {
				t.Fatalf("claim endpoint=%q, want %q", got, tt.want)
			}
		})
	}
}

func TestEndpointAdmissionRemainsSeparateFromClaimIdentity(t *testing.T) {
	errs := endpointTestErrors()
	for _, tt := range []struct {
		input, claim string
		err          error
	}{
		{"https://alice:password@EXAMPLE.com:443/api/?key=value#section", "https://example.com/api", errs.Components},
		{"https://example.com/api?", "https://example.com/api", errs.Components},
		{"http://example.com:80/api/", "http://example.com/api", errs.Insecure},
		{"ftp://EXAMPLE.com:21/api/", "ftp://example.com:21/api", errs.Insecure},
		{"local-path///", "local-path", errs.Invalid},
	} {
		t.Run(tt.input, func(t *testing.T) {
			if _, err := NormalizeHTTPSURL(tt.input, errs); err != tt.err {
				t.Fatalf("admission error=%v, want %v", err, tt.err)
			}
			if got := NormalizedSandboxClaimEndpoint(tt.input); got != tt.claim {
				t.Fatalf("claim endpoint=%q, want %q", got, tt.claim)
			}
		})
	}
}

func endpointTestErrors() EndpointURLErrors {
	return EndpointURLErrors{Invalid: errors.New("invalid URL"), Components: errors.New("URL components"), Insecure: errors.New("insecure URL")}
}
