package cli

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type artifactFile struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	Path string `json:"path"`
	URL  string `json:"url,omitempty"`
}

type artifactBundleMetadata struct {
	CreatedAt string `json:"createdAt"`
	Version   string `json:"crabboxVersion"`
	LeaseID   string `json:"leaseId,omitempty"`
	Slug      string `json:"slug,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Network   string `json:"network,omitempty"`
	TargetOS  string `json:"targetOS,omitempty"`
	RunID     string `json:"runId,omitempty"`
}

type artifactCollectResult struct {
	Directory string                 `json:"directory"`
	Files     []artifactFile         `json:"files"`
	Metadata  artifactBundleMetadata `json:"metadata"`
	Warnings  []artifactWarning      `json:"warnings,omitempty"`
	Error     *artifactCollectError  `json:"error,omitempty"`
}

type artifactCollectError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type artifactWarning struct {
	Problem  string   `json:"problem"`
	Detail   string   `json:"detail,omitempty"`
	Rescue   []string `json:"rescue,omitempty"`
	Fallback string   `json:"fallback,omitempty"`
}

type artifactPublishOptions struct {
	Directory   string
	Storage     string
	Bucket      string
	Prefix      string
	BaseURL     string
	PR          int
	Repo        string
	Template    string
	Summary     string
	SummaryFile string
	Region      string
	Profile     string
	EndpointURL string
	ACL         string
	Presign     bool
	Expires     time.Duration
	DryRun      bool
	NoComment   bool
}

func (a App) artifactsCollect(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("artifacts collect", a.Stderr)
	provider := fs.String("provider", defaults.Provider, "provider: hetzner, aws, or ssh")
	id := fs.String("id", "", "lease id or slug")
	output := fs.String("output", "", "artifact bundle directory")
	runID := fs.String("run", "", "optional run id whose retained logs should be copied")
	all := fs.Bool("all", false, "collect screenshot, video, GIF, doctor/status, logs, and metadata")
	screenshot := fs.Bool("screenshot", true, "capture desktop screenshot")
	video := fs.Bool("video", false, "record desktop video")
	gif := fs.Bool("gif", false, "create trimmed GIF from recorded video")
	contactSheet := fs.Bool("contact-sheet", true, "create a sampled contact sheet PNG next to recorded video")
	noContactSheet := fs.Bool("no-contact-sheet", false, "skip contact sheet generation")
	doctor := fs.Bool("doctor", true, "write desktop doctor output")
	webvncStatus := fs.Bool("webvnc-status", true, "write WebVNC portal status when coordinator is configured")
	metadata := fs.Bool("metadata", true, "write metadata.json")
	duration := fs.Duration("duration", 10*time.Second, "video capture duration")
	fps := fs.Float64("fps", 15, "video frames per second")
	gifWidth := fs.Int("gif-width", defaultMediaPreviewWidth, "trimmed GIF width")
	gifFPS := fs.Float64("gif-fps", defaultMediaPreviewFPS, "trimmed GIF frames per second")
	contactSheetFrames := fs.Int("contact-sheet-frames", 5, "number of sampled frames in the contact sheet")
	contactSheetCols := fs.Int("contact-sheet-cols", 5, "contact sheet columns")
	contactSheetWidth := fs.Int("contact-sheet-width", 320, "width of each contact sheet tile")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	jsonOut := fs.Bool("json", false, "print machine-readable result")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	setIDFromFirstArg(fs, id)
	if *all {
		*video = true
		*gif = true
	}
	if *gif && !*video {
		return exit(2, "artifacts collect --gif requires --video or --all")
	}
	if *duration <= 0 {
		return exit(2, "artifacts collect --duration must be positive")
	}
	if *fps <= 0 {
		return exit(2, "artifacts collect --fps must be positive")
	}
	if *gifWidth <= 0 {
		return exit(2, "artifacts collect --gif-width must be positive")
	}
	if *gifFPS <= 0 {
		return exit(2, "artifacts collect --gif-fps must be positive")
	}
	if *contactSheetFrames <= 0 {
		return exit(2, "artifacts collect --contact-sheet-frames must be positive")
	}
	if *contactSheetCols <= 0 {
		return exit(2, "artifacts collect --contact-sheet-cols must be positive")
	}
	if *contactSheetWidth <= 0 {
		return exit(2, "artifacts collect --contact-sheet-width must be positive")
	}
	if *noContactSheet {
		*contactSheet = false
	}
	cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{Desktop: true})
	if err != nil {
		return err
	}
	if isBlacksmithProvider(cfg.Provider) {
		return exit(2, "artifacts collect is not supported for provider=%s; Blacksmith owns machine connectivity", cfg.Provider)
	}
	if err := requireLeaseID(*id, "crabbox artifacts collect --id <lease-id-or-slug> [--output <dir>]", cfg); err != nil {
		return err
	}
	server, target, leaseID, err := a.resolveNetworkLeaseTarget(ctx, cfg, *id, false)
	if err != nil {
		return err
	}
	if isStaticProvider(cfg.Provider) && target.TargetOS != targetLinux {
		return exit(2, "desktop artifacts are not collected from static %s hosts because those are existing host machines, not Crabbox-created desktops", target.TargetOS)
	}
	if err := enforceManagedLeaseCapabilities(cfg, server, leaseID); err != nil {
		return err
	}
	if err := a.claimAndTouchLeaseTarget(ctx, cfg, server, leaseID, *reclaim); err != nil {
		return err
	}
	dir := strings.TrimSpace(*output)
	if dir == "" {
		dir = defaultArtifactBundleDir(leaseID, serverSlug(server))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return exit(2, "create artifact directory: %v", err)
	}

	result := artifactCollectResult{
		Directory: dir,
		Metadata: artifactBundleMetadata{
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
			Version:   version,
			LeaseID:   leaseID,
			Slug:      serverSlug(server),
			Provider:  cfg.Provider,
			Network:   string(cfg.Network),
			TargetOS:  target.TargetOS,
			RunID:     strings.TrimSpace(*runID),
		},
	}
	addFile := func(kind, path string) {
		result.Files = append(result.Files, artifactFile{Kind: kind, Name: filepath.Base(path), Path: path})
	}
	fail := func(err error, warning artifactWarning) error {
		return a.finishArtifactCollectFailure(&result, *jsonOut, err, warning)
	}

	if *metadata {
		path := filepath.Join(dir, "metadata.json")
		if err := writeJSONFile(path, result.Metadata); err != nil {
			return err
		}
		addFile("metadata", path)
	}
	if *screenshot {
		if err := waitForLoopbackVNC(ctx, &target); err != nil {
			return fail(err, artifactWarning{
				Problem: rescueVNCTargetUnreachable,
				Detail:  err.Error(),
				Rescue:  []string{desktopDoctorCommand(rescueContext{Cfg: cfg, Target: target, LeaseID: leaseID})},
			})
		}
		path := filepath.Join(dir, "screenshot.png")
		if err := captureDesktopScreenshot(ctx, target, path); err != nil {
			return fail(err, artifactWarning{
				Problem: classifyDesktopFailure(err.Error()),
				Detail:  err.Error(),
				Rescue:  []string{desktopDoctorCommand(rescueContext{Cfg: cfg, Target: target, LeaseID: leaseID})},
			})
		}
		addFile("screenshot", path)
	}
	if *doctor {
		path := filepath.Join(dir, "doctor.txt")
		out, err := runSSHOutput(ctx, target, desktopDoctorRemoteCommand(target))
		if err != nil {
			doctorErr := exit(5, "desktop doctor failed: %v", err)
			return fail(doctorErr, artifactWarning{
				Problem: classifyDesktopFailure(out),
				Detail:  trimFailureDetail(out),
				Rescue:  []string{desktopDoctorCommand(rescueContext{Cfg: cfg, Target: target, LeaseID: leaseID})},
			})
		}
		if err := os.WriteFile(path, []byte(out+"\n"), 0o644); err != nil {
			return exit(2, "write doctor artifact: %v", err)
		}
		addFile("doctor", path)
	}
	if *webvncStatus {
		if path, ok, err := a.writeArtifactWebVNCStatus(ctx, cfg, target, leaseID, dir, &result.Warnings); err != nil {
			return err
		} else if ok {
			addFile("webvnc-status", path)
		}
	}
	if strings.TrimSpace(*runID) != "" {
		logPath, runPath, err := writeArtifactRunLogs(ctx, strings.TrimSpace(*runID), dir)
		if err != nil {
			return fail(err, artifactWarning{
				Problem: rescueArtifactCaptureFailed,
				Detail:  err.Error(),
				Rescue:  []string{"crabbox logs " + strings.TrimSpace(*runID)},
			})
		}
		addFile("logs", logPath)
		addFile("run", runPath)
	}
	if *video {
		if target.TargetOS != targetLinux && !isWindowsNativeTarget(target) {
			err := exit(2, "artifacts collect --video currently requires target=linux with ffmpeg/x11grab or native Windows desktop capture")
			return fail(err, artifactWarning{
				Problem: rescueArtifactCaptureFailed,
				Detail:  err.Error(),
				Rescue:  []string{desktopDoctorCommand(rescueContext{Cfg: cfg, Target: target, LeaseID: leaseID})},
			})
		}
		path := filepath.Join(dir, "screen.mp4")
		if err := captureDesktopVideo(ctx, target, path, *duration, *fps); err != nil {
			return fail(err, artifactWarning{
				Problem: classifyDesktopFailure(err.Error()),
				Detail:  err.Error(),
				Rescue:  []string{desktopDoctorCommand(rescueContext{Cfg: cfg, Target: target, LeaseID: leaseID})},
			})
		}
		addFile("video", path)
		if *contactSheet {
			contactPath := filepath.Join(dir, "screen.contact.png")
			if _, err := createMediaContactSheet(ctx, mediaContactSheetOptions{
				Input:  path,
				Output: contactPath,
				Frames: *contactSheetFrames,
				Cols:   *contactSheetCols,
				Width:  *contactSheetWidth,
			}); err != nil {
				appendContactSheetWarning(&result.Warnings, err)
			} else {
				addFile("contact-sheet", contactPath)
			}
		}
		if *gif {
			gifPath := filepath.Join(dir, "screen.trimmed.gif")
			trimmedPath := filepath.Join(dir, "screen.trimmed.mp4")
			options := defaultMediaPreviewOptions(path, gifPath, trimmedPath)
			options.Width = *gifWidth
			options.FPS = *gifFPS
			preview, err := createMediaPreview(ctx, options)
			if err != nil {
				return fail(err, artifactWarning{
					Problem: rescueArtifactCaptureFailed,
					Detail:  err.Error(),
				})
			}
			addFile("gif", preview.Output)
			if preview.TrimmedVideoOutput != "" {
				addFile("trimmed-video", preview.TrimmedVideoOutput)
			}
		}
	}
	sortArtifactFiles(result.Files)
	if result.Files == nil {
		result.Files = []artifactFile{}
	}
	if *jsonOut {
		enc := json.NewEncoder(a.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}
	for _, warning := range result.Warnings {
		printArtifactWarning(a.Stdout, warning)
	}
	fmt.Fprintf(a.Stdout, "artifacts: %s\n", dir)
	for _, file := range result.Files {
		fmt.Fprintf(a.Stdout, "%s: %s\n", file.Kind, file.Path)
	}
	fmt.Fprintf(a.Stdout, "publish: crabbox artifacts publish --dir %s --pr <n>\n", strings.Join(readableShellWords([]string{dir}), " "))
	return nil
}

func (a App) finishArtifactCollectFailure(result *artifactCollectResult, jsonOut bool, err error, warning artifactWarning) error {
	if result == nil {
		return err
	}
	sortArtifactFiles(result.Files)
	if result.Files == nil {
		result.Files = []artifactFile{}
	}
	if strings.TrimSpace(warning.Problem) != "" {
		result.Warnings = append(result.Warnings, normalizeArtifactWarning(warning))
	}
	result.Error = &artifactCollectError{
		Code:    artifactErrorCode(result.Warnings),
		Message: strings.TrimSpace(err.Error()),
	}
	if jsonOut {
		enc := json.NewEncoder(a.Stdout)
		enc.SetIndent("", "  ")
		if encodeErr := enc.Encode(result); encodeErr != nil {
			return encodeErr
		}
		return err
	}
	for _, warning := range result.Warnings {
		printArtifactWarning(a.Stdout, warning)
	}
	return err
}

func (a App) artifactsVideo(ctx context.Context, args []string) error {
	target, cfg, leaseID, err := a.desktopCommandTarget(ctx, "artifacts video", args, false)
	if err != nil {
		return err
	}
	output, _ := stringFlagValue(args, "output")
	if strings.TrimSpace(output) == "" {
		output = "crabbox-" + normalizeLeaseSlug(leaseID) + "-screen.mp4"
	}
	duration := durationFlagValue(args, "duration", 10*time.Second)
	fps := floatFlagValue(args, "fps", 15)
	if duration <= 0 {
		return exit(2, "artifacts video --duration must be positive")
	}
	if fps <= 0 {
		return exit(2, "artifacts video --fps must be positive")
	}
	contactEnabled := boolFlagValueOr(args, "contact-sheet", true) && !boolFlagPresent(args, "no-contact-sheet")
	contactPath, _ := stringFlagValue(args, "contact-sheet-output")
	if strings.TrimSpace(contactPath) == "" {
		contactPath = contactSheetPathForVideo(output)
	}
	contactFrames := intFlagValueOr(args, "contact-sheet-frames", 5)
	contactCols := intFlagValueOr(args, "contact-sheet-cols", 5)
	contactWidth := intFlagValueOr(args, "contact-sheet-width", 320)
	if contactFrames <= 0 {
		return exit(2, "artifacts video --contact-sheet-frames must be positive")
	}
	if contactCols <= 0 {
		return exit(2, "artifacts video --contact-sheet-cols must be positive")
	}
	if contactWidth <= 0 {
		return exit(2, "artifacts video --contact-sheet-width must be positive")
	}
	if err := captureDesktopVideo(ctx, target, output, duration, fps); err != nil {
		printRescue(a.Stdout, classifyDesktopFailure(err.Error()), err.Error(), desktopDoctorCommand(rescueContext{Cfg: cfg, Target: target, LeaseID: leaseID}))
		return err
	}
	fmt.Fprintf(a.Stdout, "video: %s\n", output)
	if contactEnabled {
		if _, err := createMediaContactSheet(ctx, mediaContactSheetOptions{
			Input:  output,
			Output: contactPath,
			Frames: contactFrames,
			Cols:   contactCols,
			Width:  contactWidth,
		}); err != nil {
			printContactSheetWarning(a.Stdout, err)
		} else {
			fmt.Fprintf(a.Stdout, "contact-sheet: %s\n", contactPath)
		}
	}
	return nil
}

func (a App) artifactsGif(ctx context.Context, args []string) error {
	return a.mediaPreview(ctx, args)
}

func (a App) artifactsTemplate(ctx context.Context, args []string) error {
	_ = ctx
	initialKind := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		initialKind = args[0]
		args = args[1:]
	}
	fs := newFlagSet("artifacts template", a.Stderr)
	kind := fs.String("kind", initialKind, "template kind: openclaw or mantis")
	before := fs.String("before", "", "before screenshot/GIF URL or path")
	after := fs.String("after", "", "after screenshot/GIF URL or path")
	summary := fs.String("summary", "", "summary text")
	summaryFile := fs.String("summary-file", "", "summary markdown file")
	output := fs.String("output", "", "output markdown path; stdout when omitted")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	text, err := summaryText(*summary, *summaryFile)
	if err != nil {
		return err
	}
	body := artifactTemplateMarkdown(*kind, text, *before, *after, nil)
	if strings.TrimSpace(*output) == "" {
		fmt.Fprint(a.Stdout, body)
		return nil
	}
	if err := os.WriteFile(*output, []byte(body), 0o644); err != nil {
		return exit(2, "write template: %v", err)
	}
	fmt.Fprintf(a.Stdout, "template: %s\n", *output)
	return nil
}

func (a App) artifactsPublish(ctx context.Context, args []string) error {
	opts, err := parseArtifactPublishOptions(args, a.Stderr)
	if err != nil {
		return err
	}
	published, bodyPath, err := a.publishArtifactDirectory(ctx, opts)
	if err != nil {
		return err
	}
	for _, file := range published {
		if file.URL != "" {
			fmt.Fprintf(a.Stdout, "%s: %s\n", file.Kind, file.URL)
		} else {
			fmt.Fprintf(a.Stdout, "%s: %s\n", file.Kind, file.Path)
		}
	}
	fmt.Fprintf(a.Stdout, "markdown: %s\n", bodyPath)
	return nil
}

func (a App) publishArtifactDirectory(ctx context.Context, opts artifactPublishOptions) ([]artifactFile, string, error) {
	var err error
	var coord *CoordinatorClient
	if opts.Storage == "auto" || opts.Storage == "broker" {
		cfg, cfgErr := loadConfig()
		if cfgErr != nil {
			return nil, "", cfgErr
		}
		var useCoordinator bool
		coord, useCoordinator, err = newCoordinatorClient(cfg)
		if err != nil {
			return nil, "", err
		}
		if opts.Storage == "auto" {
			if useCoordinator && coord != nil && coord.Token != "" {
				opts.Storage = "broker"
			} else {
				opts.Storage = "local"
			}
		}
	}
	ensureArtifactPublishPrefix(&opts)
	files, err := listArtifactBundleFiles(opts.Directory)
	if err != nil {
		return nil, "", err
	}
	if len(files) == 0 {
		return nil, "", exit(2, "artifact directory has no files: %s", opts.Directory)
	}
	summary, err := summaryText(opts.Summary, opts.SummaryFile)
	if err != nil {
		return nil, "", err
	}
	var published []artifactFile
	if opts.Storage == "broker" {
		published, err = publishArtifactFilesBroker(ctx, coord, opts, files)
	} else {
		published, err = publishArtifactFiles(ctx, opts, files)
	}
	if err != nil {
		return nil, "", err
	}
	body := artifactTemplateMarkdown(opts.Template, summary, "", "", published)
	bodyPath := filepath.Join(opts.Directory, "published-artifacts.md")
	if err := os.WriteFile(bodyPath, []byte(body), 0o644); err != nil {
		return nil, "", exit(2, "write publish markdown: %v", err)
	}
	if opts.PR > 0 && !opts.NoComment {
		if opts.Storage == "local" && opts.BaseURL == "" {
			return nil, "", exit(2, "artifacts publish --pr needs brokered publishing, --storage s3|r2|cloudflare, or --base-url for already-hosted local assets")
		}
		if opts.DryRun {
			fmt.Fprintf(a.Stdout, "dry-run comment: gh issue comment %d --body-file %s\n", opts.PR, bodyPath)
		} else if err := postGitHubPRComment(ctx, opts.PR, opts.Repo, bodyPath); err != nil {
			return nil, "", err
		}
	}
	return published, bodyPath, nil
}

func defaultArtifactBundleDir(leaseID, slug string) string {
	name := strings.TrimSpace(slug)
	if name == "" {
		name = leaseID
	}
	if name == "" {
		name = time.Now().UTC().Format("20060102-150405")
	}
	return filepath.Join("artifacts", normalizeLeaseSlug(name))
}

func writeJSONFile(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return exit(2, "encode %s: %v", path, err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return exit(2, "write %s: %v", path, err)
	}
	return nil
}

func (a App) writeArtifactWebVNCStatus(ctx context.Context, cfg Config, target SSHTarget, leaseID, dir string, warnings *[]artifactWarning) (string, bool, error) {
	if isStaticProvider(cfg.Provider) || isBlacksmithProvider(cfg.Provider) {
		return "", false, nil
	}
	coord, useCoordinator, err := newTargetCoordinatorClient(cfg)
	if err != nil || !useCoordinator || coord == nil || coord.Token == "" {
		return "", false, nil
	}
	status, err := coord.WebVNCStatus(ctx, leaseID)
	path := filepath.Join(dir, "webvnc-status.json")
	payload := map[string]any{"leaseId": leaseID, "target": target.TargetOS}
	if err != nil {
		payload["error"] = err.Error()
		rescueCtx := rescueContext{Cfg: cfg, Target: target, LeaseID: leaseID}
		appendArtifactWarning(warnings, rescueVNCBridgeDisconnected, err.Error(), "", webVNCStatusRescueCommand(rescueCtx), webVNCResetRescueCommand(rescueCtx))
	} else {
		payload["status"] = status
		rescueCtx := rescueContext{Cfg: cfg, Target: target, LeaseID: leaseID}
		if !status.BridgeConnected {
			appendArtifactWarning(warnings, rescueVNCBridgeNotRunning, "portal has no active WebVNC bridge for this lease", "", webVNCDaemonStartRescueCommand(rescueCtx), webVNCResetRescueCommand(rescueCtx))
		} else if webVNCObserverSlotsExhausted(status) {
			appendArtifactWarning(warnings, rescueVNCObserverSlotsFull, "all WebVNC observer slots are in use or stale", "", webVNCDaemonStartRescueCommand(rescueCtx), webVNCResetRescueCommand(rescueCtx))
		}
	}
	if err := writeJSONFile(path, payload); err != nil {
		return "", false, err
	}
	return path, true, nil
}

func appendArtifactWarning(warnings *[]artifactWarning, problem, detail, fallback string, rescue ...string) {
	if warnings == nil {
		return
	}
	clean := normalizeArtifactWarning(artifactWarning{Problem: problem, Detail: detail, Fallback: fallback, Rescue: rescue})
	if clean.Problem != "" {
		*warnings = append(*warnings, clean)
	}
}

func appendContactSheetWarning(warnings *[]artifactWarning, err error) {
	if err == nil {
		return
	}
	appendArtifactWarning(warnings, rescueArtifactCaptureFailed, "contact-sheet skipped: "+err.Error(), "")
}

func normalizeArtifactWarning(warning artifactWarning) artifactWarning {
	clean := artifactWarning{
		Problem:  strings.TrimSpace(warning.Problem),
		Detail:   strings.TrimSpace(warning.Detail),
		Fallback: strings.TrimSpace(warning.Fallback),
	}
	for _, command := range warning.Rescue {
		if strings.TrimSpace(command) != "" {
			clean.Rescue = append(clean.Rescue, strings.TrimSpace(command))
		}
	}
	return clean
}

func artifactErrorCode(warnings []artifactWarning) string {
	if len(warnings) == 0 || strings.TrimSpace(warnings[len(warnings)-1].Problem) == "" {
		return "artifact_collect_failed"
	}
	return normalizeLeaseSlug(warnings[len(warnings)-1].Problem)
}

func printArtifactWarning(w io.Writer, warning artifactWarning) {
	printRescueWithFallback(w, warning.Problem, warning.Detail, warning.Fallback, warning.Rescue...)
}

func writeArtifactRunLogs(ctx context.Context, runID, dir string) (string, string, error) {
	coord, err := configuredCoordinator()
	if err != nil {
		return "", "", err
	}
	logText, err := coord.RunLogs(ctx, runID)
	if err != nil {
		return "", "", err
	}
	run, err := coord.Run(ctx, runID)
	if err != nil {
		return "", "", err
	}
	logPath := filepath.Join(dir, "logs.txt")
	runPath := filepath.Join(dir, "run.json")
	if err := os.WriteFile(logPath, []byte(logText), 0o644); err != nil {
		return "", "", exit(2, "write logs artifact: %v", err)
	}
	if err := writeJSONFile(runPath, run); err != nil {
		return "", "", err
	}
	return logPath, runPath, nil
}

func captureDesktopVideo(ctx context.Context, target SSHTarget, outputPath string, duration time.Duration, fps float64) error {
	if isWindowsNativeTarget(target) {
		return captureWindowsDesktopVideo(ctx, target, outputPath, duration, fps)
	}
	if target.TargetOS != targetLinux {
		return exit(2, "artifacts video currently requires target=linux or native Windows desktop capture")
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil && filepath.Dir(outputPath) != "." {
		return exit(2, "create video directory: %v", err)
	}
	file, err := os.Create(outputPath)
	if err != nil {
		return exit(2, "create video %s: %v", outputPath, err)
	}
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(outputPath)
		}
	}()
	if err := runSSHToWriter(ctx, target, desktopVideoRemoteCommand(duration, fps), file); err != nil {
		return exit(5, "capture video: %v", err)
	}
	ok = true
	return nil
}

func desktopVideoRemoteCommand(duration time.Duration, fps float64) string {
	seconds := strconv.FormatFloat(duration.Seconds(), 'f', 3, 64)
	frameRate := strconv.FormatFloat(fps, 'f', 3, 64)
	return fmt.Sprintf(`set -eu
export DISPLAY="${DISPLAY:-:99}"
if ! command -v ffmpeg >/dev/null 2>&1; then
  echo "missing ffmpeg; warm a new --desktop lease or install ffmpeg" >&2
  exit 127
fi
if command -v xdpyinfo >/dev/null 2>&1; then
  size="$(xdpyinfo | awk '/dimensions:/{print $2; exit}')"
else
  size=""
fi
if [ -z "$size" ]; then size="1920x1080"; fi
ffmpeg -hide_banner -loglevel error -y -f x11grab -video_size "$size" -framerate %s -i "$DISPLAY" -t %s -pix_fmt yuv420p -an -movflags frag_keyframe+empty_moov -f mp4 -
`, frameRate, seconds)
}

func captureWindowsDesktopVideo(ctx context.Context, target SSHTarget, outputPath string, duration time.Duration, fps float64) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return exit(2, "ffmpeg is required to encode Windows desktop video locally: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil && filepath.Dir(outputPath) != "." {
		return exit(2, "create video directory: %v", err)
	}
	tempDir, err := os.MkdirTemp("", "crabbox-windows-video-*")
	if err != nil {
		return exit(2, "create temp video dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	zipPath := filepath.Join(tempDir, "frames.zip")
	zipFile, err := os.Create(zipPath)
	if err != nil {
		return exit(2, "create frame archive: %v", err)
	}
	token := strconv.FormatInt(time.Now().UnixNano(), 36)
	remoteBase := "C:/ProgramData/crabbox"
	remoteScript := remoteBase + "/cv-" + token + ".ps1"
	remoteOutDir := remoteBase + "/cv-" + token + "-frames"
	remoteZip := remoteBase + "/cv-" + token + ".zip"
	frames, intervalMS := windowsDesktopVideoFrameTiming(duration, fps)
	localScript := filepath.Join(tempDir, "capture-windows-video.ps1")
	if err := os.WriteFile(localScript, []byte(windowsDesktopVideoCaptureScript(
		strings.ReplaceAll(remoteOutDir, "/", `\`),
		strings.ReplaceAll(remoteZip, "/", `\`),
		frames,
		intervalMS,
	)), 0o644); err != nil {
		_ = zipFile.Close()
		return exit(2, "write Windows capture script: %v", err)
	}
	if out, err := runSSHCombinedOutput(ctx, target, `powershell.exe -NoProfile -ExecutionPolicy Bypass -Command "New-Item -ItemType Directory -Force -Path C:\ProgramData\crabbox | Out-Null"`); err != nil {
		_ = zipFile.Close()
		return exit(5, "prepare Windows capture script dir: %v: %s", err, trimFailureDetail(out))
	}
	if err := copyLocalFileToTarget(ctx, target, localScript, remoteScript); err != nil {
		_ = zipFile.Close()
		return err
	}
	if err := runSSHToWriter(ctx, target, windowsDesktopVideoRemoteCommand(
		strings.ReplaceAll(remoteScript, "/", `\`),
		strings.ReplaceAll(remoteOutDir, "/", `\`),
		strings.ReplaceAll(remoteZip, "/", `\`),
		duration,
	), zipFile); err != nil {
		_ = zipFile.Close()
		return exit(5, "capture Windows video frames: %v", err)
	}
	if err := zipFile.Close(); err != nil {
		return exit(2, "close frame archive: %v", err)
	}
	framesDir := filepath.Join(tempDir, "frames")
	if err := os.MkdirAll(framesDir, 0o755); err != nil {
		return exit(2, "create frames dir: %v", err)
	}
	if err := extractFrameArchive(zipPath, framesDir); err != nil {
		return err
	}
	pattern := filepath.Join(framesDir, "frame-%06d.jpg")
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-framerate", strconv.FormatFloat(fps, 'f', 3, 64),
		"-start_number", "0",
		"-i", pattern,
		"-pix_fmt", "yuv420p",
		"-an",
		"-movflags", "+faststart",
		outputPath,
	}
	if out, err := exec.CommandContext(ctx, "ffmpeg", args...).CombinedOutput(); err != nil {
		_ = os.Remove(outputPath)
		return exit(5, "encode Windows video: %v: %s", err, tailForError(string(out)))
	}
	return nil
}

func copyLocalFileToTarget(ctx context.Context, target SSHTarget, localPath, remotePath string) error {
	args := append(scpBaseArgs(target), localPath, target.User+"@"+target.Host+":"+remotePath)
	cmd := exec.CommandContext(ctx, "scp", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return exit(5, "copy %s to target: %v: %s", filepath.Base(localPath), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func extractFrameArchive(zipPath, framesDir string) error {
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		return exit(5, "read frame archive: %v", err)
	}
	defer func() { _ = reader.Close() }()
	count := 0
	for _, file := range reader.File {
		name := filepath.Base(file.Name)
		if !strings.HasPrefix(name, "frame-") || !strings.HasSuffix(strings.ToLower(name), ".jpg") {
			continue
		}
		src, err := file.Open()
		if err != nil {
			return exit(5, "open frame %s: %v", name, err)
		}
		dstPath := filepath.Join(framesDir, name)
		dst, err := os.Create(dstPath)
		if err != nil {
			_ = src.Close()
			return exit(2, "create frame %s: %v", name, err)
		}
		_, copyErr := io.Copy(dst, src)
		closeErr := dst.Close()
		_ = src.Close()
		if copyErr != nil {
			return exit(2, "write frame %s: %v", name, copyErr)
		}
		if closeErr != nil {
			return exit(2, "close frame %s: %v", name, closeErr)
		}
		count++
	}
	if count == 0 {
		return exit(5, "frame archive contained no frames")
	}
	return nil
}

func windowsDesktopVideoFrameTiming(duration time.Duration, fps float64) (int, int) {
	frames := int(duration.Seconds()*fps + 0.999)
	if frames < 1 {
		frames = 1
	}
	intervalMS := int(1000 / fps)
	if intervalMS < 1 {
		intervalMS = 1
	}
	return frames, intervalMS
}

func windowsDesktopVideoCaptureScript(outDir, zipPath string, frames, intervalMS int) string {
	return fmt.Sprintf(`$OutDir = %s
$Zip = %s
$Frames = %d
$IntervalMS = %d
$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
New-Item -ItemType Directory -Force -Path $OutDir | Out-Null
$bounds = [System.Windows.Forms.Screen]::PrimaryScreen.Bounds
$start = [DateTime]::UtcNow
for ($i = 0; $i -lt $Frames; $i++) {
  $bitmap = New-Object System.Drawing.Bitmap $bounds.Width, $bounds.Height
  $graphics = [System.Drawing.Graphics]::FromImage($bitmap)
  $graphics.CopyFromScreen($bounds.Location, [System.Drawing.Point]::Empty, $bounds.Size)
  $path = Join-Path $OutDir ("frame-{0:D6}.jpg" -f $i)
  $bitmap.Save($path, [System.Drawing.Imaging.ImageFormat]::Jpeg)
  $graphics.Dispose()
  $bitmap.Dispose()
  $target = $start.AddMilliseconds(($i + 1) * $IntervalMS)
  $remaining = [int](($target - [DateTime]::UtcNow).TotalMilliseconds)
  if ($remaining -gt 0) { Start-Sleep -Milliseconds $remaining }
}
Compress-Archive -Path (Join-Path $OutDir "frame-*.jpg") -DestinationPath $Zip -Force
Set-Content -LiteralPath ($Zip + ".done") -Value "ok"
`, psQuote(outDir), psQuote(zipPath), frames, intervalMS)
}

func windowsDesktopVideoRemoteCommand(remoteScript, remoteOutDir, remoteZip string, duration time.Duration) string {
	return fmt.Sprintf(`$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"
$base = "C:\ProgramData\crabbox"
$password = ""
$passwordPath = Join-Path $base "windows.password"
if (Test-Path -LiteralPath $passwordPath) { $password = (Get-Content -Raw -LiteralPath $passwordPath).Trim() }
$taskName = "CrabboxVideo-" + [Guid]::NewGuid().ToString("N")
$outDir = %s
$zip = %s
$done = $zip + ".done"
$script = %s
cmd.exe /c "schtasks.exe /Delete /TN $taskName /F 2>NUL" | Out-Null
$startTime = (Get-Date).AddMinutes(1).ToString("HH:mm")
$taskRun = "powershell.exe -NoProfile -WindowStyle Hidden -ExecutionPolicy Bypass -File $script"
$createArgs = @("/Create", "/TN", $taskName, "/SC", "ONCE", "/ST", $startTime, "/TR", $taskRun, "/RU", $env:USERNAME, "/IT", "/F")
& schtasks.exe @createArgs | Out-Null
if ($LASTEXITCODE -ne 0 -and $password -ne "") {
  & schtasks.exe @($createArgs + @("/RP", $password)) | Out-Null
}
if ($LASTEXITCODE -ne 0) { throw "failed to create interactive video task" }
schtasks.exe /Run /TN $taskName | Out-Null
$deadline = (Get-Date).AddSeconds(%d)
while ((Get-Date) -lt $deadline) {
  if ((Test-Path -LiteralPath $done) -and (Test-Path -LiteralPath $zip)) {
    $stream = [IO.File]::Open($zip, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::Read)
    try {
      $buffer = New-Object byte[] 1048576
      while (($read = $stream.Read($buffer, 0, $buffer.Length)) -gt 0) {
        [Console]::OpenStandardOutput().Write($buffer, 0, $read)
      }
    } finally {
      $stream.Dispose()
    }
    schtasks.exe /Delete /TN $taskName /F | Out-Null
    Remove-Item -Recurse -Force -LiteralPath $outDir -ErrorAction SilentlyContinue
    Remove-Item -Force -LiteralPath $zip, $done, $script -ErrorAction SilentlyContinue
    exit 0
  }
  Start-Sleep -Milliseconds 250
}
schtasks.exe /Delete /TN $taskName /F | Out-Null
Remove-Item -Recurse -Force -LiteralPath $outDir -ErrorAction SilentlyContinue
Remove-Item -Force -LiteralPath $done, $script -ErrorAction SilentlyContinue
throw "scheduled interactive video did not produce output"`, psQuote(remoteOutDir), psQuote(remoteZip), psQuote(remoteScript), int(duration.Seconds())+30)
}
