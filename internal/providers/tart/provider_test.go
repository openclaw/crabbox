package tart

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	if cfg.Tart.Disk != 0 {
		t.Fatalf("default disk=%d, want 0 (clone default)", cfg.Tart.Disk)
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

func TestAcquireKeepIPFailureDeletesUnclaimedVMAndKey(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("HOME", configHome)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	binDir := t.TempDir()
	fakeTart := filepath.Join(binDir, "tart")
	if err := os.WriteFile(fakeTart, []byte("#!/bin/sh\nsleep 0.2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"list", "--source", "local", "--format", "json"}): {Stdout: "[]"},
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	b.startupObserveTimeout = 20 * time.Millisecond
	// Keep setup outside the deadline race under coverage while still forcing
	// waitForIP to fail promptly after the VM starts.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if _, err := b.Acquire(ctx, core.AcquireRequest{Keep: true, Repo: core.Repo{Root: t.TempDir()}}); err == nil {
		t.Fatal("Acquire succeeded")
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	keys, err := filepath.Glob(filepath.Join(configDir, "crabbox", "testboxes", "*", "id_ed25519"))
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Fatalf("unclaimed failed VM key count=%d paths=%v, want 0", len(keys), keys)
	}
	stopped, deleted := false, false
	for _, call := range runner.calls {
		if len(call.Args) > 0 && call.Args[0] == "stop" {
			stopped = true
		}
		if len(call.Args) > 0 && call.Args[0] == "delete" {
			deleted = true
		}
	}
	if !stopped || !deleted {
		t.Fatalf("keep=true unclaimed post-start failure should cleanup VM, stopped=%t deleted=%t calls=%v", stopped, deleted, runner.calls)
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

func TestSpecAdvertisesDesktop(t *testing.T) {
	if !(Provider{}).Spec().Features.Has(core.FeatureDesktop) {
		t.Fatal("tart Spec should advertise FeatureDesktop so --desktop is accepted")
	}
}

func TestDesktopCredentials(t *testing.T) {
	credentials, ok := (Provider{}).DesktopCredentials(core.Config{}, core.SSHTarget{})
	if !ok {
		t.Fatal("tart should provide desktop credentials")
	}
	if credentials.Username != "admin" || credentials.Password != "admin" {
		t.Fatalf("default credentials = %#v", credentials)
	}

	cfg := core.Config{}
	cfg.Tart.User = "configured-user"
	cfg.Tart.Password = "configured-password"
	credentials, ok = (Provider{}).DesktopCredentials(cfg, core.SSHTarget{User: "lease-user"})
	if !ok {
		t.Fatal("tart should provide configured desktop credentials")
	}
	if credentials.Username != "lease-user" || credentials.Password != "configured-password" {
		t.Fatalf("configured credentials = %#v", credentials)
	}

	cfg.Tart.Password = " password with spaces "
	credentials, _ = (Provider{}).DesktopCredentials(cfg, core.SSHTarget{User: "lease-user"})
	if credentials.Password != cfg.Tart.Password {
		t.Fatalf("password = %q, want exact configured value %q", credentials.Password, cfg.Tart.Password)
	}
}

func TestEnableScreenSharingEnablesService(t *testing.T) {
	runner := &recordingRunner{}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	if err := b.enableScreenSharing(context.Background(), "crabbox-blue-1234"); err != nil {
		t.Fatalf("enableScreenSharing: %v", err)
	}

	var script string
	for _, call := range runner.calls {
		if len(call.Args) >= 4 && call.Args[0] == "exec" && call.Args[2] == "bash" {
			script = call.Args[len(call.Args)-1]
		}
	}
	if script == "" {
		t.Fatal("enableScreenSharing should issue a tart exec bash script")
	}
	if !strings.Contains(script, "com.apple.screensharing") {
		t.Errorf("script should enable com.apple.screensharing\nscript: %s", script)
	}
	// Must verify the VNC listener actually came up and fail otherwise, so a lease
	// is never reported ready with Screen Sharing down.
	if !strings.Contains(script, "nc -z 127.0.0.1 5900") || !strings.Contains(script, "exit 1") {
		t.Errorf("script should verify the VNC listener and fail if absent\nscript: %s", script)
	}
	// No crabbox-managed VNC credential: nothing secret reaches the guest, and the
	// account password is never reset (that breaks secure-token accounts).
	for _, banned := range []string{"vnc.password", "dscl", "-passwd"} {
		if strings.Contains(script, banned) {
			t.Errorf("script should not reference %q\nscript: %s", banned, script)
		}
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

func TestServerFromInstanceClaimLabelsPreserved(t *testing.T) {
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

func TestApplyDefaultsInheritsExplicitSSHUser(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.SSHUser = "deployment"
	cfg.Tart = core.TartConfig{}
	applyDefaults(&cfg)
	if cfg.Tart.User != "deployment" {
		t.Fatalf("Tart.User = %q, want deployment (should inherit non-default SSHUser)", cfg.Tart.User)
	}
	if cfg.SSHUser != "deployment" {
		t.Fatalf("SSHUser = %q, want deployment", cfg.SSHUser)
	}
}

func TestApplyDefaultsIgnoresBaseSSHUser(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Tart = core.TartConfig{}
	applyDefaults(&cfg)
	if cfg.Tart.User != "admin" {
		t.Fatalf("Tart.User = %q, want admin (should not inherit base-config SSHUser 'crabbox')", cfg.Tart.User)
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

func TestResolveInstanceMissingVMReturnsClaim(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateDir)
	err := core.ClaimLeaseForRepoProviderScopePond(
		"cbx_stale999", "stale-slug", providerName, "instance:crabbox-gone-xyz", "", t.TempDir(), 30*time.Minute, false,
	)
	if err != nil {
		t.Fatalf("setup claim: %v", err)
	}

	listJSON := `[]`
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"list", "--source", "local", "--format", "json"}): {Stdout: listJSON},
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	inst, _, claim, err := b.resolveInstance(context.Background(), "cbx_stale999")
	if err != nil {
		t.Fatalf("resolveInstance should return stale claim, got error: %v", err)
	}
	if inst.Name != "crabbox-gone-xyz" {
		t.Fatalf("inst.Name = %q, want crabbox-gone-xyz", inst.Name)
	}
	if inst.State != "missing" {
		t.Fatalf("inst.State = %q, want missing", inst.State)
	}
	if claim.LeaseID != "cbx_stale999" {
		t.Fatalf("claim.LeaseID = %q, want cbx_stale999", claim.LeaseID)
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
		{"zero memory", []string{"--tart-memory", "0"}, "--tart-memory must be at least 4096 MB"},
		{"negative memory", []string{"--tart-memory", "-1"}, "--tart-memory must be at least 4096 MB"},
		{"below minimum memory", []string{"--tart-memory", "1"}, "--tart-memory must be at least 4096 MB"},
		{"negative disk", []string{"--tart-disk", "-1"}, "--tart-disk must be non-negative"},
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

func TestApplyFlagsDiskZeroUsesCloneDefault(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	vals := registerFlags(fs, cfg)
	if err := fs.Parse([]string{"--tart-disk", "0"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := applyFlags(&cfg, fs, vals); err != nil {
		t.Fatalf("applyFlags should accept --tart-disk 0 (clone default): %v", err)
	}
	if cfg.Tart.Disk != 0 {
		t.Fatalf("Disk = %d, want 0", cfg.Tart.Disk)
	}
	if core.IsTartDiskExplicit(&cfg) {
		t.Fatal("disk=0 should not be marked explicit")
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

func TestApplyFlagsRejectsInvalidEnvValues(t *testing.T) {
	cases := []struct {
		name    string
		envName string
		value   string
		want    string
	}{
		{"zero cpu from env", "CRABBOX_TART_CPUS", "0", "tart cpu count must be at least 4"},
		{"zero memory from env", "CRABBOX_TART_MEMORY", "0", "tart memory must be at least 4096"},
		{"zero disk from env", "CRABBOX_TART_DISK", "0", "tart disk size must be a positive integer"},
		{"non-integer cpu from env", "CRABBOX_TART_CPUS", "abc", "CRABBOX_TART_CPUS must be a valid integer"},
		{"non-integer memory from env", "CRABBOX_TART_MEMORY", "4.5g", "CRABBOX_TART_MEMORY must be a valid integer"},
		{"non-integer disk from env", "CRABBOX_TART_DISK", "fifty", "CRABBOX_TART_DISK must be a valid integer"},
		{"below-floor cpu from env", "CRABBOX_TART_CPUS", "2", "tart cpu count must be at least 4"},
		{"below-floor memory from env", "CRABBOX_TART_MEMORY", "1024", "tart memory must be at least 4096"},
		{"negative disk from env", "CRABBOX_TART_DISK", "-1", "tart disk size must be a positive integer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.envName, tc.value)
			cfg := core.BaseConfig()
			cfg.Provider = providerName
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			vals := registerFlags(fs, cfg)
			if err := fs.Parse(nil); err != nil {
				t.Fatalf("parse: %v", err)
			}
			err := applyFlags(&cfg, fs, vals)
			if err == nil {
				t.Fatalf("expected error for %s=%s", tc.envName, tc.value)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
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
		t.Fatal("ReleaseLease should propagate deleteVM failure")
	}
	if !strings.Contains(err.Error(), "tart delete") {
		t.Fatalf("error should mention tart delete: %v", err)
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

func TestResolveRejectsStoppedVMForRun(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	listJSON := `[{"Name":"crabbox-stopped-abc","State":"stopped","Running":false,"Disk":50,"Size":12,"Source":"ghcr.io/test:latest"}]`
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"list", "--source", "local", "--format", "json"}): {Stdout: listJSON},
			commandKey([]string{"ip", "crabbox-stopped-abc"}):                     {Stdout: "\n"},
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	err := core.ClaimLeaseForRepoProviderScopePond(
		"cbx_stopped", "stopped", providerName, "instance:crabbox-stopped-abc", "", t.TempDir(), 0, false,
	)
	if err != nil {
		t.Fatalf("setup claim: %v", err)
	}
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	_, err = b.Resolve(context.Background(), core.ResolveRequest{
		ID: "cbx_stopped",
	})
	if err == nil {
		t.Fatal("Resolve should reject stopped VM by default (SSH-target commands)")
	}
	if !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("error should mention stopped: %v", err)
	}
}

func TestResolveAllowsStoppedVMForStatus(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	listJSON := `[{"Name":"crabbox-stopped-abc","State":"stopped","Running":false,"Disk":50,"Size":12,"Source":"ghcr.io/test:latest"}]`
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"list", "--source", "local", "--format", "json"}): {Stdout: listJSON},
			commandKey([]string{"ip", "crabbox-stopped-abc"}):                     {Stdout: "\n"},
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	err := core.ClaimLeaseForRepoProviderScopePond(
		"cbx_stopped2", "stopped2", providerName, "instance:crabbox-stopped-abc", "", t.TempDir(), 0, false,
	)
	if err != nil {
		t.Fatalf("setup claim: %v", err)
	}
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "cbx_stopped2", StatusOnly: true})
	if err != nil {
		t.Fatalf("Resolve should allow stopped VM for status (StatusOnly=true): %v", err)
	}
	if lease.Server.Status != "stopped" {
		t.Fatalf("Server.Status = %q, want stopped", lease.Server.Status)
	}
}

func TestResolveRejectsStoppedVMForSSHTargetCommands(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	listJSON := `[{"Name":"crabbox-stopped-abc","State":"stopped","Running":false,"Disk":50,"Size":12,"Source":"ghcr.io/test:latest"}]`
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"list", "--source", "local", "--format", "json"}): {Stdout: listJSON},
			commandKey([]string{"ip", "crabbox-stopped-abc"}):                     {Stdout: "\n"},
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	err := core.ClaimLeaseForRepoProviderScopePond(
		"cbx_stopped3", "stopped3", providerName, "instance:crabbox-stopped-abc", "", t.TempDir(), 0, false,
	)
	if err != nil {
		t.Fatalf("setup claim: %v", err)
	}
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	// SSH-target commands (ssh, vnc, code, cache, egress) call Resolve without
	// Repo.Root and without StatusOnly. They must still be rejected for stopped VMs.
	_, err = b.Resolve(context.Background(), core.ResolveRequest{ID: "cbx_stopped3"})
	if err == nil {
		t.Fatal("Resolve should reject stopped VM for SSH-target commands (no StatusOnly)")
	}
	if !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("error should mention stopped: %v", err)
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

func TestStopVMErrorPropagation(t *testing.T) {
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

func TestCloneVMErrorPropagation(t *testing.T) {
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

func TestListInstancesInvalidJSONError(t *testing.T) {
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

func TestResolveReleaseOnlyRejectsUnclaimedVM(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateDir)

	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"list", "--source", "local", "--format", "json"}): {Stdout: sampleListJSON()},
		commandKey([]string{"ip", "crabbox-blue-1234abcd"}):                   {Stdout: "192.168.64.5\n"},
	}}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	_, err := b.Resolve(context.Background(), core.ResolveRequest{
		ID:          "crabbox-blue-1234abcd",
		ReleaseOnly: true,
	})
	if err == nil {
		t.Fatal("Resolve(ReleaseOnly) must reject an unclaimed VM, but returned nil error")
	}
	if !strings.Contains(err.Error(), "no Crabbox lease claim") {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(err.Error(), "crabbox stop") {
		t.Fatalf("unclaimed VM error must not suggest crabbox stop (circular path): %v", err)
	}
	if !strings.Contains(err.Error(), "tart stop") {
		t.Fatalf("unclaimed VM error must suggest tart stop as the direct cleanup path: %v", err)
	}
}

func sampleListJSON() string {
	return `[{"Name":"crabbox-blue-1234abcd","State":"running","Running":true,"Disk":50,"Size":15,"Source":"ghcr.io/cirruslabs/macos-sequoia-base:latest"},{"Name":"my-dev-vm","State":"stopped","Running":false,"Disk":50,"Size":12,"Source":"ghcr.io/cirruslabs/macos-sequoia-base:latest"}]`
}

// --- Mutation-testing-driven error propagation tests ---

func TestListPropagatesListInstancesError(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"list", "--source", "local", "--format", "json"}): {ExitCode: 1, Stderr: "tart not found"},
		},
		errors: map[string]error{
			commandKey([]string{"list", "--source", "local", "--format", "json"}): fmt.Errorf("exit status 1"),
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	_, err := b.List(context.Background(), core.ListRequest{})
	if err == nil {
		t.Fatal("List should propagate listInstances error")
	}
}

func TestDoctorPropagatesVersionError(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"--version"}): {ExitCode: 1, Stderr: "not found"},
		},
		errors: map[string]error{
			commandKey([]string{"--version"}): fmt.Errorf("exit status 1"),
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	_, err := b.Doctor(context.Background(), core.DoctorRequest{})
	if err == nil {
		t.Fatal("Doctor should propagate tart --version error")
	}
}

func TestDoctorPropagatesListError(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"--version"}):                                     {Stdout: "2.32.1"},
			commandKey([]string{"list", "--source", "local", "--format", "json"}): {ExitCode: 1},
		},
		errors: map[string]error{
			commandKey([]string{"list", "--source", "local", "--format", "json"}): fmt.Errorf("exit status 1"),
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	_, err := b.Doctor(context.Background(), core.DoctorRequest{})
	if err == nil {
		t.Fatal("Doctor should propagate listInstances error")
	}
}

func TestDoctorCountsCrabboxVMs(t *testing.T) {
	listJSON := `[{"Name":"crabbox-a-1","State":"running","Running":true,"Disk":50,"Size":10,"Source":"test"},{"Name":"crabbox-b-2","State":"stopped","Running":false,"Disk":50,"Size":10,"Source":"test"},{"Name":"my-vm","State":"running","Running":true,"Disk":50,"Size":10,"Source":"test"}]`
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"--version"}):                                     {Stdout: "2.32.1"},
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
	if !strings.Contains(result.Message, "leases=2") {
		t.Fatalf("Doctor should count 2 crabbox VMs, got: %s", result.Message)
	}
}

func TestDoctorProbeSSHFlag(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"--version"}):                                     {Stdout: "2.32.1"},
			commandKey([]string{"list", "--source", "local", "--format", "json"}): {Stdout: "[]"},
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	result, err := b.Doctor(context.Background(), core.DoctorRequest{ProbeSSH: true})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if !strings.Contains(result.Message, "ssh_probe=requires_running_lease") {
		t.Fatalf("Doctor(ProbeSSH=true) should report requires_running_lease, got: %s", result.Message)
	}
}

func TestReleaseLeaseFallsBackToResolve(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	err := core.ClaimLeaseForRepoProviderScopePond(
		"cbx_rel123", "slug", providerName, "instance:crabbox-rel-vm", "", t.TempDir(), 30*time.Minute, false,
	)
	if err != nil {
		t.Fatalf("setup claim: %v", err)
	}

	listJSON := `[{"Name":"crabbox-rel-vm","State":"running","Running":true,"Disk":50,"Size":10,"Source":"test"}]`
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"list", "--source", "local", "--format", "json"}): {Stdout: listJSON},
			commandKey([]string{"ip", "crabbox-rel-vm"}):                          {Stdout: "192.168.64.10"},
			commandKey([]string{"stop", "crabbox-rel-vm"}):                        {},
			commandKey([]string{"delete", "crabbox-rel-vm"}):                      {},
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	err = b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{
		Lease: core.LeaseTarget{
			LeaseID: "cbx_rel123",
		},
	})
	if err != nil {
		t.Fatalf("ReleaseLease fallback resolve: %v", err)
	}
}

func TestReleaseLeasePrunesMissingResolvedInstance(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	err := core.ClaimLeaseForRepoProviderScopePond(
		"cbx_missingrel", "missing-rel", providerName, "instance:crabbox-missing-rel", "", t.TempDir(), 30*time.Minute, false,
	)
	if err != nil {
		t.Fatalf("setup claim: %v", err)
	}
	keyPath, err := testboxKeyPath("cbx_missingrel")
	if err != nil {
		t.Fatalf("testbox key path: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatalf("create key dir: %v", err)
	}
	if err := os.WriteFile(keyPath, []byte("test-key"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"list", "--source", "local", "--format", "json"}): {Stdout: `[]`},
		},
		errors: map[string]error{
			commandKey([]string{"delete", "crabbox-missing-rel"}): fmt.Errorf("delete should not be called"),
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	err = b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{
		Lease: core.LeaseTarget{
			LeaseID: "missing-rel",
		},
	})
	if err != nil {
		t.Fatalf("ReleaseLease missing resolved instance: %v", err)
	}
	for _, call := range runner.calls {
		if commandKey(call.Args) == commandKey([]string{"delete", "crabbox-missing-rel"}) {
			t.Fatal("ReleaseLease should not delete an already-missing resolved VM")
		}
	}
	if _, ok, err := resolveLeaseClaimForProvider("cbx_missingrel", providerName); err != nil {
		t.Fatalf("resolve claim: %v", err)
	} else if ok {
		t.Fatal("ReleaseLease should prune the stale claim")
	}
	if _, err := os.Stat(filepath.Dir(keyPath)); !os.IsNotExist(err) {
		t.Fatalf("ReleaseLease should remove stored key dir, stat err=%v", err)
	}
}

func TestReleaseLeasePrunesAlreadyResolvedMissingInstance(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	err := core.ClaimLeaseForRepoProviderScopePond(
		"cbx_resolvedmissing", "resolved-missing", providerName, "instance:crabbox-resolved-missing", "", t.TempDir(), 30*time.Minute, false,
	)
	if err != nil {
		t.Fatalf("setup claim: %v", err)
	}
	keyPath, err := testboxKeyPath("cbx_resolvedmissing")
	if err != nil {
		t.Fatalf("testbox key path: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatalf("create key dir: %v", err)
	}
	if err := os.WriteFile(keyPath, []byte("test-key"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	runner := &recordingRunner{
		errors: map[string]error{
			commandKey([]string{"delete", "crabbox-resolved-missing"}): fmt.Errorf("delete should not be called"),
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	err = b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{
		Lease: core.LeaseTarget{
			LeaseID: "cbx_resolvedmissing",
			Server: core.Server{
				CloudID: "crabbox-resolved-missing",
				Status:  "missing",
			},
		},
	})
	if err != nil {
		t.Fatalf("ReleaseLease resolved missing instance: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("ReleaseLease should not call tart for already-resolved missing instance, calls=%v", runner.calls)
	}
	if _, ok, err := resolveLeaseClaimForProvider("cbx_resolvedmissing", providerName); err != nil {
		t.Fatalf("resolve claim: %v", err)
	} else if ok {
		t.Fatal("ReleaseLease should prune the stale claim")
	}
	if _, err := os.Stat(filepath.Dir(keyPath)); !os.IsNotExist(err) {
		t.Fatalf("ReleaseLease should remove stored key dir, stat err=%v", err)
	}
}

func TestReleaseLeaseEmptyName(t *testing.T) {
	runner := &recordingRunner{}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{
		Lease: core.LeaseTarget{},
	})
	if err == nil {
		t.Fatal("ReleaseLease with empty name should error")
	}
	if !strings.Contains(err.Error(), "requires a tart instance name") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReleaseLeaseFromLabels(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"stop", "crabbox-lab-vm"}):   {},
			commandKey([]string{"delete", "crabbox-lab-vm"}): {},
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{
		Lease: core.LeaseTarget{
			LeaseID: "cbx_lab456",
			Server:  core.Server{Labels: map[string]string{"instance": "crabbox-lab-vm", "lease": "cbx_lab456"}},
		},
	})
	if err != nil {
		t.Fatalf("ReleaseLease from labels: %v", err)
	}
}

func TestReleaseLeaseIDFromLabel(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"stop", "crabbox-noid-vm"}):   {},
			commandKey([]string{"delete", "crabbox-noid-vm"}): {},
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{
		Lease: core.LeaseTarget{
			Server: core.Server{
				CloudID: "crabbox-noid-vm",
				Labels:  map[string]string{"lease": "cbx_fromlab"},
			},
		},
	})
	if err != nil {
		t.Fatalf("ReleaseLease should derive leaseID from label: %v", err)
	}
}

func TestShouldCleanupEdgeCases(t *testing.T) {
	now := time.Now().UTC()

	t.Run("keep=true prevents cleanup", func(t *testing.T) {
		server := core.Server{Status: "stopped", Labels: map[string]string{"keep": "true"}}
		shouldDelete, reason := shouldCleanup(server, core.LeaseClaim{}, false, now)
		if shouldDelete {
			t.Fatal("should not cleanup keep=true")
		}
		if reason != "keep=true" {
			t.Fatalf("reason=%q want keep=true", reason)
		}
	})

	t.Run("stopped VM without claim", func(t *testing.T) {
		server := core.Server{Status: "stopped", Labels: map[string]string{}}
		shouldDelete, _ := shouldCleanup(server, core.LeaseClaim{}, false, now)
		if !shouldDelete {
			t.Fatal("stopped VM without claim should be cleaned up")
		}
	})

	t.Run("running VM without claim", func(t *testing.T) {
		server := core.Server{Status: "running", Labels: map[string]string{}}
		shouldDelete, reason := shouldCleanup(server, core.LeaseClaim{}, false, now)
		if shouldDelete {
			t.Fatalf("running VM without claim should not be cleaned up, reason=%s", reason)
		}
		if reason != "missing claim" {
			t.Fatalf("reason=%q want missing claim", reason)
		}
	})

	t.Run("expired claim", func(t *testing.T) {
		expiredTime := now.Add(-24 * time.Hour).Format(time.RFC3339)
		claim := core.LeaseClaim{
			LeaseID:            "cbx_exp",
			LastUsedAt:         expiredTime,
			IdleTimeoutSeconds: 1800,
		}
		server := core.Server{Status: "running", Labels: map[string]string{}}
		shouldDelete, reason := shouldCleanup(server, claim, true, now)
		if !shouldDelete {
			t.Fatal("expired claim should trigger cleanup")
		}
		if reason != "claim expired" {
			t.Fatalf("reason=%q want claim expired", reason)
		}
	})

	t.Run("active claim within idle window", func(t *testing.T) {
		recentTime := now.Add(-5 * time.Minute).Format(time.RFC3339)
		claim := core.LeaseClaim{
			LeaseID:            "cbx_act",
			LastUsedAt:         recentTime,
			IdleTimeoutSeconds: 1800,
		}
		server := core.Server{Status: "running", Labels: map[string]string{}}
		shouldDelete, reason := shouldCleanup(server, claim, true, now)
		if shouldDelete {
			t.Fatal("active claim should not trigger cleanup")
		}
		if reason != "claim active" {
			t.Fatalf("reason=%q want claim active", reason)
		}
	})

	t.Run("claim with zero idle timeout", func(t *testing.T) {
		recentTime := now.Add(-1 * time.Hour).Format(time.RFC3339)
		claim := core.LeaseClaim{
			LeaseID:            "cbx_zero",
			LastUsedAt:         recentTime,
			IdleTimeoutSeconds: 0,
		}
		server := core.Server{Status: "running", Labels: map[string]string{}}
		shouldDelete, reason := shouldCleanup(server, claim, true, now)
		if shouldDelete {
			t.Fatal("zero idle timeout should keep claim active")
		}
		if reason != "claim active" {
			t.Fatalf("reason=%q want claim active", reason)
		}
	})

	t.Run("claim with unparseable last_used_at", func(t *testing.T) {
		claim := core.LeaseClaim{
			LeaseID:            "cbx_bad",
			LastUsedAt:         "not-a-date",
			IdleTimeoutSeconds: 1800,
		}
		server := core.Server{Status: "running", Labels: map[string]string{}}
		shouldDelete, reason := shouldCleanup(server, claim, true, now)
		if shouldDelete {
			t.Fatal("unparseable last_used_at should keep claim active")
		}
		if reason != "claim active" {
			t.Fatalf("reason=%q want claim active", reason)
		}
	})
}

func TestInstanceRunningFunction(t *testing.T) {
	cases := []struct {
		state string
		want  bool
	}{
		{"running", true},
		{"ready", true},
		{"stopped", false},
		{"", false},
		{"suspended", false},
		{"unknown", false},
	}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			got := instanceRunning(tc.state)
			if got != tc.want {
				t.Fatalf("instanceRunning(%q) = %v, want %v", tc.state, got, tc.want)
			}
		})
	}
}

func TestGetIPNormalizesEmpty(t *testing.T) {
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
		t.Fatalf("getIP should normalize '--' to empty, got=%q", ip)
	}
}

func TestListFiltersNonCrabboxWithoutClaim(t *testing.T) {
	listJSON := `[{"Name":"my-dev-vm","State":"running","Running":true,"Disk":50,"Size":10,"Source":"test"},{"Name":"crabbox-x-1","State":"running","Running":true,"Disk":50,"Size":10,"Source":"test"}]`
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"list", "--source", "local", "--format", "json"}): {Stdout: listJSON},
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	views, err := b.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("List should return 1 VM (filtering out non-crabbox without claim), got %d", len(views))
	}
	if views[0].CloudID != "crabbox-x-1" {
		t.Fatalf("unexpected VM: %s", views[0].CloudID)
	}
}

func TestResolveStoppedVMForStatusOnly(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	err := core.ClaimLeaseForRepoProviderScopePond(
		"cbx_stopped1", "stopped-slug", providerName, "instance:crabbox-stopped-vm", "", t.TempDir(), 30*time.Minute, false,
	)
	if err != nil {
		t.Fatalf("setup claim: %v", err)
	}

	listJSON := `[{"Name":"crabbox-stopped-vm","State":"stopped","Running":false,"Disk":50,"Size":10,"Source":"test"}]`
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"list", "--source", "local", "--format", "json"}): {Stdout: listJSON},
			commandKey([]string{"ip", "crabbox-stopped-vm"}):                      {Stdout: ""},
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	lease, err := b.Resolve(context.Background(), core.ResolveRequest{
		ID:         "cbx_stopped1",
		StatusOnly: true,
	})
	if err != nil {
		t.Fatalf("Resolve(StatusOnly) should succeed for stopped VM: %v", err)
	}
	if lease.LeaseID != "cbx_stopped1" {
		t.Fatalf("lease.LeaseID=%q want cbx_stopped1", lease.LeaseID)
	}
}

func TestResolveStoppedVMWithoutStatusOnlyErrors(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	err := core.ClaimLeaseForRepoProviderScopePond(
		"cbx_stopped2", "stopped-slug2", providerName, "instance:crabbox-stopped-vm2", "", t.TempDir(), 30*time.Minute, false,
	)
	if err != nil {
		t.Fatalf("setup claim: %v", err)
	}

	listJSON := `[{"Name":"crabbox-stopped-vm2","State":"stopped","Running":false,"Disk":50,"Size":10,"Source":"test"}]`
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"list", "--source", "local", "--format", "json"}): {Stdout: listJSON},
			commandKey([]string{"ip", "crabbox-stopped-vm2"}):                     {Stdout: ""},
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	_, err = b.Resolve(context.Background(), core.ResolveRequest{
		ID: "cbx_stopped2",
	})
	if err == nil {
		t.Fatal("Resolve of stopped VM without StatusOnly should error")
	}
	if !strings.Contains(err.Error(), "is stopped") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveRunningVMByStateNotBool(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	err := core.ClaimLeaseForRepoProviderScopePond(
		"cbx_nobool1", "nobool-slug", providerName, "instance:crabbox-nobool-vm", "", t.TempDir(), 30*time.Minute, false,
	)
	if err != nil {
		t.Fatalf("setup claim: %v", err)
	}

	listJSON := `[{"Name":"crabbox-nobool-vm","State":"running","Disk":50,"Size":10,"Source":"test"}]`
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"list", "--source", "local", "--format", "json"}): {Stdout: listJSON},
			commandKey([]string{"ip", "crabbox-nobool-vm"}):                       {Stdout: "192.168.64.20\n"},
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	lease, err := b.Resolve(context.Background(), core.ResolveRequest{
		ID: "cbx_nobool1",
	})
	if err != nil {
		t.Fatalf("Resolve should succeed when Running omitted but State=running: %v", err)
	}
	if lease.LeaseID != "cbx_nobool1" {
		t.Fatalf("lease.LeaseID=%q want cbx_nobool1", lease.LeaseID)
	}
}

func TestResolveRunningVMByStateEmptyIPErrors(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	err := core.ClaimLeaseForRepoProviderScopePond(
		"cbx_noip1", "noip-slug", providerName, "instance:crabbox-noip-vm", "", t.TempDir(), 30*time.Minute, false,
	)
	if err != nil {
		t.Fatalf("setup claim: %v", err)
	}

	listJSON := `[{"Name":"crabbox-noip-vm","State":"running","Disk":50,"Size":10,"Source":"test"}]`
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"list", "--source", "local", "--format", "json"}): {Stdout: listJSON},
			commandKey([]string{"ip", "crabbox-noip-vm"}):                         {Stdout: "\n"},
		},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	_, err = b.Resolve(context.Background(), core.ResolveRequest{
		ID: "cbx_noip1",
	})
	if err == nil {
		t.Fatal("Resolve should fail when State=running but IP is empty")
	}
	if !strings.Contains(err.Error(), "no IP address") {
		t.Fatalf("error should mention no IP address, got: %v", err)
	}
}

func TestServerFromInstanceLabels(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	applyDefaults(&cfg)
	runner := &recordingRunner{}
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	inst := tartInstance{Name: "crabbox-lbl-test", State: "running", Running: true, Source: "test-image"}
	claim := core.LeaseClaim{LeaseID: "cbx_lbl", Slug: "lbl-slug"}
	server := b.serverFromInstance(inst, claim, cfg)

	if server.Labels["provider"] != providerName {
		t.Fatalf("provider=%q", server.Labels["provider"])
	}
	if server.Labels["instance"] != "crabbox-lbl-test" {
		t.Fatalf("instance=%q", server.Labels["instance"])
	}
	if server.Labels["lease"] != "cbx_lbl" {
		t.Fatalf("lease=%q", server.Labels["lease"])
	}
	if server.Labels["slug"] != "lbl-slug" {
		t.Fatalf("slug=%q", server.Labels["slug"])
	}
	if server.Labels["ssh_user"] != "admin" {
		t.Fatalf("ssh_user=%q", server.Labels["ssh_user"])
	}
	if server.Labels["ssh_port"] != sshPort {
		t.Fatalf("ssh_port=%q", server.Labels["ssh_port"])
	}
}

func TestProviderClaimsFiltersByProvider(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	err := core.ClaimLeaseForRepoProviderScopePond(
		"cbx_tart1", "s1", providerName, "instance:crabbox-tart1", "", t.TempDir(), 30*time.Minute, false,
	)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	err = core.ClaimLeaseForRepoProviderScopePond(
		"cbx_other1", "s2", "hetzner", "server:htz1", "", t.TempDir(), 30*time.Minute, false,
	)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	claims, err := providerClaims()
	if err != nil {
		t.Fatalf("providerClaims: %v", err)
	}
	if _, ok := claims["crabbox-tart1"]; !ok {
		t.Fatal("should include tart provider claim")
	}
	for name, claim := range claims {
		if claim.Provider != providerName {
			t.Fatalf("providerClaims included non-tart claim: %s provider=%s", name, claim.Provider)
		}
	}
}

func TestApplyDefaultsSetsAllFields(t *testing.T) {
	cfg := core.Config{}
	applyDefaults(&cfg)
	if cfg.Provider != providerName {
		t.Fatalf("Provider=%q", cfg.Provider)
	}
	if cfg.TargetOS != targetMacOS {
		t.Fatalf("TargetOS=%q", cfg.TargetOS)
	}
	if cfg.WindowsMode != "" {
		t.Fatalf("WindowsMode=%q", cfg.WindowsMode)
	}
	if len(cfg.SSHFallbackPorts) != 0 {
		t.Fatalf("SSHFallbackPorts=%v", cfg.SSHFallbackPorts)
	}
	if cfg.Tart.CPUs != 4 {
		t.Fatalf("CPUs=%d", cfg.Tart.CPUs)
	}
	if cfg.Tart.Memory != 8192 {
		t.Fatalf("Memory=%d", cfg.Tart.Memory)
	}
	if cfg.SSHPort != sshPort {
		t.Fatalf("SSHPort=%q", cfg.SSHPort)
	}
	if cfg.ServerType != cfg.Tart.Image {
		t.Fatalf("ServerType=%q != Image=%q", cfg.ServerType, cfg.Tart.Image)
	}
}

func TestApplyDefaultsPreservesTartUser(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Tart.User = "custom"
	applyDefaults(&cfg)
	if cfg.Tart.User != "custom" {
		t.Fatalf("Tart.User should preserve explicit value, got=%q", cfg.Tart.User)
	}
	if cfg.SSHUser != "custom" {
		t.Fatalf("SSHUser should match Tart.User, got=%q", cfg.SSHUser)
	}
}

func TestApplyFlagsTargetOSConversion(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = "linux"
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	vals := registerFlags(fs, cfg)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := applyFlags(&cfg, fs, vals); err != nil {
		t.Fatalf("applyFlags: %v", err)
	}
	if cfg.TargetOS != targetMacOS {
		t.Fatalf("non-explicit linux should convert to macos, got=%q", cfg.TargetOS)
	}
}

func TestConfigureVMSkipsDiskWhenNotExplicit(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Tart.Disk = 100
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)

	if err := b.configureVM(context.Background(), cfg, "test-vm"); err != nil {
		t.Fatalf("configureVM: %v", err)
	}
	for _, call := range runner.calls {
		if len(call.Args) > 1 && call.Args[1] == "--disk-size" {
			t.Fatal("configureVM should not set disk when not explicitly marked")
		}
	}
}

func TestValidateTartEnvIntPositive(t *testing.T) {
	t.Setenv("CRABBOX_TART_DISK", "0")
	err := validateTartEnvIntPositive("CRABBOX_TART_DISK", "disk must be positive")
	if err == nil {
		t.Fatal("validateTartEnvIntPositive should reject 0")
	}

	t.Setenv("CRABBOX_TART_DISK", "50")
	err = validateTartEnvIntPositive("CRABBOX_TART_DISK", "disk must be positive")
	if err != nil {
		t.Fatalf("should accept 50: %v", err)
	}

	t.Setenv("CRABBOX_TART_DISK", "abc")
	err = validateTartEnvIntPositive("CRABBOX_TART_DISK", "disk must be positive")
	if err == nil {
		t.Fatal("should reject non-integer")
	}
}

func TestValidateTartEnvInt(t *testing.T) {
	t.Setenv("CRABBOX_TART_CPUS", "2")
	err := validateTartEnvInt("CRABBOX_TART_CPUS", 4, "cpu must be at least 4")
	if err == nil {
		t.Fatal("should reject value below floor")
	}

	t.Setenv("CRABBOX_TART_CPUS", "8")
	err = validateTartEnvInt("CRABBOX_TART_CPUS", 4, "cpu must be at least 4")
	if err != nil {
		t.Fatalf("should accept 8: %v", err)
	}

	t.Setenv("CRABBOX_TART_CPUS", "")
	err = validateTartEnvInt("CRABBOX_TART_CPUS", 4, "cpu must be at least 4")
	if err != nil {
		t.Fatalf("should skip empty env: %v", err)
	}
}

// --- Mutation round 3: error propagation, boundary conditions, helper coverage ---

func TestConfigureVMCPUSetError(t *testing.T) {
	runner := &recordingRunner{
		errors: map[string]error{
			commandKey([]string{"set", "test-vm", "--cpu", "8"}): fmt.Errorf("cpu set failed"),
		},
	}
	cfg := core.BaseConfig()
	cfg.Tart.CPUs = 8
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	err := b.configureVM(context.Background(), cfg, "test-vm")
	if err == nil {
		t.Fatal("configureVM should propagate CPU set error")
	}
	if !strings.Contains(err.Error(), "tart set --cpu") {
		t.Fatalf("error should mention tart set --cpu: %v", err)
	}
}

func TestConfigureVMMemorySetError(t *testing.T) {
	runner := &recordingRunner{
		errors: map[string]error{
			commandKey([]string{"set", "test-vm", "--memory", "8192"}): fmt.Errorf("memory set failed"),
		},
	}
	cfg := core.BaseConfig()
	cfg.Tart.CPUs = 0
	cfg.Tart.Memory = 8192
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	err := b.configureVM(context.Background(), cfg, "test-vm")
	if err == nil {
		t.Fatal("configureVM should propagate memory set error")
	}
	if !strings.Contains(err.Error(), "tart set --memory") {
		t.Fatalf("error should mention tart set --memory: %v", err)
	}
}

func TestConfigureVMDiskSetError(t *testing.T) {
	runner := &recordingRunner{
		errors: map[string]error{
			commandKey([]string{"set", "test-vm", "--disk-size", "100"}): fmt.Errorf("disk set failed"),
		},
	}
	cfg := core.BaseConfig()
	cfg.Tart.CPUs = 0
	cfg.Tart.Memory = 0
	cfg.Tart.Disk = 100
	core.MarkTartDiskExplicit(&cfg)
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	err := b.configureVM(context.Background(), cfg, "test-vm")
	if err == nil {
		t.Fatal("configureVM should propagate disk set error")
	}
	if !strings.Contains(err.Error(), "tart set --disk-size") {
		t.Fatalf("error should mention tart set --disk-size: %v", err)
	}
}

func TestConfigureVMAllSettingsApplied(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{},
	}
	cfg := core.BaseConfig()
	cfg.Tart.CPUs = 8
	cfg.Tart.Memory = 16384
	cfg.Tart.Disk = 200
	core.MarkTartDiskExplicit(&cfg)
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	err := b.configureVM(context.Background(), cfg, "test-vm")
	if err != nil {
		t.Fatalf("configureVM: %v", err)
	}
	cpuSet, memSet, diskSet := false, false, false
	for _, call := range runner.calls {
		if len(call.Args) >= 4 && call.Args[0] == "set" {
			switch call.Args[2] {
			case "--cpu":
				cpuSet = true
				if call.Args[3] != "8" {
					t.Fatalf("CPU value = %s, want 8", call.Args[3])
				}
			case "--memory":
				memSet = true
				if call.Args[3] != "16384" {
					t.Fatalf("memory value = %s, want 16384", call.Args[3])
				}
			case "--disk-size":
				diskSet = true
				if call.Args[3] != "200" {
					t.Fatalf("disk value = %s, want 200", call.Args[3])
				}
			}
		}
	}
	if !cpuSet {
		t.Fatal("configureVM did not set CPU")
	}
	if !memSet {
		t.Fatal("configureVM did not set memory")
	}
	if !diskSet {
		t.Fatal("configureVM did not set disk")
	}
}

func TestConfigureVMSkipsZeroValues(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{},
	}
	cfg := core.BaseConfig()
	cfg.Tart.CPUs = 0
	cfg.Tart.Memory = 0
	cfg.Tart.Disk = 0
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	err := b.configureVM(context.Background(), cfg, "test-vm")
	if err != nil {
		t.Fatalf("configureVM: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("configureVM should make no calls with zero values, got %d", len(runner.calls))
	}
}

func TestCloneVMErrorDetail(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"clone", "ghcr.io/test:latest", "test-vm"}): {Stderr: "disk full"},
		},
		errors: map[string]error{
			commandKey([]string{"clone", "ghcr.io/test:latest", "test-vm"}): fmt.Errorf("clone failed"),
		},
	}
	cfg := core.BaseConfig()
	cfg.Tart.Image = "ghcr.io/test:latest"
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	err := b.cloneVM(context.Background(), cfg, "test-vm")
	if err == nil {
		t.Fatal("cloneVM should return error on failure")
	}
	if !strings.Contains(err.Error(), "tart clone") {
		t.Fatalf("error should mention tart clone: %v", err)
	}
}

func TestCloneVMSuccess(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"clone", "ghcr.io/test:latest", "test-vm"}): {},
		},
	}
	cfg := core.BaseConfig()
	cfg.Tart.Image = "ghcr.io/test:latest"
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	err := b.cloneVM(context.Background(), cfg, "test-vm")
	if err != nil {
		t.Fatalf("cloneVM should succeed: %v", err)
	}
}

func TestStopVMErrorMessage(t *testing.T) {
	runner := &recordingRunner{
		errors: map[string]error{
			commandKey([]string{"stop", "test-vm"}): fmt.Errorf("stop failed"),
		},
	}
	cfg := core.BaseConfig()
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	err := b.stopVM(context.Background(), "test-vm")
	if err == nil {
		t.Fatal("stopVM should return error on failure")
	}
	if !strings.Contains(err.Error(), "tart stop") {
		t.Fatalf("error should mention tart stop: %v", err)
	}
}

func TestDeleteVMError(t *testing.T) {
	runner := &recordingRunner{
		errors: map[string]error{
			commandKey([]string{"delete", "test-vm"}): fmt.Errorf("delete failed"),
		},
	}
	cfg := core.BaseConfig()
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	err := b.deleteVM(context.Background(), "test-vm")
	if err == nil {
		t.Fatal("deleteVM should return error on failure")
	}
	if !strings.Contains(err.Error(), "tart delete") {
		t.Fatalf("error should mention tart delete: %v", err)
	}
}

func TestInjectSSHKeyInvalidUser(t *testing.T) {
	runner := &recordingRunner{}
	cfg := core.BaseConfig()
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	err := b.injectSSHKey(context.Background(), "test-vm", "invalid user!", "ssh-rsa AAAA")
	if err == nil {
		t.Fatal("injectSSHKey should reject invalid POSIX username")
	}
	if !strings.Contains(err.Error(), "not a valid POSIX") {
		t.Fatalf("error should mention POSIX validation: %v", err)
	}
}

func TestInjectSSHKeyValidUser(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{},
	}
	cfg := core.BaseConfig()
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	err := b.injectSSHKey(context.Background(), "test-vm", "admin", "ssh-rsa AAAA")
	if err != nil {
		t.Fatalf("injectSSHKey should accept valid user: %v", err)
	}
	found := false
	for _, call := range runner.calls {
		if len(call.Args) > 0 && call.Args[0] == "exec" {
			found = true
		}
	}
	if !found {
		t.Fatal("injectSSHKey should call tart exec")
	}
}

func TestInjectSSHKeyExecError(t *testing.T) {
	runner := &recordingRunner{
		errors: map[string]error{
			"exec": fmt.Errorf("exec failed"),
		},
		responses: map[string]core.LocalCommandResult{
			"exec": {Stderr: "permission denied"},
		},
	}
	cfg := core.BaseConfig()
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	err := b.injectSSHKey(context.Background(), "test-vm", "admin", "ssh-rsa AAAA")
	if err == nil {
		t.Fatal("injectSSHKey should propagate exec error")
	}
	if !strings.Contains(err.Error(), "ssh key injection") {
		t.Fatalf("error should mention ssh key injection: %v", err)
	}
}

func TestCommandErrorWithStderr(t *testing.T) {
	result := core.LocalCommandResult{ExitCode: 5, Stderr: "some detail\n"}
	err := commandError("test-action", result, fmt.Errorf("wrapped"))
	if err == nil {
		t.Fatal("commandError should return non-nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "test-action failed") {
		t.Fatalf("should mention action: %s", msg)
	}
	if !strings.Contains(msg, "some detail") {
		t.Fatalf("should include stderr detail: %s", msg)
	}
}

func TestCommandErrorWithStdoutFallback(t *testing.T) {
	result := core.LocalCommandResult{ExitCode: 0, Stderr: "", Stdout: "stdout detail\n"}
	err := commandError("test-action", result, fmt.Errorf("wrapped"))
	if err == nil {
		t.Fatal("commandError should return non-nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "stdout detail") {
		t.Fatalf("should fallback to stdout: %s", msg)
	}
}

func TestCommandErrorNoDetail(t *testing.T) {
	result := core.LocalCommandResult{ExitCode: 0, Stderr: "", Stdout: ""}
	err := commandError("test-action", result, fmt.Errorf("original"))
	if err == nil {
		t.Fatal("commandError should return non-nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "test-action failed") {
		t.Fatalf("should mention action: %s", msg)
	}
	if !strings.Contains(msg, "original") {
		t.Fatalf("should include original error: %s", msg)
	}
}

func TestCommandErrorZeroExitCodeBecomesOne(t *testing.T) {
	result := core.LocalCommandResult{ExitCode: 0}
	err := commandError("action", result, fmt.Errorf("err"))
	var exitErr core.ExitError
	if !core.AsExitError(err, &exitErr) {
		t.Fatalf("expected ExitError, got %T", err)
	}
	if exitErr.Code == 0 {
		t.Fatal("exit code 0 should become non-zero")
	}
}

func TestFirstLineEmpty(t *testing.T) {
	if got := firstLine(""); got != "unknown" {
		t.Fatalf("firstLine(\"\") = %q, want \"unknown\"", got)
	}
	if got := firstLine("   "); got != "unknown" {
		t.Fatalf("firstLine(\"   \") = %q, want \"unknown\"", got)
	}
}

func TestFirstLineSingleLine(t *testing.T) {
	if got := firstLine("hello world"); got != "hello world" {
		t.Fatalf("firstLine(\"hello world\") = %q", got)
	}
}

func TestFirstLineMultiLine(t *testing.T) {
	if got := firstLine("first\nsecond\nthird"); got != "first" {
		t.Fatalf("firstLine multiline = %q, want \"first\"", got)
	}
}

func TestFirstNonBlankAllEmpty(t *testing.T) {
	if got := firstNonBlank("", "  ", "\t"); got != "" {
		t.Fatalf("firstNonBlank all empty = %q, want \"\"", got)
	}
}

func TestFirstNonBlankFindsFirst(t *testing.T) {
	if got := firstNonBlank("", "hello", "world"); got != "hello" {
		t.Fatalf("firstNonBlank = %q, want \"hello\"", got)
	}
}

func TestFirstNonBlankSingleValue(t *testing.T) {
	if got := firstNonBlank("only"); got != "only" {
		t.Fatalf("firstNonBlank(\"only\") = %q", got)
	}
}

func TestTartStateNormalization(t *testing.T) {
	cases := []struct {
		input, want string
	}{
		{"Running", "running"},
		{"STOPPED", "stopped"},
		{"  Ready  ", "ready"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := tartState(tc.input); got != tc.want {
			t.Fatalf("tartState(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestInstanceRunningVariousStates(t *testing.T) {
	cases := []struct {
		state string
		want  bool
	}{
		{"running", true},
		{"Running", true},
		{"  running  ", true},
		{"ready", true},
		{"Ready", true},
		{"stopped", false},
		{"suspended", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := instanceRunning(tc.state); got != tc.want {
			t.Fatalf("instanceRunning(%q) = %v, want %v", tc.state, got, tc.want)
		}
	}
}

func TestGetIPDashDashReturnsEmpty(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"ip", "test-vm"}): {Stdout: "--\n"},
		},
	}
	cfg := core.BaseConfig()
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	ip := b.getIP(context.Background(), "test-vm")
	if ip != "" {
		t.Fatalf("getIP should return empty for '--', got %q", ip)
	}
}

func TestGetIPValidAddress(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"ip", "test-vm"}): {Stdout: "192.168.64.5\n"},
		},
	}
	cfg := core.BaseConfig()
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	ip := b.getIP(context.Background(), "test-vm")
	if ip != "192.168.64.5" {
		t.Fatalf("getIP = %q, want 192.168.64.5", ip)
	}
}

func TestGetIPErrorReturnsEmpty(t *testing.T) {
	runner := &recordingRunner{
		errors: map[string]error{
			commandKey([]string{"ip", "test-vm"}): fmt.Errorf("not running"),
		},
	}
	cfg := core.BaseConfig()
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	ip := b.getIP(context.Background(), "test-vm")
	if ip != "" {
		t.Fatalf("getIP should return empty on error, got %q", ip)
	}
}

func TestConfigureTailscaleRejected(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Tailscale.Enabled = true
	_, err := (Provider{}).Configure(cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: &recordingRunner{}})
	if err == nil {
		t.Fatal("Configure should reject tailscale-enabled config")
	}
	if !strings.Contains(err.Error(), "tailscale") {
		t.Fatalf("error should mention tailscale: %v", err)
	}
}

func TestConfigureTailscaleNetwork(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Network = core.NetworkTailscale
	_, err := (Provider{}).Configure(cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: &recordingRunner{}})
	if err == nil {
		t.Fatal("Configure should reject network=tailscale")
	}
}

func TestApplyFlagsCPUExact4Accepted(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	vals := registerFlags(fs, cfg)
	if err := fs.Parse([]string{"--tart-cpu", "4"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := applyFlags(&cfg, fs, vals); err != nil {
		t.Fatalf("CPU=4 should be accepted: %v", err)
	}
	if cfg.Tart.CPUs != 4 {
		t.Fatalf("CPUs = %d, want 4", cfg.Tart.CPUs)
	}
}

func TestApplyFlagsMemoryExact4096Accepted(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	vals := registerFlags(fs, cfg)
	if err := fs.Parse([]string{"--tart-memory", "4096"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := applyFlags(&cfg, fs, vals); err != nil {
		t.Fatalf("Memory=4096 should be accepted: %v", err)
	}
	if cfg.Tart.Memory != 4096 {
		t.Fatalf("Memory = %d, want 4096", cfg.Tart.Memory)
	}
}

func TestApplyFlagsCPU3Rejected(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	vals := registerFlags(fs, cfg)
	if err := fs.Parse([]string{"--tart-cpu", "3"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	err := applyFlags(&cfg, fs, vals)
	if err == nil {
		t.Fatal("CPU=3 should be rejected (minimum is 4)")
	}
}

func TestApplyFlagsMemory4095Rejected(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	vals := registerFlags(fs, cfg)
	if err := fs.Parse([]string{"--tart-memory", "4095"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	err := applyFlags(&cfg, fs, vals)
	if err == nil {
		t.Fatal("Memory=4095 should be rejected (minimum is 4096)")
	}
}

func TestApplyFlagsWrongTypeIgnored(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	err := applyFlags(&cfg, fs, "not-a-flagValues")
	if err != nil {
		t.Fatalf("applyFlags with wrong type should be no-op, got: %v", err)
	}
}

func TestApplyFlagsImageExplicit(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	vals := registerFlags(fs, cfg)
	if err := fs.Parse([]string{"--tart-image", "ghcr.io/custom:v2"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := applyFlags(&cfg, fs, vals); err != nil {
		t.Fatalf("applyFlags: %v", err)
	}
	if cfg.Tart.Image != "ghcr.io/custom:v2" {
		t.Fatalf("Image = %q, want ghcr.io/custom:v2", cfg.Tart.Image)
	}
	if cfg.Tart.Image != "ghcr.io/custom:v2" {
		t.Fatal("image should be set to custom value")
	}
}

func TestApplyDefaultsTargetOS(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.TargetOS = ""
	cfg.Tart.Image = "test-image"
	cfg.Tart.CPUs = 8
	cfg.Tart.Memory = 8192
	applyDefaults(&cfg)
	if cfg.TargetOS != targetMacOS {
		t.Fatalf("TargetOS = %q, want %q", cfg.TargetOS, targetMacOS)
	}
}

func TestApplyDefaultsImageFallback(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Tart.Image = ""
	cfg.Tart.CPUs = 8
	cfg.Tart.Memory = 8192
	applyDefaults(&cfg)
	if cfg.Tart.Image == "" {
		t.Fatal("applyDefaults should set a default image when empty")
	}
}

func TestApplyDefaultsCPUFallback(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Tart.Image = "test"
	cfg.Tart.CPUs = 0
	cfg.Tart.Memory = 8192
	applyDefaults(&cfg)
	if cfg.Tart.CPUs <= 0 {
		t.Fatalf("applyDefaults should set positive default CPUs, got %d", cfg.Tart.CPUs)
	}
}

func TestApplyDefaultsMemoryFallback(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Tart.Image = "test"
	cfg.Tart.CPUs = 8
	cfg.Tart.Memory = 0
	applyDefaults(&cfg)
	if cfg.Tart.Memory <= 0 {
		t.Fatalf("applyDefaults should set positive default memory, got %d", cfg.Tart.Memory)
	}
}

func TestApplyDefaultsWorkRootFallback(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Tart.Image = "test"
	cfg.Tart.CPUs = 8
	cfg.Tart.Memory = 8192
	cfg.Tart.WorkRoot = ""
	cfg.WorkRoot = "/custom/root"
	applyDefaults(&cfg)
	if cfg.Tart.WorkRoot == "" {
		t.Fatal("applyDefaults should set Tart.WorkRoot from WorkRoot or default")
	}
}

func TestServerFromInstanceDefaultLabels(t *testing.T) {
	runner := &recordingRunner{}
	cfg := core.BaseConfig()
	cfg.Tart.Image = "test-image"
	cfg.Tart.User = "admin"
	cfg.Tart.WorkRoot = "/tmp"
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	inst := tartInstance{Name: "crabbox-test-1234", State: "running", Running: true, Source: "test-image"}
	claim := core.LeaseClaim{LeaseID: "lease-1", Slug: "test-slug"}
	server := b.serverFromInstance(inst, claim, cfg)
	if server.Labels["crabbox"] != "true" {
		t.Fatalf("label crabbox = %q, want true", server.Labels["crabbox"])
	}
	if server.Labels["provider"] != providerName {
		t.Fatalf("label provider = %q, want %s", server.Labels["provider"], providerName)
	}
	if server.Labels["instance"] != "crabbox-test-1234" {
		t.Fatalf("label instance = %q", server.Labels["instance"])
	}
	if server.Labels["lease"] != "lease-1" {
		t.Fatalf("label lease = %q", server.Labels["lease"])
	}
	if server.Labels["slug"] != "test-slug" {
		t.Fatalf("label slug = %q", server.Labels["slug"])
	}
	if server.Labels["ssh_user"] != "admin" {
		t.Fatalf("label ssh_user = %q", server.Labels["ssh_user"])
	}
	if server.Labels["ssh_port"] != sshPort {
		t.Fatalf("label ssh_port = %q", server.Labels["ssh_port"])
	}
}

func TestServerFromInstanceExistingLabelsRetained(t *testing.T) {
	runner := &recordingRunner{}
	cfg := core.BaseConfig()
	cfg.Tart.Image = "new-image"
	cfg.Tart.User = "new-user"
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	inst := tartInstance{Name: "test-vm", State: "running", Running: true}
	claim := core.LeaseClaim{
		Labels: map[string]string{
			"crabbox":  "true",
			"provider": "other",
			"instance": "other-vm",
			"ssh_user": "existing-user",
		},
	}
	server := b.serverFromInstance(inst, claim, cfg)
	if server.Labels["provider"] != "other" {
		t.Fatalf("should preserve existing provider label, got %q", server.Labels["provider"])
	}
	if server.Labels["instance"] != "other-vm" {
		t.Fatalf("should preserve existing instance label, got %q", server.Labels["instance"])
	}
	if server.Labels["ssh_user"] != "existing-user" {
		t.Fatalf("should preserve existing ssh_user label, got %q", server.Labels["ssh_user"])
	}
}

func TestServerFromInstanceRunningReadyStatus(t *testing.T) {
	runner := &recordingRunner{}
	cfg := core.BaseConfig()
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	inst := tartInstance{Name: "test-vm", State: "running", Running: true}
	claim := core.LeaseClaim{Labels: map[string]string{"state": "ready"}}
	server := b.serverFromInstance(inst, claim, cfg)
	if server.Status != "ready" {
		t.Fatalf("running + state=ready should produce status=ready, got %q", server.Status)
	}
}

func TestServerFromInstanceStoppedStatus(t *testing.T) {
	runner := &recordingRunner{}
	cfg := core.BaseConfig()
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	inst := tartInstance{Name: "test-vm", State: "stopped", Running: false}
	claim := core.LeaseClaim{Labels: map[string]string{"state": "ready"}}
	server := b.serverFromInstance(inst, claim, cfg)
	if server.Status == "ready" {
		t.Fatal("stopped instance should not have ready status")
	}
}

func TestListInstancesBadJSONPropagatesError(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"list", "--source", "local", "--format", "json"}): {Stdout: "not json"},
		},
	}
	cfg := core.BaseConfig()
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	_, err := b.listInstances(context.Background())
	if err == nil {
		t.Fatal("listInstances should return error on invalid JSON")
	}
	if !strings.Contains(err.Error(), "parse tart list") {
		t.Fatalf("error should mention parse: %v", err)
	}
}

func TestListInstancesCommandError(t *testing.T) {
	runner := &recordingRunner{
		errors: map[string]error{
			commandKey([]string{"list", "--source", "local", "--format", "json"}): fmt.Errorf("tart not found"),
		},
	}
	cfg := core.BaseConfig()
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	_, err := b.listInstances(context.Background())
	if err == nil {
		t.Fatal("listInstances should return error on command failure")
	}
}

func TestApplyFlagsConfigCPUZeroWithExplicit(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Tart.CPUs = 0
	core.MarkTartCPUsExplicit(&cfg)
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	vals := registerFlags(fs, cfg)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	err := applyFlags(&cfg, fs, vals)
	if err == nil {
		t.Fatal("CPU=0 with explicit mark should be rejected")
	}
}

func TestApplyFlagsConfigMemoryZeroWithExplicit(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Tart.Memory = 0
	core.MarkTartMemoryExplicit(&cfg)
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	vals := registerFlags(fs, cfg)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	err := applyFlags(&cfg, fs, vals)
	if err == nil {
		t.Fatal("Memory=0 with explicit mark should be rejected")
	}
}

func TestShouldCleanupUnparseableLastUsedAt(t *testing.T) {
	server := Server{
		Status: "running",
		Labels: map[string]string{"state": "ready"},
	}
	claim := core.LeaseClaim{
		LeaseID:            "test-lease",
		LastUsedAt:         "not-a-date",
		IdleTimeoutSeconds: 3600,
	}
	shouldDelete, reason := shouldCleanup(server, claim, true, time.Now())
	if shouldDelete {
		t.Fatalf("should not delete with unparseable LastUsedAt, reason=%s", reason)
	}
	if reason != "claim active" {
		t.Fatalf("reason = %q, want 'claim active'", reason)
	}
}

func TestShouldCleanupZeroLastUsedAt(t *testing.T) {
	server := Server{
		Status: "running",
		Labels: map[string]string{"state": "ready"},
	}
	claim := core.LeaseClaim{
		LeaseID:            "test-lease",
		LastUsedAt:         "0001-01-01T00:00:00Z",
		IdleTimeoutSeconds: 3600,
	}
	shouldDelete, reason := shouldCleanup(server, claim, true, time.Now())
	if shouldDelete {
		t.Fatalf("should not delete with zero LastUsedAt, reason=%s", reason)
	}
}

func TestConfigureDoctorError(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Tailscale.Enabled = true
	_, err := (Provider{}).ConfigureDoctor(cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: &recordingRunner{}})
	if err == nil {
		t.Fatal("ConfigureDoctor should propagate Configure error")
	}
}

func TestReleaseLeaseMessageFormat(t *testing.T) {
	runner := &recordingRunner{}
	cfg := core.BaseConfig()
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	lease := LeaseTarget{
		LeaseID: "test-lease-id",
		Server: Server{
			CloudID: "crabbox-test-vm",
			Labels:  map[string]string{},
		},
	}
	msg := b.ReleaseLeaseMessage(lease)
	if !strings.Contains(msg, "test-lease-id") {
		t.Fatalf("message should contain lease ID: %q", msg)
	}
	if !strings.Contains(msg, "crabbox-test-vm") {
		t.Fatalf("message should contain instance name: %q", msg)
	}
}

func TestReleaseLeaseMessageNoCloudID(t *testing.T) {
	runner := &recordingRunner{}
	cfg := core.BaseConfig()
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	lease := LeaseTarget{
		LeaseID: "test-lease-id",
		Server: Server{
			Labels: map[string]string{"instance": "from-labels"},
		},
	}
	msg := b.ReleaseLeaseMessage(lease)
	if !strings.Contains(msg, "from-labels") {
		t.Fatalf("should fallback to labels[instance]: %q", msg)
	}
}

func TestReleaseLeaseMessageEmptyBoth(t *testing.T) {
	runner := &recordingRunner{}
	cfg := core.BaseConfig()
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	lease := LeaseTarget{
		LeaseID: "test-lease-id",
		Server: Server{
			Labels: map[string]string{},
		},
	}
	msg := b.ReleaseLeaseMessage(lease)
	if !strings.Contains(msg, "-") {
		t.Fatalf("should show dash for missing instance: %q", msg)
	}
}

func TestValidPOSIXUserRegex(t *testing.T) {
	valid := []string{"admin", "root", "_test", "user-name", "user.name", "user_01"}
	for _, u := range valid {
		if !validPOSIXUser.MatchString(u) {
			t.Fatalf("validPOSIXUser should match %q", u)
		}
	}
	invalid := []string{"", "user name", "123start", "user@host", "user!", "a b"}
	for _, u := range invalid {
		if validPOSIXUser.MatchString(u) {
			t.Fatalf("validPOSIXUser should NOT match %q", u)
		}
	}
}

// --- Mutation round 4: applyDefaults edge cases, flags boundary, resolve/cleanup ---

func TestApplyDefaultsKeepsCustomTargetOS(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.TargetOS = "custom-os"
	cfg.Tart.Image = "test"
	cfg.Tart.CPUs = 8
	cfg.Tart.Memory = 8192
	applyDefaults(&cfg)
	if cfg.TargetOS != "custom-os" {
		t.Fatalf("TargetOS = %q, should preserve non-empty value", cfg.TargetOS)
	}
}

func TestApplyDefaultsWorkRootInheritsNonDefault(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Tart.Image = "test"
	cfg.Tart.CPUs = 8
	cfg.Tart.Memory = 8192
	cfg.Tart.WorkRoot = ""
	cfg.WorkRoot = "/custom/work/root"
	applyDefaults(&cfg)
	if cfg.Tart.WorkRoot != "/custom/work/root" {
		t.Fatalf("Tart.WorkRoot = %q, want /custom/work/root", cfg.Tart.WorkRoot)
	}
}

func TestApplyDefaultsWorkRootUsesHardcodedDefault(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Tart.Image = "test"
	cfg.Tart.CPUs = 8
	cfg.Tart.Memory = 8192
	cfg.Tart.WorkRoot = ""
	applyDefaults(&cfg)
	if cfg.Tart.WorkRoot != "/Users/admin/crabbox" {
		t.Fatalf("Tart.WorkRoot = %q, want /Users/admin/crabbox", cfg.Tart.WorkRoot)
	}
}

func TestApplyDefaultsKeepsExplicitWorkRoot(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Tart.Image = "test"
	cfg.Tart.CPUs = 8
	cfg.Tart.Memory = 8192
	cfg.Tart.WorkRoot = "/already/set"
	cfg.WorkRoot = "/other"
	applyDefaults(&cfg)
	if cfg.Tart.WorkRoot != "/already/set" {
		t.Fatalf("Tart.WorkRoot = %q, should preserve existing", cfg.Tart.WorkRoot)
	}
}

func TestApplyDefaultsKeepsPositiveCPUs(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Tart.Image = "test"
	cfg.Tart.CPUs = 16
	cfg.Tart.Memory = 8192
	applyDefaults(&cfg)
	if cfg.Tart.CPUs != 16 {
		t.Fatalf("CPUs = %d, should preserve existing 16", cfg.Tart.CPUs)
	}
}

func TestApplyDefaultsKeepsPositiveMemory(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Tart.Image = "test"
	cfg.Tart.CPUs = 8
	cfg.Tart.Memory = 32768
	applyDefaults(&cfg)
	if cfg.Tart.Memory != 32768 {
		t.Fatalf("Memory = %d, should preserve existing 32768", cfg.Tart.Memory)
	}
}

func TestApplyDefaultsSyncsSSHUserFromTart(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Tart.Image = "test"
	cfg.Tart.User = "deploy"
	cfg.Tart.CPUs = 8
	cfg.Tart.Memory = 8192
	applyDefaults(&cfg)
	if cfg.SSHUser != "deploy" {
		t.Fatalf("SSHUser = %q, should be set to Tart.User", cfg.SSHUser)
	}
}

func TestApplyDefaultsSyncsSSHPort(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Tart.Image = "test"
	cfg.Tart.CPUs = 8
	cfg.Tart.Memory = 8192
	applyDefaults(&cfg)
	if cfg.SSHPort != sshPort {
		t.Fatalf("SSHPort = %q, want %q", cfg.SSHPort, sshPort)
	}
}

func TestApplyDefaultsSyncsServerType(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Tart.Image = "my-image:v2"
	cfg.Tart.CPUs = 8
	cfg.Tart.Memory = 8192
	applyDefaults(&cfg)
	if cfg.ServerType != "my-image:v2" {
		t.Fatalf("ServerType = %q, want my-image:v2", cfg.ServerType)
	}
}

func TestApplyDefaultsResetsWindowsMode(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Tart.Image = "test"
	cfg.Tart.CPUs = 8
	cfg.Tart.Memory = 8192
	cfg.WindowsMode = "something"
	applyDefaults(&cfg)
	if cfg.WindowsMode != "" {
		t.Fatalf("WindowsMode = %q, should be cleared", cfg.WindowsMode)
	}
}

func TestApplyDefaultsResetsFallbackPorts(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Tart.Image = "test"
	cfg.Tart.CPUs = 8
	cfg.Tart.Memory = 8192
	cfg.SSHFallbackPorts = []string{"2222", "2223"}
	applyDefaults(&cfg)
	if len(cfg.SSHFallbackPorts) != 0 {
		t.Fatalf("SSHFallbackPorts = %v, should be empty", cfg.SSHFallbackPorts)
	}
}

func TestApplyFlagsConfigCPU1Rejected(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Tart.CPUs = 1
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	vals := registerFlags(fs, cfg)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	err := applyFlags(&cfg, fs, vals)
	if err == nil {
		t.Fatal("CPU=1 from config should be rejected (between 0 and 4)")
	}
}

func TestApplyFlagsConfigMemory2048Rejected(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Tart.Memory = 2048
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	vals := registerFlags(fs, cfg)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	err := applyFlags(&cfg, fs, vals)
	if err == nil {
		t.Fatal("Memory=2048 from config should be rejected (between 0 and 4096)")
	}
}

func TestApplyFlagsConfigDiskNegativeRejected(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Tart.Disk = -5
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	vals := registerFlags(fs, cfg)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	err := applyFlags(&cfg, fs, vals)
	if err == nil {
		t.Fatal("Disk=-5 from config should be rejected")
	}
}

func TestServerFromInstanceNilClaimLabels(t *testing.T) {
	runner := &recordingRunner{}
	cfg := core.BaseConfig()
	cfg.Tart.Image = "test-image"
	cfg.Tart.User = "admin"
	cfg.Tart.WorkRoot = "/tmp"
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	inst := tartInstance{Name: "crabbox-vm-1", State: "stopped", Running: false}
	claim := core.LeaseClaim{}
	server := b.serverFromInstance(inst, claim, cfg)
	if server.Labels["provider"] != providerName {
		t.Fatalf("label provider = %q, should default to %s", server.Labels["provider"], providerName)
	}
	if server.Labels["instance"] != "crabbox-vm-1" {
		t.Fatalf("label instance should default to inst.Name")
	}
	if server.Status == "ready" {
		t.Fatal("stopped instance should not show ready")
	}
}

func TestServerFromInstanceSourceFallback(t *testing.T) {
	runner := &recordingRunner{}
	cfg := core.BaseConfig()
	cfg.Tart.Image = "fallback-image"
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	inst := tartInstance{Name: "crabbox-vm-1", State: "running", Running: true, Source: ""}
	claim := core.LeaseClaim{}
	server := b.serverFromInstance(inst, claim, cfg)
	if server.Labels["server_type"] != "fallback-image" {
		t.Fatalf("server_type should fallback to cfg.Tart.Image when Source is empty, got %q", server.Labels["server_type"])
	}
}

func TestServerFromInstanceSourcePreferred(t *testing.T) {
	runner := &recordingRunner{}
	cfg := core.BaseConfig()
	cfg.Tart.Image = "config-image"
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	inst := tartInstance{Name: "crabbox-vm-1", State: "running", Running: true, Source: "source-image"}
	claim := core.LeaseClaim{}
	server := b.serverFromInstance(inst, claim, cfg)
	if server.Labels["server_type"] != "source-image" {
		t.Fatalf("server_type should prefer inst.Source, got %q", server.Labels["server_type"])
	}
}

func TestStartVMArgsFormat(t *testing.T) {
	args := startVMArgs("test-vm-name")
	if len(args) != 3 {
		t.Fatalf("startVMArgs len = %d, want 3", len(args))
	}
	if args[0] != "run" {
		t.Fatalf("args[0] = %q, want run", args[0])
	}
	if args[1] != "test-vm-name" {
		t.Fatalf("args[1] = %q, want test-vm-name", args[1])
	}
	if args[2] != "--no-graphics" {
		t.Fatalf("args[2] = %q, want --no-graphics", args[2])
	}
}

func TestShouldCleanupNegativeIdleTimeout(t *testing.T) {
	server := Server{
		Status: "running",
		Labels: map[string]string{"state": "ready"},
	}
	claim := core.LeaseClaim{
		LeaseID:            "test-lease",
		LastUsedAt:         time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339),
		IdleTimeoutSeconds: -1,
	}
	shouldDelete, reason := shouldCleanup(server, claim, true, time.Now())
	if shouldDelete {
		t.Fatalf("negative idle timeout should not trigger cleanup, reason=%s", reason)
	}
}

func TestShouldCleanupNotRunningNotReady(t *testing.T) {
	server := Server{
		Status: "suspended",
		Labels: map[string]string{"state": "suspended"},
	}
	claim := core.LeaseClaim{LeaseID: "test"}
	shouldDelete, reason := shouldCleanup(server, claim, true, time.Now())
	if !shouldDelete {
		t.Fatalf("suspended instance should be cleaned up, reason=%s", reason)
	}
	if !strings.Contains(reason, "state=") {
		t.Fatalf("reason should include state: %s", reason)
	}
}

func TestShouldCleanupEmptyStatus(t *testing.T) {
	server := Server{
		Status: "",
		Labels: map[string]string{},
	}
	claim := core.LeaseClaim{LeaseID: "test"}
	shouldDelete, reason := shouldCleanup(server, claim, true, time.Now())
	if !shouldDelete {
		t.Fatalf("empty status should trigger cleanup, reason=%s", reason)
	}
}

func TestBlankHelper(t *testing.T) {
	if got := blank("value", "fallback"); got != "value" {
		t.Fatalf("blank(\"value\", \"fallback\") = %q", got)
	}
	if got := blank("", "fallback"); got != "fallback" {
		t.Fatalf("blank(\"\", \"fallback\") = %q", got)
	}
}

func TestInstanceScopeBlankInput(t *testing.T) {
	if got := instanceScope(""); got != "" {
		t.Fatalf("instanceScope(\"\") = %q, want \"\"", got)
	}
	if got := instanceScope("  "); got != "" {
		t.Fatalf("instanceScope(\"  \") = %q, want \"\"", got)
	}
}

func TestInstanceScopePrefixAdded(t *testing.T) {
	if got := instanceScope("my-vm"); got != "instance:my-vm" {
		t.Fatalf("instanceScope(\"my-vm\") = %q", got)
	}
}

func TestInstanceNameFromScopeRejectsNonInstance(t *testing.T) {
	if got := instanceNameFromScope(""); got != "" {
		t.Fatalf("empty scope should return \"\"")
	}
	if got := instanceNameFromScope("pod:abc"); got != "" {
		t.Fatalf("non-instance scope should return \"\"")
	}
	if got := instanceNameFromScope("   "); got != "" {
		t.Fatalf("whitespace scope should return \"\"")
	}
}

func TestInstanceNameFromScopeExtractsName(t *testing.T) {
	if got := instanceNameFromScope("instance:my-vm"); got != "my-vm" {
		t.Fatalf("instanceNameFromScope = %q, want my-vm", got)
	}
}

func TestInstanceNameFromClaimPrefersLabel(t *testing.T) {
	claim := core.LeaseClaim{
		Labels:        map[string]string{"instance": "from-label"},
		ProviderScope: "instance:from-scope",
	}
	if got := instanceNameFromClaim(claim); got != "from-label" {
		t.Fatalf("instanceNameFromClaim should prefer label, got %q", got)
	}
}

func TestInstanceNameFromClaimUsesScope(t *testing.T) {
	claim := core.LeaseClaim{
		Labels:        map[string]string{},
		ProviderScope: "instance:from-scope",
	}
	if got := instanceNameFromClaim(claim); got != "from-scope" {
		t.Fatalf("instanceNameFromClaim should fallback to scope, got %q", got)
	}
}

func TestInstanceNameFromClaimReturnsEmptyForMissing(t *testing.T) {
	claim := core.LeaseClaim{
		Labels:        map[string]string{},
		ProviderScope: "",
	}
	if got := instanceNameFromClaim(claim); got != "" {
		t.Fatalf("instanceNameFromClaim should return empty, got %q", got)
	}
}

func TestNormalizeLeaseSlugEmpty(t *testing.T) {
	if got := normalizeLeaseSlug(""); got != "" {
		t.Fatalf("normalizeLeaseSlug(\"\") = %q", got)
	}
}

func TestNormalizeLeaseSlugWithPrefix(t *testing.T) {
	result := normalizeLeaseSlug("my-slug")
	if result == "" {
		t.Fatal("normalizeLeaseSlug should return non-empty for valid slug")
	}
}
