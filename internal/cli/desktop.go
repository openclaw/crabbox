package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func (a App) desktopLaunch(ctx context.Context, args []string) error {
	return a.desktopLaunchWithCommand(ctx, args, nil)
}

func (a App) desktopLaunchWithCommand(ctx context.Context, args []string, commandOverride []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("desktop launch", a.Stderr)
	provider := fs.String("provider", defaults.Provider, providerHelpSSH())
	id := fs.String("id", "", "lease id or slug")
	browser := fs.Bool("browser", false, "launch the target browser")
	url := fs.String("url", "", "URL to pass to the launched browser")
	webvnc := fs.Bool("webvnc", false, "bridge the launched desktop into the authenticated WebVNC portal")
	openPortal := fs.Bool("open", false, "open the WebVNC portal when --webvnc is set")
	takeControl := fs.Bool("take-control", false, "ask the opened WebVNC portal viewer to take keyboard and mouse control")
	fullscreen := fs.Bool("fullscreen", false, "leave launched browser fullscreen for capture/video workflows")
	egress := fs.String("egress", "", "egress profile; passes the active lease-local proxy to the browser")
	egressProxy := fs.String("egress-proxy", defaultEgressListen, "lease-local egress proxy for --egress")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	providerFlags := registerProviderFlags(fs, defaults)
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *openPortal && !*webvnc {
		return exit(2, "desktop launch --open requires --webvnc")
	}
	if *takeControl && !*webvnc {
		return exit(2, "desktop launch --take-control requires --webvnc")
	}
	if strings.TrimSpace(*egress) != "" && !*browser {
		return exit(2, "desktop launch --egress currently requires --browser")
	}
	positionalID := false
	if *id == "" && fs.NArg() > 0 {
		*id = fs.Arg(0)
		positionalID = true
	}
	cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{LeaseID: *id, Desktop: true})
	if err != nil {
		return err
	}
	if err := applyProviderFlags(&cfg, fs, providerFlags); err != nil {
		return err
	}
	cfg.Browser = *browser
	if err := validateRequestedCapabilities(cfg); err != nil {
		return err
	}
	if *webvnc && (isBlacksmithProvider(cfg.Provider) || isStaticProvider(cfg.Provider)) {
		return exit(2, "desktop launch --webvnc is unavailable for provider=%s", cfg.Provider)
	}
	if *id == "" && !isStaticProvider(cfg.Provider) {
		return exit(2, "usage: crabbox desktop launch --id <lease-id-or-slug> [--browser] [--url <url>] -- <command...>")
	}
	server, target, leaseID, err := a.resolveNetworkLeaseTargetForRepoWithConfig(ctx, &cfg, *id, false, *reclaim)
	if err != nil {
		return err
	}
	if err := enforceManagedLeaseCapabilities(cfg, server, leaseID); err != nil {
		return err
	}
	repo, err := findRepo()
	if err != nil {
		return err
	}
	if err := a.claimResolvedLeaseTargetForRepoAndRegister(ctx, leaseID, serverSlug(server), cfg, server, target, repo.Root, *reclaim); err != nil {
		return err
	}
	a.touchLeaseTargetBestEffort(ctx, cfg, LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, "")
	if err := waitForLoopbackVNC(ctx, &target); err != nil {
		return err
	}
	env, err := requestedCapabilityEnv(ctx, cfg, target)
	if err != nil {
		return err
	}
	if *browser && target.TargetOS == targetLinux {
		_, _ = runSSHCombinedOutput(ctx, target, desktopBrowserDarkModeCommand(env["BROWSER"]))
	}
	command := fs.Args()
	if positionalID && len(command) > 0 && command[0] == *id {
		command = command[1:]
	}
	command = trimCommandSeparator(command)
	if commandOverride != nil {
		command = commandOverride
	}
	expectBrowserLaunch := false
	if *browser {
		if len(command) == 0 {
			if env["BROWSER"] == "" {
				printRescue(a.Stdout, rescueBrowserNotLaunched, "browser=true requested but target did not report BROWSER", desktopDoctorCommand(rescueContext{Cfg: cfg, Target: target, LeaseID: leaseID}))
				return exit(2, "browser=true requested but target did not report BROWSER")
			}
			command = []string{env["BROWSER"]}
			expectBrowserLaunch = true
			if strings.TrimSpace(*egress) != "" {
				command = append(command, "--proxy-server=http://"+strings.TrimSpace(*egressProxy))
			}
			if strings.TrimSpace(*url) != "" {
				command = append(command, strings.TrimSpace(*url))
			}
		} else if strings.TrimSpace(*url) != "" {
			expectBrowserLaunch = desktopCommandLooksLikeBrowser(command, env["BROWSER"])
			if strings.TrimSpace(*egress) != "" {
				command = append(command, "--proxy-server=http://"+strings.TrimSpace(*egressProxy))
			}
			command = append(command, strings.TrimSpace(*url))
		} else if strings.TrimSpace(*egress) != "" {
			expectBrowserLaunch = desktopCommandLooksLikeBrowser(command, env["BROWSER"])
			command = append(command, "--proxy-server=http://"+strings.TrimSpace(*egressProxy))
		} else {
			expectBrowserLaunch = desktopCommandLooksLikeBrowser(command, env["BROWSER"])
		}
	}
	if len(command) == 0 {
		return exit(2, "usage: crabbox desktop launch --id <lease-id-or-slug> -- <command...>")
	}
	workdir := remoteJoin(cfg, leaseID, repo.Name)
	rescueCtx := rescueContext{Cfg: cfg, Target: target, LeaseID: leaseID}
	launchOptions := desktopLaunchOptions{
		WindowedBrowser: *browser && !*fullscreen,
		VerifyProcess:   !expectBrowserLaunch || target.TargetOS != targetLinux,
	}
	launchOutput, err := runDesktopLaunchRemoteCombinedOutput(ctx, target, desktopLaunchRemoteCommand(target, workdir, env, command, launchOptions))
	if err != nil {
		out := launchOutput
		printRescue(a.Stdout, classifyDesktopFailure(out), trimFailureDetail(out), desktopDoctorCommand(rescueCtx), desktopLaunchRetryCommand(rescueCtx, command))
		return exit(5, "launch desktop command: %v", err)
	}
	var windowsWindow windowsDesktopWindow
	if isWindowsNativeTarget(target) {
		windowsWindow, err = parseWindowsDesktopWindow(launchOutput)
		if err != nil {
			printRescue(a.Stdout, rescueDesktopCommandNotLaunched, trimFailureDetail(launchOutput), desktopDoctorCommand(rescueCtx), desktopLaunchRetryCommand(rescueCtx, command))
			return exit(5, "launch desktop command: %v", err)
		}
	}
	if expectBrowserLaunch && target.TargetOS == targetLinux {
		if out, err := runSSHCombinedOutput(ctx, target, desktopBrowserLaunchCheckCommand()); err != nil {
			printRescue(a.Stdout, rescueBrowserNotLaunched, trimFailureDetail(out), desktopDoctorCommand(rescueCtx), desktopLaunchRetryCommand(rescueCtx, command))
			return exit(5, "browser not launched for %s: %v", leaseID, err)
		}
	}
	fmt.Fprintf(a.Stdout, "launched: %s\n", strings.Join(command, " "))
	if windowsWindow.PID != 0 {
		fmt.Fprintf(a.Stdout, "window: pid=%d session=%d title=%q\n", windowsWindow.PID, windowsWindow.SessionID, windowsWindow.Title)
	}
	if *webvnc {
		return a.webvnc(ctx, desktopLaunchWebVNCArgs(cfg, target, leaseID, *openPortal, *takeControl))
	}
	return nil
}

type windowsDesktopWindow struct {
	PID       int
	SessionID int
	Title     string
}

const windowsDesktopWindowMarker = "CRABBOX_DESKTOP_WINDOW"

func parseWindowsDesktopWindow(output string) (windowsDesktopWindow, error) {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 4 || fields[0] != windowsDesktopWindowMarker {
			continue
		}
		values := make(map[string]string, 3)
		for _, field := range fields[1:] {
			key, value, ok := strings.Cut(field, "=")
			if !ok {
				continue
			}
			values[key] = value
		}
		pid, pidErr := strconv.Atoi(values["pid"])
		sessionID, sessionErr := strconv.Atoi(values["session"])
		title, titleErr := base64.StdEncoding.DecodeString(values["title"])
		if pidErr != nil || pid <= 0 || sessionErr != nil || sessionID < 0 || titleErr != nil || len(title) == 0 {
			return windowsDesktopWindow{}, fmt.Errorf("invalid Windows desktop window result")
		}
		return windowsDesktopWindow{PID: pid, SessionID: sessionID, Title: string(title)}, nil
	}
	return windowsDesktopWindow{}, fmt.Errorf("Windows desktop launch did not return a visible window")
}

func (a App) desktopTerminal(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("desktop terminal", a.Stderr)
	provider := fs.String("provider", defaults.Provider, providerHelpSSH())
	id := fs.String("id", "", "lease id or slug")
	fontSize := fs.Int("font-size", 14, "terminal font size")
	cols := fs.Int("cols", 100, "terminal columns")
	rows := fs.Int("rows", 32, "terminal rows")
	sixel := fs.Bool("sixel", false, "prefer a Sixel-capable terminal configuration")
	waitVisible := fs.Duration("wait-visible", 0, "delay after launch before capture")
	screenshot := fs.String("screenshot", "", "capture a screenshot after launch")
	record := fs.String("record", "", "record an MP4 after launch")
	recordDuration := fs.Duration("record-duration", 5*time.Second, "recording duration for --record")
	recordFPS := fs.Float64("record-fps", 8, "recording frames per second for --record")
	diagnostics := fs.String("diagnostics", "", "write recorder diagnostics after launch")
	contactFlags := registerContactSheetFlags(fs)
	publishFlags := registerDesktopPublishFlags(fs)
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	providerFlags := registerProviderFlags(fs, defaults)
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	markDesktopPublishExplicitFlags(fs, &publishFlags)
	if err := validateContactSheetFlags("desktop terminal", contactFlags); err != nil {
		return err
	}
	if strings.TrimSpace(*record) != "" {
		if *recordDuration <= 0 {
			return exit(2, "desktop terminal --record-duration must be positive")
		}
		if *recordFPS <= 0 {
			return exit(2, "desktop terminal --record-fps must be positive")
		}
	}
	positionalID := false
	if shouldConsumeDesktopTerminalPositionalID(*provider, *id, fs.NArg()) {
		*id = fs.Arg(0)
		positionalID = true
	}
	cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{LeaseID: *id, Desktop: true})
	if err != nil {
		return err
	}
	if err := applyProviderFlags(&cfg, fs, providerFlags); err != nil {
		return err
	}
	if err := validateRequestedCapabilities(cfg); err != nil {
		return err
	}
	if *id == "" && !isStaticProvider(cfg.Provider) {
		return exit(2, "usage: crabbox desktop terminal --id <lease-id-or-slug> -- <command...>")
	}
	command := fs.Args()
	if positionalID && len(command) > 0 && command[0] == *id {
		command = command[1:]
	}
	command = trimCommandSeparator(command)
	server, target, leaseID, err := a.resolveNetworkLeaseTargetForRepoWithConfig(ctx, &cfg, *id, false, *reclaim)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*record) != "" && !supportsDesktopVideoTarget(target) {
		return exit(2, "desktop terminal --record currently requires target=linux with ffmpeg/x11grab or native Windows desktop capture")
	}
	if err := enforceManagedLeaseCapabilities(cfg, server, leaseID); err != nil {
		return err
	}
	repo, err := findRepo()
	if err != nil {
		return err
	}
	if err := a.claimResolvedLeaseTargetForRepoAndRegister(ctx, leaseID, serverSlug(server), cfg, server, target, repo.Root, *reclaim); err != nil {
		return err
	}
	a.touchLeaseTargetBestEffort(ctx, cfg, LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, "")
	if err := waitForLoopbackVNC(ctx, &target); err != nil {
		return err
	}
	terminalTitle := desktopTerminalWindowTitle(leaseID)
	terminalCommand, err := desktopTerminalCommand(target, command, desktopTerminalOptions{
		FontSize: *fontSize,
		Cols:     *cols,
		Rows:     *rows,
		Sixel:    *sixel,
		Title:    terminalTitle,
	})
	if err != nil {
		return err
	}
	workdir := remoteJoin(cfg, leaseID, repo.Name)
	env, err := requestedCapabilityEnv(ctx, cfg, target)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*record) != "" {
		if err := rejectWaylandDesktopVideoEnv(env, "desktop terminal --record"); err != nil {
			return err
		}
	}
	rescueCtx := rescueContext{Cfg: cfg, Target: target, LeaseID: leaseID}
	if out, err := runDesktopLaunchRemoteCombinedOutput(ctx, target, desktopLaunchRemoteCommand(target, workdir, env, terminalCommand, desktopLaunchOptions{
		VerifyProcess:      true,
		VisibleWindowTitle: terminalTitle,
	})); err != nil {
		printRescue(a.Stdout, classifyDesktopFailure(out), trimFailureDetail(out), desktopDoctorCommand(rescueCtx), desktopLaunchRetryCommand(rescueCtx, terminalCommand))
		return exit(5, "launch desktop terminal: %v", err)
	}
	fmt.Fprintf(a.Stdout, "launched terminal: %s\n", strings.Join(terminalCommand, " "))
	if strings.TrimSpace(*screenshot) != "" || strings.TrimSpace(*record) != "" {
		delay := *waitVisible
		if delay <= 0 {
			delay = 2 * time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	} else if *waitVisible > 0 {
		timer := time.NewTimer(*waitVisible)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	if path := strings.TrimSpace(*screenshot); path != "" {
		if err := captureDesktopScreenshot(ctx, cfg, target, path); err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "screenshot: %s\n", path)
	}
	if path := strings.TrimSpace(*diagnostics); path != "" {
		if err := writeDesktopRecorderDiagnostics(ctx, target, path); err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "diagnostics: %s\n", path)
	}
	if path := strings.TrimSpace(*record); path != "" {
		if err := captureDesktopVideo(ctx, target, path, *recordDuration, *recordFPS); err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "video: %s\n", path)
		if contactPath, err := writeContactSheetForVideo(ctx, path, contactFlags, target.ChildEnvDenylist); err != nil {
			printContactSheetWarning(a.Stdout, err)
		} else if contactPath != "" {
			fmt.Fprintf(a.Stdout, "contact-sheet: %s\n", contactPath)
		}
		if opts, ok, err := publishOptionsFromDesktopFlags(filepath.Dir(path), publishFlags); err != nil {
			return err
		} else if ok {
			opts.ChildEnvDenylist = append([]string(nil), target.ChildEnvDenylist...)
			if err := writeProofMetadata(filepath.Join(opts.Directory, "metadata.json"), desktopProofMetadata{
				CreatedAt:      time.Now().UTC().Format(time.RFC3339),
				Version:        currentVersion(),
				LeaseID:        leaseID,
				Slug:           serverSlug(server),
				Provider:       cfg.Provider,
				Network:        string(cfg.Network),
				TargetOS:       target.TargetOS,
				Command:        command,
				TerminalCols:   *cols,
				TerminalRows:   *rows,
				TerminalSixel:  *sixel,
				RecordDuration: recordDuration.String(),
				RecordFPS:      *recordFPS,
			}); err != nil {
				return err
			}
			published, markdownPath, manifestPath, err := a.publishArtifactDirectory(ctx, opts)
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
			fmt.Fprintf(a.Stdout, "markdown: %s\n", markdownPath)
			if manifestPath != "" {
				fmt.Fprintf(a.Stdout, "manifest: %s\n", manifestPath)
			}
		}
	}
	return nil
}

func (a App) desktopRecord(ctx context.Context, args []string) error {
	return a.artifactsVideo(ctx, args)
}

func (a App) desktopProof(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("desktop proof", a.Stderr)
	provider := fs.String("provider", defaults.Provider, providerHelpSSH())
	id := fs.String("id", "", "lease id or slug")
	output := fs.String("output", "", "proof artifact directory")
	fontSize := fs.Int("font-size", 14, "terminal font size")
	cols := fs.Int("cols", 100, "terminal columns")
	rows := fs.Int("rows", 32, "terminal rows")
	sixel := fs.Bool("sixel", false, "prefer a Sixel-capable terminal configuration")
	waitVisible := fs.Duration("wait-visible", 2*time.Second, "delay after launch before capture")
	recordDuration := fs.Duration("record-duration", 5*time.Second, "recording duration")
	recordFPS := fs.Float64("record-fps", 8, "recording frames per second")
	contactFlags := registerContactSheetFlags(fs)
	publishFlags := registerDesktopPublishFlags(fs)
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	providerFlags := registerProviderFlags(fs, defaults)
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	markDesktopPublishExplicitFlags(fs, &publishFlags)
	if err := validateContactSheetFlags("desktop proof", contactFlags); err != nil {
		return err
	}
	if *recordDuration <= 0 {
		return exit(2, "desktop proof --record-duration must be positive")
	}
	if *recordFPS <= 0 {
		return exit(2, "desktop proof --record-fps must be positive")
	}
	positionalID := false
	if shouldConsumeDesktopTerminalPositionalID(*provider, *id, fs.NArg()) {
		*id = fs.Arg(0)
		positionalID = true
	}
	cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{LeaseID: *id, Desktop: true})
	if err != nil {
		return err
	}
	if err := applyProviderFlags(&cfg, fs, providerFlags); err != nil {
		return err
	}
	if err := validateRequestedCapabilities(cfg); err != nil {
		return err
	}
	if *id == "" && !isStaticProvider(cfg.Provider) {
		return exit(2, "usage: crabbox desktop proof --id <lease-id-or-slug> -- <command...>")
	}
	command := fs.Args()
	if positionalID && len(command) > 0 && command[0] == *id {
		command = command[1:]
	}
	command = trimCommandSeparator(command)
	server, target, leaseID, err := a.resolveNetworkLeaseTargetForRepoWithConfig(ctx, &cfg, *id, false, *reclaim)
	if err != nil {
		return err
	}
	if !supportsDesktopVideoTarget(target) {
		return exit(2, "desktop proof currently requires target=linux with ffmpeg/x11grab or native Windows desktop capture")
	}
	if err := enforceManagedLeaseCapabilities(cfg, server, leaseID); err != nil {
		return err
	}
	repo, err := findRepo()
	if err != nil {
		return err
	}
	if err := a.claimResolvedLeaseTargetForRepoAndRegister(ctx, leaseID, serverSlug(server), cfg, server, target, repo.Root, *reclaim); err != nil {
		return err
	}
	a.touchLeaseTargetBestEffort(ctx, cfg, LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, "")
	if err := waitForLoopbackVNC(ctx, &target); err != nil {
		return err
	}
	dir := strings.TrimSpace(*output)
	if dir == "" {
		name := normalizeLeaseSlug(firstNonBlank(serverSlug(server), leaseID))
		if name == "" {
			name = time.Now().UTC().Format("20060102-150405")
		}
		dir = filepath.Join("artifacts", name+"-proof")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return exit(2, "create proof directory: %v", err)
	}
	terminalTitle := desktopTerminalWindowTitle(leaseID)
	terminalCommand, err := desktopTerminalCommand(target, command, desktopTerminalOptions{
		FontSize: *fontSize,
		Cols:     *cols,
		Rows:     *rows,
		Sixel:    *sixel,
		Title:    terminalTitle,
	})
	if err != nil {
		return err
	}
	workdir := remoteJoin(cfg, leaseID, repo.Name)
	env, err := requestedCapabilityEnv(ctx, cfg, target)
	if err != nil {
		return err
	}
	if err := rejectWaylandDesktopVideoEnv(env, "desktop proof"); err != nil {
		return err
	}
	rescueCtx := rescueContext{Cfg: cfg, Target: target, LeaseID: leaseID}
	if out, err := runDesktopLaunchRemoteCombinedOutput(ctx, target, desktopLaunchRemoteCommand(target, workdir, env, terminalCommand, desktopLaunchOptions{
		VerifyProcess:      true,
		VisibleWindowTitle: terminalTitle,
	})); err != nil {
		printRescue(a.Stdout, classifyDesktopFailure(out), trimFailureDetail(out), desktopDoctorCommand(rescueCtx), desktopLaunchRetryCommand(rescueCtx, terminalCommand))
		return exit(5, "launch desktop proof terminal: %v", err)
	}
	fmt.Fprintf(a.Stdout, "launched terminal: %s\n", strings.Join(terminalCommand, " "))
	if *waitVisible > 0 {
		timer := time.NewTimer(*waitVisible)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	metadataPath := filepath.Join(dir, "metadata.json")
	if err := writeProofMetadata(metadataPath, desktopProofMetadata{
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
		Version:        currentVersion(),
		LeaseID:        leaseID,
		Slug:           serverSlug(server),
		Provider:       cfg.Provider,
		Network:        string(cfg.Network),
		TargetOS:       target.TargetOS,
		Command:        command,
		TerminalCols:   *cols,
		TerminalRows:   *rows,
		TerminalSixel:  *sixel,
		RecordDuration: recordDuration.String(),
		RecordFPS:      *recordFPS,
	}); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "metadata: %s\n", metadataPath)
	screenshotPath := filepath.Join(dir, "screenshot.png")
	if err := captureDesktopScreenshot(ctx, cfg, target, screenshotPath); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "screenshot: %s\n", screenshotPath)
	diagnosticsPath := filepath.Join(dir, "diagnostics.txt")
	if err := writeDesktopRecorderDiagnostics(ctx, target, diagnosticsPath); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "diagnostics: %s\n", diagnosticsPath)
	videoPath := filepath.Join(dir, "screen.mp4")
	if err := captureDesktopVideo(ctx, target, videoPath, *recordDuration, *recordFPS); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "video: %s\n", videoPath)
	if contactPath, err := writeContactSheetForVideo(ctx, videoPath, contactFlags, target.ChildEnvDenylist); err != nil {
		printContactSheetWarning(a.Stdout, err)
	} else if contactPath != "" {
		fmt.Fprintf(a.Stdout, "contact-sheet: %s\n", contactPath)
	}
	fmt.Fprintf(a.Stdout, "proof: %s\n", dir)
	if opts, ok, err := publishOptionsFromDesktopFlags(dir, publishFlags); err != nil {
		return err
	} else if ok {
		opts.ChildEnvDenylist = append([]string(nil), target.ChildEnvDenylist...)
		published, markdownPath, manifestPath, err := a.publishArtifactDirectory(ctx, opts)
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
		fmt.Fprintf(a.Stdout, "markdown: %s\n", markdownPath)
		if manifestPath != "" {
			fmt.Fprintf(a.Stdout, "manifest: %s\n", manifestPath)
		}
	}
	return nil
}

func shouldConsumeDesktopTerminalPositionalID(provider, id string, argCount int) bool {
	return id == "" && argCount > 0 && !isStaticProvider(provider)
}

func trimCommandSeparator(command []string) []string {
	if len(command) > 0 && command[0] == "--" {
		return command[1:]
	}
	return command
}

func supportsDesktopVideoTarget(target SSHTarget) bool {
	return target.TargetOS == targetLinux || isWindowsNativeTarget(target)
}

func rejectWaylandDesktopVideoEnv(env map[string]string, command string) error {
	if isWaylandDesktopEnv(env["CRABBOX_DESKTOP_ENV"]) {
		return exit(2, "%s does not support Wayland desktop envs yet; video capture currently requires an X11 desktop", command)
	}
	return nil
}

type desktopTerminalOptions struct {
	FontSize int
	Cols     int
	Rows     int
	Sixel    bool
	Title    string
}

func desktopTerminalCommand(target SSHTarget, command []string, opts desktopTerminalOptions) ([]string, error) {
	if opts.FontSize <= 0 {
		opts.FontSize = 14
	}
	if opts.Cols <= 0 {
		opts.Cols = 100
	}
	if opts.Rows <= 0 {
		opts.Rows = 32
	}
	if strings.TrimSpace(opts.Title) == "" {
		opts.Title = "Crabbox Desktop"
	}
	if isWindowsNativeTarget(target) {
		shellCommand := ""
		if len(command) > 0 {
			shellCommand = shellJoin(command)
		}
		if opts.Sixel {
			prefix := "export TERM=xterm-256color GIFGREP_INLINE=${GIFGREP_INLINE:-sixel}; "
			if shellCommand == "" {
				shellCommand = prefix + "exec /usr/bin/bash -l"
			} else {
				shellCommand = prefix + shellCommand
			}
		} else if shellCommand == "" {
			shellCommand = "exec /usr/bin/bash -l"
		}
		return []string{
			`C:\Program Files\Git\usr\bin\mintty.exe`,
			"-t", opts.Title,
			"-o", fmt.Sprintf("FontHeight=%d", opts.FontSize),
			"-o", fmt.Sprintf("Columns=%d", opts.Cols),
			"-o", fmt.Sprintf("Rows=%d", opts.Rows),
			"-o", "Scrollbar=none",
			"/usr/bin/bash", "-lc", shellCommand,
		}, nil
	}
	if target.TargetOS == targetMacOS {
		shellCommand := "exec /bin/zsh -l"
		if len(command) > 0 {
			shellCommand = shellJoin(command)
		}
		prefix := "export TERM=${TERM:-xterm-ghostty}; export GIFGREP_INLINE=${GIFGREP_INLINE:-kitty}; export GIFGREP_SOFTWARE_ANIM=${GIFGREP_SOFTWARE_ANIM:-1}; "
		return []string{
			"open", "-W", "-na", "Ghostty.app", "--args",
			"--title=" + opts.Title,
			fmt.Sprintf("--font-size=%d", opts.FontSize),
			fmt.Sprintf("--window-width=%d", opts.Cols),
			fmt.Sprintf("--window-height=%d", opts.Rows),
			"--window-padding-x=14",
			"--window-padding-y=14",
			"--background-opacity=1",
			"--macos-titlebar-style=native",
			"--window-save-state=never",
			"--quit-after-last-window-closed=true",
			"-e", "/bin/zsh", "-lc", prefix + shellCommand,
		}, nil
	}
	if len(command) == 0 {
		command = []string{"bash", "-l"}
	}
	shellCommand := shellJoin(command)
	terminalScript := fmt.Sprintf(`if [ -f /var/lib/crabbox/desktop.env ]; then . /var/lib/crabbox/desktop.env; fi
if [ "${CRABBOX_DESKTOP_ENV:-xfce}" != "xfce" ]; then
  export XDG_RUNTIME_DIR WAYLAND_DISPLAY
  if [ "${CRABBOX_DESKTOP_ENV:-}" = "gnome" ] && command -v gnome-terminal >/dev/null 2>&1; then
    export DISPLAY="${DISPLAY:-:0}"
    export GDK_BACKEND=x11 MOZ_ENABLE_WAYLAND=0
    exec gnome-terminal --wait --title=%s --working-directory="$PWD" -- bash -lc %s
  fi
  command -v foot >/dev/null 2>&1 || { echo "missing foot; warm a new Wayland desktop lease or install foot" >&2; exit 127; }
  exec foot --title=%s --font=%s bash -lc %s
fi
export DISPLAY="${DISPLAY:-:99}"
exec xterm -title %s -fa monospace -fs %s -geometry %s -e bash -lc %s`,
		shellQuote(opts.Title),
		shellQuote(shellCommand),
		shellQuote(opts.Title),
		shellQuote(fmt.Sprintf("monospace:size=%d", opts.FontSize)),
		shellQuote(shellCommand),
		shellQuote(opts.Title),
		shellQuote(fmt.Sprintf("%d", opts.FontSize)),
		shellQuote(fmt.Sprintf("%dx%d", opts.Cols, opts.Rows)),
		shellQuote(shellCommand),
	)
	return []string{"sh", "-lc", terminalScript}, nil
}

func desktopTerminalWindowTitle(leaseID string) string {
	suffix := normalizeLeaseSlug(leaseID)
	if suffix == "" {
		suffix = "session"
	}
	return "Crabbox Desktop " + suffix
}

func shellJoin(args []string) string {
	var b bytes.Buffer
	writeShellArgv(&b, args)
	return b.String()
}

func desktopLaunchWebVNCArgs(cfg Config, target SSHTarget, leaseID string, openPortal, takeControl bool) []string {
	targetOS := firstNonBlank(target.TargetOS, cfg.TargetOS)
	args := []string{"--provider", cfg.Provider, "--target", targetOS, "--id", leaseID}
	if cfg.Network != "" && cfg.Network != NetworkAuto {
		args = append(args, "--network", string(cfg.Network))
	}
	windowsMode := firstNonBlank(target.WindowsMode, cfg.WindowsMode)
	if targetOS == targetWindows && windowsMode != "" {
		args = append(args, "--windows-mode", windowsMode)
	}
	args = append(args, leaseCommandRoutingArgs(cfg, leaseID)...)
	if openPortal {
		args = append(args, "--open")
	}
	if takeControl {
		args = append(args, "--take-control")
	}
	return args
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

type desktopLaunchOptions struct {
	WindowedBrowser    bool
	VerifyProcess      bool
	VisibleWindowTitle string
}

func desktopLaunchRemoteCommand(target SSHTarget, workdir string, env map[string]string, command []string, opts desktopLaunchOptions) string {
	if isWindowsNativeTarget(target) {
		return windowsDesktopLaunchRemoteCommand(workdir, env, command)
	}
	return posixDesktopLaunchRemoteCommand(target, workdir, env, command, opts)
}

func posixDesktopLaunchRemoteCommand(target SSHTarget, workdir string, env map[string]string, command []string, opts desktopLaunchOptions) string {
	var b bytes.Buffer
	command = desktopMacOpenWaitCommand(target, command)
	b.WriteString("set -eu\n")
	if workdir != "" {
		b.WriteString("mkdir -p " + shellQuote(workdir) + "\n")
		b.WriteString("cd " + shellQuote(workdir) + "\n")
	}
	for key, value := range env {
		b.WriteString(key + "=" + shellQuote(value) + "\n")
		b.WriteString("export " + key + "\n")
	}
	b.WriteString("log=${TMPDIR:-/tmp}/crabbox-desktop-launch.log\n")
	if opts.VerifyProcess && target.TargetOS == targetLinux {
		b.WriteString(posixDesktopLaunchWindowSnapshotCommand())
	}
	b.WriteString("if command -v setsid >/dev/null 2>&1; then\n")
	b.WriteString("  setsid ")
	writeShellArgv(&b, command)
	b.WriteString(" >\"$log\" 2>&1 < /dev/null &\n")
	b.WriteString("else\n")
	b.WriteString("  nohup ")
	writeShellArgv(&b, command)
	b.WriteString(" >\"$log\" 2>&1 < /dev/null &\n")
	b.WriteString("fi\n")
	if opts.VerifyProcess {
		b.WriteString(posixDesktopLaunchVerificationCommand(target, opts.VisibleWindowTitle))
	}
	if opts.WindowedBrowser {
		b.WriteString(posixWindowBrowserCommand())
	}
	return b.String()
}

func desktopMacOpenWaitCommand(target SSHTarget, command []string) []string {
	if target.TargetOS != targetMacOS || len(command) == 0 || filepath.Base(command[0]) != "open" {
		return command
	}
	for _, arg := range command[1:] {
		if arg == "-W" || arg == "--wait-apps" {
			return command
		}
	}
	out := make([]string, 0, len(command)+1)
	out = append(out, command[0], "-W")
	return append(out, command[1:]...)
}

func posixDesktopLaunchWindowSnapshotCommand() string {
	return `launch_can_check_windows=0
launch_before_windows=""
if command -v xdotool >/dev/null 2>&1 && xdotool getmouselocation >/dev/null 2>&1; then
  launch_can_check_windows=1
  launch_before_windows=" $(xdotool search --onlyvisible --name '.*' 2>/dev/null | tr '\n' ' ' || true) "
  launch_before_active="$(xdotool getactivewindow 2>/dev/null || true)"
fi
desktop_launch_new_window() {
  for launch_window in $(xdotool search --onlyvisible --name '.*' 2>/dev/null || true); do
    case "$launch_before_windows" in
      *" $launch_window "*) ;;
      *) return 0 ;;
    esac
  done
  return 1
}
desktop_launch_observed_window() {
  desktop_launch_new_window && return 0
  launch_active="$(xdotool getactivewindow 2>/dev/null || true)"
  [ -n "$launch_before_active" ] && [ -n "$launch_active" ] && [ "$launch_active" != "$launch_before_active" ] || return 1
  case "$launch_before_windows" in
    *" $launch_active "*) return 0 ;;
  esac
  return 1
}
`
}

// posixDesktopLaunchVerificationCommand keeps detached launch success tied to
// the spawned process and, where X11 exposes it, the intended terminal window.
func posixDesktopLaunchVerificationCommand(target SSHTarget, visibleWindowTitle string) string {
	var b bytes.Buffer
	b.WriteString(`launch_pid=$!
desktop_launch_failed() {
	launch_status="${1:-}"
	if [ -z "$launch_status" ]; then
		set +e
		wait "$launch_pid"
		launch_status=$?
		set -e
	fi
  [ "$launch_status" -ne 0 ] || launch_status=1
  echo "desktop command exited during launch (status=$launch_status)" >&2
  if [ -s "$log" ]; then tail -n 20 "$log" >&2; fi
  exit "$launch_status"
}
sleep 1
`)
	if target.TargetOS == targetLinux && strings.TrimSpace(visibleWindowTitle) == "" {
		b.WriteString(`if ! kill -0 "$launch_pid" >/dev/null 2>&1; then
  set +e
  wait "$launch_pid"
  launch_status=$?
  set -e
  [ "$launch_status" -eq 0 ] || desktop_launch_failed "$launch_status"
  launch_visible=0
  if [ "$launch_can_check_windows" -eq 1 ]; then
    launch_attempt=0
    while [ "$launch_attempt" -lt 10 ]; do
      if desktop_launch_observed_window; then
        launch_visible=1
        break
      fi
      launch_attempt=$((launch_attempt + 1))
      sleep 0.5
    done
  fi
  [ "$launch_visible" -eq 1 ] || desktop_launch_failed 1
fi
`)
	} else {
		b.WriteString("kill -0 \"$launch_pid\" >/dev/null 2>&1 || desktop_launch_failed\n")
	}
	if target.TargetOS != targetLinux || strings.TrimSpace(visibleWindowTitle) == "" {
		return b.String()
	}
	b.WriteString(`if [ -f /var/lib/crabbox/desktop.env ]; then . /var/lib/crabbox/desktop.env; fi
if [ "${CRABBOX_DESKTOP_ENV:-xfce}" != "wayland" ] && command -v xdotool >/dev/null 2>&1; then
  if [ "${CRABBOX_DESKTOP_ENV:-xfce}" = "xfce" ]; then
    export DISPLAY="${DISPLAY:-:99}"
  else
    export DISPLAY="${DISPLAY:-:0}"
  fi
  launch_visible=0
  launch_attempt=0
  while [ "$launch_attempt" -lt 10 ]; do
    if [ "$launch_can_check_windows" -eq 1 ] && desktop_launch_new_window; then
      launch_visible=1
      break
    fi
    kill -0 "$launch_pid" >/dev/null 2>&1 || desktop_launch_failed
    launch_attempt=$((launch_attempt + 1))
    sleep 0.5
  done
  if [ "$launch_visible" -ne 1 ]; then
    echo "desktop window not visible: ` + shellQuote(visibleWindowTitle) + `" >&2
    if [ -s "$log" ]; then tail -n 20 "$log" >&2; fi
    kill "$launch_pid" >/dev/null 2>&1 || true
    exit 1
  fi
fi
`)
	return b.String()
}

func posixWindowBrowserCommand() string {
	return `(
  sleep 2
  if [ -f /var/lib/crabbox/desktop.env ]; then . /var/lib/crabbox/desktop.env; fi
  if [ "${CRABBOX_DESKTOP_ENV:-xfce}" != "xfce" ]; then
    export XDG_RUNTIME_DIR WAYLAND_DISPLAY
    exit 0
  fi
  export DISPLAY="${DISPLAY:-:99}"
  if command -v wmctrl >/dev/null 2>&1; then
    wmctrl -r :ACTIVE: -b remove,fullscreen,maximized_vert,maximized_horz >/dev/null 2>&1 || true
  fi
  if command -v xdotool >/dev/null 2>&1; then
    window="$(xdotool search --onlyvisible --class google-chrome 2>/dev/null | tail -1 || true)"
    if [ -z "$window" ]; then
      window="$(xdotool search --onlyvisible --class chromium 2>/dev/null | tail -1 || true)"
    fi
    if [ -n "$window" ]; then
      xdotool windowactivate "$window" windowmove "$window" 80 80 windowsize "$window" 1500 900 >/dev/null 2>&1 || true
    fi
  fi
) >/dev/null 2>&1 &
`
}

func desktopBrowserLaunchCheckCommand() string {
	return `set +e
export DISPLAY="${DISPLAY:-:99}"
sleep 5
if command -v xdotool >/dev/null 2>&1; then
  window="$(xdotool search --onlyvisible --class google-chrome 2>/dev/null | tail -1 || true)"
  [ -n "$window" ] || window="$(xdotool search --onlyvisible --class chromium 2>/dev/null | tail -1 || true)"
  if [ -n "$window" ]; then
    exit 0
  fi
  echo "browser window not visible on DISPLAY=$DISPLAY" >&2
fi
if command -v pgrep >/dev/null 2>&1 && {
  pgrep -x google-chrome >/dev/null 2>&1 ||
  pgrep -x chrome >/dev/null 2>&1 ||
  pgrep -x chromium >/dev/null 2>&1 ||
  pgrep -x chromium-browser >/dev/null 2>&1
}; then
  exit 0
fi
echo "browser process not found" >&2
exit 1`
}

func desktopBrowserDarkModeCommand(browser string) string {
	return `set +e
export DISPLAY="${DISPLAY:-:99}"
if [ -x /usr/local/bin/crabbox-configure-desktop-theme ]; then
  CRABBOX_DESKTOP_USER="$(id -un)" /usr/local/bin/crabbox-configure-desktop-theme >/dev/null 2>&1 || true
fi
browser_wrapper=` + shellQuote(strings.TrimSpace(browser)) + `
if [ "$browser_wrapper" = "/usr/local/bin/crabbox-browser" ] && [ -f "$browser_wrapper" ] && {
  ! grep -q -- "--force-dark-mode" "$browser_wrapper" 2>/dev/null ||
  ! grep -q -- "desktop-theme" "$browser_wrapper" 2>/dev/null ||
  ! grep -q -- "--user-data-dir" "$browser_wrapper" 2>/dev/null ||
  { [ -f /var/lib/crabbox/desktop.env ] && grep -q '^CRABBOX_DESKTOP_ENV=gnome$' /var/lib/crabbox/desktop.env && ! grep -q -- "--ozone-platform=x11" "$browser_wrapper" 2>/dev/null; } ||
  { [ -f /var/lib/crabbox/desktop.env ] && grep -q '^CRABBOX_DESKTOP_ENV=wayland$' /var/lib/crabbox/desktop.env && ! grep -q -- "--ozone-platform=wayland" "$browser_wrapper" 2>/dev/null; }
}; then
  browser_path="$(sed -n 's/^exec "\([^"]*\)".*/\1/p' "$browser_wrapper" | head -1)"
  if [ -n "$browser_path" ] && "$browser_path" --version 2>/dev/null | grep -Eiq 'chrome|chromium'; then
    tmp="$(mktemp)"
    if [ -f /var/lib/crabbox/desktop.env ] && grep -q '^CRABBOX_DESKTOP_ENV=gnome$' /var/lib/crabbox/desktop.env; then
      printf '%s\n' '#!/bin/sh' 'if [ -f /var/lib/crabbox/desktop.env ]; then . /var/lib/crabbox/desktop.env; fi' 'export DISPLAY="${DISPLAY:-:0}"' 'export XDG_RUNTIME_DIR WAYLAND_DISPLAY' 'export GDK_BACKEND=x11 MOZ_ENABLE_WAYLAND=0' 'profile="${CRABBOX_BROWSER_PROFILE:-$HOME/.cache/crabbox/browser-profile}"' 'theme="$(cat "${CRABBOX_DESKTOP_THEME_FILE:-$HOME/.config/crabbox/desktop-theme}" 2>/dev/null || printf dark)"' 'umask 077' 'mkdir -p "$profile"' 'chmod 700 "$profile"' 'if [ "$theme" = light ]; then' "  exec \"$browser_path\" --no-first-run --no-default-browser-check --disable-default-apps --hide-crash-restore-bubble --blink-settings=preferredColorScheme=1 --user-data-dir=\"\$profile\" --ozone-platform=x11 --window-size=1500,900 --window-position=80,80 \"\$@\"" 'fi' "exec \"$browser_path\" --no-first-run --no-default-browser-check --disable-default-apps --hide-crash-restore-bubble --force-dark-mode --enable-features=WebUIDarkMode --blink-settings=preferredColorScheme=2 --user-data-dir=\"\$profile\" --ozone-platform=x11 --window-size=1500,900 --window-position=80,80 \"\$@\"" > "$tmp"
    elif [ -f /var/lib/crabbox/desktop.env ] && grep -q '^CRABBOX_DESKTOP_ENV=wayland$' /var/lib/crabbox/desktop.env; then
      printf '%s\n' '#!/bin/sh' 'if [ -f /var/lib/crabbox/desktop.env ]; then . /var/lib/crabbox/desktop.env; fi' 'export XDG_RUNTIME_DIR WAYLAND_DISPLAY' 'export MOZ_ENABLE_WAYLAND=1' 'profile="${CRABBOX_BROWSER_PROFILE:-$HOME/.cache/crabbox/browser-profile}"' 'theme="$(cat "${CRABBOX_DESKTOP_THEME_FILE:-$HOME/.config/crabbox/desktop-theme}" 2>/dev/null || printf dark)"' 'umask 077' 'mkdir -p "$profile"' 'chmod 700 "$profile"' 'if [ "$theme" = light ]; then' "  exec \"$browser_path\" --no-first-run --no-default-browser-check --disable-default-apps --hide-crash-restore-bubble --blink-settings=preferredColorScheme=1 --user-data-dir=\"\$profile\" --ozone-platform=wayland --window-size=1500,900 --window-position=80,80 \"\$@\"" 'fi' "exec \"$browser_path\" --no-first-run --no-default-browser-check --disable-default-apps --hide-crash-restore-bubble --force-dark-mode --enable-features=WebUIDarkMode --blink-settings=preferredColorScheme=2 --user-data-dir=\"\$profile\" --ozone-platform=wayland --window-size=1500,900 --window-position=80,80 \"\$@\"" > "$tmp"
    else
      printf '%s\n' '#!/bin/sh' 'profile="${CRABBOX_BROWSER_PROFILE:-$HOME/.cache/crabbox/browser-profile}"' 'theme="$(cat "${CRABBOX_DESKTOP_THEME_FILE:-$HOME/.config/crabbox/desktop-theme}" 2>/dev/null || printf dark)"' 'umask 077' 'mkdir -p "$profile"' 'chmod 700 "$profile"' 'if [ "$theme" = light ]; then' "  exec \"$browser_path\" --no-first-run --no-default-browser-check --disable-default-apps --hide-crash-restore-bubble --blink-settings=preferredColorScheme=1 --user-data-dir=\"\$profile\" --window-size=1500,900 --window-position=80,80 \"\$@\"" 'fi' "exec \"$browser_path\" --no-first-run --no-default-browser-check --disable-default-apps --hide-crash-restore-bubble --force-dark-mode --enable-features=WebUIDarkMode --blink-settings=preferredColorScheme=2 --user-data-dir=\"\$profile\" --window-size=1500,900 --window-position=80,80 \"\$@\"" > "$tmp"
    fi
    chmod 0755 "$tmp"
    sudo install -m 0755 "$tmp" "$browser_wrapper" >/dev/null 2>&1 || install -m 0755 "$tmp" "$browser_wrapper" >/dev/null 2>&1 || true
    rm -f "$tmp"
  fi
fi
exit 0`
}

func desktopCommandLooksLikeBrowser(command []string, browserEnv string) bool {
	if len(command) == 0 {
		return false
	}
	first := strings.TrimSpace(command[0])
	if first == "" {
		return false
	}
	if strings.TrimSpace(browserEnv) != "" && first == strings.TrimSpace(browserEnv) {
		return true
	}
	lower := strings.ToLower(filepath.Base(first))
	return strings.Contains(lower, "chrome") || strings.Contains(lower, "chromium")
}

func writeShellArgv(b *bytes.Buffer, command []string) {
	for i, arg := range command {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(shellQuote(arg))
	}
}

func windowsDesktopLaunchRemoteCommand(workdir string, env map[string]string, command []string) string {
	return windowsDesktopLaunchPowerShell(workdir, env, command)
}

func windowsDesktopLaunchPowerShell(workdir string, env map[string]string, command []string) string {
	inner := windowsDesktopLaunchScript(workdir, env, command)
	return `$ErrorActionPreference = "Stop"
$base = "C:\ProgramData\crabbox"
New-Item -ItemType Directory -Force -Path $base | Out-Null
$launchID = [Guid]::NewGuid().ToString("N")
$script = Join-Path $base ("desktop-launch-" + $launchID + ".ps1")
$result = Join-Path $base ("desktop-launch-" + $launchID + ".result")
Set-Content -Encoding UTF8 -LiteralPath $script -Value ` + psQuote(inner) + `
$serviceName = "CrabboxDesktopLauncher"
$requestDirectory = Join-Path $base "desktop-launch-requests"
$request = Join-Path $requestDirectory ("desktop-launch-" + $launchID + ".request")
$requestTemp = $request + ".tmp"
$service = Get-Service -Name $serviceName -ErrorAction SilentlyContinue
if ($null -eq $service) {
` + windowsDesktopLauncherServicePowerShell() + `
  $service = Get-Service -Name $serviceName -ErrorAction Stop
}
try {
  New-Item -ItemType Directory -Force -Path $requestDirectory | Out-Null
  Set-Content -Encoding UTF8 -LiteralPath $requestTemp -Value @($script, $result)
  Move-Item -Force -LiteralPath $requestTemp -Destination $request
  if ($service.Status -ne "Running") { Start-Service -Name $serviceName }
  $deadline = [DateTime]::UtcNow.AddSeconds(45)
  while ([DateTime]::UtcNow -lt $deadline -and -not (Test-Path -LiteralPath $result)) {
    Start-Sleep -Milliseconds 200
  }
  if (-not (Test-Path -LiteralPath $result)) { throw "desktop window not visible after direct interactive-session launch" }
  $launchResult = (Get-Content -Raw -LiteralPath $result).Trim()
  if ($launchResult.StartsWith("CRABBOX_DESKTOP_ERROR message=")) {
    $encodedMessage = $launchResult.Substring("CRABBOX_DESKTOP_ERROR message=".Length)
    throw [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($encodedMessage))
  }
  if (-not $launchResult.StartsWith("CRABBOX_DESKTOP_WINDOW ")) { throw "invalid interactive desktop launch result" }
  Write-Output $launchResult
} finally {
  Remove-Item -Force -LiteralPath $requestTemp -ErrorAction SilentlyContinue
  Remove-Item -Force -LiteralPath $request -ErrorAction SilentlyContinue
  Remove-Item -Force -LiteralPath $script -ErrorAction SilentlyContinue
  Remove-Item -Force -LiteralPath $result -ErrorAction SilentlyContinue
}
`
}

func runDesktopLaunchRemoteCombinedOutput(ctx context.Context, target SSHTarget, remote string) (string, error) {
	if !isWindowsNativeTarget(target) {
		return runSSHCombinedOutput(ctx, target, remote)
	}
	var output bytes.Buffer
	command := `powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "$path=Join-Path $env:TEMP ('crabbox-desktop-launch-command-'+[Guid]::NewGuid().ToString('N')+'.ps1');$source=[Console]::In.ReadToEnd();[IO.File]::WriteAllText($path,$source,(New-Object Text.UTF8Encoding($false)));try{& powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File $path;$code=$LASTEXITCODE}finally{Remove-Item -Force -LiteralPath $path -ErrorAction SilentlyContinue};if($null -eq $code){$code=0};exit $code"`
	err := runSSHInput(
		ctx,
		target,
		command,
		strings.NewReader(remote),
		&output,
		&output,
	)
	return strings.TrimSpace(output.String()), err
}

func windowsDesktopLaunchScript(workdir string, env map[string]string, command []string) string {
	var b bytes.Buffer
	b.WriteString("param([Parameter(Mandatory=$true)][string]$ResultPath)\n")
	b.WriteString("$ErrorActionPreference = \"Stop\"\n")
	b.WriteString(`$windowSource = @'
using System;
using System.Collections.Generic;
using System.Diagnostics;
using System.IO;
using System.Runtime.InteropServices;
using System.Text;
using System.Threading;

public sealed class CrabboxDesktopWindow {
    public long Handle;
    public int ProcessId;
    public string Title;
}

public static class CrabboxDesktopWindows {
    private const uint TH32CS_SNAPPROCESS = 0x00000002;
    private const int SW_RESTORE = 9;

    [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)]
    private struct PROCESSENTRY32 {
        public uint dwSize;
        public uint cntUsage;
        public uint th32ProcessID;
        public IntPtr th32DefaultHeapID;
        public uint th32ModuleID;
        public uint cntThreads;
        public uint th32ParentProcessID;
        public int pcPriClassBase;
        public uint dwFlags;
        [MarshalAs(UnmanagedType.ByValTStr, SizeConst = 260)]
        public string szExeFile;
    }

    private delegate bool EnumWindowsProc(IntPtr window, IntPtr parameter);
    [DllImport("user32.dll")]
    private static extern bool EnumWindows(EnumWindowsProc callback, IntPtr parameter);
    [DllImport("user32.dll")]
    private static extern bool IsWindowVisible(IntPtr window);
    [DllImport("user32.dll", CharSet = CharSet.Unicode)]
    private static extern int GetWindowTextLength(IntPtr window);
    [DllImport("user32.dll", CharSet = CharSet.Unicode)]
    private static extern int GetWindowText(IntPtr window, StringBuilder title, int count);
    [DllImport("user32.dll")]
    private static extern uint GetWindowThreadProcessId(IntPtr window, out int processId);
    [DllImport("user32.dll")]
    private static extern bool SetForegroundWindow(IntPtr window);
    [DllImport("user32.dll")]
    private static extern bool BringWindowToTop(IntPtr window);
    [DllImport("user32.dll")]
    private static extern bool ShowWindowAsync(IntPtr window, int command);
    [DllImport("user32.dll")]
    private static extern IntPtr GetForegroundWindow();
    [DllImport("kernel32.dll")]
    private static extern uint GetCurrentThreadId();
    [DllImport("user32.dll")]
    private static extern bool AttachThreadInput(uint attach, uint attachTo, bool attached);
    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern IntPtr CreateToolhelp32Snapshot(uint flags, uint processId);
    [DllImport("kernel32.dll", CharSet = CharSet.Unicode)]
    private static extern bool Process32FirstW(IntPtr snapshot, ref PROCESSENTRY32 entry);
    [DllImport("kernel32.dll", CharSet = CharSet.Unicode)]
    private static extern bool Process32NextW(IntPtr snapshot, ref PROCESSENTRY32 entry);
    [DllImport("kernel32.dll")]
    private static extern bool CloseHandle(IntPtr handle);

    public static CrabboxDesktopWindow[] Visible() {
        List<CrabboxDesktopWindow> windows = new List<CrabboxDesktopWindow>();
        EnumWindows(delegate(IntPtr window, IntPtr parameter) {
            if (!IsWindowVisible(window)) return true;
            int length = GetWindowTextLength(window);
            if (length <= 0) return true;
            int processId;
            GetWindowThreadProcessId(window, out processId);
            StringBuilder title = new StringBuilder(length + 1);
            GetWindowText(window, title, title.Capacity);
            if (title.Length > 0) {
                windows.Add(new CrabboxDesktopWindow { Handle = window.ToInt64(), ProcessId = processId, Title = title.ToString() });
            }
            return true;
        }, IntPtr.Zero);
        return windows.ToArray();
    }

    public static bool RelatedTo(int candidateProcessId, int launchedProcessId, string expectedImage) {
        if (candidateProcessId == launchedProcessId) return true;
        try {
            string candidateImage = Process.GetProcessById(candidateProcessId).ProcessName;
            if (string.Equals(candidateImage, expectedImage, StringComparison.OrdinalIgnoreCase)) return true;
        } catch { }
        Dictionary<int, int> parents = new Dictionary<int, int>();
        IntPtr snapshot = CreateToolhelp32Snapshot(TH32CS_SNAPPROCESS, 0);
        if (snapshot == new IntPtr(-1)) return false;
        try {
            PROCESSENTRY32 entry = new PROCESSENTRY32();
            entry.dwSize = (uint)Marshal.SizeOf(typeof(PROCESSENTRY32));
            if (Process32FirstW(snapshot, ref entry)) {
                do {
                    parents[(int)entry.th32ProcessID] = (int)entry.th32ParentProcessID;
                } while (Process32NextW(snapshot, ref entry));
            }
        } finally {
            CloseHandle(snapshot);
        }
        int current = candidateProcessId;
        for (int depth = 0; depth < 64 && parents.ContainsKey(current); depth++) {
            current = parents[current];
            if (current == launchedProcessId) return true;
            if (current <= 0) break;
        }
        return false;
    }

    public static bool Activate(long handle) {
        IntPtr window = new IntPtr(handle);
        DateTime deadline = DateTime.UtcNow.AddSeconds(3);
        do {
            ShowWindowAsync(window, SW_RESTORE);
            BringWindowToTop(window);
            SetForegroundWindow(window);
            if (GetForegroundWindow() == window) return true;

            int ignored;
            uint currentThread = GetCurrentThreadId();
            uint targetThread = GetWindowThreadProcessId(window, out ignored);
            IntPtr foreground = GetForegroundWindow();
            uint foregroundThread = foreground == IntPtr.Zero ? 0 : GetWindowThreadProcessId(foreground, out ignored);
            bool targetAttached = targetThread != 0 && targetThread != currentThread && AttachThreadInput(currentThread, targetThread, true);
            bool foregroundAttached = foregroundThread != 0 && foregroundThread != currentThread && foregroundThread != targetThread && AttachThreadInput(currentThread, foregroundThread, true);
            try {
                BringWindowToTop(window);
                SetForegroundWindow(window);
            } finally {
                if (foregroundAttached) AttachThreadInput(currentThread, foregroundThread, false);
                if (targetAttached) AttachThreadInput(currentThread, targetThread, false);
            }
            if (GetForegroundWindow() == window) return true;
            Thread.Sleep(100);
        } while (DateTime.UtcNow < deadline);
        return false;
    }
}
'@

function Write-LaunchError([string]$message) {
  $encoded = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($message))
  Write-LaunchResult ("CRABBOX_DESKTOP_ERROR message=" + $encoded)
}

function Write-LaunchResult([string]$value) {
  $temporaryResult = $ResultPath + ".tmp-" + [Guid]::NewGuid().ToString("N")
  Set-Content -NoNewline -Encoding ASCII -LiteralPath $temporaryResult -Value $value
  Move-Item -Force -LiteralPath $temporaryResult -Destination $ResultPath
}

try {
  Add-Type -TypeDefinition $windowSource -Language CSharp
  $before = @{}
  foreach ($window in [CrabboxDesktopWindows]::Visible()) { $before[[string]$window.Handle] = $window.Title }
`)
	if workdir != "" {
		b.WriteString("  New-Item -ItemType Directory -Force -Path " + psQuote(workdir) + " | Out-Null\n")
		b.WriteString("  Set-Location -LiteralPath " + psQuote(workdir) + "\n")
	}
	for key, value := range env {
		b.WriteString("  $env:" + key + " = " + psQuote(value) + "\n")
	}
	b.WriteString("  $file = " + psQuote(command[0]) + "\n")
	b.WriteString("  $arguments = @(")
	for i, arg := range command[1:] {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(psQuote(arg))
	}
	b.WriteString(")\n")
	b.WriteString(`  function Q([string]$s){if($null -eq $s){$s=""};if($s.Length -gt 0 -and $s -notmatch '[\s"]'){return $s};$r='"';$bs=0;foreach($ch in $s.ToCharArray()){if($ch -eq '\'){$bs++;continue};if($ch -eq '"'){if($bs -gt 0){$r+=('\'*($bs*2))};$r+='\"';$bs=0;continue};if($bs -gt 0){$r+=('\'*$bs);$bs=0};$r+=$ch};if($bs -gt 0){$r+=('\'*($bs*2))};$r+='"';return $r}
  $psi=New-Object System.Diagnostics.ProcessStartInfo
  $psi.FileName=$file
  $psi.Arguments=(($arguments|ForEach-Object{Q $_}) -join ' ')
  $psi.WorkingDirectory=(Get-Location).Path
  $psi.UseShellExecute=$false
  $psi.WindowStyle=[System.Diagnostics.ProcessWindowStyle]::Normal
  $launchedProcess=[System.Diagnostics.Process]::Start($psi)
  $expectedImage=[IO.Path]::GetFileNameWithoutExtension($file)
  $deadline=[DateTime]::UtcNow.AddSeconds(30)
  $launchedWindow=$null
  while ([DateTime]::UtcNow -lt $deadline -and $null -eq $launchedWindow) {
    $candidates=@()
    foreach ($window in [CrabboxDesktopWindows]::Visible()) {
      $key=[string]$window.Handle
      $changed=-not $before.ContainsKey($key) -or $before[$key] -ne $window.Title
      if ($changed -and [CrabboxDesktopWindows]::RelatedTo($window.ProcessId, $launchedProcess.Id, $expectedImage)) { $candidates += $window }
    }
    if ($candidates.Count -gt 0) {
      $launchedWindow=$candidates | Where-Object { $_.ProcessId -eq $launchedProcess.Id } | Select-Object -First 1
      if ($null -eq $launchedWindow) { $launchedWindow=$candidates | Select-Object -First 1 }
    }
    if ($null -eq $launchedWindow) { Start-Sleep -Milliseconds 200 }
  }
  if ($null -eq $launchedWindow) { throw "desktop window not visible after launch" }
  if (-not [CrabboxDesktopWindows]::Activate($launchedWindow.Handle)) { throw "desktop window could not be brought to the foreground" }
  $sessionId=[System.Diagnostics.Process]::GetCurrentProcess().SessionId
  $encodedTitle=[Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($launchedWindow.Title))
  Write-LaunchResult ("CRABBOX_DESKTOP_WINDOW pid={0} session={1} title={2}" -f $launchedWindow.ProcessId, $sessionId, $encodedTitle)
} catch {
  Write-LaunchError $_.Exception.Message
  exit 1
}
`)
	return b.String()
}
