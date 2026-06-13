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
	"path/filepath"
	"strings"
	"testing"
	"time"
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
			Status:   "completed",
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
	if _, ok, resolveErr := resolveLeaseClaim(result.Slug); resolveErr != nil || !ok {
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
	claim, ok, resolveErr := resolveLeaseClaim(result.Slug)
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
	claim, ok, resolveErr := resolveLeaseClaim(result.Slug)
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
	claim, ok, resolveErr := resolveLeaseClaim(result.Slug)
	if resolveErr != nil || !ok {
		t.Fatalf("claim slug=%q ok=%t err=%v", result.Slug, ok, resolveErr)
	}
	if claim.Labels["state"] != "failed" || claim.Labels["uncertain"] != "true" || claim.Labels["reason"] != "gateway-error" {
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
	if _, ok, err := resolveLeaseClaim("stale-claim"); err != nil || ok {
		t.Fatalf("claim after stop ok=%t err=%v state_home=%s", ok, err, filepath.Join(stateHome, "crabbox", "claims"))
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
