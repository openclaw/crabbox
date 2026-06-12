package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
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
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	cfg.Provider = *provider
	cfg.Desktop = true
	cfg.Browser = *browser
	if err := applyTargetFlagOverrides(&cfg, fs, targetFlags); err != nil {
		return err
	}
	if err := applyNetworkModeFlagOverride(&cfg, fs, networkFlags); err != nil {
		return err
	}
	if err := validateRequestedCapabilities(cfg); err != nil {
		return err
	}
	if *webvnc && (isBlacksmithProvider(cfg.Provider) || isStaticProvider(cfg.Provider)) {
		return exit(2, "desktop launch --webvnc is unavailable for provider=%s", cfg.Provider)
	}
	if *id == "" && !isStaticProvider(cfg.Provider) {
		return exit(2, "usage: crabbox desktop launch --id <lease-id-or-slug> [--browser] [--url <url>] -- <command...>")
	}
	server, target, leaseID, err := a.resolveNetworkLeaseTarget(ctx, cfg, *id, false)
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
	if err := claimLeaseTargetForRepoConfig(leaseID, serverSlug(server), cfg, server, target, repo.Root, cfg.IdleTimeout, *reclaim); err != nil {
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
	if out, err := runSSHCombinedOutput(ctx, target, desktopLaunchRemoteCommand(target, workdir, env, command, *browser && !*fullscreen)); err != nil {
		printRescue(a.Stdout, classifyDesktopFailure(out), trimFailureDetail(out), desktopDoctorCommand(rescueCtx), desktopLaunchRetryCommand(rescueCtx, command))
		return exit(5, "launch desktop command: %v", err)
	}
	if expectBrowserLaunch && target.TargetOS == targetLinux {
		if out, err := runSSHCombinedOutput(ctx, target, desktopBrowserLaunchCheckCommand()); err != nil {
			printRescue(a.Stdout, rescueBrowserNotLaunched, trimFailureDetail(out), desktopDoctorCommand(rescueCtx), desktopLaunchRetryCommand(rescueCtx, command))
			return exit(5, "browser not launched for %s: %v", leaseID, err)
		}
	}
	fmt.Fprintf(a.Stdout, "launched: %s\n", strings.Join(command, " "))
	if *webvnc {
		return a.webvnc(ctx, desktopLaunchWebVNCArgs(cfg, target, leaseID, *openPortal, *takeControl))
	}
	return nil
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
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	cfg.Provider = *provider
	cfg.Desktop = true
	if err := applyTargetFlagOverrides(&cfg, fs, targetFlags); err != nil {
		return err
	}
	if err := applyNetworkModeFlagOverride(&cfg, fs, networkFlags); err != nil {
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
	server, target, leaseID, err := a.resolveNetworkLeaseTarget(ctx, cfg, *id, false)
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
	if err := claimLeaseTargetForRepoConfig(leaseID, serverSlug(server), cfg, server, target, repo.Root, cfg.IdleTimeout, *reclaim); err != nil {
		return err
	}
	a.touchLeaseTargetBestEffort(ctx, cfg, LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, "")
	if err := waitForLoopbackVNC(ctx, &target); err != nil {
		return err
	}
	terminalCommand, err := desktopTerminalCommand(target, command, desktopTerminalOptions{
		FontSize: *fontSize,
		Cols:     *cols,
		Rows:     *rows,
		Sixel:    *sixel,
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
	if out, err := runSSHCombinedOutput(ctx, target, desktopLaunchRemoteCommand(target, workdir, env, terminalCommand, false)); err != nil {
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
		if contactPath, err := writeContactSheetForVideo(ctx, path, contactFlags); err != nil {
			printContactSheetWarning(a.Stdout, err)
		} else if contactPath != "" {
			fmt.Fprintf(a.Stdout, "contact-sheet: %s\n", contactPath)
		}
		if opts, ok, err := publishOptionsFromDesktopFlags(filepath.Dir(path), publishFlags); err != nil {
			return err
		} else if ok {
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
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	cfg.Provider = *provider
	cfg.Desktop = true
	if err := applyTargetFlagOverrides(&cfg, fs, targetFlags); err != nil {
		return err
	}
	if err := applyNetworkModeFlagOverride(&cfg, fs, networkFlags); err != nil {
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
	server, target, leaseID, err := a.resolveNetworkLeaseTarget(ctx, cfg, *id, false)
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
	if err := claimLeaseTargetForRepoConfig(leaseID, serverSlug(server), cfg, server, target, repo.Root, cfg.IdleTimeout, *reclaim); err != nil {
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
	terminalCommand, err := desktopTerminalCommand(target, command, desktopTerminalOptions{
		FontSize: *fontSize,
		Cols:     *cols,
		Rows:     *rows,
		Sixel:    *sixel,
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
	if out, err := runSSHCombinedOutput(ctx, target, desktopLaunchRemoteCommand(target, workdir, env, terminalCommand, false)); err != nil {
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
	if contactPath, err := writeContactSheetForVideo(ctx, videoPath, contactFlags); err != nil {
		printContactSheetWarning(a.Stdout, err)
	} else if contactPath != "" {
		fmt.Fprintf(a.Stdout, "contact-sheet: %s\n", contactPath)
	}
	fmt.Fprintf(a.Stdout, "proof: %s\n", dir)
	if opts, ok, err := publishOptionsFromDesktopFlags(dir, publishFlags); err != nil {
		return err
	} else if ok {
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
			"open", "-na", "Ghostty.app", "--args",
			"--title=gifgrep tui",
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
    exec gnome-terminal --working-directory="$PWD" -- bash -lc %s
  fi
  command -v foot >/dev/null 2>&1 || { echo "missing foot; warm a new Wayland desktop lease or install foot" >&2; exit 127; }
  exec foot --title='Crabbox Desktop' --font=%s bash -lc %s
fi
export DISPLAY="${DISPLAY:-:99}"
exec xterm -fa monospace -fs %s -geometry %s -e bash -lc %s`,
		shellQuote(shellCommand),
		shellQuote(fmt.Sprintf("monospace:size=%d", opts.FontSize)),
		shellQuote(shellCommand),
		shellQuote(fmt.Sprintf("%d", opts.FontSize)),
		shellQuote(fmt.Sprintf("%dx%d", opts.Cols, opts.Rows)),
		shellQuote(shellCommand),
	)
	return []string{"sh", "-lc", terminalScript}, nil
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

func desktopLaunchRemoteCommand(target SSHTarget, workdir string, env map[string]string, command []string, windowedBrowser bool) string {
	if isWindowsNativeTarget(target) {
		return windowsDesktopLaunchRemoteCommand(workdir, env, command)
	}
	if target.TargetOS == targetMacOS {
		return posixDesktopLaunchRemoteCommand(workdir, env, command, windowedBrowser)
	}
	return posixDesktopLaunchRemoteCommand(workdir, env, command, windowedBrowser)
}

func posixDesktopLaunchRemoteCommand(workdir string, env map[string]string, command []string, windowedBrowser bool) string {
	var b bytes.Buffer
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
	b.WriteString("if command -v setsid >/dev/null 2>&1; then\n")
	b.WriteString("  setsid ")
	writeShellArgv(&b, command)
	b.WriteString(" >\"$log\" 2>&1 < /dev/null &\n")
	b.WriteString("else\n")
	b.WriteString("  nohup ")
	writeShellArgv(&b, command)
	b.WriteString(" >\"$log\" 2>&1 < /dev/null &\n")
	b.WriteString("fi\n")
	if windowedBrowser {
		b.WriteString(posixWindowBrowserCommand())
	}
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
	inner := windowsDesktopLaunchScript(workdir, env, command)
	return `$ErrorActionPreference = "Stop"
$base = "C:\ProgramData\crabbox"
$usernamePath = Join-Path $base "windows.username"
$passwordPath = Join-Path $base "windows.password"
$username = if (Test-Path -LiteralPath $usernamePath) { Get-Content -Raw -LiteralPath $usernamePath } else { $env:USERNAME }
$username = $username.Trim()
$password = if (Test-Path -LiteralPath $passwordPath) { (Get-Content -Raw -LiteralPath $passwordPath).Trim() } else { "" }
$taskName = "CrabboxDesktopLaunch-" + [Guid]::NewGuid().ToString("N")
$script = Join-Path $base ($taskName + ".ps1")
Set-Content -Encoding UTF8 -LiteralPath $script -Value ` + psQuote(inner) + `
cmd.exe /c "schtasks.exe /Delete /TN $taskName /F 2>NUL" | Out-Null
$startTime = (Get-Date).AddMinutes(1).ToString("HH:mm")
$createArgs = @("/Create", "/TN", $taskName, "/SC", "ONCE", "/ST", $startTime, "/TR", "powershell.exe -NoProfile -WindowStyle Hidden -ExecutionPolicy Bypass -File $script", "/RU", $username, "/IT", "/F")
& schtasks.exe @createArgs | Out-Null
if ($LASTEXITCODE -ne 0 -and $password -ne "") {
  & schtasks.exe @($createArgs + @("/RP", $password)) | Out-Null
}
if ($LASTEXITCODE -ne 0) { throw "failed to create interactive desktop launch task" }
& schtasks.exe /Run /TN $taskName | Out-Null
Start-Sleep -Seconds 2
& schtasks.exe /Delete /TN $taskName /F | Out-Null
Remove-Item -Force -LiteralPath $script -ErrorAction SilentlyContinue
`
}

func windowsDesktopLaunchScript(workdir string, env map[string]string, command []string) string {
	var b bytes.Buffer
	b.WriteString("$ErrorActionPreference = \"Stop\"\n")
	if workdir != "" {
		b.WriteString("New-Item -ItemType Directory -Force -Path " + psQuote(workdir) + " | Out-Null\n")
		b.WriteString("Set-Location -LiteralPath " + psQuote(workdir) + "\n")
	}
	for key, value := range env {
		b.WriteString("$env:" + key + " = " + psQuote(value) + "\n")
	}
	b.WriteString("$file = " + psQuote(command[0]) + "\n")
	b.WriteString("$arguments = @(")
	for i, arg := range command[1:] {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(psQuote(arg))
	}
	b.WriteString(")\n")
	b.WriteString(`function Q([string]$s){if($null -eq $s){$s=""};if($s.Length -gt 0 -and $s -notmatch '[\s"]'){return $s};$r='"';$bs=0;foreach($ch in $s.ToCharArray()){if($ch -eq '\'){$bs++;continue};if($ch -eq '"'){if($bs -gt 0){$r+=('\'*($bs*2))};$r+='\"';$bs=0;continue};if($bs -gt 0){$r+=('\'*$bs);$bs=0};$r+=$ch};if($bs -gt 0){$r+=('\'*($bs*2))};$r+='"';return $r}
$psi=New-Object System.Diagnostics.ProcessStartInfo
$psi.FileName=$file
$psi.Arguments=(($arguments|ForEach-Object{Q $_}) -join ' ')
$psi.WorkingDirectory=(Get-Location).Path
$psi.UseShellExecute=$false
$psi.WindowStyle=[System.Diagnostics.ProcessWindowStyle]::Normal
[System.Diagnostics.Process]::Start($psi)|Out-Null
`)
	return b.String()
}
