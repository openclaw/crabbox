package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func workspacePathFixture(t *testing.T) (root, workdir, selected, decoy, searchRoot string) {
	t.Helper()
	root = t.TempDir()
	cfg := baseConfig()
	cfg.WorkRoot = "work ' root"
	workdir = remoteJoin(cfg, "lease", "repo")
	searchRoot = filepath.Join(root, "search")
	return root, workdir, filepath.Join(root, workdir), filepath.Join(searchRoot, workdir), searchRoot
}

func runWorkspacePathCommand(t *testing.T, cwd, command, input string, env ...string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-lc", command)
	cmd.Dir = cwd
	cmd.Env = append(cmd.Environ(), env...)
	cmd.Stdin = strings.NewReader(input)
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("workspace command did not finish: %v", ctx.Err())
	}
	return out, err
}

func requireWorkspaceFile(t *testing.T, path, want string) {
	t.Helper()
	if data, err := os.ReadFile(path); err != nil || string(data) != want {
		t.Errorf("workspace file %s: error=%v, bytes=%q, want=%q", filepath.Base(path), err, data, want)
	}
}

func cloneWorkspacePath(t *testing.T, fixture gitCoherenceFixture, target string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(fixture.workspace(t, fixture.b, false), target); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteWorkspaceCommandLiteralPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX shells")
	}
	for _, mode := range []string{"argv", "shell", "script", "script-shebang", "absolute-env-control"} {
		t.Run(mode, func(t *testing.T) {
			root, workdir, selected, decoy, searchRoot := workspacePathFixture(t)
			profile := ".crabbox/env/proof.env"
			const argument = "argument ' $literal"
			payload := "printf '%s|%s\\n' \"$PROFILE_VALUE\" \"$__crabbox_workdir\"\nprintf '%s\\n' \"$@\"\ncat proof\n"
			for _, item := range []struct{ dir, value string }{{selected, "selected"}, {decoy, "decoy"}} {
				mustWriteTestFile(t, filepath.Join(item.dir, "proof"), item.value+"\n")
				mustWriteTestFile(t, filepath.Join(item.dir, profile), formatShellEnvFile(map[string]string{
					"PROFILE_VALUE": item.value, "__crabbox_workdir": "retained",
				}))
				if mode == "script" || mode == "script-shebang" {
					body := payload
					if mode == "script" {
						body = "values=(bash)\nprintf '%s\\n' \"${values[0]}\"\n" + body
					} else {
						body = "#!/bin/sh\n" + body
					}
					path := filepath.Join(item.dir, ".crabbox/scripts/proof")
					mustWriteTestFile(t, path, body)
					if err := os.Chmod(path, 0o700); err != nil {
						t.Fatal(err)
					}
				}
			}
			var command string
			switch mode {
			case "shell":
				command = remoteShellCommandWithEnvFiles(workdir, nil, []string{profile}, "set -- "+shellQuote(argument)+"; "+payload)
			case "script", "script-shebang":
				command = remoteRunScriptCommandWithEnvFiles(workdir, nil, []string{profile}, &RunScriptSpec{
					RemotePath: ".crabbox/scripts/proof", Shebang: mode == "script-shebang",
				}, []string{argument})
			default:
				if mode == "absolute-env-control" {
					workdir = selected
				}
				command = remoteCommandWithEnvFiles(workdir, nil, []string{profile}, []string{"sh", "-c", payload, "probe", argument})
			}
			out, err := runWorkspacePathCommand(t, root, command, "", "CDPATH="+searchRoot)
			want := "selected|retained\n" + argument + "\nselected\n"
			if mode == "script" {
				want = "bash\n" + want
			}
			if err != nil || string(out) != want {
				t.Errorf("workspace command: error=%v, bytes=%q, want=%q", err, out, want)
			}
			requireWorkspaceFile(t, filepath.Join(decoy, "proof"), "decoy\n")
		})
	}
}

func TestRemoteSyncWorkspaceLiteralPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX shells")
	}
	fixture := newGitCoherenceFixture(t)
	plan := fixture.plan(t, fixture.b)
	const token = "0123456789abcdef0123456789abcdef"
	for _, phase := range []string{"writer", "finalizer", "reader"} {
		t.Run(phase, func(t *testing.T) {
			root, workdir, selected, decoy, searchRoot := workspacePathFixture(t)
			cloneWorkspacePath(t, fixture, selected)
			cloneWorkspacePath(t, fixture, decoy)
			selectedMeta, decoyMeta := coherenceMetaDir(t, selected), coherenceMetaDir(t, decoy)
			mustWriteTestFile(t, filepath.Join(decoyMeta, "sync-fingerprint"), "decoy")
			var command, input string
			switch phase {
			case "writer":
				command = remoteWriteSyncManifestsNew(workdir, token)
				input = syncManifestInputForTarget(SSHTarget{TargetOS: targetLinux}, []byte("tracked.txt\x00"), nil)
			case "finalizer":
				stageCoherenceFinalize(t, selected, token)
				stageCoherenceFinalize(t, decoy, token)
				command = remoteFinalizeSync(workdir, remoteSyncFinalizeOptions{Token: token, Coherence: plan, Fingerprint: "selected"})
			case "reader":
				for _, item := range []struct{ dir, fingerprint string }{{selected, "selected"}, {decoy, "decoy"}} {
					stageCoherenceFinalize(t, item.dir, token)
					out, err := runWorkspacePathCommand(t, root, remoteFinalizeSync(item.dir, remoteSyncFinalizeOptions{
						Token: token, Coherence: plan, Fingerprint: item.fingerprint,
					}), "")
					if err != nil {
						t.Fatalf("prepare finalized workspace: %v (%d diagnostic bytes)", err, len(out))
					}
				}
				command = remoteReadSyncFingerprint(workdir, plan)
			}
			out, err := runWorkspacePathCommand(t, root, command, input, "CDPATH="+searchRoot)
			if err != nil {
				t.Errorf("%s failed: %v (%d diagnostic bytes)", phase, err, len(out))
			}
			switch phase {
			case "writer":
				requireWorkspaceFile(t, filepath.Join(selectedMeta, remoteSyncPendingManifestName(token)), "tracked.txt\x00")
				if _, err := os.Lstat(filepath.Join(decoyMeta, remoteSyncPendingManifestName(token))); !os.IsNotExist(err) {
					t.Errorf("writer touched the search-path workspace: %v", err)
				}
			case "finalizer":
				requireWorkspaceFile(t, filepath.Join(selectedMeta, "sync-finalize-complete-token"), token)
				requireWorkspaceFile(t, filepath.Join(selectedMeta, "sync-fingerprint"), "selected")
				requireWorkspaceFile(t, filepath.Join(decoyMeta, remoteSyncPendingManifestName(token)), "tracked.txt\x00")
			case "reader":
				if string(out) != "selected" {
					t.Errorf("fingerprint bytes=%q, want selected", out)
				}
			}
			requireWorkspaceFile(t, filepath.Join(decoyMeta, "sync-fingerprint"), "decoy")
		})
	}
}

func TestRemoteGitSeedLiteralPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX shells")
	}
	fixture := newGitCoherenceFixture(t)
	plan := fixture.plan(t, fixture.b)
	t.Run("existing workspace", func(t *testing.T) {
		root, workdir, selected, decoy, searchRoot := workspacePathFixture(t)
		cloneWorkspacePath(t, fixture, selected)
		cloneWorkspacePath(t, fixture, decoy)
		wrongOrigin := filepath.Join(root, "unused-origin.git")
		runGit(t, selected, "remote", "set-url", "origin", wrongOrigin)
		runGit(t, decoy, "remote", "set-url", "origin", wrongOrigin)
		out, err := runWorkspacePathCommand(t, root, remoteGitSeed(workdir, plan), "", "CDPATH="+searchRoot)
		if err != nil {
			t.Errorf("seed existing workspace: %v (%d diagnostic bytes)", err, len(out))
		}
		requireGitOutput(t, selected, fixture.origin, "remote", "get-url", "origin")
		requireGitOutput(t, decoy, wrongOrigin, "remote", "get-url", "origin")
	})
	t.Run("cold relative publication", func(t *testing.T) {
		root := t.TempDir()
		wrongBase := t.TempDir()
		// Both the correct target and the old target after cd / stay in owned roots.
		workdir := strings.TrimPrefix(filepath.ToSlash(wrongBase), "/") + "/repo"
		selected := filepath.Join(root, workdir)
		retained := filepath.Join(wrongBase, "repo", "keep")
		mustWriteTestFile(t, retained, "keep")
		out, err := runWorkspacePathCommand(t, root, remoteGitSeed(workdir, plan), "")
		if err != nil {
			t.Errorf("seed relative workspace: %v (%d diagnostic bytes)", err, len(out))
		}
		requireGitOutput(t, selected, fixture.b, "rev-parse", "HEAD")
		requireGitOutput(t, selected, plan.Tree, "write-tree")
		requireWorkspaceFile(t, retained, "keep")
		if leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(selected), ".seed.*")); err != nil || len(leftovers) != 0 {
			t.Errorf("seed staging was not cleaned: count=%d, error=%v", len(leftovers), err)
		}
	})
	t.Run("empty path rejects before clone", func(t *testing.T) {
		root := t.TempDir()
		localOrigin := filepath.Join(root, "not-a-repository")
		mustWriteTestFile(t, localOrigin, "retained")
		// A local non-repository also stops old code before its unsafe publication.
		emptyPlan := plan
		emptyPlan.RemoteURL = localOrigin
		out, err := runWorkspacePathCommand(t, root, remoteGitSeed("", emptyPlan), "")
		if err == nil || strings.Contains(string(out), "phase=clone") {
			t.Errorf("empty workspace reached clone: error=%v (%d diagnostic bytes)", err, len(out))
		}
		requireWorkspaceFile(t, localOrigin, "retained")
		if leftovers, err := filepath.Glob(filepath.Join(root, ".seed.*")); err != nil || len(leftovers) != 0 {
			t.Errorf("empty workspace left staging: count=%d, error=%v", len(leftovers), err)
		}
	})
	for _, suffix := range []string{"/", "/."} {
		t.Run("directory suffix "+suffix, func(t *testing.T) {
			root := t.TempDir()
			selected := filepath.Join(root, "repo")
			mustWriteTestFile(t, filepath.Join(selected, "keep"), "original")
			mustWriteTestFile(t, filepath.Join(root, "sibling", "keep"), "retained")
			out, err := runWorkspacePathCommand(t, root, remoteGitSeed(selected+suffix, plan), "")
			if suffix == "/" {
				if err != nil {
					t.Errorf("seed directory suffix: %v (%d diagnostic bytes)", err, len(out))
				}
				requireGitOutput(t, selected, fixture.b, "rev-parse", "HEAD")
			} else {
				t.Logf("dot directory replacement: native error=%v, diagnostic bytes=%d", err, len(out))
				if err == nil {
					t.Error("ambiguous dot directory replacement unexpectedly succeeded")
				}
				requireWorkspaceFile(t, filepath.Join(selected, "keep"), "original")
			}
			requireWorkspaceFile(t, filepath.Join(root, "sibling", "keep"), "retained")
			for _, dir := range []string{root, selected} {
				if leftovers, err := filepath.Glob(filepath.Join(dir, ".seed.*")); err != nil || len(leftovers) != 0 {
					t.Errorf("seed staging was not cleaned: count=%d, error=%v", len(leftovers), err)
				}
			}
		})
	}
}

func TestLocalActionsStepLiteralPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX shells")
	}
	for _, shell := range []string{"bash", "sh"} {
		t.Run(shell, func(t *testing.T) {
			root, workdir, selected, decoy, searchRoot := workspacePathFixture(t)
			mustWriteTestFile(t, filepath.Join(selected, "package", "proof"), "selected\n")
			mustWriteTestFile(t, filepath.Join(decoy, "package", "proof"), "decoy\n")
			var script strings.Builder
			fmt.Fprintf(&script, "set -e\nRUNNER_TEMP=%s\n", shellQuote(t.TempDir()))
			script.WriteString(localActionsRuntimeShell())
			err := appendLocalHydrateSteps(&script, []localHydrateStep{{Name: "native", Shell: shell, WorkingDirectory: "package", Run: "cat proof"}}, localHydrateScriptContext{
				Workdir: workdir, Env: map[string]string{}, Inputs: map[string]string{}, StepOutputs: map[string]map[string]string{},
			})
			if err != nil {
				t.Fatal(err)
			}
			out, err := runWorkspacePathCommand(t, root, script.String(), "", "CDPATH="+searchRoot)
			if err != nil || string(out) != "local actions: native\nselected\n" {
				t.Errorf("Actions step: error=%v, bytes=%q", err, out)
			}
			requireWorkspaceFile(t, filepath.Join(decoy, "package", "proof"), "decoy\n")
		})
	}
}

func TestRemoteGitOverlayLiteralPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX shells")
	}
	for _, reuse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reuse=%t", reuse), func(t *testing.T) {
			fixture := newGitOverlayFixture(t)
			root, workdir, selected, decoy, searchRoot := workspacePathFixture(t)
			mustWriteTestFile(t, filepath.Join(decoy, "keep"), "retained")
			if reuse {
				if err := os.MkdirAll(filepath.Dir(selected), 0o700); err != nil {
					t.Fatal(err)
				}
				runGit(t, root, "clone", "--quiet", fixture.origin, selected)
				mustWriteTestFile(t, filepath.Join(selected, "node_modules", "cached.txt"), "trusted cache")
			}
			out, err := runWorkspacePathCommand(t, root, remotePrepareGitOverlay(workdir, fixture.plan), "", "CDPATH="+searchRoot)
			if err != nil {
				t.Errorf("prepare relative overlay: %v (%d diagnostic bytes)", err, len(out))
			}
			assertNoGitOverlayResidue(t, selected)
			requireGitOutput(t, selected, fixture.repo.Head, "rev-parse", "HEAD")
			requireGitOutput(t, selected, fixture.plan.Tree, "write-tree")
			if reuse {
				requireWorkspaceFile(t, filepath.Join(selected, "node_modules", "cached.txt"), "trusted cache")
			}
			requireWorkspaceFile(t, filepath.Join(decoy, "keep"), "retained")
		})
	}
}

func TestRunEnvHelperLiteralPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX shells")
	}
	root := t.TempDir()
	decoy := filepath.Join(root, "search")
	profile := runEnvProfilePath("proof")
	helper := runEnvHelperPath("proof")
	for _, item := range []struct{ dir, value string }{{root, "selected"}, {decoy, "decoy"}} {
		mustWriteTestFile(t, filepath.Join(item.dir, profile), formatShellEnvFile(map[string]string{"PROFILE_VALUE": item.value}))
	}
	mustWriteTestFile(t, filepath.Join(root, helper), formatRunEnvHelper(profile))
	command := "bash " + shellQuote(helper) + " sh -c " + shellQuote(`printf '%s\n' "$PROFILE_VALUE"`)
	out, err := runWorkspacePathCommand(t, root, command, "", "CDPATH="+decoy)
	if err != nil || string(out) != "selected\n" {
		t.Errorf("env helper selected the wrong profile: error=%v, bytes=%q", err, out)
	}
}

func TestRemoteResultsMarkerRejectsMissingWorkspace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	fixture := newGitCoherenceFixture(t)
	caller := fixture.workspace(t, fixture.b, false)
	missing := filepath.Join(t.TempDir(), "missing")
	out, err := runWorkspacePathCommand(t, caller, remoteTouchResultsMarker(missing), "")
	if err == nil {
		t.Errorf("missing workspace was reported ready (%d diagnostic bytes)", len(out))
	}
	for _, name := range []string{".git/crabbox/results-start", ".crabbox/results-start"} {
		if _, err := os.Lstat(filepath.Join(caller, name)); !os.IsNotExist(err) {
			t.Errorf("result marker escaped into the caller: %s, error=%v", name, err)
		}
	}
	requireWorkspaceFile(t, filepath.Join(caller, "tracked.txt"), "B\n")
}

func TestProfileDoctorLiteralPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX disk tools")
	}
	root := t.TempDir()
	cfg := baseConfig()
	cfg.WorkRoot = "-work"
	workdir := profileDoctorWorkdirForLease(cfg, "lease")
	if err := os.Mkdir(filepath.Join(root, workdir), 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := runWorkspacePathCommand(t, root, profileDoctorScript(DoctorProfileConfig{MinDiskGB: 1}, workdir), "")
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[1] != "disk" {
			continue
		}
		free, parseErr := strconv.Atoi(strings.TrimPrefix(fields[2], "free_gb="))
		if parseErr != nil || free < 0 || !strings.HasSuffix(line, "path=-work") {
			t.Fatalf("disk probe did not measure the literal path: %q", line)
		}
		wantExit := 0
		if free < 1 {
			wantExit = 1
		}
		if got := exitCode(err); got != wantExit {
			t.Errorf("disk capacity exit=%d, want=%d for free_gb=%d", got, wantExit, free)
		}
		return
	}
	t.Fatalf("disk probe did not emit a capacity record: error=%v, output=%q", err, out)
}

func TestRemoteFreshPRLiteralPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX shells")
	}
	fixture := newGitCoherenceFixture(t)
	runGit(t, fixture.origin, "update-ref", "refs/pull/1/head", fixture.b)
	spec := FreshPRSpec{Owner: "example", Repo: "repo", Number: 1}
	for _, mode := range []string{"dash", "absolute"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			cfg := baseConfig()
			cfg.WorkRoot = "-work"
			if mode == "absolute" {
				cfg.WorkRoot = filepath.Join(root, "work")
			}
			workdir := remoteJoin(cfg, "lease", spec.WorkdirName())
			selected := workdir
			if !filepath.IsAbs(selected) {
				selected = filepath.Join(root, selected)
			}
			mustWriteTestFile(t, filepath.Join(root, "keep"), "retained")
			// Use Git's URL rewrite; clone, fetch, and checkout remain native and local.
			gitConfig := filepath.Join(root, "gitconfig")
			runGit(t, root, "config", "--file", gitConfig, "url."+fixture.origin+".insteadOf", "https://github.com/example/repo.git")
			out, err := runWorkspacePathCommand(t, root, remoteFreshPRCheckoutCommand(workdir, spec), "", "GIT_CONFIG_GLOBAL="+gitConfig)
			if err != nil {
				t.Errorf("fresh PR checkout: %v (%d diagnostic bytes)", err, len(out))
			}
			requireGitOutput(t, selected, fixture.b, "rev-parse", "HEAD")
			requireWorkspaceFile(t, filepath.Join(selected, "tracked.txt"), "B\n")
			requireWorkspaceFile(t, filepath.Join(root, "keep"), "retained")
		})
	}
}
