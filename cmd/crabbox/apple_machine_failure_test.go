package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/openclaw/crabbox/internal/cli"
)

// This is a native CLI stand-in, not a VM. It exercises the production process
// runner, provider, local history and final printer without Apple Container.
func appleMachineFailureCLI(t *testing.T, mode string) (string, string) {
	t.Helper()
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("Apple Machine host admission requires macOS arm64")
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(home, "config.yaml"))
	t.Setenv("CRABBOX_TEST_MACHINE_MODE", mode)
	root := filepath.Join(home, "native")
	t.Setenv("CRABBOX_TEST_MACHINE_ROOT", root)
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(home, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init", "-q", repo)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %s: %v", output, err)
	}
	t.Chdir(repo)
	path := filepath.Join(home, "fake-container")
	const script = `#!/bin/sh
set -eu
root="$CRABBOX_TEST_MACHINE_ROOT"
machines="$root/plugin-state/machine-apiserver/machines"
case "$1 $2" in
  'system status') printf '{"status":"running","paths":{"appRoot":"%s"}}' "$root" ;;
  'machine create')
    name="$4"
    mkdir -p "$machines/$name"
    printf '%s' "$name" > "$root/created-name"
    if [ "$CRABBOX_TEST_MACHINE_MODE" = create ]; then
      printf 'failed to start container %s-suffix\n' "$name" >&2
      exit 5
    fi ;;
  'machine inspect') printf '[{"id":"%s","status":"running"}]' "$3" ;;
  'machine list')
    if [ -f "$root/created-name" ] && [ -d "$machines/$(cat "$root/created-name")" ]; then
      printf '[{"id":"%s","status":"running"}]' "$(cat "$root/created-name")"
    else
      printf '[]'
    fi ;;
  'machine rm') rm -r "$machines/$3" ;;
  'machine run')
    if [ "$5" != --cwd ]; then exit 0; fi
    if [ "$CRABBOX_TEST_MACHINE_MODE" = stdout ]; then
      head -c 4194304 /dev/zero | tr '\000' X
    else
      head -c 4194304 /dev/zero | tr '\000' S >&2
    fi
    exit 23 ;;
  *) exit 99 ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path, root
}

func TestAppleMachineFailedCreationRecoveryDiagnostic(t *testing.T) {
	for _, command := range []string{"run", "warmup"} {
		t.Run(command, func(t *testing.T) {
			path, root := appleMachineFailureCLI(t, "create")
			var stdout, stderr bytes.Buffer
			args := []string{command, "--provider", "apple-machine", "--apple-machine-cli", path}
			if command == "run" {
				args = append(args, "--timing-record", "off", "--", "true")
			}
			err := (cli.App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), args)
			printError(&stderr, err)
			nameBytes, readErr := os.ReadFile(filepath.Join(root, "created-name"))
			if readErr != nil {
				t.Fatalf("%v; command error=%v stderr=%s", readErr, err, stderr.String())
			}
			name := string(nameBytes)
			if cli.ExitCodeForError(err, 0) != 5 || stdout.Len() != 0 {
				t.Fatalf("exit=%d stdout=%q stderr=%q", cli.ExitCodeForError(err, 0), stdout.String(), stderr.String())
			}
			want := "creation may be incomplete: inspect container machine inspect " + cli.ShellQuote(name) + " before manual cleanup"
			if !strings.Contains(stderr.String(), want) || !strings.Contains(stderr.String(), "failed to start container "+name+"-suffix") {
				t.Fatalf("missing recovery or original failure in stderr: %s", stderr.String())
			}
			if _, err := os.Stat(filepath.Join(root, "plugin-state", "machine-apiserver", "machines", name)); err != nil {
				t.Fatalf("unbound failed machine must remain untouched: %v", err)
			}
			claims, err := cli.ListLeaseClaims()
			if err != nil || len(claims) != 0 {
				t.Fatalf("failed creation published claims: %v %v", claims, err)
			}
		})
	}
}

func TestAppleMachineWorkloadFailureStreamsAndHistory(t *testing.T) {
	for _, stream := range []string{"stdout", "stderr"} {
		t.Run(stream, func(t *testing.T) {
			path, _ := appleMachineFailureCLI(t, stream)
			var stdout, stderr bytes.Buffer
			err := (cli.App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), []string{
				"run", "--record-local", "--timing-record", "off", "--provider", "apple-machine", "--apple-machine-cli", path, "--", "false",
			})
			printError(&stderr, err)
			if cli.ExitCodeForError(err, 0) != 23 {
				t.Fatalf("exit=%d stderr=%s", cli.ExitCodeForError(err, 0), stderr.String())
			}
			state, err := cli.CrabboxStateDir()
			if err != nil {
				t.Fatal(err)
			}
			files, err := filepath.Glob(filepath.Join(state, "history", "runs", "*", "record.json"))
			if err != nil || len(files) != 1 {
				t.Fatalf("history records=%v: %v", files, err)
			}
			data, err := os.ReadFile(files[0])
			if err != nil {
				t.Fatal(err)
			}
			var record struct {
				ID         string `json:"id"`
				Diagnostic string `json:"diagnostic"`
				ExitCode   int    `json:"exitCode"`
				RunStatus  string `json:"runStatus"`
				ErrorKind  string `json:"errorKind"`
			}
			if err := json.Unmarshal(data, &record); err != nil {
				t.Fatal(err)
			}
			const diagnostic = "apple-machine command exited 23"
			if record.Diagnostic != diagnostic || record.ExitCode != 23 || record.RunStatus != "failed" || record.ErrorKind != "command-exit" {
				t.Errorf("history diagnostic bytes=%d exit=%d status=%s kind=%s", len(record.Diagnostic), record.ExitCode, record.RunStatus, record.ErrorKind)
			}
			wantStdout := ""
			wantStderr := ""
			if stream == "stdout" {
				wantStdout = strings.Repeat("X", 4<<20)
			} else {
				wantStderr += strings.Repeat("S", 4<<20)
			}
			wantStderr += diagnostic + "\n"
			if stdout.String() != wantStdout || stderr.String() != wantStderr {
				t.Errorf("output bytes stdout=%d stderr=%d; want %d/%d; stderr prefix=%q", stdout.Len(), stderr.Len(), len(wantStdout), len(wantStderr), stderr.String()[:min(stderr.Len(), 120)])
			}
		})
	}
}
