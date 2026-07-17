package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestParseFreezeIntervalsClosesTrailingFreeze(t *testing.T) {
	input := `
[freezedetect @ 0x1] freeze_start: 0
[freezedetect @ 0x1] freeze_duration: 1.2
[freezedetect @ 0x1] freeze_end: 1.2
[freezedetect @ 0x1] freeze_start: 8.5
`
	got := parseFreezeIntervals(input, 10)
	want := []mediaInterval{{Start: 0, End: 1.2}, {Start: 8.5, End: 10}}
	if len(got) != len(want) {
		t.Fatalf("interval count=%d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("interval[%d]=%#v, want %#v", i, got[i], want[i])
		}
	}
}

func TestMotionPreviewWindowTrimsStaticEdges(t *testing.T) {
	got := motionPreviewWindow(10, []mediaInterval{
		{Start: 0, End: 2},
		{Start: 7, End: 10},
	}, 500*time.Millisecond, 1500*time.Millisecond)
	if !got.Trimmed {
		t.Fatalf("expected trimmed window: %#v", got)
	}
	if got.Start != 1.5 || got.End != 7.5 {
		t.Fatalf("window=%#v, want 1.5..7.5", got)
	}
}

func TestMotionPreviewWindowKeepsFullVideoWhenNoMotionDetected(t *testing.T) {
	got := motionPreviewWindow(10, []mediaInterval{{Start: 0, End: 10}}, 500*time.Millisecond, 1500*time.Millisecond)
	if got.Trimmed || got.Start != 0 || got.End != 10 || got.Note != "no-motion-detected" {
		t.Fatalf("window=%#v, want full no-motion window", got)
	}
}

func TestPreviewCommandsUsePaletteGIFAndTrimWindow(t *testing.T) {
	palette := strings.Join(previewPaletteArgs("desktop.mp4", "palette.png", 640, 4, 1.25, 6.5), " ")
	for _, want := range []string{
		"-ss 1.250",
		"-t 6.500",
		"-i desktop.mp4",
		"fps=4.000,scale=640:-1:flags=lanczos,palettegen=stats_mode=diff",
		"-frames:v 1",
		"-update 1",
		"palette.png",
	} {
		if !strings.Contains(palette, want) {
			t.Fatalf("palette args missing %q:\n%s", want, palette)
		}
	}

	gif := strings.Join(previewGIFArgs("desktop.mp4", "palette.png", "preview.gif", 640, 4, 1.25, 6.5), " ")
	for _, want := range []string{
		"-ss 1.250",
		"-t 6.500",
		"-i desktop.mp4",
		"-i palette.png",
		"scale=iw*sar:ih,scale=640:-1:flags=lanczos",
		"paletteuse=dither=floyd_steinberg",
		"-loop 0",
		"preview.gif",
	} {
		if !strings.Contains(gif, want) {
			t.Fatalf("gif args missing %q:\n%s", want, gif)
		}
	}
}

func TestGifsicleOptimizeArgsUseHQDefaults(t *testing.T) {
	got := strings.Join(gifsicleOptimizeArgs("preview.gif", "preview.optimized.gif", 65, 1.2), " ")
	for _, want := range []string{
		"-O3",
		"--gamma=1.200",
		"--lossy=65",
		"preview.gif",
		"-o preview.optimized.gif",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("gifsicle args missing %q:\n%s", want, got)
		}
	}
}

func TestTrimmedVideoCommandUsesSameWindow(t *testing.T) {
	got := strings.Join(trimmedVideoArgs("desktop.mp4", "change.mp4", 2, 4), " ")
	for _, want := range []string{
		"-ss 2.000",
		"-t 4.000",
		"-i desktop.mp4",
		"-c:v libx264",
		"-pix_fmt yuv420p",
		"-movflags +faststart",
		"change.mp4",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("trimmed video args missing %q:\n%s", want, got)
		}
	}
}

func TestContactSheetCommandSamplesAndTilesFrames(t *testing.T) {
	got := strings.Join(contactSheetArgs("desktop.mp4", "desktop.contact.png", 5, 5, 1, 320, 5), " ")
	for _, want := range []string{
		"-i desktop.mp4",
		"fps=1.000,scale=320:-1:flags=lanczos,tile=5x1:padding=4:margin=4:color=black",
		"-frames:v 1",
		"desktop.contact.png",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("contact sheet args missing %q:\n%s", want, got)
		}
	}
}

func TestContactSheetPathForVideo(t *testing.T) {
	tests := map[string]string{
		"screen.mp4":       "screen.contact.png",
		"/tmp/screen.webm": "/tmp/screen.contact.png",
		"screen":           "screen.contact.png",
	}
	for input, want := range tests {
		if got := contactSheetPathForVideo(input); got != want {
			t.Fatalf("contactSheetPathForVideo(%q)=%q want %q", input, got, want)
		}
	}
}

func TestMediaContactSheetStripsTargetDeniedEnvironment(t *testing.T) {
	input, logPath := installFakeMediaTools(t)
	output := filepath.Join(t.TempDir(), "contact.png")
	if _, err := createMediaContactSheet(context.Background(), mediaContactSheetOptions{
		Input:            input,
		Output:           output,
		ChildEnvDenylist: []string{"TEST_MEDIA_DESKTOP_SECRET"},
		Frames:           2,
		Cols:             2,
		Width:            160,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(output); err != nil {
		t.Fatalf("contact sheet output: %v", err)
	}
	assertFakeMediaToolLog(t, logPath, "ffprobe", "ffmpeg")
}

func TestMediaGIFStripsTargetDeniedEnvironment(t *testing.T) {
	input, logPath := installFakeMediaTools(t)
	output := filepath.Join(t.TempDir(), "preview.gif")
	opts := defaultMediaPreviewOptions(input, output, "")
	opts.ChildEnvDenylist = []string{"test_media_desktop_secret"}
	opts.TrimStatic = false
	opts.GifsicleMode = "required"
	result, err := createMediaPreview(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !result.GifsicleOptimized {
		t.Fatal("expected fake gifsicle optimization")
	}
	if _, err := os.Stat(output); err != nil {
		t.Fatalf("GIF output: %v", err)
	}
	assertFakeMediaToolLog(t, logPath, "ffprobe", "ffmpeg", "gifsicle")
}

func TestStandaloneMediaGIFCommandsStripConfiguredExternalDesktopSecret(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(App, context.Context, []string) error
	}{
		{
			name: "media-preview",
			run: func(app App, ctx context.Context, args []string) error {
				return app.mediaPreview(ctx, args)
			},
		},
		{
			name: "artifacts-gif",
			run: func(app App, ctx context.Context, args []string) error {
				return app.artifactsGif(ctx, args)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, name := range []string{
				"CRABBOX_PROVIDER",
				"CRABBOX_TARGET",
				"CRABBOX_TARGET_OS",
				"CRABBOX_EXTERNAL_COMMAND",
				"CRABBOX_EXTERNAL_ROUTING_FILE",
				"CRABBOX_EXTERNAL_DESKTOP_USERNAME",
				"CRABBOX_EXTERNAL_DESKTOP_PASSWORD_ENV",
			} {
				t.Setenv(name, "")
			}
			input, logPath := installFakeMediaTools(t)
			output := filepath.Join(t.TempDir(), "preview.gif")
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			config := `provider: external
target: macos
external:
  connection:
    desktop:
      username: screen-user
      passwordEnv: TEST_MEDIA_DESKTOP_SECRET
`
			if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CRABBOX_CONFIG", configPath)

			var stdout, stderr bytes.Buffer
			app := App{Stdout: &stdout, Stderr: &stderr}
			err := test.run(app, context.Background(), []string{
				"--input", input,
				"--output", output,
				"--no-trim-static",
				"--gifsicle", "required",
			})
			if err != nil {
				t.Fatalf("command failed: %v\nstderr: %s", err, stderr.String())
			}
			if _, err := os.Stat(output); err != nil {
				t.Fatalf("GIF output: %v", err)
			}
			assertFakeMediaToolLog(t, logPath, "ffprobe", "ffmpeg", "gifsicle")
		})
	}
}

func installFakeMediaTools(t *testing.T) (string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake media tools use POSIX shell")
	}
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
set -eu
if [ "${TEST_MEDIA_DESKTOP_SECRET+x}" = x ]; then
  echo "desktop secret leaked to $(basename "$0")" >&2
  exit 91
fi
if [ "${CRABBOX_TEST_MEDIA_KEEP:-}" != keep ]; then
  echo "unrelated environment missing from $(basename "$0")" >&2
  exit 92
fi
tool=$(basename "$0")
printf '%s\n' "$tool" >> "$CRABBOX_TEST_MEDIA_LOG"
case "$tool" in
  ffprobe)
    printf '2.000\n'
    ;;
  ffmpeg)
    output=
    for value do output=$value; done
    if [ -n "$output" ] && [ "$output" != - ]; then : > "$output"; fi
    ;;
  gifsicle)
    output=
    while [ "$#" -gt 0 ]; do
      if [ "$1" = -o ]; then shift; output=$1; break; fi
      shift
    done
    [ -n "$output" ]
    : > "$output"
    ;;
esac
`
	for _, name := range []string{"ffprobe", "ffmpeg", "gifsicle"} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	logPath := filepath.Join(dir, "tools.log")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TEST_MEDIA_DESKTOP_SECRET", "do-not-inherit")
	t.Setenv("CRABBOX_TEST_MEDIA_KEEP", "keep")
	t.Setenv("CRABBOX_TEST_MEDIA_LOG", logPath)
	input := filepath.Join(dir, "input.mp4")
	if err := os.WriteFile(input, []byte("fake video"), 0o600); err != nil {
		t.Fatal(err)
	}
	return input, logPath
}

func assertFakeMediaToolLog(t *testing.T, path string, tools ...string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	for _, tool := range tools {
		if !strings.Contains(log, tool+"\n") {
			t.Fatalf("tool log missing %s:\n%s", tool, log)
		}
	}
}
