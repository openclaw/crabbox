package cli

import (
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
