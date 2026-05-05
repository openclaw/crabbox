package cli

import (
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

func TestRecordRemoteCommandSupportsWindowsInteractiveTask(t *testing.T) {
	got := recordRemoteCommand(
		SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal},
		recordDesktopOptions{Duration: 4 * time.Second, FPS: 10, Size: "1024x768"},
	)
	for _, want := range []string{
		"CrabboxRecord-",
		"ffmpeg.exe",
		"gdigrab",
		"desktop",
		`"/IT"`,
		"windows.password",
		"ReadAllBytes",
		"} finally {",
		"schtasks.exe /Delete /TN $taskName /F",
		"Remove-Item -Force -LiteralPath $out, $done, $script",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("windows record command missing %q:\n%s", want, got)
		}
	}
}
