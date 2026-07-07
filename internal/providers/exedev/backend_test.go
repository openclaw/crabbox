package exedev

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"maps"
	"reflect"
	"slices"
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
	runner := &exeDevRecordingRunner{}
	runner.fn = func(req LocalCommandRequest) (LocalCommandResult, error) {
		base := []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new", "-o", "ConnectTimeout=10", "exe.dev", "ls --l --json"}
		if !reflect.DeepEqual(req.Args, base) {
			t.Fatalf("args=%v", req.Args)
		}
		return LocalCommandResult{Stdout: `{"vms":[{"vm_name":"crabbox-blue-12345678","ssh_dest":"crabbox-blue-12345678.exe.xyz","status":"running","tags":["crabbox","crabbox-lease-cbx_abcdef123456","crabbox-slug-blue"]},{"vm_name":"crabbox-manual-12345678","ssh_dest":"crabbox-manual-12345678.exe.xyz","status":"running"}]}`}, nil
	}
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
			"--tag crabbox-claim-cbx_111111111111",
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
	vm, err := backend.createVM(context.Background(), backend.configForRun(), "crabbox-blue-12345678", "cbx_lease", "blue", "cbx_111111111111")
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
	oldClaim := claimLeaseTargetForRepoConfigScopeIfUnchanged
	claimLeaseTargetForRepoConfigScopeIfUnchanged = func(string, string, Config, string, Server, SSHTarget, string, time.Duration, bool, LeaseClaim, bool) (LeaseClaim, error) {
		return LeaseClaim{}, primaryErr
	}
	t.Cleanup(func() { claimLeaseTargetForRepoConfigScopeIfUnchanged = oldClaim })
	runner := newExeDevAcquireRollbackRunner()
	backend := &exeDevLeaseBackend{cfg: Config{}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	_, err := backend.Acquire(context.Background(), AcquireRequest{Repo: Repo{Root: t.TempDir()}})
	assertExeDevRollbackFailure(t, err, primaryErr, runner)
}

func TestExeDevProvisioningRollbackRejectsReplacementGeneration(t *testing.T) {
	leaseID := "cbx_abcdef123456"
	slug := "blue"
	vm := ownedExeDevVM(leaseID, slug)
	runner := exeDevInventoryRunner(t, vm)
	backend := newExeDevTestBackend(Config{}, runner)
	primaryErr := errors.New("ssh not ready")

	err := backend.rollbackCreatedVM(vm.Name(), leaseID, slug, "cbx_222222222222", primaryErr)
	if err == nil || !strings.Contains(err.Error(), primaryErr.Error()) || !strings.Contains(err.Error(), "refused replacement VM") {
		t.Fatalf("err=%v, want guarded rollback refusal", err)
	}
	assertNoExeDevRM(t, runner)
}

func newExeDevAcquireRollbackRunner() *exeDevRecordingRunner {
	var created *exeDevVM
	return &exeDevRecordingRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		cmd := strings.Join(req.Args, " ")
		switch {
		case strings.Contains(cmd, "whoami --json"):
			return LocalCommandResult{Stdout: `{"email":"test@example.com"}`}, nil
		case strings.Contains(cmd, "ls --l --json"):
			vms := []exeDevVM{}
			if created != nil {
				vms = append(vms, *created)
			}
			payload, err := json.Marshal(exeDevListResponse{VMs: vms})
			return LocalCommandResult{Stdout: string(payload)}, err
		case strings.Contains(cmd, " new "):
			fields := strings.Fields(req.Args[len(req.Args)-1])
			vm := exeDevVM{Status: "running"}
			for i := 0; i < len(fields)-1; i++ {
				switch fields[i] {
				case "--name":
					vm.VMName = fields[i+1]
					vm.SSHDest = fields[i+1] + ".exe.xyz"
				case "--tag":
					vm.Tags = append(vm.Tags, fields[i+1])
				}
			}
			created = &vm
			payload, err := json.Marshal(vm)
			return LocalCommandResult{Stdout: string(payload)}, err
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
	for _, want := range []string{"exe.dev cleanup failed for VM", "manual cleanup: ssh exe.dev rm crabbox-", "exit status 1", primary.Error()} {
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
	if _, err := claimLeaseTargetForRepoConfigScopeIfUnchanged(leaseID, slug, cfg, mustExeDevControlScope(t, cfg), exeDevServer(vm, leaseID, slug, cfg, true), exeDevSSHTarget(cfg, vm), t.TempDir(), 0, false, LeaseClaim{}, false); err != nil {
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

func TestExeDevReleaseRejectsChangedSSHEndpoint(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_abcdef123456"
	vm := ownedExeDevVM(leaseID, "blue")
	backend := newExeDevTestBackend(Config{}, exeDevInventoryRunner(t, vm))
	persistExeDevClaim(t, backend.configForRun(), vm, leaseID, "blue", t.TempDir())
	vm.SSHDest = "replacement.exe.xyz:2222"
	backend.rt.Exec = exeDevInventoryRunner(t, vm)

	_, err := backend.Resolve(context.Background(), ResolveRequest{ID: vm.Name(), ReleaseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "SSH endpoint does not match") {
		t.Fatalf("err=%v, want exact SSH endpoint refusal", err)
	}
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
				if _, err := claimLeaseTargetForRepoConfigScopeIfUnchanged(lease.LeaseID, claim.Slug, cfg, claim.ProviderScope, lease.Server, exeDevSSHTarget(cfg, vm), t.TempDir(), cfg.IdleTimeout, true, claim, true); err != nil {
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

func TestExeDevReleaseRunsGuardedRemoteCleanupBeforeDelete(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_abcdef123456"
	vm := ownedExeDevVM(leaseID, "blue")
	runner := exeDevInventoryRunner(t, vm)
	backend := newExeDevTestBackend(Config{}, runner)
	claim := persistExeDevClaim(t, backend.configForRun(), vm, leaseID, "blue", t.TempDir())
	labels := maps.Clone(claim.Labels)
	labels["tailscale"] = "true"
	claim, err := updateLeaseClaimLabelsIfUnchangedAfter(leaseID, claim, labels, nil)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: leaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	cleaned := false
	lease.SSH.Host = "stale-endpoint.example"
	baseRun := runner.fn
	runner.fn = func(req LocalCommandRequest) (LocalCommandResult, error) {
		if strings.Contains(strings.Join(req.Args, " "), " rm ") && !cleaned {
			return LocalCommandResult{}, errors.New("delete ran before guarded remote cleanup")
		}
		return baseRun(req)
	}
	err = backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{
		Lease: lease,
		GuardedRemoteCleanup: func(_ context.Context, target LeaseTarget) {
			if target.SSH.Host != vm.SSHHost() {
				t.Fatalf("cleanup SSH host=%q want %q", target.SSH.Host, vm.SSHHost())
			}
			if target.Server.Labels["tailscale"] != "true" {
				t.Fatalf("cleanup labels=%v, want claimed Tailscale metadata", target.Server.Labels)
			}
			cleaned = true
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !cleaned || !hasExeDevRM(runner) {
		t.Fatalf("cleaned=%v rm=%v", cleaned, hasExeDevRM(runner))
	}
}

func TestExeDevReleaseRejectsDifferentAuthenticatedAccount(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_abcdef123456"
	slug := "blue"
	vm := ownedExeDevVM(leaseID, slug)
	baseResponse := exeDevInventoryResponse(t, vm)
	runner := &exeDevRecordingRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		if strings.Contains(strings.Join(req.Args, " "), "whoami --json") {
			return LocalCommandResult{Stdout: `{"email":"other@example.com"}`}, nil
		}
		return baseResponse(req)
	}}
	backend := newExeDevTestBackend(Config{}, runner)
	persistExeDevClaim(t, backend.configForRun(), vm, leaseID, slug, t.TempDir())

	_, err := backend.Resolve(context.Background(), ResolveRequest{ID: vm.Name(), ReleaseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "different exe.dev control route") {
		t.Fatalf("err=%v, want account-bound route refusal", err)
	}
	assertNoExeDevRM(t, runner)
}

func TestExeDevReleaseRejectsReplacementClaimGeneration(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_abcdef123456"
	slug := "blue"
	vm := ownedExeDevVM(leaseID, slug)
	runner := exeDevInventoryRunner(t, vm)
	backend := newExeDevTestBackend(Config{}, runner)
	persistExeDevClaim(t, backend.configForRun(), vm, leaseID, slug, t.TempDir())
	vm.Tags = slices.DeleteFunc(vm.Tags, func(tag string) bool {
		return strings.HasPrefix(tag, exeDevClaimGenerationTagPrefix)
	})
	vm.Tags = append(vm.Tags, exeDevClaimGenerationTagPrefix+"cbx_222222222222")
	runner.fn = exeDevInventoryResponse(t, vm)

	_, err := backend.Resolve(context.Background(), ResolveRequest{ID: leaseID, ReleaseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "generation does not match") {
		t.Fatalf("err=%v, want replacement-generation refusal", err)
	}
	assertNoExeDevRM(t, runner)
	if _, exists, readErr := readLeaseClaimWithPresence(leaseID); readErr != nil || !exists {
		t.Fatalf("claim exists=%v err=%v, want retained", exists, readErr)
	}
}

func TestExeDevReleaseSurvivesTouchedClaimRoundTrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_abcdef123456"
	slug := "blue"
	vm := ownedExeDevVM(leaseID, slug)
	runner := exeDevInventoryRunner(t, vm)
	backend := newExeDevTestBackend(Config{}, runner)
	cfg := backend.configForRun()
	repoRoot := t.TempDir()
	claim := persistExeDevClaim(t, cfg, vm, leaseID, slug, repoRoot)
	lease := LeaseTarget{
		LeaseID: leaseID,
		Server:  exeDevServer(vm, leaseID, slug, cfg, true),
	}
	lease.Server.Labels[exeDevClaimGenerationLabel] = claim.Labels[exeDevClaimGenerationLabel]
	setServerLeaseClaimSnapshot(&lease.Server, claim, true)
	touched, err := backend.Touch(context.Background(), TouchRequest{Lease: lease, State: "ready"})
	if err != nil {
		t.Fatal(err)
	}
	claim, err = claimLeaseTargetForRepoConfigScopeIfUnchanged(leaseID, slug, cfg, claim.ProviderScope, touched, exeDevSSHTarget(cfg, vm), repoRoot, cfg.IdleTimeout, true, claim, true)
	if err != nil {
		t.Fatal(err)
	}
	if claim.ProviderScope != mustExeDevControlScope(t, cfg) {
		t.Fatalf("provider scope=%q", claim.ProviderScope)
	}
	resolved, err := backend.RefreshReleaseLeaseTarget(context.Background(), lease)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: resolved}); err != nil {
		t.Fatal(err)
	}
	if !hasExeDevRM(runner) {
		t.Fatalf("rm not called: %#v", runner.calls)
	}
}

func TestExeDevReleaseRefreshRejectsConcurrentReclaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_abcdef123456"
	slug := "blue"
	vm := ownedExeDevVM(leaseID, slug)
	runner := exeDevInventoryRunner(t, vm)
	backend := newExeDevTestBackend(Config{}, runner)
	cfg := backend.configForRun()
	repoRoot := t.TempDir()
	claim := persistExeDevClaim(t, cfg, vm, leaseID, slug, repoRoot)
	lease := LeaseTarget{LeaseID: leaseID, Server: exeDevServer(vm, leaseID, slug, cfg, true)}
	setServerLeaseClaimSnapshot(&lease.Server, claim, true)

	reclaimed, err := backend.Resolve(context.Background(), ResolveRequest{ID: vm.Name(), Repo: Repo{Root: repoRoot}, Reclaim: true})
	if err != nil {
		t.Fatal(err)
	}
	reclaimedClaim, reclaimedExists, reclaimedSet := serverLeaseClaimSnapshot(reclaimed.Server)
	if !reclaimedSet || !reclaimedExists || reclaimedClaim.Labels[exeDevClaimGenerationLabel] == claim.Labels[exeDevClaimGenerationLabel] {
		t.Fatalf("reclaimed generation=%q, want rotation from %q", reclaimedClaim.Labels[exeDevClaimGenerationLabel], claim.Labels[exeDevClaimGenerationLabel])
	}
	if _, err := backend.RefreshReleaseLeaseTarget(context.Background(), lease); err == nil || !strings.Contains(err.Error(), "claim ownership changed") {
		t.Fatalf("err=%v, want changed-ownership refusal", err)
	}
	assertNoExeDevRM(t, runner)
}

func TestExeDevReleaseRefreshTreatsMissingClaimAsOwnershipChange(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_abcdef123456"
	slug := "blue"
	vm := ownedExeDevVM(leaseID, slug)
	runner := exeDevInventoryRunner(t, vm)
	backend := newExeDevTestBackend(Config{}, runner)
	claim := persistExeDevClaim(t, backend.configForRun(), vm, leaseID, slug, t.TempDir())
	lease := LeaseTarget{LeaseID: leaseID, Server: exeDevServer(vm, leaseID, slug, backend.configForRun(), true)}
	setServerLeaseClaimSnapshot(&lease.Server, claim, true)
	if err := removeLeaseClaimIfUnchangedAfter(leaseID, claim, nil); err != nil {
		t.Fatal(err)
	}

	_, err := backend.RefreshReleaseLeaseTarget(context.Background(), lease)
	if !errors.Is(err, errReleaseLeaseOwnershipChanged) {
		t.Fatalf("err=%v, want ownership-change sentinel", err)
	}
	assertNoExeDevRM(t, runner)
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

func TestExeDevReleaseRemovesUnchangedClaimWhenVMIsAlreadyAbsent(t *testing.T) {
	leaseID := "cbx_abcdef123456"
	slug := "blue"
	vm := ownedExeDevVM(leaseID, slug)
	for _, identifier := range []string{leaseID, vm.Name(), vm.SSHHost()} {
		t.Run(identifier, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			runner := &exeDevRecordingRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
				if strings.Contains(strings.Join(req.Args, " "), "whoami --json") {
					return LocalCommandResult{Stdout: `{"email":"test@example.com"}`}, nil
				}
				return LocalCommandResult{Stdout: `{"vms":[]}`}, nil
			}}
			backend := newExeDevTestBackend(Config{}, runner)
			persistExeDevClaim(t, backend.configForRun(), vm, leaseID, slug, t.TempDir())

			lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: identifier, ReleaseOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			if lease.Server.Labels[exeDevConfirmedAbsentLabel] != "true" {
				t.Fatalf("labels=%v, want confirmed-absence marker", lease.Server.Labels)
			}
			if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err != nil {
				t.Fatal(err)
			}
			assertNoExeDevRM(t, runner)
			if _, exists, err := readLeaseClaimWithPresence(leaseID); err != nil || exists {
				t.Fatalf("claim exists=%v err=%v, want removed", exists, err)
			}
		})
	}
}

func TestExeDevAbsentReleaseRechecksInventoryBeforeRemovingClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_abcdef123456"
	slug := "blue"
	vm := ownedExeDevVM(leaseID, slug)
	runner := &exeDevRecordingRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		if strings.Contains(strings.Join(req.Args, " "), "whoami --json") {
			return LocalCommandResult{Stdout: `{"email":"test@example.com"}`}, nil
		}
		return LocalCommandResult{Stdout: `{"vms":[]}`}, nil
	}}
	backend := newExeDevTestBackend(Config{}, runner)
	persistExeDevClaim(t, backend.configForRun(), vm, leaseID, slug, t.TempDir())

	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: leaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	renamed := vm
	renamed.VMName = "renamed-owned-vm"
	renamed.SSHDest = "renamed-owned-vm.exe.xyz"
	runner.fn = exeDevInventoryResponse(t, renamed)
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err == nil || !strings.Contains(err.Error(), "is present") {
		t.Fatalf("err=%v, want present-VM refusal", err)
	}
	assertNoExeDevRM(t, runner)
	if _, exists, err := readLeaseClaimWithPresence(leaseID); err != nil || !exists {
		t.Fatalf("claim exists=%v err=%v, want retained", exists, err)
	}
}

func TestExeDevAbsentReleaseRejectsRenamedOwnedVM(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_abcdef123456"
	slug := "blue"
	vm := ownedExeDevVM(leaseID, slug)
	renamed := vm
	renamed.VMName = "renamed-owned-vm"
	renamed.SSHDest = "renamed-owned-vm.exe.xyz"
	runner := exeDevInventoryRunner(t, renamed)
	backend := newExeDevTestBackend(Config{}, runner)
	persistExeDevClaim(t, backend.configForRun(), vm, leaseID, slug, t.TempDir())

	_, err := backend.Resolve(context.Background(), ResolveRequest{ID: leaseID, ReleaseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "unexpected VM") {
		t.Fatalf("err=%v, want renamed owned-VM refusal", err)
	}
	assertNoExeDevRM(t, runner)
	if _, exists, readErr := readLeaseClaimWithPresence(leaseID); readErr != nil || !exists {
		t.Fatalf("claim exists=%v err=%v, want retained", exists, readErr)
	}
}

func TestExeDevAbsentReleaseRejectsDifferentAuthenticatedAccount(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_abcdef123456"
	slug := "blue"
	vm := ownedExeDevVM(leaseID, slug)
	runner := &exeDevRecordingRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		if strings.Contains(strings.Join(req.Args, " "), "whoami --json") {
			return LocalCommandResult{Stdout: `{"email":"other@example.com"}`}, nil
		}
		return LocalCommandResult{Stdout: `{"vms":[]}`}, nil
	}}
	backend := newExeDevTestBackend(Config{}, runner)
	persistExeDevClaim(t, backend.configForRun(), vm, leaseID, slug, t.TempDir())

	_, err := backend.Resolve(context.Background(), ResolveRequest{ID: leaseID, ReleaseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "different exe.dev control route") {
		t.Fatalf("err=%v, want account-bound route refusal", err)
	}
	assertNoExeDevRM(t, runner)
	if _, exists, readErr := readLeaseClaimWithPresence(leaseID); readErr != nil || !exists {
		t.Fatalf("claim exists=%v err=%v, want retained", exists, readErr)
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
			if claim.CloudID != vm.Name() || claim.Provider != providerName || claim.ProviderScope == "" {
				t.Fatalf("claim=%#v", claim)
			}
			if !hasExeDevTagCommand(runner) {
				t.Fatalf("explicit reclaim did not bind a remote claim generation: %#v", runner.calls)
			}
			assertExeDevTagOptionsPrecedeVM(t, runner)
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

func TestExeDevBulkInventoryTreatsMalformedTagsAsUnowned(t *testing.T) {
	vm := ownedExeDevVM("cbx_abcdef123456", "blue")
	vm.Tags = append(vm.Tags, "crabbox-lease-cbx_other123456")
	runner := exeDevInventoryRunner(t, vm)
	backend := newExeDevTestBackend(Config{}, runner)

	views, err := backend.List(context.Background(), ListRequest{})
	if err != nil || len(views) != 0 {
		t.Fatalf("owned inventory views=%#v err=%v, want malformed VM omitted", views, err)
	}
	views, err = backend.List(context.Background(), ListRequest{All: true})
	if err != nil || len(views) != 1 || views[0].Name != vm.Name() {
		t.Fatalf("all inventory views=%#v err=%v, want malformed VM visible", views, err)
	}
	if _, err := backend.Resolve(context.Background(), ResolveRequest{ID: vm.Name(), ReleaseOnly: true}); err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("exact resolve err=%v, want hard malformed-tag refusal", err)
	}
}

func TestExeDevLeaseLookupSkipsOnlyUnrelatedMalformedTags(t *testing.T) {
	leaseID := "cbx_abcdef123456"
	valid := ownedExeDevVM(leaseID, "blue")
	malformed := ownedExeDevVM("cbx_other123456", "other")
	malformed.Tags = append(malformed.Tags, "crabbox-lease-cbx_second123456")

	backend := newExeDevTestBackend(Config{}, &exeDevRecordingRunner{fn: exeDevInventoryResponseMany(t, malformed, valid)})
	vm, found, err := backend.findVMByLeaseID(context.Background(), leaseID)
	if err != nil || !found || vm.Name() != valid.Name() {
		t.Fatalf("vm=%#v found=%v err=%v", vm, found, err)
	}
	if command := strings.Join(backend.rt.Exec.(*exeDevRecordingRunner).calls[0].Args, " "); strings.Contains(command, " -a") {
		t.Fatalf("lease lookup crossed into team-wide inventory: %s", command)
	}

	malformed.Tags = append(malformed.Tags, "crabbox-lease-"+leaseID)
	backend = newExeDevTestBackend(Config{}, &exeDevRecordingRunner{fn: exeDevInventoryResponseMany(t, malformed, valid)})
	if _, _, err := backend.findVMByLeaseID(context.Background(), leaseID); err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("err=%v, want malformed requested-lease refusal", err)
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
		Tags:    []string{"crabbox", "crabbox-lease-" + leaseID, "crabbox-slug-" + slug, exeDevClaimGenerationTagPrefix + "cbx_111111111111"},
	}
}

func exeDevInventoryRunner(t *testing.T, vm exeDevVM) *exeDevRecordingRunner {
	t.Helper()
	current := vm
	return &exeDevRecordingRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		command := strings.Join(req.Args, " ")
		if strings.Contains(command, "whoami --json") {
			return LocalCommandResult{Stdout: `{"email":"test@example.com"}`}, nil
		}
		if strings.Contains(command, " rm ") {
			return LocalCommandResult{Stdout: `{}`}, nil
		}
		if strings.Contains(command, " tag ") {
			fields := strings.Fields(req.Args[len(req.Args)-1])
			deleteTags := len(fields) > 2 && fields[2] == "-d"
			start := 3
			if deleteTags {
				start = 4
			}
			for _, tag := range fields[start:] {
				if deleteTags {
					current.Tags = slices.DeleteFunc(current.Tags, func(existing string) bool { return existing == tag })
				} else if !slices.Contains(current.Tags, tag) {
					current.Tags = append(current.Tags, tag)
				}
			}
			return LocalCommandResult{Stdout: `{}`}, nil
		}
		payload, err := json.Marshal(exeDevListResponse{VMs: []exeDevVM{current}})
		if err != nil {
			t.Fatal(err)
		}
		return LocalCommandResult{Stdout: string(payload)}, nil
	}}
}

func exeDevInventoryResponse(t *testing.T, vm exeDevVM) func(LocalCommandRequest) (LocalCommandResult, error) {
	return exeDevInventoryResponseMany(t, vm)
}

func exeDevInventoryResponseMany(t *testing.T, vms ...exeDevVM) func(LocalCommandRequest) (LocalCommandResult, error) {
	t.Helper()
	payload, err := json.Marshal(exeDevListResponse{VMs: vms})
	if err != nil {
		t.Fatal(err)
	}
	return func(req LocalCommandRequest) (LocalCommandResult, error) {
		command := strings.Join(req.Args, " ")
		if strings.Contains(command, "whoami --json") {
			return LocalCommandResult{Stdout: `{"email":"test@example.com"}`}, nil
		}
		if strings.Contains(command, " rm ") {
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
	server := exeDevServer(vm, leaseID, slug, cfg, true)
	server.Labels[exeDevClaimGenerationLabel] = "cbx_111111111111"
	claim, err := claimLeaseTargetForRepoConfigScopeIfUnchanged(leaseID, slug, cfg, mustExeDevControlScope(t, cfg), server, exeDevSSHTarget(cfg, vm), repoRoot, cfg.IdleTimeout, false, LeaseClaim{}, false)
	if err != nil {
		t.Fatal(err)
	}
	return claim
}

func mustExeDevControlScope(t *testing.T, cfg Config) string {
	t.Helper()
	scope, err := exeDevControlScope(cfg, exeDevAccountFingerprint("test@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	return scope
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

func hasExeDevTagCommand(runner *exeDevRecordingRunner) bool {
	for _, call := range runner.calls {
		if strings.Contains(strings.Join(call.Args, " "), " tag ") {
			return true
		}
	}
	return false
}

func assertExeDevTagOptionsPrecedeVM(t *testing.T, runner *exeDevRecordingRunner) {
	t.Helper()
	for _, call := range runner.calls {
		fields := strings.Fields(call.Args[len(call.Args)-1])
		if len(fields) > 0 && fields[0] == "tag" && (len(fields) < 2 || fields[1] != "--json") {
			t.Fatalf("exe.dev tag options must precede VM name: %q", fields)
		}
	}
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
