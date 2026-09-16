package testutil

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

type CommandIntent struct {
	Command     []string
	LiteralArgs map[int]bool
	ShellMode   bool
}

type ExistingShellResult struct {
	Commands  []string
	ErrorCode int
	Err       error
}

type ExistingShellRunner func(*testing.T, string, CommandIntent) ExistingShellResult

func VerifyExistingShellCommandIntent(t *testing.T, run ExistingShellRunner) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell contract")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash unavailable")
	}
	for _, name := range []string{"literal separator", "literal assignment", "literal singleton", "mixed operators", "quoted argv", "unmarked assignment", "explicit source", "empty source", "inferred source", "missing command"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			marker := filepath.Join(root, "must-not-exist")
			program := filepath.Join(root, "FOO=x")
			if err := os.WriteFile(program, []byte("#!/bin/sh\nprintf 'literal:%s' \"$*\"\nexit 42\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			intent := CommandIntent{}
			want, wantExit := "", 0
			switch name {
			case "literal separator":
				intent.Command = []string{"printf", "<%s>", ";", "touch", marker}
				intent.LiteralArgs = map[int]bool{2: true}
				want = "<;><touch><" + marker + ">"
			case "literal assignment":
				intent.Command = []string{"FOO=x", "argument"}
				intent.LiteralArgs = map[int]bool{0: true}
				want, wantExit = "literal:argument", 42
			case "literal singleton":
				intent.Command = []string{"FOO=x"}
				intent.LiteralArgs = map[int]bool{0: true}
				want, wantExit = "literal:", 42
			case "mixed operators":
				intent.Command = []string{"printf", "%s", ";", "&&", "printf", "%s", "tail"}
				intent.LiteralArgs = map[int]bool{2: true}
				want = ";tail"
			case "quoted argv":
				intent.Command = []string{"printf", "<%s>", "", "$literal", "a'b"}
				want = "<><$literal><a'b>"
			case "unmarked assignment":
				intent.Command = []string{"CBX_PROBE=value", "sh", "-c", `printf %s "$CBX_PROBE"`}
				want = "value"
			case "explicit source":
				intent.Command = []string{`printf %s "$unexported_fixture_state"; exit 7`}
				intent.ShellMode = true
				want, wantExit = "existing-shell", 7
			case "empty source":
				intent.Command = []string{""}
				intent.ShellMode = true
			case "inferred source":
				intent.Command = []string{"printf %s inferred"}
				want = "inferred"
			}
			result := run(t, root, intent)
			if name == "missing command" {
				if result.ErrorCode != 2 {
					t.Fatalf("missing command error=%v", result.Err)
				}
				if len(result.Commands) != 1 {
					t.Fatalf("missing command reached workload: %v", result.Commands)
				}
				return
			}
			if result.Err != nil {
				t.Fatal(result.Err)
			}
			if len(result.Commands) != 2 {
				t.Fatalf("commands=%v want preparation and workload", result.Commands)
			}
			source := "unexported_fixture_state=existing-shell\n" + result.Commands[1]
			cmd := exec.CommandContext(t.Context(), bash, "--noprofile", "--norc", "-c", source)
			cmd.Dir = root
			cmd.Env = []string{"HOME=" + root, "PATH=" + root + ":/usr/bin:/bin"}
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			commandErr := cmd.Run()
			code := 0
			if commandErr != nil {
				var exitErr *exec.ExitError
				if !errors.As(commandErr, &exitErr) {
					t.Fatal(commandErr)
				}
				code = exitErr.ExitCode()
			}
			if code != wantExit || stdout.String() != want {
				t.Fatalf("source=%q stdout=%q stderr=%q exit=%d; want %q/%d", source, stdout.String(), stderr.String(), code, want, wantExit)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("literal separator ran touch: %v", err)
			}
		})
	}
}

func VerifyNativeCommandIntent(t *testing.T, shell string, includeEmptySource bool, run func(*testing.T, CommandIntent) []string) {
	t.Helper()
	tests := []struct {
		name   string
		intent CommandIntent
		want   []string
	}{
		{"ordinary", CommandIntent{Command: []string{"printf", "%s", "hello"}}, []string{"printf", "%s", "hello"}},
		{"literal separator", CommandIntent{Command: []string{"printf", "%s", ";", "touch", "sentinel"}, LiteralArgs: map[int]bool{2: true}}, []string{"printf", "%s", ";", "touch", "sentinel"}},
		{"literal assignment executable", CommandIntent{Command: []string{"FOO=x", "argument"}, LiteralArgs: map[int]bool{0: true}}, []string{"FOO=x", "argument"}},
		{"literal singleton", CommandIntent{Command: []string{"literal command $(echo x)"}, LiteralArgs: map[int]bool{0: true}}, []string{"literal command $(echo x)"}},
		{"invalid assignment executable", CommandIntent{Command: []string{"bad-name=x", "argument"}}, []string{"bad-name=x", "argument"}},
		{"mixed operators", CommandIntent{Command: []string{"printf", "%s", ";", "&&", "printf", "%s", "done"}, LiteralArgs: map[int]bool{2: true}}, []string{shell, "-lc", "'printf' '%s' ';' && 'printf' '%s' 'done'"}},
		{"inferred source", CommandIntent{Command: []string{"printf one && printf two"}}, []string{shell, "-lc", "printf one && printf two"}},
		{"explicit source", CommandIntent{Command: []string{"printf one; exit 7"}, ShellMode: true}, []string{shell, "-lc", "printf one; exit 7"}},
		{"leading assignment", CommandIntent{Command: []string{"GREETING=hello world", "printf", "%s", "$GREETING"}}, []string{shell, "-lc", "GREETING='hello world' 'printf' '%s' '$GREETING'"}},
	}
	if includeEmptySource {
		tests = append([]struct {
			name   string
			intent CommandIntent
			want   []string
		}{{"empty explicit source", CommandIntent{Command: []string{""}, ShellMode: true}, []string{shell, "-lc", ""}}}, tests...)
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := run(t, tc.intent)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("native command=%#v want %#v", got, tc.want)
			}
		})
	}
}
