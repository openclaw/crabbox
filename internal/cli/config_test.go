package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"CRABBOX_COORDINATOR",
		"CRABBOX_COORDINATOR_TOKEN",
		"CRABBOX_COORDINATOR_ADMIN_TOKEN",
		"CRABBOX_ADMIN_TOKEN",
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
		"CRABBOX_NAMESPACE_IMAGE",
		"CRABBOX_NAMESPACE_SIZE",
		"CRABBOX_NAMESPACE_REPOSITORY",
		"CRABBOX_NAMESPACE_SITE",
		"CRABBOX_NAMESPACE_VOLUME_SIZE_GB",
		"CRABBOX_NAMESPACE_AUTO_STOP_IDLE_TIMEOUT",
		"CRABBOX_NAMESPACE_WORK_ROOT",
		"CRABBOX_NAMESPACE_DELETE_ON_RELEASE",
	} {
		t.Setenv(key, "")
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
  token: secret
  adminToken: admin-secret
  provider: aws
  access:
    clientId: access-client
    clientSecret: access-secret
    token: access-jwt
class: standard
target: windows
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
daytona:
  apiUrl: https://daytona.example.test/api
  snapshot: crabbox-ready
  target: us
  user: daytona
  workRoot: /home/daytona/crabbox
  sshGatewayHost: ssh.daytona.example.test
  sshAccessMinutes: 12
e2b:
  apiUrl: https://api.e2b.example.test
  domain: e2b.example.test
  template: crabbox-ready
  workdir: work/repo
  user: sandbox
islo:
  baseUrl: https://islo.example.test
  image: docker.io/library/ubuntu:24.04
  workdir: crabbox
  gatewayProfile: default
  snapshotName: snap-ready
  vcpus: 4
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
  junit:
    - junit.xml
cache:
  pnpm: true
  npm: false
  docker: true
  git: true
  maxGB: 120
  purgeOnRelease: true
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
	if cfg.Daytona.APIURL != "https://daytona.example.test/api" || cfg.Daytona.Snapshot != "crabbox-ready" || cfg.Daytona.Target != "us" || cfg.Daytona.User != "daytona" || cfg.Daytona.WorkRoot != "/home/daytona/crabbox" || cfg.Daytona.SSHGatewayHost != "ssh.daytona.example.test" || cfg.Daytona.SSHAccessMinutes != 12 {
		t.Fatalf("daytona config not loaded: %#v", cfg.Daytona)
	}
	if cfg.E2B.APIURL != "https://api.e2b.example.test" || cfg.E2B.Domain != "e2b.example.test" || cfg.E2B.Template != "crabbox-ready" || cfg.E2B.Workdir != "work/repo" || cfg.E2B.User != "sandbox" {
		t.Fatalf("e2b config not loaded: %#v", cfg.E2B)
	}
	if cfg.Islo.BaseURL != "https://islo.example.test" || cfg.Islo.Image != "docker.io/library/ubuntu:24.04" || cfg.Islo.Workdir != "crabbox" || cfg.Islo.GatewayProfile != "default" || cfg.Islo.SnapshotName != "snap-ready" || cfg.Islo.VCPUs != 4 || cfg.Islo.MemoryMB != 8192 || cfg.Islo.DiskGB != 40 {
		t.Fatalf("islo config not loaded: %#v", cfg.Islo)
	}
	if cfg.Tensorlake.APIURL != "https://api.tensorlake.example.test" || cfg.Tensorlake.CLIPath != "/usr/local/bin/tl" || cfg.Tensorlake.Image != "ubuntu-22.04" || cfg.Tensorlake.Snapshot != "snap-tl" || cfg.Tensorlake.OrganizationID != "org-tl" || cfg.Tensorlake.ProjectID != "proj-tl" || cfg.Tensorlake.Namespace != "ns-tl" || cfg.Tensorlake.Workdir != "/workspace/crabbox-test" || cfg.Tensorlake.CPUs != 4 || cfg.Tensorlake.MemoryMB != 8192 || cfg.Tensorlake.DiskMB != 30000 || cfg.Tensorlake.TimeoutSecs != 1800 || !cfg.Tensorlake.NoInternet {
		t.Fatalf("tensorlake config not loaded: %#v", cfg.Tensorlake)
	}
	if cfg.Proxmox.APIURL != "https://pve.example.test:8006" || cfg.Proxmox.TokenID != "crabbox@pve!test" || cfg.Proxmox.TokenSecret != "proxmox-secret" || cfg.Proxmox.Node != "pve1" || cfg.Proxmox.TemplateID != 9000 || cfg.Proxmox.Storage != "local-lvm" || cfg.Proxmox.Pool != "crabbox" || cfg.Proxmox.Bridge != "vmbr1" || cfg.Proxmox.User != "runner" || cfg.Proxmox.WorkRoot != "/work/proxmox" || cfg.Proxmox.FullClone || !cfg.Proxmox.InsecureTLS {
		t.Fatalf("proxmox config not loaded: %#v", cfg.Proxmox)
	}
	if cfg.Semaphore.Host != "semaphore.example.test" || cfg.Semaphore.Token != "semaphore-token" || cfg.Semaphore.Project != "crabbox" || cfg.Semaphore.Machine != "f1-standard-4" || cfg.Semaphore.OSImage != "ubuntu2404" || cfg.Semaphore.IdleTimeout != "15m" {
		t.Fatalf("semaphore config not loaded: %#v", cfg.Semaphore)
	}
	if cfg.Sprites.APIURL != "https://api.sprites.example.test" || cfg.Sprites.WorkRoot != "/home/sprite/test" {
		t.Fatalf("sprites config not loaded: %#v", cfg.Sprites)
	}
	if cfg.Static.Host != "win-dev.local" || cfg.Static.User != "peter" || cfg.Static.Port != "22" || cfg.WorkRoot != "/home/peter/crabbox" {
		t.Fatalf("static config not loaded: static=%#v workRoot=%s", cfg.Static, cfg.WorkRoot)
	}
	if len(cfg.Results.JUnit) != 1 || cfg.Results.JUnit[0] != "junit.xml" {
		t.Fatalf("results config not loaded: %#v", cfg.Results)
	}
	if !cfg.Cache.Pnpm || cfg.Cache.Npm || !cfg.Cache.Docker || !cfg.Cache.Git || cfg.Cache.MaxGB != 120 || !cfg.Cache.PurgeOnRelease {
		t.Fatalf("cache config not loaded: %#v", cfg.Cache)
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
	t.Setenv("CRABBOX_AZURE_SSH_CIDRS", "198.51.100.9/32,203.0.113.10/32")
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
	t.Setenv("CRABBOX_CACHE_PNPM", "false")
	t.Setenv("CRABBOX_CACHE_NPM", "false")
	t.Setenv("CRABBOX_CACHE_DOCKER", "true")
	t.Setenv("CRABBOX_CACHE_GIT", "false")
	t.Setenv("CRABBOX_CACHE_PURGE_ON_RELEASE", "true")
	t.Setenv("CRABBOX_SYNC_CHECKSUM", "true")
	t.Setenv("CRABBOX_SYNC_DELETE", "false")
	t.Setenv("CRABBOX_SYNC_GIT_SEED", "false")
	t.Setenv("CRABBOX_SYNC_FINGERPRINT", "false")
	t.Setenv("CRABBOX_SYNC_TIMEOUT", "45m")
	t.Setenv("CRABBOX_SYNC_ALLOW_LARGE", "true")
	t.Setenv("CRABBOX_ENV_ALLOW", "CI,NODE_OPTIONS,CUSTOM_*")
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
	if cfg.Daytona.APIKey != "daytona-api-env" || cfg.Daytona.APIURL != "https://daytona-env.example/api" || cfg.Daytona.Snapshot != "snapshot-env" || cfg.Daytona.Target != "target-env" || cfg.Daytona.User != "daytona-env-user" || cfg.Daytona.WorkRoot != "/home/daytona/env" || cfg.Daytona.SSHGatewayHost != "ssh.env.example" || cfg.Daytona.SSHAccessMinutes != 44 {
		t.Fatalf("unexpected daytona env: %#v", cfg.Daytona)
	}
	if cfg.E2B.APIKey != "e2b-api-env" || cfg.E2B.APIURL != "https://api.e2b-env.example" || cfg.E2B.Domain != "e2b-env.example" || cfg.E2B.Template != "template-env" || cfg.E2B.Workdir != "env-workdir" || cfg.E2B.User != "sandbox-env" {
		t.Fatalf("unexpected e2b env: %#v", cfg.E2B)
	}
	if cfg.Islo.APIKey != "islo-api-env" || cfg.Islo.BaseURL != "https://islo-env.example" || cfg.Islo.Image != "ubuntu:env" || cfg.Islo.Workdir != "env-workdir" || cfg.Islo.GatewayProfile != "env-gateway" || cfg.Islo.SnapshotName != "env-snapshot" || cfg.Islo.VCPUs != 8 || cfg.Islo.MemoryMB != 16384 || cfg.Islo.DiskGB != 80 {
		t.Fatalf("unexpected islo env: %#v", cfg.Islo)
	}
	if cfg.Tensorlake.APIKey != "tl-api-env" || cfg.Tensorlake.APIURL != "https://api.tl-env.example" || cfg.Tensorlake.CLIPath != "/opt/tl/bin/tensorlake" || cfg.Tensorlake.Image != "ubuntu:tl-env" || cfg.Tensorlake.Snapshot != "snap-tl-env" || cfg.Tensorlake.OrganizationID != "org-tl-env" || cfg.Tensorlake.ProjectID != "proj-tl-env" || cfg.Tensorlake.Namespace != "ns-tl-env" || cfg.Tensorlake.Workdir != "/workspace/tl-env" || cfg.Tensorlake.CPUs != 2.5 || cfg.Tensorlake.MemoryMB != 4096 || cfg.Tensorlake.DiskMB != 20480 || cfg.Tensorlake.TimeoutSecs != 900 || !cfg.Tensorlake.NoInternet {
		t.Fatalf("unexpected tensorlake env: %#v", cfg.Tensorlake)
	}
	if cfg.Proxmox.APIURL != "https://pve-env.example:8006" || cfg.Proxmox.TokenID != "runner@pve!env" || cfg.Proxmox.TokenSecret != "proxmox-env-secret" || cfg.Proxmox.Node != "pve-env" || cfg.Proxmox.TemplateID != 9100 || cfg.Proxmox.Storage != "ceph-env" || cfg.Proxmox.Pool != "pool-env" || cfg.Proxmox.Bridge != "vmbr2" || cfg.Proxmox.User != "runner-env" || cfg.Proxmox.WorkRoot != "/work/proxmox-env" || cfg.Proxmox.FullClone || !cfg.Proxmox.InsecureTLS {
		t.Fatalf("unexpected proxmox env: %#v", cfg.Proxmox)
	}
	if cfg.Semaphore.Host != "semaphore-env.example.test" || cfg.Semaphore.Token != "semaphore-token-env" || cfg.Semaphore.Project != "semaphore-project-env" || cfg.Semaphore.Machine != "f1-standard-env" || cfg.Semaphore.OSImage != "ubuntu-env" || cfg.Semaphore.IdleTimeout != "22m" {
		t.Fatalf("unexpected semaphore env: %#v", cfg.Semaphore)
	}
	if cfg.Sprites.Token != "sprites-token-env" || cfg.Sprites.APIURL != "https://api.sprites-env.example" || cfg.Sprites.WorkRoot != "/home/sprite/env" {
		t.Fatalf("unexpected sprites env: %#v", cfg.Sprites)
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
	if len(cfg.Results.JUnit) != 2 || cfg.Results.JUnit[1] != "build/test.xml" {
		t.Fatalf("unexpected results env: %#v", cfg.Results)
	}
	if cfg.Cache.Pnpm || cfg.Cache.Npm || !cfg.Cache.Docker || cfg.Cache.Git || !cfg.Cache.PurgeOnRelease {
		t.Fatalf("unexpected cache env: %#v", cfg.Cache)
	}
	if !cfg.Sync.Checksum || cfg.Sync.Delete || cfg.Sync.GitSeed || cfg.Sync.Fingerprint || cfg.Sync.Timeout != 45*time.Minute || !cfg.Sync.AllowLarge {
		t.Fatalf("unexpected sync env: %#v", cfg.Sync)
	}
	if len(cfg.EnvAllow) != 3 || cfg.EnvAllow[2] != "CUSTOM_*" {
		t.Fatalf("unexpected env allow: %#v", cfg.EnvAllow)
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

func TestApplyFileJobConfigCoversJobOptions(t *testing.T) {
	enabled := true
	disabled := false
	job := applyFileJobConfig(JobConfig{}, fileJobConfig{
		Provider:    "aws",
		TargetOS:    targetLinux,
		Windows:     &fileWindowsConfig{Mode: windowsModeWSL2},
		Profile:     "ci",
		Class:       "large",
		Type:        "m8i.large",
		Capacity:    &fileCapacityConfig{Market: "spot"},
		Market:      "on-demand",
		TTL:         "45m",
		IdleTimeout: "5m",
		Desktop:     &enabled,
		Browser:     &disabled,
		Code:        &enabled,
		Network:     "tailscale",
		Hydrate: &fileJobHydrateConfig{
			Actions:          &enabled,
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
	if job.Provider != "aws" || job.Target != targetLinux || job.WindowsMode != windowsModeWSL2 || job.Profile != "ci" || job.Class != "large" || job.ServerType != "m8i.large" || job.Market != "on-demand" {
		t.Fatalf("basic job fields not applied: %#v", job)
	}
	if job.TTL != 45*time.Minute || job.IdleTimeout != 5*time.Minute {
		t.Fatalf("job durations ttl=%s idle=%s", job.TTL, job.IdleTimeout)
	}
	if job.Desktop == nil || !*job.Desktop || job.Browser == nil || *job.Browser || job.Code == nil || !*job.Code || job.Network != "tailscale" {
		t.Fatalf("job UI/network fields not applied: %#v", job)
	}
	if !job.Hydrate.Actions || job.Hydrate.WaitTimeout != 12*time.Minute || job.Hydrate.KeepAliveMinutes != 3 {
		t.Fatalf("hydrate not applied: %#v", job.Hydrate)
	}
	if job.Actions.Repo != "openclaw/crabbox" || job.Actions.Workflow != ".github/workflows/ci.yml" || job.Actions.Job != "test" || job.Actions.Ref != "main" || len(job.Actions.Fields) != 2 {
		t.Fatalf("actions not applied: %#v", job.Actions)
	}
	if !job.Shell || job.Command != "pnpm test" || !job.NoSync || job.SyncOnly || job.Checksum == nil || !*job.Checksum || !job.ForceSyncLarge || len(job.JUnit) != 1 || len(job.Downloads) != 1 || job.Stop != "always" {
		t.Fatalf("command/sync fields not applied: %#v", job)
	}
}
