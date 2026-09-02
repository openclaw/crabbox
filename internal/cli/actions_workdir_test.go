package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLocalActionsHydrationLiteralPaths(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("local Actions hydration requires native Linux tools")
	}
	for _, name := range []string{"relative-bash", "relative-sh", "dash", "absolute"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			remoteHome := filepath.Join(root, "remote ")
			other := filepath.Join(root, "other")
			for _, dir := range []string{remoteHome, other} {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			mustWriteTestFile(t, filepath.Join(other, "keep"), "retained")
			nonce, err := randomHex(8)
			if err != nil {
				t.Fatal(err)
			}
			leaseID := "cbx_path_" + nonce
			cfg := baseConfig()
			cfg.WorkRoot = "work"
			if name == "dash" {
				cfg.WorkRoot = "-work"
			} else if name == "absolute" {
				cfg.WorkRoot = filepath.Join(remoteHome, "work")
			}
			cfg.Actions.Repo = "example/repo"
			cfg.Actions.Workflow = ".github/workflows/hydrate.yml"
			cfg.Actions.Job = "hydrate"
			repo := Repo{Root: filepath.Join(root, "source"), Name: "repo", Head: strings.Repeat("a", 40)}
			selected := remoteJoin(cfg, leaseID, repo.Name)
			unbound := strings.TrimPrefix(selected, remoteHome+"/")
			if !strings.HasPrefix(selected, "/") {
				selected = remoteHome + "/" + selected
			}
			mustWriteTestFile(t, filepath.Join(selected, "proof"), "selected\n")
			if err := os.Mkdir(filepath.Join(selected, "package"), 0o700); err != nil {
				t.Fatal(err)
			}
			shell := "bash"
			if name == "relative-sh" {
				shell = "sh"
			}
			actionPath := filepath.Join(repo.Root, "conditional", "action.yml")
			mustWriteTestFile(t, filepath.Join(repo.Root, "proof.lock"), "original lock\n")
			mustWriteTestFile(t, actionPath, `runs:
  using: composite
  steps:
    - run: |
        printf '%s\n' prepared '${{ hashFiles('proof.lock') }}' > "$GITHUB_WORKSPACE/snapshot"
`)
			workflow := fmt.Sprintf(`jobs:
  hydrate:
    env:
      PWD: %q
      PROOF: "${{ github.workspace }}/proof"
    steps:
      - id: pick
        run: echo "where=${{ github.workspace }}" >> "$GITHUB_OUTPUT"
      - name: prepared source
        if: %q
        uses: ./conditional
      - if: "false"
        uses: ./missing
      - name: native
        shell: %s
        working-directory: package
        run: |
          {
            cat '${{ github.workspace }}/proof'
            cat "$GITHUB_WORKSPACE/proof"
            printf '%%s\n' "$PROOF" '${{ runner.temp }}' '${{ runner.tool_cache }}'
          } > "$GITHUB_WORKSPACE/report"
          printf 'WORKSPACE=%%s\nJOB=hydrate\nENV_FILE=%%s\n' "$GITHUB_WORKSPACE" "$HOME/%s" > "$HOME/%s"
`, other, "steps.pick.outputs.where != '"+unbound+"'", shell, actionsHydrationEnvPath(leaseID), actionsHydrationStatePath(leaseID))
			mustWriteTestFile(t, filepath.Join(repo.Root, cfg.Actions.Workflow), workflow)
			changed := installActionsSourceMutationSSH(t, remoteHome, repo.Root)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			var stdout, stderr bytes.Buffer
			app := App{Stdout: &stdout, Stderr: &stderr}
			target := SSHTarget{User: "fixture", Host: "127.0.0.1", Port: "22", TargetOS: targetLinux, NoControlMaster: true}
			state, hydrateErr := app.hydrateActionsLocally(ctx, cfg, repo, target, leaseID, "hydrate", nil, 15*time.Second, true, false, nil)
			exitPath := filepath.Join(remoteHome, actionsHydrationLocalExitPath(leaseID))
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				data, err := os.ReadFile(exitPath)
				if err == nil {
					t.Logf("native hydration exit=%q", data)
					if string(data) != "0\n" {
						t.Errorf("native hydration did not complete: exit=%q", data)
					}
					break
				}
				if !os.IsNotExist(err) {
					t.Fatal(err)
				}
				select {
				case <-ctx.Done():
					t.Fatalf("native hydration did not close: %v (hydrate error=%v)", ctx.Err(), hydrateErr)
				case <-ticker.C:
				}
			}
			if hydrateErr != nil || state.Workspace != selected {
				t.Errorf("hydrate error=%v workspace=%q, want=%q (%d diagnostic bytes)", hydrateErr, state.Workspace, selected, stderr.Len())
			}
			runnerRoot := path.Join(path.Dir(selected), ".crabbox-local-actions", localActionsRunnerRootName(leaseID))
			want := "selected\nselected\n" + selected + "/proof\n" + path.Join(runnerRoot, "tmp") + "\n" + path.Join(runnerRoot, "tools") + "\n"
			requireWorkspaceFile(t, filepath.Join(selected, "report"), want)
			requireWorkspaceFile(t, filepath.Join(selected, "snapshot"), "prepared\n71f9ede0b30ceb06be7311abdc774e38756c24180b2b5f88fedfe4694024c977\n")
			requireWorkspaceFile(t, changed, "changed\n")
			requireWorkspaceFile(t, filepath.Join(other, "keep"), "retained")
		})
	}
}

func installActionsSourceMutationSSH(t *testing.T, remoteHome, repoRoot string) string {
	t.Helper()
	root := t.TempDir()
	mutatedAction := filepath.Join(root, "mutated-action.yml")
	mustWriteTestFile(t, mutatedAction, "runs:\n  using: composite\n  steps:\n    - run: echo mutated > \"$GITHUB_WORKSPACE/snapshot\"\n")
	changed := filepath.Join(root, "changed")
	actionPath := filepath.Join(repoRoot, "conditional", "action.yml")
	// Replace only SSH transport. The generated command and native tools still run.
	// Change local inputs at the first remote boundary, after they must be captured.
	ssh := "#!/bin/sh\nset -e\nremote=\nfor arg do remote=\"$arg\"; done\n" +
		"if [ ! -e " + shellQuote(changed) + " ]; then\n" +
		"  mkdir -p " + shellQuote(filepath.Dir(actionPath)) + "\n" +
		"  cp " + shellQuote(mutatedAction) + " " + shellQuote(actionPath) + "\n" +
		"  printf 'changed lock\\n' > " + shellQuote(filepath.Join(repoRoot, "proof.lock")) + "\n" +
		"  printf 'invalid: [\\n' > " + shellQuote(filepath.Join(repoRoot, ".github", "workflows", "hydrate.yml")) + "\n" +
		"  printf 'changed\\n' > " + shellQuote(changed) + "\nfi\n" +
		"export HOME=" + shellQuote(remoteHome) + "\ncd \"$HOME\" || exit 91\nexec /bin/sh -c \"$remote\"\n"
	bin := filepath.Join(root, "bin")
	mustWriteTestFile(t, filepath.Join(bin, "ssh"), ssh)
	if err := os.Chmod(filepath.Join(bin, "ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return changed
}
