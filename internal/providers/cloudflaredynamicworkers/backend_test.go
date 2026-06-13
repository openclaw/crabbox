package cloudflaredynamicworkers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderSpecAndFlags(t *testing.T) {
	spec := Provider{}.Spec()
	if spec.Name != providerName || spec.Family != "cloudflare" || spec.Kind != "delegated-run" {
		t.Fatalf("spec=%#v", spec)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != targetWorker {
		t.Fatalf("targets=%#v", spec.Targets)
	}
	for _, alias := range (Provider{}).Aliases() {
		if alias != "cf-dynamic" && alias != "cfdw" {
			t.Fatalf("unexpected alias %q", alias)
		}
	}
	fs := newTestFlagSet()
	Provider{}.RegisterFlags(fs, Config{})
	if fs.Lookup("cloudflare-dynamic-workers-token") != nil {
		t.Fatal("provider must not expose a token CLI flag")
	}
}

func TestProviderFlagsRejectExpose(t *testing.T) {
	cfg := testConfig("https://loader.example.test")
	cfg.Provider = providerName
	fs := newTestFlagSet()
	_ = fs.String("expose", "", "")
	values := Provider{}.RegisterFlags(fs, cfg)
	if err := fs.Parse([]string{"--expose", "8080"}); err != nil {
		t.Fatal(err)
	}
	err := Provider{}.ApplyFlags(&cfg, fs, values)
	if err == nil || !strings.Contains(err.Error(), "--expose is not supported") {
		t.Fatalf("error=%v", err)
	}
}

func TestConfigureNormalizesDefaultLinuxTarget(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:1")
	cfg.TargetOS = "linux"
	configured, err := Provider{}.Configure(cfg, Runtime{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
	if err != nil {
		t.Fatal(err)
	}
	if got := configured.(*backend).cfg.TargetOS; got != targetWorker {
		t.Fatalf("target=%q want %q", got, targetWorker)
	}
}

func TestConfigureRejectsExplicitLinuxTarget(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:1")
	cfg.TargetOS = core.TargetLinux
	core.MarkTargetExplicit(&cfg)
	_, err := Provider{}.Configure(cfg, Runtime{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "supports target=worker-runtime only") {
		t.Fatalf("error=%v", err)
	}
}

func TestConfigureRejectsUnsupportedImplicitTargets(t *testing.T) {
	for _, target := range []string{core.TargetMacOS, core.TargetWindows, "invalid"} {
		t.Run(target, func(t *testing.T) {
			cfg := testConfig("http://127.0.0.1:1")
			cfg.TargetOS = target
			_, err := Provider{}.Configure(cfg, Runtime{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
			if err == nil || !strings.Contains(err.Error(), "supports target=worker-runtime only") {
				t.Fatalf("target=%q error=%v", target, err)
			}
		})
	}
}

func TestListWithoutRefreshValidatesLoaderURL(t *testing.T) {
	cfg := testConfig("")
	cfg.CloudflareDynamicWorkers.LoaderURL = ""
	configured := NewBackend(Provider{}.Spec(), cfg, Runtime{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
	_, err := configured.(*backend).List(context.Background(), ListRequest{})
	if err == nil || !strings.Contains(err.Error(), "requires cloudflareDynamicWorkers.loaderUrl") {
		t.Fatalf("err=%v", err)
	}
}

func TestWarmupRejectsMissingModuleSource(t *testing.T) {
	backend := newTestBackend("http://127.0.0.1:1", &bytes.Buffer{}, &bytes.Buffer{})
	err := backend.Warmup(context.Background(), WarmupRequest{})
	if err == nil || !strings.Contains(err.Error(), "requires module source") {
		t.Fatalf("err=%v", err)
	}
}

func TestLoaderURLRejectsUnsafeComponents(t *testing.T) {
	tests := []string{
		"https://user:pass@loader.example.test",
		"https://loader.example.test/path?token=bad",
		"https://loader.example.test/path#frag",
		"http://loader.example.test",
	}
	for _, raw := range tests {
		t.Run(raw, func(t *testing.T) {
			cfg := testConfig(raw)
			if _, err := loaderURL(cfg); err == nil {
				t.Fatalf("loaderURL(%q) succeeded", raw)
			}
		})
	}
}

func TestLoaderURLRedactsQueryAndFragmentFromErrors(t *testing.T) {
	cfg := testConfig("https://loader.example.test/path?token=secret#frag")
	_, err := loaderURL(cfg)
	if err == nil {
		t.Fatal("loaderURL succeeded")
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "token=") || strings.Contains(err.Error(), "#frag") {
		t.Fatalf("loader URL error leaked sensitive components: %v", err)
	}
	if !strings.Contains(err.Error(), "https://loader.example.test/path") {
		t.Fatalf("loader URL error omitted sanitized URL: %v", err)
	}
}

func TestLoaderURLRedactsUserinfoFromInvalidHostErrors(t *testing.T) {
	cfg := testConfig("https://user:password@")
	_, err := loaderURL(cfg)
	if err == nil {
		t.Fatal("loaderURL succeeded")
	}
	if strings.Contains(err.Error(), "user") || strings.Contains(err.Error(), "password") {
		t.Fatalf("loader URL error leaked userinfo: %v", err)
	}
}

func TestLoaderURLRedactsUserinfoWhenParsingFails(t *testing.T) {
	cfg := testConfig("https://user:secret%zz@loader.example.test")
	_, err := loaderURL(cfg)
	if err == nil {
		t.Fatal("loaderURL succeeded")
	}
	for _, sensitive := range []string{"user", "secret", "loader.example.test"} {
		if strings.Contains(err.Error(), sensitive) {
			t.Fatalf("loader URL error leaked %q: %v", sensitive, err)
		}
	}
	if !strings.Contains(err.Error(), "<redacted>") {
		t.Fatalf("loader URL error omitted redaction marker: %v", err)
	}
}

func TestLoaderURLRedactsOpaqueUserinfo(t *testing.T) {
	cfg := testConfig("https:user:secret@loader.example.test")
	_, err := loaderURL(cfg)
	if err == nil {
		t.Fatal("loaderURL succeeded")
	}
	for _, sensitive := range []string{"user", "secret", "loader.example.test"} {
		if strings.Contains(err.Error(), sensitive) {
			t.Fatalf("loader URL error leaked %q: %v", sensitive, err)
		}
	}
	if !strings.Contains(err.Error(), "<redacted>") {
		t.Fatalf("loader URL error omitted redaction marker: %v", err)
	}
}

func TestDefaultHTTPClientHonorsConfiguredRunTimeout(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:1")
	cfg.CloudflareDynamicWorkers.TimeoutSecs = 60
	client := defaultHTTPClient(cfg)
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport=%T", client.Transport)
	}
	if transport.ResponseHeaderTimeout < 65*time.Second {
		t.Fatalf("response header timeout=%s, want at least configured run timeout plus overhead", transport.ResponseHeaderTimeout)
	}
}

func TestClientTimeoutCoversResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":`))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	client := &client{
		baseURL:             server.URL,
		token:               "test-token",
		http:                server.Client(),
		responseBodyTimeout: 25 * time.Millisecond,
	}

	_, err := client.Readiness(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("readiness error=%v, want deadline exceeded", err)
	}
}

func TestClientTimeoutPreservesErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid`))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	client := &client{
		baseURL:             server.URL,
		token:               "test-token",
		http:                server.Client(),
		responseBodyTimeout: 25 * time.Millisecond,
	}

	_, err := client.Run(context.Background(), runRequest{})
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("run error=%v, want typed HTTP 400", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("run error=%v, want wrapped deadline exceeded", err)
	}
}

func TestClientRejectsRunResponseIdentityMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(runResponse{ID: "run_other", Status: "succeeded"})
	}))
	defer server.Close()
	client := &client{
		baseURL:             server.URL,
		token:               "test-token",
		http:                server.Client(),
		responseBodyTimeout: time.Second,
	}

	_, err := client.Run(context.Background(), runRequest{ID: "run_expected"})
	var contractErr *responseContractError
	if !errors.As(err, &contractErr) || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("run error=%v, want response contract mismatch", err)
	}
}

func TestClientRejectsMissingGeneratedRunIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(runResponse{Status: "succeeded"})
	}))
	defer server.Close()
	client := &client{
		baseURL:             server.URL,
		token:               "test-token",
		http:                server.Client(),
		responseBodyTimeout: time.Second,
	}

	_, err := client.Run(context.Background(), runRequest{})
	var contractErr *responseContractError
	if !errors.As(err, &contractErr) || !strings.Contains(err.Error(), "missing run id") {
		t.Fatalf("run error=%v, want missing run id", err)
	}
}

func TestClientRejectsNonTerminalRunResponseState(t *testing.T) {
	for _, status := range []string{"running", " succeeded "} {
		t.Run(status, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(runResponse{ID: "run_expected", Status: status})
			}))
			defer server.Close()
			client := &client{
				baseURL:             server.URL,
				token:               "test-token",
				http:                server.Client(),
				responseBodyTimeout: time.Second,
			}

			_, err := client.Run(context.Background(), runRequest{ID: "run_expected"})
			var contractErr *responseContractError
			if !errors.As(err, &contractErr) || !strings.Contains(err.Error(), "invalid run status") {
				t.Fatalf("run error=%v, want invalid status contract error", err)
			}
		})
	}
}

func TestClientRejectsTrailingRunResponseData(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"run_expected","status":"succeeded"}garbage`))
	}))
	defer server.Close()
	client := &client{
		baseURL:             server.URL,
		token:               "test-token",
		http:                server.Client(),
		responseBodyTimeout: time.Second,
	}

	_, err := client.Run(context.Background(), runRequest{ID: "run_expected"})
	var contractErr *responseContractError
	if !errors.As(err, &contractErr) {
		t.Fatalf("run error=%v, want response contract error", err)
	}
}

func TestClientRejectsInvalidStatusIdentityAndState(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response runStatus
		want     string
	}{
		{name: "missing id", response: runStatus{Status: "running"}, want: "missing run id"},
		{name: "mismatched id", response: runStatus{ID: "run_other", Status: "running"}, want: "does not match"},
		{name: "missing status", response: runStatus{ID: "run_expected"}, want: "missing run status"},
		{name: "invalid status", response: runStatus{ID: "run_expected", Status: "garbage"}, want: "invalid run status"},
		{name: "padded status", response: runStatus{ID: "run_expected", Status: " running "}, want: "invalid run status"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(tc.response)
			}))
			defer server.Close()
			client := &client{
				baseURL:             server.URL,
				token:               "test-token",
				http:                server.Client(),
				responseBodyTimeout: time.Second,
			}

			_, err := client.Status(context.Background(), "run_expected")
			var contractErr *responseContractError
			if !errors.As(err, &contractErr) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("status error=%v, want %q", err, tc.want)
			}
		})
	}
}

func TestClientPreservesOrdinaryJSONAPIErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer server.Close()
	client := &client{
		baseURL:             server.URL,
		token:               "test-token",
		http:                server.Client(),
		responseBodyTimeout: time.Second,
	}

	_, err := client.Run(context.Background(), runRequest{ID: "run_expected"})
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("run error=%v, want typed HTTP 401", err)
	}
	var contractErr *responseContractError
	if errors.As(err, &contractErr) {
		t.Fatalf("ordinary API error misclassified as response contract error: %v", err)
	}
}

func TestClientRejectsIncompleteNon2xxLifecycleResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"id":"run_expected","status":"failed","exitCode":1}`))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	client := &client{
		baseURL:             server.URL,
		token:               "test-token",
		http:                server.Client(),
		responseBodyTimeout: 25 * time.Millisecond,
	}

	out, err := client.Run(context.Background(), runRequest{ID: "run_expected"})
	if out.ID != "run_expected" {
		t.Fatalf("run id=%q, want buffered lifecycle identity", out.ID)
	}
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("run error=%v, want typed HTTP 502", err)
	}
	var contractErr *responseContractError
	if !errors.As(err, &contractErr) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("run error=%v, want contract and deadline errors", err)
	}
}

func TestClientRecoversRunIdentityFromTruncatedNon2xxLifecycleResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"id":"run_generated","status":"failed","message":"truncated`))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	client := &client{
		baseURL:             server.URL,
		token:               "test-token",
		http:                server.Client(),
		responseBodyTimeout: 25 * time.Millisecond,
	}

	out, err := client.Run(context.Background(), runRequest{})
	if out.ID != "run_generated" {
		t.Fatalf("run id=%q, want buffered lifecycle identity", out.ID)
	}
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("run error=%v, want typed HTTP 502", err)
	}
	var contractErr *responseContractError
	if !errors.As(err, &contractErr) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("run error=%v, want contract and deadline errors", err)
	}
}

func TestClientRejectsMalformedNon2xxLifecycleResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"id":"run_expected","status":"failed","exitCode":"bad"}`))
	}))
	defer server.Close()
	client := &client{
		baseURL:             server.URL,
		token:               "test-token",
		http:                server.Client(),
		responseBodyTimeout: time.Second,
	}

	_, err := client.Run(context.Background(), runRequest{ID: "run_expected"})
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("run error=%v, want typed HTTP 502", err)
	}
	var contractErr *responseContractError
	if !errors.As(err, &contractErr) {
		t.Fatalf("run error=%v, want response contract error", err)
	}
}

func TestClientDoesNotLetErrorMessageMaskMalformedLifecycleResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"id":"run_expected","status":"failed","exitCode":"bad","message":"execution failed"}`))
	}))
	defer server.Close()
	client := &client{
		baseURL:             server.URL,
		token:               "test-token",
		http:                server.Client(),
		responseBodyTimeout: time.Second,
	}

	out, err := client.Run(context.Background(), runRequest{ID: "run_expected"})
	if out.ID != "run_expected" {
		t.Fatalf("run id=%q, want lifecycle identity", out.ID)
	}
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("run error=%v, want typed HTTP 502", err)
	}
	var contractErr *responseContractError
	if !errors.As(err, &contractErr) {
		t.Fatalf("run error=%v, want response contract error", err)
	}
}

func TestClientPreservesHTTPErrorForInvalidNon2xxLifecycleResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(runResponse{ID: "run_expected", Status: "running"})
	}))
	defer server.Close()
	client := &client{
		baseURL:             server.URL,
		token:               "test-token",
		http:                server.Client(),
		responseBodyTimeout: time.Second,
	}

	_, err := client.Run(context.Background(), runRequest{ID: "run_expected"})
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("run error=%v, want typed HTTP 400", err)
	}
	var contractErr *responseContractError
	if !errors.As(err, &contractErr) {
		t.Fatalf("run error=%v, want response contract error", err)
	}
}

func TestClientRejectsRedirectWithoutForwardingCredentialsOrPayload(t *testing.T) {
	var redirectedRequests int
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectedRequests++
		t.Errorf("redirect target received Authorization=%q", r.Header.Get("Authorization"))
	}))
	defer redirectTarget.Close()
	loader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL+"/stolen", http.StatusTemporaryRedirect)
	}))
	defer loader.Close()

	loaderClient, err := newLoaderAPI(testConfig(loader.URL), Runtime{HTTP: loader.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = loaderClient.Run(context.Background(), runRequest{
		Module: moduleSource{Name: "index.js", Source: "export default {}"},
		Env:    map[string]string{"SECRET": "forwarded-value"},
	})
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("run error=%v, want typed HTTP 307", err)
	}
	if redirectedRequests != 0 {
		t.Fatalf("redirect target requests=%d, want none", redirectedRequests)
	}
}

func TestDoctorReadinessUsesBearerAuthAndDoesNotMutate(t *testing.T) {
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatalf("Authorization=%q", r.Header.Get("Authorization"))
		}
		if r.Method != http.MethodGet || r.URL.Path != "/v1/readiness" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(readinessResponse{
			OK:                 true,
			Runner:             providerName,
			LoaderBinding:      true,
			CoordinatorBinding: true,
			DurableRunMetadata: true,
			CompatibilityDate:  "2026-06-01",
			Egress:             "blocked",
		})
	}))
	defer server.Close()
	backend := newTestBackend(server.URL, &bytes.Buffer{}, &bytes.Buffer{})
	result, err := backend.Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "pass" || !strings.Contains(result.Message, "loader_binding=true") || !strings.Contains(result.Message, "durable_run_metadata=true") {
		t.Fatalf("doctor result=%#v", result)
	}
	if strings.Join(seen, ",") != "GET /v1/readiness" {
		t.Fatalf("requests=%v", seen)
	}
}

func TestDoctorRequiresDurableRunMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(readinessResponse{
			OK:                 true,
			Runner:             providerName,
			LoaderBinding:      true,
			CoordinatorBinding: true,
		})
	}))
	defer server.Close()
	backend := newTestBackend(server.URL, &bytes.Buffer{}, &bytes.Buffer{})
	result, err := backend.Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "fail" {
		t.Fatalf("doctor result=%#v", result)
	}
}

func TestRunPostsModuleSourceWithStableCacheAndLimits(t *testing.T) {
	var got runRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			if r.URL.Path != "/v1/runs/"+got.ID || r.URL.Query().Get("acknowledgedComplete") != "true" {
				t.Fatalf("unexpected cleanup request %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(runResponse{ID: got.ID, Status: "stopped"})
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/runs" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatalf("Authorization=%q", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(runResponse{
			ID:       got.ID,
			WorkerID: got.WorkerID,
			Status:   "succeeded",
			ExitCode: 0,
			Stdout:   "stdout\n",
			Body:     "body\n",
			Stderr:   "stderr\n",
			Logs:     "logs\n",
		})
	}))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	backend := newTestBackend(server.URL, &stdout, &stderr)
	req := RunRequest{
		Repo: Repo{Root: t.TempDir(), Name: "my-app"},
		Script: &RunScriptSpec{
			Source:     "../worker module.mjs",
			RemotePath: ".crabbox/scripts/abc123-worker-module.mjs",
			Data:       []byte("export default { fetch() { return new Response('ok') } }\n"),
		},
		ScriptRequested: true,
		Env:             map[string]string{"FEATURE": "on"},
	}
	result, err := backend.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("run error=%v stderr=%q", err, stderr.String())
	}
	if !strings.HasPrefix(got.ID, "cbx_") || !strings.HasPrefix(got.WorkerID, "cfdw_") ||
		got.ID == got.WorkerID || got.CacheMode != "stable" || got.Egress != "blocked" {
		t.Fatalf("request identity/cache=%#v", got)
	}
	if got.RetainMetadata || got.RetainOnFailure {
		t.Fatalf("unexpected retention=%#v", got)
	}
	if got.Module.Name != "abc123-worker-module.mjs" || got.Module.Source != string(req.Script.Data) {
		t.Fatalf("module=%#v", got.Module)
	}
	if got.Limits.CPUMs != 50 || got.Limits.Subrequests != 12 || got.TimeoutMS != int64(15*time.Second/time.Millisecond) {
		t.Fatalf("limits/timeout=%#v timeout=%d", got.Limits, got.TimeoutMS)
	}
	if got.CompatibilityDate != "2026-06-01" || strings.Join(got.CompatibilityFlags, ",") != "nodejs_compat" {
		t.Fatalf("compat=%q flags=%v", got.CompatibilityDate, got.CompatibilityFlags)
	}
	if got.Env["FEATURE"] != "on" || got.Metadata["team"] != "platform" {
		t.Fatalf("env/metadata=%#v/%#v", got.Env, got.Metadata)
	}
	if stdout.String() != "stdout\nbody\n" || stderr.String() != "stderr\nlogs\n" {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if result.ExitCode != 0 || result.Provider != providerName || result.LeaseID != got.ID || result.Session == nil {
		t.Fatalf("result=%#v", result)
	}
}

func TestRunRejectsSuccessfulLoaderResponseWithoutStatus(t *testing.T) {
	var runID string
	var deleted bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var request runRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			runID = request.ID
			_ = json.NewEncoder(w).Encode(map[string]any{})
		case http.MethodDelete:
			if r.URL.Path != "/v1/runs/"+runID || r.URL.Query().Get("acknowledgedComplete") != "true" {
				t.Fatalf("unexpected cleanup request %s?%s", r.URL.Path, r.URL.RawQuery)
			}
			deleted = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	backend := newTestBackend(server.URL, &bytes.Buffer{}, &bytes.Buffer{})
	result, err := backend.Run(context.Background(), RunRequest{
		ScriptRequested: true,
		Script:          &RunScriptSpec{Source: "worker.mjs", Data: []byte("export default {}")},
	})
	if err == nil || result.ExitCode != 1 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if !deleted {
		t.Fatal("malformed successful response did not clean up run metadata")
	}
}

func TestRunCleansGeneratedIdentityAfterMalformedSuccessfulResponse(t *testing.T) {
	const generatedID = "run_generated"
	var deleted bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var request runRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.ID != "" {
				t.Fatalf("one-shot request id=%q, want empty", request.ID)
			}
			_ = json.NewEncoder(w).Encode(runResponse{ID: generatedID})
		case http.MethodDelete:
			if r.URL.Path != "/v1/runs/"+generatedID || r.URL.Query().Get("acknowledgedComplete") != "true" {
				t.Fatalf("unexpected cleanup request %s?%s", r.URL.Path, r.URL.RawQuery)
			}
			deleted = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	backend := newTestBackend(server.URL, &bytes.Buffer{}, &bytes.Buffer{})
	backend.cfg.CloudflareDynamicWorkers.CacheMode = "one-shot"
	result, err := backend.Run(context.Background(), RunRequest{
		ScriptRequested: true,
		Script:          &RunScriptSpec{Source: "worker.mjs", Data: []byte("export default {}")},
	})
	if err == nil || result.ExitCode != 1 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if !deleted {
		t.Fatal("malformed generated response did not clean up run metadata")
	}
}

func TestRunCleansSubmittedIdentityAfterInvalidJSONResponse(t *testing.T) {
	var runID string
	var deleted bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var request runRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			runID = request.ID
			_, _ = w.Write([]byte(`{"id":`))
		case http.MethodDelete:
			if r.URL.Path != "/v1/runs/"+runID || r.URL.Query().Get("acknowledgedComplete") != "true" {
				t.Fatalf("unexpected cleanup request %s?%s", r.URL.Path, r.URL.RawQuery)
			}
			deleted = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	backend := newTestBackend(server.URL, &bytes.Buffer{}, &bytes.Buffer{})
	result, err := backend.Run(context.Background(), RunRequest{
		ScriptRequested: true,
		Script:          &RunScriptSpec{Source: "worker.mjs", Data: []byte("export default {}")},
	})
	if err == nil || result.ExitCode != 1 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if runID == "" || !deleted {
		t.Fatalf("run id=%q deleted=%t", runID, deleted)
	}
}

func TestRunMalformedResponseCleanupIgnoresCanceledCallerContext(t *testing.T) {
	loader := &contractErrorLoader{}
	originalNewLoaderAPI := newLoaderAPI
	newLoaderAPI = func(Config, Runtime) (loaderAPI, error) {
		return loader, nil
	}
	defer func() {
		newLoaderAPI = originalNewLoaderAPI
	}()

	backend := newTestBackend("http://127.0.0.1:1", &bytes.Buffer{}, &bytes.Buffer{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := backend.Run(ctx, RunRequest{
		ScriptRequested: true,
		Script:          &RunScriptSpec{Source: "worker.mjs", Data: []byte("export default {}")},
	})
	if err == nil || result.ExitCode != 1 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if loader.cleanupID == "" || loader.cleanupContextErr != nil {
		t.Fatalf("cleanup id=%q contextErr=%v", loader.cleanupID, loader.cleanupContextErr)
	}
}

func TestRunStableCacheUsesUniqueRunIDsAndStableWorkerID(t *testing.T) {
	backend := newTestBackend("http://127.0.0.1:1", &bytes.Buffer{}, &bytes.Buffer{})
	req := RunRequest{
		Script: &RunScriptSpec{
			Source: "worker.mjs",
			Data:   []byte("export default { fetch() { return new Response('ok') } }"),
		},
	}

	firstRun, firstWorker, _, _, err := backend.runIdentity(req, "stable")
	if err != nil {
		t.Fatal(err)
	}
	secondRun, secondWorker, _, _, err := backend.runIdentity(req, "stable")
	if err != nil {
		t.Fatal(err)
	}
	if firstRun == secondRun || firstWorker == "" || firstWorker != secondWorker {
		t.Fatalf("first=%q/%q second=%q/%q", firstRun, firstWorker, secondRun, secondWorker)
	}
}

func TestRunTimingJSONRemainsFinalLineAfterUnterminatedLoaderOutput(t *testing.T) {
	var runID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			if r.URL.Path != "/v1/runs/"+runID || r.URL.Query().Get("acknowledgedComplete") != "true" {
				t.Fatalf("unexpected cleanup request %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(runResponse{ID: runID, Status: "stopped"})
			return
		}
		var request runRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		runID = request.ID
		_ = json.NewEncoder(w).Encode(runResponse{
			ID:       request.ID,
			WorkerID: request.WorkerID,
			Status:   "succeeded",
			Stderr:   "stderr without newline",
			Logs:     "logs without newline",
		})
	}))
	defer server.Close()

	var stderr bytes.Buffer
	backend := newTestBackend(server.URL, &bytes.Buffer{}, &stderr)
	result, err := backend.Run(context.Background(), RunRequest{
		TimingJSON:      true,
		ScriptRequested: true,
		Script:          &RunScriptSpec{Source: "worker.mjs", Data: []byte("export default {}")},
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n")
	if len(lines) != 3 || lines[0] != "stderr without newline" || lines[1] != "logs without newline" {
		t.Fatalf("stderr lines=%q", lines)
	}
	var timing struct {
		LeaseID string `json:"leaseId"`
	}
	if err := json.Unmarshal([]byte(lines[2]), &timing); err != nil {
		t.Fatalf("timing line=%q err=%v", lines[2], err)
	}
	if timing.LeaseID != result.LeaseID {
		t.Fatalf("timing lease=%q result=%q", timing.LeaseID, result.LeaseID)
	}
}

func TestRunPreservesStructuredFailedRunAndKeepOnFailureClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var submittedID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/runs" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var request runRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.RetainMetadata || !request.RetainOnFailure {
			t.Fatal("keep-on-failure request did not retain failed metadata")
		}
		submittedID = request.ID
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(runResponse{
			ID:       request.ID,
			Status:   "failed",
			ExitCode: 1,
			Stderr:   "execution failed",
		})
	}))
	defer server.Close()
	var stderr bytes.Buffer
	backend := newTestBackend(server.URL, &bytes.Buffer{}, &stderr)
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:            Repo{Root: t.TempDir()},
		KeepOnFailure:   true,
		RequestedSlug:   "debug-failure",
		ScriptRequested: true,
		Script: &RunScriptSpec{
			Source:     "../worker module.mjs",
			RemotePath: ".crabbox/scripts/abc123-worker-module.mjs",
			Data:       []byte("export default {}"),
		},
	})
	if err == nil {
		t.Fatal("expected failed run")
	}
	if submittedID == "" || result.LeaseID != submittedID || result.Slug != "debug-failure" || result.Session == nil || !result.Session.Kept {
		t.Fatalf("result=%#v", result)
	}
	if _, ok, resolveErr := resolveLeaseClaim(result.Slug, backend.cfg); resolveErr != nil || !ok {
		t.Fatalf("claim slug=%q ok=%t err=%v", result.Slug, ok, resolveErr)
	}
	if strings.Contains(stderr.String(), "-- <command>") || !strings.Contains(stderr.String(), "crabbox status") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestRunKeepOnFailureRemovesMetadataAfterAcknowledgedSuccess(t *testing.T) {
	var deletedID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var request runRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.RetainMetadata || !request.RetainOnFailure {
				t.Fatalf("retention=%#v", request)
			}
			_ = json.NewEncoder(w).Encode(runResponse{
				ID:       request.ID,
				WorkerID: request.WorkerID,
				Status:   "succeeded",
			})
		case http.MethodDelete:
			if r.URL.Query().Get("acknowledgedComplete") != "true" {
				t.Fatalf("delete query=%q", r.URL.RawQuery)
			}
			deletedID = strings.TrimPrefix(r.URL.Path, "/v1/runs/")
			_ = json.NewEncoder(w).Encode(runResponse{ID: deletedID, Status: "stopped"})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	backend := newTestBackend(server.URL, &bytes.Buffer{}, &bytes.Buffer{})
	result, err := backend.Run(context.Background(), RunRequest{
		KeepOnFailure:   true,
		ScriptRequested: true,
		Script:          &RunScriptSpec{Source: "worker.mjs", Data: []byte("export default {}")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if deletedID == "" || deletedID != result.LeaseID {
		t.Fatalf("deleted=%q result=%#v", deletedID, result)
	}
}

func TestRunKeepsLifecycleUncertainClaimFromLiveStatus(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var submittedID string
	deleteCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs":
			var request runRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			submittedID = request.ID
			w.Header().Set("X-Crabbox-Lifecycle-Uncertain", "true")
			_ = json.NewEncoder(w).Encode(runResponse{
				ID:                 request.ID,
				WorkerID:           request.WorkerID,
				Status:             "succeeded",
				LifecycleUncertain: true,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/runs/"+submittedID:
			_ = json.NewEncoder(w).Encode(runStatus{
				ID:       submittedID,
				WorkerID: "worker-live",
				Status:   "running",
				Metadata: map[string]string{"phase": "reconciling"},
			})
		case r.Method == http.MethodDelete:
			deleteCalled = true
			t.Fatalf("unexpected cleanup request %s", r.URL.Path)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	var stderr bytes.Buffer
	backend := newTestBackend(server.URL, &bytes.Buffer{}, &stderr)
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:            Repo{Root: t.TempDir()},
		Keep:            true,
		RequestedSlug:   "uncertain-success",
		ScriptRequested: true,
		Script:          &RunScriptSpec{Source: "worker.mjs", Data: []byte("export default {}")},
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, resolveErr := resolveLeaseClaim(result.Slug, backend.cfg)
	if resolveErr != nil || !ok {
		t.Fatalf("claim slug=%q ok=%t err=%v", result.Slug, ok, resolveErr)
	}
	if claim.Labels["state"] != "running" || claim.Labels["worker_id"] != "worker-live" ||
		claim.Labels["phase"] != "reconciling" || claim.Labels["uncertain"] != "true" {
		t.Fatalf("claim labels=%#v", claim.Labels)
	}
	if deleteCalled || !strings.Contains(stderr.String(), "lifecycle reconciliation pending") {
		t.Fatalf("deleteCalled=%t stderr=%q", deleteCalled, stderr.String())
	}
}

func TestRunServerProtectsStructuralLabels(t *testing.T) {
	server := runServer(
		"lease-real",
		"slug-real",
		runStatus{ID: "lease-real", WorkerID: "worker-real", Status: "running"},
		map[string]string{
			"provider":  "wrong",
			"lease":     "wrong",
			"slug":      "wrong",
			"target":    "wrong",
			"state":     "wrong",
			"worker_id": "wrong",
			"team":      "platform",
		},
	)
	want := map[string]string{
		"provider":  providerName,
		"lease":     "lease-real",
		"slug":      "slug-real",
		"target":    targetWorker,
		"state":     "running",
		"worker_id": "worker-real",
		"team":      "platform",
	}
	for key, value := range want {
		if server.Labels[key] != value {
			t.Fatalf("label %s=%q want %q labels=%#v", key, server.Labels[key], value, server.Labels)
		}
	}
}

func TestNotFoundErrorDoesNotTrustTypedServerErrorText(t *testing.T) {
	if notFoundError(&apiError{
		StatusCode: http.StatusServiceUnavailable,
		Status:     "service unavailable",
		Body:       "binding not found",
	}) {
		t.Fatal("typed 503 must not be treated as not found")
	}
	if !notFoundError(errors.New("legacy API 404 not found")) {
		t.Fatal("untyped legacy 404 should remain compatible")
	}
}

func TestRunWarnsWhenCompatibilityCleanupFailsAfterSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var request runRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(runResponse{
				ID:       request.ID,
				WorkerID: request.WorkerID,
				Status:   "succeeded",
			})
		case http.MethodDelete:
			http.Error(w, "temporary cleanup failure", http.StatusServiceUnavailable)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	var stderr bytes.Buffer
	backend := newTestBackend(server.URL, &bytes.Buffer{}, &stderr)
	result, err := backend.Run(context.Background(), RunRequest{
		ScriptRequested: true,
		Script:          &RunScriptSpec{Source: "worker.mjs", Data: []byte("export default {}")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || !strings.Contains(stderr.String(), "warning: cloudflare-dynamic-workers completed run metadata cleanup failed") {
		t.Fatalf("result=%#v stderr=%q", result, stderr.String())
	}
}

func TestRunPreservesKeepOnFailureClaimWhenPostResponseIsLost(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var submittedID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs":
			var request runRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			submittedID = request.ID
			_ = json.NewEncoder(w).Encode(runResponse{
				ID:       request.ID,
				Status:   "failed",
				ExitCode: 1,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/runs/"+submittedID:
			_ = json.NewEncoder(w).Encode(runStatus{
				ID:       submittedID,
				Status:   "failed",
				Metadata: map[string]string{"reason": "response-lost"},
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	var stderr bytes.Buffer
	cfg := testConfig(server.URL)
	backend := NewBackend(Provider{}.Spec(), cfg, Runtime{
		HTTP:   &http.Client{Transport: losePostResponseTransport{base: http.DefaultTransport}},
		Stdout: &bytes.Buffer{},
		Stderr: &stderr,
	}).(*backend)
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:            Repo{Root: t.TempDir()},
		KeepOnFailure:   true,
		RequestedSlug:   "uncertain-failure",
		TimingJSON:      true,
		ScriptRequested: true,
		Script: &RunScriptSpec{
			Source:     "../worker module.mjs",
			RemotePath: ".crabbox/scripts/abc123-worker-module.mjs",
			Data:       []byte("export default {}"),
		},
	})
	if err == nil {
		t.Fatal("expected lost response error")
	}
	if submittedID == "" || result.LeaseID != submittedID || result.Slug != "uncertain-failure" || result.Session == nil || !result.Session.Kept {
		t.Fatalf("result=%#v submittedID=%q", result, submittedID)
	}
	claim, ok, resolveErr := resolveLeaseClaim(result.Slug, backend.cfg)
	if resolveErr != nil || !ok {
		t.Fatalf("claim slug=%q ok=%t err=%v", result.Slug, ok, resolveErr)
	}
	if claim.Labels["state"] != "failed" || claim.Labels["uncertain"] != "true" || claim.Labels["reason"] != "response-lost" {
		t.Fatalf("claim labels=%#v", claim.Labels)
	}
	if !strings.Contains(stderr.String(), "kept uncertain run="+submittedID) ||
		!strings.Contains(stderr.String(), `"leaseId":"`+submittedID+`"`) {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestRunPreservesKeepOnFailureClaimAfterUnstructuredServerError(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var submittedID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs":
			var request runRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			submittedID = request.ID
			http.Error(w, "upstream response lost", http.StatusBadGateway)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/runs/"+submittedID:
			_ = json.NewEncoder(w).Encode(runStatus{
				ID:       submittedID,
				Status:   "failed",
				Metadata: map[string]string{"reason": "gateway-error"},
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	var stderr bytes.Buffer
	backend := newTestBackend(server.URL, &bytes.Buffer{}, &stderr)
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:            Repo{Root: t.TempDir()},
		KeepOnFailure:   true,
		RequestedSlug:   "gateway-failure",
		ScriptRequested: true,
		Script: &RunScriptSpec{
			Source: "worker.mjs",
			Data:   []byte("export default {}"),
		},
	})
	if err == nil {
		t.Fatal("expected gateway error")
	}
	if submittedID == "" || result.LeaseID != submittedID || result.Session == nil || !result.Session.Kept {
		t.Fatalf("result=%#v submittedID=%q", result, submittedID)
	}
	claim, ok, resolveErr := resolveLeaseClaim(result.Slug, backend.cfg)
	if resolveErr != nil || !ok {
		t.Fatalf("claim slug=%q ok=%t err=%v", result.Slug, ok, resolveErr)
	}
	if claim.Labels["state"] != "failed" || claim.Labels["uncertain"] != "true" || claim.Labels["reason"] != "gateway-error" {
		t.Fatalf("claim labels=%#v", claim.Labels)
	}
}

func TestRunPreservesKeepOnFailureClaimAfterInvalidLifecycleRejection(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var submittedID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs":
			var request runRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			submittedID = request.ID
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":      submittedID,
				"status":  "running",
				"message": "invalid response",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/runs/"+submittedID:
			_ = json.NewEncoder(w).Encode(runStatus{
				ID:       submittedID,
				Status:   "failed",
				Metadata: map[string]string{"reason": "invalid-response"},
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	var stderr bytes.Buffer
	backend := newTestBackend(server.URL, &bytes.Buffer{}, &stderr)
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:            Repo{Root: t.TempDir()},
		KeepOnFailure:   true,
		RequestedSlug:   "invalid-response",
		ScriptRequested: true,
		Script: &RunScriptSpec{
			Source: "worker.mjs",
			Data:   []byte("export default {}"),
		},
	})
	if err == nil {
		t.Fatal("expected invalid lifecycle response error")
	}
	if submittedID == "" || result.LeaseID != submittedID || result.Session == nil || !result.Session.Kept {
		t.Fatalf("result=%#v submittedID=%q", result, submittedID)
	}
	claim, ok, resolveErr := resolveLeaseClaim(result.Slug, backend.cfg)
	if resolveErr != nil || !ok {
		t.Fatalf("claim slug=%q ok=%t err=%v", result.Slug, ok, resolveErr)
	}
	if claim.Labels["state"] != "failed" || claim.Labels["uncertain"] != "true" || claim.Labels["reason"] != "invalid-response" {
		t.Fatalf("claim labels=%#v", claim.Labels)
	}
}

type losePostResponseTransport struct {
	base http.RoundTripper
}

func (t losePostResponseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || req.Method != http.MethodPost {
		return resp, err
	}
	_ = resp.Body.Close()
	return nil, fmt.Errorf("response lost after upload")
}

func TestWorkerModuleNameBoundsLongGeneratedPaths(t *testing.T) {
	name := workerModuleName(&RunScriptSpec{
		RemotePath: ".crabbox/scripts/abc123-" + strings.Repeat("a", 250) + ".mjs",
	})
	if len(name) > 256 {
		t.Fatalf("module name length=%d, want <=256", len(name))
	}
	if !strings.HasSuffix(name, ".mjs") {
		t.Fatalf("module name=%q, want preserved extension", name)
	}
}

func TestWorkerModuleNameUsesJavaScriptForStdin(t *testing.T) {
	name := workerModuleName(&RunScriptSpec{
		Source:     "stdin",
		RemotePath: ".crabbox/scripts/abc123-script.sh",
	})
	if name != "index.js" {
		t.Fatalf("module name=%q, want JavaScript stdin module", name)
	}
}

func TestStableRunIDIncludesForwardedEnv(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:1").CloudflareDynamicWorkers
	source := []byte("export default { fetch() { return new Response('ok') } }\n")
	first := stableRunID("worker.mjs", source, cfg, map[string]string{"TOKEN": "alpha", "FEATURE": "on"})
	reordered := stableRunID("worker.mjs", source, cfg, map[string]string{"FEATURE": "on", "TOKEN": "alpha"})
	second := stableRunID("worker.mjs", source, cfg, map[string]string{"TOKEN": "beta", "FEATURE": "on"})
	empty := stableRunID("worker.mjs", source, cfg, nil)
	renamed := stableRunID("index.js", source, cfg, map[string]string{"TOKEN": "alpha", "FEATURE": "on"})

	if first != reordered {
		t.Fatalf("stable id should be independent of env map iteration order: %q != %q", first, reordered)
	}
	if first == second {
		t.Fatal("stable id should change when forwarded env values change")
	}
	if first == empty {
		t.Fatal("stable id should distinguish forwarded env from no forwarded env")
	}
	if first == renamed {
		t.Fatal("stable id should change when the main module name changes")
	}
}

func TestTerminalStateIncludesSuccessfulRunStates(t *testing.T) {
	for _, status := range []string{"completed", "succeeded", "success", "ok"} {
		if !terminalState(status) {
			t.Fatalf("terminalState(%q)=false, want true", status)
		}
	}
}

func TestRunExplicitCacheRequiresID(t *testing.T) {
	backend := newTestBackend("http://127.0.0.1:1", &bytes.Buffer{}, &bytes.Buffer{})
	backend.cfg.CloudflareDynamicWorkers.CacheMode = "explicit"
	_, err := backend.Run(context.Background(), RunRequest{
		ScriptRequested: true,
		Script:          &RunScriptSpec{Source: "worker.mjs", Data: []byte("export default {}")},
	})
	if err == nil || !strings.Contains(err.Error(), "cache=explicit requires --id") {
		t.Fatalf("err=%v", err)
	}
}

func TestRunExplicitCachePrintsGeneratedLifecycleID(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request runRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(runResponse{
			ID:       request.ID,
			WorkerID: request.WorkerID,
			Status:   "succeeded",
		})
	}))
	defer server.Close()

	var stderr bytes.Buffer
	backend := newTestBackend(server.URL, &bytes.Buffer{}, &stderr)
	backend.cfg.CloudflareDynamicWorkers.CacheMode = "explicit"
	result, err := backend.Run(context.Background(), RunRequest{
		ID:              "named-worker",
		Repo:            Repo{Root: t.TempDir()},
		ScriptRequested: true,
		Script:          &RunScriptSpec{Source: "worker.mjs", Data: []byte("export default {}")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.LeaseID == "" || !strings.Contains(
		stderr.String(),
		"dynamic worker run="+result.LeaseID+" worker=named-worker",
	) {
		t.Fatalf("result=%#v stderr=%q", result, stderr.String())
	}
}

func TestRunRejectsIDOutsideExplicitCache(t *testing.T) {
	backend := newTestBackend("http://127.0.0.1:1", &bytes.Buffer{}, &bytes.Buffer{})
	_, err := backend.Run(context.Background(), RunRequest{
		ID:              "named-worker",
		ScriptRequested: true,
		Script:          &RunScriptSpec{Source: "worker.mjs", Data: []byte("export default {}")},
	})
	if err == nil || !strings.Contains(err.Error(), "--id requires cache=explicit") {
		t.Fatalf("err=%v", err)
	}
}

func TestRunRejectsInterceptEgressWithReusableCache(t *testing.T) {
	backend := newTestBackend("http://127.0.0.1:1", &bytes.Buffer{}, &bytes.Buffer{})
	backend.cfg.CloudflareDynamicWorkers.Egress = "intercept"
	_, err := backend.Run(context.Background(), RunRequest{
		ScriptRequested: true,
		Script:          &RunScriptSpec{Source: "worker.mjs", Data: []byte("export default {}")},
	})
	if err == nil || !strings.Contains(err.Error(), "egress=intercept requires cache=one-shot") {
		t.Fatalf("err=%v", err)
	}
}

func TestStopMissingRemoteRemovesStaleLocalClaim(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/runs/cfdw_stale" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}))
	defer server.Close()
	backend := newTestBackend(server.URL, &bytes.Buffer{}, &bytes.Buffer{})
	repoRoot := t.TempDir()
	if err := claimLease("cfdw_stale", "stale-claim", backend.cfg, repoRoot, time.Minute, false, runServer("cfdw_stale", "stale-claim", runStatus{ID: "cfdw_stale", Status: "ready"}, nil)); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	backend.rt.Stdout = &stdout
	if err := backend.Stop(context.Background(), StopRequest{ID: "stale-claim"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "removed stale cloudflare-dynamic-workers claim cfdw_stale reason=not-found") {
		t.Fatalf("stdout=%q", stdout.String())
	}
	if _, ok, err := resolveLeaseClaim("stale-claim", backend.cfg); err != nil || ok {
		t.Fatalf("claim after stop ok=%t err=%v state_home=%s", ok, err, filepath.Join(stateHome, "crabbox", "claims"))
	}
}

func TestStopMissingRemotePreservesUnrelatedExactClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/runs/shared-id" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}))
	defer server.Close()
	backend := newTestBackend(server.URL, &bytes.Buffer{}, &bytes.Buffer{})
	if err := core.ClaimLeaseForRepoProviderScope("shared-id", "other", "hetzner", "", t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}

	if err := backend.Stop(context.Background(), StopRequest{ID: "shared-id"}); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := core.ResolveLeaseClaim("shared-id")
	if err != nil || !ok || claim.Provider != "hetzner" {
		t.Fatalf("claim=%#v ok=%t err=%v", claim, ok, err)
	}
}

func TestStopPreservesConcurrentlyReplacedClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		core.RemoveLeaseClaim("cfdw_race")
		if err := core.ClaimLeaseForRepoProviderScope("cfdw_race", "replacement", "hetzner", "", t.TempDir(), time.Minute, false); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	backend := newTestBackend(server.URL, &bytes.Buffer{}, &bytes.Buffer{})
	if err := claimLease("cfdw_race", "race-claim", backend.cfg, t.TempDir(), time.Minute, false, runServer("cfdw_race", "race-claim", runStatus{ID: "cfdw_race", Status: "ready"}, nil)); err != nil {
		t.Fatal(err)
	}

	err := backend.Stop(context.Background(), StopRequest{ID: "race-claim"})
	if err == nil || !strings.Contains(err.Error(), "claim changed; retry") {
		t.Fatalf("err=%v", err)
	}
	claim, ok, resolveErr := core.ResolveLeaseClaim("cfdw_race")
	if resolveErr != nil || !ok || claim.Provider != "hetzner" {
		t.Fatalf("claim=%#v ok=%t err=%v", claim, ok, resolveErr)
	}
}

func TestCleanupDeletesTerminalMetadataBeforeClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(runStatus{ID: "cfdw_terminal", Status: "succeeded"})
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	backend := newTestBackend(server.URL, &bytes.Buffer{}, &bytes.Buffer{})
	if err := claimLease("cfdw_terminal", "terminal-claim", backend.cfg, t.TempDir(), time.Minute, false, runServer("cfdw_terminal", "terminal-claim", runStatus{ID: "cfdw_terminal", Status: "succeeded"}, nil)); err != nil {
		t.Fatal(err)
	}

	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(requests, ","); got != "GET /v1/runs/cfdw_terminal,DELETE /v1/runs/cfdw_terminal" {
		t.Fatalf("requests=%q", got)
	}
	if _, ok, err := resolveLeaseClaim("terminal-claim", backend.cfg); err != nil || ok {
		t.Fatalf("claim after cleanup ok=%t err=%v", ok, err)
	}
}

func TestCleanupPreservesClaimReplacedDuringMissingStatus(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var stderr bytes.Buffer
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		core.RemoveLeaseClaim("cfdw_race")
		if err := core.ClaimLeaseForRepoProviderScope("cfdw_race", "replacement", "hetzner", "", t.TempDir(), time.Minute, false); err != nil {
			t.Fatal(err)
		}
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}))
	defer server.Close()
	backend := newTestBackend(server.URL, &bytes.Buffer{}, &stderr)
	if err := claimLease("cfdw_race", "race-claim", backend.cfg, t.TempDir(), time.Minute, false, runServer("cfdw_race", "race-claim", runStatus{ID: "cfdw_race", Status: "ready"}, nil)); err != nil {
		t.Fatal(err)
	}

	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := core.ResolveLeaseClaim("cfdw_race")
	if err != nil || !ok || claim.Provider != "hetzner" {
		t.Fatalf("claim=%#v ok=%t err=%v", claim, ok, err)
	}
	if !strings.Contains(stderr.String(), "claim changed; retry") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestCleanupPreservesClaimReplacedDuringTerminalDelete(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var stderr bytes.Buffer
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(runStatus{ID: "cfdw_race", Status: "succeeded"})
			return
		}
		core.RemoveLeaseClaim("cfdw_race")
		if err := core.ClaimLeaseForRepoProviderScope("cfdw_race", "replacement", "hetzner", "", t.TempDir(), time.Minute, false); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	backend := newTestBackend(server.URL, &bytes.Buffer{}, &stderr)
	if err := claimLease("cfdw_race", "race-claim", backend.cfg, t.TempDir(), time.Minute, false, runServer("cfdw_race", "race-claim", runStatus{ID: "cfdw_race", Status: "succeeded"}, nil)); err != nil {
		t.Fatal(err)
	}

	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := core.ResolveLeaseClaim("cfdw_race")
	if err != nil || !ok || claim.Provider != "hetzner" {
		t.Fatalf("claim=%#v ok=%t err=%v", claim, ok, err)
	}
	if !strings.Contains(stderr.String(), "claim changed; retry") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestCleanupRetainsClaimWhenTerminalMetadataDeleteFails(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var stderr bytes.Buffer
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(runStatus{ID: "cfdw_terminal", Status: "failed"})
		case http.MethodDelete:
			http.Error(w, `{"error":"unavailable"}`, http.StatusServiceUnavailable)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	backend := newTestBackend(server.URL, &bytes.Buffer{}, &stderr)
	if err := claimLease("cfdw_terminal", "terminal-claim", backend.cfg, t.TempDir(), time.Minute, false, runServer("cfdw_terminal", "terminal-claim", runStatus{ID: "cfdw_terminal", Status: "failed"}, nil)); err != nil {
		t.Fatal(err)
	}

	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := resolveLeaseClaim("terminal-claim", backend.cfg); err != nil || !ok {
		t.Fatalf("claim after cleanup ok=%t err=%v", ok, err)
	}
	if !strings.Contains(stderr.String(), "metadata delete failed for cfdw_terminal") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestClaimsAreScopedToLoaderEndpoint(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfgA := testConfig("https://Loader-A.example.test/api/")
	cfgB := testConfig("https://loader-b.example.test/api")
	if err := claimLease("cfdw_scope_a", "scope-a", cfgA, t.TempDir(), time.Minute, false, runServer("cfdw_scope_a", "scope-a", runStatus{ID: "cfdw_scope_a", Status: "ready"}, nil)); err != nil {
		t.Fatal(err)
	}
	if err := claimLease("cfdw_scope_b", "scope-b", cfgB, t.TempDir(), time.Minute, false, runServer("cfdw_scope_b", "scope-b", runStatus{ID: "cfdw_scope_b", Status: "ready"}, nil)); err != nil {
		t.Fatal(err)
	}

	claimsA, err := providerClaims(cfgA)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimsA) != 1 || claimsA[0].LeaseID != "cfdw_scope_a" || claimsA[0].ProviderScope != "endpoint:https://loader-a.example.test/api" {
		t.Fatalf("loader A claims=%#v", claimsA)
	}
	claimsB, err := providerClaims(cfgB)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimsB) != 1 || claimsB[0].LeaseID != "cfdw_scope_b" {
		t.Fatalf("loader B claims=%#v", claimsB)
	}
	if _, ok, err := resolveLeaseClaim("scope-b", cfgA); err != nil || ok {
		t.Fatalf("loader A resolved loader B claim ok=%t err=%v", ok, err)
	}
	if _, ok, err := resolveLeaseClaim("cfdw_scope_b", cfgA); err == nil || ok || !strings.Contains(err.Error(), "different loader endpoint") {
		t.Fatalf("loader A exact loader B claim ok=%t err=%v", ok, err)
	}
	if claim, ok, err := resolveLeaseClaim("scope-b", cfgB); err != nil || !ok || claim.LeaseID != "cfdw_scope_b" {
		t.Fatalf("loader B claim=%#v ok=%t err=%v", claim, ok, err)
	}
}

func TestLoaderClaimScopePreservesEscapedPathSemantics(t *testing.T) {
	escaped := mustLoaderClaimScope(t, testConfig("https://loader.example.test/tenant%2Fone"))
	literal := mustLoaderClaimScope(t, testConfig("https://loader.example.test/tenant/one"))
	if escaped == literal {
		t.Fatalf("escaped scope=%q collides with literal scope=%q", escaped, literal)
	}
	if !strings.Contains(escaped, "/tenant%2Fone") {
		t.Fatalf("escaped scope=%q", escaped)
	}
	lowercaseEscape := mustLoaderClaimScope(t, testConfig("https://loader.example.test/tenant%2fone"))
	if lowercaseEscape != escaped {
		t.Fatalf("escape case scopes differ: lowercase=%q uppercase=%q", lowercaseEscape, escaped)
	}
	escapedTrailingSlash := mustLoaderClaimScope(t, testConfig("https://loader.example.test/tenant%2F"))
	literalWithoutSlash := mustLoaderClaimScope(t, testConfig("https://loader.example.test/tenant"))
	if escapedTrailingSlash == literalWithoutSlash || !strings.Contains(escapedTrailingSlash, "/tenant%2F") {
		t.Fatalf("escaped trailing scope=%q literal scope=%q", escapedTrailingSlash, literalWithoutSlash)
	}
}

func TestLoaderClaimScopeCanonicalizesDefaultPorts(t *testing.T) {
	implicitHTTPS := mustLoaderClaimScope(t, testConfig("https://loader.example.test/api"))
	explicitHTTPS := mustLoaderClaimScope(t, testConfig("https://loader.example.test:443/api"))
	if implicitHTTPS != explicitHTTPS {
		t.Fatalf("https scopes differ: implicit=%q explicit=%q", implicitHTTPS, explicitHTTPS)
	}
	implicitHTTP := mustLoaderClaimScope(t, testConfig("http://127.0.0.1/api"))
	explicitHTTP := mustLoaderClaimScope(t, testConfig("http://127.0.0.1:80/api"))
	if implicitHTTP != explicitHTTP {
		t.Fatalf("http scopes differ: implicit=%q explicit=%q", implicitHTTP, explicitHTTP)
	}
}

func TestLoaderClaimScopeCanonicalizesUnreservedEscapes(t *testing.T) {
	literal := mustLoaderClaimScope(t, testConfig("https://loader.example.test/api/~tenant"))
	escaped := mustLoaderClaimScope(t, testConfig("https://loader.example.test/%61pi/%7etenant"))
	if literal != escaped {
		t.Fatalf("unreserved scopes differ: literal=%q escaped=%q", literal, escaped)
	}
}

func mustLoaderClaimScope(t *testing.T, cfg Config) string {
	t.Helper()
	scope, err := loaderClaimScope(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func TestScopedSlugLookupSkipsWrongProviderExactClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := testConfig("https://loader.example.test")
	if err := claimLease("cfdw_slug_target", "shared-identifier", cfg, t.TempDir(), time.Minute, false, runServer("cfdw_slug_target", "shared-identifier", runStatus{ID: "cfdw_slug_target", Status: "ready"}, nil)); err != nil {
		t.Fatal(err)
	}
	if err := core.ClaimLeaseForRepoProviderScope("shared-identifier", "other", "hetzner", "", t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := resolveLeaseClaim("shared-identifier", cfg)
	if err != nil || !ok || claim.LeaseID != "cfdw_slug_target" {
		t.Fatalf("claim=%#v ok=%t err=%v", claim, ok, err)
	}
}

func TestExactClaimLookupIgnoresUnrelatedCorruptClaim(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	cfg := testConfig("https://loader.example.test")
	if err := claimLease("cfdw_exact", "exact-claim", cfg, t.TempDir(), time.Minute, false, runServer("cfdw_exact", "exact-claim", runStatus{ID: "cfdw_exact", Status: "ready"}, nil)); err != nil {
		t.Fatal(err)
	}
	claimsDir := filepath.Join(stateHome, "crabbox", "claims")
	if err := os.WriteFile(filepath.Join(claimsDir, "unrelated.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := resolveLeaseClaim("cfdw_exact", cfg)
	if err != nil || !ok || claim.LeaseID != "cfdw_exact" {
		t.Fatalf("claim=%#v ok=%t err=%v", claim, ok, err)
	}
}

func TestListRefreshUsesLiveStatusOverLocalClaimState(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/runs/cfdw_live" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(runStatus{
			ID:       "cfdw_live",
			Status:   "failed",
			Metadata: map[string]string{"team": "platform"},
		})
	}))
	defer server.Close()
	backend := newTestBackend(server.URL, &bytes.Buffer{}, &bytes.Buffer{})
	if err := claimLease("cfdw_live", "live-claim", backend.cfg, t.TempDir(), time.Minute, false, runServer("cfdw_live", "live-claim", runStatus{ID: "cfdw_live", Status: "ready"}, map[string]string{"state": "ready"})); err != nil {
		t.Fatal(err)
	}
	views, err := backend.List(context.Background(), ListRequest{Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 {
		t.Fatalf("views=%#v", views)
	}
	if views[0].Status != "failed" || views[0].Labels["state"] != "failed" || views[0].Labels["team"] != "platform" {
		t.Fatalf("view=%#v", views[0])
	}
}

func TestClientRedactsConfiguredTokenFromErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"bad bearer test-token Authorization: Bearer test-token"}`, http.StatusUnauthorized)
	}))
	defer server.Close()
	client, err := newLoaderAPI(testConfig(server.URL), Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Readiness(context.Background())
	if err == nil {
		t.Fatal("expected readiness error")
	}
	if strings.Contains(err.Error(), "test-token") {
		t.Fatalf("token leaked in error: %v", err)
	}
	if !strings.Contains(err.Error(), "<redacted>") {
		t.Fatalf("redaction marker missing: %v", err)
	}
}

func newTestBackend(url string, stdout, stderr *bytes.Buffer) *backend {
	return NewBackend(Provider{}.Spec(), testConfig(url), Runtime{Stdout: stdout, Stderr: stderr}).(*backend)
}

type contractErrorLoader struct {
	cleanupID         string
	cleanupContextErr error
}

func (l *contractErrorLoader) Readiness(context.Context) (readinessResponse, error) {
	return readinessResponse{}, nil
}

func (l *contractErrorLoader) Run(_ context.Context, req runRequest) (runResponse, error) {
	return runResponse{ID: req.ID}, &responseContractError{message: "malformed response"}
}

func (l *contractErrorLoader) Status(context.Context, string) (runStatus, error) {
	return runStatus{}, nil
}

func (l *contractErrorLoader) Delete(context.Context, string) error {
	return nil
}

func (l *contractErrorLoader) DeleteAcknowledgedComplete(ctx context.Context, id string) error {
	l.cleanupID = id
	l.cleanupContextErr = ctx.Err()
	return l.cleanupContextErr
}

func testConfig(url string) Config {
	cfg := Config{}
	cfg.CloudflareDynamicWorkers.LoaderURL = url
	cfg.CloudflareDynamicWorkers.Token = "test-token"
	cfg.CloudflareDynamicWorkers.CacheMode = "stable"
	cfg.CloudflareDynamicWorkers.Egress = "blocked"
	cfg.CloudflareDynamicWorkers.CPUMs = 50
	cfg.CloudflareDynamicWorkers.Subrequests = 12
	cfg.CloudflareDynamicWorkers.TimeoutSecs = 15
	cfg.CloudflareDynamicWorkers.CompatibilityDate = "2026-06-01"
	cfg.CloudflareDynamicWorkers.CompatibilityFlags = []string{"nodejs_compat"}
	cfg.CloudflareDynamicWorkers.Metadata = map[string]string{"team": "platform"}
	cfg.IdleTimeout = time.Minute
	cfg.TTL = 5 * time.Minute
	return cfg
}

func newTestFlagSet() *flag.FlagSet {
	return flag.NewFlagSet("test", flag.ContinueOnError)
}
