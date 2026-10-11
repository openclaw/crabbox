package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

type windowsArgvProcessResult struct {
	Args  []string
	Cwd   string
	Input string
	Env   string
}

func TestWindowsArgvHelperProcess(t *testing.T) {
	if os.Getenv("CRABBOX_ARGV_HELPER") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			input, _ := io.ReadAll(os.Stdin)
			cwd, _ := os.Getwd()
			_ = json.NewEncoder(os.Stdout).Encode(windowsArgvProcessResult{os.Args[i+1:], cwd, string(input), os.Getenv("CRABBOX_ARGV_VALUE")})
			fmt.Fprint(os.Stderr, "argv-helper-stderr")
			os.Exit(23)
		}
	}
	os.Exit(99)
}

func argvTestPowerShell(t *testing.T, encoded string) *exec.Cmd {
	t.Helper()
	name := "pwsh"
	if runtime.GOOS == "windows" {
		name = "powershell.exe"
	}
	powerShell, err := exec.LookPath(name)
	if err != nil {
		t.Skip("PowerShell not installed")
	}
	return exec.CommandContext(t.Context(), powerShell, "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-EncodedCommand", strings.TrimPrefix(encoded, powerShellEncodedCommandPrefix))
}

func TestWindowsNativeRemoteCommandProcessStreamsAndArgv(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	if runtime.GOOS != "windows" {
		// Exercise the Windows executable selection with the host-native helper.
		path := filepath.Join(workdir, "argv helper.exe")
		if err := os.Symlink(executable, path); err != nil {
			t.Fatal(err)
		}
		executable = path
	}
	want := windowsArgvFixture()
	args := append([]string{executable, "-test.run=^TestWindowsArgvHelperProcess$", "--"}, want...)
	command := windowsRemoteCommandWithEnvFiles(workdir, map[string]string{"CRABBOX_ARGV_HELPER": "1", "CRABBOX_ARGV_VALUE": `value"quoted`}, nil, args)
	// PowerShell 7 on non-Windows hosts can reproduce the 5.1 binder regression.
	command = PowershellCommand("$PSNativeCommandArgumentPassing = 'Legacy'\n" + decodePowerShellCommand(t, command))
	cmd := argvTestPowerShell(t, command)
	cmd.Stdin = strings.NewReader("stdin bytes\x00\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); exitCode(err) != 23 {
		t.Fatalf("exit=%v stdout=%s stderr=%s", err, &stdout, &stderr)
	}
	var got windowsArgvProcessResult
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decode output: %v: %s", err, &stdout)
	}
	if !reflect.DeepEqual(got.Args, want) || got.Input != "stdin bytes\x00\n" || got.Env != `value"quoted` {
		t.Fatalf("child received %#v; want args %#v, literal stdin and env", got, want)
	}
	resolved, _ := filepath.EvalSymlinks(workdir)
	if got.Cwd != resolved {
		t.Fatalf("cwd=%q, want %q", got.Cwd, resolved)
	}
	if stderr.String() != "argv-helper-stderr" {
		t.Fatalf("stderr=%q", &stderr)
	}
}

func TestWindowsRemoteRunScriptProcessArgv(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("requires Windows PowerShell -File argument parsing")
	}
	workdir := t.TempDir()
	path := filepath.Join(workdir, "script with spaces.ps1")
	if err := os.WriteFile(path, []byte("\xef\xbb\xbf[Console]::OutputEncoding = [Text.UTF8Encoding]::new($false); [Console]::Out.Write((ConvertTo-Json -Compress -InputObject @($args))); exit 19"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := windowsArgvFixture()
	command := windowsRemoteRunScriptCommandWithEnvFiles(workdir, nil, nil, &RunScriptSpec{RemotePath: path}, want)
	output, err := argvTestPowerShell(t, command).Output()
	if exitCode(err) != 19 {
		t.Fatalf("exit=%v output=%s", err, output)
	}
	var got []string
	if err := json.Unmarshal(output, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestWindowsNativeRemoteCommandInterpreterPaths(t *testing.T) {
	workdir := t.TempDir()
	commands := [][]string{{"Write-Output", "interpreter-ok"}}
	if runtime.GOOS == "windows" {
		commands = append(commands, []string{"cmd.exe", "/d", "/c", "echo interpreter-ok"})
		commands = append(commands, []string{"find.exe", `"interpreter-ok"`})
		path := filepath.Join(workdir, "script with spaces.cmd")
		if err := os.WriteFile(path, []byte("@echo interpreter-ok\r\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, []string{path})
	}
	for _, command := range commands {
		t.Run(command[0], func(t *testing.T) {
			encoded := windowsRemoteCommandWithEnvFiles(workdir, nil, nil, command)
			cmd := argvTestPowerShell(t, encoded)
			cmd.Stdin = strings.NewReader("interpreter-ok\n")
			output, err := cmd.CombinedOutput()
			if err != nil || strings.TrimSpace(string(output)) != "interpreter-ok" {
				t.Fatalf("output=%q err=%v", output, err)
			}
		})
	}
}
