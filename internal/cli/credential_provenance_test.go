package cli

import (
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepositoryCredentialDestinationsRejectInheritedCredentials(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "proxmox api",
			cfg: Config{
				Provider: "proxmox",
				Proxmox:  ProxmoxConfig{APIURL: "https://repo.example.test", TokenID: "user@pve!token", TokenSecret: "secret"},
				credentialProvenance: credentialDestinationProvenance{
					proxmoxAPIURL:      credentialSourceRepository,
					proxmoxTokenID:     credentialSourceTrustedFile,
					proxmoxTokenSecret: credentialSourceTrustedFile,
				},
			},
			want: "proxmox.apiUrl",
		},
		{
			name: "proxmox tls",
			cfg: Config{
				Provider: "proxmox",
				Proxmox:  ProxmoxConfig{APIURL: "https://trusted.example.test", TokenID: "user@pve!token", TokenSecret: "secret", InsecureTLS: true},
				credentialProvenance: credentialDestinationProvenance{
					proxmoxAPIURL:      credentialSourceTrustedFile,
					proxmoxTokenID:     credentialSourceTrustedFile,
					proxmoxTokenSecret: credentialSourceTrustedFile,
					proxmoxInsecureTLS: credentialSourceRepository,
				},
			},
			want: "proxmox.insecureTLS",
		},
		{
			name: "morph api",
			cfg: Config{
				Provider: "morph",
				Morph:    MorphConfig{APIURL: "https://repo.example.test", APIKey: "secret"},
				credentialProvenance: credentialDestinationProvenance{
					morphAPIURL: credentialSourceRepository,
					morphAPIKey: credentialSourceEnvironment,
				},
			},
			want: "morph.apiUrl",
		},
		{
			name: "morph gateway",
			cfg: Config{
				Provider: "morph",
				Morph:    MorphConfig{APIKey: "secret", SSHGatewayHost: "repo.example.test"},
				credentialProvenance: credentialDestinationProvenance{
					morphAPIKey:         credentialSourceEnvironment,
					morphSSHGatewayHost: credentialSourceRepository,
				},
			},
			want: "morph.sshGatewayHost",
		},
		{
			name: "e2b api",
			cfg: Config{
				Provider: "e2b",
				E2B:      E2BConfig{APIURL: "https://repo.example.test", APIKey: "secret"},
				credentialProvenance: credentialDestinationProvenance{
					e2bAPIURL: credentialSourceRepository,
					e2bAPIKey: credentialSourceEnvironment,
				},
			},
			want: "e2b.apiUrl",
		},
		{
			name: "e2b domain",
			cfg: Config{
				Provider: "e2b",
				E2B:      E2BConfig{Domain: "repo.example.test", APIKey: "secret"},
				credentialProvenance: credentialDestinationProvenance{
					e2bDomain: credentialSourceRepository,
					e2bAPIKey: credentialSourceEnvironment,
				},
			},
			want: "e2b.domain",
		},
		{
			name: "daytona api with environment credential",
			cfg: Config{
				Provider: "daytona",
				Daytona:  DaytonaConfig{APIURL: "https://repo.example.test", APIKey: "secret"},
				credentialProvenance: credentialDestinationProvenance{
					daytonaAPIURL: credentialSourceRepository,
					daytonaAPIKey: credentialSourceEnvironment,
				},
			},
			want: "daytona.apiUrl",
		},
		{
			name: "railway api",
			cfg: Config{
				Provider: "railway",
				Railway:  RailwayConfig{APIURL: "https://repo.example.test", APIToken: "secret"},
				credentialProvenance: credentialDestinationProvenance{
					railwayAPIURL:   credentialSourceRepository,
					railwayAPIToken: credentialSourceEnvironment,
				},
			},
			want: "railway.apiUrl",
		},
		{
			name: "fastapi cloud api",
			cfg: Config{
				Provider:     "fastapi-cloud",
				FastAPICloud: FastAPICloudConfig{APIURL: "https://repo.example.test", Token: "secret"},
				credentialProvenance: credentialDestinationProvenance{
					fastAPICloudAPIURL: credentialSourceRepository,
					fastAPICloudToken:  credentialSourceEnvironment,
				},
			},
			want: "fastapiCloud.apiUrl",
		},
		{
			name: "orgo api",
			cfg: Config{
				Provider: "orgo",
				Orgo:     OrgoConfig{APIBase: "https://repo.example.test", APIKey: "secret"},
				credentialProvenance: credentialDestinationProvenance{
					orgoAPIBase: credentialSourceRepository,
					orgoAPIKey:  credentialSourceEnvironment,
				},
			},
			want: "orgo.apiBase",
		},
		{
			name: "runpod api",
			cfg: Config{
				Provider: "runpod",
				Runpod:   RunpodConfig{APIURL: "https://repo.example.test", APIKey: "secret"},
				credentialProvenance: credentialDestinationProvenance{
					runpodAPIURL: credentialSourceRepository,
					runpodAPIKey: credentialSourceEnvironment,
				},
			},
			want: "runpod.apiUrl",
		},
		{
			name: "vast api",
			cfg: Config{
				Provider: "vast",
				Vast:     VastConfig{APIURL: "https://repo.example.test", APIKey: "secret"},
				credentialProvenance: credentialDestinationProvenance{
					vastAPIURL: credentialSourceRepository,
					vastAPIKey: credentialSourceEnvironment,
				},
			},
			want: "vast.apiUrl",
		},
		{
			name: "islo api",
			cfg: Config{
				Provider: "islo",
				Islo:     IsloConfig{BaseURL: "https://repo.example.test", APIKey: "secret"},
				credentialProvenance: credentialDestinationProvenance{
					isloBaseURL: credentialSourceRepository,
					isloAPIKey:  credentialSourceEnvironment,
				},
			},
			want: "islo.baseUrl",
		},
		{
			name: "tensorlake api",
			cfg: Config{
				Provider:   "tensorlake",
				Tensorlake: TensorlakeConfig{APIURL: "https://repo.example.test", APIKey: "secret"},
				credentialProvenance: credentialDestinationProvenance{
					tensorlakeAPIURL: credentialSourceRepository,
					tensorlakeAPIKey: credentialSourceEnvironment,
				},
			},
			want: "tensorlake.apiUrl",
		},
		{
			name: "upstash box api",
			cfg: Config{
				Provider:   "upstash-box",
				UpstashBox: UpstashBoxConfig{BaseURL: "https://repo.example.test", APIKey: "secret"},
				credentialProvenance: credentialDestinationProvenance{
					upstashBoxBaseURL: credentialSourceRepository,
					upstashBoxAPIKey:  credentialSourceEnvironment,
				},
			},
			want: "upstashBox.baseUrl",
		},
		{
			name: "smolvm api",
			cfg: Config{
				Provider: "smolvm",
				Smolvm:   SmolvmConfig{BaseURL: "https://repo.example.test", APIKey: "secret"},
				credentialProvenance: credentialDestinationProvenance{
					smolvmBaseURL: credentialSourceRepository,
					smolvmAPIKey:  credentialSourceEnvironment,
				},
			},
			want: "smolvm.baseUrl",
		},
		{
			name: "ascii box api",
			cfg: Config{
				Provider: "ascii-box",
				AsciiBox: AsciiBoxConfig{BaseURL: "https://repo.example.test", APIKey: "secret"},
				credentialProvenance: credentialDestinationProvenance{
					asciiBoxBaseURL: credentialSourceRepository,
					asciiBoxAPIKey:  credentialSourceEnvironment,
				},
			},
			want: "asciiBox.baseUrl",
		},
		{
			name: "cloudflare api",
			cfg: Config{
				Provider:   "cloudflare",
				Cloudflare: CloudflareConfig{APIURL: "https://repo.example.test", Token: "secret"},
				credentialProvenance: credentialDestinationProvenance{
					cloudflareAPIURL: credentialSourceRepository,
					cloudflareToken:  credentialSourceEnvironment,
				},
			},
			want: "cloudflare.apiUrl",
		},
		{
			name: "nomad address",
			cfg: Config{
				Provider: "nomad",
				Nomad:    NomadConfig{Address: "https://repo.example.test:4646", TokenEnv: "CRABBOX_TEST_NOMAD_TOKEN"},
				credentialProvenance: credentialDestinationProvenance{
					nomadAddress:  credentialSourceRepository,
					nomadTokenEnv: credentialSourceEnvironment,
				},
			},
			want: "nomad.address or nomad.tokenEnv",
		},
		{
			name: "nomad token env",
			cfg: Config{
				Provider: "nomad",
				Nomad:    NomadConfig{Address: "https://trusted.example.test:4646", TokenEnv: "CRABBOX_TEST_NOMAD_TOKEN"},
				credentialProvenance: credentialDestinationProvenance{
					nomadAddress:  credentialSourceTrustedFile,
					nomadTokenEnv: credentialSourceRepository,
				},
			},
			want: "nomad.address or nomad.tokenEnv",
		},
		{
			name: "semaphore host",
			cfg: Config{
				Provider:  "semaphore",
				Semaphore: SemaphoreConfig{Host: "repo.example.test", Token: "secret"},
				credentialProvenance: credentialDestinationProvenance{
					semaphoreHost:  credentialSourceRepository,
					semaphoreToken: credentialSourceEnvironment,
				},
			},
			want: "semaphore.host",
		},
		{
			name: "sprites api",
			cfg: Config{
				Provider: "sprites",
				Sprites:  SpritesConfig{APIURL: "https://repo.example.test", Token: "secret"},
				credentialProvenance: credentialDestinationProvenance{
					spritesAPIURL: credentialSourceRepository,
					spritesToken:  credentialSourceEnvironment,
				},
			},
			want: "sprites.apiUrl",
		},
	}

	t.Setenv("CRABBOX_TEST_NOMAD_TOKEN", "secret")
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateProviderCredentialDestination(test.cfg)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v want field %s", err, test.want)
			}
		})
	}
}

func TestNativeCredentialDestinationsRejectRepositoryEndpointsAtUse(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		cfg      Config
		want     string
	}{
		{
			name:     "azure dynamic sessions endpoint",
			provider: "azure-dynamic-sessions",
			cfg: Config{
				AzureDynamicSessions: AzureDynamicSessionsConfig{Endpoint: "https://repo.example.test"},
				credentialProvenance: credentialDestinationProvenance{
					azSessionsEndpoint: credentialSourceRepository,
				},
			},
			want: "azureDynamicSessions.endpoint",
		},
		{
			name:     "daytona api",
			provider: "daytona",
			cfg: Config{
				Daytona: DaytonaConfig{APIURL: "https://repo.example.test"},
				credentialProvenance: credentialDestinationProvenance{
					daytonaAPIURL: credentialSourceRepository,
				},
			},
			want: "daytona.apiUrl",
		},
		{
			name:     "daytona gateway",
			provider: "daytona",
			cfg: Config{
				Daytona: DaytonaConfig{SSHGatewayHost: "repo.example.test"},
				credentialProvenance: credentialDestinationProvenance{
					daytonaSSHGateway: credentialSourceRepository,
				},
			},
			want: "daytona.sshGatewayHost",
		},
		{
			name:     "tenki endpoint",
			provider: "tenki",
			cfg: Config{
				Tenki: TenkiConfig{Endpoint: "https://repo.example.test"},
				credentialProvenance: credentialDestinationProvenance{
					tenkiEndpoint: credentialSourceRepository,
				},
			},
			want: "tenki.endpoint",
		},
		{
			name:     "tenki gateway",
			provider: "tenki",
			cfg: Config{
				Tenki: TenkiConfig{Gateway: "wss://repo.example.test"},
				credentialProvenance: credentialDestinationProvenance{
					tenkiGateway: credentialSourceRepository,
				},
			},
			want: "tenki.gateway",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateNativeCredentialDestination(test.cfg, test.provider)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v want field %s", err, test.want)
			}
		})
	}
}

func TestRepositoryCredentialDestinationsAllowSameSourceCredentials(t *testing.T) {
	tests := []Config{
		{
			Provider: "proxmox",
			Proxmox:  ProxmoxConfig{APIURL: "https://repo.example.test", TokenID: "user@pve!token", TokenSecret: "secret", InsecureTLS: true},
			credentialProvenance: credentialDestinationProvenance{
				proxmoxAPIURL:      credentialSourceRepository,
				proxmoxTokenID:     credentialSourceRepository,
				proxmoxTokenSecret: credentialSourceRepository,
				proxmoxInsecureTLS: credentialSourceRepository,
			},
		},
		{
			Provider: "morph",
			Morph:    MorphConfig{APIURL: "https://repo.example.test", APIKey: "secret", SSHGatewayHost: "ssh.repo.example.test"},
			credentialProvenance: credentialDestinationProvenance{
				morphAPIURL:         credentialSourceRepository,
				morphAPIKey:         credentialSourceRepository,
				morphSSHGatewayHost: credentialSourceRepository,
			},
		},
		{
			Provider:   "cloudflare",
			Cloudflare: CloudflareConfig{APIURL: "https://repo.example.test", Token: "secret"},
			credentialProvenance: credentialDestinationProvenance{
				cloudflareAPIURL: credentialSourceRepository,
				cloudflareToken:  credentialSourceRepository,
			},
		},
		{
			Provider:  "semaphore",
			Semaphore: SemaphoreConfig{Host: "repo.example.test", Token: "secret"},
			credentialProvenance: credentialDestinationProvenance{
				semaphoreHost:  credentialSourceRepository,
				semaphoreToken: credentialSourceRepository,
			},
		},
	}

	for _, cfg := range tests {
		if err := validateProviderCredentialDestination(cfg); err != nil {
			t.Fatalf("provider=%s rejected same-source config: %v", cfg.Provider, err)
		}
	}
}

func TestRepositoryCredentialDestinationAllowsExplicitFlagOverride(t *testing.T) {
	cfg := Config{
		Provider: "proxmox",
		Proxmox:  ProxmoxConfig{APIURL: "https://repo.example.test", TokenID: "user@pve!token", TokenSecret: "secret"},
		credentialProvenance: credentialDestinationProvenance{
			proxmoxAPIURL:      credentialSourceRepository,
			proxmoxTokenID:     credentialSourceEnvironment,
			proxmoxTokenSecret: credentialSourceEnvironment,
		},
	}
	fs := newFlagSet("test", io.Discard)
	values := registerProviderFlags(fs, cfg)
	if err := parseFlags(fs, []string{"--proxmox-api-url", "https://approved.example.test"}); err != nil {
		t.Fatal(err)
	}
	if err := applyProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if err := validateProviderCredentialDestination(cfg); err != nil {
		t.Fatalf("explicit flag override rejected: %v", err)
	}
	if cfg.Proxmox.APIURL != "https://approved.example.test" {
		t.Fatalf("proxmox apiUrl=%q", cfg.Proxmox.APIURL)
	}
}

func TestVastCredentialDestinationAllowsExplicitFlagOverride(t *testing.T) {
	cfg := Config{
		Provider: "vast",
		Vast:     VastConfig{APIURL: "https://repo.example.test", APIKey: "secret"},
		credentialProvenance: credentialDestinationProvenance{
			vastAPIURL: credentialSourceRepository,
			vastAPIKey: credentialSourceEnvironment,
		},
	}
	fs := newFlagSet("test", io.Discard)
	values := registerProviderFlags(fs, cfg)
	if err := parseFlags(fs, []string{"--vast-api-url", "https://approved.example.test"}); err != nil {
		t.Fatal(err)
	}
	if err := applyProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if err := validateProviderCredentialDestination(cfg); err != nil {
		t.Fatalf("explicit vast flag override rejected: %v", err)
	}
	if cfg.Vast.APIURL != "https://approved.example.test" {
		t.Fatalf("vast apiUrl=%q", cfg.Vast.APIURL)
	}
}

func TestAzureDynamicSessionsCredentialDestinationAllowsExplicitFlagOverride(t *testing.T) {
	cfg := Config{
		Provider: "azure-dynamic-sessions",
		AzureDynamicSessions: AzureDynamicSessionsConfig{
			Endpoint: "https://repo.pool.eastus.azurecontainerapps.io",
		},
		credentialProvenance: credentialDestinationProvenance{
			azSessionsEndpoint: credentialSourceRepository,
		},
	}
	fs := newFlagSet("test", io.Discard)
	endpoint := fs.String("azure-dynamic-sessions-endpoint", cfg.AzureDynamicSessions.Endpoint, "")
	if err := parseFlags(fs, []string{"--azure-dynamic-sessions-endpoint", "https://approved.pool.eastus.azurecontainerapps.io"}); err != nil {
		t.Fatal(err)
	}
	cfg.AzureDynamicSessions.Endpoint = *endpoint
	markCredentialDestinationFlagSources(&cfg, fs)
	if err := ValidateNativeCredentialDestination(cfg, cfg.Provider); err != nil {
		t.Fatalf("explicit flag override rejected: %v", err)
	}
}

func TestRepositoryCredentialDestinationWithoutCredentialRemainsInspectable(t *testing.T) {
	cfg := Config{
		Provider: "e2b",
		E2B:      E2BConfig{APIURL: "https://repo.example.test", Domain: "repo.example.test"},
		credentialProvenance: credentialDestinationProvenance{
			e2bAPIURL: credentialSourceRepository,
			e2bDomain: credentialSourceRepository,
		},
	}
	if err := validateProviderCredentialDestination(cfg); err != nil {
		t.Fatalf("credential-free repository endpoint rejected: %v", err)
	}
}

func TestRepositoryNomadDestinationWithoutSelectedTokenRemainsInspectable(t *testing.T) {
	t.Setenv("CRABBOX_TEST_EMPTY_NOMAD_TOKEN", "")
	cfg := Config{
		Provider: "nomad",
		Nomad:    NomadConfig{Address: "https://repo.example.test:4646", TokenEnv: "CRABBOX_TEST_EMPTY_NOMAD_TOKEN"},
		credentialProvenance: credentialDestinationProvenance{
			nomadAddress:  credentialSourceRepository,
			nomadTokenEnv: credentialSourceRepository,
		},
	}
	if err := validateProviderCredentialDestination(cfg); err != nil {
		t.Fatalf("credential-free repository Nomad destination rejected: %v", err)
	}
}

func TestRepositoryProviderSettingsRemainApplied(t *testing.T) {
	cfg := baseConfig()
	file := fileConfig{
		Morph: &fileMorphConfig{
			APIURL:         "https://repo.example.test",
			Snapshot:       "snapshot-project",
			SSHGatewayHost: "ssh.repo.example.test",
			WorkRoot:       "/workspace/project",
		},
		Cloudflare: &fileCloudflareConfig{
			APIURL:  "https://runner.repo.example.test",
			Workdir: "/workspace/project",
		},
		Semaphore: &fileSemaphoreConfig{
			Host:        "repo.example.test",
			Project:     "project",
			Machine:     "f1-standard-4",
			OSImage:     "ubuntu2404",
			IdleTimeout: "20m",
		},
	}
	if err := applyFileConfigWithTrust(&cfg, file, false); err != nil {
		t.Fatal(err)
	}
	if cfg.Morph.Snapshot != "snapshot-project" || cfg.Morph.WorkRoot != "/workspace/project" {
		t.Fatalf("morph project settings changed: %#v", cfg.Morph)
	}
	if cfg.Cloudflare.Workdir != "/workspace/project" {
		t.Fatalf("cloudflare workdir=%q", cfg.Cloudflare.Workdir)
	}
	if cfg.Semaphore.Project != "project" || cfg.Semaphore.Machine != "f1-standard-4" || cfg.Semaphore.OSImage != "ubuntu2404" || cfg.Semaphore.IdleTimeout != "20m" {
		t.Fatalf("semaphore project settings changed: %#v", cfg.Semaphore)
	}
}

func TestRepositoryBrokerDestinationRejectsInheritedCredentials(t *testing.T) {
	cfg := Config{
		Coordinator: "https://repo.example.test",
		CoordToken:  "secret",
		credentialProvenance: credentialDestinationProvenance{
			coordinator: credentialSourceRepository,
			coordToken:  credentialSourceTrustedFile,
		},
	}
	if err := validateCoordinatorCredentialDestination(cfg); err == nil {
		t.Fatal("inherited coordinator credential was accepted")
	}
	if _, configured, err := newCoordinatorClient(cfg); err == nil || !configured {
		t.Fatalf("newCoordinatorClient configured=%t error=%v", configured, err)
	}

	cfg.credentialProvenance.coordToken = credentialSourceRepository
	if err := validateCoordinatorCredentialDestination(cfg); err != nil {
		t.Fatalf("same-source coordinator credential rejected: %v", err)
	}

	markCoordinatorDestinationExplicit(&cfg)
	cfg.credentialProvenance.coordToken = credentialSourceEnvironment
	if err := validateCoordinatorCredentialDestination(cfg); err != nil {
		t.Fatalf("explicit coordinator destination rejected: %v", err)
	}
}

func TestLoadBackendRejectsRepositoryCredentialDestination(t *testing.T) {
	cfg := Config{
		Provider: "e2b",
		E2B:      E2BConfig{APIURL: "https://repo.example.test", APIKey: "secret"},
		credentialProvenance: credentialDestinationProvenance{
			e2bAPIURL: credentialSourceRepository,
			e2bAPIKey: credentialSourceEnvironment,
		},
	}
	if _, err := loadBackend(cfg, Runtime{}); err == nil || !strings.Contains(err.Error(), "e2b.apiUrl") {
		t.Fatalf("loadBackend error=%v", err)
	}
}

func TestConfigMergeTracksCredentialDestinationSources(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	cfg.Provider = "e2b"
	if err := applyFileConfigWithTrust(&cfg, fileConfig{
		E2B: &fileE2BConfig{
			APIURL:   "https://repo.example.test",
			Domain:   "repo.example.test",
			Template: "project-template",
			Workdir:  "project-workdir",
		},
	}, false); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_E2B_API_KEY", "secret")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if err := validateProviderCredentialDestination(cfg); err == nil {
		t.Fatal("merged repository endpoint and environment credential were accepted")
	}
	if cfg.E2B.Template != "project-template" || cfg.E2B.Workdir != "project-workdir" {
		t.Fatalf("safe repository settings changed: %#v", cfg.E2B)
	}

	t.Setenv("CRABBOX_E2B_API_URL", "https://approved.example.test")
	t.Setenv("CRABBOX_E2B_DOMAIN", "approved.example.test")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if err := validateProviderCredentialDestination(cfg); err != nil {
		t.Fatalf("explicit environment destinations rejected: %v", err)
	}
}

func TestConfigMergeSourceBindsDirectProviderCredentials(t *testing.T) {
	tests := []struct {
		name          string
		provider      string
		file          fileConfig
		credentialEnv string
		approveEnv    string
	}{
		{
			name:       "azure dynamic sessions",
			provider:   "azure-dynamic-sessions",
			file:       fileConfig{AzureDynamicSessions: &fileAzureDynamicSessionsConfig{Endpoint: "https://repo.example.test"}},
			approveEnv: "CRABBOX_AZURE_DYNAMIC_SESSIONS_ENDPOINT",
		},
		{
			name:          "daytona",
			provider:      "daytona",
			file:          fileConfig{Daytona: &fileDaytonaConfig{APIURL: "https://repo.example.test"}},
			credentialEnv: "CRABBOX_DAYTONA_API_KEY",
			approveEnv:    "CRABBOX_DAYTONA_API_URL",
		},
		{
			name:          "railway",
			provider:      "railway",
			file:          fileConfig{Railway: &fileRailwayConfig{APIURL: "https://repo.example.test"}},
			credentialEnv: "CRABBOX_RAILWAY_API_TOKEN",
			approveEnv:    "CRABBOX_RAILWAY_API_URL",
		},
		{
			name:          "orgo",
			provider:      "orgo",
			file:          fileConfig{Orgo: &fileOrgoConfig{APIBase: "https://repo.example.test"}},
			credentialEnv: "CRABBOX_ORGO_API_KEY",
			approveEnv:    "CRABBOX_ORGO_API_BASE",
		},
		{
			name:          "runpod",
			provider:      "runpod",
			file:          fileConfig{Runpod: &fileRunpodConfig{APIURL: "https://repo.example.test"}},
			credentialEnv: "CRABBOX_RUNPOD_API_KEY",
			approveEnv:    "CRABBOX_RUNPOD_API_URL",
		},
		{
			name:          "vast",
			provider:      "vast",
			file:          fileConfig{Vast: &fileVastConfig{APIURL: "https://repo.example.test"}},
			credentialEnv: "CRABBOX_VAST_API_KEY",
			approveEnv:    "CRABBOX_VAST_API_URL",
		},
		{
			name:          "islo",
			provider:      "islo",
			file:          fileConfig{Islo: &fileIsloConfig{BaseURL: "https://repo.example.test"}},
			credentialEnv: "CRABBOX_ISLO_API_KEY",
			approveEnv:    "CRABBOX_ISLO_BASE_URL",
		},
		{
			name:       "tenki",
			provider:   "tenki",
			file:       fileConfig{Tenki: &fileTenkiConfig{Endpoint: "https://repo.example.test"}},
			approveEnv: "CRABBOX_TENKI_ENDPOINT",
		},
		{
			name:          "tensorlake",
			provider:      "tensorlake",
			file:          fileConfig{Tensorlake: &fileTensorlakeConfig{APIURL: "https://repo.example.test"}},
			credentialEnv: "CRABBOX_TENSORLAKE_API_KEY",
			approveEnv:    "CRABBOX_TENSORLAKE_API_URL",
		},
		{
			name:          "upstash box",
			provider:      "upstash-box",
			file:          fileConfig{UpstashBox: &fileUpstashBoxConfig{BaseURL: "https://repo.example.test"}},
			credentialEnv: "CRABBOX_UPSTASH_BOX_API_KEY",
			approveEnv:    "CRABBOX_UPSTASH_BOX_BASE_URL",
		},
		{
			name:          "smolvm",
			provider:      "smolvm",
			file:          fileConfig{Smolvm: &fileSmolvmConfig{BaseURL: "https://repo.example.test"}},
			credentialEnv: "CRABBOX_SMOLVM_API_KEY",
			approveEnv:    "CRABBOX_SMOLVM_BASE_URL",
		},
		{
			name:          "ascii box",
			provider:      "ascii-box",
			file:          fileConfig{AsciiBox: &fileAsciiBoxConfig{BaseURL: "https://repo.example.test"}},
			credentialEnv: "CRABBOX_ASCII_BOX_API_KEY",
			approveEnv:    "CRABBOX_ASCII_BOX_BASE_URL",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.Provider = test.provider
			if err := applyFileConfigWithTrust(&cfg, test.file, false); err != nil {
				t.Fatal(err)
			}
			if test.credentialEnv != "" {
				t.Setenv(test.credentialEnv, "secret")
			}
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			err := validateProviderCredentialDestination(cfg)
			if err == nil {
				err = ValidateNativeCredentialDestination(cfg, test.provider)
			}
			if err == nil {
				t.Fatal("repository destination with inherited auth was accepted")
			}

			t.Setenv(test.approveEnv, "https://approved.example.test")
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if err := validateProviderCredentialDestination(cfg); err != nil {
				t.Fatalf("explicit environment destination rejected: %v", err)
			}
			if err := ValidateNativeCredentialDestination(cfg, test.provider); err != nil {
				t.Fatalf("explicit environment destination rejected: %v", err)
			}
		})
	}
}

func TestConfigMergeAllowsRepositoryEndpointAndCredentialPair(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "proxmox"
	insecureTLS := true
	if err := applyFileConfigWithTrust(&cfg, fileConfig{
		Proxmox: &fileProxmoxConfig{
			APIURL:      "https://repo.example.test",
			TokenID:     "user@pve!project",
			TokenSecret: "secret",
			InsecureTLS: &insecureTLS,
			Node:        "pve-project",
		},
	}, false); err != nil {
		t.Fatal(err)
	}
	if err := validateProviderCredentialDestination(cfg); err != nil {
		t.Fatalf("repository endpoint and credential pair rejected: %v", err)
	}
	if cfg.Proxmox.Node != "pve-project" {
		t.Fatalf("proxmox node=%q", cfg.Proxmox.Node)
	}
}

func TestRepositorySSHDestinationsRejectInheritedOrAmbientAuthentication(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "static ambient auth",
			cfg: Config{
				Provider: staticProvider,
				Static:   StaticConfig{Host: "repo.example.test"},
				credentialProvenance: credentialDestinationProvenance{
					staticHost: credentialSourceRepository,
				},
			},
			want: "static.host",
		},
		{
			name: "static inherited key",
			cfg: Config{
				Provider: staticProvider,
				Static:   StaticConfig{Host: "repo.example.test"},
				SSHKey:   "/trusted/id_ed25519",
				credentialProvenance: credentialDestinationProvenance{
					staticHost: credentialSourceRepository,
					sshKey:     credentialSourceTrustedFile,
				},
			},
			want: "static.host",
		},
		{
			name: "parallels ambient auth",
			cfg: Config{
				Provider:  "parallels",
				Parallels: ParallelsConfig{Host: "repo.example.test"},
				credentialProvenance: credentialDestinationProvenance{
					parallelsHost: credentialSourceRepository,
				},
			},
			want: "parallels.host",
		},
		{
			name: "parallels inherited key",
			cfg: Config{
				Provider:  "parallels",
				Parallels: ParallelsConfig{Host: "repo.example.test", HostKey: "/trusted/id_ed25519"},
				credentialProvenance: credentialDestinationProvenance{
					parallelsHost:    credentialSourceRepository,
					parallelsHostKey: credentialSourceEnvironment,
				},
			},
			want: "parallels.host",
		},
		{
			name: "parallels fleet ambient auth",
			cfg: Config{
				Provider: "parallels",
				Parallels: ParallelsConfig{Hosts: []ParallelsHostConfig{{
					Host:       "repo.example.test",
					hostSource: credentialSourceRepository,
				}}},
			},
			want: "parallels.host",
		},
		{
			name: "exe dev ambient auth",
			cfg: Config{
				Provider: "exe-dev",
				ExeDev:   ExeDevConfig{ControlHost: "repo.example.test"},
				credentialProvenance: credentialDestinationProvenance{
					exeDevControlHost: credentialSourceRepository,
				},
			},
			want: "exeDev.controlHost",
		},
		{
			name: "external ambient auth",
			cfg: Config{
				Provider: "external",
				External: ExternalConfig{Lifecycle: externalLifecycleConfigForTest(), Connection: ExternalConnectionConfig{SSH: ExternalSSHConnectionConfig{
					Host: "repo.example.test",
				}}},
				credentialProvenance: credentialDestinationProvenance{
					externalSSHHost: credentialSourceRepository,
				},
			},
			want: "external.connection.ssh.host",
		},
		{
			name: "external repository key does not isolate ambient config",
			cfg: Config{
				Provider: "external",
				External: ExternalConfig{Lifecycle: externalLifecycleConfigForTest(), Connection: ExternalConnectionConfig{SSH: ExternalSSHConnectionConfig{
					Host: "repo.example.test",
					Key:  "repo-key",
				}}},
				credentialProvenance: credentialDestinationProvenance{
					externalSSHHost: credentialSourceRepository,
				},
			},
			want: "external.connection.ssh.host",
		},
		{
			name: "external proxy ambient auth",
			cfg: Config{
				Provider: "external",
				External: ExternalConfig{Lifecycle: externalLifecycleConfigForTest(), Connection: ExternalConnectionConfig{SSH: ExternalSSHConnectionConfig{
					ProxyCommand: "repo-proxy %h %p",
				}}},
				credentialProvenance: credentialDestinationProvenance{
					externalSSHProxy: credentialSourceRepository,
				},
			},
			want: "proxyCommand",
		},
		{
			name: "external resource name ambient auth",
			cfg: Config{
				Provider: "external",
				External: ExternalConfig{Lifecycle: externalLifecycleConfigForTest(), Connection: ExternalConnectionConfig{
					ResourceName: "repo.example.test",
				}},
				credentialProvenance: credentialDestinationProvenance{
					externalResource: credentialSourceRepository,
				},
			},
			want: "external.connection.ssh.host",
		},
		{
			name: "external trusted host template with repository resource",
			cfg: Config{
				Provider: "external",
				External: ExternalConfig{Lifecycle: externalLifecycleConfigForTest(), Connection: ExternalConnectionConfig{
					ResourceName: "repo.example.test",
					SSH:          ExternalSSHConnectionConfig{Host: "{{resourceName}}"},
				}},
				credentialProvenance: credentialDestinationProvenance{
					externalResource: credentialSourceRepository,
					externalSSHHost:  credentialSourceTrustedFile,
				},
			},
			want: "external.connection.ssh.host",
		},
		{
			name: "external trusted proxy template with repository resource",
			cfg: Config{
				Provider: "external",
				External: ExternalConfig{Lifecycle: externalLifecycleConfigForTest(), Connection: ExternalConnectionConfig{
					ResourceName: "repo.example.test",
					SSH:          ExternalSSHConnectionConfig{ProxyCommand: "proxy {{resourceName}}"},
				}},
				credentialProvenance: credentialDestinationProvenance{
					externalResource: credentialSourceRepository,
					externalSSHProxy: credentialSourceTrustedFile,
				},
			},
			want: "proxyCommand",
		},
		{
			name: "external trusted host template with repository config",
			cfg: Config{
				Provider: "external",
				External: ExternalConfig{Lifecycle: externalLifecycleConfigForTest(), Connection: ExternalConnectionConfig{SSH: ExternalSSHConnectionConfig{
					Host: "{{config.host}}",
				}}},
				credentialProvenance: credentialDestinationProvenance{
					externalConfig:  credentialSourceRepository,
					externalSSHHost: credentialSourceTrustedFile,
				},
			},
			want: "external.connection.ssh.host",
		},
		{
			name: "external trusted proxy template with repository input",
			cfg: Config{
				Provider: "external",
				External: ExternalConfig{Lifecycle: externalLifecycleConfigForTest(), Connection: ExternalConnectionConfig{SSH: ExternalSSHConnectionConfig{
					ProxyCommand: "proxy {{repo.remoteUrl}}",
				}}},
				credentialProvenance: credentialDestinationProvenance{
					externalSSHProxy: credentialSourceTrustedFile,
				},
			},
			want: "proxyCommand",
		},
		{
			name: "external repository environment opt-in",
			cfg: Config{
				Provider: "external",
				External: ExternalConfig{Lifecycle: externalLifecycleConfigForTest(), Connection: ExternalConnectionConfig{SSH: ExternalSSHConnectionConfig{
					AllowEnv: true,
				}}},
				credentialProvenance: credentialDestinationProvenance{
					externalSSHAllowEnv: credentialSourceRepository,
				},
			},
			want: "external.connection.ssh.allowEnv",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateProviderCredentialDestination(test.cfg)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v want field %s", err, test.want)
			}
		})
	}
}

func TestRepositorySSHDestinationsAllowSameSourceKeys(t *testing.T) {
	repositoryRoot, key := repositorySSHKeyFixture(t)
	tests := []Config{
		{
			Provider: staticProvider,
			Static:   StaticConfig{Host: "repo.example.test"},
			SSHKey:   key,
			credentialProvenance: credentialDestinationProvenance{
				staticHost:     credentialSourceRepository,
				sshKey:         credentialSourceRepository,
				repositoryRoot: repositoryRoot,
			},
		},
		{
			Provider:  "parallels",
			Parallels: ParallelsConfig{Host: "repo.example.test", HostKey: key},
			credentialProvenance: credentialDestinationProvenance{
				parallelsHost:    credentialSourceRepository,
				parallelsHostKey: credentialSourceRepository,
				repositoryRoot:   repositoryRoot,
			},
		},
		{
			Provider: "parallels",
			Parallels: ParallelsConfig{Hosts: []ParallelsHostConfig{{
				Host:       "repo.example.test",
				Key:        key,
				hostSource: credentialSourceRepository,
				keySource:  credentialSourceRepository,
			}}},
			credentialProvenance: credentialDestinationProvenance{repositoryRoot: repositoryRoot},
		},
	}

	for _, cfg := range tests {
		if err := validateProviderCredentialDestination(cfg); err != nil {
			t.Fatalf("provider=%s rejected same-source SSH config: %v", cfg.Provider, err)
		}
	}
}

func TestExternalCommandModeIgnoresUnusedConnectionProvenance(t *testing.T) {
	cfg := Config{
		Provider: "external",
		External: ExternalConfig{
			Command: "provider-adapter",
			Connection: ExternalConnectionConfig{SSH: ExternalSSHConnectionConfig{
				Host: "unused.example.test",
			}},
		},
		credentialProvenance: credentialDestinationProvenance{externalSSHHost: credentialSourceRepository},
	}
	if err := validateProviderCredentialDestination(cfg); err != nil {
		t.Fatalf("unused command-mode connection rejected: %v", err)
	}
}

func TestCubeSandboxRejectsRepositoryDataPlaneDestinationsWithoutAPIKey(t *testing.T) {
	tests := []struct {
		name string
		file fileCubeSandboxConfig
		want string
	}{
		{name: "API URL", file: fileCubeSandboxConfig{APIURL: "https://attacker.example.test"}, want: "cubeSandbox.apiUrl"},
		{name: "domain", file: fileCubeSandboxConfig{Domain: "attacker.example.test"}, want: "cubeSandbox.domain"},
		{name: "proxy node", file: fileCubeSandboxConfig{ProxyNodeIP: "attacker.example.test"}, want: "cubeSandbox.proxyNodeIp"},
		{name: "proxy port", file: fileCubeSandboxConfig{ProxyPortHTTP: 8080}, want: "cubeSandbox.proxyPortHttp"},
		{name: "proxy scheme", file: fileCubeSandboxConfig{ProxyScheme: "https"}, want: "cubeSandbox.proxyScheme"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Provider = "cubesandbox"
			cfg.CubeSandbox.APIKey = ""
			if err := applyFileConfigWithTrust(&cfg, fileConfig{CubeSandbox: &tt.file}, false); err != nil {
				t.Fatal(err)
			}
			err := validateProviderCredentialDestination(cfg)
			if err == nil || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), "ephemeral credentials and workspace data") {
				t.Fatalf("err=%v, want repository destination rejection for %s", err, tt.want)
			}
		})
	}
}

func TestCubeSandboxEnvironmentApprovesRepositoryDataPlaneRoute(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "cubesandbox"
	file := fileCubeSandboxConfig{
		APIURL:        "https://repo-api.example.test",
		Domain:        "repo.example.test",
		ProxyNodeIP:   "repo-proxy.example.test",
		ProxyPortHTTP: 8080,
		ProxyScheme:   "http",
	}
	if err := applyFileConfigWithTrust(&cfg, fileConfig{CubeSandbox: &file}, false); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CUBESANDBOX_DOMAIN", "approved.example.test")
	t.Setenv("CRABBOX_CUBESANDBOX_API_URL", "https://approved-api.example.test")
	t.Setenv("CRABBOX_CUBESANDBOX_PROXY_NODE_IP", "approved-proxy.example.test")
	t.Setenv("CRABBOX_CUBESANDBOX_PROXY_PORT_HTTP", "8443")
	t.Setenv("CRABBOX_CUBESANDBOX_PROXY_SCHEME", "https")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if err := validateProviderCredentialDestination(cfg); err != nil {
		t.Fatalf("environment-approved data-plane route rejected: %v", err)
	}
}

func TestCubeSandboxFlagsApproveRepositoryDataPlaneRoute(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "cubesandbox"
	file := fileCubeSandboxConfig{
		APIURL:        "https://repo-api.example.test",
		Domain:        "repo.example.test",
		ProxyNodeIP:   "repo-proxy.example.test",
		ProxyPortHTTP: 8080,
		ProxyScheme:   "http",
	}
	if err := applyFileConfigWithTrust(&cfg, fileConfig{CubeSandbox: &file}, false); err != nil {
		t.Fatal(err)
	}
	fs := newFlagSet("test", io.Discard)
	apiURL := fs.String("cubesandbox-api-url", cfg.CubeSandbox.APIURL, "")
	domain := fs.String("cubesandbox-domain", cfg.CubeSandbox.Domain, "")
	proxyNode := fs.String("cubesandbox-proxy-node-ip", cfg.CubeSandbox.ProxyNodeIP, "")
	proxyPort := fs.Int("cubesandbox-proxy-port-http", cfg.CubeSandbox.ProxyPortHTTP, "")
	proxyScheme := fs.String("cubesandbox-proxy-scheme", cfg.CubeSandbox.ProxyScheme, "")
	args := []string{
		"--cubesandbox-api-url", "https://approved-api.example.test",
		"--cubesandbox-domain", "approved.example.test",
		"--cubesandbox-proxy-node-ip", "approved-proxy.example.test",
		"--cubesandbox-proxy-port-http", "8443",
		"--cubesandbox-proxy-scheme", "https",
	}
	if err := parseFlags(fs, args); err != nil {
		t.Fatal(err)
	}
	cfg.CubeSandbox.APIURL = *apiURL
	cfg.CubeSandbox.Domain = *domain
	cfg.CubeSandbox.ProxyNodeIP = *proxyNode
	cfg.CubeSandbox.ProxyPortHTTP = *proxyPort
	cfg.CubeSandbox.ProxyScheme = *proxyScheme
	markCredentialDestinationFlagSources(&cfg, fs)
	if err := validateProviderCredentialDestination(cfg); err != nil {
		t.Fatalf("flag-approved data-plane route rejected: %v", err)
	}
}

func externalLifecycleConfigForTest() ExternalLifecycleConfig {
	return ExternalLifecycleConfig{Acquire: ExternalLifecycleOperation{Argv: []string{"provider-adapter"}}}
}

func TestExternalRoutingFileCarriesCredentialSource(t *testing.T) {
	base := Config{
		Provider: "external",
		External: ExternalConfig{
			Lifecycle: ExternalLifecycleConfig{Acquire: ExternalLifecycleOperation{Argv: []string{"provider-adapter"}}},
			Connection: ExternalConnectionConfig{SSH: ExternalSSHConnectionConfig{
				Host: "routed.example.test",
			}},
		},
	}

	repository := base
	repository.credentialProvenance.externalRouting = credentialSourceRepository
	MarkExternalRoutingCredentialSources(&repository)
	if err := validateProviderCredentialDestination(repository); err == nil {
		t.Fatal("repository routing file with ambient auth was accepted")
	}

	trusted := base
	MarkExternalRoutingCredentialSources(&trusted)
	if err := validateProviderCredentialDestination(trusted); err != nil {
		t.Fatalf("claim-bound routing state rejected: %v", err)
	}
}

func TestExternalRoutingLoaderPreservesConfiguredSource(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	external := ExternalConfig{
		Lifecycle: externalLifecycleConfigForTest(),
		Connection: ExternalConnectionConfig{SSH: ExternalSSHConnectionConfig{
			Host: "routed.example.test",
		}},
	}
	path, err := PersistExternalRouting("cbx_abcdef123456", external)
	if err != nil {
		t.Fatal(err)
	}

	repository := Config{
		Provider: "external",
		External: ExternalConfig{RoutingFile: path},
		credentialProvenance: credentialDestinationProvenance{
			externalRouting: credentialSourceRepository,
		},
	}
	if err := loadExternalRoutingConfig(&repository, path, false); err != nil {
		t.Fatal(err)
	}
	if err := validateProviderCredentialDestination(repository); err == nil {
		t.Fatal("repository-configured routing state with ambient auth was accepted")
	}

	legacyClaim := Config{Provider: "external"}
	if err := loadExternalRoutingConfig(&legacyClaim, path, true); err != nil {
		t.Fatal(err)
	}
	if err := validateProviderCredentialDestination(legacyClaim); err == nil {
		t.Fatal("legacy claim-bound routing state inherited credential trust")
	}

	approvedLegacy := Config{Provider: "external"}
	approvedConnection := external.Connection
	if err := applyFileConfigWithTrust(&approvedLegacy, fileConfig{External: &fileExternalConfig{
		Connection: &approvedConnection,
	}}, true); err != nil {
		t.Fatal(err)
	}
	if err := loadExternalRoutingConfig(&approvedLegacy, path, true); err != nil {
		t.Fatal(err)
	}
	if err := validateProviderCredentialDestination(approvedLegacy); err != nil {
		t.Fatalf("legacy routing with exact current approval rejected: %v", err)
	}

	validatedPath, err := PersistValidatedExternalRouting("cbx_abcdef123457", external)
	if err != nil {
		t.Fatal(err)
	}
	validatedClaim := Config{Provider: "external"}
	if err := loadExternalRoutingConfig(&validatedClaim, validatedPath, true); err != nil {
		t.Fatal(err)
	}
	if err := validateProviderCredentialDestination(validatedClaim); err != nil {
		t.Fatalf("validated claim-bound routing state rejected: %v", err)
	}
}

func TestExternalRoutingCredentialVersionProtectsConfigArgv(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	lifecycle := ExternalLifecycleConfig{
		Acquire: ExternalLifecycleOperation{
			Argv:            []string{"adapter", "create", "{{config.region}}"},
			AllowConfigArgv: true,
		},
	}
	external := ExternalConfig{
		Config:    map[string]any{"region": "test-region"},
		Lifecycle: lifecycle,
	}
	legacyPath, err := persistExternalRouting("cbx_abcdef123456", external, externalRoutingCredentialVersion-1)
	if err != nil {
		t.Fatal(err)
	}

	legacyClaim := Config{Provider: "external"}
	if err := loadExternalRoutingConfig(&legacyClaim, legacyPath, true); err != nil {
		t.Fatal(err)
	}
	if err := validateProviderCredentialDestination(legacyClaim); err == nil || !strings.Contains(err.Error(), "allowConfigArgv") {
		t.Fatalf("legacy routing config argv error=%v", err)
	}

	approvedLegacy := Config{Provider: "external"}
	if err := applyFileConfigWithTrust(&approvedLegacy, fileConfig{External: &fileExternalConfig{
		Config:    external.Config,
		Lifecycle: &lifecycle,
	}}, true); err != nil {
		t.Fatal(err)
	}
	if err := loadExternalRoutingConfig(&approvedLegacy, legacyPath, true); err != nil {
		t.Fatal(err)
	}
	if err := validateProviderCredentialDestination(approvedLegacy); err != nil {
		t.Fatalf("legacy routing with exact current approval rejected: %v", err)
	}

	validatedPath, err := PersistValidatedExternalRouting("cbx_abcdef123457", external)
	if err != nil {
		t.Fatal(err)
	}
	repositorySelected := Config{
		Provider: "external",
		credentialProvenance: credentialDestinationProvenance{
			externalRouting: credentialSourceRepository,
		},
	}
	if err := loadExternalRoutingConfig(&repositorySelected, validatedPath, false); err != nil {
		t.Fatal(err)
	}
	if err := validateProviderCredentialDestination(repositorySelected); err == nil || !strings.Contains(err.Error(), "allowConfigArgv") {
		t.Fatalf("repository-selected current routing config argv error=%v", err)
	}

	validatedClaim := Config{Provider: "external"}
	if err := loadExternalRoutingConfig(&validatedClaim, validatedPath, true); err != nil {
		t.Fatal(err)
	}
	if err := validateProviderCredentialDestination(validatedClaim); err != nil {
		t.Fatalf("current validated routing state rejected: %v", err)
	}
}

func TestExternalRoutingAllowEnvApprovalBindsResourceName(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	approved := ExternalConnectionConfig{
		ResourceName: "approved-resource",
		SSH: ExternalSSHConnectionConfig{
			User:     "{{resourceName}}",
			Host:     "approved.example.test",
			AllowEnv: true,
		},
	}
	cfg := Config{Provider: "external"}
	if err := applyFileConfigWithTrust(&cfg, fileConfig{External: &fileExternalConfig{Connection: &approved}}, true); err != nil {
		t.Fatal(err)
	}

	routed := approved
	routed.ResourceName = "{{env.GITHUB_TOKEN}}"
	routed.AllowEnvResourceName = true
	path, err := PersistExternalRouting("cbx_abcdef123456", ExternalConfig{
		Lifecycle:  externalLifecycleConfigForTest(),
		Connection: routed,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := loadExternalRoutingConfig(&cfg, path, true); err != nil {
		t.Fatal(err)
	}
	if err := validateProviderCredentialDestination(cfg); err == nil || !strings.Contains(err.Error(), "ssh.allowEnv") {
		t.Fatalf("repository routing resource-name change error=%v", err)
	}
}

func TestExternalLifecycleConfigArgvApproval(t *testing.T) {
	trustedConfig := fileExternalConfig{Config: map[string]any{"token": "trusted-token"}}
	applyTrustedConfig := func(t *testing.T, cfg *Config) {
		t.Helper()
		if err := applyFileConfigWithTrust(cfg, fileConfig{External: &trustedConfig}, true); err != nil {
			t.Fatal(err)
		}
	}
	applyRepositoryLifecycle := func(t *testing.T, cfg *Config, lifecycle ExternalLifecycleConfig, connection *ExternalConnectionConfig) {
		t.Helper()
		if err := applyFileConfigWithTrust(cfg, fileConfig{External: &fileExternalConfig{
			Lifecycle:  &lifecycle,
			Connection: connection,
		}}, false); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("rejects inherited config in repository argv", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Provider = "external"
		applyTrustedConfig(t, &cfg)
		applyRepositoryLifecycle(t, &cfg, ExternalLifecycleConfig{
			Acquire: ExternalLifecycleOperation{Argv: []string{"adapter", "--token", "{{config.token}}"}},
		}, nil)

		err := validateProviderCredentialDestination(cfg)
		if err == nil || !strings.Contains(err.Error(), "external.lifecycle.acquire.argv") {
			t.Fatalf("error=%v", err)
		}
		if strings.Contains(err.Error(), "trusted-token") {
			t.Fatalf("error exposed config value: %v", err)
		}
	})

	t.Run("rejects inherited config in repository steps", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Provider = "external"
		applyTrustedConfig(t, &cfg)
		applyRepositoryLifecycle(t, &cfg, ExternalLifecycleConfig{
			Acquire: ExternalLifecycleOperation{Steps: [][]string{{"adapter", "prepare"}, {"adapter", "use", "{{config.token}}"}}},
		}, nil)

		err := validateProviderCredentialDestination(cfg)
		if err == nil || !strings.Contains(err.Error(), "external.lifecycle.acquire.steps") {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("rejects inherited config through repository resource name", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Provider = "external"
		applyTrustedConfig(t, &cfg)
		connection := &ExternalConnectionConfig{ResourceName: "{{config.token}}"}
		applyRepositoryLifecycle(t, &cfg, ExternalLifecycleConfig{
			Acquire: ExternalLifecycleOperation{Argv: []string{"adapter", "create", "{{resourceName}}"}},
		}, connection)

		err := validateProviderCredentialDestination(cfg)
		if err == nil || !strings.Contains(err.Error(), "external.lifecycle.acquire.argv") {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("rejects repository resource template feeding trusted lifecycle", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Provider = "external"
		lifecycle := ExternalLifecycleConfig{
			Acquire: ExternalLifecycleOperation{Argv: []string{"adapter", "create", "{{resourceName}}"}},
		}
		if err := applyFileConfigWithTrust(&cfg, fileConfig{External: &fileExternalConfig{
			Config:    map[string]any{"token": "trusted-token"},
			Lifecycle: &lifecycle,
		}}, true); err != nil {
			t.Fatal(err)
		}
		connection := ExternalConnectionConfig{ResourceName: "{{config.token}}"}
		if err := applyFileConfigWithTrust(&cfg, fileConfig{External: &fileExternalConfig{Connection: &connection}}, false); err != nil {
			t.Fatal(err)
		}

		err := validateProviderCredentialDestination(cfg)
		if err == nil || !strings.Contains(err.Error(), "external.connection template feeding external.lifecycle.acquire.argv") {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("rejects repository cloud id template feeding trusted lifecycle", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Provider = "external"
		lifecycle := ExternalLifecycleConfig{
			Acquire: ExternalLifecycleOperation{Argv: []string{"adapter", "create"}},
			Release: ExternalLifecycleOperation{Argv: []string{"adapter", "release", "{{cloudId}}"}},
		}
		if err := applyFileConfigWithTrust(&cfg, fileConfig{External: &fileExternalConfig{
			Config:    map[string]any{"token": "trusted-token"},
			Lifecycle: &lifecycle,
		}}, true); err != nil {
			t.Fatal(err)
		}
		connection := ExternalConnectionConfig{CloudID: "{{config.token}}"}
		if err := applyFileConfigWithTrust(&cfg, fileConfig{External: &fileExternalConfig{Connection: &connection}}, false); err != nil {
			t.Fatal(err)
		}

		err := validateProviderCredentialDestination(cfg)
		if err == nil || !strings.Contains(err.Error(), "external.connection template feeding external.lifecycle.release.argv") {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("allows repository-owned config", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Provider = "external"
		lifecycle := ExternalLifecycleConfig{
			Acquire: ExternalLifecycleOperation{Argv: []string{"adapter", "--region", "{{config.region}}"}},
		}
		if err := applyFileConfigWithTrust(&cfg, fileConfig{External: &fileExternalConfig{
			Config:    map[string]any{"region": "test-region"},
			Lifecycle: &lifecycle,
		}}, false); err != nil {
			t.Fatal(err)
		}
		if err := validateProviderCredentialDestination(cfg); err != nil {
			t.Fatalf("repository-owned config rejected: %v", err)
		}
	})

	t.Run("allows trusted lifecycle", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Provider = "external"
		lifecycle := ExternalLifecycleConfig{
			Acquire: ExternalLifecycleOperation{Argv: []string{"adapter", "--region", "{{config.region}}"}},
		}
		if err := applyFileConfigWithTrust(&cfg, fileConfig{External: &fileExternalConfig{
			Config:    map[string]any{"region": "test-region"},
			Lifecycle: &lifecycle,
		}}, true); err != nil {
			t.Fatal(err)
		}
		if err := validateProviderCredentialDestination(cfg); err != nil {
			t.Fatalf("trusted lifecycle rejected: %v", err)
		}
	})

	t.Run("allows exact trusted non-secret contract", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Provider = "external"
		lifecycle := ExternalLifecycleConfig{
			Acquire: ExternalLifecycleOperation{
				Argv:            []string{"adapter", "--region", "{{config.region}}"},
				AllowConfigArgv: true,
			},
		}
		if err := applyFileConfigWithTrust(&cfg, fileConfig{External: &fileExternalConfig{
			Config:    map[string]any{"region": "test-region"},
			Lifecycle: &lifecycle,
		}}, true); err != nil {
			t.Fatal(err)
		}
		applyRepositoryLifecycle(t, &cfg, lifecycle, nil)
		if err := validateProviderCredentialDestination(cfg); err != nil {
			t.Fatalf("exact trusted lifecycle contract rejected: %v", err)
		}
	})

	t.Run("repository cannot self approve", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Provider = "external"
		applyTrustedConfig(t, &cfg)
		applyRepositoryLifecycle(t, &cfg, ExternalLifecycleConfig{
			Acquire: ExternalLifecycleOperation{
				Argv:            []string{"adapter", "{{config.token}}"},
				AllowConfigArgv: true,
			},
		}, nil)

		err := validateProviderCredentialDestination(cfg)
		if err == nil || !strings.Contains(err.Error(), "same lifecycle contract in trusted user config") {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("trusted approval binds connection templates", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Provider = "external"
		lifecycle := ExternalLifecycleConfig{
			Acquire: ExternalLifecycleOperation{
				Argv:            []string{"adapter", "create", "{{resourceName}}"},
				AllowConfigArgv: true,
			},
		}
		trustedConnection := ExternalConnectionConfig{ResourceName: "{{config.region}}"}
		if err := applyFileConfigWithTrust(&cfg, fileConfig{External: &fileExternalConfig{
			Config:     map[string]any{"region": "test-region", "token": "trusted-token"},
			Lifecycle:  &lifecycle,
			Connection: &trustedConnection,
		}}, true); err != nil {
			t.Fatal(err)
		}
		repositoryConnection := ExternalConnectionConfig{ResourceName: "{{config.token}}"}
		applyRepositoryLifecycle(t, &cfg, lifecycle, &repositoryConnection)

		if err := validateProviderCredentialDestination(cfg); err == nil {
			t.Fatal("mutated connection template inherited trusted config")
		}
	})

	t.Run("trusted approval is exact and operation bound", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Provider = "external"
		trusted := ExternalLifecycleConfig{
			Acquire: ExternalLifecycleOperation{
				Argv:            []string{"approved-adapter", "{{config.token}}"},
				AllowConfigArgv: true,
			},
		}
		if err := applyFileConfigWithTrust(&cfg, fileConfig{External: &fileExternalConfig{
			Config:    map[string]any{"token": "trusted-token"},
			Lifecycle: &trusted,
		}}, true); err != nil {
			t.Fatal(err)
		}

		changed := trusted
		changed.Acquire.Argv = []string{"repository-adapter", "{{config.token}}"}
		applyRepositoryLifecycle(t, &cfg, changed, nil)
		if err := validateProviderCredentialDestination(cfg); err == nil {
			t.Fatal("mutated lifecycle inherited trusted config")
		}

		wrongOperation := ExternalLifecycleConfig{
			Acquire: ExternalLifecycleOperation{Argv: []string{"approved-adapter", "{{config.token}}"}},
			Release: ExternalLifecycleOperation{Argv: []string{"approved-adapter", "release"}, AllowConfigArgv: true},
		}
		if err := applyFileConfigWithTrust(&cfg, fileConfig{External: &fileExternalConfig{Lifecycle: &wrongOperation}}, true); err != nil {
			t.Fatal(err)
		}
		applyRepositoryLifecycle(t, &cfg, wrongOperation, nil)
		if err := validateProviderCredentialDestination(cfg); err == nil {
			t.Fatal("approval on another operation authorized config argv")
		}
	})

	t.Run("checks every config argv operation", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Provider = "external"
		lifecycle := ExternalLifecycleConfig{
			Acquire: ExternalLifecycleOperation{
				Argv:            []string{"adapter", "create", "{{config.region}}"},
				AllowConfigArgv: true,
			},
			Release: ExternalLifecycleOperation{
				Argv: []string{"adapter", "release", "{{config.token}}"},
			},
		}
		if err := applyFileConfigWithTrust(&cfg, fileConfig{External: &fileExternalConfig{
			Config:    map[string]any{"region": "test-region", "token": "trusted-token"},
			Lifecycle: &lifecycle,
		}}, true); err != nil {
			t.Fatal(err)
		}
		applyRepositoryLifecycle(t, &cfg, lifecycle, nil)

		err := validateProviderCredentialDestination(cfg)
		if err == nil || !strings.Contains(err.Error(), "external.lifecycle.release.argv") {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("allows inherited config in lifecycle env", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Provider = "external"
		applyTrustedConfig(t, &cfg)
		applyRepositoryLifecycle(t, &cfg, ExternalLifecycleConfig{
			Acquire: ExternalLifecycleOperation{
				Argv: []string{"adapter", "create"},
				Env:  map[string]string{"ADAPTER_TOKEN": "{{config.token}}"},
			},
		}, nil)
		if err := validateProviderCredentialDestination(cfg); err != nil {
			t.Fatalf("lifecycle env rejected: %v", err)
		}
	})
}

func TestRepositorySSHDestinationsRejectExternalSameSourceKeyPaths(t *testing.T) {
	repositoryRoot, key := repositorySSHKeyFixture(t)
	externalRoot := t.TempDir()
	externalKey := filepath.Join(externalRoot, "external-key")
	if err := os.WriteFile(externalKey, []byte("external-test-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	escapingKey, err := filepath.Rel(repositoryRoot, externalKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(externalKey, filepath.Join(repositoryRoot, "outward-link")); err != nil {
		t.Fatal(err)
	}

	for _, candidate := range []string{
		filepath.Join(repositoryRoot, key),
		escapingKey,
		"outward-link",
		"missing-key",
	} {
		cfg := Config{
			Provider: staticProvider,
			Static:   StaticConfig{Host: "repo.example.test"},
			SSHKey:   candidate,
			credentialProvenance: credentialDestinationProvenance{
				staticHost:     credentialSourceRepository,
				sshKey:         credentialSourceRepository,
				repositoryRoot: repositoryRoot,
			},
		}
		if err := validateProviderCredentialDestination(cfg); err == nil {
			t.Fatalf("unsafe repository key path %q was accepted", candidate)
		}
	}
}

func repositorySSHKeyFixture(t *testing.T) (string, string) {
	t.Helper()
	repositoryRoot := t.TempDir()
	key := "repo-test-key"
	if err := os.WriteFile(filepath.Join(repositoryRoot, key), []byte("repository-test-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	return repositoryRoot, key
}

func TestConfigMergeTracksSSHDestinationSources(t *testing.T) {
	clearConfigEnv(t)

	t.Run("static inherited key rejected then environment host approved", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Provider = staticProvider
		if err := applyFileConfigWithTrust(&cfg, fileConfig{SSH: &fileSSHConfig{Key: "/trusted/id_ed25519"}}, true); err != nil {
			t.Fatal(err)
		}
		if err := applyFileConfigWithTrust(&cfg, fileConfig{Static: &fileStaticConfig{Host: "repo.example.test"}}, false); err != nil {
			t.Fatal(err)
		}
		if err := validateProviderCredentialDestination(cfg); err == nil {
			t.Fatal("repository static host with inherited key was accepted")
		}

		t.Setenv("CRABBOX_STATIC_HOST", "approved.example.test")
		if err := applyEnv(&cfg); err != nil {
			t.Fatal(err)
		}
		if err := validateProviderCredentialDestination(cfg); err != nil {
			t.Fatalf("explicit static host rejected: %v", err)
		}
	})

	t.Run("same repository static host and key allowed", func(t *testing.T) {
		repositoryRoot, key := repositorySSHKeyFixture(t)
		cfg := baseConfig()
		cfg.Provider = staticProvider
		cfg.credentialProvenance.repositoryRoot = repositoryRoot
		if err := applyFileConfigWithTrust(&cfg, fileConfig{
			Static: &fileStaticConfig{Host: "repo.example.test"},
			SSH:    &fileSSHConfig{Key: key},
		}, false); err != nil {
			t.Fatal(err)
		}
		if err := validateProviderCredentialDestination(cfg); err != nil {
			t.Fatalf("same-source static config rejected: %v", err)
		}
	})

	t.Run("parallels fleet source follows each host", func(t *testing.T) {
		repositoryRoot, key := repositorySSHKeyFixture(t)
		cfg := baseConfig()
		cfg.Provider = "parallels"
		cfg.credentialProvenance.repositoryRoot = repositoryRoot
		if err := applyFileConfigWithTrust(&cfg, fileConfig{Parallels: &fileParallelsConfig{
			Hosts: []fileParallelsHostConfig{{Host: "repo.example.test", Key: key}},
		}}, false); err != nil {
			t.Fatal(err)
		}
		if err := validateProviderCredentialDestination(cfg); err != nil {
			t.Fatalf("same-source fleet host rejected: %v", err)
		}

		cfg.Parallels.Hosts[0].Key = ""
		if err := validateProviderCredentialDestination(cfg); err == nil {
			t.Fatal("repository fleet host with ambient auth was accepted")
		}

		t.Setenv("CRABBOX_PARALLELS_HOST", "approved.example.test")
		if err := applyEnv(&cfg); err != nil {
			t.Fatal(err)
		}
		if len(cfg.Parallels.Hosts) != 0 {
			t.Fatalf("explicit host retained repository fleet: %#v", cfg.Parallels.Hosts)
		}
		if err := validateProviderCredentialDestination(cfg); err != nil {
			t.Fatalf("explicit parallels host rejected: %v", err)
		}
	})

	t.Run("exe dev environment host approves ambient auth", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Provider = "exe-dev"
		if err := applyFileConfigWithTrust(&cfg, fileConfig{ExeDev: &fileExeDevConfig{ControlHost: "repo.example.test"}}, false); err != nil {
			t.Fatal(err)
		}
		if err := validateProviderCredentialDestination(cfg); err == nil {
			t.Fatal("repository exe.dev control host was accepted")
		}

		t.Setenv("CRABBOX_EXE_DEV_CONTROL_HOST", "approved.example.test")
		if err := applyEnv(&cfg); err != nil {
			t.Fatal(err)
		}
		if err := validateProviderCredentialDestination(cfg); err != nil {
			t.Fatalf("explicit exe.dev control host rejected: %v", err)
		}
	})

	t.Run("external repository host requires trusted approval", func(t *testing.T) {
		repositoryRoot, key := repositorySSHKeyFixture(t)
		cfg := baseConfig()
		cfg.Provider = "external"
		cfg.External.Lifecycle = externalLifecycleConfigForTest()
		cfg.credentialProvenance.repositoryRoot = repositoryRoot
		connection := ExternalConnectionConfig{SSH: ExternalSSHConnectionConfig{Host: "repo.example.test"}}
		if err := applyFileConfigWithTrust(&cfg, fileConfig{External: &fileExternalConfig{Connection: &connection}}, false); err != nil {
			t.Fatal(err)
		}
		if err := validateProviderCredentialDestination(cfg); err == nil {
			t.Fatal("repository external host with ambient auth was accepted")
		}

		connection.SSH.Key = key
		if err := applyFileConfigWithTrust(&cfg, fileConfig{External: &fileExternalConfig{Connection: &connection}}, false); err != nil {
			t.Fatal(err)
		}
		if err := validateProviderCredentialDestination(cfg); err == nil {
			t.Fatal("repository external host with a same-source key was accepted")
		}
	})

	t.Run("trusted external approval survives intermediate repository layer", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Provider = "external"
		cfg.External.Lifecycle = externalLifecycleConfigForTest()
		connection := ExternalConnectionConfig{
			ResourceName: "approved.example.test",
			SSH: ExternalSSHConnectionConfig{
				Host:         "{{resourceName}}",
				ProxyCommand: "proxy approved.example.test",
				AllowEnv:     true,
			},
		}
		if err := applyFileConfigWithTrust(&cfg, fileConfig{External: &fileExternalConfig{Connection: &connection}}, true); err != nil {
			t.Fatal(err)
		}
		intermediate := ExternalConnectionConfig{
			ResourceName: "other.example.test",
			SSH: ExternalSSHConnectionConfig{
				Host:         "other.example.test",
				ProxyCommand: "proxy other.example.test",
			},
		}
		if err := applyFileConfigWithTrust(&cfg, fileConfig{External: &fileExternalConfig{Connection: &intermediate}}, false); err != nil {
			t.Fatal(err)
		}
		if err := applyFileConfigWithTrust(&cfg, fileConfig{External: &fileExternalConfig{Connection: &connection}}, false); err != nil {
			t.Fatal(err)
		}
		if err := validateProviderCredentialDestination(cfg); err != nil {
			t.Fatalf("trusted External SSH destination rejected: %v", err)
		}
	})

	t.Run("trusted external env approval is template-bound", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Provider = "external"
		cfg.External.Lifecycle = externalLifecycleConfigForTest()
		trusted := ExternalConnectionConfig{SSH: ExternalSSHConnectionConfig{
			User:     "{{env.EXTERNAL_SSH_USER}}",
			Host:     "approved.example.test",
			AllowEnv: true,
		}}
		if err := applyFileConfigWithTrust(&cfg, fileConfig{External: &fileExternalConfig{Connection: &trusted}}, true); err != nil {
			t.Fatal(err)
		}
		repository := trusted
		repository.SSH.User = "{{env.AWS_SECRET_ACCESS_KEY}}"
		if err := applyFileConfigWithTrust(&cfg, fileConfig{External: &fileExternalConfig{Connection: &repository}}, false); err != nil {
			t.Fatal(err)
		}
		if err := validateProviderCredentialDestination(cfg); err == nil || !strings.Contains(err.Error(), "ssh.allowEnv") {
			t.Fatalf("repository env template change error=%v", err)
		}
	})

	t.Run("trusted external env approval binds referenced resource name", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Provider = "external"
		cfg.External.Lifecycle = externalLifecycleConfigForTest()
		trusted := ExternalConnectionConfig{
			ResourceName: "approved-resource",
			SSH: ExternalSSHConnectionConfig{
				User:     "{{resourceName}}",
				Host:     "approved.example.test",
				AllowEnv: true,
			},
		}
		if err := applyFileConfigWithTrust(&cfg, fileConfig{External: &fileExternalConfig{Connection: &trusted}}, true); err != nil {
			t.Fatal(err)
		}
		repository := trusted
		repository.ResourceName = "{{env.GITHUB_TOKEN}}"
		repository.AllowEnvResourceName = true
		if err := applyFileConfigWithTrust(&cfg, fileConfig{External: &fileExternalConfig{Connection: &repository}}, false); err != nil {
			t.Fatal(err)
		}
		if err := validateProviderCredentialDestination(cfg); err == nil || !strings.Contains(err.Error(), "ssh.allowEnv") {
			t.Fatalf("repository resource-name change error=%v", err)
		}
	})

	t.Run("trusted external provider output approval binds adapter contract", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Provider = "external"
		trusted := fileExternalConfig{
			Command: "approved-provider",
			Args:    []string{"--profile", "approved"},
			Config:  map[string]any{"namespace": "approved"},
			Connection: &ExternalConnectionConfig{SSH: ExternalSSHConnectionConfig{
				TrustProviderOutput: true,
			}},
		}
		if err := applyFileConfigWithTrust(&cfg, fileConfig{External: &trusted}, true); err != nil {
			t.Fatal(err)
		}
		if err := ValidateExternalProviderSSHOutput(cfg); err != nil {
			t.Fatalf("trusted provider-output contract rejected: %v", err)
		}

		repository := fileExternalConfig{Command: "repository-provider"}
		if err := applyFileConfigWithTrust(&cfg, fileConfig{External: &repository}, false); err != nil {
			t.Fatal(err)
		}
		if err := ValidateExternalProviderSSHOutput(cfg); err == nil || !strings.Contains(err.Error(), "trustProviderOutput") {
			t.Fatalf("repository provider-output contract change error=%v", err)
		}
	})

	t.Run("partial explicit override does not approve repository contract", func(t *testing.T) {
		for name, markExplicit := range map[string]func(*Config){
			"flag": MarkExternalProviderOutputFlagExplicit,
			"environment": func(cfg *Config) {
				markExternalProviderOutputExplicit(cfg, credentialSourceEnvironment)
			},
		} {
			t.Run(name, func(t *testing.T) {
				cfg := baseConfig()
				cfg.Provider = "external"
				trusted := fileExternalConfig{
					Command: "approved-provider",
					Args:    []string{"--approved"},
					Connection: &ExternalConnectionConfig{SSH: ExternalSSHConnectionConfig{
						TrustProviderOutput: true,
					}},
				}
				if err := applyFileConfigWithTrust(&cfg, fileConfig{External: &trusted}, true); err != nil {
					t.Fatal(err)
				}
				if err := applyFileConfigWithTrust(&cfg, fileConfig{External: &fileExternalConfig{
					Args: []string{"--repository"},
				}}, false); err != nil {
					t.Fatal(err)
				}
				markExplicit(&cfg)
				if err := ValidateExternalProviderSSHOutput(cfg); err == nil || !strings.Contains(err.Error(), "trustProviderOutput") {
					t.Fatalf("partial %s override error=%v", name, err)
				}
			})
		}
	})

	t.Run("trusted external provider output approval binds connection inputs", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Provider = "external"
		trustedConnection := ExternalConnectionConfig{
			ResourceName: "approved-resource",
			SSH:          ExternalSSHConnectionConfig{TrustProviderOutput: true},
		}
		trusted := fileExternalConfig{
			Command:    "approved-provider",
			Connection: &trustedConnection,
		}
		if err := applyFileConfigWithTrust(&cfg, fileConfig{External: &trusted}, true); err != nil {
			t.Fatal(err)
		}
		repositoryConnection := trustedConnection
		repositoryConnection.ResourceName = "repository-resource"
		if err := applyFileConfigWithTrust(&cfg, fileConfig{External: &fileExternalConfig{
			Connection: &repositoryConnection,
		}}, false); err != nil {
			t.Fatal(err)
		}
		if err := ValidateExternalProviderSSHOutput(cfg); err == nil || !strings.Contains(err.Error(), "trustProviderOutput") {
			t.Fatalf("repository provider-output connection change error=%v", err)
		}
	})

	t.Run("repository cannot self-enable external provider output", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Provider = "external"
		repository := fileExternalConfig{
			Command: "repository-provider",
			Connection: &ExternalConnectionConfig{SSH: ExternalSSHConnectionConfig{
				TrustProviderOutput: true,
			}},
		}
		if err := applyFileConfigWithTrust(&cfg, fileConfig{External: &repository}, false); err != nil {
			t.Fatal(err)
		}
		if err := ValidateExternalProviderSSHOutput(cfg); err == nil || !strings.Contains(err.Error(), "trustProviderOutput") {
			t.Fatalf("repository provider-output opt-in error=%v", err)
		}
		if err := validateProviderCredentialDestination(cfg); err == nil || !strings.Contains(err.Error(), "trustProviderOutput") {
			t.Fatalf("repository provider-output preflight error=%v", err)
		}
	})

	t.Run("trusted external provider output rejects unencodable contract", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Provider = "external"
		trusted := fileExternalConfig{
			Command: "approved-provider",
			Config:  map[string]any{"invalid": math.Inf(1)},
			Connection: &ExternalConnectionConfig{SSH: ExternalSSHConnectionConfig{
				TrustProviderOutput: true,
			}},
		}
		err := applyFileConfigWithTrust(&cfg, fileConfig{External: &trusted}, true)
		if err == nil || !strings.Contains(err.Error(), "JSON encodable") {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("repository external proxy requires trusted approval", func(t *testing.T) {
		repositoryRoot, key := repositorySSHKeyFixture(t)
		cfg := baseConfig()
		cfg.Provider = "external"
		cfg.External.Lifecycle = externalLifecycleConfigForTest()
		cfg.credentialProvenance.repositoryRoot = repositoryRoot
		connection := ExternalConnectionConfig{SSH: ExternalSSHConnectionConfig{
			Host:         "approved.example.test",
			Key:          key,
			ProxyCommand: "repo-proxy %h %p",
		}}
		if err := applyFileConfigWithTrust(&cfg, fileConfig{External: &fileExternalConfig{Connection: &connection}}, false); err != nil {
			t.Fatal(err)
		}
		if err := validateProviderCredentialDestination(cfg); err == nil {
			t.Fatal("repository external proxy with a same-source outer key was accepted")
		}
	})

	t.Run("trusted external approval binds full SSH endpoint", func(t *testing.T) {
		mutations := map[string]func(*ExternalSSHConnectionConfig){
			"user":           func(ssh *ExternalSSHConnectionConfig) { ssh.User = "root" },
			"key":            func(ssh *ExternalSSHConnectionConfig) { ssh.Key = "/tmp/other-key" },
			"port":           func(ssh *ExternalSSHConnectionConfig) { ssh.Port = "2222" },
			"fallback ports": func(ssh *ExternalSSHConnectionConfig) { ssh.FallbackPorts = []string{"2200"} },
			"config proxy":   func(ssh *ExternalSSHConnectionConfig) { ssh.SSHConfigProxy = true },
		}
		for name, mutate := range mutations {
			t.Run(name, func(t *testing.T) {
				cfg := baseConfig()
				cfg.Provider = "external"
				cfg.External.Lifecycle = externalLifecycleConfigForTest()
				trusted := ExternalConnectionConfig{SSH: ExternalSSHConnectionConfig{
					User:          "developer",
					Host:          "approved.example.test",
					Port:          "22",
					FallbackPorts: []string{"2201", "2202"},
				}}
				if err := applyFileConfigWithTrust(&cfg, fileConfig{External: &fileExternalConfig{Connection: &trusted}}, true); err != nil {
					t.Fatal(err)
				}
				repository := trusted
				repository.SSH.FallbackPorts = append([]string(nil), trusted.SSH.FallbackPorts...)
				mutate(&repository.SSH)
				if err := applyFileConfigWithTrust(&cfg, fileConfig{External: &fileExternalConfig{Connection: &repository}}, false); err != nil {
					t.Fatal(err)
				}
				if err := validateProviderCredentialDestination(cfg); err == nil || !strings.Contains(err.Error(), "ssh endpoint") {
					t.Fatalf("repository %s change error=%v", name, err)
				}
			})
		}
	})

	t.Run("trusted external endpoint rejects repository template inputs", func(t *testing.T) {
		mutations := map[string]func(*ExternalSSHConnectionConfig){
			"user": func(ssh *ExternalSSHConnectionConfig) { ssh.User = "{{config.endpoint}}" },
			"key":  func(ssh *ExternalSSHConnectionConfig) { ssh.Key = "{{config.endpoint}}" },
			"port": func(ssh *ExternalSSHConnectionConfig) { ssh.Port = "{{config.endpoint}}" },
			"fallback port": func(ssh *ExternalSSHConnectionConfig) {
				ssh.FallbackPorts = []string{"{{config.endpoint}}"}
			},
		}
		for name, mutate := range mutations {
			t.Run(name, func(t *testing.T) {
				cfg := baseConfig()
				cfg.Provider = "external"
				cfg.External.Lifecycle = externalLifecycleConfigForTest()
				trustedConnection := ExternalConnectionConfig{SSH: ExternalSSHConnectionConfig{
					User: "developer",
					Host: "approved.example.test",
					Port: "22",
				}}
				mutate(&trustedConnection.SSH)
				trusted := fileExternalConfig{
					Config:     map[string]any{"endpoint": "approved-value"},
					Connection: &trustedConnection,
				}
				if err := applyFileConfigWithTrust(&cfg, fileConfig{External: &trusted}, true); err != nil {
					t.Fatal(err)
				}
				if err := applyFileConfigWithTrust(&cfg, fileConfig{External: &fileExternalConfig{
					Config: map[string]any{"endpoint": "repository-value"},
				}}, false); err != nil {
					t.Fatal(err)
				}
				if err := validateProviderCredentialDestination(cfg); err == nil || !strings.Contains(err.Error(), "ssh endpoint") {
					t.Fatalf("repository %s template input error=%v", name, err)
				}
			})
		}
	})
}

func TestRepositorySSHDestinationsAllowExplicitFlagOverride(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		args []string
	}{
		{
			name: "parallels",
			cfg: Config{
				Provider:  "parallels",
				Parallels: ParallelsConfig{Host: "repo.example.test"},
				credentialProvenance: credentialDestinationProvenance{
					parallelsHost: credentialSourceRepository,
				},
			},
			args: []string{"--parallels-host", "approved.example.test"},
		},
		{
			name: "exe dev",
			cfg: Config{
				Provider: "exe-dev",
				ExeDev:   ExeDevConfig{ControlHost: "repo.example.test"},
				credentialProvenance: credentialDestinationProvenance{
					exeDevControlHost: credentialSourceRepository,
				},
			},
			args: []string{"--exe-dev-control-host", "approved.example.test"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := test.cfg
			fs := newFlagSet("test", io.Discard)
			var parallelsHost *string
			if cfg.Provider == "parallels" {
				parallelsHost = fs.String("parallels-host", cfg.Parallels.Host, "")
			}
			values := registerProviderFlags(fs, cfg)
			if err := parseFlags(fs, test.args); err != nil {
				t.Fatal(err)
			}
			if err := applyProviderFlags(&cfg, fs, values); err != nil {
				t.Fatal(err)
			}
			if parallelsHost != nil {
				cfg.Parallels.Host = *parallelsHost
				cfg.Parallels.Hosts = nil
				markCredentialDestinationFlagSources(&cfg, fs)
			}
			if err := validateProviderCredentialDestination(cfg); err != nil {
				t.Fatalf("explicit host flag rejected: %v", err)
			}
		})
	}

	t.Run("static", func(t *testing.T) {
		cfg := Config{
			Provider: staticProvider,
			Static:   StaticConfig{Host: "repo.example.test"},
			credentialProvenance: credentialDestinationProvenance{
				staticHost: credentialSourceRepository,
			},
		}
		fs := newFlagSet("test", io.Discard)
		values := registerTargetFlags(fs, cfg)
		if err := parseFlags(fs, []string{"--static-host", "approved.example.test"}); err != nil {
			t.Fatal(err)
		}
		if err := applyTargetFlagOverrides(&cfg, fs, values); err != nil {
			t.Fatal(err)
		}
		if err := validateProviderCredentialDestination(cfg); err != nil {
			t.Fatalf("explicit static host rejected: %v", err)
		}
	})
}

func TestConfigMergeIgnoresRepositoryOrgoCredential(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "orgo"
	if err := applyFileConfigWithTrust(&cfg, fileConfig{
		Orgo: &fileOrgoConfig{
			APIBase: "https://repo.example.test",
			APIKey:  "test-key",
		},
	}, false); err != nil {
		t.Fatal(err)
	}
	if cfg.Orgo.APIKey != "" {
		t.Fatal("repository Orgo API key was loaded")
	}
}
