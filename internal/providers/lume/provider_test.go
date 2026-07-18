package lume

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type recordingRunner struct {
	calls     []core.LocalCommandRequest
	responses map[string]core.LocalCommandResult
	errors    map[string]error
	hook      func(core.LocalCommandRequest) (core.LocalCommandResult, error, bool)
}

const testLumeHostKey = "AAAAC3NzaC1lZDI1NTE5AAAAIOCh4W5YA0Lp2pvT+yWIG/tC7BrQalNUIHSqfjYkJei6"

func writeLumeVMConfig(t *testing.T, home, name, machineIdentifier string) {
	t.Helper()
	writeLumeVMConfigAt(t, filepath.Join(home, ".lume"), name, machineIdentifier)
}

func writeLumeVMConfigAt(t *testing.T, root, name, machineIdentifier string) {
	t.Helper()
	dir := filepath.Join(root, name)
	mustNoError(t, os.MkdirAll(dir, 0o700))
	data := fmt.Sprintf(`{"machineIdentifier":%q}`, machineIdentifier)
	mustNoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(data), 0o600))
}

func writeLumeSettings(t *testing.T, home, settings string) {
	t.Helper()
	dir := filepath.Join(home, ".config", "lume")
	t.Setenv("XDG_CONFIG_HOME", filepath.Dir(dir))
	mustNoError(t, os.MkdirAll(dir, 0o700))
	mustNoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(settings), 0o600))
}

func writeLumeKnownHost(t *testing.T, leaseID, name, key string) {
	t.Helper()
	target := core.SSHTarget{}
	mustNoError(t, core.UseLeaseKnownHosts(&target, leaseID))
	mustNoError(t, os.WriteFile(target.KnownHostsFile, []byte(lumeHostKeyAlias(name)+" ssh-ed25519 "+key+"\n"), 0o600))
}

func (r *recordingRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
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
	return core.LocalCommandResult{}, nil
}

func applyTestFlags(t *testing.T, cfg core.Config, args ...string) (core.Config, error) {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := registerFlags(fs, cfg)
	mustNoError(t, fs.Parse(args))
	err := applyFlags(&cfg, fs, values)
	return cfg, err
}

func testBackend(cfg core.Config, runner core.CommandRunner) *backend {
	rt := core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}
	return newBackend((Provider{}).Spec(), cfg, rt).(*backend)
}

func testConfig() core.Config {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	return cfg
}

func mustNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestProviderSpecAndAliases(t *testing.T) {
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

func TestShouldCleanupReadyLeaseAtLabelExpiry(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	server := Server{Status: "running", Labels: map[string]string{
		"state":      "ready",
		"expires_at": core.LeaseLabelTime(now),
	}}
	if cleanup, reason := shouldCleanup(server, core.LeaseClaim{}, now.Add(time.Second)); !cleanup || !strings.Contains(reason, "expired") {
		t.Fatalf("shouldCleanup=%v, %q; want expired ready lease cleanup", cleanup, reason)
	}
	if cleanup, reason := shouldCleanup(server, core.LeaseClaim{}, now.Add(-time.Second)); cleanup {
		t.Fatalf("shouldCleanup=%v, %q; want unexpired ready lease retained", cleanup, reason)
	}
	server.Labels["expires_at"] = core.LeaseLabelTime(now.Add(time.Hour))
	claim := core.LeaseClaim{LastUsedAt: now.Add(-14 * time.Hour).Format(time.RFC3339), IdleTimeoutSeconds: 3600}
	if cleanup, reason := shouldCleanup(server, claim, now); !cleanup || reason != "claim expired" {
		t.Fatalf("shouldCleanup=%v, %q; want stale claim fallback cleanup", cleanup, reason)
	}
	server.Labels["keep"] = "true"
	server.Labels["recovery"] = "rollback-failed"
	if cleanup, _ := shouldCleanup(server, claim, now); !cleanup {
		t.Fatal("rollback failure was retained")
	}
}

func TestShouldCleanupRetainsActiveStartup(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	server := Server{Status: "starting", Labels: map[string]string{"state": "starting", "expires_at": core.LeaseLabelTime(now.Add(time.Hour))}}
	if cleanup, reason := shouldCleanup(server, core.LeaseClaim{}, now); cleanup {
		t.Fatalf("active startup cleanup=%v reason=%q", cleanup, reason)
	}
	if cleanup, reason := shouldCleanup(server, core.LeaseClaim{}, now.Add(2*time.Hour)); !cleanup || !strings.Contains(reason, "expired") {
		t.Fatalf("expired startup cleanup=%v reason=%q", cleanup, reason)
	}
	server.Status = "provisioning (stale)"
	if cleanup, reason := shouldCleanup(server, core.LeaseClaim{}, now); !cleanup || reason != "provisioning stale" {
		t.Fatalf("stale startup cleanup=%v reason=%q", cleanup, reason)
	}
}

func TestConfigureDefaultsAndWorkRoots(t *testing.T) {
	cfg := testConfig()
	configured, err := (Provider{}).Configure(cfg, core.Runtime{})
	mustNoError(t, err)
	got := configured.(*backend).cfg
	if got.TargetOS != core.TargetMacOS || got.Lume.User != "lume" || got.Lume.Base != "crabbox-macos-golden" {
		t.Fatalf("unexpected defaults: %#v", got.Lume)
	}
	cfg = core.BaseConfig()
	cfg.Lume.User = "builder"
	cfg.Lume.WorkRoot = "/Users/lume/crabbox"
	cfg.WorkRoot = "/Users/lume/crabbox"
	applyDefaults(&cfg)
	if cfg.Lume.WorkRoot != "/Users/builder/crabbox" || cfg.WorkRoot != "/Users/builder/crabbox" {
		t.Fatalf("work roots=%q %q want overridden user's home", cfg.Lume.WorkRoot, cfg.WorkRoot)
	}
}

func TestFlagsPreserveExplicitNonMacOSTargetForValidation(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider, cfg.TargetOS = providerName, core.TargetLinux
	core.MarkTargetExplicit(&cfg)
	cfg, err := applyTestFlags(t, cfg)
	if err != nil || cfg.TargetOS != core.TargetLinux {
		t.Fatalf("target=%q apply error=%v", cfg.TargetOS, err)
	}
	if _, err := (Provider{}).Configure(cfg, core.Runtime{}); err == nil || !strings.Contains(err.Error(), "supports target=macos only") {
		t.Fatalf("Configure error=%v", err)
	}
}

func TestFlagsApplyLumeConfiguration(t *testing.T) {
	cfg := testConfig()
	cfg, err := applyTestFlags(t, cfg,
		"--lume-cli", "/opt/lume/bin/lume",
		"--lume-base", "macos-xcode-golden",
		"--lume-storage", "external",
		"--lume-user", "builder",
		"--lume-work-root", "/Users/builder/work",
	)
	mustNoError(t, err)
	if cfg.Lume.CLIPath != "/opt/lume/bin/lume" || cfg.Lume.Base != "macos-xcode-golden" || cfg.Lume.Storage != "external" || cfg.Lume.User != "builder" || cfg.WorkRoot != "/Users/builder/work" {
		t.Fatalf("unexpected lume config: %#v workRoot=%q", cfg.Lume, cfg.WorkRoot)
	}
}

func TestDoctorRequiresStoppedBase(t *testing.T) {
	cfg := testConfig()
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		"--version": {Stdout: "0.3.16\n"},
		"ls":        {Stdout: `[{"name":"crabbox-macos-golden","os":"macOS","status":"stopped","locationName":"home"}]`},
	}}
	b := testBackend(cfg, runner)
	result, err := b.Doctor(context.Background(), core.DoctorRequest{})
	mustNoError(t, err)
	if !strings.Contains(result.Message, "base_state=stopped") || !strings.Contains(result.Message, "runtime=0.3.16") {
		t.Fatalf("doctor message=%q", result.Message)
	}
	if !strings.Contains(result.Message, "leases=0") {
		t.Fatalf("base VM must not be counted as a lease: %q", result.Message)
	}

	runner.responses["ls"] = core.LocalCommandResult{Stdout: `[{"name":"crabbox-macos-golden","os":"macOS","status":"running","locationName":"home"}]`}
	if _, err := b.Doctor(context.Background(), core.DoctorRequest{}); err == nil || !strings.Contains(err.Error(), "must be stopped") {
		t.Fatalf("Doctor running base error=%v", err)
	}
}

func TestActiveMacOSGuestCountIsHostWide(t *testing.T) {
	cfg := testConfig()
	cfg.Lume.Storage = "external"
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		"ls": {Stdout: `[
			{"name":"base","os":"macOS","status":"stopped"},
			{"name":"one","os":"macOS","status":"running"},
			{"name":"two","os":"macOS","status":"starting"},
			{"name":"linux","os":"Linux","status":"running"}
		]`},
	}}
	b := testBackend(cfg, runner)
	active, err := b.activeMacOSGuestCount(context.Background(), b.configForRun())
	mustNoError(t, err)
	if active != 2 {
		t.Fatalf("active=%d want 2", active)
	}
	if len(runner.calls) != 1 || strings.Contains(strings.Join(runner.calls[0].Args, " "), "--storage") {
		t.Fatalf("capacity inventory must not be storage-filtered: %#v", runner.calls)
	}
}

func TestAcquireRejectsThirdMacOSGuestBeforeClone(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := testConfig()
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		"ls": {Stdout: `[
			{"name":"one","os":"macOS","status":"running"},
			{"name":"two","os":"macOS","status":"running"}
		]`},
	}}
	b := testBackend(cfg, runner)
	_, err := b.Acquire(context.Background(), core.AcquireRequest{RequestedLeaseID: "cbx_capacity_test"})
	if err == nil || !strings.Contains(err.Error(), "2 of 2") {
		t.Fatalf("capacity error=%v", err)
	}
	for _, call := range runner.calls {
		if len(call.Args) > 0 && call.Args[0] == "clone" {
			t.Fatalf("clone ran after host capacity was exhausted: %#v", runner.calls)
		}
	}
}

func TestAcquireRefusesDestructiveRollbackAfterAmbiguousCloneFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	const leaseID = "cbx_clonefail1234"
	vmExists := false
	deleteCalled := false
	name := ""
	runner := &recordingRunner{}
	runner.hook = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if len(req.Args) == 0 {
			return core.LocalCommandResult{}, nil, false
		}
		switch req.Args[0] {
		case "ls":
			if vmExists {
				return core.LocalCommandResult{Stdout: fmt.Sprintf(`[{"name":%q,"os":"macOS","status":"stopped"}]`, name)}, nil, true
			}
			return core.LocalCommandResult{Stdout: `[]`}, nil, true
		case "clone":
			name = req.Args[2]
			vmExists = true
			return core.LocalCommandResult{ExitCode: 1, Stderr: "partial clone failure"}, errors.New("exit status 1"), true
		case "stop":
			return core.LocalCommandResult{}, nil, true
		case "get":
			if vmExists {
				return core.LocalCommandResult{Stdout: fmt.Sprintf(`[{"name":%q,"os":"macOS","status":"stopped"}]`, name)}, nil, true
			}
			return core.LocalCommandResult{ExitCode: 1, Stderr: "Error: Virtual machine not found: " + name}, errors.New("exit status 1"), true
		case "delete":
			deleteCalled = true
			vmExists = false
			return core.LocalCommandResult{}, nil, true
		default:
			return core.LocalCommandResult{}, nil, false
		}
	}
	cfg := testConfig()
	b := testBackend(cfg, runner)
	_, err := b.Acquire(context.Background(), core.AcquireRequest{
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
	if _, ok, claimErr := resolveLeaseClaimForProvider(leaseID); claimErr != nil || ok {
		t.Fatalf("claim residue ok=%v err=%v", ok, claimErr)
	}
	keyPath, keyErr := testboxKeyPath(leaseID)
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	if _, statErr := os.Stat(keyPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("lease key residue: %v", statErr)
	}
}

func TestParseLumeVMsSkipsTimestampedStdoutLogs(t *testing.T) {
	output := "[2026-07-17T01:19:17Z] INFO: Cleaned up stale session file\n" +
		`[{"name":"worker-1","status":"running","ipAddress":"192.0.2.10"}]` +
		"\n[2026-07-17T01:19:18Z] INFO: done\n"
	instances, err := parseLumeVMs(output)
	mustNoError(t, err)
	if len(instances) != 1 || instances[0].Name != "worker-1" || instances[0].IPAddress != "192.0.2.10" {
		t.Fatalf("instances=%#v", instances)
	}
}

func TestFlagsRejectWorkRootTraversal(t *testing.T) {
	cfg := testConfig()
	if _, err := applyTestFlags(t, cfg, "--lume-work-root", "/Users/lume/../../etc"); err == nil || !strings.Contains(err.Error(), "must be beneath /Users/lume") {
		t.Fatalf("traversal error=%v", err)
	}
}

func TestFlagsPreserveStoragePathForExistingLeaseLifecycle(t *testing.T) {
	cfg := testConfig()
	cfg, err := applyTestFlags(t, cfg, "--lume-storage", "/Volumes/VMs")
	if err != nil || cfg.Lume.Storage != "/Volumes/VMs" {
		t.Fatalf("storage=%q apply error=%v", cfg.Lume.Storage, err)
	}
}

func TestAcquireRejectsStoragePathBeforeLumeMutation(t *testing.T) {
	for _, storage := range []string{"/Volumes/VMs", "ephemeral"} {
		cfg := testConfig()
		cfg.Lume.Storage = storage
		runner := &recordingRunner{}
		b := testBackend(cfg, runner)
		_, err := b.Acquire(context.Background(), core.AcquireRequest{RequestedLeaseID: "cbx_path_storage"})
		if err == nil || !strings.Contains(err.Error(), "existing lease lifecycle") {
			t.Fatalf("storage=%q Acquire error=%v", storage, err)
		}
		if len(runner.calls) != 0 {
			t.Fatalf("Lume called for path-backed storage=%q acquire: %#v", storage, runner.calls)
		}
	}
}

func TestStoragePathInventoryUsesExactGetForLifecycle(t *testing.T) {
	home := t.TempDir()
	storage := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	cfg := testConfig()
	cfg.Lume.Storage = storage
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		"get\x00crabbox-macos-golden\x00--format\x00json\x00--storage\x00" + storage: {
			Stdout: `[{"name":"crabbox-macos-golden","os":"macOS","status":"stopped"}]`,
		},
	}}
	b := testBackend(cfg, runner)
	instances, err := b.listInstancesForConfig(context.Background(), b.configForRun())
	mustNoError(t, err)
	if len(instances) != 1 || instances[0].Name != "crabbox-macos-golden" {
		t.Fatalf("instances=%#v", instances)
	}
	for _, call := range runner.calls {
		if len(call.Args) > 0 && call.Args[0] == "ls" {
			t.Fatalf("path lifecycle used unsupported list filter: %#v", runner.calls)
		}
	}
}

func TestDirectStoragePathMustExistBeforeLookup(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Lume.Storage = filepath.Join(t.TempDir(), "unmounted")
	runner := &recordingRunner{}
	b := testBackend(cfg, runner)
	if _, err := b.getInstance(context.Background(), cfg, "worker"); err == nil || !strings.Contains(err.Error(), "storage") {
		t.Fatalf("getInstance error=%v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("Lume called with unavailable storage: %#v", runner.calls)
	}
}

func TestListIncludesClaimFromPriorStorage(t *testing.T) {
	home := t.TempDir()
	storage := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	const leaseID, name = "cbx_prior_storage", "crabbox-prior-storage"
	server := core.Server{CloudID: name, Provider: providerName, Labels: map[string]string{"instance": name, "storage": storage, "storage_exact": "true", "state": "ready"}}
	mustNoError(t, core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, "prior", providerName, instanceScope(name), "", t.TempDir(), time.Minute, false, server, core.SSHTarget{}))
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		"ls": {Stdout: `[]`},
		"get\x00" + name + "\x00--format\x00json\x00--storage\x00" + storage: {Stdout: `[{"name":"crabbox-prior-storage","status":"running","ipAddress":"192.0.2.12"}]`},
	}}
	views, err := testBackend(core.BaseConfig(), runner).List(context.Background(), core.ListRequest{})
	mustNoError(t, err)
	if len(views) != 1 || views[0].CloudID != name || views[0].Status != "ready" {
		t.Fatalf("views=%#v", views)
	}
}

func TestLumeNotFoundClassificationIsSpecific(t *testing.T) {
	if !isLumeNotFoundError(errors.New("lume get failed: Error: Virtual machine not found: worker")) {
		t.Fatal("exact Lume VM-not-found error was not classified")
	}
	if isLumeNotFoundError(errors.New("exec: lume: executable file not found in $PATH")) {
		t.Fatal("missing CLI was classified as a missing VM")
	}
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{"ls": {Stdout: `[]`}}, errors: map[string]error{"get\x00worker\x00--format\x00json": errors.New("transient get failure")}}
	b := newBackend((Provider{}).Spec(), core.BaseConfig(), core.Runtime{Exec: runner}).(*backend)
	if _, _, err := b.observeVMState(context.Background(), b.configForRun(), "worker"); err == nil {
		t.Fatal("observe converted transient get failure to missing")
	}
	claim := core.LeaseClaim{LeaseID: "cbx_transient", Labels: map[string]string{"instance": "worker"}}
	if _, _, err := b.resolveClaimedInstance(context.Background(), claim); err == nil {
		t.Fatal("resolve converted transient get failure to missing")
	}
}

func TestOwnerForDestructionRejectsUnresolvedLaunch(t *testing.T) {
	claim := core.LeaseClaim{LeaseID: "cbx_pending", Labels: map[string]string{
		"run_owner_expected": "true",
		"run_owner_pending":  "true",
	}}
	if _, err := ownerForDestruction(claim); err == nil || !strings.Contains(err.Error(), "invalid pending launch metadata") {
		t.Fatalf("pending owner error=%v", err)
	}
	claim.Labels["run_owner_pending"] = "false"
	if _, err := ownerForDestruction(claim); err == nil || !strings.Contains(err.Error(), "without complete launch owner metadata") {
		t.Fatalf("missing owner error=%v", err)
	}
}

func TestOwnerForDestructionRecoversStalePendingLaunch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	token, err := newLaunchToken()
	mustNoError(t, err)
	handoff, err := prepareLaunchHandoff(token)
	mustNoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(handoff.Dir) })
	mustNoError(t, os.WriteFile(handoff.OwnerPath, []byte("99999999\n"), 0o600))
	claim := core.LeaseClaim{LeaseID: "cbx_stale_pending", Labels: map[string]string{
		"run_owner_expected": "true",
		"run_owner_pending":  "true",
		"run_launch_token":   token,
	}}
	owner, err := ownerForDestruction(claim)
	if err != nil || owner.PID != 0 {
		t.Fatalf("stale pending owner=%#v err=%v", owner, err)
	}
}

func TestConfigForClaimPinsLifecycleSettings(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Lume.Base = "current-base"
	cfg.Lume.Storage = "current-storage"
	cfg.Lume.User = "current-user"
	cfg.Lume.WorkRoot = "/Users/current-user/work"
	claim := core.LeaseClaim{Labels: map[string]string{
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

func TestConfigForClaimPreservesExactStorageLocation(t *testing.T) {
	for _, tc := range []struct{ label, exact, want string }{{"home", "", ""}, {"home", "true", "home"}, {"Home", "", "Home"}, {"unknown", "", ""}} {
		cfg := core.BaseConfig()
		cfg.Lume.Storage = "current-storage"
		claim := core.LeaseClaim{Labels: map[string]string{"storage": tc.label, "storage_exact": tc.exact}}
		if got := configForClaim(cfg, claim).Lume.Storage; got != tc.want {
			t.Fatalf("storage label %q exact=%q resolved to %q, want %q", tc.label, tc.exact, got, tc.want)
		}
	}
	labels := (&backend{}).serverFromInstance(lumeVM{LocationName: "home"}, core.LeaseClaim{}, core.BaseConfig()).Labels
	if labels["storage"] != "home" || labels["storage_exact"] != "true" {
		t.Fatal(labels)
	}
}

func TestTouchPreservesLifecycleRoutingLabels(t *testing.T) {
	cfg := testConfig()
	b := testBackend(cfg, &recordingRunner{})
	server := core.Server{Labels: map[string]string{
		"storage":              "home",
		"instance":             "worker-1",
		"run_owner_pid":        "1234",
		"run_owner_started_at": "2026-07-16T00:00:00Z",
		"run_log":              "/tmp/worker-1.log",
	}}
	got, err := b.Touch(context.Background(), core.TouchRequest{Lease: core.LeaseTarget{Server: server}, State: "ready"})
	mustNoError(t, err)
	for key, want := range server.Labels {
		if got.Labels[key] != want {
			t.Fatalf("label %s=%q want %q", key, got.Labels[key], want)
		}
	}
}

func TestStopAcceptsStoppedStateAfterSignalExit(t *testing.T) {
	cfg := testConfig()
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			"stop\x00worker-1": {ExitCode: 130, Stderr: "interrupted"},
			"get":              {Stdout: `[{"name":"worker-1","status":"stopped"}]`},
		},
		errors: map[string]error{"stop\x00worker-1": errors.New("exit status 130")},
	}
	b := testBackend(cfg, runner)
	mustNoError(t, b.stopVM(context.Background(), b.configForRun(), "worker-1", lumeRunOwner{}))
}

func TestDeleteRefusesRunningVM(t *testing.T) {
	cfg := testConfig()
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		"get": {Stdout: `[{"name":"worker-1","status":"running"}]`},
	}}
	b := testBackend(cfg, runner)
	if err := b.deleteVM(context.Background(), b.configForRun(), "worker-1", lumeRunOwner{}); err == nil || !strings.Contains(err.Error(), "refusing to delete") {
		t.Fatalf("delete running error=%v", err)
	}
	for _, call := range runner.calls {
		if len(call.Args) > 0 && call.Args[0] == "delete" {
			t.Fatalf("delete command ran for a running VM: %#v", runner.calls)
		}
	}
}

func TestReleaseRequiresExactClaimAndRemovesItAfterDelete(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	const leaseID = "cbx_release123456"
	const name = "crabbox-release-1234"
	const originalMachineID = "bHVtZS1tYWNoaW5lLW9yaWdpbmFs"
	writeLumeVMConfig(t, home, name, originalMachineID)
	vmExists := true
	vmRunning := false
	vmJSON := func() string {
		return fmt.Sprintf(`[{"name":"crabbox-release-1234","os":"macOS","status":%q,"locationName":"home"}]`, map[bool]string{false: "stopped", true: "running"}[vmRunning])
	}
	runner := &recordingRunner{}
	runner.hook = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if len(req.Args) == 0 {
			return core.LocalCommandResult{}, nil, false
		}
		switch req.Args[0] {
		case "ls":
			if vmExists {
				return core.LocalCommandResult{Stdout: vmJSON()}, nil, true
			}
			return core.LocalCommandResult{Stdout: `[]`}, nil, true
		case "get":
			if vmExists {
				return core.LocalCommandResult{Stdout: vmJSON()}, nil, true
			}
			return core.LocalCommandResult{ExitCode: 1, Stderr: "Error: Virtual machine not found: crabbox-release-1234"}, errors.New("exit status 1"), true
		case "stop":
			vmRunning = false
			return core.LocalCommandResult{}, nil, true
		case "delete":
			vmExists = false
			return core.LocalCommandResult{}, nil, true
		default:
			return core.LocalCommandResult{}, nil, false
		}
	}
	cfg := testConfig()
	b := testBackend(cfg, runner)
	lease := core.LeaseTarget{
		LeaseID: leaseID,
		Server: core.Server{CloudID: name, Labels: map[string]string{
			"lease": leaseID, "instance": name, "storage": "home", "base": "crabbox-macos-golden",
			"ssh_user": "lume", "work_root": "/Users/lume/crabbox",
		}},
	}
	immutableID, err := lumeVMImmutableID(b.configForRun(), lumeVM{Name: name, LocationName: "home"})
	mustNoError(t, err)
	lease.Server.ImmutableID = immutableID
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err == nil || !strings.Contains(err.Error(), "unclaimed") {
		t.Fatalf("unclaimed release error=%v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("unclaimed release called Lume: %#v", runner.calls)
	}
	mustNoError(t, core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, "release", providerName, instanceScope(name), "", t.TempDir(), time.Minute, false, lease.Server, core.SSHTarget{}))
	writeLumeVMConfig(t, home, name, "bHVtZS1tYWNoaW5lLXJlcGxhY2VtZW50")
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("replacement release error=%v", err)
	}
	if !vmExists {
		t.Fatal("replacement VM was deleted")
	}
	if _, ok, err := resolveLeaseClaimForProvider(leaseID); err != nil || !ok {
		t.Fatalf("claim after replacement refusal ok=%v err=%v", ok, err)
	}
	writeLumeVMConfig(t, home, name, originalMachineID)
	vmRunning = true
	remoteCleanupCalled := false
	mustNoError(t, b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease, GuardedRemoteCleanup: func(context.Context, core.LeaseTarget) { remoteCleanupCalled = true }}))
	if remoteCleanupCalled {
		t.Fatal("guarded cleanup ran without a prepared SSH endpoint")
	}
	if vmExists {
		t.Fatal("claimed VM was not deleted")
	}
	if _, ok, err := resolveLeaseClaimForProvider(leaseID); err != nil || ok {
		t.Fatalf("claim after release ok=%v err=%v", ok, err)
	}
	deleted := false
	for _, call := range runner.calls {
		if len(call.Args) > 0 && call.Args[0] == "delete" {
			deleted = true
		}
	}
	if !deleted {
		t.Fatalf("delete call missing: %#v", runner.calls)
	}
}

func TestPrepareLeaseUsesPerLeaseKnownHosts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := testConfig()
	leaseID := "cbx_00000000-0000-0000-0000-000000000001"
	writeLumeKnownHost(t, leaseID, "worker-1", testLumeHostKey)
	claim := core.LeaseClaim{LeaseID: leaseID, Labels: map[string]string{"instance": "worker-1", "state": "ready"}}
	b := testBackend(cfg, &recordingRunner{})
	lease, err := b.prepareLease(context.Background(), b.configForRun(), lumeVM{Name: "worker-1", Status: "running", IPAddress: "192.0.2.10"}, claim, false)
	mustNoError(t, err)
	if lease.SSH.DisableHostKeyChecking || filepath.Base(lease.SSH.KnownHostsFile) != "known_hosts" || !strings.Contains(lease.SSH.KnownHostsFile, leaseID) || lease.SSH.HostKeyAlias != lumeHostKeyAlias("worker-1") {
		t.Fatalf("known hosts not isolated: %#v", lease.SSH)
	}
	if !lease.SSH.SSHConfigProxy {
		t.Fatal("SSHConfigProxy = false, want OpenSSH readiness for the local Lume guest")
	}
}

func TestRebindResolvedLeaseTargetRestoresHostKeyAlias(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const leaseID = "cbx_rebind123456"
	writeLumeKnownHost(t, leaseID, "worker-1", testLumeHostKey)
	target := core.LeaseTarget{Server: core.Server{CloudID: "worker-1", Labels: map[string]string{"state": "ready"}}}
	b := &backend{}
	mustNoError(t, b.RebindResolvedLeaseTarget(&target, leaseID))
	if target.SSH.HostKeyAlias != lumeHostKeyAlias("worker-1") {
		t.Fatalf("host key alias=%q", target.SSH.HostKeyAlias)
	}
}

func TestRebindResolvedLeaseTargetRequiresAuthenticatedPin(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	b := &backend{}
	for _, state := range []string{"starting", "ready"} {
		target := core.LeaseTarget{Server: core.Server{CloudID: "worker-1", Labels: map[string]string{"state": state}}}
		if err := b.RebindResolvedLeaseTarget(&target, "cbx_untrusted_"+state); err == nil {
			t.Fatalf("state=%s accepted without authenticated host-key pin", state)
		}
	}
	const leaseID = "cbx_untrusted_malformed"
	writeLumeKnownHost(t, leaseID, "worker-1", "AQ==")
	target := core.LeaseTarget{Server: core.Server{CloudID: "worker-1", Labels: map[string]string{"state": "ready"}}}
	if err := b.RebindResolvedLeaseTarget(&target, leaseID); err == nil {
		t.Fatal("accepted malformed SSH key blob as authenticated pin")
	}
}

func TestLumeVMImmutableIDChangesWithMachineIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	const name = "worker-identity"
	writeLumeVMConfig(t, home, name, "bHVtZS1tYWNoaW5lLW9uZQ==")
	cfg := core.BaseConfig()
	first, err := lumeVMImmutableID(cfg, lumeVM{Name: name, LocationName: "home"})
	mustNoError(t, err)
	writeLumeVMConfig(t, home, name, "bHVtZS1tYWNoaW5lLXR3bw==")
	second, err := lumeVMImmutableID(cfg, lumeVM{Name: name, LocationName: "home"})
	mustNoError(t, err)
	if first == second || !strings.HasPrefix(first, "lume-machine-") || !strings.HasPrefix(second, "lume-machine-") {
		t.Fatalf("immutable IDs first=%q second=%q", first, second)
	}
}

func TestLumeVMImmutableIDUsesConfiguredStorageLocation(t *testing.T) {
	home := t.TempDir()
	storageRoot := filepath.Join(home, "lume-fast")
	t.Setenv("HOME", home)
	writeLumeSettings(t, home, fmt.Sprintf("defaultLocationName: fast\nvmLocations:\n  - name: fast\n    path: %q\n", storageRoot))
	const name = "worker-fast"
	writeLumeVMConfigAt(t, storageRoot, name, "bHVtZS1mYXN0")
	if _, err := lumeVMImmutableID(core.BaseConfig(), lumeVM{Name: name, LocationName: "fast"}); err != nil {
		t.Fatal(err)
	}
}

func TestLumeVMImmutableIDDefaultsMissingLocationsToHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeLumeSettings(t, home, "telemetryEnabled: false\n")
	writeLumeVMConfig(t, home, "worker-home-default", "bHVtZS1ob21lLWRlZmF1bHQ=")
	if _, err := lumeVMImmutableID(core.BaseConfig(), lumeVM{Name: "worker-home-default", LocationName: "home"}); err != nil {
		t.Fatal(err)
	}
}

func TestLumeStorageKeywordsAndLocationsAreCaseSensitive(t *testing.T) {
	home := t.TempDir()
	storageRoot := filepath.Join(home, "capital-ephemeral")
	t.Setenv("HOME", home)
	writeLumeSettings(t, home, fmt.Sprintf("defaultLocationName: Ephemeral\nvmLocations:\n  - name: Ephemeral\n    path: %q\n", storageRoot))
	const name = "worker-capital-ephemeral"
	writeLumeVMConfigAt(t, storageRoot, name, "bHVtZS1jYXNl")
	cfg := core.BaseConfig()
	cfg.Lume.Storage = "Ephemeral"
	if isDirectStoragePath(cfg.Lume.Storage) {
		t.Fatal("configured location Ephemeral treated as ephemeral storage keyword")
	}
	if _, err := lumeVMImmutableID(cfg, lumeVM{Name: name, LocationName: "Ephemeral"}); err != nil {
		t.Fatal(err)
	}
	if !isDirectStoragePath("ephemeral") {
		t.Fatal("lowercase ephemeral storage keyword not recognized")
	}
}

func TestCloneUsesConfiguredStorage(t *testing.T) {
	cfg := testConfig()
	cfg.Lume.Base = "golden"
	cfg.Lume.Storage = "fast"
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(cfg, runner)
	mustNoError(t, b.cloneVM(context.Background(), b.configForRun(), "worker-1"))
	if len(runner.calls) != 1 {
		t.Fatalf("calls=%d", len(runner.calls))
	}
	want := "clone\x00golden\x00worker-1\x00--source-storage\x00fast\x00--dest-storage\x00fast"
	if got := strings.Join(runner.calls[0].Args, "\x00"); got != want {
		t.Fatalf("clone args=%q want %q", got, want)
	}
}

func TestPrepareBootstrapTrustCarriesKeyWithoutNetworkPassword(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	publicKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest crabbox-test"
	trust, err := prepareBootstrapTrust("worker-1", "lume", publicKey)
	mustNoError(t, err)
	defer removeBootstrapTrust(trust)
	secondTrust, err := prepareBootstrapTrust("worker-1", "lume", publicKey)
	mustNoError(t, err)
	defer removeBootstrapTrust(secondTrust)
	if secondTrust.Dir == trust.Dir {
		t.Fatalf("bootstrap directories were reused: %s", trust.Dir)
	}
	info, err := os.Stat(trust.Dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("trust directory info=%#v err=%v", info, err)
	}
	for name, want := range map[string]string{
		"challenge":      trust.Challenge,
		"ssh_user":       "lume",
		"authorized_key": publicKey,
	} {
		data, readErr := os.ReadFile(filepath.Join(trust.Dir, name))
		if readErr != nil || strings.TrimSpace(string(data)) != want {
			t.Fatalf("%s=%q err=%v want %q", name, data, readErr, want)
		}
	}
}

func TestWaitForGuestIdentityPinsKeyFromVirtioFSChallenge(t *testing.T) {
	dir := t.TempDir()
	trust := bootstrapTrust{Dir: dir, Challenge: "test-challenge"}
	identity := "test-challenge 00112233-4455-6677-8899-AABBCCDDEEFF ssh-ed25519 " + testLumeHostKey + "\n"
	mustNoError(t, os.WriteFile(filepath.Join(dir, "identity"), []byte(identity), 0o600))
	knownHosts := filepath.Join(dir, "known_hosts")
	b := &backend{}
	platformUUID, err := b.waitForGuestIdentity(context.Background(), "worker-1", "192.0.2.10", trust, knownHosts)
	mustNoError(t, err)
	if platformUUID != "00112233-4455-6677-8899-AABBCCDDEEFF" {
		t.Fatalf("platform UUID=%q", platformUUID)
	}
	data, err := os.ReadFile(knownHosts)
	mustNoError(t, err)
	got := string(data)
	if !strings.Contains(got, lumeHostKeyAlias("worker-1")+" ssh-ed25519 "+testLumeHostKey) || strings.Contains(got, "192.0.2.10") {
		t.Fatalf("known_hosts=%q", got)
	}
	identity = "test-challenge 00112233-4455-6677-8899-AABBCCDDEEFF ssh-ed25519 AQ==\n"
	mustNoError(t, os.WriteFile(filepath.Join(dir, "identity"), []byte(identity), 0o600))
	if _, err := pinBootstrapHostKey("192.0.2.10", lumeHostKeyAlias("worker-1"), trust, filepath.Join(dir, "known_hosts")); err == nil {
		t.Fatal("accepted malformed SSH key blob from bootstrap identity")
	}
}

func TestWaitForRunningVMIgnoresLumeSSHAvailableFalseNegative(t *testing.T) {
	cfg := testConfig()
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		"get": {Stdout: `[{"name":"worker-1","os":"macOS","status":"running","ipAddress":"192.0.2.10","sshAvailable":false}]`},
	}}
	b := testBackend(cfg, runner)
	visible := false
	inst, err := b.waitForRunningVM(context.Background(), b.configForRun(), "worker-1", lumeRunOwner{}, func() { visible = true })
	mustNoError(t, err)
	if !visible || inst.IPAddress != "192.0.2.10" || inst.SSHAvailable == nil || *inst.SSHAvailable {
		t.Fatalf("instance=%#v", inst)
	}
}

func TestWaitForRunningVMReportsEarlyOwnerExit(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "owner.log")
	mustNoError(t, os.WriteFile(logPath, []byte("capacity unavailable\n"), 0o600))
	cfg := testConfig()
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		"get": {Stdout: `[{"name":"worker-1","status":"stopped"}]`},
	}}
	b := testBackend(cfg, runner)
	_, err := b.waitForRunningVM(context.Background(), b.configForRun(), "worker-1", lumeRunOwner{PID: 2147483647, StartIdentity: "missing", LogPath: logPath}, func() {})
	if err == nil || !strings.Contains(err.Error(), "owner exited during startup: capacity unavailable") {
		t.Fatalf("owner exit error=%v", err)
	}
}
