package tart

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
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

func TestConfigureRejectsExplicitNonMacOS(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	core.MarkTargetExplicit(&cfg)
	if _, err := (Provider{}).Configure(cfg, core.Runtime{}); err == nil {
		t.Fatal("Configure accepted explicit linux target")
	}

	cfg = core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetWindows
	core.MarkTargetExplicit(&cfg)
	if _, err := (Provider{}).Configure(cfg, core.Runtime{}); err == nil {
		t.Fatal("Configure accepted explicit windows target")
	}
}

func TestConfigureDefaultsImplicitLinuxToMacOS(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	backend, err := (Provider{}).Configure(cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: &recordingRunner{}})
	if err != nil {
		t.Fatalf("Configure rejected implicit linux target (e.g. doctor --provider tart): %v", err)
	}
	if backend == nil {
		t.Fatal("Configure returned nil backend")
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
		commandKey([]string{"list", "--source", "local", "--format", "json"}): {Stdout: sampleListJSON()},
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
		commandKey([]string{"--version"}):                                     {Stdout: "tart 2.12.0\n"},
		commandKey([]string{"list", "--source", "local", "--format", "json"}): {Stdout: `[]`},
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
			commandKey([]string{"exec", "crabbox-blue-1234", "bash", "-c", "mkdir -p ~admin/.ssh && chmod 700 ~admin/.ssh && echo 'ssh-ed25519 AAAA test' >> ~admin/.ssh/authorized_keys && chmod 600 ~admin/.ssh/authorized_keys"}): {},
		},
		errors: map[string]error{},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	err := b.injectSSHKey(context.Background(), "crabbox-blue-1234", "admin", "ssh-ed25519 AAAA test")
	if err != nil {
		t.Fatalf("injectSSHKey: %v", err)
	}
	for _, call := range runner.calls {
		if len(call.Args) >= 2 && call.Args[0] == "ip" {
			t.Fatal("injectSSHKey should not call tart ip; tart exec connects by VM name")
		}
	}
}

func TestInjectSSHKeyTargetsConfiguredUser(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"exec", "crabbox-blue-1234", "bash", "-c", "mkdir -p ~root/.ssh && chmod 700 ~root/.ssh && echo 'ssh-ed25519 AAAA test' >> ~root/.ssh/authorized_keys && chmod 600 ~root/.ssh/authorized_keys"}): {},
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	err := b.injectSSHKey(context.Background(), "crabbox-blue-1234", "root", "ssh-ed25519 AAAA test")
	if err != nil {
		t.Fatalf("injectSSHKey: %v", err)
	}
	found := false
	for _, call := range runner.calls {
		for _, arg := range call.Args {
			if strings.Contains(arg, "~root/.ssh") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("injectSSHKey should target ~root/.ssh when user is root")
	}
}

func TestInjectSSHKeyRejectsShellInjection(t *testing.T) {
	runner := &recordingRunner{}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	badUsers := []string{
		"admin; echo injected #",
		"$(whoami)",
		"user`id`",
		"root && rm -rf /",
		"",
		"user name",
	}
	for _, user := range badUsers {
		err := b.injectSSHKey(context.Background(), "crabbox-test", user, "ssh-ed25519 AAAA test")
		if err == nil {
			t.Errorf("injectSSHKey should reject user=%q", user)
		}
	}
	if len(runner.calls) != 0 {
		t.Fatalf("no tart commands should be issued for invalid users, got %d calls", len(runner.calls))
	}
}

func TestInstanceNameFromScopeRequiresPrefix(t *testing.T) {
	cases := []struct {
		scope string
		want  string
	}{
		{"instance:crabbox-blue-1234", "crabbox-blue-1234"},
		{"instance:", ""},
		{"", ""},
		{"http://not-instance", ""},
		{"crabbox-blue-1234", ""},
		{"  instance:crabbox-x  ", "crabbox-x"},
	}
	for _, tc := range cases {
		got := instanceNameFromScope(tc.scope)
		if got != tc.want {
			t.Errorf("instanceNameFromScope(%q) = %q, want %q", tc.scope, got, tc.want)
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
	inst := tartInstance{Name: "crabbox-blue-1234abcd", State: "running", Running: true}
	_, err := b.prepareLease(context.Background(), cfg, inst, "--", core.LeaseClaim{LeaseID: "cbx_test"}, false)
	if err == nil {
		t.Fatal("prepareLease should reject IP \"--\" for running VM")
	}
}

func TestResolveInstanceUsesRealState(t *testing.T) {
	listJSON := `[{"Name":"crabbox-blue-abc123","State":"stopped","Running":false,"Disk":50,"Size":12,"Source":"ghcr.io/cirruslabs/macos-sequoia-base:latest"}]`
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"list", "--source", "local", "--format", "json"}): {Stdout: listJSON},
			commandKey([]string{"ip", "crabbox-blue-abc123"}):                     {Stdout: "192.168.64.5\n"},
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

func TestWaitForIPDetectsStoppedVM(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"ip", "crabbox-stopped"}): {Stderr: "no IP address found, is your VM running?\n", ExitCode: 1},
		},
		errors: map[string]error{
			commandKey([]string{"ip", "crabbox-stopped"}): fmt.Errorf("exit status 1"),
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := b.waitForIP(ctx, "crabbox-stopped")
	if err == nil {
		t.Fatal("waitForIP should fail fast for a stopped VM")
	}
	errMsg := err.Error()
	if !strings.Contains(errMsg, "is your VM running") {
		t.Fatalf("waitForIP error = %q, want tart's stopped-VM diagnostic", errMsg)
	}
}

func TestConfigureVMSkipsImplicitDiskSize(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	err := b.configureVM(context.Background(), cfg, "crabbox-test")
	if err != nil {
		t.Fatalf("configureVM: %v", err)
	}
	for _, call := range runner.calls {
		for i, arg := range call.Args {
			if arg == "--disk-size" {
				t.Fatalf("configureVM called tart set --disk-size %s with implicit default; would break images with larger disks", call.Args[i+1])
			}
		}
	}
}

func TestConfigureVMAppliesExplicitDiskSize(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Tart.Disk = 200
	core.MarkTartDiskExplicit(&cfg)
	applyDefaults(&cfg)
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	err := b.configureVM(context.Background(), cfg, "crabbox-test")
	if err != nil {
		t.Fatalf("configureVM: %v", err)
	}
	found := false
	for _, call := range runner.calls {
		for _, arg := range call.Args {
			if arg == "--disk-size" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("configureVM should apply tart set --disk-size when explicitly set")
	}
}

func TestServerFromInstanceDefaultsLabels(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: &recordingRunner{}}).(*backend)

	inst := tartInstance{Name: "crabbox-blue-abc123", State: "running", Source: "ghcr.io/test:latest"}
	claim := core.LeaseClaim{LeaseID: "cbx_test", Slug: "my-slug"}
	server := b.serverFromInstance(inst, claim, cfg)

	checks := map[string]string{
		"crabbox":     "true",
		"provider":    providerName,
		"instance":    "crabbox-blue-abc123",
		"lease":       "cbx_test",
		"slug":        "my-slug",
		"state":       "running",
		"server_type": "ghcr.io/test:latest",
		"image":       cfg.Tart.Image,
		"ssh_user":    cfg.Tart.User,
		"ssh_port":    sshPort,
	}
	for key, want := range checks {
		if got := server.Labels[key]; got != want {
			t.Errorf("label %s = %q, want %q", key, got, want)
		}
	}
}

func TestServerFromInstancePreservesExistingLabels(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: &recordingRunner{}}).(*backend)

	inst := tartInstance{Name: "crabbox-blue-abc123", State: "running"}
	claim := core.LeaseClaim{
		LeaseID: "cbx_test",
		Labels: map[string]string{
			"ssh_user":  "customuser",
			"ssh_port":  "2222",
			"work_root": "/custom/root",
			"state":     "ready",
		},
	}
	server := b.serverFromInstance(inst, claim, cfg)
	if server.Labels["ssh_user"] != "customuser" {
		t.Fatalf("ssh_user = %q, want customuser (should preserve existing)", server.Labels["ssh_user"])
	}
	if server.Labels["ssh_port"] != "2222" {
		t.Fatalf("ssh_port = %q, want 2222 (should preserve existing)", server.Labels["ssh_port"])
	}
	if server.Labels["work_root"] != "/custom/root" {
		t.Fatalf("work_root = %q, want /custom/root (should preserve existing)", server.Labels["work_root"])
	}
}

func TestServerFromInstancePromotesRunningReady(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: &recordingRunner{}}).(*backend)

	inst := tartInstance{Name: "crabbox-blue-abc123", State: "running"}
	claim := core.LeaseClaim{LeaseID: "cbx_test", Labels: map[string]string{"state": "ready"}}
	server := b.serverFromInstance(inst, claim, cfg)
	if server.Status != "ready" {
		t.Fatalf("Status = %q, want ready (running instance with state=ready label)", server.Status)
	}
}

func TestServerFromInstanceDoesNotPromoteStoppedReady(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: &recordingRunner{}}).(*backend)

	inst := tartInstance{Name: "crabbox-blue-abc123", State: "stopped"}
	claim := core.LeaseClaim{LeaseID: "cbx_test", Labels: map[string]string{"state": "ready"}}
	server := b.serverFromInstance(inst, claim, cfg)
	if server.Status == "ready" {
		t.Fatal("Status = ready for stopped instance, should not promote")
	}
}

func TestPrepareLeaseSetsUserFromLabel(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: &recordingRunner{}}).(*backend)

	inst := tartInstance{Name: "crabbox-blue-abc123", State: "running"}
	claim := core.LeaseClaim{LeaseID: "cbx_test", Labels: map[string]string{"ssh_user": "customuser"}}
	lt, err := b.prepareLease(context.Background(), cfg, inst, "192.0.2.10", claim, false)
	if err != nil {
		t.Fatalf("prepareLease: %v", err)
	}
	if lt.SSH.User != "customuser" {
		t.Fatalf("SSH.User = %q, want customuser (label should override default)", lt.SSH.User)
	}
}

func TestPrepareLeaseDoesNotOverrideUserWithEmpty(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: &recordingRunner{}}).(*backend)

	inst := tartInstance{Name: "crabbox-blue-abc123", State: "running"}
	claim := core.LeaseClaim{LeaseID: "cbx_test", Labels: map[string]string{}}
	lt, err := b.prepareLease(context.Background(), cfg, inst, "192.0.2.10", claim, false)
	if err != nil {
		t.Fatalf("prepareLease: %v", err)
	}
	if lt.SSH.User != "admin" {
		t.Fatalf("SSH.User = %q, want admin (empty label should not override config)", lt.SSH.User)
	}
}

func TestPrepareLeaseSetsWorkRootFromLabel(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: &recordingRunner{}}).(*backend)

	inst := tartInstance{Name: "crabbox-blue-abc123", State: "running"}
	claim := core.LeaseClaim{LeaseID: "cbx_test", Labels: map[string]string{"work_root": "/custom/work"}}
	lt, err := b.prepareLease(context.Background(), cfg, inst, "192.0.2.10", claim, false)
	if err != nil {
		t.Fatalf("prepareLease: %v", err)
	}
	if lt.Server.Labels["work_root"] != "/custom/work" {
		t.Fatalf("work_root label = %q, want /custom/work", lt.Server.Labels["work_root"])
	}
}

func TestPrepareLeaseDoesNotOverrideWorkRootWithEmpty(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: &recordingRunner{}}).(*backend)

	inst := tartInstance{Name: "crabbox-blue-abc123", State: "running"}
	claim := core.LeaseClaim{LeaseID: "cbx_test", Labels: map[string]string{}}
	lt, err := b.prepareLease(context.Background(), cfg, inst, "192.0.2.10", claim, false)
	if err != nil {
		t.Fatalf("prepareLease: %v", err)
	}
	if lt.Server.Labels["work_root"] != cfg.Tart.WorkRoot {
		t.Fatalf("work_root = %q, want %q (empty label should not override config)", lt.Server.Labels["work_root"], cfg.Tart.WorkRoot)
	}
}

func TestApplyDefaultsPreservesTargetOS(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.TargetOS = core.TargetMacOS
	applyDefaults(&cfg)
	if cfg.TargetOS != core.TargetMacOS {
		t.Fatalf("TargetOS = %q, want macos (should preserve non-empty)", cfg.TargetOS)
	}
}

func TestApplyDefaultsPreservesWorkRoot(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Tart.WorkRoot = "/custom/work"
	applyDefaults(&cfg)
	if cfg.Tart.WorkRoot != "/custom/work" {
		t.Fatalf("Tart.WorkRoot = %q, want /custom/work (should preserve non-empty)", cfg.Tart.WorkRoot)
	}
}

func TestConfigureVMAppliesCPUAndMemory(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Tart.CPUs = 8
	cfg.Tart.Memory = 16384
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	if err := b.configureVM(context.Background(), cfg, "crabbox-test"); err != nil {
		t.Fatalf("configureVM: %v", err)
	}
	foundCPU, foundMem := false, false
	for _, call := range runner.calls {
		for i, arg := range call.Args {
			if arg == "--cpu" && i+1 < len(call.Args) && call.Args[i+1] == "8" {
				foundCPU = true
			}
			if arg == "--memory" && i+1 < len(call.Args) && call.Args[i+1] == "16384" {
				foundMem = true
			}
		}
	}
	if !foundCPU {
		t.Fatal("configureVM should apply --cpu when CPUs > 0")
	}
	if !foundMem {
		t.Fatal("configureVM should apply --memory when Memory > 0")
	}
}

func TestConfigureVMSkipsZeroCPUAndMemory(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Tart.CPUs = 0
	cfg.Tart.Memory = 0
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	if err := b.configureVM(context.Background(), cfg, "crabbox-test"); err != nil {
		t.Fatalf("configureVM: %v", err)
	}
	for _, call := range runner.calls {
		for _, arg := range call.Args {
			if arg == "--cpu" {
				t.Fatal("configureVM should not apply --cpu when CPUs == 0")
			}
			if arg == "--memory" {
				t.Fatal("configureVM should not apply --memory when Memory == 0")
			}
		}
	}
}

func TestShouldCleanupStoppedInstance(t *testing.T) {
	server := Server{Status: "stopped", Labels: map[string]string{}}
	ok, reason := shouldCleanup(server, core.LeaseClaim{}, true, time.Now())
	if !ok || reason != "instance state=stopped" {
		t.Fatalf("cleanup=%v reason=%q, want true/instance state=stopped", ok, reason)
	}
}

func TestShouldCleanupZeroIdleTimeout(t *testing.T) {
	server := Server{Status: "running", Labels: map[string]string{}}
	claim := core.LeaseClaim{
		LeaseID:            "cbx_123",
		LastUsedAt:         time.Now().Add(-48 * time.Hour).Format(time.RFC3339),
		IdleTimeoutSeconds: 0,
	}
	ok, reason := shouldCleanup(server, claim, true, time.Now())
	if ok {
		t.Fatalf("cleanup=%v reason=%q; zero idle timeout should keep claim active", ok, reason)
	}
	if reason != "claim active" {
		t.Fatalf("reason=%q, want \"claim active\"", reason)
	}
}

func TestFirstLineNewlineAtStart(t *testing.T) {
	got := firstLine("\nfoo")
	if got != "foo" {
		t.Fatalf("firstLine(\"\\nfoo\") = %q, want \"foo\"", got)
	}
}

func TestConfigureVMSkipsExplicitZeroDisk(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Tart.Disk = 0
	core.MarkTartDiskExplicit(&cfg)
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	if err := b.configureVM(context.Background(), cfg, "crabbox-test"); err != nil {
		t.Fatalf("configureVM: %v", err)
	}
	for _, call := range runner.calls {
		for _, arg := range call.Args {
			if arg == "--disk-size" {
				t.Fatal("configureVM should not apply --disk-size 0 even when explicit")
			}
		}
	}
}

func TestShouldCleanupGracePeriodNotExpired(t *testing.T) {
	server := Server{Status: "running", Labels: map[string]string{}}
	now := time.Now()
	claim := core.LeaseClaim{
		LeaseID:            "cbx_123",
		LastUsedAt:         now.Add(-2 * time.Hour).Format(time.RFC3339),
		IdleTimeoutSeconds: int((1 * time.Hour).Seconds()),
	}
	ok, reason := shouldCleanup(server, claim, true, now)
	if ok {
		t.Fatalf("cleanup=%v reason=%q; idle expired but 12h grace period should keep it active", ok, reason)
	}
	if reason != "claim active" {
		t.Fatalf("reason=%q, want \"claim active\"", reason)
	}
}

func TestResolveInstanceByLeaseID(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateDir)
	err := core.ClaimLeaseForRepoProviderScopePond(
		"cbx_claim123", "my-slug", providerName, "instance:crabbox-blue-abc123", "", t.TempDir(), 30*time.Minute, false,
	)
	if err != nil {
		t.Fatalf("setup claim: %v", err)
	}

	listJSON := `[{"Name":"crabbox-blue-abc123","State":"running","Running":true,"Disk":50,"Size":12,"Source":"ghcr.io/test:latest"}]`
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"list", "--source", "local", "--format", "json"}): {Stdout: listJSON},
			commandKey([]string{"ip", "crabbox-blue-abc123"}):                     {Stdout: "192.168.64.5\n"},
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	inst, ip, claim, err := b.resolveInstance(context.Background(), "cbx_claim123")
	if err != nil {
		t.Fatalf("resolveInstance by LeaseID: %v", err)
	}
	if inst.Name != "crabbox-blue-abc123" {
		t.Fatalf("inst.Name = %q, want crabbox-blue-abc123", inst.Name)
	}
	if ip != "192.168.64.5" {
		t.Fatalf("ip = %q, want 192.168.64.5", ip)
	}
	if claim.LeaseID != "cbx_claim123" {
		t.Fatalf("claim.LeaseID = %q, want cbx_claim123", claim.LeaseID)
	}
}

func TestResolveInstanceBySlug(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateDir)
	err := core.ClaimLeaseForRepoProviderScopePond(
		"cbx_slug456", "test-slug", providerName, "instance:crabbox-blue-def456", "", t.TempDir(), 30*time.Minute, false,
	)
	if err != nil {
		t.Fatalf("setup claim: %v", err)
	}

	listJSON := `[{"Name":"crabbox-blue-def456","State":"running","Running":true,"Disk":50,"Size":12,"Source":"ghcr.io/test:latest"}]`
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"list", "--source", "local", "--format", "json"}): {Stdout: listJSON},
			commandKey([]string{"ip", "crabbox-blue-def456"}):                     {Stdout: "192.168.64.6\n"},
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	inst, ip, _, err := b.resolveInstance(context.Background(), "test-slug")
	if err != nil {
		t.Fatalf("resolveInstance by slug: %v", err)
	}
	if inst.Name != "crabbox-blue-def456" {
		t.Fatalf("inst.Name = %q, want crabbox-blue-def456", inst.Name)
	}
	if ip != "192.168.64.6" {
		t.Fatalf("ip = %q, want 192.168.64.6", ip)
	}
}

func TestApplyFlagsRejectsNonPositiveResources(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"zero cpu", []string{"--tart-cpu", "0"}, "--tart-cpu must be at least 4"},
		{"negative cpu", []string{"--tart-cpu", "-1"}, "--tart-cpu must be at least 4"},
		{"below minimum cpu", []string{"--tart-cpu", "2"}, "--tart-cpu must be at least 4"},
		{"zero memory", []string{"--tart-memory", "0"}, "--tart-memory must be a positive integer"},
		{"negative memory", []string{"--tart-memory", "-1"}, "--tart-memory must be a positive integer"},
		{"zero disk", []string{"--tart-disk", "0"}, "--tart-disk must be a positive integer"},
		{"negative disk", []string{"--tart-disk", "-1"}, "--tart-disk must be a positive integer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := core.BaseConfig()
			cfg.Provider = providerName
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			vals := registerFlags(fs, cfg)
			if err := fs.Parse(tc.args); err != nil {
				t.Fatalf("parse: %v", err)
			}
			err := applyFlags(&cfg, fs, vals)
			if err == nil {
				t.Fatal("expected error for non-positive resource flag")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestApplyFlagsAcceptsPositiveResources(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	vals := registerFlags(fs, cfg)
	if err := fs.Parse([]string{"--tart-cpu", "4", "--tart-memory", "8192", "--tart-disk", "100"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := applyFlags(&cfg, fs, vals); err != nil {
		t.Fatalf("applyFlags: %v", err)
	}
	if cfg.Tart.CPUs != 4 {
		t.Fatalf("CPUs = %d, want 4", cfg.Tart.CPUs)
	}
	if cfg.Tart.Memory != 8192 {
		t.Fatalf("Memory = %d, want 8192", cfg.Tart.Memory)
	}
	if cfg.Tart.Disk != 100 {
		t.Fatalf("Disk = %d, want 100", cfg.Tart.Disk)
	}
}

func TestApplyFlagsNegativeFromConfigRejected(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Tart.CPUs = -2
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	vals := registerFlags(fs, cfg)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	err := applyFlags(&cfg, fs, vals)
	if err == nil {
		t.Fatal("expected error for negative CPU from config")
	}
	if !strings.Contains(err.Error(), "tart cpu count must be at least 4") {
		t.Fatalf("error %q does not contain expected message", err.Error())
	}
}

func TestDoctorHappyPath(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"--version"}):                                     {Stdout: "2.32.1\n"},
			commandKey([]string{"list", "--source", "local", "--format", "json"}): {Stdout: `[{"Name":"crabbox-blue-1234","State":"running","Running":true,"Disk":50,"Size":15,"Source":"ghcr.io/test:latest"}]`},
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	result, err := b.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if result.Provider != providerName {
		t.Fatalf("Provider = %q, want %q", result.Provider, providerName)
	}
	if !strings.Contains(result.Message, "cli=ready") {
		t.Fatalf("Message missing cli=ready: %q", result.Message)
	}
	if !strings.Contains(result.Message, "runtime=2.32.1") {
		t.Fatalf("Message missing runtime version: %q", result.Message)
	}
	if !strings.Contains(result.Message, "leases=1") {
		t.Fatalf("Message missing leases=1: %q", result.Message)
	}
}

func TestDoctorCountsOnlyCrabboxVMs(t *testing.T) {
	listJSON := `[
		{"Name":"crabbox-blue-1234","State":"running","Running":true,"Disk":50,"Size":15,"Source":"ghcr.io/test:latest"},
		{"Name":"my-personal-vm","State":"stopped","Running":false,"Disk":50,"Size":12,"Source":"ghcr.io/other:latest"},
		{"Name":"sequoia-base","State":"stopped","Running":false,"Disk":50,"Size":10,"Source":"ghcr.io/cirruslabs/macos-sequoia-base:latest"}
	]`
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"--version"}):                                     {Stdout: "2.32.1\n"},
			commandKey([]string{"list", "--source", "local", "--format", "json"}): {Stdout: listJSON},
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	result, err := b.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if !strings.Contains(result.Message, "leases=1") {
		t.Fatalf("Doctor should count only crabbox- VMs (want leases=1): %q", result.Message)
	}
}

func TestDoctorTartNotInstalled(t *testing.T) {
	runner := &recordingRunner{
		errors: map[string]error{
			commandKey([]string{"--version"}): fmt.Errorf("exec: tart: not found"),
		},
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"--version"}): {ExitCode: 127, Stderr: "tart: command not found"},
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	_, err := b.Doctor(context.Background(), core.DoctorRequest{})
	if err == nil {
		t.Fatal("Doctor should fail when tart is not installed")
	}
	if !strings.Contains(err.Error(), "tart --version") {
		t.Fatalf("error should mention tart --version: %v", err)
	}
}

func TestReleaseLease(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"stop", "crabbox-blue-1234"}):   {},
			commandKey([]string{"delete", "crabbox-blue-1234"}): {},
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{
		Lease: core.LeaseTarget{
			LeaseID: "cbx_test123",
			Server:  core.Server{CloudID: "crabbox-blue-1234", Labels: map[string]string{"instance": "crabbox-blue-1234"}},
		},
	})
	if err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}
	var sawStop, sawDelete bool
	for _, call := range runner.calls {
		key := commandKey(call.Args)
		if key == commandKey([]string{"stop", "crabbox-blue-1234"}) {
			sawStop = true
		}
		if key == commandKey([]string{"delete", "crabbox-blue-1234"}) {
			sawDelete = true
		}
	}
	if !sawStop {
		t.Fatal("ReleaseLease should call tart stop")
	}
	if !sawDelete {
		t.Fatal("ReleaseLease should call tart delete")
	}
}

func TestReleaseLeaseRequiresInstanceName(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{
		Lease: core.LeaseTarget{Server: core.Server{}},
	})
	if err == nil {
		t.Fatal("ReleaseLease should fail without instance name")
	}
	if !strings.Contains(err.Error(), "release requires") {
		t.Fatalf("error should say release requires: %v", err)
	}
}

func TestReleaseLeaseMessage(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: &recordingRunner{}}).(*backend)

	msg := b.ReleaseLeaseMessage(core.LeaseTarget{
		LeaseID: "cbx_abc",
		Server:  core.Server{CloudID: "crabbox-blue-1234"},
	})
	if !strings.Contains(msg, "cbx_abc") {
		t.Fatalf("message should contain lease ID: %q", msg)
	}
	if !strings.Contains(msg, "crabbox-blue-1234") {
		t.Fatalf("message should contain instance name: %q", msg)
	}
}

func TestReleaseLeaseInfersLeaseIDFromLabels(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"stop", "crabbox-blue-1234"}):   {},
			commandKey([]string{"delete", "crabbox-blue-1234"}): {},
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{
		Lease: core.LeaseTarget{
			Server: core.Server{
				CloudID: "crabbox-blue-1234",
				Labels:  map[string]string{"instance": "crabbox-blue-1234", "lease": "cbx_inferred"},
			},
		},
	})
	if err != nil {
		t.Fatalf("ReleaseLease with inferred LeaseID: %v", err)
	}
}

func TestReleaseLeaseDeleteError(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"stop", "crabbox-blue-1234"}):   {},
			commandKey([]string{"delete", "crabbox-blue-1234"}): {ExitCode: 1, Stderr: "VM not found"},
		},
		errors: map[string]error{
			commandKey([]string{"delete", "crabbox-blue-1234"}): fmt.Errorf("exit status 1"),
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{
		Lease: core.LeaseTarget{
			LeaseID: "cbx_test123",
			Server:  core.Server{CloudID: "crabbox-blue-1234"},
		},
	})
	if err == nil {
		t.Fatal("ReleaseLease should propagate deleteVM error")
	}
}

func TestCleanupSkipsNonCrabboxVMs(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	listJSON := `[{"Name":"my-dev-vm","State":"stopped","Running":false,"Disk":50,"Size":10,"Source":"ghcr.io/test:latest"},{"Name":"crabbox-old-1234","State":"stopped","Running":false,"Disk":50,"Size":10,"Source":"ghcr.io/test:latest"}]`
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"list", "--source", "local", "--format", "json"}): {Stdout: listJSON},
			commandKey([]string{"stop", "crabbox-old-1234"}):                      {},
			commandKey([]string{"delete", "crabbox-old-1234"}):                    {},
		},
	}
	var stdout strings.Builder
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: &stdout, Stderr: io.Discard, Exec: runner}).(*backend)

	err := b.Cleanup(context.Background(), core.CleanupRequest{})
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	output := stdout.String()
	if strings.Contains(output, "my-dev-vm") {
		t.Fatal("Cleanup should not mention non-crabbox VMs in output")
	}
	if !strings.Contains(output, "removed=1") {
		t.Fatalf("expected removed=1 in output: %q", output)
	}
}

func TestCleanupDeleteError(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	listJSON := `[{"Name":"crabbox-broken-1234","State":"stopped","Running":false,"Disk":50,"Size":10,"Source":"ghcr.io/test:latest"}]`
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"list", "--source", "local", "--format", "json"}): {Stdout: listJSON},
			commandKey([]string{"stop", "crabbox-broken-1234"}):                   {},
			commandKey([]string{"delete", "crabbox-broken-1234"}):                 {ExitCode: 1, Stderr: "busy"},
		},
		errors: map[string]error{
			commandKey([]string{"delete", "crabbox-broken-1234"}): fmt.Errorf("exit status 1"),
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	err := b.Cleanup(context.Background(), core.CleanupRequest{})
	if err == nil {
		t.Fatal("Cleanup should propagate deleteVM error")
	}
}

func TestCleanupRemovesOrphanedClaims(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateDir)
	err := core.ClaimLeaseForRepoProviderScopePond(
		"cbx_orphan", "orphan-slug", providerName, "instance:crabbox-gone-9999", "", t.TempDir(), 30*time.Minute, false,
	)
	if err != nil {
		t.Fatalf("setup orphan claim: %v", err)
	}
	listJSON := `[]`
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"list", "--source", "local", "--format", "json"}): {Stdout: listJSON},
		},
	}
	var stdout strings.Builder
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: &stdout, Stderr: io.Discard, Exec: runner}).(*backend)

	err = b.Cleanup(context.Background(), core.CleanupRequest{})
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	output := stdout.String()
	if !strings.Contains(output, "missing instance") {
		t.Fatalf("should report orphaned claim removal: %q", output)
	}
	if !strings.Contains(output, "claims_removed=1") {
		t.Fatalf("should report claims_removed=1: %q", output)
	}
}

func TestCleanupRemovesMalformedClaimsWithNoInstance(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateDir)
	err := core.ClaimLeaseForRepoProviderScopePond(
		"cbx_noinstance", "no-instance", providerName, "", "", t.TempDir(), 30*time.Minute, false,
	)
	if err != nil {
		t.Fatalf("setup malformed claim: %v", err)
	}
	err = core.ClaimLeaseForRepoProviderScopePond(
		"cbx_missingvm", "missing-vm", providerName, "instance:crabbox-missing-abc123", "", t.TempDir(), 30*time.Minute, false,
	)
	if err != nil {
		t.Fatalf("setup normal orphan claim: %v", err)
	}
	listJSON := `[]`
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"list", "--source", "local", "--format", "json"}): {Stdout: listJSON},
		},
	}
	var stdout strings.Builder
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: &stdout, Stderr: io.Discard, Exec: runner}).(*backend)

	err = b.Cleanup(context.Background(), core.CleanupRequest{DryRun: true})
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	output := stdout.String()
	if !strings.Contains(output, "cbx_noinstance") {
		t.Fatalf("malformed claim with no instance should be reported: %q", output)
	}
	if !strings.Contains(output, "malformed claim") {
		t.Fatalf("should use 'malformed claim' reason: %q", output)
	}
	if !strings.Contains(output, "cbx_missingvm") {
		t.Fatalf("normal orphan claim should also be reported: %q", output)
	}
}

func TestResolveInstanceByDirectName(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	listJSON := `[{"Name":"crabbox-blue-def456","State":"running","Running":true,"Disk":50,"Size":12,"Source":"ghcr.io/test:latest"}]`
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"list", "--source", "local", "--format", "json"}): {Stdout: listJSON},
			commandKey([]string{"ip", "crabbox-blue-def456"}):                     {Stdout: "192.168.64.7\n"},
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	inst, ip, _, err := b.resolveInstance(context.Background(), "crabbox-blue-def456")
	if err != nil {
		t.Fatalf("resolveInstance by name: %v", err)
	}
	if inst.Name != "crabbox-blue-def456" {
		t.Fatalf("inst.Name = %q", inst.Name)
	}
	if ip != "192.168.64.7" {
		t.Fatalf("ip = %q", ip)
	}
}

func TestResolveInstanceNotFound(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	listJSON := `[{"Name":"crabbox-blue-1234","State":"running","Running":true,"Disk":50,"Size":12,"Source":"ghcr.io/test:latest"}]`
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"list", "--source", "local", "--format", "json"}): {Stdout: listJSON},
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	_, _, _, err := b.resolveInstance(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("resolveInstance should fail for nonexistent identifier")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error should mention not found: %v", err)
	}
}

func TestResolveInstanceEmptyIdentifier(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: &recordingRunner{}}).(*backend)

	_, _, _, err := b.resolveInstance(context.Background(), "")
	if err == nil {
		t.Fatal("resolveInstance should fail for empty identifier")
	}
	if !strings.Contains(err.Error(), "requires --id") {
		t.Fatalf("error should mention --id: %v", err)
	}
}

func TestPrepareLeaseAppliesSSHUserFromLabel(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	runner := &recordingRunner{}
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	inst := tartInstance{Name: "crabbox-blue-1234", State: "running"}
	claim := core.LeaseClaim{
		LeaseID: "cbx_test",
		Labels: map[string]string{
			"ssh_user": "root",
		},
	}
	lt, err := b.prepareLease(context.Background(), cfg, inst, "192.0.2.10", claim, false)
	if err != nil {
		t.Fatalf("prepareLease: %v", err)
	}
	if lt.SSH.User != "root" {
		t.Fatalf("SSH.User = %q, want root", lt.SSH.User)
	}
}

func TestPrepareLeaseMissingIP(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: &recordingRunner{}}).(*backend)

	running := tartInstance{Name: "crabbox-blue-1234", State: "running", Running: true}
	_, err := b.prepareLease(context.Background(), cfg, running, "", core.LeaseClaim{LeaseID: "cbx_test"}, false)
	if err == nil {
		t.Fatal("prepareLease should fail with empty IP for running VM")
	}
	_, err = b.prepareLease(context.Background(), cfg, running, "--", core.LeaseClaim{LeaseID: "cbx_test"}, false)
	if err == nil {
		t.Fatal("prepareLease should fail with '--' IP for running VM")
	}
}

func TestPrepareLeaseStoppedVMReturnsState(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: &recordingRunner{}}).(*backend)

	stopped := tartInstance{Name: "crabbox-blue-1234", State: "stopped", Running: false}
	lt, err := b.prepareLease(context.Background(), cfg, stopped, "", core.LeaseClaim{LeaseID: "cbx_test"}, false)
	if err != nil {
		t.Fatalf("prepareLease should return stopped state, not error: %v", err)
	}
	if lt.Server.Status != "stopped" {
		t.Fatalf("Server.Status = %q, want stopped", lt.Server.Status)
	}
	if lt.SSH.Host != "" {
		t.Fatalf("SSH.Host should be empty for stopped VM, got %q", lt.SSH.Host)
	}
}

func TestApplyDefaultsPreservesExplicitTarget(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.TargetOS = "macos"
	applyDefaults(&cfg)
	if cfg.TargetOS != "macos" {
		t.Fatalf("applyDefaults overrode explicit macos target: %q", cfg.TargetOS)
	}
}

func TestApplyDefaultsConvertsEmptyTarget(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.TargetOS = ""
	applyDefaults(&cfg)
	if cfg.TargetOS != "macos" {
		t.Fatalf("applyDefaults should set empty target to macos: %q", cfg.TargetOS)
	}
}

func TestCleanupRemovesStoppedCrabboxVMs(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	listJSON := `[{"Name":"crabbox-old-1234","State":"stopped","Running":false,"Disk":50,"Size":10,"Source":"ghcr.io/test:latest"},{"Name":"my-personal-vm","State":"stopped","Running":false,"Disk":50,"Size":10,"Source":"ghcr.io/test:latest"}]`
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"list", "--source", "local", "--format", "json"}): {Stdout: listJSON},
			commandKey([]string{"stop", "crabbox-old-1234"}):                      {},
			commandKey([]string{"delete", "crabbox-old-1234"}):                    {},
		},
	}
	var stdout strings.Builder
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: &stdout, Stderr: io.Discard, Exec: runner}).(*backend)

	err := b.Cleanup(context.Background(), core.CleanupRequest{})
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	var sawDelete bool
	for _, call := range runner.calls {
		if commandKey(call.Args) == commandKey([]string{"delete", "crabbox-old-1234"}) {
			sawDelete = true
		}
	}
	if !sawDelete {
		t.Fatal("Cleanup should delete stopped crabbox VMs")
	}
	for _, call := range runner.calls {
		if commandKey(call.Args) == commandKey([]string{"delete", "my-personal-vm"}) {
			t.Fatal("Cleanup should not touch non-crabbox VMs")
		}
	}
}

func TestCleanupDryRunDoesNotDelete(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	listJSON := `[{"Name":"crabbox-old-1234","State":"stopped","Running":false,"Disk":50,"Size":10,"Source":"ghcr.io/test:latest"}]`
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"list", "--source", "local", "--format", "json"}): {Stdout: listJSON},
		},
	}
	var stdout strings.Builder
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: &stdout, Stderr: io.Discard, Exec: runner}).(*backend)

	err := b.Cleanup(context.Background(), core.CleanupRequest{DryRun: true})
	if err != nil {
		t.Fatalf("Cleanup dry-run: %v", err)
	}
	for _, call := range runner.calls {
		if len(call.Args) > 0 && (call.Args[0] == "stop" || call.Args[0] == "delete") {
			t.Fatalf("dry-run should not call %s", call.Args[0])
		}
	}
	if !strings.Contains(stdout.String(), "would remove") {
		t.Fatalf("dry-run should print 'would remove': %q", stdout.String())
	}
}

func TestStopVMAndDeleteVM(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"stop", "test-vm"}):   {},
			commandKey([]string{"delete", "test-vm"}): {},
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	if err := b.stopVM(context.Background(), "test-vm"); err != nil {
		t.Fatalf("stopVM: %v", err)
	}
	if err := b.deleteVM(context.Background(), "test-vm"); err != nil {
		t.Fatalf("deleteVM: %v", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(runner.calls))
	}
}

func TestStopVMError(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"stop", "test-vm"}): {ExitCode: 1, Stderr: "VM not running"},
		},
		errors: map[string]error{
			commandKey([]string{"stop", "test-vm"}): fmt.Errorf("exit status 1"),
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	err := b.stopVM(context.Background(), "test-vm")
	if err == nil {
		t.Fatal("stopVM should propagate error")
	}
}

func TestCloneVM(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"clone", "ghcr.io/test:latest", "crabbox-blue-1234"}): {},
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Tart.Image = "ghcr.io/test:latest"
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	err := b.cloneVM(context.Background(), cfg, "crabbox-blue-1234")
	if err != nil {
		t.Fatalf("cloneVM: %v", err)
	}
	if len(runner.calls) != 1 || runner.calls[0].Args[0] != "clone" {
		t.Fatalf("expected clone call, got %v", runner.calls)
	}
}

func TestCloneVMError(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"clone", "ghcr.io/test:latest", "crabbox-blue-1234"}): {ExitCode: 1, Stderr: "image not found"},
		},
		errors: map[string]error{
			commandKey([]string{"clone", "ghcr.io/test:latest", "crabbox-blue-1234"}): fmt.Errorf("exit status 1"),
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Tart.Image = "ghcr.io/test:latest"
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	err := b.cloneVM(context.Background(), cfg, "crabbox-blue-1234")
	if err == nil {
		t.Fatal("cloneVM should fail on tart clone error")
	}
	if !strings.Contains(err.Error(), "image not found") {
		t.Fatalf("error should contain tart stderr: %v", err)
	}
}

func TestListInstances(t *testing.T) {
	listJSON := `[{"Name":"vm1","State":"running","Running":true,"Disk":50,"Size":15,"Source":"img1"},{"Name":"vm2","State":"stopped","Running":false,"Disk":30,"Size":10,"Source":"img2"}]`
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"list", "--source", "local", "--format", "json"}): {Stdout: listJSON},
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	instances, err := b.listInstances(context.Background())
	if err != nil {
		t.Fatalf("listInstances: %v", err)
	}
	if len(instances) != 2 {
		t.Fatalf("expected 2 instances, got %d", len(instances))
	}
	if instances[0].Name != "vm1" || instances[1].Name != "vm2" {
		t.Fatalf("instance names = %q, %q", instances[0].Name, instances[1].Name)
	}
}

func TestListInstancesParseError(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"list", "--source", "local", "--format", "json"}): {Stdout: "not json"},
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	_, err := b.listInstances(context.Background())
	if err == nil {
		t.Fatal("listInstances should fail on invalid JSON")
	}
}

func TestInstanceRunning(t *testing.T) {
	cases := []struct {
		state string
		want  bool
	}{
		{"running", true},
		{"Running", true},
		{"ready", true},
		{"stopped", false},
		{"suspended", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := instanceRunning(tc.state); got != tc.want {
			t.Errorf("instanceRunning(%q) = %v, want %v", tc.state, got, tc.want)
		}
	}
}

func TestTartState(t *testing.T) {
	if got := tartState("  Running  "); got != "running" {
		t.Errorf("tartState = %q, want running", got)
	}
	if got := tartState("STOPPED"); got != "stopped" {
		t.Errorf("tartState = %q, want stopped", got)
	}
}

func TestInstanceScope(t *testing.T) {
	if got := instanceScope("crabbox-blue-1234"); got != "instance:crabbox-blue-1234" {
		t.Fatalf("instanceScope = %q", got)
	}
	if got := instanceScope(""); got != "" {
		t.Fatalf("instanceScope empty = %q", got)
	}
	if got := instanceScope("  "); got != "" {
		t.Fatalf("instanceScope whitespace = %q", got)
	}
}

func TestInstanceNameFromScope(t *testing.T) {
	if got := instanceNameFromScope("instance:crabbox-blue-1234"); got != "crabbox-blue-1234" {
		t.Fatalf("instanceNameFromScope = %q", got)
	}
	if got := instanceNameFromScope("something-else"); got != "" {
		t.Fatalf("instanceNameFromScope without prefix should return empty, got %q", got)
	}
}

func TestInstanceNameFromClaim(t *testing.T) {
	claim := core.LeaseClaim{Labels: map[string]string{"instance": "crabbox-blue-1234"}}
	if got := instanceNameFromClaim(claim); got != "crabbox-blue-1234" {
		t.Fatalf("instanceNameFromClaim from labels = %q", got)
	}
	claim2 := core.LeaseClaim{ProviderScope: "instance:crabbox-green-5678"}
	if got := instanceNameFromClaim(claim2); got != "crabbox-green-5678" {
		t.Fatalf("instanceNameFromClaim from scope = %q", got)
	}
}

func TestFirstNonBlank(t *testing.T) {
	if got := firstNonBlank("", "  ", "hello", "world"); got != "hello" {
		t.Fatalf("firstNonBlank = %q, want hello", got)
	}
	if got := firstNonBlank("", "", ""); got != "" {
		t.Fatalf("firstNonBlank all blank = %q", got)
	}
	if got := firstNonBlank("first"); got != "first" {
		t.Fatalf("firstNonBlank single = %q", got)
	}
}

func TestCommandError(t *testing.T) {
	err := commandError("tart stop", core.LocalCommandResult{ExitCode: 1, Stderr: "VM not running"}, fmt.Errorf("exit status 1"))
	if !strings.Contains(err.Error(), "VM not running") {
		t.Fatalf("commandError should include stderr: %v", err)
	}
	if !strings.Contains(err.Error(), "tart stop") {
		t.Fatalf("commandError should include action: %v", err)
	}
}

func TestCommandErrorFallsBackToStdout(t *testing.T) {
	err := commandError("tart stop", core.LocalCommandResult{ExitCode: 1, Stdout: "some output"}, fmt.Errorf("exit status 1"))
	if !strings.Contains(err.Error(), "some output") {
		t.Fatalf("commandError should fall back to stdout: %v", err)
	}
}

func TestCommandErrorMinimalExitCode(t *testing.T) {
	err := commandError("tart stop", core.LocalCommandResult{ExitCode: 0}, fmt.Errorf("exit status 1"))
	var exitErr core.ExitError
	if !core.AsExitError(err, &exitErr) {
		t.Fatalf("expected ExitError, got %T", err)
	}
	if exitErr.Code != 1 {
		t.Fatalf("exit code = %d, want 1 (minimum)", exitErr.Code)
	}
}

func TestIsTartProviderName(t *testing.T) {
	for _, name := range []string{"tart", "Tart", "TART", "local-tart", "macos-vm", " tart "} {
		if !isTartProviderName(name) {
			t.Errorf("isTartProviderName(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"docker", "aws", "hyperv", ""} {
		if isTartProviderName(name) {
			t.Errorf("isTartProviderName(%q) = true, want false", name)
		}
	}
}

func TestGetIPReturnsEmptyOnError(t *testing.T) {
	runner := &recordingRunner{
		errors: map[string]error{
			commandKey([]string{"ip", "test-vm"}): fmt.Errorf("not running"),
		},
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"ip", "test-vm"}): {},
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	ip := b.getIP(context.Background(), "test-vm")
	if ip != "" {
		t.Fatalf("getIP should return empty on error, got %q", ip)
	}
}

func TestGetIPStripsDoubleDash(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"ip", "test-vm"}): {Stdout: "--\n"},
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	ip := b.getIP(context.Background(), "test-vm")
	if ip != "" {
		t.Fatalf("getIP should return empty for '--', got %q", ip)
	}
}

func TestTouchPreservesProviderLabels(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: &recordingRunner{}}).(*backend)

	original := core.LeaseTarget{
		Server: core.Server{
			Labels: map[string]string{
				"image":     "ghcr.io/test:latest",
				"instance":  "crabbox-blue-1234",
				"ssh_user":  "admin",
				"ssh_port":  "22",
				"work_root": "/Users/admin/crabbox",
			},
		},
	}
	server, err := b.Touch(context.Background(), core.TouchRequest{
		Lease: original,
		State: "ready",
	})
	if err != nil {
		t.Fatalf("Touch: %v", err)
	}
	for _, key := range []string{"image", "instance", "ssh_user", "ssh_port", "work_root"} {
		if server.Labels[key] != original.Server.Labels[key] {
			t.Errorf("Touch lost label %s: got %q, want %q", key, server.Labels[key], original.Server.Labels[key])
		}
	}
}

func TestConfigureDoctor(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	p := Provider{}
	backend, err := p.ConfigureDoctor(cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: &recordingRunner{}})
	if err != nil {
		t.Fatalf("ConfigureDoctor: %v", err)
	}
	if backend.Spec().Name != providerName {
		t.Fatalf("Spec().Name = %q, want %q", backend.Spec().Name, providerName)
	}
}

func sampleListJSON() string {
	return `[{"Name":"crabbox-blue-1234abcd","State":"running","Running":true,"Disk":50,"Size":15,"Source":"ghcr.io/cirruslabs/macos-sequoia-base:latest"},{"Name":"my-dev-vm","State":"stopped","Running":false,"Disk":50,"Size":12,"Source":"ghcr.io/cirruslabs/macos-sequoia-base:latest"}]`
}
