package external

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
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

func currentSlugReservationOwnerIdentity(t *testing.T) (string, string) {
	t.Helper()
	started, err := core.LocalProcessStartIdentity(os.Getpid())
	if err != nil {
		t.Skipf("process start identity unavailable: %v", err)
	}
	bootID, err := core.LocalProcessBootIdentity()
	if err != nil {
		t.Skipf("process boot identity unavailable: %v", err)
	}
	return started, bootID
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

func TestFixedLeaseIDCapabilityRequiresExplicitOptIn(t *testing.T) {
	cfg := testConfig()
	backend := &leaseBackend{cfg: cfg}
	if backend.SupportsRequestedLeaseID() || (Provider{}).SupportsControllerFixedLeaseID(cfg) {
		t.Fatal("external protocol v1 must not advertise idempotent fixed lease IDs by default")
	}
	baseScope := externalClaimScope(cfg)
	cfg.External.Capabilities.IdempotentLeaseID = true
	backend.cfg = cfg
	if !backend.SupportsRequestedLeaseID() || !(Provider{}).SupportsControllerFixedLeaseID(cfg) {
		t.Fatal("explicit idempotent fixed lease ID capability was ignored")
	}
	if externalClaimScope(cfg) == baseScope {
		t.Fatal("provider routing scope did not bind the fixed lease ID capability")
	}
	cfg.External.Command = ""
	cfg.External.Lifecycle = core.ExternalLifecycleConfig{
		Acquire: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "new", "{{name}}"}},
		List: core.ExternalLifecycleOperation{
			Argv: []string{"devboxctl", "list"}, Output: lifecycleOutputJSONNameArray,
		},
		Release: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "rm", "{{name}}"}},
	}
	cfg.External.Connection = core.ExternalConnectionConfig{SSH: core.ExternalSSHConnectionConfig{User: "developer"}}
	if (Provider{}).SupportsControllerFixedLeaseID(cfg) {
		t.Fatal("declarative lifecycle advertised controller fixed-ID support without raw release identity attestation")
	}
	cfg.External.Lifecycle.Acquire.Output = lifecycleOutputJSONLease
	if (Provider{}).SupportsControllerFixedLeaseID(cfg) {
		t.Fatal("declarative lifecycle advertised controller fixed-ID support without a raw resolver")
	}
	cfg.External.Lifecycle.Resolve = core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "inspect", "{{name}}"}}
	if (Provider{}).SupportsControllerFixedLeaseID(cfg) {
		t.Fatal("declarative lifecycle advertised controller fixed-ID support when resolve output was synthesized")
	}
	cfg.External.Lifecycle.Resolve.Output = lifecycleOutputJSONLease
	if (Provider{}).SupportsControllerFixedLeaseID(cfg) {
		t.Fatal("declarative lifecycle advertised controller fixed-ID support when release was not bound to raw cloudId")
	}
	cfg.External.Lifecycle.Release.Argv = []string{"devboxctl", "rm", "--id={{cloudId}}"}
	if (Provider{}).SupportsControllerFixedLeaseID(cfg) {
		t.Fatal("embedded cloudId placeholder counted as an exact destructive target argument")
	}
	cfg.External.Lifecycle.Release.Argv = nil
	cfg.External.Lifecycle.Release.Steps = [][]string{
		{"devboxctl", "verify", "{{cloudId}}"},
		{"devboxctl", "rm", "{{name}}"},
	}
	if (Provider{}).SupportsControllerFixedLeaseID(cfg) {
		t.Fatal("one cloudId-bound release step qualified an unbound destructive step")
	}
	cfg.External.Lifecycle.Release.Steps = nil
	cfg.External.Lifecycle.Release.Argv = []string{"devboxctl", "rm", "{{cloudId}}"}
	if (Provider{}).SupportsControllerFixedLeaseID(cfg) {
		t.Fatal("synthesized name inventory qualified for controller absence reconciliation")
	}
	cfg.External.Lifecycle.List.Output = lifecycleOutputJSONLeaseArray
	if !(Provider{}).SupportsControllerFixedLeaseID(cfg) {
		t.Fatal("declarative lifecycle complete raw identity contract was ignored")
	}
}

func TestControllerIdentityContractRequiresValidConfiguredInventory(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.External = core.ExternalConfig{
		Capabilities: core.ExternalCapabilitiesConfig{IdempotentLeaseID: true},
		Lifecycle: core.ExternalLifecycleConfig{
			Acquire: core.ExternalLifecycleOperation{
				Argv: []string{"devboxctl", "new", "{{name}}"}, Output: lifecycleOutputJSONLease,
			},
			Resolve: core.ExternalLifecycleOperation{
				Argv: []string{"devboxctl", "inspect", "{{name}}"}, Output: lifecycleOutputJSONLease,
			},
			List:    core.ExternalLifecycleOperation{Output: lifecycleOutputJSONLeaseArray},
			Release: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "rm", "{{cloudId}}"}},
		},
		Connection: core.ExternalConnectionConfig{SSH: core.ExternalSSHConnectionConfig{User: "developer"}},
	}
	if (Provider{}).SupportsControllerFixedLeaseID(cfg) {
		t.Fatal("unconfigured inventory command qualified for controller fixed-ID support")
	}
	if err := (Provider{}).ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "list.argv or steps is required") {
		t.Fatalf("validation error=%v", err)
	}
}

func TestControllerProviderScopeFailsClosedForUnencodableConfig(t *testing.T) {
	cfg := testConfig()
	cfg.External.Config = map[string]any{"invalid": make(chan struct{})}
	if scope, err := (Provider{}).ControllerProviderScope(cfg); err == nil || scope != "" {
		t.Fatalf("scope=%q err=%v", scope, err)
	}
}

func TestFixedLeaseIDCapabilityFlag(t *testing.T) {
	cfg := testConfig()
	fs := flag.NewFlagSet("external", flag.ContinueOnError)
	values := registerFlags(fs, cfg)
	if err := fs.Parse([]string{"--external-idempotent-lease-id=true"}); err != nil {
		t.Fatal(err)
	}
	if err := applyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if !cfg.External.Capabilities.IdempotentLeaseID {
		t.Fatal("fixed lease ID capability flag was ignored")
	}
}

func TestProtocolControllerListRequiresCompleteRawIdentity(t *testing.T) {
	valid := protocolLease{
		LeaseID: "cbx_abcdef123456", Slug: "fast-coral", Name: "crabbox-fast-coral-deadbeef", CloudID: "provider/resource-1",
	}
	for name, mutate := range map[string]func(*protocolLease){
		"leaseId": func(lease *protocolLease) { lease.LeaseID = "" },
		"slug":    func(lease *protocolLease) { lease.Slug = "" },
		"name":    func(lease *protocolLease) { lease.Name = "" },
		"cloudId": func(lease *protocolLease) { lease.CloudID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			lease := valid
			mutate(&lease)
			response, err := json.Marshal(protocolResponse{ProtocolVersion: protocolVersion, Leases: []protocolLease{lease}})
			if err != nil {
				t.Fatal(err)
			}
			cfg := testConfig()
			cfg.External.Capabilities.IdempotentLeaseID = true
			backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Exec: &sequenceRunner{responses: []string{string(response)}}}}
			_, err = backend.invokeProtocol(context.Background(), protocolRequest{Operation: "list"})
			if err == nil || !strings.Contains(err.Error(), "missing raw "+name) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestProtocolControllerListRejectsNullButAcceptsEmptyArray(t *testing.T) {
	cfg := testConfig()
	cfg.External.Capabilities.IdempotentLeaseID = true
	runner := &sequenceRunner{responses: []string{
		`{"protocolVersion":1,"leases":null}`,
		`{"protocolVersion":1,"leases":[]}`,
	}}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Exec: runner}}
	if _, err := backend.invokeProtocol(context.Background(), protocolRequest{Operation: "list"}); err == nil || !strings.Contains(err.Error(), "requires a JSON lease array") {
		t.Fatalf("null inventory error=%v", err)
	}
	response, err := backend.invokeProtocol(context.Background(), protocolRequest{Operation: "list"})
	if err != nil || response.Leases == nil || len(response.Leases) != 0 {
		t.Fatalf("empty inventory response=%#v error=%v", response, err)
	}
}

func TestProtocolLegacyListRetainsPartialInventoryCompatibility(t *testing.T) {
	runner := &sequenceRunner{responses: []string{`{"protocolVersion":1,"leases":[{"name":"legacy-resource"}]}`}}
	backend := &leaseBackend{cfg: testConfig(), rt: core.Runtime{Exec: runner}}
	response, err := backend.invokeProtocol(context.Background(), protocolRequest{Operation: "list"})
	if err != nil || len(response.Leases) != 1 || response.Leases[0].Name != "legacy-resource" {
		t.Fatalf("legacy response=%#v error=%v", response, err)
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

func TestInvokeRejectsOversizedProviderOutputBeforeJSONDecode(t *testing.T) {
	runner := &recordingRunner{stdout: strings.Repeat("x", externalProviderOutputMaxBytes+1)}
	backend := &leaseBackend{cfg: testConfig(), rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	if _, err := backend.invoke(context.Background(), protocolRequest{Operation: "list"}); err == nil || !strings.Contains(err.Error(), "exceeded 1048576-byte output limit") {
		t.Fatalf("oversized provider output error=%v", err)
	}
	if len(runner.requests) != 1 || runner.requests[0].MaxCapturedOutputBytes != externalProviderOutputMaxBytes {
		t.Fatalf("provider command request=%#v", runner.requests)
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
	if len(runner.requests) != 1 || runner.requests[0].MaxCapturedOutputBytes != externalProviderOutputMaxBytes {
		t.Fatalf("lifecycle command request=%#v", runner.requests)
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

func TestInvokeDeclarativeLifecycleParsesRawLease(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.External = core.ExternalConfig{
		Lifecycle: core.ExternalLifecycleConfig{
			Acquire: core.ExternalLifecycleOperation{
				Argv:   []string{"devboxctl", "new", "{{name}}"},
				Output: lifecycleOutputJSONLease,
			},
		},
	}
	runner := &recordingRunner{stdout: `{
		"leaseId":"cbx_abcdef123456",
		"slug":"fast-coral",
		"name":"crabbox-fast-coral-deadbeef",
		"cloudId":"provider/resource-123",
		"status":"ready",
		"ssh":{"user":"developer","host":"resource-123","port":"22"}
	}`}
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
	if response.SynthesizedIdentity || response.Lease == nil || response.Lease.CloudID != "provider/resource-123" ||
		response.Lease.SSH == nil || response.Lease.SSH.Host != "resource-123" {
		t.Fatalf("response=%#v", response)
	}
}

func TestInvokeDeclarativeLifecycleRawLeaseRequiresCompleteIdentity(t *testing.T) {
	for name, output := range map[string]string{
		"leaseId": `{"slug":"fast-coral","name":"box","cloudId":"provider/resource"}`,
		"slug":    `{"leaseId":"cbx_abcdef123456","name":"box","cloudId":"provider/resource"}`,
		"name":    `{"leaseId":"cbx_abcdef123456","slug":"fast-coral","cloudId":"provider/resource"}`,
		"cloudId": `{"leaseId":"cbx_abcdef123456","slug":"fast-coral","name":"box"}`,
	} {
		t.Run(name, func(t *testing.T) {
			cfg := core.BaseConfig()
			cfg.External.Lifecycle.Acquire = core.ExternalLifecycleOperation{
				Argv: []string{"devboxctl", "new", "box"}, Output: lifecycleOutputJSONLease,
			}
			runner := &recordingRunner{stdout: output}
			backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
			_, err := backend.invokeLifecycle(context.Background(), protocolRequest{Operation: "acquire"})
			if err == nil || !strings.Contains(err.Error(), "missing raw "+name) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestInvokeDeclarativeLifecycleRawLeaseRejectsRoutingLabels(t *testing.T) {
	for _, key := range []string{"lease", "slug", "name", externalResourceNameLabel, externalResourceNameFromEnv} {
		t.Run(key, func(t *testing.T) {
			cfg := core.BaseConfig()
			cfg.External.Lifecycle.Acquire = core.ExternalLifecycleOperation{
				Argv: []string{"devboxctl", "new", "box"}, Output: lifecycleOutputJSONLease,
			}
			lease := protocolLease{
				LeaseID: "cbx_abcdef123456", Slug: "fast-coral", Name: "box",
				CloudID: "provider/resource", Labels: map[string]string{key: "untrusted-target"},
			}
			output, err := json.Marshal(lease)
			if err != nil {
				t.Fatal(err)
			}
			runner := &recordingRunner{stdout: string(output)}
			backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
			_, err = backend.invokeLifecycle(context.Background(), protocolRequest{Operation: "acquire"})
			if err == nil || !strings.Contains(err.Error(), "reserved routing label") {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestInvokeDeclarativeLifecycleRawLeaseRejectsInvalidIdentityText(t *testing.T) {
	valid := protocolLease{
		LeaseID: "cbx_abcdef123456", Slug: "fast-coral", Name: "box-one", CloudID: "provider/resource-1",
	}
	for name, mutate := range map[string]func(*protocolLease){
		"noncanonical leaseId": func(lease *protocolLease) { lease.LeaseID = "legacy-id" },
		"unnormalized slug":    func(lease *protocolLease) { lease.Slug = "Fast_Coral" },
		"whitespace name":      func(lease *protocolLease) { lease.Name = " box-one" },
		"control name":         func(lease *protocolLease) { lease.Name = "box-one\ninjected" },
		"oversized cloudId":    func(lease *protocolLease) { lease.CloudID = strings.Repeat("x", 4097) },
	} {
		t.Run(name, func(t *testing.T) {
			lease := valid
			mutate(&lease)
			output, err := json.Marshal(lease)
			if err != nil {
				t.Fatal(err)
			}
			cfg := core.BaseConfig()
			cfg.External.Lifecycle.Acquire = core.ExternalLifecycleOperation{
				Argv: []string{"devboxctl", "new", "box"}, Output: lifecycleOutputJSONLease,
			}
			backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: &recordingRunner{stdout: string(output)}}}
			_, err = backend.invokeLifecycle(context.Background(), protocolRequest{Operation: "acquire"})
			if err == nil || !strings.Contains(err.Error(), "has invalid raw") {
				t.Fatalf("error=%v", err)
			}
			if strings.Contains(err.Error(), "injected") || strings.Contains(err.Error(), strings.Repeat("x", 100)) {
				t.Fatalf("raw invalid identity leaked through error: %v", err)
			}
		})
	}
}

func TestInvokeDeclarativeLifecycleParsesCompleteRawLeaseInventory(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.External.Lifecycle.List = core.ExternalLifecycleOperation{
		Argv: []string{"devboxctl", "list"}, Output: lifecycleOutputJSONLeaseArray,
	}
	runner := &recordingRunner{stdout: `[
		{"leaseId":"cbx_abcdef123456","slug":"fast-coral","name":"box-one","cloudId":"provider/resource-1"},
		{"leaseId":"cbx_abcdef123457","slug":"slow-coral","name":"box-two","cloudId":"provider/resource-2"}
	]`}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	response, err := backend.invokeLifecycle(context.Background(), protocolRequest{Operation: "list"})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Leases) != 2 || response.Leases[1].CloudID != "provider/resource-2" {
		t.Fatalf("response=%#v", response)
	}
}

func TestInvokeDeclarativeLifecycleArrayOutputRejectsNullAndAcceptsEmpty(t *testing.T) {
	for name, output := range map[string]string{
		"name":  lifecycleOutputJSONNameArray,
		"lease": lifecycleOutputJSONLeaseArray,
	} {
		t.Run(name, func(t *testing.T) {
			cfg := core.BaseConfig()
			cfg.External.Lifecycle.List = core.ExternalLifecycleOperation{
				Argv: []string{"devboxctl", "list"}, Output: output,
			}
			backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: &recordingRunner{stdout: "null"}}}
			if _, err := backend.invokeLifecycle(context.Background(), protocolRequest{Operation: "list"}); err == nil || !strings.Contains(err.Error(), "returned null") {
				t.Fatalf("null error=%v", err)
			}
			backend.rt.Exec = &recordingRunner{stdout: "[]"}
			response, err := backend.invokeLifecycle(context.Background(), protocolRequest{Operation: "list"})
			if err != nil || len(response.Leases) != 0 {
				t.Fatalf("empty response=%#v error=%v", response, err)
			}
		})
	}
}

func TestInvokeDeclarativeLifecyclePreservesPartialLegacyLeaseInventory(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.External.Lifecycle.List = core.ExternalLifecycleOperation{
		Argv: []string{"devboxctl", "list"}, Output: lifecycleOutputJSONLeaseArray,
	}
	runner := &recordingRunner{stdout: `[{"leaseId":"cbx_abcdef123456","slug":"fast-coral","name":"box-one"}]`}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	response, err := backend.invokeLifecycle(context.Background(), protocolRequest{Operation: "list"})
	if err != nil || len(response.Leases) != 1 || response.Leases[0].CloudID != "" {
		t.Fatalf("response=%#v error=%v", response, err)
	}
}

func TestInvokeDeclarativeLifecycleRejectsIncompleteControllerRawLeaseInventory(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.External = core.ExternalConfig{
		Capabilities: core.ExternalCapabilitiesConfig{IdempotentLeaseID: true},
		Lifecycle: core.ExternalLifecycleConfig{
			Acquire: core.ExternalLifecycleOperation{
				Argv: []string{"devboxctl", "new", "{{name}}"}, Output: lifecycleOutputJSONLease,
			},
			Resolve: core.ExternalLifecycleOperation{
				Argv: []string{"devboxctl", "inspect", "{{name}}"}, Output: lifecycleOutputJSONLease,
			},
			List: core.ExternalLifecycleOperation{
				Argv: []string{"devboxctl", "list"}, Output: lifecycleOutputJSONLeaseArray,
			},
			Release: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "rm", "{{cloudId}}"}},
		},
	}
	runner := &recordingRunner{stdout: `[{"leaseId":"cbx_abcdef123456","slug":"fast-coral","name":"box-one"}]`}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	_, err := backend.invokeLifecycle(context.Background(), protocolRequest{Operation: "list"})
	if err == nil || !strings.Contains(err.Error(), "list lease 1 JSON lease is missing raw cloudId") {
		t.Fatalf("error=%v", err)
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

func TestConfirmedAbsentLocalCleanupRemovesMatchingRoutingAndSlugReservation(t *testing.T) {
	isolateCrabboxState(t)
	cfg := testConfig()
	backend := &leaseBackend{cfg: cfg}
	leaseID := "cbx_abcdef123456"
	slug := "fast-coral"
	routingPath, err := core.PersistExternalRouting(leaseID, cfg.External)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := backend.slugReservationDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	reservationPath := slugReservationPath(dir, slug)
	record := slugReservationRecord{
		LeaseID: leaseID, Slug: slug, Token: "crashed-owner", PID: 1 << 30,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reservationPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	expected := core.ProviderIdentityExpectation{
		LeaseID: leaseID, AttemptLeaseID: leaseID, Slug: slug, ResourceID: "provider/resource",
	}
	if err := backend.CleanupConfirmedAbsentLocalState(context.Background(), core.ConfirmedAbsentLocalCleanupRequest{
		ExpectedProviderIdentity: expected,
		ProviderScope:            backend.claimScope(),
	}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{routingPath, reservationPath} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("local sidecar still exists at %s: %v", path, err)
		}
	}
}

func TestConfirmedAbsentLocalCleanupPreservesChangedSlugReservationAndRouting(t *testing.T) {
	isolateCrabboxState(t)
	cfg := testConfig()
	backend := &leaseBackend{cfg: cfg}
	leaseID := "cbx_abcdef123456"
	slug := "fast-coral"
	routingPath, err := core.PersistExternalRouting(leaseID, cfg.External)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := backend.slugReservationDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	reservationPath := slugReservationPath(dir, slug)
	record := slugReservationRecord{
		LeaseID: "cbx_replacement123", Slug: slug, Token: "replacement-owner",
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reservationPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	err = backend.CleanupConfirmedAbsentLocalState(context.Background(), core.ConfirmedAbsentLocalCleanupRequest{
		ExpectedProviderIdentity: core.ProviderIdentityExpectation{
			LeaseID: leaseID, AttemptLeaseID: leaseID, Slug: slug, ResourceID: "provider/resource",
		},
		ProviderScope: backend.claimScope(),
	})
	if err == nil || !strings.Contains(err.Error(), "reservation identity changed") {
		t.Fatalf("cleanup error=%v", err)
	}
	for _, path := range []string{routingPath, reservationPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("changed local sidecar removed at %s: %v", path, err)
		}
	}
}

func TestConfirmedAbsentLocalCleanupPreservesReplacedRouting(t *testing.T) {
	isolateCrabboxState(t)
	cfg := testConfig()
	backend := &leaseBackend{cfg: cfg}
	leaseID := "cbx_abcdef123456"
	replacement := cfg.External
	replacement.WorkRoot = "/home/replacement/crabbox"
	routingPath, err := core.PersistExternalRouting(leaseID, replacement)
	if err != nil {
		t.Fatal(err)
	}
	err = backend.CleanupConfirmedAbsentLocalState(context.Background(), core.ConfirmedAbsentLocalCleanupRequest{
		ExpectedProviderIdentity: core.ProviderIdentityExpectation{
			LeaseID: leaseID, AttemptLeaseID: leaseID, Slug: "fast-coral", ResourceID: "provider/resource",
		},
		ProviderScope: backend.claimScope(),
	})
	if err == nil || !strings.Contains(err.Error(), "routing state changed") {
		t.Fatalf("cleanup error=%v", err)
	}
	if _, err := os.Stat(routingPath); err != nil {
		t.Fatalf("replacement routing removed: %v", err)
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
	if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), `must be "json-lease"`) {
		t.Fatalf("non-list output err=%v", err)
	}
	cfg.External.Lifecycle.Acquire.Output = lifecycleOutputJSONLease
	cfg.External.Lifecycle.Resolve.Output = lifecycleOutputJSONLease
	if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "resolve.output requires argv or steps") {
		t.Fatalf("output without command err=%v", err)
	}
	cfg.External.Lifecycle.Acquire.Output = ""
	cfg.External.Lifecycle.Resolve.Output = ""
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
	if err == nil || slug != "" || reservation != nil {
		t.Fatalf("fixed current-scope collision slug=%q reservation=%#v err=%v", slug, reservation, err)
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
	if err == nil || second != "" || secondReservation != nil {
		t.Fatalf("fixed reserved collision slug=%q reservation=%#v err=%v", second, secondReservation, err)
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

func TestAllocateLeaseSlugReclaimsFreshSameAttemptReservationAfterCrash(t *testing.T) {
	isolateCrabboxState(t)
	backend := &leaseBackend{cfg: testConfig()}
	dir, err := backend.slugReservationDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	record := slugReservationRecord{
		LeaseID:        "cbx_123456789abc",
		Slug:           "stable-attempt",
		CreatedAt:      time.Now().UTC().Format(time.RFC3339Nano),
		Token:          "same-attempt-owner",
		PID:            1 << 30,
		ProcessStarted: "crashed-owner",
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(slugReservationPath(dir, record.Slug), data, 0o600); err != nil {
		t.Fatal(err)
	}
	slug, reservation, err := backend.allocateLeaseSlug(record.LeaseID, record.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if reservation != nil {
		defer reservation.Release()
	}
	if slug != record.Slug {
		t.Fatalf("same attempt slug=%q want=%q", slug, record.Slug)
	}
}

func TestAllocateLeaseSlugPreservesActiveSameAttemptReservation(t *testing.T) {
	isolateCrabboxState(t)
	started, bootID := currentSlugReservationOwnerIdentity(t)
	backend := &leaseBackend{cfg: testConfig()}
	dir, err := backend.slugReservationDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	record := slugReservationRecord{
		LeaseID:        "cbx_123456789abc",
		Slug:           "stable-attempt",
		CreatedAt:      time.Now().UTC().Format(time.RFC3339Nano),
		Token:          "active-attempt-owner",
		PID:            os.Getpid(),
		ProcessStarted: started,
		BootID:         bootID,
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	path := slugReservationPath(dir, record.Slug)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	slug, reservation, err := backend.allocateLeaseSlug(record.LeaseID, record.Slug)
	if err == nil || slug != "" || reservation != nil || !strings.Contains(err.Error(), "still owns slug") {
		t.Fatalf("active same-attempt slug=%q reservation=%#v err=%v", slug, reservation, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("active same-attempt reservation was removed: %v", err)
	}
}

func TestAllocateLeaseSlugReclaimsSameAttemptAfterPIDReuse(t *testing.T) {
	isolateCrabboxState(t)
	started, bootID := currentSlugReservationOwnerIdentity(t)
	backend := &leaseBackend{cfg: testConfig()}
	dir, err := backend.slugReservationDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	record := slugReservationRecord{
		LeaseID:        "cbx_123456789abc",
		Slug:           "stable-attempt",
		CreatedAt:      time.Now().UTC().Format(time.RFC3339Nano),
		Token:          "crashed-attempt-owner",
		PID:            os.Getpid(),
		ProcessStarted: started + "-previous",
		BootID:         bootID,
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(slugReservationPath(dir, record.Slug), data, 0o600); err != nil {
		t.Fatal(err)
	}
	slug, reservation, err := backend.allocateLeaseSlug(record.LeaseID, record.Slug)
	if err != nil {
		t.Fatal(err)
	}
	defer reservation.Release()
	if slug != record.Slug {
		t.Fatalf("reused-PID same attempt slug=%q want=%q", slug, record.Slug)
	}
}

func TestSlugReservationOwnerRejectsPriorBootPIDReuse(t *testing.T) {
	if !core.LocalProcessBootIdentityRequired() {
		t.Skip("boot-bound process identity is Linux-only")
	}
	started, bootID := currentSlugReservationOwnerIdentity(t)
	replacement := byte('0')
	if bootID[0] == replacement {
		replacement = '1'
	}
	priorBootID := string(replacement) + bootID[1:]
	record := slugReservationRecord{
		LeaseID: "cbx_123456789abc", Slug: "stable-attempt", Token: "prior-boot-owner",
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), PID: os.Getpid(),
		ProcessStarted: started, BootID: priorBootID,
	}
	if slugReservationOwnerMatches(record) {
		t.Fatalf("prior-boot owner matched reused live pid: %#v", record)
	}
	record.BootID = bootID
	record.ProcessStarted = ""
	if slugReservationOwnerMatches(record) {
		t.Fatalf("Linux owner without start ticks matched live pid: %#v", record)
	}
}

func TestAllocateLeaseSlugRejectsMismatchedSameAttemptReservation(t *testing.T) {
	isolateCrabboxState(t)
	backend := &leaseBackend{cfg: testConfig()}
	dir, err := backend.slugReservationDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	record := slugReservationRecord{
		LeaseID: "cbx_123456789abc", Slug: "different-slug", Token: "owner-token",
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	path := slugReservationPath(dir, "stable-attempt")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	slug, reservation, err := backend.allocateLeaseSlug(record.LeaseID, "stable-attempt")
	if err == nil || slug != "" || reservation != nil || !strings.Contains(err.Error(), "invalid slug reservation identity") {
		t.Fatalf("mismatched same-attempt slug=%q reservation=%#v err=%v", slug, reservation, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("mismatched same-attempt reservation was removed: %v", err)
	}
}

func TestAllocateLeaseSlugPreservesActiveStaleReservation(t *testing.T) {
	isolateCrabboxState(t)
	started, bootID := currentSlugReservationOwnerIdentity(t)
	backend := &leaseBackend{cfg: testConfig()}
	dir, err := backend.slugReservationDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	active := slugReservationRecord{
		LeaseID:        "cbx_active",
		Slug:           "shared",
		CreatedAt:      time.Now().Add(-externalSlugReservationTTL - time.Minute).UTC().Format(time.RFC3339Nano),
		Token:          "active-token",
		PID:            os.Getpid(),
		ProcessStarted: started,
		BootID:         bootID,
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
	if err == nil || slug != "" || reservation != nil {
		t.Fatalf("fixed active reservation collision slug=%q reservation=%#v err=%v", slug, reservation, err)
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

func TestSlugReservationWritePersistsCurrentBootAndProcessIdentity(t *testing.T) {
	isolateCrabboxState(t)
	backend := &leaseBackend{cfg: testConfig()}
	dir, err := backend.slugReservationDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureSlugReservationDir(dir); err != nil {
		t.Fatal(err)
	}
	path := slugReservationPath(dir, "stable-attempt")
	if err := writeSlugReservation(path, "cbx_123456789abc", "stable-attempt", "owner-token"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record slugReservationRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	started, bootID := currentSlugReservationOwnerIdentity(t)
	if record.PID != os.Getpid() || record.ProcessStarted != started {
		t.Fatalf("reservation process identity=%#v", record)
	}
	if core.LocalProcessBootIdentityRequired() && (record.BootID == "" || record.BootID != bootID) {
		t.Fatalf("reservation boot identity=%#v want=%q", record, bootID)
	}
}

func TestSlugReservationWriteIsAtomicAndRecoverableAfterDirectorySyncFailure(t *testing.T) {
	isolateCrabboxState(t)
	backend := &leaseBackend{cfg: testConfig()}
	dir, err := backend.slugReservationDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureSlugReservationDir(dir); err != nil {
		t.Fatal(err)
	}
	path := slugReservationPath(dir, "stable-attempt")
	syncErr := errors.New("directory sync unavailable")
	err = writeSlugReservationWithSync(path, "cbx_stable", "stable-attempt", "first-token", func(string) error {
		return syncErr
	})
	if !errors.Is(err, syncErr) {
		t.Fatalf("write error=%v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record slugReservationRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("installed reservation was partial: %v data=%q", err, data)
	}
	if record.LeaseID != "cbx_stable" || record.Slug != "stable-attempt" || record.Token != "first-token" {
		t.Fatalf("installed reservation=%#v", record)
	}
	if record.PID != 0 || record.ProcessStarted != "" || record.BootID != "" {
		t.Fatalf("indeterminate reservation retained an active owner: %#v", record)
	}
	temps, err := filepath.Glob(filepath.Join(dir, ".slug-reservation-*.tmp"))
	if err != nil || len(temps) != 0 {
		t.Fatalf("temporary reservations=%v err=%v", temps, err)
	}
	slug, reservation, err := backend.allocateLeaseSlug("cbx_stable", "stable-attempt")
	if err != nil {
		t.Fatal(err)
	}
	defer reservation.Release()
	if slug != "stable-attempt" {
		t.Fatalf("recovered fixed slug=%q", slug)
	}
}

func TestEnsureSlugReservationDirSyncsEveryCreatedAncestor(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "crabbox", "external-slug-reservations", "scope")
	var synced []string
	if err := ensureSlugReservationDirWithSync(dir, func(path string) error {
		synced = append(synced, filepath.Clean(path))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		root,
		filepath.Join(root, "crabbox"),
		filepath.Join(root, "crabbox", "external-slug-reservations"),
		dir,
	} {
		found := false
		for _, got := range synced {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("directory durability syncs=%q missing %q", synced, want)
		}
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("reservation directory mode=%#o", info.Mode().Perm())
	}
}

func TestEnsureSlugReservationDirRetriesPreviouslyFailedAncestorSync(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "crabbox", "external-slug-reservations", "scope")
	syncErr := errors.New("root sync interrupted")
	failed := false
	err := ensureSlugReservationDirWithSync(dir, func(path string) error {
		if filepath.Clean(path) == root && !failed {
			failed = true
			return syncErr
		}
		return nil
	})
	if !errors.Is(err, syncErr) {
		t.Fatalf("first ensure error=%v", err)
	}
	var retriedRoot bool
	if err := ensureSlugReservationDirWithSync(dir, func(path string) error {
		if filepath.Clean(path) == root {
			retriedRoot = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !retriedRoot {
		t.Fatal("retry did not repeat the previously failed ancestor sync")
	}
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

func TestAcquireUsesRequestedLeaseIdentity(t *testing.T) {
	isolateCrabboxState(t)
	runner := &sequenceRunner{responses: []string{
		`{"protocolVersion":1,"lease":{"name":"created-without-ssh"}}`,
	}}
	backend := &leaseBackend{cfg: testConfig(), rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	_, err := backend.Acquire(context.Background(), core.AcquireRequest{
		RequestedLeaseID: "cbx_123456789abc",
		RequestedSlug:    "stable-attempt",
		Keep:             true,
	})
	if err == nil {
		t.Fatal("invalid response unexpectedly succeeded")
	}
	if len(runner.requests) != 1 || runner.requests[0].Desired == nil {
		t.Fatalf("requests=%#v", runner.requests)
	}
	desired := runner.requests[0].Desired
	if desired.LeaseID != "cbx_123456789abc" || desired.Slug != "stable-attempt" || !strings.Contains(desired.Name, "stable-attempt") {
		t.Fatalf("desired=%#v", desired)
	}
}

func TestFixedKeptAcquireFailureRetainsSlugReservation(t *testing.T) {
	isolateCrabboxState(t)
	const leaseID = "cbx_123456789abc"
	const slug = "stable-attempt"
	lease := protocolLease{
		LeaseID: leaseID, Slug: slug, Name: core.LeaseProviderName(leaseID, slug), CloudID: "provider/resource-1",
	}
	response, err := json.Marshal(protocolResponse{ProtocolVersion: protocolVersion, Lease: &lease})
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.External.Capabilities.IdempotentLeaseID = true
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: &sequenceRunner{responses: []string{string(response)}}}}
	_, err = backend.Acquire(context.Background(), core.AcquireRequest{
		RequestedLeaseID: leaseID, RequestedSlug: slug, Keep: true,
	})
	if err == nil || !strings.Contains(err.Error(), "SSH host and user are required") {
		t.Fatalf("error=%v", err)
	}
	dir, err := backend.slugReservationDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(slugReservationPath(dir, slug)); err != nil {
		t.Fatalf("controller-owned reservation missing after kept failure: %v", err)
	}
	if got, reservation, err := backend.allocateLeaseSlug("cbx_abcdef123456", slug); err == nil || got != "" || reservation != nil {
		t.Fatalf("duplicate slug=%q reservation=%#v err=%v", got, reservation, err)
	}
}

func TestFixedLeaseIDAcquireRejectsDivergentProviderIdentity(t *testing.T) {
	for name, response := range map[string]string{
		"slug": `{"protocolVersion":1,"lease":{"leaseId":"cbx_123456789abc","slug":"different-slug","name":"crabbox-stable-attempt-123456789abc","cloudId":"provider/resource-1"}}`,
		"name": `{"protocolVersion":1,"lease":{"leaseId":"cbx_123456789abc","slug":"stable-attempt","name":"different-name","cloudId":"provider/resource-1"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			isolateCrabboxState(t)
			runner := &sequenceRunner{responses: []string{response}}
			cfg := testConfig()
			cfg.External.Capabilities.IdempotentLeaseID = true
			backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
			_, err := backend.Acquire(context.Background(), core.AcquireRequest{
				RequestedLeaseID: "cbx_123456789abc",
				RequestedSlug:    "stable-attempt",
				Keep:             true,
			})
			if err == nil || !strings.Contains(err.Error(), "external provider lease "+name+" changed") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestProtocolFixedLeaseIDAcquireRequiresCompleteRawIdentity(t *testing.T) {
	valid := protocolLease{
		LeaseID: "cbx_123456789abc", Slug: "stable-attempt",
		Name: core.LeaseProviderName("cbx_123456789abc", "stable-attempt"), CloudID: "provider/resource-1",
	}
	for name, mutate := range map[string]func(*protocolLease){
		"leaseId": func(lease *protocolLease) { lease.LeaseID = "" },
		"slug":    func(lease *protocolLease) { lease.Slug = "" },
		"name":    func(lease *protocolLease) { lease.Name = "" },
		"cloudId": func(lease *protocolLease) { lease.CloudID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			isolateCrabboxState(t)
			lease := valid
			mutate(&lease)
			response, err := json.Marshal(protocolResponse{ProtocolVersion: protocolVersion, Lease: &lease})
			if err != nil {
				t.Fatal(err)
			}
			cfg := testConfig()
			cfg.External.Capabilities.IdempotentLeaseID = true
			backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: &sequenceRunner{responses: []string{string(response)}}}}
			_, err = backend.Acquire(context.Background(), core.AcquireRequest{
				RequestedLeaseID: valid.LeaseID, RequestedSlug: valid.Slug, Keep: true,
			})
			if err == nil || !strings.Contains(err.Error(), "missing raw "+name) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestAcquireAcknowledgesRawIdentityBeforeSidecarsAndRollsBackKeptLease(t *testing.T) {
	isolateCrabboxState(t)
	const leaseID = "cbx_123456789abc"
	const slug = "stable-attempt"
	lease := protocolLease{
		LeaseID: leaseID, Slug: slug, Name: core.LeaseProviderName(leaseID, slug), CloudID: "provider/resource-1",
		SSH: &protocolSSH{Host: "127.0.0.1", User: "tester", Port: "1"},
	}
	response, err := json.Marshal(protocolResponse{ProtocolVersion: protocolVersion, Lease: &lease})
	if err != nil {
		t.Fatal(err)
	}
	runner := &sequenceRunner{responses: []string{string(response), `{"protocolVersion":1}`}}
	cfg := testConfig()
	cfg.External.Capabilities.IdempotentLeaseID = true
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	ackErr := errors.New("controller state unavailable")
	_, err = backend.Acquire(context.Background(), core.AcquireRequest{
		RequestedLeaseID: leaseID, RequestedSlug: slug, Keep: true,
		OnAcquired: func(acquired core.LeaseTarget) error {
			if acquired.LeaseID != leaseID || acquired.Server.CloudID != lease.CloudID || acquired.Server.Labels["slug"] != slug {
				return fmt.Errorf("raw identity=%#v", acquired)
			}
			path, pathErr := core.ExternalRoutingPath(leaseID)
			if pathErr != nil {
				return pathErr
			}
			if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
				return fmt.Errorf("external routing existed before acquire acknowledgment: %v", statErr)
			}
			return ackErr
		},
	})
	if !errors.Is(err, ackErr) || !strings.Contains(err.Error(), "acknowledge raw external provider acquisition") {
		t.Fatalf("error=%v", err)
	}
	if len(runner.operations) != 2 || runner.operations[0] != "acquire" || runner.operations[1] != "release" {
		t.Fatalf("operations=%#v", runner.operations)
	}
	if !runner.releaseHasDeadline {
		t.Fatal("callback rejection rollback did not receive a bounded detached deadline")
	}
	dir, err := backend.slugReservationDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(slugReservationPath(dir, slug)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reservation remained after successful callback-rejection rollback: %v", err)
	}
}

func TestAcquireAcknowledgesRawIdentityBeforeSSHValidation(t *testing.T) {
	isolateCrabboxState(t)
	const leaseID = "cbx_123456789abc"
	const slug = "stable-attempt"
	lease := protocolLease{
		LeaseID: leaseID, Slug: slug, Name: core.LeaseProviderName(leaseID, slug), CloudID: "provider/resource-1",
	}
	response, err := json.Marshal(protocolResponse{ProtocolVersion: protocolVersion, Lease: &lease})
	if err != nil {
		t.Fatal(err)
	}
	runner := &sequenceRunner{responses: []string{string(response)}}
	cfg := testConfig()
	cfg.External.Capabilities.IdempotentLeaseID = true
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	acknowledged := false
	_, err = backend.Acquire(context.Background(), core.AcquireRequest{
		RequestedLeaseID: leaseID, RequestedSlug: slug, Keep: true,
		OnAcquired: func(acquired core.LeaseTarget) error {
			acknowledged = acquired.LeaseID == leaseID && acquired.Server.CloudID == lease.CloudID
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "SSH host and user are required") {
		t.Fatalf("error=%v", err)
	}
	if !acknowledged {
		t.Fatal("raw provider identity was not acknowledged before SSH validation")
	}
	if len(runner.operations) != 1 || runner.operations[0] != "acquire" {
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

func TestReleaseOnlyResolveWithoutClaimRejectsWrongExpectedResource(t *testing.T) {
	isolateCrabboxState(t)
	runner := &sequenceRunner{responses: []string{
		`{"protocolVersion":1,"lease":{"leaseId":"cbx_expected123","slug":"fast-coral","cloudId":"provider/wrong"}}`,
	}}
	backend := &leaseBackend{cfg: testConfig(), rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	expected := core.ProviderIdentityExpectation{
		LeaseID:        "cbx_expected123",
		AttemptLeaseID: "cbx_expected123",
		Slug:           "fast-coral",
		ResourceID:     "provider/expected",
	}
	_, err := backend.Resolve(context.Background(), core.ResolveRequest{
		ID:                       "cbx_expected123",
		ReleaseOnly:              true,
		ExpectedProviderIdentity: expected,
	})
	if err == nil || !strings.Contains(err.Error(), "resource ID mismatch before release") {
		t.Fatalf("wrong release-only resource error=%v", err)
	}
	if len(runner.operations) != 1 || runner.operations[0] != "resolve" {
		t.Fatalf("wrong release-only resource reached release: %#v", runner.operations)
	}
	request := runner.requests[0]
	if request.Expected == nil || request.Expected.LeaseID != expected.LeaseID ||
		request.Expected.AttemptLeaseID != expected.AttemptLeaseID || request.Expected.Slug != expected.Slug ||
		request.Expected.CloudID != expected.ResourceID {
		t.Fatalf("resolve did not receive full expected provider identity: %#v", request.Expected)
	}
	if request.Desired == nil || request.Desired.LeaseID != expected.LeaseID || request.Desired.Slug != expected.Slug {
		t.Fatalf("release-only resolve did not receive fixed desired identity: %#v", request.Desired)
	}
}

func TestReleaseOnlyResolveRequiresRawExpectedIdentity(t *testing.T) {
	expected := core.ProviderIdentityExpectation{
		LeaseID:        "cbx_expected123",
		AttemptLeaseID: "cbx_expected123",
		Slug:           "fast-coral",
		ResourceID:     "provider/expected",
	}
	for name, response := range map[string]string{
		"lease ID": `{"protocolVersion":1,"lease":{"slug":"fast-coral","cloudId":"provider/expected"}}`,
		"slug":     `{"protocolVersion":1,"lease":{"leaseId":"cbx_expected123","cloudId":"provider/expected"}}`,
		"resource": `{"protocolVersion":1,"lease":{"leaseId":"cbx_expected123","slug":"fast-coral"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			isolateCrabboxState(t)
			runner := &sequenceRunner{responses: []string{response}}
			backend := &leaseBackend{cfg: testConfig(), rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
			_, err := backend.Resolve(context.Background(), core.ResolveRequest{
				ID:                       expected.LeaseID,
				ReleaseOnly:              true,
				ExpectedProviderIdentity: expected,
			})
			if err == nil || !strings.Contains(err.Error(), "mismatch before release") || !strings.Contains(err.Error(), "<empty>") {
				t.Fatalf("missing raw %s error=%v", name, err)
			}
		})
	}
}

func TestReleaseOnlyResolveAcceptsRawExpectedIdentity(t *testing.T) {
	isolateCrabboxState(t)
	runner := &sequenceRunner{responses: []string{
		`{"protocolVersion":1,"lease":{"leaseId":"cbx_expected123","slug":"fast-coral","cloudId":"provider/expected"}}`,
	}}
	backend := &leaseBackend{cfg: testConfig(), rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	expected := core.ProviderIdentityExpectation{
		LeaseID:        "cbx_expected123",
		AttemptLeaseID: "cbx_expected123",
		Slug:           "fast-coral",
		ResourceID:     "provider/expected",
	}
	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{
		ID:                       expected.LeaseID,
		ReleaseOnly:              true,
		ExpectedProviderIdentity: expected,
	})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != expected.LeaseID || lease.Server.Labels["slug"] != expected.Slug || lease.Server.DisplayID() != expected.ResourceID {
		t.Fatalf("lease=%#v", lease)
	}
}

func TestExternalReleaseRevalidatesExpectedProviderIdentity(t *testing.T) {
	isolateCrabboxState(t)
	runner := &sequenceRunner{responses: []string{`{"protocolVersion":1}`}}
	backend := &leaseBackend{cfg: testConfig(), rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	lease := core.LeaseTarget{
		LeaseID: "cbx_other123",
		Server: core.Server{CloudID: "provider/expected", Labels: map[string]string{
			"slug": "fast-coral",
		}},
	}
	err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{
		Lease: lease,
		ExpectedProviderIdentity: core.ProviderIdentityExpectation{
			LeaseID: "cbx_expected123", AttemptLeaseID: "cbx_expected123",
			Slug: "fast-coral", ResourceID: "provider/expected",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "lease ID mismatch before release") {
		t.Fatalf("release identity error=%v", err)
	}
	if len(runner.operations) != 0 {
		t.Fatalf("release adapter invoked with mismatched identity: %#v", runner.operations)
	}
	lease.LeaseID = "cbx_expected123"
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{
		Lease: lease,
		ExpectedProviderIdentity: core.ProviderIdentityExpectation{
			LeaseID: "cbx_expected123", AttemptLeaseID: "cbx_expected123",
			Slug: "fast-coral", ResourceID: "provider/expected",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if len(runner.requests) != 1 || runner.requests[0].Expected == nil || runner.requests[0].Expected.CloudID != "provider/expected" {
		t.Fatalf("release adapter did not receive full expected identity: %#v", runner.requests)
	}
}

func TestIdentityBoundExternalReleasePreservesRecoveryState(t *testing.T) {
	isolateCrabboxState(t)
	cfg := testConfig()
	leaseID := "cbx_expected123"
	slug := "fast-coral"
	resourceID := "provider/expected"
	claimExternalLease(t, cfg, leaseID, slug, t.TempDir(), time.Minute, false)
	server := core.Server{CloudID: resourceID, Labels: map[string]string{"slug": slug}}
	if err := core.UpdateLeaseClaimEndpoint(leaseID, server, core.SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	routingPath, err := core.PersistExternalRouting(leaseID, cfg.External)
	if err != nil {
		t.Fatal(err)
	}
	runner := &sequenceRunner{responses: []string{`{"protocolVersion":1}`}}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	expected := core.ProviderIdentityExpectation{
		LeaseID: leaseID, AttemptLeaseID: leaseID, Slug: slug, ResourceID: resourceID,
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{
		Lease: core.LeaseTarget{LeaseID: leaseID, Server: server}, ExpectedProviderIdentity: expected,
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider(leaseID, providerName); err != nil || !ok {
		t.Fatalf("claim retained=%t err=%v", ok, err)
	}
	if _, err := os.Stat(routingPath); err != nil {
		t.Fatalf("routing state was removed before confirmed absence: %v", err)
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

func TestIdentityBoundReadOnlyResolveDoesNotPersistRouting(t *testing.T) {
	isolateCrabboxState(t)
	leaseID := "cbx_abcdef123456"
	runner := &sequenceRunner{responses: []string{
		`{"protocolVersion":1,"lease":{"leaseId":"cbx_abcdef123456","slug":"shared","name":"devbox-shared","cloudId":"provider/expected","ssh":{"host":"127.0.0.1","user":"tester","port":"1"}}}`,
	}}
	backend := &leaseBackend{cfg: testConfig(), rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := backend.Resolve(ctx, core.ResolveRequest{
		ID: leaseID,
		ExpectedProviderIdentity: core.ProviderIdentityExpectation{
			LeaseID: leaseID, AttemptLeaseID: leaseID, Slug: "shared", ResourceID: "provider/expected",
		},
		NoLocalStateMutations: true,
	})
	if err == nil {
		t.Fatal("expected canceled SSH readiness")
	}
	path, err := core.ExternalRoutingPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only resolve persisted routing state: %v", err)
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

func TestDeclarativeReleaseOnlyResolveRejectsManufacturedExpectedIdentity(t *testing.T) {
	isolateCrabboxState(t)
	cfg := core.BaseConfig()
	cfg.External = core.ExternalConfig{
		Lifecycle: core.ExternalLifecycleConfig{
			Acquire: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "new", "{{name}}"}},
			List: core.ExternalLifecycleOperation{
				Argv: []string{"devboxctl", "list"}, Output: lifecycleOutputJSONLeaseArray,
			},
			Release: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "rm", "{{resourceName}}"}},
		},
		Connection: core.ExternalConnectionConfig{
			ResourceName: "{{leaseIdSlug}}",
			SSH:          core.ExternalSSHConnectionConfig{User: "developer"},
		},
	}
	expected := core.ProviderIdentityExpectation{
		AttemptLeaseID: "cbx_expected456", Slug: "fixed-slug", ResourceID: "provider/fixed-resource",
	}
	runner := &recordingRunner{}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	_, err := backend.Resolve(context.Background(), core.ResolveRequest{
		ID: "cbx_expected456", ReleaseOnly: true, ExpectedProviderIdentity: expected,
	})
	if err == nil || !strings.Contains(err.Error(), "requires resolver-returned provider identity") {
		t.Fatalf("manufactured expected identity error=%v", err)
	}
	if len(runner.requests) != 0 {
		t.Fatalf("declarative release ran after manufactured resolve identity: %#v", runner.requests)
	}
}

func TestDeclarativeReleaseOnlyResolveAcceptsCommandAttestedIdentity(t *testing.T) {
	isolateCrabboxState(t)
	cfg := core.BaseConfig()
	cfg.External = core.ExternalConfig{
		Capabilities: core.ExternalCapabilitiesConfig{IdempotentLeaseID: true},
		Lifecycle: core.ExternalLifecycleConfig{
			Acquire: core.ExternalLifecycleOperation{
				Argv: []string{"devboxctl", "new", "{{name}}"}, Output: lifecycleOutputJSONLease,
			},
			Resolve: core.ExternalLifecycleOperation{
				Argv: []string{"devboxctl", "inspect", "{{name}}"}, Output: lifecycleOutputJSONLease,
			},
			List: core.ExternalLifecycleOperation{
				Argv: []string{"devboxctl", "list"}, Output: lifecycleOutputJSONNameArray,
			},
			Release: core.ExternalLifecycleOperation{Argv: []string{"devboxctl", "rm", "{{cloudId}}"}},
		},
		Connection: core.ExternalConnectionConfig{SSH: core.ExternalSSHConnectionConfig{User: "developer"}},
	}
	expected := core.ProviderIdentityExpectation{
		LeaseID: "cbx_abcdef123456", AttemptLeaseID: "cbx_abcdef123456",
		Slug: "fixed-slug", ResourceID: "provider/fixed-resource",
	}
	rawLease, err := json.Marshal(protocolLease{
		LeaseID: expected.LeaseID, Slug: expected.Slug,
		Name: core.LeaseProviderName(expected.LeaseID, expected.Slug), CloudID: expected.ResourceID,
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{stdout: string(rawLease)}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{
		ID: expected.LeaseID, ReleaseOnly: true, ExpectedProviderIdentity: expected,
	})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != expected.LeaseID || lease.Server.Labels["slug"] != expected.Slug || lease.Server.DisplayID() != expected.ResourceID {
		t.Fatalf("lease=%#v", lease)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{
		Lease: lease, ExpectedProviderIdentity: expected,
	}); err != nil {
		t.Fatal(err)
	}
	if len(runner.requests) != 2 || strings.Join(runner.requests[1].Args, "|") != "rm|"+expected.ResourceID {
		t.Fatalf("release requests=%#v", runner.requests)
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
	responses          []string
	operations         []string
	requests           []protocolRequest
	releaseHasDeadline bool
}

func (r *sequenceRunner) Run(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	var request protocolRequest
	if err := json.NewDecoder(req.Stdin).Decode(&request); err != nil {
		return core.LocalCommandResult{}, err
	}
	r.operations = append(r.operations, request.Operation)
	r.requests = append(r.requests, request)
	if request.Operation == "release" {
		_, r.releaseHasDeadline = ctx.Deadline()
	}
	response := r.responses[0]
	r.responses = r.responses[1:]
	return core.LocalCommandResult{Stdout: response}, nil
}
