package lume

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type cmdRes = core.LocalCommandResult
type claim = core.LeaseClaim
type labels = map[string]string
type results = map[string]cmdRes

var bg = context.Background()
var join = filepath.Join

func base() core.Config { return core.BaseConfig() }

type fake struct {
	calls     []core.LocalCommandRequest
	responses results
	errors    map[string]error
	hook      func(core.LocalCommandRequest) (cmdRes, error, bool)
}

const hostKey = "AAAAC3NzaC1lZDI1NTE5AAAAIOCh4W5YA0Lp2pvT+yWIG/tC7BrQalNUIHSqfjYkJei6"

func want(t *testing.T, err error, text string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), text) {
		t.Fatalf("error=%v want %q", err, text)
	}
}

func putVM(t *testing.T, home, name, machineIdentifier string) {
	t.Helper()
	putVMAt(t, join(home, ".lume"), name, machineIdentifier)
}

func putVMAt(t *testing.T, root, name, machineIdentifier string) {
	t.Helper()
	dir := join(root, name)
	must(t, os.MkdirAll(dir, 0o700))
	data := fmt.Sprintf(`{"machineIdentifier":%q}`, machineIdentifier)
	must(t, os.WriteFile(join(dir, "config.json"), []byte(data), 0o600))
}

func writeLumeSettings(t *testing.T, home, settings string) {
	t.Helper()
	dir := join(home, ".config", "lume")
	t.Setenv("XDG_CONFIG_HOME", filepath.Dir(dir))
	must(t, os.MkdirAll(dir, 0o700))
	must(t, os.WriteFile(join(dir, "config.yaml"), []byte(settings), 0o600))
}

func writeLumeKnownHost(t *testing.T, leaseID, name, key string) {
	t.Helper()
	target := core.SSHTarget{}
	must(t, core.UseLeaseKnownHosts(&target, leaseID))
	must(t, os.WriteFile(target.KnownHostsFile, []byte(lumeHostKeyAlias(name)+" ssh-ed25519 "+key+"\n"), 0o600))
}

func (r *fake) Run(_ context.Context, req core.LocalCommandRequest) (cmdRes, error) {
	r.calls = append(r.calls, req)
	if r.hook != nil {
		if result, err, handled := r.hook(req); handled {
			return result, err
		}
	}
	key := strings.Join(req.Args, "\x00")
	if err, ok := r.errors[key]; ok {
		return r.responses[key], err
	}
	if result, ok := r.responses[key]; ok {
		return result, nil
	}
	if len(req.Args) > 0 {
		if result, ok := r.responses[req.Args[0]]; ok {
			return result, nil
		}
	}
	return cmdRes{}, nil
}

func applyTestFlags(t *testing.T, cfg core.Config, args ...string) (core.Config, error) {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := registerFlags(fs, cfg)
	must(t, fs.Parse(args))
	err := applyFlags(&cfg, fs, values)
	return cfg, err
}

func backendFor(cfg core.Config, runner core.CommandRunner) *backend {
	rt := core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}
	return newBackend((Provider{}).Spec(), cfg, rt).(*backend)
}

func configFor() core.Config {
	cfg := base()
	cfg.Provider = providerName
	return cfg
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestSpecAndAliases(t *testing.T) {
	p := Provider{}
	for _, alias := range []string{"lume", "local-lume", "lume-macos"} {
		got, err := core.ProviderFor(alias)
		if err != nil {
			t.Fatalf("ProviderFor(%q): %v", alias, err)
		}
		if got.Name() != providerName {
			t.Fatalf("ProviderFor(%q).Name=%q", alias, got.Name())
		}
	}
	spec := p.Spec()
	if spec.Kind != core.ProviderKindSSHLease || spec.Family != "local-vm" {
		t.Fatalf("unexpected spec: %#v", spec)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetMacOS {
		t.Fatalf("targets=%v want macos only", spec.Targets)
	}
	for _, feature := range []core.Feature{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup} {
		if !spec.Features.Has(feature) {
			t.Fatalf("features=%v missing %s", spec.Features, feature)
		}
	}
	if spec.Features.Has(core.FeatureDesktop) {
		t.Fatalf("desktop must remain disabled until Crabbox can bridge Lume's host-side VNC endpoint")
	}
}

func TestCleanupExpiry(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	server := Server{Status: "running", Labels: labels{
		"state":      "ready",
		"expires_at": core.LeaseLabelTime(now),
	}}
	if cleanup, reason := shouldCleanup(server, claim{}, now.Add(time.Second)); !cleanup || !strings.Contains(reason, "expired") {
		t.Fatalf("shouldCleanup=%v, %q; want expired ready lease cleanup", cleanup, reason)
	}
	if cleanup, reason := shouldCleanup(server, claim{}, now.Add(-time.Second)); cleanup {
		t.Fatalf("shouldCleanup=%v, %q; want unexpired ready lease retained", cleanup, reason)
	}
	server.Labels["expires_at"] = core.LeaseLabelTime(now.Add(time.Hour))
	claim := claim{LastUsedAt: now.Add(-14 * time.Hour).Format(time.RFC3339), IdleTimeoutSeconds: 3600}
	if cleanup, reason := shouldCleanup(server, claim, now); !cleanup || reason != "claim expired" {
		t.Fatalf("shouldCleanup=%v, %q; want stale claim fallback cleanup", cleanup, reason)
	}
	server.Labels["keep"] = "true"
	server.Labels["recovery"] = "rollback-failed"
	if cleanup, _ := shouldCleanup(server, claim, now); !cleanup {
		t.Fatal("rollback failure was retained")
	}
}

func TestCleanupKeepsStartup(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	server := Server{Status: "starting", Labels: labels{"state": "starting", "expires_at": core.LeaseLabelTime(now.Add(time.Hour))}}
	if cleanup, reason := shouldCleanup(server, claim{}, now); cleanup {
		t.Fatalf("active startup cleanup=%v reason=%q", cleanup, reason)
	}
	if cleanup, reason := shouldCleanup(server, claim{}, now.Add(2*time.Hour)); !cleanup || !strings.Contains(reason, "expired") {
		t.Fatalf("expired startup cleanup=%v reason=%q", cleanup, reason)
	}
	server.Status = "provisioning (stale)"
	if cleanup, reason := shouldCleanup(server, claim{}, now); !cleanup || reason != "provisioning stale" {
		t.Fatalf("stale startup cleanup=%v reason=%q", cleanup, reason)
	}
}

func TestDefaults(t *testing.T) {
	cfg := configFor()
	configured, err := (Provider{}).Configure(cfg, core.Runtime{})
	must(t, err)
	got := configured.(*backend).cfg
	if got.TargetOS != core.TargetMacOS || got.Lume.User != "lume" || got.Lume.Base != "crabbox-macos-golden" {
		t.Fatalf("unexpected defaults: %#v", got.Lume)
	}
	cfg = base()
	cfg.Lume.User = "builder"
	cfg.Lume.WorkRoot = "/Users/lume/crabbox"
	cfg.WorkRoot = "/Users/lume/crabbox"
	applyDefaults(&cfg)
	if cfg.Lume.WorkRoot != "/Users/builder/crabbox" || cfg.WorkRoot != "/Users/builder/crabbox" {
		t.Fatalf("work roots=%q %q want overridden user's home", cfg.Lume.WorkRoot, cfg.WorkRoot)
	}
}

func TestRejectsNonMacOS(t *testing.T) {
	cfg := base()
	cfg.Provider, cfg.TargetOS = providerName, core.TargetLinux
	core.MarkTargetExplicit(&cfg)
	cfg, err := applyTestFlags(t, cfg)
	if err != nil || cfg.TargetOS != core.TargetLinux {
		t.Fatalf("target=%q apply error=%v", cfg.TargetOS, err)
	}
	_, err = (Provider{}).Configure(cfg, core.Runtime{})
	want(t, err, "supports target=macos only")
}

func TestFlags(t *testing.T) {
	cfg := configFor()
	cfg, err := applyTestFlags(t, cfg,
		"--lume-cli", "/opt/lume/bin/lume",
		"--lume-base", "macos-xcode-golden",
		"--lume-storage", "external",
		"--lume-user", "builder",
		"--lume-work-root", "/Users/builder/work",
	)
	must(t, err)
	if cfg.Lume.CLIPath != "/opt/lume/bin/lume" || cfg.Lume.Base != "macos-xcode-golden" || cfg.Lume.Storage != "external" || cfg.Lume.User != "builder" || cfg.WorkRoot != "/Users/builder/work" {
		t.Fatalf("unexpected lume config: %#v workRoot=%q", cfg.Lume, cfg.WorkRoot)
	}
}

func TestDoctorStoppedBase(t *testing.T) {
	cfg := configFor()
	runner := &fake{responses: results{
		"--version": {Stdout: "0.3.16\n"},
		"ls":        {Stdout: `[{"name":"crabbox-macos-golden","os":"macOS","status":"stopped","locationName":"home"}]`},
	}}
	b := backendFor(cfg, runner)
	result, err := b.Doctor(bg, core.DoctorRequest{})
	must(t, err)
	if !strings.Contains(result.Message, "base_state=stopped") || !strings.Contains(result.Message, "runtime=0.3.16") {
		t.Fatalf("doctor message=%q", result.Message)
	}
	if !strings.Contains(result.Message, "leases=0") {
		t.Fatalf("base VM must not be counted as a lease: %q", result.Message)
	}

	runner.responses["ls"] = cmdRes{Stdout: `[{"name":"crabbox-macos-golden","os":"macOS","status":"running","locationName":"home"}]`}
	_, err = b.Doctor(bg, core.DoctorRequest{})
	want(t, err, "must be stopped")
}

func TestHostGuestCount(t *testing.T) {
	cfg := configFor()
	cfg.Lume.Storage = "external"
	runner := &fake{responses: results{
		"ls": {Stdout: `[
			{"name":"base","os":"macOS","status":"stopped"},
			{"name":"one","os":"macOS","status":"running"},
			{"name":"two","os":"macOS","status":"starting"},
			{"name":"stale","os":"macOS","status":"provisioning (stale)"},
			{"name":"linux","os":"Linux","status":"running"}
		]`},
	}}
	b := backendFor(cfg, runner)
	active, err := b.activeMacOSGuestCount(bg, b.configForRun())
	must(t, err)
	if active != 2 {
		t.Fatalf("active=%d want 2", active)
	}
	if len(runner.calls) != 1 || strings.Contains(strings.Join(runner.calls[0].Args, " "), "--storage") {
		t.Fatalf("capacity inventory must not be storage-filtered: %#v", runner.calls)
	}
}

func TestAcquireGuestLimit(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := configFor()
	runner := &fake{responses: results{
		"ls": {Stdout: `[
			{"name":"one","os":"macOS","status":"running"},
			{"name":"two","os":"macOS","status":"running"}
		]`},
	}}
	b := backendFor(cfg, runner)
	_, err := b.Acquire(bg, core.AcquireRequest{RequestedLeaseID: "cbx_capacity_test"})
	want(t, err, "2 of 2")
	for _, call := range runner.calls {
		if len(call.Args) > 0 && call.Args[0] == "clone" {
			t.Fatalf("clone ran after host capacity was exhausted: %#v", runner.calls)
		}
	}
}

func TestAmbiguousCloneRetainsVM(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", join(home, ".local", "state"))
	must(t, os.MkdirAll(join(home, ".lume"), 0o700))
	const leaseID = "cbx_clonefail1234"
	vmExists := false
	deleteCalled := false
	name := ""
	acquireCtx, cancelAcquire := context.WithCancel(bg)
	defer cancelAcquire()
	runner := &fake{}
	runner.hook = func(req core.LocalCommandRequest) (cmdRes, error, bool) {
		if len(req.Args) == 0 {
			return cmdRes{}, nil, false
		}
		switch req.Args[0] {
		case "ls":
			if vmExists {
				return cmdRes{Stdout: fmt.Sprintf(`[{"name":%q,"os":"macOS","status":"stopped"}]`, name)}, nil, true
			}
			return cmdRes{Stdout: `[]`}, nil, true
		case "clone":
			name = req.Args[2]
			vmExists = true
			putVM(t, home, name, "YW1iaWd1b3VzLWNsb25l")
			cancelAcquire()
			return cmdRes{ExitCode: 1, Stderr: "partial clone failure"}, errors.New("exit status 1"), true
		case "stop":
			return cmdRes{}, nil, true
		case "get":
			if vmExists {
				return cmdRes{Stdout: fmt.Sprintf(`[{"name":%q,"os":"macOS","status":"stopped"}]`, name)}, nil, true
			}
			return cmdRes{ExitCode: 1, Stderr: "Error: Virtual machine not found: " + name}, errors.New("exit status 1"), true
		case "delete":
			deleteCalled = true
			vmExists = false
			return cmdRes{}, nil, true
		default:
			return cmdRes{}, nil, false
		}
	}
	cfg := configFor()
	b := backendFor(cfg, runner)
	_, err := b.Acquire(acquireCtx, core.AcquireRequest{
		Repo:             core.Repo{Root: t.TempDir()},
		RequestedLeaseID: leaseID,
		RequestedSlug:    "clonefail",
	})
	if err == nil || !strings.Contains(err.Error(), "partial clone failure") || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("Acquire error=%v", err)
	}
	var exitErr core.ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 5 {
		t.Fatalf("ambiguous clone exit=%#v", exitErr)
	}
	if !vmExists || deleteCalled {
		t.Fatalf("ambiguous clone was destructively rolled back vmExists=%v deleteCalled=%v calls=%#v", vmExists, deleteCalled, runner.calls)
	}
	wantClone := strings.Join([]string{"clone", "crabbox-macos-golden", name, "--source-storage", join(home, ".lume"), "--dest-storage", join(home, ".lume")}, "\x00")
	clonePinned := false
	for _, call := range runner.calls {
		if len(call.Args) > 0 && call.Args[0] == "clone" && strings.Join(call.Args, "\x00") == wantClone {
			clonePinned = true
		}
	}
	if !clonePinned {
		t.Fatalf("clone did not use pinned storage args=%q calls=%#v", wantClone, runner.calls)
	}
	recovery, ok, claimErr := resolveLeaseClaimForProvider(leaseID)
	if claimErr != nil || !ok {
		t.Fatalf("recovery claim ok=%v err=%v", ok, claimErr)
	}
	if recovery.Labels["recovery"] != "clone-ambiguous" || recovery.Labels["state"] != "error" || recovery.Labels["instance"] != name || recovery.Labels["storage_id"] == "" || recovery.CloudImmutableID == "" {
		t.Fatalf("recovery claim=%#v", recovery)
	}
	keyPath, keyErr := testboxKeyPath(leaseID)
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	if _, statErr := os.Stat(keyPath); statErr != nil {
		t.Fatalf("recovery lease key: %v", statErr)
	}
	putVM(t, home, name, "cmVwbGFjZW1lbnQtdm0=")
	err = b.Cleanup(bg, core.CleanupRequest{})
	want(t, err, "identity changed")
	if !vmExists || deleteCalled {
		t.Fatalf("replacement VM was deleted vmExists=%v deleteCalled=%v calls=%#v", vmExists, deleteCalled, runner.calls)
	}
	if current, stillOK, currentErr := resolveLeaseClaimForProvider(leaseID); currentErr != nil || !stillOK || current.CloudImmutableID != recovery.CloudImmutableID {
		t.Fatalf("recovery claim after identity refusal=%#v ok=%v err=%v", current, stillOK, currentErr)
	}
}

func TestAcquireControllerAcceptancePrecedesNormalClaimPublication(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", join(home, ".local", "state"))
	storage := join(home, ".lume")
	must(t, os.MkdirAll(storage, 0o700))
	const leaseID = "cbx_controller_acceptance"
	name := ""
	runner := &fake{hook: func(req core.LocalCommandRequest) (cmdRes, error, bool) {
		if len(req.Args) == 0 {
			return cmdRes{}, nil, false
		}
		switch req.Args[0] {
		case "ls":
			return cmdRes{Stdout: `[]`}, nil, true
		case "clone":
			name = req.Args[2]
			putVMAt(t, storage, name, "Y29udHJvbGxlci1hY2NlcHRhbmNl")
			return cmdRes{}, nil, true
		case "get":
			return cmdRes{Stdout: fmt.Sprintf(`[{"name":%q,"os":"macOS","status":"stopped","locationName":%q}]`, name, storage)}, nil, true
		default:
			return cmdRes{}, nil, false
		}
	}}
	accepted := false
	_, err := backendFor(configFor(), runner).Acquire(bg, core.AcquireRequest{
		Repo:             core.Repo{Root: t.TempDir()},
		RequestedLeaseID: leaseID,
		RequestedSlug:    "controller-acceptance",
		OnAcquired: func(acquired core.LeaseTarget) error {
			accepted = true
			if acquired.Server.ImmutableID == "" {
				t.Fatal("OnAcquired received no immutable identity")
			}
			pending, ok, claimErr := resolveLeaseClaimForProvider(leaseID)
			if claimErr != nil || !ok || pending.Labels["recovery"] != "clone-pending" || pending.CloudImmutableID != "" {
				t.Fatalf("claim during OnAcquired=%#v ok=%v err=%v", pending, ok, claimErr)
			}
			return errors.New("controller rejected identity")
		},
	})
	want(t, err, "controller rejected identity")
	if !accepted {
		t.Fatal("OnAcquired was not called")
	}
	if _, statErr := os.Stat(join(storage, name)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("rejected VM was not rolled back: %v", statErr)
	}
	if current, ok, claimErr := resolveLeaseClaimForProvider(leaseID); claimErr != nil || ok {
		t.Fatalf("rejected claim after rollback=%#v ok=%v err=%v", current, ok, claimErr)
	}
}

func TestAmbiguousCloneRetainsPendingClaimWhenStorageChanges(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", join(home, ".local", "state"))
	storage := join(home, ".lume")
	must(t, os.MkdirAll(storage, 0o700))
	const leaseID = "cbx_clone_storage_swap"
	runner := &fake{hook: func(req core.LocalCommandRequest) (cmdRes, error, bool) {
		if len(req.Args) == 0 {
			return cmdRes{}, nil, false
		}
		switch req.Args[0] {
		case "ls":
			return cmdRes{Stdout: `[]`}, nil, true
		case "clone":
			must(t, os.WriteFile(join(storage, lumeStorageIdentityFile), []byte(strings.Repeat("b", 64)+"\n"), 0o600))
			return cmdRes{ExitCode: 1, Stderr: "storage switched"}, errors.New("exit status 1"), true
		case "get":
			return cmdRes{ExitCode: 1, Stderr: "Error: Virtual machine not found: crabbox-clone-storage-swap"}, errors.New("exit status 1"), true
		default:
			return cmdRes{}, nil, false
		}
	}}
	_, err := backendFor(configFor(), runner).Acquire(bg, core.AcquireRequest{
		Repo:             core.Repo{Root: t.TempDir()},
		RequestedLeaseID: leaseID,
		RequestedSlug:    "clone-storage-swap",
	})
	want(t, err, "storage identity changed")
	recovery, ok, claimErr := resolveLeaseClaimForProvider(leaseID)
	if claimErr != nil || !ok || recovery.Labels["recovery"] != "clone-pending" || recovery.Labels["storage_id"] == strings.Repeat("b", 64) {
		t.Fatalf("pending recovery claim=%#v ok=%v err=%v", recovery, ok, claimErr)
	}
	keyPath, keyErr := testboxKeyPath(leaseID)
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	if _, statErr := os.Stat(keyPath); statErr != nil {
		t.Fatalf("pending recovery key: %v", statErr)
	}
}

func TestAmbiguousCloneRetainsPendingClaimUntilIdentityAvailable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", join(home, ".local", "state"))
	must(t, os.MkdirAll(join(home, ".lume"), 0o700))
	const leaseID = "cbx_clone_identity_pending"
	runner := &fake{hook: func(req core.LocalCommandRequest) (cmdRes, error, bool) {
		if len(req.Args) == 0 {
			return cmdRes{}, nil, false
		}
		switch req.Args[0] {
		case "ls":
			return cmdRes{Stdout: `[]`}, nil, true
		case "clone":
			return cmdRes{ExitCode: 1, Stderr: "partial clone failure"}, errors.New("exit status 1"), true
		case "get":
			return cmdRes{ExitCode: 1, Stderr: "Error: Virtual machine not found"}, errors.New("exit status 1"), true
		default:
			return cmdRes{}, nil, false
		}
	}}
	_, err := backendFor(configFor(), runner).Acquire(bg, core.AcquireRequest{
		Repo:             core.Repo{Root: t.TempDir()},
		RequestedLeaseID: leaseID,
		RequestedSlug:    "clone-identity-pending",
	})
	want(t, err, "ambiguous")
	recovery, ok, claimErr := resolveLeaseClaimForProvider(leaseID)
	if claimErr != nil || !ok || recovery.Labels["recovery"] != "clone-pending" || recovery.CloudImmutableID != "" {
		t.Fatalf("pending recovery claim=%#v ok=%v err=%v", recovery, ok, claimErr)
	}
	keyPath, keyErr := testboxKeyPath(leaseID)
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	if _, statErr := os.Stat(keyPath); statErr != nil {
		t.Fatalf("pending recovery key: %v", statErr)
	}
}

func TestParseSkipsLogs(t *testing.T) {
	output := "[2026-07-17T01:19:17Z] INFO: Cleaned up stale session file\n" +
		`[{"name":"worker-1","status":"running","ipAddress":"192.0.2.10"}]` +
		"\n[2026-07-17T01:19:18Z] INFO: done\n"
	instances, err := parseLumeVMs(output)
	must(t, err)
	if len(instances) != 1 || instances[0].Name != "worker-1" || instances[0].IPAddress != "192.0.2.10" {
		t.Fatalf("instances=%#v", instances)
	}
}

func TestRejectsWorkRootTraversal(t *testing.T) {
	cfg := configFor()
	_, err := applyTestFlags(t, cfg, "--lume-work-root", "/Users/lume/../../etc")
	want(t, err, "must be beneath /Users/lume")
}

func TestKeepStoragePath(t *testing.T) {
	cfg := configFor()
	cfg, err := applyTestFlags(t, cfg, "--lume-storage", "/Volumes/VMs")
	if err != nil || cfg.Lume.Storage != "/Volumes/VMs" {
		t.Fatalf("storage=%q apply error=%v", cfg.Lume.Storage, err)
	}
}

func TestRejectsStoragePath(t *testing.T) {
	for _, storage := range []string{"/Volumes/VMs", `C:\\VMs`} {
		cfg := configFor()
		cfg.Lume.Storage = storage
		runner := &fake{}
		b := backendFor(cfg, runner)
		_, err := b.Acquire(bg, core.AcquireRequest{RequestedLeaseID: "cbx_path_storage"})
		want(t, err, "existing lease lifecycle")
		if len(runner.calls) != 0 {
			t.Fatalf("Lume called for path-backed storage=%q acquire: %#v", storage, runner.calls)
		}
	}
}

func TestStorageExactGet(t *testing.T) {
	home := t.TempDir()
	storage := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", join(home, ".local", "state"))
	cfg := configFor()
	cfg.Lume.Storage = storage
	runner := &fake{responses: results{
		"get\x00crabbox-macos-golden\x00--format\x00json\x00--storage\x00" + storage: {
			Stdout: `[{"name":"crabbox-macos-golden","os":"macOS","status":"stopped"}]`,
		},
	}}
	b := backendFor(cfg, runner)
	instances, err := b.listInstancesForConfig(bg, b.configForRun())
	must(t, err)
	if len(instances) != 1 || instances[0].Name != "crabbox-macos-golden" {
		t.Fatalf("instances=%#v", instances)
	}
	for _, call := range runner.calls {
		if len(call.Args) > 0 && call.Args[0] == "ls" {
			t.Fatalf("path lifecycle used unsupported list filter: %#v", runner.calls)
		}
	}
}

func TestStorageMustExist(t *testing.T) {
	cfg := base()
	cfg.Lume.Storage = join(t.TempDir(), "unmounted")
	runner := &fake{}
	b := backendFor(cfg, runner)
	_, err := b.getInstance(bg, cfg, "worker")
	want(t, err, "storage")
	if len(runner.calls) != 0 {
		t.Fatalf("Lume called with unavailable storage: %#v", runner.calls)
	}
}

func TestStorageIdentityConcurrentInitialization(t *testing.T) {
	root := t.TempDir()
	const workers = 16
	identities := make(chan string, workers)
	errorsSeen := make(chan error, workers)
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			identity, err := ensureLumeStorageIdentity(root)
			if err != nil {
				errorsSeen <- err
				return
			}
			identities <- identity
		}()
	}
	group.Wait()
	close(identities)
	close(errorsSeen)
	for err := range errorsSeen {
		t.Fatal(err)
	}
	wantIdentity := ""
	for identity := range identities {
		if wantIdentity == "" {
			wantIdentity = identity
		}
		if identity != wantIdentity {
			t.Fatalf("storage identities differ: got=%q want=%q", identity, wantIdentity)
		}
	}
	if installed, err := readLumeStorageIdentity(root); err != nil || installed != wantIdentity {
		t.Fatalf("installed storage identity=%q err=%v want=%q", installed, err, wantIdentity)
	}
}

func TestCleanupRetainsClaimWhenStorageMountVanishes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", join(home, ".local", "state"))
	storage := join(home, "mounted-vms")
	const leaseID, name = "cbx_mount_vanished", "crabbox-mount-vanished"
	putVMAt(t, storage, name, "bW91bnRlZC12bQ==")
	storageID := strings.Repeat("a", 64)
	must(t, os.WriteFile(join(storage, ".crabbox-lume-storage-id"), []byte(storageID+"\n"), 0o600))
	cfg := base()
	cfg.Lume.Storage = storage
	immutableID, err := lumeVMImmutableID(cfg, lumeVM{Name: name, LocationName: storage})
	must(t, err)
	server := core.Server{CloudID: name, ImmutableID: immutableID, Provider: providerName, Labels: labels{
		"instance": name, "storage": storage, "storage_exact": "true", "storage_id": storageID, "state": "ready",
	}}
	must(t, core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, "mount-vanished", providerName, instanceScope(name), "", t.TempDir(), time.Minute, false, server, core.SSHTarget{}))
	keyPath, _, err := ensureTestboxKeyForConfig(cfg, leaseID)
	must(t, err)

	backing := storage + ".backing"
	must(t, os.Rename(storage, backing))
	must(t, os.Mkdir(storage, 0o700))
	runner := &fake{hook: func(req core.LocalCommandRequest) (cmdRes, error, bool) {
		if len(req.Args) > 0 && req.Args[0] == "get" {
			return cmdRes{ExitCode: 1, Stderr: "Error: Virtual machine not found: " + name}, errors.New("exit status 1"), true
		}
		return cmdRes{}, nil, false
	}}
	err = backendFor(base(), runner).Cleanup(bg, core.CleanupRequest{})
	want(t, err, "storage identity")
	if current, ok, claimErr := resolveLeaseClaimForProvider(leaseID); claimErr != nil || !ok || current.CloudImmutableID != immutableID {
		t.Fatalf("claim after vanished mount=%#v ok=%v err=%v", current, ok, claimErr)
	}
	if _, statErr := os.Stat(keyPath); statErr != nil {
		t.Fatalf("lease key removed after vanished mount: %v", statErr)
	}
	must(t, os.Remove(storage))
	must(t, os.Rename(backing, storage))
	remountedMarker, readErr := os.ReadFile(join(storage, ".crabbox-lume-storage-id"))
	if remountedID := strings.TrimSpace(string(remountedMarker)); readErr != nil || remountedID != storageID {
		t.Fatalf("remounted storage identity=%q err=%v want=%q", remountedID, readErr, storageID)
	}
}

func TestCleanupBindsPendingCloneIdentityBeforeDelete(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", join(home, ".local", "state"))
	storage := join(home, ".lume")
	const leaseID, name = "cbx_pending_clone_vm", "crabbox-pending-clone-vm"
	putVMAt(t, storage, name, "cGVuZGluZy1jbG9uZQ==")
	storageID, err := ensureLumeStorageIdentity(storage)
	must(t, err)
	server := core.Server{CloudID: name, Provider: providerName, Status: "provisioning", Labels: labels{
		"instance": name, "storage": storage, "storage_exact": "true", "storage_id": storageID,
		"state": "provisioning", "recovery": "clone-pending", "run_owner_expected": "false",
	}}
	must(t, core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, "pending-clone-vm", providerName, instanceScope(name), "", t.TempDir(), time.Minute, false, server, core.SSHTarget{}))
	cfg := base()
	cfg.Lume.Storage = storage
	keyPath, _, err := ensureTestboxKeyForConfig(cfg, leaseID)
	must(t, err)
	runner := &fake{responses: results{
		"get": {Stdout: fmt.Sprintf(`[{"name":%q,"os":"macOS","status":"stopped","locationName":%q}]`, name, storage)},
	}}
	must(t, backendFor(base(), runner).Cleanup(bg, core.CleanupRequest{}))
	if _, statErr := os.Stat(join(storage, name)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("pending clone VM remains after identity-bound cleanup: %v", statErr)
	}
	if current, ok, claimErr := resolveLeaseClaimForProvider(leaseID); claimErr != nil || ok {
		t.Fatalf("pending clone claim after cleanup=%#v ok=%v err=%v", current, ok, claimErr)
	}
	if _, statErr := os.Stat(keyPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("pending clone key after cleanup: %v", statErr)
	}
}

func TestCleanupRetainsPendingCloneWhenStorageChangesDuringIdentityBind(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", join(home, ".local", "state"))
	storage := join(home, ".lume")
	const leaseID, name = "cbx_pending_clone_swap", "crabbox-pending-clone-swap"
	putVMAt(t, storage, name, "cGVuZGluZy1jbG9uZS1zd2Fw")
	storageID := strings.Repeat("a", 64)
	must(t, os.WriteFile(join(storage, lumeStorageIdentityFile), []byte(storageID+"\n"), 0o600))
	server := core.Server{CloudID: name, Provider: providerName, Status: "provisioning", Labels: labels{
		"instance": name, "storage": storage, "storage_exact": "true", "storage_id": storageID,
		"state": "provisioning", "recovery": "clone-pending", "run_owner_expected": "false",
	}}
	must(t, core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, "pending-clone-swap", providerName, instanceScope(name), "", t.TempDir(), time.Minute, false, server, core.SSHTarget{}))
	cfg := base()
	cfg.Lume.Storage = storage
	keyPath, _, err := ensureTestboxKeyForConfig(cfg, leaseID)
	must(t, err)
	getCalls := 0
	runner := &fake{hook: func(req core.LocalCommandRequest) (cmdRes, error, bool) {
		if len(req.Args) == 0 || req.Args[0] != "get" {
			return cmdRes{}, nil, false
		}
		getCalls++
		if getCalls == 2 {
			must(t, os.WriteFile(join(storage, lumeStorageIdentityFile), []byte(strings.Repeat("b", 64)+"\n"), 0o600))
		}
		return cmdRes{Stdout: fmt.Sprintf(`[{"name":%q,"os":"macOS","status":"stopped","locationName":%q}]`, name, storage)}, nil, true
	}}
	err = backendFor(base(), runner).Cleanup(bg, core.CleanupRequest{})
	want(t, err, "storage identity changed")
	if current, ok, claimErr := resolveLeaseClaimForProvider(leaseID); claimErr != nil || !ok || current.Labels["recovery"] != "clone-pending" || current.CloudImmutableID != "" {
		t.Fatalf("pending claim after storage swap=%#v ok=%v err=%v", current, ok, claimErr)
	}
	if _, statErr := os.Stat(keyPath); statErr != nil {
		t.Fatalf("pending key removed after storage swap: %v", statErr)
	}
}

func TestCleanupRetainsMissingPendingCloneRegardlessOfAge(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", join(home, ".local", "state"))
	storage := join(home, ".lume")
	must(t, os.MkdirAll(storage, 0o700))
	storageID, err := ensureLumeStorageIdentity(storage)
	must(t, err)
	const leaseID, name = "cbx_fresh_pending_clone", "crabbox-fresh-pending-clone"
	server := core.Server{CloudID: name, Provider: providerName, Status: "provisioning", Labels: labels{
		"instance": name, "storage": storage, "storage_exact": "true", "storage_id": storageID,
		"state": "provisioning", "recovery": "clone-pending", "run_owner_expected": "false",
	}}
	must(t, core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, "fresh-pending-clone", providerName, instanceScope(name), "", t.TempDir(), time.Minute, false, server, core.SSHTarget{}))
	claimPath := join(home, ".local", "state", "crabbox", "claims", leaseID+".json")
	claimData, err := os.ReadFile(claimPath)
	must(t, err)
	var stored core.LeaseClaim
	must(t, json.Unmarshal(claimData, &stored))
	stored.ClaimedAt = time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	claimData, err = json.MarshalIndent(stored, "", "  ")
	must(t, err)
	must(t, os.WriteFile(claimPath, append(claimData, '\n'), 0o600))
	if cleanup, reason := shouldCleanup(server, stored, time.Now().UTC()); !cleanup || reason != "clone pending stale" {
		t.Fatalf("old present pending clone cleanup=%v reason=%q", cleanup, reason)
	}
	cfg := base()
	cfg.Lume.Storage = storage
	keyPath, _, err := ensureTestboxKeyForConfig(cfg, leaseID)
	must(t, err)
	runner := &fake{hook: func(req core.LocalCommandRequest) (cmdRes, error, bool) {
		if len(req.Args) > 0 && req.Args[0] == "get" {
			return cmdRes{ExitCode: 1, Stderr: "Error: Virtual machine not found: " + name}, errors.New("exit status 1"), true
		}
		return cmdRes{}, nil, false
	}}
	must(t, backendFor(base(), runner).Cleanup(bg, core.CleanupRequest{}))
	if current, ok, claimErr := resolveLeaseClaimForProvider(leaseID); claimErr != nil || !ok || current.Labels["recovery"] != "clone-pending" {
		t.Fatalf("fresh pending claim=%#v ok=%v err=%v", current, ok, claimErr)
	}
	if _, statErr := os.Stat(keyPath); statErr != nil {
		t.Fatalf("pending key removed: %v", statErr)
	}
}

func TestCleanupRetainsClaimWhenStorageChangesAfterMissingObservation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", join(home, ".local", "state"))
	storage := join(home, "mounted-vms")
	const leaseID, name = "cbx_mount_changed_late", "crabbox-mount-changed-late"
	putVMAt(t, storage, name, "bGF0ZS1tb3VudC1jaGFuZ2U=")
	storageID := strings.Repeat("a", 64)
	must(t, os.WriteFile(join(storage, lumeStorageIdentityFile), []byte(storageID+"\n"), 0o600))
	cfg := base()
	cfg.Lume.Storage = storage
	immutableID, err := lumeVMImmutableID(cfg, lumeVM{Name: name, LocationName: storage})
	must(t, err)
	server := core.Server{CloudID: name, ImmutableID: immutableID, Provider: providerName, Status: "stopped", Labels: labels{
		"instance": name, "storage": storage, "storage_exact": "true", "storage_id": storageID, "state": "stopped",
	}}
	must(t, core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, "mount-changed-late", providerName, instanceScope(name), "", t.TempDir(), time.Minute, false, server, core.SSHTarget{}))
	keyPath, _, err := ensureTestboxKeyForConfig(cfg, leaseID)
	must(t, err)
	getCalls := 0
	runner := &fake{hook: func(req core.LocalCommandRequest) (cmdRes, error, bool) {
		if len(req.Args) == 0 || req.Args[0] != "get" {
			return cmdRes{}, nil, false
		}
		getCalls++
		if getCalls < 3 {
			return cmdRes{Stdout: fmt.Sprintf(`[{"name":%q,"os":"macOS","status":"stopped","locationName":%q}]`, name, storage)}, nil, true
		}
		must(t, os.WriteFile(join(storage, lumeStorageIdentityFile), []byte(strings.Repeat("b", 64)+"\n"), 0o600))
		return cmdRes{ExitCode: 1, Stderr: "Error: Virtual machine not found: " + name}, errors.New("exit status 1"), true
	}}
	err = backendFor(base(), runner).Cleanup(bg, core.CleanupRequest{})
	want(t, err, "storage identity changed")
	if current, ok, claimErr := resolveLeaseClaimForProvider(leaseID); claimErr != nil || !ok || current.CloudImmutableID != immutableID {
		t.Fatalf("claim after late storage change=%#v ok=%v err=%v", current, ok, claimErr)
	}
	if _, statErr := os.Stat(keyPath); statErr != nil {
		t.Fatalf("lease key removed after late storage change: %v", statErr)
	}
}

func TestDeleteVMRevalidatesStorageIdentityImmediatelyBeforeDelete(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	storage := join(home, ".lume")
	const name = "crabbox-delete-storage-swap"
	putVMAt(t, storage, name, "ZGVsZXRlLXN0b3JhZ2Utc3dhcA==")
	storageID := strings.Repeat("a", 64)
	must(t, os.WriteFile(join(storage, lumeStorageIdentityFile), []byte(storageID+"\n"), 0o600))
	cfg := base()
	cfg.Lume.Storage = storage
	immutableID, err := lumeVMImmutableID(cfg, lumeVM{Name: name, LocationName: storage})
	must(t, err)
	claim := core.LeaseClaim{CloudImmutableID: immutableID, Labels: labels{
		"instance": name, "storage": storage, "storage_exact": "true", "storage_id": storageID,
	}}
	must(t, os.WriteFile(join(storage, lumeStorageIdentityFile), []byte(strings.Repeat("b", 64)+"\n"), 0o600))
	err = backendFor(base(), &fake{}).deleteVM(cfg, name, claim, lumeRunOwner{})
	want(t, err, "storage identity changed")
	if _, statErr := os.Stat(join(storage, name, "config.json")); statErr != nil {
		t.Fatalf("VM deleted from unconfirmed storage: %v", statErr)
	}
}

func TestListUsesClaimStorage(t *testing.T) {
	home := t.TempDir()
	storage := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", join(home, ".local", "state"))
	const leaseID, name = "cbx_prior_storage", "crabbox-prior-storage"
	server := core.Server{CloudID: name, Provider: providerName, Labels: labels{"instance": name, "storage": storage, "storage_exact": "true", "state": "ready"}}
	must(t, core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, "prior", providerName, instanceScope(name), "", t.TempDir(), time.Minute, false, server, core.SSHTarget{}))
	runner := &fake{responses: results{
		"ls": {Stdout: `[]`},
		"get\x00" + name + "\x00--format\x00json\x00--storage\x00" + storage: {Stdout: `[{"name":"crabbox-prior-storage","status":"running","ipAddress":"192.0.2.12"}]`},
	}}
	views, err := backendFor(base(), runner).List(bg, core.ListRequest{})
	must(t, err)
	if len(views) != 1 || views[0].CloudID != name || views[0].Status != "ready" {
		t.Fatalf("views=%#v", views)
	}
}

func TestLumeNotFoundIsSpecific(t *testing.T) {
	if !isLumeNotFoundError(errors.New("lume get failed: Error: Virtual machine not found: worker")) {
		t.Fatal("exact Lume VM-not-found error was not classified")
	}
	if isLumeNotFoundError(errors.New("exec: lume: executable file not found in $PATH")) {
		t.Fatal("missing CLI was classified as a missing VM")
	}
	runner := &fake{responses: results{"ls": {Stdout: `[]`}}, errors: map[string]error{"get\x00worker\x00--format\x00json": errors.New("transient get failure")}}
	b := newBackend((Provider{}).Spec(), base(), core.Runtime{Exec: runner}).(*backend)
	if _, _, err := b.observeVMState(bg, b.configForRun(), "worker"); err == nil {
		t.Fatal("observe converted transient get failure to missing")
	}
	claim := claim{LeaseID: "cbx_transient", Labels: labels{"instance": "worker"}}
	if _, _, err := b.resolveClaimedInstance(bg, claim); err == nil {
		t.Fatal("resolve converted transient get failure to missing")
	}
}

func TestRejectsUnresolvedOwner(t *testing.T) {
	claim := claim{LeaseID: "cbx_pending", Labels: labels{
		"run_owner_expected": "true",
		"run_owner_pending":  "true",
	}}
	_, err := ownerForDestruction(claim)
	want(t, err, "invalid pending launch metadata")
	claim.Labels["run_owner_pending"] = "false"
	_, err = ownerForDestruction(claim)
	want(t, err, "without complete launch owner metadata")
}

func TestRecoversPendingOwner(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", join(home, ".local", "state"))
	token, err := newLaunchToken()
	must(t, err)
	handoff, err := prepareLaunchHandoff(token)
	must(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(handoff.Dir) })
	must(t, os.WriteFile(handoff.OwnerPath, []byte("99999999\n"), 0o600))
	claim := claim{LeaseID: "cbx_stale_pending", Labels: labels{
		"run_owner_expected": "true",
		"run_owner_pending":  "true",
		"run_launch_token":   token,
	}}
	owner, err := ownerForDestruction(claim)
	if err != nil || owner.PID != 0 {
		t.Fatalf("stale pending owner=%#v err=%v", owner, err)
	}
}

func TestClaimPinsLifecycle(t *testing.T) {
	cfg := base()
	cfg.Lume.Base = "current-base"
	cfg.Lume.Storage = "current-storage"
	cfg.Lume.User = "current-user"
	cfg.Lume.WorkRoot = "/Users/current-user/work"
	claim := claim{Labels: labels{
		"base":      "lease-base",
		"storage":   "lease-storage",
		"ssh_user":  "lease-user",
		"work_root": "/Users/lease-user/work",
	}}
	got := configForClaim(cfg, claim)
	if got.Lume.Base != "lease-base" || got.Lume.Storage != "lease-storage" || got.SSHUser != "lease-user" || got.WorkRoot != "/Users/lease-user/work" {
		t.Fatalf("claim config=%#v sshUser=%q workRoot=%q", got.Lume, got.SSHUser, got.WorkRoot)
	}
}

func TestClaimConfigPinsStorage(t *testing.T) {
	for _, tc := range []struct{ label, exact, want string }{{"home", "", ""}, {"home", "true", "home"}, {"Home", "", "Home"}, {"unknown", "", ""}} {
		cfg := base()
		cfg.Lume.Storage = "current-storage"
		claim := claim{Labels: labels{"storage": tc.label, "storage_exact": tc.exact}}
		if got := configForClaim(cfg, claim).Lume.Storage; got != tc.want {
			t.Fatalf("storage label %q exact=%q resolved to %q, want %q", tc.label, tc.exact, got, tc.want)
		}
	}
	labels := (&backend{}).serverFromInstance(lumeVM{LocationName: "home"}, claim{}, base()).Labels
	if labels["storage"] != "home" || labels["storage_exact"] != "true" {
		t.Fatal(labels)
	}
}

func TestTouchKeepsRouting(t *testing.T) {
	cfg := configFor()
	b := backendFor(cfg, &fake{})
	server := core.Server{Labels: labels{
		"storage":              "home",
		"instance":             "worker-1",
		"run_owner_pid":        "1234",
		"run_owner_started_at": "2026-07-16T00:00:00Z",
		"run_log":              "/tmp/worker-1.log",
	}}
	got, err := b.Touch(bg, core.TouchRequest{Lease: core.LeaseTarget{Server: server}, State: "ready"})
	must(t, err)
	for key, want := range server.Labels {
		if got.Labels[key] != want {
			t.Fatalf("label %s=%q want %q", key, got.Labels[key], want)
		}
	}
}

func TestStopRequiresExactOwner(t *testing.T) {
	cfg := configFor()
	runner := &fake{responses: results{
		"get": {Stdout: `[{"name":"worker-1","status":"running"}]`},
	}}
	b := backendFor(cfg, runner)
	want(t, b.stopVM(bg, b.configForRun(), "worker-1", lumeRunOwner{}), "exact launch owner")
	if len(runner.calls) != 1 || runner.calls[0].Args[0] != "get" {
		t.Fatalf("unexpected Lume mutation: %#v", runner.calls)
	}
}

func TestReleaseClaim(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", join(home, ".local", "state"))
	const leaseID = "cbx_release123456"
	const name = "crabbox-release-1234"
	const originalMachineID = "bHVtZS1tYWNoaW5lLW9yaWdpbmFs"
	putVM(t, home, name, originalMachineID)
	storageID, err := ensureLumeStorageIdentity(join(home, ".lume"))
	must(t, err)
	vmExists := true
	vmState := "stopped"
	vmJSON := func() string {
		return fmt.Sprintf(`[{"name":"crabbox-release-1234","os":"macOS","status":%q,"locationName":"home"}]`, vmState)
	}
	runner := &fake{}
	runner.hook = func(req core.LocalCommandRequest) (cmdRes, error, bool) {
		if len(req.Args) == 0 {
			return cmdRes{}, nil, false
		}
		switch req.Args[0] {
		case "ls":
			if vmExists {
				return cmdRes{Stdout: vmJSON()}, nil, true
			}
			return cmdRes{Stdout: `[]`}, nil, true
		case "get":
			if vmExists {
				return cmdRes{Stdout: vmJSON()}, nil, true
			}
			return cmdRes{ExitCode: 1, Stderr: "Error: Virtual machine not found: crabbox-release-1234"}, errors.New("exit status 1"), true
		case "stop":
			vmState = "stopped"
			return cmdRes{}, nil, true
		case "delete":
			vmExists = false
			return cmdRes{}, nil, true
		default:
			return cmdRes{}, nil, false
		}
	}
	cfg := configFor()
	b := backendFor(cfg, runner)
	lease := core.LeaseTarget{
		LeaseID: leaseID,
		Server: core.Server{CloudID: name, Labels: labels{
			"lease": leaseID, "instance": name, "storage": "home", "base": "crabbox-macos-golden",
			"storage_id": storageID, "ssh_user": "lume", "work_root": "/Users/lume/crabbox",
		}},
	}
	immutableID, err := lumeVMImmutableID(b.configForRun(), lumeVM{Name: name, LocationName: "home"})
	must(t, err)
	lease.Server.ImmutableID = immutableID
	want(t, b.ReleaseLease(bg, core.ReleaseLeaseRequest{Lease: lease}), "unclaimed")
	if len(runner.calls) != 0 {
		t.Fatalf("unclaimed release called Lume: %#v", runner.calls)
	}
	must(t, core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, "release", providerName, instanceScope(name), "", t.TempDir(), time.Minute, false, lease.Server, core.SSHTarget{}))
	putVM(t, home, name, "bHVtZS1tYWNoaW5lLXJlcGxhY2VtZW50")
	want(t, b.ReleaseLease(bg, core.ReleaseLeaseRequest{Lease: lease}), "identity changed")
	if !vmExists {
		t.Fatal("replacement VM was deleted")
	}
	if _, ok, err := resolveLeaseClaimForProvider(leaseID); err != nil || !ok {
		t.Fatalf("claim after replacement refusal ok=%v err=%v", ok, err)
	}
	putVM(t, home, name, originalMachineID)
	vmState = "provisioning (stale)"
	remoteCleanupCalled := false
	must(t, b.ReleaseLease(bg, core.ReleaseLeaseRequest{Lease: lease, GuardedRemoteCleanup: func(context.Context, core.LeaseTarget) { remoteCleanupCalled = true }}))
	if remoteCleanupCalled {
		t.Fatal("guarded cleanup ran without a prepared SSH endpoint")
	}
	if _, err := os.Stat(join(home, ".lume", name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("claimed VM directory still exists: %v", err)
	}
	if _, ok, err := resolveLeaseClaimForProvider(leaseID); err != nil || ok {
		t.Fatalf("claim after release ok=%v err=%v", ok, err)
	}
	for _, call := range runner.calls {
		if len(call.Args) > 0 && call.Args[0] == "delete" {
			t.Fatalf("unsafe name-based delete call: %#v", runner.calls)
		}
	}
}

func TestPrepareLeaseUsesKnownHosts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := configFor()
	leaseID := "cbx_00000000-0000-0000-0000-000000000001"
	writeLumeKnownHost(t, leaseID, "worker-1", hostKey)
	claim := claim{LeaseID: leaseID, Labels: labels{"instance": "worker-1", "state": "ready"}}
	b := backendFor(cfg, &fake{})
	lease, err := b.prepareLease(bg, b.configForRun(), lumeVM{Name: "worker-1", Status: "running", IPAddress: "192.0.2.10"}, claim, false)
	must(t, err)
	if lease.SSH.DisableHostKeyChecking || filepath.Base(lease.SSH.KnownHostsFile) != "known_hosts" || !strings.Contains(lease.SSH.KnownHostsFile, leaseID) || lease.SSH.HostKeyAlias != lumeHostKeyAlias("worker-1") {
		t.Fatalf("known hosts not isolated: %#v", lease.SSH)
	}
	if !lease.SSH.SSHConfigProxy {
		t.Fatal("SSHConfigProxy = false, want OpenSSH readiness for the local Lume guest")
	}
}

func TestRebindHostAlias(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const leaseID = "cbx_rebind123456"
	writeLumeKnownHost(t, leaseID, "worker-1", hostKey)
	target := core.LeaseTarget{Server: core.Server{CloudID: "worker-1", Labels: labels{"state": "ready"}}}
	b := &backend{}
	must(t, b.RebindResolvedLeaseTarget(&target, leaseID))
	if target.SSH.HostKeyAlias != lumeHostKeyAlias("worker-1") {
		t.Fatalf("host key alias=%q", target.SSH.HostKeyAlias)
	}
}

func TestRebindHostPin(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	b := &backend{}
	for _, state := range []string{"starting", "ready"} {
		target := core.LeaseTarget{Server: core.Server{CloudID: "worker-1", Labels: labels{"state": state}}}
		if err := b.RebindResolvedLeaseTarget(&target, "cbx_untrusted_"+state); err == nil {
			t.Fatalf("state=%s accepted without authenticated host-key pin", state)
		}
	}
	const leaseID = "cbx_untrusted_malformed"
	writeLumeKnownHost(t, leaseID, "worker-1", "AQ==")
	target := core.LeaseTarget{Server: core.Server{CloudID: "worker-1", Labels: labels{"state": "ready"}}}
	if err := b.RebindResolvedLeaseTarget(&target, leaseID); err == nil {
		t.Fatal("accepted malformed SSH key blob as authenticated pin")
	}
}

func TestImmutableID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	const name = "worker-identity"
	putVM(t, home, name, "bHVtZS1tYWNoaW5lLW9uZQ==")
	cfg := base()
	first, err := lumeVMImmutableID(cfg, lumeVM{Name: name, LocationName: "home"})
	must(t, err)
	putVM(t, home, name, "bHVtZS1tYWNoaW5lLXR3bw==")
	second, err := lumeVMImmutableID(cfg, lumeVM{Name: name, LocationName: "home"})
	must(t, err)
	if first == second || !strings.HasPrefix(first, "lume-machine-") || !strings.HasPrefix(second, "lume-machine-") {
		t.Fatalf("immutable IDs first=%q second=%q", first, second)
	}
}

func TestImmutableIDStorage(t *testing.T) {
	home := t.TempDir()
	storageRoot := join(home, "lume-fast")
	t.Setenv("HOME", home)
	writeLumeSettings(t, home, fmt.Sprintf("defaultLocationName: fast\nvmLocations:\n  - name: fast\n    path: %q\n", storageRoot))
	const name = "worker-fast"
	putVMAt(t, storageRoot, name, "bHVtZS1mYXN0")
	if _, err := lumeVMImmutableID(base(), lumeVM{Name: name, LocationName: "fast"}); err != nil {
		t.Fatal(err)
	}
}

func TestImmutableIDHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeLumeSettings(t, home, "telemetryEnabled: false\n")
	putVM(t, home, "worker-home-default", "bHVtZS1ob21lLWRlZmF1bHQ=")
	if _, err := lumeVMImmutableID(base(), lumeVM{Name: "worker-home-default", LocationName: "home"}); err != nil {
		t.Fatal(err)
	}
}

func TestEphemeralStorageRejected(t *testing.T) {
	cfg := configFor()
	_, err := applyTestFlags(t, cfg, "--lume-storage", "ephemeral")
	want(t, err, "excludes ephemeral VMs from inventory")
}

func TestCloneUsesStorage(t *testing.T) {
	cfg := configFor()
	cfg.Lume.Base = "golden"
	cfg.Lume.Storage = "fast"
	runner := &fake{responses: results{}}
	b := backendFor(cfg, runner)
	must(t, b.cloneVM(bg, b.configForRun(), "worker-1"))
	if len(runner.calls) != 1 {
		t.Fatalf("calls=%d", len(runner.calls))
	}
	want := "clone\x00golden\x00worker-1\x00--source-storage\x00fast\x00--dest-storage\x00fast"
	if got := strings.Join(runner.calls[0].Args, "\x00"); got != want {
		t.Fatalf("clone args=%q want %q", got, want)
	}
}

func TestBootstrapKeyOnly(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	publicKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest crabbox-test"
	trust, err := prepareBootstrapTrust("worker-1", "lume", publicKey)
	must(t, err)
	defer removeBootstrapTrust(trust)
	secondTrust, err := prepareBootstrapTrust("worker-1", "lume", publicKey)
	must(t, err)
	defer removeBootstrapTrust(secondTrust)
	if secondTrust.Dir == trust.Dir {
		t.Fatalf("bootstrap directories were reused: %s", trust.Dir)
	}
	info, err := os.Stat(trust.Dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("trust directory info=%#v err=%v", info, err)
	}
	for name, want := range (labels{
		"challenge":      trust.Challenge,
		"ssh_user":       "lume",
		"authorized_key": publicKey,
	}) {
		data, readErr := os.ReadFile(join(trust.Dir, name))
		if readErr != nil || strings.TrimSpace(string(data)) != want {
			t.Fatalf("%s=%q err=%v want %q", name, data, readErr, want)
		}
	}
}

func TestGuestIdentityPin(t *testing.T) {
	dir := t.TempDir()
	trust := bootstrapTrust{Dir: dir, Challenge: "test-challenge"}
	identity := "test-challenge 00112233-4455-6677-8899-AABBCCDDEEFF ssh-ed25519 " + hostKey + "\n"
	must(t, os.WriteFile(join(dir, "identity"), []byte(identity), 0o600))
	knownHosts := join(dir, "known_hosts")
	b := &backend{}
	platformUUID, err := b.waitForGuestIdentity(bg, "worker-1", "192.0.2.10", trust, knownHosts)
	must(t, err)
	if platformUUID != "00112233-4455-6677-8899-AABBCCDDEEFF" {
		t.Fatalf("platform UUID=%q", platformUUID)
	}
	data, err := os.ReadFile(knownHosts)
	must(t, err)
	got := string(data)
	if !strings.Contains(got, lumeHostKeyAlias("worker-1")+" ssh-ed25519 "+hostKey) || strings.Contains(got, "192.0.2.10") {
		t.Fatalf("known_hosts=%q", got)
	}
	identity = "test-challenge 00112233-4455-6677-8899-AABBCCDDEEFF ssh-ed25519 AQ==\n"
	must(t, os.WriteFile(join(dir, "identity"), []byte(identity), 0o600))
	if _, err := pinBootstrapHostKey("192.0.2.10", lumeHostKeyAlias("worker-1"), trust, join(dir, "known_hosts")); err == nil {
		t.Fatal("accepted malformed SSH key blob from bootstrap identity")
	}
}

func TestWaitSSHFalseNegative(t *testing.T) {
	cfg := configFor()
	runner := &fake{responses: results{
		"get": {Stdout: `[{"name":"worker-1","os":"macOS","status":"running","ipAddress":"192.0.2.10","sshAvailable":false}]`},
	}}
	b := backendFor(cfg, runner)
	visible := false
	inst, err := b.waitForRunningVM(bg, b.configForRun(), "worker-1", lumeRunOwner{}, func() { visible = true })
	must(t, err)
	if !visible || inst.IPAddress != "192.0.2.10" || inst.SSHAvailable == nil || *inst.SSHAvailable {
		t.Fatalf("instance=%#v", inst)
	}
}

func TestWaitOwnerExit(t *testing.T) {
	logPath := join(t.TempDir(), "owner.log")
	must(t, os.WriteFile(logPath, []byte("capacity unavailable\n"), 0o600))
	cfg := configFor()
	runner := &fake{responses: results{
		"get": {Stdout: `[{"name":"worker-1","status":"stopped"}]`},
	}}
	b := backendFor(cfg, runner)
	_, err := b.waitForRunningVM(bg, b.configForRun(), "worker-1", lumeRunOwner{PID: 2147483647, StartIdentity: "missing", LogPath: logPath}, func() {})
	want(t, err, "owner exited during startup: capacity unavailable")
}
