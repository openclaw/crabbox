package blaxel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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

func TestValidateSandboxEndpointRestrictsBearerTokenDestinations(t *testing.T) {
	got, err := validateSandboxEndpoint("https://SBX-ONE-WORKSPACE.us-pdx-1.BL.RUN:443/api/", "https://api.blaxel.ai")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://sbx-one-workspace.us-pdx-1.bl.run/api" {
		t.Fatalf("endpoint=%q", got)
	}
	if got, err := validateSandboxEndpoint("http://127.0.0.1:8080/sandbox/sbx-1", "http://localhost:9999"); err != nil || got != "http://127.0.0.1:8080/sandbox/sbx-1" {
		t.Fatalf("loopback endpoint=%q err=%v", got, err)
	}
	for _, tc := range []struct {
		name       string
		endpoint   string
		management string
	}{
		{name: "userinfo", endpoint: "https://user:pass@sbx-one.us-pdx-1.bl.run", management: "https://api.blaxel.ai"},
		{name: "query", endpoint: "https://sbx-one.us-pdx-1.bl.run?token=secret", management: "https://api.blaxel.ai"},
		{name: "fragment", endpoint: "https://sbx-one.us-pdx-1.bl.run#secret", management: "https://api.blaxel.ai"},
		{name: "http remote", endpoint: "http://sbx-one.us-pdx-1.bl.run", management: "https://api.blaxel.ai"},
		{name: "untrusted host", endpoint: "https://evil.example/sandbox", management: "https://api.blaxel.ai"},
		{name: "loopback with remote management", endpoint: "http://127.0.0.1:8080/sandbox", management: "https://api.blaxel.ai"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := validateSandboxEndpoint(tc.endpoint, tc.management); err == nil {
				t.Fatalf("validateSandboxEndpoint(%q, %q) succeeded", tc.endpoint, tc.management)
			}
		})
	}
}

func TestClientHeadersAndListShapes(t *testing.T) {
	var sawWorkspace, sawVersion, sawAuth bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/sandboxes" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		sawWorkspace = r.Header.Get("X-Blaxel-Workspace") == "workspace-test"
		sawVersion = r.Header.Get("Blaxel-Version") == defaultAPIVersion
		sawAuth = r.Header.Get("Authorization") == "Bearer test-key"
		if r.URL.Query().Get("limit") != "2" {
			t.Fatalf("query=%s", r.URL.RawQuery)
		}
		if r.URL.Query().Get("q") != "workspace-token" {
			t.Fatalf("query=%s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{
				"metadata": map[string]any{
					"name":   "sbx-1",
					"url":    serverURL(r) + "/sandbox/sbx-1",
					"labels": map[string]string{"env": "dev"},
				},
				"spec":   map[string]any{"region": "us-pdx-1", "runtime": map[string]any{"image": "ubuntu:24.04"}},
				"status": "DEPLOYED",
			}},
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
	result, err := client.ListSandboxes(context.Background(), ListSandboxesRequest{
		Limit:  2,
		Labels: map[string]string{"crabbox.blaxel.scope": "workspace-token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawWorkspace || !sawVersion || !sawAuth {
		t.Fatalf("headers workspace=%t version=%t auth=%t", sawWorkspace, sawVersion, sawAuth)
	}
	if len(result.Sandboxes) != 1 ||
		result.Sandboxes[0].ID != "sbx-1" ||
		result.Sandboxes[0].Endpoint == "" ||
		result.Sandboxes[0].Labels["env"] != "dev" ||
		result.Next != "cursor-2" {
		t.Fatalf("result=%#v", result)
	}

	bare, err := parseSandboxList([]byte(`[{"metadata":{"name":"sbx-2","url":"https://sbx.example","labels":{"team":"core"}},"state":"RUNNING"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(bare.Sandboxes) != 1 || bare.Sandboxes[0].ID != "sbx-2" || bare.Sandboxes[0].Status != "RUNNING" {
		t.Fatalf("bare=%#v", bare)
	}
}

func TestClientUsesManagementShapeAndSandboxDataPlane(t *testing.T) {
	var sawPutLabels bool
	var sawProcess bool
	var sawUpload bool
	var sawComplete bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/sandboxes/sbx-1":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"metadata": map[string]any{
					"name": "sbx-1",
					"url":  "http://" + r.Host + "/sandbox/sbx-1",
				},
				"spec":   map[string]any{"region": "us-pdx-1", "runtime": map[string]any{"image": "ubuntu:24.04"}},
				"status": "DEPLOYED",
			})
		case r.Method == http.MethodPut && r.URL.Path == "/v0/sandboxes/sbx-1":
			sawPutLabels = true
			var body struct {
				Metadata struct {
					Labels map[string]string `json:"labels"`
				} `json:"metadata"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Metadata.Labels["crabbox.lease"] != "blx_sbx-1" {
				t.Fatalf("put labels=%#v", body.Metadata.Labels)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"metadata": map[string]any{"name": "sbx-1", "url": "http://" + r.Host + "/sandbox/sbx-1", "labels": body.Metadata.Labels},
				"status":   "DEPLOYED",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/sandbox/sbx-1/process":
			sawProcess = true
			var body struct {
				Command           string `json:"command"`
				WaitForCompletion bool   `json:"waitForCompletion"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Command != "'go' 'test' './...'" || body.WaitForCompletion {
				t.Fatalf("process body=%#v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"pid": "1234", "status": "running"})
		case r.Method == http.MethodPost && r.URL.Path == "/sandbox/sbx-1/filesystem-multipart/initiate/tmp/archive.tgz":
			_ = json.NewEncoder(w).Encode(map[string]any{"uploadId": "upload-1", "path": "/tmp/archive.tgz"})
		case r.Method == http.MethodPut && r.URL.Path == "/sandbox/sbx-1/filesystem-multipart/upload-1/part":
			sawUpload = true
			if r.Header.Get("Blaxel-Version") != defaultAPIVersion || r.Header.Get("X-Blaxel-Workspace") != "workspace-test" {
				t.Fatalf("multipart headers version=%q workspace=%q", r.Header.Get("Blaxel-Version"), r.Header.Get("X-Blaxel-Workspace"))
			}
			if r.URL.Query().Get("partNumber") != "1" {
				t.Fatalf("query=%s", r.URL.RawQuery)
			}
			if err := r.ParseMultipartForm(1024); err != nil {
				t.Fatal(err)
			}
			file, _, err := r.FormFile("file")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			data, err := io.ReadAll(file)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != "\x00\x01archive" {
				t.Fatalf("multipart upload=%q", data)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"etag": "etag-1", "partNumber": 1, "size": len(data)})
		case r.Method == http.MethodPost && r.URL.Path == "/sandbox/sbx-1/filesystem-multipart/upload-1/complete":
			sawComplete = true
			var body struct {
				Parts []multipartUploadPart `json:"parts"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if len(body.Parts) != 1 || body.Parts[0].ETag != "etag-1" {
				t.Fatalf("complete body=%#v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "ok"})
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
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
	if _, err := client.UpdateSandboxLabels(context.Background(), "sbx-1", map[string]string{"crabbox.lease": "blx_sbx-1"}); err != nil {
		t.Fatal(err)
	}
	process, err := client.ExecuteProcess(context.Background(), "sbx-1", ExecuteProcessRequest{
		Command: "go",
		Args:    []string{"test", "./..."},
	})
	if err != nil {
		t.Fatal(err)
	}
	if process.ID != "1234" || process.Status != "running" {
		t.Fatalf("process=%#v", process)
	}
	if err := client.UploadFile(context.Background(), "sbx-1", "/tmp/archive.tgz", strings.NewReader("\x00\x01archive")); err != nil {
		t.Fatal(err)
	}
	if !sawPutLabels || !sawProcess || !sawUpload || !sawComplete {
		t.Fatalf("sawPutLabels=%t sawProcess=%t sawUpload=%t sawComplete=%t", sawPutLabels, sawProcess, sawUpload, sawComplete)
	}
}

func TestClientRejectsUnsafeSandboxEndpointBeforeDataPlaneAuth(t *testing.T) {
	var dataPlaneRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/sandboxes/sbx-1":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"metadata": map[string]any{
					"name": "sbx-1",
					"url":  "https://evil.example/sandbox/sbx-1",
				},
				"status": "DEPLOYED",
			})
		default:
			dataPlaneRequests++
			t.Fatalf("unexpected data-plane request %s %s", r.Method, r.URL.Path)
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
	_, err = client.ExecuteProcess(context.Background(), "sbx-1", ExecuteProcessRequest{Command: "true"})
	if err == nil || !strings.Contains(err.Error(), "not a trusted Blaxel data-plane origin") {
		t.Fatalf("ExecuteProcess err=%v", err)
	}
	if dataPlaneRequests != 0 {
		t.Fatalf("dataPlaneRequests=%d", dataPlaneRequests)
	}
}

func serverURL(r *http.Request) string {
	return "http://" + r.Host
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
