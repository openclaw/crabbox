package superserve

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSuperserveClientCreateListActivateAndDelete(t *testing.T) {
	var requests []struct {
		method string
		path   string
		query  string
		body   string
		key    string
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests = append(requests, struct {
			method string
			path   string
			query  string
			body   string
			key    string
		}{r.Method, r.URL.Path, r.URL.RawQuery, string(body), r.Header.Get("X-API-Key")})
		if r.Header.Get("Authorization") != "" {
			t.Fatalf("unexpected Authorization header: %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sandboxes":
			writeTestJSON(w, map[string]any{"id": "sb_123", "status": "running"})
		case r.Method == http.MethodGet && r.URL.Path == "/sandboxes":
			if r.URL.Query().Get("metadata."+metadataProviderKey) != providerName || r.URL.Query().Get("metadata."+metadataEndpointKey) == "" {
				t.Fatalf("metadata filters missing: %s", r.URL.RawQuery)
			}
			writeTestJSON(w, []map[string]any{{"id": "sb_123", "status": "running", "metadata": map[string]string{metadataProviderKey: providerName}}})
		case r.Method == http.MethodPost && r.URL.Path == "/sandboxes/sb_123/activate":
			writeTestJSON(w, map[string]any{"access_token": "ss_test_token", "sandbox": map[string]any{"id": "sb_123", "status": "running"}})
		case r.Method == http.MethodPatch && r.URL.Path == "/sandboxes/sb_123":
			writeTestJSON(w, map[string]any{"id": "sb_123", "metadata": map[string]string{metadataProviderKey: providerName}})
		case r.Method == http.MethodDelete && r.URL.Path == "/sandboxes/sb_123":
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	t.Setenv("CRABBOX_SUPERSERVE_API_KEY", "ss_test_key")
	client, err := newSuperserveClient(testConfigWithBaseURL(server.URL), Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateSandbox(context.Background(), createSandboxRequest{Template: "superserve/base"}); err != nil {
		t.Fatalf("CreateSandbox err=%v", err)
	}
	if _, err := client.ListSandboxes(context.Background(), map[string]string{metadataProviderKey: providerName, metadataEndpointKey: "endpoint"}); err != nil {
		t.Fatalf("ListSandboxes err=%v", err)
	}
	access, err := client.ActivateSandbox(context.Background(), "sb_123")
	if err != nil {
		t.Fatalf("ActivateSandbox err=%v", err)
	}
	if access.AccessToken != "ss_test_token" {
		t.Fatalf("access token=%q", access.AccessToken)
	}
	if _, err := client.UpdateSandboxMetadata(context.Background(), "sb_123", map[string]string{metadataProviderKey: providerName}); err != nil {
		t.Fatalf("UpdateSandboxMetadata err=%v", err)
	}
	if err := client.DeleteSandbox(context.Background(), "sb_123"); err != nil {
		t.Fatalf("DeleteSandbox err=%v", err)
	}
	for _, req := range requests {
		if req.key != "ss_test_key" {
			t.Fatalf("%s %s X-API-Key=%q", req.method, req.path, req.key)
		}
	}
}

func TestSuperserveClientRejectsCrossOriginRedirect(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("cross-origin redirect target should not be reached")
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/sandboxes", http.StatusFound)
	}))
	defer server.Close()
	t.Setenv("CRABBOX_SUPERSERVE_API_KEY", "ss_test_key")
	client, err := newSuperserveClient(testConfigWithBaseURL(server.URL), Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	err = client.Probe(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refused cross-origin redirect") {
		t.Fatalf("err=%v, want cross-origin redirect refusal", err)
	}
}

func TestSuperserveClientRedactsSecretsFromErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"bad ss_test_key token {\"access_token\": \"ss_test_token\"}"}`)
	}))
	defer server.Close()
	t.Setenv("CRABBOX_SUPERSERVE_API_KEY", "ss_test_key")
	client, err := newSuperserveClient(testConfigWithBaseURL(server.URL), Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	err = client.Probe(context.Background())
	if err == nil {
		t.Fatal("expected probe error")
	}
	if strings.Contains(err.Error(), "ss_test_key") || strings.Contains(err.Error(), "ss_test_token") {
		t.Fatalf("secret leaked in error: %v", err)
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("error was not redacted: %v", err)
	}
}

func TestSuperserveClientUpdateDoesNotFabricateMissingMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/sandboxes/sb_123" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeTestJSON(w, map[string]any{"id": "sb_123"})
	}))
	defer server.Close()
	t.Setenv("CRABBOX_SUPERSERVE_API_KEY", "ss_test_key")
	client, err := newSuperserveClient(testConfigWithBaseURL(server.URL), Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	sb, err := client.UpdateSandboxMetadata(context.Background(), "sb_123", map[string]string{metadataClaimKey: leasePrefix + "sb_123"})
	if err != nil {
		t.Fatalf("UpdateSandboxMetadata err=%v", err)
	}
	if sb.Metadata != nil {
		t.Fatalf("metadata was fabricated from request: %#v", sb.Metadata)
	}
}

func TestDataPlaneHostForSandbox(t *testing.T) {
	got := dataPlaneHostForSandbox("sb_123", "sandbox.example.test")
	if got != "boxd-sb_123.sandbox.example.test" {
		t.Fatalf("host=%q", got)
	}
	if dataPlaneHostForSandbox("", "sandbox.example.test") != "" {
		t.Fatal("empty sandbox id should not derive host")
	}
}

func writeTestJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func testConfigWithBaseURL(baseURL string) Config {
	cfg := testConfig()
	cfg.Superserve.BaseURL = baseURL
	return cfg
}
