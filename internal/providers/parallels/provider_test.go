package parallels

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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

func TestResolveReportsPartialFleetInventory(t *testing.T) {
	backend := &leaseBackend{
		DirectSSHBackend: sharedBackend(testParallelsFleetConfig(), &parallelsFleetRunner{}),
	}
	_, err := backend.Resolve(context.Background(), ResolveRequest{ID: "missing-lease"})
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
	leases, err := backend.List(context.Background(), ListRequest{})
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
	err := backend.Cleanup(context.Background(), CleanupRequest{})
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

	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
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

	err := backend.Cleanup(context.Background(), CleanupRequest{})
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
	lease := LeaseTarget{
		LeaseID: "cbx_unclaimed",
		Server: core.Server{
			CloudID: "vm-good",
			Name:    "crabbox-cbx-unclaimed-blue",
			Labels:  map[string]string{"lease": "cbx_unclaimed", "host": "local"},
		},
	}
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err == nil || !strings.Contains(err.Error(), "no exact local claim") {
		t.Fatalf("ReleaseLease unclaimed err=%v", err)
	}
	if runner.deleteCalls != 0 {
		t.Fatalf("deleteCalls=%d want 0", runner.deleteCalls)
	}
}

func TestResolveUnclaimedVMRequiresExplicitAdoption(t *testing.T) {
	tests := []struct {
		name    string
		request ResolveRequest
		wantErr string
	}{
		{name: "reuse", request: ResolveRequest{ID: "vm-good", Repo: core.Repo{Root: "/repo"}}, wantErr: "explicit --reclaim"},
		{name: "status", request: ResolveRequest{ID: "vm-good", StatusOnly: true}},
		{name: "reclaim", request: ResolveRequest{ID: "vm-good", Repo: core.Repo{Root: "/repo"}, Reclaim: true}},
		{name: "release", request: ResolveRequest{ID: "vm-good", ReleaseOnly: true}, wantErr: "no exact local claim"},
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
	return shared.DirectSSHBackend{Cfg: cfg, RT: Runtime{Exec: runner, Stderr: io.Discard}}
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
	deleteCalls int
	deleteErr   error
}

func (r *parallelsCleanupRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	if req.Name != "prlctl" || len(req.Args) == 0 {
		return core.LocalCommandResult{}, errors.New("unexpected command")
	}
	switch req.Args[0] {
	case "list":
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
	snapshotErr error
	startErr    error
	deleteErr   error
	cloneID     string
	cloneName   string
	cloneArgs   []string
}

func (r *parallelsAcquireRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	if req.Name != "prlctl" || len(req.Args) == 0 {
		return core.LocalCommandResult{}, errors.New("unexpected command")
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
	}{{name: "zero", config: core.ParallelsConfig{Template: "", Source: "", SourceID: "", SourceSnapshot: "", SourceSnapshotID: "", CloneMode: "", Host: "", HostUser: "", HostKey: "", VMRoot: "", User: "", WorkRoot: "", SelectedHost: "internal-selected-host", StartupTimeout: 0}, fields: []core.ProviderConfigShowField{{JSONName: "template", JSONValue: "", TextName: "template", TextValue: "-"}, {JSONName: "source", JSONValue: "", TextName: "source", TextValue: "-"}, {JSONName: "sourceId", JSONValue: "", TextName: "source_id", TextValue: "-"}, {JSONName: "sourceSnapshot", JSONValue: "", TextName: "snapshot", TextValue: "-"}, {JSONName: "sourceSnapshotId", JSONValue: "", TextName: "snapshot_id", TextValue: "-"}, {JSONName: "cloneMode", JSONValue: "", TextName: "clone_mode", TextValue: ""}, {JSONName: "host", JSONValue: "", TextName: "host", TextValue: "local"}, {JSONName: "hostUser", JSONValue: ""}, {JSONName: "hostKey", JSONValue: "missing"}, {JSONName: "vmRoot", JSONValue: ""}, {JSONName: "user", JSONValue: "", TextName: "user", TextValue: ""}, {JSONName: "workRoot", JSONValue: "", TextName: "work_root", TextValue: ""}, {JSONName: "startupTimeout", JSONValue: "0s", TextName: "startup_timeout", TextValue: "0s"}, {JSONName: "templates", JSONValue: map[string]core.ParallelsTemplateConfig(nil), TextName: "templates", TextValue: "0"}, {JSONName: "hosts", JSONValue: []core.ParallelsHostConfig(nil), TextName: "hosts", TextValue: "0"}}},
		{name: "raw", config: core.ParallelsConfig{Template: " Template reference ", Source: " Source reference ", SourceID: " SourceID reference ", SourceSnapshot: " SourceSnapshot reference ", SourceSnapshotID: " SourceSnapshotID reference ", CloneMode: " CloneMode reference ", Host: " Host reference ", HostUser: " HostUser reference ", HostKey: " HostKey reference ", VMRoot: " VMRoot reference ", User: " User reference ", WorkRoot: " WorkRoot reference ", SelectedHost: "internal-selected-host", StartupTimeout: -1500 * time.Millisecond}, fields: []core.ProviderConfigShowField{{JSONName: "template", JSONValue: " Template reference ", TextName: "template", TextValue: " Template reference "}, {JSONName: "source", JSONValue: " Source reference ", TextName: "source", TextValue: " Source reference "}, {JSONName: "sourceId", JSONValue: " SourceID reference ", TextName: "source_id", TextValue: " SourceID reference "}, {JSONName: "sourceSnapshot", JSONValue: " SourceSnapshot reference ", TextName: "snapshot", TextValue: " SourceSnapshot reference "}, {JSONName: "sourceSnapshotId", JSONValue: " SourceSnapshotID reference ", TextName: "snapshot_id", TextValue: " SourceSnapshotID reference "}, {JSONName: "cloneMode", JSONValue: " CloneMode reference ", TextName: "clone_mode", TextValue: " CloneMode reference "}, {JSONName: "host", JSONValue: " Host reference ", TextName: "host", TextValue: " Host reference "}, {JSONName: "hostUser", JSONValue: " HostUser reference "}, {JSONName: "hostKey", JSONValue: "configured"}, {JSONName: "vmRoot", JSONValue: " VMRoot reference "}, {JSONName: "user", JSONValue: " User reference ", TextName: "user", TextValue: " User reference "}, {JSONName: "workRoot", JSONValue: " WorkRoot reference ", TextName: "work_root", TextValue: " WorkRoot reference "}, {JSONName: "startupTimeout", JSONValue: "-1.5s", TextName: "startup_timeout", TextValue: "-1.5s"}, {JSONName: "templates", JSONValue: map[string]core.ParallelsTemplateConfig(nil), TextName: "templates", TextValue: "0"}, {JSONName: "hosts", JSONValue: []core.ParallelsHostConfig(nil), TextName: "hosts", TextValue: "0"}}},
		{name: "whitespace", config: core.ParallelsConfig{Template: " \t ", Source: " \t ", SourceID: " \t ", SourceSnapshot: " \t ", SourceSnapshotID: " \t ", CloneMode: " \t ", Host: " \t ", HostUser: " \t ", HostKey: " \t ", VMRoot: " \t ", User: " \t ", WorkRoot: " \t ", SelectedHost: "internal-selected-host", StartupTimeout: -1500 * time.Millisecond}, fields: []core.ProviderConfigShowField{{JSONName: "template", JSONValue: " \t ", TextName: "template", TextValue: " \t "}, {JSONName: "source", JSONValue: " \t ", TextName: "source", TextValue: " \t "}, {JSONName: "sourceId", JSONValue: " \t ", TextName: "source_id", TextValue: " \t "}, {JSONName: "sourceSnapshot", JSONValue: " \t ", TextName: "snapshot", TextValue: " \t "}, {JSONName: "sourceSnapshotId", JSONValue: " \t ", TextName: "snapshot_id", TextValue: " \t "}, {JSONName: "cloneMode", JSONValue: " \t ", TextName: "clone_mode", TextValue: " \t "}, {JSONName: "host", JSONValue: " \t ", TextName: "host", TextValue: " \t "}, {JSONName: "hostUser", JSONValue: " \t "}, {JSONName: "hostKey", JSONValue: "configured"}, {JSONName: "vmRoot", JSONValue: " \t "}, {JSONName: "user", JSONValue: " \t ", TextName: "user", TextValue: " \t "}, {JSONName: "workRoot", JSONValue: " \t ", TextName: "work_root", TextValue: " \t "}, {JSONName: "startupTimeout", JSONValue: "-1.5s", TextName: "startup_timeout", TextValue: "-1.5s"}, {JSONName: "templates", JSONValue: map[string]core.ParallelsTemplateConfig(nil), TextName: "templates", TextValue: "0"}, {JSONName: "hosts", JSONValue: []core.ParallelsHostConfig(nil), TextName: "hosts", TextValue: "0"}}}} {
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
			if !empty {
				cfg.Parallels.Templates["raw template key"] = core.ParallelsTemplateConfig{Source: "source", SourceID: "source-id", SourceSnapshot: "snapshot", SourceSnapshotID: "snapshot-id", TargetOS: "linux", WindowsMode: "normal", CloneMode: "linked", Host: "host", HostUser: "host-user", HostKey: "loaded-template-key", VMRoot: "/vm", User: "user", WorkRoot: "/work"}
				first := core.ParallelsHostConfig{Name: "first", Host: "first-host", User: "host-user", Key: "loaded-host-key", VMRoot: "/vm", Targets: []string{"linux", "linux", "macos"}, MaxVMs: 3}
				second := core.ParallelsHostConfig{Name: "second", Host: "second-host", Targets: []string{}, MaxVMs: 0}
				cfg.Parallels.Hosts = []core.ParallelsHostConfig{first, second, first}
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
			if len(values) != 15 || len(texts) != 12 {
				t.Fatalf("field counts=%d/%d", len(values), len(texts))
			}
			for _, omitted := range []string{"hostUser", "hostKey", "vmRoot", "selectedHost", "SelectedHost"} {
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
	)
	cfg := core.Config{}
	cfg.Parallels.HostKey = topLevelKey
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
		for _, secret := range []string{topLevelKey, templateKey, hostKey} {
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

	if cfg.Parallels.HostKey != topLevelKey || cfg.Parallels.Templates["macos"].HostKey != templateKey || cfg.Parallels.Hosts[0].Key != hostKey {
		t.Fatal("config-show redaction mutated the effective Parallels config")
	}
	if redactedParallelsTemplateConfigs(nil) != nil || redactedParallelsHostConfigs(nil) != nil {
		t.Fatal("config-show redaction changed nil Parallels collections")
	}
}
