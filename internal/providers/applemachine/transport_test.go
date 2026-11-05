package applemachine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func transportBackend(t *testing.T) (*backend, *recordingRunner, *machineFixture, string) {
	t.Helper()
	originalGOOS, originalGOARCH := hostGOOS, hostGOARCH
	hostGOOS, hostGOARCH = "darwin", "arm64"
	t.Cleanup(func() { hostGOOS, hostGOARCH = originalGOOS, originalGOARCH })
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := filepath.Join(home, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{}
	fixture := newMachineFixture(t, runner)
	return testBackend(runner), runner, fixture, repo
}

// Apple Container 1.0.0 through 1.6.1 pass machine-run words to init, which
// invokes the user's shell with -c "$*". Exercise that reparse with a real sh.
func reparseMachineCommand(ctx context.Context, req core.LocalCommandRequest, words []string) (core.LocalCommandResult, error) {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", strings.Join(words, " "))
	cmd.Dir = req.Dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	result := core.LocalCommandResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	emitMachineOutput(req, result)
	return result, err
}

func emitMachineOutput(req core.LocalCommandRequest, result core.LocalCommandResult) {
	if req.Stdout != nil {
		_, _ = io.WriteString(req.Stdout, result.Stdout)
	}
	if req.Stderr != nil {
		_, _ = io.WriteString(req.Stderr, result.Stderr)
	}
}

func TestRunShellTransportArgv(t *testing.T) {
	for _, tc := range []struct {
		name, value, quoted string
	}{
		{"empty", "", "''"},
		{"spaces", "a b", "'a b'"},
		{"leading and trailing spaces", " a b ", "' a b '"},
		{"tabs", "a\tb", "'a\tb'"},
		{"newlines", "a\nb\n", "'a\nb\n'"},
		{"carriage return", "a\rb", "'a\rb'"},
		{"single quote", "it's", "'it'\\''s'"},
		{"mixed quotes and newline", "'\"\n'", "''\\''\"\n'\\'''"},
		{"double quotes", `say "hello"`, `'say "hello"'`},
		{"backslashes", `a\b\`, `'a\b\'`},
		{"globs", "* ? [ab]", "'* ? [ab]'"},
		{"expansion", "$HOME ${USER} $(printf changed) `printf changed`", "'$HOME ${USER} $(printf changed) `printf changed`'"},
		{"metacharacters", "a|tr a b; (false) & <input >output", "'a|tr a b; (false) & <input >output'"},
		{"non ASCII", "café 日本語 🦀", "'café 日本語 🦀'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := []string{"/usr/bin/printf", "%s\\000", tc.value}
			checkMachineTransport(t, core.RunRequest{Command: command}, "'/usr/bin/printf' '%s\\000' "+tc.quoted, tc.value+"\x00", 0)
		})
	}
}

func TestRunShellTransportIntent(t *testing.T) {
	for _, tc := range []struct {
		name     string
		command  []string
		shell    bool
		literal  map[int]bool
		source   string
		stdout   string
		exitCode int
	}{
		{name: "positional argv", command: []string{"/bin/sh", "-c", `printf '<%s>\n' "$0" "$1"`, "arg zero", "one two"}, source: `'/bin/sh' '-c' 'printf '\''<%s>\n'\'' "$0" "$1"' 'arg zero' 'one two'`, stdout: "<arg zero>\n<one two>\n"},
		{name: "set e", command: []string{"/bin/sh", "-c", "set -e; false; echo still-ran"}, source: "'/bin/sh' '-c' 'set -e; false; echo still-ran'", exitCode: 1},
		{name: "printf script", command: []string{"/bin/sh", "-c", `printf "ready\n"`}, source: `'/bin/sh' '-c' 'printf "ready\n"'`, stdout: "ready\n"},
		{name: "printf arguments", command: []string{"/usr/bin/printf", "[%s]\\n", "a b", "c"}, source: "'/usr/bin/printf' '[%s]\\n' 'a b' 'c'", stdout: "[a b]\n[c]\n"},
		{name: "literal preset", command: []string{"/bin/echo", "a|tr a b"}, literal: map[int]bool{1: true}, source: "'/bin/echo' 'a|tr a b'", stdout: "a|tr a b\n"},
		{name: "literal operator", command: []string{"/bin/echo", "&&", "$HOME"}, literal: map[int]bool{1: true, 2: true}, source: "'/bin/echo' '&&' '$HOME'", stdout: "&& $HOME\n"},
		{name: "shell script", command: []string{`printf '[%s]\n' "a b" c`}, shell: true, source: `'/bin/sh' '-lc' 'printf '\''[%s]\n'\'' "a b" c'`, stdout: "[a b]\n[c]\n"},
		{name: "shell words", command: []string{"echo", "a b"}, shell: true, source: "'/bin/sh' '-lc' 'echo a b'", stdout: "a b\n"},
		{name: "empty shell", command: []string{""}, shell: true, source: "'/bin/sh' '-lc' ''"},
		{name: "inferred shell", command: []string{"printf x; printf y"}, source: "'/bin/sh' '-lc' 'printf x; printf y'", stdout: "xy"},
		{name: "inferred operator with literal preset", command: []string{"/bin/echo", "a|tr a b", "&&", "printf", "done"}, literal: map[int]bool{1: true}, source: `'/bin/sh' '-lc' ''\''/bin/echo'\'' '\''a|tr a b'\'' && '\''printf'\'' '\''done'\'''`, stdout: "a|tr a b\ndone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkMachineTransport(t, core.RunRequest{Command: tc.command, ShellMode: tc.shell, CommandLiteralArgs: tc.literal}, tc.source, tc.stdout, tc.exitCode)
		})
	}
	for _, code := range []int{0, 1, 2, 7, 42, 127, 255} {
		for _, shell := range []bool{false, true} {
			t.Run(fmt.Sprintf("exit %d shell=%t", code, shell), func(t *testing.T) {
				script := fmt.Sprintf("exit %d", code)
				command := []string{"/bin/sh", "-c", script}
				prefix := "'/bin/sh' '-c' "
				if shell {
					command, prefix = []string{script}, "'/bin/sh' '-lc' "
				}
				checkMachineTransport(t, core.RunRequest{Command: command, ShellMode: shell}, prefix+"'"+script+"'", "", code)
			})
		}
	}
}

func checkMachineTransport(t *testing.T, req core.RunRequest, source, stdout string, code int) {
	t.Helper()
	b, runner, fixture, repo := transportBackend(t)
	const leaseID = "cbx_123456789abc"
	fixture.claim(t, leaseID, "transport", repo)
	var output bytes.Buffer
	b.rt.Stdout = &output
	fixture.before = func(native core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if len(native.Args) < 2 || native.Args[0] != "machine" || native.Args[1] != "run" {
			return core.LocalCommandResult{}, nil, false
		}
		result, err := reparseMachineCommand(t.Context(), native, native.Args[6:])
		return result, err, true
	}
	req.ID, req.Repo = leaseID, core.Repo{Root: repo}
	result, err := b.Run(t.Context(), req)
	if result.ExitCode != code || core.ExitCodeForError(err, 0) != code || (err != nil) != (code != 0) {
		t.Errorf("exit=%d err=%v; want %d", result.ExitCode, err, code)
	}
	if output.String() != stdout {
		t.Errorf("stdout=%q; want %q", output.String(), stdout)
	}
	want := []string{"machine", "run", "--name", machineName(leaseID), "--cwd", repo, source}
	got := runner.requests[len(runner.requests)-1]
	if !reflect.DeepEqual(got.Args, want) {
		t.Errorf("native argv=%q; want %q", got.Args, want)
	}
}

func TestMachineCreationStdoutPurity(t *testing.T) {
	const payload = "{\"ok\":true}\n\x00unchanged\xff"
	for _, mode := range []string{"new run", "reused run", "warmup"} {
		t.Run(mode, func(t *testing.T) {
			b, _, fixture, repo := transportBackend(t)
			var stdout, stderr bytes.Buffer
			b.rt.Stdout, b.rt.Stderr = &stdout, &stderr
			var createdName string
			fixture.before = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
				if len(req.Args) < 2 || req.Args[0] != "machine" {
					return core.LocalCommandResult{}, nil, false
				}
				switch req.Args[1] {
				case "create":
					createdName = req.Args[3]
					fixture.add(t, createdName)
					result := core.LocalCommandResult{Stdout: createdName + "\n", Stderr: "creating machine\n"}
					emitMachineOutput(req, result)
					return result, nil, true
				case "run":
					if req.Args[4] == "--cwd" {
						result := core.LocalCommandResult{Stdout: payload, Stderr: "workload diagnostic\n"}
						emitMachineOutput(req, result)
						return result, nil, true
					}
					if !reflect.DeepEqual(req.Args, []string{"machine", "run", "--name", createdName, "':'"}) {
						t.Errorf("readiness argv=%q", req.Args)
					}
					result, err := reparseMachineCommand(t.Context(), req, req.Args[4:])
					return result, err, true
				}
				return core.LocalCommandResult{}, nil, false
			}
			if mode == "warmup" {
				if err := b.Warmup(t.Context(), core.WarmupRequest{Repo: core.Repo{Root: repo}, RequestedSlug: "stdout-probe"}); err != nil {
					t.Fatal(err)
				}
				lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
				if len(lines) != 2 || !strings.HasPrefix(lines[0], "leased cbx_") || !strings.Contains(lines[0], "machine="+createdName) || !strings.HasPrefix(lines[1], "warmup complete total=") {
					t.Errorf("warmup stdout=%q", stdout.String())
				}
			} else {
				req := core.RunRequest{Repo: core.Repo{Root: repo}, Command: []string{"cat", "synthetic-output"}}
				if mode == "reused run" {
					req.ID = "cbx_123456789abc"
					fixture.claim(t, req.ID, "stdout-probe", repo)
				}
				if _, err := b.Run(t.Context(), req); err != nil {
					t.Fatal(err)
				}
				if stdout.String() != payload {
					t.Errorf("workload stdout=%q; want %q", stdout.String(), payload)
				}
				if !strings.HasSuffix(stderr.String(), "workload diagnostic\n") {
					t.Errorf("workload stderr=%q", stderr.String())
				}
			}
			if mode != "reused run" && !strings.Contains(stderr.String(), createdName+"\ncreating machine\n") {
				t.Errorf("creation diagnostics missing from stderr=%q", stderr.String())
			}
		})
	}
}

func TestCreateMachineFailureStreams(t *testing.T) {
	for _, diagnostic := range []core.LocalCommandResult{
		{Stdout: "create progress\n", Stderr: "image unavailable\n", ExitCode: 9},
		{Stdout: "image unavailable\n", ExitCode: 9},
	} {
		t.Run(diagnostic.Stderr, func(t *testing.T) {
			b, _, fixture, _ := transportBackend(t)
			var stdout, stderr bytes.Buffer
			b.rt.Stdout, b.rt.Stderr = &stdout, &stderr
			cause := errors.New("native create failed")
			fixture.before = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
				emitMachineOutput(req, diagnostic)
				return diagnostic, cause, true
			}
			err := b.createMachine(t.Context(), "crabbox-test")
			if core.ExitCodeForError(err, 0) != 5 || !errors.Is(err, cause) || !strings.Contains(err.Error(), "image unavailable") {
				t.Errorf("create error=%v", err)
			}
			if stdout.Len() != 0 || stderr.String() != diagnostic.Stdout+diagnostic.Stderr {
				t.Errorf("stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}
