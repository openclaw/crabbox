package cli

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setExternalRoutingTestHome(t *testing.T) string {
	t.Helper()
	root := privateExternalRoutingTempDir(t)
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	return root
}

func privateExternalRoutingTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// testing.TempDir creates its numbered child with 0777 before umask.
	// Keep security-sensitive routing fixtures deterministic under umask 000.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("secure external routing temp directory: %v", err)
	}
	return dir
}

func TestExternalRoutingPathHonorsAbsoluteXDGConfigHome(t *testing.T) {
	root := t.TempDir()
	xdgDir := filepath.Join(root, "xdg-config")
	t.Setenv("HOME", filepath.Join(root, "different-home"))
	t.Setenv("XDG_CONFIG_HOME", xdgDir)

	path, err := ExternalRoutingPath("cbx_abcdef123456")
	if err != nil {
		t.Fatal(err)
	}
	wantDir := filepath.Join(xdgDir, "crabbox", "external")
	if filepath.Dir(path) != wantDir {
		t.Fatalf("routing directory=%q, want %q", filepath.Dir(path), wantDir)
	}
}

func TestExternalRoutingPathRejectsInvalidXDGConfigHome(t *testing.T) {
	for _, dir := range []string{
		"relative/config",
		" " + filepath.Join(t.TempDir(), "config"),
		filepath.Join(t.TempDir(), "config") + " ",
	} {
		t.Run(dir, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", dir)
			if _, err := ExternalRoutingPath("cbx_abcdef123456"); err == nil || !strings.Contains(err.Error(), "XDG_CONFIG_HOME must be an absolute path") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestWriteExternalRoutingAtomicSyncsBeforeRenameAndThroughDirectoryChain(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new", "routing", "chain")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "route.json")
	events := []string{}
	err := writeExternalRoutingAtomic(
		path,
		[]byte("{}\n"),
		func(file *os.File) error {
			events = append(events, "file")
			return file.Sync()
		},
		func(from, to string) error {
			events = append(events, "rename")
			return os.Rename(from, to)
		},
		func(syncPath string) error {
			events = append(events, "dir:"+filepath.Clean(syncPath))
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 3 || events[0] != "file" || events[1] != "rename" {
		t.Fatalf("persistence order=%v, want file sync then rename", events)
	}
	current := filepath.Clean(dir)
	for i := 2; ; i++ {
		if i >= len(events) || events[i] != "dir:"+current {
			t.Fatalf("persistence order=%v, want directory %q at index %d", events, current, i)
		}
		parent := filepath.Dir(current)
		if parent == current {
			if i != len(events)-1 {
				t.Fatalf("persistence order has extra events after filesystem root: %v", events)
			}
			break
		}
		current = parent
	}
}

func TestWriteExternalRoutingAtomicDoesNotRenameUnsyncedTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "route.json")
	syncErr := errors.New("file sync unavailable")
	renameCalled := false
	err := writeExternalRoutingAtomic(
		path,
		[]byte("{}\n"),
		func(*os.File) error { return syncErr },
		func(string, string) error {
			renameCalled = true
			return nil
		},
		func(string) error { return nil },
	)
	if !errors.Is(err, syncErr) || !strings.Contains(err.Error(), "sync external routing file") {
		t.Fatalf("error=%v, want file sync failure", err)
	}
	if renameCalled {
		t.Fatal("rename ran after temp file sync failure")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("routing file exists after temp sync failure: %v", err)
	}
}

func TestWriteExternalRoutingAtomicRetriesExistingAncestorChainAfterSyncFailure(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "first", "second")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "route.json")
	failedAncestor := filepath.Join(base, "first")
	syncErr := errors.New("ancestor sync unavailable")
	err := writeExternalRoutingAtomic(
		path,
		[]byte("{\"command\":\"provider\"}\n"),
		func(file *os.File) error { return file.Sync() },
		os.Rename,
		func(syncPath string) error {
			if filepath.Clean(syncPath) == filepath.Clean(failedAncestor) {
				return syncErr
			}
			return nil
		},
	)
	if !errors.Is(err, syncErr) {
		t.Fatalf("error=%v, want ancestor sync failure", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("installed routing file missing after directory sync failure: %v", statErr)
	}

	calls := map[string]int{}
	err = writeExternalRoutingAtomic(
		path,
		[]byte("{\"command\":\"provider\"}\n"),
		func(file *os.File) error { return file.Sync() },
		os.Rename,
		func(syncPath string) error {
			calls[filepath.Clean(syncPath)]++
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls[filepath.Clean(dir)] != 1 || calls[filepath.Clean(failedAncestor)] != 1 {
		t.Fatalf("retry did not sync installed directory and existing ancestor chain: %v", calls)
	}
	root := filepath.VolumeName(path) + string(os.PathSeparator)
	if calls[filepath.Clean(root)] != 1 {
		t.Fatalf("retry did not sync chain through filesystem root: %v", calls)
	}
}

func TestConfirmedAbsentRoutingRemovalRequiresDirectorySyncAndRetriesAfterDeletion(t *testing.T) {
	setExternalRoutingTestHome(t)
	const leaseID = "cbx_123456789abc"
	cfg := ExternalConfig{Command: "provider"}
	path, err := PersistExternalRouting(leaseID, cfg)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := LoadExternalRouting(path)
	if err != nil {
		t.Fatal(err)
	}
	syncErr := errors.New("routing directory sync unavailable")
	err = removeExternalRoutingIfUnchangedWithSync(leaseID, expected, func(string) error { return syncErr })
	if err == nil || !strings.Contains(err.Error(), syncErr.Error()) {
		t.Fatalf("routing removal error=%v", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("routing file remains after removal sync failure: %v", statErr)
	}
	var synced string
	if err := removeExternalRoutingIfUnchangedWithSync(leaseID, expected, func(dir string) error {
		synced = filepath.Clean(dir)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if synced != filepath.Clean(filepath.Dir(path)) {
		t.Fatalf("retry synced %q want %q", synced, filepath.Dir(path))
	}
}

func TestRemoveExternalRoutingIfUnchangedPreservesDesktopRouteChange(t *testing.T) {
	setExternalRoutingTestHome(t)
	const leaseID = "cbx_desktop_runtime_123456"
	stored := ExternalConfig{
		Command:  "provider-adapter",
		WorkRoot: "/work/crabbox",
		Connection: ExternalConnectionConfig{
			SSH: ExternalSSHConnectionConfig{User: "runner", Host: "example.internal"},
			Desktop: ExternalDesktopConfig{
				Username:    "stored-screen-user",
				PasswordEnv: "STORED_SCREEN_PASSWORD",
			},
		},
	}
	SetExternalRoutingTarget(&stored, targetMacOS, windowsModeNormal)
	path, err := PersistExternalRouting(leaseID, stored)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadExternalRouting(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Connection.Desktop != stored.Connection.Desktop {
		t.Fatalf("stored desktop route changed: got=%#v want=%#v", loaded.Connection.Desktop, stored.Connection.Desktop)
	}

	expected := loaded
	expected.Connection.Desktop = ExternalDesktopConfig{
		Username:    "runtime-screen-user",
		PasswordEnv: "RUNTIME_SCREEN_PASSWORD",
	}
	if err := RemoveExternalRoutingIfUnchanged(leaseID, expected); err == nil || !strings.Contains(err.Error(), "external routing state changed") {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("desktop route change was removed: %v", err)
	}
}

func TestLoadExternalRoutingWithDigestBindsExactGeneration(t *testing.T) {
	setExternalRoutingTestHome(t)
	const leaseID = "cbx_digest_route_123456"
	path, err := PersistValidatedExternalRouting(leaseID, ExternalConfig{Command: "first-provider", WorkRoot: "/work/crabbox"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := LoadExternalRouting(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := ExternalRoutingDigest(first)
	if len(digest) != sha256.Size*2 {
		t.Fatalf("digest=%q", digest)
	}
	if _, err := PersistValidatedExternalRouting(leaseID, ExternalConfig{Command: "replacement-provider", WorkRoot: "/work/crabbox"}); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadExternalRoutingWithDigest(path, digest); err == nil || !strings.Contains(err.Error(), "generation changed") {
		t.Fatalf("err=%v", err)
	}
}

func TestPersistExternalRoutingPreservesOnlyCurrentLoadedGeneration(t *testing.T) {
	setExternalRoutingTestHome(t)
	const leaseID = "cbx_generation_route_123456"
	base := ExternalConfig{Command: "provider", WorkRoot: "/work/crabbox"}
	path, err := PersistValidatedExternalRouting(leaseID, base)
	if err != nil {
		t.Fatal(err)
	}
	first, err := LoadExternalRouting(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PersistValidatedExternalRouting(leaseID, first); err != nil {
		t.Fatal(err)
	}
	restored, err := LoadExternalRouting(path)
	if err != nil {
		t.Fatal(err)
	}
	if ExternalRoutingGeneration(restored) != ExternalRoutingGeneration(first) || ExternalRoutingDigest(restored) != ExternalRoutingDigest(first) {
		t.Fatalf("unchanged loaded route changed generation: first=%s/%s restored=%s/%s", ExternalRoutingGeneration(first), ExternalRoutingDigest(first), ExternalRoutingGeneration(restored), ExternalRoutingDigest(restored))
	}

	// A reacquire/rewrite without the loaded generation is a distinct route,
	// even when every semantic field is identical.
	if _, err := PersistValidatedExternalRouting(leaseID, base); err != nil {
		t.Fatal(err)
	}
	rewritten, err := LoadExternalRouting(path)
	if err != nil {
		t.Fatal(err)
	}
	if ExternalRoutingGeneration(rewritten) == ExternalRoutingGeneration(first) || ExternalRoutingDigest(rewritten) == ExternalRoutingDigest(first) {
		t.Fatalf("identical rewrite reused stale generation: first=%s/%s rewritten=%s/%s", ExternalRoutingGeneration(first), ExternalRoutingDigest(first), ExternalRoutingGeneration(rewritten), ExternalRoutingDigest(rewritten))
	}
	if err := RemoveExternalRoutingIfUnchanged(leaseID, first); err == nil || !strings.Contains(err.Error(), "generation changed") {
		t.Fatalf("stale cleanup err=%v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stale cleanup removed rewritten route: %v", err)
	}
}

func TestRemoveExternalRoutingIfUnchangedPreservesChangedRoute(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*ExternalConfig)
	}{
		{
			name: "work root",
			mutate: func(cfg *ExternalConfig) {
				cfg.WorkRoot = "/replacement/work"
			},
		},
		{
			name: "target",
			mutate: func(cfg *ExternalConfig) {
				SetExternalRoutingTarget(cfg, targetWindows, windowsModeWSL2)
			},
		},
		{
			name: "provider command",
			mutate: func(cfg *ExternalConfig) {
				cfg.Command = "replacement-provider-adapter"
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			setExternalRoutingTestHome(t)
			const leaseID = "cbx_changed_route_123456"
			stored := ExternalConfig{
				Command:  "provider-adapter",
				WorkRoot: "/work/crabbox",
				Connection: ExternalConnectionConfig{
					Desktop: ExternalDesktopConfig{Username: "stored-user", PasswordEnv: "STORED_PASSWORD"},
				},
			}
			SetExternalRoutingTarget(&stored, targetMacOS, windowsModeNormal)
			path, err := PersistExternalRouting(leaseID, stored)
			if err != nil {
				t.Fatal(err)
			}
			expected, err := LoadExternalRouting(path)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(&expected)
			err = RemoveExternalRoutingIfUnchanged(leaseID, expected)
			if err == nil || !strings.Contains(err.Error(), "external routing state changed") {
				t.Fatalf("err=%v", err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("changed routing state was not preserved: %v", err)
			}
			loaded, err := LoadExternalRouting(path)
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Command != stored.Command || loaded.WorkRoot != stored.WorkRoot || loaded.Connection.Desktop != stored.Connection.Desktop {
				t.Fatalf("stored route mutated after refused CAS cleanup: %#v", loaded)
			}
			if targetOS, windowsMode := ExternalRoutingTarget(loaded); targetOS != targetMacOS || windowsMode != windowsModeNormal {
				t.Fatalf("stored target changed after refused CAS cleanup: target=%s windows-mode=%s", targetOS, windowsMode)
			}
		})
	}
}

func TestExternalRoutingRoundTripUsesPrivateHashedPath(t *testing.T) {
	setExternalRoutingTestHome(t)
	cfg := ExternalConfig{
		Command:      "node",
		Args:         []string{"/tmp/provider.mjs", "--token", "secret-arg"},
		Config:       map[string]any{"token": "secret-config"},
		Capabilities: ExternalCapabilitiesConfig{IdempotentLeaseID: true},
		WorkRoot:     "/workspaces/crabbox",
	}
	SetExternalRoutingTarget(&cfg, targetMacOS, windowsModeNormal)
	SetExternalRoutingArchitecture(&cfg, ArchitectureARM64)
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
	if loaded.Command != cfg.Command || len(loaded.Args) != 3 || loaded.Config["token"] != "secret-config" || !loaded.Capabilities.IdempotentLeaseID || loaded.WorkRoot != cfg.WorkRoot {
		t.Fatalf("loaded=%#v", loaded)
	}
	if targetOS, windowsMode := ExternalRoutingTarget(loaded); targetOS != targetMacOS || windowsMode != windowsModeNormal {
		t.Fatalf("routing target=%s windows-mode=%s", targetOS, windowsMode)
	}
	if architecture := ExternalRoutingArchitecture(loaded); architecture != ArchitectureARM64 {
		t.Fatalf("routing architecture=%s", architecture)
	}
	RemoveExternalRouting("../unsafe/lease")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("routing file still exists: %v", err)
	}
}

func TestRestoreExternalLeaseTargetPreservesPersistedPlatformDefaultWorkRoot(t *testing.T) {
	setExternalRoutingTestHome(t)
	const leaseID = "cbx_routed_root_123456"
	routing := ExternalConfig{Command: "provider-adapter", WorkRoot: defaultPOSIXWorkRoot}
	SetExternalRoutingTarget(&routing, targetMacOS, windowsModeNormal)
	path, err := PersistExternalRouting(leaseID, routing)
	if err != nil {
		t.Fatal(err)
	}
	cfg := baseConfig()
	cfg.Provider = "external"
	if err := loadExternalRoutingConfig(&cfg, path, true); err != nil {
		t.Fatal(err)
	}
	if err := restoreExternalLeaseTarget(&cfg, false, false); err != nil {
		t.Fatal(err)
	}

	if cfg.TargetOS != targetMacOS || cfg.WorkRoot != defaultPOSIXWorkRoot {
		t.Fatalf("restored config target=%q workRoot=%q", cfg.TargetOS, cfg.WorkRoot)
	}
}

func TestDeclarativeExternalRoutingRoundTrip(t *testing.T) {
	setExternalRoutingTestHome(t)
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
	root := setExternalRoutingTestHome(t)
	leaseID := "cbx_abcdef123456"
	oldRouting := ExternalConfig{
		Command:  "old-provider",
		Args:     []string{"release"},
		WorkRoot: "/old/work",
	}
	SetExternalRoutingTarget(&oldRouting, targetMacOS, windowsModeNormal)
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
	cfg.targetExplicit = true
	cfg.WindowsMode = windowsModeWSL2
	cfg.explicitWindowsMode = windowsModeWSL2
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
	if cfg.TargetOS != targetMacOS || cfg.WindowsMode != windowsModeNormal {
		t.Fatalf("target=%s windows-mode=%s", cfg.TargetOS, cfg.WindowsMode)
	}
}

func TestAutoRouteExternalLeaseRejectsAmbiguousAlias(t *testing.T) {
	root := setExternalRoutingTestHome(t)
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
	root := setExternalRoutingTestHome(t)
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

func TestAutoRouteExternalLeaseFailsClosedWithoutRoutingState(t *testing.T) {
	root := setExternalRoutingTestHome(t)
	leaseID := "cbx_abcdef123456"
	if err := claimLeaseForRepoProviderScope(leaseID, "old-box", "external", "old-scope", root, time.Minute, false); err != nil {
		t.Fatal(err)
	}
	cfg := baseConfig()
	fs := newFlagSet("test", os.Stderr)
	if err := autoRouteExternalLease(&cfg, fs, "old-box"); err == nil || !strings.Contains(err.Error(), "routing state is missing") {
		t.Fatalf("err=%v", err)
	}
	selected := baseConfig()
	selected.Provider = "external"
	selected.External = ExternalConfig{Command: "current-provider", WorkRoot: "/work/crabbox"}
	if err := routeExternalLeaseClaim(&selected, leaseID); err == nil || !strings.Contains(err.Error(), "refusing unverified cleanup") {
		t.Fatalf("forced route err=%v", err)
	}
}

func TestTargetLinuxClearsAmbientWindowsMode(t *testing.T) {
	cfg := baseConfig()
	cfg.TargetOS = targetWindows
	cfg.WindowsMode = windowsModeWSL2
	cfg.explicitWindowsMode = windowsModeWSL2
	fs := newFlagSet("test", os.Stderr)
	flags := registerTargetFlags(fs, cfg)
	if err := parseFlags(fs, []string{"--target", "linux"}); err != nil {
		t.Fatal(err)
	}
	if err := applyTargetFlagOverrides(&cfg, fs, flags); err != nil {
		t.Fatal(err)
	}
	if cfg.TargetOS != targetLinux || cfg.WindowsMode != windowsModeNormal || cfg.explicitWindowsMode != "" {
		t.Fatalf("config=%#v", cfg)
	}
}

func TestExplicitExternalRoutingRestoresLinuxTarget(t *testing.T) {
	cfg := baseConfig()
	cfg.TargetOS = targetWindows
	cfg.WindowsMode = windowsModeWSL2
	fs := newFlagSet("test", os.Stderr)
	provider := fs.String("provider", cfg.Provider, "")
	fs.String("external-routing-file", "", "")
	if err := parseFlags(fs, []string{"--provider", "external", "--external-routing-file", "/tmp/route.json"}); err != nil {
		t.Fatal(err)
	}
	cfg.Provider = *provider
	if err := autoRouteExternalLease(&cfg, fs, "old-box"); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "external" || cfg.TargetOS != targetLinux || cfg.WindowsMode != windowsModeNormal {
		t.Fatalf("config=%#v", cfg)
	}
}

func TestAutoRouteExternalLeaseHonorsConfiguredRoutingFile(t *testing.T) {
	root := setExternalRoutingTestHome(t)
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
	t.Setenv("CRABBOX_EXTERNAL_DESKTOP_USERNAME", "runtime-user")
	t.Setenv("CRABBOX_EXTERNAL_DESKTOP_PASSWORD_ENV", "RUNTIME_DESKTOP_PASSWORD")
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
	if cfg.External.Command != "cbx_111111111111" || !cfg.External.routingLoaded {
		t.Fatalf("configured routing was not loaded: %#v", cfg.External)
	}
	if cfg.External.Connection.Desktop.Username != "runtime-user" || cfg.External.Connection.Desktop.PasswordEnv != "RUNTIME_DESKTOP_PASSWORD" {
		t.Fatalf("desktop environment overrides were lost: %#v", cfg.External.Connection.Desktop)
	}
}

func TestRestoreExternalLeaseTargetKeepsNormalModeForExplicitLinux(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "external"
	cfg.TargetOS = targetLinux
	cfg.WindowsMode = windowsModeNormal
	SetExternalRoutingTarget(&cfg.External, targetWindows, windowsModeWSL2)
	if err := restoreExternalLeaseTarget(&cfg, true, false); err != nil {
		t.Fatal(err)
	}
	if cfg.TargetOS != targetLinux || cfg.WindowsMode != windowsModeNormal {
		t.Fatalf("target=%q windows-mode=%q", cfg.TargetOS, cfg.WindowsMode)
	}
}

func TestRouteExternalLeaseClaimOverridesAmbientRouting(t *testing.T) {
	root := setExternalRoutingTestHome(t)
	paths := map[string]string{}
	for _, leaseID := range []string{"cbx_111111111111", "cbx_222222222222"} {
		path, err := PersistExternalRouting(leaseID, ExternalConfig{Command: leaseID, WorkRoot: "/work/crabbox"})
		if err != nil {
			t.Fatal(err)
		}
		paths[leaseID] = path
		if err := claimLeaseForRepoProviderScope(leaseID, leaseID, "external", leaseID, root, time.Minute, false); err != nil {
			t.Fatal(err)
		}
	}
	ambient, err := LoadExternalRouting(paths["cbx_111111111111"])
	if err != nil {
		t.Fatal(err)
	}
	cfg := baseConfig()
	cfg.Provider = "external"
	cfg.External = ambient
	if err := routeExternalLeaseClaim(&cfg, "cbx_222222222222"); err != nil {
		t.Fatal(err)
	}
	if cfg.External.RoutingFile != paths["cbx_222222222222"] || cfg.External.Command != "cbx_222222222222" {
		t.Fatalf("config=%#v", cfg)
	}
}

func TestRunExistingExternalLeaseLoadsPersistedRoutingBeforeValidation(t *testing.T) {
	root := setExternalRoutingTestHome(t)
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
	root := setExternalRoutingTestHome(t)
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
	server, _, gotLeaseID, err := app.resolveLeaseTargetWithRequestConfig(context.Background(), &cfg, ResolveRequest{ID: "old-box"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "external" || cfg.External.Command != oldRouting.Command || server.Name != oldRouting.Command || gotLeaseID != "old-box" {
		t.Fatalf("config=%#v server=%#v lease=%q", cfg, server, gotLeaseID)
	}
}

func TestLeaseTargetConfigPreservesExplicitNonExternalProvider(t *testing.T) {
	root := setExternalRoutingTestHome(t)
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
