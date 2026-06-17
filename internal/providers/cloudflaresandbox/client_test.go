package cloudflaresandbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBridgeClientHealthOpenAPIAuthAndNonMutatingDoctorRoutes(t *testing.T) {
	var requests []struct {
		method string
		path   string
		auth   string
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, struct {
			method string
			path   string
			auth   string
		}{r.Method, r.URL.Path, r.Header.Get("Authorization")})
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/health":
			if r.Header.Get("Authorization") != "" {
				t.Fatalf("health unexpectedly received Authorization header")
			}
			writeTestJSON(w, map[string]any{"ok": true, "version": "test"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/openapi.json":
			if r.Header.Get("Authorization") != "Bearer cf_sandbox_test_token" {
				t.Fatalf("openapi Authorization=%q", r.Header.Get("Authorization"))
			}
			writeTestJSON(w, map[string]any{"openapi": "3.1.0", "info": map[string]any{"title": "Cloudflare Sandbox Bridge"}})
		default:
			t.Fatalf("doctor made unexpected mutating or unknown call: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.CloudflareSandbox.BridgeURL = server.URL
	cfg.CloudflareSandbox.Token = "cf_sandbox_test_token"
	backend := NewBackend((Provider{}).Spec(), cfg, Runtime{HTTP: server.Client()}).(*backend)
	result, err := backend.Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider != providerName || result.Status != "ok" {
		t.Fatalf("doctor result=%#v", result)
	}
	if len(requests) != 2 || requests[0].method != http.MethodGet || requests[0].path != "/health" || requests[1].path != "/v1/openapi.json" {
		t.Fatalf("requests=%#v", requests)
	}
	for _, check := range result.Checks {
		if check.Details["mutation"] != "false" {
			t.Fatalf("check missing mutation=false: %#v", check)
		}
	}
}

func TestBridgeClientRuntimeEndpointShapeIsTypedForPlan02(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		paths = append(paths, r.Method+" "+r.URL.Path+" "+string(body))
		if r.Header.Get("Authorization") != "Bearer cf_sandbox_test_token" {
			t.Fatalf("%s %s Authorization=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
			writeTestJSON(w, map[string]any{"id": "sb_123", "status": "running"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sb_123":
			writeTestJSON(w, map[string]any{"id": "sb_123", "status": "running"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes":
			writeTestJSON(w, []map[string]any{{"id": "sb_123"}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/running":
			writeTestJSON(w, []map[string]any{{"id": "sb_123", "status": "running"}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes/sb_123/exec":
			writeTestJSON(w, map[string]any{"stdout": "ok\n", "exitCode": 0})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes/sb_123/files/write":
			if r.URL.Query().Get("path") != "/tmp/archive.tgz" {
				t.Fatalf("upload path query=%q", r.URL.RawQuery)
			}
			if r.Header.Get("Content-Type") != "application/octet-stream" {
				t.Fatalf("upload content-type=%q", r.Header.Get("Content-Type"))
			}
			if string(body) != "archive" {
				t.Fatalf("upload body=%q", body)
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes/sb_123/persist":
			writeTestJSON(w, map[string]any{"id": "persist_123"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes/sb_123/hydrate":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/warm-pool":
			writeTestJSON(w, map[string]any{"ready": 1, "total": 2})
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/sandboxes/sb_123":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.CloudflareSandbox.BridgeURL = server.URL
	cfg.CloudflareSandbox.Token = "cf_sandbox_test_token"
	api, err := newBridgeClient(cfg, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.CreateSandbox(context.Background(), createSandboxRequest{Name: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.GetSandbox(context.Background(), "sb_123"); err != nil {
		t.Fatal(err)
	}
	if _, err := api.ListSandboxes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := api.ListRunning(context.Background()); err != nil {
		t.Fatal(err)
	}
	var stdout strings.Builder
	if _, err := api.Exec(context.Background(), "sb_123", execRequest{Command: "echo ok"}, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "ok\n" {
		t.Fatalf("stdout=%q", stdout.String())
	}
	if err := api.UploadFile(context.Background(), "sb_123", "/tmp/archive.tgz", strings.NewReader("archive")); err != nil {
		t.Fatal(err)
	}
	if _, err := api.Persist(context.Background(), "sb_123", persistRequest{Path: "/workspace"}); err != nil {
		t.Fatal(err)
	}
	if err := api.Hydrate(context.Background(), "sb_123", hydrateRequest{ID: "persist_123"}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.WarmPool(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := api.DeleteSandbox(context.Background(), "sb_123"); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 10 {
		t.Fatalf("paths=%#v", paths)
	}
}

func TestBridgeClientExecParsesSSEOutputBeforeExit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/sandboxes/sb_123/exec" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: stdout\n")
		_, _ = io.WriteString(w, `data: {"chunk":"`+base64.StdEncoding.EncodeToString([]byte("early\n"))+`"}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = io.WriteString(w, "event: stderr\n")
		_, _ = io.WriteString(w, `data: {"chunk":"`+base64.StdEncoding.EncodeToString([]byte("warn\n"))+`"}`+"\n\n")
		_, _ = io.WriteString(w, "event: exit\n")
		_, _ = io.WriteString(w, `data: {"exitCode":7}`+"\n\n")
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.CloudflareSandbox.BridgeURL = server.URL
	api, err := newBridgeClient(cfg, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	result, err := api.Exec(context.Background(), "sb_123", execRequest{Command: "test"}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 7 || stdout.String() != "early\n" || stderr.String() != "warn\n" {
		t.Fatalf("result=%#v stdout=%q stderr=%q", result, stdout.String(), stderr.String())
	}
}

func TestBridgeClientRedactsTokenFromErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"bad cf_sandbox_test_token"}`)
	}))
	defer server.Close()

	cfg := testConfig()
	cfg.CloudflareSandbox.BridgeURL = server.URL
	cfg.CloudflareSandbox.Token = "cf_sandbox_test_token"
	api, err := newBridgeClient(cfg, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = api.OpenAPI(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "cf_sandbox_test_token") {
		t.Fatalf("secret leaked in error: %v", err)
	}
}

func writeTestJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		panic(err)
	}
}
