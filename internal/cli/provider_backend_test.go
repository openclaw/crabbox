package cli

import (
	"context"
	"io"
	"testing"
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
	for _, name := range []string{"hetzner", "aws", "azure", "gcp", "google", "google-cloud", "ssh", "static", "static-ssh", "blacksmith", "blacksmith-testbox", "namespace", "namespace-devbox", "daytona", "islo", "e2b", "sprites"} {
		if _, err := ProviderFor(name); err != nil {
			t.Fatalf("ProviderFor(%q): %v", name, err)
		}
	}
	if _, err := ProviderFor("missing"); err == nil {
		t.Fatal("expected missing provider to fail")
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

	cfg.Provider = "e2b"
	backend, err = loadBackend(cfg, testRuntimeWithRunner(&recordingCommandRunner{}))
	if err != nil {
		t.Fatalf("load e2b backend: %v", err)
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
