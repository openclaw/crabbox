package tart

import (
	"context"
	"encoding/json"
	"flag"
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

func TestPrepareLeaseSetsPublicHost(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	runner := &recordingRunner{}
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	inst := tartInstance{Name: "crabbox-blue-1234abcd", State: "running"}
	lt, err := b.prepareLease(context.Background(), cfg, inst, "192.0.2.10", core.LeaseClaim{LeaseID: "cbx_test"}, false)
	if err != nil {
		t.Fatalf("prepareLease: %v", err)
	}
	if lt.Server.PublicNet.IPv4.IP != "192.0.2.10" {
		t.Fatalf("Server.PublicNet.IPv4.IP = %q, want 192.0.2.10 (status/inspect read this)", lt.Server.PublicNet.IPv4.IP)
	}
	if lt.SSH.Host != "192.0.2.10" {
		t.Fatalf("SSH.Host = %q, want 192.0.2.10", lt.SSH.Host)
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
		commandKey([]string{"--version"}):                {Stdout: "tart 2.12.0\n"},
		commandKey([]string{"list", "--format", "json"}): {Stdout: `[]`},
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

func TestStartVMArgsHeadless(t *testing.T) {
	args := startVMArgs("crabbox-blue-1234abcd")
	if len(args) != 3 || args[0] != "run" || args[2] != "--no-graphics" {
		t.Fatalf("startVMArgs=%v want [run <name> --no-graphics]", args)
	}
}

func TestApplyFlagsRejectsExplicitLinuxTarget(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	core.MarkTargetExplicit(&cfg)

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String("target", "linux", "")
	if err := fs.Set("target", "linux"); err != nil {
		t.Fatal(err)
	}

	err := applyFlags(&cfg, fs, flagValues{})
	if err == nil {
		t.Fatal("applyFlags should reject explicit --target linux")
	}
}

func TestApplyFlagsRejectsExplicitWindowsTarget(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetWindows
	core.MarkTargetExplicit(&cfg)

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String("target", "linux", "")
	if err := fs.Set("target", "windows"); err != nil {
		t.Fatal(err)
	}

	err := applyFlags(&cfg, fs, flagValues{})
	if err == nil {
		t.Fatal("applyFlags should reject explicit --target windows")
	}
}

func TestApplyFlagsDefaultsLinuxToMacOS(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String("target", "linux", "")

	err := applyFlags(&cfg, fs, flagValues{})
	if err != nil {
		t.Fatalf("applyFlags failed: %v", err)
	}
	if cfg.TargetOS != core.TargetMacOS {
		t.Fatalf("TargetOS=%s want macos (should default baseConfig linux to macos)", cfg.TargetOS)
	}
}

func TestApplyFlagsRejectsExplicitTargetFromEnv(t *testing.T) {
	t.Setenv("CRABBOX_TARGET", "linux")
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	core.MarkTargetExplicit(&cfg)

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String("target", "linux", "")

	err := applyFlags(&cfg, fs, flagValues{})
	if err == nil {
		t.Fatal("applyFlags should reject explicit target=linux from env")
	}
}

func TestApplyFlagsRejectsExplicitTargetFromYAML(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	core.MarkTargetExplicit(&cfg)

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String("target", "linux", "")

	err := applyFlags(&cfg, fs, flagValues{})
	if err == nil {
		t.Fatal("applyFlags should reject explicit target=linux from YAML")
	}
}

func TestApplyFlagsAcceptsExplicitMacOS(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetMacOS
	core.MarkTargetExplicit(&cfg)

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String("target", "linux", "")
	if err := fs.Set("target", "macos"); err != nil {
		t.Fatal(err)
	}

	err := applyFlags(&cfg, fs, flagValues{})
	if err != nil {
		t.Fatalf("applyFlags should accept explicit --target macos: %v", err)
	}
	if cfg.TargetOS != core.TargetMacOS {
		t.Fatalf("TargetOS=%s want macos", cfg.TargetOS)
	}
}

func TestInjectSSHKeyDoesNotCallTartIP(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"exec", "crabbox-blue-1234", "bash", "-c", "mkdir -p ~/.ssh && chmod 700 ~/.ssh && echo 'ssh-ed25519 AAAA test' >> ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys"}): {},
		},
		errors: map[string]error{},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	err := b.injectSSHKey(context.Background(), "crabbox-blue-1234", "ssh-ed25519 AAAA test")
	if err != nil {
		t.Fatalf("injectSSHKey: %v", err)
	}
	for _, call := range runner.calls {
		if len(call.Args) >= 2 && call.Args[0] == "ip" {
			t.Fatal("injectSSHKey should not call tart ip; tart exec connects by VM name")
		}
	}
}

func TestGetIPFiltersDashDash(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"ip", "crabbox-test-vm"}): {Stdout: "--\n"},
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	ip := b.getIP(context.Background(), "crabbox-test-vm")
	if ip != "" {
		t.Fatalf("getIP returned %q for sentinel \"--\", want empty string", ip)
	}
}

func TestGetIPReturnsValidIP(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"ip", "crabbox-test-vm"}): {Stdout: "192.168.64.5\n"},
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	ip := b.getIP(context.Background(), "crabbox-test-vm")
	if ip != "192.168.64.5" {
		t.Fatalf("getIP=%q want 192.168.64.5", ip)
	}
}

func TestPreparLeaseRejectsDashDashIP(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	runner := &recordingRunner{}
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	inst := tartInstance{Name: "crabbox-blue-1234abcd", State: "running"}
	_, err := b.prepareLease(context.Background(), cfg, inst, "--", core.LeaseClaim{LeaseID: "cbx_test"}, false)
	if err == nil {
		t.Fatal("prepareLease should reject IP \"--\"")
	}
}

func TestResolveInstanceUsesRealState(t *testing.T) {
	listJSON := `[{"Name":"crabbox-blue-abc123","State":"stopped","Running":false,"Disk":50,"Size":12,"Source":"ghcr.io/cirruslabs/macos-sequoia-base:latest"}]`
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"list", "--format", "json"}):  {Stdout: listJSON},
			commandKey([]string{"ip", "crabbox-blue-abc123"}): {Stdout: "192.168.64.5\n"},
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	inst, ip, _, err := b.resolveInstance(context.Background(), "crabbox-blue-abc123")
	if err != nil {
		t.Fatalf("resolveInstance: %v", err)
	}
	if inst.State != "stopped" {
		t.Fatalf("resolveInstance returned State=%q, want \"stopped\" (real tart state, not fabricated)", inst.State)
	}
	if inst.Running {
		t.Fatal("resolveInstance returned Running=true for a stopped VM")
	}
	if ip != "192.168.64.5" {
		t.Fatalf("ip=%q want 192.168.64.5", ip)
	}
}

func sampleListJSON() string {
	return `[{"Name":"crabbox-blue-1234abcd","State":"running","Running":true,"Disk":50,"Size":15,"Source":"ghcr.io/cirruslabs/macos-sequoia-base:latest"},{"Name":"my-dev-vm","State":"stopped","Running":false,"Disk":50,"Size":12,"Source":"ghcr.io/cirruslabs/macos-sequoia-base:latest"}]`
}
