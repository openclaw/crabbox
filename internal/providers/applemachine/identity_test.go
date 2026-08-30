package applemachine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type machineFixture struct {
	root     string
	machines map[string]machine
	runner   *recordingRunner
	before   func(core.LocalCommandRequest) (core.LocalCommandResult, error, bool)
}

func newMachineFixture(t *testing.T, runner *recordingRunner) *machineFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &machineFixture{root: root, machines: map[string]machine{}, runner: runner}
	runner.hook = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if f.before != nil {
			if result, err, handled := f.before(req); handled {
				return result, err, true
			}
		}
		args := req.Args
		if strings.Join(args, " ") == "system status --format json" {
			data, _ := json.Marshal(map[string]string{"status": "running", "appRoot": f.root})
			return core.LocalCommandResult{Stdout: string(data)}, nil, true
		}
		if len(args) < 3 || args[0] != "machine" {
			return core.LocalCommandResult{}, nil, false
		}
		switch args[1] {
		case "create":
			name := args[3]
			f.add(t, name)
			return core.LocalCommandResult{}, nil, true
		case "inspect":
			item, ok := f.machines[args[2]]
			if !ok {
				return core.LocalCommandResult{}, errors.New("not found"), true
			}
			data, _ := json.Marshal([]machine{item})
			return core.LocalCommandResult{Stdout: string(data)}, nil, true
		case "list":
			items := []machine{}
			for _, item := range f.machines {
				items = append(items, item)
			}
			data, _ := json.Marshal(items)
			return core.LocalCommandResult{Stdout: string(data)}, nil, true
		case "rm":
			if _, ok := f.machines[args[2]]; !ok {
				return core.LocalCommandResult{}, errors.New("not found"), true
			}
			dir, err := machineDirectory(f.root, args[2])
			if err != nil {
				return core.LocalCommandResult{}, err, true
			}
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}
			delete(f.machines, args[2])
			return core.LocalCommandResult{}, nil, true
		}
		return core.LocalCommandResult{}, nil, false
	}
	return f
}

func (f *machineFixture) add(t *testing.T, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(f.root, "plugin-state", "machine-apiserver", "machines", name), 0o700); err != nil {
		t.Fatal(err)
	}
	f.machines[name] = machine{ID: name, Status: "stopped"}
}

func (f *machineFixture) claim(t *testing.T, leaseID, slug, repo string) core.LeaseClaim {
	t.Helper()
	name := machineName(leaseID)
	f.add(t, name)
	identity, err := createMachineIdentity(f.root, name)
	if err != nil {
		t.Fatal(err)
	}
	b := testBackend(f.runner)
	server := machineServer(f.machines[name], leaseID, slug, b.cfg)
	server.ImmutableID = identity
	server.Labels["apple_machine_storage"] = f.root
	claim, err := core.ClaimLeaseTargetForRepoConfigScopeIfUnchangedDurable(leaseID, slug, b.cfg, f.root, server, core.SSHTarget{}, repo, time.Hour, false, core.LeaseClaim{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Revision == "" {
		t.Fatal("claim not published")
	}
	return claim
}

func identityFixture(t *testing.T) (*backend, *machineFixture, core.LeaseClaim) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	runner := &recordingRunner{}
	f := newMachineFixture(t, runner)
	claim := f.claim(t, "cbx_0123456789ab", "test-machine", t.TempDir())
	return testBackend(runner), f, claim
}

func requireClaimUnchanged(t *testing.T, claim core.LeaseClaim) {
	t.Helper()
	got, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID)
	if err != nil || !exists || !reflect.DeepEqual(got, claim) {
		t.Fatalf("claim changed: exists=%v err=%v", exists, err)
	}
}

func requireNoMachineMutation(t *testing.T, f *machineFixture) {
	t.Helper()
	for _, req := range f.runner.requests {
		if len(req.Args) > 1 && req.Args[0] == "machine" && (req.Args[1] == "rm" || req.Args[1] == "run" || req.Args[1] == "create") {
			t.Fatalf("unexpected mutation: %v", req.Args)
		}
	}
}

func TestStopRejectsUnboundAndReplacedMachines(t *testing.T) {
	for _, kind := range []string{"legacy", "wrong-provider", "wrong-scope", "wrong-name", "wrong-label", "missing-marker", "replacement", "symlink-marker", "symlink-bundle", "malformed-marker", "daemon-root-changed"} {
		t.Run(kind, func(t *testing.T) {
			b, f, claim := identityFixture(t)
			dir, _ := machineDirectory(f.root, claim.CloudID)
			marker := filepath.Join(dir, machineOwnershipFile)
			changed := claim
			changed.Labels = map[string]string{}
			for k, v := range claim.Labels {
				changed.Labels[k] = v
			}
			switch kind {
			case "legacy":
				changed.CloudID = ""
				changed.CloudImmutableID = ""
				changed.ProviderScope = ""
				changed.Labels = nil
			case "wrong-provider":
				changed.Provider = "tart"
			case "wrong-scope":
				changed.ProviderScope = f.root + "-other"
			case "wrong-name":
				changed.CloudID = "crabbox-other"
			case "wrong-label":
				changed.Labels["lease"] = "cbx_ffffffffffff"
			case "missing-marker":
				if err := os.Remove(marker); err != nil {
					t.Fatal(err)
				}
			case "replacement":
				if err := os.WriteFile(marker, []byte(strings.Repeat("a", 64)+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink-marker":
				if err := os.Rename(marker, marker+"-saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(marker+"-saved", marker); err != nil {
					t.Skip(err)
				}
			case "symlink-bundle":
				if err := os.Rename(dir, dir+"-saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(dir+"-saved", dir); err != nil {
					t.Skip(err)
				}
			case "malformed-marker":
				if err := os.WriteFile(marker, []byte("not an identity"), 0600); err != nil {
					t.Fatal(err)
				}
			case "daemon-root-changed":
				f.root = t.TempDir()
			}
			if !reflect.DeepEqual(claim, changed) {
				var err error
				changed, err = core.ReplaceLeaseClaimIfUnchangedDurableReturning(claim.LeaseID, claim, changed)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := b.Stop(t.Context(), StopRequest{ID: claim.LeaseID}); err == nil {
				t.Fatal("unsafe stop succeeded")
			}
			requireNoMachineMutation(t, f)
			requireClaimUnchanged(t, changed)
		})
	}
}

func TestStopBoundStoppedMachineAndConfirmAbsence(t *testing.T) {
	b, f, claim := identityFixture(t)
	t.Setenv("CONTAINER_APP_ROOT", t.TempDir())
	if err := b.Stop(t.Context(), StopRequest{ID: claim.Slug}); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID); err != nil || exists {
		t.Fatalf("claim remains: %v %v", exists, err)
	}
	if len(f.machines) != 0 {
		t.Fatal("machine remains")
	}
	for _, req := range f.runner.requests {
		if len(req.Args) > 1 && req.Args[1] == "run" {
			t.Fatal("stop booted machine")
		}
	}
	last := f.runner.requests[len(f.runner.requests)-1]
	if strings.Join(last.Args, " ") != "machine list --format json" {
		t.Fatalf("no final inventory: %v", last.Args)
	}
}

func TestDeleteRetainsClaimWhenRemovalOrInventoryIsUncertain(t *testing.T) {
	for _, kind := range []string{"rm-failure", "still-present", "empty", "null", "invalid", "duplicate", "missing-id", "missing-status", "wrong-shape", "root-switch"} {
		t.Run(kind, func(t *testing.T) {
			b, f, claim := identityFixture(t)
			removed := false
			f.before = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
				key := strings.Join(req.Args, " ")
				if key == "machine rm "+claim.CloudID {
					if kind == "rm-failure" {
						return core.LocalCommandResult{}, errors.New("delete failed"), true
					}
					removed = true
					if kind == "still-present" {
						return core.LocalCommandResult{}, nil, true
					}
				}
				if removed && key == "system status --format json" && kind == "root-switch" {
					return core.LocalCommandResult{Stdout: `{"status":"running","appRoot":"/"}`}, nil, true
				}
				if removed && key == "machine list --format json" {
					outputs := map[string]string{"empty": "", "null": "null", "invalid": "[", "duplicate": `[{"id":"other","status":"stopped"},{"id":"other","status":"stopped"}]`, "missing-id": `[{"status":"stopped"}]`, "missing-status": `[{"id":"other"}]`, "wrong-shape": "{}"}
					if output, ok := outputs[kind]; ok {
						return core.LocalCommandResult{Stdout: output}, nil, true
					}
				}
				return core.LocalCommandResult{}, nil, false
			}
			if err := b.Stop(t.Context(), StopRequest{ID: claim.LeaseID}); err == nil {
				t.Fatal("uncertain deletion succeeded")
			}
			requireClaimUnchanged(t, claim)
		})
	}
}

func TestStopRetriesConfirmedAbsenceWithoutAnotherDelete(t *testing.T) {
	b, f, claim := identityFixture(t)
	removed := false
	f.before = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		key := strings.Join(req.Args, " ")
		if key == "machine rm "+claim.CloudID {
			removed = true
		}
		if removed && key == "machine list --format json" {
			return core.LocalCommandResult{}, errors.New("transient inventory failure"), true
		}
		return core.LocalCommandResult{}, nil, false
	}
	if err := b.Stop(t.Context(), StopRequest{ID: claim.LeaseID}); err == nil {
		t.Fatal("uncertain deletion succeeded")
	}
	requireClaimUnchanged(t, claim)
	if len(f.machines) != 0 {
		t.Fatal("fixture did not delete machine")
	}
	f.before = nil
	f.runner.requests = nil
	if err := b.Stop(t.Context(), StopRequest{ID: claim.LeaseID}); err != nil {
		t.Fatal(err)
	}
	requireNoMachineMutation(t, f)
	if _, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID); err != nil || exists {
		t.Fatal("confirmed absence retained claim")
	}
}

func TestAbsentInventoryDoesNotRetireExistingStorage(t *testing.T) {
	b, f, claim := identityFixture(t)
	delete(f.machines, claim.CloudID)
	if err := b.Stop(t.Context(), StopRequest{ID: claim.LeaseID}); err == nil {
		t.Fatal("retired claim while bundle still exists")
	}
	requireNoMachineMutation(t, f)
	requireClaimUnchanged(t, claim)
}

func TestCleanupRejectsChangedClaimBeforeProviderAdmission(t *testing.T) {
	b, f, claim := identityFixture(t)
	changed := claim
	changed.RepoRoot = t.TempDir()
	changed, err := core.ReplaceLeaseClaimIfUnchangedDurableReturning(claim.LeaseID, claim, changed)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.removeBoundLease(t.Context(), claim); err == nil {
		t.Fatal("stale cleanup succeeded")
	}
	if len(f.runner.requests) != 0 {
		t.Fatal("provider called before claim admission")
	}
	requireClaimUnchanged(t, changed)
}

func TestCleanupFencesClaimThroughProviderDeletion(t *testing.T) {
	b, f, claim := identityFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	f.before = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if len(req.Args) > 1 && req.Args[1] == "rm" {
			close(entered)
			<-release
		}
		return core.LocalCommandResult{}, nil, false
	}
	stopDone := make(chan error, 1)
	go func() { stopDone <- b.removeBoundLease(context.Background(), claim) }()
	<-entered
	writerStarted, writerDone := make(chan struct{}), make(chan error, 1)
	go func() { close(writerStarted); writerDone <- core.RemoveLeaseClaimIfUnchanged(claim.LeaseID, claim) }()
	<-writerStarted
	select {
	case err := <-writerDone:
		close(release)
		t.Fatalf("claim fence escaped: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-stopDone; err != nil {
		t.Fatal(err)
	}
	if err := <-writerDone; err == nil {
		t.Fatal("stale writer succeeded after deletion")
	}
}

func TestLegacyReuseCannotAdoptMachine(t *testing.T) {
	b, f, claim := identityFixture(t)
	legacy := claim
	legacy.CloudImmutableID = ""
	legacy, err := core.ReplaceLeaseClaimIfUnchangedDurableReturning(claim.LeaseID, claim, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.resolveLease(t.Context(), claim.LeaseID, t.TempDir(), true); err == nil {
		t.Fatal("legacy reuse succeeded")
	}
	requireClaimUnchanged(t, legacy)
	requireNoMachineMutation(t, f)
	if len(f.runner.requests) != 0 {
		t.Fatal("legacy binding reached native runtime")
	}
}

func TestReuseRetainsIdentityAndHonorsRepositoryReclaim(t *testing.T) {
	b, f, claim := identityFixture(t)
	repo := t.TempDir()
	if _, err := b.resolveLease(t.Context(), claim.LeaseID, repo, false); err == nil {
		t.Fatal("cross-repo reuse without reclaim succeeded")
	}
	requireClaimUnchanged(t, claim)
	updated, err := b.resolveLease(t.Context(), claim.LeaseID, repo, true)
	if err != nil {
		t.Fatal(err)
	}
	if updated.RepoRoot != repo || updated.CloudID != claim.CloudID || updated.CloudImmutableID != claim.CloudImmutableID || updated.ProviderScope != claim.ProviderScope || updated.Revision == claim.Revision {
		t.Fatalf("unexpected refresh: %+v", updated)
	}
	requireNoMachineMutation(t, f)
}

func TestAcquisitionRollbackRequiresOriginalBindingAndClaim(t *testing.T) {
	for _, kind := range []string{"ready-failure", "replacement", "binding-failure", "claim-appeared"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			originalGOOS, originalGOARCH := hostGOOS, hostGOARCH
			hostGOOS, hostGOARCH = "darwin", "arm64"
			t.Cleanup(func() { hostGOOS, hostGOARCH = originalGOOS, originalGOARCH })
			home, err := os.UserHomeDir()
			if err != nil {
				t.Fatal(err)
			}
			runner := &recordingRunner{}
			f := newMachineFixture(t, runner)
			b := testBackend(runner)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var leaseID string
			var successor core.LeaseClaim
			f.before = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
				if len(req.Args) > 3 && req.Args[1] == "create" {
					name := req.Args[3]
					leaseID = "cbx_" + strings.TrimPrefix(name, "crabbox-")
					if kind == "binding-failure" {
						return core.LocalCommandResult{}, nil, true
					}
					if kind == "claim-appeared" {
						if err := core.ClaimLeaseForRepoProvider(leaseID, "successor", providerName, home, time.Hour, false); err != nil {
							t.Fatal(err)
						}
						successor, _, err = core.ReadLeaseClaimWithPresence(leaseID)
						if err != nil {
							t.Fatal(err)
						}
					}
				}
				if len(req.Args) > 3 && req.Args[1] == "run" {
					if kind == "replacement" {
						dir, err := machineDirectory(f.root, req.Args[3])
						if err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(filepath.Join(dir, machineOwnershipFile), []byte(strings.Repeat("a", 64)+"\n"), 0600); err != nil {
							t.Fatal(err)
						}
					}
					cancel()
					return core.LocalCommandResult{}, context.Canceled, true
				}
				return core.LocalCommandResult{}, nil, false
			}
			if _, err := b.createLease(ctx, Repo{Root: filepath.Join(home, "src", "fixture")}, false, ""); err == nil {
				t.Fatal("failed acquisition succeeded")
			}
			removed := false
			for _, req := range runner.requests {
				if len(req.Args) > 1 && req.Args[1] == "rm" {
					removed = true
				}
			}
			if removed != (kind == "ready-failure") {
				t.Fatalf("cleanup=%v for %s", removed, kind)
			}
			if kind == "claim-appeared" {
				requireClaimUnchanged(t, successor)
			}
			if kind == "replacement" {
				if _, exists, err := core.ReadLeaseClaimWithPresence(leaseID); err != nil || !exists {
					t.Fatal("replacement lost claim")
				}
			}
			if kind == "ready-failure" {
				if _, exists, err := core.ReadLeaseClaimWithPresence(leaseID); err != nil || exists {
					t.Fatal("rollback left claim")
				}
				if len(f.machines) != 0 {
					t.Fatal("rollback left machine")
				}
			}
		})
	}
}

func TestAcquisitionRejectsEmptyRepositoryBeforeMutation(t *testing.T) {
	b, f, _ := identityFixture(t)
	originalGOOS, originalGOARCH := hostGOOS, hostGOARCH
	hostGOOS, hostGOARCH = "darwin", "arm64"
	defer func() { hostGOOS, hostGOARCH = originalGOOS, originalGOARCH }()
	if _, err := b.createLease(t.Context(), Repo{}, false, ""); err == nil {
		t.Fatal("empty repo accepted")
	}
	requireNoMachineMutation(t, f)
}

func TestControlRejectsPartialOrInvalidInspection(t *testing.T) {
	for _, output := range []string{"", "null", "[]", `{}`, `[{"id":"other","status":"running"}]`, `[{"id":"crabbox-test"}]`, `[{"id":"crabbox-test","status":"running"}] trailing`} {
		runner := &recordingRunner{fallback: core.LocalCommandResult{Stdout: output}}
		if _, err := testBackend(runner).inspectMachine(t.Context(), "crabbox-test"); err == nil {
			t.Fatalf("accepted %q", output)
		}
	}
	for _, result := range []core.LocalCommandResult{{ExitCode: 1, Stdout: `[{"id":"crabbox-test","status":"running"}]`}, {Stdout: strings.Repeat("x", 1024*1024+1)}} {
		runner := &recordingRunner{fallback: result}
		if _, err := testBackend(runner).inspectMachine(t.Context(), "crabbox-test"); err == nil {
			t.Fatal("accepted failed/oversized response")
		}
		if runner.requests[0].MaxCapturedOutputBytes == 0 {
			t.Fatal("unbounded control capture")
		}
	}
}

func TestStorageRootRequiresDaemonEvidence(t *testing.T) {
	for _, output := range []string{"", "null", `{}`, `{"status":"stopped","appRoot":"/"}`, `{"status":"running","appRoot":"relative"}`, `{"status":"running"}`} {
		runner := &recordingRunner{fallback: core.LocalCommandResult{Stdout: output}}
		if _, err := testBackend(runner).storageRoot(t.Context()); err == nil {
			t.Fatalf("accepted %q", output)
		}
	}
}
