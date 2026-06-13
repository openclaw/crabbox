package nvidiabrev

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
	"testing"
	"time"
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

func TestNormalizeBrevState(t *testing.T) {
	tests := []struct {
		status string
		want   string
	}{
		{status: "starting", want: "booting"},
		{status: "paused", want: "stopped"},
		{status: "off", want: "stopped"},
		{status: "deleted", want: "released"},
		{status: "error", want: "failed"},
		{status: "failed", want: "failed"},
		{status: "", want: "unknown"},
	}
	for _, test := range tests {
		t.Run(test.status, func(t *testing.T) {
			if got := normalizeBrevState(brevWorkspace{Status: test.status}); got != test.want {
				t.Fatalf("normalizeBrevState(%q)=%q, want %q", test.status, got, test.want)
			}
		})
	}
}

func TestNvidiaBrevDefaultsPreserveExplicitGenericWorkRoot(t *testing.T) {
	cfg := Config{
		WorkRoot: "/srv/crabbox",
		NvidiaBrev: NvidiaBrevConfig{
			WorkRoot: "/tmp/crabbox",
		},
	}
	markWorkRootExplicit(&cfg)
	applyNvidiaBrevDefaults(&cfg)
	if cfg.WorkRoot != "/srv/crabbox" || cfg.NvidiaBrev.WorkRoot != "/srv/crabbox" {
		t.Fatalf("workRoot=%q nvidiaBrev.workRoot=%q", cfg.WorkRoot, cfg.NvidiaBrev.WorkRoot)
	}
}

func TestNvidiaBrevDefaultsPreserveExplicitProviderWorkRoot(t *testing.T) {
	cfg := Config{
		WorkRoot: "/srv/crabbox",
		NvidiaBrev: NvidiaBrevConfig{
			WorkRoot: "/tmp/crabbox",
		},
	}
	markNvidiaBrevWorkRootExplicit(&cfg)
	applyNvidiaBrevDefaults(&cfg)
	if cfg.WorkRoot != "/tmp/crabbox" || cfg.NvidiaBrev.WorkRoot != "/tmp/crabbox" {
		t.Fatalf("workRoot=%q nvidiaBrev.workRoot=%q", cfg.WorkRoot, cfg.NvidiaBrev.WorkRoot)
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

func TestNvidiaBrevApplyFlagsMarksExplicitSettings(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := RegisterNvidiaBrevProviderFlags(fs, Config{})
	if err := fs.Parse([]string{"--nvidia-brev-release-action", "stop", "--nvidia-brev-work-root", "/work/brev"}); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Provider: providerName}
	if err := ApplyNvidiaBrevProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.NvidiaBrev.ReleaseAction != "stop" || !releaseActionExplicit(cfg) || cfg.NvidiaBrev.WorkRoot != "/work/brev" || !nvidiaBrevWorkRootExplicit(&cfg) {
		t.Fatalf("release action not marked explicit: %#v", cfg.NvidiaBrev)
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

func TestNvidiaBrevDoctorHonorsConfiguredOrgForReadOnlyInventory(t *testing.T) {
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		runner.calls = append(runner.calls, req)
		assertReadOnlyBrevCommand(t, req)
		switch strings.Join(req.Args, " ") {
		case "--version":
			return LocalCommandResult{Stdout: "brev version 1.0.0\n"}, nil
		case "ls --json --org example-org":
			return LocalCommandResult{Stdout: `{"workspaces":[{"id":"workspace-1"}]}`}, nil
		default:
			t.Fatalf("unexpected command: %s %s", req.Name, strings.Join(req.Args, " "))
		}
		return LocalCommandResult{}, nil
	}
	backend := &nvidiaBrevBackend{
		spec: Provider{}.Spec(),
		cfg:  Config{NvidiaBrev: NvidiaBrevConfig{CLI: "brev", Org: "example-org"}},
		rt:   Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard},
	}
	result, err := backend.Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Message, "leases=1") {
		t.Fatalf("doctor result=%#v", result)
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

func TestNvidiaBrevAcquireCreatesRefreshesParsesSSHAndClaims(t *testing.T) {
	stateDir, home := isolateNvidiaBrevState(t)
	_ = stateDir
	restoreID := stubNvidiaBrevLeaseID("cbx_123456789abc")
	defer restoreID()
	name := brevProviderName("cbx_123456789abc", "demo")
	writeBrevSSHConfig(t, home, `Host `+name+`
  HostName 203.0.113.10
  User ubuntu
  Port 2222
  IdentityFile "`+filepath.Join(home, ".brev", "brev.pem")+`"
  UserKnownHostsFile /dev/null
`)
	restoreWait := stubNvidiaBrevWaitForSSH(t, nil)
	defer restoreWait()

	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[]}`},
		{args: "create crabbox-demo-* --detached --stoppable --gpu-name A100 --mode vm"},
		{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws-123","name":"{createdName}","status":"RUNNING","build_status":"READY","shell_status":"READY","health_status":"HEALTHY","instance_type":"gpu-a100","gpu":"A100"}]}`},
		{args: "refresh"},
	}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{NvidiaBrev: NvidiaBrevConfig{GPUName: "A100", Mode: "vm", ReleaseAction: "stop"}}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	lease, err := backend.Acquire(context.Background(), AcquireRequest{Repo: Repo{Root: t.TempDir()}, RequestedSlug: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID == "" || lease.Server.CloudID != "ws-123" || lease.Server.Labels["brev_workspace_id"] != "ws-123" {
		t.Fatalf("unexpected lease: %#v", lease)
	}
	if lease.SSH.Host != "203.0.113.10" || lease.SSH.Port != "2222" || lease.SSH.User != "ubuntu" || lease.SSH.Key == "" {
		t.Fatalf("unexpected SSH target: %#v", lease.SSH)
	}
	claim, ok, err := resolveLeaseClaimForProvider(lease.LeaseID)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if claim.CloudID != "ws-123" || claim.Labels["brev_workspace_name"] != runner.createdName || claim.Labels["release"] != "stop" {
		t.Fatalf("claim=%#v", claim)
	}
	assertNoNvidiaBrevSecretArgs(t, runner.calls)
}

func TestNvidiaBrevAcquireRevalidatesOrganizationAfterCreate(t *testing.T) {
	isolateNvidiaBrevState(t)
	restoreID := stubNvidiaBrevLeaseID("cbx_123456789abc")
	defer restoreID()
	listCalls := 0
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		runner.calls = append(runner.calls, req)
		switch {
		case len(req.Args) > 0 && req.Args[0] == "create":
			writeBrevActiveOrg(t, "org-other")
			return LocalCommandResult{}, nil
		case strings.Join(req.Args, " ") == "ls --json --all":
			listCalls++
			if listCalls == 1 {
				return LocalCommandResult{Stdout: `{"workspaces":[]}`}, nil
			}
			return LocalCommandResult{Stdout: `{"workspaces":[{"id":"ws-org-switch","name":"crabbox-org-switch-123456789abc","status":"RUNNING","build_status":"READY","shell_status":"READY","health_status":"HEALTHY"}]}`}, nil
		default:
			return LocalCommandResult{}, fmt.Errorf("unexpected command: %s", strings.Join(req.Args, " "))
		}
	}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	_, err := backend.Acquire(context.Background(), AcquireRequest{Repo: Repo{Root: t.TempDir()}, RequestedSlug: "org-switch"})
	if err == nil || !strings.Contains(err.Error(), "active Brev organization changed") {
		t.Fatalf("err=%v, want organization change rejection", err)
	}
	claim, ok, claimErr := resolveLeaseClaimForProvider("cbx_123456789abc")
	if claimErr != nil || !ok {
		t.Fatalf("recovery claim ok=%v err=%v claim=%#v", ok, claimErr, claim)
	}
	if claim.CloudID != "ws-org-switch" ||
		claim.Labels["state"] != "deleting" ||
		claim.Labels["brev_recovery"] != "org_changed" ||
		claim.Labels["brev_create_org_id"] != "org-test" ||
		claim.Labels["brev_observed_org_id"] != "org-other" ||
		claim.Labels["brev_current_org_id"] != "org-other" ||
		claim.Labels["brev_org_id"] != "" {
		t.Fatalf("recovery claim=%#v", claim)
	}
	for _, call := range runner.calls {
		if len(call.Args) > 0 && call.Args[0] == "delete" {
			t.Fatalf("organization switch executed ambiguous rollback: %#v", call.Args)
		}
	}
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: claim.LeaseID}}); err == nil || !strings.Contains(err.Error(), "manual reconciliation") {
		t.Fatalf("ambiguous recovery err=%v", err)
	}
}

func TestNvidiaBrevAcquireRetainsOrganizationChangeRecoveryWhenKeepIsEnabled(t *testing.T) {
	isolateNvidiaBrevState(t)
	restoreID := stubNvidiaBrevLeaseID("cbx_123456789abc")
	defer restoreID()
	listCalls := 0
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		runner.calls = append(runner.calls, req)
		switch {
		case len(req.Args) > 0 && req.Args[0] == "create":
			writeBrevActiveOrg(t, "org-other")
			return LocalCommandResult{}, nil
		case strings.Join(req.Args, " ") == "ls --json --all":
			listCalls++
			if listCalls == 1 {
				return LocalCommandResult{Stdout: `{"workspaces":[]}`}, nil
			}
			return LocalCommandResult{Stdout: `{"workspaces":[{"id":"ws-org-keep","name":"crabbox-org-keep-123456789abc","status":"RUNNING","build_status":"READY","shell_status":"READY","health_status":"HEALTHY"}]}`}, nil
		default:
			return LocalCommandResult{}, fmt.Errorf("unexpected command: %s", strings.Join(req.Args, " "))
		}
	}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	_, err := backend.Acquire(context.Background(), AcquireRequest{Repo: Repo{Root: t.TempDir()}, RequestedSlug: "org-keep", Keep: true})
	if err == nil || !strings.Contains(err.Error(), "active Brev organization changed") {
		t.Fatalf("err=%v, want organization change rejection", err)
	}
	claim, ok, claimErr := resolveLeaseClaimForProvider("cbx_123456789abc")
	if claimErr != nil || !ok {
		t.Fatalf("recovery claim ok=%v err=%v", ok, claimErr)
	}
	if claim.Labels["brev_recovery"] != "org_changed" || claim.Labels["keep"] != "true" {
		t.Fatalf("recovery claim=%#v", claim)
	}
}

func TestNvidiaBrevAcquireRetainsOrganizationChangeDuringReadyCheck(t *testing.T) {
	isolateNvidiaBrevState(t)
	restoreID := stubNvidiaBrevLeaseID("cbx_123456789abc")
	defer restoreID()
	listCalls := 0
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		runner.calls = append(runner.calls, req)
		switch {
		case len(req.Args) > 0 && req.Args[0] == "create":
			return LocalCommandResult{}, nil
		case strings.Join(req.Args, " ") == "ls --json --all":
			listCalls++
			if listCalls == 1 {
				return LocalCommandResult{Stdout: `{"workspaces":[]}`}, nil
			}
			writeBrevActiveOrg(t, "org-other")
			return LocalCommandResult{Stdout: `{"workspaces":[{"id":"ws-org-race","name":"crabbox-org-race-123456789abc","status":"RUNNING","build_status":"READY","shell_status":"READY","health_status":"HEALTHY"}]}`}, nil
		default:
			return LocalCommandResult{}, fmt.Errorf("unexpected command: %s", strings.Join(req.Args, " "))
		}
	}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	_, err := backend.Acquire(context.Background(), AcquireRequest{Repo: Repo{Root: t.TempDir()}, RequestedSlug: "org-race"})
	if err == nil || !strings.Contains(err.Error(), "active Brev organization changed") {
		t.Fatalf("err=%v, want organization change rejection", err)
	}
	claim, ok, claimErr := resolveLeaseClaimForProvider("cbx_123456789abc")
	if claimErr != nil || !ok {
		t.Fatalf("recovery claim ok=%v err=%v", ok, claimErr)
	}
	if claim.CloudID != "ws-org-race" ||
		claim.Labels["brev_recovery"] != "org_changed" ||
		claim.Labels["brev_create_org_id"] != "org-test" ||
		claim.Labels["brev_observed_org_id"] != "org-test" ||
		claim.Labels["brev_current_org_id"] != "org-other" {
		t.Fatalf("recovery claim=%#v", claim)
	}
	for _, call := range runner.calls {
		if len(call.Args) > 0 && call.Args[0] == "delete" {
			t.Fatalf("organization race executed ambiguous rollback: %#v", call.Args)
		}
	}
}

func TestNvidiaBrevAcquireRollsBackCreatedWorkspaceOnPostCreateFailure(t *testing.T) {
	_, _ = isolateNvidiaBrevState(t)
	restoreWait := stubNvidiaBrevWaitForSSH(t, nil)
	defer restoreWait()
	restoreID := stubNvidiaBrevLeaseID("cbx_123456789abc")
	defer restoreID()
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[]}`},
		{args: "create crabbox-rollback-* --detached --gpu-name A100 --mode vm"},
		{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws-rollback","name":"{createdName}","status":"RUNNING","build_status":"READY","shell_status":"READY","health_status":"HEALTHY"}]}`},
		{args: "refresh"},
		{args: "delete ws-rollback"},
		{args: "ls --json --all", stdout: `{"workspaces":[]}`},
	}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{NvidiaBrev: NvidiaBrevConfig{GPUName: "A100", Mode: "vm"}}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	_, err := backend.Acquire(context.Background(), AcquireRequest{Repo: Repo{Root: t.TempDir()}, RequestedSlug: "rollback"})
	if err == nil || !strings.Contains(err.Error(), "read nvidia-brev SSH config") {
		t.Fatalf("err=%v, want SSH config failure", err)
	}
	if got := runner.joinedCalls(); !strings.Contains(got, "delete ws-rollback") {
		t.Fatalf("rollback did not delete created workspace; calls=%s", got)
	}
}

func TestNvidiaBrevAcquireRollbackDeletesEvenWhenReleaseActionStops(t *testing.T) {
	_, _ = isolateNvidiaBrevState(t)
	restoreWait := stubNvidiaBrevWaitForSSH(t, nil)
	defer restoreWait()
	restoreID := stubNvidiaBrevLeaseID("cbx_123456789abc")
	defer restoreID()
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[]}`},
		{args: "create crabbox-rollback-* --detached --stoppable --gpu-name A100 --mode vm"},
		{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws-rollback","name":"{createdName}","status":"RUNNING","build_status":"READY","shell_status":"READY","health_status":"HEALTHY"}]}`},
		{args: "refresh"},
		{args: "delete ws-rollback"},
		{args: "ls --json --all", stdout: `{"workspaces":[]}`},
	}}
	cfg := Config{NvidiaBrev: NvidiaBrevConfig{GPUName: "A100", Mode: "vm", ReleaseAction: "stop"}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), cfg, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	_, err := backend.Acquire(context.Background(), AcquireRequest{Repo: Repo{Root: t.TempDir()}, RequestedSlug: "rollback"})
	if err == nil || !strings.Contains(err.Error(), "read nvidia-brev SSH config") {
		t.Fatalf("err=%v, want SSH config failure", err)
	}
	if got := runner.joinedCalls(); !strings.Contains(got, "delete ws-rollback") || strings.Contains(got, "stop ws-rollback") {
		t.Fatalf("rollback should delete unclaimed workspace regardless of release action; calls=%s", got)
	}
}

func TestNvidiaBrevAcquireRetainsNameOnlyClaimAfterAmbiguousCreateFailure(t *testing.T) {
	isolateNvidiaBrevState(t)
	restoreID := stubNvidiaBrevLeaseID("cbx_123456789abc")
	defer restoreID()
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[]}`},
		{args: "create crabbox-create-error-* --detached --gpu-name A100 --mode vm", err: errors.New("connection reset after request")},
		{args: "ls --json --all", stdout: `{"workspaces":[]}`},
	}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	_, err := backend.Acquire(context.Background(), AcquireRequest{Repo: Repo{Root: t.TempDir()}, RequestedSlug: "create-error"})
	if err == nil || !strings.Contains(err.Error(), "connection reset after request") {
		t.Fatalf("err=%v, want create failure", err)
	}
	claim, ok, claimErr := resolveLeaseClaimForProvider("cbx_123456789abc")
	if claimErr != nil || !ok {
		t.Fatalf("recovery claim ok=%v err=%v", ok, claimErr)
	}
	if claim.CloudID != "" ||
		claim.Labels["state"] != "deleting" ||
		claim.Labels["brev_recovery"] != "create_unknown" ||
		claim.Labels["brev_org_id"] != "org-test" ||
		claim.Labels["brev_workspace_name"] != "crabbox-create-error-123456789abc" {
		t.Fatalf("recovery claim=%#v", claim)
	}
	if got := runner.joinedCalls(); strings.Contains(got, "delete ") {
		t.Fatalf("ambiguous create failure executed unsafe delete: %s", got)
	}
}

func TestNvidiaBrevAcquireDoesNotClaimLocallyRejectedCreate(t *testing.T) {
	isolateNvidiaBrevState(t)
	restoreID := stubNvidiaBrevLeaseID("cbx_123456789abc")
	defer restoreID()
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --org example-org --all", stdout: `{"workspaces":[]}`},
	}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{NvidiaBrev: NvidiaBrevConfig{Org: "example-org"}}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	_, err := backend.Acquire(context.Background(), AcquireRequest{Repo: Repo{Root: t.TempDir()}, RequestedSlug: "preflight"})
	if err == nil || !strings.Contains(err.Error(), "does not support --org") {
		t.Fatalf("err=%v, want local mutation rejection", err)
	}
	if _, ok, claimErr := resolveLeaseClaimForProvider("cbx_123456789abc"); claimErr != nil || ok {
		t.Fatalf("local rejection persisted claim ok=%v err=%v", ok, claimErr)
	}
	if got := runner.joinedCalls(); strings.Contains(got, "create ") {
		t.Fatalf("local rejection invoked create: %s", got)
	}
}

func TestNvidiaBrevKeptAmbiguousCreateCanReleaseByLeaseIDAfterWorkspaceAppears(t *testing.T) {
	isolateNvidiaBrevState(t)
	restoreID := stubNvidiaBrevLeaseID("cbx_123456789abc")
	defer restoreID()
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[]}`},
		{args: "create crabbox-create-keep-* --detached --gpu-name A100 --mode vm", err: errors.New("connection reset after request")},
		{args: "ls --json --all", stdout: `{"workspaces":[]}`},
	}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	_, err := backend.Acquire(context.Background(), AcquireRequest{Repo: Repo{Root: t.TempDir()}, RequestedSlug: "create-keep", Keep: true})
	if err == nil || !strings.Contains(err.Error(), "connection reset after request") {
		t.Fatalf("err=%v, want create failure", err)
	}
	claim, ok, claimErr := resolveLeaseClaimForProvider("cbx_123456789abc")
	if claimErr != nil || !ok {
		t.Fatalf("recovery claim ok=%v err=%v", ok, claimErr)
	}
	if claim.CloudID != "" || claim.Labels["state"] != "failed" || claim.Labels["brev_recovery"] != "create_unknown" || claim.Labels["keep"] != "true" {
		t.Fatalf("recovery claim=%#v", claim)
	}
	runner.responses = append(runner.responses, scriptedBrevResponse{args: "ls --json --all", stdout: `{"workspaces":[]}`})
	statusTarget, err := backend.Resolve(context.Background(), ResolveRequest{ID: claim.LeaseID, StatusOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if statusTarget.Server.Status != "failed" || statusTarget.Server.Name != "crabbox-create-keep-123456789abc" {
		t.Fatalf("status target=%#v", statusTarget)
	}
	runner.responses = append(runner.responses,
		scriptedBrevResponse{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws-create-keep","name":"crabbox-create-keep-123456789abc","status":"RUNNING"}]}`},
		scriptedBrevResponse{args: "delete ws-create-keep"},
		scriptedBrevResponse{args: "ls --json --all", stdout: `{"workspaces":[]}`},
	)
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: claim.LeaseID}}); err != nil {
		t.Fatal(err)
	}
	if _, ok, claimErr := resolveLeaseClaimForProvider(claim.LeaseID); claimErr != nil || ok {
		t.Fatalf("released recovery claim retained ok=%v err=%v", ok, claimErr)
	}
}

func TestNvidiaBrevNameOnlyRecoveryStatusReportsWorkspaceThatAppeared(t *testing.T) {
	isolateNvidiaBrevState(t)
	leaseID := "cbx_123456789abd"
	workspace := brevWorkspace{Name: "crabbox-delayed-123456789abd", Status: "CREATING"}
	server := workspaceToServer(Config{}, workspace, leaseID, "delayed", true)
	server.Status = "failed"
	server.Labels["state"] = "failed"
	server.Labels["keep"] = "true"
	server.Labels["brev_recovery"] = "create_unknown"
	if err := claimTestNvidiaBrevLeaseTargetForRepoConfig(leaseID, "delayed", Config{Provider: providerName}, server, SSHTarget{}, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	live := `{"workspaces":[{"id":"ws-delayed","name":"crabbox-delayed-123456789abd","status":"RUNNING","build_status":"READY","shell_status":"READY","health_status":"HEALTHY"}]}`
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: live},
		{args: "ls --json --all", stdout: live},
	}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	target, err := backend.Resolve(context.Background(), ResolveRequest{ID: leaseID, StatusOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if target.Server.CloudID != "ws-delayed" || target.Server.Status != "ready" {
		t.Fatalf("status target=%#v", target)
	}
}

func TestNvidiaBrevKeptAmbiguousCreateHonorsStopPolicy(t *testing.T) {
	isolateNvidiaBrevState(t)
	restoreID := stubNvidiaBrevLeaseID("cbx_123456789abc")
	defer restoreID()
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[]}`},
		{args: "create crabbox-create-stop-* --detached --stoppable --gpu-name A100 --mode vm", err: errors.New("connection reset after request")},
		{args: "ls --json --all", stdout: `{"workspaces":[]}`},
	}}
	cfg := Config{NvidiaBrev: NvidiaBrevConfig{ReleaseAction: "stop"}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), cfg, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	_, err := backend.Acquire(context.Background(), AcquireRequest{Repo: Repo{Root: t.TempDir()}, RequestedSlug: "create-stop", Keep: true})
	if err == nil {
		t.Fatal("expected create failure")
	}
	runner.responses = append(runner.responses,
		scriptedBrevResponse{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws-create-stop","name":"crabbox-create-stop-123456789abc","status":"RUNNING"}]}`},
		scriptedBrevResponse{args: "stop ws-create-stop"},
	)
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: "cbx_123456789abc"}}); err != nil {
		t.Fatal(err)
	}
	claim, ok, claimErr := resolveLeaseClaimForProvider("cbx_123456789abc")
	if claimErr != nil || !ok {
		t.Fatalf("stopped recovery claim ok=%v err=%v", ok, claimErr)
	}
	if claim.CloudID != "ws-create-stop" || claim.Labels["state"] != "stopped" || claim.Labels["release"] != "stop" {
		t.Fatalf("stopped recovery claim=%#v", claim)
	}
	if got := runner.joinedCalls(); strings.Contains(got, "delete ws-create-stop") {
		t.Fatalf("stop policy deleted recovered workspace: %s", got)
	}
}

func TestNvidiaBrevKeptAmbiguousCreateClaimExpiresOnExplicitRelease(t *testing.T) {
	state, _ := isolateNvidiaBrevState(t)
	restoreID := stubNvidiaBrevLeaseID("cbx_123456789abc")
	defer restoreID()
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[]}`},
		{args: "create crabbox-create-expired-* --detached --stoppable --gpu-name A100 --mode vm", err: errors.New("connection reset after request")},
		{args: "ls --json --all", stdout: `{"workspaces":[]}`},
	}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{NvidiaBrev: NvidiaBrevConfig{ReleaseAction: "stop"}}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	_, err := backend.Acquire(context.Background(), AcquireRequest{Repo: Repo{Root: t.TempDir()}, RequestedSlug: "create-expired", Keep: true})
	if err == nil {
		t.Fatal("expected create failure")
	}
	updateNvidiaBrevClaim(t, state, "cbx_123456789abc", func(claim map[string]any) {
		labels := claim["labels"].(map[string]any)
		labels["created_at"] = fmt.Sprint(time.Now().Add(-brevCreateRecoveryGrace - time.Minute).Unix())
	})
	runner.responses = append(runner.responses, scriptedBrevResponse{args: "ls --json --all", stdout: `{"workspaces":[]}`})
	releaseTarget, err := backend.Resolve(context.Background(), ResolveRequest{ID: "cbx_123456789abc", ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if releaseTarget.LeaseID != "cbx_123456789abc" {
		t.Fatalf("release target=%#v", releaseTarget)
	}
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: releaseTarget}); err != nil {
		t.Fatal(err)
	}
	if _, ok, claimErr := resolveLeaseClaimForProvider("cbx_123456789abc"); claimErr != nil || ok {
		t.Fatalf("expired recovery claim retained ok=%v err=%v", ok, claimErr)
	}
	if backend.RetainLeaseClaimAfterRelease(releaseTarget) {
		t.Fatal("removed stale recovery reported retained claim")
	}
	if got := backend.ReleaseLeaseMessage(releaseTarget); got != "removed stale lease=cbx_123456789abc workspace=crabbox-create-expired-123456789abc absent=true" {
		t.Fatalf("release message=%q", got)
	}
}

func TestNvidiaBrevNameOnlyReadinessFailureClaimExpiresOnExplicitRelease(t *testing.T) {
	state, _ := isolateNvidiaBrevState(t)
	leaseID := "cbx_123456789abd"
	workspace := brevWorkspace{Name: "crabbox-readiness-failed-123456789abd", Status: "CREATING"}
	server := workspaceToServer(Config{}, workspace, leaseID, "readiness-failed", true)
	server.Status = "failed"
	server.Labels["state"] = "failed"
	server.Labels["keep"] = "true"
	server.Labels["brev_recovery"] = "kept_acquire_failed"
	if err := claimTestNvidiaBrevLeaseTargetForRepoConfig(leaseID, "readiness-failed", Config{Provider: providerName}, server, SSHTarget{}, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	updateNvidiaBrevClaim(t, state, leaseID, func(claim map[string]any) {
		labels := claim["labels"].(map[string]any)
		labels["created_at"] = fmt.Sprint(time.Now().Add(-brevCreateRecoveryGrace - time.Minute).Unix())
	})
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[]}`},
	}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	releaseTarget, err := backend.Resolve(context.Background(), ResolveRequest{ID: leaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: releaseTarget}); err != nil {
		t.Fatal(err)
	}
	if _, ok, claimErr := resolveLeaseClaimForProvider(leaseID); claimErr != nil || ok {
		t.Fatalf("expired readiness claim retained ok=%v err=%v", ok, claimErr)
	}
}

func TestNvidiaBrevKnownIDReadinessFailureClaimExpiresOnExplicitRelease(t *testing.T) {
	state, _ := isolateNvidiaBrevState(t)
	leaseID := "cbx_123456789abe"
	workspace := brevWorkspace{ID: "ws-readiness-gone", Name: "crabbox-readiness-gone-123456789abe", Status: "RUNNING"}
	server := workspaceToServer(Config{}, workspace, leaseID, "readiness-gone", true)
	server.Status = "failed"
	server.Labels["state"] = "failed"
	server.Labels["keep"] = "true"
	server.Labels["brev_recovery"] = "kept_acquire_failed"
	if err := claimTestNvidiaBrevLeaseTargetForRepoConfig(leaseID, "readiness-gone", Config{Provider: providerName}, server, SSHTarget{}, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	updateNvidiaBrevClaim(t, state, leaseID, func(claim map[string]any) {
		labels := claim["labels"].(map[string]any)
		labels["created_at"] = fmt.Sprint(time.Now().Add(-brevCreateRecoveryGrace - time.Minute).Unix())
	})
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[]}`},
	}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	releaseTarget, err := backend.Resolve(context.Background(), ResolveRequest{ID: leaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: releaseTarget}); err != nil {
		t.Fatal(err)
	}
	if _, ok, claimErr := resolveLeaseClaimForProvider(leaseID); claimErr != nil || ok {
		t.Fatalf("expired known-id claim retained ok=%v err=%v", ok, claimErr)
	}
}

func TestNvidiaBrevAcquireRollsBackWorkspaceObservedAfterCreateFailure(t *testing.T) {
	isolateNvidiaBrevState(t)
	restoreID := stubNvidiaBrevLeaseID("cbx_123456789abc")
	defer restoreID()
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[]}`},
		{args: "create crabbox-create-observed-* --detached --gpu-name A100 --mode vm", err: errors.New("response lost")},
		{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws-create-observed","name":"{createdName}","status":"RUNNING"}]}`},
		{args: "delete ws-create-observed"},
		{args: "ls --json --all", stdout: `{"workspaces":[]}`},
	}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	_, err := backend.Acquire(context.Background(), AcquireRequest{Repo: Repo{Root: t.TempDir()}, RequestedSlug: "create-observed"})
	if err == nil || !strings.Contains(err.Error(), "response lost") {
		t.Fatalf("err=%v, want create failure", err)
	}
	if _, ok, claimErr := resolveLeaseClaimForProvider("cbx_123456789abc"); claimErr != nil || ok {
		t.Fatalf("rollback claim retained ok=%v err=%v", ok, claimErr)
	}
	if got := runner.joinedCalls(); !strings.Contains(got, "delete ws-create-observed") {
		t.Fatalf("observed workspace not rolled back: %s", got)
	}
}

func TestNvidiaBrevAcquireRollbackDeletesKnownWorkspaceWhenClaimPersistenceFails(t *testing.T) {
	_, home := isolateNvidiaBrevState(t)
	restoreWait := stubNvidiaBrevWaitForSSH(t, nil)
	defer restoreWait()
	restoreID := stubNvidiaBrevLeaseID("cbx_123456789abc")
	defer restoreID()
	writeBrevSSHConfig(t, home, `Host crabbox-persist-fail-123456789abc
  HostName 203.0.113.10
  User brev
  IdentityFile "`+filepath.Join(home, ".brev", "brev.pem")+`"
`)
	oldPersist := persistLeaseTargetForRepoConfig
	persistLeaseTargetForRepoConfig = func(string, string, Config, Server, SSHTarget, string, bool) error {
		return errors.New("state disk full")
	}
	defer func() { persistLeaseTargetForRepoConfig = oldPersist }()
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[]}`},
		{args: "create crabbox-persist-fail-* --detached --gpu-name A100 --mode vm"},
		{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws-persist-fail","name":"{createdName}","status":"RUNNING","build_status":"READY","shell_status":"READY","health_status":"HEALTHY"}]}`},
		{args: "refresh"},
		{args: "delete ws-persist-fail"},
		{args: "ls --json --all", stdout: `{"workspaces":[]}`},
	}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	_, err := backend.Acquire(context.Background(), AcquireRequest{Repo: Repo{Root: t.TempDir()}, RequestedSlug: "persist-fail"})
	if err == nil || !strings.Contains(err.Error(), "state disk full") {
		t.Fatalf("err=%v, want persistence failure", err)
	}
	if got := runner.joinedCalls(); !strings.Contains(got, "delete ws-persist-fail") {
		t.Fatalf("persistence failure left created workspace running: %s", got)
	}
}

func TestNvidiaBrevResolveStartsStoppedWorkspaceBeforeSSH(t *testing.T) {
	_, home := isolateNvidiaBrevState(t)
	writeBrevSSHConfig(t, home, `Host crabbox-stopped-cbx123456789
  HostName 203.0.113.10
  User brev
  IdentityFile "`+filepath.Join(home, ".brev", "brev.pem")+`"
`)
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws-stop","name":"crabbox-stopped-cbx123456789","status":"STOPPED"}]}`},
		{args: "start ws-stop --detached"},
		{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws-stop","name":"crabbox-stopped-cbx123456789","status":"RUNNING","build_status":"READY","shell_status":"READY","health_status":"HEALTHY"}]}`},
		{args: "refresh"},
	}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: "ws-stop"})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.Status != "ready" || lease.SSH.Host != "203.0.113.10" {
		t.Fatalf("stopped workspace not restarted and resolved: server=%#v ssh=%#v", lease.Server, lease.SSH)
	}
	if got := runner.joinedCalls(); !strings.Contains(got, "start ws-stop --detached") || !strings.Contains(got, "refresh") {
		t.Fatalf("resolve did not start before SSH refresh: %s", got)
	}
}

func TestNvidiaBrevResolveDoesNotStartStoppedWorkspaceForStaleClaim(t *testing.T) {
	state, _ := isolateNvidiaBrevState(t)
	leaseID := "cbx_555566667777"
	workspace := brevWorkspace{ID: "ws-stale-start", Name: "crabbox-stale-start-555566667777", Status: "STOPPED"}
	server := workspaceToServer(Config{}, workspace, leaseID, "stale-start", false)
	repoRoot := t.TempDir()
	if err := claimTestNvidiaBrevLeaseTargetForRepoConfig(leaseID, "stale-start", Config{Provider: providerName}, server, SSHTarget{}, repoRoot, false); err != nil {
		t.Fatal(err)
	}
	started := false
	runner := &fakeRunner{run: func(req LocalCommandRequest) (LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "ls --json --all":
			updateNvidiaBrevClaim(t, state, leaseID, func(claim map[string]any) {
				labels := claim["labels"].(map[string]any)
				labels["last_touched_at"] = "1800000000"
				labels["expires_at"] = "1800000300"
			})
			return LocalCommandResult{Stdout: `{"workspaces":[{"id":"ws-stale-start","name":"crabbox-stale-start-555566667777","status":"STOPPED"}]}`}, nil
		case "start ws-stale-start --detached":
			started = true
			return LocalCommandResult{}, nil
		default:
			return LocalCommandResult{}, fmt.Errorf("unexpected command: %s", strings.Join(req.Args, " "))
		}
	}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	_, err := backend.Resolve(context.Background(), ResolveRequest{ID: leaseID, Repo: Repo{Root: repoRoot}})
	if err == nil || !strings.Contains(err.Error(), "claim changed; retry") {
		t.Fatalf("err=%v, want stale claim conflict", err)
	}
	if started {
		t.Fatal("stale claim started provider workspace")
	}
}

func TestNvidiaBrevResolvePreservesClaimLabels(t *testing.T) {
	state, home := isolateNvidiaBrevState(t)
	leaseID := "cbx_123456789abc"
	slug := "preserved"
	repoRoot := t.TempDir()
	cfg := Config{Provider: providerName, IdleTimeout: 45 * time.Minute, TTL: 3 * time.Hour}
	workspace := brevWorkspace{
		ID:           "ws-preserved",
		Name:         "crabbox-preserved-123456789abc",
		Status:       "RUNNING",
		BuildStatus:  "READY",
		ShellStatus:  "READY",
		HealthStatus: "HEALTHY",
		InstanceType: "gpu-l40s",
		InstanceKind: "gpu",
		GPU:          "L40S",
	}
	server := workspaceToServer(cfg, workspace, leaseID, slug, true)
	if err := claimTestNvidiaBrevLeaseTargetForRepoConfig(leaseID, slug, cfg, server, SSHTarget{}, repoRoot, false); err != nil {
		t.Fatal(err)
	}
	updateNvidiaBrevClaim(t, state, leaseID, func(claim map[string]any) {
		labels := claim["labels"].(map[string]any)
		labels["created_at"] = "1700000000"
		labels["last_touched_at"] = "1700000100"
		labels["expires_at"] = "1700000200"
	})
	writeBrevSSHConfig(t, home, `Host crabbox-preserved-123456789abc
  HostName 203.0.113.10
  User brev
  IdentityFile "`+filepath.Join(home, ".brev", "brev.pem")+`"
`)
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws-preserved","name":"crabbox-preserved-123456789abc","status":"RUNNING","build_status":"READY","shell_status":"READY","health_status":"HEALTHY"}]}`},
		{args: "refresh"},
	}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: leaseID, Repo: Repo{Root: repoRoot}})
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"keep":            "true",
		"created_at":      "1700000000",
		"last_touched_at": "1700000100",
		"expires_at":      "1700000200",
		"server_type":     "gpu-l40s",
		"instance_kind":   "gpu",
		"gpu":             "L40S",
	} {
		if got := lease.Server.Labels[key]; got != want {
			t.Fatalf("resolved label %s=%q want %q", key, got, want)
		}
	}
	claim, ok, err := resolveLeaseClaimForProvider(leaseID)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if claim.Labels["keep"] != "true" || claim.Labels["created_at"] != "1700000000" || claim.Labels["expires_at"] != "1700000200" || claim.Labels["server_type"] != "gpu-l40s" || claim.Labels["gpu"] != "L40S" {
		t.Fatalf("claim lifecycle labels changed: %#v", claim.Labels)
	}
}

func TestNvidiaBrevResolveDoesNotOverwriteConcurrentTouch(t *testing.T) {
	state, home := isolateNvidiaBrevState(t)
	leaseID := "cbx_abcdef123456"
	slug := "concurrent"
	repoRoot := t.TempDir()
	workspace := brevWorkspace{
		ID:           "ws-concurrent",
		Name:         "crabbox-concurrent-abcdef123456",
		Status:       "RUNNING",
		BuildStatus:  "READY",
		ShellStatus:  "READY",
		HealthStatus: "HEALTHY",
	}
	server := workspaceToServer(Config{}, workspace, leaseID, slug, true)
	if err := claimTestNvidiaBrevLeaseTargetForRepoConfig(leaseID, slug, Config{Provider: providerName}, server, SSHTarget{}, repoRoot, false); err != nil {
		t.Fatal(err)
	}
	writeBrevSSHConfig(t, home, `Host crabbox-concurrent-abcdef123456
  HostName 203.0.113.10
  User brev
  IdentityFile "`+filepath.Join(home, ".brev", "brev.pem")+`"
`)
	runner := &fakeRunner{run: func(req LocalCommandRequest) (LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "ls --json --all":
			return LocalCommandResult{Stdout: `{"workspaces":[{"id":"ws-concurrent","name":"crabbox-concurrent-abcdef123456","status":"RUNNING","build_status":"READY","shell_status":"READY","health_status":"HEALTHY"}]}`}, nil
		case "refresh":
			updateNvidiaBrevClaim(t, state, leaseID, func(claim map[string]any) {
				labels := claim["labels"].(map[string]any)
				labels["last_touched_at"] = "1800000000"
				labels["expires_at"] = "1800000300"
			})
			return LocalCommandResult{}, nil
		default:
			return LocalCommandResult{}, fmt.Errorf("unexpected command: %s", strings.Join(req.Args, " "))
		}
	}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	_, err := backend.Resolve(context.Background(), ResolveRequest{ID: leaseID, Repo: Repo{Root: repoRoot}})
	if err == nil || !strings.Contains(err.Error(), "claim changed; retry") {
		t.Fatalf("err=%v, want concurrent claim conflict", err)
	}
	claim, ok, err := resolveLeaseClaimForProvider(leaseID)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if claim.Labels["last_touched_at"] != "1800000000" || claim.Labels["expires_at"] != "1800000300" {
		t.Fatalf("concurrent touch was overwritten: %#v", claim.Labels)
	}
}

func TestNvidiaBrevClaimedLifecycleRejectsOrganizationMismatchBeforeInventory(t *testing.T) {
	isolateNvidiaBrevState(t)
	leaseID := "cbx_123456789ac3"
	workspace := brevWorkspace{ID: "ws-org-mismatch", Name: "crabbox-org-mismatch-123456789ac3", Status: "RUNNING"}
	server := workspaceToServer(Config{}, workspace, leaseID, "org-mismatch", false)
	if err := claimTestNvidiaBrevLeaseTargetForRepoConfig(leaseID, "org-mismatch", Config{Provider: providerName}, server, SSHTarget{}, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	writeBrevActiveOrg(t, "org-other")
	runner := &scriptedBrevRunner{}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	if _, err := backend.Resolve(context.Background(), ResolveRequest{ID: leaseID}); err == nil || !strings.Contains(err.Error(), "active Brev organization changed") {
		t.Fatalf("resolve err=%v, want organization mismatch", err)
	}
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID}}); err == nil || !strings.Contains(err.Error(), "active Brev organization changed") {
		t.Fatalf("release err=%v, want organization mismatch", err)
	}
	if got := runner.joinedCalls(); got != "" {
		t.Fatalf("organization mismatch queried provider inventory: %s", got)
	}
}

func TestNvidiaBrevAcquireKeepSkipsRollback(t *testing.T) {
	_, _ = isolateNvidiaBrevState(t)
	restoreID := stubNvidiaBrevLeaseID("cbx_123456789abc")
	defer restoreID()
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[]}`},
		{args: "create crabbox-keep-* --detached --gpu-name A100 --mode vm"},
		{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws-keep","name":"{createdName}","status":"RUNNING","build_status":"READY","shell_status":"READY","health_status":"HEALTHY"}]}`},
		{args: "refresh"},
	}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{NvidiaBrev: NvidiaBrevConfig{GPUName: "A100", Mode: "vm"}}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	_, err := backend.Acquire(context.Background(), AcquireRequest{Repo: Repo{Root: t.TempDir()}, RequestedSlug: "keep", Keep: true})
	if err == nil {
		t.Fatal("expected acquire failure")
	}
	if got := runner.joinedCalls(); strings.Contains(got, "delete ws-keep") || strings.Contains(got, "stop ws-keep") {
		t.Fatalf("keep rollback mutated workspace; calls=%s", got)
	}
	claim, ok, claimErr := resolveLeaseClaimForProvider("cbx_123456789abc")
	if claimErr != nil || !ok {
		t.Fatalf("retained claim ok=%v err=%v", ok, claimErr)
	}
	if claim.CloudID != "ws-keep" ||
		claim.Labels["state"] != "failed" ||
		claim.Labels["keep"] != "true" ||
		claim.Labels["brev_recovery"] != "kept_acquire_failed" ||
		claim.Labels["brev_org_id"] != "org-test" {
		t.Fatalf("retained claim=%#v", claim)
	}
}

func TestNvidiaBrevResolveParsesProxySSHConfig(t *testing.T) {
	_, home := isolateNvidiaBrevState(t)
	writeBrevSSHConfig(t, home, `Host crabbox-proxy-cbx123456789
  User brev
  IdentityFile "`+filepath.Join(home, ".brev", "brev.pem")+`"
  ProxyCommand /tmp/cloudflared access ssh --hostname proxy.example
  UserKnownHostsFile /dev/null
`)
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws-proxy","name":"crabbox-proxy-cbx123456789","status":"RUNNING","build_status":"READY","shell_status":"READY","health_status":"HEALTHY"}]}`},
		{args: "refresh"},
	}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: "ws-proxy"})
	if err != nil {
		t.Fatal(err)
	}
	if !lease.SSH.SSHConfigProxy || lease.SSH.ProxyCommand == "" || lease.SSH.Host != "crabbox-proxy-cbx123456789" {
		t.Fatalf("proxy target not preserved: %#v", lease.SSH)
	}
}

func TestNvidiaBrevResolveRejectsConfiguredOrgForSSHConfigSafety(t *testing.T) {
	isolateNvidiaBrevState(t)
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --org example-org --all", stdout: `{"workspaces":[{"id":"ws-proxy","name":"crabbox-proxy-cbx123456789","status":"RUNNING","build_status":"READY","shell_status":"READY","health_status":"HEALTHY"}]}`},
	}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{NvidiaBrev: NvidiaBrevConfig{Org: "example-org"}}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	_, err := backend.Resolve(context.Background(), ResolveRequest{ID: "ws-proxy"})
	if err == nil || !strings.Contains(err.Error(), "brev refresh does not support --org") || strings.Contains(err.Error(), "example-org") {
		t.Fatalf("err=%v, want safe org-scoped SSH rejection without org value", err)
	}
	if strings.Contains(runner.joinedCalls(), "refresh") {
		t.Fatalf("org-scoped resolve refreshed active-org SSH config: %s", runner.joinedCalls())
	}
}

func TestNvidiaBrevListFiltersOwnedByDefault(t *testing.T) {
	isolateNvidiaBrevState(t)
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json", stdout: `{"workspaces":[{"id":"ws-owned","name":"crabbox-owned-cbx123456789","status":"RUNNING"},{"id":"ws-manual","name":"manual-workspace","status":"RUNNING"}]}`},
		{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws-owned","name":"crabbox-owned-cbx123456789","status":"RUNNING"},{"id":"ws-manual","name":"manual-workspace","status":"RUNNING"}]}`},
	}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	owned, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(owned) != 1 || owned[0].CloudID != "ws-owned" {
		t.Fatalf("owned list=%#v", owned)
	}
	all, err := backend.List(context.Background(), ListRequest{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("all list=%#v", all)
	}
}

func TestNvidiaBrevListAssociatesDelayedWorkspaceWithNameOnlyRecoveryClaim(t *testing.T) {
	isolateNvidiaBrevState(t)
	leaseID := "cbx_123456789abc"
	workspace := brevWorkspace{Name: "crabbox-delayed-list-123456789abc", Status: "CREATING"}
	server := workspaceToServer(Config{NvidiaBrev: NvidiaBrevConfig{ReleaseAction: "stop"}}, workspace, leaseID, "delayed-list", true)
	server.Status = "failed"
	server.Labels["state"] = "failed"
	server.Labels["keep"] = "true"
	server.Labels["brev_recovery"] = "create_unknown"
	if err := claimTestNvidiaBrevLeaseTargetForRepoConfig(leaseID, "delayed-list", Config{Provider: providerName}, server, SSHTarget{}, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json", stdout: `{"workspaces":[{"id":"ws-delayed-list","name":"crabbox-delayed-list-123456789abc","status":"RUNNING"}]}`},
	}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	servers, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 ||
		servers[0].CloudID != "ws-delayed-list" ||
		servers[0].Labels["keep"] != "true" ||
		servers[0].Labels["release"] != "stop" ||
		servers[0].Labels["brev_recovery"] != "create_unknown" {
		t.Fatalf("servers=%#v", servers)
	}
}

func TestNvidiaBrevReleaseDeleteRemovesClaimAfterProviderSuccess(t *testing.T) {
	isolateNvidiaBrevState(t)
	leaseID := "cbx_123456789abc"
	server := workspaceToServer(Config{}, brevWorkspace{ID: "ws-delete", Name: "crabbox-delete-123456789abc", Status: "RUNNING"}, leaseID, "delete", false)
	if err := claimTestNvidiaBrevLeaseTargetForRepoConfig(leaseID, "delete", Config{Provider: providerName}, server, SSHTarget{}, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws-delete","name":"crabbox-delete-123456789abc","status":"RUNNING"}]}`},
		{args: "delete ws-delete"},
		{args: "ls --json --all", stdout: `{"workspaces":[]}`},
	}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: server}}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := resolveLeaseClaimForProvider(leaseID); err != nil || ok {
		t.Fatalf("claim retained ok=%v err=%v", ok, err)
	}
}

func TestNvidiaBrevReleaseDeleteRetainsClaimUntilWorkspaceDisappears(t *testing.T) {
	isolateNvidiaBrevState(t)
	leaseID := "cbx_123456789abd"
	workspace := brevWorkspace{ID: "ws-delete-pending", Name: "crabbox-delete-pending-123456789abd", Status: "DELETING"}
	server := workspaceToServer(Config{}, workspace, leaseID, "delete-pending", false)
	if err := claimTestNvidiaBrevLeaseTargetForRepoConfig(leaseID, "delete-pending", Config{Provider: providerName}, server, SSHTarget{Host: "203.0.113.9", Port: "22", User: "brev"}, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := resolveLeaseClaimForProvider(leaseID)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "delete ws-delete-pending"},
		{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws-delete-pending","name":"crabbox-delete-pending-123456789abd","status":"DELETING"}]}`},
	}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	client, err := backend.client()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := backend.deleteWorkspaceAndRemoveClaim(ctx, client, workspace, claim); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v want context canceled", err)
	}
	pending, ok, err := resolveLeaseClaimForProvider(leaseID)
	if err != nil || !ok {
		t.Fatalf("pending deletion claim not retained ok=%v err=%v", ok, err)
	}
	if pending.Labels["state"] != "deleting" || pending.SSHHost != "" || pending.SSHPort != 0 {
		t.Fatalf("pending deletion claim=%#v", pending)
	}
	if pending.Labels["brev_org_id"] != "org-test" {
		t.Fatalf("pending deletion org=%q want org-test", pending.Labels["brev_org_id"])
	}
	for _, identifier := range []string{leaseID, "delete-pending", workspace.ID, workspace.Name} {
		if _, err := backend.Resolve(context.Background(), ResolveRequest{ID: identifier}); err == nil || !strings.Contains(err.Error(), "is deleting") {
			t.Fatalf("normal resolve id=%q err=%v, want deleting rejection", identifier, err)
		}
	}
	writeBrevActiveOrg(t, "org-other")
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{Server: Server{CloudID: workspace.ID}}}); err == nil || !strings.Contains(err.Error(), "active Brev organization changed") {
		t.Fatalf("cross-org release err=%v, want scope rejection", err)
	}
	if _, ok, err := resolveLeaseClaimForProvider(leaseID); err != nil || !ok {
		t.Fatalf("cross-org release removed claim ok=%v err=%v", ok, err)
	}
	writeBrevActiveOrg(t, "org-test")
	runner.responses = append(runner.responses, scriptedBrevResponse{args: "ls --json --all", stdout: `{"workspaces":[]}`})
	releaseTarget, err := backend.Resolve(context.Background(), ResolveRequest{ID: workspace.Name, StatusOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if releaseTarget.Server.Status != "deleting" || releaseTarget.Server.CloudID != "ws-delete-pending" {
		t.Fatalf("release target=%#v", releaseTarget)
	}
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{Server: Server{CloudID: workspace.ID}}}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := resolveLeaseClaimForProvider(leaseID); err != nil || ok {
		t.Fatalf("reconciled deletion claim retained ok=%v err=%v", ok, err)
	}
	if got := runner.joinedCalls(); strings.Count(got, "delete ws-delete-pending") != 1 {
		t.Fatalf("delete retried instead of reconciling: %s", got)
	}
}

func TestNvidiaBrevReleaseDeletePersistsRecoveryBeforeProviderCommand(t *testing.T) {
	isolateNvidiaBrevState(t)
	leaseID := "cbx_123456789ac0"
	workspace := brevWorkspace{ID: "ws-delete-retry", Name: "crabbox-delete-retry-123456789ac0", Status: "RUNNING"}
	server := workspaceToServer(Config{}, workspace, leaseID, "delete-retry", false)
	if err := claimTestNvidiaBrevLeaseTargetForRepoConfig(leaseID, "delete-retry", Config{Provider: providerName}, server, SSHTarget{Host: "203.0.113.9", Port: "22", User: "brev"}, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws-delete-retry","name":"crabbox-delete-retry-123456789ac0","status":"RUNNING"}]}`},
		{args: "delete ws-delete-retry", err: errors.New("provider delete interrupted")},
	}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID}})
	if err == nil || !strings.Contains(err.Error(), "provider delete interrupted") {
		t.Fatalf("release err=%v, want provider delete failure", err)
	}
	pending, ok, err := resolveLeaseClaimForProvider(leaseID)
	if err != nil || !ok {
		t.Fatalf("pending claim ok=%v err=%v", ok, err)
	}
	if pending.Labels["state"] != "deleting" || pending.SSHHost != "" || pending.SSHPort != 0 {
		t.Fatalf("provider failure did not persist recovery claim: %#v", pending)
	}
	runner.responses = append(runner.responses,
		scriptedBrevResponse{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws-delete-retry","name":"crabbox-delete-retry-123456789ac0","status":"RUNNING"}]}`},
		scriptedBrevResponse{args: "delete ws-delete-retry"},
		scriptedBrevResponse{args: "ls --json --all", stdout: `{"workspaces":[]}`},
	)
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID}}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := resolveLeaseClaimForProvider(leaseID); err != nil || ok {
		t.Fatalf("retry retained claim ok=%v err=%v", ok, err)
	}
	if got := strings.Count(runner.joinedCalls(), "delete ws-delete-retry"); got != 2 {
		t.Fatalf("delete attempts=%d want 2: %s", got, runner.joinedCalls())
	}
}

func TestNvidiaBrevReleaseBackfillsLegacyClaimOrganizationScope(t *testing.T) {
	state, _ := isolateNvidiaBrevState(t)
	leaseID := "cbx_123456789ac1"
	workspace := brevWorkspace{ID: "ws-delete-legacy", Name: "crabbox-delete-legacy-123456789ac1", Status: "RUNNING"}
	server := workspaceToServer(Config{}, workspace, leaseID, "delete-legacy", false)
	if err := claimTestNvidiaBrevLeaseTargetForRepoConfig(leaseID, "delete-legacy", Config{Provider: providerName}, server, SSHTarget{Host: "203.0.113.9", Port: "22", User: "brev"}, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	updateNvidiaBrevClaim(t, state, leaseID, func(claim map[string]any) {
		labels := claim["labels"].(map[string]any)
		delete(labels, "brev_org_id")
	})
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws-delete-legacy","name":"crabbox-delete-legacy-123456789ac1","status":"RUNNING"}]}`},
		{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws-delete-legacy","name":"crabbox-delete-legacy-123456789ac1","status":"RUNNING"}]}`},
		{args: "delete ws-delete-legacy"},
		{args: "ls --json --all", stdout: `{"workspaces":[]}`},
	}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID}}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := resolveLeaseClaimForProvider(leaseID); err != nil || ok {
		t.Fatalf("legacy release retained claim ok=%v err=%v", ok, err)
	}
	if got := runner.joinedCalls(); strings.Count(got, "ls --json --all") != 3 || !strings.Contains(got, "delete ws-delete-legacy") {
		t.Fatalf("legacy migration calls=%s", got)
	}
}

func TestNvidiaBrevAmbiguousCreateRecoveryRetainsNameOnlyClaimDuringGrace(t *testing.T) {
	state, _ := isolateNvidiaBrevState(t)
	leaseID := "cbx_123456789ac2"
	workspace := brevWorkspace{Name: "crabbox-create-unknown-123456789ac2", Status: "CREATING"}
	server := workspaceToServer(Config{}, workspace, leaseID, "create-unknown", false)
	server.Status = "deleting"
	server.Labels["state"] = "deleting"
	server.Labels["brev_recovery"] = "create_unknown"
	if err := claimTestNvidiaBrevLeaseTargetForRepoConfig(leaseID, "create-unknown", Config{Provider: providerName}, server, SSHTarget{}, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[]}`},
		{args: "ls --json --all", stdout: `{"workspaces":[]}`},
	}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID}})
	if err == nil || !strings.Contains(err.Error(), "ambiguous create recovery claim retained") {
		t.Fatalf("release err=%v, want grace retention", err)
	}
	if _, ok, err := resolveLeaseClaimForProvider(leaseID); err != nil || !ok {
		t.Fatalf("grace removed recovery claim ok=%v err=%v", ok, err)
	}
	updateNvidiaBrevClaim(t, state, leaseID, func(claim map[string]any) {
		labels := claim["labels"].(map[string]any)
		labels["created_at"] = fmt.Sprint(time.Now().Add(-brevCreateRecoveryGrace - time.Minute).Unix())
	})
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID}}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := resolveLeaseClaimForProvider(leaseID); err != nil || ok {
		t.Fatalf("expired recovery claim retained ok=%v err=%v", ok, err)
	}
}

func TestNvidiaBrevCleanupReconcilesDeletingClaimAfterWorkspaceDisappears(t *testing.T) {
	state, _ := isolateNvidiaBrevState(t)
	leaseID := "cbx_123456789abe"
	workspace := brevWorkspace{ID: "ws-delete-gone", Name: "crabbox-delete-gone-123456789abe", Status: "RUNNING"}
	server := workspaceToServer(Config{}, workspace, leaseID, "delete-gone", false)
	if err := claimTestNvidiaBrevLeaseTargetForRepoConfig(leaseID, "delete-gone", Config{Provider: providerName}, server, SSHTarget{Host: "203.0.113.9", Port: "22", User: "brev"}, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	updateNvidiaBrevClaim(t, state, leaseID, func(claim map[string]any) {
		labels := claim["labels"].(map[string]any)
		labels["state"] = "deleting"
		labels["brev_org_id"] = "org-test"
	})
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[]}`},
		{args: "ls --json --all", stdout: `{"workspaces":[]}`},
		{args: "ls --json --all", stdout: `{"workspaces":[]}`},
		{args: "ls --json --all", stdout: `{"workspaces":[]}`},
	}}
	var stderr strings.Builder
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: &stderr}).(*nvidiaBrevBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := resolveLeaseClaimForProvider(leaseID); err != nil || !ok {
		t.Fatalf("dry-run removed deleting claim ok=%v err=%v", ok, err)
	}
	if !strings.Contains(stderr.String(), "state=deleting present=false") {
		t.Fatalf("cleanup stderr=%q", stderr.String())
	}
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := resolveLeaseClaimForProvider(leaseID); err != nil || ok {
		t.Fatalf("cleanup retained absent deleting claim ok=%v err=%v", ok, err)
	}
	if strings.Contains(runner.joinedCalls(), "delete ws-delete-gone") {
		t.Fatalf("cleanup retried delete for absent workspace: %s", runner.joinedCalls())
	}
}

func TestNvidiaBrevDeletingClaimRejectsOrgScopedLifecycle(t *testing.T) {
	state, _ := isolateNvidiaBrevState(t)
	leaseID := "cbx_123456789abf"
	workspace := brevWorkspace{ID: "ws-delete-org", Name: "crabbox-delete-org-123456789abf", Status: "RUNNING"}
	server := workspaceToServer(Config{}, workspace, leaseID, "delete-org", false)
	if err := claimTestNvidiaBrevLeaseTargetForRepoConfig(leaseID, "delete-org", Config{Provider: providerName}, server, SSHTarget{}, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	updateNvidiaBrevClaim(t, state, leaseID, func(claim map[string]any) {
		labels := claim["labels"].(map[string]any)
		labels["state"] = "deleting"
	})
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --org example-org --all", stdout: `{"workspaces":[{"id":"ws-delete-org","name":"crabbox-delete-org-123456789abf","status":"DELETING"}]}`},
	}}
	cfg := Config{NvidiaBrev: NvidiaBrevConfig{Org: "example-org"}}
	var stderr strings.Builder
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), cfg, Runtime{Exec: runner, Stdout: io.Discard, Stderr: &stderr}).(*nvidiaBrevBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{DryRun: true}); err != nil {
		t.Fatalf("org-scoped dry-run err=%v", err)
	}
	if !strings.Contains(stderr.String(), "state=deleting present=true") {
		t.Fatalf("org-scoped dry-run stderr=%q", stderr.String())
	}
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err == nil || !strings.Contains(err.Error(), "scopes read-only Brev inventory") {
		t.Fatalf("cleanup err=%v, want org-scoped lifecycle rejection", err)
	}
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID}}); err == nil || !strings.Contains(err.Error(), "scopes read-only Brev inventory") {
		t.Fatalf("release err=%v, want org-scoped lifecycle rejection", err)
	}
	if _, ok, err := resolveLeaseClaimForProvider(leaseID); err != nil || !ok {
		t.Fatalf("org-scoped lifecycle removed deleting claim ok=%v err=%v", ok, err)
	}
	if got := runner.joinedCalls(); got != "ls --json --org example-org --all" {
		t.Fatalf("org-scoped lifecycle commands: %s", got)
	}
}

func TestNvidiaBrevReleaseStopRetainsClaimOnProviderFailure(t *testing.T) {
	isolateNvidiaBrevState(t)
	leaseID := "cbx_abcdef123456"
	server := workspaceToServer(Config{NvidiaBrev: NvidiaBrevConfig{ReleaseAction: "stop"}}, brevWorkspace{ID: "ws-stop", Name: "crabbox-stop-abcdef123456", Status: "RUNNING"}, leaseID, "stop", false)
	if err := claimTestNvidiaBrevLeaseTargetForRepoConfig(leaseID, "stop", Config{Provider: providerName}, server, SSHTarget{}, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws-stop","name":"crabbox-stop-abcdef123456","status":"RUNNING"}]}`},
		{args: "stop ws-stop", err: errors.New("provider refused stop")},
	}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: server}}); err == nil {
		t.Fatal("expected release failure")
	}
	if _, ok, err := resolveLeaseClaimForProvider(leaseID); err != nil || !ok {
		t.Fatalf("claim not retained ok=%v err=%v", ok, err)
	}
}

func TestNvidiaBrevStopDoesNotMutateProviderForStaleClaim(t *testing.T) {
	state, _ := isolateNvidiaBrevState(t)
	leaseID := "cbx_999900001111"
	workspace := brevWorkspace{ID: "ws-stale", Name: "crabbox-stale-999900001111", Status: "RUNNING"}
	server := workspaceToServer(Config{}, workspace, leaseID, "stale", false)
	if err := claimTestNvidiaBrevLeaseTargetForRepoConfig(leaseID, "stale", Config{Provider: providerName}, server, SSHTarget{}, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	stale, ok, err := resolveLeaseClaimForProvider(leaseID)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	updateNvidiaBrevClaim(t, state, leaseID, func(claim map[string]any) {
		labels := claim["labels"].(map[string]any)
		labels["last_touched_at"] = "1800000000"
		labels["expires_at"] = "1800000300"
	})
	runner := &scriptedBrevRunner{}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{NvidiaBrev: NvidiaBrevConfig{ReleaseAction: "stop"}}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	client, err := backend.client()
	if err != nil {
		t.Fatal(err)
	}
	err = backend.stopWorkspaceAndPersistClaim(context.Background(), client, workspace, stale)
	if err == nil || !strings.Contains(err.Error(), "claim changed; retry") {
		t.Fatalf("err=%v, want stale claim conflict", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("stale claim mutated provider: %s", runner.joinedCalls())
	}
}

func TestNvidiaBrevDeleteDoesNotMutateProviderForStaleClaim(t *testing.T) {
	state, _ := isolateNvidiaBrevState(t)
	leaseID := "cbx_999900002222"
	workspace := brevWorkspace{ID: "ws-stale-delete", Name: "crabbox-stale-delete-999900002222", Status: "RUNNING"}
	server := workspaceToServer(Config{}, workspace, leaseID, "stale-delete", false)
	if err := claimTestNvidiaBrevLeaseTargetForRepoConfig(leaseID, "stale-delete", Config{Provider: providerName}, server, SSHTarget{}, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	stale, ok, err := resolveLeaseClaimForProvider(leaseID)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	updateNvidiaBrevClaim(t, state, leaseID, func(claim map[string]any) {
		labels := claim["labels"].(map[string]any)
		labels["last_touched_at"] = "1800000000"
	})
	runner := &scriptedBrevRunner{}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	client, err := backend.client()
	if err != nil {
		t.Fatal(err)
	}
	err = backend.deleteWorkspaceAndRemoveClaim(context.Background(), client, workspace, stale)
	if err == nil || !strings.Contains(err.Error(), "claim changed; retry") {
		t.Fatalf("err=%v, want stale claim conflict", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("stale claim mutated provider: %s", runner.joinedCalls())
	}
}

func TestNvidiaBrevReleaseStopRetainsStoppedClaimOnSuccess(t *testing.T) {
	state, _ := isolateNvidiaBrevState(t)
	leaseID := "cbx_111122223333"
	server := workspaceToServer(Config{NvidiaBrev: NvidiaBrevConfig{ReleaseAction: "stop"}}, brevWorkspace{ID: "ws-stop", Name: "crabbox-stop-111122223333", Status: "RUNNING"}, leaseID, "stop", true)
	if err := claimTestNvidiaBrevLeaseTargetForRepoConfig(leaseID, "stop", Config{Provider: providerName}, server, SSHTarget{}, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	updateNvidiaBrevClaim(t, state, leaseID, func(claim map[string]any) {
		labels := claim["labels"].(map[string]any)
		labels["created_at"] = "1700000000"
		labels["expires_at"] = "1700000200"
	})
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws-stop","name":"crabbox-stop-111122223333","status":"RUNNING"}]}`},
		{args: "stop ws-stop"},
	}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: server}}); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := resolveLeaseClaimForProvider(leaseID)
	if err != nil || !ok {
		t.Fatalf("claim not retained ok=%v err=%v", ok, err)
	}
	if claim.Labels["state"] != "stopped" || claim.Labels["keep"] != "true" || claim.Labels["created_at"] != "1700000000" || claim.Labels["expires_at"] != "1700000200" || claim.SSHHost != "" || claim.SSHPort != 0 {
		t.Fatalf("stopped claim not updated safely: %#v", claim)
	}
}

func TestNvidiaBrevExplicitDeleteOverridesStoredStopPolicy(t *testing.T) {
	isolateNvidiaBrevState(t)
	leaseID := "cbx_444455556666"
	server := workspaceToServer(Config{NvidiaBrev: NvidiaBrevConfig{ReleaseAction: "stop"}}, brevWorkspace{ID: "ws-override", Name: "crabbox-override-444455556666", Status: "RUNNING"}, leaseID, "override", false)
	if err := claimTestNvidiaBrevLeaseTargetForRepoConfig(leaseID, "override", Config{Provider: providerName}, server, SSHTarget{}, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws-override","name":"crabbox-override-444455556666","status":"RUNNING"}]}`},
		{args: "delete ws-override"},
		{args: "ls --json --all", stdout: `{"workspaces":[]}`},
	}}
	cfg := Config{NvidiaBrev: NvidiaBrevConfig{ReleaseAction: "delete"}}
	markReleaseActionExplicit(&cfg)
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), cfg, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: server}}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := resolveLeaseClaimForProvider(leaseID); err != nil || ok {
		t.Fatalf("claim retained ok=%v err=%v", ok, err)
	}
}

func TestNvidiaBrevExplicitStopOverridesAndReplacesStoredDeletePolicy(t *testing.T) {
	isolateNvidiaBrevState(t)
	leaseID := "cbx_555566667777"
	server := workspaceToServer(Config{}, brevWorkspace{ID: "ws-stop-override", Name: "crabbox-stop-override-555566667777", Status: "RUNNING"}, leaseID, "stop-override", false)
	if err := claimTestNvidiaBrevLeaseTargetForRepoConfig(leaseID, "stop-override", Config{Provider: providerName}, server, SSHTarget{}, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws-stop-override","name":"crabbox-stop-override-555566667777","status":"RUNNING"}]}`},
		{args: "stop ws-stop-override"},
	}}
	cfg := Config{NvidiaBrev: NvidiaBrevConfig{ReleaseAction: "stop"}}
	markReleaseActionExplicit(&cfg)
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), cfg, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: server}}); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := resolveLeaseClaimForProvider(leaseID)
	if err != nil || !ok {
		t.Fatalf("claim not retained ok=%v err=%v", ok, err)
	}
	if claim.Labels["state"] != "stopped" || claim.Labels["release"] != "stop" {
		t.Fatalf("explicit stop policy not persisted: %#v", claim.Labels)
	}
}

func TestNvidiaBrevRetainLeaseClaimAfterReleaseUsesStoredPolicy(t *testing.T) {
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{}).(*nvidiaBrevBackend)
	if !backend.RetainLeaseClaimAfterRelease(LeaseTarget{Server: Server{Labels: map[string]string{"release": "stop"}}}) {
		t.Fatal("stored stop policy did not retain claim")
	}
	if backend.RetainLeaseClaimAfterRelease(LeaseTarget{Server: Server{Labels: map[string]string{"release": "delete"}}}) {
		t.Fatal("stored delete policy retained claim")
	}
	cfg := Config{NvidiaBrev: NvidiaBrevConfig{ReleaseAction: "delete"}}
	markReleaseActionExplicit(&cfg)
	backend = NewNvidiaBrevBackend(Provider{}.Spec(), cfg, Runtime{}).(*nvidiaBrevBackend)
	if backend.RetainLeaseClaimAfterRelease(LeaseTarget{Server: Server{Labels: map[string]string{"release": "stop"}}}) {
		t.Fatal("explicit delete policy did not override stored stop policy")
	}
	cfg.NvidiaBrev.ReleaseAction = "stop"
	backend = NewNvidiaBrevBackend(Provider{}.Spec(), cfg, Runtime{}).(*nvidiaBrevBackend)
	if backend.RetainLeaseClaimAfterRelease(LeaseTarget{Server: Server{Labels: map[string]string{"state": "deleting", "release": "delete"}}}) {
		t.Fatal("deleting claim retained under explicit stop override")
	}
}

func TestNvidiaBrevReleaseLeaseMessageUsesEffectivePolicy(t *testing.T) {
	lease := LeaseTarget{
		LeaseID: "cbx_123456789abc",
		Server: Server{
			CloudID: "ws-message",
			Labels:  map[string]string{"release": "stop"},
		},
	}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{}).(*nvidiaBrevBackend)
	if got := backend.ReleaseLeaseMessage(lease); got != "stopped lease=cbx_123456789abc workspace=ws-message retained=true" {
		t.Fatalf("stop message=%q", got)
	}
	lease.Server.Labels["release"] = "delete"
	if got := backend.ReleaseLeaseMessage(lease); got != "deleted lease=cbx_123456789abc workspace=ws-message" {
		t.Fatalf("delete message=%q", got)
	}
	cfg := Config{NvidiaBrev: NvidiaBrevConfig{ReleaseAction: "stop"}}
	markReleaseActionExplicit(&cfg)
	backend = NewNvidiaBrevBackend(Provider{}.Spec(), cfg, Runtime{}).(*nvidiaBrevBackend)
	if got := backend.ReleaseLeaseMessage(lease); got != "stopped lease=cbx_123456789abc workspace=ws-message retained=true" {
		t.Fatalf("explicit stop message=%q", got)
	}
	lease.Server.Labels["state"] = "deleting"
	if got := backend.ReleaseLeaseMessage(lease); got != "deleted lease=cbx_123456789abc workspace=ws-message" {
		t.Fatalf("deleting message=%q", got)
	}
}

func TestNvidiaBrevReleaseRefusesUnclaimedWorkspace(t *testing.T) {
	isolateNvidiaBrevState(t)
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws-manual","name":"manual-workspace","status":"RUNNING"}]}`},
	}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: "ws-manual"}})
	if err == nil || !strings.Contains(err.Error(), "without a local Crabbox claim") {
		t.Fatalf("err=%v", err)
	}
	if strings.Contains(runner.joinedCalls(), "delete ws-manual") {
		t.Fatalf("unclaimed release mutated workspace: %s", runner.joinedCalls())
	}
}

func TestNvidiaBrevCleanupDryRunSkipsUnclaimedManualWorkspace(t *testing.T) {
	isolateNvidiaBrevState(t)
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws-owned","name":"crabbox-owned-123456789abc","status":"RUNNING"},{"id":"ws-manual","name":"manual-workspace","status":"RUNNING"}]}`},
	}}
	var stderr strings.Builder
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: &stderr}).(*nvidiaBrevBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(runner.joinedCalls(), "delete") || strings.Contains(runner.joinedCalls(), "stop") {
		t.Fatalf("dry run mutated workspace: %s", runner.joinedCalls())
	}
	if !strings.Contains(stderr.String(), "reason=not-crabbox-owned") || !strings.Contains(stderr.String(), "reason=no-local-cleanup-claim") {
		t.Fatalf("cleanup stderr=%q", stderr.String())
	}
}

func TestNvidiaBrevCleanupDeletesOnlyCrabboxOwnedWorkspaces(t *testing.T) {
	state, _ := isolateNvidiaBrevState(t)
	leaseID := "cbx_123456789abc"
	server := workspaceToServer(Config{}, brevWorkspace{ID: "ws-owned", Name: "crabbox-owned-123456789abc", Status: "RUNNING"}, leaseID, "owned", false)
	cfg := Config{Provider: providerName, IdleTimeout: time.Hour, TTL: 24 * time.Hour}
	if err := claimTestNvidiaBrevLeaseTargetForRepoConfig(leaseID, "owned", cfg, server, SSHTarget{}, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	markNvidiaBrevClaimLastUsed(t, state, leaseID, time.Now().Add(-2*time.Hour))
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws-owned","name":"crabbox-owned-123456789abc","status":"RUNNING"},{"id":"ws-manual","name":"manual-workspace","status":"RUNNING"}]}`},
		{args: "delete ws-owned"},
		{args: "ls --json --all", stdout: `{"workspaces":[]}`},
	}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if got := runner.joinedCalls(); !strings.Contains(got, "delete ws-owned") || strings.Contains(got, "delete ws-manual") {
		t.Fatalf("unexpected cleanup calls: %s", got)
	}
}

func TestNvidiaBrevCleanupStopRetainsStoppedClaim(t *testing.T) {
	state, _ := isolateNvidiaBrevState(t)
	leaseID := "cbx_777788889999"
	server := workspaceToServer(Config{NvidiaBrev: NvidiaBrevConfig{ReleaseAction: "stop"}}, brevWorkspace{ID: "ws-stop-cleanup", Name: "crabbox-stop-cleanup-777788889999", Status: "RUNNING"}, leaseID, "stop-cleanup", false)
	cfg := Config{Provider: providerName, IdleTimeout: time.Hour, TTL: 24 * time.Hour}
	if err := claimTestNvidiaBrevLeaseTargetForRepoConfig(leaseID, "stop-cleanup", cfg, server, SSHTarget{}, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	markNvidiaBrevClaimLastUsed(t, state, leaseID, time.Now().Add(-2*time.Hour))
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws-stop-cleanup","name":"crabbox-stop-cleanup-777788889999","status":"RUNNING"}]}`},
		{args: "stop ws-stop-cleanup"},
		{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws-stop-cleanup","name":"crabbox-stop-cleanup-777788889999","status":"STOPPED"}]}`},
	}}
	var stderr strings.Builder
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: &stderr}).(*nvidiaBrevBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := resolveLeaseClaimForProvider(leaseID)
	if err != nil || !ok {
		t.Fatalf("stopped cleanup claim not retained ok=%v err=%v", ok, err)
	}
	if claim.Labels["state"] != "stopped" {
		t.Fatalf("claim state=%q want stopped: %#v", claim.Labels["state"], claim)
	}
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if got := runner.joinedCalls(); strings.Count(got, "stop ws-stop-cleanup") != 1 {
		t.Fatalf("stopped workspace was stopped repeatedly: %s", got)
	}
	if !strings.Contains(stderr.String(), "reason=stopped") {
		t.Fatalf("cleanup stderr=%q", stderr.String())
	}
}

func TestNvidiaBrevCleanupReconcilesAlreadyStoppedClaim(t *testing.T) {
	isolateNvidiaBrevState(t)
	leaseID := "cbx_222233334444"
	workspace := brevWorkspace{ID: "ws-external-stop", Name: "crabbox-external-stop-222233334444", Status: "STOPPED"}
	server := workspaceToServer(Config{NvidiaBrev: NvidiaBrevConfig{ReleaseAction: "stop"}}, brevWorkspace{ID: workspace.ID, Name: workspace.Name, Status: "RUNNING"}, leaseID, "external-stop", false)
	cfg := Config{Provider: providerName, IdleTimeout: time.Hour}
	if err := claimTestNvidiaBrevLeaseTargetForRepoConfig(leaseID, "external-stop", cfg, server, SSHTarget{Host: "203.0.113.8", Port: "22", User: "brev"}, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws-external-stop","name":"crabbox-external-stop-222233334444","status":"STOPPED"}]}`},
	}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(runner.joinedCalls(), "stop ws-external-stop") {
		t.Fatalf("already stopped workspace was stopped again: %s", runner.joinedCalls())
	}
	claim, ok, err := resolveLeaseClaimForProvider(leaseID)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if claim.Labels["state"] != "stopped" || claim.SSHHost != "" || claim.SSHPort != 0 {
		t.Fatalf("stopped claim was not reconciled: %#v", claim)
	}
}

func TestNvidiaBrevCleanupHonorsExpiredTTLLabel(t *testing.T) {
	state, _ := isolateNvidiaBrevState(t)
	leaseID := "cbx_aaaabbbbcccc"
	server := workspaceToServer(Config{}, brevWorkspace{ID: "ws-ttl", Name: "crabbox-ttl-aaaabbbbcccc", Status: "RUNNING"}, leaseID, "ttl", false)
	cfg := Config{Provider: providerName, IdleTimeout: 24 * time.Hour, TTL: time.Hour}
	if err := claimTestNvidiaBrevLeaseTargetForRepoConfig(leaseID, "ttl", cfg, server, SSHTarget{}, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	updateNvidiaBrevClaim(t, state, leaseID, func(claim map[string]any) {
		claim["lastUsedAt"] = time.Now().UTC().Format(time.RFC3339)
		labels := claim["labels"].(map[string]any)
		labels["expires_at"] = fmt.Sprint(time.Now().Add(-time.Minute).UTC().Unix())
	})
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws-ttl","name":"crabbox-ttl-aaaabbbbcccc","status":"RUNNING"}]}`},
		{args: "delete ws-ttl"},
		{args: "ls --json --all", stdout: `{"workspaces":[]}`},
	}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if got := runner.joinedCalls(); !strings.Contains(got, "delete ws-ttl") {
		t.Fatalf("cleanup did not honor expired TTL: %s", got)
	}
}

func TestNvidiaBrevCleanupPreservesActiveAndKeepClaims(t *testing.T) {
	state, _ := isolateNvidiaBrevState(t)
	activeID := "cbx_aabbccddeeff"
	keepID := "cbx_ffeeccbbaa00"
	cfg := Config{Provider: providerName, IdleTimeout: time.Hour, TTL: 24 * time.Hour}
	activeServer := workspaceToServer(cfg, brevWorkspace{ID: "ws-active", Name: "crabbox-active-aabbccddeeff", Status: "RUNNING"}, activeID, "active", false)
	keepServer := workspaceToServer(cfg, brevWorkspace{ID: "ws-keep", Name: "crabbox-keep-ffeeccbbaa00", Status: "RUNNING"}, keepID, "keep", true)
	if err := claimTestNvidiaBrevLeaseTargetForRepoConfig(activeID, "active", cfg, activeServer, SSHTarget{}, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	if err := claimTestNvidiaBrevLeaseTargetForRepoConfig(keepID, "keep", cfg, keepServer, SSHTarget{}, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	markNvidiaBrevClaimLastUsed(t, state, activeID, time.Now())
	markNvidiaBrevClaimLastUsed(t, state, keepID, time.Now().Add(-48*time.Hour))
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws-active","name":"crabbox-active-aabbccddeeff","status":"RUNNING"},{"id":"ws-keep","name":"crabbox-keep-ffeeccbbaa00","status":"RUNNING"}]}`},
	}}
	var stderr strings.Builder
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: &stderr}).(*nvidiaBrevBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if got := runner.joinedCalls(); strings.Contains(got, "delete") || strings.Contains(got, "stop") {
		t.Fatalf("cleanup mutated active/keep claims: %s", got)
	}
	if !strings.Contains(stderr.String(), "reason=active-claim") || !strings.Contains(stderr.String(), "reason=keep") {
		t.Fatalf("cleanup stderr=%q", stderr.String())
	}
}

func TestNvidiaBrevTouchRefreshesClaimBeforeCleanup(t *testing.T) {
	state, _ := isolateNvidiaBrevState(t)
	leaseID := "cbx_444455556666"
	server := workspaceToServer(Config{}, brevWorkspace{ID: "ws-touch", Name: "crabbox-touch-444455556666", Status: "RUNNING"}, leaseID, "touch", false)
	cfg := Config{Provider: providerName, IdleTimeout: time.Hour}
	if err := claimTestNvidiaBrevLeaseTargetForRepoConfig(leaseID, "touch", cfg, server, SSHTarget{Host: "203.0.113.8", Port: "22", User: "brev"}, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	markNvidiaBrevClaimLastUsed(t, state, leaseID, time.Now().Add(-2*time.Hour))
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws-touch","name":"crabbox-touch-444455556666","status":"RUNNING"}]}`},
	}}
	var stderr strings.Builder
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: &stderr}).(*nvidiaBrevBackend)
	if _, err := backend.Touch(context.Background(), TouchRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: server, SSH: SSHTarget{Host: "203.0.113.8", Port: "22", User: "brev"}}, State: "ready", IdleTimeout: 3 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := resolveLeaseClaimForProvider(leaseID)
	if err != nil || !ok {
		t.Fatalf("claim after touch ok=%v err=%v", ok, err)
	}
	if claim.IdleTimeoutSeconds != int((3 * time.Hour).Seconds()) {
		t.Fatalf("idle timeout seconds=%d", claim.IdleTimeoutSeconds)
	}
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if got := runner.joinedCalls(); strings.Contains(got, "delete ws-touch") || strings.Contains(got, "stop ws-touch") {
		t.Fatalf("cleanup released touched active lease: %s", got)
	}
	if !strings.Contains(stderr.String(), "reason=active-claim") {
		t.Fatalf("cleanup stderr=%q", stderr.String())
	}
}

func TestNvidiaBrevTouchRefusesStoppedClaim(t *testing.T) {
	state, _ := isolateNvidiaBrevState(t)
	leaseID := "cbx_888899990000"
	server := workspaceToServer(Config{}, brevWorkspace{ID: "ws-touch-stopped", Name: "crabbox-touch-stopped-888899990000", Status: "RUNNING"}, leaseID, "touch-stopped", false)
	cfg := Config{Provider: providerName, IdleTimeout: time.Hour}
	target := SSHTarget{Host: "203.0.113.8", Port: "22", User: "brev"}
	if err := claimTestNvidiaBrevLeaseTargetForRepoConfig(leaseID, "touch-stopped", cfg, server, target, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	updateNvidiaBrevClaim(t, state, leaseID, func(claim map[string]any) {
		labels := claim["labels"].(map[string]any)
		labels["state"] = "stopped"
	})
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{Exec: &scriptedBrevRunner{}, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	_, err := backend.Touch(context.Background(), TouchRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: server, SSH: target}, State: "ready"})
	if err == nil || !strings.Contains(err.Error(), "is stopped") {
		t.Fatalf("err=%v, want stopped claim rejection", err)
	}
	claim, ok, err := resolveLeaseClaimForProvider(leaseID)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if claim.Labels["state"] != "stopped" {
		t.Fatalf("stopped claim overwritten: %#v", claim.Labels)
	}
}

func TestNvidiaBrevTouchRefusesDeletingClaim(t *testing.T) {
	state, _ := isolateNvidiaBrevState(t)
	leaseID := "cbx_888899990001"
	server := workspaceToServer(Config{}, brevWorkspace{ID: "ws-touch-deleting", Name: "crabbox-touch-deleting-888899990001", Status: "DELETING"}, leaseID, "touch-deleting", false)
	cfg := Config{Provider: providerName, IdleTimeout: time.Hour}
	if err := claimTestNvidiaBrevLeaseTargetForRepoConfig(leaseID, "touch-deleting", cfg, server, SSHTarget{}, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	updateNvidiaBrevClaim(t, state, leaseID, func(claim map[string]any) {
		labels := claim["labels"].(map[string]any)
		labels["state"] = "deleting"
	})
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{Exec: &scriptedBrevRunner{}, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	_, err := backend.Touch(context.Background(), TouchRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: server}, State: "ready"})
	if err == nil || !strings.Contains(err.Error(), "is deleting") {
		t.Fatalf("err=%v, want deleting claim rejection", err)
	}
}

func TestNvidiaBrevSlugAllocationIncludesLocalClaims(t *testing.T) {
	isolateNvidiaBrevState(t)
	leaseID := "cbx_123456789abc"
	server := workspaceToServer(Config{}, brevWorkspace{ID: "ws-shared", Name: "crabbox-shared-123456789abc", Status: "RUNNING"}, leaseID, "shared", false)
	if err := claimTestNvidiaBrevLeaseTargetForRepoConfig(leaseID, "shared", Config{Provider: providerName}, server, SSHTarget{}, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	claims, err := listLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	if got := allocateBrevLeaseSlug("cbx_abcdef123456", "shared", nil, claims); got != "shared-01" {
		t.Fatalf("slug=%q want shared-01", got)
	}
}

func TestNvidiaBrevCollisionSlugFitsWorkspaceNameLimit(t *testing.T) {
	leaseID := "cbx_123456789abc"
	first := allocateBrevLeaseSlug(leaseID, strings.Repeat("a", 80), nil, nil)
	servers := []LeaseView{{Labels: map[string]string{"slug": first}}}
	second := allocateBrevLeaseSlug(leaseID, strings.Repeat("a", 80), servers, nil)
	if first == second || !strings.HasSuffix(second, "-01") {
		t.Fatalf("collision slug first=%q second=%q", first, second)
	}
	if name := brevProviderName(leaseID, second); len(name) > brevWorkspaceNameMaxLen {
		t.Fatalf("workspace name length=%d name=%q", len(name), name)
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

type scriptedBrevResponse struct {
	args   string
	stdout string
	stderr string
	err    error
}

type scriptedBrevRunner struct {
	calls       []LocalCommandRequest
	responses   []scriptedBrevResponse
	createdName string
}

func (r *scriptedBrevRunner) Run(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	r.calls = append(r.calls, req)
	if req.Name != "brev" {
		return LocalCommandResult{}, errors.New("unexpected command name " + req.Name)
	}
	if len(r.responses) == 0 {
		return LocalCommandResult{}, errors.New("unexpected command: " + strings.Join(req.Args, " "))
	}
	next := r.responses[0]
	r.responses = r.responses[1:]
	got := strings.Join(req.Args, " ")
	if strings.Contains(next.args, "*") {
		prefix, suffix, _ := strings.Cut(next.args, "*")
		if !strings.HasPrefix(got, prefix) || !strings.HasSuffix(got, suffix) {
			return LocalCommandResult{}, errors.New("got command " + got + ", want " + next.args)
		}
	} else if got != next.args {
		return LocalCommandResult{}, errors.New("got command " + got + ", want " + next.args)
	}
	if len(req.Args) >= 2 && req.Args[0] == "create" {
		r.createdName = req.Args[1]
	}
	stdout := strings.ReplaceAll(next.stdout, "{createdName}", r.createdName)
	return LocalCommandResult{Stdout: stdout, Stderr: next.stderr}, next.err
}

func (r *scriptedBrevRunner) joinedCalls() string {
	var calls []string
	for _, call := range r.calls {
		calls = append(calls, strings.Join(call.Args, " "))
	}
	return strings.Join(calls, "\n")
}

func isolateNvidiaBrevState(t *testing.T) (string, string) {
	t.Helper()
	isolateBrevContextFiles(t)
	state := t.TempDir()
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("HOME", home)
	writeBrevActiveOrg(t, "org-test")
	return state, home
}

func claimTestNvidiaBrevLeaseTargetForRepoConfig(leaseID, slug string, cfg Config, server Server, target SSHTarget, repoRoot string, reclaim bool) error {
	labels := make(map[string]string, len(server.Labels)+1)
	for key, value := range server.Labels {
		labels[key] = value
	}
	labels["brev_org_id"] = "org-test"
	server.Labels = labels
	return claimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, target, repoRoot, reclaim)
}

func writeBrevActiveOrg(t *testing.T, id string) {
	t.Helper()
	home := os.Getenv("HOME")
	dir := filepath.Join(home, ".brev")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data := []byte(fmt.Sprintf(`{"id":%q,"name":"test-org"}`, id))
	if err := os.WriteFile(filepath.Join(dir, "active_org.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	credentials := []byte(fmt.Sprintf(`{"api_key":"bak-test","api_key_org_id":%q}`, id))
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), credentials, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeBrevSSHConfig(t *testing.T, home, data string) {
	t.Helper()
	dir := filepath.Join(home, ".brev")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ssh_config"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func stubNvidiaBrevWaitForSSH(t *testing.T, err error) func() {
	t.Helper()
	old := waitForSSH
	waitForSSH = func(context.Context, *SSHTarget, io.Writer) error { return err }
	return func() { waitForSSH = old }
}

func stubNvidiaBrevLeaseID(id string) func() {
	old := newLeaseID
	newLeaseID = func() string { return id }
	return func() { newLeaseID = old }
}

func markNvidiaBrevClaimLastUsed(t *testing.T, stateDir, leaseID string, lastUsed time.Time) {
	t.Helper()
	updateNvidiaBrevClaim(t, stateDir, leaseID, func(claim map[string]any) {
		claim["lastUsedAt"] = lastUsed.UTC().Format(time.RFC3339)
	})
}

func updateNvidiaBrevClaim(t *testing.T, stateDir, leaseID string, update func(map[string]any)) {
	t.Helper()
	path := filepath.Join(stateDir, "crabbox", "claims", leaseID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var claim map[string]any
	if err := json.Unmarshal(data, &claim); err != nil {
		t.Fatal(err)
	}
	update(claim)
	updated, err := json.MarshalIndent(claim, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, updated, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertNoNvidiaBrevSecretArgs(t *testing.T, calls []LocalCommandRequest) {
	t.Helper()
	for _, call := range calls {
		for _, arg := range call.Args {
			switch strings.ToLower(arg) {
			case "nvidia-brev-token", "--nvidia-brev-token", "nvidia-brev-api-key", "--nvidia-brev-api-key", "nvidia-brev-password", "--nvidia-brev-password", "nvidia-brev-private-key", "--nvidia-brev-private-key":
				t.Fatalf("secret-like Brev arg surfaced: %s", strings.Join(call.Args, " "))
			}
		}
	}
}
