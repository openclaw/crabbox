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
	dir := filepath.Join(home, ".lume", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data := fmt.Sprintf(`{"machineIdentifier":%q}`, machineIdentifier)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeLumeKnownHost(t *testing.T, leaseID, name, key string) {
	t.Helper()
	target := core.SSHTarget{}
	if err := core.UseLeaseKnownHosts(&target, leaseID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target.KnownHostsFile, []byte(lumeHostKeyAlias(name)+" ssh-ed25519 "+key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
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
}

func TestConfigureDefaultsAndWorkRoots(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	configured, err := (Provider{}).Configure(cfg, core.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
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
	cfg = core.BaseConfig()
	cfg.Lume.User = "lume"
	cfg.Lume.WorkRoot = "/Users/lume/crabbox"
	cfg.WorkRoot = "/Users/lume/other"
	applyDefaults(&cfg)
	if cfg.Lume.WorkRoot != "/Users/lume/crabbox" || cfg.WorkRoot != "/Users/lume/crabbox" {
		t.Fatalf("work roots=%q %q want provider-specific root", cfg.Lume.WorkRoot, cfg.WorkRoot)
	}
}

func TestConfigureRejectsExplicitNonMacOS(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	core.MarkTargetExplicit(&cfg)
	if _, err := (Provider{}).Configure(cfg, core.Runtime{}); err == nil {
		t.Fatal("Configure accepted explicit linux target")
	}
}

func TestFlagsPreserveExplicitNonMacOSTargetForValidation(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	core.MarkTargetExplicit(&cfg)
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := registerFlags(fs, cfg)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if err := applyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.TargetOS != core.TargetLinux {
		t.Fatalf("target=%q want explicit Linux target preserved", cfg.TargetOS)
	}
	if _, err := (Provider{}).Configure(cfg, core.Runtime{}); err == nil || !strings.Contains(err.Error(), "supports target=macos only") {
		t.Fatalf("Configure error=%v", err)
	}
}

func TestFlagsApplyLumeConfiguration(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := registerFlags(fs, cfg)
	if err := fs.Parse([]string{
		"--lume-cli", "/opt/lume/bin/lume",
		"--lume-base", "macos-xcode-golden",
		"--lume-storage", "external",
		"--lume-user", "builder",
		"--lume-work-root", "/Users/builder/work",
	}); err != nil {
		t.Fatal(err)
	}
	if err := applyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Lume.CLIPath != "/opt/lume/bin/lume" || cfg.Lume.Base != "macos-xcode-golden" || cfg.Lume.Storage != "external" || cfg.Lume.User != "builder" || cfg.WorkRoot != "/Users/builder/work" {
		t.Fatalf("unexpected lume config: %#v workRoot=%q", cfg.Lume, cfg.WorkRoot)
	}
}

func TestDoctorRequiresStoppedBase(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		"--version": {Stdout: "0.3.16\n"},
		"ls":        {Stdout: `[{"name":"crabbox-macos-golden","os":"macOS","status":"stopped","locationName":"home"}]`},
	}}
	b := newBackend((Provider{}).Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	result, err := b.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
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
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Lume.Storage = "external"
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		"ls": {Stdout: `[
			{"name":"base","os":"macOS","status":"stopped"},
			{"name":"one","os":"macOS","status":"running"},
			{"name":"two","os":"macOS","status":"starting"},
			{"name":"linux","os":"Linux","status":"running"}
		]`},
	}}
	b := newBackend((Provider{}).Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	active, err := b.activeMacOSGuestCount(context.Background(), b.configForRun())
	if err != nil {
		t.Fatal(err)
	}
	if active != 2 {
		t.Fatalf("active=%d want 2", active)
	}
	if len(runner.calls) != 1 || strings.Contains(strings.Join(runner.calls[0].Args, " "), "--storage") {
		t.Fatalf("capacity inventory must not be storage-filtered: %#v", runner.calls)
	}
}

func TestAcquireRejectsThirdMacOSGuestBeforeClone(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		"ls": {Stdout: `[
			{"name":"one","os":"macOS","status":"running"},
			{"name":"two","os":"macOS","status":"running"}
		]`},
	}}
	b := newBackend((Provider{}).Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
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
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend((Provider{}).Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
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
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 1 || instances[0].Name != "worker-1" || instances[0].IPAddress != "192.0.2.10" {
		t.Fatalf("instances=%#v", instances)
	}
}

func TestFlagsRejectWorkRootTraversal(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := registerFlags(fs, cfg)
	if err := fs.Parse([]string{"--lume-work-root", "/Users/lume/../../etc"}); err != nil {
		t.Fatal(err)
	}
	if err := applyFlags(&cfg, fs, values); err == nil || !strings.Contains(err.Error(), "must be beneath /Users/lume") {
		t.Fatalf("traversal error=%v", err)
	}
}

func TestFlagsPreserveStoragePathForExistingLeaseLifecycle(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := registerFlags(fs, cfg)
	if err := fs.Parse([]string{"--lume-storage", "/Volumes/VMs"}); err != nil {
		t.Fatal(err)
	}
	if err := applyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Lume.Storage != "/Volumes/VMs" {
		t.Fatalf("storage=%q", cfg.Lume.Storage)
	}
}

func TestAcquireRejectsStoragePathBeforeLumeMutation(t *testing.T) {
	for _, storage := range []string{"/Volumes/VMs", "ephemeral"} {
		cfg := core.BaseConfig()
		cfg.Provider = providerName
		cfg.Lume.Storage = storage
		runner := &recordingRunner{}
		b := newBackend((Provider{}).Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
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
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Lume.Storage = "/Volumes/VMs"
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		"get\x00crabbox-macos-golden\x00--format\x00json\x00--storage\x00/Volumes/VMs": {
			Stdout: `[{"name":"crabbox-macos-golden","os":"macOS","status":"stopped"}]`,
		},
	}}
	b := newBackend((Provider{}).Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	instances, err := b.listInstancesForConfig(context.Background(), b.configForRun())
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 1 || instances[0].Name != "crabbox-macos-golden" {
		t.Fatalf("instances=%#v", instances)
	}
	for _, call := range runner.calls {
		if len(call.Args) > 0 && call.Args[0] == "ls" {
			t.Fatalf("path lifecycle used unsupported list filter: %#v", runner.calls)
		}
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
	if err != nil {
		t.Fatal(err)
	}
	handoff, err := prepareLaunchHandoff(token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(handoff.Dir) })
	if err := os.WriteFile(handoff.OwnerPath, []byte("99999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
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
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend((Provider{}).Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: &recordingRunner{}}).(*backend)
	server := core.Server{Labels: map[string]string{
		"storage":              "home",
		"instance":             "worker-1",
		"run_owner_pid":        "1234",
		"run_owner_started_at": "2026-07-16T00:00:00Z",
		"run_log":              "/tmp/worker-1.log",
	}}
	got, err := b.Touch(context.Background(), core.TouchRequest{Lease: core.LeaseTarget{Server: server}, State: "ready"})
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range server.Labels {
		if got.Labels[key] != want {
			t.Fatalf("label %s=%q want %q", key, got.Labels[key], want)
		}
	}
}

func TestStopAcceptsStoppedStateAfterSignalExit(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			"stop\x00worker-1": {ExitCode: 130, Stderr: "interrupted"},
			"get":              {Stdout: `[{"name":"worker-1","status":"stopped"}]`},
		},
		errors: map[string]error{"stop\x00worker-1": errors.New("exit status 130")},
	}
	b := newBackend((Provider{}).Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	if err := b.stopVM(context.Background(), b.configForRun(), "worker-1", lumeRunOwner{}); err != nil {
		t.Fatalf("stop should reconcile the stopped state: %v", err)
	}
}

func TestDeleteRefusesRunningVM(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		"get": {Stdout: `[{"name":"worker-1","status":"running"}]`},
	}}
	b := newBackend((Provider{}).Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
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
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend((Provider{}).Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	lease := core.LeaseTarget{
		LeaseID: leaseID,
		Server: core.Server{CloudID: name, Labels: map[string]string{
			"lease": leaseID, "instance": name, "storage": "home", "base": "crabbox-macos-golden",
			"ssh_user": "lume", "work_root": "/Users/lume/crabbox",
		}},
	}
	immutableID, err := lumeVMImmutableID(b.configForRun(), lumeVM{Name: name, LocationName: "home"})
	if err != nil {
		t.Fatal(err)
	}
	lease.Server.ImmutableID = immutableID
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err == nil || !strings.Contains(err.Error(), "unclaimed") {
		t.Fatalf("unclaimed release error=%v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("unclaimed release called Lume: %#v", runner.calls)
	}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, "release", providerName, instanceScope(name), "", t.TempDir(), time.Minute, false, lease.Server, core.SSHTarget{}); err != nil {
		t.Fatal(err)
	}
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
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease, GuardedRemoteCleanup: func(context.Context, core.LeaseTarget) { remoteCleanupCalled = true }}); err != nil {
		t.Fatalf("claimed release: %v", err)
	}
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
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	leaseID := "cbx_00000000-0000-0000-0000-000000000001"
	writeLumeKnownHost(t, leaseID, "worker-1", testLumeHostKey)
	claim := core.LeaseClaim{LeaseID: leaseID, Labels: map[string]string{"instance": "worker-1", "state": "ready"}}
	b := newBackend((Provider{}).Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: &recordingRunner{}}).(*backend)
	lease, err := b.prepareLease(context.Background(), b.configForRun(), lumeVM{Name: "worker-1", Status: "running", IPAddress: "192.0.2.10"}, claim, false)
	if err != nil {
		t.Fatal(err)
	}
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
	if err := b.RebindResolvedLeaseTarget(&target, leaseID); err != nil {
		t.Fatal(err)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	writeLumeVMConfig(t, home, name, "bHVtZS1tYWNoaW5lLXR3bw==")
	second, err := lumeVMImmutableID(cfg, lumeVM{Name: name, LocationName: "home"})
	if err != nil {
		t.Fatal(err)
	}
	if first == second || !strings.HasPrefix(first, "lume-machine-") || !strings.HasPrefix(second, "lume-machine-") {
		t.Fatalf("immutable IDs first=%q second=%q", first, second)
	}
}

func TestLumeVMImmutableIDUsesConfiguredStorageLocation(t *testing.T) {
	home := t.TempDir()
	configHome := filepath.Join(home, ".config")
	storageRoot := filepath.Join(home, "lume-fast")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	if err := os.MkdirAll(filepath.Join(configHome, "lume"), 0o700); err != nil {
		t.Fatal(err)
	}
	settings := fmt.Sprintf("defaultLocationName: fast\nvmLocations:\n  - name: fast\n    path: %q\n", storageRoot)
	if err := os.WriteFile(filepath.Join(configHome, "lume", "config.yaml"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	const name = "worker-fast"
	if err := os.MkdirAll(filepath.Join(storageRoot, name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storageRoot, name, "config.json"), []byte(`{"machineIdentifier":"bHVtZS1mYXN0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := lumeVMImmutableID(core.BaseConfig(), lumeVM{Name: name, LocationName: "fast"}); err != nil {
		t.Fatal(err)
	}
}

func TestLumeVMImmutableIDDefaultsMissingLocationsToHome(t *testing.T) {
	home := t.TempDir()
	configHome := filepath.Join(home, ".config")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	if err := os.MkdirAll(filepath.Join(configHome, "lume"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configHome, "lume", "config.yaml"), []byte("telemetryEnabled: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const name = "worker-home-default"
	writeLumeVMConfig(t, home, name, "bHVtZS1ob21lLWRlZmF1bHQ=")
	if _, err := lumeVMImmutableID(core.BaseConfig(), lumeVM{Name: name, LocationName: "home"}); err != nil {
		t.Fatal(err)
	}
}

func TestLumeStorageKeywordsAndLocationsAreCaseSensitive(t *testing.T) {
	home := t.TempDir()
	configHome := filepath.Join(home, ".config")
	storageRoot := filepath.Join(home, "capital-ephemeral")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	if err := os.MkdirAll(filepath.Join(configHome, "lume"), 0o700); err != nil {
		t.Fatal(err)
	}
	settings := fmt.Sprintf("defaultLocationName: Ephemeral\nvmLocations:\n  - name: Ephemeral\n    path: %q\n", storageRoot)
	if err := os.WriteFile(filepath.Join(configHome, "lume", "config.yaml"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	const name = "worker-capital-ephemeral"
	if err := os.MkdirAll(filepath.Join(storageRoot, name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storageRoot, name, "config.json"), []byte(`{"machineIdentifier":"bHVtZS1jYXNl"}`), 0o600); err != nil {
		t.Fatal(err)
	}
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
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Lume.Base = "golden"
	cfg.Lume.Storage = "fast"
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := newBackend((Provider{}).Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	if err := b.cloneVM(context.Background(), b.configForRun(), "worker-1"); err != nil {
		t.Fatal(err)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	defer removeBootstrapTrust(trust)
	secondTrust, err := prepareBootstrapTrust("worker-1", "lume", publicKey)
	if err != nil {
		t.Fatal(err)
	}
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
	if err := os.WriteFile(filepath.Join(dir, "identity"), []byte(identity), 0o600); err != nil {
		t.Fatal(err)
	}
	knownHosts := filepath.Join(dir, "known_hosts")
	b := &backend{}
	platformUUID, err := b.waitForGuestIdentity(context.Background(), "worker-1", "192.0.2.10", trust, knownHosts)
	if err != nil {
		t.Fatal(err)
	}
	if platformUUID != "00112233-4455-6677-8899-AABBCCDDEEFF" {
		t.Fatalf("platform UUID=%q", platformUUID)
	}
	data, err := os.ReadFile(knownHosts)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, lumeHostKeyAlias("worker-1")+" ssh-ed25519 "+testLumeHostKey) || strings.Contains(got, "192.0.2.10") {
		t.Fatalf("known_hosts=%q", got)
	}
	identity = "test-challenge 00112233-4455-6677-8899-AABBCCDDEEFF ssh-ed25519 AQ==\n"
	if err := os.WriteFile(filepath.Join(dir, "identity"), []byte(identity), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := pinBootstrapHostKey("192.0.2.10", lumeHostKeyAlias("worker-1"), trust, filepath.Join(dir, "known_hosts")); err == nil {
		t.Fatal("accepted malformed SSH key blob from bootstrap identity")
	}
}

func TestWaitForRunningVMIgnoresLumeSSHAvailableFalseNegative(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		"get": {Stdout: `[{"name":"worker-1","status":"running","ipAddress":"192.0.2.10","sshAvailable":false}]`},
	}}
	b := newBackend((Provider{}).Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	inst, err := b.waitForRunningVM(context.Background(), b.configForRun(), "worker-1", lumeRunOwner{})
	if err != nil {
		t.Fatal(err)
	}
	if inst.IPAddress != "192.0.2.10" || inst.SSHAvailable == nil || *inst.SSHAvailable {
		t.Fatalf("instance=%#v", inst)
	}
}

func TestWaitForRunningVMReportsEarlyOwnerExit(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "owner.log")
	if err := os.WriteFile(logPath, []byte("capacity unavailable\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		"get": {Stdout: `[{"name":"worker-1","status":"stopped"}]`},
	}}
	b := newBackend((Provider{}).Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
	_, err := b.waitForRunningVM(context.Background(), b.configForRun(), "worker-1", lumeRunOwner{PID: 2147483647, StartIdentity: "missing", LogPath: logPath})
	if err == nil || !strings.Contains(err.Error(), "owner exited during startup: capacity unavailable") {
		t.Fatalf("owner exit error=%v", err)
	}
}
