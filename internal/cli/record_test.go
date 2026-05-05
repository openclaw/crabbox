package cli

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestDefaultRecordingPath(t *testing.T) {
	if got := defaultRecordingPath("cbx_123", "Blue Lobster"); got != "crabbox-blue-lobster-recording.mp4" {
		t.Fatalf("path=%q", got)
	}
	if got := defaultRecordingPath("cbx_123", ""); got != "crabbox-cbx-123-recording.mp4" {
		t.Fatalf("fallback path=%q", got)
	}
}

func TestRecordWhileCommandArgsOnlyStripsPositionalID(t *testing.T) {
	if got := recordWhileCommandArgs([]string{"test", "-f", "foo"}, "test", false); strings.Join(got, " ") != "test -f foo" {
		t.Fatalf("flag id should not strip driver command, got %q", got)
	}
	if got := recordWhileCommandArgs([]string{"test", "driver"}, "test", true); strings.Join(got, " ") != "driver" {
		t.Fatalf("positional id should be stripped, got %q", got)
	}
}

func TestRecordDesktopControlTargetUsesWindowsConsoleForWSL2(t *testing.T) {
	target := recordDesktopControlTarget(SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2})
	if target.WindowsMode != windowsModeNormal {
		t.Fatalf("record desktop target should use Windows console mode, got %q", target.WindowsMode)
	}
}

func TestRecordRemoteCommandUsesFFmpegX11Grab(t *testing.T) {
	got := recordRemoteCommand(SSHTarget{TargetOS: targetLinux}, recordDesktopOptions{
		Duration: 3 * time.Second,
		FPS:      12,
		Size:     "1280x720",
	})
	for _, want := range []string{
		`DISPLAY="${DISPLAY:-:99}"`,
		"command -v ffmpeg",
		"-f x11grab",
		"-video_size '1280x720'",
		"-framerate 12",
		"-t 3",
		"-movflags frag_keyframe+empty_moov",
		"-f mp4 pipe:1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("record command missing %q:\n%s", want, got)
		}
	}
}

func TestRecordRemoteUntilStopCommandUsesStopFile(t *testing.T) {
	got := recordRemoteUntilStopCommand(
		SSHTarget{TargetOS: targetLinux},
		recordDesktopOptions{Duration: 30 * time.Second, FPS: 8, Size: "auto"},
		"/tmp/out.mp4",
		"/tmp/stop",
		"/tmp/ready",
	)
	for _, want := range []string{
		"out='/tmp/out.mp4'",
		"stop='/tmp/stop'",
		"ready='/tmp/ready'",
		"-f x11grab",
		"-framerate 8",
		"-t 45",
		`printf ready > "$ready"`,
		`if [ -f "$stop" ]`,
		`kill -INT "$pid"`,
		`cat "$out"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("record until-stop command missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "-video_size") {
		t.Fatalf("auto size should omit -video_size:\n%s", got)
	}
}

func TestRecordRemoteCommandSupportsWindowsInteractiveTask(t *testing.T) {
	for _, target := range []SSHTarget{
		{TargetOS: targetWindows, WindowsMode: windowsModeNormal},
		{TargetOS: targetWindows, WindowsMode: windowsModeWSL2},
	} {
		got := recordRemoteCommand(
			target,
			recordDesktopOptions{Duration: 4 * time.Second, FPS: 10, Size: "1024x768"},
		)
		assertWindowsRecordCommand(t, got)
	}
}

func assertWindowsRecordCommand(t *testing.T, got string) {
	t.Helper()
	for _, want := range []string{
		"CrabboxRecord-",
		"ffmpeg.exe",
		"gdigrab",
		"desktop",
		`"/IT"`,
		"windows.password",
		"ReadAllBytes",
		"-PassThru",
		"ExitCode",
		"ffmpeg.exe produced no recording",
		"} finally {",
		"schtasks.exe /Delete /TN $taskName /F",
		"Remove-Item -Force -LiteralPath $out, $done, $script",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("windows record command missing %q:\n%s", want, got)
		}
	}
}

func TestRunRecordWhileLocalCommandTimesOut(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep command unavailable")
	}
	start := time.Now()
	err := runRecordWhileLocalCommand(t.Context(), recordWhileCommandOptions{
		Command: []string{"sleep", "5"},
		Timeout: 100 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out after 100ms") {
		t.Fatalf("timeout error=%v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("driver timeout took too long: %s", elapsed)
	}
}
