package parallels

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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

func TestAcquireKeepsStoredKeyWhenRollbackDeleteFails(t *testing.T) {
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
	keyMatches := storedTestboxKeyMatches(t)
	if len(keyMatches) != 1 {
		t.Fatalf("stored keys=%v, want one preserved key after rollback delete failure", keyMatches)
	}
}

func storedTestboxKeyMatches(t *testing.T) []string {
	t.Helper()
	var patterns []string
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		patterns = append(patterns, filepath.Join(xdg, "crabbox", "testboxes", "*", "id_ed25519"))
	}
	if home := os.Getenv("HOME"); home != "" {
		patterns = append(patterns,
			filepath.Join(home, ".config", "crabbox", "testboxes", "*", "id_ed25519"),
			filepath.Join(home, "Library", "Application Support", "crabbox", "testboxes", "*", "id_ed25519"),
		)
	}
	var matches []string
	for _, pattern := range patterns {
		found, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		matches = append(matches, found...)
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
	if err := core.ClaimLeaseForRepoProvider(leaseID, "blue", "parallels", "/repo", time.Minute, false); err != nil {
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
