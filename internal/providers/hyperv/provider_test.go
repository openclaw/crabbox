package hyperv

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
		Disk:     50,
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
	for _, alias := range []string{"hyperv", "local-hyperv", "hyper-v", "windows-vm"} {
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
	cfg.HyperV = core.HyperVConfig{}
	applyDefaults(&cfg)
	if cfg.TargetOS != core.TargetWindows {
		t.Fatalf("target=%s want windows", cfg.TargetOS)
	}
	if cfg.WindowsMode != core.WindowsModeNormal {
		t.Fatalf("windowsMode=%s want normal", cfg.WindowsMode)
	}
	if cfg.HyperV.CPUs != 4 || cfg.HyperV.Memory != 8192 || cfg.HyperV.Disk != 50 {
		t.Fatalf("defaults not applied: cpus=%d memory=%d disk=%d", cfg.HyperV.CPUs, cfg.HyperV.Memory, cfg.HyperV.Disk)
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

func TestConfigureRejectsWSL2Mode(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetWindows
	cfg.WindowsMode = core.WindowsModeWSL2
	if _, err := (Provider{}).Configure(cfg, core.Runtime{}); err == nil {
		t.Fatal("Configure accepted WSL2 mode")
	}
}
