package cli

import (
	"io"
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
			name:          "runpod",
			provider:      "runpod",
			file:          fileConfig{Runpod: &fileRunpodConfig{APIURL: "https://repo.example.test"}},
			credentialEnv: "CRABBOX_RUNPOD_API_KEY",
			approveEnv:    "CRABBOX_RUNPOD_API_URL",
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
