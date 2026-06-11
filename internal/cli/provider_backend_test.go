package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type recordingCommandRunner struct {
	calls  []LocalCommandRequest
	result LocalCommandResult
	err    error
}

func (r *recordingCommandRunner) Run(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	r.calls = append(r.calls, req)
	return r.result, r.err
}

func testRuntimeWithRunner(r CommandRunner) Runtime {
	return Runtime{Stdout: io.Discard, Stderr: io.Discard, Clock: realClock{}, Exec: r}
}

func TestProviderRegistryCanonicalAndAliases(t *testing.T) {
	for _, tc := range []struct {
		name      string
		canonical string
	}{
		{name: "hetzner", canonical: "hetzner"},
		{name: "aws", canonical: "aws"},
		{name: "azure", canonical: "azure"},
		{name: "azure-dynamic-sessions", canonical: "azure-dynamic-sessions"},
		{name: "gcp", canonical: "gcp"},
		{name: "google", canonical: "gcp"},
		{name: "google-cloud", canonical: "gcp"},
		{name: "incus", canonical: "incus"},
		{name: "proxmox", canonical: "proxmox"},
		{name: "ssh", canonical: "ssh"},
		{name: "static", canonical: "ssh"},
		{name: "static-ssh", canonical: "ssh"},
		{name: "exe-dev", canonical: "exe-dev"},
		{name: "exe", canonical: "exe-dev"},
		{name: "exedev", canonical: "exe-dev"},
		{name: "blacksmith", canonical: "blacksmith-testbox"},
		{name: "blacksmith-testbox", canonical: "blacksmith-testbox"},
		{name: "namespace", canonical: "namespace-devbox"},
		{name: "namespace-devbox", canonical: "namespace-devbox"},
		{name: "morph", canonical: "morph"},
		{name: "daytona", canonical: "daytona"},
		{name: "islo", canonical: "islo"},
		{name: "e2b", canonical: "e2b"},
		{name: "modal", canonical: "modal"},
		{name: "cloudflare", canonical: "cloudflare"},
		{name: "cf", canonical: "cloudflare"},
		{name: "sprites", canonical: "sprites"},
		{name: "local-container", canonical: "local-container"},
		{name: "docker", canonical: "local-container"},
		{name: "container", canonical: "local-container"},
		{name: "local-docker", canonical: "local-container"},
	} {
		provider, err := ProviderFor(tc.name)
		if err != nil {
			t.Fatalf("ProviderFor(%q): %v", tc.name, err)
		}
		if provider.Name() != tc.canonical {
			t.Fatalf("ProviderFor(%q).Name() = %q, want %q", tc.name, provider.Name(), tc.canonical)
		}
	}
	if _, err := ProviderFor("missing"); err == nil {
		t.Fatal("expected missing provider to fail")
	}
}

func TestProviderHelpAllIncludesWandbAndMorph(t *testing.T) {
	all := providerHelpAll()
	if !strings.Contains(all, "wandb") || !strings.Contains(all, "morph") {
		t.Fatalf("providerHelpAll() = %q, want wandb and morph", all)
	}
	if strings.Contains(providerHelpSSH(), "azure-dynamic-sessions") {
		t.Fatalf("providerHelpSSH() = %q, want only ssh-capable providers", providerHelpSSH())
	}
}

func TestAzureBackendFlagRoutesToDynamicSessions(t *testing.T) {
	defaults := baseConfig()
	fs := newFlagSet("test", io.Discard)
	values := registerLeaseCreateFlags(fs, defaults)
	if err := parseFlags(fs, []string{"--provider", "azure", "--azure-backend", "dynamic-sessions"}); err != nil {
		t.Fatal(err)
	}
	cfg := defaults
	if err := applyLeaseCreateFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "azure-dynamic-sessions" || cfg.AzureBackend != AzureBackendDynamicSessions || cfg.ServerType != "" {
		t.Fatalf("provider=%q azureBackend=%q serverType=%q", cfg.Provider, cfg.AzureBackend, cfg.ServerType)
	}
}

func TestProviderFlagsRouteAzureBackendWithoutLeaseCreate(t *testing.T) {
	defaults := baseConfig()
	fs := newFlagSet("test", io.Discard)
	providerFlag := fs.String("provider", defaults.Provider, "")
	values := registerProviderFlags(fs, defaults)
	if err := parseFlags(fs, []string{"--provider", "azure", "--azure-backend", "dynamic-sessions"}); err != nil {
		t.Fatal(err)
	}
	cfg := defaults
	cfg.Provider = *providerFlag
	if err := applyProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "azure-dynamic-sessions" || cfg.AzureBackend != AzureBackendDynamicSessions {
		t.Fatalf("provider=%q azureBackend=%q", cfg.Provider, cfg.AzureBackend)
	}
}

func TestAzureBackendFlagOverridesDynamicSessionsConfig(t *testing.T) {
	defaults := baseConfig()
	defaults.Provider = "azure-dynamic-sessions"
	defaults.AzureBackend = AzureBackendDynamicSessions
	fs := newFlagSet("test", io.Discard)
	values := registerLeaseCreateFlags(fs, defaults)
	if err := parseFlags(fs, []string{"--azure-backend", "vm"}); err != nil {
		t.Fatal(err)
	}
	cfg := defaults
	if err := applyLeaseCreateFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "azure" || cfg.AzureBackend != AzureBackendVM || cfg.ServerType == "" {
		t.Fatalf("provider=%q azureBackend=%q serverType=%q", cfg.Provider, cfg.AzureBackend, cfg.ServerType)
	}
}

func TestProviderFlagsOverrideDynamicSessionsConfigWithoutLeaseCreate(t *testing.T) {
	defaults := baseConfig()
	defaults.Provider = "azure-dynamic-sessions"
	defaults.AzureBackend = AzureBackendDynamicSessions
	fs := newFlagSet("test", io.Discard)
	providerFlag := fs.String("provider", defaults.Provider, "")
	values := registerProviderFlags(fs, defaults)
	if err := parseFlags(fs, []string{"--azure-backend", "vm"}); err != nil {
		t.Fatal(err)
	}
	cfg := defaults
	cfg.Provider = *providerFlag
	if err := applyProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "azure" || cfg.AzureBackend != AzureBackendVM {
		t.Fatalf("provider=%q azureBackend=%q", cfg.Provider, cfg.AzureBackend)
	}
}

func TestLoadBackendWrapsCoordinatorOnlyForSupportedSSHProviders(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "aws"
	cfg.Coordinator = "https://coordinator.example"
	backend, err := loadBackend(cfg, testRuntimeWithRunner(&recordingCommandRunner{}))
	if err != nil {
		t.Fatalf("load aws coordinator backend: %v", err)
	}
	if _, ok := backend.(*coordinatorLeaseBackend); !ok {
		t.Fatalf("backend=%T, want coordinatorLeaseBackend", backend)
	}

	cfg.Provider = "ssh"
	backend, err = loadBackend(cfg, testRuntimeWithRunner(&recordingCommandRunner{}))
	if err != nil {
		t.Fatalf("load static ssh backend: %v", err)
	}
	if _, ok := backend.(*coordinatorLeaseBackend); ok {
		t.Fatalf("static ssh unexpectedly used coordinator wrapper")
	}

	cfg.Provider = "exe-dev"
	backend, err = loadBackend(cfg, testRuntimeWithRunner(&recordingCommandRunner{}))
	if err != nil {
		t.Fatalf("load exe-dev backend: %v", err)
	}
	if _, ok := backend.(SSHLeaseBackend); !ok {
		t.Fatalf("backend=%T, want ssh lease backend", backend)
	}

	cfg.Provider = "blacksmith-testbox"
	backend, err = loadBackend(cfg, testRuntimeWithRunner(&recordingCommandRunner{}))
	if err != nil {
		t.Fatalf("load blacksmith backend: %v", err)
	}
	if _, ok := backend.(DelegatedRunBackend); !ok {
		t.Fatalf("backend=%T, want delegated run backend", backend)
	}

	cfg.Provider = "namespace-devbox"
	backend, err = loadBackend(cfg, testRuntimeWithRunner(&recordingCommandRunner{}))
	if err != nil {
		t.Fatalf("load namespace backend: %v", err)
	}
	if _, ok := backend.(SSHLeaseBackend); !ok {
		t.Fatalf("backend=%T, want ssh lease backend", backend)
	}

	cfg.Provider = "proxmox"
	backend, err = loadBackend(cfg, testRuntimeWithRunner(&recordingCommandRunner{}))
	if err != nil {
		t.Fatalf("load proxmox backend: %v", err)
	}
	if _, ok := backend.(SSHLeaseBackend); !ok {
		t.Fatalf("backend=%T, want ssh lease backend", backend)
	}

	cfg.Provider = "e2b"
	backend, err = loadBackend(cfg, testRuntimeWithRunner(&recordingCommandRunner{}))
	if err != nil {
		t.Fatalf("load e2b backend: %v", err)
	}
	if _, ok := backend.(DelegatedRunBackend); !ok {
		t.Fatalf("backend=%T, want delegated run backend", backend)
	}

	cfg.Provider = "modal"
	backend, err = loadBackend(cfg, testRuntimeWithRunner(&recordingCommandRunner{}))
	if err != nil {
		t.Fatalf("load modal backend: %v", err)
	}
	if _, ok := backend.(DelegatedRunBackend); !ok {
		t.Fatalf("backend=%T, want delegated run backend", backend)
	}

	cfg.Provider = "sprites"
	backend, err = loadBackend(cfg, testRuntimeWithRunner(&recordingCommandRunner{}))
	if err != nil {
		t.Fatalf("load sprites backend: %v", err)
	}
	if _, ok := backend.(SSHLeaseBackend); !ok {
		t.Fatalf("backend=%T, want ssh lease backend", backend)
	}

	cfg.Provider = "local-container"
	backend, err = loadBackend(cfg, testRuntimeWithRunner(&recordingCommandRunner{}))
	if err != nil {
		t.Fatalf("load local-container backend: %v", err)
	}
	if _, ok := backend.(SSHLeaseBackend); !ok {
		t.Fatalf("backend=%T, want ssh lease backend", backend)
	}
}

func TestProviderFlagsApplyNamespaceWithoutCoreEdits(t *testing.T) {
	defaults := baseConfig()
	fs := newFlagSet("test", io.Discard)
	provider := fs.String("provider", defaults.Provider, "")
	values := registerProviderFlags(fs, defaults)
	if err := parseFlags(fs, []string{
		"--provider", "namespace-devbox",
		"--namespace-image", "crabbox-ready",
		"--namespace-size", "L",
		"--namespace-work-root", "/workspaces/test",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := defaults
	cfg.Provider = *provider
	if err := applyProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Namespace.Image != "crabbox-ready" || cfg.Namespace.Size != "L" || cfg.Namespace.WorkRoot != "/workspaces/test" {
		t.Fatalf("namespace flags not applied: %#v", cfg.Namespace)
	}
}

func TestProviderFlagsApplyMorphWithoutCoreEdits(t *testing.T) {
	defaults := baseConfig()
	fs := newFlagSet("test", io.Discard)
	provider := fs.String("provider", defaults.Provider, "")
	values := registerProviderFlags(fs, defaults)
	if err := parseFlags(fs, []string{
		"--provider", "morph",
		"--morph-api-url", "https://morph.example.test",
		"--morph-snapshot", "snapshot_123",
		"--morph-work-root", "/tmp/morph-work",
		"--morph-delete-on-release",
		"--morph-wake-on-ssh=false",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := defaults
	cfg.Provider = *provider
	if err := applyProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Morph.APIURL != "https://morph.example.test" || cfg.Morph.Snapshot != "snapshot_123" || cfg.Morph.WorkRoot != "/tmp/morph-work" || cfg.WorkRoot != "/tmp/morph-work" || !cfg.Morph.DeleteOnRelease || cfg.Morph.WakeOnSSH {
		t.Fatalf("morph flags not applied: %#v workRoot=%q", cfg.Morph, cfg.WorkRoot)
	}
}

func TestProviderFlagsApplyExeDevWithoutCoreEdits(t *testing.T) {
	defaults := baseConfig()
	fs := newFlagSet("test", io.Discard)
	provider := fs.String("provider", defaults.Provider, "")
	values := registerProviderFlags(fs, defaults)
	if err := parseFlags(fs, []string{
		"--provider", "exe",
		"--exe-dev-control-host", "ssh.exe.example.test",
		"--exe-dev-image", "ubuntu:24.04",
		"--exe-dev-user", "runner",
		"--exe-dev-work-root", "/tmp/work",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := defaults
	cfg.Provider = *provider
	if err := applyProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.ExeDev.ControlHost != "ssh.exe.example.test" || cfg.ExeDev.Image != "ubuntu:24.04" || cfg.ExeDev.User != "runner" || cfg.SSHUser != "runner" || cfg.WorkRoot != "/tmp/work" {
		t.Fatalf("exe-dev flags not applied: %#v", cfg.ExeDev)
	}
}

func TestProviderFlagsApplyLocalContainerWithoutCoreEdits(t *testing.T) {
	defaults := baseConfig()
	fs := newFlagSet("test", io.Discard)
	provider := fs.String("provider", defaults.Provider, "")
	values := registerProviderFlags(fs, defaults)
	if err := parseFlags(fs, []string{
		"--provider", "docker",
		"--local-container-runtime", "docker",
		"--local-container-image", "ubuntu:24.04",
		"--local-container-user", "runner",
		"--local-container-work-root", "/workspace/crabbox",
		"--local-container-cpus", "4",
		"--local-container-memory", "8g",
		"--local-container-network", "bridge",
		"--local-container-docker-socket",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := defaults
	cfg.Provider = *provider
	if err := applyProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "local-container" || cfg.LocalContainer.Runtime != "docker" || cfg.LocalContainer.Image != "ubuntu:24.04" || cfg.LocalContainer.User != "runner" || cfg.SSHUser != "runner" || cfg.WorkRoot != "/workspace/crabbox" || cfg.LocalContainer.CPUs != 4 || cfg.LocalContainer.Memory != "8g" || cfg.LocalContainer.Network != "bridge" || !cfg.LocalContainer.DockerSocket {
		t.Fatalf("local-container flags not applied: provider=%s cfg=%#v", cfg.Provider, cfg.LocalContainer)
	}
}

func TestSSHCommandConfigAppliesProviderFlags(t *testing.T) {
	defaults := baseConfig()
	fs := newFlagSet("ssh", io.Discard)
	provider := fs.String("provider", defaults.Provider, "")
	id := fs.String("id", "", "")
	providerFlags := registerProviderFlags(fs, defaults)
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	if err := parseFlags(fs, []string{
		"--provider", "local-container",
		"--local-container-runtime", "podman",
		"--id", "example-podman",
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadSSHCommandConfig(fs, *provider, providerFlags, targetFlags, networkFlags, leaseTargetConfigOptions{LeaseID: *id})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "local-container" || cfg.LocalContainer.Runtime != "podman" {
		t.Fatalf("ssh config did not apply local-container runtime: provider=%s runtime=%s", cfg.Provider, cfg.LocalContainer.Runtime)
	}
}

func TestProviderFlagsApplyProxmoxWithoutSecrets(t *testing.T) {
	defaults := baseConfig()
	fs := newFlagSet("test", io.Discard)
	provider := fs.String("provider", defaults.Provider, "")
	values := registerProviderFlags(fs, defaults)
	if err := parseFlags(fs, []string{
		"--provider", "proxmox",
		"--proxmox-api-url", "https://pve.example.test:8006",
		"--proxmox-node", "pve1",
		"--proxmox-template-id", "9000",
		"--proxmox-user", "runner",
		"--proxmox-work-root", "/work/test",
		"--proxmox-insecure-tls",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := defaults
	cfg.Provider = *provider
	if err := applyProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Proxmox.APIURL != "https://pve.example.test:8006" || cfg.Proxmox.Node != "pve1" || cfg.Proxmox.TemplateID != 9000 || cfg.Proxmox.User != "runner" || cfg.SSHUser != "runner" || cfg.WorkRoot != "/work/test" || !cfg.Proxmox.InsecureTLS {
		t.Fatalf("proxmox flags not applied: %#v", cfg.Proxmox)
	}
	if cfg.ServerType != "template-9000" {
		t.Fatalf("server type=%q want template-9000", cfg.ServerType)
	}
}

func TestLeaseCreateFlagsApplySelectedProviderFlags(t *testing.T) {
	defaults := baseConfig()
	fs := newFlagSet("test", io.Discard)
	values := registerLeaseCreateFlags(fs, defaults)
	if err := parseFlags(fs, []string{
		"--provider", "blacksmith-testbox",
		"--blacksmith-org", "openclaw",
		"--blacksmith-workflow", ".github/workflows/testbox.yml",
		"--blacksmith-job", "test",
		"--blacksmith-ref", "feature",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := baseConfig()
	if err := applyLeaseCreateFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Blacksmith.Org != "openclaw" || cfg.Blacksmith.Workflow != ".github/workflows/testbox.yml" || cfg.Blacksmith.Job != "test" || cfg.Blacksmith.Ref != "feature" {
		t.Fatalf("blacksmith flags not applied through provider registry: %#v", cfg.Blacksmith)
	}
}

func TestLeaseCreateFlagsApplyCacheVolumes(t *testing.T) {
	defaults := baseConfig()
	fs := newFlagSet("test", io.Discard)
	values := registerLeaseCreateFlags(fs, defaults)
	if err := parseFlags(fs, []string{
		"--provider", "blacksmith-testbox",
		"--blacksmith-workflow", ".github/workflows/testbox.yml",
		"--cache-volume", "pnpm=repo-linux-node24-lock:/var/cache/crabbox/pnpm",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := baseConfig()
	if err := applyLeaseCreateFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Cache.Volumes) != 1 || cfg.Cache.Volumes[0].Name != "pnpm" || !cfg.Cache.Volumes[0].Required {
		t.Fatalf("cache volume flag not applied: %#v", cfg.Cache.Volumes)
	}
}

func TestLeaseCreateFlagsMergeCacheVolumes(t *testing.T) {
	defaults := baseConfig()
	fs := newFlagSet("test", io.Discard)
	values := registerLeaseCreateFlags(fs, defaults)
	if err := parseFlags(fs, []string{
		"--provider", "blacksmith-testbox",
		"--cache-volume", "npm=repo-linux-node24-npm:/var/cache/crabbox/npm",
		"--cache-volume", "pnpm=repo-linux-node24-pnpm:/var/cache/crabbox/pnpm",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := baseConfig()
	cfg.Cache.Volumes = []CacheVolumeConfig{
		{Name: "pnpm-store", Key: "repo-linux-node24-pnpm", Path: "/var/cache/crabbox/pnpm"},
	}
	if err := applyLeaseCreateFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Cache.Volumes) != 2 {
		t.Fatalf("cache volumes not merged: %#v", cfg.Cache.Volumes)
	}
	if cfg.Cache.Volumes[0].Name != "pnpm" || !cfg.Cache.Volumes[0].Required {
		t.Fatalf("duplicate cache volume was not upgraded to required: %#v", cfg.Cache.Volumes)
	}
	if cfg.Cache.Volumes[1].Name != "npm" || cfg.Cache.Volumes[1].Key != "repo-linux-node24-npm" || !cfg.Cache.Volumes[1].Required {
		t.Fatalf("new cache volume was not appended: %#v", cfg.Cache.Volumes)
	}
}

func TestRequiredCacheVolumeRejectsUnsupportedProvider(t *testing.T) {
	defaults := baseConfig()
	fs := newFlagSet("test", io.Discard)
	values := registerLeaseCreateFlags(fs, defaults)
	if err := parseFlags(fs, []string{
		"--provider", "aws",
		"--cache-volume", "pnpm=repo-linux-node24-lock:/var/cache/crabbox/pnpm",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := baseConfig()
	err := applyLeaseCreateFlags(&cfg, fs, values)
	if err == nil || !strings.Contains(err.Error(), "does not support required cache volume") {
		t.Fatalf("err=%v, want unsupported cache volume", err)
	}
}

func TestRequiredCacheVolumeRejectsExistingLease(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	defaults := baseConfig()
	fs := newFlagSet("test", io.Discard)
	values := registerLeaseCreateFlags(fs, defaults)
	if err := parseFlags(fs, []string{
		"--provider", "blacksmith-testbox",
		"--cache-volume", "pnpm=repo-linux-node24-lock:/var/cache/crabbox/pnpm",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := baseConfig()
	err := applyLeaseCreateFlagsForLease(&cfg, fs, values, "tbx_existing")
	if err == nil || !strings.Contains(err.Error(), "cannot be verified for existing lease") {
		t.Fatalf("err=%v, want existing lease cache volume rejection", err)
	}
}

func TestConfiguredCacheVolumeAllowsExistingLeaseReuse(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	defaults := baseConfig()
	fs := newFlagSet("test", io.Discard)
	values := registerLeaseCreateFlags(fs, defaults)
	if err := parseFlags(fs, []string{"--provider", "blacksmith-testbox"}); err != nil {
		t.Fatal(err)
	}
	cfg := baseConfig()
	cfg.Cache.Volumes = []CacheVolumeConfig{{
		Name:     "pnpm",
		Key:      "repo-linux-node24-lock",
		Path:     "/var/cache/crabbox/pnpm",
		Required: true,
	}}
	if err := claimLeaseForRepoProvider("tbx_existing", "existing", "blacksmith-testbox", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := updateLeaseClaimCacheVolumes("tbx_existing", CacheVolumeStickyDiskSpecs(cfg.Cache.Volumes)); err != nil {
		t.Fatal(err)
	}
	if err := applyLeaseCreateFlagsForLease(&cfg, fs, values, "tbx_existing"); err != nil {
		t.Fatalf("configured cache volume should allow reuse: %v", err)
	}
}

func TestConfiguredCacheVolumeRejectsExistingLeaseWithoutClaimedVolume(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	defaults := baseConfig()
	fs := newFlagSet("test", io.Discard)
	values := registerLeaseCreateFlags(fs, defaults)
	if err := parseFlags(fs, []string{"--provider", "blacksmith-testbox"}); err != nil {
		t.Fatal(err)
	}
	cfg := baseConfig()
	cfg.Cache.Volumes = []CacheVolumeConfig{{
		Name:     "pnpm",
		Key:      "repo-linux-node24-lock",
		Path:     "/var/cache/crabbox/pnpm",
		Required: true,
	}}
	if err := claimLeaseForRepoProvider("tbx_existing", "existing", "blacksmith-testbox", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	err := applyLeaseCreateFlagsForLease(&cfg, fs, values, "tbx_existing")
	if err == nil || !strings.Contains(err.Error(), "is not recorded on existing lease") {
		t.Fatalf("err=%v, want missing cache volume claim rejection", err)
	}
}

func TestLeaseCreateFlagsReapplyProxmoxDefaultsAfterProviderOverride(t *testing.T) {
	defaults := baseConfig()
	defaults.Provider = "hetzner"
	defaults.Proxmox.TemplateID = 9000
	defaults.Proxmox.User = "runner"
	defaults.Proxmox.WorkRoot = "/work/proxmox"
	defaults.ServerType = serverTypeForConfig(defaults)

	fs := newFlagSet("test", io.Discard)
	values := registerLeaseCreateFlags(fs, defaults)
	if err := parseFlags(fs, []string{"--provider", "proxmox"}); err != nil {
		t.Fatal(err)
	}
	cfg := defaults
	if err := applyLeaseCreateFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.SSHUser != "runner" {
		t.Fatalf("ssh user=%q want proxmox default", cfg.SSHUser)
	}
	if cfg.WorkRoot != "/work/proxmox" {
		t.Fatalf("work root=%q want proxmox default", cfg.WorkRoot)
	}
	if cfg.ServerType != "template-9000" {
		t.Fatalf("server type=%q want template-9000", cfg.ServerType)
	}
}

func TestLeaseCreateFlagsDeriveGCPTypeForAlias(t *testing.T) {
	defaults := baseConfig()
	fs := newFlagSet("test", io.Discard)
	values := registerLeaseCreateFlags(fs, defaults)
	if err := parseFlags(fs, []string{"--provider", "google", "--class", "standard"}); err != nil {
		t.Fatal(err)
	}
	cfg := baseConfig()
	if err := applyLeaseCreateFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "google" {
		t.Fatalf("provider should remain raw until backend load, got %q", cfg.Provider)
	}
	if cfg.ServerType != "c4-standard-32" {
		t.Fatalf("server type=%q want gcp default", cfg.ServerType)
	}
}

func TestLoadLeaseTargetConfigReappliesProxmoxDefaultsAfterProviderOverride(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "crabbox.yaml")
	t.Setenv("CRABBOX_CONFIG", configPath)
	if err := os.WriteFile(configPath, []byte(`provider: hetzner
proxmox:
  templateId: 9000
  user: runner
  workRoot: /work/proxmox
`), 0o600); err != nil {
		t.Fatal(err)
	}

	defaults := defaultConfig()
	fs := newFlagSet("test", io.Discard)
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	if err := parseFlags(fs, nil); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadLeaseTargetConfig(fs, "proxmox", targetFlags, networkFlags, leaseTargetConfigOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SSHUser != "runner" {
		t.Fatalf("ssh user=%q want proxmox default", cfg.SSHUser)
	}
	if cfg.WorkRoot != "/work/proxmox" {
		t.Fatalf("work root=%q want proxmox default", cfg.WorkRoot)
	}
	if cfg.ServerType != "template-9000" {
		t.Fatalf("server type=%q want template-9000", cfg.ServerType)
	}
}

func TestLeaseCreateFlagsRejectSnapshotSandboxResourceNoops(t *testing.T) {
	defaults := baseConfig()
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "class", args: []string{"--provider", "daytona", "--class", "standard"}},
		{name: "type", args: []string{"--provider", "daytona", "--type", "large"}},
		{name: "e2b class", args: []string{"--provider", "e2b", "--class", "standard"}},
		{name: "e2b type", args: []string{"--provider", "e2b", "--type", "large"}},
		{name: "modal class", args: []string{"--provider", "modal", "--class", "standard"}},
		{name: "modal type", args: []string{"--provider", "modal", "--type", "large"}},
		{name: "sprites class", args: []string{"--provider", "sprites", "--class", "standard"}},
		{name: "sprites type", args: []string{"--provider", "sprites", "--type", "large"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFlagSet("test", io.Discard)
			values := registerLeaseCreateFlags(fs, defaults)
			if err := parseFlags(fs, tc.args); err != nil {
				t.Fatal(err)
			}
			cfg := defaults
			if err := applyLeaseCreateFlags(&cfg, fs, values); err == nil {
				t.Fatalf("expected %v to be rejected", tc.args)
			}
		})
	}
}

func TestValidateRequestedCapabilitiesUsesProviderSpec(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "blacksmith-testbox"
	cfg.Desktop = true
	if err := validateRequestedCapabilities(cfg); err == nil {
		t.Fatal("expected blacksmith desktop capability rejection")
	}

	cfg = baseConfig()
	cfg.Provider = "hetzner"
	cfg.Desktop = true
	if err := validateRequestedCapabilities(cfg); err != nil {
		t.Fatalf("hetzner desktop capability rejected: %v", err)
	}
}

func TestRejectDelegatedSyncOptionsAllowsArchiveSyncControls(t *testing.T) {
	spec := ProviderSpec{Name: "modal", Kind: ProviderKindDelegatedRun, Features: FeatureSet{FeatureArchiveSync}}
	if err := RejectDelegatedSyncOptionsForSpec(spec, RunRequest{SyncOnly: true}); err != nil {
		t.Fatalf("archive sync provider should allow --sync-only: %v", err)
	}
	if err := RejectDelegatedSyncOptionsForSpec(spec, RunRequest{ForceSyncLarge: true}); err != nil {
		t.Fatalf("archive sync provider should allow --force-sync-large: %v", err)
	}
	if err := RejectDelegatedSyncOptionsForSpec(spec, RunRequest{ChecksumSync: true}); err == nil {
		t.Fatal("archive sync provider should still reject --checksum")
	}
	if err := RejectDelegatedSyncOptionsForSpec(spec, RunRequest{RequiredArtifactGlobs: []string{"reports/data/manifest.json"}}); err == nil {
		t.Fatal("archive sync provider should reject --require-artifact")
	}
	spec.Features = append(spec.Features, FeatureRunArtifacts)
	if err := RejectDelegatedSyncOptionsForSpec(spec, RunRequest{RequiredArtifactGlobs: []string{"reports/data/manifest.json"}}); err != nil {
		t.Fatalf("delegated artifact provider should allow --require-artifact: %v", err)
	}
	if err := RejectDelegatedSyncOptionsForSpec(spec, RunRequest{ArtifactGlobs: []string{"reports/data/**"}}); err != nil {
		t.Fatalf("delegated artifact provider should allow --artifact-glob: %v", err)
	}
	if err := RejectDelegatedSyncOptionsForSpec(ProviderSpec{Name: "islo"}, RunRequest{SyncOnly: true}); err == nil {
		t.Fatal("plain delegated provider should reject --sync-only")
	}
}

func TestRejectDelegatedSyncOptionsAllowsProofFeature(t *testing.T) {
	spec := ProviderSpec{Name: "blacksmith-testbox", Kind: ProviderKindDelegatedRun, Features: FeatureSet{FeatureRunProof}}
	if err := RejectDelegatedSyncOptionsForSpec(spec, RunRequest{EmitProof: "/tmp/proof.md"}); err != nil {
		t.Fatalf("delegated proof provider should allow --emit-proof: %v", err)
	}
	if err := RejectDelegatedSyncOptionsForSpec(ProviderSpec{Name: "islo"}, RunRequest{EmitProof: "/tmp/proof.md"}); err == nil {
		t.Fatal("plain delegated provider should reject --emit-proof")
	}
}

func TestProviderFlagsApplyDaytonaAndIsloWithoutCoreEdits(t *testing.T) {
	defaults := baseConfig()
	fs := newFlagSet("test", io.Discard)
	provider := fs.String("provider", defaults.Provider, "")
	values := registerProviderFlags(fs, defaults)
	if err := parseFlags(fs, []string{
		"--provider", "daytona",
		"--daytona-snapshot", "snap-crabbox",
		"--daytona-target", "us",
		"--daytona-work-root", "/home/daytona/work",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := defaults
	cfg.Provider = *provider
	if err := applyProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Daytona.Snapshot != "snap-crabbox" || cfg.Daytona.Target != "us" || cfg.Daytona.WorkRoot != "/home/daytona/work" {
		t.Fatalf("daytona flags not applied: %#v", cfg.Daytona)
	}

	fs = newFlagSet("test", io.Discard)
	provider = fs.String("provider", defaults.Provider, "")
	values = registerProviderFlags(fs, defaults)
	if err := parseFlags(fs, []string{
		"--provider", "islo",
		"--islo-image", "ubuntu:24.04",
		"--islo-vcpus", "4",
		"--islo-memory-mb", "8192",
	}); err != nil {
		t.Fatal(err)
	}
	cfg = defaults
	cfg.Provider = *provider
	if err := applyProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Islo.Image != "ubuntu:24.04" || cfg.Islo.VCPUs != 4 || cfg.Islo.MemoryMB != 8192 {
		t.Fatalf("islo flags not applied: %#v", cfg.Islo)
	}

	fs = newFlagSet("test", io.Discard)
	provider = fs.String("provider", defaults.Provider, "")
	values = registerProviderFlags(fs, defaults)
	if err := parseFlags(fs, []string{
		"--provider", "e2b",
		"--e2b-template", "crabbox-ready",
		"--e2b-workdir", "work/repo",
	}); err != nil {
		t.Fatal(err)
	}
	cfg = defaults
	cfg.Provider = *provider
	if err := applyProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.E2B.Template != "crabbox-ready" || cfg.E2B.Workdir != "work/repo" {
		t.Fatalf("e2b flags not applied: %#v", cfg.E2B)
	}

	fs = newFlagSet("test", io.Discard)
	provider = fs.String("provider", defaults.Provider, "")
	values = registerProviderFlags(fs, defaults)
	if err := parseFlags(fs, []string{
		"--provider", "modal",
		"--modal-app", "crabbox-test",
		"--modal-image", "python:3.13-slim",
		"--modal-workdir", "/workspace/test",
	}); err != nil {
		t.Fatal(err)
	}
	cfg = defaults
	cfg.Provider = *provider
	if err := applyProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Modal.App != "crabbox-test" || cfg.Modal.Image != "python:3.13-slim" || cfg.Modal.Workdir != "/workspace/test" {
		t.Fatalf("modal flags not applied: %#v", cfg.Modal)
	}

	fs = newFlagSet("test", io.Discard)
	provider = fs.String("provider", defaults.Provider, "")
	values = registerProviderFlags(fs, defaults)
	if err := parseFlags(fs, []string{
		"--provider", "sprites",
		"--sprites-api-url", "https://sprites.example.test",
		"--sprites-work-root", "/home/sprite/work",
	}); err != nil {
		t.Fatal(err)
	}
	cfg = defaults
	cfg.Provider = *provider
	if err := applyProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Sprites.APIURL != "https://sprites.example.test" || cfg.Sprites.WorkRoot != "/home/sprite/work" {
		t.Fatalf("sprites flags not applied: %#v", cfg.Sprites)
	}
}

func TestProviderFlagsApplyIncusWithoutCoreEdits(t *testing.T) {
	defaults := baseConfig()
	fs := newFlagSet("test", io.Discard)
	provider := fs.String("provider", defaults.Provider, "")
	values := registerProviderFlags(fs, defaults)
	if err := parseFlags(fs, []string{
		"--provider", "incus",
		"--incus-instance-type", "vm",
		"--incus-image", "images:ubuntu/24.04/cloud",
		"--incus-user", "ubuntu",
		"--incus-work-root", "/workspace/incus",
		"--incus-proxy-listen-port", "2201",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := defaults
	cfg.Provider = *provider
	if err := applyProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Incus.InstanceType != "virtual-machine" || cfg.Incus.User != "ubuntu" || cfg.Incus.WorkRoot != "/workspace/incus" {
		t.Fatalf("incus flags not applied: %#v", cfg.Incus)
	}
	if cfg.SSHPort != "2201" || cfg.WorkRoot != "/workspace/incus" || cfg.ServerType != "virtual-machine:images:ubuntu/24.04/cloud" {
		t.Fatalf("derived incus config wrong: sshPort=%q workRoot=%q serverType=%q", cfg.SSHPort, cfg.WorkRoot, cfg.ServerType)
	}
}

func TestRedactedSSHUserOnlyForDaytona(t *testing.T) {
	target := SSHTarget{User: "tok_live_secret"}
	if got := redactedSSHUser(Config{Provider: "hetzner"}, Server{Provider: "hetzner"}, target); got != target.User {
		t.Fatalf("redactedSSHUser hetzner=%q", got)
	}
	if got := redactedSSHUser(Config{Provider: "hetzner"}, Server{Provider: "hetzner"}, SSHTarget{User: "secret", AuthSecret: true}); got != "<token>" {
		t.Fatalf("redactedSSHUser auth secret=%q", got)
	}
	if got := redactedSSHUser(Config{Provider: "daytona"}, Server{}, target); got != "<token>" {
		t.Fatalf("redactedSSHUser daytona=%q", got)
	}
}
