package exedev

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

type exeDevRecordingRunner struct {
	calls []LocalCommandRequest
	fn    func(LocalCommandRequest) (LocalCommandResult, error)
}

func (r *exeDevRecordingRunner) Run(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	r.calls = append(r.calls, req)
	if r.fn != nil {
		return r.fn(req)
	}
	return LocalCommandResult{}, nil
}

func TestExeDevListFiltersCrabboxVMsByDefault(t *testing.T) {
	runner := &exeDevRecordingRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		base := []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new", "-o", "ConnectTimeout=10", "exe.dev", "ls --l --json"}
		if !reflect.DeepEqual(req.Args, base) {
			t.Fatalf("args=%v", req.Args)
		}
		return LocalCommandResult{Stdout: `{"vms":[{"vm_name":"crabbox-blue-12345678","ssh_dest":"crabbox-blue-12345678.exe.xyz","status":"running","tags":["crabbox","crabbox-lease-cbx_abcdef123456","crabbox-slug-blue"]},{"vm_name":"crabbox-manual-12345678","ssh_dest":"crabbox-manual-12345678.exe.xyz","status":"running"}]}`}, nil
	}}
	backend := &exeDevLeaseBackend{cfg: Config{}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	views, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Name != "crabbox-blue-12345678" || views[0].Provider != providerName {
		t.Fatalf("views=%#v", views)
	}
	views, err = backend.List(context.Background(), ListRequest{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 {
		t.Fatalf("views=%#v", views)
	}
}

func TestExeDevDoctorListsInventory(t *testing.T) {
	runner := &exeDevRecordingRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		got := strings.Join(req.Args, " ")
		if !strings.Contains(got, "exe.dev ls --l --json") {
			t.Fatalf("args=%v", req.Args)
		}
		if strings.Contains(got, " new ") || strings.Contains(got, " rm ") {
			t.Fatalf("doctor used mutating command: %v", req.Args)
		}
		return LocalCommandResult{Stdout: `{"vms":[{"vm_name":"crabbox-blue-12345678","ssh_dest":"crabbox-blue-12345678.exe.xyz","status":"running","tags":["crabbox","crabbox-lease-cbx_abcdef123456","crabbox-slug-blue"]}]}`}, nil
	}}
	doctor, err := Provider{}.ConfigureDoctor(Config{}, Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner})
	if err != nil {
		t.Fatal(err)
	}
	result, err := doctor.Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider != providerName || !strings.Contains(result.Message, "cli=ready") || !strings.Contains(result.Message, "api=list") || !strings.Contains(result.Message, "leases=1") {
		t.Fatalf("result=%#v", result)
	}
}

func TestExeDevCreateVMUsesSSHControlAPI(t *testing.T) {
	runner := &exeDevRecordingRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		got := strings.Join(req.Args, " ")
		for _, want := range []string{
			"exe.dev new",
			"--name crabbox-blue-12345678",
			"--json",
			"--tag crabbox",
			"--tag crabbox-lease-cbx_lease",
			"--tag crabbox-slug-blue",
			"--no-email",
			"--image ubuntu:24.04",
			"--cpu 4",
			"--memory 8GB",
			"--disk 40GB",
			"--command 'sleep infinity'",
		} {
			if !strings.Contains(got, want) {
				t.Fatalf("args=%v missing %q", req.Args, want)
			}
		}
		return LocalCommandResult{Stdout: `{"vm_name":"crabbox-blue-12345678","ssh_dest":"crabbox-blue-12345678.exe.xyz","status":"running"}`}, nil
	}}
	cfg := Config{ExeDev: ExeDevConfig{
		Image:   "ubuntu:24.04",
		CPUs:    4,
		Memory:  "8GB",
		Disk:    "40GB",
		Command: "sleep infinity",
		NoEmail: true,
	}}
	backend := &exeDevLeaseBackend{cfg: cfg, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	vm, err := backend.createVM(context.Background(), backend.configForRun(), "crabbox-blue-12345678", "cbx_lease", "blue")
	if err != nil {
		t.Fatal(err)
	}
	if vm.Name() != "crabbox-blue-12345678" || vm.SSHHost() != "crabbox-blue-12345678.exe.xyz" {
		t.Fatalf("vm=%#v", vm)
	}
}

func TestExeDevAcquireReportsRollbackFailureAfterPrepareFailure(t *testing.T) {
	primaryErr := errors.New("ssh not ready")
	oldWait := waitForSSHReady
	waitForSSHReady = func(context.Context, *SSHTarget, io.Writer, string, time.Duration) error {
		return primaryErr
	}
	t.Cleanup(func() { waitForSSHReady = oldWait })
	runner := newExeDevAcquireRollbackRunner()
	backend := &exeDevLeaseBackend{cfg: Config{}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	_, err := backend.Acquire(context.Background(), AcquireRequest{Repo: Repo{Root: t.TempDir()}})
	assertExeDevRollbackFailure(t, err, primaryErr, runner)
}

func TestExeDevAcquireReportsRollbackFailureAfterClaimFailure(t *testing.T) {
	primaryErr := errors.New("claim failed")
	oldWait := waitForSSHReady
	waitForSSHReady = func(context.Context, *SSHTarget, io.Writer, string, time.Duration) error {
		return nil
	}
	t.Cleanup(func() { waitForSSHReady = oldWait })
	oldClaim := claimLeaseTargetForRepoConfigIfUnchanged
	claimLeaseTargetForRepoConfigIfUnchanged = func(string, string, Config, Server, SSHTarget, string, time.Duration, bool, LeaseClaim, bool) (LeaseClaim, error) {
		return LeaseClaim{}, primaryErr
	}
	t.Cleanup(func() { claimLeaseTargetForRepoConfigIfUnchanged = oldClaim })
	runner := newExeDevAcquireRollbackRunner()
	backend := &exeDevLeaseBackend{cfg: Config{}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	_, err := backend.Acquire(context.Background(), AcquireRequest{Repo: Repo{Root: t.TempDir()}})
	assertExeDevRollbackFailure(t, err, primaryErr, runner)
}

func newExeDevAcquireRollbackRunner() *exeDevRecordingRunner {
	return &exeDevRecordingRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		cmd := strings.Join(req.Args, " ")
		switch {
		case strings.Contains(cmd, "ls --l --json"):
			return LocalCommandResult{Stdout: `{"vms":[]}`}, nil
		case strings.Contains(cmd, " new "):
			return LocalCommandResult{Stdout: `{"vm_name":"created-vm","ssh_dest":"created-vm.exe.xyz","status":"running"}`}, nil
		case strings.Contains(cmd, " rm "):
			return LocalCommandResult{ExitCode: 1}, errors.New("exit status 1")
		default:
			return LocalCommandResult{}, nil
		}
	}}
}

func assertExeDevRollbackFailure(t *testing.T, err error, primary error, runner *exeDevRecordingRunner) {
	t.Helper()
	if err == nil {
		t.Fatal("Acquire succeeded, want rollback failure")
	}
	var exitErr ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 1 {
		t.Fatalf("err=%#v, want rendered ExitError code 1", err)
	}
	for _, want := range []string{"exe.dev cleanup failed for VM", "manual cleanup: crabbox stop --provider exe-dev --id", "exit status 1", primary.Error()} {
		if !strings.Contains(exitErr.Message, want) {
			t.Fatalf("exit message=%q missing %q", exitErr.Message, want)
		}
	}
	for _, call := range runner.calls {
		if strings.Contains(strings.Join(call.Args, " "), " rm ") {
			return
		}
	}
	t.Fatalf("rollback did not call rm: %#v", runner.calls)
}

func TestExeDevDefaultsPreserveCustomTopLevelWorkRoot(t *testing.T) {
	cfg := Config{WorkRoot: "/custom/crabbox"}
	applyExeDevDefaults(&cfg)
	if cfg.WorkRoot != "/custom/crabbox" || cfg.ExeDev.WorkRoot != "/custom/crabbox" {
		t.Fatalf("workRoot=%q exeDev.workRoot=%q", cfg.WorkRoot, cfg.ExeDev.WorkRoot)
	}

	cfg = Config{WorkRoot: "/custom/crabbox", ExeDev: ExeDevConfig{WorkRoot: "/exe/crabbox"}}
	applyExeDevDefaults(&cfg)
	if cfg.WorkRoot != "/exe/crabbox" || cfg.ExeDev.WorkRoot != "/exe/crabbox" {
		t.Fatalf("workRoot=%q exeDev.workRoot=%q", cfg.WorkRoot, cfg.ExeDev.WorkRoot)
	}
}

func TestExeDevControlSurfacesJSONError(t *testing.T) {
	runner := &exeDevRecordingRunner{fn: func(LocalCommandRequest) (LocalCommandResult, error) {
		return LocalCommandResult{ExitCode: 1, Stdout: `{"error":"active plan required"}`}, errors.New("exit status 1")
	}}
	backend := &exeDevLeaseBackend{cfg: Config{}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	_, err := backend.controlOutput(context.Background(), []string{"new", "--json"})
	if err == nil || !strings.Contains(err.Error(), "active plan required") {
		t.Fatalf("err=%v", err)
	}
}

func TestExeDevControlRejectsSSHOptionLikeHost(t *testing.T) {
	runner := &exeDevRecordingRunner{}
	backend := &exeDevLeaseBackend{cfg: Config{ExeDev: ExeDevConfig{ControlHost: "-oProxyCommand=sh"}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	if _, err := backend.controlOutput(context.Background(), []string{"ls", "--json"}); err == nil || !strings.Contains(err.Error(), "invalid exe.dev control host") {
		t.Fatalf("err=%v, want invalid control host", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("ssh should not run for invalid host, calls=%v", runner.calls)
	}
}

func TestExeDevControlHostUsesSeparatePortArgument(t *testing.T) {
	runner := &exeDevRecordingRunner{}
	backend := &exeDevLeaseBackend{cfg: Config{ExeDev: ExeDevConfig{ControlHost: "alice@control.example:2222"}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	if _, err := backend.controlOutput(context.Background(), []string{"ls", "--json"}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls=%d, want 1", len(runner.calls))
	}
	want := []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new", "-o", "ConnectTimeout=10", "-p", "2222", "alice@control.example", "ls --json"}
	if !reflect.DeepEqual(runner.calls[0].Args, want) {
		t.Fatalf("args=%v want %v", runner.calls[0].Args, want)
	}
}

func TestExeDevControlHostPreservesBareIPv6Destination(t *testing.T) {
	runner := &exeDevRecordingRunner{}
	backend := &exeDevLeaseBackend{cfg: Config{ExeDev: ExeDevConfig{ControlHost: "alice@2001:db8::10"}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	if _, err := backend.controlOutput(context.Background(), []string{"ls", "--json"}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls=%d, want 1", len(runner.calls))
	}
	want := []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new", "-o", "ConnectTimeout=10", "alice@2001:db8::10", "ls --json"}
	if !reflect.DeepEqual(runner.calls[0].Args, want) {
		t.Fatalf("args=%v want %v", runner.calls[0].Args, want)
	}
}

func TestExeDevControlHostAcceptsBracketedIPv6Port(t *testing.T) {
	runner := &exeDevRecordingRunner{}
	backend := &exeDevLeaseBackend{cfg: Config{ExeDev: ExeDevConfig{ControlHost: "alice@[2001:db8::10]:2222"}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	if _, err := backend.controlOutput(context.Background(), []string{"ls", "--json"}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls=%d, want 1", len(runner.calls))
	}
	want := []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new", "-o", "ConnectTimeout=10", "-p", "2222", "alice@2001:db8::10", "ls --json"}
	if !reflect.DeepEqual(runner.calls[0].Args, want) {
		t.Fatalf("args=%v want %v", runner.calls[0].Args, want)
	}
}

func TestExeDevControlHostPreservesScopedIPv6Destination(t *testing.T) {
	runner := &exeDevRecordingRunner{}
	backend := &exeDevLeaseBackend{cfg: Config{ExeDev: ExeDevConfig{ControlHost: "alice@fe80::1%eth0"}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	if _, err := backend.controlOutput(context.Background(), []string{"ls", "--json"}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls=%d, want 1", len(runner.calls))
	}
	want := []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new", "-o", "ConnectTimeout=10", "alice@fe80::1%eth0", "ls --json"}
	if !reflect.DeepEqual(runner.calls[0].Args, want) {
		t.Fatalf("args=%v want %v", runner.calls[0].Args, want)
	}
}

func TestExeDevResolveVMUsesTaggedLeaseIdentity(t *testing.T) {
	runner := &exeDevRecordingRunner{fn: func(LocalCommandRequest) (LocalCommandResult, error) {
		return LocalCommandResult{Stdout: `{"vms":[{"vm_name":"crabbox-blue-12345678","ssh_dest":"crabbox-blue-12345678.exe.xyz","status":"running","tags":["crabbox","crabbox-lease-cbx_abcdef123456","crabbox-slug-blue"]}]}`}, nil
	}}
	backend := &exeDevLeaseBackend{cfg: Config{}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	_, leaseID, slug, err := backend.resolveVM(context.Background(), "crabbox-blue-12345678")
	if err != nil {
		t.Fatal(err)
	}
	if leaseID != "cbx_abcdef123456" || slug != "blue" {
		t.Fatalf("leaseID=%q slug=%q", leaseID, slug)
	}
}

func TestExeDevResolveCanonicalLeaseIDScansTags(t *testing.T) {
	leaseID := "cbx_abcdef123456"
	runner := &exeDevRecordingRunner{fn: func(LocalCommandRequest) (LocalCommandResult, error) {
		return LocalCommandResult{Stdout: `{"vms":[{"vm_name":"crabbox-custom-12345678","ssh_dest":"crabbox-custom-12345678.exe.xyz","status":"running","tags":["crabbox","crabbox-lease-` + leaseID + `","crabbox-slug-custom"]}]}`}, nil
	}}
	backend := &exeDevLeaseBackend{cfg: Config{}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	vm, gotLeaseID, slug, err := backend.resolveVM(context.Background(), leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if vm.Name() != "crabbox-custom-12345678" || gotLeaseID != leaseID || slug != "custom" {
		t.Fatalf("vm=%s leaseID=%q slug=%q", vm.Name(), gotLeaseID, slug)
	}
}

func TestExeDevResolveVMNameRecoversExistingClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_abcdef123456"
	slug := "blue-lobster"
	name := leaseProviderName(leaseID, slug)
	cfg := Config{Provider: providerName}
	applyExeDevDefaults(&cfg)
	vm := exeDevVM{VMName: name, SSHDest: name + ".exe.xyz", Status: "running"}
	if _, err := claimLeaseTargetForRepoConfigIfUnchanged(leaseID, slug, cfg, exeDevServer(vm, leaseID, slug, cfg, true), exeDevSSHTarget(cfg, vm), t.TempDir(), 0, false, LeaseClaim{}, false); err != nil {
		t.Fatal(err)
	}
	runner := &exeDevRecordingRunner{fn: func(LocalCommandRequest) (LocalCommandResult, error) {
		return LocalCommandResult{Stdout: `{"vms":[{"vm_name":"` + name + `","ssh_dest":"` + name + `.exe.xyz","status":"running"}]}`}, nil
	}}
	backend := &exeDevLeaseBackend{cfg: Config{}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	_, gotLeaseID, gotSlug, err := backend.resolveVM(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	if gotLeaseID != leaseID || gotSlug != slug {
		t.Fatalf("leaseID=%q slug=%q", gotLeaseID, gotSlug)
	}
}

func TestExeDevReleaseRejectsUnclaimedRawIdentifiers(t *testing.T) {
	vm := exeDevVM{VMName: "manual-box", SSHDest: "manual-box.exe.xyz", Status: "running"}
	for _, identifier := range []string{vm.Name(), "manual box", vm.SSHHost()} {
		t.Run(identifier, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			runner := exeDevInventoryRunner(t, vm)
			backend := newExeDevTestBackend(Config{}, runner)
			_, err := backend.Resolve(context.Background(), ResolveRequest{ID: identifier, ReleaseOnly: true})
			if err == nil || !strings.Contains(err.Error(), "no exact local claim") {
				t.Fatalf("err=%v, want exact local claim refusal", err)
			}
			assertNoExeDevRM(t, runner)
		})
	}
}

func TestExeDevReleaseRejectsTaggedVMWithoutLocalClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_abcdef123456"
	vm := ownedExeDevVM(leaseID, "blue")
	runner := exeDevInventoryRunner(t, vm)
	backend := newExeDevTestBackend(Config{}, runner)
	_, err := backend.Resolve(context.Background(), ResolveRequest{ID: vm.Name(), ReleaseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "no exact local claim") {
		t.Fatalf("err=%v, want exact local claim refusal", err)
	}
	assertNoExeDevRM(t, runner)
}

func TestExeDevReleaseRejectsClaimBoundToDifferentVM(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_abcdef123456"
	vm := ownedExeDevVM(leaseID, "blue")
	cfg := Config{Provider: providerName}
	applyExeDevDefaults(&cfg)
	other := vm
	other.VMName = "crabbox-other-12345678"
	persistExeDevClaim(t, cfg, other, leaseID, "blue", t.TempDir())
	runner := exeDevInventoryRunner(t, vm)
	backend := newExeDevTestBackend(cfg, runner)
	_, err := backend.Resolve(context.Background(), ResolveRequest{ID: vm.Name(), ReleaseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "not bound to an exact provider/resource claim") {
		t.Fatalf("err=%v, want exact resource binding refusal", err)
	}
	assertNoExeDevRM(t, runner)
}

func TestExeDevReleaseRequiresUnchangedClaimAndRemoteTags(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(t *testing.T, backend *exeDevLeaseBackend, runner *exeDevRecordingRunner, lease LeaseTarget, claim LeaseClaim, vm exeDevVM)
		want   string
	}{
		{
			name: "claim changed",
			mutate: func(t *testing.T, backend *exeDevLeaseBackend, _ *exeDevRecordingRunner, lease LeaseTarget, claim LeaseClaim, vm exeDevVM) {
				cfg := backend.configForRun()
				if _, err := claimLeaseTargetForRepoConfigIfUnchanged(lease.LeaseID, claim.Slug, cfg, lease.Server, exeDevSSHTarget(cfg, vm), t.TempDir(), cfg.IdleTimeout, true, claim, true); err != nil {
					t.Fatal(err)
				}
			},
			want: "claim changed",
		},
		{
			name: "control route changed",
			mutate: func(_ *testing.T, backend *exeDevLeaseBackend, _ *exeDevRecordingRunner, _ LeaseTarget, _ LeaseClaim, _ exeDevVM) {
				backend.cfg.ExeDev.ControlHost = "other.exe.dev"
			},
			want: "different exe.dev control route",
		},
		{
			name: "remote tags removed",
			mutate: func(t *testing.T, _ *exeDevLeaseBackend, runner *exeDevRecordingRunner, _ LeaseTarget, _ LeaseClaim, vm exeDevVM) {
				vm.Tags = nil
				runner.fn = exeDevInventoryResponse(t, vm)
			},
			want: "no complete Crabbox ownership tags",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			leaseID := "cbx_abcdef123456"
			vm := ownedExeDevVM(leaseID, "blue")
			runner := exeDevInventoryRunner(t, vm)
			backend := newExeDevTestBackend(Config{}, runner)
			claim := persistExeDevClaim(t, backend.configForRun(), vm, leaseID, "blue", t.TempDir())
			lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: vm.Name(), ReleaseOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(t, backend, runner, lease, claim, vm)
			err = backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err=%v, want %q", err, test.want)
			}
			assertNoExeDevRM(t, runner)
			if _, exists, readErr := readLeaseClaimWithPresence(leaseID); readErr != nil || !exists {
				t.Fatalf("claim exists=%v err=%v, want retained", exists, readErr)
			}
		})
	}
}

func TestExeDevReleaseDeletesExactlyClaimedVMAndRemovesClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_abcdef123456"
	vm := ownedExeDevVM(leaseID, "blue")
	runner := exeDevInventoryRunner(t, vm)
	backend := newExeDevTestBackend(Config{}, runner)
	persistExeDevClaim(t, backend.configForRun(), vm, leaseID, "blue", t.TempDir())
	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: vm.SSHHost(), ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if !hasExeDevRM(runner) {
		t.Fatalf("rm not called: %#v", runner.calls)
	}
	if _, exists, err := readLeaseClaimWithPresence(leaseID); err != nil || exists {
		t.Fatalf("claim exists=%v err=%v, want removed", exists, err)
	}
}

func TestExeDevReleaseRetainsClaimWhenDeleteFails(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_abcdef123456"
	vm := ownedExeDevVM(leaseID, "blue")
	runner := exeDevInventoryRunner(t, vm)
	backend := newExeDevTestBackend(Config{}, runner)
	persistExeDevClaim(t, backend.configForRun(), vm, leaseID, "blue", t.TempDir())
	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: vm.Name(), ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	runner.fn = func(req LocalCommandRequest) (LocalCommandResult, error) {
		if strings.Contains(strings.Join(req.Args, " "), " rm ") {
			return LocalCommandResult{ExitCode: 1}, errors.New("delete failed")
		}
		return exeDevInventoryResponse(t, vm)(req)
	}
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err == nil || !strings.Contains(err.Error(), "delete failed") {
		t.Fatalf("err=%v, want delete failure", err)
	}
	if _, exists, err := readLeaseClaimWithPresence(leaseID); err != nil || !exists {
		t.Fatalf("claim exists=%v err=%v, want retained", exists, err)
	}
}

func TestExeDevReuseRequiresExplicitAdoptionAndPersistsExactBinding(t *testing.T) {
	leaseID := "cbx_abcdef123456"
	vm := ownedExeDevVM(leaseID, "blue")
	for _, reclaim := range []bool{false, true} {
		t.Run(map[bool]string{false: "implicit refused", true: "explicit adopted"}[reclaim], func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			runner := exeDevInventoryRunner(t, vm)
			backend := newExeDevTestBackend(Config{}, runner)
			lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: vm.Name(), Repo: Repo{Root: t.TempDir()}, Reclaim: reclaim})
			if !reclaim {
				if err == nil || !strings.Contains(err.Error(), "reuse with --reclaim") {
					t.Fatalf("err=%v, want explicit reclaim refusal", err)
				}
				if _, exists, readErr := readLeaseClaimWithPresence(leaseID); readErr != nil || exists {
					t.Fatalf("claim exists=%v err=%v, want absent", exists, readErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			claim, exists, readErr := readLeaseClaimWithPresence(leaseID)
			if readErr != nil || !exists {
				t.Fatalf("claim exists=%v err=%v", exists, readErr)
			}
			if claim.CloudID != vm.Name() || claim.Provider != providerName || claim.Labels[exeDevControlScopeLabel] == "" {
				t.Fatalf("claim=%#v", claim)
			}
			if _, exists, snapshotSet := serverLeaseClaimSnapshot(lease.Server); !snapshotSet || !exists {
				t.Fatalf("claim snapshot set=%v exists=%v", snapshotSet, exists)
			}
		})
	}
}

func TestExeDevOwnershipRejectsConflictingTags(t *testing.T) {
	vm := ownedExeDevVM("cbx_abcdef123456", "blue")
	vm.Tags = append(vm.Tags, "crabbox-lease-cbx_other123456")
	if _, _, _, err := exeDevOwnershipIdentity(vm); err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("err=%v, want conflicting tag refusal", err)
	}
}

func TestExeDevOwnershipRejectsNoncanonicalLeaseTag(t *testing.T) {
	vm := ownedExeDevVM("exe_abcdef123456", "blue")
	if _, _, _, err := exeDevOwnershipIdentity(vm); err == nil || !strings.Contains(err.Error(), "invalid Crabbox lease tag") {
		t.Fatalf("err=%v, want invalid lease tag refusal", err)
	}
}

func ownedExeDevVM(leaseID, slug string) exeDevVM {
	name := leaseProviderName(leaseID, slug)
	return exeDevVM{
		VMName:  name,
		SSHDest: name + ".exe.xyz",
		Status:  "running",
		Tags:    []string{"crabbox", "crabbox-lease-" + leaseID, "crabbox-slug-" + slug},
	}
}

func exeDevInventoryRunner(t *testing.T, vm exeDevVM) *exeDevRecordingRunner {
	t.Helper()
	return &exeDevRecordingRunner{fn: exeDevInventoryResponse(t, vm)}
}

func exeDevInventoryResponse(t *testing.T, vm exeDevVM) func(LocalCommandRequest) (LocalCommandResult, error) {
	t.Helper()
	payload, err := json.Marshal(exeDevListResponse{VMs: []exeDevVM{vm}})
	if err != nil {
		t.Fatal(err)
	}
	return func(req LocalCommandRequest) (LocalCommandResult, error) {
		if strings.Contains(strings.Join(req.Args, " "), " rm ") {
			return LocalCommandResult{Stdout: `{}`}, nil
		}
		return LocalCommandResult{Stdout: string(payload)}, nil
	}
}

func newExeDevTestBackend(cfg Config, runner *exeDevRecordingRunner) *exeDevLeaseBackend {
	applyExeDevDefaults(&cfg)
	return &exeDevLeaseBackend{cfg: cfg, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
}

func persistExeDevClaim(t *testing.T, cfg Config, vm exeDevVM, leaseID, slug, repoRoot string) LeaseClaim {
	t.Helper()
	claim, err := claimLeaseTargetForRepoConfigIfUnchanged(leaseID, slug, cfg, exeDevServer(vm, leaseID, slug, cfg, true), exeDevSSHTarget(cfg, vm), repoRoot, cfg.IdleTimeout, false, LeaseClaim{}, false)
	if err != nil {
		t.Fatal(err)
	}
	return claim
}

func assertNoExeDevRM(t *testing.T, runner *exeDevRecordingRunner) {
	t.Helper()
	if hasExeDevRM(runner) {
		t.Fatalf("unexpected rm call: %#v", runner.calls)
	}
}

func hasExeDevRM(runner *exeDevRecordingRunner) bool {
	for _, call := range runner.calls {
		if strings.Contains(strings.Join(call.Args, " "), " rm ") {
			return true
		}
	}
	return false
}

func TestExeDevFlagsRejectGenericClassAndType(t *testing.T) {
	for _, args := range [][]string{
		{"--class", "beast"},
		{"--type", "ubuntu:24.04"},
	} {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fs.String("class", "", "")
		fs.String("type", "", "")
		values := RegisterExeDevProviderFlags(fs, Config{})
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		cfg := Config{Provider: providerName}
		err := ApplyExeDevProviderFlags(&cfg, fs, values)
		if err == nil || !strings.Contains(err.Error(), "not supported for provider=exe-dev") {
			t.Fatalf("args=%v err=%v", args, err)
		}
	}
}

func TestExeDevConfigureRejectsUnsupportedTargetAndTailscale(t *testing.T) {
	for name, cfg := range map[string]Config{
		"macos target": {TargetOS: "macos"},
		"tailscale":    {TargetOS: targetLinux, Tailscale: TailscaleConfig{Enabled: true}},
		"network":      {TargetOS: targetLinux, Network: "tailscale"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Provider{}.Configure(cfg, Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: &exeDevRecordingRunner{}})
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestExeDevSSHTargetUsesSSHDestUserPortAndWorkRootLabel(t *testing.T) {
	cfg := Config{SSHKey: "/tmp/crabbox-default-key", ExeDev: ExeDevConfig{WorkRoot: "/tmp/crabbox", User: "runner"}}
	applyExeDevDefaults(&cfg)
	vm := exeDevVM{VMName: "crabbox-blue-12345678", SSHDest: "ubuntu@crabbox-blue-12345678.exe.xyz:2200", Status: "running"}
	target := exeDevSSHTarget(cfg, vm)
	if target.User != "ubuntu" || target.Host != "crabbox-blue-12345678.exe.xyz" || target.Port != "2200" {
		t.Fatalf("target=%#v", target)
	}
	if target.Key != "" {
		t.Fatalf("target key=%q, want empty to allow exe.dev ssh config or agent auth", target.Key)
	}
	server := exeDevServer(vm, "cbx_lease", "blue", cfg, true)
	if server.Labels["work_root"] != "/tmp/crabbox" {
		t.Fatalf("labels=%#v", server.Labels)
	}
}
