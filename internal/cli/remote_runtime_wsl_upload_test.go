package cli

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestNativeWSLUploadCommand(t *testing.T) {
	for _, shell := range []wslStageShell{wslStageCMD, wslStagePowerShell} {
		for _, installer := range []string{"exit 0\n", strings.Repeat("#", wslStageMaxHelper-1) + "\n"} {
			command, err := nativeWSLUploadCommand(installer, 1024, time.Minute, shell)
			if err != nil || command == "" || len(command) >= wslStageLauncherCommandLimit {
				t.Fatalf("shell=%s installer bytes=%d command bytes=%d error=%v", shell, len(installer), len(command), err)
			}
		}
	}
	for _, timeout := range []time.Duration{time.Millisecond, 11 * time.Minute, nativeWSLUploadMaxWait} {
		if command, err := nativeWSLUploadCommand("exit 0\n", 1024, timeout, wslStageCMD); command == "" || err != nil {
			t.Fatalf("supported timeout %s: %v", timeout, err)
		}
	}
	if strings.Contains(wslPOSIXHelperBootstrap, "bash") || !strings.HasSuffix(wslPOSIXHelperBootstrap, `exec /usr/bin/env BASH_ENV=/dev/null ENV=/dev/null /bin/sh -c "$h" sh "$@"`) {
		t.Fatal("native upload did not select POSIX sh")
	}
	for _, tt := range []struct {
		name, installer string
		size            int64
		timeout         time.Duration
		shell           wslStageShell
	}{
		{"empty", "", 0, time.Second, wslStageCMD},
		{"long", strings.Repeat("x", wslStageMaxHelper+1), 0, time.Second, wslStageCMD},
		{"nul", "exit\x00", 0, time.Second, wslStageCMD},
		{"negative", "exit 0", -1, time.Second, wslStageCMD},
		{"large", "exit 0", wslStageMaxSize + 1, time.Second, wslStageCMD},
		{"unlimited", "exit 0", 0, 0, wslStageCMD},
		{"submillisecond", "exit 0", 0, time.Millisecond - time.Nanosecond, wslStageCMD},
		{"wait_overflow", "exit 0", 0, nativeWSLUploadMaxWait + time.Nanosecond, wslStageCMD},
		{"shell", "exit 0", 0, time.Second, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if command, err := nativeWSLUploadCommand(tt.installer, tt.size, tt.timeout, tt.shell); command != "" || err == nil {
				t.Fatalf("command=%q error=%v", command, err)
			}
		})
	}
}

// These substitutions exercise the framing algorithm using local PowerShell and
// POSIX sh. They do not simulate or qualify Windows OpenSSH or WSL pipe handles.
func portableNativeWSLUploadScript(t *testing.T, source string, preamble bool) string {
	t.Helper()
	windowsInput := `  if(!('Cbx.SshStdin' -as [type])){Add-Type -Name SshStdin -Namespace Cbx -MemberDefinition '[DllImport("kernel32.dll")]public static extern IntPtr GetStdHandle(int n);'}
  $h=[Microsoft.Win32.SafeHandles.SafeFileHandle]::new([Cbx.SshStdin]::GetStdHandle(-10),$false)
  $src=[IO.FileStream]::new($h,[IO.FileAccess]::Read,1,$true)`
	for _, replacement := range [][2]string{
		{windowsInput, `  $src=[Console]::OpenStandardInput()`},
		{`[Diagnostics.ProcessStartInfo]::new('wsl.exe')`, `[Diagnostics.ProcessStartInfo]::new('/usr/bin/env')`},
		{`'--exec "/usr/bin/env" `, `'`},
		{`$dst=[IO.FileStream]::new($p.StandardInput.BaseStream.SafeFileHandle,[IO.FileAccess]::Write,1,$false)`, `$dst=$p.StandardInput.BaseStream`},
	} {
		if strings.Count(source, replacement[0]) != 1 {
			t.Fatalf("portable fixture substitution missing or ambiguous: %s", replacement[0])
		}
		source = strings.Replace(source, replacement[0], replacement[1], 1)
	}
	if preamble {
		source = strings.Replace(source, `$enc=[Text.UTF8Encoding]::new($false)`, `$enc=[Text.UTF8Encoding]::new($true)`, 1)
	}
	return source
}

func runPortableNativeWSLUpload(t *testing.T, installer string, payload []byte, declared int64, timeout time.Duration, preamble bool) (int, []byte, []byte) {
	t.Helper()
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("local PowerShell Core unavailable")
	}
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("local POSIX shell unavailable")
	}
	script := portableNativeWSLUploadScript(t, nativeWSLUploadScript(len(installer), declared, timeout), preamble)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, pwsh, "-NoProfile", "-NonInteractive", "-Command", script)
	command.WaitDelay = time.Second
	command.Stdin = bytes.NewReader(append([]byte(installer), payload...))
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err = command.Run()
	if ctx.Err() != nil {
		t.Fatalf("portable upload exceeded outer fixture deadline: %v", err)
	}
	return exitCode(err), stdout.Bytes(), stderr.Bytes()
}

func TestNativeWSLUploadPortableBytesAndExit(t *testing.T) {
	payload := bytes.Repeat([]byte{0, 1, 13, 10, 128, 254, 255}, 73)
	installer := "# owned UTF-8 fixture: é\ndd bs=1 count=\"$1\" 2>/dev/null\nprintf 'owned stderr' >&2\nexit 23\n\n"
	for _, preamble := range []bool{false, true} {
		code, stdout, stderr := runPortableNativeWSLUpload(t, installer, payload, int64(len(payload)), 5*time.Second, preamble)
		if code != 23 || !bytes.Equal(stdout, payload) || string(stderr) != "owned stderr" {
			t.Fatalf("preamble=%t exit=%d stdout=%x stderr=%q", preamble, code, stdout, stderr)
		}
	}
}

func TestNativeWSLUploadPortableShortFrame(t *testing.T) {
	code, _, stderr := runPortableNativeWSLUpload(t, "dd bs=1 count=\"$1\" 2>/dev/null\n", []byte("short"), 12, 5*time.Second, false)
	if code != 125 || !strings.Contains(string(stderr), "WSL upload ended before its finite frame") {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
}

func TestNativeWSLUploadPortableTimeout(t *testing.T) {
	// The owned shell itself waits; it creates no child processes to clean up.
	code, _, stderr := runPortableNativeWSLUpload(t, "while :; do :; done\n", nil, 0, time.Second, false)
	if code != 124 || strings.TrimSpace(string(stderr)) != "WSL upload timed out" {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
}
