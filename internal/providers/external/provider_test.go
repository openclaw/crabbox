package external

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func testConfig() core.Config {
	cfg := core.BaseConfig()
	cfg.External = core.ExternalConfig{
		Command:  "provider-command",
		Args:     []string{"--profile", "test"},
		Config:   map[string]any{"namespace": "dev", "cpu": 32},
		WorkRoot: "/home/tester/crabbox",
	}
	return cfg
}

func isolateCrabboxState(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	return home
}

func claimExternalLease(t *testing.T, cfg core.Config, leaseID, slug, repoRoot string, idleTimeout time.Duration, reclaim bool) {
	t.Helper()
	if err := core.ClaimLeaseForRepoProviderScope(leaseID, slug, providerName, externalClaimScope(cfg), repoRoot, idleTimeout, reclaim); err != nil {
		t.Fatal(err)
	}
}

func TestProviderSpec(t *testing.T) {
	spec := (Provider{}).Spec()
	if spec.Name != providerName || spec.Family != "external" {
		t.Fatalf("spec=%#v", spec)
	}
	for _, feature := range []core.Feature{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureDesktop, core.FeatureBrowser, core.FeatureCode} {
		if !spec.Features.Has(feature) {
			t.Fatalf("missing feature %s", feature)
		}
	}
}

func TestRouteConfigUsesProviderWorkRoot(t *testing.T) {
	cfg := testConfig()
	cfg.WorkRoot = core.BaseConfig().WorkRoot
	if err := (Provider{}).RouteConfig(&cfg, nil, nil); err != nil {
		t.Fatal(err)
	}
	if cfg.WorkRoot != "/home/tester/crabbox" {
		t.Fatalf("work root=%q", cfg.WorkRoot)
	}
}

func TestCommandRoutingArgsUsesPrivateLeaseState(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	args := (Provider{}).CommandRoutingArgs(testConfig(), "cbx_abcdef123456")
	if len(args) != 2 || args[0] != "--external-routing-file" || !strings.HasSuffix(args[1], ".json") {
		t.Fatalf("args=%#v", args)
	}
}

func TestProtocolClaimScopeIgnoresZeroLifecycleConnection(t *testing.T) {
	cfg := testConfig()
	before := externalClaimScope(cfg)
	cfg.External.Connection.SSH.User = "developer"
	if after := externalClaimScope(cfg); after != before {
		t.Fatalf("protocol scope changed after zero lifecycle connection: before=%s after=%s", before, after)
	}
	cfg.External.Command = ""
	cfg.External.Lifecycle.Acquire.Argv = []string{"devboxctl", "new", "{{name}}"}
	cfg.External.Lifecycle.List.Argv = []string{"devboxctl", "list"}
	cfg.External.Lifecycle.List.Output = lifecycleOutputJSONNameArray
	cfg.External.Lifecycle.Release.Argv = []string{"devboxctl", "rm", "{{name}}"}
	lifecycleScope := externalClaimScope(cfg)
	cfg.External.Connection.SSH.Host = "{{name}}.example"
	if after := externalClaimScope(cfg); after == lifecycleScope {
		t.Fatalf("lifecycle scope did not include connection: %s", after)
	}
}

func TestConfigurePreservesExplicitTopLevelWorkRoot(t *testing.T) {
	cfg := testConfig()
	cfg.WorkRoot = "/workspace/top-level"
	cfg.External.WorkRoot = core.BaseConfig().External.WorkRoot
	backend, err := (Provider{}).Configure(cfg, core.Runtime{Exec: &recordingRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	if got := backend.(*leaseBackend).cfg.WorkRoot; got != "/workspace/top-level" {
		t.Fatalf("work root=%q", got)
	}
}

func TestConfigureProviderWorkRootOverridesTopLevelWorkRoot(t *testing.T) {
	cfg := testConfig()
	cfg.WorkRoot = "/workspace/top-level"
	cfg.External.WorkRoot = "/workspace/provider"
	backend, err := (Provider{}).Configure(cfg, core.Runtime{Exec: &recordingRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	if got := backend.(*leaseBackend).cfg.WorkRoot; got != "/workspace/provider" {
		t.Fatalf("work root=%q", got)
	}
}

func TestConfigureRejectsUnsafeTopLevelWorkRoot(t *testing.T) {
	cfg := testConfig()
	cfg.WorkRoot = "/tmp"
	cfg.External.WorkRoot = core.BaseConfig().External.WorkRoot
	if _, err := (Provider{}).Configure(cfg, core.Runtime{Exec: &recordingRunner{}}); err == nil || !strings.Contains(err.Error(), "too broad") {
		t.Fatalf("err=%v", err)
	}
}

func TestFlagsOverrideArgsAndConfigJSON(t *testing.T) {
	cfg := testConfig()
	fs := flag.NewFlagSet("external", flag.ContinueOnError)
	values := registerFlags(fs, cfg)
	if err := fs.Parse([]string{
		"--external-arg=/tmp/new provider.mjs",
		"--external-arg=--profile",
		"--external-config-json", `{"namespace":"prod","cpu":64}`,
	}); err != nil {
		t.Fatal(err)
	}
	if err := applyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.External.Args, "|") != "/tmp/new provider.mjs|--profile" {
		t.Fatalf("args=%#v", cfg.External.Args)
	}
	if cfg.External.Config["namespace"] != "prod" || cfg.External.Config["cpu"] != float64(64) {
		t.Fatalf("config=%#v", cfg.External.Config)
	}
}

func TestFlagHelpDoesNotExposeLoadedArgsOrConfig(t *testing.T) {
	cfg := testConfig()
	cfg.External.Args = []string{"--token", "secret-arg"}
	cfg.External.Config = map[string]any{"token": "secret-config"}
	fs := flag.NewFlagSet("external", flag.ContinueOnError)
	var output bytes.Buffer
	fs.SetOutput(&output)
	registerFlags(fs, cfg)
	fs.PrintDefaults()
	for _, secret := range []string{"secret-arg", "secret-config"} {
		if strings.Contains(output.String(), secret) {
			t.Fatalf("help leaked %q:\n%s", secret, output.String())
		}
	}
}

func TestInvokeSendsVersionedJSONRequest(t *testing.T) {
	runner := &recordingRunner{stdout: `{"protocolVersion":1,"message":"ready"}`}
	backend := &leaseBackend{cfg: testConfig(), rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	response, err := backend.invoke(context.Background(), protocolRequest{Operation: "doctor"})
	if err != nil {
		t.Fatal(err)
	}
	if response.Message != "ready" {
		t.Fatalf("response=%#v", response)
	}
	if runner.name != "provider-command" || strings.Join(runner.args, " ") != "--profile test" {
		t.Fatalf("command=%q args=%#v", runner.name, runner.args)
	}
	var request protocolRequest
	if err := json.Unmarshal(runner.stdin, &request); err != nil {
		t.Fatal(err)
	}
	if request.ProtocolVersion != 1 || request.Operation != "doctor" || request.Config["namespace"] != "dev" {
		t.Fatalf("request=%#v", request)
	}
}

func TestInvokeRejectsUnversionedResponse(t *testing.T) {
	runner := &recordingRunner{stdout: `{}`}
	backend := &leaseBackend{cfg: testConfig(), rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	if _, err := backend.invoke(context.Background(), protocolRequest{Operation: "doctor"}); err == nil || !strings.Contains(err.Error(), "protocol version 0") {
		t.Fatalf("err=%v", err)
	}
}

func TestInvokeReportsErrorOnlyResponse(t *testing.T) {
	runner := &recordingRunner{stdout: `{"error":"quota exhausted"}`}
	backend := &leaseBackend{cfg: testConfig(), rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	if _, err := backend.invoke(context.Background(), protocolRequest{Operation: "doctor"}); err == nil || !strings.Contains(err.Error(), "quota exhausted") || strings.Contains(err.Error(), "protocol version") {
		t.Fatalf("err=%v", err)
	}
}

func TestInvokeDeclarativeLifecycleExpandsArgvAndBuildsLease(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.External = core.ExternalConfig{
		Config: map[string]any{"size": "cpu16"},
		Lifecycle: core.ExternalLifecycleConfig{
			Acquire: core.ExternalLifecycleOperation{
				Argv: []string{"devboxctl", "new", "{{resourceName}}", "--size", "{{config.size}}"},
			},
			List: core.ExternalLifecycleOperation{
				Argv:   []string{"devboxctl", "list", "--format", "json"},
				Output: lifecycleOutputJSONNameArray,
			},
			Release: core.ExternalLifecycleOperation{
				Argv: []string{"devboxctl", "rm", "--yes", "{{name}}"},
			},
		},
		Connection: core.ExternalConnectionConfig{
			ResourceName: "{{leaseIdSlug}}",
			CloudID:      "devboxes/{{name}}",
			ServerType:   "{{config.size}}",
			Labels:       map[string]string{"backend": "pod"},
			SSH: core.ExternalSSHConnectionConfig{
				User:           "developer",
				Host:           "{{resourceName}}",
				SSHConfigProxy: true,
			},
		},
		WorkRoot: "/home/developer/crabbox",
	}
	runner := &recordingRunner{}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	response, err := backend.invoke(context.Background(), protocolRequest{
		Operation: "acquire",
		Desired: &desiredLease{
			LeaseID: "cbx_abcdef123456",
			Slug:    "fast-coral",
			Name:    "crabbox-fast-coral-deadbeef",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if runner.name != "devboxctl" || strings.Join(runner.args, "|") != "new|cbx-abcdef123456|--size|cpu16" {
		t.Fatalf("command=%q args=%#v", runner.name, runner.args)
	}
	if response.Lease == nil || response.Lease.CloudID != "devboxes/crabbox-fast-coral-deadbeef" || response.Lease.ServerType != "cpu16" {
		t.Fatalf("response=%#v", response)
	}
	if response.Lease.SSH == nil || response.Lease.SSH.User != "developer" || response.Lease.SSH.Host != "cbx-abcdef123456" || !response.Lease.SSH.SSHConfigProxy {
		t.Fatalf("ssh=%#v", response.Lease.SSH)
	}
	if response.Lease.Labels["backend"] != "pod" || response.Lease.Labels[externalResourceNameLabel] != "cbx-abcdef123456" {
		t.Fatalf("labels=%#v", response.Lease.Labels)
	}
}

func TestInvokeDeclarativeLifecycleValidatesConnectionBeforeAcquire(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.External = core.ExternalConfig{
		Lifecycle: core.ExternalLifecycleConfig{
			Acquire: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "new", "{{name}}"}},
			List: core.ExternalLifecycleOperation{
				Argv:   []string{"devboxctl", "list", "--format", "json"},
				Output: lifecycleOutputJSONNameArray,
			},
			Release: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "rm", "{{name}}"}},
		},
		Connection: core.ExternalConnectionConfig{
			SSH: core.ExternalSSHConnectionConfig{
				User: "{{env.MISSING_DEVBOX_USER}}",
				Host: "{{name}}",
			},
		},
		WorkRoot: "/home/developer/crabbox",
	}
	runner := &recordingRunner{}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	_, err := backend.invoke(context.Background(), protocolRequest{
		Operation: "acquire",
		Desired: &desiredLease{
			LeaseID: "cbx_abcdef123456",
			Slug:    "fast-coral",
			Name:    "crabbox-fast-coral-deadbeef",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "external connection ssh.user") || !strings.Contains(err.Error(), "MISSING_DEVBOX_USER") {
		t.Fatalf("err=%v", err)
	}
	if runner.name != "" {
		t.Fatalf("acquire command ran before connection validation: %s %#v", runner.name, runner.args)
	}
}

func TestInvokeDeclarativeLifecycleConnectionTemplatesUseRequestContext(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.External = core.ExternalConfig{
		Lifecycle: core.ExternalLifecycleConfig{
			Acquire: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "new", "{{name}}", "--repo", "{{repo.name}}"}},
			List: core.ExternalLifecycleOperation{
				Argv:   []string{"devboxctl", "list", "--format", "json"},
				Output: lifecycleOutputJSONNameArray,
			},
			Release: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "rm", "{{name}}"}},
		},
		Connection: core.ExternalConnectionConfig{
			SSH: core.ExternalSSHConnectionConfig{
				User: "developer",
				Host: "{{repo.name}}-{{name}}",
			},
		},
	}
	runner := &recordingRunner{}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	response, err := backend.invoke(context.Background(), protocolRequest{
		Operation: "acquire",
		Desired: &desiredLease{
			LeaseID: "cbx_abcdef123456",
			Slug:    "fast-coral",
			Name:    "crabbox-fast-coral-deadbeef",
		},
		Repo: &protocolRepo{Name: "my-app"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Lease == nil || response.Lease.SSH == nil || response.Lease.SSH.Host != "my-app-crabbox-fast-coral-deadbeef" {
		t.Fatalf("response=%#v", response)
	}
	if strings.Join(runner.args, "|") != "new|crabbox-fast-coral-deadbeef|--repo|my-app" {
		t.Fatalf("args=%#v", runner.args)
	}
}

func TestInvokeDeclarativeLifecycleParsesNameInventory(t *testing.T) {
	isolateCrabboxState(t)
	cfg := core.BaseConfig()
	cfg.External = core.ExternalConfig{
		Lifecycle: core.ExternalLifecycleConfig{
			Acquire: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "new", "{{name}}"}},
			List: core.ExternalLifecycleOperation{
				Argv:   []string{"devboxctl", "list", "--format", "json"},
				Output: lifecycleOutputJSONNameArray,
			},
			Release: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "rm", "{{name}}"}},
		},
		Connection: core.ExternalConnectionConfig{
			SSH: core.ExternalSSHConnectionConfig{User: "developer", Host: "{{name}}", SSHConfigProxy: true},
		},
		WorkRoot: "/home/developer/crabbox",
	}
	claimExternalLease(t, cfg, "cbx_abcdef123456", "fast-coral", t.TempDir(), time.Minute, false)
	if err := core.UpdateLeaseClaimEndpoint(
		"cbx_abcdef123456",
		core.Server{Name: "crabbox-fast-coral-deadbeef", Labels: map[string]string{
			"name":                    "crabbox-fast-coral-deadbeef",
			"slug":                    "fast-coral",
			externalResourceNameLabel: "devbox-fast-coral",
		}},
		core.SSHTarget{},
	); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{stdout: `["devbox-fast-coral","unclaimed-box"]`}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	response, err := backend.invoke(context.Background(), protocolRequest{Operation: "list"})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Leases) != 2 {
		t.Fatalf("leases=%#v", response.Leases)
	}
	if response.Leases[0].LeaseID != "cbx_abcdef123456" || response.Leases[0].Slug != "fast-coral" {
		t.Fatalf("claimed lease=%#v", response.Leases[0])
	}
	if response.Leases[1].LeaseID != "unclaimed-box" || response.Leases[1].Name != "unclaimed-box" {
		t.Fatalf("unclaimed lease=%#v", response.Leases[1])
	}
}

func TestDeclarativeLifecycleCleanupDryRunDoesNotRunCommand(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.External = core.ExternalConfig{
		Lifecycle: core.ExternalLifecycleConfig{
			Acquire: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "new", "{{name}}"}},
			List: core.ExternalLifecycleOperation{
				Argv:   []string{"devboxctl", "list", "--format", "json"},
				Output: lifecycleOutputJSONNameArray,
			},
			Release: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "rm", "{{name}}"}},
			Cleanup: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "gc"}},
		},
		Connection: core.ExternalConnectionConfig{
			SSH: core.ExternalSSHConnectionConfig{User: "developer", Host: "{{name}}"},
		},
	}
	runner := &recordingRunner{}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if runner.name != "" {
		t.Fatalf("cleanup command ran during dry-run: %s %#v", runner.name, runner.args)
	}
}

func TestDeclarativeLifecycleDefaultTouchUpdatesLocalLabels(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.IdleTimeout = time.Minute
	cfg.TTL = time.Hour
	cfg.External = core.ExternalConfig{
		Lifecycle: core.ExternalLifecycleConfig{
			Acquire: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "new", "{{name}}"}},
			List: core.ExternalLifecycleOperation{
				Argv:   []string{"devboxctl", "list", "--format", "json"},
				Output: lifecycleOutputJSONNameArray,
			},
			Release: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "rm", "{{name}}"}},
		},
		Connection: core.ExternalConnectionConfig{
			SSH: core.ExternalSSHConnectionConfig{User: "developer", Host: "{{name}}"},
		},
	}
	created := time.Now().UTC().Add(-10 * time.Minute)
	labels := core.DirectLeaseLabels(cfg, "cbx_abcdef123456", "fast-coral", providerName, "", false, created)
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: &recordingRunner{}}}
	server, err := backend.Touch(context.Background(), core.TouchRequest{
		Lease: core.LeaseTarget{
			LeaseID: "cbx_abcdef123456",
			Server:  core.Server{Name: "devbox-fast-coral", Labels: labels},
		},
		State:       "ready",
		IdleTimeout: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if server.Labels["last_touched_at"] == labels["last_touched_at"] {
		t.Fatalf("last_touched_at was not refreshed: %#v", server.Labels)
	}
	if server.Labels["state"] != "ready" || server.Labels["expires_at"] == labels["expires_at"] {
		t.Fatalf("touch labels not refreshed: %#v", server.Labels)
	}
}

func TestInvokeDeclarativeLifecycleFiltersNameInventory(t *testing.T) {
	isolateCrabboxState(t)
	cfg := core.BaseConfig()
	cfg.External = core.ExternalConfig{
		Lifecycle: core.ExternalLifecycleConfig{
			Acquire: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "new", "{{name}}"}},
			List: core.ExternalLifecycleOperation{
				Argv:       []string{"devboxctl", "list", "--format", "json"},
				Output:     lifecycleOutputJSONNameArray,
				NamePrefix: "cbx-",
			},
			Release: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "rm", "{{name}}"}},
		},
		Connection: core.ExternalConnectionConfig{
			SSH: core.ExternalSSHConnectionConfig{User: "developer", Host: "{{name}}"},
		},
		WorkRoot: "/home/developer/crabbox",
	}
	runner := &recordingRunner{stdout: `["cbx-owned","manual-box"]`}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	response, err := backend.invoke(context.Background(), protocolRequest{Operation: "list"})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Leases) != 1 || response.Leases[0].Name != "cbx-owned" {
		t.Fatalf("leases=%#v", response.Leases)
	}
}

func TestDeclarativeLifecycleExpandsExplicitEnvironmentPlaceholder(t *testing.T) {
	t.Setenv("DEVBOX_USER", "alice")
	templateCtx, err := lifecycleContext(protocolRequest{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := expandLifecycleValue("{{env.DEVBOX_USER}}@{{name}}", templateCtx)
	if err != nil {
		t.Fatal(err)
	}
	if got != "alice@" {
		t.Fatalf("expanded=%q", got)
	}
	if _, err := expandLifecycleValue("{{env.MISSING_DEVBOX_USER}}", templateCtx); err == nil || !strings.Contains(err.Error(), "is not set") {
		t.Fatalf("err=%v", err)
	}
}

func TestDeclarativeLifecycleIDFallsBackToLeaseID(t *testing.T) {
	templateCtx, err := lifecycleContext(protocolRequest{
		Lease: &protocolLease{
			LeaseID: "cbx_abcdef123456",
			Slug:    "fast-coral",
			Name:    "devbox-fast-coral",
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := expandLifecycleValue("{{id}}|{{leaseIdSlug}}", templateCtx)
	if err != nil {
		t.Fatal(err)
	}
	if got != "cbx_abcdef123456|cbx-abcdef123456" {
		t.Fatalf("expanded=%q", got)
	}
}

func TestDeclarativeLifecycleReusesPersistedResourceName(t *testing.T) {
	t.Setenv("DEVBOX_RESOURCE", "new-resource")
	cfg := core.BaseConfig()
	cfg.External = core.ExternalConfig{
		Lifecycle: core.ExternalLifecycleConfig{
			Acquire: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "new", "{{resourceName}}"}},
			List: core.ExternalLifecycleOperation{
				Argv:   []string{"devboxctl", "list"},
				Output: lifecycleOutputJSONNameArray,
			},
			Release: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "rm", "{{resourceName}}"}},
		},
		Connection: core.ExternalConnectionConfig{
			ResourceName: "{{env.DEVBOX_RESOURCE}}",
			SSH:          core.ExternalSSHConnectionConfig{User: "developer"},
		},
	}
	runner := &recordingRunner{}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	if _, err := backend.invoke(context.Background(), protocolRequest{
		Operation: "release",
		Lease: &protocolLease{
			LeaseID: "cbx_abcdef123456",
			Slug:    "fast-coral",
			Name:    "crabbox-fast-coral-deadbeef",
			Labels:  map[string]string{externalResourceNameLabel: "original-resource"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if runner.name != "devboxctl" || strings.Join(runner.args, "|") != "rm|original-resource" {
		t.Fatalf("command=%q args=%#v", runner.name, runner.args)
	}
}

func TestDeclarativeLifecycleUsesLegacyLeaseNameWhenResourceLabelMissing(t *testing.T) {
	t.Setenv("DEVBOX_RESOURCE", "new-resource")
	cfg := core.BaseConfig()
	cfg.External = core.ExternalConfig{
		Lifecycle: core.ExternalLifecycleConfig{
			Acquire: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "new", "{{resourceName}}"}},
			List: core.ExternalLifecycleOperation{
				Argv:   []string{"devboxctl", "list"},
				Output: lifecycleOutputJSONNameArray,
			},
			Release: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "rm", "{{resourceName}}"}},
		},
		Connection: core.ExternalConnectionConfig{
			ResourceName: "{{env.DEVBOX_RESOURCE}}",
			SSH:          core.ExternalSSHConnectionConfig{User: "developer"},
		},
	}
	runner := &recordingRunner{}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	if _, err := backend.invoke(context.Background(), protocolRequest{
		Operation: "release",
		Lease: &protocolLease{
			LeaseID: "cbx_abcdef123456",
			Slug:    "fast-coral",
			Name:    "legacy-resource",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if runner.name != "devboxctl" || strings.Join(runner.args, "|") != "rm|legacy-resource" {
		t.Fatalf("command=%q args=%#v", runner.name, runner.args)
	}
}

func TestDeclarativeInventoryUsesListedNameForLegacyClaim(t *testing.T) {
	isolateCrabboxState(t)
	t.Setenv("DEVBOX_RESOURCE", "new-resource")
	cfg := core.BaseConfig()
	cfg.External = core.ExternalConfig{
		Lifecycle: core.ExternalLifecycleConfig{
			Acquire: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "new", "{{resourceName}}"}},
			List: core.ExternalLifecycleOperation{
				Argv:   []string{"devboxctl", "list"},
				Output: lifecycleOutputJSONNameArray,
			},
			Release: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "rm", "{{resourceName}}"}},
		},
		Connection: core.ExternalConnectionConfig{
			ResourceName: "{{env.DEVBOX_RESOURCE}}",
			SSH:          core.ExternalSSHConnectionConfig{User: "developer"},
		},
		WorkRoot: "/home/developer/crabbox",
	}
	claimExternalLease(t, cfg, "cbx_abcdef123456", "fast-coral", t.TempDir(), time.Minute, false)
	if err := core.UpdateLeaseClaimEndpoint(
		"cbx_abcdef123456",
		core.Server{Name: "legacy-resource", Labels: map[string]string{
			"name": "legacy-resource",
			"slug": "fast-coral",
		}},
		core.SSHTarget{},
	); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{stdout: `["legacy-resource"]`}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	response, err := backend.invoke(context.Background(), protocolRequest{Operation: "list"})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Leases) != 1 ||
		response.Leases[0].LeaseID != "cbx_abcdef123456" ||
		response.Leases[0].SSH == nil ||
		response.Leases[0].SSH.Host != "legacy-resource" ||
		response.Leases[0].Labels[externalResourceNameLabel] != "legacy-resource" {
		t.Fatalf("leases=%#v", response.Leases)
	}
}

func TestDeclarativeResolveThenReleaseReusesPersistedResourceName(t *testing.T) {
	isolateCrabboxState(t)
	t.Setenv("DEVBOX_RESOURCE", "new-resource")
	cfg := core.BaseConfig()
	cfg.External = core.ExternalConfig{
		Lifecycle: core.ExternalLifecycleConfig{
			Acquire: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "new", "{{resourceName}}"}},
			List: core.ExternalLifecycleOperation{
				Argv:   []string{"devboxctl", "list"},
				Output: lifecycleOutputJSONNameArray,
			},
			Release: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "rm", "{{resourceName}}"}},
		},
		Connection: core.ExternalConnectionConfig{
			ResourceName: "{{env.DEVBOX_RESOURCE}}",
			SSH:          core.ExternalSSHConnectionConfig{User: "developer"},
		},
		WorkRoot: "/home/developer/crabbox",
	}
	claimExternalLease(t, cfg, "cbx_abcdef123456", "fast-coral", t.TempDir(), time.Minute, false)
	if err := core.UpdateLeaseClaimEndpoint(
		"cbx_abcdef123456",
		core.Server{Name: "crabbox-fast-coral-deadbeef", Labels: map[string]string{
			"name":                    "crabbox-fast-coral-deadbeef",
			"slug":                    "fast-coral",
			externalResourceNameLabel: "original-resource",
		}},
		core.SSHTarget{},
	); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "fast-coral", ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.Labels[externalResourceNameLabel] != "original-resource" {
		t.Fatalf("lease=%#v", lease)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if runner.name != "devboxctl" || strings.Join(runner.args, "|") != "rm|original-resource" {
		t.Fatalf("command=%q args=%#v", runner.name, runner.args)
	}
}

func TestValidateConfigRejectsMixedOrIncompleteDeclarativeModes(t *testing.T) {
	cfg := testConfig()
	cfg.External.Lifecycle.Acquire.Argv = []string{"devboxctl", "new", "{{name}}"}
	if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("mixed mode err=%v", err)
	}
	cfg.External.Command = ""
	if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "release.argv") {
		t.Fatalf("missing release err=%v", err)
	}
	cfg.External.Lifecycle.Release.Argv = []string{"devboxctl", "rm", "{{name}}"}
	if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "list.argv") {
		t.Fatalf("missing list err=%v", err)
	}
	cfg.External.Lifecycle.List = core.ExternalLifecycleOperation{
		Argv:   []string{"devboxctl", "list"},
		Output: lifecycleOutputJSONNameArray,
	}
	cfg.External.Lifecycle.Acquire.Output = lifecycleOutputJSONNameArray
	if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "only supported for list") {
		t.Fatalf("non-list output err=%v", err)
	}
	cfg.External.Lifecycle.Acquire.Output = ""
	cfg.External.Lifecycle.List.NamePrefix = "cbx-"
	cfg.External.Lifecycle.List.Output = lifecycleOutputJSONLeaseArray
	if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "namePrefix requires") {
		t.Fatalf("list name prefix err=%v", err)
	}
}

func TestProtocolLeaseMapsProxyAndServer(t *testing.T) {
	cfg := testConfig()
	lease := protocolLease{
		LeaseID:    "cbx_000000000123",
		Slug:       "test",
		Name:       "devbox-test",
		Status:     "running",
		ServerType: "cpu32",
		SSH: &protocolSSH{
			User:           "tester",
			Host:           "devbox-test",
			Port:           "22",
			SSHConfigProxy: true,
			ProxyCommand:   "provider proxy %h %p",
		},
	}.target(cfg, true)
	if lease.Server.Provider != providerName || lease.Server.ServerType.Name != "cpu32" {
		t.Fatalf("server=%#v", lease.Server)
	}
	if lease.Server.Labels["name"] != "devbox-test" {
		t.Fatalf("labels=%#v", lease.Server.Labels)
	}
	if !lease.SSH.SSHConfigProxy || lease.SSH.ProxyCommand != "provider proxy %h %p" {
		t.Fatalf("ssh=%#v", lease.SSH)
	}
}

func TestProtocolLeaseProxyCommandImpliesProxyMode(t *testing.T) {
	lease := protocolLease{
		LeaseID: "cbx_abcdef123456",
		Slug:    "test",
		Name:    "devbox-test",
		SSH: &protocolSSH{
			User:         "tester",
			Host:         "devbox-test",
			ProxyCommand: "provider proxy devbox-test %p",
		},
	}.target(testConfig(), true)
	if !lease.SSH.SSHConfigProxy {
		t.Fatalf("ssh=%#v", lease.SSH)
	}
}

func TestProtocolLeaseDefaultsReadyCheck(t *testing.T) {
	lease := protocolLease{
		LeaseID: "cbx_abcdef123456",
		Slug:    "test",
		Name:    "devbox-test",
		SSH: &protocolSSH{
			User: "tester",
			Host: "devbox-test",
		},
	}.target(testConfig(), true)
	for _, want := range []string{"bash", "python3", "git", "rsync", "tar"} {
		if !strings.Contains(lease.SSH.ReadyCheck, want) {
			t.Fatalf("ready check %q missing %q", lease.SSH.ReadyCheck, want)
		}
	}
}

func TestAllocateLeaseSlugIgnoresOtherExternalScopes(t *testing.T) {
	isolateCrabboxState(t)
	cfg := testConfig()
	otherCfg := testConfig()
	otherCfg.External.Config = map[string]any{"namespace": "prod", "cpu": 32}
	claimExternalLease(t, otherCfg, "cbx_other", "shared", t.TempDir(), time.Minute, false)
	backend := &leaseBackend{cfg: cfg}
	slug, err := backend.allocateLeaseSlug("cbx_new", "shared")
	if err != nil {
		t.Fatal(err)
	}
	if slug != "shared" {
		t.Fatalf("slug=%q, want shared when collision is outside scope", slug)
	}
	claimExternalLease(t, cfg, "cbx_current", "shared", t.TempDir(), time.Minute, false)
	slug, err = backend.allocateLeaseSlug("cbx_next", "shared")
	if err != nil {
		t.Fatal(err)
	}
	if slug == "shared" || !strings.HasPrefix(slug, "shared-") {
		t.Fatalf("slug=%q, want current-scope collision suffix", slug)
	}
}

func TestLeaseSlugForClaimUsesProviderReturnedSlug(t *testing.T) {
	lease := protocolLease{
		LeaseID: "provider-id",
		Slug:    "provider-slug",
		Name:    "provider-name",
	}.target(testConfig(), false)
	if got := leaseSlugForClaim(lease, "requested-slug"); got != "provider-slug" {
		t.Fatalf("slug=%q", got)
	}
}

func TestDoctorExecutesProviderAsChildProcess(t *testing.T) {
	cfg := testConfig()
	cfg.External.Command = os.Args[0]
	cfg.External.Args = []string{"-test.run=TestExternalProviderHelperProcess", "--"}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: processRunner{}}}
	result, err := backend.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Message != "child process ready" {
		t.Fatalf("result=%#v", result)
	}
}

func TestAcquireReleasesInvalidLeaseResponse(t *testing.T) {
	isolateCrabboxState(t)
	runner := &sequenceRunner{responses: []string{
		`{"protocolVersion":1,"lease":{"name":"created-without-ssh"}}`,
		`{"protocolVersion":1}`,
	}}
	backend := &leaseBackend{cfg: testConfig(), rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	_, err := backend.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "invalid", Keep: false})
	if err == nil || !strings.Contains(err.Error(), "SSH host and user are required") {
		t.Fatalf("err=%v", err)
	}
	if len(runner.operations) != 2 || runner.operations[0] != "acquire" || runner.operations[1] != "release" {
		t.Fatalf("operations=%#v", runner.operations)
	}
}

func TestResolveRejectsReplacementLeaseIdentity(t *testing.T) {
	isolateCrabboxState(t)
	repo := t.TempDir()
	cfg := testConfig()
	claimExternalLease(t, cfg, "cbx_000000000001", "shared", repo, time.Minute, false)
	server := core.Server{Name: "devbox-shared", Labels: map[string]string{"name": "devbox-shared", "slug": "shared"}}
	if err := core.UpdateLeaseClaimEndpoint("cbx_000000000001", server, core.SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	runner := &sequenceRunner{responses: []string{
		`{"protocolVersion":1,"lease":{"leaseId":"cbx_000000000002","slug":"shared","name":"devbox-shared"}}`,
	}}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	if _, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "shared", ReleaseOnly: true}); err == nil || !strings.Contains(err.Error(), "lease identity changed") {
		t.Fatalf("err=%v", err)
	}
}

func TestResolveRejectsLeaseWithoutStableIdentity(t *testing.T) {
	isolateCrabboxState(t)
	runner := &sequenceRunner{responses: []string{
		`{"protocolVersion":1,"lease":{"slug":"shared","name":"devbox-shared"}}`,
	}}
	backend := &leaseBackend{cfg: testConfig(), rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	if _, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "shared", ReleaseOnly: true}); err == nil || !strings.Contains(err.Error(), "no stable leaseId") {
		t.Fatalf("err=%v", err)
	}
}

func TestResolveRejectsNonCanonicalLeaseID(t *testing.T) {
	isolateCrabboxState(t)
	runner := &sequenceRunner{responses: []string{
		`{"protocolVersion":1,"lease":{"leaseId":"../../outside","slug":"shared","name":"devbox-shared"}}`,
	}}
	backend := &leaseBackend{cfg: testConfig(), rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	if _, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "shared"}); err == nil || !strings.Contains(err.Error(), "cbx_") {
		t.Fatalf("err=%v", err)
	}
}

func TestReleaseAllowsLegacyProviderLeaseID(t *testing.T) {
	runner := &sequenceRunner{responses: []string{
		`{"protocolVersion":1}`,
	}}
	backend := &leaseBackend{cfg: testConfig(), rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	lease := core.LeaseTarget{LeaseID: "provider-id", Server: core.Server{Name: "legacy-devbox"}}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if len(runner.operations) != 1 || runner.operations[0] != "release" {
		t.Fatalf("operations=%#v", runner.operations)
	}
}

func TestResolvePersistsRoutingBeforeSSHReadiness(t *testing.T) {
	isolateCrabboxState(t)
	runner := &sequenceRunner{responses: []string{
		`{"protocolVersion":1,"lease":{"leaseId":"cbx_abcdef123456","slug":"shared","name":"devbox-shared","ssh":{"host":"127.0.0.1","user":"tester","port":"1"}}}`,
	}}
	backend := &leaseBackend{cfg: testConfig(), rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := backend.Resolve(ctx, core.ResolveRequest{ID: "shared"}); err == nil {
		t.Fatal("expected canceled SSH readiness")
	}
	path, err := core.ExternalRoutingPath("cbx_abcdef123456")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("routing state missing: %v", err)
	}
}

func TestAcquirePersistsRoutingBeforeSSHReadinessForKeptLease(t *testing.T) {
	isolateCrabboxState(t)
	runner := &sequenceRunner{responses: []string{
		`{"protocolVersion":1,"lease":{"slug":"shared","name":"devbox-shared","ssh":{"host":"127.0.0.1","user":"tester","port":"1"}}}`,
	}}
	backend := &leaseBackend{cfg: testConfig(), rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := backend.Acquire(ctx, core.AcquireRequest{RequestedSlug: "shared", Keep: true}); err == nil {
		t.Fatal("expected canceled SSH readiness")
	}
	if len(runner.requests) == 0 || runner.requests[0].Desired == nil {
		t.Fatalf("requests=%#v", runner.requests)
	}
	leaseID := runner.requests[0].Desired.LeaseID
	path, err := core.ExternalRoutingPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("routing state missing for %s: %v", leaseID, err)
	}
	if len(runner.operations) != 1 || runner.operations[0] != "acquire" {
		t.Fatalf("operations=%#v", runner.operations)
	}
}

func TestResolvePreservesClaimedLifecycleLabels(t *testing.T) {
	isolateCrabboxState(t)
	repo := t.TempDir()
	cfg := testConfig()
	claimExternalLease(t, cfg, "cbx_000000000003", "ephemeral", repo, time.Minute, false)
	server := core.Server{Name: "devbox-ephemeral", Labels: map[string]string{
		"name":         "devbox-ephemeral",
		"slug":         "ephemeral",
		"keep":         "false",
		"created_at":   "100",
		"expires_at":   "200",
		"ttl_secs":     "100",
		"idle_timeout": "50",
	}}
	if err := core.UpdateLeaseClaimEndpoint("cbx_000000000003", server, core.SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	runner := &sequenceRunner{responses: []string{
		`{"protocolVersion":1,"lease":{"leaseId":"cbx_000000000003","slug":"ephemeral","name":"devbox-ephemeral"}}`,
	}}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "ephemeral", ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.Labels["keep"] != "false" || lease.Server.Labels["created_at"] != "100" || lease.Server.Labels["expires_at"] != "200" {
		t.Fatalf("labels=%#v", lease.Server.Labels)
	}
}

func TestDeclarativeReleaseOnlyResolveSkipsSSHConnectionExpansion(t *testing.T) {
	isolateCrabboxState(t)
	repo := t.TempDir()
	cfg := core.BaseConfig()
	cfg.External = core.ExternalConfig{
		Lifecycle: core.ExternalLifecycleConfig{
			Acquire: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "new", "{{name}}"}},
			List: core.ExternalLifecycleOperation{
				Argv:   []string{"devboxctl", "list", "--format", "json"},
				Output: lifecycleOutputJSONNameArray,
			},
			Release: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "rm", "{{name}}"}},
		},
		Connection: core.ExternalConnectionConfig{
			SSH: core.ExternalSSHConnectionConfig{
				User: "{{env.MISSING_DEVBOX_USER}}",
				Host: "{{name}}",
			},
		},
	}
	claimExternalLease(t, cfg, "cbx_000000000006", "ephemeral", repo, time.Minute, false)
	server := core.Server{Name: "devbox-ephemeral", Labels: map[string]string{"name": "devbox-ephemeral", "slug": "ephemeral"}}
	if err := core.UpdateLeaseClaimEndpoint("cbx_000000000006", server, core.SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: &recordingRunner{}}}
	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "ephemeral", ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != "cbx_000000000006" || lease.Server.Name != "devbox-ephemeral" {
		t.Fatalf("lease=%#v", lease)
	}
}

func TestCleanupReconcilesExternalClaims(t *testing.T) {
	isolateCrabboxState(t)
	repo := t.TempDir()
	cfg := testConfig()
	claimExternalLease(t, cfg, "cbx_000000000004", "live", repo, time.Minute, false)
	claimExternalLease(t, cfg, "cbx_000000000005", "stale", repo, time.Minute, false)
	runner := &sequenceRunner{responses: []string{
		`{"protocolVersion":1}`,
		`{"protocolVersion":1,"leases":[{"leaseId":"cbx_000000000004","slug":"live","name":"live"}]}`,
	}}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider("live", providerName); err != nil || !ok {
		t.Fatalf("live claim ok=%v err=%v", ok, err)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider("stale", providerName); err != nil || ok {
		t.Fatalf("stale claim ok=%v err=%v", ok, err)
	}
}

func TestCleanupPreservesOtherExternalScopeClaims(t *testing.T) {
	isolateCrabboxState(t)
	repo := t.TempDir()
	cfg := testConfig()
	otherCfg := testConfig()
	otherCfg.External.Config = map[string]any{"namespace": "prod", "cpu": 32}
	claimExternalLease(t, cfg, "cbx_000000000007", "stale", repo, time.Minute, false)
	claimExternalLease(t, otherCfg, "cbx_000000000008", "other", repo, time.Minute, false)
	runner := &sequenceRunner{responses: []string{
		`{"protocolVersion":1}`,
		`{"protocolVersion":1,"leases":[]}`,
	}}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider("stale", providerName); err != nil || ok {
		t.Fatalf("same-scope stale claim ok=%v err=%v", ok, err)
	}
	if claim, ok, err := core.ResolveLeaseClaimForProvider("other", providerName); err != nil || !ok || claim.LeaseID != "cbx_000000000008" {
		t.Fatalf("other-scope claim=%#v ok=%v err=%v", claim, ok, err)
	}
}

func TestCleanupRejectsMalformedInventoryBeforeRemovingClaims(t *testing.T) {
	isolateCrabboxState(t)
	cfg := testConfig()
	claimExternalLease(t, cfg, "cbx_000000000006", "live", t.TempDir(), time.Minute, false)
	runner := &sequenceRunner{responses: []string{
		`{"protocolVersion":1}`,
		`{"protocolVersion":1,"leases":[{"name":"missing-id"}]}`,
	}}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{}); err == nil || !strings.Contains(err.Error(), "missing leaseId") {
		t.Fatalf("err=%v", err)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider("live", providerName); err != nil || !ok {
		t.Fatalf("claim removed ok=%v err=%v", ok, err)
	}
}

func TestExternalProviderHelperProcess(t *testing.T) {
	if !strings.Contains(strings.Join(os.Args, " "), "TestExternalProviderHelperProcess") {
		return
	}
	var request protocolRequest
	if err := json.NewDecoder(os.Stdin).Decode(&request); err != nil {
		os.Exit(2)
	}
	if request.ProtocolVersion != protocolVersion || request.Operation != "doctor" || request.Config["namespace"] != "dev" {
		os.Exit(3)
	}
	_, _ = io.WriteString(os.Stdout, `{"protocolVersion":1,"message":"child process ready"}`)
	os.Exit(0)
}

type recordingRunner struct {
	name   string
	args   []string
	stdin  []byte
	stdout string
}

func (r *recordingRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.name = req.Name
	r.args = append([]string(nil), req.Args...)
	if req.Stdin != nil {
		r.stdin, _ = io.ReadAll(req.Stdin)
	}
	return core.LocalCommandResult{Stdout: r.stdout}, nil
}

type processRunner struct{}

func (processRunner) Run(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	cmd := exec.CommandContext(ctx, req.Name, req.Args...)
	cmd.Stdin = req.Stdin
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return core.LocalCommandResult{Stdout: stdout.String(), Stderr: stderr.String()}, err
}

type sequenceRunner struct {
	responses  []string
	operations []string
	requests   []protocolRequest
}

func (r *sequenceRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	var request protocolRequest
	if err := json.NewDecoder(req.Stdin).Decode(&request); err != nil {
		return core.LocalCommandResult{}, err
	}
	r.operations = append(r.operations, request.Operation)
	r.requests = append(r.requests, request)
	response := r.responses[0]
	r.responses = r.responses[1:]
	return core.LocalCommandResult{Stdout: response}, nil
}
