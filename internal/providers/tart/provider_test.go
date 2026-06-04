package tart

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type recordingRunner struct {
	calls     []core.LocalCommandRequest
	responses map[string]core.LocalCommandResult
	errors    map[string]error
}

func (r *recordingRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.calls = append(r.calls, req)
	key := commandKey(req.Args)
	if err, ok := r.errors[key]; ok {
		return r.responses[key], err
	}
	if result, ok := r.responses[key]; ok {
		return result, nil
	}
	if len(req.Args) > 0 {
		if err, ok := r.errors[req.Args[0]]; ok {
			return r.responses[req.Args[0]], err
		}
		if result, ok := r.responses[req.Args[0]]; ok {
			return result, nil
		}
	}
	return core.LocalCommandResult{}, nil
}

func commandKey(args []string) string {
	return strings.Join(args, "\x00")
}

func TestProviderSpecAndAliases(t *testing.T) {
	p := Provider{}
	if p.Name() != providerName {
		t.Fatalf("Name=%q want %s", p.Name(), providerName)
	}
	for _, alias := range []string{"tart", "local-tart", "macos-vm"} {
		got, err := core.ProviderFor(alias)
		if err != nil {
			t.Fatalf("ProviderFor(%q): %v", alias, err)
		}
		if got.Name() != providerName {
			t.Fatalf("ProviderFor(%q).Name=%q", alias, got.Name())
		}
	}
	spec := p.Spec()
	if spec.Kind != core.ProviderKindSSHLease || spec.Family != "local-vm" {
		t.Fatalf("unexpected spec: %#v", spec)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetMacOS {
		t.Fatalf("targets=%v want macos only", spec.Targets)
	}
	for _, feature := range []core.Feature{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup} {
		if !spec.Features.Has(feature) {
			t.Fatalf("features=%v missing %s", spec.Features, feature)
		}
	}
	if spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("coordinator=%s want never", spec.Coordinator)
	}
}

func TestConfigureRejectsNonMacOS(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	if _, err := (Provider{}).Configure(cfg, core.Runtime{}); err == nil {
		t.Fatal("Configure accepted non-macos target")
	}

	cfg = core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetWindows
	if _, err := (Provider{}).Configure(cfg, core.Runtime{}); err == nil {
		t.Fatal("Configure accepted windows target")
	}
}

func TestConfigureRejectsTailscale(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Tailscale.Enabled = true
	if _, err := (Provider{}).Configure(cfg, core.Runtime{}); err == nil {
		t.Fatal("Configure accepted tailscale")
	}
}

func TestConfigureAcceptsMacOS(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetMacOS
	backend, err := (Provider{}).Configure(cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: &recordingRunner{}})
	if err != nil {
		t.Fatalf("Configure failed for macos: %v", err)
	}
	if backend == nil {
		t.Fatal("Configure returned nil backend")
	}
}

func TestConfigureAcceptsEmptyTarget(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = ""
	backend, err := (Provider{}).Configure(cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: &recordingRunner{}})
	if err != nil {
		t.Fatalf("Configure failed for empty target: %v", err)
	}
	if backend == nil {
		t.Fatal("Configure returned nil backend")
	}
}

func TestApplyDefaults(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Tart = core.TartConfig{}
	applyDefaults(&cfg)
	if cfg.Tart.Image != "ghcr.io/cirruslabs/macos-sequoia-base:latest" {
		t.Fatalf("default image=%q", cfg.Tart.Image)
	}
	if cfg.Tart.User != "admin" {
		t.Fatalf("default user=%q", cfg.Tart.User)
	}
	if cfg.Tart.CPUs != 4 {
		t.Fatalf("default cpus=%d", cfg.Tart.CPUs)
	}
	if cfg.Tart.Memory != 8192 {
		t.Fatalf("default memory=%d", cfg.Tart.Memory)
	}
	if cfg.Tart.Disk != 50 {
		t.Fatalf("default disk=%d", cfg.Tart.Disk)
	}
	if cfg.SSHUser != "admin" || cfg.SSHPort != sshPort {
		t.Fatalf("derived SSH fields wrong: user=%s port=%s", cfg.SSHUser, cfg.SSHPort)
	}
}

func TestListInstancesFiltersCrabboxPrefix(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"list", "--format", "json"}): {Stdout: sampleListJSON()},
	}}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	views, err := b.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 {
		t.Fatalf("views=%d want 1", len(views))
	}
	if views[0].CloudID != "crabbox-blue-1234abcd" {
		t.Fatalf("unexpected view: %#v", views[0])
	}
}

func TestListJSONDecode(t *testing.T) {
	var instances []tartInstance
	if err := json.Unmarshal([]byte(sampleListJSON()), &instances); err != nil {
		t.Fatal(err)
	}
	if len(instances) != 2 || instances[0].Name != "crabbox-blue-1234abcd" {
		t.Fatalf("decoded=%#v", instances)
	}
}

func TestDoctorReady(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"--version"}):                  {Stdout: "tart 2.12.0\n"},
		commandKey([]string{"list", "--format", "json"}):   {Stdout: `[]`},
	}}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	res, err := b.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Provider != providerName || !strings.Contains(res.Message, "cli=ready") || !strings.Contains(res.Message, "tart 2.12.0") {
		t.Fatalf("doctor result=%#v", res)
	}
}

func TestInstanceScopeRoundTrip(t *testing.T) {
	name := "crabbox-blue-1234abcd"
	if got := instanceNameFromScope(instanceScope(name)); got != name {
		t.Fatalf("instance name=%q want %q", got, name)
	}
}

func TestShouldCleanupRespectsKeepLabel(t *testing.T) {
	server := Server{Status: "stopped", Labels: map[string]string{"keep": "true"}}
	if ok, reason := shouldCleanup(server, core.LeaseClaim{}, true, time.Now()); ok || reason != "keep=true" {
		t.Fatalf("cleanup=%v reason=%s", ok, reason)
	}
}

func TestShouldCleanupExpiredClaim(t *testing.T) {
	server := Server{Status: "running", Labels: map[string]string{}}
	claim := core.LeaseClaim{LeaseID: "cbx_123", LastUsedAt: time.Now().Add(-48 * time.Hour).Format(time.RFC3339), IdleTimeoutSeconds: int((30 * time.Minute).Seconds())}
	if ok, reason := shouldCleanup(server, claim, true, time.Now()); !ok || reason != "claim expired" {
		t.Fatalf("cleanup=%v reason=%s", ok, reason)
	}
}

func TestShouldCleanupSkipsMissingClaim(t *testing.T) {
	server := Server{Status: "running", Labels: map[string]string{}}
	if ok, reason := shouldCleanup(server, core.LeaseClaim{}, false, time.Now()); ok || reason != "missing claim" {
		t.Fatalf("cleanup=%v reason=%s", ok, reason)
	}
}

func sampleListJSON() string {
	return `[{"Name":"crabbox-blue-1234abcd","State":"Running","Disk":50,"Size":"15 GB","Source":"ghcr.io/cirruslabs/macos-sequoia-base:latest"},{"Name":"my-dev-vm","State":"Stopped","Disk":50,"Size":"12 GB","Source":"ghcr.io/cirruslabs/macos-sequoia-base:latest"}]`
}
