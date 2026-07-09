package nomad

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	nomadapi "github.com/hashicorp/nomad/api"
	core "github.com/openclaw/crabbox/internal/cli"
)

func TestNomadClientRefusesCrossOriginRedirectBeforeTokenReplay(t *testing.T) {
	const token = "nomad-test-token"
	var sinkRequests atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		sinkRequests.Add(1)
	}))
	defer sink.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Nomad-Token"); got != token {
			t.Errorf("origin token=%q want configured token", got)
		}
		http.Redirect(w, r, sink.URL+"/stolen?location-secret=value", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	t.Setenv("NOMAD_TOKEN", token)
	client, err := newNomadClient(nomadTestConfig(origin.URL), Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.AgentSelf(context.Background())
	if !errors.Is(err, errNomadCrossOriginRedirect) {
		t.Fatalf("error=%v want cross-origin redirect refusal", err)
	}
	if got := sinkRequests.Load(); got != 0 {
		t.Fatalf("redirect sink received %d requests", got)
	}
	for _, leaked := range []string{token, "location-secret", "/stolen"} {
		if strings.Contains(err.Error(), leaked) {
			t.Fatalf("redirect error leaked %q: %v", leaked, err)
		}
	}
}

func TestNomadClientFollowsSameOriginRedirect(t *testing.T) {
	const token = "nomad-test-token"
	var redirected atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/regions":
			http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
		case "/redirected":
			redirected.Store(true)
			if got := r.Header.Get("X-Nomad-Token"); got != token {
				t.Errorf("redirected token=%q want configured token", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`["global"]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	t.Setenv("NOMAD_TOKEN", token)
	client, err := newNomadClient(nomadTestConfig(server.URL), Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	regions, err := client.Regions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !redirected.Load() || len(regions) != 1 || regions[0] != "global" {
		t.Fatalf("redirected=%t regions=%v", redirected.Load(), regions)
	}
}

func TestNomadClientPreservesCallerRedirectPolicy(t *testing.T) {
	wantErr := errors.New("caller stopped redirect")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	source := server.Client()
	source.CheckRedirect = func(*http.Request, []*http.Request) error { return wantErr }

	client, err := newNomadClient(nomadTestConfig(server.URL), Runtime{HTTP: source})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Regions(context.Background())
	if !errors.Is(err, wantErr) {
		t.Fatalf("error=%v want caller redirect policy", err)
	}
}

func TestNomadClientSanitizesRedirectLimit(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hop := requests.Add(1)
		http.Redirect(w, r, fmt.Sprintf("/redirect/%d?limit-secret=value", hop), http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	client, err := newNomadClient(nomadTestConfig(server.URL), Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Regions(context.Background())
	if !errors.Is(err, errNomadRedirectLimit) {
		t.Fatalf("error=%v want redirect limit", err)
	}
	if strings.Contains(err.Error(), "limit-secret") || strings.Contains(err.Error(), "/redirect/") {
		t.Fatalf("redirect limit error leaked Location details: %v", err)
	}
}

func TestNomadClientSanitizesMalformedRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "http://redirect.example.test/%zz?location-secret=value")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	client, err := newNomadClient(nomadTestConfig(server.URL), Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.AgentSelf(context.Background())
	if !errors.Is(err, errNomadInvalidRedirect) {
		t.Fatalf("error=%v want invalid redirect refusal", err)
	}
	if strings.Contains(err.Error(), "location-secret") || strings.Contains(err.Error(), "%zz") {
		t.Fatalf("invalid redirect error leaked Location details: %v", err)
	}
}

func TestNomadClientSanitizesMalformedExecRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "http://redirect.example.test/%zz?location-secret=value")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	client, err := newNomadClient(nomadTestConfig(server.URL), Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.AllocationExec(context.Background(), nomadExecRequest{
		AllocationID: "alloc-test",
		NodeID:       "node-test",
		NodeName:     "node-test",
		JobID:        "job-test",
		Task:         "task-test",
		Command:      []string{"true"},
	})
	if !errors.Is(err, errNomadInvalidRedirect) {
		t.Fatalf("error=%v want invalid redirect refusal", err)
	}
	if strings.Contains(err.Error(), "location-secret") || strings.Contains(err.Error(), "%zz") {
		t.Fatalf("invalid exec redirect error leaked Location details: %v", err)
	}
}

func TestConfigureNomadHTTPClientMatchesSDKTLSDefaults(t *testing.T) {
	cfg := nomadTestConfig("https://nomad.example.test:4646")
	cfg.Nomad.SkipVerify = true
	apiConfig, err := newNomadAPIConfig(cfg, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if err := configureNomadHTTPClient(apiConfig, nil); err != nil {
		t.Fatal(err)
	}
	transport, ok := apiConfig.HttpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport=%T want *http.Transport", apiConfig.HttpClient.Transport)
	}
	if transport.TLSClientConfig == nil || transport.TLSClientConfig.MinVersion != tls.VersionTLS12 || !transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("TLS config=%#v", transport.TLSClientConfig)
	}
	if transport.ForceAttemptHTTP2 {
		t.Fatal("Nomad SDK compatibility requires HTTP/2 disabled")
	}
}

func TestNomadClientGuardsUnixSocketRedirects(t *testing.T) {
	var sinkRequests atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		sinkRequests.Add(1)
	}))
	defer sink.Close()

	socketFile, err := os.CreateTemp("", "crabbox-nomad-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	socketPath := socketFile.Name()
	if err := socketFile.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(socketPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL+"/stolen?location-secret=value", http.StatusTemporaryRedirect)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	address := (&url.URL{Scheme: "unix", Path: socketPath}).String()
	client, err := newNomadClient(nomadTestConfig(address), Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Regions(context.Background())
	if !errors.Is(err, errNomadCrossOriginRedirect) {
		t.Fatalf("error=%v want Unix redirect refusal", err)
	}
	if got := sinkRequests.Load(); got != 0 {
		t.Fatalf("redirect sink received %d Unix requests", got)
	}
}

func TestSameNomadOriginUsesEffectivePorts(t *testing.T) {
	a, _ := url.Parse("https://nomad.example.test")
	b, _ := url.Parse("https://nomad.example.test:443/redirected")
	c, _ := url.Parse("http://nomad.example.test:443/redirected")
	if !sameNomadOrigin(a, b) {
		t.Fatal("default HTTPS port should share origin")
	}
	if sameNomadOrigin(a, c) {
		t.Fatal("scheme change should not share origin")
	}
}

func nomadTestConfig(address string) Config {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.Nomad.Address = address
	return cfg
}

func TestRegisterJobUsesCreateOnlyModifyIndexGuard(t *testing.T) {
	var request nomadapi.JobRegisterRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/v1/jobs" {
			t.Fatalf("request=%s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"EvalID":"eval-create"}`))
	}))
	defer server.Close()

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.Nomad.Address = server.URL
	client, err := newNomadClient(cfg, Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	jobID := "crabbox-create-only"
	evalID, err := client.RegisterJob(context.Background(), &nomadapi.Job{ID: &jobID})
	if err != nil {
		t.Fatal(err)
	}
	if evalID != "eval-create" || !request.EnforceIndex || request.JobModifyIndex != 0 || stringValue(request.Job.ID) != jobID {
		t.Fatalf("evalID=%q request=%#v", evalID, request)
	}
}

func TestNewNomadAPIConfigMapsSafeConfigAndTokenEnv(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.Nomad.Address = "https://nomad.example.test:4646"
	cfg.Nomad.Region = "global"
	cfg.Nomad.Namespace = "team-a"
	cfg.Nomad.TokenEnv = "TEAM_A_NOMAD_TOKEN"
	cfg.Nomad.CACert = "/certs/ca.pem"
	cfg.Nomad.CAPath = "/certs"
	cfg.Nomad.ClientCert = "/certs/client.pem"
	cfg.Nomad.ClientKey = "/certs/client.key"
	cfg.Nomad.TLSServerName = "nomad.example.test"
	cfg.Nomad.SkipVerify = true
	apiConfig, err := newNomadAPIConfig(cfg, func(name string) string {
		if name == "TEAM_A_NOMAD_TOKEN" {
			return "secret-token"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if apiConfig.Address != cfg.Nomad.Address ||
		apiConfig.Region != cfg.Nomad.Region ||
		apiConfig.Namespace != cfg.Nomad.Namespace ||
		apiConfig.SecretID != "secret-token" {
		t.Fatalf("apiConfig=%#v", apiConfig)
	}
	if apiConfig.TLSConfig == nil ||
		apiConfig.TLSConfig.CACert != cfg.Nomad.CACert ||
		apiConfig.TLSConfig.CAPath != cfg.Nomad.CAPath ||
		apiConfig.TLSConfig.ClientCert != cfg.Nomad.ClientCert ||
		apiConfig.TLSConfig.ClientKey != cfg.Nomad.ClientKey ||
		apiConfig.TLSConfig.TLSServerName != cfg.Nomad.TLSServerName ||
		!apiConfig.TLSConfig.Insecure {
		t.Fatalf("tlsConfig=%#v", apiConfig.TLSConfig)
	}
}

func TestLiveClientOptionsCarryContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := liveClient{cfg: Config{
		Nomad: NomadConfig{Region: "global", Namespace: "team-a"},
	}}
	query := client.queryOptions(ctx)
	if query.Region != "global" || query.Namespace != "team-a" {
		t.Fatalf("query options=%#v", query)
	}
	if err := query.Context().Err(); err != context.Canceled {
		t.Fatalf("query context err=%v, want canceled", err)
	}
	write := client.writeOptions(ctx)
	if write.Region != "global" || write.Namespace != "team-a" {
		t.Fatalf("write options=%#v", write)
	}
	if err := write.Context().Err(); err != context.Canceled {
		t.Fatalf("write context err=%v, want canceled", err)
	}
}

func TestNewNomadAPIConfigRequiresAddress(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.Nomad.Address = ""
	if _, err := newNomadAPIConfig(cfg, func(string) string { return "" }); err == nil {
		t.Fatal("expected missing address error")
	}
}

func TestNewNomadAPIConfigAcceptsUnixSocketAddress(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.Nomad.Address = "unix:///var/run/nomad.sock"
	apiConfig, err := newNomadAPIConfig(cfg, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if apiConfig.Address != cfg.Nomad.Address {
		t.Fatalf("apiConfig.Address=%q", apiConfig.Address)
	}
}

func TestValidateConfigRejectsRelativeUnixSocketAddress(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.Nomad.Address = "unix://relative.sock"
	if err := validateConfig(cfg); err == nil {
		t.Fatal("expected relative unix socket address error")
	}
}
