package opensandbox

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	sdk "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
)

func TestRunCommandStreamRedactsOpaqueAPIKeyInErrorBody(t *testing.T) {
	const lifecycleSecret = "opaque-opensandbox-lifecycle-secret"
	const headerSecret = "opaque-opensandbox-header-secret"
	const querySecret = "opaque-opensandbox-query+secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-EXECD-ACCESS-TOKEN") != headerSecret || r.URL.Query().Get("signature") != querySecret {
			t.Errorf("request missing endpoint credentials: headers=%v query=%v", r.Header, r.URL.Query())
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("provider rejected lifecycle=" + lifecycleSecret + " header=" + headerSecret + " query=" + querySecret + " region=eu"))
	}))
	defer server.Close()

	client := &sdkOpenSandboxClient{key: lifecycleSecret, client: server.Client()}
	err := client.runCommandStream(context.Background(), execdConnection{
		baseURL:  server.URL,
		rawQuery: "signature=" + url.QueryEscape(querySecret),
		headers:  map[string]string{"X-EXECD-ACCESS-TOKEN": headerSecret},
	}, sdk.RunCommandRequest{}, func(commandStreamEvent) error {
		return nil
	})
	if err == nil {
		t.Fatal("runCommandStream() error=nil")
	}
	for _, secret := range []string{lifecycleSecret, headerSecret, querySecret, url.QueryEscape(querySecret)} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("runCommandStream() leaked endpoint credential: %q", err)
		}
	}
	if !strings.Contains(err.Error(), "region=eu") || strings.Count(err.Error(), "[redacted]") != 3 {
		t.Fatalf("runCommandStream() returned unsafe diagnostic: %q", err)
	}
}

func TestProbeRedactsOpaqueAPIKeyFromLiveSDKResponse(t *testing.T) {
	const secret = "opaque-opensandbox-sdk-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"unauthorized","message":"provider rejected opaque=` + secret + ` region=eu"}`))
	}))
	defer server.Close()

	client := &sdkOpenSandboxClient{
		base:   server.URL,
		key:    secret,
		client: secureOpenSandboxHTTPClient(server.Client()),
	}
	err := client.Probe(context.Background())
	if err == nil {
		t.Fatal("Probe() error=nil")
	}
	if strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "region=eu") || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("Probe() returned unsafe diagnostic: %q", err)
	}
	var apiErr *sdk.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Probe() lost SDK error identity: %v", err)
	}
}

func TestRedactProviderErrorPreservesErrorChain(t *testing.T) {
	const secret = "opaque-opensandbox-secret"
	cause := errors.New("provider rejected opaque=" + secret + " region=eu")
	got := (&sdkOpenSandboxClient{key: secret}).redactProviderError(cause)
	if strings.Contains(got.Error(), secret) || !strings.Contains(got.Error(), "region=eu") || !strings.Contains(got.Error(), "[redacted]") {
		t.Fatalf("redactProviderError() returned unsafe diagnostic: %q", got)
	}
	if !errors.Is(got, cause) {
		t.Fatal("redactProviderError() did not preserve the original error chain")
	}
}
