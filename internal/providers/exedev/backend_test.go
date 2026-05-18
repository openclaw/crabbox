package exedev

import (
	"context"
	"errors"
	"flag"
	"io"
	"reflect"
	"strings"
	"testing"
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
		all := []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new", "-o", "ConnectTimeout=10", "exe.dev", "ls --l --json --a"}
		if !reflect.DeepEqual(req.Args, base) && !reflect.DeepEqual(req.Args, all) {
			t.Fatalf("args=%v", req.Args)
		}
		return LocalCommandResult{Stdout: `{"vms":[{"vm_name":"crabbox-blue-12345678","ssh_dest":"crabbox-blue-12345678.exe.xyz","status":"running"},{"vm_name":"manual-box","ssh_dest":"manual-box.exe.xyz","status":"running"}]}`}, nil
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
		return LocalCommandResult{Stdout: `{"vms":[{"vm_name":"crabbox-blue-12345678","ssh_dest":"crabbox-blue-12345678.exe.xyz","status":"running"}]}`}, nil
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
	if err := claimLeaseForRepoProvider(leaseID, slug, providerName, t.TempDir(), 0, false); err != nil {
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
