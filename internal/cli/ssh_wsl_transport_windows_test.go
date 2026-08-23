//go:build windows

package cli

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const fakeWSLTransportHelper = "CRABBOX_FAKE_WSL_TRANSPORT_HELPER"

func init() {
	if os.Getenv(fakeWSLTransportHelper) != "1" {
		return
	}
	if strings.EqualFold(filepath.Base(os.Args[0]), "ssh.exe") {
		os.Exit(runFakeWSLTransportSSH(os.Args[1:]))
	}
	os.Exit(runFakeWSLTransport(os.Args[1:]))
}

func appendFakeWSLLog(line string) {
	file, err := os.OpenFile(os.Getenv("CRABBOX_FAKE_WSL_LOG"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	fmt.Fprintln(file, line)
	file.Close()
}

func requireFakeWSL(t *testing.T, ok bool, format string, args ...any) {
	t.Helper()
	if !ok {
		t.Fatalf(format, args...)
	}
}

func runFakeWSLTransportSSH(args []string) int {
	port := ""
	for index := range args {
		if args[index] == "-p" && index+1 < len(args) {
			port = args[index+1]
		}
	}
	input, _ := io.ReadAll(os.Stdin)
	appendFakeWSLLog(fmt.Sprintf("ssh:%s:%x", port, sha256.Sum256(input)))
	if port == "2222" {
		fmt.Print("first-attempt-noise")
		return 255
	}
	fmt.Print("second-attempt-ok")
	return 0
}

func runFakeWSLTransport(args []string) int {
	if len(args) < 3 || args[0] != "--exec" {
		return 90
	}
	root, mode := os.Getenv("CRABBOX_FAKE_WSL_ROOT"), os.Getenv("CRABBOX_FAKE_WSL_MODE")
	hostPath := func(path string) string {
		return filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(path, "/tmp/")))
	}
	switch {
	case args[1] == "sh" && args[2] == "-c" && len(args) == 7:
		dir := hostPath(args[5])
		if mode == "stage-timeout" {
			time.Sleep(30 * time.Second)
		}
		if mode == "collision" {
			_ = os.Mkdir(dir, 0o700)
			_ = os.WriteFile(filepath.Join(dir, "sentinel"), []byte("owned elsewhere"), 0o600)
			return 93
		}
		if mode == "stage-fail" {
			return 92
		}
		expected, err := strconv.Atoi(args[6])
		script, readErr := io.ReadAll(os.Stdin)
		if err != nil || readErr != nil || len(script) != expected {
			return 91
		}
		if os.Mkdir(dir, 0o700) != nil ||
			os.WriteFile(filepath.Join(dir, ".crabbox-owned"), nil, 0o600) != nil ||
			os.WriteFile(filepath.Join(dir, "script.sh"), script, 0o600) != nil {
			return 94
		}
		fmt.Printf("script=%x input=%x\n", sha256.Sum256(script), sha256.Sum256(nil))
		fmt.Fprintln(os.Stderr, "wsl-stderr-marker")
		if bytes.Contains(script, []byte("sleep-for-timeout")) {
			time.Sleep(30 * time.Second)
		}
		code := 0
		if bytes.Contains(script, []byte("exit-23")) {
			code = 23
		}
		if mode == "cleanup-timeout" {
			time.Sleep(30 * time.Second)
		}
		if mode == "cleanup-fail" {
			fmt.Fprintln(os.Stderr, "WSL2 command cleanup failed: exit 41")
			if code == 0 {
				code = 41
			}
			return code
		}
		if os.RemoveAll(dir) != nil {
			return 98
		}
		return code
	case args[1] == "sh" && args[2] == "-c" && len(args) == 6:
		dir := hostPath(args[5])
		if _, err := os.Stat(filepath.Join(dir, ".crabbox-owned")); err != nil {
			return 0
		}
		if mode == "fallback-cleanup-fail" {
			return 42
		}
		if os.RemoveAll(dir) != nil {
			return 98
		}
		return 0
	default:
		return 99
	}
}

func TestWSL2TransportStagesWithoutWindowsAutomount(t *testing.T) {
	executable, err := os.Executable()
	requireFakeWSL(t, err == nil, "executable: %v", err)
	payload, err := os.ReadFile(executable)
	requireFakeWSL(t, err == nil, "read executable: %v", err)
	powerShell, err := exec.LookPath("powershell.exe")
	requireFakeWSL(t, err == nil, "find powershell.exe: %v", err)

	binDir, windowsDir := t.TempDir(), t.TempDir()
	openSSHDir := filepath.Join(windowsDir, "System32", "OpenSSH")
	requireFakeWSL(t, os.MkdirAll(openSSHDir, 0o700) == nil, "create OpenSSH fixture")
	fakeWSL := filepath.Join(binDir, "wsl.exe")
	for _, path := range []string{fakeWSL, filepath.Join(openSSHDir, "ssh.exe")} {
		requireFakeWSL(t, os.WriteFile(path, payload, 0o755) == nil, "write %s", path)
	}
	root, logPath := t.TempDir(), filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("WINDIR", windowsDir)
	t.Setenv(fakeWSLTransportHelper, "1")
	t.Setenv("CRABBOX_FAKE_WSL_ROOT", root)
	t.Setenv("CRABBOX_FAKE_WSL_LOG", logPath)

	run := func(command string, input []byte) (string, string, error) {
		script := decodePowerShellCommand(t, command)
		fakeWSLQuote := psQuote(fakeWSL)
		script = strings.ReplaceAll(
			script,
			`[System.Diagnostics.ProcessStartInfo]::new("wsl.exe")`,
			`[System.Diagnostics.ProcessStartInfo]::new(`+fakeWSLQuote+`)`,
		)
		script = strings.ReplaceAll(script, `& wsl.exe`, `& `+fakeWSLQuote)
		for _, unqualified := range []string{
			`[System.Diagnostics.ProcessStartInfo]::new("wsl.exe")`,
			`& wsl.exe`,
		} {
			requireFakeWSL(t, !strings.Contains(script, unqualified), "transport retained unqualified executable %q", unqualified)
		}
		scriptPath := filepath.Join(t.TempDir(), "transport.ps1")
		mustWriteTestFile(t, scriptPath, script)
		cmd := exec.Command(powerShell, "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", scriptPath)
		cmd.Stdin = bytes.NewReader(input)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err
	}
	clearRoot := func() {
		entries, _ := filepath.Glob(filepath.Join(root, "*"))
		for _, entry := range entries {
			_ = os.RemoveAll(entry)
		}
	}
	assertEmpty := func() {
		entries, _ := os.ReadDir(root)
		requireFakeWSL(t, len(entries) == 0, "WSL transport residue=%v", entries)
	}

	control := []byte("control-script\n")
	stdout, stderr, err := run(wsl2StdinScriptCommandWithWaitTimeout(len(control), 5*time.Second), control)
	requireFakeWSL(t, err == nil && strings.Contains(stderr, "wsl-stderr-marker"), "control transport err=%v stdout=%q stderr=%q", err, stdout, stderr)
	for _, want := range []string{fmt.Sprintf("script=%x", sha256.Sum256(control)), fmt.Sprintf("input=%x", sha256.Sum256(nil))} {
		requireFakeWSL(t, strings.Contains(stdout, want), "stdout=%q missing %q", stdout, want)
	}
	assertEmpty()

	_, _, err = run(wsl2StdinScriptCommandWithWaitTimeout(len("exit-23"), 5*time.Second), []byte("exit-23"))
	requireFakeWSL(t, exitCode(err) == 23, "exit=%d err=%v want 23", exitCode(err), err)
	_, stderr, err = run(wsl2StdinScriptCommandWithWaitTimeout(len("sleep-for-timeout"), 250*time.Millisecond), []byte("sleep-for-timeout"))
	requireFakeWSL(t, err != nil && strings.Contains(stderr, "timed out"), "timeout err=%v stderr=%q", err, stderr)
	assertEmpty()

	_, _, err = run(wsl2StdinScriptCommandWithWaitTimeout(len(control)+1, 5*time.Second), control)
	requireFakeWSL(t, err != nil, "short framed control script succeeded")
	assertEmpty()

	t.Setenv("CRABBOX_FAKE_WSL_MODE", "stage-fail")
	_, _, err = run(wsl2StdinScriptCommandWithWaitTimeout(len("stage-fail"), 0), []byte("stage-fail"))
	requireFakeWSL(t, err != nil, "staging failure succeeded")
	assertEmpty()

	t.Setenv("CRABBOX_FAKE_WSL_MODE", "stage-timeout")
	_, stderr, err = run(wsl2StdinScriptCommandWithWaitTimeout(len(control), 250*time.Millisecond), control)
	requireFakeWSL(t, err != nil && strings.Contains(stderr, "timed out"), "staging timeout err=%v stderr=%q", err, stderr)
	assertEmpty()

	t.Setenv("CRABBOX_FAKE_WSL_MODE", "collision")
	_, _, err = run(wsl2StdinScriptCommandWithWaitTimeout(len("collision"), 0), []byte("collision"))
	requireFakeWSL(t, err != nil, "path collision succeeded")
	entries, _ := os.ReadDir(root)
	requireFakeWSL(t, len(entries) == 1, "collision path was cleaned: %v", entries)
	sentinel, sentinelErr := os.ReadFile(filepath.Join(root, entries[0].Name(), "sentinel"))
	requireFakeWSL(t, sentinelErr == nil && string(sentinel) == "owned elsewhere", "collision path was modified")
	clearRoot()

	t.Setenv("CRABBOX_FAKE_WSL_MODE", "cleanup-fail")
	_, stderr, err = run(wsl2StdinScriptCommandWithWaitTimeout(len("success"), 0), []byte("success"))
	requireFakeWSL(t, exitCode(err) == 41 && strings.Contains(stderr, "cleanup failed"),
		"cleanup-only failure err=%v stderr=%q", err, stderr)
	clearRoot()
	_, stderr, err = run(wsl2StdinScriptCommandWithWaitTimeout(len("exit-23"), 0), []byte("exit-23"))
	requireFakeWSL(t, exitCode(err) == 23 && strings.Contains(stderr, "cleanup failed"),
		"cleanup precedence err=%v stderr=%q", err, stderr)
	clearRoot()

	t.Setenv("CRABBOX_FAKE_WSL_MODE", "cleanup-timeout")
	_, stderr, err = run(wsl2StdinScriptCommandWithWaitTimeout(len("success"), 250*time.Millisecond), []byte("success"))
	requireFakeWSL(t, err != nil && strings.Contains(stderr, "timed out"),
		"cleanup timeout err=%v stderr=%q", err, stderr)
	clearRoot()

	t.Setenv("CRABBOX_FAKE_WSL_MODE", "fallback-cleanup-fail")
	_, stderr, err = run(wsl2StdinScriptCommandWithWaitTimeout(len("sleep-for-timeout"), 250*time.Millisecond), []byte("sleep-for-timeout"))
	requireFakeWSL(t, err != nil && strings.Contains(stderr, "cleanup failed: exit 42"),
		"fallback cleanup failure err=%v stderr=%q", err, stderr)
	clearRoot()

	t.Setenv("CRABBOX_FAKE_WSL_MODE", "")
	_ = os.WriteFile(logPath, nil, 0o600)
	out, err := runWSL2ControlScriptCombinedOutput(t.Context(), SSHTarget{
		User: "crabbox", Host: "example.test", Port: "2222", FallbackPorts: []string{"22"},
		TargetOS: targetWindows, WindowsMode: windowsModeWSL2, NoControlMaster: true,
	}, "control", 0, "1", "1")
	requireFakeWSL(t, err == nil && out == "second-attempt-ok", "fallback output=%q err=%v", out, err)
	logData, err := os.ReadFile(logPath)
	requireFakeWSL(t, err == nil, "read fallback log: %v", err)
	lines := strings.Split(strings.TrimSpace(string(logData)), "\n")
	requireFakeWSL(t, len(lines) == 2 && !strings.Contains(out, "first-attempt") &&
		strings.TrimPrefix(lines[0], "ssh:2222:") == strings.TrimPrefix(lines[1], "ssh:22:"),
		"fallback calls=%q output=%q", lines, out)
}
