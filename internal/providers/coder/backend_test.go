package coder

import (
	"context"
	"errors"
	"flag"
	"io"
	"strings"
	"testing"
	"time"
)

type fakeRunner struct {
	calls []LocalCommandRequest
	run   func(LocalCommandRequest) (LocalCommandResult, error)
}

func (r *fakeRunner) Run(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	r.calls = append(r.calls, req)
	if r.run != nil {
		return r.run(req)
	}
	return LocalCommandResult{}, nil
}

func TestCoderProviderSpec(t *testing.T) {
	spec := Provider{}.Spec()
	if spec.Name != coderProvider || spec.Kind != "ssh-lease" || spec.Coordinator != "never" {
		t.Fatalf("unexpected spec: %#v", spec)
	}
	for _, feature := range []Feature{Feature("ssh"), Feature("crabbox-sync"), Feature("cleanup")} {
		if !spec.Features.Has(feature) {
			t.Fatalf("features=%v missing %s", spec.Features, feature)
		}
	}
}

func TestCoderFlagsApplyWithoutSecrets(t *testing.T) {
	cfg := Config{Provider: coderProvider, TargetOS: targetLinux, Coder: CoderConfig{CLIPath: "coder", WorkRoot: "/home/coder/crabbox", WorkspacePrefix: "crabbox-", Wait: "yes"}}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := RegisterCoderProviderFlags(fs, cfg)
	if err := fs.Parse([]string{"--coder-template", "go-dev", "--coder-preset", "large", "--coder-parameter", "region=iad,size=large", "--coder-delete-on-release"}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyCoderProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Coder.Template != "go-dev" || cfg.Coder.Preset != "large" || len(cfg.Coder.Parameters) != 2 || !cfg.Coder.DeleteOnRelease {
		t.Fatalf("flags not applied: %#v", cfg.Coder)
	}
	fs.VisitAll(func(f *flag.Flag) {
		if strings.Contains(f.Name, "token") || strings.Contains(f.Name, "session") {
			t.Fatalf("coder provider must not expose token/session flags: %s", f.Name)
		}
	})
}

func TestCoderCreateCommandUsesTemplateParametersAndNoTokenArgv(t *testing.T) {
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		args := strings.Join(req.Args, " ")
		for _, forbidden := range []string{"CODER_SESSION_TOKEN", "token", "secret"} {
			if strings.Contains(strings.ToLower(args), strings.ToLower(forbidden)) {
				t.Fatalf("create argv leaked forbidden value %q: %s", forbidden, args)
			}
		}
		if strings.Contains(args, "--wait ") {
			t.Fatalf("create args must not use unsupported --wait flag:\n%s", args)
		}
		for _, want := range []string{"create", "--yes", "--template go-dev", "--preset large", "--no-wait", "--use-parameter-defaults", "--parameter region=iad", "--parameter size=large", "--rich-parameter-file /tmp/params.yaml", "crabbox-blue"} {
			if !strings.Contains(args, want) {
				t.Fatalf("create args missing %q:\n%s", want, args)
			}
		}
		return LocalCommandResult{}, nil
	}
	client := &coderClient{cliPath: "coder", runner: runner, stdout: io.Discard, stderr: io.Discard}
	cfg := Config{Coder: CoderConfig{Template: "go-dev", Preset: "large", Wait: "no", UseParameterDefaults: true, Parameters: []string{"region=iad", "size=large"}, RichParameterFile: "/tmp/params.yaml"}}
	if err := client.create(context.Background(), cfg, "crabbox-blue"); err != nil {
		t.Fatal(err)
	}
}

func TestCoderSSHTargetUsesProxyCommand(t *testing.T) {
	target := coderSSHTarget(Config{Coder: CoderConfig{CLIPath: "/opt/Coder CLI/coder", Wait: "yes"}}, "crabbox-blue")
	if !target.SSHConfigProxy || target.Host != "crabbox-blue" || target.User != "coder" || target.TargetOS != targetLinux {
		t.Fatalf("unexpected target: %#v", target)
	}
	for _, want := range []string{"'/opt/Coder CLI/coder'", "ssh", "--stdio", "--wait", "'yes'", "'crabbox-blue'"} {
		if !strings.Contains(target.ProxyCommand, want) {
			t.Fatalf("proxy command %q missing %q", target.ProxyCommand, want)
		}
	}
	if !strings.Contains(target.ReadyCheck, "command -v git") || !strings.Contains(target.ReadyCheck, "command -v rsync") || !strings.Contains(target.ReadyCheck, "command -v tar") {
		t.Fatalf("ready check missing expected tools: %q", target.ReadyCheck)
	}
}

func TestCoderReleaseStopsByDefaultAndDeletesOnlyWhenConfigured(t *testing.T) {
	for _, tc := range []struct {
		name     string
		delete   bool
		wantArgs string
	}{
		{name: "stop default", wantArgs: "stop --yes crabbox-blue"},
		{name: "delete opt in", delete: true, wantArgs: "delete --yes crabbox-blue"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{}
			backend, err := NewCoderLeaseBackend(Provider{}.Spec(), Config{Coder: CoderConfig{CLIPath: "coder", WorkspacePrefix: "crabbox-", WorkRoot: "/home/coder/crabbox", Wait: "yes", DeleteOnRelease: tc.delete}}, Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner})
			if err != nil {
				t.Fatal(err)
			}
			err = backend.(*coderLeaseBackend).ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: "cbx_123", Server: Server{Name: "crabbox-blue"}}})
			if err != nil {
				t.Fatal(err)
			}
			if len(runner.calls) != 1 || strings.Join(runner.calls[0].Args, " ") != tc.wantArgs {
				t.Fatalf("calls=%#v want %s", runner.calls, tc.wantArgs)
			}
		})
	}
}

func TestCoderAcquireStopsWorkspaceWhenPostCreateInventoryMissesIt(t *testing.T) {
	runner := &fakeRunner{}
	listCalls := 0
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "list -o json":
			listCalls++
			return LocalCommandResult{Stdout: `[]`}, nil
		case "create --yes --template go-dev crabbox-blue":
			return LocalCommandResult{}, nil
		case "stop --yes crabbox-blue":
			return LocalCommandResult{}, nil
		default:
			t.Fatalf("unexpected command: %s", strings.Join(req.Args, " "))
		}
		return LocalCommandResult{}, nil
	}
	backend, err := NewCoderLeaseBackend(Provider{}.Spec(), Config{IdleTimeout: time.Hour, Coder: CoderConfig{CLIPath: "coder", Template: "go-dev", WorkspacePrefix: "crabbox-", WorkRoot: "/home/coder/crabbox", Wait: "yes"}}, Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner})
	if err != nil {
		t.Fatal(err)
	}
	_, err = backend.(*coderLeaseBackend).Acquire(context.Background(), AcquireRequest{RequestedSlug: "blue", Repo: Repo{Root: t.TempDir()}})
	if err == nil || !strings.Contains(err.Error(), "created but not found") {
		t.Fatalf("expected inventory miss error, got %v", err)
	}
	if listCalls != 2 {
		t.Fatalf("list calls=%d want 2", listCalls)
	}
	if got := strings.Join(runner.calls[len(runner.calls)-1].Args, " "); got != "stop --yes crabbox-blue" {
		t.Fatalf("final rollback command=%q", got)
	}
}

func TestCoderDoctorClassifiesMissingLoginNonMutating(t *testing.T) {
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "version":
			return LocalCommandResult{Stdout: "Coder v2.33.5"}, nil
		case "whoami -o json":
			return LocalCommandResult{ExitCode: 1, Stderr: "You are not logged in"}, errors.New("exit 1")
		default:
			t.Fatalf("doctor must be non-mutating, got: %s", strings.Join(req.Args, " "))
		}
		return LocalCommandResult{}, nil
	}
	backend := &coderLeaseBackend{spec: Provider{}.Spec(), cfg: Config{Coder: CoderConfig{CLIPath: "coder"}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	result, err := backend.Doctor(context.Background(), DoctorRequest{})
	if err == nil {
		t.Fatal("expected missing login error")
	}
	if !strings.Contains(result.Message, "auth=missing_login") || !strings.Contains(result.Message, "mutation=false") {
		t.Fatalf("unexpected doctor result: %#v", result)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("calls=%d want 2", len(runner.calls))
	}
}

func TestCoderListAndCleanupFilterCrabboxOwnedStoppedWorkspaces(t *testing.T) {
	installCoderClaimState(t)
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "list -o json":
			return LocalCommandResult{Stdout: `[
				{"id":"ws1","name":"crabbox-blue","template_name":"go-dev","latest_build":{"status":"stopped"}},
				{"id":"ws2","name":"personal","template_name":"go-dev","latest_build":{"status":"running"}}
			]`}, nil
		case "stop --yes crabbox-blue":
			return LocalCommandResult{}, nil
		default:
			t.Fatalf("unexpected command: %s", strings.Join(req.Args, " "))
		}
		return LocalCommandResult{}, nil
	}
	backend, err := NewCoderLeaseBackend(Provider{}.Spec(), Config{Coder: CoderConfig{CLIPath: "coder", WorkspacePrefix: "crabbox-", WorkRoot: "/home/coder/crabbox", Wait: "yes"}}, Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner})
	if err != nil {
		t.Fatal(err)
	}
	servers, err := backend.(*coderLeaseBackend).List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || servers[0].Name != "crabbox-blue" || serverSlug(servers[0]) != "blue" {
		t.Fatalf("unexpected servers: %#v", servers)
	}
	if err := backend.(*coderLeaseBackend).Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(runner.calls[len(runner.calls)-1].Args, " "); got != "stop --yes crabbox-blue" {
		t.Fatalf("cleanup final call=%q", got)
	}
}

func TestCoderCleanupSkipsActiveClaimedAndRunningUnclaimedWorkspaces(t *testing.T) {
	installCoderClaimState(t)
	if err := claimLeaseForRepoProvider("cbx_active", "active", coderProvider, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "list -o json":
			return LocalCommandResult{Stdout: `[
				{"id":"ws1","name":"crabbox-active","template_name":"go-dev","latest_build":{"status":"running","resources":[{"agents":[{"name":"main","operating_system":"linux","status":"connected","lifecycle_state":"ready"}]}]}},
				{"id":"ws2","name":"crabbox-unclaimed","template_name":"go-dev","latest_build":{"status":"running","resources":[{"agents":[{"name":"main","operating_system":"linux","status":"connected","lifecycle_state":"ready"}]}]}},
				{"id":"ws3","name":"personal","template_name":"go-dev","latest_build":{"status":"running"}}
			]`}, nil
		default:
			t.Fatalf("cleanup must skip active/running workspaces, got: %s", strings.Join(req.Args, " "))
		}
		return LocalCommandResult{}, nil
	}
	backend, err := NewCoderLeaseBackend(Provider{}.Spec(), Config{Coder: CoderConfig{CLIPath: "coder", WorkspacePrefix: "crabbox-", WorkRoot: "/home/coder/crabbox", Wait: "yes"}}, Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.(*coderLeaseBackend).Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("cleanup made mutating calls: %#v", runner.calls)
	}
}

func TestCoderListAndResolveUseStandardCrabboxLabels(t *testing.T) {
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "list -o json":
			return LocalCommandResult{Stdout: `[{"id":"ws1","name":"team-workspace","template_name":"go-dev","labels":{"crabbox":"true","created_by":"crabbox","provider":"coder","lease":"cbx_label","slug":"blue-lobster"},"latest_build":{"status":"stopped"}}]`}, nil
		default:
			t.Fatalf("unexpected command: %s", strings.Join(req.Args, " "))
		}
		return LocalCommandResult{}, nil
	}
	backend := &coderLeaseBackend{spec: Provider{}.Spec(), cfg: Config{Coder: CoderConfig{CLIPath: "coder", WorkspacePrefix: "crabbox-", WorkRoot: "/home/coder/crabbox", Wait: "yes"}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	servers, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || servers[0].Name != "team-workspace" || serverSlug(servers[0]) != "blue-lobster" {
		t.Fatalf("unexpected servers: %#v", servers)
	}
	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: "cbx_label", StatusOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != "cbx_label" || lease.Server.Name != "team-workspace" || serverSlug(lease.Server) != "blue-lobster" {
		t.Fatalf("unexpected lease: %#v", lease)
	}
}

func TestCoderListAndResolveUseLegacyCrabboxLabels(t *testing.T) {
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "list -o json":
			return LocalCommandResult{Stdout: `[{"id":"ws1","name":"team-workspace","template_name":"go-dev","labels":{"crabbox_lease_id":"cbx_legacy","crabbox_slug":"legacy-lobster"},"latest_build":{"status":"stopped"}}]`}, nil
		default:
			t.Fatalf("unexpected command: %s", strings.Join(req.Args, " "))
		}
		return LocalCommandResult{}, nil
	}
	backend := &coderLeaseBackend{spec: Provider{}.Spec(), cfg: Config{Coder: CoderConfig{CLIPath: "coder", WorkspacePrefix: "crabbox-", WorkRoot: "/home/coder/crabbox", Wait: "yes"}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	servers, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || servers[0].Name != "team-workspace" || serverSlug(servers[0]) != "legacy-lobster" {
		t.Fatalf("unexpected servers: %#v", servers)
	}
	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: "cbx_legacy", StatusOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != "cbx_legacy" || lease.Server.Name != "team-workspace" || serverSlug(lease.Server) != "legacy-lobster" {
		t.Fatalf("unexpected lease: %#v", lease)
	}
}

func TestCoderCleanupSkipsProviderLabelWithoutCrabboxOwnership(t *testing.T) {
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "list -o json":
			return LocalCommandResult{Stdout: `[{"id":"ws1","name":"team-workspace","template_name":"go-dev","labels":{"provider":"coder"},"latest_build":{"status":"stopped"}}]`}, nil
		default:
			t.Fatalf("cleanup must not act on provider-only labels, got: %s", strings.Join(req.Args, " "))
		}
		return LocalCommandResult{}, nil
	}
	backend := &coderLeaseBackend{spec: Provider{}.Spec(), cfg: Config{Coder: CoderConfig{CLIPath: "coder", WorkspacePrefix: "crabbox-", WorkRoot: "/home/coder/crabbox", Wait: "yes"}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("cleanup made mutating calls: %#v", runner.calls)
	}
}

func TestCoderCleanupSkipsProviderAndSlugWithoutCrabboxMarker(t *testing.T) {
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "list -o json":
			return LocalCommandResult{Stdout: `[{"id":"ws1","name":"team-workspace","template_name":"go-dev","labels":{"provider":"coder","slug":"blue-lobster"},"latest_build":{"status":"stopped"}}]`}, nil
		default:
			t.Fatalf("cleanup must not act on provider+slug labels without Crabbox markers, got: %s", strings.Join(req.Args, " "))
		}
		return LocalCommandResult{}, nil
	}
	backend := &coderLeaseBackend{spec: Provider{}.Spec(), cfg: Config{Coder: CoderConfig{CLIPath: "coder", WorkspacePrefix: "crabbox-", WorkRoot: "/home/coder/crabbox", Wait: "yes"}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("cleanup made mutating calls: %#v", runner.calls)
	}
}

func TestCoderCleanupSkipsGenericSlugWithoutCrabboxMarker(t *testing.T) {
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "list -o json":
			return LocalCommandResult{Stdout: `[{"id":"ws1","name":"team-workspace","template_name":"go-dev","labels":{"slug":"blue-lobster"},"latest_build":{"status":"stopped"}}]`}, nil
		default:
			t.Fatalf("cleanup must not act on generic slug labels, got: %s", strings.Join(req.Args, " "))
		}
		return LocalCommandResult{}, nil
	}
	backend := &coderLeaseBackend{spec: Provider{}.Spec(), cfg: Config{Coder: CoderConfig{CLIPath: "coder", WorkspacePrefix: "crabbox-", WorkRoot: "/home/coder/crabbox", Wait: "yes"}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("cleanup made mutating calls: %#v", runner.calls)
	}
}

func TestCoderListUsesPrefixOwnershipDespiteUnrelatedProviderLabel(t *testing.T) {
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "list -o json":
			return LocalCommandResult{Stdout: `[{"id":"ws1","name":"crabbox-blue","template_name":"go-dev","labels":{"provider":"terraform"},"latest_build":{"status":"stopped"}}]`}, nil
		default:
			t.Fatalf("unexpected command: %s", strings.Join(req.Args, " "))
		}
		return LocalCommandResult{}, nil
	}
	backend := &coderLeaseBackend{spec: Provider{}.Spec(), cfg: Config{Coder: CoderConfig{CLIPath: "coder", WorkspacePrefix: "crabbox-", WorkRoot: "/home/coder/crabbox", Wait: "yes"}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	servers, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || servers[0].Name != "crabbox-blue" || serverSlug(servers[0]) != "blue" {
		t.Fatalf("unexpected servers: %#v", servers)
	}
}

func TestCoderResolveRejectsEmptyNormalizedSlugMatch(t *testing.T) {
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "list -o json":
			return LocalCommandResult{Stdout: `[{"id":"ws1","name":"team-workspace","template_name":"go-dev","labels":{"crabbox":"true","created_by":"crabbox"},"latest_build":{"status":"stopped"}}]`}, nil
		default:
			t.Fatalf("unexpected command: %s", strings.Join(req.Args, " "))
		}
		return LocalCommandResult{}, nil
	}
	backend := &coderLeaseBackend{spec: Provider{}.Spec(), cfg: Config{Coder: CoderConfig{CLIPath: "coder", WorkspacePrefix: "crabbox-", WorkRoot: "/home/coder/crabbox", Wait: "yes"}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	if _, err := backend.Resolve(context.Background(), ResolveRequest{ID: "!!!", StatusOnly: true}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found, got %v", err)
	}
}

func TestCoderResolveStatusOnlyDoesNotStartOrSSH(t *testing.T) {
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "list -o json":
			return LocalCommandResult{Stdout: `[{"id":"ws1","name":"crabbox-blue","template_name":"go-dev","latest_build":{"status":"stopped"}}]`}, nil
		default:
			t.Fatalf("status-only resolve must not mutate or prepare SSH, got: %s", strings.Join(req.Args, " "))
		}
		return LocalCommandResult{}, nil
	}
	backend := &coderLeaseBackend{spec: Provider{}.Spec(), cfg: Config{Coder: CoderConfig{CLIPath: "coder", WorkspacePrefix: "crabbox-", WorkRoot: "/home/coder/crabbox", Wait: "yes"}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: "blue", StatusOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.Status != "stopped" || lease.SSH.Host != "" {
		t.Fatalf("unexpected lease: %#v", lease)
	}
}

func TestCoderResolveClaimUsesStoredWorkspaceAcrossPrefixChanges(t *testing.T) {
	installCoderClaimState(t)
	if err := claimLeaseForRepoProvider("cbx_prefix", "blue", coderProvider, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	server := Server{Name: "crabbox-blue", Labels: map[string]string{"coder_workspace": "crabbox-blue", "coder_workspace_ref": "crabbox-blue"}}
	if err := updateLeaseClaimEndpoint("cbx_prefix", server, SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "list -o json":
			return LocalCommandResult{Stdout: `[{"id":"ws1","name":"crabbox-blue","template_name":"go-dev","latest_build":{"status":"stopped"}}]`}, nil
		default:
			t.Fatalf("status-only resolve must only list, got: %s", strings.Join(req.Args, " "))
		}
		return LocalCommandResult{}, nil
	}
	backend := &coderLeaseBackend{spec: Provider{}.Spec(), cfg: Config{Coder: CoderConfig{CLIPath: "coder", WorkspacePrefix: "other-", WorkRoot: "/home/coder/crabbox", Wait: "yes"}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: "cbx_prefix", StatusOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.Name != "crabbox-blue" || lease.LeaseID != "cbx_prefix" {
		t.Fatalf("unexpected lease: %#v", lease)
	}
}

func TestCoderOwnerQualifiedResolveAndReleaseUseOwnerWorkspace(t *testing.T) {
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "list --all -o json":
			return LocalCommandResult{Stdout: `[{"id":"ws1","name":"shared","owner_name":"alice","template_name":"go-dev","latest_build":{"status":"running","resources":[{"agents":[{"name":"main","operating_system":"linux","status":"connected","lifecycle_state":"ready"}]}]}}]`}, nil
		case "stop --yes alice/shared":
			return LocalCommandResult{}, nil
		default:
			t.Fatalf("unexpected command: %s", strings.Join(req.Args, " "))
		}
		return LocalCommandResult{}, nil
	}
	backend := &coderLeaseBackend{spec: Provider{}.Spec(), cfg: Config{Coder: CoderConfig{CLIPath: "coder", WorkspacePrefix: "crabbox-", WorkRoot: "/home/coder/crabbox", Wait: "yes"}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: "alice/shared", StatusOnly: true, ReadyProbe: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.SSH.Host != "alice/shared" || lease.Server.Labels["coder_workspace_ref"] != "alice/shared" {
		t.Fatalf("owner-qualified target not preserved: %#v", lease)
	}
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(runner.calls[len(runner.calls)-1].Args, " "); got != "stop --yes alice/shared" {
		t.Fatalf("release command=%q", got)
	}
}

func TestCoderWorkspaceNameRules(t *testing.T) {
	for _, tc := range []struct {
		prefix  string
		slug    string
		leaseID string
		want    string
	}{
		{prefix: "crabbox-", slug: "Blue Workspace", leaseID: "cbx_123456abcdef", want: "crabbox-blue-workspace"},
		{prefix: "cbx", slug: "new", leaseID: "cbx_123456abcdef", want: "cbx-cbx-new"},
		{prefix: "crabbox-", slug: "this-name-is-much-longer-than-coder-allows", leaseID: "cbx_123456abcdef", want: "crabbox-this-name-is-much-longer"},
	} {
		got, err := coderWorkspaceName(tc.prefix, tc.slug, tc.leaseID)
		if err != nil {
			t.Fatalf("coderWorkspaceName(%q,%q): %v", tc.prefix, tc.slug, err)
		}
		if got != tc.want {
			t.Fatalf("coderWorkspaceName(%q,%q)=%q want %q", tc.prefix, tc.slug, got, tc.want)
		}
	}
}

func installCoderClaimState(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("CRABBOX_CONFIG", t.TempDir()+"/missing.yaml")
}
