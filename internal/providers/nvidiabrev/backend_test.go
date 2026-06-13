package nvidiabrev

import (
	"context"
	"flag"
	"io"
	"strings"
	"testing"
)

func TestNvidiaBrevProviderSpec(t *testing.T) {
	spec := Provider{}.Spec()
	if spec.Name != providerName || spec.Family != "nvidia-brev" || spec.Kind != "ssh-lease" || spec.Coordinator != "never" {
		t.Fatalf("unexpected spec: %#v", spec)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != targetLinux {
		t.Fatalf("targets=%#v, want linux only", spec.Targets)
	}
	for _, feature := range []Feature{"ssh", "crabbox-sync", "cleanup"} {
		if !spec.Features.Has(feature) {
			t.Fatalf("missing feature %q in %#v", feature, spec.Features)
		}
	}
	if got := strings.Join(Provider{}.Aliases(), ","); got != "brev,nvidia" {
		t.Fatalf("aliases=%q", got)
	}
}

func TestNvidiaBrevProviderDefaults(t *testing.T) {
	cfg := Config{}
	applyNvidiaBrevDefaults(&cfg)
	if cfg.NvidiaBrev.CLI != "brev" ||
		cfg.NvidiaBrev.GPUName != "A100" ||
		cfg.NvidiaBrev.Mode != "vm" ||
		cfg.NvidiaBrev.ReleaseAction != "delete" ||
		cfg.NvidiaBrev.Target != "container" ||
		cfg.NvidiaBrev.WorkRoot != "/tmp/crabbox" ||
		cfg.TargetOS != targetLinux {
		t.Fatalf("defaults not applied: %#v", cfg.NvidiaBrev)
	}
}

func TestNvidiaBrevSecretFlagsAreNotRegistered(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	RegisterNvidiaBrevProviderFlags(fs, Config{})
	for _, name := range []string{
		"nvidia-brev-token",
		"nvidia-brev-api-key",
		"nvidia-brev-password",
		"nvidia-brev-private-key",
		"nvidia-brev-refresh-token",
	} {
		if fs.Lookup(name) != nil {
			t.Fatalf("secret-like NVIDIA Brev value surfaced as --%s", name)
		}
	}
	for _, name := range []string{
		"nvidia-brev-cli",
		"nvidia-brev-org",
		"nvidia-brev-type",
		"nvidia-brev-gpu-name",
		"nvidia-brev-provider",
		"nvidia-brev-mode",
		"nvidia-brev-launchable",
		"nvidia-brev-startup-script",
		"nvidia-brev-release-action",
		"nvidia-brev-target",
		"nvidia-brev-user",
		"nvidia-brev-work-root",
	} {
		if fs.Lookup(name) == nil {
			t.Fatalf("missing non-secret flag --%s", name)
		}
	}
}

func TestNvidiaBrevApplyFlagsRejectsGenericClassAndType(t *testing.T) {
	for _, args := range [][]string{
		{"--class", "beast"},
		{"--type", "ubuntu:24.04"},
	} {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fs.String("class", "", "")
		fs.String("type", "", "")
		values := RegisterNvidiaBrevProviderFlags(fs, Config{})
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		cfg := Config{Provider: providerName}
		err := ApplyNvidiaBrevProviderFlags(&cfg, fs, values)
		if err == nil || !strings.Contains(err.Error(), "not supported for provider=nvidia-brev") {
			t.Fatalf("args=%v err=%v", args, err)
		}
	}
}

func TestNvidiaBrevValidateConfigRejectsInvalidEnums(t *testing.T) {
	if err := (Provider{}).ValidateConfig(Config{NvidiaBrev: NvidiaBrevConfig{ReleaseAction: "archive"}}); err == nil {
		t.Fatal("invalid release action accepted")
	}
	if err := (Provider{}).ValidateConfig(Config{NvidiaBrev: NvidiaBrevConfig{Target: "desktop"}}); err == nil {
		t.Fatal("invalid target accepted")
	}
	if err := (Provider{}).ValidateConfig(Config{NvidiaBrev: NvidiaBrevConfig{ReleaseAction: "stop", Target: "host"}}); err != nil {
		t.Fatalf("valid enum values rejected: %v", err)
	}
}

func TestNvidiaBrevConfigureRejectsUnsupportedTargetAndTailscale(t *testing.T) {
	for name, cfg := range map[string]Config{
		"macos target": {TargetOS: "macos"},
		"tailscale":    {TargetOS: targetLinux, Tailscale: TailscaleConfig{Enabled: true}},
		"network":      {TargetOS: targetLinux, Network: "tailscale"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Provider{}.Configure(cfg, Runtime{Stdout: io.Discard, Stderr: io.Discard})
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestNvidiaBrevDoctorRunsReadOnlyCommands(t *testing.T) {
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		runner.calls = append(runner.calls, req)
		assertReadOnlyBrevCommand(t, req)
		switch strings.Join(req.Args, " ") {
		case "--version":
			return LocalCommandResult{Stdout: "brev version 1.0.0\n"}, nil
		case "ls --json":
			return LocalCommandResult{Stdout: `{"workspaces":[{"id":"workspace-1"},{"id":"workspace-2"}]}`}, nil
		default:
			t.Fatalf("unexpected command: %s %s", req.Name, strings.Join(req.Args, " "))
		}
		return LocalCommandResult{}, nil
	}
	doctor, err := Provider{}.ConfigureDoctor(Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	result, err := doctor.Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider != providerName || !strings.Contains(result.Message, "mutation=false") || !strings.Contains(result.Message, "leases=2") {
		t.Fatalf("unexpected doctor result: %#v", result)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("calls=%d want 2", len(runner.calls))
	}
}

func TestNvidiaBrevDoctorAcceptsEmptyWorkspaceList(t *testing.T) {
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		runner.calls = append(runner.calls, req)
		assertReadOnlyBrevCommand(t, req)
		if strings.Join(req.Args, " ") == "--version" {
			return LocalCommandResult{Stdout: "brev version 1.0.0\n"}, nil
		}
		return LocalCommandResult{Stdout: `{"workspaces": null}`}, nil
	}
	backend := &nvidiaBrevBackend{
		spec: Provider{}.Spec(),
		cfg:  Config{NvidiaBrev: NvidiaBrevConfig{CLI: "brev"}},
		rt:   Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard},
	}
	result, err := backend.Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Message, "leases=0") {
		t.Fatalf("doctor did not accept empty account JSON: %#v", result)
	}
}

func TestNvidiaBrevDoctorRejectsMalformedInventoryJSON(t *testing.T) {
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		runner.calls = append(runner.calls, req)
		assertReadOnlyBrevCommand(t, req)
		if strings.Join(req.Args, " ") == "--version" {
			return LocalCommandResult{Stdout: "brev version 1.0.0\n"}, nil
		}
		return LocalCommandResult{Stdout: `{"items":[]}`}, nil
	}
	backend := &nvidiaBrevBackend{
		spec: Provider{}.Spec(),
		cfg:  Config{NvidiaBrev: NvidiaBrevConfig{CLI: "brev"}},
		rt:   Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard},
	}
	if _, err := backend.Doctor(context.Background(), DoctorRequest{}); err == nil || !strings.Contains(err.Error(), "missing workspaces field") {
		t.Fatalf("Doctor err=%v, want missing workspaces field", err)
	}
}

func TestNvidiaBrevLifecycleStubsAreExplicitlyUnsupported(t *testing.T) {
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{}).(*nvidiaBrevBackend)
	if _, err := backend.Acquire(context.Background(), AcquireRequest{}); err == nil || !strings.Contains(err.Error(), "PLAN-02") {
		t.Fatalf("Acquire err=%v", err)
	}
	if _, err := backend.Resolve(context.Background(), ResolveRequest{}); err == nil || !strings.Contains(err.Error(), "PLAN-02") {
		t.Fatalf("Resolve err=%v", err)
	}
	if _, err := backend.List(context.Background(), ListRequest{}); err == nil || !strings.Contains(err.Error(), "PLAN-02") {
		t.Fatalf("List err=%v", err)
	}
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{}); err == nil || !strings.Contains(err.Error(), "PLAN-02") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err == nil || !strings.Contains(err.Error(), "PLAN-02") {
		t.Fatalf("Cleanup err=%v", err)
	}
}

func assertReadOnlyBrevCommand(t *testing.T, req LocalCommandRequest) {
	t.Helper()
	if req.Name != "brev" {
		t.Fatalf("command name=%q, want brev", req.Name)
	}
	for _, arg := range req.Args {
		switch strings.ToLower(arg) {
		case "create", "start", "stop", "delete", "shell", "exec", "port-forward":
			t.Fatalf("doctor used mutating Brev command: %s %s", req.Name, strings.Join(req.Args, " "))
		}
	}
}

type fakeRunner struct {
	calls []LocalCommandRequest
	run   func(LocalCommandRequest) (LocalCommandResult, error)
}

func (r *fakeRunner) Run(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	if r.run != nil {
		return r.run(req)
	}
	r.calls = append(r.calls, req)
	return LocalCommandResult{}, nil
}
