package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestParseParallelsVMsUsesFullListIP(t *testing.T) {
	vms, err := parseParallelsVMs(`[
		{"uuid":"bd","status":"running","ip_configured":"10.211.55.3","name":"Ubuntu"},
		{"ID":"id2","Name":"macOS","State":"running","Network":{"ipAddresses":[{"type":"ipv6","ip":"fe80::1"},{"type":"ipv4","ip":"10.211.55.6"}]}}
	]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(vms) != 2 {
		t.Fatalf("len=%d", len(vms))
	}
	if vms[0].ID != "bd" || vms[0].Name != "Ubuntu" || vms[0].IP != "10.211.55.3" {
		t.Fatalf("first VM not normalized: %#v", vms[0])
	}
	if vms[1].ID != "id2" || vms[1].IP != "10.211.55.6" {
		t.Fatalf("network IP not normalized: %#v", vms[1])
	}
}

func TestParseParallelsSnapshots(t *testing.T) {
	snapshots, err := parseParallelsSnapshots(`{
		"{snap1}":{"name":"fresh","date":"2026-03-12 13:55:00","state":"poweron","current":false,"parent":""}
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 1 || snapshots[0].ID != "{snap1}" || snapshots[0].Name != "fresh" {
		t.Fatalf("snapshots=%#v", snapshots)
	}
}

func TestParallelsLeaseVMNameRoundTrip(t *testing.T) {
	name := parallelsLeaseVMName("cbx_abcdef123456", "My Fast VM")
	if name != "crabbox-cbx-abcdef123456-my-fast-vm" {
		t.Fatalf("name=%q", name)
	}
	leaseID, slug := parallelsLeaseFromVMName(name)
	if leaseID != "cbx_abcdef123456" || slug != "my-fast-vm" {
		t.Fatalf("lease=%q slug=%q", leaseID, slug)
	}
}

func TestResolveParallelsVMMatchesLeaseIDAndSlug(t *testing.T) {
	runner := parallelsResolveFakeRunner{stdout: `[
		{"uuid":"vm1","status":"running","ip_configured":"10.0.0.2","name":"crabbox-cbx-abcdef123456-blue-lobster"}
	]`}
	_, vm, err := ResolveParallelsVM(context.Background(), Config{}, runner, "blue-lobster")
	if err != nil {
		t.Fatal(err)
	}
	if vm.ID != "vm1" {
		t.Fatalf("vm=%#v", vm)
	}
	_, vm, err = ResolveParallelsVM(context.Background(), Config{}, runner, "cbx_abcdef123456")
	if err != nil {
		t.Fatal(err)
	}
	if vm.ID != "vm1" {
		t.Fatalf("vm=%#v", vm)
	}
}

func TestParallelsLabelsFromNamePreservesStoredLeaseMetadata(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_abcdef123456"
	if err := writeParallelsLeaseLabels(leaseID, map[string]string{
		"lease":      leaseID,
		"slug":       "stored-slug",
		"keep":       "false",
		"state":      "ready",
		"expires_at": "2026-05-21T18:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	labels := parallelsLabelsFromName("crabbox-cbx-abcdef123456-live-slug")
	if labels["keep"] != "false" || labels["state"] != "ready" || labels["expires_at"] == "" {
		t.Fatalf("labels did not preserve stored metadata: %#v", labels)
	}
	if labels["slug"] != "live-slug" {
		t.Fatalf("VM name slug should remain authoritative: %#v", labels)
	}
}

func TestParallelsDeleteRefusesNonCrabboxVM(t *testing.T) {
	runner := &parallelsFakeRunner{
		stdout: `[
			{"ID":"vm1","Name":"Ubuntu 25.10","State":"stopped","Network":{"ipAddresses":[{"type":"ipv4","ip":"10.0.0.2"}]}}
		]`,
	}
	client := NewParallelsClient(Config{}, runner)
	err := client.Delete(context.Background(), "Ubuntu 25.10")
	if err == nil || !strings.Contains(err.Error(), "refusing to delete non-Crabbox") {
		t.Fatalf("err=%v", err)
	}
	if runner.deleteCalled {
		t.Fatal("delete command should not run")
	}
}

func TestParallelsRemoteCommandUsesSSHHostThenCommand(t *testing.T) {
	runner := &parallelsFakeRunner{}
	client := NewParallelsClient(Config{Parallels: ParallelsConfig{Host: "mac.example", HostUser: "build"}}, runner)
	_, _ = client.Version(context.Background())
	if runner.lastReq.Name != "ssh" {
		t.Fatalf("name=%q", runner.lastReq.Name)
	}
	if len(runner.lastReq.Args) != 2 {
		t.Fatalf("args=%#v", runner.lastReq.Args)
	}
	if runner.lastReq.Args[0] != "build@mac.example" {
		t.Fatalf("host arg=%q", runner.lastReq.Args[0])
	}
	if strings.HasPrefix(runner.lastReq.Args[1], "-- ") {
		t.Fatalf("remote command should not start with --: %#v", runner.lastReq.Args)
	}
	if !strings.HasPrefix(runner.lastReq.Args[1], "PATH=/usr/local/bin:/opt/homebrew/bin:$PATH ") {
		t.Fatalf("remote command should add Mac binary dirs to PATH: %q", runner.lastReq.Args[1])
	}
	if !strings.Contains(runner.lastReq.Args[1], "prlctl") || !strings.Contains(runner.lastReq.Args[1], "--version") {
		t.Fatalf("remote command=%q", runner.lastReq.Args[1])
	}
}

func TestParallelsCloneRejectsSnapshotIDForFullAndUnlink(t *testing.T) {
	for _, cloneMode := range []string{"full", "unlink"} {
		t.Run(cloneMode, func(t *testing.T) {
			runner := &parallelsFakeRunner{
				stdout: `[{"uuid":"vm1","status":"stopped","ip_configured":"10.0.0.2","name":"crabbox-cbx-abcdef123456-fork"}]`,
			}
			client := NewParallelsClient(Config{
				Parallels: ParallelsConfig{CloneMode: cloneMode},
			}, runner)
			_, err := client.Clone(context.Background(), "source-vm", "{snap1}", "cbx_abcdef123456", "fork", true)
			if err == nil || !strings.Contains(err.Error(), "prlctl selects snapshots only for linked clones") {
				t.Fatalf("err=%v", err)
			}
			if len(runner.requests) != 0 {
				t.Fatalf("clone should fail before prlctl: %#v", runner.requests)
			}
		})
	}
}

func TestParallelsLinkedCloneRequiresExplicitSnapshot(t *testing.T) {
	runner := &parallelsFakeRunner{}
	client := NewParallelsClient(Config{
		Parallels: ParallelsConfig{CloneMode: "linked"},
	}, runner)
	_, err := client.Clone(context.Background(), "source-vm", "", "cbx_abcdef123456", "fork", true)
	if err == nil || !strings.Contains(err.Error(), "require --parallels-source-snapshot") {
		t.Fatalf("err=%v", err)
	}
	if len(runner.requests) != 0 {
		t.Fatalf("clone should fail before prlctl: %#v", runner.requests)
	}
}

func TestValidateParallelsSnapshotCloneModeRejectsPowerOnDryRun(t *testing.T) {
	snapshot := ParallelsSnapshot{Name: "macOS 26.3.1 LATEST", State: "poweron"}
	err := validateParallelsSnapshotCloneMode(snapshot, "linked")
	if err == nil || !strings.Contains(err.Error(), "power-off snapshot") {
		t.Fatalf("err=%v", err)
	}
}

func TestApplyParallelsTemplateConfig(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "parallels"
	cfg.Parallels.Templates = map[string]ParallelsTemplateConfig{
		"tahoe-latest": {
			Source:         "macOS Tahoe",
			SourceSnapshot: "macOS 26.3.1 LATEST",
			TargetOS:       targetMacOS,
			User:           "alice",
			Host:           "mac-host.example.net",
		},
	}
	if err := ApplyParallelsTemplateConfig(&cfg, "tahoe-latest"); err != nil {
		t.Fatal(err)
	}
	if cfg.Parallels.Source != "macOS Tahoe" || cfg.Parallels.SourceSnapshot != "macOS 26.3.1 LATEST" || cfg.TargetOS != targetMacOS || cfg.SSHUser != "alice" || cfg.Parallels.Host != "mac-host.example.net" {
		t.Fatalf("cfg=%#v", cfg)
	}
}

func TestApplyParallelsTemplateConfigDefaultsDoNotReapplyAppliedTemplate(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "parallels"
	cfg.TargetOS = targetLinux
	cfg.WindowsMode = windowsModeNormal
	cfg.Parallels.Templates = map[string]ParallelsTemplateConfig{
		"win": {
			Source:      "Windows 11",
			TargetOS:    targetWindows,
			WindowsMode: windowsModeWSL2,
		},
	}
	if err := ApplyParallelsTemplateConfig(&cfg, "win"); err != nil {
		t.Fatal(err)
	}
	cfg.TargetOS = targetLinux
	cfg.WindowsMode = windowsModeNormal
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.TargetOS != targetLinux || cfg.WindowsMode != windowsModeNormal {
		t.Fatalf("applied template should not override explicit target later: target=%s windowsMode=%s", cfg.TargetOS, cfg.WindowsMode)
	}

	cfg = baseConfig()
	cfg.Provider = "parallels"
	cfg.Parallels.Template = "win"
	cfg.Parallels.Templates = map[string]ParallelsTemplateConfig{
		"win": {
			Source:      "Windows 11",
			TargetOS:    targetWindows,
			WindowsMode: windowsModeWSL2,
		},
	}
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.TargetOS != targetWindows || cfg.WindowsMode != windowsModeWSL2 || cfg.Parallels.Source != "Windows 11" {
		t.Fatalf("unapplied configured template should apply: target=%s windowsMode=%s parallels=%#v", cfg.TargetOS, cfg.WindowsMode, cfg.Parallels)
	}
}

func TestApplyProviderConfigDefaultsReturnsMissingParallelsTemplate(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "parallels"
	cfg.Parallels.Template = "missing"
	if err := applyProviderConfigDefaults(&cfg); err == nil || !strings.Contains(err.Error(), `parallels template "missing" not found`) {
		t.Fatalf("err=%v", err)
	}
}

func TestParallelsCandidateConfigsFiltersTarget(t *testing.T) {
	cfg := baseConfig()
	cfg.TargetOS = targetMacOS
	cfg.Parallels.Hosts = []ParallelsHostConfig{
		{Name: "linux-host", Host: "linux.example", Targets: []string{targetLinux}},
		{Name: "mac-host", Host: "mac.example", User: "build", Targets: []string{targetMacOS}, MaxVMs: 2},
	}
	candidates := ParallelsCandidateConfigs(cfg)
	if len(candidates) != 1 {
		t.Fatalf("len=%d candidates=%#v", len(candidates), candidates)
	}
	got := candidates[0].Parallels
	if got.SelectedHost != "mac-host" || got.Host != "mac.example" || got.HostUser != "build" {
		t.Fatalf("host=%#v", got)
	}
}

func TestParallelsEnsureGuestReadyInstallsPOSIXReadyScript(t *testing.T) {
	runner := &parallelsFakeRunner{}
	client := NewParallelsClient(Config{}, runner)
	err := client.EnsureGuestReady(context.Background(), "vm1", Config{SSHUser: "runner", WorkRoot: "/work/test", TargetOS: targetLinux})
	if err != nil {
		t.Fatal(err)
	}
	if runner.lastReq.Name != "prlctl" {
		t.Fatalf("name=%q", runner.lastReq.Name)
	}
	got := strings.Join(runner.lastReq.Args, "\n")
	for _, want := range []string{"exec", "vm1", "desktop=false", "cat >/usr/local/bin/crabbox-ready", "apt-get install", "test -w '/work/test'"} {
		if !strings.Contains(got, want) {
			t.Fatalf("guest prep command missing %q:\n%s", want, got)
		}
	}
}

func TestParallelsEnsureGuestReadyUpgradesReadyGuestForDesktop(t *testing.T) {
	runner := &parallelsFakeRunner{}
	client := NewParallelsClient(Config{}, runner)
	err := client.EnsureGuestReady(context.Background(), "vm1", Config{SSHUser: "runner", WorkRoot: "/work/test", TargetOS: targetLinux, Desktop: true})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(runner.lastReq.Args, "\n")
	for _, want := range []string{
		"desktop=true",
		"command -v websockify",
		"[ -f /usr/share/novnc/vnc.html ]",
		"systemctl is-active --quiet crabbox-x11vnc.service",
		"novnc websockify",
		"/etc/systemd/system/crabbox-xvfb.service",
		"/etc/systemd/system/crabbox-desktop.service",
		"/etc/systemd/system/crabbox-x11vnc.service",
		"-rfbport 5900",
		"systemctl enable --now crabbox-xvfb.service crabbox-desktop.service crabbox-x11vnc.service",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("desktop guest prep missing %q:\n%s", want, got)
		}
	}
}

func TestParallelsEnsureGuestReadyEnablesMacOSRemoteLogin(t *testing.T) {
	runner := &parallelsFakeRunner{}
	client := NewParallelsClient(Config{}, runner)
	err := client.EnsureGuestReady(context.Background(), "vm1", Config{SSHUser: "runner", WorkRoot: "/Users/runner/crabbox", TargetOS: targetMacOS})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(runner.lastReq.Args, "\n")
	for _, want := range []string{"launchctl load -w /System/Library/LaunchDaemons/ssh.plist", "launchctl enable system/com.openssh.sshd", "launchctl kickstart -k system/com.openssh.sshd"} {
		if !strings.Contains(got, want) {
			t.Fatalf("macOS guest prep missing %q:\n%s", want, got)
		}
	}
}

func TestParallelsEnsureGuestReadySkipsWindows(t *testing.T) {
	runner := &parallelsFakeRunner{}
	client := NewParallelsClient(Config{}, runner)
	if err := client.EnsureGuestReady(context.Background(), "vm1", Config{SSHUser: "runner", TargetOS: targetWindows}); err != nil {
		t.Fatal(err)
	}
	if runner.lastReq.Name != "" {
		t.Fatalf("unexpected command: %#v", runner.lastReq)
	}
}

type parallelsFakeRunner struct {
	stdout       string
	deleteCalled bool
	lastReq      LocalCommandRequest
	requests     []LocalCommandRequest
}

func (r *parallelsFakeRunner) Run(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	r.lastReq = req
	r.requests = append(r.requests, req)
	if len(req.Args) > 0 && req.Args[0] == "delete" {
		r.deleteCalled = true
	}
	return LocalCommandResult{Stdout: r.stdout}, nil
}

type parallelsResolveFakeRunner struct {
	stdout string
}

func (r parallelsResolveFakeRunner) Run(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	if len(req.Args) > 1 && req.Args[0] == "list" && req.Args[1] == "-i" {
		return LocalCommandResult{Stderr: "not found"}, errors.New("not found")
	}
	return LocalCommandResult{Stdout: r.stdout}, nil
}
