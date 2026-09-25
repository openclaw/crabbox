package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestActionsWorkspaceConsumers(t *testing.T) {
	for _, consumer := range []string{"code", "cache", "editor"} {
		for _, state := range []string{"same", "configured", "bound", "foreign", "unreadable", "absent"} {
			if consumer == "editor" && state != "foreign" && state != "unreadable" {
				continue // Successful editor handoff intentionally waits for the session.
			}
			t.Run(consumer+"/"+state, func(t *testing.T) {
				clearConfigEnv(t)
				isolateRunTestUserDirs(t, t.TempDir())
				root := t.TempDir()
				runGit(t, root, "init", "-q")
				const origin = "https://git.example.test/group/project.git"
				runGit(t, root, "remote", "add", "origin", origin)
				t.Chdir(root)
				config := filepath.Join(t.TempDir(), "config.yaml")
				cfg := defaultConfig()
				configBody := "provider: run-env-profile-test\n"
				if state == "configured" {
					cfg.Actions.Repo = "example-org/workflow"
					configBody += "actions:\n  repo: example-org/workflow\n"
				}
				if err := os.WriteFile(config, []byte(configBody), 0600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("CRABBOX_CONFIG", config)
				remoteOrigin := origin
				if state == "foreign" {
					remoteOrigin = "https://git.example.test/group/other.git"
				} else if state == "configured" || state == "bound" {
					remoteOrigin = "https://github.com/example-org/workflow.git"
				}
				marker := "WORKSPACE=/work/custom-project\nENV_FILE=/work/foreign.env\nRUN_ID=123\n"
				if state == "absent" {
					marker = ""
				}
				log := filepath.Join(t.TempDir(), "command")
				t.Setenv("CRABBOX_TEST_MARKER", marker)
				t.Setenv("CRABBOX_TEST_ORIGIN", remoteOrigin)
				t.Setenv("CRABBOX_TEST_READ_ERROR", state)
				target := actionsWorkspaceTestSSH(t, `
case "$remote" in
  *'cat "$HOME"/'*'.crabbox/actions/'*)
    [ "$CRABBOX_TEST_READ_ERROR" != unreadable ] || exit 42
    printf '%s' "$CRABBOX_TEST_MARKER"
    ;;
  *'exact_git_root || exit 2'*'git remote get-url origin'*)
    printf '%s\n' "$CRABBOX_TEST_ORIGIN"
    ;;
  *cache-command*) printf '%s\n' "$remote" > `+shellQuote(log)+` ;;
esac`)
				repo := Repo{Root: root, Name: filepath.Base(root), RemoteURL: origin}
				if state == "bound" {
					if err := ClaimLeaseForRepoProvider("cbx_env_profile_test", "", "run-env-profile-test", root, time.Minute, false); err != nil {
						t.Fatal(err)
					}
					claim, exists, err := prepareActionsWorkspaceClaim(t.Context(), "cbx_env_profile_test", repo)
					if err != nil {
						t.Fatal(err)
					}
					if err := bindActionsWorkspaceClaim(t.Context(), target, repo, GitHubRepo{Owner: "example-org", Name: "workflow"}, parseActionsHydrationState(marker), claim, exists); err != nil {
						t.Fatal(err)
					}
				}
				var err error
				var stdout, stderr bytes.Buffer
				app := App{Stdout: &stdout, Stderr: &stderr}
				switch consumer {
				case "code":
					var workspace, folder string
					var hydrated bool
					workspace, folder, hydrated, err = codeWorkspace(t.Context(), target, cfg, "cbx_env_profile_test", repo)
					if (state == "same" || state == "configured" || state == "bound") && (workspace != "/work/custom-project" || folder != workspace || !hydrated) {
						t.Fatalf("custom workspace lost: %q %q hydrated=%t", workspace, folder, hydrated)
					}
					if state == "absent" && (workspace != remoteJoin(defaultConfig(), "cbx_env_profile_test", repo.Name) || hydrated) {
						t.Fatal("absent marker did not preserve canonical workspace")
					}
					if err != nil && (workspace != "" || folder != "" || hydrated) {
						t.Fatal("unverified workspace escaped the consumer")
					}
				case "cache":
					err = app.cacheWarm(t.Context(), []string{"--id", "cbx_env_profile_test", "--", "cache-command"})
				case "editor":
					err = app.runEditorHandoff(t.Context(), "zed", editorHandoffSpecs["zed"], resolvedSSHCommandTarget{
						Config: defaultConfig(), Lease: LeaseTarget{SSH: target, LeaseID: "cbx_env_profile_test"},
					}, true)
				}
				wantErr := state == "foreign" || state == "unreadable"
				if (err != nil) != wantErr {
					t.Fatalf("error=%v, want error=%t", err, wantErr)
				}
				command, readErr := os.ReadFile(log)
				if wantErr {
					if !os.IsNotExist(readErr) || stdout.Len() != 0 || strings.Contains(stderr.String(), "using GitHub Actions workspace") {
						t.Fatal("unverified marker reached command or handoff")
					}
				} else if consumer == "cache" {
					if readErr != nil {
						t.Fatal(readErr)
					}
					if state != "absent" && (!strings.Contains(string(command), "/work/custom-project") || !strings.Contains(string(command), "/work/foreign.env")) {
						t.Fatal("same-repository path or environment was not preserved")
					}
					if state == "absent" && strings.Contains(string(command), "/work/foreign.env") {
						t.Fatal("absent marker contributed environment")
					}
				}
			})
		}
	}
}

func TestActionsRepositoryIdentity(t *testing.T) {
	const canonical = "https://github.com/example-org/project.git"
	for _, remote := range []string{
		canonical,
		"https://github.com/Example-Org/Project",
		"https://github.com/example-org/project.git/",
		"https://fixture-user:fixture-password@github.com/example-org/project.git",
		"git@github.com:example-org/project.git",
		"ssh://git@github.com/example-org/project.git",
		"git://github.com/example-org/project.git",
		"http://GITHUB.COM/example-org/project.git",
	} {
		if actionsRepositoryIdentity(remote) != actionsRepositoryIdentity(canonical) {
			t.Errorf("equivalent GitHub remote %q did not match", remote)
		}
	}
	for _, remote := range []string{
		"https://github.com/other/project.git",
		"https://github.com/example-org/other.git",
		"ssh://git@github.com:2222/example-org/project.git",
		"ssh://other@github.com/example-org/project.git",
		"https://github.com/example-org/project.git/child",
		"https://github.com/example-org/project.git?other",
		"https://github.com/example-org/project.git#other",
		"https://github.com/other/../example-org/project.git",
		"example-org/project",
		"github.com/example-org/project",
	} {
		if actionsRepositoryIdentity(remote) == actionsRepositoryIdentity(canonical) {
			t.Errorf("distinct remote %q matched", remote)
		}
	}
	for _, remote := range []string{
		"ssh://builder@git.example.test:2222/group/project.git",
		"ssh://builder:fixture-password@git.example.test:2222/group/project.git",
		"builder@git.example.test:group/project.git",
		"https://git.example.test/group/project.git",
		"file:///work/origin.git",
		"/work/origin.git",
	} {
		if actionsRepositoryIdentity(remote) != remote {
			t.Errorf("non-GitHub identity changed: %q", remote)
		}
	}
	for _, remote := range []string{"", " \t", "origin\ninjected"} {
		if actionsRepositoryIdentity(remote) != "" {
			t.Error("missing or unverifiable origin accepted")
		}
	}
}

func TestActionsWorkspaceClaimBinding(t *testing.T) {
	for _, kind := range []string{"selected", "invoking", "unselected", "nested", "changed-claim", "wrong-owner", "absent-claim"} {
		t.Run(kind, func(t *testing.T) {
			isolateRunTestUserDirs(t, t.TempDir())
			target := actionsWorkspaceTestSSH(t, `exec /bin/sh -c "$remote"`)
			repo := Repo{Root: t.TempDir(), RemoteURL: "https://github.com/example-org/source.git"}
			workspace := t.TempDir()
			runGit(t, workspace, "init", "-q")
			origin := "git@github.com:example-org/workflow.git"
			if kind == "invoking" {
				origin = repo.RemoteURL
			} else if kind == "unselected" {
				origin = "https://github.com/example-org/foreign.git"
			}
			runGit(t, workspace, "remote", "add", "origin", origin)
			state := actionsHydrationState{Workspace: workspace, RunID: "123", EnvFile: filepath.Join(workspace, "environment")}
			if kind == "nested" {
				state.Workspace = filepath.Join(workspace, "nested")
				if err := os.Mkdir(state.Workspace, 0700); err != nil {
					t.Fatal(err)
				}
			}
			const leaseID = "cbx_binding"
			if kind != "absent-claim" {
				if err := ClaimLeaseForRepoProvider(leaseID, "", "aws", repo.Root, time.Minute, false); err != nil {
					t.Fatal(err)
				}
			}
			claim, exists, err := prepareActionsWorkspaceClaim(t.Context(), leaseID, repo)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "changed-claim" {
				if _, err := UpdateLeaseClaimLabelsIfUnchanged(leaseID, claim, map[string]string{"state": "ready"}); err != nil {
					t.Fatal(err)
				}
			} else if kind == "wrong-owner" {
				repo.Root = t.TempDir()
			}
			err = bindActionsWorkspaceClaim(t.Context(), target, repo, GitHubRepo{Owner: "example-org", Name: "workflow"}, state, claim, exists)
			wantErr := kind == "unselected" || kind == "nested" || kind == "changed-claim" || kind == "wrong-owner"
			if (err != nil) != wantErr {
				t.Fatalf("binding error=%v, want error=%t", err, wantErr)
			}
			saved, present, err := ReadLeaseClaimWithPresence(leaseID)
			if err != nil || present != exists {
				t.Fatalf("claim presence changed: present=%t err=%v", present, err)
			}
			if kind != "selected" {
				if saved.ActionsWorkspace != nil {
					t.Fatal("unverified or implicit hydration persisted extra authority")
				}
				return
			}
			want := &ActionsWorkspaceBinding{Origin: "https://github.com/example-org/workflow", MarkerFingerprint: actionsWorkspaceMarkerFingerprint(state)}
			if !reflect.DeepEqual(saved.ActionsWorkspace, want) {
				t.Fatalf("binding=%+v, want %+v", saved.ActionsWorkspace, want)
			}
			if err := verifyRetainedActionsWorkspace(t.Context(), target, leaseID, defaultConfig(), repo, state); err != nil {
				t.Fatalf("flag-only hydration did not authorize bare attach: %v", err)
			}
			changed := state
			changed.EnvFile += ".replacement"
			if err := verifyRetainedActionsWorkspace(t.Context(), target, leaseID, defaultConfig(), repo, changed); err == nil {
				t.Fatal("changed marker reused prior hydration consent")
			}
			runGit(t, workspace, "remote", "set-url", "origin", "https://github.com/example-org/foreign.git")
			if err := verifyRetainedActionsWorkspace(t.Context(), target, leaseID, defaultConfig(), repo, state); err == nil {
				t.Fatal("reassigned origin reused prior hydration consent")
			}
			runGit(t, workspace, "remote", "set-url", "origin", origin)
			cleared, exists, err := prepareActionsWorkspaceClaim(t.Context(), leaseID, repo)
			if err != nil || !exists || cleared.ActionsWorkspace != nil {
				t.Fatalf("replacement hydration did not revoke consent: %v", err)
			}
			if err := verifyRetainedActionsWorkspace(t.Context(), target, leaseID, defaultConfig(), repo, state); err == nil {
				t.Fatal("legacy unbound foreign marker was accepted")
			}
			cfg := defaultConfig()
			cfg.Actions.Repo = "example-org/workflow"
			if err := verifyRetainedActionsWorkspace(t.Context(), target, leaseID, cfg, repo, state); err != nil {
				t.Fatalf("explicit configured repository was rejected: %v", err)
			}
		})
	}
}

func actionsWorkspaceTestSSH(t *testing.T, body string) SSHTarget {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake SSH fixture")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nfor remote; do :; done\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return SSHTarget{Host: "workspace.example.test", Port: "22", FallbackPorts: []string{}, TargetOS: targetLinux}
}

func TestReadActionsWorkspaceStateDistinguishesAbsentAndUnverifiableMarkers(t *testing.T) {
	for _, kind := range []string{"absent-directory", "absent-marker", "valid", "empty", "malformed", "unreadable", "unsearchable-parent", "dangling-link", "directory"} {
		t.Run(kind, func(t *testing.T) {
			target := actionsWorkspaceTestSSH(t, `exec /bin/sh -c "$remote"`)
			home := t.TempDir()
			t.Setenv("HOME", home)
			const leaseID = "cbx_marker_identity"
			marker := filepath.Join(home, actionsHydrationStatePath(leaseID))
			parent := filepath.Dir(marker)
			if kind != "absent-directory" {
				if err := os.MkdirAll(parent, 0700); err != nil {
					t.Fatal(err)
				}
			}
			content := "WORKSPACE=/work/foreign-project\nENV_FILE=/work/foreign.env\n"
			switch kind {
			case "empty":
				content = ""
			case "malformed":
				content = "RUN_ID=123\n"
			}
			switch kind {
			case "absent-directory", "absent-marker":
			case "directory":
				if err := os.Mkdir(marker, 0700); err != nil {
					t.Fatal(err)
				}
			case "dangling-link":
				if err := os.Symlink(filepath.Join(parent, "missing"), marker); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.WriteFile(marker, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "unreadable" || kind == "unsearchable-parent" {
				if os.Geteuid() == 0 {
					t.Skip("root can read mode-000 fixtures")
				}
				path := marker
				if kind == "unsearchable-parent" {
					path = parent
				}
				if err := os.Chmod(path, 0000); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(path, 0700) })
				// The lifecycle reader deliberately tolerates incomplete state.
				// Adoption must not inherit its successful-empty failure behavior.
				raw, err := readActionsHydrationState(t.Context(), target, leaseID)
				if err != nil || raw.Workspace != "" {
					t.Fatalf("raw reader baseline changed: state=%+v err=%v", raw, err)
				}
			}
			state, err := readActionsWorkspaceState(t.Context(), target, leaseID)
			switch kind {
			case "absent-directory", "absent-marker":
				if err != nil || state.Workspace != "" {
					t.Fatalf("genuine absence: state=%+v err=%v", state, err)
				}
			case "valid":
				if err != nil || state.Workspace != "/work/foreign-project" {
					t.Fatalf("marker read: state=%+v err=%v", state, err)
				}
			default:
				if err == nil || state.Workspace != "" {
					t.Fatalf("unverifiable marker accepted: state=%+v err=%v", state, err)
				}
			}
		})
	}
}

func TestVerifyActionsWorkspaceExactGitRoot(t *testing.T) {
	const origin = "https://github.com/example-org/project.git"
	target := actionsWorkspaceTestSSH(t, `exec /bin/sh -c "$remote"`)
	parent := t.TempDir()
	workspace := filepath.Join(parent, "custom workspace's root")
	if err := os.MkdirAll(filepath.Join(workspace, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	runGit(t, workspace, "init", "-q")
	runGit(t, workspace, "remote", "add", "origin", "git@github.com:example-org/project.git")
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(workspace, alias); err != nil {
		t.Fatal(err)
	}
	t.Chdir(parent)
	for _, tc := range []struct {
		name, path, remote string
		wantErr            bool
	}{
		{"absolute", workspace, origin, false},
		{"relative", filepath.Base(workspace), origin, false},
		{"symlink", alias, origin, false},
		{"parent-repo", filepath.Join(workspace, "nested"), origin, true},
		{"missing", filepath.Join(parent, "missing"), origin, true},
		{"not-a-repo", parent, origin, true},
		{"foreign", workspace, "https://github.com/example-org/other.git", true},
		{"missing-invoking-origin", workspace, "", true},
		{"missing-workspace", "", origin, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyActionsWorkspace(t.Context(), target, Repo{RemoteURL: tc.remote}, actionsHydrationState{Workspace: tc.path})
			if (err != nil) != tc.wantErr {
				t.Fatalf("verification error=%v, want error=%t", err, tc.wantErr)
			}
		})
	}
	runGit(t, workspace, "remote", "remove", "origin")
	if err := verifyActionsWorkspace(t.Context(), target, Repo{RemoteURL: origin}, actionsHydrationState{Workspace: workspace}); err == nil {
		t.Fatal("missing workspace origin accepted")
	}
}

func TestVerifyActionsWorkspaceProbeFailures(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       error
		canceled   bool
		deadline   bool
	}{
		{name: "missing origin", body: "exit 0"},
		{name: "transport", body: "printf private-origin >&2; exit 255"},
		{name: "credentialed mismatch", body: "printf '%s\\n' 'https://fixture:private-origin@github.com/example-org/other.git'"},
		{name: "bounded output", body: "printf '%05000d' 0", want: ErrSSHOutputLimit},
		{name: "canceled", body: "exit 99", canceled: true, want: context.Canceled},
		{name: "deadline", body: "exec /bin/sleep 10", deadline: true, want: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := actionsWorkspaceTestSSH(t, tc.body)
			ctx := t.Context()
			if tc.canceled {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if tc.deadline {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
			}
			err := verifyActionsWorkspace(ctx, target, Repo{RemoteURL: "https://git.example.test/project.git"}, actionsHydrationState{Workspace: "/work/project"})
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("error=%v, want failure containing %v", err, tc.want)
			}
			if strings.Contains(err.Error(), "private-origin") {
				t.Fatal("probe leaked diagnostics")
			}
		})
	}
}

func TestActionsWorkspaceTargetCommands(t *testing.T) {
	for _, target := range []SSHTarget{
		{TargetOS: targetLinux},
		{TargetOS: targetMacOS},
		{TargetOS: targetWindows, WindowsMode: windowsModeWSL2},
		{TargetOS: targetWindows, WindowsMode: windowsModeNormal},
	} {
		command := remoteActionsWorkspaceOrigin(target, "/work/custom project's root")
		if isWindowsNativeTarget(target) {
			command = decodePowerShellCommand(t, command)
			for _, want := range []string{"Test-CrabboxSameDirectory", "GetFinalPathNameByHandle", "rev-parse --show-toplevel", "remote get-url origin", "custom project''s root"} {
				if !strings.Contains(command, want) {
					t.Errorf("native Windows probe missing %q", want)
				}
			}
		} else if !strings.Contains(command, "exact_git_root || exit 2") {
			t.Error("POSIX probe missing exact-root check")
		}
		for _, forbidden := range []string{"remote set-url", "remote add", "rm -", "write-tree", "git fetch", "git clone"} {
			if strings.Contains(command, forbidden) {
				t.Errorf("read-only probe includes %q", forbidden)
			}
		}
	}
}

func TestVerifyActionsWorkspaceWSLTransportPreservesCallerDeadline(t *testing.T) {
	oldStage := stageWSLSpool
	t.Cleanup(func() { stageWSLSpool = oldStage })
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	stopped := errors.New("stop at WSL transport")
	stageWSLSpool = func(spool *wslStageSpool, stageCtx context.Context, _ *SSHTarget, timing wslStageTiming, _, _ string, _ io.Writer) (string, error) {
		got, _ := stageCtx.Deadline()
		want, _ := ctx.Deadline()
		if !got.Equal(want) || timing.operation != 15*time.Second {
			t.Fatal("identity probe changed caller or execution deadline")
		}
		reader, err := spool.input.reset()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		_, _, command, _ := decodeWSLStage(t, raw)
		if !strings.Contains(command, "exact_git_root || exit 2") {
			t.Fatal("identity probe did not use WSL POSIX transport")
		}
		return "", stopped
	}
	target := SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}
	err := verifyActionsWorkspace(ctx, target, Repo{RemoteURL: "https://git.example.test/project.git"}, actionsHydrationState{Workspace: "/work/project"})
	if !errors.Is(err, stopped) {
		t.Fatalf("error=%v", err)
	}
}

func TestRemoteActionsWorkspaceOriginNativeWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("native Windows filesystem identity")
	}
	workspace := t.TempDir()
	runGit(t, workspace, "init", "-q")
	runGit(t, workspace, "remote", "add", "origin", "https://git.example.test/project.git")
	nested := filepath.Join(workspace, "nested")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	target := SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}
	for _, path := range []string{workspace, nested, filepath.Join(workspace, "missing")} {
		command := remoteActionsWorkspaceOrigin(target, path)
		out, err := runDecodedWindowsPowerShell(t, command)
		if path == workspace {
			if err != nil || strings.TrimSpace(string(out)) != "https://git.example.test/project.git" {
				t.Fatalf("root probe=%s err=%v", out, err)
			}
		} else if err == nil {
			t.Fatalf("non-root workspace accepted: %s", path)
		}
	}
}

func TestReadActionsWorkspaceStateNativeWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("native Windows marker reader")
	}
	const leaseID = "cbx_marker_identity"
	target := SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}
	for _, kind := range []string{"absent", "valid", "hidden", "empty", "directory"} {
		t.Run(kind, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "marker.env")
			workspace := `C:\work\project`
			if kind == "hidden" {
				workspace = `C:\work\foreign-project`
			}
			switch kind {
			case "valid", "hidden":
				if err := os.WriteFile(marker, []byte("WORKSPACE="+workspace+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if kind == "hidden" {
					if out, err := runWindowsPowerShellScript(t, "[IO.File]::SetAttributes("+psQuote(marker)+", [IO.FileAttributes]::Hidden)"); err != nil {
						t.Fatalf("hide fixture: %s err=%v", out, err)
					}
				}
			case "empty":
				if err := os.WriteFile(marker, nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(marker, 0700); err != nil {
					t.Fatal(err)
				}
			}
			script := decodePowerShellCommand(t, remoteReadActionsWorkspaceState(target, leaseID))
			script = strings.Replace(script, psQuote(windowsActionsHydrationPath(actionsHydrationStatePath(leaseID))), psQuote(marker), 1)
			out, err := runWindowsPowerShellScript(t, script)
			switch kind {
			case "absent":
				if err != nil || len(out) != 0 {
					t.Fatalf("absent marker: %s err=%v", out, err)
				}
			case "valid", "hidden":
				if err != nil || parseActionsHydrationState(string(out)).Workspace != workspace {
					t.Fatalf("valid marker: %s err=%v", out, err)
				}
				if kind == "hidden" {
					check := "if (([IO.File]::GetAttributes(" + psQuote(marker) + ") -band [IO.FileAttributes]::Hidden) -eq 0) { exit 1 }"
					if out, err := runWindowsPowerShellScript(t, check); err != nil {
						t.Fatalf("marker attributes changed: %s err=%v", out, err)
					}
				}
			case "empty":
				if err != nil || strings.TrimSpace(string(out)) != "MARKER_PRESENT=1" {
					t.Fatalf("empty marker lost presence: %s err=%v", out, err)
				}
			case "directory":
				if err == nil {
					t.Fatal("non-file marker accepted")
				}
			}
		})
	}
}
