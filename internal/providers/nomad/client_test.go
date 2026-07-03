package nomad

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	nomadapi "github.com/hashicorp/nomad/api"
	core "github.com/openclaw/crabbox/internal/cli"
)

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
