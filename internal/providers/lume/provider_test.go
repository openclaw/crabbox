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

func TestConfigureDefaultsToMacOS(t *testing.T) {
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
}

func TestApplyDefaultsDerivesWorkRootFromOverriddenUser(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Lume.User = "builder"
	cfg.Lume.WorkRoot = "/Users/lume/crabbox"
	cfg.WorkRoot = "/Users/lume/crabbox"
	applyDefaults(&cfg)
	if cfg.Lume.WorkRoot != "/Users/builder/crabbox" || cfg.WorkRoot != "/Users/builder/crabbox" {
		t.Fatalf("work roots=%q %q want overridden user's home", cfg.Lume.WorkRoot, cfg.WorkRoot)
	}
}

func TestApplyDefaultsPreservesExplicitDefaultUserWorkRoot(t *testing.T) {
	cfg := core.BaseConfig()
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

func TestConfigurePreservesStoragePathFromConfig(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Lume.Storage = "/Volumes/VMs"
	configured, err := (Provider{}).Configure(cfg, core.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if configured.(*backend).cfg.Lume.Storage != "/Volumes/VMs" {
		t.Fatalf("storage=%q", configured.(*backend).cfg.Lume.Storage)
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

func TestStorageIdentityDistinguishesSameNameAcrossLocations(t *testing.T) {
	registered := lumeStorageIdentity(lumeVM{Name: "worker", LocationName: "fast"}, "")
	direct := lumeStorageIdentity(lumeVM{Name: "worker", LocationName: "/Volumes/VMs"}, "/Volumes/VMs")
	if registered == direct {
		t.Fatalf("storage identities collided: %q", registered)
	}
}

func TestLumeNotFoundClassificationIsSpecific(t *testing.T) {
	if !isLumeNotFoundError(errors.New("lume get failed: Error: Virtual machine not found: worker")) {
		t.Fatal("exact Lume VM-not-found error was not classified")
	}
	if isLumeNotFoundError(errors.New("exec: lume: executable file not found in $PATH")) {
		t.Fatal("missing CLI was classified as a missing VM")
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

func TestConfigForClaimTreatsHomeAndLegacyUnknownAsDefaultStorage(t *testing.T) {
	for _, label := range []string{"home", "unknown"} {
		cfg := core.BaseConfig()
		cfg.Lume.Storage = "current-storage"
		got := configForClaim(cfg, core.LeaseClaim{Labels: map[string]string{"storage": label}})
		if got.Lume.Storage != "" {
			t.Fatalf("storage label %q resolved to %q, want home storage", label, got.Lume.Storage)
		}
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
	vmExists := true
	runner := &recordingRunner{}
	runner.hook = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if len(req.Args) == 0 {
			return core.LocalCommandResult{}, nil, false
		}
		switch req.Args[0] {
		case "ls":
			if vmExists {
				return core.LocalCommandResult{Stdout: `[{"name":"crabbox-release-1234","os":"macOS","status":"stopped","locationName":"home"}]`}, nil, true
			}
			return core.LocalCommandResult{Stdout: `[]`}, nil, true
		case "get":
			if vmExists {
				return core.LocalCommandResult{Stdout: `[{"name":"crabbox-release-1234","os":"macOS","status":"stopped","locationName":"home"}]`}, nil, true
			}
			return core.LocalCommandResult{ExitCode: 1, Stderr: "Error: Virtual machine not found: crabbox-release-1234"}, errors.New("exit status 1"), true
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
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err == nil || !strings.Contains(err.Error(), "unclaimed") {
		t.Fatalf("unclaimed release error=%v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("unclaimed release called Lume: %#v", runner.calls)
	}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, "release", providerName, instanceScope(name), "", t.TempDir(), time.Minute, false, lease.Server, core.SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatalf("claimed release: %v", err)
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
	claim := core.LeaseClaim{LeaseID: leaseID, Labels: map[string]string{"instance": "worker-1"}}
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

func TestLumeHostKeyAliasEncodesUnsafeVMName(t *testing.T) {
	alias := lumeHostKeyAlias("worker name,[host]:22")
	if invalidLogName.MatchString(alias) || strings.ContainsAny(alias, " ,[]:") {
		t.Fatalf("unsafe host-key alias=%q", alias)
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
	const hostKey = "AAAAC3NzaC1lZDI1NTE5AAAAIEyJYp+0tWfIvx9RZ3k4LZ5bDXb3Y+N+UZxF6p8h"
	identity := "test-challenge 00112233-4455-6677-8899-AABBCCDDEEFF ssh-ed25519 " + hostKey + "\n"
	if err := os.WriteFile(filepath.Join(dir, "identity"), []byte(identity), 0o600); err != nil {
		t.Fatal(err)
	}
	knownHosts := filepath.Join(dir, "known_hosts")
	b := &backend{}
	if err := b.waitForGuestIdentity(context.Background(), "worker-1", "192.0.2.10", trust, knownHosts); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(knownHosts)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, lumeHostKeyAlias("worker-1")+" ssh-ed25519 "+hostKey) || strings.Contains(got, "192.0.2.10") {
		t.Fatalf("known_hosts=%q", got)
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
