package cli

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

//go:embed testdata/windows-native-stdin.ps1
var windowsNativeStdinFixture string

func windowsNativeStdinTestProgram() string {
	// Only parameterize the frame length; execute the generated transport body.
	program := windowsPowerShellCopyExactInput("$destination", 9876543210)
	return "param([long]$Expected)\n" + strings.Replace(program, "$remaining = [Int64]9876543210", "$remaining = [Int64]$Expected", 1)
}

func windowsNativeStdinPendingTestProgram(t *testing.T) string {
	t.Helper()
	program := windowsNativeStdinTestProgram()
	read := `$read = $stdin.ReadAsync($buffer, 0, $readSize).GetAwaiter().GetResult()`
	if strings.Count(program, read) != 1 {
		t.Fatal("native read observation point changed")
	}
	return strings.Replace(program, read, `$pending = $stdin.ReadAsync($buffer, 0, $readSize)
if ($pending.IsCompleted) { throw "empty pipe read was not pending" }
[Console]::Out.WriteLine('READ_PENDING')
[Console]::Out.Flush()
$read = $pending.GetAwaiter().GetResult()`, 1)
}

// The historical ConsoleStream reader predates the current main FileStream
// helper. It is a standalone counterfactual, not a production fallback.
// A blocked process exits 124 via watchdog.
const windowsNativeStdinBaseline = `param([long]$Expected)
$stdin = [Console]::OpenStandardInput()
$remaining = $Expected
$buffer = New-Object byte[] 65536
while ($remaining -gt 0) {
	$readSize = [int][Math]::Min([Int64]$buffer.Length, $remaining)
	$read = $stdin.Read($buffer, 0, $readSize)
	if ($read -le 0) { throw "SSH stdin ended before the framed payload" }
	$destination.Write($buffer, 0, $read)
	$remaining -= $read
}
`

func TestWindowsNativeStdinProgramContract(t *testing.T) {
	program := windowsNativeStdinTestProgram()
	for name, script := range map[string]string{
		"dynamic": strings.TrimPrefix(program, "param([long]$Expected)\n"),
		"literal": windowsPowerShellCopyExactInput("$destination", 0),
	} {
		length := "$Expected"
		if name == "literal" {
			length = "0"
		}
		if !strings.HasPrefix(script, "$remaining = [Int64]"+length+"\nif ($remaining -gt 0) {\n") || !strings.HasSuffix(script, "}\n") {
			t.Fatalf("%s zero frame does not guard all stdin initialization", name)
		}
	}
	zero := decodePowerShellCommand(t, windowsPowerShellStdinScriptCommand(0))
	if !strings.Contains(zero, windowsPowerShellCopyExactInput("$scriptFile", 0)) {
		t.Fatal("zero script frame bypassed the shared input helper")
	}
	if !strings.Contains(windowsNativeStdinPendingTestProgram(t), "if ($pending.IsCompleted)") {
		t.Fatal("cancellation fixture does not require a pending read")
	}
	for _, want := range []string{
		`if (-not ("Cbx.SshStdin" -as [type]))`,
		`Add-Type -Name SshStdin -Namespace Cbx`,
		`SafeFileHandle]::new([Cbx.SshStdin]::GetStdHandle(-10), $false)`,
		`$stdin = $null`,
		`[IO.FileStream]::new($stdinHandle, [IO.FileAccess]::Read, 1, $true)`,
		`$remaining = [Int64]$Expected`,
		`[Math]::Min([Int64]$buffer.Length, $remaining)`,
		`$stdin.ReadAsync($buffer, 0, $readSize).GetAwaiter().GetResult()`,
		`if ($read -le 0) { throw "SSH stdin ended before the framed payload" }`,
		`$destination.Write($buffer, 0, $read)`,
		`$remaining -= $read`,
		`} finally {`, `if ($null -ne $stdin) { $stdin.Dispose() }`, `$stdinHandle.Dispose()`,
	} {
		if !strings.Contains(program, want) {
			t.Fatalf("native reader missing %q", want)
		}
	}
	for _, forbidden := range []string{"OpenStandardInput", "$stdin.Read(", "catch", "Start-Sleep", "ReadToEnd", "CopyTo"} {
		if strings.Contains(program, forbidden) {
			t.Fatalf("native reader contains %q", forbidden)
		}
	}
	for _, count := range []int{0, 4991, 65537, int(^uint(0) >> 1)} {
		command := windowsPowerShellStdinScriptCommand(count)
		t.Logf("frame size=%d encoded command length=%d", count, len(command))
		if len(command) >= 8191 {
			t.Fatal("native reader exceeds cmd.exe command limit")
		}
	}
	// Opt-in export lets a parent run this exact generated program on Windows
	// without installing Go or making a managed-run success claim.
	if dir := os.Getenv("CRABBOX_NATIVE_STDIN_PROOF_DIR"); dir != "" {
		writeWindowsNativeStdinFixture(t, dir, true)
	}
}

func writeWindowsNativeStdinFixture(t *testing.T, dir string, baseline bool) (string, string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"reader-fixed.ps1":         windowsNativeStdinTestProgram(),
		"reader-zero.ps1":          "param([long]$Expected)\n" + windowsPowerShellCopyExactInput("$destination", 0),
		"reader-pending.ps1":       windowsNativeStdinPendingTestProgram(t),
		"windows-native-stdin.ps1": windowsNativeStdinFixture,
	}
	if baseline {
		files["reader-baseline.ps1"] = windowsNativeStdinBaseline
	}
	for name, program := range files {
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := f.WriteString(program)
		closeErr := f.Close()
		if writeErr != nil || closeErr != nil {
			t.Fatalf("write fixture: %v %v", writeErr, closeErr)
		}
	}
	return filepath.Join(dir, "windows-native-stdin.ps1"), filepath.Join(dir, "reader-fixed.ps1")
}

func windowsNativeStdinPowerShell(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("requires native Windows asynchronous pipe handles; exported fixture can run separately")
	}
	path, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Fatal("native Windows PowerShell is unavailable")
	}
	return path
}

func TestWindowsNativeStdinRuntime(t *testing.T) {
	powerShell := windowsNativeStdinPowerShell(t)
	fixture, reader := writeWindowsNativeStdinFixture(t, t.TempDir(), false)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, powerShell, "-NoLogo", "-NoProfile", "-NonInteractive", "-File", fixture, "-ReaderPath", reader)
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err != nil || !bytes.Contains(out, []byte("NATIVE_STDIN_CONTRACT_COMPLETE")) {
		t.Fatalf("native pipe contract: %v\n%s", err, out)
	}
	for _, name := range []string{"empty", "zero-before-input", "partial", "4k-boundary", "8k-boundary", "64k-boundary", "large-binary", "short", "write-error"} {
		if !bytes.Contains(out, []byte("PASS "+name+"\r\n")) {
			t.Fatalf("missing native case %s:\n%s", name, out)
		}
	}
	t.Logf("%s", out)
}

func TestWindowsNativeStdinCancellation(t *testing.T) {
	powerShell := windowsNativeStdinPowerShell(t)
	dir := t.TempDir()
	fixture, _ := writeWindowsNativeStdinFixture(t, dir, false)
	// Signal only after the real async read is pending, so cancellation cannot
	// accidentally pass by killing PowerShell before it enters the reader.
	reader := filepath.Join(dir, "reader-pending.ps1")
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, powerShell, "-NoLogo", "-NoProfile", "-NonInteractive", "-File", fixture, "-ReaderPath", reader, "-Block")
	cmd.WaitDelay = time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	output := bufio.NewReader(stdout)
	ready, readyErr := output.ReadString('\n')
	line, readErr := output.ReadString('\n')
	cancel()
	if err := cmd.Wait(); err == nil || strings.TrimSpace(ready) != "READY" || readyErr != nil || strings.TrimSpace(line) != "READ_PENDING" || readErr != nil {
		t.Fatalf("cancelled native read: ready=%q line=%q read=%v wait=%v stderr=%s", ready, line, readErr, err, stderr.String())
	}
}

func TestWindowsNativeStdinAmbiguousDeliveryDoesNotReplay(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("local POSIX executable fixture; native handle tests run on Windows")
	}
	dir := t.TempDir()
	script := `#!/bin/sh
if [ "$1" = -E ]; then shift 2; fi
printf x >> "$CRABBOX_STDIN_TEST_DIR/calls"
printf '%s\n' "$@" > "$CRABBOX_STDIN_TEST_DIR/args"
cat
printf 'ambiguous connection closed\n' >&2
exit 255
`
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_STDIN_TEST_DIR", dir)
	target := SSHTarget{
		User: "alice", Host: "example.test", Port: "2222", FallbackPorts: []string{},
		Key: filepath.Join(dir, "lease-key"), CertificateFile: filepath.Join(dir, "lease-cert"),
		KnownHostsFile: filepath.Join(dir, "known_hosts"), HostKeyAlias: "lease-alias",
		TargetOS: targetWindows, WindowsMode: windowsModeNormal,
	}
	input := make([]byte, 65537)
	for i := range input {
		input[i] = byte(i % 251)
	}
	remote := windowsPowerShellStdinScriptCommand(len(input))
	var stdout, stderr bytes.Buffer
	err := runSSHInput(t.Context(), target, remote, bytes.NewReader(input), &stdout, &stderr)
	if exitCode(err) != 255 || !bytes.Equal(stdout.Bytes(), input) || stderr.String() != "ambiguous connection closed\n" {
		t.Fatalf("delivery exit=%d stdout bytes=%d stderr=%q", exitCode(err), stdout.Len(), stderr.String())
	}
	calls, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil || string(calls) != "x" {
		t.Fatalf("ambiguous delivery was replayed: calls=%q err=%v", calls, err)
	}
	args, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(args), "\n"), "\n")
	if want := sshArgsWithOptions(target, remote, "10", "3"); !reflect.DeepEqual(got, want) {
		t.Fatalf("native finite input changed SSH identity/options: got=%q want=%q", got, want)
	}
}
