package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"CRABBOX_COORDINATOR",
		"CRABBOX_COORDINATOR_MODE",
		"CRABBOX_COORDINATOR_AUTO_WEBVNC",
		"CRABBOX_COORDINATOR_TOKEN",
		"CRABBOX_COORDINATOR_ADMIN_TOKEN",
		"CRABBOX_ADMIN_TOKEN",
		"CRABBOX_HOST_ID",
		"CRABBOX_AWS_MAC_HOST_ID",
		"CRABBOX_NETWORK",
		"CRABBOX_TAILSCALE",
		"CRABBOX_TAILSCALE_TAGS",
		"CRABBOX_TAILSCALE_HOSTNAME_TEMPLATE",
		"CRABBOX_TAILSCALE_AUTH_KEY_ENV",
		"CRABBOX_TAILSCALE_AUTH_KEY",
		"CRABBOX_TAILSCALE_EXIT_NODE",
		"CRABBOX_TAILSCALE_EXIT_NODE_ALLOW_LAN_ACCESS",
		"CRABBOX_ACCESS_CLIENT_ID",
		"CRABBOX_ACCESS_CLIENT_SECRET",
		"CRABBOX_ACCESS_TOKEN",
		"CF_ACCESS_CLIENT_ID",
		"CF_ACCESS_CLIENT_SECRET",
		"CF_ACCESS_TOKEN",
		"CRABBOX_AZURE_BACKEND",
		"CRABBOX_AZURE_DYNAMIC_SESSIONS_ENDPOINT",
		"CRABBOX_AZURE_DYNAMIC_SESSIONS_POOL",
		"CRABBOX_AZURE_DYNAMIC_SESSIONS_API_VERSION",
		"CRABBOX_AZURE_DYNAMIC_SESSIONS_WORKDIR",
		"CRABBOX_AZURE_DYNAMIC_SESSIONS_TIMEOUT_SECS",
		"CRABBOX_GCP_PROJECT",
		"GOOGLE_CLOUD_PROJECT",
		"GCP_PROJECT_ID",
		"CRABBOX_GCP_ZONE",
		"CRABBOX_GCP_IMAGE",
		"CRABBOX_GCP_NETWORK",
		"CRABBOX_GCP_SUBNET",
		"CRABBOX_GCP_TAGS",
		"CRABBOX_GCP_SSH_CIDRS",
		"CRABBOX_GCP_ROOT_GB",
		"CRABBOX_GCP_SERVICE_ACCOUNT",
		"CRABBOX_DIGITALOCEAN_REGION",
		"CRABBOX_DIGITALOCEAN_IMAGE",
		"CRABBOX_DIGITALOCEAN_VPC",
		"CRABBOX_DIGITALOCEAN_SSH_CIDRS",
		"CRABBOX_DAYTONA_API_KEY",
		"DAYTONA_API_KEY",
		"CRABBOX_DAYTONA_JWT_TOKEN",
		"DAYTONA_JWT_TOKEN",
		"CRABBOX_DAYTONA_ORGANIZATION_ID",
		"DAYTONA_ORGANIZATION_ID",
		"CRABBOX_DAYTONA_API_URL",
		"DAYTONA_API_URL",
		"CRABBOX_DAYTONA_SNAPSHOT",
		"DAYTONA_SNAPSHOT",
		"CRABBOX_DAYTONA_TARGET",
		"DAYTONA_TARGET",
		"CRABBOX_DAYTONA_USER",
		"CRABBOX_DAYTONA_WORK_ROOT",
		"CRABBOX_DAYTONA_SSH_GATEWAY_HOST",
		"CRABBOX_DAYTONA_SSH_ACCESS_MINUTES",
		"CRABBOX_E2B_API_KEY",
		"E2B_API_KEY",
		"CRABBOX_E2B_API_URL",
		"E2B_API_URL",
		"CRABBOX_E2B_DOMAIN",
		"E2B_DOMAIN",
		"CRABBOX_E2B_TEMPLATE",
		"CRABBOX_E2B_WORKDIR",
		"CRABBOX_E2B_USER",
		"CRABBOX_OPENSANDBOX_API_URL",
		"OPEN_SANDBOX_API_URL",
		"CRABBOX_OPENSANDBOX_API_KEY",
		"OPEN_SANDBOX_API_KEY",
		"CRABBOX_OPENSANDBOX_IMAGE",
		"CRABBOX_OPENSANDBOX_WORKDIR",
		"CRABBOX_OPENSANDBOX_CPU",
		"CRABBOX_OPENSANDBOX_MEMORY",
		"CRABBOX_OPENSANDBOX_TIMEOUT_SECS",
		"CRABBOX_OPENSANDBOX_EXEC_TIMEOUT_SECS",
		"CRABBOX_OPENSANDBOX_PLATFORM_OS",
		"CRABBOX_OPENSANDBOX_PLATFORM_ARCH",
		"CRABBOX_OPENSANDBOX_SECURE_ACCESS",
		"CRABBOX_OPENSANDBOX_USE_SERVER_PROXY",
		"CRABBOX_ISLO_API_KEY",
		"ISLO_API_KEY",
		"CRABBOX_ISLO_BASE_URL",
		"ISLO_BASE_URL",
		"CRABBOX_ISLO_IMAGE",
		"CRABBOX_ISLO_WORKDIR",
		"CRABBOX_ISLO_GATEWAY_PROFILE",
		"CRABBOX_ISLO_SNAPSHOT_NAME",
		"CRABBOX_ISLO_VCPUS",
		"CRABBOX_ISLO_MEMORY_MB",
		"CRABBOX_ISLO_DISK_GB",
		"CRABBOX_FREESTYLE_API_KEY",
		"FREESTYLE_API_KEY",
		"CRABBOX_FREESTYLE_API_URL",
		"FREESTYLE_API_URL",
		"CRABBOX_FREESTYLE_WORKDIR",
		"CRABBOX_FREESTYLE_VCPUS",
		"CRABBOX_FREESTYLE_MEMORY_GB",
		"CRABBOX_TENKI_CLI",
		"TENKI_CLI",
		"CRABBOX_TENKI_ENDPOINT",
		"TENKI_ENDPOINT",
		"CRABBOX_TENKI_GATEWAY",
		"TENKI_GATEWAY",
		"CRABBOX_TENKI_WORKSPACE",
		"CRABBOX_TENKI_PROJECT",
		"CRABBOX_TENKI_IMAGE",
		"CRABBOX_TENKI_SNAPSHOT",
		"CRABBOX_TENKI_WORK_ROOT",
		"CRABBOX_TENKI_CPUS",
		"CRABBOX_TENKI_MEMORY_MB",
		"CRABBOX_TENKI_DISK_GB",
		"CRABBOX_TENSORLAKE_API_KEY",
		"TENSORLAKE_API_KEY",
		"CRABBOX_TENSORLAKE_API_URL",
		"TENSORLAKE_API_URL",
		"CRABBOX_TENSORLAKE_CLI",
		"CRABBOX_TENSORLAKE_IMAGE",
		"CRABBOX_TENSORLAKE_SNAPSHOT",
		"CRABBOX_TENSORLAKE_ORGANIZATION_ID",
		"TENSORLAKE_ORGANIZATION_ID",
		"CRABBOX_TENSORLAKE_PROJECT_ID",
		"TENSORLAKE_PROJECT_ID",
		"CRABBOX_TENSORLAKE_NAMESPACE",
		"INDEXIFY_NAMESPACE",
		"CRABBOX_TENSORLAKE_WORKDIR",
		"CRABBOX_TENSORLAKE_CPUS",
		"CRABBOX_TENSORLAKE_MEMORY_MB",
		"CRABBOX_TENSORLAKE_DISK_MB",
		"CRABBOX_TENSORLAKE_TIMEOUT_SECS",
		"CRABBOX_TENSORLAKE_NO_INTERNET",
		"CRABBOX_DOCKER_SANDBOX_CLI",
		"CRABBOX_DOCKER_SANDBOX_AGENT",
		"CRABBOX_DOCKER_SANDBOX_TEMPLATE",
		"CRABBOX_DOCKER_SANDBOX_CPUS",
		"CRABBOX_DOCKER_SANDBOX_MEMORY",
		"CRABBOX_DOCKER_SANDBOX_CLONE",
		"CRABBOX_DOCKER_SANDBOX_WORKDIR",
		"CRABBOX_DOCKER_SANDBOX_EXTRA_WORKSPACES",
		"CRABBOX_DOCKER_SANDBOX_MCP",
		"CRABBOX_DOCKER_SANDBOX_KIT",
		"CRABBOX_ANTHROPIC_SANDBOX_RUNTIME_CLI",
		"CRABBOX_ANTHROPIC_SANDBOX_RUNTIME_SETTINGS",
		"CRABBOX_ANTHROPIC_SANDBOX_RUNTIME_DEBUG",
		"CRABBOX_ASCII_BOX_API_KEY",
		"ASCII_BOX_API_KEY",
		"CRABBOX_ASCII_BOX_BASE_URL",
		"ASCII_BOX_BASE_URL",
		"CRABBOX_ASCII_BOX_CLI",
		"BOX_CLI",
		"CRABBOX_ASCII_BOX_WORKDIR",
		"CRABBOX_APPLE_CONTAINER_CLI",
		"CRABBOX_APPLE_CONTAINER_IMAGE",
		"CRABBOX_APPLE_CONTAINER_USER",
		"CRABBOX_APPLE_CONTAINER_WORK_ROOT",
		"CRABBOX_APPLE_CONTAINER_CPUS",
		"CRABBOX_APPLE_CONTAINER_MEMORY",
		"CRABBOX_APPLE_CONTAINER_EXTRA_RUN_ARGS",
		"CRABBOX_APPLE_VZ_HELPER",
		"CRABBOX_APPLE_VZ_IMAGE",
		"CRABBOX_APPLE_VZ_IMAGE_SHA256",
		"CRABBOX_APPLE_VZ_USER",
		"CRABBOX_APPLE_VZ_WORK_ROOT",
		"CRABBOX_APPLE_VZ_CPUS",
		"CRABBOX_APPLE_VZ_MEMORY",
		"CRABBOX_APPLE_VZ_DISK",
		"CRABBOX_MULTIPASS_CLI",
		"CRABBOX_MULTIPASS_IMAGE",
		"CRABBOX_MULTIPASS_USER",
		"CRABBOX_MULTIPASS_WORK_ROOT",
		"CRABBOX_MULTIPASS_CPUS",
		"CRABBOX_MULTIPASS_MEMORY",
		"CRABBOX_MULTIPASS_DISK",
		"CRABBOX_MULTIPASS_LAUNCH_TIMEOUT",
		"CRABBOX_WANDB_API_KEY",
		"WANDB_API_KEY",
		"CRABBOX_WANDB_DEFAULT_IMAGE",
		"WANDB_DEFAULT_IMAGE",
		"CRABBOX_WANDB_MAX_LIFETIME_SECONDS",
		"WANDB_MAX_LIFETIME_SECONDS",
		"CRABBOX_CLOUDFLARE_RUNNER_URL",
		"CRABBOX_CLOUDFLARE_RUNNER_TOKEN",
		"CRABBOX_CLOUDFLARE_WORKDIR",
		"CRABBOX_SEMAPHORE_HOST",
		"SEMAPHORE_HOST",
		"CRABBOX_SEMAPHORE_TOKEN",
		"SEMAPHORE_API_TOKEN",
		"CRABBOX_SEMAPHORE_PROJECT",
		"SEMAPHORE_PROJECT",
		"CRABBOX_SEMAPHORE_MACHINE",
		"CRABBOX_SEMAPHORE_OS_IMAGE",
		"CRABBOX_SEMAPHORE_IDLE_TIMEOUT",
		"CRABBOX_SPRITES_TOKEN",
		"SPRITES_TOKEN",
		"SPRITE_TOKEN",
		"SETUP_SPRITE_TOKEN",
		"CRABBOX_SPRITES_API_URL",
		"SPRITES_API_URL",
		"CRABBOX_SPRITES_WORK_ROOT",
		"CRABBOX_LOCAL_CONTAINER_RUNTIME",
		"CRABBOX_LOCAL_CONTAINER_IMAGE",
		"CRABBOX_LOCAL_CONTAINER_USER",
		"CRABBOX_LOCAL_CONTAINER_WORK_ROOT",
		"CRABBOX_LOCAL_CONTAINER_CPUS",
		"CRABBOX_LOCAL_CONTAINER_MEMORY",
		"CRABBOX_LOCAL_CONTAINER_NETWORK",
		"CRABBOX_LOCAL_CONTAINER_DOCKER_SOCKET",
		"CRABBOX_NAMESPACE_IMAGE",
		"CRABBOX_NAMESPACE_SIZE",
		"CRABBOX_NAMESPACE_REPOSITORY",
		"CRABBOX_NAMESPACE_SITE",
		"CRABBOX_NAMESPACE_VOLUME_SIZE_GB",
		"CRABBOX_NAMESPACE_AUTO_STOP_IDLE_TIMEOUT",
		"CRABBOX_NAMESPACE_WORK_ROOT",
		"CRABBOX_NAMESPACE_DELETE_ON_RELEASE",
		"CRABBOX_MORPH_API_KEY",
		"MORPH_API_KEY",
		"CRABBOX_MORPH_API_URL",
		"CRABBOX_MORPH_SNAPSHOT",
		"CRABBOX_MORPH_SSH_GATEWAY_HOST",
		"CRABBOX_MORPH_WORK_ROOT",
		"CRABBOX_MORPH_DELETE_ON_RELEASE",
		"CRABBOX_MORPH_WAKE_ON_SSH",
		"CRABBOX_EXE_DEV_CONTROL_HOST",
		"EXE_DEV_CONTROL_HOST",
		"CRABBOX_EXE_DEV_IMAGE",
		"EXE_DEV_IMAGE",
		"CRABBOX_EXE_DEV_CPUS",
		"CRABBOX_EXE_DEV_MEMORY",
		"EXE_DEV_MEMORY",
		"CRABBOX_EXE_DEV_DISK",
		"EXE_DEV_DISK",
		"CRABBOX_EXE_DEV_COMMAND",
		"CRABBOX_EXE_DEV_USER",
		"CRABBOX_EXE_DEV_WORK_ROOT",
		"CRABBOX_EXE_DEV_NO_EMAIL",
		"CRABBOX_RAILWAY_API_TOKEN",
		"RAILWAY_API_TOKEN",
		"CRABBOX_RAILWAY_API_URL",
		"RAILWAY_API_URL",
		"CRABBOX_RAILWAY_PROJECT_ID",
		"RAILWAY_PROJECT_ID",
		"CRABBOX_RAILWAY_ENVIRONMENT_ID",
		"RAILWAY_ENVIRONMENT_ID",
	} {
		t.Setenv(key, "")
	}
}

func TestDockerSandboxConfigDefaultsFileAndEnv(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	if cfg.DockerSandbox.CLIPath != "sbx" || cfg.DockerSandbox.Agent != "shell" || cfg.DockerSandbox.Workdir != "" {
		t.Fatalf("dockerSandbox defaults not applied: %#v", cfg.DockerSandbox)
	}
	clone := true
	template := "ubuntu"
	cpus := 2.5
	memory := "6g"
	workdir := "/workspace/my-app"
	extraWorkspaces := []string{"/tmp/extra"}
	mcp := []string{"context7", "all"}
	kit := []string{"example-org/base"}
	applyFileConfig(&cfg, fileConfig{
		Provider: "docker-sandbox",
		DockerSandbox: &fileDockerSandboxConfig{
			CLIPath:         "/opt/sbx",
			Agent:           "shell",
			Template:        &template,
			CPUs:            &cpus,
			Memory:          &memory,
			Clone:           &clone,
			Workdir:         &workdir,
			ExtraWorkspaces: &extraWorkspaces,
			MCP:             &mcp,
			Kit:             &kit,
		},
	})
	if cfg.Provider != "docker-sandbox" || cfg.DockerSandbox.CLIPath != "/opt/sbx" || cfg.DockerSandbox.Template != "ubuntu" || cfg.DockerSandbox.CPUs != 2.5 || cfg.DockerSandbox.Memory != "6g" || !cfg.DockerSandbox.Clone || cfg.DockerSandbox.Workdir != "/workspace/my-app" {
		t.Fatalf("file dockerSandbox config not applied: %#v", cfg.DockerSandbox)
	}
	if strings.Join(cfg.DockerSandbox.ExtraWorkspaces, ",") != "/tmp/extra" || strings.Join(cfg.DockerSandbox.MCP, ",") != "context7,all" || strings.Join(cfg.DockerSandbox.Kit, ",") != "example-org/base" {
		t.Fatalf("file dockerSandbox list config not applied: %#v", cfg.DockerSandbox)
	}

	t.Setenv("CRABBOX_DOCKER_SANDBOX_CLI", "/usr/local/bin/sbx")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_AGENT", "shell")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_TEMPLATE", "debian")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_CPUS", "4")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_MEMORY", "8g")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_CLONE", "false")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_WORKDIR", "/workspace/env-app")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_EXTRA_WORKSPACES", "/tmp/a,/tmp/b")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_MCP", "context7,all")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_KIT", "kit-a,kit-b")
	if err := applyEnv(&cfg); err != nil {
		t.Fatalf("applyEnv err=%v", err)
	}
	if cfg.DockerSandbox.CLIPath != "/usr/local/bin/sbx" || cfg.DockerSandbox.Template != "debian" || cfg.DockerSandbox.CPUs != 4 || cfg.DockerSandbox.Memory != "8g" || cfg.DockerSandbox.Clone || cfg.DockerSandbox.Workdir != "/workspace/env-app" {
		t.Fatalf("env dockerSandbox config not applied: %#v", cfg.DockerSandbox)
	}
	if strings.Join(cfg.DockerSandbox.ExtraWorkspaces, ",") != "/tmp/a,/tmp/b" || strings.Join(cfg.DockerSandbox.MCP, ",") != "context7,all" || strings.Join(cfg.DockerSandbox.Kit, ",") != "kit-a,kit-b" {
		t.Fatalf("env dockerSandbox list config not applied: %#v", cfg.DockerSandbox)
	}
}

func TestDigitalOceanConfigFileAndEnv(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	applyFileConfig(&cfg, fileConfig{
		Provider: "digitalocean",
		DigitalOcean: &fileDigitalOceanConfig{
			Region:   "sfo3",
			Image:    "ubuntu-24-04-x64",
			VPCUUID:  "vpc-file",
			SSHCIDRs: []string{"203.0.113.0/24"},
		},
	})
	if cfg.Provider != "digitalocean" || cfg.DigitalOcean.Region != "sfo3" || cfg.Location == "sfo3" || cfg.DigitalOcean.Image != "ubuntu-24-04-x64" || cfg.Image == "ubuntu-24-04-x64" || cfg.DigitalOcean.VPCUUID != "vpc-file" {
		t.Fatalf("file digitalocean config not applied: cfg=%#v do=%#v", cfg, cfg.DigitalOcean)
	}
	if strings.Join(cfg.DigitalOcean.SSHCIDRs, ",") != "203.0.113.0/24" {
		t.Fatalf("file digitalocean ssh cidrs=%v", cfg.DigitalOcean.SSHCIDRs)
	}

	t.Setenv("CRABBOX_DIGITALOCEAN_REGION", "nyc3")
	t.Setenv("CRABBOX_DIGITALOCEAN_IMAGE", "ubuntu-22-04-x64")
	t.Setenv("CRABBOX_DIGITALOCEAN_VPC", "vpc-env")
	t.Setenv("CRABBOX_DIGITALOCEAN_SSH_CIDRS", "198.51.100.0/24,2001:db8::/64")
	if err := applyEnv(&cfg); err != nil {
		t.Fatalf("applyEnv err=%v", err)
	}
	if cfg.DigitalOcean.Region != "nyc3" || cfg.Location == "nyc3" || cfg.DigitalOcean.Image != "ubuntu-22-04-x64" || cfg.Image == "ubuntu-22-04-x64" || cfg.DigitalOcean.VPCUUID != "vpc-env" {
		t.Fatalf("env digitalocean config not applied: cfg=%#v do=%#v", cfg, cfg.DigitalOcean)
	}
	if strings.Join(cfg.DigitalOcean.SSHCIDRs, ",") != "198.51.100.0/24,2001:db8::/64" {
		t.Fatalf("env digitalocean ssh cidrs=%v", cfg.DigitalOcean.SSHCIDRs)
	}
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatalf("applyProviderConfigDefaults err=%v", err)
	}
	base := baseConfig()
	if cfg.Location != base.Location || cfg.Image != base.Image {
		t.Fatalf("digitalocean defaults leaked into generic fields: cfg=%#v", cfg)
	}
}

func TestDigitalOceanPortableOSSelection(t *testing.T) {
	t.Run("supported selector maps to provider image", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Provider = "digitalocean"
		cfg.OSImage = "ubuntu:24.04"
		cfg.osImageExplicit = true
		if err := applyProviderConfigDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.DigitalOcean.Image != "ubuntu-24-04-x64" {
			t.Fatalf("DigitalOcean.Image=%q", cfg.DigitalOcean.Image)
		}
	})

	t.Run("unsupported selector is deferred to acquisition", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Provider = "digitalocean"
		if err := applyProviderConfigDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.DigitalOcean.Image != "ubuntu-24-04-x64" {
			t.Fatalf("default DigitalOcean.Image=%q", cfg.DigitalOcean.Image)
		}
		cfg.OSImage = "ubuntu:26.04"
		cfg.osImageExplicit = true
		if err := applyProviderConfigDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.DigitalOcean.Image != "" {
			t.Fatalf("DigitalOcean.Image=%q, want unresolved provider image", cfg.DigitalOcean.Image)
		}
	})

	t.Run("provider image overrides portable selector", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Provider = "digitalocean"
		cfg.OSImage = "ubuntu:26.04"
		cfg.osImageExplicit = true
		cfg.DigitalOcean.Image = "custom-image"
		cfg.digitalOceanImageExplicit = true
		if err := applyProviderConfigDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.DigitalOcean.Image != "custom-image" {
			t.Fatalf("DigitalOcean.Image=%q", cfg.DigitalOcean.Image)
		}
	})
}

func TestDigitalOceanUnsupportedPortableOSDoesNotBlockCLIOverrides(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	if err := os.WriteFile(configPath, []byte("provider: digitalocean\nos: ubuntu:26.04\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("portable os override", func(t *testing.T) {
		cfg, err := loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		fs := newFlagSet("test", io.Discard)
		values := registerLeaseCreateFlags(fs, cfg)
		if err := parseFlags(fs, []string{"--os", "ubuntu:24.04"}); err != nil {
			t.Fatal(err)
		}
		if err := applyLeaseCreateFlags(&cfg, fs, values); err != nil {
			t.Fatal(err)
		}
		if cfg.DigitalOcean.Image != "ubuntu-24-04-x64" {
			t.Fatalf("DigitalOcean.Image=%q", cfg.DigitalOcean.Image)
		}
	})

	t.Run("provider override", func(t *testing.T) {
		cfg, err := loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		fs := newFlagSet("test", io.Discard)
		values := registerLeaseCreateFlags(fs, cfg)
		if err := parseFlags(fs, []string{"--provider", "aws"}); err != nil {
			t.Fatal(err)
		}
		if err := applyLeaseCreateFlags(&cfg, fs, values); err != nil {
			t.Fatal(err)
		}
		if cfg.Provider != "aws" {
			t.Fatalf("Provider=%q", cfg.Provider)
		}
	})
}

func TestDigitalOceanEnvDoesNotMutateGenericFieldsForOtherProviders(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	cfg.Provider = "hetzner"
	originalLocation := cfg.Location
	originalImage := cfg.Image
	t.Setenv("CRABBOX_DIGITALOCEAN_REGION", "nyc3")
	t.Setenv("CRABBOX_DIGITALOCEAN_IMAGE", "ubuntu-22-04-x64")

	if err := applyEnv(&cfg); err != nil {
		t.Fatalf("applyEnv err=%v", err)
	}
	if cfg.DigitalOcean.Region != "nyc3" || cfg.DigitalOcean.Image != "ubuntu-22-04-x64" {
		t.Fatalf("digitalocean env not stored: do=%#v", cfg.DigitalOcean)
	}
	if cfg.Location != originalLocation || cfg.Image != originalImage {
		t.Fatalf("digitalocean env leaked into generic fields: location=%q image=%q", cfg.Location, cfg.Image)
	}
}

func TestDigitalOceanDefaultsPreserveExplicitGenericBaseValues(t *testing.T) {
	clearConfigEnv(t)
	base := baseConfig()
	cfg := baseConfig()
	applyFileConfig(&cfg, fileConfig{
		Provider: "digitalocean",
		SSH: &fileSSHConfig{
			User: base.SSHUser,
			Port: base.SSHPort,
		},
		Hetzner: &fileHetznerConfig{
			Location: base.Location,
			Image:    base.Image,
		},
		DigitalOcean: &fileDigitalOceanConfig{
			Region: "sfo3",
			Image:  "ubuntu-24-04-x64",
		},
	})

	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatalf("applyProviderConfigDefaults err=%v", err)
	}
	if cfg.Location != base.Location {
		t.Fatalf("Location=%q want explicit %q", cfg.Location, base.Location)
	}
	if cfg.Image != base.Image {
		t.Fatalf("Image=%q want explicit %q", cfg.Image, base.Image)
	}
	if cfg.SSHUser != base.SSHUser || cfg.SSHPort != base.SSHPort {
		t.Fatalf("SSH=%s@:%s want explicit %s@:%s", cfg.SSHUser, cfg.SSHPort, base.SSHUser, base.SSHPort)
	}
	if cfg.DigitalOcean.Region != "sfo3" || cfg.DigitalOcean.Image != "ubuntu-24-04-x64" {
		t.Fatalf("DigitalOcean=%#v", cfg.DigitalOcean)
	}
}

func TestDigitalOceanDefaultsPreserveExplicitGenericWorkRoot(t *testing.T) {
	cfg := baseConfig()
	applyFileConfig(&cfg, fileConfig{
		Provider: "tart",
		WorkRoot: "/srv/crabbox",
		SSH:      &fileSSHConfig{User: "alice", Port: "2200"},
		Windows:  &fileWindowsConfig{Mode: windowsModeNormal},
	})

	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.WorkRoot != cfg.Tart.WorkRoot {
		t.Fatalf("Tart WorkRoot=%q want provider root %q before override", cfg.WorkRoot, cfg.Tart.WorkRoot)
	}
	cfg.WindowsMode = windowsModeWSL2
	cfg.Provider = "digitalocean"
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.WorkRoot != "/srv/crabbox" {
		t.Fatalf("DigitalOcean WorkRoot=%q want explicit generic root", cfg.WorkRoot)
	}
	if cfg.SSHUser != "alice" {
		t.Fatalf("DigitalOcean SSHUser=%q want explicit generic user", cfg.SSHUser)
	}
	if cfg.SSHPort != "2200" {
		t.Fatalf("DigitalOcean SSHPort=%q want explicit generic port", cfg.SSHPort)
	}
	if cfg.WindowsMode != windowsModeNormal {
		t.Fatalf("DigitalOcean WindowsMode=%q want explicit generic mode", cfg.WindowsMode)
	}
}

func TestDigitalOceanDefaultsIgnoreStaticProviderOverlays(t *testing.T) {
	cfg := baseConfig()
	applyFileConfig(&cfg, fileConfig{
		Provider: "ssh",
		WorkRoot: "/srv/crabbox",
		SSH:      &fileSSHConfig{User: "alice", Port: "2200"},
		Static: &fileStaticConfig{
			User:     "builder",
			Port:     "2202",
			WorkRoot: "/srv/static",
		},
	})
	normalizeTargetConfig(&cfg)
	if cfg.SSHUser != "alice" || cfg.SSHPort != "2200" || cfg.WorkRoot != "/srv/static" {
		t.Fatalf("static source settings user=%q port=%q root=%q", cfg.SSHUser, cfg.SSHPort, cfg.WorkRoot)
	}

	cfg.Provider = "digitalocean"
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.SSHUser != "alice" || cfg.SSHPort != "2200" || cfg.WorkRoot != "/srv/crabbox" {
		t.Fatalf("DigitalOcean settings user=%q port=%q root=%q", cfg.SSHUser, cfg.SSHPort, cfg.WorkRoot)
	}
}

func TestDigitalOceanDefaultsDoNotLeakAcrossProviderOverride(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	cfg.Provider = "digitalocean"
	wantLocation := cfg.Location
	wantImage := cfg.Image
	wantSSHUser := cfg.SSHUser
	wantSSHPort := cfg.SSHPort
	wantFallbackPorts := append([]string(nil), cfg.SSHFallbackPorts...)

	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatalf("digitalocean defaults: %v", err)
	}
	cfg.Provider = "hetzner"
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatalf("hetzner defaults: %v", err)
	}

	if cfg.Location != wantLocation || cfg.Image != wantImage ||
		cfg.SSHUser != wantSSHUser || cfg.SSHPort != wantSSHPort ||
		strings.Join(cfg.SSHFallbackPorts, ",") != strings.Join(wantFallbackPorts, ",") {
		t.Fatalf("digitalocean defaults leaked after provider override: %#v", cfg)
	}
}

func TestDockerSandboxEmptyFileConfigDoesNotClearExistingValues(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	cfg.DockerSandbox = DockerSandboxConfig{
		CLIPath:         "/opt/sbx",
		Agent:           "shell",
		Template:        "ubuntu",
		CPUs:            2,
		Memory:          "4g",
		Clone:           true,
		Workdir:         "/workspace/my-app",
		ExtraWorkspaces: []string{"/tmp/extra"},
		MCP:             []string{"context7"},
		Kit:             []string{"example-org/base"},
	}
	applyFileConfig(&cfg, fileConfig{DockerSandbox: &fileDockerSandboxConfig{}})
	if cfg.DockerSandbox.CLIPath != "/opt/sbx" || cfg.DockerSandbox.Agent != "shell" || cfg.DockerSandbox.Template != "ubuntu" || cfg.DockerSandbox.CPUs != 2 || cfg.DockerSandbox.Memory != "4g" || !cfg.DockerSandbox.Clone || cfg.DockerSandbox.Workdir != "/workspace/my-app" {
		t.Fatalf("empty file dockerSandbox config cleared existing scalar values: %#v", cfg.DockerSandbox)
	}
	if strings.Join(cfg.DockerSandbox.ExtraWorkspaces, ",") != "/tmp/extra" || strings.Join(cfg.DockerSandbox.MCP, ",") != "context7" || strings.Join(cfg.DockerSandbox.Kit, ",") != "example-org/base" {
		t.Fatalf("empty file dockerSandbox config cleared existing list values: %#v", cfg.DockerSandbox)
	}
}

func TestDockerSandboxFileConfigCanClearInheritedLists(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	cfg.DockerSandbox.ExtraWorkspaces = []string{"/tmp/inherited"}
	cfg.DockerSandbox.MCP = []string{"context7"}
	cfg.DockerSandbox.Kit = []string{"example-org/base"}

	var file fileConfig
	if err := yaml.Unmarshal([]byte(`
dockerSandbox:
  extraWorkspaces: []
  mcp: []
  kit: []
`), &file); err != nil {
		t.Fatal(err)
	}
	if err := applyFileConfig(&cfg, file); err != nil {
		t.Fatalf("applyFileConfig err=%v", err)
	}
	if len(cfg.DockerSandbox.ExtraWorkspaces) != 0 || len(cfg.DockerSandbox.MCP) != 0 || len(cfg.DockerSandbox.Kit) != 0 {
		t.Fatalf("repo dockerSandbox empty lists did not clear inherited values: %#v", cfg.DockerSandbox)
	}
}

func TestDockerSandboxFileConfigCanClearInheritedRuntimeDefaults(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	cfg.DockerSandbox.Template = "ubuntu"
	cfg.DockerSandbox.CPUs = 4
	cfg.DockerSandbox.Memory = "8g"
	cfg.DockerSandbox.Workdir = "/workspace/inherited"

	var file fileConfig
	if err := yaml.Unmarshal([]byte(`
dockerSandbox:
  template: ""
  cpus: 0
  memory: ""
  workdir: ""
`), &file); err != nil {
		t.Fatal(err)
	}
	if err := applyFileConfig(&cfg, file); err != nil {
		t.Fatalf("applyFileConfig err=%v", err)
	}
	if cfg.DockerSandbox.Template != "" || cfg.DockerSandbox.CPUs != 0 || cfg.DockerSandbox.Memory != "" || cfg.DockerSandbox.Workdir != "" {
		t.Fatalf("repo dockerSandbox runtime defaults did not clear inherited values: %#v", cfg.DockerSandbox)
	}
}

func TestDockerSandboxFileConfigRejectsNegativeCPUs(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	var file fileConfig
	if err := yaml.Unmarshal([]byte(`
provider: docker-sandbox
dockerSandbox:
  cpus: -1
`), &file); err != nil {
		t.Fatal(err)
	}
	err := applyFileConfig(&cfg, file)
	if err == nil {
		t.Fatal("applyConfigFile err=<nil>, want negative dockerSandbox cpus rejection")
	}
	if !strings.Contains(err.Error(), "docker-sandbox cpus must be non-negative") {
		t.Fatalf("applyConfigFile err=%v, want negative dockerSandbox cpus rejection", err)
	}
}

func TestDockerSandboxConfigAcceptsMCPFromFileAndEnv(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	var file fileConfig
	if err := yaml.Unmarshal([]byte(`
provider: docker-sandbox
dockerSandbox:
  mcp:
    - context7
    - all
`), &file); err != nil {
		t.Fatal(err)
	}
	err := applyFileConfig(&cfg, file)
	if err != nil {
		t.Fatalf("applyFileConfig mcp err=%v", err)
	}
	if strings.Join(cfg.DockerSandbox.MCP, ",") != "context7,all" {
		t.Fatalf("applyFileConfig mcp cfg=%#v", cfg.DockerSandbox)
	}

	t.Setenv("CRABBOX_DOCKER_SANDBOX_MCP", "one,two")
	err = applyEnv(&cfg)
	if err != nil {
		t.Fatalf("applyEnv mcp err=%v", err)
	}
	if strings.Join(cfg.DockerSandbox.MCP, ",") != "one,two" {
		t.Fatalf("applyEnv mcp cfg=%#v", cfg.DockerSandbox)
	}
}

func TestAnthropicSandboxRuntimeConfigDefaultsFileAndEnv(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	if cfg.AnthropicSRT.CLIPath != "srt" || cfg.AnthropicSRT.Settings != "" || cfg.AnthropicSRT.Debug {
		t.Fatalf("anthropicSandboxRuntime defaults not applied: %#v", cfg.AnthropicSRT)
	}
	settings := ".crabbox/srt-settings.json"
	debug := true
	applyFileConfig(&cfg, fileConfig{
		Provider: "anthropic-sandbox-runtime",
		AnthropicSRT: &fileAnthropicSRTConfig{
			CLIPath:  "/opt/srt",
			Settings: &settings,
			Debug:    &debug,
		},
	})
	if cfg.Provider != "anthropic-sandbox-runtime" || cfg.AnthropicSRT.CLIPath != "/opt/srt" || cfg.AnthropicSRT.Settings != settings || !cfg.AnthropicSRT.Debug {
		t.Fatalf("file anthropicSandboxRuntime config not applied: %#v", cfg.AnthropicSRT)
	}

	t.Setenv("CRABBOX_ANTHROPIC_SANDBOX_RUNTIME_CLI", "/usr/local/bin/srt")
	t.Setenv("CRABBOX_ANTHROPIC_SANDBOX_RUNTIME_SETTINGS", ".crabbox/env-srt-settings.json")
	t.Setenv("CRABBOX_ANTHROPIC_SANDBOX_RUNTIME_DEBUG", "false")
	if err := applyEnv(&cfg); err != nil {
		t.Fatalf("applyEnv err=%v", err)
	}
	if cfg.AnthropicSRT.CLIPath != "/usr/local/bin/srt" || cfg.AnthropicSRT.Settings != ".crabbox/env-srt-settings.json" || cfg.AnthropicSRT.Debug {
		t.Fatalf("env anthropicSandboxRuntime config not applied: %#v", cfg.AnthropicSRT)
	}
}

func TestAnthropicSandboxRuntimeFileConfigCanClearSettings(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	cfg.AnthropicSRT.Settings = ".crabbox/inherited-srt-settings.json"

	var file fileConfig
	if err := yaml.Unmarshal([]byte(`
anthropicSandboxRuntime:
  settings: ""
`), &file); err != nil {
		t.Fatal(err)
	}
	if err := applyFileConfig(&cfg, file); err != nil {
		t.Fatalf("applyFileConfig err=%v", err)
	}
	if cfg.AnthropicSRT.Settings != "" {
		t.Fatalf("settings=%q want cleared", cfg.AnthropicSRT.Settings)
	}
}

func TestAsciiBoxConfigDefaultsFileAndEnv(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	applyFileConfig(&cfg, fileConfig{
		Provider: "ascii-box",
		AsciiBox: &fileAsciiBoxConfig{
			BaseURL: "https://box.example.test",
			CLIPath: "/tmp/box",
			Workdir: "/home/user/project",
		},
	})
	if cfg.Provider != "ascii-box" || cfg.AsciiBox.BaseURL != "https://box.example.test" || cfg.AsciiBox.CLIPath != "/tmp/box" || cfg.AsciiBox.Workdir != "/home/user/project" {
		t.Fatalf("file asciiBox config not applied: %#v", cfg.AsciiBox)
	}

	t.Setenv("ASCII_BOX_API_KEY", "fallback-key")
	t.Setenv("ASCII_BOX_BASE_URL", "https://fallback.example.test")
	t.Setenv("CRABBOX_ASCII_BOX_API_KEY", "override-key")
	t.Setenv("CRABBOX_ASCII_BOX_BASE_URL", "https://override.example.test")
	t.Setenv("CRABBOX_ASCII_BOX_CLI", "/opt/box")
	t.Setenv("CRABBOX_ASCII_BOX_WORKDIR", "/home/user/env-project")
	applyEnv(&cfg)
	if cfg.AsciiBox.APIKey != "override-key" || cfg.AsciiBox.BaseURL != "https://override.example.test" || cfg.AsciiBox.CLIPath != "/opt/box" || cfg.AsciiBox.Workdir != "/home/user/env-project" {
		t.Fatalf("env asciiBox config not applied: %#v", cfg.AsciiBox)
	}
}

func TestAppleContainerConfigDefaultsFileAndEnv(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	if cfg.AppleContainer.CLIPath != "container" || cfg.AppleContainer.User != "crabbox" {
		t.Fatalf("apple container defaults not applied: %#v", cfg.AppleContainer)
	}
	applyFileConfig(&cfg, fileConfig{
		Provider: "apple-container",
		AppleContainer: &fileAppleContainerConfig{
			CLIPath:      "/opt/bin/container",
			Image:        "example-org/my-app:test",
			User:         "runner",
			WorkRoot:     "/work/example",
			CPUs:         4,
			Memory:       "8g",
			ExtraRunArgs: []string{"--mount", "type=virtiofs,source=/tmp,target=/tmp"},
		},
	})
	if cfg.Provider != "apple-container" || cfg.AppleContainer.CLIPath != "/opt/bin/container" || cfg.AppleContainer.Image != "example-org/my-app:test" || cfg.AppleContainer.User != "runner" || cfg.AppleContainer.WorkRoot != "/work/example" || cfg.AppleContainer.CPUs != 4 || cfg.AppleContainer.Memory != "8g" || len(cfg.AppleContainer.ExtraRunArgs) != 2 {
		t.Fatalf("file appleContainer config not applied: %#v", cfg.AppleContainer)
	}

	t.Setenv("CRABBOX_APPLE_CONTAINER_CLI", "/usr/local/bin/container")
	t.Setenv("CRABBOX_APPLE_CONTAINER_IMAGE", "example-org/other:live")
	t.Setenv("CRABBOX_APPLE_CONTAINER_USER", "env-user")
	t.Setenv("CRABBOX_APPLE_CONTAINER_WORK_ROOT", "/work/env")
	t.Setenv("CRABBOX_APPLE_CONTAINER_CPUS", "6")
	t.Setenv("CRABBOX_APPLE_CONTAINER_MEMORY", "12g")
	t.Setenv("CRABBOX_APPLE_CONTAINER_EXTRA_RUN_ARGS", "--dns 1.1.1.1")
	applyEnv(&cfg)
	if cfg.AppleContainer.CLIPath != "/usr/local/bin/container" || cfg.AppleContainer.Image != "example-org/other:live" || cfg.AppleContainer.User != "env-user" || cfg.AppleContainer.WorkRoot != "/work/env" || cfg.AppleContainer.CPUs != 6 || cfg.AppleContainer.Memory != "12g" || len(cfg.AppleContainer.ExtraRunArgs) != 2 {
		t.Fatalf("env appleContainer config not applied: %#v", cfg.AppleContainer)
	}
}

func TestAppleVZConfigDefaultsFileAndEnv(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	cfg.Provider = "apple-vz"
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.AppleVZ.User != "crabbox" || cfg.AppleVZ.WorkRoot != "/work/crabbox" || cfg.AppleVZ.CPUs != 4 || cfg.AppleVZ.MemoryMiB != 8192 || cfg.AppleVZ.DiskGiB != 30 {
		t.Fatalf("apple-vz defaults not applied: %#v", cfg.AppleVZ)
	}
	if cfg.AppleVZ.ImageSHA256 == "" {
		t.Fatalf("apple-vz default image checksum not applied: %#v", cfg.AppleVZ)
	}
	if cfg.SSHUser != "crabbox" || cfg.SSHPort != "22" || cfg.WorkRoot != "/work/crabbox" || cfg.TargetOS != targetLinux {
		t.Fatalf("apple-vz derived defaults not applied: sshUser=%q sshPort=%q workRoot=%q target=%q", cfg.SSHUser, cfg.SSHPort, cfg.WorkRoot, cfg.TargetOS)
	}
	fileCPUs := 6
	fileMemoryMiB := 12288
	fileDiskGiB := 64
	applyFileConfig(&cfg, fileConfig{
		Provider: "apple-vz",
		AppleVZ: &fileAppleVZConfig{
			HelperPath:  "/opt/bin/crabbox-apple-vz-helper",
			Image:       "https://example.test/custom.img",
			ImageSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			User:        "runner",
			WorkRoot:    "/work/example",
			CPUs:        &fileCPUs,
			MemoryMiB:   &fileMemoryMiB,
			DiskGiB:     &fileDiskGiB,
		},
	})
	if cfg.Provider != "apple-vz" || cfg.AppleVZ.HelperPath != "/opt/bin/crabbox-apple-vz-helper" || cfg.AppleVZ.Image != "https://example.test/custom.img" || cfg.AppleVZ.ImageSHA256 != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || cfg.AppleVZ.User != "runner" || cfg.AppleVZ.WorkRoot != "/work/example" || cfg.AppleVZ.CPUs != 6 || cfg.AppleVZ.MemoryMiB != 12288 || cfg.AppleVZ.DiskGiB != 64 {
		t.Fatalf("file appleVZ config not applied: %#v", cfg.AppleVZ)
	}
	if !AppleVZCPUsExplicit(cfg) || !AppleVZMemoryExplicit(cfg) || !AppleVZDiskExplicit(cfg) {
		t.Fatal("file appleVZ numeric settings should be marked explicit")
	}

	t.Setenv("CRABBOX_APPLE_VZ_HELPER", "/usr/local/bin/crabbox-apple-vz-helper")
	t.Setenv("CRABBOX_APPLE_VZ_IMAGE", "https://example.test/env.img")
	t.Setenv("CRABBOX_APPLE_VZ_IMAGE_SHA256", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	t.Setenv("CRABBOX_APPLE_VZ_USER", "env-user")
	t.Setenv("CRABBOX_APPLE_VZ_WORK_ROOT", "/work/env")
	t.Setenv("CRABBOX_APPLE_VZ_CPUS", "8")
	t.Setenv("CRABBOX_APPLE_VZ_MEMORY", "16384")
	t.Setenv("CRABBOX_APPLE_VZ_DISK", "80")
	applyEnv(&cfg)
	if cfg.AppleVZ.HelperPath != "/usr/local/bin/crabbox-apple-vz-helper" || cfg.AppleVZ.Image != "https://example.test/env.img" || cfg.AppleVZ.ImageSHA256 != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" || cfg.AppleVZ.User != "env-user" || cfg.AppleVZ.WorkRoot != "/work/env" || cfg.AppleVZ.CPUs != 8 || cfg.AppleVZ.MemoryMiB != 16384 || cfg.AppleVZ.DiskGiB != 80 {
		t.Fatalf("env appleVZ config not applied: %#v", cfg.AppleVZ)
	}
	if !AppleVZCPUsExplicit(cfg) || !AppleVZMemoryExplicit(cfg) || !AppleVZDiskExplicit(cfg) {
		t.Fatal("env appleVZ numeric settings should be marked explicit")
	}
}

func TestAppleVZNumericSettingsPreserveExplicitZero(t *testing.T) {
	clearConfigEnv(t)
	fileZeroCPUs := 0
	fileZero := 0
	fileZeroDisk := 0
	cfg := baseConfig()
	applyFileConfig(&cfg, fileConfig{AppleVZ: &fileAppleVZConfig{
		CPUs:      &fileZeroCPUs,
		MemoryMiB: &fileZero,
		DiskGiB:   &fileZeroDisk,
	}})
	if cfg.AppleVZ.CPUs != 0 || cfg.AppleVZ.MemoryMiB != 0 || cfg.AppleVZ.DiskGiB != 0 ||
		!AppleVZCPUsExplicit(cfg) || !AppleVZMemoryExplicit(cfg) || !AppleVZDiskExplicit(cfg) {
		t.Fatalf("file appleVZ=%+v explicit=%v/%v/%v", cfg.AppleVZ, AppleVZCPUsExplicit(cfg), AppleVZMemoryExplicit(cfg), AppleVZDiskExplicit(cfg))
	}

	cfg = baseConfig()
	t.Setenv("CRABBOX_APPLE_VZ_CPUS", "0")
	t.Setenv("CRABBOX_APPLE_VZ_MEMORY", "0")
	t.Setenv("CRABBOX_APPLE_VZ_DISK", "0")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.AppleVZ.CPUs != 0 || cfg.AppleVZ.MemoryMiB != 0 || cfg.AppleVZ.DiskGiB != 0 ||
		!AppleVZCPUsExplicit(cfg) || !AppleVZMemoryExplicit(cfg) || !AppleVZDiskExplicit(cfg) {
		t.Fatalf("env appleVZ=%+v explicit=%v/%v/%v", cfg.AppleVZ, AppleVZCPUsExplicit(cfg), AppleVZMemoryExplicit(cfg), AppleVZDiskExplicit(cfg))
	}
}

func TestAppleVZNumericSettingsRejectInvalidEnvironmentValues(t *testing.T) {
	for _, name := range []string{"CRABBOX_APPLE_VZ_CPUS", "CRABBOX_APPLE_VZ_MEMORY", "CRABBOX_APPLE_VZ_DISK"} {
		t.Run(name, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			t.Setenv(name, "garbage")
			if err := applyEnv(&cfg); err == nil || !strings.Contains(err.Error(), name+" must be an integer") {
				t.Fatalf("applyEnv error=%v", err)
			}
		})
	}
}

func TestAppleVZConfigDefaultsRedactSignedImageServerType(t *testing.T) {
	for _, image := range []string{
		"https://alice:secret@example.test/images/ubuntu.img?token=private#fragment",
		"HTTPS://alice:secret@example.test/images/ubuntu.img?token=private#fragment",
	} {
		cfg := baseConfig()
		cfg.Provider = "apple-vz"
		cfg.AppleVZ.Image = image
		cfg.AppleVZ.ImageSHA256 = strings.Repeat("a", 64)

		if err := applyProviderConfigDefaults(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.ServerType != "<remote-image>" {
			t.Fatalf("ServerType=%q", cfg.ServerType)
		}
		if !strings.Contains(cfg.AppleVZ.Image, "token=private") {
			t.Fatalf("AppleVZ.Image should retain the request URL in memory: %q", cfg.AppleVZ.Image)
		}
	}
}

func TestMultipassConfigDefaultsFileAndEnv(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	if cfg.Multipass.CLIPath != "multipass" || cfg.Multipass.Image != "26.04" || cfg.Multipass.User != "crabbox" {
		t.Fatalf("multipass defaults not applied: %#v", cfg.Multipass)
	}
	applyFileConfig(&cfg, fileConfig{
		Provider: "multipass",
		Multipass: &fileMultipassConfig{
			CLIPath:       "/opt/bin/multipass",
			Image:         "24.04",
			User:          "runner",
			WorkRoot:      "/work/example",
			CPUs:          4,
			Memory:        "8G",
			Disk:          "40G",
			LaunchTimeout: "7m",
		},
	})
	if cfg.Provider != "multipass" || cfg.Multipass.CLIPath != "/opt/bin/multipass" || cfg.Multipass.Image != "24.04" || cfg.Multipass.User != "runner" || cfg.Multipass.WorkRoot != "/work/example" || cfg.Multipass.CPUs != 4 || cfg.Multipass.Memory != "8G" || cfg.Multipass.Disk != "40G" || cfg.Multipass.LaunchTimeout != 7*time.Minute {
		t.Fatalf("file multipass config not applied: %#v", cfg.Multipass)
	}

	t.Setenv("CRABBOX_MULTIPASS_CLI", "/usr/local/bin/multipass")
	t.Setenv("CRABBOX_MULTIPASS_IMAGE", "26.04")
	t.Setenv("CRABBOX_MULTIPASS_USER", "env-user")
	t.Setenv("CRABBOX_MULTIPASS_WORK_ROOT", "/work/env")
	t.Setenv("CRABBOX_MULTIPASS_CPUS", "6")
	t.Setenv("CRABBOX_MULTIPASS_MEMORY", "12G")
	t.Setenv("CRABBOX_MULTIPASS_DISK", "80G")
	t.Setenv("CRABBOX_MULTIPASS_LAUNCH_TIMEOUT", "11m")
	applyEnv(&cfg)
	if cfg.Multipass.CLIPath != "/usr/local/bin/multipass" || cfg.Multipass.Image != "26.04" || cfg.Multipass.User != "env-user" || cfg.Multipass.WorkRoot != "/work/env" || cfg.Multipass.CPUs != 6 || cfg.Multipass.Memory != "12G" || cfg.Multipass.Disk != "80G" || cfg.Multipass.LaunchTimeout != 11*time.Minute {
		t.Fatalf("env multipass config not applied: %#v", cfg.Multipass)
	}
}

func TestTartConfigDefaultsFileAndEnv(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	cfg.Provider = "tart"
	cfg.Tart.Image = "ghcr.io/test:latest"
	cfg.Tart.User = "admin"
	cfg.Tart.WorkRoot = "/Users/admin/work"
	cfg.Tart.CPUs = 4
	cfg.Tart.Memory = 8192
	cfg.Tart.Disk = 50
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.SSHUser != "admin" {
		t.Fatalf("SSHUser=%q, want admin", cfg.SSHUser)
	}
	if cfg.SSHPort != "22" {
		t.Fatalf("SSHPort=%q, want 22", cfg.SSHPort)
	}
	if cfg.SSHFallbackPorts != nil {
		t.Fatalf("SSHFallbackPorts=%v, want nil", cfg.SSHFallbackPorts)
	}
	if cfg.WorkRoot != "/Users/admin/work" {
		t.Fatalf("WorkRoot=%q, want /Users/admin/work", cfg.WorkRoot)
	}
	if cfg.TargetOS != "macos" {
		t.Fatalf("TargetOS=%q, want macos", cfg.TargetOS)
	}
	if cfg.ServerType != "ghcr.io/test:latest" {
		t.Fatalf("ServerType=%q, want ghcr.io/test:latest", cfg.ServerType)
	}

	// env overrides
	t.Setenv("CRABBOX_TART_IMAGE", "ghcr.io/env:latest")
	t.Setenv("CRABBOX_TART_USER", "env-user")
	t.Setenv("CRABBOX_TART_WORK_ROOT", "/work/env")
	t.Setenv("CRABBOX_TART_CPUS", "8")
	t.Setenv("CRABBOX_TART_MEMORY", "16384")
	t.Setenv("CRABBOX_TART_DISK", "100")
	applyEnv(&cfg)
	if cfg.Tart.Image != "ghcr.io/env:latest" || cfg.Tart.User != "env-user" || cfg.Tart.WorkRoot != "/work/env" || cfg.Tart.CPUs != 8 || cfg.Tart.Memory != 16384 || cfg.Tart.Disk != 100 {
		t.Fatalf("env tart config not applied: %+v", cfg.Tart)
	}
	if !cfg.tartDiskExplicit {
		t.Fatal("positive CRABBOX_TART_DISK should mark tart disk explicit")
	}
	t.Setenv("CRABBOX_TART_DISK", "0")
	applyEnv(&cfg)
	if cfg.Tart.Disk != 0 {
		t.Fatalf("zero CRABBOX_TART_DISK disk=%d, want clone default 0", cfg.Tart.Disk)
	}
	if cfg.tartDiskExplicit {
		t.Fatal("zero CRABBOX_TART_DISK should not mark tart disk explicit")
	}
}

func TestIncusConfigDefaultsFileAndEnv(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	if cfg.Incus.Remote != "local" || cfg.Incus.Project != "" || cfg.Incus.InstanceType != "container" || cfg.Incus.Image != "images:ubuntu/24.04/cloud" {
		t.Fatalf("incus defaults not applied: %#v", cfg.Incus)
	}
	deleteOnRelease := false
	insecureTLS := true
	applyFileConfig(&cfg, fileConfig{
		Provider: "incus",
		Incus: &fileIncusConfig{
			Remote:            "lab",
			Project:           "crabbox",
			Address:           "https://incus.example.test:8443",
			Socket:            "~/incus.sock",
			InstanceType:      "vm",
			Image:             "images:ubuntu/26.04/cloud",
			Profile:           "crabbox",
			User:              "ubuntu",
			WorkRoot:          "/workspace/incus",
			DeleteOnRelease:   &deleteOnRelease,
			StartTimeout:      "12m",
			LaunchPort:        "22",
			ProxyListenHost:   "127.0.0.1",
			ProxyListenPort:   "2201",
			ProxyDevice:       "ssh-proxy",
			TLSServerCert:     "~/certs/incus.crt",
			InsecureTLS:       &insecureTLS,
			RemoteImageServer: "https://images.example.test",
		},
	})
	if cfg.Incus.Remote != "lab" || cfg.Incus.Project != "crabbox" || cfg.Incus.Address != "https://incus.example.test:8443" || !strings.HasSuffix(cfg.Incus.Socket, "/incus.sock") {
		t.Fatalf("file incus config not applied: %#v", cfg.Incus)
	}
	if cfg.Incus.InstanceType != "vm" || cfg.Incus.Image != "images:ubuntu/26.04/cloud" || cfg.Incus.Profile != "crabbox" || cfg.Incus.User != "ubuntu" || cfg.Incus.WorkRoot != "/workspace/incus" {
		t.Fatalf("file incus identity config not applied: %#v", cfg.Incus)
	}
	if cfg.Incus.DeleteOnRelease || cfg.Incus.StartTimeout != 12*time.Minute || cfg.Incus.ProxyListenPort != "2201" || cfg.Incus.ProxyDevice != "ssh-proxy" || !strings.HasSuffix(cfg.Incus.TLSServerCert, "/certs/incus.crt") || !cfg.Incus.InsecureTLS || cfg.Incus.RemoteImageServer != "https://images.example.test" {
		t.Fatalf("file incus runtime config not applied: %#v", cfg.Incus)
	}

	t.Setenv("CRABBOX_INCUS_REMOTE", "env-remote")
	t.Setenv("CRABBOX_INCUS_PROJECT", "env-project")
	t.Setenv("CRABBOX_INCUS_ADDRESS", "https://env-incus.example.test:8443")
	t.Setenv("CRABBOX_INCUS_SOCKET", "~/env-incus.sock")
	t.Setenv("CRABBOX_INCUS_INSTANCE_TYPE", "container")
	t.Setenv("CRABBOX_INCUS_IMAGE", "images:debian/12/cloud")
	t.Setenv("CRABBOX_INCUS_PROFILE", "env-profile")
	t.Setenv("CRABBOX_INCUS_USER", "crabuser")
	t.Setenv("CRABBOX_INCUS_WORK_ROOT", "/env/work")
	t.Setenv("CRABBOX_INCUS_DELETE_ON_RELEASE", "true")
	t.Setenv("CRABBOX_INCUS_START_TIMEOUT", "5m")
	t.Setenv("CRABBOX_INCUS_LAUNCH_PORT", "2222")
	t.Setenv("CRABBOX_INCUS_PROXY_LISTEN_HOST", "0.0.0.0")
	t.Setenv("CRABBOX_INCUS_PROXY_LISTEN_PORT", "2223")
	t.Setenv("CRABBOX_INCUS_PROXY_DEVICE", "env-proxy")
	t.Setenv("CRABBOX_INCUS_TLS_SERVER_CERT", "~/env-incus.crt")
	t.Setenv("CRABBOX_INCUS_INSECURE_TLS", "false")
	t.Setenv("CRABBOX_INCUS_REMOTE_IMAGE_SERVER", "https://env-images.example.test")
	if err := applyEnv(&cfg); err != nil {
		t.Fatalf("applyEnv err=%v", err)
	}
	if cfg.Incus.Remote != "env-remote" || cfg.Incus.Project != "env-project" || cfg.Incus.Address != "https://env-incus.example.test:8443" || !strings.HasSuffix(cfg.Incus.Socket, "/env-incus.sock") {
		t.Fatalf("env incus config not applied: %#v", cfg.Incus)
	}
	if cfg.Incus.InstanceType != "container" || cfg.Incus.Image != "images:debian/12/cloud" || cfg.Incus.Profile != "env-profile" || cfg.Incus.User != "crabuser" || cfg.Incus.WorkRoot != "/env/work" {
		t.Fatalf("env incus identity config not applied: %#v", cfg.Incus)
	}
	if !cfg.Incus.DeleteOnRelease || cfg.Incus.StartTimeout != 5*time.Minute || cfg.Incus.LaunchPort != "2222" || cfg.Incus.ProxyListenPort != "2223" || cfg.Incus.ProxyDevice != "env-proxy" || !strings.HasSuffix(cfg.Incus.TLSServerCert, "/env-incus.crt") || cfg.Incus.InsecureTLS || cfg.Incus.RemoteImageServer != "https://env-images.example.test" {
		t.Fatalf("env incus runtime config not applied: %#v", cfg.Incus)
	}
}

func TestLoadConfigIncusPreservesExplicitTopLevelSSHUserAndWorkRoot(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	cfgPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	body := "provider: incus\nssh:\n  user: alice\nworkRoot: /tmp/custom\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SSHUser != "alice" {
		t.Fatalf("SSHUser=%q want alice", cfg.SSHUser)
	}
	if cfg.WorkRoot != "/tmp/custom" {
		t.Fatalf("WorkRoot=%q want /tmp/custom", cfg.WorkRoot)
	}
}

func TestLoadConfigIncusPreservesExplicitTopLevelSSHUserAndWorkRootFromEnv(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	cfgPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	t.Setenv("CRABBOX_SSH_USER", "alice")
	t.Setenv("CRABBOX_WORK_ROOT", "/tmp/custom")
	if err := os.WriteFile(cfgPath, []byte("provider: incus\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SSHUser != "alice" {
		t.Fatalf("SSHUser=%q want alice", cfg.SSHUser)
	}
	if cfg.WorkRoot != "/tmp/custom" {
		t.Fatalf("WorkRoot=%q want /tmp/custom", cfg.WorkRoot)
	}
}

func TestLoadConfigIncusSpecificUserAndWorkRootOverrideTopLevel(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	cfgPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	body := "provider: incus\nssh:\n  user: alice\nworkRoot: /tmp/custom\nincus:\n  user: ubuntu\n  workRoot: /workspace/incus\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.SSHUser != "ubuntu" {
		t.Fatalf("SSHUser=%q want ubuntu", cfg.SSHUser)
	}
	if cfg.WorkRoot != "/workspace/incus" {
		t.Fatalf("WorkRoot=%q want /workspace/incus", cfg.WorkRoot)
	}
}

func TestTartConfigYAMLExplicitZeroPreserved(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	file := fileConfig{}
	zero := 0
	negative := -1
	file.Tart = &fileTartConfig{
		CPUs:   &zero,
		Memory: &zero,
		Disk:   &negative,
	}
	if err := applyFileConfig(&cfg, file); err != nil {
		t.Fatal(err)
	}
	if cfg.Tart.CPUs != 0 {
		t.Fatalf("Tart.CPUs=%d, want 0 (explicit YAML zero must be preserved)", cfg.Tart.CPUs)
	}
	if cfg.Tart.Memory != 0 {
		t.Fatalf("Tart.Memory=%d, want 0 (explicit YAML zero must be preserved)", cfg.Tart.Memory)
	}
	if cfg.Tart.Disk != -1 {
		t.Fatalf("Tart.Disk=%d, want -1 (explicit YAML negative must be preserved)", cfg.Tart.Disk)
	}
	if !IsTartCPUsExplicit(&cfg) {
		t.Fatal("tartCPUsExplicit must be true after YAML sets cpus")
	}
	if !IsTartMemoryExplicit(&cfg) {
		t.Fatal("tartMemoryExplicit must be true after YAML sets memory")
	}
}

func TestTartConfigYAMLMissingFieldsNotOverwritten(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	cfg.Tart.CPUs = 8
	cfg.Tart.Memory = 16384
	file := fileConfig{}
	file.Tart = &fileTartConfig{
		Image: "ghcr.io/test:latest",
	}
	if err := applyFileConfig(&cfg, file); err != nil {
		t.Fatal(err)
	}
	if cfg.Tart.CPUs != 8 {
		t.Fatalf("Tart.CPUs=%d, want 8 (missing YAML field must not overwrite)", cfg.Tart.CPUs)
	}
	if cfg.Tart.Memory != 16384 {
		t.Fatalf("Tart.Memory=%d, want 16384 (missing YAML field must not overwrite)", cfg.Tart.Memory)
	}
}

func TestOpenComputerConfigYAMLExplicitZeroPreserved(t *testing.T) {
	cfg := baseConfig()
	cfg.OpenComputer.CPU = 8
	cfg.OpenComputer.MemoryMB = 16384
	cfg.OpenComputer.TimeoutSecs = 600
	cfg.OpenComputer.ExecTimeoutSecs = 7200
	zero := 0
	file := fileConfig{OpenComputer: &fileOpenComputerConfig{
		CPU:             &zero,
		MemoryMB:        &zero,
		TimeoutSecs:     &zero,
		ExecTimeoutSecs: &zero,
	}}
	if err := applyFileConfig(&cfg, file); err != nil {
		t.Fatal(err)
	}
	if cfg.OpenComputer.CPU != 0 || cfg.OpenComputer.MemoryMB != 0 || cfg.OpenComputer.TimeoutSecs != 0 || cfg.OpenComputer.ExecTimeoutSecs != 0 {
		t.Fatalf("explicit zero values not preserved: %#v", cfg.OpenComputer)
	}
}

func TestOpenComputerConfigYAMLCannotSetAPIURL(t *testing.T) {
	cfg := baseConfig()
	var file fileConfig
	if err := yaml.Unmarshal([]byte("openComputer:\n  apiUrl: https://attacker.example\n"), &file); err != nil {
		t.Fatal(err)
	}
	if err := applyFileConfig(&cfg, file); err != nil {
		t.Fatal(err)
	}
	if cfg.OpenComputer.APIURL != "" {
		t.Fatalf("repository config set OpenComputer API URL to %q", cfg.OpenComputer.APIURL)
	}
}

func TestOpenComputerBurstConfigYAMLAndEnv(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	var file fileConfig
	if err := yaml.Unmarshal([]byte("openComputer:\n  burst: true\n"), &file); err != nil {
		t.Fatal(err)
	}
	if err := applyFileConfig(&cfg, file); err != nil {
		t.Fatal(err)
	}
	if !cfg.OpenComputer.Burst {
		t.Fatal("openComputer.burst YAML was not applied")
	}

	cfg.OpenComputer.Burst = false
	t.Setenv("CRABBOX_OPENCOMPUTER_BURST", "true")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.OpenComputer.Burst {
		t.Fatal("CRABBOX_OPENCOMPUTER_BURST was not applied")
	}
}

func TestTartEnvExplicitFlags(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	t.Setenv("CRABBOX_TART_CPUS", "8")
	t.Setenv("CRABBOX_TART_MEMORY", "16384")
	applyEnv(&cfg)
	if !IsTartCPUsExplicit(&cfg) {
		t.Fatal("tartCPUsExplicit must be true after env sets CRABBOX_TART_CPUS")
	}
	if !IsTartMemoryExplicit(&cfg) {
		t.Fatal("tartMemoryExplicit must be true after env sets CRABBOX_TART_MEMORY")
	}
}

func TestRepoConfigBareEnvWildcardDoesNotForwardEveryLocalVariable(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	repo := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_PROVIDER", "")
	t.Setenv("CRABBOX_DEFAULT_CLASS", "")
	t.Setenv("CRABBOX_PROOF_API_TOKEN", "critical-secret-value")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(cwd); err != nil {
			t.Fatal(err)
		}
	}()
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(".crabbox.yaml", []byte("env:\n  allow:\n    - '*'\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := allowedEnv(cfg.EnvAllow); got["CRABBOX_PROOF_API_TOKEN"] != "" {
		t.Fatalf("bare wildcard forwarded proof secret: %q", got["CRABBOX_PROOF_API_TOKEN"])
	}
}

func TestProfileEnvConfigYAMLShape(t *testing.T) {
	var env fileProfileEnvConfig
	if err := yaml.Unmarshal([]byte("CI: 1\nNODE_OPTIONS: --max-old-space-size=4096\nallow:\n  - CUSTOM_*\n"), &env); err != nil {
		t.Fatal(err)
	}
	if env.Values["CI"] != "1" || env.Values["NODE_OPTIONS"] != "--max-old-space-size=4096" {
		t.Fatalf("profile env values not decoded: %#v", env.Values)
	}
	if len(env.Allow) != 1 || env.Allow[0] != "CUSTOM_*" {
		t.Fatalf("profile env allow not decoded: %#v", env.Allow)
	}
	data, err := yaml.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	for _, want := range []string{"CI: \"1\"", "NODE_OPTIONS: --max-old-space-size=4096", "allow:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("marshaled profile env missing %q:\n%s", want, out)
		}
	}
}

func TestProfileEnvConfigYAMLRejectsNonMapping(t *testing.T) {
	var env fileProfileEnvConfig
	err := yaml.Unmarshal([]byte("- CI=1\n"), &env)
	if err == nil || !strings.Contains(err.Error(), "profile env must be a mapping") {
		t.Fatalf("error=%v want profile env mapping error", err)
	}
}

func TestRepoConfigClearsInheritedCacheVolumes(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	repo := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	userPath := userConfigPath()
	if err := os.MkdirAll(filepath.Dir(userPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userPath, []byte("cache:\n  volumes:\n    - key: user-cache\n      path: /var/cache/crabbox/user\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(cwd); err != nil {
			t.Fatal(err)
		}
	}()
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(".crabbox.yaml", []byte("cache:\n  volumes: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Cache.Volumes) != 0 {
		t.Fatalf("repo config did not clear inherited cache volumes: %#v", cfg.Cache.Volumes)
	}
}

func TestRepoConfigCannotRedirectInheritedXCPNgCredentials(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	repo := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	userPath := userConfigPath()
	if err := os.MkdirAll(filepath.Dir(userPath), 0o700); err != nil {
		t.Fatal(err)
	}
	userConfig := "provider: xcp-ng\nxcpNg:\n  apiUrl: https://trusted.example.test\n  username: root\n  password: user-secret\n"
	if err := os.WriteFile(userPath, []byte(userConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(cwd); err != nil {
			t.Fatal(err)
		}
	}()
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	projectConfig := "xcpNg:\n  apiUrl: https://attacker.example.test\n  insecureTls: true\n  template: project-template\n"
	if err := os.WriteFile(".crabbox.yaml", []byte(projectConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.XCPNg.APIURL != "https://trusted.example.test" || cfg.XCPNg.InsecureTLS {
		t.Fatalf("project config changed trusted connection: %#v", cfg.XCPNg)
	}
	if cfg.XCPNg.Password != "user-secret" || cfg.XCPNg.Template != "project-template" {
		t.Fatalf("unexpected merged xcp-ng config: %#v", cfg.XCPNg)
	}
}

func TestXCPNgHigherPrecedenceNamesClearInheritedUUIDs(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	cfg.XCPNg.TemplateUUID = "old-template-uuid"
	cfg.XCPNg.SRUUID = "old-sr-uuid"
	cfg.XCPNg.NetworkUUID = "old-network-uuid"
	if err := applyFileConfig(&cfg, fileConfig{XCPNg: &fileXCPNgConfig{
		Template: "new-template",
		SR:       "new-sr",
		Network:  "new-network",
	}}); err != nil {
		t.Fatal(err)
	}
	if cfg.XCPNg.TemplateUUID != "" || cfg.XCPNg.SRUUID != "" || cfg.XCPNg.NetworkUUID != "" {
		t.Fatalf("file names did not clear inherited UUIDs: %#v", cfg.XCPNg)
	}

	cfg.XCPNg.TemplateUUID = "old-template-uuid"
	cfg.XCPNg.SRUUID = "old-sr-uuid"
	cfg.XCPNg.NetworkUUID = "old-network-uuid"
	t.Setenv("CRABBOX_XCP_NG_TEMPLATE", "env-template")
	t.Setenv("CRABBOX_XCP_NG_TEMPLATE_UUID", "")
	t.Setenv("CRABBOX_XCP_NG_SR", "env-sr")
	t.Setenv("CRABBOX_XCP_NG_SR_UUID", "")
	t.Setenv("CRABBOX_XCP_NG_NETWORK", "env-network")
	t.Setenv("CRABBOX_XCP_NG_NETWORK_UUID", "")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.XCPNg.TemplateUUID != "" || cfg.XCPNg.SRUUID != "" || cfg.XCPNg.NetworkUUID != "" {
		t.Fatalf("environment names did not clear inherited UUIDs: %#v", cfg.XCPNg)
	}
}

func TestRepoConfigCannotOverrideFreestyleAPIURL(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	repo := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_PROVIDER", "")
	t.Setenv("CRABBOX_DEFAULT_CLASS", "")
	userPath := userConfigPath()
	if err := os.MkdirAll(filepath.Dir(userPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userPath, []byte("freestyle:\n  apiUrl: https://trusted.example.test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(cwd); err != nil {
			t.Fatal(err)
		}
	}()
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(".crabbox.yaml", []byte("freestyle:\n  apiUrl: https://untrusted.example.test\n  workdir: repo-workdir\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Freestyle.APIURL != "https://trusted.example.test" {
		t.Fatalf("Freestyle.APIURL=%q, want trusted user endpoint", cfg.Freestyle.APIURL)
	}
	if cfg.Freestyle.Workdir != "repo-workdir" {
		t.Fatalf("Freestyle.Workdir=%q, want repository config applied", cfg.Freestyle.Workdir)
	}
}

func TestCacheVolumesOmittedKeepsInheritedConfig(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	cfg.Cache.Volumes = []CacheVolumeConfig{{Key: "user-cache", Path: "/var/cache/crabbox/user"}}
	pnpm := false
	file := fileConfig{Cache: &fileCacheConfig{Pnpm: &pnpm}}
	if err := applyFileConfig(&cfg, file); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Cache.Volumes) != 1 || cfg.Cache.Volumes[0].Key != "user-cache" {
		t.Fatalf("omitted cache volumes should keep inherited value: %#v", cfg.Cache.Volumes)
	}
}

func TestApplyFileParallelsTemplateConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	existing := ParallelsTemplateConfig{
		Source:   "macOS Tahoe",
		TargetOS: targetMacOS,
	}
	got := applyFileParallelsTemplateConfig(existing, fileParallelsTemplateConfig{
		Source:           "Windows 11",
		SourceID:         "{source-id}",
		SourceSnapshot:   "Known Good",
		SourceSnapshotID: "{snapshot-id}",
		Target:           targetLinux,
		TargetOS:         targetWindows,
		WindowsMode:      windowsModeNormal,
		CloneMode:        "linked",
		Host:             "mac.example.test",
		HostUser:         "build",
		HostKey:          "~/keys/parallels",
		VMRoot:           "~/Parallels",
		User:             "runner",
		WorkRoot:         "C:\\crabbox",
	})
	if got.Source != "Windows 11" || got.SourceID != "{source-id}" || got.SourceSnapshot != "Known Good" || got.SourceSnapshotID != "{snapshot-id}" {
		t.Fatalf("source fields=%#v", got)
	}
	if got.TargetOS != targetWindows || got.WindowsMode != windowsModeNormal || got.CloneMode != "linked" {
		t.Fatalf("target fields=%#v", got)
	}
	if got.Host != "mac.example.test" || got.HostUser != "build" || got.User != "runner" || got.WorkRoot != "C:\\crabbox" {
		t.Fatalf("host/user fields=%#v", got)
	}
	if got.HostKey != filepath.Join(home, "keys/parallels") || got.VMRoot != filepath.Join(home, "Parallels") {
		t.Fatalf("expanded paths hostKey=%q vmRoot=%q", got.HostKey, got.VMRoot)
	}
}

func TestApplyFileParallelsHostConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	targets := []string{targetMacOS, targetLinux}
	got := applyFileParallelsHostConfig(fileParallelsHostConfig{
		Name:    " mac-mini ",
		Host:    " mac.example.test ",
		User:    " build ",
		Key:     " ~/keys/fleet ",
		VMRoot:  " ~/Parallels ",
		Targets: targets,
		MaxVMs:  3,
	})
	targets[0] = targetWindows
	if got.Name != "mac-mini" || got.Host != "mac.example.test" || got.User != "build" {
		t.Fatalf("trimmed fields=%#v", got)
	}
	if got.Key != filepath.Join(home, "keys/fleet") || got.VMRoot != filepath.Join(home, "Parallels") {
		t.Fatalf("expanded paths key=%q vmRoot=%q", got.Key, got.VMRoot)
	}
	if got.MaxVMs != 3 || len(got.Targets) != 2 || got.Targets[0] != targetMacOS || got.Targets[1] != targetLinux {
		t.Fatalf("targets/max=%#v", got)
	}
}

func TestParallelsServerTypeForConfig(t *testing.T) {
	if got := parallelsServerTypeForConfig(Config{}); got != "template" {
		t.Fatalf("empty=%q", got)
	}
	if got := parallelsServerTypeForConfig(Config{Parallels: ParallelsConfig{Template: "macOS Tahoe Latest"}}); got != "template-macos-tahoe-latest" {
		t.Fatalf("template=%q", got)
	}
	if got := parallelsServerTypeForConfig(Config{Parallels: ParallelsConfig{SourceID: "{VM-ID}"}}); got != "template-vm-id" {
		t.Fatalf("source id=%q", got)
	}
}

func TestApplyParallelsTemplateConfigSourceIDsAndEmptyName(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "parallels"
	cfg.Parallels.Source = "old-source"
	cfg.Parallels.SourceSnapshot = "old-snapshot"
	cfg.Parallels.Templates = map[string]ParallelsTemplateConfig{
		"ids": {
			SourceID:         "{source-id}",
			SourceSnapshotID: "{snapshot-id}",
			TargetOS:         "windows",
			WindowsMode:      windowsModeNormal,
			CloneMode:        "linked",
			HostKey:          "/keys/fleet",
			VMRoot:           "/vms",
			WorkRoot:         "C:\\work",
		},
	}
	if err := ApplyParallelsTemplateConfig(&cfg, " "); err != nil {
		t.Fatal(err)
	}
	if cfg.Parallels.Template != "" || cfg.parallelsTemplateApplied {
		t.Fatalf("empty name should be no-op: %#v", cfg.Parallels)
	}
	if err := ApplyParallelsTemplateConfig(&cfg, "ids"); err != nil {
		t.Fatal(err)
	}
	if cfg.Parallels.Source != "old-source" || cfg.Parallels.SourceID != "{source-id}" || cfg.Parallels.SourceSnapshot != "old-snapshot" || cfg.Parallels.SourceSnapshotID != "{snapshot-id}" {
		t.Fatalf("source ids=%#v", cfg.Parallels)
	}
	if cfg.TargetOS != targetWindows || cfg.WindowsMode != windowsModeNormal || cfg.WorkRoot != "C:\\work" {
		t.Fatalf("defaults target=%s windows=%s work=%s", cfg.TargetOS, cfg.WindowsMode, cfg.WorkRoot)
	}
	if cfg.Parallels.CloneMode != "linked" || cfg.Parallels.HostKey != "/keys/fleet" || cfg.Parallels.VMRoot != "/vms" || !cfg.parallelsTemplateApplied {
		t.Fatalf("template fields=%#v applied=%v", cfg.Parallels, cfg.parallelsTemplateApplied)
	}
}

func TestLoadConfigFromUserFile(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_PROVIDER", "")
	t.Setenv("CRABBOX_DEFAULT_CLASS", "")
	path := userConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`broker:
  url: https://crabbox.example.test
  mode: registered
  autoWebVNC: false
  token: secret
  adminToken: admin-secret
  provider: aws
  access:
    clientId: access-client
    clientSecret: access-secret
    token: access-jwt
class: standard
target: windows
hostId: h-neutral-file
windows:
  mode: wsl2
lease:
  ttl: 2h
  idleTimeout: 45m
aws:
  region: eu-west-1
  rootGB: 800
  sshCIDRs:
    - 198.51.100.7/32
sync:
  checksum: true
  gitSeed: false
  baseRef: trunk
  timeout: 30m
  warnFiles: 100
  warnBytes: 200
  failFiles: 300
  failBytes: 400
  allowLarge: true
  exclude:
    - .artifacts
    - tmp
env:
  allow:
    - CI
    - NODE_OPTIONS
    - CUSTOM_*
capacity:
  market: spot
  strategy: most-available
  fallback: on-demand-after-120s
  hints: false
  regions:
    - eu-west-1
actions:
  repo: openclaw/crabbox
  workflow: .github/workflows/crabbox.yml
  job: hydrate
  ref: main
  fields:
    - crabbox_docker_cache=true
    - crabbox_prepare_images=1
  runnerLabels:
    - crabbox
    - linux-large
  runnerVersion: latest
  ephemeral: false
blacksmith:
  org: openclaw
  workflow: .github/workflows/blacksmith-testbox.yml
  job: hydrate
  ref: main
  idleTimeout: 90m
  debug: true
namespace:
  image: crabbox-ready
  size: L
  repository: github.com/openclaw/crabbox
  site: fra1
  volumeSizeGB: 120
  autoStopIdleTimeout: 1h
  workRoot: /workspaces/test
  deleteOnRelease: true
morph:
  apiKey: morph-file-key
  apiUrl: https://morph.example.test
  snapshot: snapshot-file
  sshGatewayHost: ssh.morph.example.test
  workRoot: /tmp/morph-test
  deleteOnRelease: true
  wakeOnSSH: false
daytona:
  apiUrl: https://daytona.example.test/api
  snapshot: crabbox-ready
  target: us
  user: daytona
  workRoot: /home/daytona/crabbox
  sshGatewayHost: ssh.daytona.example.test
  sshAccessMinutes: 12
azureDynamicSessions:
  endpoint: https://pool.env.eastus.azurecontainerapps.io
  pool: pool
  apiVersion: 2025-02-02-preview
  workdir: /workspace/file
  timeoutSecs: 120
e2b:
  apiUrl: https://api.e2b.example.test
  domain: e2b.example.test
  template: crabbox-ready
  workdir: work/repo
  user: sandbox
railway:
  apiUrl: https://railway.example.test/graphql/v2
  projectId: project-file
  environmentId: environment-file
runpod:
  apiUrl: https://runpod.example.test/v1
  cloudType: SECURE
  instanceId: NVIDIA L4
  image: runpod/pytorch:custom
  templateId: tpl-file
  diskGB: 25
  user: runpod-user
  workRoot: /workspaces/runpod-test
islo:
  baseUrl: https://islo.example.test
  image: docker.io/library/ubuntu:24.04
  workdir: crabbox
  gatewayProfile: default
  snapshotName: snap-ready
  vcpus: 4
  memoryMB: 8192
  diskGB: 40
freestyle:
  apiUrl: https://freestyle.example.test
  workdir: team/repo
  vcpus: 4
  memoryGB: 8
tenki:
  cliPath: /usr/local/bin/tenki
  endpoint: https://api.tenki.example.test
  gateway: wss://gateway.tenki.example.test
  workspace: ws_file
  project: proj_file
  image: ubuntu:tenki
  workRoot: /home/tenki/test
  cpus: 4
  memoryMB: 8192
  diskGB: 40
tensorlake:
  apiUrl: https://api.tensorlake.example.test
  cliPath: /usr/local/bin/tl
  image: ubuntu-22.04
  snapshot: snap-tl
  organizationId: org-tl
  projectId: proj-tl
  namespace: ns-tl
  workdir: /workspace/crabbox-test
  cpus: 4
  memoryMB: 8192
  diskMB: 30000
  timeoutSecs: 1800
  noInternet: true
openComputer:
  apiUrl: https://opencomputer.example.test
  workdir: /workspace/oc-test
  cpu: 8
  memoryMB: 16384
  timeoutSecs: 600
  execTimeoutSecs: 7200
openSandbox:
  apiUrl: https://opensandbox-file-ignored.example.test
  image: docker.io/library/python:3.12
  workdir: /workspace/osb-test
  cpu: "2"
  memory: 4Gi
  timeoutSecs: 900
  execTimeoutSecs: 1800
  platformOS: linux
  platformArch: arm64
  secureAccess: true
  useServerProxy: true
cloudflare:
  apiUrl: https://cloudflare.example.test
  token: cloudflare-token
  workdir: /workspace/cf-test
proxmox:
  apiUrl: https://pve.example.test:8006
  tokenId: crabbox@pve!test
  tokenSecret: proxmox-secret
  node: pve1
  templateId: 9000
  storage: local-lvm
  pool: crabbox
  bridge: vmbr1
  user: runner
  workRoot: /work/proxmox
  fullClone: false
  insecureTLS: true
xcpNg:
  apiUrl: https://xcp-ng.example.test
  username: root
  password: xcp-ng-secret
  template: ubuntu-template
  templateUuid: tpl-0001
  sr: default-sr
  srUuid: sr-0001
  network: pool-network
  networkUuid: net-0001
  host: host-0001
  user: runner
  workRoot: /work/xcp-ng
  insecureTLS: true
semaphore:
  host: semaphore.example.test
  token: semaphore-token
  project: crabbox
  machine: f1-standard-4
  osImage: ubuntu2404
  idleTimeout: 15m
sprites:
  apiUrl: https://api.sprites.example.test
  workRoot: /home/sprite/test
static:
  id: win-dev
  name: windows-dev
  host: win-dev.local
  user: peter
  port: "22"
  workRoot: /home/peter/crabbox
results:
  auto: true
  junit:
    - junit.xml
run:
  preflightTools:
    - node
    - bun
cache:
  pnpm: true
  npm: false
  docker: true
  git: true
  maxGB: 120
  purgeOnRelease: true
  volumes:
    - name: pnpm-store
      key: my-app-linux-amd64-node24-pnpm10-lock
      path: /var/cache/crabbox/pnpm
      sizeGB: 80
      required: true
ssh:
  key: ~/.ssh/crabbox
  fallbackPorts:
    - "22"
    - "2022"
`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "aws" {
		t.Fatalf("Provider=%q want aws", cfg.Provider)
	}
	if cfg.TargetOS != targetWindows || cfg.WindowsMode != windowsModeWSL2 {
		t.Fatalf("target config not loaded: target=%s windowsMode=%s", cfg.TargetOS, cfg.WindowsMode)
	}
	if cfg.ServerType != "m8i.large" {
		t.Fatalf("ServerType=%q want m8i.large", cfg.ServerType)
	}
	if cfg.SSHUser != "Administrator" {
		t.Fatalf("SSHUser=%q want Administrator", cfg.SSHUser)
	}
	if cfg.Coordinator != "https://crabbox.example.test" || cfg.CoordToken != "secret" || cfg.CoordAdminToken != "admin-secret" {
		t.Fatalf("broker config not loaded: %#v", cfg)
	}
	if cfg.BrokerMode != BrokerModeRegistered || cfg.BrokerAutoWebVNC {
		t.Fatalf("broker registration config not loaded: mode=%q autoWebVNC=%t", cfg.BrokerMode, cfg.BrokerAutoWebVNC)
	}
	if cfg.HostID != "h-neutral-file" {
		t.Fatalf("host id not loaded: %q", cfg.HostID)
	}
	if cfg.Access.ClientID != "access-client" || cfg.Access.ClientSecret != "access-secret" || cfg.Access.Token != "access-jwt" {
		t.Fatalf("access config not loaded: %#v", cfg.Access)
	}
	if cfg.TTL.String() != "2h0m0s" || cfg.IdleTimeout.String() != "45m0s" {
		t.Fatalf("lease config not loaded: ttl=%s idle=%s", cfg.TTL, cfg.IdleTimeout)
	}
	if cfg.AWSRootGB != 800 {
		t.Fatalf("AWSRootGB=%d want 800", cfg.AWSRootGB)
	}
	if len(cfg.AWSSSHCIDRs) != 1 || cfg.AWSSSHCIDRs[0] != "198.51.100.7/32" {
		t.Fatalf("AWSSSHCIDRs=%v", cfg.AWSSSHCIDRs)
	}
	if cfg.SSHKey != filepath.Join(home, ".ssh", "crabbox") {
		t.Fatalf("SSHKey=%q", cfg.SSHKey)
	}
	if len(cfg.SSHFallbackPorts) != 2 || cfg.SSHFallbackPorts[0] != "22" || cfg.SSHFallbackPorts[1] != "2022" {
		t.Fatalf("SSHFallbackPorts=%v", cfg.SSHFallbackPorts)
	}
	if !cfg.Sync.Checksum || cfg.Sync.GitSeed || cfg.Sync.BaseRef != "trunk" {
		t.Fatalf("sync config not loaded: %#v", cfg.Sync)
	}
	if cfg.Sync.Timeout.String() != "30m0s" || cfg.Sync.WarnFiles != 100 || cfg.Sync.WarnBytes != 200 || cfg.Sync.FailFiles != 300 || cfg.Sync.FailBytes != 400 || !cfg.Sync.AllowLarge {
		t.Fatalf("sync guardrails not loaded: %#v", cfg.Sync)
	}
	if len(cfg.Sync.Excludes) != 2 || cfg.Sync.Excludes[0] != ".artifacts" || cfg.Sync.Excludes[1] != "tmp" {
		t.Fatalf("sync excludes not loaded: %#v", cfg.Sync.Excludes)
	}
	if len(cfg.EnvAllow) != 3 || cfg.EnvAllow[2] != "CUSTOM_*" {
		t.Fatalf("env allow not loaded: %#v", cfg.EnvAllow)
	}
	if cfg.Capacity.Strategy != "most-available" || cfg.Capacity.Hints || len(cfg.Capacity.Regions) != 1 || cfg.Capacity.Regions[0] != "eu-west-1" {
		t.Fatalf("capacity config not loaded: %#v", cfg.Capacity)
	}
	if cfg.Actions.Repo != "openclaw/crabbox" || cfg.Actions.Workflow != ".github/workflows/crabbox.yml" || cfg.Actions.Job != "hydrate" || cfg.Actions.Ref != "main" {
		t.Fatalf("actions config not loaded: %#v", cfg.Actions)
	}
	if len(cfg.Actions.Fields) != 2 || cfg.Actions.Fields[0] != "crabbox_docker_cache=true" || cfg.Actions.Fields[1] != "crabbox_prepare_images=1" {
		t.Fatalf("actions fields config not loaded: %#v", cfg.Actions.Fields)
	}
	if cfg.Actions.Ephemeral || len(cfg.Actions.RunnerLabels) != 2 || cfg.Actions.RunnerLabels[1] != "linux-large" {
		t.Fatalf("actions runner config not loaded: %#v", cfg.Actions)
	}
	if cfg.Blacksmith.Org != "openclaw" || cfg.Blacksmith.Workflow != ".github/workflows/blacksmith-testbox.yml" || cfg.Blacksmith.Job != "hydrate" || cfg.Blacksmith.Ref != "main" || cfg.Blacksmith.IdleTimeout != 90*time.Minute || !cfg.Blacksmith.Debug {
		t.Fatalf("blacksmith config not loaded: %#v", cfg.Blacksmith)
	}
	if cfg.Namespace.Image != "crabbox-ready" || cfg.Namespace.Size != "L" || cfg.Namespace.Repository != "github.com/openclaw/crabbox" || cfg.Namespace.Site != "fra1" || cfg.Namespace.VolumeSizeGB != 120 || cfg.Namespace.AutoStopIdleTimeout != time.Hour || cfg.Namespace.WorkRoot != "/workspaces/test" || !cfg.Namespace.DeleteOnRelease {
		t.Fatalf("namespace config not loaded: %#v", cfg.Namespace)
	}
	if cfg.Morph.APIKey != "morph-file-key" || cfg.Morph.APIURL != "https://morph.example.test" || cfg.Morph.Snapshot != "snapshot-file" || cfg.Morph.SSHGatewayHost != "ssh.morph.example.test" || cfg.Morph.WorkRoot != "/tmp/morph-test" || !cfg.Morph.DeleteOnRelease || cfg.Morph.WakeOnSSH {
		t.Fatalf("morph config not loaded: %#v", cfg.Morph)
	}
	if cfg.Daytona.APIURL != "https://daytona.example.test/api" || cfg.Daytona.Snapshot != "crabbox-ready" || cfg.Daytona.Target != "us" || cfg.Daytona.User != "daytona" || cfg.Daytona.WorkRoot != "/home/daytona/crabbox" || cfg.Daytona.SSHGatewayHost != "ssh.daytona.example.test" || cfg.Daytona.SSHAccessMinutes != 12 {
		t.Fatalf("daytona config not loaded: %#v", cfg.Daytona)
	}
	if cfg.AzureDynamicSessions.Endpoint != "https://pool.env.eastus.azurecontainerapps.io" || cfg.AzureDynamicSessions.Pool != "pool" || cfg.AzureDynamicSessions.Workdir != "/workspace/file" || cfg.AzureDynamicSessions.TimeoutSecs != 120 {
		t.Fatalf("azure dynamic sessions config not loaded: %#v", cfg.AzureDynamicSessions)
	}
	if cfg.E2B.APIURL != "https://api.e2b.example.test" || cfg.E2B.Domain != "e2b.example.test" || cfg.E2B.Template != "crabbox-ready" || cfg.E2B.Workdir != "work/repo" || cfg.E2B.User != "sandbox" {
		t.Fatalf("e2b config not loaded: %#v", cfg.E2B)
	}
	if cfg.Railway.APIURL != "https://railway.example.test/graphql/v2" || cfg.Railway.ProjectID != "project-file" || cfg.Railway.EnvironmentID != "environment-file" {
		t.Fatalf("railway config not loaded: %#v", cfg.Railway)
	}
	if cfg.Runpod.APIURL != "https://runpod.example.test/v1" || cfg.Runpod.CloudType != "SECURE" || cfg.Runpod.InstanceID != "NVIDIA L4" || cfg.Runpod.Image != "runpod/pytorch:custom" || cfg.Runpod.TemplateID != "tpl-file" || cfg.Runpod.DiskGB != 25 || cfg.Runpod.User != "runpod-user" || cfg.Runpod.WorkRoot != "/workspaces/runpod-test" {
		t.Fatalf("runpod config not loaded: %#v", cfg.Runpod)
	}
	if cfg.Islo.BaseURL != "https://islo.example.test" || cfg.Islo.Image != "docker.io/library/ubuntu:24.04" || cfg.Islo.Workdir != "crabbox" || cfg.Islo.GatewayProfile != "default" || cfg.Islo.SnapshotName != "snap-ready" || cfg.Islo.VCPUs != 4 || cfg.Islo.MemoryMB != 8192 || cfg.Islo.DiskGB != 40 {
		t.Fatalf("islo config not loaded: %#v", cfg.Islo)
	}
	if cfg.Freestyle.APIURL != "https://freestyle.example.test" || cfg.Freestyle.Workdir != "team/repo" || cfg.Freestyle.VCPUs != 4 || cfg.Freestyle.MemoryGB != 8 {
		t.Fatalf("freestyle config not loaded: %#v", cfg.Freestyle)
	}
	if cfg.Tenki.CLIPath != "/usr/local/bin/tenki" || cfg.Tenki.Endpoint != "https://api.tenki.example.test" || cfg.Tenki.Gateway != "wss://gateway.tenki.example.test" || cfg.Tenki.Workspace != "ws_file" || cfg.Tenki.Project != "proj_file" || cfg.Tenki.Image != "ubuntu:tenki" || cfg.Tenki.WorkRoot != "/home/tenki/test" || cfg.Tenki.CPUs != 4 || cfg.Tenki.MemoryMB != 8192 || cfg.Tenki.DiskGB != 40 {
		t.Fatalf("tenki config not loaded: %#v", cfg.Tenki)
	}
	if cfg.Tensorlake.APIURL != "https://api.tensorlake.example.test" || cfg.Tensorlake.CLIPath != "/usr/local/bin/tl" || cfg.Tensorlake.Image != "ubuntu-22.04" || cfg.Tensorlake.Snapshot != "snap-tl" || cfg.Tensorlake.OrganizationID != "org-tl" || cfg.Tensorlake.ProjectID != "proj-tl" || cfg.Tensorlake.Namespace != "ns-tl" || cfg.Tensorlake.Workdir != "/workspace/crabbox-test" || cfg.Tensorlake.CPUs != 4 || cfg.Tensorlake.MemoryMB != 8192 || cfg.Tensorlake.DiskMB != 30000 || cfg.Tensorlake.TimeoutSecs != 1800 || !cfg.Tensorlake.NoInternet {
		t.Fatalf("tensorlake config not loaded: %#v", cfg.Tensorlake)
	}
	if cfg.OpenComputer.APIURL != "" || cfg.OpenComputer.Workdir != "/workspace/oc-test" || cfg.OpenComputer.CPU != 8 || cfg.OpenComputer.MemoryMB != 16384 || cfg.OpenComputer.TimeoutSecs != 600 || cfg.OpenComputer.ExecTimeoutSecs != 7200 {
		t.Fatalf("opencomputer config not loaded: %#v", cfg.OpenComputer)
	}
	if cfg.OpenSandbox.APIURL != "" || cfg.OpenSandbox.Image != "docker.io/library/python:3.12" || cfg.OpenSandbox.Workdir != "/workspace/osb-test" || cfg.OpenSandbox.CPU != "2" || cfg.OpenSandbox.Memory != "4Gi" || cfg.OpenSandbox.TimeoutSecs != 900 || cfg.OpenSandbox.ExecTimeoutSecs != 1800 || cfg.OpenSandbox.PlatformOS != "linux" || cfg.OpenSandbox.PlatformArch != "arm64" || !cfg.OpenSandbox.SecureAccess || !cfg.OpenSandbox.UseServerProxy {
		t.Fatalf("opensandbox config not loaded safely: %#v", cfg.OpenSandbox)
	}
	if cfg.Cloudflare.APIURL != "https://cloudflare.example.test" || cfg.Cloudflare.Token != "cloudflare-token" || cfg.Cloudflare.Workdir != "/workspace/cf-test" {
		t.Fatalf("cloudflare config not loaded: %#v", cfg.Cloudflare)
	}
	if cfg.Proxmox.APIURL != "https://pve.example.test:8006" || cfg.Proxmox.TokenID != "crabbox@pve!test" || cfg.Proxmox.TokenSecret != "proxmox-secret" || cfg.Proxmox.Node != "pve1" || cfg.Proxmox.TemplateID != 9000 || cfg.Proxmox.Storage != "local-lvm" || cfg.Proxmox.Pool != "crabbox" || cfg.Proxmox.Bridge != "vmbr1" || cfg.Proxmox.User != "runner" || cfg.Proxmox.WorkRoot != "/work/proxmox" || cfg.Proxmox.FullClone || !cfg.Proxmox.InsecureTLS {
		t.Fatalf("proxmox config not loaded: %#v", cfg.Proxmox)
	}
	if cfg.XCPNg.APIURL != "https://xcp-ng.example.test" || cfg.XCPNg.Username != "root" || cfg.XCPNg.Password != "xcp-ng-secret" || cfg.XCPNg.Template != "ubuntu-template" || cfg.XCPNg.TemplateUUID != "tpl-0001" || cfg.XCPNg.SR != "default-sr" || cfg.XCPNg.SRUUID != "sr-0001" || cfg.XCPNg.Network != "pool-network" || cfg.XCPNg.NetworkUUID != "net-0001" || cfg.XCPNg.Host != "host-0001" || cfg.XCPNg.User != "runner" || cfg.XCPNg.WorkRoot != "/work/xcp-ng" || !cfg.XCPNg.InsecureTLS {
		t.Fatalf("xcpNg config not loaded: %#v", cfg.XCPNg)
	}
	if cfg.Semaphore.Host != "semaphore.example.test" || cfg.Semaphore.Token != "semaphore-token" || cfg.Semaphore.Project != "crabbox" || cfg.Semaphore.Machine != "f1-standard-4" || cfg.Semaphore.OSImage != "ubuntu2404" || cfg.Semaphore.IdleTimeout != "15m" {
		t.Fatalf("semaphore config not loaded: %#v", cfg.Semaphore)
	}
	if cfg.Sprites.APIURL != "https://api.sprites.example.test" || cfg.Sprites.WorkRoot != "/home/sprite/test" {
		t.Fatalf("sprites config not loaded: %#v", cfg.Sprites)
	}
	if cfg.Static.Host != "win-dev.local" || cfg.Static.User != "peter" || cfg.Static.Port != "22" || cfg.Static.WorkRoot != "/home/peter/crabbox" {
		t.Fatalf("static config not loaded: static=%#v", cfg.Static)
	}
	if cfg.WorkRoot != defaultPOSIXWorkRoot {
		t.Fatalf("static work root leaked into active provider: workRoot=%s", cfg.WorkRoot)
	}
	if len(cfg.Results.JUnit) != 1 || cfg.Results.JUnit[0] != "junit.xml" || !cfg.Results.Auto {
		t.Fatalf("results config not loaded: %#v", cfg.Results)
	}
	if len(cfg.Run.PreflightTools) != 2 || cfg.Run.PreflightTools[0] != "node" || cfg.Run.PreflightTools[1] != "bun" {
		t.Fatalf("run config not loaded: %#v", cfg.Run)
	}
	if !cfg.Cache.Pnpm || cfg.Cache.Npm || !cfg.Cache.Docker || !cfg.Cache.Git || cfg.Cache.MaxGB != 120 || !cfg.Cache.PurgeOnRelease {
		t.Fatalf("cache config not loaded: %#v", cfg.Cache)
	}
	if len(cfg.Cache.Volumes) != 1 || cfg.Cache.Volumes[0].Name != "pnpm-store" || cfg.Cache.Volumes[0].Key != "my-app-linux-amd64-node24-pnpm10-lock" || cfg.Cache.Volumes[0].Path != "/var/cache/crabbox/pnpm" || cfg.Cache.Volumes[0].SizeGB != 80 || !cfg.Cache.Volumes[0].Required {
		t.Fatalf("cache volumes config not loaded: %#v", cfg.Cache.Volumes)
	}
}

func TestNormalizeBrokerConfig(t *testing.T) {
	t.Run("defaults to managed", func(t *testing.T) {
		cfg := Config{}
		if err := normalizeBrokerConfig(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.BrokerMode != BrokerModeManaged {
			t.Fatalf("mode=%q", cfg.BrokerMode)
		}
	})
	t.Run("registered requires coordinator", func(t *testing.T) {
		cfg := Config{BrokerMode: BrokerModeRegistered}
		if err := normalizeBrokerConfig(&cfg); err == nil || !strings.Contains(err.Error(), "requires broker.url") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("rejects unknown mode", func(t *testing.T) {
		cfg := Config{BrokerMode: "mirror"}
		if err := normalizeBrokerConfig(&cfg); err == nil || !strings.Contains(err.Error(), "managed or registered") {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestLoadConfigExeDevWorkRootDefaults(t *testing.T) {
	for name, tc := range map[string]struct {
		body    string
		want    string
		wantExe string
	}{
		"default": {
			body:    "provider: exe-dev\n",
			want:    "/tmp/crabbox",
			wantExe: "/tmp/crabbox",
		},
		"top-level": {
			body:    "provider: exe-dev\nworkRoot: /custom/crabbox\n",
			want:    "/custom/crabbox",
			wantExe: "/custom/crabbox",
		},
		"provider-specific": {
			body:    "provider: exe-dev\nworkRoot: /custom/crabbox\nexeDev:\n  workRoot: /exe/crabbox\n",
			want:    "/exe/crabbox",
			wantExe: "/exe/crabbox",
		},
	} {
		t.Run(name, func(t *testing.T) {
			clearConfigEnv(t)
			home := t.TempDir()
			path := filepath.Join(home, "config.yaml")
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
			t.Setenv("CRABBOX_CONFIG", path)
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := loadConfig()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.WorkRoot != tc.want || cfg.ExeDev.WorkRoot != tc.wantExe {
				t.Fatalf("workRoot=%q exeDev.workRoot=%q", cfg.WorkRoot, cfg.ExeDev.WorkRoot)
			}
		})
	}
}

func TestLoadConfigRoutesAzureBackendToDynamicSessions(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	cfgPath := filepath.Join(home, "config.yaml")
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	if err := os.WriteFile(cfgPath, []byte(`
provider: azure
azure:
  backend: dynamic-sessions
azureDynamicSessions:
  endpoint: http://127.0.0.1:8787/
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "azure-dynamic-sessions" || cfg.AzureBackend != AzureBackendDynamicSessions || cfg.ServerType != "" {
		t.Fatalf("provider=%q azureBackend=%q serverType=%q", cfg.Provider, cfg.AzureBackend, cfg.ServerType)
	}
}

func TestLoadConfigRoutesAzureBackendFromEnv(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(home, "missing.yaml"))
	t.Setenv("CRABBOX_PROVIDER", "azure")
	t.Setenv("CRABBOX_AZURE_BACKEND", "azds")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "azure-dynamic-sessions" || cfg.AzureBackend != AzureBackendDynamicSessions {
		t.Fatalf("provider=%q azureBackend=%q", cfg.Provider, cfg.AzureBackend)
	}
}

func TestLoadConfigMXCCapabilityEnvOverrides(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(home, "missing.yaml"))
	t.Setenv("CRABBOX_MXC_ALLOW_DACL_MUTATION", "true")
	t.Setenv("CRABBOX_MXC_ALLOW_WINDOWS_UI", "true")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.MXC.AllowDACLMutation || !cfg.MXC.AllowWindowsUI {
		t.Fatalf("mxc=%+v", cfg.MXC)
	}
}

func TestLoadConfigTailscaleBlock(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	path := userConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`provider: aws
network: public
tailscale:
  enabled: true
  network: tailscale
  tags:
    - tag:crabbox
    - tag:ci
  hostnameTemplate: cbx-{slug}
  authKeyEnv: TEST_TS_AUTH_KEY
  exitNode: mac-studio.tailnet.ts.net
  exitNodeAllowLanAccess: true
`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Tailscale.Enabled || cfg.Network != NetworkTailscale || cfg.Tailscale.HostnameTemplate != "cbx-{slug}" || cfg.Tailscale.AuthKeyEnv != "TEST_TS_AUTH_KEY" || cfg.Tailscale.ExitNode != "mac-studio.tailnet.ts.net" || !cfg.Tailscale.ExitNodeAllowLANAccess {
		t.Fatalf("tailscale config not loaded: network=%s tailscale=%#v", cfg.Network, cfg.Tailscale)
	}
	if len(cfg.Tailscale.Tags) != 2 || cfg.Tailscale.Tags[1] != "tag:ci" {
		t.Fatalf("tailscale tags not loaded: %#v", cfg.Tailscale.Tags)
	}
}

func TestEnvOverridesConfig(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_PROVIDER", "hetzner")
	t.Setenv("CRABBOX_DEFAULT_CLASS", "fast")
	t.Setenv("CRABBOX_SERVER_TYPE", "cx22")
	t.Setenv("CRABBOX_DESKTOP", "true")
	t.Setenv("CRABBOX_BROWSER", "true")
	t.Setenv("CRABBOX_CODE", "true")
	t.Setenv("CRABBOX_TTL", "3h")
	t.Setenv("CRABBOX_IDLE_TIMEOUT", "20m")
	t.Setenv("CRABBOX_AWS_SSH_CIDRS", "198.51.100.7/32,203.0.113.8/32")
	t.Setenv("CRABBOX_AZURE_OS_DISK", "managed")
	t.Setenv("CRABBOX_AZURE_SSH_CIDRS", "198.51.100.9/32,203.0.113.10/32")
	t.Setenv("CRABBOX_AZURE_DYNAMIC_SESSIONS_ENDPOINT", "https://env-pool.env.westus.azurecontainerapps.io")
	t.Setenv("CRABBOX_AZURE_DYNAMIC_SESSIONS_POOL", "env-pool")
	t.Setenv("CRABBOX_AZURE_DYNAMIC_SESSIONS_API_VERSION", "2025-02-02-preview")
	t.Setenv("CRABBOX_AZURE_DYNAMIC_SESSIONS_WORKDIR", "/workspace/env")
	t.Setenv("CRABBOX_AZURE_DYNAMIC_SESSIONS_TIMEOUT_SECS", "90")
	t.Setenv("CRABBOX_GCP_PROJECT", "crabbox-project")
	t.Setenv("CRABBOX_GCP_ZONE", "europe-west2-b")
	t.Setenv("CRABBOX_GCP_IMAGE", "projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-amd64")
	t.Setenv("CRABBOX_GCP_NETWORK", "crabbox-net")
	t.Setenv("CRABBOX_GCP_SUBNET", "crabbox-subnet")
	t.Setenv("CRABBOX_GCP_TAGS", "crabbox-ssh,crabbox-ci")
	t.Setenv("CRABBOX_GCP_SSH_CIDRS", "198.51.100.11/32,203.0.113.12/32")
	t.Setenv("CRABBOX_GCP_ROOT_GB", "900")
	t.Setenv("CRABBOX_GCP_SERVICE_ACCOUNT", "runner@crabbox-project.iam.gserviceaccount.com")
	t.Setenv("CRABBOX_SSH_FALLBACK_PORTS", "none")
	t.Setenv("CRABBOX_ACCESS_CLIENT_ID", "env-access-client")
	t.Setenv("CRABBOX_ACCESS_CLIENT_SECRET", "env-access-secret")
	t.Setenv("CRABBOX_ACCESS_TOKEN", "env-access-jwt")
	t.Setenv("CRABBOX_COORDINATOR_ADMIN_TOKEN", "env-admin-secret")
	t.Setenv("CRABBOX_HOST_ID", "h-neutral-env")
	t.Setenv("CRABBOX_NETWORK", "public")
	t.Setenv("CRABBOX_CAPACITY_HINTS", "false")
	t.Setenv("CRABBOX_CAPACITY_REGIONS", "eu-west-1,us-east-1")
	t.Setenv("CRABBOX_CAPACITY_AVAILABILITY_ZONES", "eu-west-1a,eu-west-1b")
	t.Setenv("CRABBOX_TAILSCALE_TAGS", "tag:crabbox,tag:ci")
	t.Setenv("CRABBOX_TAILSCALE_HOSTNAME_TEMPLATE", "lease-{id}")
	t.Setenv("CRABBOX_TAILSCALE_AUTH_KEY", "tskey-secret")
	t.Setenv("CRABBOX_TAILSCALE_EXIT_NODE", "mac-studio.tailnet.ts.net")
	t.Setenv("CRABBOX_TAILSCALE_EXIT_NODE_ALLOW_LAN_ACCESS", "1")
	t.Setenv("CRABBOX_TARGET", "macos")
	t.Setenv("CRABBOX_STATIC_HOST", "mac.local")
	t.Setenv("MORPH_API_KEY", "morph-api-file")
	t.Setenv("CRABBOX_MORPH_API_KEY", "morph-api-env")
	t.Setenv("CRABBOX_MORPH_API_URL", "https://morph-env.example")
	t.Setenv("CRABBOX_MORPH_SNAPSHOT", "snapshot-env")
	t.Setenv("CRABBOX_MORPH_SSH_GATEWAY_HOST", "ssh.morph-env.example")
	t.Setenv("CRABBOX_MORPH_WORK_ROOT", "/tmp/morph-env")
	t.Setenv("CRABBOX_MORPH_DELETE_ON_RELEASE", "true")
	t.Setenv("CRABBOX_MORPH_WAKE_ON_SSH", "false")
	t.Setenv("DAYTONA_API_KEY", "daytona-api-file")
	t.Setenv("CRABBOX_DAYTONA_API_KEY", "daytona-api-env")
	t.Setenv("DAYTONA_API_URL", "https://daytona-file.example/api")
	t.Setenv("CRABBOX_DAYTONA_API_URL", "https://daytona-env.example/api")
	t.Setenv("DAYTONA_SNAPSHOT", "snapshot-file")
	t.Setenv("CRABBOX_DAYTONA_SNAPSHOT", "snapshot-env")
	t.Setenv("DAYTONA_TARGET", "target-file")
	t.Setenv("CRABBOX_DAYTONA_TARGET", "target-env")
	t.Setenv("CRABBOX_DAYTONA_USER", "daytona-env-user")
	t.Setenv("CRABBOX_DAYTONA_WORK_ROOT", "/home/daytona/env")
	t.Setenv("CRABBOX_DAYTONA_SSH_GATEWAY_HOST", "ssh.env.example")
	t.Setenv("CRABBOX_DAYTONA_SSH_ACCESS_MINUTES", "44")
	t.Setenv("E2B_API_KEY", "e2b-api-file")
	t.Setenv("CRABBOX_E2B_API_KEY", "e2b-api-env")
	t.Setenv("E2B_API_URL", "https://api.e2b-file.example")
	t.Setenv("CRABBOX_E2B_API_URL", "https://api.e2b-env.example")
	t.Setenv("E2B_DOMAIN", "e2b-file.example")
	t.Setenv("CRABBOX_E2B_DOMAIN", "e2b-env.example")
	t.Setenv("CRABBOX_E2B_TEMPLATE", "template-env")
	t.Setenv("CRABBOX_E2B_WORKDIR", "env-workdir")
	t.Setenv("CRABBOX_E2B_USER", "sandbox-env")
	t.Setenv("RAILWAY_API_TOKEN", "railway-token-file")
	t.Setenv("CRABBOX_RAILWAY_API_TOKEN", "railway-token-env")
	t.Setenv("RAILWAY_API_URL", "https://railway-file.example/graphql/v2")
	t.Setenv("CRABBOX_RAILWAY_API_URL", "https://railway-env.example/graphql/v2")
	t.Setenv("RAILWAY_PROJECT_ID", "railway-project-file")
	t.Setenv("CRABBOX_RAILWAY_PROJECT_ID", "railway-project-env")
	t.Setenv("RAILWAY_ENVIRONMENT_ID", "railway-environment-file")
	t.Setenv("CRABBOX_RAILWAY_ENVIRONMENT_ID", "railway-environment-env")
	t.Setenv("RUNPOD_API_KEY", "runpod-key-file")
	t.Setenv("CRABBOX_RUNPOD_API_KEY", "runpod-key-env")
	t.Setenv("RUNPOD_API_URL", "https://runpod-file.example/v1")
	t.Setenv("CRABBOX_RUNPOD_API_URL", "https://runpod-env.example/v1")
	t.Setenv("RUNPOD_CLOUD_TYPE", "COMMUNITY")
	t.Setenv("CRABBOX_RUNPOD_CLOUD_TYPE", "SECURE")
	t.Setenv("RUNPOD_INSTANCE_ID", "NVIDIA RTX A4000")
	t.Setenv("CRABBOX_RUNPOD_INSTANCE_ID", "NVIDIA L4")
	t.Setenv("RUNPOD_IMAGE", "runpod/pytorch:file")
	t.Setenv("CRABBOX_RUNPOD_IMAGE", "runpod/pytorch:env")
	t.Setenv("RUNPOD_TEMPLATE_ID", "tpl-file")
	t.Setenv("CRABBOX_RUNPOD_TEMPLATE_ID", "tpl-env")
	t.Setenv("CRABBOX_RUNPOD_DISK_GB", "30")
	t.Setenv("CRABBOX_RUNPOD_USER", "runpod-env-user")
	t.Setenv("CRABBOX_RUNPOD_WORK_ROOT", "/work/runpod-env")
	t.Setenv("ISLO_API_KEY", "islo-api-file")
	t.Setenv("CRABBOX_ISLO_API_KEY", "islo-api-env")
	t.Setenv("ISLO_BASE_URL", "https://islo-file.example")
	t.Setenv("CRABBOX_ISLO_BASE_URL", "https://islo-env.example")
	t.Setenv("CRABBOX_ISLO_IMAGE", "ubuntu:env")
	t.Setenv("CRABBOX_ISLO_WORKDIR", "env-workdir")
	t.Setenv("CRABBOX_ISLO_GATEWAY_PROFILE", "env-gateway")
	t.Setenv("CRABBOX_ISLO_SNAPSHOT_NAME", "env-snapshot")
	t.Setenv("CRABBOX_ISLO_VCPUS", "8")
	t.Setenv("CRABBOX_ISLO_MEMORY_MB", "16384")
	t.Setenv("CRABBOX_ISLO_DISK_GB", "80")
	t.Setenv("FREESTYLE_API_KEY", "freestyle-key-file")
	t.Setenv("CRABBOX_FREESTYLE_API_KEY", "freestyle-key-env")
	t.Setenv("FREESTYLE_API_URL", "https://freestyle-file.example")
	t.Setenv("CRABBOX_FREESTYLE_API_URL", "https://freestyle-env.example")
	t.Setenv("CRABBOX_FREESTYLE_WORKDIR", "env/repo")
	t.Setenv("CRABBOX_FREESTYLE_VCPUS", "6")
	t.Setenv("CRABBOX_FREESTYLE_MEMORY_GB", "16")
	t.Setenv("TENKI_CLI", "/usr/bin/tenki-file")
	t.Setenv("CRABBOX_TENKI_CLI", "/opt/tenki/bin/tenki")
	t.Setenv("TENKI_ENDPOINT", "https://api.tenki-file.example")
	t.Setenv("CRABBOX_TENKI_ENDPOINT", "https://api.tenki-env.example")
	t.Setenv("TENKI_GATEWAY", "wss://gateway.tenki-file.example")
	t.Setenv("CRABBOX_TENKI_GATEWAY", "wss://gateway.tenki-env.example")
	t.Setenv("CRABBOX_TENKI_WORKSPACE", "ws_env")
	t.Setenv("CRABBOX_TENKI_PROJECT", "proj_env")
	t.Setenv("CRABBOX_TENKI_IMAGE", "ubuntu:tenki-env")
	t.Setenv("CRABBOX_TENKI_SNAPSHOT", "snap-env")
	t.Setenv("CRABBOX_TENKI_WORK_ROOT", "/home/tenki/env")
	t.Setenv("CRABBOX_TENKI_CPUS", "8")
	t.Setenv("CRABBOX_TENKI_MEMORY_MB", "16384")
	t.Setenv("CRABBOX_TENKI_DISK_GB", "80")
	t.Setenv("TENSORLAKE_API_KEY", "tl-api-file")
	t.Setenv("CRABBOX_TENSORLAKE_API_KEY", "tl-api-env")
	t.Setenv("TENSORLAKE_API_URL", "https://api.tl-file.example")
	t.Setenv("CRABBOX_TENSORLAKE_API_URL", "https://api.tl-env.example")
	t.Setenv("CRABBOX_TENSORLAKE_CLI", "/opt/tl/bin/tensorlake")
	t.Setenv("CRABBOX_TENSORLAKE_IMAGE", "ubuntu:tl-env")
	t.Setenv("CRABBOX_TENSORLAKE_SNAPSHOT", "snap-tl-env")
	t.Setenv("TENSORLAKE_ORGANIZATION_ID", "org-tl-file")
	t.Setenv("CRABBOX_TENSORLAKE_ORGANIZATION_ID", "org-tl-env")
	t.Setenv("TENSORLAKE_PROJECT_ID", "proj-tl-file")
	t.Setenv("CRABBOX_TENSORLAKE_PROJECT_ID", "proj-tl-env")
	t.Setenv("INDEXIFY_NAMESPACE", "ns-tl-file")
	t.Setenv("CRABBOX_TENSORLAKE_NAMESPACE", "ns-tl-env")
	t.Setenv("CRABBOX_TENSORLAKE_WORKDIR", "/workspace/tl-env")
	t.Setenv("CRABBOX_TENSORLAKE_CPUS", "2.5")
	t.Setenv("CRABBOX_TENSORLAKE_MEMORY_MB", "4096")
	t.Setenv("CRABBOX_TENSORLAKE_DISK_MB", "20480")
	t.Setenv("CRABBOX_TENSORLAKE_TIMEOUT_SECS", "900")
	t.Setenv("CRABBOX_TENSORLAKE_NO_INTERNET", "true")
	t.Setenv("OPENCOMPUTER_API_URL", "https://oc-file.example")
	t.Setenv("CRABBOX_OPENCOMPUTER_API_URL", "https://oc-env.example")
	t.Setenv("CRABBOX_OPENCOMPUTER_WORKDIR", "/workspace/oc-env")
	t.Setenv("CRABBOX_OPENCOMPUTER_CPU", "6")
	t.Setenv("CRABBOX_OPENCOMPUTER_MEMORY_MB", "12288")
	t.Setenv("CRABBOX_OPENCOMPUTER_TIMEOUT_SECS", "1200")
	t.Setenv("CRABBOX_OPENCOMPUTER_EXEC_TIMEOUT_SECS", "2400")
	t.Setenv("OPEN_SANDBOX_API_URL", "https://opensandbox-file.example")
	t.Setenv("CRABBOX_OPENSANDBOX_API_URL", "https://opensandbox-env.example")
	t.Setenv("CRABBOX_OPENSANDBOX_IMAGE", "ubuntu:osb-env")
	t.Setenv("CRABBOX_OPENSANDBOX_WORKDIR", "/workspace/osb-env")
	t.Setenv("CRABBOX_OPENSANDBOX_CPU", "750m")
	t.Setenv("CRABBOX_OPENSANDBOX_MEMORY", "1536Mi")
	t.Setenv("CRABBOX_OPENSANDBOX_TIMEOUT_SECS", "123")
	t.Setenv("CRABBOX_OPENSANDBOX_EXEC_TIMEOUT_SECS", "456")
	t.Setenv("CRABBOX_OPENSANDBOX_PLATFORM_OS", "linux")
	t.Setenv("CRABBOX_OPENSANDBOX_PLATFORM_ARCH", "amd64")
	t.Setenv("CRABBOX_OPENSANDBOX_SECURE_ACCESS", "true")
	t.Setenv("CRABBOX_OPENSANDBOX_USE_SERVER_PROXY", "true")
	t.Setenv("CRABBOX_CLOUDFLARE_RUNNER_URL", "https://cloudflare-env.example")
	t.Setenv("CRABBOX_CLOUDFLARE_RUNNER_TOKEN", "cloudflare-env-token")
	t.Setenv("CRABBOX_CLOUDFLARE_WORKDIR", "/workspace/cloudflare-env")
	t.Setenv("CRABBOX_PROXMOX_API_URL", "https://pve-env.example:8006")
	t.Setenv("CRABBOX_PROXMOX_TOKEN_ID", "runner@pve!env")
	t.Setenv("CRABBOX_PROXMOX_TOKEN_SECRET", "proxmox-env-secret")
	t.Setenv("CRABBOX_PROXMOX_NODE", "pve-env")
	t.Setenv("CRABBOX_PROXMOX_TEMPLATE_ID", "9100")
	t.Setenv("CRABBOX_PROXMOX_STORAGE", "ceph-env")
	t.Setenv("CRABBOX_PROXMOX_POOL", "pool-env")
	t.Setenv("CRABBOX_PROXMOX_BRIDGE", "vmbr2")
	t.Setenv("CRABBOX_PROXMOX_USER", "runner-env")
	t.Setenv("CRABBOX_PROXMOX_WORK_ROOT", "/work/proxmox-env")
	t.Setenv("CRABBOX_PROXMOX_FULL_CLONE", "false")
	t.Setenv("CRABBOX_PROXMOX_INSECURE_TLS", "true")
	t.Setenv("CRABBOX_XCP_NG_API_URL", "https://xcp-ng-env.example.test")
	t.Setenv("CRABBOX_XCP_NG_USERNAME", "root-env")
	t.Setenv("CRABBOX_XCP_NG_PASSWORD", "xcp-ng-env-secret")
	t.Setenv("CRABBOX_XCP_NG_TEMPLATE", "template-env")
	t.Setenv("CRABBOX_XCP_NG_TEMPLATE_UUID", "tpl-env")
	t.Setenv("CRABBOX_XCP_NG_SR", "sr-env")
	t.Setenv("CRABBOX_XCP_NG_SR_UUID", "sr-uuid-env")
	t.Setenv("CRABBOX_XCP_NG_NETWORK", "network-env")
	t.Setenv("CRABBOX_XCP_NG_NETWORK_UUID", "network-uuid-env")
	t.Setenv("CRABBOX_XCP_NG_HOST", "host-env")
	t.Setenv("CRABBOX_XCP_NG_USER", "runner-xcp-env")
	t.Setenv("CRABBOX_XCP_NG_WORK_ROOT", "/work/xcp-ng-env")
	t.Setenv("CRABBOX_XCP_NG_INSECURE_TLS", "true")
	t.Setenv("SEMAPHORE_HOST", "semaphore-file.example.test")
	t.Setenv("CRABBOX_SEMAPHORE_HOST", "semaphore-env.example.test")
	t.Setenv("SEMAPHORE_API_TOKEN", "semaphore-token-file")
	t.Setenv("CRABBOX_SEMAPHORE_TOKEN", "semaphore-token-env")
	t.Setenv("SEMAPHORE_PROJECT", "semaphore-project-file")
	t.Setenv("CRABBOX_SEMAPHORE_PROJECT", "semaphore-project-env")
	t.Setenv("CRABBOX_SEMAPHORE_MACHINE", "f1-standard-env")
	t.Setenv("CRABBOX_SEMAPHORE_OS_IMAGE", "ubuntu-env")
	t.Setenv("CRABBOX_SEMAPHORE_IDLE_TIMEOUT", "22m")
	t.Setenv("SPRITE_TOKEN", "sprite-token-file")
	t.Setenv("SETUP_SPRITE_TOKEN", "setup-sprite-token-file")
	t.Setenv("SPRITES_TOKEN", "sprites-token-file")
	t.Setenv("CRABBOX_SPRITES_TOKEN", "sprites-token-env")
	t.Setenv("SPRITES_API_URL", "https://api.sprites-file.example")
	t.Setenv("CRABBOX_SPRITES_API_URL", "https://api.sprites-env.example")
	t.Setenv("CRABBOX_SPRITES_WORK_ROOT", "/home/sprite/env")
	t.Setenv("CRABBOX_LOCAL_CONTAINER_RUNTIME", "docker")
	t.Setenv("CRABBOX_LOCAL_CONTAINER_IMAGE", "ubuntu:env")
	t.Setenv("CRABBOX_LOCAL_CONTAINER_USER", "runner-env")
	t.Setenv("CRABBOX_LOCAL_CONTAINER_WORK_ROOT", "/workspace/env")
	t.Setenv("CRABBOX_LOCAL_CONTAINER_CPUS", "6")
	t.Setenv("CRABBOX_LOCAL_CONTAINER_MEMORY", "12g")
	t.Setenv("CRABBOX_LOCAL_CONTAINER_NETWORK", "bridge")
	t.Setenv("CRABBOX_LOCAL_CONTAINER_DOCKER_SOCKET", "true")
	t.Setenv("CRABBOX_NAMESPACE_IMAGE", "namespace-env-image")
	t.Setenv("CRABBOX_NAMESPACE_SIZE", "XL")
	t.Setenv("CRABBOX_NAMESPACE_REPOSITORY", "github.com/openclaw/env")
	t.Setenv("CRABBOX_NAMESPACE_SITE", "iad1")
	t.Setenv("CRABBOX_NAMESPACE_VOLUME_SIZE_GB", "300")
	t.Setenv("CRABBOX_NAMESPACE_AUTO_STOP_IDLE_TIMEOUT", "4h")
	t.Setenv("CRABBOX_NAMESPACE_WORK_ROOT", "/workspaces/env")
	t.Setenv("CRABBOX_NAMESPACE_DELETE_ON_RELEASE", "true")
	t.Setenv("CRABBOX_BLACKSMITH_IDLE_TIMEOUT", "2h")
	t.Setenv("CRABBOX_BLACKSMITH_DEBUG", "true")
	t.Setenv("CRABBOX_ACTIONS_RUNNER_LABELS", "crabbox,linux-large")
	t.Setenv("CRABBOX_ACTIONS_EPHEMERAL", "false")
	t.Setenv("CRABBOX_RESULTS_JUNIT", "junit.xml,build/test.xml")
	t.Setenv("CRABBOX_RESULTS_AUTO", "true")
	t.Setenv("CRABBOX_CACHE_PNPM", "false")
	t.Setenv("CRABBOX_CACHE_NPM", "false")
	t.Setenv("CRABBOX_CACHE_DOCKER", "true")
	t.Setenv("CRABBOX_CACHE_GIT", "false")
	t.Setenv("CRABBOX_CACHE_PURGE_ON_RELEASE", "true")
	t.Setenv("CRABBOX_CACHE_VOLUMES", "pnpm=env-pnpm:/var/cache/crabbox/pnpm,npm-cache:/var/cache/crabbox/npm")
	t.Setenv("CRABBOX_SYNC_CHECKSUM", "true")
	t.Setenv("CRABBOX_SYNC_DELETE", "false")
	t.Setenv("CRABBOX_SYNC_GIT_SEED", "false")
	t.Setenv("CRABBOX_SYNC_FINGERPRINT", "false")
	t.Setenv("CRABBOX_SYNC_TIMEOUT", "45m")
	t.Setenv("CRABBOX_SYNC_ALLOW_LARGE", "true")
	t.Setenv("CRABBOX_ENV_ALLOW", "CI,NODE_OPTIONS,CUSTOM_*")
	t.Setenv("CRABBOX_PREFLIGHT_TOOLS", "node,bun,docker")
	path := userConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("provider: aws\nclass: beast\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "hetzner" || cfg.Class != "fast" || cfg.ServerType != "cx22" || !cfg.ServerTypeExplicit || cfg.TTL.String() != "3h0m0s" || cfg.IdleTimeout.String() != "20m0s" {
		t.Fatalf("unexpected config: provider=%s class=%s type=%s ttl=%s idle=%s", cfg.Provider, cfg.Class, cfg.ServerType, cfg.TTL, cfg.IdleTimeout)
	}
	if !cfg.Desktop || !cfg.Browser || !cfg.Code {
		t.Fatalf("capability env not loaded: desktop=%t browser=%t code=%t", cfg.Desktop, cfg.Browser, cfg.Code)
	}
	if len(cfg.AWSSSHCIDRs) != 2 || cfg.AWSSSHCIDRs[0] != "198.51.100.7/32" || cfg.AWSSSHCIDRs[1] != "203.0.113.8/32" {
		t.Fatalf("AWSSSHCIDRs=%v", cfg.AWSSSHCIDRs)
	}
	if len(cfg.AzureSSHCIDRs) != 2 || cfg.AzureSSHCIDRs[0] != "198.51.100.9/32" || cfg.AzureSSHCIDRs[1] != "203.0.113.10/32" {
		t.Fatalf("AzureSSHCIDRs=%v", cfg.AzureSSHCIDRs)
	}
	if cfg.AzureOSDisk != "managed" {
		t.Fatalf("AzureOSDisk=%q", cfg.AzureOSDisk)
	}
	if !cfg.AzureOSDiskExplicit {
		t.Fatal("AzureOSDiskExplicit=false, want true")
	}
	if cfg.AzureDynamicSessions.Endpoint != "https://env-pool.env.westus.azurecontainerapps.io" || cfg.AzureDynamicSessions.Pool != "env-pool" || cfg.AzureDynamicSessions.Workdir != "/workspace/env" || cfg.AzureDynamicSessions.TimeoutSecs != 90 {
		t.Fatalf("unexpected azure dynamic sessions env: %#v", cfg.AzureDynamicSessions)
	}
	if cfg.GCPProject != "crabbox-project" || cfg.GCPZone != "europe-west2-b" || cfg.GCPNetwork != "crabbox-net" || cfg.GCPSubnet != "crabbox-subnet" || cfg.GCPRootGB != 900 || cfg.GCPServiceAccount != "runner@crabbox-project.iam.gserviceaccount.com" {
		t.Fatalf("unexpected gcp env: project=%s zone=%s network=%s subnet=%s root=%d service=%s", cfg.GCPProject, cfg.GCPZone, cfg.GCPNetwork, cfg.GCPSubnet, cfg.GCPRootGB, cfg.GCPServiceAccount)
	}
	if len(cfg.GCPTags) != 2 || cfg.GCPTags[1] != "crabbox-ci" || len(cfg.GCPSSHCIDRs) != 2 || cfg.GCPSSHCIDRs[1] != "203.0.113.12/32" {
		t.Fatalf("unexpected gcp tags/cidrs: tags=%v cidrs=%v", cfg.GCPTags, cfg.GCPSSHCIDRs)
	}
	if len(cfg.SSHFallbackPorts) != 0 {
		t.Fatalf("SSHFallbackPorts=%v want disabled fallback", cfg.SSHFallbackPorts)
	}
	if cfg.Access.ClientID != "env-access-client" || cfg.Access.ClientSecret != "env-access-secret" || cfg.Access.Token != "env-access-jwt" {
		t.Fatalf("unexpected access config: %#v", cfg.Access)
	}
	if cfg.CoordAdminToken != "env-admin-secret" {
		t.Fatalf("unexpected admin token state: %q", cfg.CoordAdminToken)
	}
	if cfg.HostID != "h-neutral-env" {
		t.Fatalf("unexpected host id: %q", cfg.HostID)
	}
	if cfg.TargetOS != targetMacOS || cfg.Static.Host != "mac.local" {
		t.Fatalf("unexpected target env: target=%s static=%#v", cfg.TargetOS, cfg.Static)
	}
	if cfg.Network != NetworkPublic || cfg.Tailscale.AuthKey != "tskey-secret" || cfg.Tailscale.HostnameTemplate != "lease-{id}" || cfg.Tailscale.ExitNode != "mac-studio.tailnet.ts.net" || !cfg.Tailscale.ExitNodeAllowLANAccess {
		t.Fatalf("unexpected tailscale env: network=%s tailscale=%#v", cfg.Network, cfg.Tailscale)
	}
	if cfg.Capacity.Hints || len(cfg.Capacity.Regions) != 2 || len(cfg.Capacity.AvailabilityZones) != 2 {
		t.Fatalf("unexpected capacity env: %#v", cfg.Capacity)
	}
	if len(cfg.Tailscale.Tags) != 2 || cfg.Tailscale.Tags[1] != "tag:ci" {
		t.Fatalf("unexpected tailscale tags: %#v", cfg.Tailscale.Tags)
	}
	if cfg.Morph.APIKey != "morph-api-env" || cfg.Morph.APIURL != "https://morph-env.example" || cfg.Morph.Snapshot != "snapshot-env" || cfg.Morph.SSHGatewayHost != "ssh.morph-env.example" || cfg.Morph.WorkRoot != "/tmp/morph-env" || !cfg.Morph.DeleteOnRelease || cfg.Morph.WakeOnSSH {
		t.Fatalf("unexpected morph env: %#v", cfg.Morph)
	}
	if cfg.Daytona.APIKey != "daytona-api-env" || cfg.Daytona.APIURL != "https://daytona-env.example/api" || cfg.Daytona.Snapshot != "snapshot-env" || cfg.Daytona.Target != "target-env" || cfg.Daytona.User != "daytona-env-user" || cfg.Daytona.WorkRoot != "/home/daytona/env" || cfg.Daytona.SSHGatewayHost != "ssh.env.example" || cfg.Daytona.SSHAccessMinutes != 44 {
		t.Fatalf("unexpected daytona env: %#v", cfg.Daytona)
	}
	if cfg.E2B.APIKey != "e2b-api-env" || cfg.E2B.APIURL != "https://api.e2b-env.example" || cfg.E2B.Domain != "e2b-env.example" || cfg.E2B.Template != "template-env" || cfg.E2B.Workdir != "env-workdir" || cfg.E2B.User != "sandbox-env" {
		t.Fatalf("unexpected e2b env: %#v", cfg.E2B)
	}
	if cfg.Railway.APIToken != "railway-token-env" || cfg.Railway.APIURL != "https://railway-env.example/graphql/v2" || cfg.Railway.ProjectID != "railway-project-env" || cfg.Railway.EnvironmentID != "railway-environment-env" {
		t.Fatalf("unexpected railway env: %#v", cfg.Railway)
	}
	if cfg.Runpod.APIKey != "runpod-key-env" || cfg.Runpod.APIURL != "https://runpod-env.example/v1" || cfg.Runpod.CloudType != "SECURE" || cfg.Runpod.InstanceID != "NVIDIA L4" || cfg.Runpod.Image != "runpod/pytorch:env" || cfg.Runpod.TemplateID != "tpl-env" || cfg.Runpod.DiskGB != 30 || cfg.Runpod.User != "runpod-env-user" || cfg.Runpod.WorkRoot != "/work/runpod-env" {
		t.Fatalf("unexpected runpod env: %#v", cfg.Runpod)
	}
	if cfg.Islo.APIKey != "islo-api-env" || cfg.Islo.BaseURL != "https://islo-env.example" || cfg.Islo.Image != "ubuntu:env" || cfg.Islo.Workdir != "env-workdir" || cfg.Islo.GatewayProfile != "env-gateway" || cfg.Islo.SnapshotName != "env-snapshot" || cfg.Islo.VCPUs != 8 || cfg.Islo.MemoryMB != 16384 || cfg.Islo.DiskGB != 80 {
		t.Fatalf("unexpected islo env: %#v", cfg.Islo)
	}
	if cfg.Freestyle.APIKey != "freestyle-key-env" || cfg.Freestyle.APIURL != "https://freestyle-env.example" || cfg.Freestyle.Workdir != "env/repo" || cfg.Freestyle.VCPUs != 6 || cfg.Freestyle.MemoryGB != 16 {
		t.Fatalf("unexpected freestyle env: %#v", cfg.Freestyle)
	}
	if cfg.Tenki.CLIPath != "/opt/tenki/bin/tenki" || cfg.Tenki.Endpoint != "https://api.tenki-env.example" || cfg.Tenki.Gateway != "wss://gateway.tenki-env.example" || cfg.Tenki.Workspace != "ws_env" || cfg.Tenki.Project != "proj_env" || cfg.Tenki.Image != "ubuntu:tenki-env" || cfg.Tenki.Snapshot != "snap-env" || cfg.Tenki.WorkRoot != "/home/tenki/env" || cfg.Tenki.CPUs != 8 || cfg.Tenki.MemoryMB != 16384 || cfg.Tenki.DiskGB != 80 {
		t.Fatalf("unexpected tenki env: %#v", cfg.Tenki)
	}
	if cfg.Tensorlake.APIKey != "tl-api-env" || cfg.Tensorlake.APIURL != "https://api.tl-env.example" || cfg.Tensorlake.CLIPath != "/opt/tl/bin/tensorlake" || cfg.Tensorlake.Image != "ubuntu:tl-env" || cfg.Tensorlake.Snapshot != "snap-tl-env" || cfg.Tensorlake.OrganizationID != "org-tl-env" || cfg.Tensorlake.ProjectID != "proj-tl-env" || cfg.Tensorlake.Namespace != "ns-tl-env" || cfg.Tensorlake.Workdir != "/workspace/tl-env" || cfg.Tensorlake.CPUs != 2.5 || cfg.Tensorlake.MemoryMB != 4096 || cfg.Tensorlake.DiskMB != 20480 || cfg.Tensorlake.TimeoutSecs != 900 || !cfg.Tensorlake.NoInternet {
		t.Fatalf("unexpected tensorlake env: %#v", cfg.Tensorlake)
	}
	if cfg.OpenComputer.APIURL != "https://oc-env.example" || cfg.OpenComputer.Workdir != "/workspace/oc-env" || cfg.OpenComputer.CPU != 6 || cfg.OpenComputer.MemoryMB != 12288 || cfg.OpenComputer.TimeoutSecs != 1200 || cfg.OpenComputer.ExecTimeoutSecs != 2400 {
		t.Fatalf("unexpected opencomputer env: %#v", cfg.OpenComputer)
	}
	if cfg.OpenSandbox.APIURL != "https://opensandbox-env.example" || cfg.OpenSandbox.Image != "ubuntu:osb-env" || cfg.OpenSandbox.Workdir != "/workspace/osb-env" || cfg.OpenSandbox.CPU != "750m" || cfg.OpenSandbox.Memory != "1536Mi" || cfg.OpenSandbox.TimeoutSecs != 123 || cfg.OpenSandbox.ExecTimeoutSecs != 456 || cfg.OpenSandbox.PlatformOS != "linux" || cfg.OpenSandbox.PlatformArch != "amd64" || !cfg.OpenSandbox.SecureAccess || !cfg.OpenSandbox.UseServerProxy {
		t.Fatalf("unexpected opensandbox env: %#v", cfg.OpenSandbox)
	}
	if cfg.Cloudflare.APIURL != "https://cloudflare-env.example" || cfg.Cloudflare.Token != "cloudflare-env-token" || cfg.Cloudflare.Workdir != "/workspace/cloudflare-env" {
		t.Fatalf("unexpected cloudflare env: %#v", cfg.Cloudflare)
	}
	if cfg.Proxmox.APIURL != "https://pve-env.example:8006" || cfg.Proxmox.TokenID != "runner@pve!env" || cfg.Proxmox.TokenSecret != "proxmox-env-secret" || cfg.Proxmox.Node != "pve-env" || cfg.Proxmox.TemplateID != 9100 || cfg.Proxmox.Storage != "ceph-env" || cfg.Proxmox.Pool != "pool-env" || cfg.Proxmox.Bridge != "vmbr2" || cfg.Proxmox.User != "runner-env" || cfg.Proxmox.WorkRoot != "/work/proxmox-env" || cfg.Proxmox.FullClone || !cfg.Proxmox.InsecureTLS {
		t.Fatalf("unexpected proxmox env: %#v", cfg.Proxmox)
	}
	if cfg.XCPNg.APIURL != "https://xcp-ng-env.example.test" || cfg.XCPNg.Username != "root-env" || cfg.XCPNg.Password != "xcp-ng-env-secret" || cfg.XCPNg.Template != "template-env" || cfg.XCPNg.TemplateUUID != "tpl-env" || cfg.XCPNg.SR != "sr-env" || cfg.XCPNg.SRUUID != "sr-uuid-env" || cfg.XCPNg.Network != "network-env" || cfg.XCPNg.NetworkUUID != "network-uuid-env" || cfg.XCPNg.Host != "host-env" || cfg.XCPNg.User != "runner-xcp-env" || cfg.XCPNg.WorkRoot != "/work/xcp-ng-env" || !cfg.XCPNg.InsecureTLS {
		t.Fatalf("unexpected xcp-ng env: %#v", cfg.XCPNg)
	}
	if cfg.Semaphore.Host != "semaphore-env.example.test" || cfg.Semaphore.Token != "semaphore-token-env" || cfg.Semaphore.Project != "semaphore-project-env" || cfg.Semaphore.Machine != "f1-standard-env" || cfg.Semaphore.OSImage != "ubuntu-env" || cfg.Semaphore.IdleTimeout != "22m" {
		t.Fatalf("unexpected semaphore env: %#v", cfg.Semaphore)
	}
	if cfg.Sprites.Token != "sprites-token-env" || cfg.Sprites.APIURL != "https://api.sprites-env.example" || cfg.Sprites.WorkRoot != "/home/sprite/env" {
		t.Fatalf("unexpected sprites env: %#v", cfg.Sprites)
	}
	if cfg.LocalContainer.Runtime != "docker" || cfg.LocalContainer.Image != "ubuntu:env" || cfg.LocalContainer.User != "runner-env" || cfg.LocalContainer.WorkRoot != "/workspace/env" || cfg.LocalContainer.CPUs != 6 || cfg.LocalContainer.Memory != "12g" || cfg.LocalContainer.Network != "bridge" || !cfg.LocalContainer.DockerSocket {
		t.Fatalf("unexpected local-container env: %#v", cfg.LocalContainer)
	}
	if cfg.Blacksmith.IdleTimeout != 2*time.Hour || !cfg.Blacksmith.Debug {
		t.Fatalf("unexpected blacksmith env: %#v", cfg.Blacksmith)
	}
	if cfg.Namespace.Image != "namespace-env-image" || cfg.Namespace.Size != "XL" || cfg.Namespace.Repository != "github.com/openclaw/env" || cfg.Namespace.Site != "iad1" || cfg.Namespace.VolumeSizeGB != 300 || cfg.Namespace.AutoStopIdleTimeout != 4*time.Hour || cfg.Namespace.WorkRoot != "/workspaces/env" || !cfg.Namespace.DeleteOnRelease {
		t.Fatalf("unexpected namespace env: %#v", cfg.Namespace)
	}
	if len(cfg.Actions.RunnerLabels) != 2 || cfg.Actions.RunnerLabels[1] != "linux-large" || cfg.Actions.Ephemeral {
		t.Fatalf("unexpected actions env: %#v", cfg.Actions)
	}
	if len(cfg.Results.JUnit) != 2 || cfg.Results.JUnit[1] != "build/test.xml" || !cfg.Results.Auto {
		t.Fatalf("unexpected results env: %#v", cfg.Results)
	}
	if cfg.Cache.Pnpm || cfg.Cache.Npm || !cfg.Cache.Docker || cfg.Cache.Git || !cfg.Cache.PurgeOnRelease {
		t.Fatalf("unexpected cache env: %#v", cfg.Cache)
	}
	if len(cfg.Cache.Volumes) != 2 || cfg.Cache.Volumes[0].Name != "pnpm" || cfg.Cache.Volumes[0].Key != "env-pnpm" || cfg.Cache.Volumes[1].Key != "npm-cache" {
		t.Fatalf("unexpected cache volume env: %#v", cfg.Cache.Volumes)
	}
	if !cfg.Sync.Checksum || cfg.Sync.Delete || cfg.Sync.GitSeed || cfg.Sync.Fingerprint || cfg.Sync.Timeout != 45*time.Minute || !cfg.Sync.AllowLarge {
		t.Fatalf("unexpected sync env: %#v", cfg.Sync)
	}
	if len(cfg.EnvAllow) != 3 || cfg.EnvAllow[2] != "CUSTOM_*" {
		t.Fatalf("unexpected env allow: %#v", cfg.EnvAllow)
	}
	if len(cfg.Run.PreflightTools) != 3 || cfg.Run.PreflightTools[1] != "bun" {
		t.Fatalf("unexpected preflight tools: %#v", cfg.Run.PreflightTools)
	}
}

func TestApplyEnvRejectsNegativeOpenSandboxTimeouts(t *testing.T) {
	for _, name := range []string{"CRABBOX_OPENSANDBOX_TIMEOUT_SECS", "CRABBOX_OPENSANDBOX_EXEC_TIMEOUT_SECS"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "-1")
			cfg := baseConfig()
			err := applyEnv(&cfg)
			if err == nil || !strings.Contains(err.Error(), name+" must be non-negative") {
				t.Fatalf("err=%v, want negative timeout rejection", err)
			}
		})
	}
}

func TestExplicitProviderImagesSurvivePortableOSDefaults(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	cfgPath := filepath.Join(home, "crabbox.yaml")
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	if err := os.WriteFile(cfgPath, []byte(`
os: ubuntu:24.04
hetzner:
  image: ubuntu-26.04
azure:
  image: Canonical:ubuntu-26_04-lts:server:latest
islo:
  image: docker.io/library/ubuntu:26.04
localContainer:
  image: ubuntu:26.04
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Image != "ubuntu-26.04" || cfg.AzureImage != defaultAzureLinuxImage || cfg.Islo.Image != "docker.io/library/ubuntu:26.04" || cfg.LocalContainer.Image != "ubuntu:26.04" {
		t.Fatalf("explicit images were overwritten: hetzner=%q azure=%q islo=%q local=%q", cfg.Image, cfg.AzureImage, cfg.Islo.Image, cfg.LocalContainer.Image)
	}
}

func TestPortableOSDefaultsRespectTargetAlias(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	cfgPath := filepath.Join(home, "crabbox.yaml")
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	if err := os.WriteFile(cfgPath, []byte(`
target: ubuntu
os: ubuntu:24.04
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TargetOS != targetLinux || cfg.Image != "ubuntu-24.04" || cfg.AzureImage != "Canonical:ubuntu-24_04-lts:server:latest" {
		t.Fatalf("portable os defaults not applied through target alias: target=%q image=%q azure=%q", cfg.TargetOS, cfg.Image, cfg.AzureImage)
	}
}

func TestAppleContainerImageFollowsOSImageDefault(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	cfgPath := filepath.Join(home, "crabbox.yaml")
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	if err := os.WriteFile(cfgPath, []byte("target: linux\nos: ubuntu:24.04\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	// apple-container must track the OS image default the same way local-container does.
	if cfg.AppleContainer.Image == "" || cfg.AppleContainer.Image != cfg.LocalContainer.Image {
		t.Fatalf("apple-container image should follow the os default like local-container: apple=%q local=%q", cfg.AppleContainer.Image, cfg.LocalContainer.Image)
	}
	if cfg.AppleContainer.Image == baseConfig().AppleContainer.Image {
		t.Fatalf("--os did not update apple-container image: still base %q", cfg.AppleContainer.Image)
	}
}

func TestAppleContainerExplicitImageSurvivesOSDefault(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	cfgPath := filepath.Join(home, "crabbox.yaml")
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	if err := os.WriteFile(cfgPath, []byte("target: linux\nos: ubuntu:24.04\nappleContainer:\n  image: my-org/custom:tag\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AppleContainer.Image != "my-org/custom:tag" {
		t.Fatalf("explicit apple-container image was overwritten by --os: %q", cfg.AppleContainer.Image)
	}
}

func TestAppleVZImageFollowsOSImageDefault(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	cfgPath := filepath.Join(home, "crabbox.yaml")
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	if err := os.WriteFile(cfgPath, []byte("provider: apple-vz\ntarget: linux\nos: ubuntu:24.04\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfg.AppleVZ.Image, "ubuntu-24.04-server-cloudimg-arm64.img") {
		t.Fatalf("apple-vz image should follow --os default: %q", cfg.AppleVZ.Image)
	}
	if cfg.AppleVZ.ImageSHA256 != "6a61b967ba4a27dd1966f835a67643073ed55c2860ce3dc1cb0517282e6b8bec" {
		t.Fatalf("apple-vz checksum should follow --os default: %q", cfg.AppleVZ.ImageSHA256)
	}
}

func TestAppleVZExplicitImageSurvivesOSDefault(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	cfgPath := filepath.Join(home, "crabbox.yaml")
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	if err := os.WriteFile(cfgPath, []byte("provider: apple-vz\ntarget: linux\nos: ubuntu:24.04\nappleVZ:\n  image: https://example.test/custom.img\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AppleVZ.Image != "https://example.test/custom.img" {
		t.Fatalf("explicit apple-vz image was overwritten by --os: %q", cfg.AppleVZ.Image)
	}
	if cfg.AppleVZ.ImageSHA256 != "" {
		t.Fatalf("custom apple-vz image should clear default checksum unless explicitly set: %q", cfg.AppleVZ.ImageSHA256)
	}
}

func TestAppleVZExplicitChecksumSurvivesOSDefault(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	cfgPath := filepath.Join(home, "crabbox.yaml")
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	checksum := strings.Repeat("b", 64)
	if err := os.WriteFile(cfgPath, []byte("provider: apple-vz\ntarget: linux\nos: ubuntu:24.04\nappleVZ:\n  imageSHA256: "+checksum+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AppleVZ.ImageSHA256 != checksum {
		t.Fatalf("explicit apple-vz checksum was overwritten by OS defaults: %q", cfg.AppleVZ.ImageSHA256)
	}

	t.Setenv("CRABBOX_APPLE_VZ_IMAGE_SHA256", strings.Repeat("c", 64))
	cfg, err = loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AppleVZ.ImageSHA256 != strings.Repeat("c", 64) {
		t.Fatalf("environment apple-vz checksum was overwritten by OS defaults: %q", cfg.AppleVZ.ImageSHA256)
	}
}

func TestAppleVZPreservesExplicitTopLevelWorkRoot(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	cfgPath := filepath.Join(home, "crabbox.yaml")
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	if err := os.WriteFile(cfgPath, []byte("provider: apple-vz\nworkRoot: /custom/crabbox\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkRoot != "/custom/crabbox" {
		t.Fatalf("WorkRoot=%q want /custom/crabbox", cfg.WorkRoot)
	}
}

func TestAppleVZSpecificWorkRootOverridesTopLevel(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	cfgPath := filepath.Join(home, "crabbox.yaml")
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	if err := os.WriteFile(cfgPath, []byte("provider: apple-vz\nworkRoot: /custom/crabbox\nappleVZ:\n  workRoot: /work/apple-vz\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkRoot != "/work/apple-vz" {
		t.Fatalf("WorkRoot=%q want /work/apple-vz", cfg.WorkRoot)
	}
}

func TestMultipassImageFollowsOSImageDefault(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	cfgPath := filepath.Join(home, "crabbox.yaml")
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	if err := os.WriteFile(cfgPath, []byte("target: linux\nos: ubuntu:24.04\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Multipass.Image != "24.04" {
		t.Fatalf("multipass image should follow --os default: %q", cfg.Multipass.Image)
	}
}

func TestMultipassExplicitImageSurvivesOSDefault(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	cfgPath := filepath.Join(home, "crabbox.yaml")
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	if err := os.WriteFile(cfgPath, []byte("target: linux\nos: ubuntu:24.04\nmultipass:\n  image: daily:26.04\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Multipass.Image != "daily:26.04" {
		t.Fatalf("explicit multipass image was overwritten by --os: %q", cfg.Multipass.Image)
	}
}

func TestPortableOSHigherPrecedenceOverridesEarlierPortableDefaults(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	cfgPath := filepath.Join(home, "crabbox.yaml")
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	t.Setenv("CRABBOX_OS", "ubuntu:26.04")
	if err := os.WriteFile(cfgPath, []byte(`
os: ubuntu:24.04
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OSImage != "ubuntu:26.04" || cfg.Image != "ubuntu-24.04" || cfg.AzureImage != defaultAzureLinuxImage {
		t.Fatalf("higher precedence os did not override provider defaults: os=%q image=%q azure=%q", cfg.OSImage, cfg.Image, cfg.AzureImage)
	}
}

func TestWandbConfigAPIKeyBeatsGenericWANDBEnv(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("WANDB_API_KEY", "generic-env-key")

	path := userConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("provider: wandb\nwandb:\n  apiKey: config-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Wandb.APIKey != "config-key" {
		t.Fatalf("Wandb.APIKey = %q, want config-key", cfg.Wandb.APIKey)
	}
}

func TestCrabboxWandbAPIKeyOverridesConfig(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_WANDB_API_KEY", "crabbox-env-key")
	t.Setenv("WANDB_API_KEY", "generic-env-key")

	path := userConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("provider: wandb\nwandb:\n  apiKey: config-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Wandb.APIKey != "crabbox-env-key" {
		t.Fatalf("Wandb.APIKey = %q, want crabbox-env-key", cfg.Wandb.APIKey)
	}
}

func TestTailscaleEnvOverrides(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_PROVIDER", "hetzner")
	t.Setenv("CRABBOX_NETWORK", "tailscale")
	t.Setenv("CRABBOX_TAILSCALE", "1")
	t.Setenv("CRABBOX_TAILSCALE_TAGS", "tag:crabbox,tag:ci")
	t.Setenv("CRABBOX_TAILSCALE_HOSTNAME_TEMPLATE", "lease-{slug}")
	t.Setenv("CRABBOX_TAILSCALE_AUTH_KEY", "tskey-secret")
	t.Setenv("CRABBOX_TAILSCALE_EXIT_NODE", "100.100.100.100")
	t.Setenv("CRABBOX_TAILSCALE_EXIT_NODE_ALLOW_LAN_ACCESS", "true")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Network != NetworkTailscale || !cfg.Tailscale.Enabled || cfg.Tailscale.AuthKey != "tskey-secret" || cfg.Tailscale.HostnameTemplate != "lease-{slug}" || cfg.Tailscale.ExitNode != "100.100.100.100" || !cfg.Tailscale.ExitNodeAllowLANAccess {
		t.Fatalf("unexpected tailscale env: network=%s tailscale=%#v", cfg.Network, cfg.Tailscale)
	}
	if len(cfg.Tailscale.Tags) != 2 || cfg.Tailscale.Tags[1] != "tag:ci" {
		t.Fatalf("unexpected tailscale tags: %#v", cfg.Tailscale.Tags)
	}
}

func TestProviderAliasCanonicalizedBeforeDefaults(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_PROVIDER", "google")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "crabbox-project")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "gcp" || cfg.ServerType != "c4-standard-192" {
		t.Fatalf("provider=%q type=%q want gcp c4-standard-192", cfg.Provider, cfg.ServerType)
	}
}

func TestConfigFileServerTypeIsExplicit(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	path := userConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("provider: gcp\nserverType: c4-standard-192\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServerType != "c4-standard-192" || !cfg.ServerTypeExplicit {
		t.Fatalf("serverType=%q explicit=%t, want explicit c4-standard-192", cfg.ServerType, cfg.ServerTypeExplicit)
	}
	if largeDefaultServerType(cfg) {
		t.Fatalf("explicit config serverType should not warn as a large default")
	}
}

func TestInvalidNetworkConfigFails(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	path := userConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("network: private\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(); err == nil {
		t.Fatal("expected invalid network config to fail")
	}
}

func TestInvalidNetworkEnvFails(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_NETWORK", "tailnet")

	if _, err := loadConfig(); err == nil {
		t.Fatal("expected invalid CRABBOX_NETWORK to fail")
	}
}

func TestDockerSandboxCPUEnvCanBeOverriddenByFlags(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_PROVIDER", "docker-sandbox")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_CPUS", "2.5")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig err=%v, want command flags to get a chance to override provider config", err)
	}
	fs := newFlagSet("test", io.Discard)
	values := registerProviderFlags(fs, cfg)
	if err := parseFlags(fs, []string{"--docker-sandbox-cpus", "2"}); err != nil {
		t.Fatal(err)
	}
	if err := applyProviderFlags(&cfg, fs, values); err != nil {
		t.Fatalf("applyProviderFlags err=%v, want valid CLI override to win", err)
	}
	if cfg.DockerSandbox.CPUs != 2 {
		t.Fatalf("cpus=%g, want CLI override 2", cfg.DockerSandbox.CPUs)
	}
}

func TestInvalidDockerSandboxCPUEnvNonNumericFailsDuringLoad(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_PROVIDER", "docker-sandbox")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_CPUS", "not-a-number")

	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "CRABBOX_DOCKER_SANDBOX_CPUS") {
		t.Fatalf("loadConfig err=%v, want docker-sandbox CPU env parse rejection", err)
	}
}

func TestAccessAuthState(t *testing.T) {
	for name, tc := range map[string]struct {
		access AccessConfig
		want   string
	}{
		"missing": {
			want: "missing",
		},
		"incomplete": {
			access: AccessConfig{ClientID: "client"},
			want:   "incomplete",
		},
		"service token": {
			access: AccessConfig{ClientID: "client", ClientSecret: "secret"},
			want:   "service-token",
		},
		"token": {
			access: AccessConfig{Token: "jwt"},
			want:   "token",
		},
		"service token plus token": {
			access: AccessConfig{ClientID: "client", ClientSecret: "secret", Token: "jwt"},
			want:   "service-token+token",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := accessAuthState(tc.access); got != tc.want {
				t.Fatalf("accessAuthState()=%q want %q", got, tc.want)
			}
		})
	}
}

func TestRepoConfigIsYamlOnly(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldwd) })

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_PROVIDER", "")
	t.Setenv("CRABBOX_DEFAULT_CLASS", "")
	if err := os.WriteFile(".crabbox.json", []byte(`{"profile":"json-profile","provider":"aws"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(".crabbox.yaml", []byte("profile: yaml-profile\nprovider: aws\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Profile != "yaml-profile" || cfg.Provider != "aws" {
		t.Fatalf("unexpected config: profile=%s provider=%s", cfg.Profile, cfg.Provider)
	}
}

func TestConfigHelperBranches(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "explicit.yaml"))

	if got := configPaths(); len(got) != 1 || got[0] != os.Getenv("CRABBOX_CONFIG") {
		t.Fatalf("configPaths=%v", got)
	}
	if got := writableConfigPath(); got != os.Getenv("CRABBOX_CONFIG") {
		t.Fatalf("writableConfigPath=%q", got)
	}

	cfgPath, err := writeUserFileConfig(fileConfig{Profile: "written", Provider: "aws"})
	if err != nil {
		t.Fatal(err)
	}
	if cfgPath != os.Getenv("CRABBOX_CONFIG") {
		t.Fatalf("write path=%q", cfgPath)
	}
	file, err := readFileConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if file.Profile != "written" || file.Provider != "aws" {
		t.Fatalf("file config=%#v", file)
	}
	info, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("config mode=%04o want 0600", got)
	}

	if err := os.Chmod(cfgPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := writeUserFileConfig(fileConfig{Profile: "rewritten"}); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("rewritten config mode=%04o want 0600", got)
	}
	if err := os.Chmod(cfgPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := configFilePermissionProblem(cfgPath); got == "" {
		t.Fatal("expected config permission problem")
	}
	if got := configFilePermissionProblem(""); got != "" {
		t.Fatalf("empty path permission problem=%q", got)
	}
	if got := configFilePermissionProblem(filepath.Join(t.TempDir(), "missing.yaml")); got != "" {
		t.Fatalf("missing path permission problem=%q", got)
	}
	if err := os.Chmod(cfgPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := configFilePermissionProblem(cfgPath); got != "" {
		t.Fatalf("secure config permission problem=%q", got)
	}

	empty, err := readFileConfig(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if empty.Profile != "" {
		t.Fatalf("missing file config=%#v", empty)
	}
	emptyPath := filepath.Join(t.TempDir(), "empty.yaml")
	if err := os.WriteFile(emptyPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	empty, err = readFileConfig(emptyPath)
	if err != nil {
		t.Fatal(err)
	}
	if empty.Profile != "" {
		t.Fatalf("empty file config=%#v", empty)
	}
	badPath := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(badPath, []byte("profile: [unterminated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readFileConfig(badPath); err == nil {
		t.Fatal("expected parse error for bad config")
	}

	if got := expandUserPath("~"); got != home {
		t.Fatalf("expand ~= %q want %q", got, home)
	}
	if got := expandUserPath("~/bin"); got != filepath.Join(home, "bin") {
		t.Fatalf("expand ~/bin=%q", got)
	}
	if got := expandUserPath("/tmp/x"); got != "/tmp/x" {
		t.Fatalf("absolute path changed to %q", got)
	}

	duration := 10 * time.Minute
	applyLeaseDuration(&duration, "")
	applyLeaseDuration(&duration, "bad")
	applyLeaseDuration(&duration, "0s")
	if duration != 10*time.Minute {
		t.Fatalf("invalid durations changed value to %s", duration)
	}
	applyLeaseDuration(&duration, "15m")
	if duration != 15*time.Minute {
		t.Fatalf("duration=%s", duration)
	}
}

func TestConfigHelperErrorBranches(t *testing.T) {
	t.Run("unavailable user config dir", func(t *testing.T) {
		t.Setenv("CRABBOX_CONFIG", "")
		t.Setenv("XDG_CONFIG_HOME", "")
		t.Setenv("HOME", "")
		if _, err := writeUserFileConfig(fileConfig{Profile: "missing-home"}); err == nil {
			t.Fatal("expected unavailable user config dir error")
		}
	})

	t.Run("config parent is file", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "not-dir")
		if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CRABBOX_CONFIG", filepath.Join(parent, "config.yaml"))
		if _, err := writeUserFileConfig(fileConfig{Profile: "mkdir-fails"}); err == nil {
			t.Fatal("expected config directory create error")
		}
	})

	t.Run("config path is directory", func(t *testing.T) {
		path := t.TempDir()
		t.Setenv("CRABBOX_CONFIG", path)
		if _, err := writeUserFileConfig(fileConfig{Profile: "write-fails"}); err == nil {
			t.Fatal("expected config write error")
		}
	})
}

func TestWindowsWSLWorkRoot(t *testing.T) {
	if got := windowsWSLWorkRoot(Config{}); got != defaultPOSIXWorkRoot {
		t.Fatalf("windowsWSLWorkRoot default=%q want %q", got, defaultPOSIXWorkRoot)
	}
	if got := windowsWSLWorkRoot(Config{WorkRoot: "/work/custom"}); got != "/work/custom" {
		t.Fatalf("windowsWSLWorkRoot custom=%q", got)
	}
}

func TestEnvHelperBranches(t *testing.T) {
	t.Setenv("CRABBOX_INT", "42")
	t.Setenv("CRABBOX_BAD_INT", "oops")
	if got := getenvInt("CRABBOX_INT", 7); got != 42 {
		t.Fatalf("int=%d", got)
	}
	if got := getenvInt("CRABBOX_BAD_INT", 7); got != 7 {
		t.Fatalf("bad int fallback=%d", got)
	}
	if got := getenvInt("CRABBOX_MISSING_INT", 7); got != 7 {
		t.Fatalf("missing int fallback=%d", got)
	}
	t.Setenv("CRABBOX_INT32", "2147483647")
	t.Setenv("CRABBOX_INT32_OVERFLOW", "2147483648")
	if got := getenvInt32("CRABBOX_INT32", 7); got != 2147483647 {
		t.Fatalf("int32=%d", got)
	}
	if got := getenvInt32("CRABBOX_INT32_OVERFLOW", 7); got != 7 {
		t.Fatalf("overflow int32 fallback=%d", got)
	}
	t.Setenv("CRABBOX_FLOAT", "1.5")
	t.Setenv("CRABBOX_BAD_FLOAT", "oops")
	if got := getenvFloat("CRABBOX_FLOAT", 7); got != 1.5 {
		t.Fatalf("float=%f", got)
	}
	if got := getenvFloat("CRABBOX_BAD_FLOAT", 7); got != 7 {
		t.Fatalf("bad float fallback=%f", got)
	}
	if got := getenvFloat("CRABBOX_MISSING_FLOAT", 7); got != 7 {
		t.Fatalf("missing float fallback=%f", got)
	}

	for _, tc := range []struct {
		name  string
		value string
		want  bool
		ok    bool
	}{
		{"CRABBOX_BOOL_TRUE", "yes", true, true},
		{"CRABBOX_BOOL_FALSE", "off", false, true},
		{"CRABBOX_BOOL_BAD", "maybe", false, false},
		{"CRABBOX_BOOL_EMPTY", "", false, false},
	} {
		if tc.value != "" {
			t.Setenv(tc.name, tc.value)
		}
		got, ok := getenvBool(tc.name)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("getenvBool(%s)=%v,%v want %v,%v", tc.name, got, ok, tc.want, tc.ok)
		}
	}

	list := splitCommaList(" CI, ,NODE_OPTIONS,CUSTOM_* ")
	if len(list) != 3 || list[0] != "CI" || list[2] != "CUSTOM_*" {
		t.Fatalf("splitCommaList=%v", list)
	}
	t.Setenv("CRABBOX_LIST", "CI,NODE_OPTIONS")
	if list, ok := getenvList("CRABBOX_LIST"); !ok || len(list) != 2 || list[1] != "NODE_OPTIONS" {
		t.Fatalf("getenvList=%v ok=%t", list, ok)
	}
}

func TestFileProfileEnvConfigUnmarshalRejectsNonMapping(t *testing.T) {
	var cfg struct {
		Env fileProfileEnvConfig `yaml:"env"`
	}
	err := yaml.Unmarshal([]byte("env: []\n"), &cfg)
	if err == nil || !strings.Contains(err.Error(), "profile env must be a mapping") {
		t.Fatalf("err=%v", err)
	}
}

func TestWriteUserFileConfigPreservesProfileEnvShape(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "explicit.yaml"))

	path, err := writeUserFileConfig(fileConfig{
		Profiles: map[string]fileProfileConfig{
			"qa": {
				Env: fileProfileEnvConfig{
					Values: map[string]string{"CI": "1"},
					Allow:  []string{"QA_*"},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "values:") || !strings.Contains(text, "CI: \"1\"") || !strings.Contains(text, "allow:") {
		t.Fatalf("unexpected profile env YAML:\n%s", text)
	}
	file, err := readFileConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	env := file.Profiles["qa"].Env
	if env.Values["CI"] != "1" || strings.Join(env.Allow, ",") != "QA_*" {
		t.Fatalf("env=%#v", env)
	}
}

func TestNamespaceDevboxSizeForConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{name: "explicit namespace size", cfg: Config{Namespace: NamespaceConfig{Size: " xl "}, Class: "standard"}, want: "XL"},
		{name: "explicit server type", cfg: Config{ServerType: " l ", ServerTypeExplicit: true, Class: "standard"}, want: "L"},
		{name: "class default", cfg: Config{Class: "large"}, want: "L"},
		{name: "empty default", cfg: Config{}, want: "M"},
		{name: "custom class", cfg: Config{Class: "gpu"}, want: "GPU"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := namespaceDevboxSizeForConfig(tc.cfg); got != tc.want {
				t.Fatalf("size=%q want %q", got, tc.want)
			}
		})
	}
}

func TestConfigServerTypeHelperBranches(t *testing.T) {
	if got := incusServerTypeForConfig(Config{}); got != "container" {
		t.Fatalf("incus default=%q", got)
	}
	if got := incusServerTypeForConfig(Config{Incus: IncusConfig{InstanceType: "vm", Image: "images:ubuntu/24.04/cloud"}}); got != "vm:images:ubuntu/24.04/cloud" {
		t.Fatalf("incus vm=%q", got)
	}
	if got := proxmoxServerTypeForConfig(Config{}); got != "template" {
		t.Fatalf("proxmox default=%q", got)
	}
	if got := proxmoxServerTypeForConfig(Config{Proxmox: ProxmoxConfig{TemplateID: 9000}}); got != "template-9000" {
		t.Fatalf("proxmox template=%q", got)
	}
	for _, tc := range []struct {
		class string
		want  string
	}{
		{class: "standard", want: "S"},
		{class: "fast", want: "M"},
		{class: "beast", want: "XL"},
	} {
		if got := namespaceDevboxSizeForClass(tc.class); got != tc.want {
			t.Fatalf("namespaceDevboxSizeForClass(%q)=%q want %q", tc.class, got, tc.want)
		}
	}
}

func TestApplyFileConfigCloudProviderBranches(t *testing.T) {
	enabled := true
	disabled := false
	cfg := Config{}
	applyFileConfig(&cfg, fileConfig{
		TargetOS:         targetLinux,
		Desktop:          &enabled,
		Browser:          &disabled,
		Code:             &enabled,
		ServerType:       "custom-type",
		CoordinatorToken: "coord-token",
		HostID:           "",
		Broker: &fileBrokerConfig{
			Provider: "aws",
			Access:   &fileAccessConfig{ClientID: "access-id", ClientSecret: "access-secret", Token: "access-token"},
		},
		Hetzner: &fileHetznerConfig{Location: "fsn1", Image: "ubuntu-24.04", SSHKey: "hetzner-key"},
		AWS: &fileAWSConfig{
			Region:          "eu-central-1",
			AMI:             "ami-test",
			SecurityGroupID: "sg-test",
			SubnetID:        "subnet-test",
			InstanceProfile: "profile-test",
			RootGB:          123,
			SSHCIDRs:        []string{"198.51.100.1/32"},
			MacHostID:       "h-mac",
		},
		Azure: &fileAzureConfig{
			SubscriptionID: "sub",
			TenantID:       "tenant",
			ClientID:       "client",
			Location:       "westeurope",
			ResourceGroup:  "rg",
			Image:          "ubuntu",
			OSDisk:         "ephemeral",
			VNet:           "vnet",
			Subnet:         "subnet",
			NSG:            "nsg",
			SSHCIDRs:       []string{"198.51.100.2/32"},
			Network:        "public",
		},
		AzureDynamicSessions: &fileAzureDynamicSessionsConfig{
			Endpoint:    "https://pool.env.eastus.azurecontainerapps.io",
			Pool:        "pool",
			APIVersion:  "2025-02-02-preview",
			Workdir:     "/workspace/file",
			TimeoutSecs: 120,
		},
		GCP: &fileGCPConfig{
			Project:        "project",
			Zone:           "europe-west1-b",
			Image:          "ubuntu",
			Network:        "net",
			Subnet:         "subnet",
			Tags:           []string{"crabbox"},
			SSHCIDRs:       []string{"198.51.100.3/32"},
			RootGB:         456,
			ServiceAccount: "runner@example.iam.gserviceaccount.com",
		},
	})
	if !cfg.Desktop || cfg.Browser || !cfg.Code || cfg.TargetOS != targetLinux || cfg.ServerType != "custom-type" {
		t.Fatalf("top-level config not applied: %#v", cfg)
	}
	if cfg.Provider != "aws" || cfg.Access.ClientID != "access-id" || cfg.CoordToken != "coord-token" {
		t.Fatalf("broker/access config not applied: provider=%s access=%#v token=%s", cfg.Provider, cfg.Access, cfg.CoordToken)
	}
	if cfg.Location != "fsn1" || cfg.ProviderKey != "hetzner-key" || cfg.HostID != "h-mac" || cfg.AWSRootGB != 123 {
		t.Fatalf("hetzner/aws config not applied: location=%s key=%s host=%s root=%d", cfg.Location, cfg.ProviderKey, cfg.HostID, cfg.AWSRootGB)
	}
	if cfg.AzureOSDisk != "ephemeral" || !cfg.AzureOSDiskExplicit || cfg.AzureNetwork != "public" {
		t.Fatalf("azure config not applied: %#v", cfg)
	}
	if cfg.AzureDynamicSessions.Pool != "pool" || cfg.AzureDynamicSessions.Workdir != "/workspace/file" || cfg.AzureDynamicSessions.TimeoutSecs != 120 {
		t.Fatalf("azure dynamic sessions config not applied: %#v", cfg.AzureDynamicSessions)
	}
	if cfg.GCPProject != "project" || !cfg.gcpProjectExplicit || cfg.GCPRootGB != 456 || cfg.GCPServiceAccount == "" {
		t.Fatalf("gcp config not applied: %#v", cfg)
	}
}

func TestApplyFileJobConfigCoversJobOptions(t *testing.T) {
	enabled := true
	disabled := false
	job := applyFileJobConfig(JobConfig{}, fileJobConfig{
		Provider:     "aws",
		TargetOS:     targetLinux,
		Windows:      &fileWindowsConfig{Mode: windowsModeWSL2},
		Profile:      "ci",
		Class:        "large",
		Architecture: "arm64",
		Type:         "m8i.large",
		Capacity:     &fileCapacityConfig{Market: "spot"},
		Market:       "on-demand",
		TTL:          "45m",
		IdleTimeout:  "5m",
		Desktop:      &enabled,
		Browser:      &disabled,
		Code:         &enabled,
		Network:      "tailscale",
		Hydrate: &fileJobHydrateConfig{
			Actions:          &enabled,
			GitHubRunner:     &enabled,
			WaitTimeout:      "12m",
			KeepAliveMinutes: 3,
		},
		Actions: &fileJobActionsConfig{
			Repo:     "openclaw/crabbox",
			Workflow: ".github/workflows/ci.yml",
			Job:      "test",
			Ref:      "main",
			Fields:   []string{"a=1", "a=1", "b=2"},
		},
		Shell:          &enabled,
		Command:        "pnpm test",
		NoSync:         &enabled,
		SyncOnly:       &disabled,
		Checksum:       &enabled,
		ForceSyncLarge: &enabled,
		JUnit:          []string{"junit.xml", "junit.xml"},
		Downloads:      []string{"out=out", "out=out"},
		Stop:           "always",
	})
	if job.Provider != "aws" || job.Target != targetLinux || job.WindowsMode != windowsModeWSL2 || job.Profile != "ci" || job.Class != "large" || job.Architecture != "arm64" || job.ServerType != "m8i.large" || job.Market != "on-demand" {
		t.Fatalf("basic job fields not applied: %#v", job)
	}
	if job.TTL != 45*time.Minute || job.IdleTimeout != 5*time.Minute {
		t.Fatalf("job durations ttl=%s idle=%s", job.TTL, job.IdleTimeout)
	}
	if job.Desktop == nil || !*job.Desktop || job.Browser == nil || *job.Browser || job.Code == nil || !*job.Code || job.Network != "tailscale" {
		t.Fatalf("job UI/network fields not applied: %#v", job)
	}
	if !job.Hydrate.Actions || !job.Hydrate.GitHubRunner || job.Hydrate.WaitTimeout != 12*time.Minute || job.Hydrate.KeepAliveMinutes != 3 {
		t.Fatalf("hydrate not applied: %#v", job.Hydrate)
	}
	if job.Actions.Repo != "openclaw/crabbox" || job.Actions.Workflow != ".github/workflows/ci.yml" || job.Actions.Job != "test" || job.Actions.Ref != "main" || len(job.Actions.Fields) != 2 {
		t.Fatalf("actions not applied: %#v", job.Actions)
	}
	if !job.Shell || job.Command != "pnpm test" || !job.NoSync || job.SyncOnly || job.Checksum == nil || !*job.Checksum || !job.ForceSyncLarge || len(job.JUnit) != 1 || len(job.Downloads) != 1 || job.Stop != "always" {
		t.Fatalf("command/sync fields not applied: %#v", job)
	}
}
