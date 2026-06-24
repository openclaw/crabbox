package nomad

import (
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

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
