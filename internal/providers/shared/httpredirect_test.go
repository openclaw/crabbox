package shared

import (
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"reflect"
	"testing"
	"time"
)

func TestControlAndDataHTTPClients(t *testing.T) {
	control, data := ControlAndDataHTTPClients(nil, 23*time.Second)
	if control == nil || data == nil || control == data || control.Timeout != 23*time.Second || data.Timeout != 0 {
		t.Fatalf("default clients control=%+v data=%+v", control, data)
	}
	control.Timeout = time.Second
	if data.Timeout != 0 {
		t.Fatal("default control and data settings are coupled")
	}

	transport := &http.Transport{DisableKeepAlives: true}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	redirectErr := errors.New("caller redirect policy")
	redirectCalls := 0
	redirect := func(*http.Request, []*http.Request) error { redirectCalls++; return redirectErr }
	injected := &http.Client{Transport: transport, Jar: jar, Timeout: 17 * time.Second, CheckRedirect: redirect}
	control, data = ControlAndDataHTTPClients(injected, time.Second)
	if control != injected || data != injected {
		t.Fatal("injected client identity changed")
	}
	if injected.Transport != transport || injected.Jar != jar || injected.Timeout != 17*time.Second || reflect.ValueOf(injected.CheckRedirect).Pointer() != reflect.ValueOf(redirect).Pointer() {
		t.Fatal("constructor mutated the injected client")
	}
	for _, client := range []*http.Client{control, data, injected} {
		if !errors.Is(client.CheckRedirect(nil, nil), redirectErr) {
			t.Fatal("caller redirect policy changed")
		}
	}
	if redirectCalls != 3 {
		t.Fatalf("redirect calls=%d", redirectCalls)
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
