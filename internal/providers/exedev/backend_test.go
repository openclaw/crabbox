package exedev

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type exeDevRecordingRunner struct {
	calls []core.LocalCommandRequest
	fn    func(core.LocalCommandRequest) (core.LocalCommandResult, error)
}

func (r *exeDevRecordingRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.calls = append(r.calls, req)
	if r.fn != nil {
		return r.fn(req)
	}
	return core.LocalCommandResult{}, nil
}

func TestExeDevListFiltersCrabboxVMsByDefault(t *testing.T) {
	runner := &exeDevRecordingRunner{}
	runner.fn = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		base := []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new", "-o", "ConnectTimeout=10", "exe.dev", "ls --l --json"}
		if !reflect.DeepEqual(req.Args, base) {
			t.Fatalf("args=%v", req.Args)
		}
		return core.LocalCommandResult{Stdout: `{"vms":[{"vm_name":"crabbox-blue-12345678","ssh_dest":"crabbox-blue-12345678.exe.xyz","status":"running","tags":["crabbox","crabbox-lease-cbx_abcdef123456","crabbox-slug-blue"]},{"vm_name":"crabbox-manual-12345678","ssh_dest":"crabbox-manual-12345678.exe.xyz","status":"running"}]}`}, nil
	}
	backend := &exeDevLeaseBackend{cfg: core.Config{}, rt: core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	views, err := backend.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Name != "crabbox-blue-12345678" || views[0].Provider != providerName {
		t.Fatalf("views=%#v", views)
	}
	views, err = backend.List(context.Background(), core.ListRequest{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 {
		t.Fatalf("views=%#v", views)
	}
}

func TestExeDevDoctorListsInventory(t *testing.T) {
	runner := &exeDevRecordingRunner{fn: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		got := strings.Join(req.Args, " ")
		if !strings.Contains(got, "exe.dev ls --l --json") {
			t.Fatalf("args=%v", req.Args)
		}
		if strings.Contains(got, " new ") || strings.Contains(got, " rm ") {
			t.Fatalf("doctor used mutating command: %v", req.Args)
		}
		return core.LocalCommandResult{Stdout: `{"vms":[{"vm_name":"crabbox-blue-12345678","ssh_dest":"crabbox-blue-12345678.exe.xyz","status":"running","tags":["crabbox","crabbox-lease-cbx_abcdef123456","crabbox-slug-blue"]}]}`}, nil
	}}
	doctor, err := Provider{}.ConfigureDoctor(core.Config{}, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner})
	if err != nil {
		t.Fatal(err)
	}
	result, err := doctor.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider != providerName || !strings.Contains(result.Message, "cli=ready") || !strings.Contains(result.Message, "api=list") || !strings.Contains(result.Message, "leases=1") {
		t.Fatalf("result=%#v", result)
	}
}

func TestExeDevCreateVMUsesSSHControlAPI(t *testing.T) {
	runner := &exeDevRecordingRunner{fn: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
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
		return core.LocalCommandResult{Stdout: `{"vm_name":"crabbox-blue-12345678","ssh_dest":"crabbox-blue-12345678.exe.xyz","status":"running"}`}, nil
	}}
	cfg := core.Config{ExeDev: core.ExeDevConfig{
		Image:   "ubuntu:24.04",
		CPUs:    4,
		Memory:  "8GB",
		Disk:    "40GB",
		Command: "sleep infinity",
		NoEmail: true,
	}}
	backend := &exeDevLeaseBackend{cfg: cfg, rt: core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
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
	waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
		return primaryErr
	}
	t.Cleanup(func() { waitForSSHReady = oldWait })
	runner := newExeDevAcquireRollbackRunner()
	backend := &exeDevLeaseBackend{cfg: core.Config{}, rt: core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	_, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}})
	assertExeDevRollbackFailure(t, err, primaryErr, runner)
}

func TestExeDevAcquireReportsRollbackFailureAfterClaimFailure(t *testing.T) {
	primaryErr := errors.New("claim failed")
	oldWait := waitForSSHReady
	waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
		return nil
	}
	t.Cleanup(func() { waitForSSHReady = oldWait })
	oldClaim := claimLeaseTargetForRepoConfigScopeIfUnchanged
	claimLeaseTargetForRepoConfigScopeIfUnchanged = func(string, string, core.Config, string, core.Server, core.SSHTarget, string, time.Duration, bool, core.LeaseClaim, bool) (core.LeaseClaim, error) {
		return core.LeaseClaim{}, primaryErr
	}
	t.Cleanup(func() { claimLeaseTargetForRepoConfigScopeIfUnchanged = oldClaim })
	runner := newExeDevAcquireRollbackRunner()
	backend := &exeDevLeaseBackend{cfg: core.Config{}, rt: core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	_, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}})
	assertExeDevRollbackFailure(t, err, primaryErr, runner)
}

func TestExeDevProvisioningRollbackRejectsReplacementGeneration(t *testing.T) {
	for _, tc := range []struct {
		name    string
		primary error
		code    int
	}{
		{name: "opaque", primary: errors.New("ssh not ready"), code: 1},
		{name: "typed", primary: core.ExitError{Code: 69, Message: "ssh not ready"}, code: 69},
		{name: "signed", primary: core.ExitError{Code: -1, Message: "ssh not ready"}, code: -1},
		{name: "zero", primary: core.ExitError{Code: 0, Message: "ssh not ready"}, code: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leaseID := "cbx_abcdef123456"
			slug := "blue"
			vm := ownedExeDevVM(leaseID, slug)
			runner := exeDevInventoryRunner(t, vm)
			backend := newExeDevTestBackend(core.Config{}, runner)

			err := backend.rollbackCreatedVM(vm.Name(), leaseID, slug, "cbx_222222222222", tc.primary)
			if err == nil || !strings.Contains(err.Error(), tc.primary.Error()) || !strings.Contains(err.Error(), "refused replacement VM") {
				t.Fatalf("err=%v, want guarded rollback refusal", err)
			}
			var public core.ExitError
			if !errors.As(err, &public) || public.Code != tc.code {
				t.Fatalf("rollback exit=%d, want primary code %d", public.Code, tc.code)
			}
			assertNoExeDevRM(t, runner)
		})
	}
}

func newExeDevAcquireRollbackRunner() *exeDevRecordingRunner {
	var created *exeDevVM
	return &exeDevRecordingRunner{fn: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		cmd := strings.Join(req.Args, " ")
		switch {
		case strings.Contains(cmd, "whoami --json"):
			return core.LocalCommandResult{Stdout: `{"email":"test@example.com"}`}, nil
		case strings.Contains(cmd, "ls --l --json"):
			vms := []exeDevVM{}
			if created != nil {
				vms = append(vms, *created)
			}
			payload, err := json.Marshal(exeDevListResponse{VMs: vms})
			return core.LocalCommandResult{Stdout: string(payload)}, err
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
			return core.LocalCommandResult{Stdout: string(payload)}, err
		case strings.Contains(cmd, " rm "):
			return core.LocalCommandResult{ExitCode: 1}, errors.New("exit status 1")
		default:
			return core.LocalCommandResult{}, nil
		}
	}}
}

func assertExeDevRollbackFailure(t *testing.T, err error, primary error, runner *exeDevRecordingRunner) {
	t.Helper()
	if err == nil {
		t.Fatal("Acquire succeeded, want rollback failure")
	}
	var exitErr core.ExitError
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
	cfg := core.Config{WorkRoot: "/custom/crabbox"}
	applyExeDevDefaults(&cfg)
	if cfg.WorkRoot != "/custom/crabbox" || cfg.ExeDev.WorkRoot != "/custom/crabbox" {
		t.Fatalf("workRoot=%q exeDev.workRoot=%q", cfg.WorkRoot, cfg.ExeDev.WorkRoot)
	}

	cfg = core.Config{WorkRoot: "/custom/crabbox", ExeDev: core.ExeDevConfig{WorkRoot: "/exe/crabbox"}}
	applyExeDevDefaults(&cfg)
	if cfg.WorkRoot != "/exe/crabbox" || cfg.ExeDev.WorkRoot != "/exe/crabbox" {
		t.Fatalf("workRoot=%q exeDev.workRoot=%q", cfg.WorkRoot, cfg.ExeDev.WorkRoot)
	}
}

func TestExeDevControlSurfacesJSONError(t *testing.T) {
	runner := &exeDevRecordingRunner{fn: func(core.LocalCommandRequest) (core.LocalCommandResult, error) {
		return core.LocalCommandResult{ExitCode: 1, Stdout: `{"error":"active plan required"}`}, errors.New("exit status 1")
	}}
	backend := &exeDevLeaseBackend{cfg: core.Config{}, rt: core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	_, err := backend.controlOutput(context.Background(), []string{"new", "--json"})
	if err == nil || !strings.Contains(err.Error(), "active plan required") {
		t.Fatalf("err=%v", err)
	}
}

func TestExeDevControlRejectsSSHOptionLikeHost(t *testing.T) {
	runner := &exeDevRecordingRunner{}
	backend := &exeDevLeaseBackend{cfg: core.Config{ExeDev: core.ExeDevConfig{ControlHost: "-oProxyCommand=sh"}}, rt: core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	if _, err := backend.controlOutput(context.Background(), []string{"ls", "--json"}); err == nil || !strings.Contains(err.Error(), "invalid exe.dev control host") {
		t.Fatalf("err=%v, want invalid control host", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("ssh should not run for invalid host, calls=%v", runner.calls)
	}
}

func TestExeDevControlHostUsesSeparatePortArgument(t *testing.T) {
	runner := &exeDevRecordingRunner{}
	backend := &exeDevLeaseBackend{cfg: core.Config{ExeDev: core.ExeDevConfig{ControlHost: "alice@control.example:2222"}}, rt: core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
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
	backend := &exeDevLeaseBackend{cfg: core.Config{ExeDev: core.ExeDevConfig{ControlHost: "alice@2001:db8::10"}}, rt: core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
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
	backend := &exeDevLeaseBackend{cfg: core.Config{ExeDev: core.ExeDevConfig{ControlHost: "alice@[2001:db8::10]:2222"}}, rt: core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
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
	backend := &exeDevLeaseBackend{cfg: core.Config{ExeDev: core.ExeDevConfig{ControlHost: "alice@fe80::1%eth0"}}, rt: core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
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
	runner := &exeDevRecordingRunner{fn: func(core.LocalCommandRequest) (core.LocalCommandResult, error) {
		return core.LocalCommandResult{Stdout: `{"vms":[{"vm_name":"crabbox-blue-12345678","ssh_dest":"crabbox-blue-12345678.exe.xyz","status":"running","tags":["crabbox","crabbox-lease-cbx_abcdef123456","crabbox-slug-blue"]}]}`}, nil
	}}
	backend := &exeDevLeaseBackend{cfg: core.Config{}, rt: core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
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
	runner := &exeDevRecordingRunner{fn: func(core.LocalCommandRequest) (core.LocalCommandResult, error) {
		return core.LocalCommandResult{Stdout: `{"vms":[{"vm_name":"crabbox-custom-12345678","ssh_dest":"crabbox-custom-12345678.exe.xyz","status":"running","tags":["crabbox","crabbox-lease-` + leaseID + `","crabbox-slug-custom"]}]}`}, nil
	}}
	backend := &exeDevLeaseBackend{cfg: core.Config{}, rt: core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
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
	name := core.LeaseProviderName(leaseID, slug)
	cfg := core.Config{Provider: providerName}
	applyExeDevDefaults(&cfg)
	vm := exeDevVM{VMName: name, SSHDest: name + ".exe.xyz", Status: "running"}
	if _, err := claimLeaseTargetForRepoConfigScopeIfUnchanged(leaseID, slug, cfg, mustExeDevControlScope(t, cfg), exeDevServer(vm, leaseID, slug, cfg, true), exeDevSSHTarget(cfg, vm), t.TempDir(), 0, false, core.LeaseClaim{}, false); err != nil {
		t.Fatal(err)
	}
	runner := &exeDevRecordingRunner{fn: func(core.LocalCommandRequest) (core.LocalCommandResult, error) {
		return core.LocalCommandResult{Stdout: `{"vms":[{"vm_name":"` + name + `","ssh_dest":"` + name + `.exe.xyz","status":"running"}]}`}, nil
	}}
	backend := &exeDevLeaseBackend{cfg: core.Config{}, rt: core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
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
			backend := newExeDevTestBackend(core.Config{}, runner)
			_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: identifier, ReleaseOnly: true})
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
	backend := newExeDevTestBackend(core.Config{}, runner)
	_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: vm.Name(), ReleaseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "no exact local claim") {
		t.Fatalf("err=%v, want exact local claim refusal", err)
	}
	assertNoExeDevRM(t, runner)
}

func TestExeDevReleaseRejectsClaimBoundToDifferentVM(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_abcdef123456"
	vm := ownedExeDevVM(leaseID, "blue")
	cfg := core.Config{Provider: providerName}
	applyExeDevDefaults(&cfg)
	other := vm
	other.VMName = "crabbox-other-12345678"
	persistExeDevClaim(t, cfg, other, leaseID, "blue", t.TempDir())
	runner := exeDevInventoryRunner(t, vm)
	backend := newExeDevTestBackend(cfg, runner)
	_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: vm.Name(), ReleaseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "not bound to an exact provider/resource claim") {
		t.Fatalf("err=%v, want exact resource binding refusal", err)
	}
	assertNoExeDevRM(t, runner)
}

func TestExeDevReleaseRejectsChangedSSHEndpoint(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_abcdef123456"
	vm := ownedExeDevVM(leaseID, "blue")
	backend := newExeDevTestBackend(core.Config{}, exeDevInventoryRunner(t, vm))
	persistExeDevClaim(t, backend.configForRun(), vm, leaseID, "blue", t.TempDir())
	vm.SSHDest = "replacement.exe.xyz:2222"
	backend.rt.Exec = exeDevInventoryRunner(t, vm)

	_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: vm.Name(), ReleaseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "SSH endpoint does not match") {
		t.Fatalf("err=%v, want exact SSH endpoint refusal", err)
	}
}

func TestExeDevReleaseRequiresUnchangedClaimAndRemoteTags(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(t *testing.T, backend *exeDevLeaseBackend, runner *exeDevRecordingRunner, lease core.LeaseTarget, claim core.LeaseClaim, vm exeDevVM)
		want   string
	}{
		{
			name: "claim changed",
			mutate: func(t *testing.T, backend *exeDevLeaseBackend, _ *exeDevRecordingRunner, lease core.LeaseTarget, claim core.LeaseClaim, vm exeDevVM) {
				cfg := backend.configForRun()
				if _, err := claimLeaseTargetForRepoConfigScopeIfUnchanged(lease.LeaseID, claim.Slug, cfg, claim.ProviderScope, lease.Server, exeDevSSHTarget(cfg, vm), t.TempDir(), cfg.IdleTimeout, true, claim, true); err != nil {
					t.Fatal(err)
				}
			},
			want: "claim changed",
		},
		{
			name: "control route changed",
			mutate: func(_ *testing.T, backend *exeDevLeaseBackend, _ *exeDevRecordingRunner, _ core.LeaseTarget, _ core.LeaseClaim, _ exeDevVM) {
				backend.cfg.ExeDev.ControlHost = "other.exe.dev"
			},
			want: "different exe.dev control route",
		},
		{
			name: "remote tags removed",
			mutate: func(t *testing.T, _ *exeDevLeaseBackend, runner *exeDevRecordingRunner, _ core.LeaseTarget, _ core.LeaseClaim, vm exeDevVM) {
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
			backend := newExeDevTestBackend(core.Config{}, runner)
			claim := persistExeDevClaim(t, backend.configForRun(), vm, leaseID, "blue", t.TempDir())
			lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: vm.Name(), ReleaseOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(t, backend, runner, lease, claim, vm)
			err = backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err=%v, want %q", err, test.want)
			}
			assertNoExeDevRM(t, runner)
			if _, exists, readErr := core.ReadLeaseClaimWithPresence(leaseID); readErr != nil || !exists {
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
	backend := newExeDevTestBackend(core.Config{}, runner)
	persistExeDevClaim(t, backend.configForRun(), vm, leaseID, "blue", t.TempDir())
	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: vm.SSHHost(), ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if !hasExeDevRM(runner) {
		t.Fatalf("rm not called: %#v", runner.calls)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(leaseID); err != nil || exists {
		t.Fatalf("claim exists=%v err=%v, want removed", exists, err)
	}
}

func TestExeDevReleaseRunsGuardedRemoteCleanupBeforeDelete(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_abcdef123456"
	vm := ownedExeDevVM(leaseID, "blue")
	runner := exeDevInventoryRunner(t, vm)
	backend := newExeDevTestBackend(core.Config{}, runner)
	claim := persistExeDevClaim(t, backend.configForRun(), vm, leaseID, "blue", t.TempDir())
	labels := maps.Clone(claim.Labels)
	labels["tailscale"] = "true"
	claim, err := core.UpdateLeaseClaimLabelsIfUnchangedAfter(leaseID, claim, labels, nil)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	cleaned := false
	lease.SSH.Host = "stale-endpoint.example"
	baseRun := runner.fn
	runner.fn = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		if strings.Contains(strings.Join(req.Args, " "), " rm ") && !cleaned {
			return core.LocalCommandResult{}, errors.New("delete ran before guarded remote cleanup")
		}
		return baseRun(req)
	}
	err = backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{
		Lease: lease,
		GuardedRemoteCleanup: func(_ context.Context, target core.LeaseTarget) {
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
	runner := &exeDevRecordingRunner{fn: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		if strings.Contains(strings.Join(req.Args, " "), "whoami --json") {
			return core.LocalCommandResult{Stdout: `{"email":"other@example.com"}`}, nil
		}
		return baseResponse(req)
	}}
	backend := newExeDevTestBackend(core.Config{}, runner)
	persistExeDevClaim(t, backend.configForRun(), vm, leaseID, slug, t.TempDir())

	_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: vm.Name(), ReleaseOnly: true})
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
	backend := newExeDevTestBackend(core.Config{}, runner)
	persistExeDevClaim(t, backend.configForRun(), vm, leaseID, slug, t.TempDir())
	vm.Tags = slices.DeleteFunc(vm.Tags, func(tag string) bool {
		return strings.HasPrefix(tag, exeDevClaimGenerationTagPrefix)
	})
	vm.Tags = append(vm.Tags, exeDevClaimGenerationTagPrefix+"cbx_222222222222")
	runner.fn = exeDevInventoryResponse(t, vm)

	_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, ReleaseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "generation does not match") {
		t.Fatalf("err=%v, want replacement-generation refusal", err)
	}
	assertNoExeDevRM(t, runner)
	if _, exists, readErr := core.ReadLeaseClaimWithPresence(leaseID); readErr != nil || !exists {
		t.Fatalf("claim exists=%v err=%v, want retained", exists, readErr)
	}
}

func TestExeDevReleaseSurvivesTouchedClaimRoundTrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_abcdef123456"
	slug := "blue"
	vm := ownedExeDevVM(leaseID, slug)
	runner := exeDevInventoryRunner(t, vm)
	backend := newExeDevTestBackend(core.Config{}, runner)
	cfg := backend.configForRun()
	repoRoot := t.TempDir()
	claim := persistExeDevClaim(t, cfg, vm, leaseID, slug, repoRoot)
	lease := core.LeaseTarget{
		LeaseID: leaseID,
		Server:  exeDevServer(vm, leaseID, slug, cfg, true),
	}
	lease.Server.Labels[exeDevClaimGenerationLabel] = claim.Labels[exeDevClaimGenerationLabel]
	core.SetServerLeaseClaimSnapshot(&lease.Server, claim, true)
	touched, err := backend.Touch(context.Background(), core.TouchRequest{Lease: lease, State: "ready"})
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
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: resolved}); err != nil {
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
	backend := newExeDevTestBackend(core.Config{}, runner)
	cfg := backend.configForRun()
	repoRoot := t.TempDir()
	claim := persistExeDevClaim(t, cfg, vm, leaseID, slug, repoRoot)
	lease := core.LeaseTarget{LeaseID: leaseID, Server: exeDevServer(vm, leaseID, slug, cfg, true)}
	core.SetServerLeaseClaimSnapshot(&lease.Server, claim, true)

	reclaimed, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: vm.Name(), Repo: core.Repo{Root: repoRoot}, Reclaim: true})
	if err != nil {
		t.Fatal(err)
	}
	reclaimedClaim, reclaimedExists, reclaimedSet := core.ServerLeaseClaimSnapshot(reclaimed.Server)
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
	backend := newExeDevTestBackend(core.Config{}, runner)
	claim := persistExeDevClaim(t, backend.configForRun(), vm, leaseID, slug, t.TempDir())
	lease := core.LeaseTarget{LeaseID: leaseID, Server: exeDevServer(vm, leaseID, slug, backend.configForRun(), true)}
	core.SetServerLeaseClaimSnapshot(&lease.Server, claim, true)
	if err := core.RemoveLeaseClaimIfUnchangedAfter(leaseID, claim, nil); err != nil {
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
	backend := newExeDevTestBackend(core.Config{}, runner)
	persistExeDevClaim(t, backend.configForRun(), vm, leaseID, "blue", t.TempDir())
	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: vm.Name(), ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	runner.fn = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		if strings.Contains(strings.Join(req.Args, " "), " rm ") {
			return core.LocalCommandResult{ExitCode: 1}, errors.New("delete failed")
		}
		return exeDevInventoryResponse(t, vm)(req)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err == nil || !strings.Contains(err.Error(), "delete failed") {
		t.Fatalf("err=%v, want delete failure", err)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(leaseID); err != nil || !exists {
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
			runner := &exeDevRecordingRunner{fn: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				if strings.Contains(strings.Join(req.Args, " "), "whoami --json") {
					return core.LocalCommandResult{Stdout: `{"email":"test@example.com"}`}, nil
				}
				return core.LocalCommandResult{Stdout: `{"vms":[]}`}, nil
			}}
			backend := newExeDevTestBackend(core.Config{}, runner)
			persistExeDevClaim(t, backend.configForRun(), vm, leaseID, slug, t.TempDir())

			lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: identifier, ReleaseOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			if lease.Server.Labels[exeDevConfirmedAbsentLabel] != "true" {
				t.Fatalf("labels=%v, want confirmed-absence marker", lease.Server.Labels)
			}
			if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
				t.Fatal(err)
			}
			assertNoExeDevRM(t, runner)
			if _, exists, err := core.ReadLeaseClaimWithPresence(leaseID); err != nil || exists {
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
	runner := &exeDevRecordingRunner{fn: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		if strings.Contains(strings.Join(req.Args, " "), "whoami --json") {
			return core.LocalCommandResult{Stdout: `{"email":"test@example.com"}`}, nil
		}
		return core.LocalCommandResult{Stdout: `{"vms":[]}`}, nil
	}}
	backend := newExeDevTestBackend(core.Config{}, runner)
	persistExeDevClaim(t, backend.configForRun(), vm, leaseID, slug, t.TempDir())

	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	renamed := vm
	renamed.VMName = "renamed-owned-vm"
	renamed.SSHDest = "renamed-owned-vm.exe.xyz"
	runner.fn = exeDevInventoryResponse(t, renamed)
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err == nil || !strings.Contains(err.Error(), "is present") {
		t.Fatalf("err=%v, want present-VM refusal", err)
	}
	assertNoExeDevRM(t, runner)
	if _, exists, err := core.ReadLeaseClaimWithPresence(leaseID); err != nil || !exists {
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
	backend := newExeDevTestBackend(core.Config{}, runner)
	persistExeDevClaim(t, backend.configForRun(), vm, leaseID, slug, t.TempDir())

	_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, ReleaseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "unexpected VM") {
		t.Fatalf("err=%v, want renamed owned-VM refusal", err)
	}
	assertNoExeDevRM(t, runner)
	if _, exists, readErr := core.ReadLeaseClaimWithPresence(leaseID); readErr != nil || !exists {
		t.Fatalf("claim exists=%v err=%v, want retained", exists, readErr)
	}
}

func TestExeDevAbsentReleaseRejectsDifferentAuthenticatedAccount(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_abcdef123456"
	slug := "blue"
	vm := ownedExeDevVM(leaseID, slug)
	runner := &exeDevRecordingRunner{fn: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		if strings.Contains(strings.Join(req.Args, " "), "whoami --json") {
			return core.LocalCommandResult{Stdout: `{"email":"other@example.com"}`}, nil
		}
		return core.LocalCommandResult{Stdout: `{"vms":[]}`}, nil
	}}
	backend := newExeDevTestBackend(core.Config{}, runner)
	persistExeDevClaim(t, backend.configForRun(), vm, leaseID, slug, t.TempDir())

	_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, ReleaseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "different exe.dev control route") {
		t.Fatalf("err=%v, want account-bound route refusal", err)
	}
	assertNoExeDevRM(t, runner)
	if _, exists, readErr := core.ReadLeaseClaimWithPresence(leaseID); readErr != nil || !exists {
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
			backend := newExeDevTestBackend(core.Config{}, runner)
			lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: vm.Name(), Repo: core.Repo{Root: t.TempDir()}, Reclaim: reclaim})
			if !reclaim {
				if err == nil || !strings.Contains(err.Error(), "reuse with --reclaim") {
					t.Fatalf("err=%v, want explicit reclaim refusal", err)
				}
				if _, exists, readErr := core.ReadLeaseClaimWithPresence(leaseID); readErr != nil || exists {
					t.Fatalf("claim exists=%v err=%v, want absent", exists, readErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			claim, exists, readErr := core.ReadLeaseClaimWithPresence(leaseID)
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
			if _, exists, snapshotSet := core.ServerLeaseClaimSnapshot(lease.Server); !snapshotSet || !exists {
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
	backend := newExeDevTestBackend(core.Config{}, runner)

	views, err := backend.List(context.Background(), core.ListRequest{})
	if err != nil || len(views) != 0 {
		t.Fatalf("owned inventory views=%#v err=%v, want malformed VM omitted", views, err)
	}
	views, err = backend.List(context.Background(), core.ListRequest{All: true})
	if err != nil || len(views) != 1 || views[0].Name != vm.Name() {
		t.Fatalf("all inventory views=%#v err=%v, want malformed VM visible", views, err)
	}
	if _, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: vm.Name(), ReleaseOnly: true}); err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("exact resolve err=%v, want hard malformed-tag refusal", err)
	}
}

func TestExeDevLeaseLookupSkipsOnlyUnrelatedMalformedTags(t *testing.T) {
	leaseID := "cbx_abcdef123456"
	valid := ownedExeDevVM(leaseID, "blue")
	malformed := ownedExeDevVM("cbx_other123456", "other")
	malformed.Tags = append(malformed.Tags, "crabbox-lease-cbx_second123456")

	backend := newExeDevTestBackend(core.Config{}, &exeDevRecordingRunner{fn: exeDevInventoryResponseMany(t, malformed, valid)})
	vm, found, err := backend.findVMByLeaseID(context.Background(), leaseID)
	if err != nil || !found || vm.Name() != valid.Name() {
		t.Fatalf("vm=%#v found=%v err=%v", vm, found, err)
	}
	if command := strings.Join(backend.rt.Exec.(*exeDevRecordingRunner).calls[0].Args, " "); strings.Contains(command, " -a") {
		t.Fatalf("lease lookup crossed into team-wide inventory: %s", command)
	}

	malformed.Tags = append(malformed.Tags, "crabbox-lease-"+leaseID)
	backend = newExeDevTestBackend(core.Config{}, &exeDevRecordingRunner{fn: exeDevInventoryResponseMany(t, malformed, valid)})
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
	name := core.LeaseProviderName(leaseID, slug)
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
	return &exeDevRecordingRunner{fn: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		command := strings.Join(req.Args, " ")
		if strings.Contains(command, "whoami --json") {
			return core.LocalCommandResult{Stdout: `{"email":"test@example.com"}`}, nil
		}
		if strings.Contains(command, " rm ") {
			return core.LocalCommandResult{Stdout: `{}`}, nil
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
			return core.LocalCommandResult{Stdout: `{}`}, nil
		}
		payload, err := json.Marshal(exeDevListResponse{VMs: []exeDevVM{current}})
		if err != nil {
			t.Fatal(err)
		}
		return core.LocalCommandResult{Stdout: string(payload)}, nil
	}}
}

func exeDevInventoryResponse(t *testing.T, vm exeDevVM) func(core.LocalCommandRequest) (core.LocalCommandResult, error) {
	return exeDevInventoryResponseMany(t, vm)
}

func exeDevInventoryResponseMany(t *testing.T, vms ...exeDevVM) func(core.LocalCommandRequest) (core.LocalCommandResult, error) {
	t.Helper()
	payload, err := json.Marshal(exeDevListResponse{VMs: vms})
	if err != nil {
		t.Fatal(err)
	}
	return func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		command := strings.Join(req.Args, " ")
		if strings.Contains(command, "whoami --json") {
			return core.LocalCommandResult{Stdout: `{"email":"test@example.com"}`}, nil
		}
		if strings.Contains(command, " rm ") {
			return core.LocalCommandResult{Stdout: `{}`}, nil
		}
		return core.LocalCommandResult{Stdout: string(payload)}, nil
	}
}

func newExeDevTestBackend(cfg core.Config, runner *exeDevRecordingRunner) *exeDevLeaseBackend {
	applyExeDevDefaults(&cfg)
	return &exeDevLeaseBackend{cfg: cfg, rt: core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
}

func persistExeDevClaim(t *testing.T, cfg core.Config, vm exeDevVM, leaseID, slug, repoRoot string) core.LeaseClaim {
	t.Helper()
	server := exeDevServer(vm, leaseID, slug, cfg, true)
	server.Labels[exeDevClaimGenerationLabel] = "cbx_111111111111"
	claim, err := claimLeaseTargetForRepoConfigScopeIfUnchanged(leaseID, slug, cfg, mustExeDevControlScope(t, cfg), server, exeDevSSHTarget(cfg, vm), repoRoot, cfg.IdleTimeout, false, core.LeaseClaim{}, false)
	if err != nil {
		t.Fatal(err)
	}
	return claim
}

func mustExeDevControlScope(t *testing.T, cfg core.Config) string {
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
		values := RegisterExeDevProviderFlags(fs, core.Config{})
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		cfg := core.Config{Provider: providerName}
		err := ApplyExeDevProviderFlags(&cfg, fs, values)
		if err == nil || !strings.Contains(err.Error(), "not supported for provider=exe-dev") {
			t.Fatalf("args=%v err=%v", args, err)
		}
	}
}

func TestExeDevConfigureRejectsUnsupportedTargetAndTailscale(t *testing.T) {
	for name, cfg := range map[string]core.Config{
		"macos target": {TargetOS: "macos"},
		"tailscale":    {TargetOS: targetLinux, Tailscale: core.TailscaleConfig{Enabled: true}},
		"network":      {TargetOS: targetLinux, Network: "tailscale"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Provider{}.Configure(cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: &exeDevRecordingRunner{}})
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestExeDevSSHTargetUsesSSHDestUserPortAndWorkRootLabel(t *testing.T) {
	cfg := core.Config{SSHKey: "/tmp/crabbox-default-key", ExeDev: core.ExeDevConfig{WorkRoot: "/tmp/crabbox", User: "runner"}}
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

func TestExeDevConfigFlagContract(t *testing.T) {
	t.Setenv("USER", "fixture-user")
	for _, provider := range []string{"exe-dev", "exe", "exedev", " EXE-DEV ", "aws"} {
		for _, cpu := range []int{0, -2} {
			cfg := core.BaseConfig()
			cfg.Provider = provider
			fs := flag.NewFlagSet("contract", flag.ContinueOnError)
			values := RegisterExeDevProviderFlags(fs, cfg)
			count := 0
			fs.VisitAll(func(*flag.Flag) { count++ })
			if count != 9 || fs.Lookup("exe-dev-work-root").DefValue != "" || fs.Lookup("exe-dev-image").DefValue != "" {
				t.Fatal("raw flag default surface changed")
			}
			args := []string{"--exe-dev-control-host=", "--exe-dev-image=  ", fmt.Sprintf("--exe-dev-cpus=%d", cpu), "--exe-dev-memory=", "--exe-dev-disk=", "--exe-dev-command=command", "--exe-dev-user=fixture-user", "--exe-dev-work-root=/workspace/flag", "--exe-dev-no-email=false"}
			if err := fs.Parse(args); err != nil {
				t.Fatal(err)
			}
			before := fmt.Sprintf("%#v", cfg)
			if err := ApplyExeDevProviderFlags(&cfg, fs, struct{}{}); err != nil {
				t.Fatal(err)
			}
			if fmt.Sprintf("%#v", cfg) != before {
				t.Fatal("wrong type changed config")
			}
			if err := ApplyExeDevProviderFlags(&cfg, fs, values); err != nil {
				t.Fatal(err)
			}
			want := core.ExeDevConfig{Image: "  ", CPUs: cpu, Command: "command", User: "fixture-user", WorkRoot: "/workspace/flag"}
			if provider == "exe-dev" || provider == "exe" || provider == "exedev" {
				want.ControlHost = "exe.dev"
				want.CPUs = 2
				want.Memory = "4GB"
				want.Disk = "10GB"
				if cfg.WorkRoot != "/workspace/flag" || cfg.ServerType != "default" {
					t.Fatal("selected defaults missing")
				}
			}
			if cfg.ExeDev != want {
				t.Fatalf("provider=%q flags=%#v want=%#v", provider, cfg.ExeDev, want)
			}
		}
	}
	cfg := core.BaseConfig()
	cfg.Provider = "aws"
	fs := flag.NewFlagSet("contract", flag.ContinueOnError)
	values := RegisterExeDevProviderFlags(fs, cfg)
	cfg.ExeDev = core.ExeDevConfig{ControlHost: "layered", CPUs: 6, Image: "layered", Memory: "8GB", Disk: "20GB", Command: "layered", User: "layered", WorkRoot: "/layered", NoEmail: false}
	want := cfg.ExeDev
	if err := ApplyExeDevProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.ExeDev != want {
		t.Fatal("unvisited flags overwrote layered config")
	}
}

func TestExeDevConfigFlagPhaseContract(t *testing.T) {
	for _, provider := range []string{"exe-dev", "exe", "exedev", " EXE-DEV ", "aws"} {
		for _, args := range [][]string{{"--type=machine", "--class="}, {"--type="}} {
			cfg := core.BaseConfig()
			cfg.Provider = provider
			fs := flag.NewFlagSet("contract", flag.ContinueOnError)
			fs.String("class", "", "")
			fs.String("type", "", "")
			RegisterExeDevProviderFlags(fs, cfg)
			if err := fs.Parse(append(args, "--exe-dev-image=fixture-image")); err != nil {
				t.Fatal(err)
			}
			before := fmt.Sprintf("%#v", cfg)
			err := ApplyExeDevProviderFlags(&cfg, fs, struct{}{})
			selected := provider == "exe-dev" || provider == "exe" || provider == "exedev"
			if selected {
				want := "--type is not supported for provider=exe-dev; use --exe-dev-image"
				if len(args) == 2 {
					want = "--class is not supported for provider=exe-dev; use --exe-dev-cpus, --exe-dev-memory, and --exe-dev-disk"
				}
				var exitErr core.ExitError
				if err == nil || err.Error() != want || !errors.As(err, &exitErr) || exitErr.Code != 2 {
					t.Fatalf("err=%v want=%q", err, want)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprintf("%#v", cfg) != before {
				t.Fatal("guard/wrong type changed config")
			}
		}
	}
}

func TestExeDevConfigEffectiveDefaultsContract(t *testing.T) {
	t.Setenv("USER", "fixture-user")
	for _, tc := range []struct{ providerRoot, generic, want string }{{"", "", "/tmp/crabbox"}, {"", "/work/crabbox", "/tmp/crabbox"}, {"", "/custom/root", "/custom/root"}, {"/specific/root", "/custom/root", "/specific/root"}, {"  ", "/custom/root", "  "}} {
		cfg := core.Config{WorkRoot: tc.generic, ExeDev: core.ExeDevConfig{WorkRoot: tc.providerRoot, CPUs: -2}}
		applyExeDevDefaults(&cfg)
		if cfg.ExeDev.ControlHost != "exe.dev" || cfg.ExeDev.CPUs != 2 || cfg.ExeDev.Memory != "4GB" || cfg.ExeDev.Disk != "10GB" || cfg.ExeDev.WorkRoot != tc.want || cfg.WorkRoot != tc.want || cfg.ExeDev.NoEmail || cfg.ExeDev.Image != "" {
			t.Fatalf("defaults=%#v want root=%q", cfg.ExeDev, tc.want)
		}
	}
	cfg := core.Config{ExeDev: core.ExeDevConfig{ControlHost: "  ", CPUs: 6, Memory: "  ", Disk: "  ", Image: "  "}}
	applyExeDevDefaults(&cfg)
	if cfg.ExeDev.ControlHost != "  " || cfg.ExeDev.Memory != "  " || cfg.ExeDev.Disk != "  " || cfg.ExeDev.CPUs != 6 {
		t.Fatal("raw whitespace fallback predicate changed")
	}
	for _, tc := range []struct{ raw, want string }{{"", "default"}, {"  ", "default"}, {" image ", "image"}} {
		cfg.ExeDev.Image = tc.raw
		if got := exeDevImage(cfg); got != tc.want {
			t.Fatalf("display=%q want=%q", got, tc.want)
		}
	}
	backend := NewExeDevLeaseBackend(Provider{}.Spec(), core.Config{}, core.Runtime{}).(*exeDevLeaseBackend)
	if backend.cfg.ExeDev.NoEmail || backend.cfg.ExeDev.Image != "" {
		t.Fatal("constructor filled raw false/image")
	}
	raw := backend.cfg
	raw.ExeDev.CPUs = 0
	backend.cfg = raw
	effective := backend.configForRun()
	if effective.ExeDev.CPUs != 2 || backend.cfg.ExeDev.CPUs != 0 {
		t.Fatal("configForRun lost copy/default behavior")
	}
}

func TestExeDevConfigCreateArgumentsContract(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USER", "fixture-user")
	for _, tc := range []struct {
		name, image          string
		noEmail, paddedSizes bool
	}{{"empty", "", false, false}, {"whitespace", "  ", false, true}, {"configured", " image ", true, false}} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &exeDevRecordingRunner{fn: func(core.LocalCommandRequest) (core.LocalCommandResult, error) {
				return core.LocalCommandResult{Stdout: `{"vm_name":"fixture-vm","ssh_dest":"fixture-vm.example","status":"running"}`}, nil
			}}
			cfg := core.Config{ExeDev: core.ExeDevConfig{Image: tc.image, NoEmail: tc.noEmail, Command: " echo ok "}}
			if tc.paddedSizes {
				cfg.ExeDev.Memory = "  "
				cfg.ExeDev.Disk = "  "
			}
			backend := &exeDevLeaseBackend{cfg: cfg, rt: core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
			if _, err := backend.createVM(t.Context(), backend.configForRun(), "fixture-vm", "cbx_fixture", "fixture", "cbx_generation"); err != nil {
				t.Fatal(err)
			}
			want := "new --name fixture-vm --json --tag crabbox --tag crabbox-lease-cbx_fixture --tag crabbox-slug-fixture --tag crabbox-claim-cbx_generation"
			if tc.noEmail {
				want += " --no-email"
			}
			if tc.image == " image " {
				want += " --image image"
			}
			want += " --cpu 2"
			if !tc.paddedSizes {
				want += " --memory 4GB --disk 10GB"
			}
			want += " --command 'echo ok'"
			wantArgs := []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new", "-o", "ConnectTimeout=10", "exe.dev", want}
			if len(runner.calls) != 1 || runner.calls[0].Name != "ssh" || !reflect.DeepEqual(runner.calls[0].Args, wantArgs) {
				t.Fatalf("recorded calls=%#v want args=%v", runner.calls, wantArgs)
			}
		})
	}
}

func TestInheritedWorkRootCallerContract(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USER", "fixture-user")
	for _, tc := range []struct{ providerRoot, genericRoot, want string }{
		{"", "", "/tmp/crabbox"},
		{"", "/work/crabbox", "/tmp/crabbox"},
		{"", "/Users/ec2-user/crabbox", "/tmp/crabbox"},
		{"", "C:\\crabbox", "/tmp/crabbox"},
		{"", " /work/crabbox ", " /work/crabbox "},
		{"", "/WORK/crabbox", "/WORK/crabbox"},
		{"", "c:\\crabbox", "c:\\crabbox"},
		{"", "/srv/custom", "/srv/custom"},
		{"", "/Users/alice/custom", "/Users/alice/custom"},
		{"", "D:\\custom", "D:\\custom"},
		{"", "  ", "  "},
		{" ", "/srv/custom", " "},
		{"/work/crabbox", "/srv/custom", "/work/crabbox"},
		{"relative", "/srv/custom", "relative"},
		{"/provider/root", "/srv/custom", "/provider/root"},
	} {
		for _, explicit := range []bool{false, true} {
			cfg := core.Config{Provider: "prior", WorkRoot: "/recorded/root", SSHUser: "fixture-user", SSHPort: "1234", SSHFallbackPorts: []string{"4567"}, ServerType: "prior-type", Network: "prior-network"}
			if explicit {
				core.MarkWorkRootExplicit(&cfg)
				cfg.TargetOS = "existing-target"
				cfg.WindowsMode = "prior-mode"
			}
			cfg.WorkRoot = tc.genericRoot
			cfg.ExeDev.WorkRoot = tc.providerRoot

			want := cfg
			want.Provider = "exe-dev"
			if !explicit {
				want.TargetOS = "linux"
			}
			want.ExeDev.WorkRoot = tc.want
			want.WorkRoot = tc.want
			want.ExeDev.ControlHost = "exe.dev"
			want.ExeDev.CPUs = 2
			want.ExeDev.Memory = "4GB"
			want.ExeDev.Disk = "10GB"
			want.SSHPort = "22"
			want.SSHFallbackPorts = nil
			want.ServerType = "default"
			applyExeDevDefaults(&cfg)
			if !reflect.DeepEqual(cfg, want) {
				t.Fatalf("whole config differs for roots=%q/%q explicit=%t: got=%#v want=%#v", tc.providerRoot, tc.genericRoot, explicit, cfg, want)
			}
		}
	}
}
