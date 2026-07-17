package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type contactSheetFlagValues struct {
	Enabled *bool
	Skip    *bool
	Path    *string
	Frames  *int
	Cols    *int
	Width   *int
}

type desktopPublishFlagValues struct {
	PR          *int
	Dir         *string
	Storage     *string
	Bucket      *string
	Prefix      *string
	BaseURL     *string
	Repo        *string
	Template    *string
	Summary     *string
	SummaryFile *string
	DryRun      *bool
	NoComment   *bool
	Explicit    map[string]bool
}

type desktopProofMetadata struct {
	CreatedAt      string   `json:"createdAt"`
	Version        string   `json:"crabboxVersion"`
	LeaseID        string   `json:"leaseId,omitempty"`
	Slug           string   `json:"slug,omitempty"`
	Provider       string   `json:"provider,omitempty"`
	Network        string   `json:"network,omitempty"`
	TargetOS       string   `json:"targetOS,omitempty"`
	Command        []string `json:"command,omitempty"`
	TerminalCols   int      `json:"terminalCols,omitempty"`
	TerminalRows   int      `json:"terminalRows,omitempty"`
	TerminalSixel  bool     `json:"terminalSixel,omitempty"`
	RecordDuration string   `json:"recordDuration,omitempty"`
	RecordFPS      float64  `json:"recordFps,omitempty"`
}

func registerContactSheetFlags(fs *flag.FlagSet) contactSheetFlagValues {
	return contactSheetFlagValues{
		Enabled: fs.Bool("contact-sheet", true, "create a sampled contact sheet PNG next to recorded video"),
		Skip:    fs.Bool("no-contact-sheet", false, "skip contact sheet generation"),
		Path:    fs.String("contact-sheet-output", "", "contact sheet PNG output path"),
		Frames:  fs.Int("contact-sheet-frames", 5, "number of sampled frames in the contact sheet"),
		Cols:    fs.Int("contact-sheet-cols", 5, "contact sheet columns"),
		Width:   fs.Int("contact-sheet-width", 320, "width of each contact sheet tile"),
	}
}

func registerDesktopPublishFlags(fs *flag.FlagSet) desktopPublishFlagValues {
	return desktopPublishFlagValues{
		PR:          fs.Int("publish-pr", 0, "publish the proof bundle as a GitHub PR comment"),
		Dir:         fs.String("publish-dir", "", "artifact directory to publish; defaults to the record output directory"),
		Storage:     fs.String("publish-storage", "auto", "artifact storage backend: auto, broker, local, s3, cloudflare, or r2"),
		Bucket:      fs.String("publish-bucket", "", "artifact storage bucket"),
		Prefix:      fs.String("publish-prefix", "", "artifact object prefix"),
		BaseURL:     fs.String("publish-base-url", "", "public base URL for inline-ready asset links"),
		Repo:        fs.String("publish-repo", "", "GitHub repository slug for gh, e.g. openclaw/crabbox"),
		Template:    fs.String("publish-template", "openclaw", "comment template: openclaw or mantis"),
		Summary:     fs.String("publish-summary", "", "summary text for the PR comment"),
		SummaryFile: fs.String("publish-summary-file", "", "summary markdown file for the PR comment"),
		DryRun:      fs.Bool("publish-dry-run", false, "write publish markdown without upload/comment side effects"),
		NoComment:   fs.Bool("publish-no-comment", false, "upload/write markdown but skip the GitHub PR comment"),
	}
}

func markDesktopPublishExplicitFlags(fs *flag.FlagSet, flags *desktopPublishFlagValues) {
	if flags == nil {
		return
	}
	flags.Explicit = map[string]bool{}
	fs.Visit(func(f *flag.Flag) {
		if strings.HasPrefix(f.Name, "publish-") {
			flags.Explicit[strings.TrimPrefix(f.Name, "publish-")] = true
		}
	})
}

func validateContactSheetFlags(command string, flags contactSheetFlagValues) error {
	if flags.Frames != nil && *flags.Frames <= 0 {
		return exit(2, "%s --contact-sheet-frames must be positive", command)
	}
	if flags.Cols != nil && *flags.Cols <= 0 {
		return exit(2, "%s --contact-sheet-cols must be positive", command)
	}
	if flags.Width != nil && *flags.Width <= 0 {
		return exit(2, "%s --contact-sheet-width must be positive", command)
	}
	return nil
}

func contactSheetEnabled(flags contactSheetFlagValues) bool {
	if flags.Skip != nil && *flags.Skip {
		return false
	}
	return flags.Enabled == nil || *flags.Enabled
}

func writeContactSheetForVideo(ctx context.Context, videoPath string, flags contactSheetFlagValues, childEnvDenylist []string) (string, error) {
	if !contactSheetEnabled(flags) {
		return "", nil
	}
	output := ""
	if flags.Path != nil {
		output = strings.TrimSpace(*flags.Path)
	}
	if output == "" {
		output = contactSheetPathForVideo(videoPath)
	}
	_, err := createMediaContactSheet(ctx, mediaContactSheetOptions{
		Input:            videoPath,
		Output:           output,
		ChildEnvDenylist: childEnvDenylist,
		Frames:           valueOrDefault(flags.Frames, 5),
		Cols:             valueOrDefault(flags.Cols, 5),
		Width:            valueOrDefault(flags.Width, 320),
	})
	if err != nil {
		return "", err
	}
	return output, nil
}

func printContactSheetWarning(out io.Writer, err error) {
	if err == nil {
		return
	}
	fmt.Fprintf(out, "warning: contact-sheet skipped: %v\n", err)
}

func valueOrDefault(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}

func publishOptionsFromDesktopFlags(dir string, flags desktopPublishFlagValues) (artifactPublishOptions, bool, error) {
	if flags.PR == nil || *flags.PR <= 0 {
		return artifactPublishOptions{}, false, nil
	}
	publishDir := strings.TrimSpace(dir)
	if flags.Dir != nil && strings.TrimSpace(*flags.Dir) != "" {
		publishDir = strings.TrimSpace(*flags.Dir)
	}
	if publishDir == "" || publishDir == "." {
		return artifactPublishOptions{}, false, exit(2, "desktop publish requires --publish-dir or a --record path inside an artifact directory")
	}
	publishArgs := []string{
		"--dir", publishDir,
		"--pr", strconv.Itoa(*flags.PR),
	}
	appendPublishString := func(flagName string, value *string) {
		if value != nil && strings.TrimSpace(*value) != "" {
			publishArgs = append(publishArgs, "--"+flagName, *value)
		}
	}
	if flags.Explicit != nil && flags.Explicit["storage"] {
		appendPublishString("storage", flags.Storage)
	}
	appendPublishString("bucket", flags.Bucket)
	appendPublishString("prefix", flags.Prefix)
	appendPublishString("base-url", flags.BaseURL)
	appendPublishString("repo", flags.Repo)
	appendPublishString("template", flags.Template)
	appendPublishString("summary", flags.Summary)
	appendPublishString("summary-file", flags.SummaryFile)
	if flags.DryRun != nil && *flags.DryRun {
		publishArgs = append(publishArgs, "--dry-run")
	}
	if flags.NoComment != nil && *flags.NoComment {
		publishArgs = append(publishArgs, "--no-comment")
	}
	opts, err := parseArtifactPublishOptions(publishArgs, os.Stderr)
	if err != nil {
		return artifactPublishOptions{}, false, err
	}
	return opts, true, nil
}

func writeDesktopRecorderDiagnostics(ctx context.Context, target SSHTarget, outputPath string) error {
	if strings.TrimSpace(outputPath) == "" {
		return nil
	}
	var b strings.Builder
	b.WriteString("local:\n")
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if path, err := exec.LookPath(tool); err == nil {
			fmt.Fprintf(&b, "- %s: %s\n", tool, path)
		} else {
			fmt.Fprintf(&b, "- %s: missing (%v)\n", tool, err)
		}
	}
	if err := probeLoopbackVNC(ctx, target, "2", "1"); err == nil {
		b.WriteString("- vnc-loopback: ok\n")
	} else {
		fmt.Fprintf(&b, "- vnc-loopback: failed (%v)\n", err)
	}
	b.WriteString("remote:\n")
	out, err := runSSHOutput(ctx, target, desktopRecorderDiagnosticsRemoteCommand(target))
	if strings.TrimSpace(out) != "" {
		b.WriteString(strings.TrimSpace(out))
		b.WriteByte('\n')
	}
	if err != nil {
		fmt.Fprintf(&b, "- diagnostics-error: %v\n", err)
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil && filepath.Dir(outputPath) != "." {
		return exit(2, "create diagnostics directory: %v", err)
	}
	return os.WriteFile(outputPath, []byte(b.String()), 0o644)
}

func desktopRecorderDiagnosticsRemoteCommand(target SSHTarget) string {
	if isWindowsNativeTarget(target) {
		return powershellCommand(`Write-Output "- target-os: windows"
foreach ($name in "schtasks.exe","powershell.exe") {
  $cmd = Get-Command $name -ErrorAction SilentlyContinue
  if ($cmd) { Write-Output "- $name: $($cmd.Source)" } else { Write-Output "- ${name}: missing" }
}
$base = "C:\ProgramData\crabbox"
Write-Output "- programdata-crabbox: $(Test-Path -LiteralPath $base)"
Write-Output "- windows-username-file: $(Test-Path -LiteralPath (Join-Path $base "windows.username"))"
Write-Output "- windows-password-file: $(Test-Path -LiteralPath (Join-Path $base "windows.password"))"
$service = Get-Service -Name tvnserver -ErrorAction SilentlyContinue
if ($service) { Write-Output "- tvnserver: $($service.Status)" } else { Write-Output "- tvnserver: missing" }
try { query user | ForEach-Object { Write-Output "- query-user: $_" } } catch { Write-Output "- query-user: failed $($_.Exception.Message)" }
`)
	}
	return `set +e
echo "- target-os: ${CRABBOX_TARGET_OS:-linux}"
echo "- display: ${DISPLAY:-}"
if command -v ffmpeg >/dev/null 2>&1; then echo "- remote-ffmpeg: $(command -v ffmpeg)"; else echo "- remote-ffmpeg: missing"; fi
if command -v xdpyinfo >/dev/null 2>&1; then
  echo "- xdpyinfo: $(command -v xdpyinfo)"
  xdpyinfo | awk '/dimensions:/{print "- screen-size: "$2; exit}'
else
  echo "- xdpyinfo: missing"
fi
if command -v ss >/dev/null 2>&1 && ss -ltn | grep -q '127.0.0.1:5900'; then echo "- vnc-listener: ok"; else echo "- vnc-listener: missing"; fi
`
}

func writeProofMetadata(path string, metadata desktopProofMetadata) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil && filepath.Dir(path) != "." {
		return exit(2, "create proof metadata directory: %v", err)
	}
	return writeJSONFile(path, metadata)
}
