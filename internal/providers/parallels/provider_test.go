package parallels

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func TestApplyFlagsNameOverridesClearIDOverrides(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = "parallels"
	cfg.Parallels.SourceID = "old-source-id"
	cfg.Parallels.SourceSnapshotID = "old-snapshot-id"

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	provider := Provider{}
	values := provider.RegisterFlags(fs, cfg)
	if err := fs.Parse([]string{
		"--parallels-source", "Ubuntu 25.10",
		"--parallels-source-snapshot", "fresh",
	}); err != nil {
		t.Fatal(err)
	}
	if err := provider.ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Parallels.Source != "Ubuntu 25.10" || cfg.Parallels.SourceID != "" {
		t.Fatalf("source override not applied cleanly: %#v", cfg.Parallels)
	}
	if cfg.Parallels.SourceSnapshot != "fresh" || cfg.Parallels.SourceSnapshotID != "" {
		t.Fatalf("snapshot override not applied cleanly: %#v", cfg.Parallels)
	}
}

func TestApplyFlagsKeepsExplicitTargetOverTemplate(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = "parallels"
	cfg.TargetOS = core.TargetLinux
	cfg.WindowsMode = core.WindowsModeNormal
	cfg.Parallels.Templates = map[string]core.ParallelsTemplateConfig{
		"win": {
			TargetOS:    core.TargetWindows,
			WindowsMode: core.WindowsModeWSL2,
			Source:      "Windows 11",
		},
	}

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String("target", "", "")
	fs.String("windows-mode", "", "")
	provider := Provider{}
	values := provider.RegisterFlags(fs, cfg)
	if err := fs.Parse([]string{
		"--target", "linux",
		"--windows-mode", "normal",
		"--parallels-template", "win",
	}); err != nil {
		t.Fatal(err)
	}
	if err := provider.ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.TargetOS != core.TargetLinux || cfg.WindowsMode != core.WindowsModeNormal {
		t.Fatalf("explicit target flags should win over template: target=%s windowsMode=%s", cfg.TargetOS, cfg.WindowsMode)
	}
	if cfg.Parallels.Source != "Windows 11" {
		t.Fatalf("template source should still apply: %#v", cfg.Parallels)
	}
}

func TestApplyFlagsRejectsInvalidStartupTimeout(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = "parallels"

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	provider := Provider{}
	values := provider.RegisterFlags(fs, cfg)
	if err := fs.Parse([]string{"--parallels-startup-timeout", "nope"}); err != nil {
		t.Fatal(err)
	}
	if err := provider.ApplyFlags(&cfg, fs, values); err == nil {
		t.Fatal("expected invalid startup timeout error")
	}
}

func TestApplyFlagsExplicitHostBypassesConfiguredFleet(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = "parallels"
	cfg.Parallels.SelectedHost = "stale-fleet-host"
	cfg.Parallels.Hosts = []core.ParallelsHostConfig{{Name: "fleet", Host: "fleet.example"}}

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	provider := Provider{}
	values := provider.RegisterFlags(fs, cfg)
	if err := fs.Parse([]string{"--parallels-host", "100.123.224.76"}); err != nil {
		t.Fatal(err)
	}
	if err := provider.ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Parallels.Host != "100.123.224.76" || cfg.Parallels.SelectedHost != "" || len(cfg.Parallels.Hosts) != 0 {
		t.Fatalf("explicit host did not bypass fleet: %#v", cfg.Parallels)
	}
}

func TestResolveReportsDiscoveredIP(t *testing.T) {
	seedParallelsCleanupState(t)
	runner := &parallelsCleanupRunner{
		vmJSON:     `[{"ID":"vm-good","Name":"crabbox-cbx-good-blue","State":"running"}]`,
		nextVMJSON: `[{"ID":"vm-good","Name":"crabbox-cbx-good-blue","State":"running","ip_configured":"192.0.2.10"}]`,
	}
	backend := &leaseBackend{DirectSSHBackend: sharedBackend(testParallelsCleanupConfig(), runner)}
	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "blue"})
	if err != nil {
		t.Fatal(err)
	}
	if lease.SSH.Host != "192.0.2.10" || lease.Server.PublicNet.IPv4.IP != "192.0.2.10" {
		t.Fatalf("discovered SSH host=%q server IP=%q, want 192.0.2.10", lease.SSH.Host, lease.Server.PublicNet.IPv4.IP)
	}
}

func TestResolvePreservesVMWhenIPDiscoveryFails(t *testing.T) {
	for _, mode := range []struct {
		name      string
		request   core.ResolveRequest
		wantError bool
	}{
		{name: "connection", request: core.ResolveRequest{ID: "blue"}, wantError: true},
		{name: "release", request: core.ResolveRequest{ID: "blue", ReleaseOnly: true}},
		{name: "status", request: core.ResolveRequest{ID: "blue", StatusOnly: true, NoLocalStateMutations: true}},
		{name: "status readiness", request: core.ResolveRequest{ID: "blue", StatusOnly: true, ReadyProbe: true, NoLocalStateMutations: true}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			leaseID, _ := seedParallelsCleanupState(t)
			runner := &parallelsCleanupRunner{vmJSON: `[{"ID":"vm-good","Name":"crabbox-cbx-good-blue","State":"running"}]`}
			backend := &leaseBackend{DirectSSHBackend: sharedBackend(testParallelsCleanupConfig(), runner)}
			// No reported IP; cancellation makes discovery fail without a timed wait.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			lease, err := backend.Resolve(ctx, mode.request)
			if mode.wantError {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("Resolve error=%v, want original discovery cancellation", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if lease.Server.CloudID != "vm-good" || lease.LeaseID != leaseID || lease.SSH.Host != "" {
				t.Fatalf("unexpected resolved lease=%#v ssh=%#v", lease.Server, lease.SSH)
			}
			if !reflect.DeepEqual(runner.execVMIDs, []string{"vm-good"}) {
				t.Fatalf("guest metadata lookup lost VM identity: %v", runner.execVMIDs)
			}
		})
	}
}

func TestApplyFlagsSetsMacOSBootstrapKey(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = "parallels"
	cfg.TargetOS = core.TargetMacOS
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	provider := Provider{}
	values := provider.RegisterFlags(fs, cfg)
	if err := fs.Parse([]string{"--parallels-bootstrap-key", "/Users/build/.ssh/bootstrap"}); err != nil {
		t.Fatal(err)
	}
	if err := provider.ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Parallels.BootstrapKey != "/Users/build/.ssh/bootstrap" {
		t.Fatalf("bootstrap key=%q", cfg.Parallels.BootstrapKey)
	}
}

func TestDesktopCredentialsUseConfiguredMacOSAccount(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.TargetOS = core.TargetMacOS
	cfg.Parallels.User = "configured-user"
	cfg.Parallels.Password = " account password with spaces "

	credentials, ok := (Provider{}).DesktopCredentials(cfg, core.SSHTarget{TargetOS: core.TargetMacOS, User: "lease-user"})
	if !ok {
		t.Fatal("Parallels should provide configured macOS account credentials")
	}
	if credentials.Username != "lease-user" || credentials.Password != cfg.Parallels.Password {
		t.Fatalf("credentials=%#v", credentials)
	}

	cfg.Parallels.Password = ""
	if credentials, ok = (Provider{}).DesktopCredentials(cfg, core.SSHTarget{TargetOS: core.TargetMacOS}); ok || credentials != (core.DesktopCredentials{}) {
		t.Fatalf("empty password unexpectedly enabled account credentials: %#v", credentials)
	}
	cfg.Parallels.Password = "configured"
	cfg.TargetOS = core.TargetLinux
	if credentials, ok = (Provider{}).DesktopCredentials(cfg, core.SSHTarget{TargetOS: core.TargetLinux}); ok || credentials != (core.DesktopCredentials{}) {
		t.Fatalf("Linux unexpectedly received macOS account credentials: %#v", credentials)
	}
}

func TestParallelsSSHTargetScrubsDesktopPasswordEnvironment(t *testing.T) {
	target := parallelsSSHTarget(core.BaseConfig(), "192.0.2.10")
	if len(target.ChildEnvDenylist) != 1 || target.ChildEnvDenylist[0] != "CRABBOX_PARALLELS_PASSWORD" {
		t.Fatalf("child environment denylist=%v", target.ChildEnvDenylist)
	}
	leaseTarget := parallelsLeaseSSHTarget(core.BaseConfig(), "192.0.2.10")
	if len(leaseTarget.ChildEnvDenylist) != 1 || leaseTarget.ChildEnvDenylist[0] != "CRABBOX_PARALLELS_PASSWORD" {
		t.Fatalf("lease child environment denylist=%v", leaseTarget.ChildEnvDenylist)
	}
}

func TestApplyParallelsClaimSSHPort(t *testing.T) {
	target := core.SSHTarget{Port: "2222", FallbackPorts: []string{"22"}}
	claim := core.LeaseClaim{
		LeaseID: "cbx_example",
		CloudID: "vm-id",
		SSHPort: 22,
		Labels:  map[string]string{"host": "mac.example"},
	}
	if !applyParallelsClaimSSHPort(&target, claim, "cbx_example", "vm-id", "mac.example") {
		t.Fatal("exact Parallels claim endpoint was not applied")
	}
	if target.Port != "22" || len(target.FallbackPorts) != 0 {
		t.Fatalf("target endpoint=%#v", target)
	}

	wrong := core.SSHTarget{Port: "2222", FallbackPorts: []string{"22"}}
	if applyParallelsClaimSSHPort(&wrong, claim, "cbx_other", "vm-id", "mac.example") {
		t.Fatal("mismatched claim endpoint was applied")
	}
	if wrong.Port != "2222" || len(wrong.FallbackPorts) != 1 {
		t.Fatalf("mismatched target was mutated: %#v", wrong)
	}

	cfg := core.BaseConfig()
	if cfg.SSHPort != "2222" {
		t.Fatalf("base SSHPort=%q, want generic 2222 default", cfg.SSHPort)
	}
	if !applyParallelsClaimSSHPortToConfig(&cfg, claim, "cbx_example", "vm-id", "mac.example") {
		t.Fatal("exact Parallels claim port was not applied to config")
	}
	if cfg.SSHPort != "22" || len(cfg.SSHFallbackPorts) != 0 {
		t.Fatalf("config endpoint port=%q fallback=%v", cfg.SSHPort, cfg.SSHFallbackPorts)
	}
	if applyParallelsClaimSSHPortToConfig(&cfg, claim, "cbx_other", "vm-id", "mac.example") {
		t.Fatal("mismatched claim port was applied to config")
	}
	if applyParallelsClaimSSHPortToConfig(&cfg, claim, "cbx_example", "other-vm", "mac.example") {
		t.Fatal("claim port from a different VM was applied to config")
	}
	if applyParallelsClaimSSHPortToConfig(&cfg, claim, "cbx_example", "vm-id", "other.example") {
		t.Fatal("claim port from a different Parallels host was applied to config")
	}
}

func TestResolveReusesSavedSSHPortWhenToolsReportsNoIP(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))

	leaseID := "cbx_good"
	server := core.Server{
		CloudID:  "vm-good",
		Provider: "parallels",
		Name:     "crabbox-cbx-good-blue",
		Labels:   map[string]string{"provider": "parallels", "lease": leaseID, "slug": "blue", "host": "local"},
	}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, "blue", "parallels", "", "", "/repo", time.Minute, false, server, core.SSHTarget{Port: "22"}); err != nil {
		t.Fatal(err)
	}

	expiry := time.Now().Add(time.Hour).Unix()
	runner := &parallelsReuseDHCPRunner{
		vmJSON: `[{
			"ID":"vm-good","Name":"crabbox-cbx-good-blue","State":"running",
			"Hardware":{"net0":{"enabled":true,"mac":"001C4233EEDD"}},
			"Network":{"ipAddresses":[]}
		}]`,
		leases: "[vnic0]\n10.211.55.9=\"" + strconv.FormatInt(expiry, 10) + ",1800,001c4233eedd,01001c4233eedd\"\n",
	}
	cfg := core.BaseConfig()
	cfg.Provider = "parallels"
	cfg.TargetOS = core.TargetMacOS
	cfg.SSHPort = "2222"
	cfg.Parallels.BootstrapKey = "/Users/build/.ssh/bootstrap"
	backend := &leaseBackend{DirectSSHBackend: sharedBackend(cfg, runner)}

	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "blue"})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != leaseID || lease.Server.PublicNet.IPv4.IP != "10.211.55.9" || lease.SSH.Host != "10.211.55.9" || lease.SSH.Port != "22" {
		t.Fatalf("reuse lease=%#v ssh=%#v", lease.Server, lease.SSH)
	}
	if lease.Server.Labels["ip_source"] != "dhcp-mac" {
		t.Fatalf("ip_source=%q, want dhcp-mac", lease.Server.Labels["ip_source"])
	}
	if !reflect.DeepEqual(runner.probedPorts, []string{"22"}) {
		t.Fatalf("probed ports=%v, want saved port 22 only", runner.probedPorts)
	}
}

func TestResolvePreservesExactClaimSSHPortWhenIPDiscoveryFails(t *testing.T) {
	for _, mode := range []struct {
		name      string
		request   core.ResolveRequest
		wantError bool
	}{
		{name: "connection", request: core.ResolveRequest{ID: "blue"}, wantError: true},
		{name: "release", request: core.ResolveRequest{ID: "blue", ReleaseOnly: true}},
		{name: "status", request: core.ResolveRequest{ID: "blue", StatusOnly: true, NoLocalStateMutations: true}},
		{name: "status readiness", request: core.ResolveRequest{ID: "blue", StatusOnly: true, ReadyProbe: true, NoLocalStateMutations: true}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", filepath.Join(root, "home"))
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
			t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))

			leaseID := "cbx_good"
			server := core.Server{
				CloudID:  "vm-good",
				Provider: "parallels",
				Name:     "crabbox-cbx-good-blue",
				Labels:   map[string]string{"provider": "parallels", "lease": leaseID, "slug": "blue", "host": "local"},
			}
			if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, "blue", "parallels", "", "", "/repo", time.Minute, false, server, core.SSHTarget{Port: "22"}); err != nil {
				t.Fatal(err)
			}

			runner := &parallelsCleanupRunner{vmJSON: `[{"ID":"vm-good","Name":"crabbox-cbx-good-blue","State":"running"}]`}
			cfg := testParallelsCleanupConfig()
			cfg.SSHPort = "2222"
			cfg.SSHFallbackPorts = []string{"22"}
			backend := &leaseBackend{DirectSSHBackend: sharedBackend(cfg, runner)}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			lease, err := backend.Resolve(ctx, mode.request)
			if mode.wantError {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("Resolve error=%v, want original discovery cancellation after exact-claim SSH port restore", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if lease.Server.CloudID != "vm-good" || lease.LeaseID != leaseID || lease.SSH.Host != "" {
				t.Fatalf("unexpected resolved lease=%#v ssh=%#v", lease.Server, lease.SSH)
			}
			if lease.SSH.Port != "22" || len(lease.SSH.FallbackPorts) != 0 {
				t.Fatalf("status/release endpoint=%#v, want saved claim port 22", lease.SSH)
			}
			if !reflect.DeepEqual(runner.execVMIDs, []string{"vm-good"}) {
				t.Fatalf("guest metadata lookup lost VM identity: %v", runner.execVMIDs)
			}
		})
	}
}

func TestDoctorHostAndProbePathsOmitPasswordFromChildEnvironment(t *testing.T) {
	const password = "synthetic-parallels-password"
	t.Setenv("CRABBOX_PARALLELS_PASSWORD", password)
	runner := &parallelsReuseDHCPRunner{
		vmJSON: `[{"ID":"source-vm","Name":"source-vm","State":"running"}]`,
	}
	cfg := core.BaseConfig()
	cfg.Provider = "parallels"
	cfg.TargetOS = core.TargetMacOS
	cfg.Parallels.Source = "source-vm"
	backend := &leaseBackend{DirectSSHBackend: sharedBackend(cfg, runner)}

	_, _ = backend.Doctor(context.Background(), core.DoctorRequest{ProbeSSH: true})
	if len(runner.requests) == 0 {
		t.Fatal("doctor issued no host commands")
	}
	for _, req := range runner.requests {
		if req.Env == nil {
			t.Fatal("doctor host command inherited the process environment")
		}
		if commandRequestHasEnvName(req, "CRABBOX_PARALLELS_PASSWORD") {
			t.Fatal("CRABBOX_PARALLELS_PASSWORD was present in a doctor child environment")
		}
		if commandRequestPlacesValueOnArgv(req, password) {
			t.Fatal("password value was placed on argv")
		}
	}
	target := parallelsLeaseSSHTarget(cfg, "192.0.2.10")
	if len(target.ChildEnvDenylist) != 1 || target.ChildEnvDenylist[0] != "CRABBOX_PARALLELS_PASSWORD" {
		t.Fatalf("doctor SSH target denylist=%v", target.ChildEnvDenylist)
	}
}

func TestValidateConfigRestrictsBootstrapFallback(t *testing.T) {
	tests := []struct {
		name    string
		target  string
		key     string
		wantErr string
	}{
		{name: "macOS absolute", target: core.TargetMacOS, key: "/Users/build/.ssh/bootstrap"},
		{name: "macOS unset", target: core.TargetMacOS},
		{name: "Linux rejected", target: core.TargetLinux, key: "/Users/build/.ssh/bootstrap", wantErr: "only for macOS"},
		{name: "relative rejected", target: core.TargetMacOS, key: ".ssh/bootstrap", wantErr: "absolute path"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := core.BaseConfig()
			cfg.TargetOS = test.target
			cfg.Parallels.BootstrapKey = test.key
			err := (Provider{}).ValidateConfig(cfg)
			if test.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("err=%v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestParallelsMacOSBootstrapFallbackGate(t *testing.T) {
	toolsErr := errors.New("PRL_ERR_VM_EXEC_GUEST_TOOL_NOT_AVAILABLE")
	tests := []struct {
		name   string
		target string
		key    string
		err    error
		want   bool
	}{
		{name: "macOS tools unavailable", target: core.TargetMacOS, key: "/Users/build/.ssh/bootstrap", err: toolsErr, want: true},
		{name: "no bootstrap key", target: core.TargetMacOS, err: toolsErr},
		{name: "Linux", target: core.TargetLinux, key: "/Users/build/.ssh/bootstrap", err: toolsErr},
		{name: "Windows", target: core.TargetWindows, key: "/Users/build/.ssh/bootstrap", err: toolsErr},
		{name: "unrelated error", target: core.TargetMacOS, key: "/Users/build/.ssh/bootstrap", err: errors.New("permission denied")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := core.BaseConfig()
			cfg.TargetOS = test.target
			cfg.Parallels.BootstrapKey = test.key
			if got := parallelsMacOSBootstrapFallbackAllowed(cfg, test.err); got != test.want {
				t.Fatalf("got=%t want=%t", got, test.want)
			}
		})
	}
}

func TestResolveReportsPartialFleetInventory(t *testing.T) {
	backend := &leaseBackend{
		DirectSSHBackend: sharedBackend(testParallelsFleetConfig(), &parallelsFleetRunner{}),
	}
	_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "missing-lease"})
	if err == nil {
		t.Fatal("Resolve err=nil, want partial fleet inventory error")
	}
	for _, want := range []string{"fleet inventory incomplete", "bad-host", "ssh failed"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err=%q missing %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "lease not found") {
		t.Fatalf("err=%q should not report false not-found", err)
	}
}

func TestListReportsPartialFleetInventory(t *testing.T) {
	backend := &leaseBackend{
		DirectSSHBackend: sharedBackend(testParallelsFleetConfig(), &parallelsFleetRunner{}),
	}
	leases, err := backend.List(context.Background(), core.ListRequest{})
	if err == nil {
		t.Fatalf("List err=nil leases=%#v, want partial fleet inventory error", leases)
	}
	for _, want := range []string{"fleet inventory incomplete", "bad-host", "ssh failed"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err=%q missing %q", err, want)
		}
	}
}

func TestParallelsHostNameUsesDirectRemoteHost(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Parallels.Host = "mac-studio.example"
	if got := parallelsHostName(cfg); got != "mac-studio.example" {
		t.Fatalf("host=%q", got)
	}

	cfg.Parallels.SelectedHost = "studio-fleet"
	if got := parallelsHostName(cfg); got != "studio-fleet" {
		t.Fatalf("selected host=%q", got)
	}
}

func TestParallelsProxyCommandIsNonInteractive(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Parallels.Host = "mac-studio.example"
	cfg.Parallels.HostUser = "builder"
	got := parallelsProxyCommand(cfg, "192.0.2.10")
	for _, want := range []string{"BatchMode=yes", "ConnectTimeout=10", "builder@mac-studio.example", "192.0.2.10:%p"} {
		if !strings.Contains(got, want) {
			t.Fatalf("proxy command missing %q: %s", want, got)
		}
	}
}

func TestCleanupStopsOnPartialFleetInventory(t *testing.T) {
	runner := &parallelsFleetRunner{}
	backend := &leaseBackend{
		DirectSSHBackend: sharedBackend(testParallelsFleetConfig(), runner),
	}
	err := backend.Cleanup(context.Background(), core.CleanupRequest{})
	if err == nil {
		t.Fatal("Cleanup err=nil, want partial fleet inventory error")
	}
	if runner.deleteCalls != 0 {
		t.Fatalf("deleteCalls=%d want 0 before complete inventory", runner.deleteCalls)
	}
}

func TestCleanupRemovesClaimAndStoredKeyAfterDelete(t *testing.T) {
	leaseID, keyPath := seedParallelsCleanupState(t)
	runner := &parallelsCleanupRunner{}
	backend := &leaseBackend{
		DirectSSHBackend: sharedBackend(testParallelsCleanupConfig(), runner),
	}

	if err := backend.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if runner.deleteCalls != 1 {
		t.Fatalf("deleteCalls=%d want 1", runner.deleteCalls)
	}
	if _, ok, err := core.ResolveLeaseClaim("blue"); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatalf("claim for %s still resolves after cleanup", leaseID)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("key path still exists or unexpected stat error: %v", err)
	}
}

func TestCleanupKeepsClaimAndStoredKeyWhenDeleteFails(t *testing.T) {
	leaseID, keyPath := seedParallelsCleanupState(t)
	runner := &parallelsCleanupRunner{deleteErr: errors.New("delete failed")}
	backend := &leaseBackend{
		DirectSSHBackend: sharedBackend(testParallelsCleanupConfig(), runner),
	}

	err := backend.Cleanup(context.Background(), core.CleanupRequest{})
	if err == nil || !strings.Contains(err.Error(), "delete failed") {
		t.Fatalf("Cleanup err=%v, want delete failure", err)
	}
	if _, ok, err := core.ResolveLeaseClaim("blue"); err != nil {
		t.Fatal(err)
	} else if !ok {
		t.Fatalf("claim for %s was removed after failed delete", leaseID)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("key path missing after failed delete: %v", err)
	}
}

func TestReleaseRequiresExactClaim(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	runner := &parallelsCleanupRunner{}
	backend := &leaseBackend{
		DirectSSHBackend: sharedBackend(testParallelsCleanupConfig(), runner),
	}
	lease := core.LeaseTarget{
		LeaseID: "cbx_unclaimed",
		Server: core.Server{
			CloudID: "vm-good",
			Name:    "crabbox-cbx-unclaimed-blue",
			Labels:  map[string]string{"lease": "cbx_unclaimed", "host": "local"},
		},
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err == nil || !strings.Contains(err.Error(), "no exact local claim") {
		t.Fatalf("ReleaseLease unclaimed err=%v", err)
	}
	if runner.deleteCalls != 0 {
		t.Fatalf("deleteCalls=%d want 0", runner.deleteCalls)
	}
}

func TestResolveUnclaimedVMRequiresExplicitAdoption(t *testing.T) {
	tests := []struct {
		name    string
		request core.ResolveRequest
		wantErr string
	}{
		{name: "reuse", request: core.ResolveRequest{ID: "vm-good", Repo: core.Repo{Root: "/repo"}}, wantErr: "explicit --reclaim"},
		{name: "status", request: core.ResolveRequest{ID: "vm-good", StatusOnly: true}},
		{name: "reclaim", request: core.ResolveRequest{ID: "vm-good", Repo: core.Repo{Root: "/repo"}, Reclaim: true}},
		{name: "release", request: core.ResolveRequest{ID: "vm-good", ReleaseOnly: true}, wantErr: "no exact local claim"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", filepath.Join(root, "home"))
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
			t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
			backend := &leaseBackend{
				DirectSSHBackend: sharedBackend(testParallelsCleanupConfig(), &parallelsCleanupRunner{}),
			}

			lease, err := backend.Resolve(context.Background(), test.request)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("Resolve err=%v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if lease.LeaseID != "cbx_good" || lease.Server.CloudID != "vm-good" {
				t.Fatalf("Resolve lease=%#v", lease)
			}
			if test.request.Reclaim {
				owned, err := exactParallelsClaimOwned(lease.LeaseID, lease.Server.CloudID, "local")
				if err != nil {
					t.Fatal(err)
				}
				if !owned {
					t.Fatal("explicit reclaim did not persist an exact VM/host claim")
				}
			}
		})
	}
}

func TestAcquireRemovesStoredKeyAfterPostKeyFailure(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not available")
	}
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	cfg := core.BaseConfig()
	cfg.Provider = "parallels"
	cfg.TargetOS = core.TargetLinux
	cfg.Parallels.Source = "source-vm"
	cfg.Parallels.SourceSnapshot = "missing-snapshot"
	cfg.Parallels.Hosts = []core.ParallelsHostConfig{{Name: "fixture-host", MaxVMs: 2}}
	runner := &parallelsAcquireRunner{snapshotErr: errors.New("snapshot lookup failed")}
	backend := &leaseBackend{
		DirectSSHBackend: sharedBackend(cfg, runner),
	}

	_, err := backend.acquireOnce(context.Background(), false, "")
	if err == nil || !strings.Contains(err.Error(), "snapshot lookup failed") {
		t.Fatalf("acquireOnce err=%v, want snapshot lookup failure", err)
	}
	keyMatches := storedTestboxKeyMatches(t)
	if len(keyMatches) != 0 {
		t.Fatalf("stored keys remain after failed acquire: %v", keyMatches)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, release, err := core.ReserveParallelsFleetCapacity(ctx, cfg, runner, "source-vm")
	if err != nil {
		t.Fatalf("failed acquisition retained its capacity reservation: %v", err)
	}
	release()
}

func TestAcquireHoldsCapacityThroughCloneOnly(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not available")
	}
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	cfg := core.BaseConfig()
	cfg.Provider = "parallels"
	cfg.TargetOS = core.TargetLinux
	cfg.Parallels.Source = "source-vm"
	cfg.Parallels.CloneMode = "full"
	cfg.Parallels.Hosts = []core.ParallelsHostConfig{{Name: "fixture-host", MaxVMs: 2}}
	var observed []string
	runner := &parallelsAcquireRunner{startErr: errors.New("stop after clone")}
	runner.beforeCommand = func(command string) {
		if command != "clone" && command != "start" {
			return
		}
		observed = append(observed, command)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, release, err := core.ReserveParallelsFleetCapacity(ctx, cfg, &parallelsAcquireRunner{}, "source-vm")
		if release != nil {
			release()
		}
		if command == "clone" {
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("clone ran without retaining the capacity reservation: %v", err)
			}
		} else if err != nil {
			t.Fatalf("capacity reservation remained held during startup: %v", err)
		}
	}
	backend := &leaseBackend{DirectSSHBackend: sharedBackend(cfg, runner)}
	if _, err := backend.acquireOnce(context.Background(), false, ""); err == nil || !strings.Contains(err.Error(), "stop after clone") {
		t.Fatalf("acquireOnce err=%v, want controlled post-clone stop", err)
	}
	if strings.Join(observed, ",") != "clone,start" {
		t.Fatalf("observed acquisition stages=%v, want clone and start", observed)
	}
}

func TestParallelsAcquireKeepsStoredKeyWhenRollbackDeleteFails(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not available")
	}
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	cfg := core.BaseConfig()
	cfg.Provider = "parallels"
	cfg.TargetOS = core.TargetLinux
	cfg.Parallels.Source = "source-vm"
	cfg.Parallels.CloneMode = "full"
	cfg.Parallels.VMRoot = "/Volumes/VM Storage"
	runner := &parallelsAcquireRunner{
		startErr:  errors.New("start failed"),
		deleteErr: errors.New("delete failed"),
	}
	backend := &leaseBackend{
		DirectSSHBackend: sharedBackend(cfg, runner),
	}

	_, err := backend.acquireOnce(context.Background(), false, "")
	if err == nil || !strings.Contains(err.Error(), "start failed") {
		t.Fatalf("acquireOnce err=%v, want start failure", err)
	}
	wantCloneArgs := []string{"clone", "source-vm", "--name", runner.cloneName, "--dst", cfg.Parallels.VMRoot}
	if runner.cloneName == "" || !reflect.DeepEqual(runner.cloneArgs, wantCloneArgs) {
		t.Fatalf("clone args=%q, want %q", runner.cloneArgs, wantCloneArgs)
	}
	keyMatches := storedTestboxKeyMatches(t)
	if len(keyMatches) != 1 {
		t.Fatalf("stored keys=%v, want one preserved key after rollback delete failure", keyMatches)
	}
}

func storedTestboxKeyMatches(t *testing.T) []string {
	t.Helper()
	key, err := core.TestboxKeyPath("fixture-root")
	if err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(filepath.Dir(key)), "*", filepath.Base(key)))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

func sharedBackend(cfg core.Config, runner core.CommandRunner) shared.DirectSSHBackend {
	return shared.DirectSSHBackend{Cfg: cfg, RT: core.Runtime{Exec: runner, Stderr: io.Discard}}
}

func testParallelsFleetConfig() core.Config {
	cfg := core.BaseConfig()
	cfg.Provider = "parallels"
	cfg.TargetOS = core.TargetLinux
	cfg.Parallels.Hosts = []core.ParallelsHostConfig{
		{Name: "good-host", Host: "good.example"},
		{Name: "bad-host", Host: "bad.example"},
	}
	return cfg
}

func testParallelsCleanupConfig() core.Config {
	cfg := core.BaseConfig()
	cfg.Provider = "parallels"
	cfg.TargetOS = core.TargetLinux
	return cfg
}

func seedParallelsCleanupState(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	leaseID := "cbx_good"
	server := core.Server{
		CloudID:  "vm-good",
		Provider: "parallels",
		Name:     "crabbox-cbx-good-blue",
		Labels:   map[string]string{"provider": "parallels", "lease": leaseID, "slug": "blue", "host": "local"},
	}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, "blue", "parallels", "", "", "/repo", time.Minute, false, server, core.SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("private key"), 0o600); err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{
		"provider":   "parallels",
		"lease":      leaseID,
		"slug":       "blue",
		"state":      "ready",
		"expires_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
	}
	data, err := json.Marshal(labels)
	if err != nil {
		t.Fatal(err)
	}
	labelsPath := filepath.Join(os.Getenv("XDG_STATE_HOME"), "crabbox", "parallels", "leases", leaseID+".json")
	if err := os.MkdirAll(filepath.Dir(labelsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(labelsPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return leaseID, keyPath
}

type parallelsFleetRunner struct {
	deleteCalls int
}

func (r *parallelsFleetRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	if req.Name != "ssh" || len(req.Args) < 2 {
		return core.LocalCommandResult{}, errors.New("unexpected command")
	}
	host := req.Args[len(req.Args)-2]
	remote := req.Args[len(req.Args)-1]
	if strings.Contains(remote, " delete ") {
		r.deleteCalls++
	}
	if host == "bad.example" {
		return core.LocalCommandResult{Stderr: "permission denied"}, errors.New("ssh failed")
	}
	return core.LocalCommandResult{Stdout: `[{"ID":"vm-good","Name":"crabbox-cbx-good-blue","State":"running","ip_configured":"10.0.0.5"}]`}, nil
}

type parallelsCleanupRunner struct {
	nextVMJSON  string
	vmJSON      string
	execVMIDs   []string
	deleteCalls int
	deleteErr   error
}

func (r *parallelsCleanupRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	if req.Name != "prlctl" || len(req.Args) == 0 {
		return core.LocalCommandResult{}, errors.New("unexpected command")
	}
	switch req.Args[0] {
	case "exec":
		if len(req.Args) > 1 {
			r.execVMIDs = append(r.execVMIDs, req.Args[1])
		}
		return core.LocalCommandResult{}, nil
	case "list":
		if r.vmJSON != "" {
			vmJSON := r.vmJSON
			if r.nextVMJSON != "" {
				r.vmJSON, r.nextVMJSON = r.nextVMJSON, ""
			}
			return core.LocalCommandResult{Stdout: vmJSON}, nil
		}
		return core.LocalCommandResult{Stdout: `[{"ID":"vm-good","Name":"crabbox-cbx-good-blue","State":"stopped","ip_configured":"10.0.0.5"}]`}, nil
	case "delete":
		r.deleteCalls++
		if r.deleteErr != nil {
			return core.LocalCommandResult{Stderr: r.deleteErr.Error()}, r.deleteErr
		}
		return core.LocalCommandResult{}, nil
	default:
		return core.LocalCommandResult{}, errors.New("unexpected prlctl command")
	}
}

type parallelsAcquireRunner struct {
	beforeCommand func(string)
	snapshotErr   error
	startErr      error
	deleteErr     error
	cloneID       string
	cloneName     string
	cloneArgs     []string
}

func (r *parallelsAcquireRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	if req.Name != "prlctl" || len(req.Args) == 0 {
		return core.LocalCommandResult{}, errors.New("unexpected command")
	}
	if r.beforeCommand != nil {
		r.beforeCommand(req.Args[0])
	}
	switch req.Args[0] {
	case "list":
		if len(req.Args) > 1 && req.Args[1] == "-i" {
			id := r.cloneID
			if id == "" {
				id = "clone-id"
			}
			name := r.cloneName
			if name == "" {
				name = "crabbox-cbx-test-blue"
			}
			return core.LocalCommandResult{Stdout: fmt.Sprintf(`[{"ID":%q,"Name":%q,"State":"running","ip_configured":"10.0.0.5"}]`, id, name)}, nil
		}
		return core.LocalCommandResult{Stdout: `[{"ID":"source-id","Name":"source-vm","State":"stopped","ip_configured":"10.0.0.5"}]`}, nil
	case "snapshot-list":
		if r.snapshotErr != nil {
			return core.LocalCommandResult{Stderr: r.snapshotErr.Error()}, r.snapshotErr
		}
		return core.LocalCommandResult{Stdout: `[]`}, nil
	case "clone":
		r.cloneArgs = append([]string(nil), req.Args...)
		r.cloneID = "clone-id"
		for i := 0; i+1 < len(req.Args); i++ {
			if req.Args[i] == "--name" {
				r.cloneName = req.Args[i+1]
			}
		}
		return core.LocalCommandResult{}, nil
	case "start":
		if r.startErr != nil {
			return core.LocalCommandResult{Stderr: r.startErr.Error()}, r.startErr
		}
		return core.LocalCommandResult{}, nil
	case "stop":
		return core.LocalCommandResult{}, nil
	case "delete":
		if r.deleteErr != nil {
			return core.LocalCommandResult{Stderr: r.deleteErr.Error()}, r.deleteErr
		}
		return core.LocalCommandResult{}, nil
	default:
		return core.LocalCommandResult{}, errors.New("unexpected prlctl command")
	}
}

func TestParallelsConfigShowCompleteRawFields(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config core.ParallelsConfig
		fields []core.ProviderConfigShowField
	}{{name: "zero", config: core.ParallelsConfig{Template: "", Source: "", SourceID: "", SourceSnapshot: "", SourceSnapshotID: "", CloneMode: "", Host: "", HostUser: "", HostKey: "", VMRoot: "", User: "", WorkRoot: "", SelectedHost: "internal-selected-host", StartupTimeout: 0}, fields: []core.ProviderConfigShowField{{JSONName: "template", JSONValue: "", TextName: "template", TextValue: "-"}, {JSONName: "source", JSONValue: "", TextName: "source", TextValue: "-"}, {JSONName: "sourceId", JSONValue: "", TextName: "source_id", TextValue: "-"}, {JSONName: "sourceSnapshot", JSONValue: "", TextName: "snapshot", TextValue: "-"}, {JSONName: "sourceSnapshotId", JSONValue: "", TextName: "snapshot_id", TextValue: "-"}, {JSONName: "cloneMode", JSONValue: "", TextName: "clone_mode", TextValue: ""}, {JSONName: "host", JSONValue: "", TextName: "host", TextValue: "local"}, {JSONName: "maxVMs", JSONValue: 0, TextName: "max_vms", TextValue: "0"}, {JSONName: "hostUser", JSONValue: ""}, {JSONName: "hostKey", JSONValue: "missing"}, {JSONName: "bootstrapKey", JSONValue: "missing"}, {JSONName: "vmRoot", JSONValue: ""}, {JSONName: "user", JSONValue: "", TextName: "user", TextValue: ""}, {JSONName: "auth", JSONValue: "missing", TextName: "auth", TextValue: "missing"}, {JSONName: "workRoot", JSONValue: "", TextName: "work_root", TextValue: ""}, {JSONName: "startupTimeout", JSONValue: "0s", TextName: "startup_timeout", TextValue: "0s"}, {JSONName: "templates", JSONValue: map[string]core.ParallelsTemplateConfig(nil), TextName: "templates", TextValue: "0"}, {JSONName: "hosts", JSONValue: []core.ParallelsHostConfig(nil), TextName: "hosts", TextValue: "0"}}},
		{name: "raw", config: core.ParallelsConfig{Template: " Template reference ", Source: " Source reference ", SourceID: " SourceID reference ", SourceSnapshot: " SourceSnapshot reference ", SourceSnapshotID: " SourceSnapshotID reference ", CloneMode: " CloneMode reference ", Host: " Host reference ", HostUser: " HostUser reference ", HostKey: " HostKey reference ", VMRoot: " VMRoot reference ", User: " User reference ", WorkRoot: " WorkRoot reference ", SelectedHost: "internal-selected-host", MaxVMs: 4, StartupTimeout: -1500 * time.Millisecond}, fields: []core.ProviderConfigShowField{{JSONName: "template", JSONValue: " Template reference ", TextName: "template", TextValue: " Template reference "}, {JSONName: "source", JSONValue: " Source reference ", TextName: "source", TextValue: " Source reference "}, {JSONName: "sourceId", JSONValue: " SourceID reference ", TextName: "source_id", TextValue: " SourceID reference "}, {JSONName: "sourceSnapshot", JSONValue: " SourceSnapshot reference ", TextName: "snapshot", TextValue: " SourceSnapshot reference "}, {JSONName: "sourceSnapshotId", JSONValue: " SourceSnapshotID reference ", TextName: "snapshot_id", TextValue: " SourceSnapshotID reference "}, {JSONName: "cloneMode", JSONValue: " CloneMode reference ", TextName: "clone_mode", TextValue: " CloneMode reference "}, {JSONName: "host", JSONValue: " Host reference ", TextName: "host", TextValue: " Host reference "}, {JSONName: "maxVMs", JSONValue: 4, TextName: "max_vms", TextValue: "4"}, {JSONName: "hostUser", JSONValue: " HostUser reference "}, {JSONName: "hostKey", JSONValue: "configured"}, {JSONName: "bootstrapKey", JSONValue: "missing"}, {JSONName: "vmRoot", JSONValue: " VMRoot reference "}, {JSONName: "user", JSONValue: " User reference ", TextName: "user", TextValue: " User reference "}, {JSONName: "auth", JSONValue: "missing", TextName: "auth", TextValue: "missing"}, {JSONName: "workRoot", JSONValue: " WorkRoot reference ", TextName: "work_root", TextValue: " WorkRoot reference "}, {JSONName: "startupTimeout", JSONValue: "-1.5s", TextName: "startup_timeout", TextValue: "-1.5s"}, {JSONName: "templates", JSONValue: map[string]core.ParallelsTemplateConfig(nil), TextName: "templates", TextValue: "0"}, {JSONName: "hosts", JSONValue: []core.ParallelsHostConfig(nil), TextName: "hosts", TextValue: "0"}}},
		{name: "whitespace", config: core.ParallelsConfig{Template: " \t ", Source: " \t ", SourceID: " \t ", SourceSnapshot: " \t ", SourceSnapshotID: " \t ", CloneMode: " \t ", Host: " \t ", HostUser: " \t ", HostKey: " \t ", VMRoot: " \t ", User: " \t ", WorkRoot: " \t ", SelectedHost: "internal-selected-host", MaxVMs: -1, StartupTimeout: -1500 * time.Millisecond}, fields: []core.ProviderConfigShowField{{JSONName: "template", JSONValue: " \t ", TextName: "template", TextValue: " \t "}, {JSONName: "source", JSONValue: " \t ", TextName: "source", TextValue: " \t "}, {JSONName: "sourceId", JSONValue: " \t ", TextName: "source_id", TextValue: " \t "}, {JSONName: "sourceSnapshot", JSONValue: " \t ", TextName: "snapshot", TextValue: " \t "}, {JSONName: "sourceSnapshotId", JSONValue: " \t ", TextName: "snapshot_id", TextValue: " \t "}, {JSONName: "cloneMode", JSONValue: " \t ", TextName: "clone_mode", TextValue: " \t "}, {JSONName: "host", JSONValue: " \t ", TextName: "host", TextValue: " \t "}, {JSONName: "maxVMs", JSONValue: -1, TextName: "max_vms", TextValue: "-1"}, {JSONName: "hostUser", JSONValue: " \t "}, {JSONName: "hostKey", JSONValue: "configured"}, {JSONName: "bootstrapKey", JSONValue: "missing"}, {JSONName: "vmRoot", JSONValue: " \t "}, {JSONName: "user", JSONValue: " \t ", TextName: "user", TextValue: " \t "}, {JSONName: "auth", JSONValue: "missing", TextName: "auth", TextValue: "missing"}, {JSONName: "workRoot", JSONValue: " \t ", TextName: "work_root", TextValue: " \t "}, {JSONName: "startupTimeout", JSONValue: "-1.5s", TextName: "startup_timeout", TextValue: "-1.5s"}, {JSONName: "templates", JSONValue: map[string]core.ParallelsTemplateConfig(nil), TextName: "templates", TextValue: "0"}, {JSONName: "hosts", JSONValue: []core.ParallelsHostConfig(nil), TextName: "hosts", TextValue: "0"}}}} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := core.Config{Parallels: tc.config}
			want := core.ProviderConfigShowSection{JSONKey: "parallels", TextLabel: "parallels", Providers: []string{"parallels"}, Fields: tc.fields}
			for _, selection := range []string{"unselected-display", "parallels"} {
				cfg.Provider = selection
				before := cfg
				got := (Provider{}).ConfigShowSection(cfg)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("section=%#v want%#v", got, want)
				}
				if !reflect.DeepEqual(cfg, before) {
					t.Fatal("projection mutated config")
				}
			}
		})
	}
}

func TestParallelsConfigShowTypedCollections(t *testing.T) {
	for _, empty := range []bool{true, false} {
		t.Run(map[bool]string{true: "nonnull empty", false: "populated ordered duplicates"}[empty], func(t *testing.T) {
			cfg := core.Config{}
			cfg.Parallels.Templates = map[string]core.ParallelsTemplateConfig{}
			cfg.Parallels.Hosts = []core.ParallelsHostConfig{}
			cfg.Parallels.MaxVMs = 7
			if !empty {
				cfg.Parallels.Templates["raw template key"] = core.ParallelsTemplateConfig{Source: "source", SourceID: "source-id", SourceSnapshot: "snapshot", SourceSnapshotID: "snapshot-id", TargetOS: "linux", WindowsMode: "normal", CloneMode: "linked", Host: "host", HostUser: "host-user", HostKey: "loaded-template-key", VMRoot: "/vm", User: "user", WorkRoot: "/work"}
				first := core.ParallelsHostConfig{Name: "first", Host: "first-host", User: "host-user", Key: "loaded-host-key", VMRoot: "/vm", Targets: []string{"linux", "linux", "macos"}, MaxVMs: 3}
				second := core.ParallelsHostConfig{Name: "second", Host: "second-host", Targets: []string{}, MaxVMs: 0}
				cfg.Parallels.Hosts = []core.ParallelsHostConfig{first, second, first}
				cfg.Parallels.SelectedHost = "first"
			}
			before, err := json.Marshal(cfg.Parallels)
			if err != nil {
				t.Fatal(err)
			}
			section := (Provider{}).ConfigShowSection(cfg)
			values := map[string]any{}
			texts := map[string]string{}
			for _, field := range section.Fields {
				values[field.JSONName] = field.JSONValue
				if field.TextName != "" {
					texts[field.TextName] = field.TextValue
				}
			}
			if len(values) != 18 || len(texts) != 14 {
				t.Fatalf("field counts=%d/%d", len(values), len(texts))
			}
			if values["maxVMs"] != 7 || texts["max_vms"] != "7" {
				t.Fatal("direct-host display substituted a selected fleet limit")
			}
			for _, omitted := range []string{"hostUser", "hostKey", "bootstrapKey", "vmRoot", "selectedHost", "SelectedHost"} {
				if _, ok := texts[omitted]; ok {
					t.Fatalf("JSON-only/internal field in text: %s", omitted)
				}
			}
			if _, ok := values["selectedHost"]; ok {
				t.Fatal("internal selectedHost exposed")
			}
			templates, ok := values["templates"].(map[string]core.ParallelsTemplateConfig)
			if !ok || templates == nil {
				t.Fatalf("templates type/nil=%T %v", values["templates"], templates == nil)
			}
			hosts, ok := values["hosts"].([]core.ParallelsHostConfig)
			if !ok || hosts == nil {
				t.Fatalf("hosts type/nil=%T %v", values["hosts"], hosts == nil)
			}
			encoded, err := json.Marshal(values)
			if err != nil {
				t.Fatal(err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			if empty {
				if len(templates) != 0 || len(hosts) != 0 || texts["templates"] != "0" || texts["hosts"] != "0" {
					t.Fatal("empty collections changed")
				}
				if !strings.Contains(string(encoded), `"templates":{}`) || !strings.Contains(string(encoded), `"hosts":[]`) {
					t.Fatalf("empty JSON=%s", encoded)
				}
			} else {
				wantTemplate := map[string]any{"Source": "source", "SourceID": "source-id", "SourceSnapshot": "snapshot", "SourceSnapshotID": "snapshot-id", "TargetOS": "linux", "WindowsMode": "normal", "CloneMode": "linked", "Host": "host", "HostUser": "host-user", "HostKey": "configured", "VMRoot": "/vm", "User": "user", "WorkRoot": "/work"}
				if !reflect.DeepEqual(decoded["templates"], map[string]any{"raw template key": wantTemplate}) {
					t.Fatalf("typed template JSON=%#v", decoded["templates"])
				}
				wantFirst := map[string]any{"Name": "first", "Host": "first-host", "User": "host-user", "Key": "configured", "VMRoot": "/vm", "Targets": []any{"linux", "linux", "macos"}, "MaxVMs": float64(3)}
				wantSecond := map[string]any{"Name": "second", "Host": "second-host", "User": "", "Key": "missing", "VMRoot": "", "Targets": []any{}, "MaxVMs": float64(0)}
				if !reflect.DeepEqual(decoded["hosts"], []any{wantFirst, wantSecond, wantFirst}) {
					t.Fatalf("typed host JSON=%#v", decoded["hosts"])
				}
				if texts["templates"] != "1" || texts["hosts"] != "3" {
					t.Fatalf("counts=%v", texts)
				}
				if &hosts[0].Targets[0] != &cfg.Parallels.Hosts[0].Targets[0] {
					t.Fatal("Targets shallow alias changed")
				}
				hosts[0].Host = "result-only"
				template := templates["raw template key"]
				template.Host = "result-only"
				templates["raw template key"] = template
			}
			after, err := json.Marshal(cfg.Parallels)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("projection or outer-result edits mutated source")
			}
		})
	}
}

func TestConfigShowRedactsParallelsSSHKeys(t *testing.T) {
	const (
		topLevelKey = "top-level-private-key-sentinel"
		templateKey = "template-private-key-sentinel"
		hostKey     = "host-private-key-sentinel"
		password    = "guest-account-password-sentinel"
	)
	cfg := core.Config{}
	cfg.Parallels.HostKey = topLevelKey
	cfg.Parallels.Password = password
	cfg.Parallels.Templates = map[string]core.ParallelsTemplateConfig{
		"macos": {
			Source:  "macOS Tahoe",
			Host:    "template-host.example.test",
			HostKey: templateKey,
		},
	}
	cfg.Parallels.Hosts = []core.ParallelsHostConfig{{
		Name: "builder",
		Host: "builder.example.test",
		Key:  hostKey,
	}}

	var text bytes.Buffer
	section := (Provider{}).ConfigShowSection(cfg)
	values := map[string]any{}
	text.WriteString(section.TextLabel)
	for _, field := range section.Fields {
		values[field.JSONName] = field.JSONValue
		if field.TextName != "" {
			text.WriteString(" " + field.TextName + "=" + field.TextValue)
		}
	}
	text.WriteByte('\n')
	jsonData, err := json.Marshal(map[string]any{"parallels": values})
	if err != nil {
		t.Fatal(err)
	}
	for name, output := range map[string]string{"text": text.String(), "json": string(jsonData)} {
		for _, secret := range []string{topLevelKey, templateKey, hostKey, password} {
			if strings.Contains(output, secret) {
				t.Fatalf("%s config output leaked %q: %s", name, secret, output)
			}
		}
	}

	var decoded map[string]any
	if err := json.Unmarshal(jsonData, &decoded); err != nil {
		t.Fatal(err)
	}
	parallels := decoded["parallels"].(map[string]any)
	if parallels["hostKey"] != "configured" {
		t.Fatalf("top-level hostKey=%#v, want configured", parallels["hostKey"])
	}
	if parallels["auth"] != "configured" {
		t.Fatalf("parallels auth=%#v, want configured", parallels["auth"])
	}
	templates := parallels["templates"].(map[string]any)
	template := templates["macos"].(map[string]any)
	if template["HostKey"] != "configured" || template["Source"] != "macOS Tahoe" {
		t.Fatalf("template view=%#v", template)
	}
	hosts := parallels["hosts"].([]any)
	host := hosts[0].(map[string]any)
	if host["Key"] != "configured" || host["Host"] != "builder.example.test" {
		t.Fatalf("host view=%#v", host)
	}

	if cfg.Parallels.HostKey != topLevelKey || cfg.Parallels.Password != password || cfg.Parallels.Templates["macos"].HostKey != templateKey || cfg.Parallels.Hosts[0].Key != hostKey {
		t.Fatal("config-show redaction mutated the effective Parallels config")
	}
	if redactedParallelsTemplateConfigs(nil) != nil || redactedParallelsHostConfigs(nil) != nil {
		t.Fatal("config-show redaction changed nil Parallels collections")
	}
}

type parallelsReuseDHCPRunner struct {
	vmJSON      string
	leases      string
	requests    []core.LocalCommandRequest
	probedPorts []string
}

func (r *parallelsReuseDHCPRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.requests = append(r.requests, req)
	rendered := req.Name + " " + strings.Join(req.Args, " ")
	switch {
	case strings.Contains(rendered, "prlctl") && strings.Contains(rendered, "list"):
		return core.LocalCommandResult{Stdout: r.vmJSON}, nil
	case strings.Contains(rendered, "parallels_dhcp_leases"):
		return core.LocalCommandResult{Stdout: r.leases}, nil
	case req.Name == "/usr/bin/nc" || strings.Contains(rendered, "/usr/bin/nc"):
		port := ""
		if len(req.Args) > 0 {
			port = req.Args[len(req.Args)-1]
		}
		r.probedPorts = append(r.probedPorts, port)
		if port != "22" {
			return core.LocalCommandResult{Stderr: "connection refused"}, errors.New("connection refused")
		}
		return core.LocalCommandResult{}, nil
	default:
		return core.LocalCommandResult{}, nil
	}
}

func commandRequestHasEnvName(req core.LocalCommandRequest, name string) bool {
	prefix := strings.ToUpper(name) + "="
	for _, entry := range req.Env {
		if strings.HasPrefix(strings.ToUpper(entry), prefix) {
			return true
		}
	}
	return false
}

func commandRequestPlacesValueOnArgv(req core.LocalCommandRequest, value string) bool {
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

// parallelsTouchRunner fails every command so the test proves Touch resolves the
// lease host from its label instead of probing candidate hosts with prlctl.
type parallelsTouchRunner struct {
	calls int
}

func (r *parallelsTouchRunner) Run(context.Context, core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.calls++
	return core.LocalCommandResult{}, errors.New("unexpected command")
}

func TestParallelsTouchPersistsIdlePolicyOverride(t *testing.T) {
	override := 45 * time.Minute
	for _, tc := range []struct {
		name      string
		storedKey string
		override  *time.Duration
		want      string
	}{
		{"preserve", "idle_timeout_secs", nil, "1800"},
		{"replace", "idle_timeout_secs", &override, "2700"},
		{"preserve legacy", "idle_timeout", nil, "1800"},
		{"replace legacy", "idle_timeout", &override, "2700"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", filepath.Join(root, "home"))
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
			t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
			leaseID := "cbx_touch_idle"
			server := core.Server{
				CloudID:  "vm-touch",
				Provider: "parallels",
				Name:     "crabbox-cbx-touch-idle-blue",
				Labels: map[string]string{
					"provider":   "parallels",
					"lease":      leaseID,
					"slug":       "blue",
					"host":       "local",
					tc.storedKey: "1800",
				},
			}
			before := maps.Clone(server.Labels)
			runner := &parallelsTouchRunner{}
			cfg := core.BaseConfig()
			cfg.Provider = "parallels"
			cfg.TargetOS = core.TargetLinux
			cfg.IdleTimeout = 5 * time.Minute
			backend := &leaseBackend{DirectSSHBackend: shared.DirectSSHBackend{Cfg: cfg, RT: core.Runtime{Exec: runner, Stderr: io.Discard}}}

			got, err := backend.Touch(context.Background(), core.TouchRequest{
				Lease:               core.LeaseTarget{LeaseID: leaseID, Server: server},
				State:               "ready",
				IdleTimeout:         cfg.IdleTimeout,
				IdleTimeoutOverride: tc.override,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got.Labels["idle_timeout_secs"] != tc.want || got.Labels["idle_timeout"] != tc.want {
				t.Fatalf("returned idle policy=%v, want both idle_timeout keys=%s", got.Labels, tc.want)
			}
			if !maps.Equal(persistedParallelsLeaseLabels(t, leaseID), got.Labels) {
				t.Fatalf("persisted=%v, want the returned labels %v", persistedParallelsLeaseLabels(t, leaseID), got.Labels)
			}
			if !maps.Equal(server.Labels, before) {
				t.Fatalf("touch mutated the input labels: %v, want %v", server.Labels, before)
			}
			if runner.calls != 0 {
				t.Fatalf("runner calls=%d, want 0 for a labeled host", runner.calls)
			}
		})
	}
}

func persistedParallelsLeaseLabels(t *testing.T, leaseID string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(os.Getenv("XDG_STATE_HOME"), "crabbox", "parallels", "leases", leaseID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var labels map[string]string
	if err := json.Unmarshal(data, &labels); err != nil {
		t.Fatal(err)
	}
	return labels
}
