package coder

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
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

func TestParseCoderWorkspacesTreatsEmptyInventoryAsEmptyList(t *testing.T) {
	for _, out := range []string{"", "No workspaces found!", "  No workspaces found!\n"} {
		workspaces, err := parseCoderWorkspaces(out)
		if err != nil {
			t.Fatalf("parseCoderWorkspaces(%q): %v", out, err)
		}
		if len(workspaces) != 0 {
			t.Fatalf("parseCoderWorkspaces(%q) returned %#v, want empty", out, workspaces)
		}
	}
}

func TestCoderSSHTargetUsesProxyCommand(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	target := coderSSHTarget(Config{Coder: CoderConfig{CLIPath: "/opt/Coder CLI/coder", Wait: "yes"}}, "crabbox-blue", "ws1")
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
	if target.KnownHostsFile == "" || !strings.Contains(target.KnownHostsFile, filepath.Join("crabbox", coderProvider, "known_hosts.d")) {
		t.Fatalf("known_hosts file should be isolated under crabbox config dir: %q", target.KnownHostsFile)
	}
	if info, err := os.Stat(filepath.Dir(target.KnownHostsFile)); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("known_hosts dir not prepared securely: info=%#v err=%v", info, err)
	}
}

func TestCoderSSHTargetUsesValidHostForOwnerQualifiedWorkspace(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	target := coderSSHTarget(Config{Coder: CoderConfig{CLIPath: "coder", Wait: "yes"}}, "alice/shared", "ws1")
	if !regexp.MustCompile(`^coder-alice-shared-[0-9a-f]{6}$`).MatchString(target.Host) {
		t.Fatalf("Host=%q want unique owner-qualified alias", target.Host)
	}
	if !strings.Contains(target.ProxyCommand, "'alice/shared'") {
		t.Fatalf("proxy command %q missing owner-qualified ref", target.ProxyCommand)
	}
}

func TestCoderSSHTargetKeepsOwnerQualifiedHostsUnique(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	alice := coderSSHTarget(Config{Coder: CoderConfig{CLIPath: "coder", Wait: "yes"}}, "alice/shared", "ws1")
	bob := coderSSHTarget(Config{Coder: CoderConfig{CLIPath: "coder", Wait: "yes"}}, "bob/shared", "ws2")
	if alice.Host == bob.Host {
		t.Fatalf("owner-qualified SSH hosts collided: %q", alice.Host)
	}
}

func TestCoderSSHTargetKnownHostsChangesWithWorkspaceID(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	first := coderSSHTarget(Config{Coder: CoderConfig{CLIPath: "coder", Wait: "yes"}}, "crabbox-blue", "ws1")
	second := coderSSHTarget(Config{Coder: CoderConfig{CLIPath: "coder", Wait: "yes"}}, "crabbox-blue", "ws2")
	if first.Host != second.Host {
		t.Fatalf("workspace aliases should stay stable for SSH config reuse: %q vs %q", first.Host, second.Host)
	}
	if first.KnownHostsFile == second.KnownHostsFile {
		t.Fatalf("known_hosts file should change with workspace identity: %q", first.KnownHostsFile)
	}
}

func TestCoderReleaseStopsByDefaultAndDeletesOnlyWhenConfigured(t *testing.T) {
	for _, tc := range []struct {
		name     string
		delete   bool
		labels   map[string]string
		wantArgs string
	}{
		{name: "stop default", wantArgs: "stop --yes crabbox-blue"},
		{name: "delete opt in", delete: true, wantArgs: "delete --yes crabbox-blue"},
		{name: "current delete config overrides persisted stop", delete: true, labels: map[string]string{"coder_release_action": "stop"}, wantArgs: "delete --yes crabbox-blue"},
		{name: "current stop config overrides persisted delete", labels: map[string]string{"coder_release_action": "delete"}, wantArgs: "stop --yes crabbox-blue"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{}
			backend, err := NewCoderLeaseBackend(Provider{}.Spec(), Config{Coder: CoderConfig{CLIPath: "coder", WorkspacePrefix: "crabbox-", WorkRoot: "/home/coder/crabbox", Wait: "yes", DeleteOnRelease: tc.delete}}, Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner})
			if err != nil {
				t.Fatal(err)
			}
			err = backend.(*coderLeaseBackend).ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: "cbx_123", Server: Server{Name: "crabbox-blue", Labels: tc.labels}}})
			if err != nil {
				t.Fatal(err)
			}
			if len(runner.calls) != 1 || strings.Join(runner.calls[0].Args, " ") != tc.wantArgs {
				t.Fatalf("calls=%#v want %s", runner.calls, tc.wantArgs)
			}
		})
	}
}

func TestCoderReleaseRemovesClaimWhenRemoteWorkspaceAlreadyGone(t *testing.T) {
	installCoderClaimState(t)
	writeCoderClaim(t, "cbx_gone", `{
		"leaseID":"cbx_gone",
		"slug":"blue",
		"provider":"coder",
		"repoRoot":"/tmp/repo",
		"claimedAt":"2026-01-01T00:00:00Z",
		"lastUsedAt":"2026-01-01T00:00:00Z",
		"idleTimeoutSeconds":1800,
		"labels":{"coder_workspace_ref":"crabbox-blue","coder_workspace":"crabbox-blue"}
	}`)
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "list -o json":
			return LocalCommandResult{Stdout: `[]`}, nil
		case "stop --yes crabbox-blue":
			return LocalCommandResult{ExitCode: 1, Stderr: "workspace not found"}, errors.New("workspace not found")
		default:
			t.Fatalf("unexpected command: %s", strings.Join(req.Args, " "))
		}
		return LocalCommandResult{}, nil
	}
	backend, err := NewCoderLeaseBackend(Provider{}.Spec(), Config{Coder: CoderConfig{CLIPath: "coder", WorkspacePrefix: "crabbox-", WorkRoot: "/home/coder/crabbox", Wait: "yes"}}, Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := backend.(*coderLeaseBackend).Resolve(context.Background(), ResolveRequest{ID: "cbx_gone", ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.Status != "missing" || lease.Server.Labels["coder_workspace_ref"] != "crabbox-blue" {
		t.Fatalf("stale claim target not preserved: %#v", lease)
	}
	if err := backend.(*coderLeaseBackend).ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	claims, err := listLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 0 {
		t.Fatalf("stale claim was not removed: %#v", claims)
	}
}

func TestCoderAcquireRollbackUsesStopByDefaultAndSkipsCreateFailureRollback(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		createErr               error
		createErrWorkspaceFound bool
		deleteOnRelease         bool
		wantErr                 string
		wantRollback            bool
		wantAction              string
		wantListCalls           int
	}{
		{name: "inventory miss stops created workspace by default", wantErr: "created but not found", wantRollback: true, wantAction: "stop --yes", wantListCalls: 2},
		{name: "inventory miss deletes created workspace when configured", deleteOnRelease: true, wantErr: "created but not found", wantRollback: true, wantAction: "delete --yes", wantListCalls: 2},
		{name: "create failure without workspace removes claim only", createErr: errors.New("build failed"), wantErr: "build failed", wantListCalls: 2},
		{name: "create failure with workspace stops created workspace by default", createErr: errors.New("build failed"), createErrWorkspaceFound: true, wantErr: "build failed", wantRollback: true, wantAction: "stop --yes", wantListCalls: 2},
		{name: "create failure with workspace deletes created workspace when configured", createErr: errors.New("build failed"), createErrWorkspaceFound: true, deleteOnRelease: true, wantErr: "build failed", wantRollback: true, wantAction: "delete --yes", wantListCalls: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installCoderClaimState(t)
			runner := &fakeRunner{}
			listCalls := 0
			createdName := ""
			runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
				command := strings.Join(req.Args, " ")
				switch {
				case command == "list -o json":
					listCalls++
					if tc.createErr != nil && listCalls == 2 && tc.createErrWorkspaceFound {
						return LocalCommandResult{Stdout: fmt.Sprintf(`[{"id":"ws1","name":%q,"template_name":"go-dev","latest_build":{"status":"stopped"}}]`, createdName)}, nil
					}
					return LocalCommandResult{Stdout: `[]`}, nil
				case strings.HasPrefix(command, "create --yes --template go-dev crabbox-blue"):
					createdName = req.Args[len(req.Args)-1]
					if tc.createErr != nil {
						return LocalCommandResult{ExitCode: 1, Stderr: tc.createErr.Error()}, tc.createErr
					}
					return LocalCommandResult{}, nil
				case strings.HasPrefix(command, "stop --yes crabbox-blue"):
					return LocalCommandResult{}, nil
				case strings.HasPrefix(command, "delete --yes crabbox-blue"):
					return LocalCommandResult{}, nil
				default:
					t.Fatalf("unexpected command: %s", command)
				}
				return LocalCommandResult{}, nil
			}
			backend, err := NewCoderLeaseBackend(Provider{}.Spec(), Config{IdleTimeout: time.Hour, Coder: CoderConfig{CLIPath: "coder", Template: "go-dev", WorkspacePrefix: "crabbox-", WorkRoot: "/home/coder/crabbox", Wait: "yes", DeleteOnRelease: tc.deleteOnRelease}}, Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner})
			if err != nil {
				t.Fatal(err)
			}
			_, err = backend.(*coderLeaseBackend).Acquire(context.Background(), AcquireRequest{RequestedSlug: "blue", Repo: Repo{Root: t.TempDir()}})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected %q error, got %v", tc.wantErr, err)
			}
			if listCalls != tc.wantListCalls {
				t.Fatalf("list calls=%d want %d", listCalls, tc.wantListCalls)
			}
			if !tc.wantRollback {
				for _, call := range runner.calls {
					command := strings.Join(call.Args, " ")
					if strings.HasPrefix(command, "delete --yes") || strings.HasPrefix(command, "stop --yes") {
						t.Fatalf("unexpected rollback call: %#v", runner.calls)
					}
				}
				return
			}
			wantAction := tc.wantAction + " " + createdName
			if got := strings.Join(runner.calls[len(runner.calls)-1].Args, " "); got != wantAction {
				t.Fatalf("final rollback command=%q want %q", got, wantAction)
			}
		})
	}
}

func TestCoderAcquireKeepFailurePersistsWorkspaceRefBeforeReady(t *testing.T) {
	installCoderClaimState(t)
	runner := &fakeRunner{}
	listCalls := 0
	createdName := ""
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		command := strings.Join(req.Args, " ")
		switch {
		case command == "list -o json":
			listCalls++
			if listCalls == 1 {
				return LocalCommandResult{Stdout: `[{"id":"existing","name":"crabbox-blue","template_name":"go-dev","latest_build":{"status":"stopped"}}]`}, nil
			}
			return LocalCommandResult{ExitCode: 1, Stderr: "inventory unavailable"}, errors.New("inventory unavailable")
		case strings.HasPrefix(command, "create --yes --template go-dev crabbox-blue-"):
			createdName = req.Args[len(req.Args)-1]
			return LocalCommandResult{}, nil
		default:
			t.Fatalf("unexpected command: %s", command)
		}
		return LocalCommandResult{}, nil
	}
	backend, err := NewCoderLeaseBackend(Provider{}.Spec(), Config{IdleTimeout: time.Hour, Coder: CoderConfig{CLIPath: "coder", Template: "go-dev", WorkspacePrefix: "crabbox-", WorkRoot: "/home/coder/crabbox", Wait: "yes"}}, Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner})
	if err != nil {
		t.Fatal(err)
	}
	_, err = backend.(*coderLeaseBackend).Acquire(context.Background(), AcquireRequest{RequestedSlug: "blue", Keep: true, Repo: Repo{Root: t.TempDir()}})
	if err == nil || !strings.Contains(err.Error(), "inventory unavailable") {
		t.Fatalf("expected inventory error, got %v", err)
	}
	if createdName == "" {
		t.Fatal("create was not called")
	}
	claims, err := listLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 {
		t.Fatalf("claims=%#v want one kept failed claim", claims)
	}
	if claims[0].Labels["coder_workspace_ref"] != createdName || claims[0].Labels["coder_workspace"] != createdName {
		t.Fatalf("workspace ref not persisted before readiness failure: claim=%#v created=%q", claims[0], createdName)
	}
}

func TestCoderAcquireRollbackFailureHintMatchesReleasePolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		delete bool
	}{
		{name: "stop default"},
		{name: "delete configured", delete: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installCoderClaimState(t)
			runner := &fakeRunner{}
			listCalls := 0
			createdName := ""
			runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
				command := strings.Join(req.Args, " ")
				switch {
				case command == "list -o json":
					listCalls++
					if listCalls == 1 {
						return LocalCommandResult{Stdout: `[]`}, nil
					}
					return LocalCommandResult{Stdout: `[]`}, nil
				case strings.HasPrefix(command, "create --yes --template go-dev crabbox-blue"):
					createdName = req.Args[len(req.Args)-1]
					return LocalCommandResult{}, nil
				case strings.HasPrefix(command, "stop --yes crabbox-blue"):
					return LocalCommandResult{ExitCode: 1, Stderr: "release failed"}, errors.New("release failed")
				case strings.HasPrefix(command, "delete --yes crabbox-blue"):
					return LocalCommandResult{ExitCode: 1, Stderr: "release failed"}, errors.New("release failed")
				default:
					t.Fatalf("unexpected command: %s", command)
				}
				return LocalCommandResult{}, nil
			}
			backend, err := NewCoderLeaseBackend(Provider{}.Spec(), Config{IdleTimeout: time.Hour, Coder: CoderConfig{CLIPath: "coder", Template: "go-dev", WorkspacePrefix: "crabbox-", WorkRoot: "/home/coder/crabbox", Wait: "yes", DeleteOnRelease: tc.delete}}, Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner})
			if err != nil {
				t.Fatal(err)
			}
			_, err = backend.(*coderLeaseBackend).Acquire(context.Background(), AcquireRequest{RequestedSlug: "blue", Repo: Repo{Root: t.TempDir()}})
			want := "manual cleanup: crabbox stop --provider coder --id " + createdName
			if tc.delete {
				want = "manual cleanup: crabbox stop --provider coder --coder-delete-on-release --id " + createdName
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("rollback hint missing %q: %v", want, err)
			}
		})
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

func TestCoderDoctorDoesNotClassifyServerFailureAsMissingLogin(t *testing.T) {
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "version":
			return LocalCommandResult{Stdout: "Coder v2.33.5"}, nil
		case "whoami -o json":
			return LocalCommandResult{ExitCode: 1, Stderr: "dial tcp: connection refused"}, errors.New("exit 1")
		default:
			t.Fatalf("doctor must be non-mutating, got: %s", strings.Join(req.Args, " "))
		}
		return LocalCommandResult{}, nil
	}
	backend := &coderLeaseBackend{spec: Provider{}.Spec(), cfg: Config{Coder: CoderConfig{CLIPath: "coder"}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	result, err := backend.Doctor(context.Background(), DoctorRequest{})
	if err == nil {
		t.Fatal("expected auth failure")
	}
	if !strings.Contains(result.Message, "auth=failed") || strings.Contains(result.Message, "auth=missing_login") {
		t.Fatalf("unexpected doctor result: %#v", result)
	}
	if got := result.Checks[1].Details["classification"]; got != "auth_failed" {
		t.Fatalf("classification=%q want auth_failed; checks=%#v", got, result.Checks)
	}
}

func TestCoderDoctorPreservesChecksOnInventoryFailure(t *testing.T) {
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "version":
			return LocalCommandResult{Stdout: "Coder v2.33.5"}, nil
		case "whoami -o json":
			return LocalCommandResult{Stdout: `{"username":"alice"}`}, nil
		case "list -o json":
			return LocalCommandResult{ExitCode: 1, Stderr: "inventory unavailable"}, errors.New("inventory unavailable")
		default:
			t.Fatalf("doctor must only read inventory, got: %s", strings.Join(req.Args, " "))
		}
		return LocalCommandResult{}, nil
	}
	backend := &coderLeaseBackend{spec: Provider{}.Spec(), cfg: Config{Coder: CoderConfig{CLIPath: "coder", WorkspacePrefix: "crabbox-", WorkRoot: "/home/coder/crabbox", Wait: "yes"}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	result, err := backend.Doctor(context.Background(), DoctorRequest{})
	if err == nil || !strings.Contains(err.Error(), "inventory unavailable") {
		t.Fatalf("expected inventory error, got %v", err)
	}
	if len(result.Checks) != 3 || result.Checks[2].Check != "inventory" || result.Checks[2].Status != "fail" {
		t.Fatalf("unexpected doctor checks: %#v", result.Checks)
	}
}

func TestCoderListIncludesButCleanupSkipsUnclaimedStoppedWorkspaces(t *testing.T) {
	installCoderClaimState(t)
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "list -o json":
			return LocalCommandResult{Stdout: `[
				{"id":"ws1","name":"crabbox-blue","template_name":"go-dev","latest_build":{"status":"stopped"}},
					{"id":"ws2","name":"personal","template_name":"go-dev","latest_build":{"status":"running"}}
				]`}, nil
		default:
			t.Fatalf("cleanup must not mutate unclaimed Coder workspaces, got: %s", strings.Join(req.Args, " "))
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
	if len(runner.calls) != 2 {
		t.Fatalf("expected list and cleanup inventory calls only, calls=%#v", runner.calls)
	}
	if servers[0].Labels["work_root"] != "/home/coder/crabbox" {
		t.Fatalf("work_root label=%q", servers[0].Labels["work_root"])
	}
}

func TestShouldCleanupCoderRequiresLocalClaimForStoppedWorkspace(t *testing.T) {
	server := Server{Name: "crabbox-blue", Status: "stopped", Labels: map[string]string{"slug": "blue"}}
	ok, reason := shouldCleanupCoder(server, LeaseClaim{}, false, time.Now())
	if ok || reason != "missing claim" {
		t.Fatalf("cleanup=%v reason=%q; stopped unclaimed Coder workspaces must be preserved", ok, reason)
	}
	expired := LeaseClaim{LeaseID: "cbx_expired", LastUsedAt: time.Now().Add(-48 * time.Hour).Format(time.RFC3339), IdleTimeoutSeconds: int((30 * time.Minute).Seconds())}
	ok, reason = shouldCleanupCoder(server, expired, true, time.Now())
	if !ok || reason != "claim expired" {
		t.Fatalf("cleanup=%v reason=%q; expired local claim should be cleanup-eligible", ok, reason)
	}
}

func TestCoderCleanupSkipsKeptClaims(t *testing.T) {
	installCoderClaimState(t)
	if err := claimLeaseForRepoProvider("cbx_keep", "blue", coderProvider, t.TempDir(), time.Hour, true); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "list -o json":
			return LocalCommandResult{Stdout: `[{"id":"ws1","name":"crabbox-blue","template_name":"go-dev","latest_build":{"status":"stopped"}}]`}, nil
		default:
			t.Fatalf("kept cleanup should not mutate, got: %s", strings.Join(req.Args, " "))
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
		t.Fatalf("expected cleanup to stop after inventory for keep=true claim, calls=%#v", runner.calls)
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

func TestCoderCleanupSkipsStoppedActiveClaim(t *testing.T) {
	installCoderClaimState(t)
	if err := claimLeaseForRepoProvider("cbx_stopped", "stopped", coderProvider, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "list -o json":
			return LocalCommandResult{Stdout: `[
				{"id":"ws1","name":"crabbox-stopped","template_name":"go-dev","latest_build":{"status":"stopped"}}
			]`}, nil
		default:
			t.Fatalf("cleanup must not act on a stopped active claim, got: %s", strings.Join(req.Args, " "))
		}
		return LocalCommandResult{}, nil
	}
	backend, err := NewCoderLeaseBackend(Provider{}.Spec(), Config{Coder: CoderConfig{CLIPath: "coder", WorkspacePrefix: "crabbox-", WorkRoot: "/home/coder/crabbox", Wait: "yes", DeleteOnRelease: true}}, Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner})
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

func TestCoderCleanupUsesListAllForExpiredOwnerQualifiedClaim(t *testing.T) {
	installCoderClaimState(t)
	writeCoderClaim(t, "cbx_owner_expired", `{
		"leaseID":"cbx_owner_expired",
		"slug":"shared",
		"provider":"coder",
		"repoRoot":"/tmp/repo",
		"claimedAt":"2026-01-01T00:00:00Z",
		"lastUsedAt":"2026-01-01T00:00:00Z",
		"idleTimeoutSeconds":1800,
		"labels":{"coder_workspace_ref":"alice/shared"}
	}`)
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "list --all -o json":
			return LocalCommandResult{Stdout: `[{"id":"ws1","name":"shared","owner_name":"alice","template_name":"go-dev","latest_build":{"status":"stopped"}}]`}, nil
		case "stop --yes alice/shared":
			return LocalCommandResult{}, nil
		default:
			t.Fatalf("cleanup must use owner-qualified inventory/ref, got: %s", strings.Join(req.Args, " "))
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
	if len(runner.calls) != 2 {
		t.Fatalf("calls=%#v want list --all then stop", runner.calls)
	}
	if got := strings.Join(runner.calls[1].Args, " "); got != "stop --yes alice/shared" {
		t.Fatalf("cleanup final call=%q", got)
	}
}

func TestCoderCleanupUsesPersistedReleasePolicy(t *testing.T) {
	for _, tc := range []struct {
		name         string
		claimLabels  string
		configDelete bool
		wantAction   string
	}{
		{
			name:         "old claims default to stop despite delete config",
			claimLabels:  `"labels":{"coder_workspace":"crabbox-blue"}`,
			configDelete: true,
			wantAction:   "stop --yes crabbox-blue",
		},
		{
			name:        "delete claim persists delete action",
			claimLabels: `"labels":{"coder_workspace":"crabbox-blue","coder_release_action":"delete"}`,
			wantAction:  "delete --yes crabbox-blue",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installCoderClaimState(t)
			writeCoderClaim(t, "cbx_expired", `{
				"leaseID":"cbx_expired",
				"slug":"blue",
				"provider":"coder",
				"repoRoot":"/tmp/repo",
				"claimedAt":"2026-01-01T00:00:00Z",
				"lastUsedAt":"2026-01-01T00:00:00Z",
				"idleTimeoutSeconds":1800,
				`+tc.claimLabels+`
			}`)
			runner := &fakeRunner{}
			runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
				switch strings.Join(req.Args, " ") {
				case "list -o json":
					return LocalCommandResult{Stdout: `[{"id":"ws1","name":"crabbox-blue","template_name":"go-dev","latest_build":{"status":"stopped"}}]`}, nil
				case "stop --yes crabbox-blue", "delete --yes crabbox-blue":
					return LocalCommandResult{}, nil
				default:
					t.Fatalf("unexpected cleanup command: %s", strings.Join(req.Args, " "))
				}
				return LocalCommandResult{}, nil
			}
			backend, err := NewCoderLeaseBackend(Provider{}.Spec(), Config{Coder: CoderConfig{CLIPath: "coder", WorkspacePrefix: "crabbox-", WorkRoot: "/home/coder/crabbox", Wait: "yes", DeleteOnRelease: tc.configDelete}}, Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner})
			if err != nil {
				t.Fatal(err)
			}
			if err := backend.(*coderLeaseBackend).Cleanup(context.Background(), CleanupRequest{}); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(runner.calls[len(runner.calls)-1].Args, " "); got != tc.wantAction {
				t.Fatalf("cleanup action=%q want %q", got, tc.wantAction)
			}
		})
	}
}

func TestCoderCleanupDoesNotApplyBareClaimToOwnerQualifiedInventory(t *testing.T) {
	installCoderClaimState(t)
	writeCoderClaim(t, "cbx_owner_expired", `{
		"leaseID":"cbx_owner_expired",
		"slug":"shared",
		"provider":"coder",
		"repoRoot":"/tmp/repo",
		"claimedAt":"2026-01-01T00:00:00Z",
		"lastUsedAt":"2026-01-01T00:00:00Z",
		"idleTimeoutSeconds":1800,
		"labels":{"coder_workspace_ref":"alice/shared"}
	}`)
	writeCoderClaim(t, "cbx_bare_expired", `{
		"leaseID":"cbx_bare_expired",
		"slug":"shared",
		"provider":"coder",
		"repoRoot":"/tmp/repo",
		"claimedAt":"2026-01-01T00:00:00Z",
		"lastUsedAt":"2026-01-01T00:00:00Z",
		"idleTimeoutSeconds":1800,
		"labels":{"coder_workspace":"shared"}
	}`)
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "list --all -o json":
			return LocalCommandResult{Stdout: `[
				{"id":"ws1","name":"shared","owner_name":"alice","template_name":"go-dev","latest_build":{"status":"stopped"}},
				{"id":"ws2","name":"shared","owner_name":"bob","template_name":"go-dev","latest_build":{"status":"stopped"}}
			]`}, nil
		case "stop --yes alice/shared":
			return LocalCommandResult{}, nil
		default:
			t.Fatalf("cleanup must not apply bare claim to owner-qualified row, got: %s", strings.Join(req.Args, " "))
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
	if len(runner.calls) != 2 {
		t.Fatalf("calls=%#v want list --all and only alice stop", runner.calls)
	}
}

func TestCoderCleanupAppliesBareClaimToUniqueOwnerQualifiedInventory(t *testing.T) {
	installCoderClaimState(t)
	writeCoderClaim(t, "cbx_other_kept", `{
		"leaseID":"cbx_other_kept",
		"slug":"other",
		"provider":"coder",
		"repoRoot":"/tmp/repo",
		"claimedAt":"2026-01-01T00:00:00Z",
		"lastUsedAt":"2026-01-01T00:00:00Z",
		"idleTimeoutSeconds":1800,
		"labels":{"coder_workspace_ref":"alice/other","keep":"true"}
	}`)
	writeCoderClaim(t, "cbx_bare_expired", `{
		"leaseID":"cbx_bare_expired",
		"slug":"shared",
		"provider":"coder",
		"repoRoot":"/tmp/repo",
		"claimedAt":"2026-01-01T00:00:00Z",
		"lastUsedAt":"2026-01-01T00:00:00Z",
		"idleTimeoutSeconds":1800,
		"labels":{"coder_workspace":"shared"}
	}`)
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "list --all -o json":
			return LocalCommandResult{Stdout: `[
				{"id":"ws1","name":"shared","owner_name":"alice","template_name":"go-dev","latest_build":{"status":"stopped"}},
				{"id":"ws2","name":"other","owner_name":"alice","template_name":"go-dev","latest_build":{"status":"stopped"}}
			]`}, nil
		case "stop --yes alice/shared":
			return LocalCommandResult{}, nil
		default:
			t.Fatalf("unique owner-qualified row should accept bare claim, got: %s", strings.Join(req.Args, " "))
		}
		return LocalCommandResult{}, nil
	}
	backend, err := NewCoderLeaseBackend(Provider{}.Spec(), Config{Coder: CoderConfig{CLIPath: "coder", WorkspacePrefix: "crabbox-", WorkRoot: "/home/coder/crabbox", Wait: "yes"}}, Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := listCoderClaimsByWorkspace(Config{Coder: CoderConfig{WorkspacePrefix: "crabbox-"}})
	if err != nil {
		t.Fatal(err)
	}
	if !coderClaimsNeedListAll(claims) {
		t.Fatal("owner-qualified claim should force cleanup list --all")
	}
	if err := backend.(*coderLeaseBackend).Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("calls=%#v want list --all and only shared stop", runner.calls)
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

func TestCoderResolveAdoptedWorkspaceSynthesizesStableLeaseID(t *testing.T) {
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "list -o json":
			return LocalCommandResult{Stdout: `[{"id":"ws1","name":"crabbox-blue","template_name":"go-dev","latest_build":{"status":"stopped"}}]`}, nil
		default:
			t.Fatalf("unexpected command: %s", strings.Join(req.Args, " "))
		}
		return LocalCommandResult{}, nil
	}
	backend := &coderLeaseBackend{spec: Provider{}.Spec(), cfg: Config{Coder: CoderConfig{CLIPath: "coder", WorkspacePrefix: "crabbox-", WorkRoot: "/home/coder/crabbox", Wait: "yes"}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	first, err := backend.Resolve(context.Background(), ResolveRequest{ID: "blue", StatusOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	second, err := backend.Resolve(context.Background(), ResolveRequest{ID: "crabbox-blue", StatusOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^cbx_[a-f0-9]{12}$`).MatchString(first.LeaseID) || first.LeaseID != second.LeaseID {
		t.Fatalf("adopted workspace lease IDs must be stable canonical IDs, first=%q second=%q", first.LeaseID, second.LeaseID)
	}
	if serverSlug(first.Server) != "blue" {
		t.Fatalf("adopted workspace slug=%q want blue", serverSlug(first.Server))
	}
}

func TestCoderResolveCrabboxMarkerWorkspaceSynthesizesLeaseID(t *testing.T) {
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
	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: "team-workspace", StatusOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^cbx_[a-f0-9]{12}$`).MatchString(lease.LeaseID) {
		t.Fatalf("marker-owned workspace lease ID=%q want stable canonical ID", lease.LeaseID)
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
	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: "blue", StatusOnly: true, ReadyProbe: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.Status != "stopped" || lease.SSH.Host != "" {
		t.Fatalf("unexpected lease: %#v", lease)
	}
}

func TestCoderResolveStatusOnlyIncludesSSHForReadyWorkspace(t *testing.T) {
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "list -o json":
			return LocalCommandResult{Stdout: `[{"id":"ws1","name":"crabbox-blue","template_name":"go-dev","latest_build":{"status":"running","resources":[{"agents":[{"name":"main","operating_system":"linux","status":"connected","lifecycle_state":"ready"}]}]}}]`}, nil
		default:
			t.Fatalf("status-only ready resolve must only list, got: %s", strings.Join(req.Args, " "))
		}
		return LocalCommandResult{}, nil
	}
	backend := &coderLeaseBackend{spec: Provider{}.Spec(), cfg: Config{Coder: CoderConfig{CLIPath: "coder", WorkspacePrefix: "crabbox-", WorkRoot: "/home/coder/crabbox", Wait: "yes"}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: "blue", StatusOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.SSH.Host != "crabbox-blue" || !lease.SSH.SSHConfigProxy {
		t.Fatalf("expected status-only ready lease to include SSH target, got %#v", lease)
	}
}

func TestCoderResolveRunningWorkspaceWithoutReadyAgentDoesNotPrepareSSH(t *testing.T) {
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "list -o json":
			return LocalCommandResult{Stdout: `[{"id":"ws1","name":"crabbox-blue","template_name":"go-dev","latest_build":{"status":"running","resources":[{"agents":[{"name":"main","operating_system":"linux","status":"connecting","lifecycle_state":"starting"}]}]}}]`}, nil
		default:
			t.Fatalf("status-only ready probe must only list, got: %s", strings.Join(req.Args, " "))
		}
		return LocalCommandResult{}, nil
	}
	backend := &coderLeaseBackend{spec: Provider{}.Spec(), cfg: Config{Coder: CoderConfig{CLIPath: "coder", WorkspacePrefix: "crabbox-", WorkRoot: "/home/coder/crabbox", Wait: "yes"}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: "blue", StatusOnly: true, ReadyProbe: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.Status != "running" || lease.Server.Labels["state"] != "running" {
		t.Fatalf("running workspace should not be reported ready: %#v", lease)
	}
	if lease.SSH.Host != "" || lease.SSH.ProxyCommand != "" {
		t.Fatalf("running workspace without ready agent should not prepare SSH: %#v", lease)
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
	lease, err = backend.Resolve(context.Background(), ResolveRequest{ID: "crabbox-blue", StatusOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.Name != "crabbox-blue" || lease.LeaseID != "cbx_prefix" {
		t.Fatalf("workspace-name resolve ignored stored claim: %#v", lease)
	}
}

func TestCoderResolveDoesNotListAllForUnrelatedOwnerQualifiedClaim(t *testing.T) {
	installCoderClaimState(t)
	if err := claimLeaseForRepoProvider("cbx_owner", "shared", coderProvider, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	server := Server{Name: "shared", Labels: map[string]string{"coder_workspace_ref": "alice/shared"}}
	if err := updateLeaseClaimEndpoint("cbx_owner", server, SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "list -o json":
			return LocalCommandResult{Stdout: `[{"id":"ws1","name":"crabbox-blue","template_name":"go-dev","latest_build":{"status":"stopped"}}]`}, nil
		default:
			t.Fatalf("unrelated owner-qualified claim must not force list --all, got: %s", strings.Join(req.Args, " "))
		}
		return LocalCommandResult{}, nil
	}
	backend := &coderLeaseBackend{spec: Provider{}.Spec(), cfg: Config{Coder: CoderConfig{CLIPath: "coder", WorkspacePrefix: "crabbox-", WorkRoot: "/home/coder/crabbox", Wait: "yes"}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: "blue", StatusOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.Name != "crabbox-blue" {
		t.Fatalf("unexpected lease: %#v", lease)
	}
}

func TestCoderResolveBareWorkspaceNameUsesListAllForMatchingOwnerQualifiedClaim(t *testing.T) {
	installCoderClaimState(t)
	if err := claimLeaseForRepoProvider("cbx_owner", "shared", coderProvider, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	server := Server{Name: "shared", Labels: map[string]string{"coder_workspace_ref": "alice/shared"}}
	if err := updateLeaseClaimEndpoint("cbx_owner", server, SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "list --all -o json":
			return LocalCommandResult{Stdout: `[{"id":"ws1","name":"shared","owner_name":"alice","template_name":"go-dev","latest_build":{"status":"stopped"}}]`}, nil
		default:
			t.Fatalf("matching owner-qualified claim should force list --all, got: %s", strings.Join(req.Args, " "))
		}
		return LocalCommandResult{}, nil
	}
	backend := &coderLeaseBackend{spec: Provider{}.Spec(), cfg: Config{Coder: CoderConfig{CLIPath: "coder", WorkspacePrefix: "crabbox-", WorkRoot: "/home/coder/crabbox", Wait: "yes"}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: "shared", StatusOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != "cbx_owner" || lease.Server.CloudID != "alice/shared" {
		t.Fatalf("unexpected lease: %#v", lease)
	}
}

func TestCoderListUsesListAllForOwnerQualifiedClaims(t *testing.T) {
	installCoderClaimState(t)
	if err := claimLeaseForRepoProvider("cbx_owner", "shared", coderProvider, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	server := Server{Name: "shared", Labels: map[string]string{"coder_workspace_ref": "alice/shared"}}
	if err := updateLeaseClaimEndpoint("cbx_owner", server, SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "list --all -o json":
			return LocalCommandResult{Stdout: `[{"id":"ws1","name":"shared","owner_name":"alice","template_name":"go-dev","latest_build":{"status":"stopped"}}]`}, nil
		default:
			t.Fatalf("owner-qualified claim should make list use list --all, got: %s", strings.Join(req.Args, " "))
		}
		return LocalCommandResult{}, nil
	}
	backend := &coderLeaseBackend{spec: Provider{}.Spec(), cfg: Config{Coder: CoderConfig{CLIPath: "coder", WorkspacePrefix: "crabbox-", WorkRoot: "/home/coder/crabbox", Wait: "yes"}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	servers, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || servers[0].CloudID != "alice/shared" || servers[0].Labels["lease"] != "cbx_owner" {
		t.Fatalf("unexpected servers: %#v", servers)
	}
}

func TestCoderResolvePreservesClaimTimingLabels(t *testing.T) {
	installCoderClaimState(t)
	writeCoderClaim(t, "cbx_timing", `{
		"leaseID":"cbx_timing",
		"slug":"blue",
		"provider":"coder",
		"repoRoot":"/tmp/repo",
		"claimedAt":"2026-01-01T00:00:00Z",
		"lastUsedAt":"2026-01-01T00:00:00Z",
		"idleTimeoutSeconds":1800,
		"labels":{
			"coder_workspace":"crabbox-blue",
			"created_at":"1767225600",
			"last_touched_at":"1767225600",
			"idle_timeout":"1800",
			"idle_timeout_secs":"1800",
			"expires_at":"1767227400"
		}
	}`)
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "list -o json":
			return LocalCommandResult{Stdout: `[{"id":"ws1","name":"crabbox-blue","template_name":"go-dev","latest_build":{"status":"stopped"}}]`}, nil
		default:
			t.Fatalf("unexpected command: %s", strings.Join(req.Args, " "))
		}
		return LocalCommandResult{}, nil
	}
	backend := &coderLeaseBackend{spec: Provider{}.Spec(), cfg: Config{Coder: CoderConfig{CLIPath: "coder", WorkspacePrefix: "crabbox-", WorkRoot: "/home/coder/crabbox", Wait: "yes"}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: "crabbox-blue", StatusOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"lease":             "cbx_timing",
		"slug":              "blue",
		"created_at":        "1767225600",
		"last_touched_at":   "1767225600",
		"idle_timeout":      "1800",
		"idle_timeout_secs": "1800",
		"expires_at":        "1767227400",
	} {
		if got := lease.Server.Labels[key]; got != want {
			t.Fatalf("label %s=%q want %q; labels=%#v", key, got, want, lease.Server.Labels)
		}
	}
	if lease.Server.Labels["state"] != "stopped" {
		t.Fatalf("state label should still reflect current workspace state, labels=%#v", lease.Server.Labels)
	}
}

func TestCoderResolvePreservesRemoteWorkspaceLabels(t *testing.T) {
	installCoderClaimState(t)
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "list -o json":
			return LocalCommandResult{Stdout: `[{"id":"ws1","name":"crabbox-blue","template_name":"go-dev","labels":{"crabbox":"true","created_by":"crabbox","lease":"cbx_abcdef123456","slug":"blue","keep":"true","created_at":"1767225600","last_touched_at":"1767225601","idle_timeout":"1800","idle_timeout_secs":"1800","expires_at":"1767227400"},"latest_build":{"status":"stopped"}}]`}, nil
		default:
			t.Fatalf("unexpected command: %s", strings.Join(req.Args, " "))
		}
		return LocalCommandResult{}, nil
	}
	backend := &coderLeaseBackend{spec: Provider{}.Spec(), cfg: Config{Coder: CoderConfig{CLIPath: "coder", WorkspacePrefix: "crabbox-", WorkRoot: "/home/coder/crabbox", Wait: "yes"}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: "crabbox-blue", StatusOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"lease":             "cbx_abcdef123456",
		"slug":              "blue",
		"keep":              "true",
		"created_at":        "1767225600",
		"last_touched_at":   "1767225601",
		"idle_timeout":      "1800",
		"idle_timeout_secs": "1800",
		"expires_at":        "1767227400",
	} {
		if got := lease.Server.Labels[key]; got != want {
			t.Fatalf("label %s=%q want %q; labels=%#v", key, got, want, lease.Server.Labels)
		}
	}
}

func TestCoderResolveKeepLabelUsesClaimKeepMetadata(t *testing.T) {
	installCoderClaimState(t)
	if err := claimLeaseForRepoProvider("cbx_keepflag", "blue", coderProvider, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	if err := claimLeaseForRepoProvider("cbx_keeptrue", "green", coderProvider, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	server := Server{Name: "crabbox-green", Labels: map[string]string{"keep": "true"}}
	if err := updateLeaseClaimEndpoint("cbx_keeptrue", server, SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	backend := &coderLeaseBackend{spec: Provider{}.Spec(), cfg: Config{Coder: CoderConfig{CLIPath: "coder", WorkspacePrefix: "crabbox-", WorkRoot: "/home/coder/crabbox", Wait: "yes"}}}
	if keep, err := backend.resolveKeepLabel("cbx_keepflag"); err != nil || keep {
		t.Fatalf("ordinary resolveKeepLabel keep=%v err=%v", keep, err)
	}
	if keep, err := backend.resolveKeepLabel("cbx_keeptrue"); err != nil || !keep {
		t.Fatalf("kept resolveKeepLabel keep=%v err=%v", keep, err)
	}
}

func TestCoderResolveClaimUsesListAllForOwnerQualifiedWorkspaceRef(t *testing.T) {
	installCoderClaimState(t)
	if err := claimLeaseForRepoProvider("cbx_owner", "shared", coderProvider, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	server := Server{Name: "shared", Labels: map[string]string{"coder_workspace_ref": "alice/shared"}}
	if err := updateLeaseClaimEndpoint("cbx_owner", server, SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "list --all -o json":
			return LocalCommandResult{Stdout: `[{"id":"ws1","name":"shared","owner_name":"alice","template_name":"go-dev","latest_build":{"status":"stopped"}}]`}, nil
		default:
			t.Fatalf("expected owner-qualified claim resolve to use list --all, got: %s", strings.Join(req.Args, " "))
		}
		return LocalCommandResult{}, nil
	}
	backend := &coderLeaseBackend{spec: Provider{}.Spec(), cfg: Config{Coder: CoderConfig{CLIPath: "coder", WorkspacePrefix: "crabbox-", WorkRoot: "/home/coder/crabbox", Wait: "yes"}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: "cbx_owner", StatusOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != "cbx_owner" || lease.Server.Labels["coder_workspace_ref"] != "alice/shared" {
		t.Fatalf("unexpected lease: %#v", lease)
	}
}

func TestCoderResolveSlugClaimDisambiguatesOwnerQualifiedWorkspaces(t *testing.T) {
	installCoderClaimState(t)
	if err := claimLeaseForRepoProvider("cbx_owner", "shared", coderProvider, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	server := Server{Name: "shared", Labels: map[string]string{"coder_workspace_ref": "alice/shared"}}
	if err := updateLeaseClaimEndpoint("cbx_owner", server, SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "list --all -o json":
			return LocalCommandResult{Stdout: `[
				{"id":"ws1","name":"shared","owner_name":"bob","template_name":"go-dev","latest_build":{"status":"stopped"}},
				{"id":"ws2","name":"shared","owner_name":"alice","template_name":"go-dev","latest_build":{"status":"stopped"}}
			]`}, nil
		default:
			t.Fatalf("expected owner-qualified claim resolve to use list --all, got: %s", strings.Join(req.Args, " "))
		}
		return LocalCommandResult{}, nil
	}
	backend := &coderLeaseBackend{spec: Provider{}.Spec(), cfg: Config{Coder: CoderConfig{CLIPath: "coder", WorkspacePrefix: "crabbox-", WorkRoot: "/home/coder/crabbox", Wait: "yes"}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: "shared", StatusOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != "cbx_owner" || lease.Server.CloudID != "alice/shared" {
		t.Fatalf("unexpected lease: %#v", lease)
	}
}

func TestCoderResolveNeedsListAllUsesOnlyOwnerQualifiedRequestOrClaim(t *testing.T) {
	installCoderClaimState(t)
	if err := claimLeaseForRepoProvider("cbx_owner", "shared", coderProvider, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	server := Server{Name: "shared", Labels: map[string]string{"coder_workspace_ref": "alice/shared"}}
	if err := updateLeaseClaimEndpoint("cbx_owner", server, SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	backend := &coderLeaseBackend{spec: Provider{}.Spec(), cfg: Config{Coder: CoderConfig{CLIPath: "coder", WorkspacePrefix: "crabbox-", WorkRoot: "/home/coder/crabbox", Wait: "yes"}}}
	tests := []struct {
		id   string
		want bool
	}{
		{id: "alice/shared", want: true},
		{id: "cbx_owner", want: true},
		{id: "blue", want: false},
	}
	for _, tc := range tests {
		got, err := backend.resolveNeedsListAll(tc.id)
		if err != nil {
			t.Fatalf("resolveNeedsListAll(%q): %v", tc.id, err)
		}
		if got != tc.want {
			t.Fatalf("resolveNeedsListAll(%q)=%v want %v", tc.id, got, tc.want)
		}
	}
}

func TestCoderClaimLookupPreservesOwnerQualifiedWorkspaceRefs(t *testing.T) {
	installCoderClaimState(t)
	if err := claimLeaseForRepoProvider("cbx_alice", "shared", coderProvider, t.TempDir(), time.Hour, true); err != nil {
		t.Fatal(err)
	}
	aliceServer := Server{Name: "shared", Labels: map[string]string{"coder_workspace_ref": "alice/shared"}}
	if err := updateLeaseClaimEndpoint("cbx_alice", aliceServer, SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	if err := claimLeaseForRepoProvider("cbx_local", "shared", coderProvider, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	localServer := Server{Name: "shared", Labels: map[string]string{"coder_workspace": "shared"}}
	if err := updateLeaseClaimEndpoint("cbx_local", localServer, SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	claims, err := listCoderClaimsByWorkspace(Config{Coder: CoderConfig{WorkspacePrefix: "crabbox-"}})
	if err != nil {
		t.Fatal(err)
	}
	aliceClaim, ok := coderClaimForWorkspace(claims, coderWorkspace{Name: "shared", Owner: "alice"})
	if !ok || aliceClaim.LeaseID != "cbx_alice" {
		t.Fatalf("owner-qualified workspace resolved wrong claim: ok=%v claim=%#v", ok, aliceClaim)
	}
	bobClaim, ok := coderClaimForWorkspace(claims, coderWorkspace{Name: "shared", Owner: "bob"})
	if ok {
		t.Fatalf("owner-qualified workspace without exact claim matched %#v", bobClaim)
	}
	localClaim, ok := coderClaimForWorkspace(claims, coderWorkspace{Name: "shared"})
	if !ok || localClaim.LeaseID != "cbx_local" || coderClaimKeep(localClaim) {
		t.Fatalf("bare workspace resolved wrong claim: ok=%v claim=%#v", ok, localClaim)
	}
}

func TestCoderResolveRejectsAmbiguousBareClaimWorkspace(t *testing.T) {
	backend := &coderLeaseBackend{spec: Provider{}.Spec(), cfg: Config{Coder: CoderConfig{WorkspacePrefix: "crabbox-"}}}
	workspaces := []coderWorkspace{
		{Name: "shared", Owner: "alice", Template: "go-dev"},
		{Name: "shared", Owner: "bob", Template: "go-dev"},
	}
	claims := map[string]LeaseClaim{
		coderClaimKey("shared"): {
			LeaseID: "cbx_local",
			Slug:    "shared",
			Labels:  map[string]string{"coder_workspace": "shared"},
		},
	}
	_, _, _, err := backend.resolveWorkspace("cbx_local", workspaces, claims, coderWorkspaceNameCounts(workspaces))
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguous bare claim error, got %v", err)
	}
}

func TestCoderResolveClaimFallbackUsesLeaseSuffixedWorkspaceName(t *testing.T) {
	cfg := Config{Coder: CoderConfig{WorkspacePrefix: "crabbox-"}}
	claim := LeaseClaim{LeaseID: "cbx_123456abcdef", Slug: "blue", Labels: map[string]string{}}
	workspaceName, err := coderClaimWorkspaceName(cfg, claim)
	if err != nil {
		t.Fatal(err)
	}
	backend := &coderLeaseBackend{spec: Provider{}.Spec(), cfg: cfg}
	workspace, leaseID, slug, err := backend.resolveWorkspace(claim.LeaseID, []coderWorkspace{{Name: workspaceName, Template: "go-dev"}}, map[string]LeaseClaim{
		coderClaimKey(workspaceName): claim,
	}, map[string]int{coderClaimKey(workspaceName): 1})
	if err != nil {
		t.Fatal(err)
	}
	if workspace.Name != workspaceName || leaseID != claim.LeaseID || slug != claim.Slug {
		t.Fatalf("unexpected fallback resolution workspace=%#v lease=%q slug=%q", workspace, leaseID, slug)
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
	if !regexp.MustCompile(`^coder-alice-shared-[0-9a-f]{6}$`).MatchString(lease.SSH.Host) || !strings.Contains(lease.SSH.ProxyCommand, "'alice/shared'") || lease.Server.Labels["coder_workspace_ref"] != "alice/shared" || lease.Server.CloudID != "alice/shared" {
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
	} {
		got, err := coderWorkspaceName(tc.prefix, tc.slug, tc.leaseID)
		if err != nil {
			t.Fatalf("coderWorkspaceName(%q,%q): %v", tc.prefix, tc.slug, err)
		}
		if got != tc.want {
			t.Fatalf("coderWorkspaceName(%q,%q)=%q want %q", tc.prefix, tc.slug, got, tc.want)
		}
	}
	got, err := coderWorkspaceName("crabbox-", "this-name-is-much-longer-than-coder-allows", "cbx_123456abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if matched := regexp.MustCompile(`^crabbox-this-name-is-much-[0-9a-f]{6}$`).MatchString(got); !matched {
		t.Fatalf("unexpected long workspace name %q", got)
	}
}

func TestCoderWorkspaceNameDisambiguatesLongSlugs(t *testing.T) {
	first, err := coderWorkspaceName("crabbox-", "this-name-is-much-longer-than-coder-allows-alpha", "cbx_first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := coderWorkspaceName("crabbox-", "this-name-is-much-longer-than-coder-allows-bravo", "cbx_second")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("long slugs collided: %q", first)
	}
}

func TestCoderUniqueWorkspaceNameKeepsFriendlySlugWhenAvailable(t *testing.T) {
	slug, name, err := coderUniqueWorkspaceName(nil, "crabbox-", "blue", "cbx_123456abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if slug != "blue" {
		t.Fatalf("friendly slug=%q want blue", slug)
	}
	wantSuffix := "-" + coderWorkspaceHash("cbx_123456abcdef")
	if !strings.HasPrefix(name, "crabbox-blue-") || !strings.HasSuffix(name, wantSuffix) {
		t.Fatalf("workspace name=%q should include lease hash suffix %q", name, wantSuffix)
	}
}

func TestCoderUniqueWorkspaceNameErrorsOnLeaseSuffixedCollision(t *testing.T) {
	collisionSlug := coderCollisionSlug("blue", "cbx_123456abcdef")
	existingName, err := coderWorkspaceName("crabbox-", collisionSlug, "cbx_123456abcdef")
	if err != nil {
		t.Fatal(err)
	}
	slug, name, err := coderUniqueWorkspaceName([]coderWorkspace{{Name: existingName}}, "crabbox-", "blue", "cbx_123456abcdef")
	if err == nil {
		t.Fatalf("expected collision error, got slug=%q name=%q", slug, name)
	}
	if !strings.Contains(err.Error(), "collides") {
		t.Fatalf("expected collision error, got %v", err)
	}
}

func installCoderClaimState(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("CRABBOX_CONFIG", t.TempDir()+"/missing.yaml")
}

func writeCoderClaim(t *testing.T, leaseID, body string) {
	t.Helper()
	path := filepath.Join(os.Getenv("XDG_STATE_HOME"), "crabbox", "claims", leaseID+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.TrimSpace(body)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}
