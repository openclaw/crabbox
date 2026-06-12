package cloudflaredynamicworkers

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
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
			OK:                true,
			Runner:            providerName,
			LoaderBinding:     true,
			CompatibilityDate: "2026-06-01",
			Egress:            "blocked",
		})
	}))
	defer server.Close()
	backend := newTestBackend(server.URL, &bytes.Buffer{}, &bytes.Buffer{})
	result, err := backend.Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "pass" || !strings.Contains(result.Message, "loader_binding=true") {
		t.Fatalf("doctor result=%#v", result)
	}
	if strings.Join(seen, ",") != "GET /v1/readiness" {
		t.Fatalf("requests=%v", seen)
	}
}

func TestRunPostsModuleSourceWithStableCacheAndLimits(t *testing.T) {
	var got runRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			Source: "worker.mjs",
			Data:   []byte("export default { fetch() { return new Response('ok') } }\n"),
		},
		ScriptRequested: true,
		Env:             map[string]string{"FEATURE": "on"},
	}
	result, err := backend.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("run error=%v stderr=%q", err, stderr.String())
	}
	if !strings.HasPrefix(got.ID, "cfdw_") || got.CacheMode != "stable" || got.Egress != "blocked" {
		t.Fatalf("request identity/cache=%#v", got)
	}
	if got.Module.Name != "worker.mjs" || got.Module.Source != string(req.Script.Data) {
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

func TestStableRunIDIncludesForwardedEnv(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:1").CloudflareDynamicWorkers
	source := []byte("export default { fetch() { return new Response('ok') } }\n")
	first := stableRunID(source, cfg, map[string]string{"TOKEN": "alpha", "FEATURE": "on"})
	reordered := stableRunID(source, cfg, map[string]string{"FEATURE": "on", "TOKEN": "alpha"})
	second := stableRunID(source, cfg, map[string]string{"TOKEN": "beta", "FEATURE": "on"})
	empty := stableRunID(source, cfg, nil)

	if first != reordered {
		t.Fatalf("stable id should be independent of env map iteration order: %q != %q", first, reordered)
	}
	if first == second {
		t.Fatal("stable id should change when forwarded env values change")
	}
	if first == empty {
		t.Fatal("stable id should distinguish forwarded env from no forwarded env")
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
