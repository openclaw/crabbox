package hyperv

import (
	"context"
	"encoding/json"
	"errors"
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
		if err, ok := r.errors[req.Args[len(req.Args)-1]]; ok {
			return r.responses[req.Args[len(req.Args)-1]], err
		}
		if result, ok := r.responses[req.Args[len(req.Args)-1]]; ok {
			return result, nil
		}
	}
	return core.LocalCommandResult{}, nil
}

func commandKey(args []string) string {
	return strings.Join(args, "\x00")
}

func testBackend(runner *recordingRunner) *backend {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.HyperV = core.HyperVConfig{
		Image:    `C:\Images\windows.vhdx`,
		User:     "crabbox",
		WorkRoot: `C:\crabbox`,
		CPUs:     4,
		Memory:   8192,
		Switch:   "Default Switch",
	}
	return newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
}

func TestProviderSpecAndAliases(t *testing.T) {
	p := Provider{}
	if p.Name() != providerName {
		t.Fatalf("Name=%q want %s", p.Name(), providerName)
	}
	spec := p.Spec()
	if spec.Kind != core.ProviderKindSSHLease || spec.Family != "local-vm" {
		t.Fatalf("unexpected spec: %#v", spec)
	}
	if spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("coordinator=%s want never", spec.Coordinator)
	}
	for _, feature := range []core.Feature{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup} {
		if !spec.Features.Has(feature) {
			t.Fatalf("features=%v missing %s", spec.Features, feature)
		}
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetWindows || spec.Targets[0].WindowsMode != core.WindowsModeNormal {
		t.Fatalf("unexpected targets: %#v", spec.Targets)
	}
}

func TestProviderAliasesResolve(t *testing.T) {
	for _, alias := range []string{"hyperv"} {
		got, err := core.ProviderFor(alias)
		if err != nil {
			t.Fatalf("ProviderFor(%q): %v", alias, err)
		}
		if got.Name() != providerName {
			t.Fatalf("ProviderFor(%q).Name=%q", alias, got.Name())
		}
	}
}

func TestConfigureRejectsLinux(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	if _, err := (Provider{}).Configure(cfg, core.Runtime{}); err == nil {
		t.Fatal("Configure accepted linux target")
	}
}

func TestConfigureRejectsMacOS(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetMacOS
	if _, err := (Provider{}).Configure(cfg, core.Runtime{}); err == nil {
		t.Fatal("Configure accepted macos target")
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

func TestConfigureAcceptsWindows(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetWindows
	cfg.WindowsMode = core.WindowsModeNormal
	if _, err := (Provider{}).Configure(cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: &recordingRunner{}}); err != nil {
		t.Fatalf("Configure rejected windows target: %v", err)
	}
}

func TestConfigureAcceptsEmpty(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = ""
	if _, err := (Provider{}).Configure(cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: &recordingRunner{}}); err != nil {
		t.Fatalf("Configure rejected empty target: %v", err)
	}
}

func TestApplyDefaults(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = ""
	cfg.HyperV = core.HyperVConfig{}
	applyDefaults(&cfg)
	if cfg.TargetOS != core.TargetWindows {
		t.Fatalf("target=%s want windows", cfg.TargetOS)
	}
	if cfg.WindowsMode != core.WindowsModeNormal {
		t.Fatalf("windowsMode=%s want normal", cfg.WindowsMode)
	}
	if cfg.HyperV.CPUs != 4 || cfg.HyperV.Memory != 8192 {
		t.Fatalf("defaults not applied: cpus=%d memory=%d", cfg.HyperV.CPUs, cfg.HyperV.Memory)
	}
	if cfg.HyperV.Switch != "Default Switch" {
		t.Fatalf("switch=%s want Default Switch", cfg.HyperV.Switch)
	}
	if cfg.HyperV.User != "crabbox" {
		t.Fatalf("user=%s want crabbox", cfg.HyperV.User)
	}
	if cfg.SSHUser != "crabbox" || cfg.SSHPort != sshPort {
		t.Fatalf("ssh user=%s port=%s", cfg.SSHUser, cfg.SSHPort)
	}
	if cfg.WorkRoot != `C:\crabbox` {
		t.Fatalf("workRoot=%s want C:\\crabbox", cfg.WorkRoot)
	}
}

func TestApplyDefaultsHonorsExplicitSSHUser(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.HyperV = core.HyperVConfig{}
	cfg.SSHUser = "devuser"
	applyDefaults(&cfg)
	if cfg.HyperV.User != "devuser" {
		t.Fatalf("explicit --ssh-user not inherited: HyperV.User=%s want devuser", cfg.HyperV.User)
	}
	if cfg.SSHUser != "devuser" {
		t.Fatalf("SSHUser=%s want devuser", cfg.SSHUser)
	}

	// An explicit --hyperv-user still wins over --ssh-user.
	cfg = core.BaseConfig()
	cfg.Provider = providerName
	cfg.HyperV = core.HyperVConfig{User: "winuser"}
	cfg.SSHUser = "devuser"
	applyDefaults(&cfg)
	if cfg.HyperV.User != "winuser" || cfg.SSHUser != "winuser" {
		t.Fatalf("explicit --hyperv-user not preserved: HyperV.User=%s SSHUser=%s want winuser", cfg.HyperV.User, cfg.SSHUser)
	}
}

func TestDoctorReportsConfiguredImage(t *testing.T) {
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })

	b := testBackend(&recordingRunner{})
	result, err := b.Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if !strings.Contains(result.Message, `image=C:\Images\windows.vhdx`) {
		t.Fatalf("doctor message missing configured image: %q", result.Message)
	}
}

func TestCleanupScopedToCrabboxPrefix(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })

	vms := []hypervVM{
		{Name: "crabbox-blue-1234", State: 2},
		{Name: "my-personal-vm", State: 2},
		{Name: "crabbox-red-5678", State: 3},
	}
	cfg := b.configForRun()
	claims := map[string]core.LeaseClaim{}
	var views []LeaseView
	for _, vm := range vms {
		claim := claims[vm.Name]
		if claim.LeaseID == "" && !strings.HasPrefix(vm.Name, "crabbox-") {
			continue
		}
		views = append(views, b.serverFromInstance(vm, claim, cfg))
	}

	if len(views) != 2 {
		t.Fatalf("list should filter to crabbox- prefix, got %d views", len(views))
	}
	for _, v := range views {
		if !strings.HasPrefix(v.Name, "crabbox-") {
			t.Fatalf("list included non-crabbox VM: %s", v.Name)
		}
	}
}

func TestRemoveVMRefuseNonCrabbox(t *testing.T) {
	b := testBackend(&recordingRunner{})
	err := b.removeVM(context.Background(), "my-personal-vm")
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("removeVM should refuse non-crabbox VM, err=%v", err)
	}
}

func TestParseVMListSingle(t *testing.T) {
	raw := `{"Name":"crabbox-blue-1234","State":2}`
	vms, err := parseVMList(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(vms) != 1 || vms[0].Name != "crabbox-blue-1234" || vms[0].State != 2 {
		t.Fatalf("unexpected: %#v", vms)
	}
}

func TestParseVMListArray(t *testing.T) {
	raw := `[{"Name":"crabbox-blue-1234","State":2},{"Name":"crabbox-red-5678","State":3}]`
	vms, err := parseVMList(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(vms) != 2 {
		t.Fatalf("expected 2 VMs, got %d", len(vms))
	}
	if vms[0].Name != "crabbox-blue-1234" || vms[1].Name != "crabbox-red-5678" {
		t.Fatalf("unexpected: %#v", vms)
	}
}

func TestParseVMListEmpty(t *testing.T) {
	for _, raw := range []string{"", "null"} {
		vms, err := parseVMList(raw)
		if err != nil {
			t.Fatalf("raw=%q err=%v", raw, err)
		}
		if len(vms) != 0 {
			t.Fatalf("raw=%q expected empty, got %d", raw, len(vms))
		}
	}
}

func TestParseFirstIPv4(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{`["172.20.0.5","fe80::1"]`, "172.20.0.5"},
		{`["fe80::1","192.168.1.100"]`, "192.168.1.100"},
		{`"10.0.0.1"`, "10.0.0.1"},
		{`null`, ""},
		{`""`, ""},
		{`["fe80::1"]`, ""},
	}
	for _, tc := range tests {
		got := parseFirstIPv4(tc.raw)
		if got != tc.want {
			t.Fatalf("parseFirstIPv4(%q)=%q want %q", tc.raw, got, tc.want)
		}
	}
}

func TestIsIPv4(t *testing.T) {
	for _, good := range []string{"192.168.1.1", "10.0.0.1", "172.20.0.5", "0.0.0.0", "255.255.255.255"} {
		if !isIPv4(good) {
			t.Fatalf("isIPv4(%q) should be true", good)
		}
	}
	for _, bad := range []string{"fe80::1", "abc", "192.168.1", "192.168.1.1.1", "300.0.0.1"} {
		if isIPv4(bad) {
			t.Fatalf("isIPv4(%q) should be false", bad)
		}
	}
}

func TestHypervState(t *testing.T) {
	tests := map[int]string{
		2:  "running",
		3:  "off",
		6:  "saved",
		9:  "paused",
		99: "unknown",
	}
	for state, want := range tests {
		if got := hypervState(state); got != want {
			t.Fatalf("hypervState(%d)=%q want %q", state, got, want)
		}
	}
}

func TestInstanceScopeRoundTrip(t *testing.T) {
	name := "crabbox-blue-1234abcd"
	if got := instanceNameFromScope(instanceScope(name)); got != name {
		t.Fatalf("instance name=%q want %q", got, name)
	}
}

func TestShouldCleanupRespectsKeepLabel(t *testing.T) {
	server := Server{Status: "off", Labels: map[string]string{"keep": "true"}}
	if ok, reason := shouldCleanup(server, core.LeaseClaim{}, true, time.Now()); ok || reason != "keep=true" {
		t.Fatalf("cleanup=%v reason=%s", ok, reason)
	}
}

func TestShouldCleanupExpiredClaim(t *testing.T) {
	server := Server{Status: "running", Labels: map[string]string{}}
	claim := core.LeaseClaim{
		LeaseID:            "cbx_123",
		LastUsedAt:         time.Now().Add(-48 * time.Hour).Format(time.RFC3339),
		IdleTimeoutSeconds: int((30 * time.Minute).Seconds()),
	}
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

func TestShouldCleanupStoppedVM(t *testing.T) {
	server := Server{Status: "off", Labels: map[string]string{}}
	if ok, reason := shouldCleanup(server, core.LeaseClaim{}, false, time.Now()); !ok {
		t.Fatalf("should cleanup stopped VM, got cleanup=%v reason=%s", ok, reason)
	}
}

func TestEscapePSString(t *testing.T) {
	tests := map[string]string{
		"hello":       "hello",
		"it's a test": "it''s a test",
		"no'pe":       "no''pe",
	}
	for input, want := range tests {
		if got := escapePSString(input); got != want {
			t.Fatalf("escapePSString(%q)=%q want %q", input, got, want)
		}
	}
}

func TestVMListJSONRoundTrip(t *testing.T) {
	vms := []hypervVM{
		{Name: "crabbox-blue-1234", State: 2},
		{Name: "crabbox-red-5678", State: 3},
	}
	data, err := json.Marshal(vms)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseVMList(string(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed) != 2 || parsed[0].Name != vms[0].Name || parsed[1].State != vms[1].State {
		t.Fatalf("round-trip mismatch: %#v", parsed)
	}
}

func TestServerFromInstanceLabels(t *testing.T) {
	b := testBackend(&recordingRunner{})
	server := b.serverFromInstance(
		hypervVM{Name: "crabbox-blue-1234", State: 2},
		core.LeaseClaim{},
		b.configForRun(),
	)
	if server.CloudID != "crabbox-blue-1234" {
		t.Fatalf("cloudID=%q", server.CloudID)
	}
	if server.Labels["provider"] != providerName {
		t.Fatalf("provider label=%q", server.Labels["provider"])
	}
	if server.Labels["instance"] != "crabbox-blue-1234" {
		t.Fatalf("instance label=%q", server.Labels["instance"])
	}
	if server.Status != "running" {
		t.Fatalf("status=%q want running", server.Status)
	}
}

func TestServerFromInstancePopulatesIPFromClaim(t *testing.T) {
	b := testBackend(&recordingRunner{})
	server := b.serverFromInstance(
		hypervVM{Name: "crabbox-blue-1234", State: 2},
		core.LeaseClaim{SSHHost: "192.168.1.50"},
		b.configForRun(),
	)
	if server.PublicNet.IPv4.IP != "192.168.1.50" {
		t.Fatalf("PublicNet.IPv4.IP=%q want 192.168.1.50", server.PublicNet.IPv4.IP)
	}
}

func TestServerFromInstanceNoIPWithoutClaim(t *testing.T) {
	b := testBackend(&recordingRunner{})
	server := b.serverFromInstance(
		hypervVM{Name: "crabbox-blue-1234", State: 2},
		core.LeaseClaim{},
		b.configForRun(),
	)
	if server.PublicNet.IPv4.IP != "" {
		t.Fatalf("PublicNet.IPv4.IP=%q want empty", server.PublicNet.IPv4.IP)
	}
}

func TestCreateVMUsesDifferencingDisk(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{},
		errors:    map[string]error{},
	}
	b := testBackend(runner)
	cfg := b.configForRun()
	cfg.HyperV.Image = `C:\Images\windows.vhdx`

	err := b.createVM(context.Background(), cfg, "crabbox-blue-1234", "ssh-ed25519 AAAA test")
	if err != nil {
		t.Fatalf("createVM: %v", err)
	}

	var foundDiff, foundNewVM, foundStart, foundInject bool
	for _, call := range runner.calls {
		script := call.Args[len(call.Args)-1]
		if strings.Contains(script, "New-VHD") && strings.Contains(script, "-Differencing") &&
			strings.Contains(script, `-ParentPath 'C:\Images\windows.vhdx'`) {
			foundDiff = true
		}
		if strings.Contains(script, "New-VM") && strings.Contains(script, "-VHDPath") && !strings.Contains(script, "-NewVHDPath") {
			foundNewVM = true
		}
		if strings.Contains(script, "Start-VM") {
			foundStart = true
		}
		if strings.Contains(script, "Invoke-Command") && strings.Contains(script, "authorized_keys") {
			foundInject = true
		}
	}
	if !foundDiff {
		t.Error("createVM should back the lease with a differencing disk over the template")
	}
	if !foundNewVM {
		t.Error("createVM did not use -VHDPath (existing VHD)")
	}
	if !foundStart {
		t.Error("createVM did not start the VM")
	}
	if !foundInject {
		t.Error("createVM did not inject SSH key via PowerShell Direct")
	}
}

// Leases must not copy or resize the template: the differencing disk avoids the
// multi-GB per-lease copy and inherits the template's virtual size.
func TestCreateVMDoesNotCopyOrResizeTemplate(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{},
		errors:    map[string]error{},
	}
	b := testBackend(runner)
	cfg := b.configForRun()
	cfg.HyperV.Image = `C:\Images\windows.vhdx`

	if err := b.createVM(context.Background(), cfg, "crabbox-blue-1234", ""); err != nil {
		t.Fatalf("createVM: %v", err)
	}
	for _, call := range runner.calls {
		script := call.Args[len(call.Args)-1]
		if strings.Contains(script, "Copy-Item") {
			t.Error("createVM should not copy the template (use a differencing disk)")
		}
		if strings.Contains(script, "Resize-VHD") {
			t.Error("createVM should not resize the lease disk (it inherits the template size)")
		}
	}
}

func TestCreateVMPlacesVMFilesUnderHypervVMDir(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{},
		errors:    map[string]error{},
	}
	b := testBackend(runner)
	cfg := b.configForRun()
	cfg.HyperV.Image = `C:\Images\windows.vhdx`

	if err := b.createVM(context.Background(), cfg, "crabbox-blue-1234", ""); err != nil {
		t.Fatalf("createVM: %v", err)
	}

	wantVMDir := hypervVMDir()
	var foundPathedNewVM bool
	for _, call := range runner.calls {
		script := call.Args[len(call.Args)-1]
		if strings.Contains(script, "New-VM") && strings.Contains(script, "-Path '"+wantVMDir+"'") {
			foundPathedNewVM = true
		}
	}
	if !foundPathedNewVM {
		t.Fatalf("createVM should pass New-VM -Path %q so VM config/runtime files don't default to the system drive", wantVMDir)
	}
}

// --hyperv-init-password must write the first-boot RunOnce into the lease disk
// BEFORE the VM is created/booted, keep the password out of host command lines,
// and leave the template (ParentPath) untouched.
func TestCreateVMInitPasswordInjectsRunOnceBeforeBoot(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	b.cfg.HyperV.InitPassword = true
	b.cfg.HyperV.GuestPassword = "s3cret-pa$$word"
	cfg := b.configForRun()
	cfg.HyperV.Image = `C:\Images\windows.vhdx`

	if err := b.createVM(context.Background(), cfg, "crabbox-blue-1234", ""); err != nil {
		t.Fatalf("createVM: %v", err)
	}

	injectIdx, newVMIdx := -1, -1
	for i, call := range runner.calls {
		script := call.Args[len(call.Args)-1]
		if strings.Contains(script, "Mount-VHD") && strings.Contains(script, "RunOnce") {
			injectIdx = i
			for _, want := range []string{`net user "crabbox"`, "reg.exe load", "reg.exe unload", "Dismount-VHD", "$env:_CRABBOX_GP"} {
				if !strings.Contains(script, want) {
					t.Errorf("init-password script missing %q", want)
				}
			}
			if strings.Contains(script, `C:\Images\windows.vhdx`) {
				t.Error("init-password script must mount the lease disk, not the template")
			}
			var foundEnv bool
			for _, e := range call.Env {
				if strings.Contains(e, "_CRABBOX_GP=s3cret-pa$$word") {
					foundEnv = true
				}
			}
			if !foundEnv {
				t.Error("_CRABBOX_GP env var not found on the injection call")
			}
		}
		for _, arg := range call.Args {
			if strings.Contains(arg, "s3cret-pa$$word") {
				t.Fatal("guest password found in command args; should be passed via environment only")
			}
		}
		if strings.Contains(script, "New-VM ") {
			newVMIdx = i
		}
	}
	if injectIdx < 0 {
		t.Fatal("createVM should inject the first-boot password RunOnce when init-password is enabled")
	}
	if newVMIdx < 0 || newVMIdx < injectIdx {
		t.Fatalf("password injection (call %d) must happen before New-VM (call %d)", injectIdx, newVMIdx)
	}
}

func TestCreateVMNoInitPasswordByDefault(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	cfg := b.configForRun()
	cfg.HyperV.Image = `C:\Images\windows.vhdx`

	if err := b.createVM(context.Background(), cfg, "crabbox-blue-1234", ""); err != nil {
		t.Fatalf("createVM: %v", err)
	}
	for _, call := range runner.calls {
		script := call.Args[len(call.Args)-1]
		if strings.Contains(script, "Mount-VHD") || strings.Contains(script, "RunOnce") {
			t.Fatal("createVM must not touch the lease disk offline unless --hyperv-init-password is set")
		}
	}
}

func TestAcquireInitPasswordRequiresExplicitPassword(t *testing.T) {
	b := testBackend(&recordingRunner{})
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })

	b.cfg.HyperV.InitPassword = true
	b.cfg.HyperV.GuestPassword = ""
	_, err := b.Acquire(context.Background(), core.AcquireRequest{})
	if err == nil || !strings.Contains(err.Error(), "CRABBOX_HYPERV_GUEST_PASSWORD") {
		t.Fatalf("Acquire should require an explicit guest password with init-password, got: %v", err)
	}
}

func TestAcquireInitPasswordRejectsCmdUnsafePassword(t *testing.T) {
	b := testBackend(&recordingRunner{})
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })

	b.cfg.HyperV.InitPassword = true
	for _, password := range []string{`pa"ss`, `pa%ss`} {
		b.cfg.HyperV.GuestPassword = password
		_, err := b.Acquire(context.Background(), core.AcquireRequest{})
		if err == nil || !strings.Contains(err.Error(), "cannot carry") {
			t.Fatalf("Acquire should reject cmd-unsafe init password %q, got: %v", password, err)
		}
	}
}

func TestAcquireRejectsISO(t *testing.T) {
	b := testBackend(&recordingRunner{})
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })

	b.cfg.HyperV.Image = `C:\Images\win-server.iso`
	_, err := b.Acquire(context.Background(), core.AcquireRequest{})
	if err == nil || !strings.Contains(err.Error(), "does not support ISO") {
		t.Fatalf("Acquire should reject ISO images, got: %v", err)
	}
}

func TestQueryVMParsesSingle(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"-NoProfile", "-NonInteractive", "-Command",
				`Get-VM -Name 'crabbox-blue-1234' | Select-Object Name, State | ConvertTo-Json -Compress`}): {
				Stdout: `{"Name":"crabbox-blue-1234","State":3}`,
			},
		},
	}
	b := testBackend(runner)
	vm, err := b.queryVM(context.Background(), "crabbox-blue-1234")
	if err != nil {
		t.Fatalf("queryVM: %v", err)
	}
	if vm.State != 3 {
		t.Fatalf("state=%d want 3 (off)", vm.State)
	}
}

func TestEnsureGitInstallsWhenMissing(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)

	if err := b.ensureGit(context.Background(), "crabbox-blue-1234", "crabbox"); err != nil {
		t.Fatalf("ensureGit: %v", err)
	}
	var script string
	for _, call := range runner.calls {
		s := call.Args[len(call.Args)-1]
		if strings.Contains(s, "Invoke-Command") && strings.Contains(s, "MinGit") {
			script = s
		}
	}
	if script == "" {
		t.Fatal("ensureGit should install git over PowerShell Direct when missing")
	}
	for _, want := range []string{"Get-Command git", "git-for-windows", "Expand-Archive", "SetEnvironmentVariable"} {
		if !strings.Contains(script, want) {
			t.Errorf("ensureGit script missing %q", want)
		}
	}
	// MinGit's etc\gitconfig includes C:/Program Files/Git/etc/gitconfig, so
	// extracting it to that path makes the include self-referential and every
	// guest git command fails with "exceeded maximum include depth".
	if !strings.Contains(script, `C:\Program Files\MinGit`) {
		t.Error("ensureGit must extract MinGit to its own directory")
	}
	if strings.Contains(script, `$dst='C:\Program Files\Git'`) {
		t.Error("ensureGit must not extract MinGit to C:\\Program Files\\Git (self-referential gitconfig include)")
	}
}

func ensureGitScript(t *testing.T) string {
	t.Helper()
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	if err := b.ensureGit(context.Background(), "crabbox-blue-1234", "crabbox"); err != nil {
		t.Fatalf("ensureGit: %v", err)
	}
	for _, call := range runner.calls {
		s := call.Args[len(call.Args)-1]
		if strings.Contains(s, "Invoke-Command") && strings.Contains(s, "MinGit") {
			return s
		}
	}
	t.Fatal("ensureGit install script not found")
	return ""
}

// The MinGit archive is extracted into Program Files and added to the machine
// PATH inside the guest, so the download must be pinned to an immutable
// release asset -- never the floating "latest" API -- and verified against
// the release's published SHA-256 before extraction.
func TestEnsureGitPinsImmutableMinGitRelease(t *testing.T) {
	script := ensureGitScript(t)
	if !strings.Contains(script, minGitURL) {
		t.Error("ensureGit must download the pinned MinGit release asset")
	}
	if !strings.Contains(script, minGitSHA256) {
		t.Error("ensureGit must embed the expected MinGit SHA-256")
	}
	for _, banned := range []string{"releases/latest", "Invoke-RestMethod"} {
		if strings.Contains(script, banned) {
			t.Errorf("ensureGit must not resolve MinGit via %q (mutable release reference)", banned)
		}
	}
	if !strings.Contains(minGitURL, "/releases/download/v") {
		t.Errorf("minGitURL %q is not an immutable release asset URL", minGitURL)
	}
	if len(minGitSHA256) != 64 {
		t.Errorf("minGitSHA256 length = %d, want 64 hex chars", len(minGitSHA256))
	}
}

// Success path: the hash must be computed and compared BEFORE Expand-Archive
// runs, so a tampered archive is never extracted.
func TestEnsureGitVerifiesChecksumBeforeExtraction(t *testing.T) {
	script := ensureGitScript(t)
	idxHash := strings.Index(script, "Get-FileHash")
	idxCompare := strings.Index(script, minGitSHA256)
	idxExtract := strings.Index(script, "Expand-Archive")
	if idxHash < 0 || idxCompare < 0 || idxExtract < 0 {
		t.Fatalf("ensureGit script missing verification steps: hash=%d compare=%d extract=%d", idxHash, idxCompare, idxExtract)
	}
	if !(idxHash < idxExtract && idxCompare < idxExtract) {
		t.Errorf("checksum verification (hash@%d, compare@%d) must precede extraction (@%d)", idxHash, idxCompare, idxExtract)
	}
}

// Mismatch path: a wrong hash must fail closed -- delete the downloaded
// archive and throw (PowerShell Direct surfaces the throw as a non-zero exit,
// which ensureGit returns as an error after retries).
func TestEnsureGitFailsClosedOnChecksumMismatch(t *testing.T) {
	script := ensureGitScript(t)
	mismatch := strings.Index(script, "MinGit SHA-256 mismatch")
	if mismatch < 0 {
		t.Fatal("ensureGit script has no checksum-mismatch branch")
	}
	branch := script[strings.Index(script, "$hash"):strings.Index(script, "Expand-Archive")]
	for _, want := range []string{"Remove-Item $zip", "throw"} {
		if !strings.Contains(branch, want) {
			t.Errorf("checksum-mismatch branch missing %q", want)
		}
	}
}

func TestInjectSSHKeyLocksAdminKeyACL(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)

	if err := b.injectSSHKey(context.Background(), "crabbox-blue-1234", "Administrator", "ssh-ed25519 AAAA test"); err != nil {
		t.Fatalf("injectSSHKey: %v", err)
	}
	var script string
	for _, call := range runner.calls {
		s := call.Args[len(call.Args)-1]
		if strings.Contains(s, "administrators_authorized_keys") {
			script = s
		}
	}
	if script == "" {
		t.Fatal("injectSSHKey should write administrators_authorized_keys")
	}
	// Windows OpenSSH ignores administrators_authorized_keys unless it is owned
	// only by SYSTEM + Administrators with inheritance disabled.
	for _, want := range []string{"icacls", "/inheritance:r", "SYSTEM:F", `BUILTIN\Administrators:F`} {
		if !strings.Contains(script, want) {
			t.Errorf("admin-key ACL lockdown missing %q\nscript: %s", want, script)
		}
	}
}

func TestInjectSSHKeyPasswordNotInArgs(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{},
	}
	b := testBackend(runner)
	b.cfg.HyperV.GuestPassword = "s3cret-pa$$word"

	_ = b.injectSSHKey(context.Background(), "crabbox-blue-1234", "crabbox", "ssh-ed25519 AAAA test")

	for _, call := range runner.calls {
		for _, arg := range call.Args {
			if strings.Contains(arg, "s3cret-pa$$word") {
				t.Fatal("guest password found in command args; should be passed via environment only")
			}
		}
		if len(call.Env) == 0 {
			t.Fatal("injectSSHKey should pass password via Env, not Args")
		}
		var foundEnv bool
		for _, e := range call.Env {
			if strings.Contains(e, "_CRABBOX_GP=s3cret-pa$$word") {
				foundEnv = true
			}
		}
		if !foundEnv {
			t.Fatal("_CRABBOX_GP env var not found in command Env")
		}
	}
}

func TestEnsureOpenSSHInstallsAndStartsSshd(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)

	if err := b.ensureOpenSSH(context.Background(), "crabbox-blue-1234", "crabbox"); err != nil {
		t.Fatalf("ensureOpenSSH: %v", err)
	}

	var script string
	for _, call := range runner.calls {
		s := call.Args[len(call.Args)-1]
		if strings.Contains(s, "Invoke-Command") && strings.Contains(s, "OpenSSH.Server") {
			script = s
		}
	}
	if script == "" {
		t.Fatal("ensureOpenSSH should invoke an OpenSSH install over PowerShell Direct")
	}
	for _, want := range []string{"Add-WindowsCapability", "Start-Service sshd", "OpenSSH-Server-In-TCP"} {
		if !strings.Contains(script, want) {
			t.Errorf("ensureOpenSSH script missing %q", want)
		}
	}
}

func TestEnsureOpenSSHPasswordNotInArgs(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	b.cfg.HyperV.GuestPassword = "s3cret-pa$$word"

	_ = b.ensureOpenSSH(context.Background(), "crabbox-blue-1234", "crabbox")

	for _, call := range runner.calls {
		for _, arg := range call.Args {
			if strings.Contains(arg, "s3cret-pa$$word") {
				t.Fatal("guest password found in command args; should be passed via environment only")
			}
		}
	}
}

func TestResolveInstancePropagatesQueryError(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{},
		errors:    map[string]error{},
	}
	runner.errors[commandKey([]string{"-NoProfile", "-NonInteractive", "-Command",
		`Get-VM -Name 'crabbox-blue-1234' | Select-Object Name, State | ConvertTo-Json -Compress`})] =
		fmt.Errorf("powershell exec failed")
	runner.responses[commandKey([]string{"-NoProfile", "-NonInteractive", "-Command",
		`Get-VM -Name 'crabbox-blue-1234' | Select-Object Name, State | ConvertTo-Json -Compress`})] =
		core.LocalCommandResult{Stderr: "VM not found"}

	b := testBackend(runner)
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })

	_, _, err := b.resolveInstance(context.Background(), "crabbox-blue-1234")
	if err == nil {
		t.Fatal("resolveInstance should propagate VM query failure, not return synthetic State=2")
	}
	if !strings.Contains(err.Error(), "not reachable") && !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRemoveVMQueriesActualVHDPaths(t *testing.T) {
	customPath := `D:\VMs\crabbox-blue-1234.vhdx`
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{},
	}
	runner.responses[commandKey([]string{"-NoProfile", "-NonInteractive", "-Command",
		`Get-VMHardDiskDrive -VMName 'crabbox-blue-1234' -ErrorAction SilentlyContinue | Select-Object -ExpandProperty Path`})] =
		core.LocalCommandResult{Stdout: customPath + "\n"}
	b := testBackend(runner)
	_ = b.removeVM(context.Background(), "crabbox-blue-1234")

	var foundVHDQuery bool
	for _, call := range runner.calls {
		script := call.Args[len(call.Args)-1]
		if strings.Contains(script, "Get-VMHardDiskDrive") {
			foundVHDQuery = true
		}
	}
	if !foundVHDQuery {
		t.Error("removeVM did not query actual VHD paths before deletion")
	}
}

func TestCreateVMDisablesAutomaticCheckpoints(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	cfg := b.configForRun()
	cfg.HyperV.Image = `C:\Images\windows.vhdx`

	if err := b.createVM(context.Background(), cfg, "crabbox-blue-1234", ""); err != nil {
		t.Fatalf("createVM: %v", err)
	}

	var found bool
	for _, call := range runner.calls {
		script := call.Args[len(call.Args)-1]
		if strings.Contains(script, "Set-VM") && strings.Contains(script, "-AutomaticCheckpointsEnabled $false") {
			found = true
		}
	}
	if !found {
		t.Fatal("createVM should disable automatic checkpoints on lease VMs")
	}
}

// Regression: client Hyper-V auto-checkpoints attach a <name>_<guid>.avhdx in
// place of the base disk. removeVM must still delete the base VHDX and the
// per-VM config directory, or every lease leaks a disk-sized file on release.
func TestRemoveVMCleansBaseDiskAndDirDespiteCheckpoint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	name := "crabbox-blue-1234"
	vhdDir := hypervVHDDir()
	vmDir := filepath.Join(hypervVMDir(), name)
	if err := os.MkdirAll(vhdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		t.Fatal(err)
	}
	baseVHD := filepath.Join(vhdDir, name+".vhdx")
	checkpoint := filepath.Join(vhdDir, name+"_4F2A.avhdx")
	for _, p := range []string{baseVHD, checkpoint} {
		if err := os.WriteFile(p, []byte("disk"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	// The attached disk reported by Hyper-V is the checkpoint .avhdx, not the base.
	runner.responses[commandKey([]string{"-NoProfile", "-NonInteractive", "-Command",
		`Get-VMHardDiskDrive -VMName 'crabbox-blue-1234' -ErrorAction SilentlyContinue | Select-Object -ExpandProperty Path`})] =
		core.LocalCommandResult{Stdout: checkpoint + "\n"}
	b := testBackend(runner)

	if err := b.removeVM(context.Background(), name); err != nil {
		t.Fatalf("removeVM: %v", err)
	}
	for _, p := range []string{baseVHD, checkpoint, vmDir} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("removeVM left %s behind (err=%v)", p, err)
		}
	}
}

func TestConfigureRejectsWSL2Mode(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetWindows
	cfg.WindowsMode = core.WindowsModeWSL2
	if _, err := (Provider{}).Configure(cfg, core.Runtime{}); err == nil {
		t.Fatal("Configure accepted WSL2 mode")
	}
}

func TestCleanupNoOpOnNonWindows(t *testing.T) {
	b := testBackend(&recordingRunner{})
	oldOS := hypervHostOS
	hypervHostOS = "linux"
	t.Cleanup(func() { hypervHostOS = oldOS })

	err := b.Cleanup(context.Background(), core.CleanupRequest{})
	if err != nil {
		t.Fatalf("Cleanup on non-Windows should succeed (skip), got: %v", err)
	}
}

func TestListInstancesErrorOnNonWindows(t *testing.T) {
	b := testBackend(&recordingRunner{})
	oldOS := hypervHostOS
	hypervHostOS = "linux"
	t.Cleanup(func() { hypervHostOS = oldOS })

	_, err := b.listInstances(context.Background())
	if err == nil {
		t.Fatal("listInstances should return error on non-Windows")
	}
	if !errors.Is(err, errNotWindows) {
		t.Fatalf("expected errNotWindows, got: %v", err)
	}
}

func TestApplyDefaultsPreservesExplicitTarget(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.TargetOS = core.TargetWindows
	cfg.WindowsMode = core.WindowsModeNormal
	applyDefaults(&cfg)
	if cfg.TargetOS != core.TargetWindows {
		t.Fatalf("applyDefaults changed explicit TargetOS to %s", cfg.TargetOS)
	}
}

func TestApplyFlagsRejectsExplicitLinuxTarget(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux

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

func TestApplyFlagsRejectsExplicitMacOSTarget(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetMacOS

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String("target", "linux", "")
	if err := fs.Set("target", "macos"); err != nil {
		t.Fatal(err)
	}

	err := applyFlags(&cfg, fs, flagValues{})
	if err == nil {
		t.Fatal("applyFlags should reject explicit --target macos")
	}
}

func TestApplyFlagsDefaultsLinuxToWindows(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String("target", "linux", "")

	err := applyFlags(&cfg, fs, flagValues{})
	if err != nil {
		t.Fatalf("applyFlags failed: %v", err)
	}
	if cfg.TargetOS != core.TargetWindows {
		t.Fatalf("TargetOS=%s want windows (should default baseConfig linux to windows)", cfg.TargetOS)
	}
}

func TestApplyFlagsAcceptsExplicitWindows(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetWindows

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String("target", "linux", "")
	if err := fs.Set("target", "windows"); err != nil {
		t.Fatal(err)
	}

	err := applyFlags(&cfg, fs, flagValues{})
	if err != nil {
		t.Fatalf("applyFlags should accept explicit --target windows: %v", err)
	}
	if cfg.TargetOS != core.TargetWindows {
		t.Fatalf("TargetOS=%s want windows", cfg.TargetOS)
	}
}

// A non-Windows target set via YAML or env (no CLI --target flag) must be
// rejected, not silently rewritten to windows.
func TestApplyFlagsRejectsExplicitConfigTarget(t *testing.T) {
	for _, target := range []string{core.TargetLinux, core.TargetMacOS} {
		cfg := core.BaseConfig()
		cfg.Provider = providerName
		cfg.TargetOS = target
		core.MarkTargetExplicit(&cfg) // simulates target set from YAML/env

		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.String("target", "linux", "") // present but NOT set (no CLI flag)

		if err := applyFlags(&cfg, fs, flagValues{}); err == nil {
			t.Fatalf("applyFlags should reject explicit config target=%s, got TargetOS=%s", target, cfg.TargetOS)
		}
	}
}

func TestApplyFlagsAcceptsExplicitConfigWindows(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetWindows
	core.MarkTargetExplicit(&cfg)

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String("target", "linux", "")

	if err := applyFlags(&cfg, fs, flagValues{}); err != nil {
		t.Fatalf("applyFlags should accept explicit config target=windows: %v", err)
	}
	if cfg.TargetOS != core.TargetWindows {
		t.Fatalf("TargetOS=%s want windows", cfg.TargetOS)
	}
}

func TestApplyFlagsHyperVUserAndWorkRoot(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String("target", "linux", "")
	vals := registerFlags(fs, core.BaseConfig())
	if err := fs.Set("hyperv-user", "Administrator"); err != nil {
		t.Fatal(err)
	}
	if err := fs.Set("hyperv-work-root", `C:\work`); err != nil {
		t.Fatal(err)
	}
	if err := applyFlags(&cfg, fs, vals); err != nil {
		t.Fatalf("applyFlags: %v", err)
	}
	if cfg.HyperV.User != "Administrator" {
		t.Fatalf("--hyperv-user not applied: %q", cfg.HyperV.User)
	}
	if cfg.HyperV.WorkRoot != `C:\work` {
		t.Fatalf("--hyperv-work-root not applied: %q", cfg.HyperV.WorkRoot)
	}
}

func TestApplyFlagsHyperVInitPassword(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String("target", "linux", "")
	vals := registerFlags(fs, core.BaseConfig())
	if err := fs.Set("hyperv-init-password", "true"); err != nil {
		t.Fatal(err)
	}
	if err := applyFlags(&cfg, fs, vals); err != nil {
		t.Fatalf("applyFlags: %v", err)
	}
	if !cfg.HyperV.InitPassword {
		t.Fatal("--hyperv-init-password not applied")
	}
}
