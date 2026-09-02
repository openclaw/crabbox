package cli

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseParallelsVMsUsesFullListIP(t *testing.T) {
	vms, err := parseParallelsVMs(`[
		{"uuid":"bd","status":"running","ip_configured":"10.211.55.3","name":"Ubuntu"},
		{"ID":"id2","Name":"macOS","State":"running","Hardware":{"net0":{"enabled":true,"mac":"00:1C:42:33:EE:DD"},"net1":{"enabled":false,"mac":"001C42FFFFFF"}},"Network":{"ipAddresses":[{"type":"ipv6","ip":"fe80::1"},{"type":"ipv4","ip":"10.211.55.6"}]}}
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
	if vms[1].ID != "id2" || vms[1].IP != "10.211.55.6" || !reflect.DeepEqual(vms[1].MACs, []string{"001c4233eedd"}) {
		t.Fatalf("network IP not normalized: %#v", vms[1])
	}
}

func TestNormalizeParallelsMAC(t *testing.T) {
	tests := []struct {
		value string
		want  string
		ok    bool
	}{
		{value: "001C4233EEDD", want: "001c4233eedd", ok: true},
		{value: "00:1c:42:33:ee:dd", want: "001c4233eedd", ok: true},
		{value: "00-1C-42-33-EE-DD", want: "001c4233eedd", ok: true},
		{value: "001c.4233.eedd", want: "001c4233eedd", ok: true},
		{value: "001c4233eed", ok: false},
		{value: "001c4233eedz", ok: false},
	}
	for _, test := range tests {
		got, ok := normalizeParallelsMAC(test.value)
		if got != test.want || ok != test.ok {
			t.Errorf("normalizeParallelsMAC(%q)=(%q,%t), want (%q,%t)", test.value, got, ok, test.want, test.ok)
		}
	}
}

func TestResolveParallelsDHCPLeaseIP(t *testing.T) {
	now := time.Unix(1_788_360_000, 0)
	base := `[vnic0]
10.211.55.3="1788360901,1800,001c4233eedd,01001c4233eedd"
10.211.55.4="1784407577,1800,001c4207f695,01001c4207f695"
`
	tests := []struct {
		name    string
		data    string
		macs    []string
		want    string
		wantErr string
	}{
		{name: "fresh exact match", data: base, macs: []string{"00:1C:42:33:EE:DD"}, want: "10.211.55.3"},
		{name: "stale rejected", data: base, macs: []string{"001C4207F695"}, wantErr: "no fresh"},
		{name: "missing rejected", data: base, macs: []string{"001C42000000"}, wantErr: "no Parallels DHCP lease"},
		{name: "no VM MAC rejected", data: base, wantErr: "no usable NIC MAC"},
		{name: "malformed rejected", data: "10.211.55.3=bad\n", macs: []string{"001C4233EEDD"}, wantErr: "invalid quoted record"},
		{name: "duplicate same IP accepted", data: base + `10.211.55.3="1788360902,1800,001c4233eedd,01001c4233eedd"` + "\n", macs: []string{"001C4233EEDD"}, want: "10.211.55.3"},
		{name: "ambiguous rejected", data: base + `10.211.55.9="1788360902,1800,001c4233eedd,01001c4233eedd"` + "\n", macs: []string{"001C4233EEDD"}, wantErr: "ambiguous"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := resolveParallelsDHCPLeaseIP(test.data, test.macs, now)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("got=%q err=%v, want error containing %q", got, err, test.wantErr)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("got=%q err=%v, want %q", got, err, test.want)
			}
		})
	}
}

func TestParallelsWaitForIPPrefersToolsDiscovery(t *testing.T) {
	runner := &parallelsDHCPRunner{vmJSON: `[{
		"ID":"vm1","Name":"macOS","State":"running","ip_configured":"10.211.55.8",
		"Hardware":{"net0":{"enabled":true,"mac":"001C4233EEDD"}}
	}]`}
	cfg := Config{TargetOS: targetMacOS, SSHPort: "22", Parallels: ParallelsConfig{BootstrapKey: "/Users/runner/.ssh/bootstrap"}}
	vm, err := NewParallelsClient(cfg, runner).WaitForIP(context.Background(), "vm1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if vm.IP != "10.211.55.8" || vm.IPSource != "tools" {
		t.Fatalf("vm=%#v", vm)
	}
	if len(runner.requests) != 1 {
		t.Fatalf("Tools discovery should skip DHCP reads and probes: %#v", runner.requests)
	}
}

func TestParallelsWaitForIPUsesDHCPFallbackAndVerifiesSSH(t *testing.T) {
	expiry := time.Now().Add(time.Hour).Unix()
	runner := &parallelsDHCPRunner{vmJSON: `[{
		"ID":"vm1","Name":"macOS","State":"running",
		"Hardware":{"net0":{"enabled":true,"mac":"001C4233EEDD"}},
		"Network":{"ipAddresses":[]}
	}]`, leases: "[vnic0]\n10.211.55.9=\"" + strconv.FormatInt(expiry, 10) + ",1800,001c4233eedd,01001c4233eedd\"\n"}
	cfg := Config{TargetOS: targetMacOS, SSHPort: "22", Parallels: ParallelsConfig{BootstrapKey: "/Users/runner/.ssh/bootstrap"}}
	vm, err := NewParallelsClient(cfg, runner).WaitForIP(context.Background(), "vm1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if vm.IP != "10.211.55.9" || vm.IPSource != "dhcp-mac" {
		t.Fatalf("vm=%#v", vm)
	}
	if len(runner.requests) != 3 || runner.requests[1].Name != "/bin/cat" || runner.requests[2].Name != "/usr/bin/nc" {
		t.Fatalf("requests=%#v", runner.requests)
	}
	if got := runner.requests[2].Args; !reflect.DeepEqual(got, []string{"-z", "-w", "2", "10.211.55.9", "22"}) {
		t.Fatalf("nc args=%#v", got)
	}
}

func TestParallelsDHCPFallbackRunsOnRemoteHost(t *testing.T) {
	expiry := time.Now().Add(time.Hour).Unix()
	runner := &parallelsDHCPRunner{vmJSON: `[{"ID":"vm1","Name":"macOS","State":"running","Hardware":{"net0":{"enabled":true,"mac":"001C4233EEDD"}}}]`, leases: "[vnic0]\n10.211.55.9=\"" + strconv.FormatInt(expiry, 10) + ",1800,001c4233eedd,01001c4233eedd\"\n"}
	cfg := Config{TargetOS: targetMacOS, SSHPort: "22", Parallels: ParallelsConfig{Host: "mac.example", HostUser: "build", BootstrapKey: "/Users/build/.ssh/bootstrap"}}
	vm, err := NewParallelsClient(cfg, runner).WaitForIP(context.Background(), "vm1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if vm.IPSource != "dhcp-mac" || len(runner.requests) != 3 {
		t.Fatalf("vm=%#v requests=%#v", vm, runner.requests)
	}
	for _, req := range runner.requests {
		if req.Name != directSSHExecutable() || !containsString(req.Args, "build@mac.example") {
			t.Fatalf("fallback command did not stay on Parallels host: %#v", req)
		}
	}
	if !strings.Contains(runner.requests[1].Args[len(runner.requests[1].Args)-1], parallelsDHCPLeasesPath) ||
		!strings.Contains(runner.requests[2].Args[len(runner.requests[2].Args)-1], "/usr/bin/nc") {
		t.Fatalf("remote fallback requests=%#v", runner.requests)
	}
}

func TestParallelsWaitForIPDoesNotFallbackWithoutBootstrapIdentity(t *testing.T) {
	runner := &parallelsDHCPRunner{vmJSON: `[{"ID":"vm1","Name":"macOS","State":"running","Hardware":{"net0":{"enabled":true,"mac":"001C4233EEDD"}}}]`}
	_, err := NewParallelsClient(Config{TargetOS: targetMacOS}, runner).WaitForIP(context.Background(), "vm1", time.Nanosecond)
	if err == nil || len(runner.requests) != 1 {
		t.Fatalf("err=%v requests=%#v", err, runner.requests)
	}
}

func TestParallelsWaitForGuestExecShortCircuitsOnlyForConfiguredMacOSFallback(t *testing.T) {
	for _, test := range []struct {
		name      string
		target    string
		bootstrap string
		wantTools bool
	}{
		{name: "macOS fallback", target: targetMacOS, bootstrap: "/Users/build/.ssh/bootstrap", wantTools: true},
		{name: "macOS without fallback", target: targetMacOS},
		{name: "Linux", target: targetLinux, bootstrap: "/Users/build/.ssh/bootstrap"},
		{name: "Windows", target: targetWindows, bootstrap: "/Users/build/.ssh/bootstrap"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			runner := &parallelsGuestExecUnavailableRunner{cancel: cancel}
			cfg := Config{TargetOS: test.target, Parallels: ParallelsConfig{BootstrapKey: test.bootstrap}}
			err := NewParallelsClient(cfg, runner).WaitForGuestExec(ctx, "vm1", cfg, time.Minute)
			if test.wantTools {
				if err == nil || !ParallelsGuestToolsUnavailable(err) {
					t.Fatalf("err=%v, want Tools unavailable", err)
				}
				return
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err=%v, want context cancellation after normal retry path", err)
			}
		})
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
	if runner.lastReq.Name != directSSHExecutable() {
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

func TestParallelsCloneDestination(t *testing.T) {
	const vmName = "crabbox-cbx-abcdef123456-fork"
	for _, mode := range []struct {
		name       string
		cloneMode  string
		snapshotID string
		flags      []string
	}{
		{name: "default", snapshotID: "{snap1}", flags: []string{"--linked", "-i", "{snap1}"}},
		{name: "linked", cloneMode: "linked", snapshotID: "{snap1}", flags: []string{"--linked", "-i", "{snap1}"}},
		{name: "full", cloneMode: "full"},
		{name: "unlink", cloneMode: "unlink", flags: []string{"--unlink"}},
	} {
		for _, root := range []struct {
			name  string
			value string
			want  string
		}{
			{name: "configured", value: "/Volumes/VMs", want: "/Volumes/VMs"},
			{name: "spaces", value: "/Volumes/VM Storage", want: "/Volumes/VM Storage"},
			{name: "trimmed", value: " \t/Volumes/VM Storage\n", want: "/Volumes/VM Storage"},
			{name: "trailing-slash", value: "/Volumes/VMs/", want: "/Volumes/VMs/"},
			{name: "unset"},
			{name: "blank", value: " \t\n"},
		} {
			t.Run(mode.name+"/"+root.name, func(t *testing.T) {
				t.Setenv("XDG_STATE_HOME", t.TempDir())
				runner := &parallelsCloneRunner{t: t}
				if mode.snapshotID != "" {
					runner.steps = append(runner.steps, parallelsCloneStep{
						request: LocalCommandRequest{Name: "prlctl", Args: []string{"snapshot-list", "source-vm", "-j"}},
						stdout:  `{"{snap1}":{"name":"fresh","state":"poweroff"}}`,
					})
				}
				wantArgs := []string{"clone", "source-vm", "--name", vmName}
				if root.want != "" {
					wantArgs = append(wantArgs, "--dst", root.want)
				}
				wantArgs = append(wantArgs, mode.flags...)
				runner.steps = append(runner.steps,
					parallelsCloneStep{request: LocalCommandRequest{Name: "prlctl", Args: wantArgs}},
					parallelsCloneStep{
						request: LocalCommandRequest{Name: "prlctl", Args: []string{"list", "-i", "-f", "-j", vmName}},
						stdout:  `[{"uuid":"clone-id","name":"` + vmName + `","status":"stopped"}]`,
					},
				)
				client := NewParallelsClient(Config{Parallels: ParallelsConfig{
					VMRoot: root.value, CloneMode: mode.cloneMode,
				}}, runner)
				server, err := client.Clone(context.Background(), "source-vm", mode.snapshotID, "cbx_abcdef123456", "fork", true)
				if err != nil {
					t.Fatal(err)
				}
				if len(runner.requests) != len(runner.steps) {
					t.Fatalf("requests=%#v, want %d commands", runner.requests, len(runner.steps))
				}
				if server.CloudID != "clone-id" || server.Name != vmName {
					t.Fatalf("server=%#v", server)
				}
				for key, want := range map[string]string{
					"provider": "parallels", "lease": "cbx_abcdef123456", "slug": "fork",
					"source": "source-vm", "source_snapshot": mode.snapshotID, "host": "local", "keep": "true",
				} {
					if server.Labels[key] != want {
						t.Errorf("label %s=%q, want %q", key, server.Labels[key], want)
					}
				}
			})
		}
	}
}

func TestParallelsCloneRemoteDestination(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const prefix = "PATH=/usr/local/bin:/opt/homebrew/bin:$PATH "
	runner := &parallelsCloneRunner{t: t, steps: []parallelsCloneStep{
		{request: LocalCommandRequest{Name: directSSHExecutable(), Args: []string{
			"build@mac.example",
			prefix + "'prlctl' 'clone' 'source-vm' '--name' 'crabbox-cbx-abcdef123456-fork' '--dst' '/Volumes/VM Storage'",
		}}},
		{
			request: LocalCommandRequest{Name: directSSHExecutable(), Args: []string{
				"build@mac.example", prefix + "'prlctl' 'list' '-i' '-f' '-j' 'crabbox-cbx-abcdef123456-fork'",
			}},
			stdout: `[{"uuid":"clone-id","name":"crabbox-cbx-abcdef123456-fork","status":"stopped"}]`,
		},
	}}
	client := NewParallelsClient(Config{Parallels: ParallelsConfig{
		Host: "mac.example", HostUser: "build", VMRoot: " /Volumes/VM Storage ", CloneMode: "full",
	}}, runner)
	server, err := client.Clone(context.Background(), "source-vm", "", "cbx_abcdef123456", "fork", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.requests) != len(runner.steps) || server.CloudID != "clone-id" || server.Labels["host"] != "mac.example" {
		t.Fatalf("server=%#v requests=%#v", server, runner.requests)
	}
}

func TestParallelsCloneRejectsSnapshotIDForFullAndUnlink(t *testing.T) {
	for _, cloneMode := range []string{"full", "unlink"} {
		t.Run(cloneMode, func(t *testing.T) {
			runner := &parallelsFakeRunner{}
			client := NewParallelsClient(Config{
				Parallels: ParallelsConfig{CloneMode: cloneMode, VMRoot: "/Volumes/VM Storage"},
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
	for _, cloneMode := range []string{"", "linked"} {
		for _, snapshotID := range []string{"", " \t\n"} {
			runner := &parallelsFakeRunner{}
			client := NewParallelsClient(Config{
				Parallels: ParallelsConfig{CloneMode: cloneMode, VMRoot: "/Volumes/VM Storage"},
			}, runner)
			_, err := client.Clone(context.Background(), "source-vm", snapshotID, "cbx_abcdef123456", "fork", true)
			if err == nil || !strings.Contains(err.Error(), "require --parallels-source-snapshot") {
				t.Fatalf("mode=%q snapshot=%q err=%v", cloneMode, snapshotID, err)
			}
			if len(runner.requests) != 0 {
				t.Fatalf("clone should fail before prlctl: %#v", runner.requests)
			}
		}
	}
}

func TestParallelsCloneRejectsPowerOnSnapshot(t *testing.T) {
	runner := &parallelsCloneRunner{t: t, steps: []parallelsCloneStep{{
		request: LocalCommandRequest{Name: "prlctl", Args: []string{"snapshot-list", "source-vm", "-j"}},
		stdout:  `{"{snap1}":{"name":"live","state":"poweron"}}`,
	}}}
	client := NewParallelsClient(Config{Parallels: ParallelsConfig{
		CloneMode: "linked", VMRoot: "/Volumes/VM Storage",
	}}, runner)
	_, err := client.Clone(context.Background(), "source-vm", "{snap1}", "cbx_abcdef123456", "fork", true)
	if err == nil || !strings.Contains(err.Error(), "power-off snapshot") {
		t.Fatalf("err=%v", err)
	}
	if len(runner.requests) != 1 {
		t.Fatalf("only snapshot lookup should run: %#v", runner.requests)
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

func TestParallelsEnsureGuestReadyEnablesMacOSScreenSharing(t *testing.T) {
	runner := &parallelsFakeRunner{}
	client := NewParallelsClient(Config{}, runner)
	err := client.EnsureGuestReady(context.Background(), "vm1", Config{SSHUser: "runner", WorkRoot: "/Users/runner/crabbox", TargetOS: targetMacOS, Desktop: true})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(runner.lastReq.Args, "\n")
	for _, want := range []string{
		"desktop=true",
		"mkdir -p /var/db/crabbox",
		"/var/db/crabbox/vnc.password",
		"openssl rand -hex 4",
		"NOPASSWD: /bin/cat /var/db/crabbox/vnc.password",
		"-setvnclegacy -vnclegacy yes",
		"-setvncpw -vncpw \"$vnc_password\"",
		"-access -on -users \"$user\" -privs -all",
		"VNCAlwaysStartOnConsole -bool true",
		"com.apple.screensharing",
		"nc -z 127.0.0.1 5900",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("macOS desktop guest prep missing %q:\n%s", want, got)
		}
	}
	for _, banned := range []string{"dscl . -passwd", "-passwd /Users", "-access -off", "-privs -none"} {
		if strings.Contains(got, banned) {
			t.Fatalf("macOS desktop guest prep changes the account password with %q:\n%s", banned, got)
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

func TestParallelsBootstrapMacOSOverSSHUsesHostIdentityAndStreamsLeaseKey(t *testing.T) {
	runner := &parallelsDHCPRunner{}
	cfg := Config{
		TargetOS: targetMacOS,
		SSHUser:  "parallels-01",
		SSHPort:  "22",
		WorkRoot: "/Users/parallels-01/crabbox",
		Parallels: ParallelsConfig{
			BootstrapKey: "/Users/aiworker/.ssh/id_ed25519",
		},
	}
	publicKey := "ssh-ed25519 AAAAlease fixture"
	if err := NewParallelsClient(cfg, runner).BootstrapMacOSOverSSH(context.Background(), "10.211.55.9", cfg, publicKey); err != nil {
		t.Fatal(err)
	}
	if len(runner.requests) != 1 {
		t.Fatalf("requests=%#v", runner.requests)
	}
	req := runner.requests[0]
	if req.Name != "/usr/bin/ssh" {
		t.Fatalf("name=%q", req.Name)
	}
	rendered := strings.Join(req.Args, " ")
	for _, want := range []string{"-i /Users/aiworker/.ssh/id_ed25519", "IdentitiesOnly=yes", "BatchMode=yes", "PasswordAuthentication=no", "parallels-01@10.211.55.9", "sudo -n /bin/sh -s"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("bootstrap args missing %q: %s", want, rendered)
		}
	}
	if strings.Contains(rendered, publicKey) {
		t.Fatalf("public key leaked onto argv: %s", rendered)
	}
	for _, want := range []string{publicKey, "authorized_keys", "cat >/usr/local/bin/crabbox-ready", "test -w '/Users/parallels-01/crabbox'"} {
		if !strings.Contains(runner.stdin, want) {
			t.Fatalf("bootstrap stdin missing %q", want)
		}
	}
}

func TestParallelsBootstrapMacOSOverSSHExecutesNestedSSHOnRemoteHost(t *testing.T) {
	runner := &parallelsDHCPRunner{}
	cfg := Config{
		TargetOS: targetMacOS,
		SSHUser:  "parallels-01",
		SSHPort:  "22",
		WorkRoot: "/Users/parallels-01/crabbox",
		Parallels: ParallelsConfig{
			Host:         "mac.example",
			HostUser:     "build",
			BootstrapKey: "/Users/build/.ssh/bootstrap",
		},
	}
	if err := NewParallelsClient(cfg, runner).BootstrapMacOSOverSSH(context.Background(), "10.211.55.9", cfg, "ssh-ed25519 AAAAlease"); err != nil {
		t.Fatal(err)
	}
	req := runner.requests[0]
	if req.Name != directSSHExecutable() || !containsString(req.Args, "build@mac.example") {
		t.Fatalf("request=%#v", req)
	}
	remote := req.Args[len(req.Args)-1]
	for _, want := range []string{"'/usr/bin/ssh'", "'/Users/build/.ssh/bootstrap'", "'parallels-01@10.211.55.9'", "'sudo' '-n' '/bin/sh' '-s'"} {
		if !strings.Contains(remote, want) {
			t.Fatalf("remote bootstrap command missing %q: %s", want, remote)
		}
	}
}

type parallelsDHCPRunner struct {
	vmJSON   string
	leases   string
	requests []LocalCommandRequest
	stdin    string
}

type parallelsGuestExecUnavailableRunner struct {
	cancel context.CancelFunc
}

func (r *parallelsGuestExecUnavailableRunner) Run(_ context.Context, _ LocalCommandRequest) (LocalCommandResult, error) {
	r.cancel()
	return LocalCommandResult{Stderr: "PRL_ERR_VM_EXEC_GUEST_TOOL_NOT_AVAILABLE"}, errors.New("exit status 1")
}

func (r *parallelsDHCPRunner) Run(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	r.requests = append(r.requests, req)
	if req.Stdin != nil {
		data, _ := io.ReadAll(req.Stdin)
		r.stdin = string(data)
	}
	rendered := req.Name + " " + strings.Join(req.Args, " ")
	switch {
	case strings.Contains(rendered, "prlctl") && strings.Contains(rendered, "list"):
		return LocalCommandResult{Stdout: r.vmJSON}, nil
	case strings.Contains(rendered, parallelsDHCPLeasesPath):
		return LocalCommandResult{Stdout: r.leases}, nil
	default:
		return LocalCommandResult{}, nil
	}
}

type parallelsCloneStep struct {
	request LocalCommandRequest
	stdout  string
}

type parallelsCloneRunner struct {
	t        *testing.T
	steps    []parallelsCloneStep
	requests []LocalCommandRequest
}

func (r *parallelsCloneRunner) Run(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	r.t.Helper()
	index := len(r.requests)
	r.requests = append(r.requests, req)
	if index >= len(r.steps) {
		r.t.Fatalf("unexpected command: %#v", req)
	}
	step := r.steps[index]
	if req.Name != step.request.Name || !reflect.DeepEqual(req.Args, step.request.Args) {
		r.t.Errorf("command %d: got %s %q; want %s %q", index, req.Name, req.Args, step.request.Name, step.request.Args)
	}
	return LocalCommandResult{Stdout: step.stdout}, nil
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
