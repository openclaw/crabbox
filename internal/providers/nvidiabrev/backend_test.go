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

func TestNvidiaBrevDefaultsPreserveExplicitGenericWorkRoot(t *testing.T) {
	cfg := Config{WorkRoot: "/srv/crabbox"}
	applyNvidiaBrevDefaults(&cfg)
	if cfg.WorkRoot != "/srv/crabbox" || cfg.NvidiaBrev.WorkRoot != "/srv/crabbox" {
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
		{args: "create crabbox-demo-* --detached --gpu-name A100 --mode vm"},
		{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws-123","name":"{createdName}","status":"RUNNING","build_status":"READY","shell_status":"READY","health_status":"HEALTHY","instance_type":"gpu-a100","gpu":"A100"}]}`},
		{args: "refresh"},
	}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{NvidiaBrev: NvidiaBrevConfig{GPUName: "A100", Mode: "vm"}}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
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
	if claim.CloudID != "ws-123" || claim.Labels["brev_workspace_name"] != runner.createdName {
		t.Fatalf("claim=%#v", claim)
	}
	assertNoNvidiaBrevSecretArgs(t, runner.calls)
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

func TestNvidiaBrevReleaseDeleteRemovesClaimAfterProviderSuccess(t *testing.T) {
	isolateNvidiaBrevState(t)
	leaseID := "cbx_123456789abc"
	server := workspaceToServer(Config{}, brevWorkspace{ID: "ws-delete", Name: "crabbox-delete-123456789abc", Status: "RUNNING"}, leaseID, "delete", false)
	if err := claimLeaseTargetForRepoConfig(leaseID, "delete", Config{Provider: providerName}, server, SSHTarget{}, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws-delete","name":"crabbox-delete-123456789abc","status":"RUNNING"}]}`},
		{args: "delete ws-delete"},
	}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: server}}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := resolveLeaseClaimForProvider(leaseID); err != nil || ok {
		t.Fatalf("claim retained ok=%v err=%v", ok, err)
	}
}

func TestNvidiaBrevReleaseStopRetainsClaimOnProviderFailure(t *testing.T) {
	isolateNvidiaBrevState(t)
	leaseID := "cbx_abcdef123456"
	server := workspaceToServer(Config{}, brevWorkspace{ID: "ws-stop", Name: "crabbox-stop-abcdef123456", Status: "RUNNING"}, leaseID, "stop", false)
	if err := claimLeaseTargetForRepoConfig(leaseID, "stop", Config{Provider: providerName}, server, SSHTarget{}, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws-stop","name":"crabbox-stop-abcdef123456","status":"RUNNING"}]}`},
		{args: "stop ws-stop", err: errors.New("provider refused stop")},
	}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{NvidiaBrev: NvidiaBrevConfig{ReleaseAction: "stop"}}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: server}}); err == nil {
		t.Fatal("expected release failure")
	}
	if _, ok, err := resolveLeaseClaimForProvider(leaseID); err != nil || !ok {
		t.Fatalf("claim not retained ok=%v err=%v", ok, err)
	}
}

func TestNvidiaBrevReleaseStopRetainsStoppedClaimOnSuccess(t *testing.T) {
	isolateNvidiaBrevState(t)
	leaseID := "cbx_111122223333"
	server := workspaceToServer(Config{}, brevWorkspace{ID: "ws-stop", Name: "crabbox-stop-111122223333", Status: "RUNNING"}, leaseID, "stop", false)
	if err := claimLeaseTargetForRepoConfig(leaseID, "stop", Config{Provider: providerName}, server, SSHTarget{}, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws-stop","name":"crabbox-stop-111122223333","status":"RUNNING"}]}`},
		{args: "stop ws-stop"},
	}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{NvidiaBrev: NvidiaBrevConfig{ReleaseAction: "stop"}}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: server}}); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := resolveLeaseClaimForProvider(leaseID)
	if err != nil || !ok {
		t.Fatalf("claim not retained ok=%v err=%v", ok, err)
	}
	if claim.Labels["state"] != "stopped" || claim.SSHHost != "" || claim.SSHPort != 0 {
		t.Fatalf("stopped claim not updated safely: %#v", claim)
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
	if err := claimLeaseTargetForRepoConfig(leaseID, "owned", cfg, server, SSHTarget{}, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	markNvidiaBrevClaimLastUsed(t, state, leaseID, time.Now().Add(-2*time.Hour))
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws-owned","name":"crabbox-owned-123456789abc","status":"RUNNING"},{"id":"ws-manual","name":"manual-workspace","status":"RUNNING"}]}`},
		{args: "delete ws-owned"},
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
	server := workspaceToServer(Config{}, brevWorkspace{ID: "ws-stop-cleanup", Name: "crabbox-stop-cleanup-777788889999", Status: "RUNNING"}, leaseID, "stop-cleanup", false)
	cfg := Config{Provider: providerName, IdleTimeout: time.Hour, TTL: 24 * time.Hour}
	if err := claimLeaseTargetForRepoConfig(leaseID, "stop-cleanup", cfg, server, SSHTarget{}, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	markNvidiaBrevClaimLastUsed(t, state, leaseID, time.Now().Add(-2*time.Hour))
	runner := &scriptedBrevRunner{responses: []scriptedBrevResponse{
		{args: "ls --json --all", stdout: `{"workspaces":[{"id":"ws-stop-cleanup","name":"crabbox-stop-cleanup-777788889999","status":"RUNNING"}]}`},
		{args: "stop ws-stop-cleanup"},
	}}
	backend := NewNvidiaBrevBackend(Provider{}.Spec(), Config{NvidiaBrev: NvidiaBrevConfig{ReleaseAction: "stop"}}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
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
}

func TestNvidiaBrevCleanupHonorsExpiredTTLLabel(t *testing.T) {
	state, _ := isolateNvidiaBrevState(t)
	leaseID := "cbx_aaaabbbbcccc"
	server := workspaceToServer(Config{}, brevWorkspace{ID: "ws-ttl", Name: "crabbox-ttl-aaaabbbbcccc", Status: "RUNNING"}, leaseID, "ttl", false)
	cfg := Config{Provider: providerName, IdleTimeout: 24 * time.Hour, TTL: time.Hour}
	if err := claimLeaseTargetForRepoConfig(leaseID, "ttl", cfg, server, SSHTarget{}, t.TempDir(), false); err != nil {
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
	if err := claimLeaseTargetForRepoConfig(activeID, "active", cfg, activeServer, SSHTarget{}, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	if err := claimLeaseTargetForRepoConfig(keepID, "keep", cfg, keepServer, SSHTarget{}, t.TempDir(), false); err != nil {
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
	if err := claimLeaseTargetForRepoConfig(leaseID, "touch", cfg, server, SSHTarget{Host: "203.0.113.8", Port: "22", User: "brev"}, t.TempDir(), false); err != nil {
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
	return LocalCommandResult{Stdout: stdout}, next.err
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
	state := t.TempDir()
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("HOME", home)
	return state, home
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
