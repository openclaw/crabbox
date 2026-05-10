package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type mediaPreviewResult struct {
	Input                    string  `json:"input"`
	Output                   string  `json:"output"`
	TrimmedVideoOutput       string  `json:"trimmedVideoOutput,omitempty"`
	SourceDurationSeconds    float64 `json:"sourceDurationSeconds"`
	PreviewStartSeconds      float64 `json:"previewStartSeconds"`
	PreviewDurationSeconds   float64 `json:"previewDurationSeconds"`
	TrimmedStaticEdges       bool    `json:"trimmedStaticEdges"`
	DetectedFreezeIntervals  int     `json:"detectedFreezeIntervals"`
	DetectedMotionWindowNote string  `json:"detectedMotionWindowNote,omitempty"`
	GifsicleOptimized        bool    `json:"gifsicleOptimized"`
}

type mediaContactSheetResult struct {
	Input                 string  `json:"input"`
	Output                string  `json:"output"`
	SourceDurationSeconds float64 `json:"sourceDurationSeconds"`
	Frames                int     `json:"frames"`
	Cols                  int     `json:"cols"`
	Rows                  int     `json:"rows"`
	Width                 int     `json:"width"`
}

type mediaPreviewOptions struct {
	Input              string
	Output             string
	TrimmedVideoOutput string
	Width              int
	FPS                float64
	TrimStatic         bool
	TrimPadding        time.Duration
	FreezeDuration     time.Duration
	FreezeNoise        string
	MinDuration        time.Duration
	GifsicleMode       string
	GifsicleLossy      int
	GifsicleGamma      float64
	JSON               bool
}

type mediaContactSheetOptions struct {
	Input  string
	Output string
	Frames int
	Cols   int
	Width  int
}

type mediaInterval struct {
	Start float64
	End   float64
}

const (
	defaultMediaPreviewWidth         = 1000
	defaultMediaPreviewFPS           = 24
	defaultMediaPreviewGifsicleMode  = "auto"
	defaultMediaPreviewGifsicleLossy = 65
	defaultMediaPreviewGifsicleGamma = 1.2
)

func defaultMediaPreviewOptions(input, output, trimmedVideoOutput string) mediaPreviewOptions {
	return mediaPreviewOptions{
		Input:              input,
		Output:             output,
		TrimmedVideoOutput: trimmedVideoOutput,
		Width:              defaultMediaPreviewWidth,
		FPS:                defaultMediaPreviewFPS,
		TrimStatic:         true,
		TrimPadding:        750 * time.Millisecond,
		FreezeDuration:     500 * time.Millisecond,
		FreezeNoise:        "-50dB",
		MinDuration:        1500 * time.Millisecond,
		GifsicleMode:       defaultMediaPreviewGifsicleMode,
		GifsicleLossy:      defaultMediaPreviewGifsicleLossy,
		GifsicleGamma:      defaultMediaPreviewGifsicleGamma,
	}
}

func (a App) mediaPreview(ctx context.Context, args []string) error {
	fs := newFlagSet("media preview", a.Stderr)
	input := fs.String("input", "", "input MP4/video path")
	output := fs.String("output", "", "output GIF preview path")
	trimmedVideoOutput := fs.String("trimmed-video-output", "", "optional output MP4 trimmed to the same motion window")
	width := fs.Int("width", defaultMediaPreviewWidth, "preview width in pixels")
	fps := fs.Float64("fps", defaultMediaPreviewFPS, "preview frames per second")
	trimStatic := fs.Bool("trim-static", true, "trim leading and trailing static regions before making the preview")
	noTrimStatic := fs.Bool("no-trim-static", false, "disable static-region trimming")
	trimPadding := fs.Duration("trim-padding", 750*time.Millisecond, "padding kept before first motion and after last motion")
	freezeDuration := fs.Duration("freeze-duration", 500*time.Millisecond, "minimum still duration for ffmpeg freezedetect")
	freezeNoise := fs.String("freeze-noise", "-50dB", "ffmpeg freezedetect noise threshold")
	minDuration := fs.Duration("min-duration", 1500*time.Millisecond, "minimum preview duration after trimming")
	gifsicleMode := fs.String("gifsicle", defaultMediaPreviewGifsicleMode, "gifsicle optimization: auto, off, or required")
	gifsicleLossy := fs.Int("gifsicle-lossy", defaultMediaPreviewGifsicleLossy, "gifsicle lossy compression value")
	gifsicleGamma := fs.Float64("gifsicle-gamma", defaultMediaPreviewGifsicleGamma, "gifsicle gamma value")
	jsonOut := fs.Bool("json", false, "print machine-readable result metadata")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *noTrimStatic {
		*trimStatic = false
	}
	opts := defaultMediaPreviewOptions(*input, *output, *trimmedVideoOutput)
	opts.Width = *width
	opts.FPS = *fps
	opts.TrimStatic = *trimStatic
	opts.TrimPadding = *trimPadding
	opts.FreezeDuration = *freezeDuration
	opts.FreezeNoise = *freezeNoise
	opts.MinDuration = *minDuration
	opts.GifsicleMode = *gifsicleMode
	opts.GifsicleLossy = *gifsicleLossy
	opts.GifsicleGamma = *gifsicleGamma
	opts.JSON = *jsonOut
	result, err := createMediaPreview(ctx, opts)
	if err != nil {
		return err
	}
	if opts.JSON {
		enc := json.NewEncoder(a.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}
	if result.TrimmedStaticEdges {
		fmt.Fprintf(a.Stdout, "wrote %s from %.3fs..%.3fs\n", result.Output, result.PreviewStartSeconds, result.PreviewStartSeconds+result.PreviewDurationSeconds)
	} else {
		fmt.Fprintf(a.Stdout, "wrote %s\n", result.Output)
	}
	if result.TrimmedVideoOutput != "" {
		fmt.Fprintf(a.Stdout, "wrote %s\n", result.TrimmedVideoOutput)
	}
	return nil
}

func createMediaPreview(ctx context.Context, opts mediaPreviewOptions) (mediaPreviewResult, error) {
	if strings.TrimSpace(opts.Input) == "" {
		return mediaPreviewResult{}, exit(2, "media preview requires --input")
	}
	if strings.TrimSpace(opts.Output) == "" {
		return mediaPreviewResult{}, exit(2, "media preview requires --output")
	}
	if opts.Width <= 0 {
		return mediaPreviewResult{}, exit(2, "media preview --width must be positive")
	}
	if opts.FPS <= 0 {
		return mediaPreviewResult{}, exit(2, "media preview --fps must be positive")
	}
	if opts.FreezeDuration <= 0 {
		return mediaPreviewResult{}, exit(2, "media preview --freeze-duration must be positive")
	}
	if opts.MinDuration < 0 {
		return mediaPreviewResult{}, exit(2, "media preview --min-duration must be non-negative")
	}
	gifsicleMode := strings.ToLower(strings.TrimSpace(opts.GifsicleMode))
	if gifsicleMode == "" {
		gifsicleMode = defaultMediaPreviewGifsicleMode
	}
	if gifsicleMode != "auto" && gifsicleMode != "off" && gifsicleMode != "required" {
		return mediaPreviewResult{}, exit(2, "media preview --gifsicle must be auto, off, or required")
	}
	if opts.GifsicleLossy < 0 {
		return mediaPreviewResult{}, exit(2, "media preview --gifsicle-lossy must be non-negative")
	}
	if opts.GifsicleGamma <= 0 {
		return mediaPreviewResult{}, exit(2, "media preview --gifsicle-gamma must be positive")
	}
	if _, err := os.Stat(opts.Input); err != nil {
		return mediaPreviewResult{}, exit(2, "read input video: %v", err)
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return mediaPreviewResult{}, exit(2, "ffmpeg is required for media preview: %v", err)
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		return mediaPreviewResult{}, exit(2, "ffprobe is required for media preview: %v", err)
	}

	duration, err := probeMediaDuration(ctx, opts.Input)
	if err != nil {
		return mediaPreviewResult{}, err
	}
	start := 0.0
	previewDuration := duration
	freezeCount := 0
	trimmed := false
	note := ""
	if opts.TrimStatic && duration > 0 {
		freezes, err := detectFreezeIntervals(ctx, opts.Input, duration, opts.FreezeNoise, opts.FreezeDuration)
		if err != nil {
			return mediaPreviewResult{}, err
		}
		freezeCount = len(freezes)
		window := motionPreviewWindow(duration, freezes, opts.TrimPadding, opts.MinDuration)
		start = window.Start
		previewDuration = window.End - window.Start
		trimmed = window.Trimmed
		note = window.Note
	}

	if err := os.MkdirAll(filepath.Dir(opts.Output), 0o755); err != nil && filepath.Dir(opts.Output) != "." {
		return mediaPreviewResult{}, exit(2, "create output directory: %v", err)
	}
	palette := strings.TrimSuffix(opts.Output, filepath.Ext(opts.Output)) + ".palette.png"
	defer os.Remove(palette)
	if err := runMediaCommand(ctx, "ffmpeg", previewPaletteArgs(opts.Input, palette, opts.Width, opts.FPS, start, previewDuration)...); err != nil {
		return mediaPreviewResult{}, err
	}
	if err := runMediaCommand(ctx, "ffmpeg", previewGIFArgs(opts.Input, palette, opts.Output, opts.Width, opts.FPS, start, previewDuration)...); err != nil {
		return mediaPreviewResult{}, err
	}
	optimized := false
	if gifsicleMode != "off" {
		optimized, err = optimizeGIF(ctx, opts.Output, opts.GifsicleLossy, opts.GifsicleGamma, gifsicleMode == "required")
		if err != nil {
			return mediaPreviewResult{}, err
		}
	}
	if opts.TrimmedVideoOutput != "" {
		if err := os.MkdirAll(filepath.Dir(opts.TrimmedVideoOutput), 0o755); err != nil && filepath.Dir(opts.TrimmedVideoOutput) != "." {
			return mediaPreviewResult{}, exit(2, "create trimmed video output directory: %v", err)
		}
		if err := runMediaCommand(ctx, "ffmpeg", trimmedVideoArgs(opts.Input, opts.TrimmedVideoOutput, start, previewDuration)...); err != nil {
			return mediaPreviewResult{}, err
		}
	}
	return mediaPreviewResult{
		Input:                    opts.Input,
		Output:                   opts.Output,
		TrimmedVideoOutput:       opts.TrimmedVideoOutput,
		SourceDurationSeconds:    roundMillis(duration),
		PreviewStartSeconds:      roundMillis(start),
		PreviewDurationSeconds:   roundMillis(previewDuration),
		TrimmedStaticEdges:       trimmed,
		DetectedFreezeIntervals:  freezeCount,
		DetectedMotionWindowNote: note,
		GifsicleOptimized:        optimized,
	}, nil
}

func createMediaContactSheet(ctx context.Context, opts mediaContactSheetOptions) (mediaContactSheetResult, error) {
	if strings.TrimSpace(opts.Input) == "" {
		return mediaContactSheetResult{}, exit(2, "media contact sheet requires input")
	}
	if strings.TrimSpace(opts.Output) == "" {
		return mediaContactSheetResult{}, exit(2, "media contact sheet requires output")
	}
	if opts.Frames <= 0 {
		opts.Frames = 5
	}
	if opts.Cols <= 0 || opts.Cols > opts.Frames {
		opts.Cols = opts.Frames
	}
	if opts.Width <= 0 {
		opts.Width = 320
	}
	if _, err := os.Stat(opts.Input); err != nil {
		return mediaContactSheetResult{}, exit(2, "read input video: %v", err)
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return mediaContactSheetResult{}, exit(2, "ffmpeg is required for media contact sheet: %v", err)
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		return mediaContactSheetResult{}, exit(2, "ffprobe is required for media contact sheet: %v", err)
	}
	duration, err := probeMediaDuration(ctx, opts.Input)
	if err != nil {
		return mediaContactSheetResult{}, err
	}
	rows := int(math.Ceil(float64(opts.Frames) / float64(opts.Cols)))
	if err := os.MkdirAll(filepath.Dir(opts.Output), 0o755); err != nil && filepath.Dir(opts.Output) != "." {
		return mediaContactSheetResult{}, exit(2, "create contact sheet directory: %v", err)
	}
	if err := runMediaCommand(ctx, "ffmpeg", contactSheetArgs(opts.Input, opts.Output, opts.Frames, opts.Cols, rows, opts.Width, duration)...); err != nil {
		return mediaContactSheetResult{}, err
	}
	return mediaContactSheetResult{
		Input:                 opts.Input,
		Output:                opts.Output,
		SourceDurationSeconds: roundMillis(duration),
		Frames:                opts.Frames,
		Cols:                  opts.Cols,
		Rows:                  rows,
		Width:                 opts.Width,
	}, nil
}

func probeMediaDuration(ctx context.Context, input string) (float64, error) {
	out, err := commandOutput(ctx, "ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", input)
	if err != nil {
		return 0, exit(2, "ffprobe duration failed: %v: %s", err, strings.TrimSpace(out))
	}
	duration, err := strconv.ParseFloat(strings.TrimSpace(out), 64)
	if err != nil || duration <= 0 {
		return 0, exit(2, "ffprobe returned invalid duration %q", strings.TrimSpace(out))
	}
	return duration, nil
}

func detectFreezeIntervals(ctx context.Context, input string, duration float64, noise string, freezeDuration time.Duration) ([]mediaInterval, error) {
	filter := fmt.Sprintf("freezedetect=n=%s:d=%.3f", noise, freezeDuration.Seconds())
	out, err := commandOutput(ctx, "ffmpeg", "-hide_banner", "-i", input, "-vf", filter, "-an", "-f", "null", "-")
	if err != nil {
		return nil, exit(2, "ffmpeg freezedetect failed: %v: %s", err, tailForError(out))
	}
	return parseFreezeIntervals(out, duration), nil
}

func previewPaletteArgs(input, palette string, width int, fps, start, duration float64) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-y"}
	args = appendTrimInputArgs(args, input, start, duration)
	args = append(args,
		"-vf", fmt.Sprintf("fps=%s,scale=%d:-1:flags=lanczos,palettegen=stats_mode=diff", formatMediaSeconds(fps), width),
		"-frames:v", "1",
		"-update", "1",
		palette,
	)
	return args
}

func previewGIFArgs(input, palette, output string, width int, fps, start, duration float64) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-y"}
	args = appendTrimInputArgs(args, input, start, duration)
	args = append(args,
		"-i", palette,
		"-lavfi", fmt.Sprintf("fps=%s,scale=iw*sar:ih,scale=%d:-1:flags=lanczos[x];[x][1:v]paletteuse=dither=floyd_steinberg", formatMediaSeconds(fps), width),
		"-loop", "0",
		output,
	)
	return args
}

func gifsicleOptimizeArgs(input, output string, lossy int, gamma float64) []string {
	return []string{
		"-O3",
		fmt.Sprintf("--gamma=%s", formatMediaSeconds(gamma)),
		fmt.Sprintf("--lossy=%d", lossy),
		input,
		"-o",
		output,
	}
}

func optimizeGIF(ctx context.Context, output string, lossy int, gamma float64, required bool) (bool, error) {
	if _, err := exec.LookPath("gifsicle"); err != nil {
		if required {
			return false, exit(2, "gifsicle is required for media preview: %v", err)
		}
		return false, nil
	}
	temp := strings.TrimSuffix(output, filepath.Ext(output)) + ".optimized.gif"
	defer os.Remove(temp)
	if err := runMediaCommand(ctx, "gifsicle", gifsicleOptimizeArgs(output, temp, lossy, gamma)...); err != nil {
		return false, err
	}
	if err := os.Rename(temp, output); err != nil {
		return false, exit(2, "replace optimized GIF: %v", err)
	}
	return true, nil
}

func trimmedVideoArgs(input, output string, start, duration float64) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-y"}
	args = appendTrimInputArgs(args, input, start, duration)
	args = append(args,
		"-an",
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-pix_fmt", "yuv420p",
		"-movflags", "+faststart",
		output,
	)
	return args
}

func contactSheetArgs(input, output string, frames, cols, rows, width int, duration float64) []string {
	sampleFPS := 1.0
	if duration > 0 {
		sampleFPS = float64(frames) / duration
	}
	filter := fmt.Sprintf("fps=%s,scale=%d:-1:flags=lanczos,tile=%dx%d:padding=4:margin=4:color=black", formatMediaSeconds(sampleFPS), width, cols, rows)
	return []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-i", input,
		"-vf", filter,
		"-frames:v", "1",
		output,
	}
}

func contactSheetPathForVideo(path string) string {
	ext := filepath.Ext(path)
	if ext == "" {
		return path + ".contact.png"
	}
	return strings.TrimSuffix(path, ext) + ".contact.png"
}

func appendTrimInputArgs(args []string, input string, start, duration float64) []string {
	if start > 0 {
		args = append(args, "-ss", formatMediaSeconds(start))
	}
	if duration > 0 {
		args = append(args, "-t", formatMediaSeconds(duration))
	}
	return append(args, "-i", input)
}

func runMediaCommand(ctx context.Context, name string, args ...string) error {
	out, err := commandOutput(ctx, name, args...)
	if err != nil {
		return exit(2, "%s failed: %v: %s", name, err, tailForError(out))
	}
	return nil
}

func commandOutput(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

func commandOutputWithEnv(ctx context.Context, env []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

func tailForError(text string) string {
	text = strings.TrimSpace(text)
	const limit = 4096
	if len(text) <= limit {
		return text
	}
	return text[len(text)-limit:]
}

func parseFreezeIntervals(text string, duration float64) []mediaInterval {
	startRE := regexp.MustCompile(`freeze_start:\s*([0-9]+(?:\.[0-9]+)?)`)
	endRE := regexp.MustCompile(`freeze_end:\s*([0-9]+(?:\.[0-9]+)?)`)
	var intervals []mediaInterval
	var current *float64
	for _, line := range strings.Split(text, "\n") {
		if match := startRE.FindStringSubmatch(line); len(match) == 2 {
			value, _ := strconv.ParseFloat(match[1], 64)
			current = &value
		}
		if match := endRE.FindStringSubmatch(line); len(match) == 2 && current != nil {
			value, _ := strconv.ParseFloat(match[1], 64)
			intervals = append(intervals, mediaInterval{Start: *current, End: value})
			current = nil
		}
	}
	if current != nil && duration > *current {
		intervals = append(intervals, mediaInterval{Start: *current, End: duration})
	}
	return normalizeIntervals(intervals, duration)
}

func normalizeIntervals(intervals []mediaInterval, duration float64) []mediaInterval {
	clean := make([]mediaInterval, 0, len(intervals))
	for _, interval := range intervals {
		start := math.Max(0, math.Min(duration, interval.Start))
		end := math.Max(0, math.Min(duration, interval.End))
		if end > start {
			clean = append(clean, mediaInterval{Start: start, End: end})
		}
	}
	sort.Slice(clean, func(i, j int) bool {
		if clean[i].Start == clean[j].Start {
			return clean[i].End < clean[j].End
		}
		return clean[i].Start < clean[j].Start
	})
	merged := make([]mediaInterval, 0, len(clean))
	for _, interval := range clean {
		if len(merged) == 0 || interval.Start > merged[len(merged)-1].End {
			merged = append(merged, interval)
			continue
		}
		if interval.End > merged[len(merged)-1].End {
			merged[len(merged)-1].End = interval.End
		}
	}
	return merged
}

type motionWindow struct {
	Start   float64
	End     float64
	Trimmed bool
	Note    string
}

func motionPreviewWindow(duration float64, freezes []mediaInterval, padding, minDuration time.Duration) motionWindow {
	if duration <= 0 {
		return motionWindow{Start: 0, End: 0, Note: "invalid-duration"}
	}
	freezes = normalizeIntervals(freezes, duration)
	active := nonFrozenIntervals(duration, freezes)
	if len(active) == 0 {
		return motionWindow{Start: 0, End: duration, Note: "no-motion-detected"}
	}
	start := active[0].Start - padding.Seconds()
	end := active[len(active)-1].End + padding.Seconds()
	start = math.Max(0, start)
	end = math.Min(duration, end)
	minSeconds := minDuration.Seconds()
	if minSeconds > 0 && end-start < minSeconds {
		center := (start + end) / 2
		start = math.Max(0, center-minSeconds/2)
		end = math.Min(duration, start+minSeconds)
		start = math.Max(0, end-minSeconds)
	}
	trimmed := start > 0.05 || duration-end > 0.05
	return motionWindow{Start: start, End: end, Trimmed: trimmed}
}

func nonFrozenIntervals(duration float64, freezes []mediaInterval) []mediaInterval {
	var active []mediaInterval
	pos := 0.0
	for _, frozen := range freezes {
		if frozen.Start > pos {
			active = append(active, mediaInterval{Start: pos, End: frozen.Start})
		}
		if frozen.End > pos {
			pos = frozen.End
		}
	}
	if pos < duration {
		active = append(active, mediaInterval{Start: pos, End: duration})
	}
	return active
}

func formatMediaSeconds(value float64) string {
	return strconv.FormatFloat(value, 'f', 3, 64)
}

func roundMillis(value float64) float64 {
	return math.Round(value*1000) / 1000
}
