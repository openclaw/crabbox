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
	vm, err := NewParallelsClient(cfg, runner).WaitForIP(context.Background(), "vm1", time.Second, ParallelsIPWaitExisting)
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
	vm, err := NewParallelsClient(cfg, runner).WaitForIP(context.Background(), "vm1", time.Second, ParallelsIPWaitExisting)
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
	vm, err := NewParallelsClient(cfg, runner).WaitForIP(context.Background(), "vm1", time.Second, ParallelsIPWaitExisting)
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
	_, err := NewParallelsClient(Config{TargetOS: targetMacOS}, runner).WaitForIP(context.Background(), "vm1", time.Nanosecond, ParallelsIPWaitExisting)
	if err == nil || len(runner.requests) != 1 {
		t.Fatalf("err=%v requests=%#v", err, runner.requests)
	}
}

func TestParallelsWaitForIPTimeoutExplainsDiscovery(t *testing.T) {
	running := `[{"ID":"vm1","Name":"macOS","State":"running","Hardware":{"net0":{"enabled":true,"mac":"001C425AA8E6"}}}]`
	stopped := `[{"ID":"vm1","Name":"macOS","State":"stopped","Hardware":{"net0":{"enabled":true,"mac":"001C425AA8E6"}}}]`
	otherLease := "[vnic0]\n10.211.55.79=\"" + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) + ",1800,001c426ad157,01001c426ad157\"\n"
	staleLease := "[vnic0]\n10.211.55.79=\"" + strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10) + ",1800,001c425aa8e6,01001c425aa8e6\"\n"
	bootstrap := "/Users/build/.ssh/bootstrap"
	for _, test := range []struct {
		name      string
		vmJSON    string
		leases    string
		target    string
		cloneMode string
		bootstrap string
		existing  bool
		want      []string
		reject    []string
	}{
		{
			name: "linked macOS without fallback", vmJSON: running, target: targetMacOS,
			want:   []string{"clone_mode=linked", "macs=001c425aa8e6", "tools_ip=none", "set parallels.bootstrapKey", "retry with parallels.cloneMode=full", "clear parallels.sourceSnapshot and parallels.sourceSnapshotId", "source VM's current state", "cannot select a source snapshot"},
			reject: []string{"no matching DHCP lease was found"},
		},
		{
			name: "linked macOS fallback with no lease for the clone", vmJSON: running, leases: otherLease, target: targetMacOS, bootstrap: bootstrap,
			want:   []string{"DHCP fallback: no Parallels DHCP lease", "no matching DHCP lease was found in the Parallels host lease file", "check guest boot and network configuration", "prlctl capture <new-vm-id> --file <png>", "acquisition cleans up failed clones", "on the Parallels host while IP discovery is still waiting", "retry with parallels.cloneMode=full"},
			reject: []string{"set parallels.bootstrapKey", "prlctl capture vm1", "may not have booted"},
		},
		{
			name: "full clone omits clone mode advice", vmJSON: running, target: targetMacOS, cloneMode: "full", bootstrap: bootstrap, leases: otherLease,
			want:   []string{"clone_mode=full", "no matching DHCP lease was found"},
			reject: []string{"retry with parallels.cloneMode=full"},
		},
		{
			name: "stopped VM gets no boot advice", vmJSON: stopped, target: targetMacOS, bootstrap: bootstrap, leases: otherLease,
			want:   []string{"last_state=stopped", "clone_mode=linked"},
			reject: []string{"no matching DHCP lease was found", "retry with parallels.cloneMode=full"},
		},
		{
			name: "linux guest gets no macOS fallback advice", vmJSON: running, target: targetLinux,
			want:   []string{"clone_mode=linked", "retry with parallels.cloneMode=full"},
			reject: []string{"bootstrapKey"},
		},
		{
			name: "existing VM does not inherit configured clone mode", vmJSON: running, target: targetMacOS, cloneMode: "linked", bootstrap: bootstrap, leases: otherLease, existing: true,
			want:   []string{"clone_mode=unknown", "no matching DHCP lease was found", "inspect the existing VM", "prlctl capture <existing-vm-id> --file <png>"},
			reject: []string{"clone_mode=linked", "retry with parallels.cloneMode=full", "cleans up failed clones", "capture <new-vm-id>"},
		},
		{
			name: "expired lease is not missing", vmJSON: running, target: targetMacOS, bootstrap: bootstrap, leases: staleLease,
			want:   []string{"DHCP fallback: no fresh Parallels DHCP lease"},
			reject: []string{"no matching DHCP lease was found", "check guest boot"},
		},
		{
			name: "malformed lease file is not missing", vmJSON: running, target: targetMacOS, bootstrap: bootstrap, leases: "10.211.55.79=bad\n",
			want:   []string{"DHCP fallback: parse Parallels DHCP leases"},
			reject: []string{"no matching DHCP lease was found", "check guest boot"},
		},
		{
			name: "unknown MAC is not a missing lease", vmJSON: `[{"ID":"vm1","State":"running"}]`, target: targetMacOS, bootstrap: bootstrap,
			want:   []string{"macs=-", "tools_ip=none", "no usable NIC MAC"},
			reject: []string{"no matching DHCP lease was found", "check guest boot"},
		},
		{
			name: "failed VM query leaves Tools IP unknown", vmJSON: "invalid JSON", target: targetMacOS, bootstrap: bootstrap,
			want:   []string{"last_state=-", "clone_mode=linked", "macs=-", "tools_ip=unknown"},
			reject: []string{"tools_ip=none", "DHCP fallback:", "retry with parallels.cloneMode=full"},
		},
		{
			name: "missing existing VM leaves Tools IP unknown", vmJSON: "[]", target: targetMacOS, bootstrap: bootstrap, existing: true,
			want:   []string{"last_state=-", "clone_mode=unknown", "tools_ip=unknown"},
			reject: []string{"tools_ip=none", "DHCP fallback:", "retry with parallels.cloneMode=full", "cleans up failed clones"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &parallelsDHCPRunner{vmJSON: test.vmJSON, leases: test.leases}
			cfg := Config{TargetOS: test.target, SSHPort: "22", Parallels: ParallelsConfig{CloneMode: test.cloneMode, BootstrapKey: test.bootstrap}}
			purpose := ParallelsIPWaitAcquisition
			if test.existing {
				purpose = ParallelsIPWaitExisting
			}
			_, err := NewParallelsClient(cfg, runner).WaitForIP(context.Background(), "vm1", time.Nanosecond, purpose)
			if err == nil || ExitCodeForError(err, 1) != 5 {
				t.Fatalf("expected timeout with exit code 5, got %v", err)
			}
			for _, want := range test.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error missing %q: %v", want, err)
				}
			}
			for _, reject := range test.reject {
				if strings.Contains(err.Error(), reject) {
					t.Errorf("error unexpectedly contains %q: %v", reject, err)
				}
			}
		})
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

func TestSelectParallelsFleetConfigAppliesDirectHostMaxVMs(t *testing.T) {
	const twoCrabboxVMs = `[
			{"ID":"vm1","Name":"crabbox-cbx-aaaaaaaaaaaa-one","State":"running"},
			{"ID":"vm2","Name":"crabbox-cbx-bbbbbbbbbbbb-two","State":"running"}
		]`
	for _, tc := range []struct {
		name      string
		maxVMs    int
		hosts     []ParallelsHostConfig
		wantHost  string
		wantAtCap bool
	}{
		{name: "direct host at limit", maxVMs: 1, wantAtCap: true},
		{name: "direct host below limit", maxVMs: 3, wantHost: "mac.example"},
		{name: "direct host unset stays unlimited", wantHost: "mac.example"},
		{
			name:     "fleet limit wins over top level",
			maxVMs:   1,
			hosts:    []ParallelsHostConfig{{Name: "fleet", Host: "fleet.example", MaxVMs: 5}},
			wantHost: "fleet.example",
		},
		{
			name:      "fleet limit applies over higher top level",
			maxVMs:    9,
			hosts:     []ParallelsHostConfig{{Name: "fleet", Host: "fleet.example", MaxVMs: 1}},
			wantAtCap: true,
		},
		{
			name:     "top level is not a fleet default",
			maxVMs:   1,
			hosts:    []ParallelsHostConfig{{Name: "fleet", Host: "fleet.example"}},
			wantHost: "fleet.example",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Provider = parallelsProvider
			cfg.Parallels.Host = "mac.example"
			cfg.Parallels.MaxVMs = tc.maxVMs
			cfg.Parallels.Hosts = tc.hosts
			runner := &parallelsFakeRunner{stdout: twoCrabboxVMs}
			selected, err := SelectParallelsFleetConfig(context.Background(), cfg, runner, "")
			if tc.wantAtCap {
				if err == nil || !strings.Contains(err.Error(), "is at maxVMs capacity") {
					t.Fatalf("err=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err=%v", err)
			}
			if selected.Parallels.Host != tc.wantHost {
				t.Fatalf("host=%q want %q", selected.Parallels.Host, tc.wantHost)
			}
		})
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

func TestParallelsEnsureGuestReadyVerifiesMacOSSSHListener(t *testing.T) {
	runner := &parallelsFakeRunner{}
	client := NewParallelsClient(Config{}, runner)
	err := client.EnsureGuestReady(context.Background(), "vm1", Config{
		SSHUser:  "runner",
		WorkRoot: "/Users/runner/crabbox",
		TargetOS: targetMacOS,
		SSHPort:  "2222",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(runner.lastReq.Args, "\n")
	// Best-effort launchctl calls do not establish listener availability.
	// Authenticated SSH readiness remains a separate, later check.
	for _, want := range []string{
		"nc -z 127.0.0.1",
		"ssh_ready=1",
		`test "$ssh_ready" -eq 1`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("macOS readiness helper missing %q:\n%s", want, got)
		}
	}
	// The configured port is not necessarily the one sshd listens on: crabbox
	// falls back to 22 on templates that serve there. Probing a single port
	// would fail guests that work today, so both candidates must be tried.
	for _, want := range []string{"2222", "22"} {
		if !strings.Contains(got, want) {
			t.Fatalf("macOS readiness helper missing candidate port %q:\n%s", want, got)
		}
	}
}

func TestParallelsEnsureGuestReadyRechecksMacOSSSHListenerWhenHelperExists(t *testing.T) {
	runner := &parallelsFakeRunner{}
	client := NewParallelsClient(Config{}, runner)
	err := client.EnsureGuestReady(context.Background(), "vm1", Config{
		SSHUser:  "runner",
		WorkRoot: "/Users/runner/crabbox",
		TargetOS: targetMacOS,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(runner.lastReq.Args, "\n")
	// A guest prepared by an older crabbox carries a crabbox-ready that predates
	// the listener probe. If the early exit trusts that helper alone, such a
	// guest skips remote-login setup entirely and the new check never runs.
	if !strings.Contains(got, "crabbox_ssh_listening") {
		t.Fatalf("early exit does not re-verify the SSH listener:\n%s", got)
	}
	idx := strings.Index(got, "if [ -x /usr/local/bin/crabbox-ready ]")
	if idx < 0 {
		t.Fatalf("ready-helper short circuit not found:\n%s", got)
	}
	line := got[idx:]
	if end := strings.Index(line, "\n"); end >= 0 {
		line = line[:end]
	}
	if !strings.Contains(line, "crabbox_ssh_listening") {
		t.Fatalf("ready-helper short circuit does not gate on the listener: %q", line)
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

func TestParallelsEnsureGuestReadyUsesMacOSAccountCredentialsWithoutReset(t *testing.T) {
	runner := &parallelsFakeRunner{}
	client := NewParallelsClient(Config{}, runner)
	err := client.EnsureGuestReady(context.Background(), "vm1", Config{
		SSHUser:  "runner",
		WorkRoot: "/Users/runner/crabbox",
		TargetOS: targetMacOS,
		Desktop:  true,
		Parallels: ParallelsConfig{
			Password: "account-password-never-sent-to-guest",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(runner.lastReq.Args, "\n")
	for _, want := range []string{
		"-access -on -users \"$user\" -privs -all",
		"VNCAlwaysStartOnConsole -bool true",
		"com.apple.screensharing",
		"nc -z 127.0.0.1 5900",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("macOS account desktop prep missing %q:\n%s", want, got)
		}
	}
	for _, banned := range []string{
		"account-password-never-sent-to-guest",
		"/var/db/crabbox/vnc.password",
		"setvncpw",
		"dscl",
		"-passwd",
	} {
		if strings.Contains(got, banned) {
			t.Fatalf("macOS account desktop prep references %q:\n%s", banned, got)
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

func TestParallelsHostAndBootstrapCommandsOmitPasswordFromChildEnvironment(t *testing.T) {
	const password = "synthetic-parallels-password"
	t.Setenv("CRABBOX_PARALLELS_PASSWORD", password)
	tests := []struct {
		name string
		cfg  Config
		run  func(*testing.T, *ParallelsClient)
	}{
		{
			name: "prlctl local",
			cfg:  Config{TargetOS: targetMacOS},
			run: func(t *testing.T, client *ParallelsClient) {
				t.Helper()
				if _, err := client.Version(context.Background()); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "prlctl remote",
			cfg:  Config{TargetOS: targetMacOS, Parallels: ParallelsConfig{Host: "mac.example", HostUser: "build"}},
			run: func(t *testing.T, client *ParallelsClient) {
				t.Helper()
				if _, err := client.Version(context.Background()); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "bootstrap local",
			cfg: Config{
				TargetOS: targetMacOS,
				SSHUser:  "parallels-01",
				SSHPort:  "22",
				WorkRoot: "/Users/parallels-01/crabbox",
				Parallels: ParallelsConfig{
					BootstrapKey: "/Users/aiworker/.ssh/id_ed25519",
				},
			},
			run: func(t *testing.T, client *ParallelsClient) {
				t.Helper()
				if err := client.BootstrapMacOSOverSSH(context.Background(), "10.211.55.9", client.Cfg, "ssh-ed25519 AAAAlease"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "bootstrap remote",
			cfg: Config{
				TargetOS: targetMacOS,
				SSHUser:  "parallels-01",
				SSHPort:  "22",
				WorkRoot: "/Users/parallels-01/crabbox",
				Parallels: ParallelsConfig{
					Host:         "mac.example",
					HostUser:     "build",
					BootstrapKey: "/Users/build/.ssh/bootstrap",
				},
			},
			run: func(t *testing.T, client *ParallelsClient) {
				t.Helper()
				if err := client.BootstrapMacOSOverSSH(context.Background(), "10.211.55.9", client.Cfg, "ssh-ed25519 AAAAlease"); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &parallelsDHCPRunner{}
			test.run(t, NewParallelsClient(test.cfg, runner))
			if len(runner.requests) == 0 {
				t.Fatal("expected host or bootstrap command")
			}
			for _, req := range runner.requests {
				assertParallelsChildOmitsPassword(t, req, password)
			}
		})
	}
}

func assertParallelsChildOmitsPassword(t *testing.T, req LocalCommandRequest, password string) {
	t.Helper()
	if req.Env == nil {
		t.Fatal("Parallels host command inherited the process environment")
	}
	if commandRequestHasEnvName(req, parallelsPasswordEnvName) {
		t.Fatal("CRABBOX_PARALLELS_PASSWORD was present in the child environment")
	}
	if commandRequestPlacesValueOnArgv(req, password) {
		t.Fatal("password value was placed on argv")
	}
}

func commandRequestHasEnvName(req LocalCommandRequest, name string) bool {
	prefix := strings.ToUpper(name) + "="
	for _, entry := range req.Env {
		if strings.HasPrefix(strings.ToUpper(entry), prefix) {
			return true
		}
	}
	return false
}

func commandRequestPlacesValueOnArgv(req LocalCommandRequest, value string) bool {
	if value == "" {
		return false
	}
	if strings.Contains(req.Name, value) {
		return true
	}
	for _, arg := range req.Args {
		if strings.Contains(arg, value) {
			return true
		}
	}
	return false
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

func TestParallelsEnsureReadyInstallsMacOSNodeBaseline(t *testing.T) {
	script := parallelsPOSIXEnsureReadyScript("parallels-01", "/Users/parallels-01/crabbox", false, false, sshPortCandidates("22", nil))

	installer := sharedMacOSNodeInstall()
	if !strings.Contains(script, installer) {
		t.Fatal("ensure-ready script does not embed the shared macOS Node installer verbatim")
	}

	// The installer has to run before the readiness gate, otherwise a guest whose
	// crabbox-ready predates the Node checks exits early and never installs Node.
	gate := "if [ -x /usr/local/bin/crabbox-ready ]"
	if got, want := strings.Index(script, installer), strings.Index(script, gate); got == -1 || want == -1 || got > want {
		t.Fatalf("Node install must precede the readiness gate: install=%d gate=%d", got, want)
	}

	// Only macOS guests get the baseline; the Linux branch must be untouched.
	// Matched without surrounding indentation so reindenting the generated
	// script does not fail this for a no-op formatting change.
	sw := strings.Index(script, "command -v sw_vers >/dev/null 2>&1")
	if sw == -1 || sw > strings.Index(script, "crabbox_node_bin=") {
		t.Fatal("Node handling is not guarded by the macOS sw_vers check")
	}
}

// The script is delivered to `sudo -n /bin/sh -s` on stdin, so any child that
// reads stdin silently swallows the remainder of the script -- and the shell
// still exits 0, so the damage is invisible.
func TestParallelsEnsureReadyKeepsStdinOffGuestChildren(t *testing.T) {
	script := parallelsPOSIXEnsureReadyScript("parallels-01", "/Users/parallels-01/crabbox", false, false, sshPortCandidates("22", nil))
	for _, want := range []string{
		`-c 'bash -lc "command -v node"' </dev/null`,
		`-c 'bash -lc "command -v npm"' </dev/null`,
		`-c 'bash -lc "command -v npx"' </dev/null`,
		`/bin/bash "$crabbox_node_installer" </dev/null`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("child may consume the script from stdin: missing %q", want)
		}
	}
}

// A template can already satisfy sshReadyCommand with Node supplied through the
// SSH user's login shell (Homebrew, nvm, asdf). Downloading over the top of that
// would make a previously working template depend on nodejs.org being reachable.
func TestParallelsEnsureReadyPreservesUserManagedNode(t *testing.T) {
	script := parallelsPOSIXEnsureReadyScript("parallels-01", "/Users/parallels-01/crabbox", false, false, sshPortCandidates("22", nil))

	for _, want := range []string{
		`crabbox_node_bin=$(su - "$user" -c 'bash -lc "command -v node"' </dev/null 2>/dev/null || true)`,
		`crabbox_npm_bin=$(su - "$user" -c 'bash -lc "command -v npm"' </dev/null 2>/dev/null || true)`,
		`ln -sfn "$crabbox_node_bin" /usr/local/bin/node`,
		`ln -sfn "$crabbox_npm_bin" /usr/local/bin/npm`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("user-managed runtime is not preserved: missing %q", want)
		}
	}

	// The lookup must drop to the guest user; sourcing their login files as root
	// would run user-controlled shell setup with full privileges.
	if !strings.Contains(script, `su - "$user" -c 'bash -lc`) {
		t.Fatal("login environment must be probed as the guest user, not as root")
	}

	// It must use bash -lc like sshReadyCommand does. The user's default login
	// shell (zsh on macOS) reads different rc files, so `su - user -c 'command
	// -v node'` can miss a runtime the probe resolves, and vice versa.
	if strings.Contains(script, `su - "$user" -c 'command -v`) {
		t.Fatal("detection must mirror the probe's bash -lc, not the default login shell")
	}

	// Preservation has to be reached before the installer, or the download still
	// happens and the healthy runtime is pointless.
	preserve := strings.Index(script, "crabbox_node_bin=$(su")
	install := strings.Index(script, "crabbox_node_installer=")
	if preserve == -1 || install == -1 || preserve > install {
		t.Fatalf("preservation must precede the installer: preserve=%d install=%d", preserve, install)
	}

	// Self-linking would break an existing standard install rather than heal it.
	if !strings.Contains(script, `[ "$crabbox_node_bin" != /usr/local/bin/node ]`) {
		t.Fatal("preservation must not link /usr/local/bin/node onto itself")
	}
}

// The installer stays the fallback for a guest with no runtime at all.
func TestParallelsEnsureReadyInstallsWhenNoRuntimeExists(t *testing.T) {
	script := parallelsPOSIXEnsureReadyScript("parallels-01", "/Users/parallels-01/crabbox", false, false, sshPortCandidates("22", nil))

	installer := sharedMacOSNodeInstall()
	idx := strings.Index(script, installer)
	if idx == -1 {
		t.Fatal("shared installer is no longer embedded")
	}
	// It must be gated on preservation having failed, not run unconditionally.
	prefix := script[:idx]
	gate := `if [ "$crabbox_node_preserved" != true ]; then`
	if !strings.Contains(prefix, gate) {
		t.Fatal("installer is not gated on the preservation result")
	}
	// And preservation is only claimed once the linked runtime actually works on
	// the PATH crabbox-ready uses, so a shim that needs its manager falls back.
	if !strings.Contains(prefix, "crabbox_node_preserved=true") {
		t.Fatal("preservation is never verified before the installer is skipped")
	}
	if strings.Index(script, "crabbox_node_preserved=true") > strings.Index(script, gate) {
		t.Fatal("preservation must be proven before the installer gate is evaluated")
	}
}

func TestParallelsMacOSReadyScriptMatchesReadinessContract(t *testing.T) {
	script := parallelsPOSIXEnsureReadyScript("parallels-01", "/Users/parallels-01/crabbox", false, false, sshPortCandidates("22", nil))

	macReady := `#!/bin/sh
set -eu
export PATH="/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
rsync --version >/dev/null
curl --version >/dev/null
node --version >/dev/null
npm --version >/dev/null
test -w '/Users/parallels-01/crabbox'
`
	if !strings.Contains(script, macReady) {
		t.Fatal("macOS crabbox-ready does not assert the Node readiness contract on an explicit PATH")
	}

	// sshReadyCommand requires node and npm for macOS targets; crabbox-ready must
	// agree, or the ensure-ready gate reports success while readiness still fails.
	ready := sshReadyCommand(SSHTarget{TargetOS: targetMacOS})
	for _, want := range []string{"node --version", "npm --version"} {
		if !strings.Contains(ready, want) {
			t.Fatalf("sshReadyCommand no longer requires %q; revisit crabbox-ready", want)
		}
	}
}

func TestParallelsLinuxReadyScriptUnchangedByNodeBaseline(t *testing.T) {
	script := parallelsPOSIXEnsureReadyScript("worker", "/work/crabbox", false, false, sshPortCandidates("22", nil))

	linuxReady := `#!/usr/bin/env bash
set -euo pipefail
git --version >/dev/null
rsync --version >/dev/null
curl --version >/dev/null
jq --version >/dev/null
test -w '/work/crabbox'
`
	if !strings.Contains(script, linuxReady) {
		t.Fatal("Linux crabbox-ready changed; the Node baseline is macOS-only")
	}
}
