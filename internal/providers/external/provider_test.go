package external

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

func envContains(env []string, entry string) bool {
	for _, candidate := range env {
		if candidate == entry {
			return true
		}
	}
	return false
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

func TestConfigurePreservesOverridesAppliedToLoadedRouting(t *testing.T) {
	isolateCrabboxState(t)
	saved := testConfig()
	path, err := core.PersistExternalRouting("cbx_abcdef123456", saved.External)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := core.LoadExternalRouting(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded.Command = "override-provider"
	loaded.WorkRoot = "/override/work"
	cfg := core.BaseConfig()
	cfg.External = loaded
	cfg.WorkRoot = loaded.WorkRoot
	backend, err := (Provider{}).Configure(cfg, core.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	got := backend.(*leaseBackend).cfg
	if got.External.Command != loaded.Command || got.WorkRoot != loaded.WorkRoot {
		t.Fatalf("config=%#v", got)
	}
}

func TestConfigureLoadsConfiguredRoutingFile(t *testing.T) {
	isolateCrabboxState(t)
	saved := testConfig()
	path, err := core.PersistExternalRouting("cbx_abcdef123456", saved.External)
	if err != nil {
		t.Fatal(err)
	}
	cfg := core.BaseConfig()
	cfg.External.RoutingFile = path
	backend, err := (Provider{}).Configure(cfg, core.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	got := backend.(*leaseBackend).cfg
	if got.External.Command != saved.External.Command || got.WorkRoot != saved.External.WorkRoot {
		t.Fatalf("config=%#v", got)
	}
}

func TestApplyFlagsLoadsConfiguredRoutingFile(t *testing.T) {
	isolateCrabboxState(t)
	saved := testConfig()
	path, err := core.PersistExternalRouting("cbx_abcdef123456", saved.External)
	if err != nil {
		t.Fatal(err)
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.External.RoutingFile = path
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := registerFlags(fs, cfg)
	if err := applyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.External.Command != saved.External.Command || cfg.WorkRoot != saved.External.WorkRoot || !core.ExternalRoutingLoaded(cfg.External) {
		t.Fatalf("config=%#v", cfg)
	}
}

func TestApplyFlagsExplicitRoutingOverridesStaleConfiguredPath(t *testing.T) {
	isolateCrabboxState(t)
	saved := testConfig()
	path, err := core.PersistExternalRouting("cbx_abcdef123456", saved.External)
	if err != nil {
		t.Fatal(err)
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.External.RoutingFile = filepath.Join(t.TempDir(), "missing.json")
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := registerFlags(fs, cfg)
	if err := fs.Parse([]string{"--external-routing-file", path}); err != nil {
		t.Fatal(err)
	}
	if err := applyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.External.RoutingFile != path || cfg.External.Command != saved.External.Command {
		t.Fatalf("config=%#v", cfg)
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

func TestInvokeDeclarativeLifecycleRunsOrderedSteps(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.External = core.ExternalConfig{
		Config: map[string]any{"size": "cpu16"},
		Lifecycle: core.ExternalLifecycleConfig{
			Acquire: core.ExternalLifecycleOperation{
				Steps: [][]string{
					{"devboxctl", "new", "{{resourceName}}", "--size", "{{config.size}}"},
					{"devboxctl", "setup", "{{resourceName}}"},
				},
			},
			List: core.ExternalLifecycleOperation{
				Argv:   []string{"devboxctl", "list"},
				Output: lifecycleOutputJSONNameArray,
			},
			Release: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "rm", "{{resourceName}}"}, AllowEnvArgv: true},
		},
		Connection: core.ExternalConnectionConfig{
			ResourceName: "{{leaseIdSlug}}",
			SSH:          core.ExternalSSHConnectionConfig{User: "developer", Host: "{{resourceName}}"},
		},
	}
	runner := &recordingRunner{}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	response, err := backend.invokeLifecycle(context.Background(), protocolRequest{
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
	if len(runner.requests) != 2 {
		t.Fatalf("requests=%#v", runner.requests)
	}
	if got := runner.requests[0]; got.Name != "devboxctl" || strings.Join(got.Args, "|") != "new|cbx-abcdef123456|--size|cpu16" {
		t.Fatalf("first=%#v", got)
	}
	if got := runner.requests[1]; got.Name != "devboxctl" || strings.Join(got.Args, "|") != "setup|cbx-abcdef123456" {
		t.Fatalf("second=%#v", got)
	}
	if response.Lease == nil || response.Lease.Labels[externalResourceNameLabel] != "cbx-abcdef123456" {
		t.Fatalf("response=%#v", response)
	}
}

func TestInvokeDeclarativeLifecycleRollsBackFailedAcquireStep(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.External = core.ExternalConfig{
		Lifecycle: core.ExternalLifecycleConfig{
			Acquire: core.ExternalLifecycleOperation{
				Steps: [][]string{
					{"devboxctl", "new", "{{resourceName}}"},
					{"devboxctl", "setup", "{{resourceName}}"},
				},
				RollbackOnFailure: true,
			},
			List: core.ExternalLifecycleOperation{
				Argv:   []string{"devboxctl", "list"},
				Output: lifecycleOutputJSONNameArray,
			},
			Release: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "rm", "--yes", "{{resourceName}}"}},
		},
		Connection: core.ExternalConnectionConfig{
			ResourceName: "{{leaseIdSlug}}",
			SSH:          core.ExternalSSHConnectionConfig{User: "developer", Host: "{{resourceName}}"},
		},
	}
	runner := &failingLifecycleStepRunner{failAt: 2}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	_, err := backend.invokeLifecycle(context.Background(), protocolRequest{
		Operation: "acquire",
		Desired: &desiredLease{
			LeaseID: "cbx_abcdef123456",
			Slug:    "fast-coral",
			Name:    "crabbox-fast-coral-deadbeef",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "acquire step 2 failed") {
		t.Fatalf("err=%v", err)
	}
	if len(runner.requests) != 3 {
		t.Fatalf("requests=%#v", runner.requests)
	}
	if got := runner.requests[2]; got.Name != "devboxctl" || strings.Join(got.Args, "|") != "rm|--yes|cbx-abcdef123456" {
		t.Fatalf("rollback=%#v", got)
	}
}

func TestInvokeDeclarativeLifecycleKeepsFailedAcquireWhenRequested(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.External = core.ExternalConfig{
		Lifecycle: core.ExternalLifecycleConfig{
			Acquire: core.ExternalLifecycleOperation{
				Steps: [][]string{
					{"devboxctl", "new", "{{resourceName}}"},
					{"devboxctl", "setup", "{{resourceName}}"},
				},
				RollbackOnFailure: true,
			},
			List: core.ExternalLifecycleOperation{
				Argv:   []string{"devboxctl", "list"},
				Output: lifecycleOutputJSONNameArray,
			},
			Release: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "rm", "--yes", "{{resourceName}}"}},
		},
		Connection: core.ExternalConnectionConfig{
			ResourceName: "{{leaseIdSlug}}",
			SSH:          core.ExternalSSHConnectionConfig{User: "developer", Host: "{{resourceName}}"},
		},
	}
	runner := &failingLifecycleStepRunner{failAt: 2}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	_, err := backend.invokeLifecycle(context.Background(), protocolRequest{
		Operation: "acquire",
		Keep:      true,
		Desired: &desiredLease{
			LeaseID: "cbx_abcdef123456",
			Slug:    "fast-coral",
			Name:    "crabbox-fast-coral-deadbeef",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "acquire step 2 failed") {
		t.Fatalf("err=%v", err)
	}
	if len(runner.requests) != 2 {
		t.Fatalf("keep=true unexpectedly ran rollback: %#v", runner.requests)
	}
}

func TestInvokeDeclarativeLifecycleReportsFailedRollback(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.External = core.ExternalConfig{
		Lifecycle: core.ExternalLifecycleConfig{
			Acquire: core.ExternalLifecycleOperation{
				Steps: [][]string{
					{"devboxctl", "new", "{{resourceName}}"},
					{"devboxctl", "setup", "{{resourceName}}"},
				},
				RollbackOnFailure: true,
			},
			List: core.ExternalLifecycleOperation{
				Argv:   []string{"devboxctl", "list"},
				Output: lifecycleOutputJSONNameArray,
			},
			Release: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "rm", "--yes", "{{resourceName}}"}},
		},
		Connection: core.ExternalConnectionConfig{
			ResourceName: "{{leaseIdSlug}}",
			SSH:          core.ExternalSSHConnectionConfig{User: "developer", Host: "{{resourceName}}"},
		},
	}
	runner := &failingLifecycleRollbackRunner{}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	_, err := backend.invokeLifecycle(context.Background(), protocolRequest{
		Operation: "acquire",
		Desired: &desiredLease{
			LeaseID: "cbx_abcdef123456",
			Slug:    "fast-coral",
			Name:    "crabbox-fast-coral-deadbeef",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "acquire step 2 failed") ||
		!strings.Contains(err.Error(), "rollback failed") || !strings.Contains(err.Error(), "delete failed") {
		t.Fatalf("err=%v", err)
	}
	if !runner.rollbackHasDeadline {
		t.Fatal("rollback command did not receive a bounded context")
	}
}

func TestInvokeDeclarativeLifecycleExpandsAllStepsBeforeRunning(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.External = core.ExternalConfig{
		Lifecycle: core.ExternalLifecycleConfig{
			Acquire: core.ExternalLifecycleOperation{
				Steps: [][]string{
					{"devboxctl", "new", "{{resourceName}}"},
					{"devboxctl", "setup", "{{env.MISSING_DEVBOX_SETUP}}"},
				},
				RollbackOnFailure: true,
			},
			List: core.ExternalLifecycleOperation{
				Argv:   []string{"devboxctl", "list"},
				Output: lifecycleOutputJSONNameArray,
			},
			Release: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "rm", "--yes", "{{resourceName}}"}},
		},
		Connection: core.ExternalConnectionConfig{
			ResourceName: "{{leaseIdSlug}}",
			SSH:          core.ExternalSSHConnectionConfig{User: "developer", Host: "{{resourceName}}"},
		},
	}
	runner := &recordingRunner{}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	_, err := backend.invokeLifecycle(context.Background(), protocolRequest{
		Operation: "acquire",
		Desired: &desiredLease{
			LeaseID: "cbx_abcdef123456",
			Slug:    "fast-coral",
			Name:    "crabbox-fast-coral-deadbeef",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "acquire step 2") || !strings.Contains(err.Error(), "MISSING_DEVBOX_SETUP") {
		t.Fatalf("err=%v", err)
	}
	if len(runner.requests) != 0 {
		t.Fatalf("commands ran before all steps expanded: %#v", runner.requests)
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

func TestInvokeDeclarativeLifecyclePassesSecretEnvWithoutArgvExposure(t *testing.T) {
	t.Setenv("DEVBOX_TOKEN", "super-secret-token")
	cfg := core.BaseConfig()
	cfg.External = core.ExternalConfig{
		Lifecycle: core.ExternalLifecycleConfig{
			Acquire: core.ExternalLifecycleOperation{
				Argv: []string{"devboxctl", "new", "{{name}}"},
				Env: map[string]string{
					"DEVBOX_TOKEN": "{{env.DEVBOX_TOKEN}}",
					"DEVBOX_NAME":  "{{name}}",
				},
			},
			List: core.ExternalLifecycleOperation{
				Argv:   []string{"devboxctl", "list"},
				Output: lifecycleOutputJSONNameArray,
			},
			Release: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "rm", "{{name}}"}},
		},
		Connection: core.ExternalConnectionConfig{
			SSH: core.ExternalSSHConnectionConfig{User: "developer", Host: "{{name}}"},
		},
	}
	runner := &recordingRunner{}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	_, err := backend.invokeLifecycle(context.Background(), protocolRequest{
		Operation: "acquire",
		Desired:   &desiredLease{LeaseID: "cbx_abcdef123456", Slug: "fast-coral", Name: "devbox-fast-coral"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.requests) != 1 {
		t.Fatalf("requests=%#v", runner.requests)
	}
	gotArgv := runner.requests[0].Name + " " + strings.Join(runner.requests[0].Args, " ")
	if strings.Contains(gotArgv, "super-secret-token") {
		t.Fatalf("secret leaked through argv: %q", gotArgv)
	}
	if !envContains(runner.requests[0].Env, "DEVBOX_TOKEN=super-secret-token") {
		t.Fatal("env missing DEVBOX_TOKEN entry")
	}
	if !envContains(runner.requests[0].Env, "DEVBOX_NAME=devbox-fast-coral") {
		t.Fatal("env missing DEVBOX_NAME entry")
	}
}

func TestInvokeDeclarativeLifecyclePreservesEnvResourceNameProvenance(t *testing.T) {
	t.Setenv("DEVBOX_RESOURCE", "durable-resource-name")
	cfg := core.BaseConfig()
	cfg.External = core.ExternalConfig{
		Lifecycle: core.ExternalLifecycleConfig{
			Acquire: core.ExternalLifecycleOperation{
				Argv: []string{"devboxctl", "new"},
				Env:  map[string]string{"DEVBOX_RESOURCE": "{{env.DEVBOX_RESOURCE}}"},
			},
			Release: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "rm", "{{resourceName}}"}},
		},
		Connection: core.ExternalConnectionConfig{
			ResourceName:         "{{env.DEVBOX_RESOURCE}}",
			AllowEnvResourceName: true,
			SSH:                  core.ExternalSSHConnectionConfig{User: "developer", Host: "{{name}}"},
		},
	}
	runner := &recordingRunner{}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	response, err := backend.invokeLifecycle(context.Background(), protocolRequest{
		Operation: "acquire",
		Desired:   &desiredLease{LeaseID: "cbx_abcdef123456", Slug: "fast-coral", Name: "devbox-fast-coral"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Lease == nil || response.Lease.Labels[externalResourceNameFromEnv] != "true" {
		t.Fatalf("lease labels=%#v, want env resourceName provenance", response.Lease.Labels)
	}
	_, err = backend.invokeLifecycle(context.Background(), protocolRequest{Operation: "release", Lease: response.Lease})
	if err == nil || !strings.Contains(err.Error(), "environment-derived value") {
		t.Fatalf("err=%v, want env resourceName argv rejection", err)
	}
	if strings.Contains(err.Error(), "durable-resource-name") {
		t.Fatalf("resource value leaked through error: %v", err)
	}
	if len(runner.requests) != 1 {
		t.Fatalf("release command ran despite secret resourceName: %#v", runner.requests)
	}
}

func TestInvokeDeclarativeLifecycleRejectsEnvironmentDerivedArgv(t *testing.T) {
	t.Setenv("DEVBOX_TOKEN", "super-secret-token")
	t.Setenv("DEVBOX_REGION", "us-test-1")
	t.Setenv("E2B_API_KEY", "e2b-secret-key")
	for name, cfg := range map[string]core.ExternalConfig{
		"apiKey": {
			Lifecycle: core.ExternalLifecycleConfig{
				Acquire: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "new", "--api-key", "{{env.E2B_API_KEY}}"}},
			},
		},
		"direct": {
			Lifecycle: core.ExternalLifecycleConfig{
				Acquire: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "new", "--token", "{{env.DEVBOX_TOKEN}}"}},
			},
		},
		"mixed": {
			Lifecycle: core.ExternalLifecycleConfig{
				Acquire: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "new", "{{env.DEVBOX_TOKEN}}-{{env.DEVBOX_REGION}}"}},
			},
		},
		"resourceName": {
			Lifecycle: core.ExternalLifecycleConfig{
				Acquire: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "new", "{{resourceName}}"}},
			},
			Connection: core.ExternalConnectionConfig{ResourceName: "{{env.DEVBOX_TOKEN}}"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			fullCfg := core.BaseConfig()
			fullCfg.External = cfg
			runner := &recordingRunner{}
			backend := &leaseBackend{cfg: fullCfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
			_, err := backend.invokeLifecycle(context.Background(), protocolRequest{
				Operation: "acquire",
				Desired:   &desiredLease{LeaseID: "cbx_abcdef123456", Slug: "fast-coral", Name: "devbox-fast-coral"},
			})
			if err == nil || !strings.Contains(err.Error(), "environment-derived value") {
				t.Fatalf("err=%v, want argv secret rejection", err)
			}
			if strings.Contains(err.Error(), "super-secret-token") {
				t.Fatalf("secret leaked through error: %v", err)
			}
			if len(runner.requests) != 0 {
				t.Fatalf("command ran despite secret argv: %#v", runner.requests)
			}
		})
	}
}

func TestInvokeDeclarativeLifecycleAllowsBenignEnvironmentArgv(t *testing.T) {
	t.Setenv("AUTH_MODE", "oauth")
	t.Setenv("GIT_AUTHOR_NAME", "Alice")
	t.Setenv("E2B_API_KEY_FILE", "/tmp/e2b-key")
	t.Setenv("SSH_PRIVATE_KEY_PATH", "/tmp/id_ed25519")
	cfg := core.BaseConfig()
	cfg.External = core.ExternalConfig{
		Lifecycle: core.ExternalLifecycleConfig{
			Acquire: core.ExternalLifecycleOperation{
				Argv:         []string{"devboxctl", "new", "--auth-mode", "{{env.AUTH_MODE}}", "--author", "{{env.GIT_AUTHOR_NAME}}", "--api-key-file", "{{env.E2B_API_KEY_FILE}}", "-i", "{{env.SSH_PRIVATE_KEY_PATH}}"},
				AllowEnvArgv: true,
			},
			List: core.ExternalLifecycleOperation{
				Argv:   []string{"devboxctl", "list"},
				Output: lifecycleOutputJSONNameArray,
			},
			Release: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "rm", "{{name}}"}},
		},
		Connection: core.ExternalConnectionConfig{
			SSH: core.ExternalSSHConnectionConfig{User: "developer", Host: "{{name}}"},
		},
	}
	runner := &recordingRunner{}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	_, err := backend.invokeLifecycle(context.Background(), protocolRequest{
		Operation: "acquire",
		Desired:   &desiredLease{LeaseID: "cbx_abcdef123456", Slug: "fast-coral", Name: "devbox-fast-coral"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.requests) != 1 {
		t.Fatalf("requests=%#v", runner.requests)
	}
	if got := strings.Join(runner.requests[0].Args, "|"); got != "new|--auth-mode|oauth|--author|Alice|--api-key-file|/tmp/e2b-key|-i|/tmp/id_ed25519" {
		t.Fatalf("args=%q", got)
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
			Acquire: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "new", "{{resourceName}}"}, AllowEnvArgv: true},
			List: core.ExternalLifecycleOperation{
				Argv:   []string{"devboxctl", "list"},
				Output: lifecycleOutputJSONNameArray,
			},
			Release: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "rm", "{{resourceName}}"}, AllowEnvArgv: true},
		},
		Connection: core.ExternalConnectionConfig{
			ResourceName:         "{{env.DEVBOX_RESOURCE}}",
			AllowEnvResourceName: true,
			SSH:                  core.ExternalSSHConnectionConfig{User: "developer"},
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
			Acquire: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "new", "{{resourceName}}"}, AllowEnvArgv: true},
			List: core.ExternalLifecycleOperation{
				Argv:   []string{"devboxctl", "list"},
				Output: lifecycleOutputJSONNameArray,
			},
			Release: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "rm", "{{resourceName}}"}, AllowEnvArgv: true},
		},
		Connection: core.ExternalConnectionConfig{
			ResourceName:         "{{env.DEVBOX_RESOURCE}}",
			AllowEnvResourceName: true,
			SSH:                  core.ExternalSSHConnectionConfig{User: "developer"},
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
			Acquire: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "new", "{{resourceName}}"}, AllowEnvArgv: true},
			List: core.ExternalLifecycleOperation{
				Argv:   []string{"devboxctl", "list"},
				Output: lifecycleOutputJSONNameArray,
			},
			Release: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "rm", "{{resourceName}}"}},
		},
		Connection: core.ExternalConnectionConfig{
			ResourceName:         "{{env.DEVBOX_RESOURCE}}",
			AllowEnvResourceName: true,
			SSH:                  core.ExternalSSHConnectionConfig{User: "developer"},
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

func TestDeclarativeInventoryPreservesEnvResourceNameProvenance(t *testing.T) {
	isolateCrabboxState(t)
	cfg := core.BaseConfig()
	cfg.External = core.ExternalConfig{
		Lifecycle: core.ExternalLifecycleConfig{
			Acquire: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "new", "{{name}}"}},
			List: core.ExternalLifecycleOperation{
				Argv:   []string{"devboxctl", "list"},
				Output: lifecycleOutputJSONNameArray,
			},
			Release: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "rm", "{{resourceName}}"}},
		},
		Connection: core.ExternalConnectionConfig{
			SSH: core.ExternalSSHConnectionConfig{User: "developer", Host: "{{resourceName}}"},
		},
		WorkRoot: "/home/developer/crabbox",
	}
	claimExternalLease(t, cfg, "cbx_abcdef123456", "fast-coral", t.TempDir(), time.Minute, false)
	if err := core.UpdateLeaseClaimEndpoint(
		"cbx_abcdef123456",
		core.Server{Name: "env-resource", Labels: map[string]string{
			"name":                      "env-resource",
			"slug":                      "fast-coral",
			externalResourceNameLabel:   "env-resource",
			externalResourceNameFromEnv: "true",
		}},
		core.SSHTarget{},
	); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{stdout: `["env-resource"]`}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	response, err := backend.invoke(context.Background(), protocolRequest{Operation: "list"})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Leases) != 1 || response.Leases[0].Labels[externalResourceNameFromEnv] != "true" {
		t.Fatalf("leases=%#v, want env resourceName provenance", response.Leases)
	}
	_, err = backend.invoke(context.Background(), protocolRequest{Operation: "release", Lease: &response.Leases[0]})
	if err == nil || !strings.Contains(err.Error(), "environment-derived value") {
		t.Fatalf("err=%v, want env resourceName argv rejection", err)
	}
	if len(runner.requests) != 1 {
		t.Fatalf("release command ran despite env-derived resourceName: %#v", runner.requests)
	}
}

func TestDeclarativeResolveThenReleaseReusesPersistedResourceName(t *testing.T) {
	isolateCrabboxState(t)
	t.Setenv("DEVBOX_RESOURCE", "new-resource")
	cfg := core.BaseConfig()
	cfg.External = core.ExternalConfig{
		Lifecycle: core.ExternalLifecycleConfig{
			Acquire: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "new", "{{resourceName}}"}, AllowEnvArgv: true},
			List: core.ExternalLifecycleOperation{
				Argv:   []string{"devboxctl", "list"},
				Output: lifecycleOutputJSONNameArray,
			},
			Release: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "rm", "{{resourceName}}"}, AllowEnvArgv: true},
		},
		Connection: core.ExternalConnectionConfig{
			ResourceName:         "{{env.DEVBOX_RESOURCE}}",
			AllowEnvResourceName: true,
			SSH:                  core.ExternalSSHConnectionConfig{User: "developer"},
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
	// A private per-lease route remains authoritative when a provider upgrade
	// changes the lifecycle scope encoded in the current routing state.
	cfg.External.Config = map[string]any{"cluster": "new-cluster"}
	routingPath, err := core.PersistExternalRouting("cbx_abcdef123456", cfg.External)
	if err != nil {
		t.Fatal(err)
	}
	cfg.External.RoutingFile = routingPath
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

func TestResolveClaimMatchesCloudID(t *testing.T) {
	root := isolateCrabboxState(t)
	cfg := testConfig()
	leaseID := "cbx_abcdef123456"
	claimExternalLease(t, cfg, leaseID, "fast-coral", root, time.Minute, false)
	if err := core.UpdateLeaseClaimEndpoint(leaseID, core.Server{CloudID: "provider/resource-123"}, core.SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	backend := &leaseBackend{cfg: cfg}
	claim, ok, err := backend.resolveClaim("provider/resource-123")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || claim.LeaseID != leaseID {
		t.Fatalf("claim=%#v ok=%v", claim, ok)
	}
}

func TestDeclarativeResolveThenReleasePreservesEnvResourceNameProvenance(t *testing.T) {
	isolateCrabboxState(t)
	cfg := core.BaseConfig()
	cfg.External = core.ExternalConfig{
		Lifecycle: core.ExternalLifecycleConfig{
			Acquire: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "new", "{{name}}"}},
			List: core.ExternalLifecycleOperation{
				Argv:   []string{"devboxctl", "list"},
				Output: lifecycleOutputJSONNameArray,
			},
			Release: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "rm", "{{resourceName}}"}},
		},
		Connection: core.ExternalConnectionConfig{
			SSH: core.ExternalSSHConnectionConfig{User: "developer"},
		},
		WorkRoot: "/home/developer/crabbox",
	}
	claimExternalLease(t, cfg, "cbx_abcdef123456", "fast-coral", t.TempDir(), time.Minute, false)
	if err := core.UpdateLeaseClaimEndpoint(
		"cbx_abcdef123456",
		core.Server{Name: "crabbox-fast-coral-deadbeef", Labels: map[string]string{
			"name":                      "crabbox-fast-coral-deadbeef",
			"slug":                      "fast-coral",
			externalResourceNameLabel:   "env-resource",
			externalResourceNameFromEnv: "true",
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
	if lease.Server.Labels[externalResourceNameFromEnv] != "true" {
		t.Fatalf("lease=%#v, want env resourceName provenance", lease)
	}
	err = backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease})
	if err == nil || !strings.Contains(err.Error(), "environment-derived value") {
		t.Fatalf("err=%v, want env resourceName argv rejection", err)
	}
	if len(runner.requests) != 0 {
		t.Fatalf("release command ran despite env-derived resourceName: %#v", runner.requests)
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
	slug, reservation, err := backend.allocateLeaseSlug("cbx_new", "shared")
	if err != nil {
		t.Fatal(err)
	}
	if reservation != nil {
		reservation.Release()
	}
	if slug != "shared" {
		t.Fatalf("slug=%q, want shared when collision is outside scope", slug)
	}
	claimExternalLease(t, cfg, "cbx_current", "shared", t.TempDir(), time.Minute, false)
	slug, reservation, err = backend.allocateLeaseSlug("cbx_next", "shared")
	if err != nil {
		t.Fatal(err)
	}
	if reservation != nil {
		defer reservation.Release()
	}
	if slug == "shared" || !strings.HasPrefix(slug, "shared-") {
		t.Fatalf("slug=%q, want current-scope collision suffix", slug)
	}
}

func TestAllocateLeaseSlugReservesRequestedSlug(t *testing.T) {
	isolateCrabboxState(t)
	backend := &leaseBackend{cfg: testConfig()}
	first, firstReservation, err := backend.allocateLeaseSlug("cbx_first", "shared")
	if err != nil {
		t.Fatal(err)
	}
	defer firstReservation.Release()
	if first != "shared" {
		t.Fatalf("first slug=%q, want shared", first)
	}
	second, secondReservation, err := backend.allocateLeaseSlug("cbx_second", "shared")
	if err != nil {
		t.Fatal(err)
	}
	if secondReservation != nil {
		defer secondReservation.Release()
	}
	if second == "shared" || !strings.HasPrefix(second, "shared-") {
		t.Fatalf("second slug=%q, want reserved collision suffix", second)
	}
}

func TestAllocateLeaseSlugChecksGeneratedSlugClaims(t *testing.T) {
	isolateCrabboxState(t)
	cfg := testConfig()
	leaseID := "cbx_new"
	generated := core.NewLeaseSlug(leaseID)
	claimExternalLease(t, cfg, "cbx_existing", generated, t.TempDir(), time.Minute, false)
	backend := &leaseBackend{cfg: cfg}
	slug, reservation, err := backend.allocateLeaseSlug(leaseID, "")
	if err != nil {
		t.Fatal(err)
	}
	if reservation != nil {
		defer reservation.Release()
	}
	if slug == generated || !strings.HasPrefix(slug, generated+"-") {
		t.Fatalf("slug=%q, want generated collision suffix for %q", slug, generated)
	}
}

func TestReserveLeaseSlugRechecksClaimsUnderLock(t *testing.T) {
	isolateCrabboxState(t)
	cfg := testConfig()
	backend := &leaseBackend{cfg: cfg}
	claimExternalLease(t, cfg, "cbx_existing", "shared", t.TempDir(), time.Minute, false)
	reservation, reserved, err := backend.reserveLeaseSlug("shared", "cbx_next")
	if err != nil {
		t.Fatal(err)
	}
	if reservation != nil {
		defer reservation.Release()
	}
	if reserved {
		t.Fatal("reserved slug that was already claimed")
	}
}

func TestAllocateLeaseSlugReclaimsStaleReservation(t *testing.T) {
	isolateCrabboxState(t)
	backend := &leaseBackend{cfg: testConfig()}
	dir, err := backend.slugReservationDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := slugReservationRecord{
		LeaseID:   "cbx_stale",
		Slug:      "shared",
		CreatedAt: time.Now().Add(-externalSlugReservationTTL - time.Minute).UTC().Format(time.RFC3339Nano),
	}
	data, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(slugReservationPath(dir, "shared"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	slug, reservation, err := backend.allocateLeaseSlug("cbx_next", "shared")
	if err != nil {
		t.Fatal(err)
	}
	if reservation != nil {
		defer reservation.Release()
	}
	if slug != "shared" {
		t.Fatalf("slug=%q, want reclaimed shared", slug)
	}
}

func TestAllocateLeaseSlugPreservesActiveStaleReservation(t *testing.T) {
	isolateCrabboxState(t)
	backend := &leaseBackend{cfg: testConfig()}
	dir, err := backend.slugReservationDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	active := slugReservationRecord{
		LeaseID:   "cbx_active",
		Slug:      "shared",
		CreatedAt: time.Now().Add(-externalSlugReservationTTL - time.Minute).UTC().Format(time.RFC3339Nano),
		Token:     "active-token",
		PID:       os.Getpid(),
	}
	data, err := json.Marshal(active)
	if err != nil {
		t.Fatal(err)
	}
	path := slugReservationPath(dir, "shared")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	slug, reservation, err := backend.allocateLeaseSlug("cbx_next", "shared")
	if err != nil {
		t.Fatal(err)
	}
	if reservation != nil {
		defer reservation.Release()
	}
	if slug == "shared" || !strings.HasPrefix(slug, "shared-") {
		t.Fatalf("slug=%q, want suffix while active reservation remains", slug)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("active reservation was removed: %v", err)
	}
}

func TestAllocateLeaseSlugReclaimsMalformedStaleReservation(t *testing.T) {
	isolateCrabboxState(t)
	backend := &leaseBackend{cfg: testConfig()}
	dir, err := backend.slugReservationDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := slugReservationPath(dir, "shared")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-externalSlugReservationTTL - time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	slug, reservation, err := backend.allocateLeaseSlug("cbx_next", "shared")
	if err != nil {
		t.Fatal(err)
	}
	if reservation != nil {
		defer reservation.Release()
	}
	if slug != "shared" {
		t.Fatalf("slug=%q, want reclaimed shared", slug)
	}
}

func TestSlugReservationReleasePreservesNewOwner(t *testing.T) {
	isolateCrabboxState(t)
	backend := &leaseBackend{cfg: testConfig()}
	slug, reservation, err := backend.allocateLeaseSlug("cbx_first", "shared")
	if err != nil {
		t.Fatal(err)
	}
	if slug != "shared" || reservation == nil {
		t.Fatalf("slug=%q reservation=%#v", slug, reservation)
	}
	replacement := slugReservationRecord{
		LeaseID:   "cbx_second",
		Slug:      "shared",
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Token:     "replacement-token",
	}
	data, err := json.Marshal(replacement)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reservation.path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	reservation.Release()
	if _, err := os.Stat(reservation.path); err != nil {
		t.Fatalf("replacement reservation was removed: %v", err)
	}
	_ = os.Remove(reservation.path)
}

func TestResolveClaimRejectsDuplicateScopedSlug(t *testing.T) {
	isolateCrabboxState(t)
	cfg := testConfig()
	claimExternalLease(t, cfg, "cbx_first", "shared", t.TempDir(), time.Minute, false)
	claimExternalLease(t, cfg, "cbx_second", "shared", t.TempDir(), time.Minute, false)
	backend := &leaseBackend{cfg: cfg}
	if _, ok, err := backend.resolveClaim("shared"); err == nil || ok || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ok=%v err=%v, want ambiguous slug", ok, err)
	}
	if claim, ok, err := backend.resolveClaim("cbx_first"); err != nil || !ok || claim.LeaseID != "cbx_first" {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
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

func TestAcquireRollbackReleaseUsesBoundedDetachedContext(t *testing.T) {
	isolateCrabboxState(t)
	oldTimeout := lifecycleRollbackTimeout
	lifecycleRollbackTimeout = 10 * time.Millisecond
	t.Cleanup(func() { lifecycleRollbackTimeout = oldTimeout })

	runner := &blockingAcquireRollbackRunner{}
	backend := &leaseBackend{cfg: testConfig(), rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	_, err := backend.Acquire(ctx, core.AcquireRequest{RequestedSlug: "invalid", Keep: false})
	elapsed := time.Since(start)
	if err == nil ||
		!strings.Contains(err.Error(), "SSH host and user are required") ||
		!strings.Contains(err.Error(), "external provider cleanup failed") ||
		!strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("err=%v, want validation error with bounded cleanup failure", err)
	}
	var exit core.ExitError
	if !core.AsExitError(err, &exit) || exit.Code != 5 || !strings.Contains(exit.Message, "external provider cleanup failed") {
		t.Fatalf("exit=%#v ok=%v, want primary validation exit with cleanup message", exit, core.AsExitError(err, &exit))
	}
	if elapsed > time.Second {
		t.Fatalf("Acquire took %s, want bounded cleanup to return promptly", elapsed)
	}
	if len(runner.operations) != 2 || runner.operations[0] != "acquire" || runner.operations[1] != "release" {
		t.Fatalf("operations=%#v", runner.operations)
	}
	if !runner.releaseHasDeadline {
		t.Fatal("release rollback did not receive a deadline")
	}
}

func TestAcquireRollbackReleasePreservesCanceledPrimaryError(t *testing.T) {
	isolateCrabboxState(t)
	oldTimeout := lifecycleRollbackTimeout
	lifecycleRollbackTimeout = 10 * time.Millisecond
	t.Cleanup(func() { lifecycleRollbackTimeout = oldTimeout })

	runner := &blockingAcquireRollbackRunner{acquireResponse: `{"protocolVersion":1,"lease":{"slug":"invalid","name":"created-with-ssh","ssh":{"host":"127.0.0.1","user":"tester","port":"1"}}}`}
	backend := &leaseBackend{cfg: testConfig(), rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := backend.Acquire(ctx, core.AcquireRequest{RequestedSlug: "invalid", Keep: false})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled in error chain", err)
	}
	if !strings.Contains(err.Error(), "external provider cleanup failed") || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("err=%v, want bounded cleanup failure message", err)
	}
	var exit core.ExitError
	if core.AsExitError(err, &exit) {
		t.Fatalf("exit=%#v, want non-ExitError primary to keep fallback classification", exit)
	}
	if len(runner.operations) != 2 || runner.operations[0] != "acquire" || runner.operations[1] != "release" {
		t.Fatalf("operations=%#v", runner.operations)
	}
	if !runner.releaseHasDeadline {
		t.Fatal("release rollback did not receive a deadline")
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
	name     string
	args     []string
	stdin    []byte
	stdout   string
	requests []core.LocalCommandRequest
}

func (r *recordingRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.name = req.Name
	r.args = append([]string(nil), req.Args...)
	r.requests = append(r.requests, req)
	if req.Stdin != nil {
		r.stdin, _ = io.ReadAll(req.Stdin)
	}
	return core.LocalCommandResult{Stdout: r.stdout}, nil
}

type failingLifecycleStepRunner struct {
	requests []core.LocalCommandRequest
	failAt   int
}

func (r *failingLifecycleStepRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.requests = append(r.requests, req)
	if len(r.requests) == r.failAt {
		return core.LocalCommandResult{ExitCode: 17, Stderr: "setup failed"}, errors.New("exit status 17")
	}
	return core.LocalCommandResult{}, nil
}

type failingLifecycleRollbackRunner struct {
	requests            []core.LocalCommandRequest
	rollbackHasDeadline bool
}

func (r *failingLifecycleRollbackRunner) Run(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.requests = append(r.requests, req)
	switch len(r.requests) {
	case 2:
		return core.LocalCommandResult{ExitCode: 17, Stderr: "setup failed"}, errors.New("exit status 17")
	case 3:
		_, r.rollbackHasDeadline = ctx.Deadline()
		return core.LocalCommandResult{ExitCode: 18, Stderr: "delete failed"}, errors.New("exit status 18")
	default:
		return core.LocalCommandResult{}, nil
	}
}

type blockingAcquireRollbackRunner struct {
	acquireResponse    string
	operations         []string
	releaseHasDeadline bool
}

func (r *blockingAcquireRollbackRunner) Run(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	var request protocolRequest
	if err := json.NewDecoder(req.Stdin).Decode(&request); err != nil {
		return core.LocalCommandResult{}, err
	}
	r.operations = append(r.operations, request.Operation)
	switch request.Operation {
	case "acquire":
		response := r.acquireResponse
		if response == "" {
			response = `{"protocolVersion":1,"lease":{"name":"created-without-ssh"}}`
		}
		return core.LocalCommandResult{Stdout: response}, nil
	case "release":
		_, r.releaseHasDeadline = ctx.Deadline()
		<-ctx.Done()
		return core.LocalCommandResult{ExitCode: 124, Stderr: "release timed out"}, ctx.Err()
	default:
		return core.LocalCommandResult{}, nil
	}
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
