package blaxel

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestValidateAPIURLCanonicalizesAndRejectsUnsafe(t *testing.T) {
	got, err := ValidateAPIURL("https://API.BLAXEL.AI:443/v1/")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://api.blaxel.ai" {
		t.Fatalf("ValidateAPIURL canonical=%q", got)
	}
	if got, err := ValidateAPIURL("http://localhost:8080/v1"); err != nil || got != "http://localhost:8080" {
		t.Fatalf("loopback ValidateAPIURL=%q err=%v", got, err)
	}
	for _, raw := range []string{
		"https://user:pass@api.blaxel.ai",
		"https://api.blaxel.ai?api_key=secret",
		"https://api.blaxel.ai#secret",
		"http://api.blaxel.ai",
	} {
		if _, err := ValidateAPIURL(raw); err == nil {
			t.Fatalf("ValidateAPIURL(%q) succeeded", raw)
		}
	}
}

func TestClientHeadersAndListShapes(t *testing.T) {
	var sawWorkspace, sawVersion, sawAuth bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sandboxes" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		sawWorkspace = r.Header.Get("X-Blaxel-Workspace") == "workspace-test"
		sawVersion = r.Header.Get("Blaxel-Version") == defaultAPIVersion
		sawAuth = r.Header.Get("Authorization") == "Bearer test-key"
		if r.URL.Query().Get("limit") != "2" {
			t.Fatalf("query=%s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"id": "sbx_1", "name": "one"}},
			"meta": map[string]any{"nextCursor": "cursor-2"},
		})
	}))
	defer server.Close()

	client, err := newBlaxelClient(core.Config{Blaxel: core.BlaxelConfig{
		APIURL:    server.URL,
		APIKey:    "test-key",
		Workspace: "workspace-test",
	}}, core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.ListSandboxes(context.Background(), ListSandboxesRequest{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !sawWorkspace || !sawVersion || !sawAuth {
		t.Fatalf("headers workspace=%t version=%t auth=%t", sawWorkspace, sawVersion, sawAuth)
	}
	if len(result.Sandboxes) != 1 || result.Sandboxes[0].ID != "sbx_1" || result.Next != "cursor-2" {
		t.Fatalf("result=%#v", result)
	}

	bare, err := parseSandboxList([]byte(`[{"id":"sbx_2","name":"two"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(bare.Sandboxes) != 1 || bare.Sandboxes[0].ID != "sbx_2" {
		t.Fatalf("bare=%#v", bare)
	}
}

func TestClientUpdatesLabelsAndUploadsBinaryFile(t *testing.T) {
	var sawPatch bool
	var sawUpload bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPatch && r.URL.Path == "/v1/sandboxes/sbx_1":
			sawPatch = true
			var body struct {
				Labels map[string]string `json:"labels"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Labels["crabbox.lease"] != "blx_sbx_1" {
				t.Fatalf("patch labels=%#v", body.Labels)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "sbx_1", "labels": body.Labels})
		case r.Method == http.MethodPut && r.URL.Path == "/v1/sandboxes/sbx_1/filesystem":
			sawUpload = true
			var body WriteFileRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Path != "/tmp/archive.tgz" || body.Encoding != "base64" {
				t.Fatalf("upload body=%#v", body)
			}
			decoded, err := base64.StdEncoding.DecodeString(body.Content)
			if err != nil {
				t.Fatal(err)
			}
			if string(decoded) != "\x00\x01archive" {
				t.Fatalf("decoded upload=%q", decoded)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	client, err := newBlaxelClient(core.Config{Blaxel: core.BlaxelConfig{
		APIURL: server.URL,
		APIKey: "test-key",
	}}, core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.UpdateSandboxLabels(context.Background(), "sbx_1", map[string]string{"crabbox.lease": "blx_sbx_1"}); err != nil {
		t.Fatal(err)
	}
	if err := client.UploadFile(context.Background(), "sbx_1", "/tmp/archive.tgz", strings.NewReader("\x00\x01archive")); err != nil {
		t.Fatal(err)
	}
	if !sawPatch || !sawUpload {
		t.Fatalf("sawPatch=%t sawUpload=%t", sawPatch, sawUpload)
	}
}

func TestClientRedactsAPIKeyFromErrors(t *testing.T) {
	t.Setenv("CRABBOX_BLAXEL_API_KEY", "secret-key")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad secret-key", http.StatusUnauthorized)
	}))
	defer server.Close()
	client, err := newBlaxelClient(core.Config{Blaxel: core.BlaxelConfig{
		APIURL: server.URL,
		APIKey: "secret-key",
	}}, core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ListSandboxes(context.Background(), ListSandboxesRequest{})
	if err == nil {
		t.Fatal("ListSandboxes succeeded")
	}
	if strings.Contains(err.Error(), "secret-key") || !strings.Contains(err.Error(), "<redacted>") {
		t.Fatalf("error not redacted: %v", err)
	}
}

func TestSecureHTTPClientRedirectPolicy(t *testing.T) {
	client := secureHTTPClient(&http.Client{})
	req := &http.Request{URL: mustParseURL(t, "https://api.blaxel.ai/next")}
	var via []*http.Request
	for i := 0; i < 10; i++ {
		via = append(via, &http.Request{URL: mustParseURL(t, "https://api.blaxel.ai/loop")})
	}
	if err := client.CheckRedirect(req, via); err == nil || !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Fatalf("redirect cap err=%v", err)
	}
	crossOriginVia := []*http.Request{{URL: mustParseURL(t, "https://api.blaxel.ai/start")}}
	crossOriginReq := &http.Request{URL: mustParseURL(t, "https://evil.example/next")}
	if err := client.CheckRedirect(crossOriginReq, crossOriginVia); err == nil || !strings.Contains(err.Error(), "refused cross-origin redirect") {
		t.Fatalf("cross-origin err=%v", err)
	}
	originalErr := errors.New("original policy")
	withOriginal := secureHTTPClient(&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return originalErr
	}})
	if err := withOriginal.CheckRedirect(req, crossOriginVia); !errors.Is(err, originalErr) {
		t.Fatalf("original policy err=%v", err)
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
