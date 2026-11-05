package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The repository's own hydrate workflow must leave its readiness marker where
// Crabbox reads it: $HOME/.crabbox/actions on Linux runners and the shared
// ProgramData handoff directory on native Windows runners.
func TestRepositoryHydrateWorkflowWritesMarkerWhereCrabboxReadsIt(t *testing.T) {
	bash := hydrateWorkflowBash(t)
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "hydrate.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Name  string `yaml:"name"`
				Shell string `yaml:"shell"`
				Run   string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	var script string
	for _, job := range workflow.Jobs {
		for _, step := range job.Steps {
			if strings.Contains(step.Run, "READY_AT=") {
				if step.Shell != "bash" {
					t.Fatalf("marker step shell=%q; the test runs it with bash", step.Shell)
				}
				script = step.Run
			}
		}
	}
	if script == "" {
		t.Fatal("hydrate workflow has no step that writes the readiness marker")
	}

	for _, tc := range []struct {
		runnerOS string
		dir      func(home, programData string) string
	}{
		{"Linux", func(home, _ string) string { return filepath.Join(home, ".crabbox", "actions") }},
		{"Windows", func(_, programData string) string { return filepath.Join(programData, "crabbox", "actions") }},
	} {
		t.Run(tc.runnerOS, func(t *testing.T) {
			home, programData, workspace := t.TempDir(), t.TempDir(), t.TempDir()
			// Windows paths may contain apostrophes; the env file must carry them intact.
			runnerTemp := filepath.Join(t.TempDir(), "O'Hara")
			if err := os.Mkdir(runnerTemp, 0o700); err != nil {
				t.Fatal(err)
			}
			gitEnv := append(os.Environ(), "GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.com", "GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.com")
			for _, args := range [][]string{{"init", "-q"}, {"commit", "-q", "--allow-empty", "-m", "fixture"}} {
				cmd := exec.Command("git", append([]string{"-C", workspace}, args...)...)
				cmd.Env = gitEnv
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v: %s", args, err, out)
				}
			}
			scriptPath := filepath.Join(t.TempDir(), "step.sh")
			if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(bash, "--noprofile", "--norc", "-eo", "pipefail", scriptPath)
			cmd.Env = append(gitEnv,
				"HOME="+home, "PROGRAMDATA="+programData, "RUNNER_OS="+tc.runnerOS,
				"GITHUB_WORKSPACE="+workspace, "GITHUB_RUN_ID=4242", "RUNNER_TEMP="+runnerTemp, "RUNNER_TOOL_CACHE="+t.TempDir(),
				"CRABBOX_ID=cbx_123", "CRABBOX_JOB=hydrate",
			)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("marker step: %v: %s", err, out)
			}
			dir := tc.dir(home, programData)
			marker, err := os.ReadFile(filepath.Join(dir, "cbx_123.env"))
			if err != nil {
				t.Fatalf("marker not written to %s: %v", dir, err)
			}
			state := parseActionsHydrationState(string(marker))
			if state.RunID != "4242" || state.Job != "hydrate" || state.Workspace == "" || state.EnvFile == "" {
				t.Fatalf("unexpected marker: %#v", state)
			}
			if tc.runnerOS == "Windows" {
				state = normalizeActionsHydrationStateForTarget(SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}, state)
				// Crabbox reads %ProgramData%\crabbox\actions\<lease>.env on native Windows.
				read := windowsActionsHydrationPath(actionsHydrationStatePath("cbx_123"))
				if !strings.EqualFold(read, `C:\ProgramData\crabbox\actions\cbx_123.env`) {
					t.Fatalf("crabbox reads %s", read)
				}
			}
			if got, want := state.EnvFile[strings.LastIndexAny(state.EnvFile, `/\`)+1:], "cbx_123.env.sh"; got != want {
				t.Fatalf("ENV_FILE=%q, want a %s beside the marker", state.EnvFile, want)
			}
			if _, err := os.Stat(filepath.Join(dir, "cbx_123.env.sh")); err != nil {
				t.Fatalf("env file not written beside the marker: %v", err)
			}
			if tc.runnerOS == "Windows" && runtime.GOOS == "windows" {
				assertNativeWindowsCommandUsesHydratedEnv(t, workspace, state.EnvFile, runnerTemp)
			}
			if tc.runnerOS == "Linux" {
				out, err := exec.Command(bash, "--noprofile", "--norc", "-c", `. "$1" && printf '%s' "$RUNNER_TEMP"`, "source", state.EnvFile).Output()
				if err != nil || string(out) != runnerTemp {
					t.Fatalf("sourcing the env file gave RUNNER_TEMP=%q, %v; want %q", out, err, runnerTemp)
				}
			}
		})
	}
}

// assertNativeWindowsCommandUsesHydratedEnv runs a command through the exact
// prefix native Windows `crabbox run` sends, which imports the hydration env
// file, and checks that commands still resolve and paths arrive intact.
func assertNativeWindowsCommandUsesHydratedEnv(t *testing.T, workspace, envFile, runnerTemp string) {
	t.Helper()
	command := windowsRemoteCommandWithEnvFiles(workspace, nil, []string{envFile}, []string{"cmd.exe", "/d", "/c", "set", "RUNNER_TEMP"})
	fields := strings.Fields(command)
	if len(fields) < 2 || fields[0] != "powershell.exe" {
		t.Fatalf("unexpected native Windows command: %q", command)
	}
	out, err := exec.Command(fields[0], fields[1:]...).CombinedOutput()
	text := strings.ReplaceAll(string(out), "\r\n", "\n")
	if err != nil || !strings.Contains(text, "RUNNER_TEMP="+runnerTemp+"\n") {
		envData, _ := os.ReadFile(envFile)
		t.Fatalf("command did not run with the hydrated env (want RUNNER_TEMP=%s): %v\n%s\nenv file:\n%s", runnerTemp, err, text, envData)
	}
}

func hydrateWorkflowBash(t *testing.T) string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	if runtime.GOOS == "windows" {
		// System32 and WindowsApps bash.exe launch WSL, not Git Bash.
		lower := strings.ToLower(bash)
		if strings.Contains(lower, `\system32\`) || strings.Contains(lower, `\windowsapps\`) {
			t.Skip("Git Bash not available")
		}
	}
	return bash
}
