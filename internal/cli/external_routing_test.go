package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExternalRoutingRoundTripUsesPrivateHashedPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := ExternalConfig{
		Command:  "node",
		Args:     []string{"/tmp/provider.mjs", "--token", "secret-arg"},
		Config:   map[string]any{"token": "secret-config"},
		WorkRoot: "/workspaces/crabbox",
	}
	path, err := PersistExternalRouting("../unsafe/lease", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
	if path == "" || path[len(path)-5:] != ".json" {
		t.Fatalf("path=%q", path)
	}
	loaded, err := LoadExternalRouting(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Command != cfg.Command || len(loaded.Args) != 3 || loaded.Config["token"] != "secret-config" || loaded.WorkRoot != cfg.WorkRoot {
		t.Fatalf("loaded=%#v", loaded)
	}
	RemoveExternalRouting("../unsafe/lease")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("routing file still exists: %v", err)
	}
}

func TestDeclarativeExternalRoutingRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := ExternalConfig{
		Config: map[string]any{"size": "cpu16"},
		Lifecycle: ExternalLifecycleConfig{
			Acquire: ExternalLifecycleOperation{
				Steps: [][]string{
					{"devboxctl", "new", "{{name}}"},
					{"devboxctl", "setup", "{{name}}"},
				},
				RollbackOnFailure: true,
			},
			List: ExternalLifecycleOperation{
				Argv:   []string{"devboxctl", "list", "--format", "json"},
				Output: "json-name-array",
			},
			Release: ExternalLifecycleOperation{Argv: []string{"devboxctl", "rm", "{{name}}"}},
		},
		Connection: ExternalConnectionConfig{
			SSH: ExternalSSHConnectionConfig{
				User:           "{{env.DEVBOX_USER}}",
				Host:           "{{name}}",
				SSHConfigProxy: true,
			},
		},
		WorkRoot: "/home/developer/crabbox",
	}
	path, err := PersistExternalRouting("cbx_abcdef123456", cfg)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadExternalRouting(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Lifecycle.Acquire.Steps; len(got) != 2 ||
		len(got[0]) != 3 || got[0][0] != "devboxctl" || got[0][2] != "{{name}}" ||
		len(got[1]) != 3 || got[1][1] != "setup" ||
		!loaded.Lifecycle.Acquire.RollbackOnFailure {
		t.Fatalf("acquire=%#v", loaded.Lifecycle.Acquire)
	}
	if loaded.Connection.SSH.User != "{{env.DEVBOX_USER}}" || !loaded.Connection.SSH.SSHConfigProxy {
		t.Fatalf("connection=%#v", loaded.Connection)
	}
}

func TestLoadExternalRoutingRejectsBroadPermissions(t *testing.T) {
	path := t.TempDir() + "/routing.json"
	if err := os.WriteFile(path, []byte(`{"command":"provider"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm()&0o077 == 0 {
		t.Skipf("test process umask created a private file mode=%o", info.Mode().Perm())
	}
	if _, err := LoadExternalRouting(path); err == nil {
		t.Fatal("expected insecure routing file rejection")
	}
}

func TestAutoRouteExternalLeaseUsesPersistedClaimRouting(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	leaseID := "cbx_abcdef123456"
	oldRouting := ExternalConfig{
		Command:  "old-provider",
		Args:     []string{"release"},
		WorkRoot: "/old/work",
	}
	wantPath, err := PersistExternalRouting(leaseID, oldRouting)
	if err != nil {
		t.Fatal(err)
	}
	if err := claimLeaseForRepoProviderScope(
		leaseID,
		"old-box",
		"external",
		"old-scope",
		root,
		time.Minute,
		false,
	); err != nil {
		t.Fatal(err)
	}

	cfg := baseConfig()
	cfg.Provider = "external"
	cfg.External = ExternalConfig{Command: "new-provider", WorkRoot: "/new/work"}
	cfg.TargetOS = targetWindows
	cfg.WindowsMode = windowsModeWSL2
	fs := newFlagSet("test", os.Stderr)
	if err := autoRouteExternalLease(&cfg, fs, "old-box"); err != nil {
		t.Fatal(err)
	}
	if cfg.External.RoutingFile != wantPath {
		t.Fatalf("routing file=%q, want %q", cfg.External.RoutingFile, wantPath)
	}
	if cfg.External.Command != oldRouting.Command || cfg.External.WorkRoot != oldRouting.WorkRoot || cfg.WorkRoot != oldRouting.WorkRoot {
		t.Fatalf("config=%#v", cfg)
	}
	if cfg.TargetOS != targetLinux || cfg.WindowsMode != windowsModeNormal {
		t.Fatalf("target=%s windows-mode=%s", cfg.TargetOS, cfg.WindowsMode)
	}
}

func TestAutoRouteExternalLeaseRejectsAmbiguousAlias(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	for _, leaseID := range []string{"cbx_111111111111", "cbx_222222222222"} {
		if _, err := PersistExternalRouting(leaseID, ExternalConfig{Command: "provider", WorkRoot: "/work/crabbox"}); err != nil {
			t.Fatal(err)
		}
		if err := claimLeaseForRepoProviderScope(leaseID, "shared-alias", "external", leaseID, root, time.Minute, false); err != nil {
			t.Fatal(err)
		}
	}
	exact := baseConfig()
	exactFS := newFlagSet("test", os.Stderr)
	if err := autoRouteExternalLease(&exact, exactFS, "cbx_111111111111"); err != nil {
		t.Fatalf("exact lease id should remain authoritative: %v", err)
	}
	cfg := baseConfig()
	fs := newFlagSet("test", os.Stderr)
	err := autoRouteExternalLease(&cfg, fs, "shared-alias")
	if err == nil || !strings.Contains(err.Error(), "multiple lease claims") {
		t.Fatalf("err=%v", err)
	}
	selected := baseConfig()
	selected.Provider = "external"
	selectedFS := newFlagSet("test", os.Stderr)
	if err := autoRouteExternalLease(&selected, selectedFS, "shared-alias"); err != nil {
		t.Fatalf("configured external scope should resolve duplicate aliases: %v", err)
	}
	if selected.External.RoutingFile != "" {
		t.Fatalf("duplicate alias should defer to configured scope, routing=%q", selected.External.RoutingFile)
	}
}

func TestAutoRouteExternalLeaseRejectsCrossProviderAliasCollision(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	if _, err := PersistExternalRouting("cbx_111111111111", ExternalConfig{Command: "provider", WorkRoot: "/work/crabbox"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		leaseID  string
		provider string
	}{{"cbx_111111111111", "external"}, {"cbx_222222222222", "aws"}} {
		if err := claimLeaseForRepoProviderScope(tc.leaseID, "shared-alias", tc.provider, tc.leaseID, root, time.Minute, false); err != nil {
			t.Fatal(err)
		}
	}
	cfg := baseConfig()
	fs := newFlagSet("test", os.Stderr)
	if err := autoRouteExternalLease(&cfg, fs, "shared-alias"); err == nil || !strings.Contains(err.Error(), "multiple lease claims") {
		t.Fatalf("err=%v", err)
	}

	explicit := baseConfig()
	explicit.Provider = "external"
	explicitFS := newFlagSet("test", os.Stderr)
	explicitFS.String("provider", "external", "")
	if err := explicitFS.Set("provider", "external"); err != nil {
		t.Fatal(err)
	}
	if err := autoRouteExternalLease(&explicit, explicitFS, "shared-alias"); err != nil {
		t.Fatalf("explicit external provider should disambiguate: %v", err)
	}
}

func TestAutoRouteExternalLeaseHonorsConfiguredRoutingFile(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	var selectedPath string
	for _, leaseID := range []string{"cbx_111111111111", "cbx_222222222222"} {
		path, err := PersistExternalRouting(leaseID, ExternalConfig{Command: leaseID, WorkRoot: "/work/crabbox"})
		if err != nil {
			t.Fatal(err)
		}
		if selectedPath == "" {
			selectedPath = path
		}
		if err := claimLeaseForRepoProviderScope(leaseID, "shared-alias", "external", leaseID, root, time.Minute, false); err != nil {
			t.Fatal(err)
		}
	}
	cfg := baseConfig()
	cfg.Provider = "external"
	cfg.External.RoutingFile = selectedPath
	fs := newFlagSet("test", os.Stderr)
	if err := autoRouteExternalLease(&cfg, fs, "shared-alias"); err != nil {
		t.Fatal(err)
	}
	if cfg.External.RoutingFile != selectedPath {
		t.Fatalf("routing file=%q, want %q", cfg.External.RoutingFile, selectedPath)
	}
}

func TestRunExistingExternalLeaseLoadsPersistedRoutingBeforeValidation(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	leaseID := "cbx_abcdef123456"
	oldRouting := ExternalConfig{Command: "old-provider", WorkRoot: "/old/work"}
	if _, err := PersistExternalRouting(leaseID, oldRouting); err != nil {
		t.Fatal(err)
	}
	if err := claimLeaseForRepoProviderScope(leaseID, "old-box", "external", "old-scope", root, time.Minute, false); err != nil {
		t.Fatal(err)
	}

	defaults := baseConfig()
	defaults.Provider = "external"
	defaults.External = ExternalConfig{WorkRoot: "/new/work"}
	fs := newFlagSet("run", os.Stderr)
	values := registerLeaseCreateFlags(fs, defaults)
	if err := parseFlags(fs, nil); err != nil {
		t.Fatal(err)
	}
	cfg := defaults
	if err := applyLeaseCreateFlagsForLease(&cfg, fs, values, "old-box"); err != nil {
		t.Fatal(err)
	}
	if cfg.External.Command != oldRouting.Command || cfg.WorkRoot != oldRouting.WorkRoot {
		t.Fatalf("config=%#v", cfg)
	}
}

func TestResolveLeaseTargetUsesPersistedExternalRouting(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	leaseID := "cbx_abcdef123456"
	oldRouting := ExternalConfig{Command: "old-provider", WorkRoot: "/old/work"}
	if _, err := PersistExternalRouting(leaseID, oldRouting); err != nil {
		t.Fatal(err)
	}
	if err := claimLeaseForRepoProviderScope(leaseID, "old-box", "external", "old-scope", root, time.Minute, false); err != nil {
		t.Fatal(err)
	}
	cfg := baseConfig()
	app := App{Stdout: os.Stdout, Stderr: os.Stderr}
	server, _, gotLeaseID, err := app.resolveLeaseTargetWithConfig(context.Background(), &cfg, "old-box")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "external" || cfg.External.Command != oldRouting.Command || server.Name != oldRouting.Command || gotLeaseID != "old-box" {
		t.Fatalf("config=%#v server=%#v lease=%q", cfg, server, gotLeaseID)
	}
}

func TestLeaseTargetConfigPreservesExplicitNonExternalProvider(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	leaseID := "cbx_abcdef123456"
	if _, err := PersistExternalRouting(leaseID, ExternalConfig{Command: "old-provider", WorkRoot: "/old/work"}); err != nil {
		t.Fatal(err)
	}
	if err := claimLeaseForRepoProviderScope(leaseID, "old-box", "external", "old-scope", root, time.Minute, false); err != nil {
		t.Fatal(err)
	}
	defaults := baseConfig()
	fs := newFlagSet("code", os.Stderr)
	provider := fs.String("provider", defaults.Provider, "")
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	if err := parseFlags(fs, []string{"--provider", "aws"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{LeaseID: "old-box"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "aws" || cfg.External.RoutingFile != "" || !cfg.providerExplicit {
		t.Fatalf("config=%#v", cfg)
	}
}
