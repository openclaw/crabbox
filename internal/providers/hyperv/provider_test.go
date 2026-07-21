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
	onRun     func(core.LocalCommandRequest)
	respond   func(core.LocalCommandRequest) (core.LocalCommandResult, error, bool)
	// blockUntilCtx simulates a command that blocks until cancellation.
	blockUntilCtx func(core.LocalCommandRequest) bool
}

func (r *recordingRunner) Run(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.calls = append(r.calls, req)
	if r.onRun != nil {
		r.onRun(req)
	}
	if r.blockUntilCtx != nil && r.blockUntilCtx(req) {
		<-ctx.Done()
		return core.LocalCommandResult{}, ctx.Err()
	}
	if r.respond != nil {
		if result, err, ok := r.respond(req); ok {
			return result, err
		}
	}
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
		Image:         `C:\Images\windows.vhdx`,
		User:          "crabbox",
		WorkRoot:      `C:\crabbox`,
		CPUs:          4,
		Memory:        8192,
		Switch:        "Default Switch",
		GuestPassword: "test-password",
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

func TestRemoveVMStorageRefusesMalformedCrabboxNames(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	b := testBackend(&recordingRunner{})

	for _, name := range []string{
		"crabbox-",
		"crabbox-../../target",
		`crabbox-..\..\target`,
		"crabbox-Blue-1234",
		"crabbox-blue-1234 ",
	} {
		t.Run(name, func(t *testing.T) {
			if err := b.removeVMStorage(name, nil); err == nil || !strings.Contains(err.Error(), "refusing") {
				t.Fatalf("removeVMStorage(%q) err=%v, want refusal", name, err)
			}
		})
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
		{`["0.0.0.0","127.0.0.1","169.254.1.2","224.0.0.1","192.168.1.20"]`, "192.168.1.20"},
		{`null`, ""},
		{`""`, ""},
		{`["fe80::1"]`, ""},
		{`["0.0.0.0","127.0.0.1","169.254.1.2","224.0.0.1"]`, ""},
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
		3:  "stopped",
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

func TestShouldCleanupProtectsRetainedLease(t *testing.T) {
	now := time.Now().UTC()
	server := Server{Status: "stopped", Labels: map[string]string{
		"keep":       "true",
		"expires_at": core.LeaseLabelTime(now.Add(-time.Hour)),
	}}
	claim := core.LeaseClaim{LeaseID: "cbx_123", Labels: server.Labels}
	if ok, reason := shouldCleanup(server, claim, true, now); ok || reason != "keep=true" {
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
	if ok, reason := shouldCleanup(server, core.LeaseClaim{}, false, time.Now()); ok || reason != "missing claim" {
		t.Fatalf("cleanup=%v reason=%s, want false missing claim", ok, reason)
	}
}

func TestReleaseRequiresExactClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })
	const leaseID = "cbx_unclaimed12345"
	const name = "crabbox-unclaimed-1234"
	runner := &recordingRunner{}
	b := testBackend(runner)
	lease := LeaseTarget{
		LeaseID: leaseID,
		Server:  Server{CloudID: name, Labels: map[string]string{"lease": leaseID, "instance": name}},
	}
	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err == nil || !strings.Contains(err.Error(), "no exact local claim") {
		t.Fatalf("ReleaseLease unclaimed err=%v", err)
	}
	for _, call := range runner.calls {
		if len(call.Args) > 0 && strings.Contains(call.Args[len(call.Args)-1], "Remove-VM") {
			t.Fatalf("unclaimed release called Remove-VM: %#v", call)
		}
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

func TestServerFromInstanceOverridesStaleReadyState(t *testing.T) {
	b := testBackend(&recordingRunner{})
	server := b.serverFromInstance(
		hypervVM{Name: "crabbox-blue-1234", State: 3},
		core.LeaseClaim{Labels: map[string]string{"state": "ready"}},
		b.configForRun(),
	)
	if server.Status != "stopped" || server.Labels["state"] != "stopped" {
		t.Fatalf("status=%q state=%q, want stopped", server.Status, server.Labels["state"])
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

	err := b.createVM(context.Background(), cfg, "crabbox-blue-1234")
	if err != nil {
		t.Fatalf("createVM: %v", err)
	}

	var foundDiff, foundNewVM, foundStart, foundConnect, foundInject bool
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
		if strings.Contains(script, "Connect-VMNetworkAdapter") {
			foundConnect = true
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
	if foundConnect || foundInject {
		t.Error("createVM must leave the VM disconnected and defer guest SSH configuration to Acquire")
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

	if err := b.createVM(context.Background(), cfg, "crabbox-blue-1234"); err != nil {
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

	if err := b.createVM(context.Background(), cfg, "crabbox-blue-1234"); err != nil {
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

	if err := b.createVM(context.Background(), cfg, "crabbox-blue-1234"); err != nil {
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
			if !strings.Contains(script, "HKLM\\crabbox-init-") || !strings.Contains(script, "HKLM:\\crabbox-init-") {
				t.Errorf("init-password script missing unique hive name: %s", script)
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

func TestHyperVInitHiveNameIsUniqueAndSafe(t *testing.T) {
	first := hypervInitHiveName(`C:\Hyper-V\one.vhdx`)
	second := hypervInitHiveName(`C:\Hyper-V\two.vhdx`)
	if first == second {
		t.Fatalf("hive names collide: %q", first)
	}
	for _, name := range []string{first, second} {
		if !strings.HasPrefix(name, "crabbox-init-") || strings.ContainsAny(name, `\/: `) {
			t.Fatalf("unsafe hive name %q", name)
		}
	}
}

func TestCreateVMNoInitPasswordByDefault(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	cfg := b.configForRun()
	cfg.HyperV.Image = `C:\Images\windows.vhdx`

	if err := b.createVM(context.Background(), cfg, "crabbox-blue-1234"); err != nil {
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

func TestAcquireRequiresExplicitGuestPassword(t *testing.T) {
	b := testBackend(&recordingRunner{})
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })

	b.cfg.HyperV.GuestPassword = ""
	_, err := b.Acquire(context.Background(), core.AcquireRequest{})
	if err == nil || !strings.Contains(err.Error(), "requires an explicit CRABBOX_HYPERV_GUEST_PASSWORD") {
		t.Fatalf("Acquire should require an explicit guest password, got: %v", err)
	}
}

func TestAcquireRejectsUnsafeSSHUser(t *testing.T) {
	b := testBackend(&recordingRunner{})
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })

	for _, user := range []string{"user name", `DOMAIN\user`, "user@domain", "user*", "user?"} {
		b.cfg.HyperV.User = user
		_, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}})
		if err == nil || !strings.Contains(err.Error(), "--hyperv-user must be a local account name") {
			t.Fatalf("Acquire should reject SSH user %q, got: %v", user, err)
		}
	}
}

func TestAcquireQuarantinesSSHBeforeConnectingNetwork(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })

	ctx, cancel := context.WithCancel(context.Background())
	var claimSeenBeforeStart bool
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.onRun = func(req core.LocalCommandRequest) {
		if len(req.Args) == 0 {
			return
		}
		script := req.Args[len(req.Args)-1]
		if strings.Contains(script, "Start-VM") {
			claims, err := listLeaseClaims()
			claimSeenBeforeStart = err == nil && len(claims) == 1
		}
		if strings.Contains(script, "Connect-VMNetworkAdapter") {
			cancel()
		}
	}
	runner.respond = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if len(req.Args) > 0 {
			script := req.Args[len(req.Args)-1]
			if strings.Contains(script, "Get-VMNetworkAdapter") && strings.Contains(script, "ConvertTo-Json") {
				return core.LocalCommandResult{Stdout: `["192.0.2.10"]`}, nil, true
			}
		}
		return core.LocalCommandResult{}, nil, false
	}
	b := testBackend(runner)
	_, err := b.Acquire(ctx, core.AcquireRequest{
		Repo:          core.Repo{Root: t.TempDir()},
		RequestedSlug: "network-order",
	})
	if err == nil {
		t.Fatal("Acquire unexpectedly succeeded after test cancellation")
	}
	if !claimSeenBeforeStart {
		t.Fatal("Acquire started the VM before persisting its provisional claim")
	}

	startIdx, stageIdx, connectIdx, activateIdx := -1, -1, -1, -1
	for i, call := range runner.calls {
		if len(call.Args) == 0 {
			continue
		}
		script := call.Args[len(call.Args)-1]
		switch {
		case strings.Contains(script, "Start-VM"):
			startIdx = i
		case strings.Contains(script, "Crabbox-SSH-Quarantine") && !strings.Contains(script, "Remove-NetFirewallRule"):
			stageIdx = i
		case strings.Contains(script, "Connect-VMNetworkAdapter"):
			connectIdx = i
		case strings.Contains(script, "Remove-NetFirewallRule") && strings.Contains(script, "Start-Service sshd"):
			activateIdx = i
		}
	}
	if startIdx < 0 || stageIdx < 0 || connectIdx < 0 || activateIdx < 0 {
		t.Fatalf("missing lifecycle calls start=%d stage=%d connect=%d activate=%d", startIdx, stageIdx, connectIdx, activateIdx)
	}
	if !(startIdx < stageIdx && stageIdx < connectIdx && connectIdx < activateIdx) {
		t.Fatalf("unsafe lifecycle order start=%d stage=%d connect=%d activate=%d", startIdx, stageIdx, connectIdx, activateIdx)
	}
}

func TestAcquireKeepPersistsClaimAndKeyBeforeBootstrap(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })

	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := b.Acquire(ctx, core.AcquireRequest{
		Repo:          core.Repo{Root: t.TempDir()},
		RequestedSlug: "retained-failure",
		Keep:          true,
	})
	if err == nil {
		t.Fatal("Acquire unexpectedly succeeded")
	}

	claims, claimErr := listLeaseClaims()
	if claimErr != nil {
		t.Fatalf("listLeaseClaims: %v", claimErr)
	}
	if len(claims) != 1 {
		t.Fatalf("claims=%#v, want retained lease claim", claims)
	}
	claim := claims[0]
	t.Cleanup(func() {
		removeLeaseClaim(claim.LeaseID)
		removeStoredTestboxKey(claim.LeaseID)
	})
	if claim.Provider != providerName || instanceNameFromClaim(claim) == "" {
		t.Fatalf("retained claim missing provider identity: %#v", claim)
	}
	keyPath, keyErr := testboxKeyPath(claim.LeaseID)
	if keyErr != nil {
		t.Fatalf("testboxKeyPath: %v", keyErr)
	}
	if _, statErr := os.Stat(keyPath); statErr != nil {
		t.Fatalf("retained lease key missing: %v", statErr)
	}
}

func TestAcquirePersistsProvisionalClaimThenRollsBackFailedLease(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })

	var observed core.LeaseClaim
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.onRun = func(req core.LocalCommandRequest) {
		if len(req.Args) == 0 || !strings.Contains(req.Args[len(req.Args)-1], "Get-VMNetworkAdapter") {
			return
		}
		claims, err := listLeaseClaims()
		if err == nil && len(claims) == 1 {
			observed = claims[0]
		}
	}
	b := testBackend(runner)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := b.Acquire(ctx, core.AcquireRequest{
		Repo:          core.Repo{Root: t.TempDir()},
		RequestedSlug: "rollback-failure",
	})
	if err == nil {
		t.Fatal("Acquire unexpectedly succeeded")
	}
	if observed.LeaseID == "" || observed.Provider != providerName || instanceNameFromClaim(observed) == "" {
		t.Fatalf("bootstrap started without a provisional claim: %#v", observed)
	}
	claims, claimErr := listLeaseClaims()
	if claimErr != nil {
		t.Fatalf("listLeaseClaims: %v", claimErr)
	}
	if len(claims) != 0 {
		t.Fatalf("failed non-retained lease left claims: %#v", claims)
	}
	keyPath, keyErr := testboxKeyPath(observed.LeaseID)
	if keyErr != nil {
		t.Fatalf("testboxKeyPath: %v", keyErr)
	}
	if _, statErr := os.Stat(keyPath); !os.IsNotExist(statErr) {
		t.Fatalf("failed non-retained lease left key at %s: %v", keyPath, statErr)
	}
}

func TestAcquireInitPasswordRejectsCmdUnsafePassword(t *testing.T) {
	b := testBackend(&recordingRunner{})
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })

	b.cfg.HyperV.InitPassword = true
	for _, password := range []string{`pa"ss`, `pa%ss`, `%TEMP%`, `a"&calc&"b`} {
		b.cfg.HyperV.GuestPassword = password
		_, err := b.Acquire(context.Background(), core.AcquireRequest{})
		if err == nil || !strings.Contains(err.Error(), "cannot carry") {
			t.Fatalf("Acquire should reject cmd-unsafe init password %q, got: %v", password, err)
		}
	}
}

// The user name lands inside the same double-quoted cmd.exe RunOnce command
// as the password, so it gets the same metacharacter validation -- an
// embedded quote must not be able to alter the elevated first-boot command.
func TestAcquireInitPasswordRejectsCmdUnsafeUser(t *testing.T) {
	b := testBackend(&recordingRunner{})
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })

	b.cfg.HyperV.InitPassword = true
	b.cfg.HyperV.GuestPassword = "SafePass1!"
	for _, user := range []string{`us"er`, `us%er`, `u" & calc & "`, `%PATH%`} {
		b.cfg.HyperV.User = user
		_, err := b.Acquire(context.Background(), core.AcquireRequest{})
		if err == nil || !strings.Contains(err.Error(), "user name") {
			t.Fatalf("Acquire should reject cmd-unsafe init user %q, got: %v", user, err)
		}
	}
}

// Acquire persists the lease claim and its SSH endpoint in ONE atomic write.
// Success must leave a claim that already carries the endpoint -- there is no
// separate endpoint update whose failure could strand a half-written claim.
func TestPersistLeaseWritesClaimAndEndpointAtomically(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	b := testBackend(&recordingRunner{})
	cfg := b.configForRun()
	lease := LeaseTarget{LeaseID: "cbx_atomic123456"}
	lease.Server = b.serverFromInstance(hypervVM{Name: "crabbox-atom-1234", State: 2}, core.LeaseClaim{}, cfg)
	lease.Server.PublicNet.IPv4.IP = "172.20.0.9"
	lease.SSH = sshTargetFromConfig(cfg, "172.20.0.9")

	req := AcquireRequest{}
	req.Repo.Root = t.TempDir()
	if err := persistLease("cbx_atomic123456", "atomslug", "crabbox-atom-1234", cfg, req, lease); err != nil {
		t.Fatalf("persistLease: %v", err)
	}
	t.Cleanup(func() { removeLeaseClaim("cbx_atomic123456") })

	claims, err := listLeaseClaims()
	if err != nil {
		t.Fatalf("listLeaseClaims: %v", err)
	}
	var found *core.LeaseClaim
	for i := range claims {
		if claims[i].LeaseID == "cbx_atomic123456" {
			found = &claims[i]
		}
	}
	if found == nil {
		t.Fatal("persistLease did not write the claim")
	}
	if found.SSHHost != "172.20.0.9" {
		t.Fatalf("claim SSHHost=%q want 172.20.0.9 (endpoint must be in the same write as the claim)", found.SSHHost)
	}
	if instanceNameFromClaim(*found) != "crabbox-atom-1234" {
		t.Fatalf("claim instance=%q want crabbox-atom-1234", instanceNameFromClaim(*found))
	}
}

// When the atomic persist fails, NO claim may remain: Acquire's error path
// removes the VM, so any surviving lease state would point at a resource that
// no longer exists.
func TestPersistLeaseFailureLeavesNoStaleClaim(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "state-not-a-dir")
	if err := os.WriteFile(blocker, []byte("plain file"), 0o644); err != nil {
		t.Fatal(err)
	}
	// crabboxStateDir joins XDG_STATE_HOME with "crabbox"; pointing it at a
	// plain file makes every claim write fail.
	t.Setenv("XDG_STATE_HOME", blocker)

	b := testBackend(&recordingRunner{})
	cfg := b.configForRun()
	lease := LeaseTarget{LeaseID: "cbx_atomfail12345"}
	lease.Server = b.serverFromInstance(hypervVM{Name: "crabbox-atom-fail", State: 2}, core.LeaseClaim{}, cfg)
	lease.SSH = sshTargetFromConfig(cfg, "172.20.0.10")

	req := AcquireRequest{}
	req.Repo.Root = t.TempDir()
	if err := persistLease("cbx_atomfail12345", "atomfail", "crabbox-atom-fail", cfg, req, lease); err == nil {
		t.Fatal("persistLease should fail when the state directory is unwritable")
	}
	info, err := os.Stat(blocker)
	if err != nil || info.IsDir() {
		t.Fatalf("state path mutated: err=%v isDir=%v -- nothing may be persisted on failure", err, info.IsDir())
	}
}

func TestResolveStatusOnlyAllowsRetainedLeaseWithoutIP(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	name := "crabbox-retained-no-ip"
	queryScript := fmt.Sprintf(`Get-VM -ErrorAction Stop | Where-Object { $_.Name -eq '%s' } | Select-Object Name, State | ConvertTo-Json -Compress`, name)
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		queryScript: {Stdout: `{"Name":"crabbox-retained-no-ip","State":2}`},
	}}
	b := testBackend(runner)
	cfg := b.configForRun()
	req := AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, Keep: true}
	claim := core.LeaseClaim{
		LeaseID:       "cbx_retained12345",
		Slug:          "retained-no-ip",
		Provider:      providerName,
		ProviderScope: instanceScope(name),
		Labels:        map[string]string{"instance": name, "state": "leased"},
	}
	lease := LeaseTarget{
		Server:  b.serverFromInstance(hypervVM{Name: name, State: 2}, claim, cfg),
		LeaseID: claim.LeaseID,
	}
	if err := persistLease(claim.LeaseID, claim.Slug, name, cfg, req, lease); err != nil {
		t.Fatalf("persistLease: %v", err)
	}
	t.Cleanup(func() { removeLeaseClaim(claim.LeaseID) })

	resolved, err := b.Resolve(context.Background(), ResolveRequest{ID: claim.LeaseID, StatusOnly: true})
	if err != nil {
		t.Fatalf("Resolve status-only: %v", err)
	}
	if resolved.LeaseID != claim.LeaseID || resolved.Server.PublicNet.IPv4.IP != "" {
		t.Fatalf("resolved=%#v, want endpoint-less retained lease", resolved)
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
				`Get-VM -ErrorAction Stop | Where-Object { $_.Name -eq 'crabbox-blue-1234' } | Select-Object Name, State | ConvertTo-Json -Compress`}): {
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

func TestReleasePrunesClaimAndKeyWhenVMIsMissing(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })

	const leaseID = "cbx_missing123456"
	const name = "crabbox-missing-1234"
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	cfg := b.configForRun()
	if _, _, err := ensureTestboxKeyForConfig(cfg, leaseID); err != nil {
		t.Fatalf("ensureTestboxKeyForConfig: %v", err)
	}
	claim := core.LeaseClaim{
		LeaseID:       leaseID,
		Slug:          "missing-vm",
		Provider:      providerName,
		ProviderScope: instanceScope(name),
		Labels:        map[string]string{"instance": name, "lease": leaseID},
	}
	lease := LeaseTarget{
		Server:  b.serverFromInstance(hypervVM{Name: name, State: 2}, claim, cfg),
		LeaseID: leaseID,
	}
	req := AcquireRequest{Repo: core.Repo{Root: t.TempDir()}}
	if err := persistLease(leaseID, claim.Slug, name, cfg, req, lease); err != nil {
		t.Fatalf("persistLease: %v", err)
	}
	baseVHD := filepath.Join(hypervVHDDir(), name+".vhdx")
	vmConfig := filepath.Join(hypervVMDir(), name, "Virtual Machines", "lease.vmcx")
	if err := os.MkdirAll(filepath.Dir(baseVHD), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(vmConfig), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(baseVHD, []byte("disk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vmConfig, []byte("config"), 0o644); err != nil {
		t.Fatal(err)
	}

	resolved, err := b.Resolve(context.Background(), ResolveRequest{ID: leaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatalf("Resolve release-only: %v", err)
	}
	if resolved.Server.Status != "missing" {
		t.Fatalf("resolved status=%q want missing", resolved.Server.Status)
	}
	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: resolved}); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}
	if claims, err := listLeaseClaims(); err != nil || len(claims) != 0 {
		t.Fatalf("claims after release=%#v err=%v", claims, err)
	}
	keyPath, err := testboxKeyPath(leaseID)
	if err != nil {
		t.Fatalf("testboxKeyPath: %v", err)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("missing VM release left key %s: %v", keyPath, err)
	}
	for _, path := range []string{baseVHD, vmConfig} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("missing VM release left storage %s: %v", path, err)
		}
	}
	for _, call := range runner.calls {
		if len(call.Args) > 0 && strings.Contains(call.Args[len(call.Args)-1], "Remove-VM") {
			t.Fatal("missing VM release should prune local state without calling Remove-VM")
		}
	}
}

func TestCleanupMissingClaimRemovesDeterministicStorage(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })

	const leaseID = "cbx_cleanupmissing"
	const name = "crabbox-cleanup-missing"
	b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{}})
	cfg := b.configForRun()
	claim := core.LeaseClaim{
		LeaseID:       leaseID,
		Slug:          "cleanup-missing",
		Provider:      providerName,
		ProviderScope: instanceScope(name),
		Labels: map[string]string{
			"instance":   name,
			"lease":      leaseID,
			"state":      "provisioning",
			"created_at": core.LeaseLabelTime(time.Now().Add(-2 * hypervProvisioningClaimGrace)),
		},
	}
	lease := LeaseTarget{
		Server:  b.serverFromInstance(hypervVM{Name: name, State: 2}, claim, cfg),
		LeaseID: leaseID,
	}
	if err := persistLease(leaseID, claim.Slug, name, cfg, AcquireRequest{Repo: core.Repo{Root: t.TempDir()}}, lease); err != nil {
		t.Fatalf("persistLease: %v", err)
	}
	baseVHD := filepath.Join(hypervVHDDir(), name+".vhdx")
	if err := os.MkdirAll(filepath.Dir(baseVHD), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(baseVHD, []byte("disk"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, err := os.Stat(baseVHD); !os.IsNotExist(err) {
		t.Fatalf("cleanup left deterministic disk %s: %v", baseVHD, err)
	}
	if claims, err := listLeaseClaims(); err != nil || len(claims) != 0 {
		t.Fatalf("claims after cleanup=%#v err=%v", claims, err)
	}
}

func TestCleanupMissingKeepClaimPreservesStorage(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })

	const leaseID = "cbx_cleanupkeep"
	const name = "crabbox-cleanup-keep"
	b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{}})
	cfg := b.configForRun()
	claim := core.LeaseClaim{
		LeaseID:       leaseID,
		Slug:          "cleanup-keep",
		Provider:      providerName,
		ProviderScope: instanceScope(name),
		Labels: map[string]string{
			"instance":   name,
			"lease":      leaseID,
			"state":      "ready",
			"keep":       "true",
			"created_at": core.LeaseLabelTime(time.Now().Add(-2 * hypervProvisioningClaimGrace)),
		},
	}
	lease := LeaseTarget{
		Server:  b.serverFromInstance(hypervVM{Name: name, State: 2}, claim, cfg),
		LeaseID: leaseID,
	}
	if err := persistLease(leaseID, claim.Slug, name, cfg, AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, Keep: true}, lease); err != nil {
		t.Fatalf("persistLease: %v", err)
	}
	baseVHD := filepath.Join(hypervVHDDir(), name+".vhdx")
	if err := os.MkdirAll(filepath.Dir(baseVHD), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(baseVHD, []byte("disk"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, err := os.Stat(baseVHD); err != nil {
		t.Fatalf("cleanup removed retained disk %s: %v", baseVHD, err)
	}
	if claims, err := listLeaseClaims(); err != nil || len(claims) != 1 || claims[0].LeaseID != leaseID {
		t.Fatalf("claims after cleanup=%#v err=%v", claims, err)
	}
}

func TestMissingClaimCleanupWaitsForProvisioningGrace(t *testing.T) {
	now := time.Now().UTC()
	claim := core.LeaseClaim{
		LeaseID: "cbx_provisioning",
		Labels: map[string]string{
			"state":      "provisioning",
			"created_at": core.LeaseLabelTime(now),
		},
	}
	if ready, reason := missingClaimCleanupReady(claim, now); ready || reason != "provisioning grace" {
		t.Fatalf("ready=%v reason=%q", ready, reason)
	}
	claim.Labels["created_at"] = core.LeaseLabelTime(now.Add(-2 * hypervProvisioningClaimGrace))
	if ready, reason := missingClaimCleanupReady(claim, now); !ready || reason != "" {
		t.Fatalf("stale claim ready=%v reason=%q", ready, reason)
	}
	claim.Labels["keep"] = "true"
	if ready, reason := missingClaimCleanupReady(claim, now); ready || reason != "keep=true" {
		t.Fatalf("retained claim ready=%v reason=%q", ready, reason)
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
	for _, want := range []string{"icacls.exe", "/inheritance:r", "*S-1-5-18:F", "*S-1-5-32-544:F", "$LASTEXITCODE", "Port|ListenAddress|AddressFamily|HostKey|", "Port 22", "AddressFamily any", "PasswordAuthentication no", "PubkeyAuthentication yes", "AuthenticationMethods publickey", "AllowUsers administrator", "|Include)", "sshd_config validation failed", "ssh_host_*", "ssh-keygen.exe", "-A", "SSH host key generation failed", "SecurityIdentifier]::new('S-1-5-18')", "SecurityIdentifier]::new('S-1-5-32-544')", "$hostKeyACL.SetOwner($adminsSID)", "SetAccessRuleProtection($true, $false)", "Set-Acl -LiteralPath $_.FullName", "Start-Service sshd", "OpenSSH-Server-In-TCP", "Crabbox-SSH-Quarantine", "Remove-NetFirewallRule"} {
		if !strings.Contains(script, want) {
			t.Errorf("admin-key SSH lockdown missing %q\nscript: %s", want, script)
		}
	}
	if strings.Contains(script, "$globalLines += 'KbdInteractiveAuthentication") {
		t.Fatal("Windows OpenSSH does not support KbdInteractiveAuthentication")
	}
	if strings.Index(script, "ssh-keygen.exe") > strings.Index(script, "Start-Service sshd") {
		t.Fatal("SSH host keys must be regenerated before sshd starts")
	}
	if strings.Index(script, "ssh-keygen.exe") > strings.Index(script, "& $sshdExe -t") {
		t.Fatal("SSH host keys must be regenerated before sshd_config validation")
	}
	if strings.Index(script, "Set-Acl -LiteralPath $_.FullName") > strings.Index(script, "& $sshdExe -t") {
		t.Fatal("SSH host key ACLs must be locked down before sshd_config validation")
	}
	if strings.Contains(script, "Add-Content") {
		t.Fatalf("SSH key injection retained template keys: %s", script)
	}
	if strings.Count(script, "Set-Content -Encoding ASCII") < 3 {
		t.Fatalf("SSH key injection should replace user, administrator, and config files: %s", script)
	}
	if strings.Index(script, "Remove-NetFirewallRule") < strings.Index(script, "Start-Service sshd") {
		t.Fatal("SSH quarantine must be removed only after sshd starts with the final config")
	}
}

func TestStageSSHKeyKeepsPortQuarantined(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)

	if err := b.stageSSHKey(context.Background(), "crabbox-blue-1234", "crabbox", "ssh-ed25519 AAAA test"); err != nil {
		t.Fatalf("stageSSHKey: %v", err)
	}
	script := runner.calls[len(runner.calls)-1].Args[len(runner.calls[len(runner.calls)-1].Args)-1]
	for _, want := range []string{"Crabbox-SSH-Quarantine", "Action Block", "Stop-Service", "StartupType Disabled", "AllowUsers crabbox"} {
		if !strings.Contains(script, want) {
			t.Errorf("pre-network SSH lockdown missing %q", want)
		}
	}
	for _, unwanted := range []string{"Start-Service sshd", "Remove-NetFirewallRule", "ssh-keygen.exe"} {
		if strings.Contains(script, unwanted) {
			t.Errorf("pre-network SSH lockdown must not activate SSH: %s", script)
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

func TestEnsureOpenSSHInstallsWithSshdClosed(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)

	if err := b.ensureOpenSSH(context.Background(), "crabbox-blue-1234", "crabbox"); err != nil {
		t.Fatalf("ensureOpenSSH: %v", err)
	}

	var script string
	for _, call := range runner.calls {
		s := call.Args[len(call.Args)-1]
		if strings.Contains(s, "Invoke-Command") && strings.Contains(s, "crabbox-openssh.msi") {
			script = s
		}
	}
	if script == "" {
		t.Fatal("ensureOpenSSH should invoke an OpenSSH install over PowerShell Direct")
	}
	for _, want := range []string{
		"Get-CimInstance Win32_Processor",
		"switch ([int]$nativeArch)",
		"9 {",
		win32OpenSSHAMD64URL,
		win32OpenSSHAMD64SHA256,
		"12 {",
		win32OpenSSHARM64URL,
		win32OpenSSHARM64SHA256,
		"unsupported Windows architecture for OpenSSH",
		"Get-FileHash",
		"msiexec.exe",
		"Get-Service -Name sshd",
		"Stop-Service",
		"StartupType Manual",
		"Disable-NetFirewallRule",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("ensureOpenSSH script missing %q", want)
		}
	}
	for _, unwanted := range []string{"Add-WindowsCapability", "OpenSSH.Server~~~~"} {
		if strings.Contains(script, unwanted) {
			t.Errorf("ensureOpenSSH should no longer use Features-on-Demand: %s", script)
		}
	}
	for _, unwanted := range []string{"Start-Service sshd", "New-NetFirewallRule"} {
		if strings.Contains(script, unwanted) {
			t.Errorf("ensureOpenSSH exposes SSH before key-only configuration: %s", script)
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
		`Get-VM -ErrorAction Stop | Where-Object { $_.Name -eq 'crabbox-blue-1234' } | Select-Object Name, State | ConvertTo-Json -Compress`})] =
		fmt.Errorf("powershell exec failed")
	runner.responses[commandKey([]string{"-NoProfile", "-NonInteractive", "-Command",
		`Get-VM -ErrorAction Stop | Where-Object { $_.Name -eq 'crabbox-blue-1234' } | Select-Object Name, State | ConvertTo-Json -Compress`})] =
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
		`Get-VM -ErrorAction Stop | Where-Object { $_.Name -eq 'crabbox-blue-1234' } | Select-Object Name, State | ConvertTo-Json -Compress`})] =
		core.LocalCommandResult{Stdout: `{"Name":"crabbox-blue-1234","State":2}`}
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

	if err := b.createVM(context.Background(), cfg, "crabbox-blue-1234"); err != nil {
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
	checkpoint := filepath.Join(vhdDir, name+"_4F2A4D3E-59D0-42B4-9D33-CA8B34C7CB4A.avhdx")
	dataDisk := filepath.Join(vhdDir, name+"-data.vhdx")
	for _, p := range []string{baseVHD, checkpoint, dataDisk} {
		if err := os.WriteFile(p, []byte("disk"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.responses[commandKey([]string{"-NoProfile", "-NonInteractive", "-Command",
		`Get-VM -ErrorAction Stop | Where-Object { $_.Name -eq 'crabbox-blue-1234' } | Select-Object Name, State | ConvertTo-Json -Compress`})] =
		core.LocalCommandResult{Stdout: `{"Name":"crabbox-blue-1234","State":2}`}
	// The attached disk reported by Hyper-V is the checkpoint .avhdx, not the base.
	runner.responses[commandKey([]string{"-NoProfile", "-NonInteractive", "-Command",
		`Get-VMHardDiskDrive -VMName 'crabbox-blue-1234' -ErrorAction SilentlyContinue | Select-Object -ExpandProperty Path`})] =
		core.LocalCommandResult{Stdout: checkpoint + "\n" + dataDisk + "\n"}
	b := testBackend(runner)

	if err := b.removeVM(context.Background(), name); err != nil {
		t.Fatalf("removeVM: %v", err)
	}
	for _, p := range []string{baseVHD, checkpoint, vmDir} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("removeVM left %s behind (err=%v)", p, err)
		}
	}
	if _, err := os.Stat(dataDisk); err != nil {
		t.Fatalf("removeVM deleted attached user data disk %s: %v", dataDisk, err)
	}
}

func TestRemoveVMPreservesDataDiskInsideVMDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	name := "crabbox-blue-1234"
	vmDir := filepath.Join(hypervVMDir(), name)
	dataDisk := filepath.Join(vmDir, "Virtual Hard Disks", "user-data.vhdx")
	configFile := filepath.Join(vmDir, "Virtual Machines", "lease.vmcx")
	for _, path := range []string{dataDisk, configFile} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.responses[commandKey([]string{"-NoProfile", "-NonInteractive", "-Command",
		`Get-VM -ErrorAction Stop | Where-Object { $_.Name -eq 'crabbox-blue-1234' } | Select-Object Name, State | ConvertTo-Json -Compress`})] =
		core.LocalCommandResult{Stdout: `{"Name":"crabbox-blue-1234","State":2}`}
	b := testBackend(runner)

	if err := b.removeVM(context.Background(), name); err != nil {
		t.Fatalf("removeVM: %v", err)
	}
	if _, err := os.Stat(dataDisk); err != nil {
		t.Fatalf("removeVM deleted user data disk: %v", err)
	}
	if _, err := os.Stat(configFile); !os.IsNotExist(err) {
		t.Fatalf("removeVM left Hyper-V config file: %v", err)
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

func TestApplyFlagsDefersTargetValidation(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	core.MarkTargetExplicit(&cfg)

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String("target", "linux", "")

	if err := applyFlags(&cfg, fs, flagValues{}); err != nil {
		t.Fatalf("applyFlags should defer target validation: %v", err)
	}
	if cfg.TargetOS != core.TargetLinux {
		t.Fatalf("applyFlags rewrote explicit target to %s", cfg.TargetOS)
	}
	if _, err := (Provider{}).Configure(cfg, core.Runtime{}); err == nil {
		t.Fatal("central provider configuration should reject target=linux")
	}
}

func TestApplyFlagsAllowsLaterWindowsOverride(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String("target", "linux", "")
	if err := fs.Set("target", "windows"); err != nil {
		t.Fatal(err)
	}

	err := applyFlags(&cfg, fs, flagValues{})
	if err != nil {
		t.Fatalf("applyFlags should not reject a target flag before it is applied: %v", err)
	}
	cfg.TargetOS = core.TargetWindows
	if cfg.TargetOS != core.TargetWindows {
		t.Fatalf("TargetOS=%s want windows", cfg.TargetOS)
	}
	if _, err := (Provider{}).Configure(cfg, core.Runtime{}); err != nil {
		t.Fatalf("provider should accept target after override: %v", err)
	}
}

func TestApplyFlagsDefaultsImplicitTargetToWindows(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String("target", "linux", "")

	if err := applyFlags(&cfg, fs, flagValues{}); err != nil {
		t.Fatalf("applyFlags: %v", err)
	}
	if cfg.TargetOS != core.TargetWindows {
		t.Fatalf("TargetOS=%s want windows", cfg.TargetOS)
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

func readinessProbe(req core.LocalCommandRequest) bool {
	if len(req.Args) == 0 {
		return false
	}
	return strings.Contains(req.Args[len(req.Args)-1], "{ $true }")
}

// A blocked readiness probe must time out and retry.
func TestWaitGuestReadyRetriesThroughBlockingBoot(t *testing.T) {
	runner := &recordingRunner{}
	blocked := 0
	runner.blockUntilCtx = func(req core.LocalCommandRequest) bool {
		if readinessProbe(req) && blocked < 2 {
			blocked++
			return true
		}
		return false
	}
	b := testBackend(runner)
	b.guestReadyProbeTimeout = 20 * time.Millisecond
	b.guestReadyBudget = 5 * time.Second
	b.guestRetryBackoff = time.Millisecond

	if err := b.waitGuestReady(context.Background(), "crabbox-blue-1234", "crabbox"); err != nil {
		t.Fatalf("waitGuestReady should succeed after transient boot blocking: %v", err)
	}
	if blocked != 2 {
		t.Fatalf("expected 2 blocked probes before success, got %d", blocked)
	}
	probes := 0
	for _, c := range runner.calls {
		if readinessProbe(c) {
			probes++
		}
	}
	if probes < 3 {
		t.Fatalf("expected >=3 readiness probes (2 blocked + 1 success), got %d", probes)
	}
}

// An unresponsive guest must fail at the boot budget.
func TestWaitGuestReadyFailsAfterBudget(t *testing.T) {
	runner := &recordingRunner{}
	runner.blockUntilCtx = func(req core.LocalCommandRequest) bool { return readinessProbe(req) }
	b := testBackend(runner)
	b.guestReadyProbeTimeout = 15 * time.Millisecond
	b.guestReadyBudget = 60 * time.Millisecond
	b.guestRetryBackoff = time.Millisecond

	err := b.waitGuestReady(context.Background(), "crabbox-blue-1234", "crabbox")
	if err == nil {
		t.Fatal("waitGuestReady should fail once the boot budget is exceeded")
	}
	if !strings.Contains(err.Error(), "did not accept PowerShell Direct") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWaitGuestReadyAbortsOnContextCancel(t *testing.T) {
	runner := &recordingRunner{}
	runner.blockUntilCtx = func(core.LocalCommandRequest) bool { return true }
	b := testBackend(runner)
	b.guestReadyProbeTimeout = time.Second
	b.guestReadyBudget = time.Minute
	b.guestRetryBackoff = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.waitGuestReady(ctx, "crabbox-blue-1234", "crabbox"); !errors.Is(err, context.Canceled) {
		t.Fatalf("waitGuestReady should return context.Canceled, got: %v", err)
	}
}

// A blocked guest call must time out and retry.
func TestInvokeInGuestBoundsBlockingAttempt(t *testing.T) {
	runner := &recordingRunner{}
	first := true
	runner.blockUntilCtx = func(core.LocalCommandRequest) bool {
		if first {
			first = false
			return true
		}
		return false
	}
	b := testBackend(runner)
	b.guestInvokeTimeout = 20 * time.Millisecond
	b.guestRetryBackoff = time.Millisecond

	if err := b.invokeInGuest(context.Background(), "crabbox-blue-1234", "crabbox", "1", "probe"); err != nil {
		t.Fatalf("invokeInGuest should retry past a wedged attempt: %v", err)
	}
	if len(runner.calls) < 2 {
		t.Fatalf("expected a retry after the bounded attempt, got %d calls", len(runner.calls))
	}
}

func TestInvokeInGuestAbortsOnContextCancel(t *testing.T) {
	runner := &recordingRunner{}
	runner.blockUntilCtx = func(core.LocalCommandRequest) bool { return true }
	b := testBackend(runner)
	b.guestInvokeTimeout = time.Second
	b.guestRetryBackoff = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.invokeInGuest(ctx, "crabbox-blue-1234", "crabbox", "1", "probe"); !errors.Is(err, context.Canceled) {
		t.Fatalf("invokeInGuest should return context.Canceled, got: %v", err)
	}
}

// The readiness probe must precede the SSH lockdown.
func TestAcquireWaitsForGuestReadyBeforeLockdown(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })

	ctx, cancel := context.WithCancel(context.Background())
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.onRun = func(req core.LocalCommandRequest) {
		if len(req.Args) == 0 {
			return
		}
		// Stop after the lockdown; only the call order matters.
		if strings.Contains(req.Args[len(req.Args)-1], "Crabbox-SSH-Quarantine") {
			cancel()
		}
	}
	b := testBackend(runner)
	_, _ = b.Acquire(ctx, core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "ready-order"})

	readyIdx, stageIdx := -1, -1
	for i, call := range runner.calls {
		if len(call.Args) == 0 {
			continue
		}
		script := call.Args[len(call.Args)-1]
		switch {
		case readyIdx < 0 && readinessProbe(call):
			readyIdx = i
		case stageIdx < 0 && strings.Contains(script, "Crabbox-SSH-Quarantine") && !strings.Contains(script, "Remove-NetFirewallRule"):
			stageIdx = i
		}
	}
	if readyIdx < 0 || stageIdx < 0 {
		t.Fatalf("missing calls: readiness=%d lockdown=%d", readyIdx, stageIdx)
	}
	if readyIdx >= stageIdx {
		t.Fatalf("readiness probe (%d) must precede pre-network lockdown (%d)", readyIdx, stageIdx)
	}
}
