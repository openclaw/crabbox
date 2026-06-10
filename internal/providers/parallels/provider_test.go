package parallels

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
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
