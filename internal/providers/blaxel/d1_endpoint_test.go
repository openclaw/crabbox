package blaxel

import (
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestD1EndpointCharacterization(t *testing.T) {
	for _, tc := range []struct{ raw, want, message string }{
		{" HTTPS://EXAMPLE.COM:443/base/v1/v0/// ", "https://example.com/base", ""},
		{"https://[FE80::AB%25EthZ]:443/base/v1/v0", "https://[fe80::ab%25EthZ]/base", ""},
		{"https://[FE80::AB%25EthZ]:8443/base/v1/v0", "https://[fe80::ab%25EthZ]:8443/base", ""},
		{"https://EXAMPLE.COM/a%2Fb/v1/v0", "https://example.com/a/b", ""},
		{"http://LOCALHOST:80/", "http://localhost", ""},
		{"http://127.12.3.4:8080/", "http://127.12.3.4:8080", ""},
		{"http://[::1]:80/", "http://[::1]", ""},
		{"https://EXAMPLE.COM/base/v1/v0/child", "https://example.com/base/v1/v0/child", ""},
		{"https://example.com/v0/v1", "https://example.com/v0", ""},
		{"relative", "", "must be an absolute HTTP(S) URL"},
		{"https://example.com:bad", "", "must be an absolute HTTP(S) URL"},
		{"https://example.com/%zz", "", "must be an absolute HTTP(S) URL"},
		{"https:opaque", "", "must be an absolute HTTP(S) URL"},
		{"https://example.com?", "", "must not contain userinfo, query parameters, or a fragment"},
		{"https://u:p@example.com", "", "must not contain userinfo, query parameters, or a fragment"},
		{"https://example.com/#x", "", "must not contain userinfo, query parameters, or a fragment"},
		{"http://example.com", "", "must use HTTPS except for loopback development endpoints"},
		{"http://localhost.", "", "must use HTTPS except for loopback development endpoints"},
		{"http://[::1%25EthZ]", "", "must use HTTPS except for loopback development endpoints"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := ValidateAPIURL(tc.raw)
			if got != tc.want {
				t.Fatalf("got=%q want=%q err=%v", got, tc.want, err)
			}
			if tc.message == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || err.Error() != "provider=blaxel API URL "+tc.message || core.ExitCodeForError(err, 1) != 2 {
				t.Fatalf("err=%v want=%q", err, tc.message)
			}
		})
	}
}
