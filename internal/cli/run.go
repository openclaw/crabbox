package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (a App) warmup(ctx context.Context, args []string) error {
	started := time.Now()
	defaults := defaultConfig()
	fs := newFlagSet("warmup", a.Stderr)
	provider := fs.String("provider", defaults.Provider, "provider: hetzner, aws, or blacksmith-testbox")
	profile := fs.String("profile", defaults.Profile, "profile")
	class := fs.String("class", defaults.Class, "machine class")
	serverType := fs.String("type", getenv("CRABBOX_SERVER_TYPE", ""), "provider server/instance type")
	ttl := fs.Duration("ttl", defaults.TTL, "maximum lease lifetime")
	idleTimeout := fs.Duration("idle-timeout", defaults.IdleTimeout, "idle timeout")
	keep := fs.Bool("keep", true, "keep server after warmup")
	actionsRunner := fs.Bool("actions-runner", false, "register this box as an ephemeral GitHub Actions runner")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	blacksmithFlags := registerBlacksmithFlags(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	cfg.Provider = *provider
	cfg.Profile = *profile
	cfg.Class = *class
	if flagWasSet(fs, "type") {
		cfg.ServerType = *serverType
	}
	if cfg.ServerType == "" || ((flagWasSet(fs, "provider") || flagWasSet(fs, "class")) && !flagWasSet(fs, "type")) {
		cfg.ServerType = serverTypeForProviderClass(cfg.Provider, *class)
	}
	if flagWasSet(fs, "ttl") {
		cfg.TTL = *ttl
	}
	if flagWasSet(fs, "idle-timeout") {
		cfg.IdleTimeout = *idleTimeout
	}
	applyBlacksmithFlagOverrides(&cfg, fs, blacksmithFlags)
	if cfg.TTL <= 0 {
		return exit(2, "ttl must be positive")
	}
	if cfg.IdleTimeout <= 0 {
		return exit(2, "idle timeout must be positive")
	}
	repo, err := findRepo()
	if err != nil {
		return err
	}
	if isBlacksmithProvider(cfg.Provider) {
		if *actionsRunner {
			return exit(2, "--actions-runner is not supported for provider=%s; Blacksmith owns runner hydration", cfg.Provider)
		}
		return a.blacksmithWarmup(ctx, cfg, repo, *keep, *reclaim)
	}

	coord, useCoordinator, err := newCoordinatorClient(cfg)
	if err != nil {
		return err
	}
	var server Server
	var target SSHTarget
	var leaseID string
	if useCoordinator {
		server, target, leaseID, err = a.acquireCoordinatorWithRetry(ctx, cfg, coord, *keep)
	} else {
		server, target, leaseID, err = a.acquireWithRetry(ctx, cfg, *keep)
	}
	if err != nil {
		return err
	}
	if err := claimLeaseForRepo(leaseID, serverSlug(server), repo.Root, cfg.IdleTimeout, *reclaim); err != nil {
		a.releaseAcquiredLeaseBestEffort(ctx, cfg, coord, useCoordinator, server, target, leaseID)
		return err
	}
	fmt.Fprintf(a.Stdout, "leased %s slug=%s provider=%s server=%s type=%s ip=%s idle_timeout=%s expires=%s\n", leaseID, blank(serverSlug(server), "-"), cfg.Provider, server.DisplayID(), server.ServerType.Name, target.Host, cfg.IdleTimeout, blank(leaseLabelTimeDisplay(server.Labels["expires_at"]), server.Labels["expires_at"]))
	fmt.Fprintf(a.Stdout, "ready ssh=%s@%s:%s workroot=%s\n", target.User, target.Host, target.Port, cfg.WorkRoot)
	if *actionsRunner {
		ghRepo, err := resolveGitHubRepo(repo, cfg.Actions.Repo)
		if err != nil {
			return err
		}
		if err := a.registerGitHubActionsRunner(ctx, cfg, target, leaseID, serverSlug(server), ghRepo, "", nil); err != nil {
			return err
		}
	}
	fmt.Fprintf(a.Stdout, "warmup complete total=%s\n", time.Since(started).Round(time.Millisecond))
	return nil
}

func (a App) runCommand(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("run", a.Stderr)
	provider := fs.String("provider", defaults.Provider, "provider: hetzner, aws, or blacksmith-testbox")
	profile := fs.String("profile", defaults.Profile, "profile")
	class := fs.String("class", defaults.Class, "machine class")
	serverType := fs.String("type", getenv("CRABBOX_SERVER_TYPE", ""), "provider server/instance type")
	ttl := fs.Duration("ttl", defaults.TTL, "maximum lease lifetime")
	idleTimeout := fs.Duration("idle-timeout", defaults.IdleTimeout, "idle timeout")
	leaseIDFlag := fs.String("id", "", "existing lease or server id")
	keep := fs.Bool("keep", false, "keep server after command")
	noSync := fs.Bool("no-sync", false, "skip rsync")
	syncOnly := fs.Bool("sync-only", false, "sync and exit")
	debugSync := fs.Bool("debug", false, "print detailed sync timing")
	shellMode := fs.Bool("shell", false, "run command through the remote shell")
	checksumSync := fs.Bool("checksum", false, "use checksum rsync instead of size/time")
	forceSyncLarge := fs.Bool("force-sync-large", false, "allow unusually large sync candidates")
	junitResults := fs.String("junit", "", "comma-separated remote JUnit XML paths to record")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	blacksmithFlags := registerBlacksmithFlags(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	command := fs.Args()
	if len(command) > 0 && command[0] == "--" {
		command = command[1:]
	}
	if len(command) == 0 && !*syncOnly {
		return exit(2, "usage: crabbox run [flags] -- <command...>")
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	cfg.Provider = *provider
	cfg.Profile = *profile
	cfg.Class = *class
	if flagWasSet(fs, "type") {
		cfg.ServerType = *serverType
	}
	if cfg.ServerType == "" || ((flagWasSet(fs, "provider") || flagWasSet(fs, "class")) && !flagWasSet(fs, "type")) {
		cfg.ServerType = serverTypeForProviderClass(cfg.Provider, *class)
	}
	if flagWasSet(fs, "ttl") {
		cfg.TTL = *ttl
	}
	if flagWasSet(fs, "idle-timeout") {
		cfg.IdleTimeout = *idleTimeout
	}
	if flagWasSet(fs, "checksum") {
		cfg.Sync.Checksum = *checksumSync
	}
	if *junitResults != "" {
		cfg.Results.JUnit = splitCommaList(*junitResults)
	}
	applyBlacksmithFlagOverrides(&cfg, fs, blacksmithFlags)
	if cfg.TTL <= 0 {
		return exit(2, "ttl must be positive")
	}
	if cfg.IdleTimeout <= 0 {
		return exit(2, "idle timeout must be positive")
	}
	repo, err := findRepo()
	if err != nil {
		return err
	}
	if isBlacksmithProvider(cfg.Provider) {
		return a.blacksmithRun(ctx, cfg, repo, blacksmithRunOptions{
			ID:          *leaseIDFlag,
			Keep:        *keep,
			Reclaim:     *reclaim,
			SyncOnly:    *syncOnly,
			Debug:       *debugSync,
			ShellMode:   *shellMode,
			Command:     command,
			IdleTimeout: cfg.IdleTimeout,
		})
	}

	var server Server
	var target SSHTarget
	var leaseID string
	acquired := false
	coord, useCoordinator, err := newCoordinatorClient(cfg)
	if err != nil {
		return err
	}
	if *leaseIDFlag != "" {
		if useCoordinator {
			var lease CoordinatorLease
			lease, err = coord.GetLease(ctx, *leaseIDFlag)
			if err == nil {
				server, target, leaseID = leaseToServerTarget(lease, cfg)
				if !flagWasSet(fs, "idle-timeout") && lease.IdleTimeoutSeconds > 0 {
					cfg.IdleTimeout = time.Duration(lease.IdleTimeoutSeconds) * time.Second
				}
			}
		} else {
			server, target, leaseID, err = a.findLease(ctx, cfg, *leaseIDFlag)
			if err == nil && !flagWasSet(fs, "idle-timeout") {
				if duration, ok := parseDurationSecondsLabel(server.Labels["idle_timeout_secs"]); ok {
					cfg.IdleTimeout = duration
				} else if duration, ok := parseDurationSecondsLabel(server.Labels["idle_timeout"]); ok {
					cfg.IdleTimeout = duration
				}
			}
		}
	} else {
		if useCoordinator {
			server, target, leaseID, err = a.acquireCoordinatorWithRetry(ctx, cfg, coord, *keep)
		} else {
			server, target, leaseID, err = a.acquireWithRetry(ctx, cfg, *keep)
		}
		acquired = true
	}
	if err != nil {
		return err
	}
	if err := claimLeaseForRepo(leaseID, serverSlug(server), repo.Root, cfg.IdleTimeout, *reclaim); err != nil {
		if acquired && !*keep {
			a.releaseAcquiredLeaseBestEffort(ctx, cfg, coord, useCoordinator, server, target, leaseID)
		}
		return err
	}
	if !useCoordinator && leaseID != "" {
		server = a.touchDirectLeaseBestEffort(ctx, cfg, server, blank(server.Labels["state"], "ready"))
	}
	if acquired {
		defer func() {
			if !*keep {
				a.releaseAcquiredLeaseBestEffort(context.Background(), cfg, coord, useCoordinator, server, target, leaseID)
			}
		}()
	}
	if useCoordinator && leaseID != "" {
		var heartbeatIdleTimeout *time.Duration
		if *leaseIDFlag != "" && flagWasSet(fs, "idle-timeout") {
			heartbeatIdleTimeout = &cfg.IdleTimeout
			if lease, err := coord.UpdateLeaseIdleTimeout(ctx, leaseID, *heartbeatIdleTimeout); err == nil {
				fmt.Fprintf(a.Stderr, "updated idle_timeout=%s expires=%s\n", cfg.IdleTimeout, blank(lease.ExpiresAt, "-"))
			} else {
				return err
			}
		}
		stopHeartbeat := startCoordinatorHeartbeat(ctx, coord, leaseID, cfg.IdleTimeout, heartbeatIdleTimeout, a.Stderr)
		defer stopHeartbeat()
	}

	if cfg.Sync.BaseRef == "" {
		cfg.Sync.BaseRef = repo.BaseRef
	}
	timings := runTimings{started: time.Now()}
	workdir := filepath.ToSlash(filepath.Join(cfg.WorkRoot, leaseID, repo.Name))
	actionsEnvFile := ""
	if state, err := readActionsHydrationState(ctx, target, leaseID); err == nil && state.Workspace != "" {
		workdir = state.Workspace
		actionsEnvFile = state.EnvFile
		fmt.Fprintf(a.Stderr, "using GitHub Actions workspace %s\n", workdir)
	}
	if !*noSync {
		syncStart := time.Now()
		fmt.Fprintf(a.Stderr, "syncing %s -> %s:%s\n", repo.Root, target.Host, workdir)
		stepStart := time.Now()
		if err := waitForSSHReady(ctx, &target, a.Stderr, "before sync", 2*time.Minute); err != nil {
			return err
		}
		timings.syncSteps.sshReady = time.Since(stepStart)
		stepStart = time.Now()
		if err := runSSHQuiet(ctx, target, remoteMkdir(workdir)); err != nil {
			return exit(7, "create remote workdir: %v", err)
		}
		timings.syncSteps.mkdir = time.Since(stepStart)
		stepStart = time.Now()
		manifest, err := syncManifest(repo.Root, configuredExcludes(cfg))
		if err != nil {
			return exit(6, "build sync file list: %v", err)
		}
		timings.syncSteps.manifest = time.Since(stepStart)
		stepStart = time.Now()
		if err := checkSyncPreflight(manifest, cfg, *forceSyncLarge, a.Stderr); err != nil {
			return err
		}
		timings.syncSteps.preflight = time.Since(stepStart)
		fingerprint := ""
		if cfg.Sync.Fingerprint {
			stepStart = time.Now()
			fingerprint, err = syncFingerprintForManifest(repo, cfg, manifest)
			timings.syncSteps.fingerprintLocal = time.Since(stepStart)
			if err != nil {
				fmt.Fprintf(a.Stderr, "warning: sync fingerprint failed: %v\n", err)
			} else if fingerprint != "" {
				stepStart = time.Now()
				remoteFingerprint, err := runSSHOutput(ctx, target, remoteReadSyncFingerprint(workdir))
				timings.syncSteps.fingerprintRemote = time.Since(stepStart)
				if err == nil && remoteFingerprint == fingerprint {
					timings.sync = time.Since(syncStart)
					timings.syncSkipped = true
					fmt.Fprintf(a.Stderr, "No changes detected, skipping sync (%s)\n", timings.sync.Round(time.Millisecond))
					goto afterSync
				}
			}
		}
		if cfg.Sync.GitSeed {
			stepStart = time.Now()
			if err := runSSHQuiet(ctx, target, remoteGitSeed(workdir, repo.RemoteURL, repo.Head)); err != nil {
				fmt.Fprintf(a.Stderr, "warning: remote git seed failed: %v\n", err)
			}
			timings.syncSteps.gitSeed = time.Since(stepStart)
		}
		manifestData := manifest.NUL()
		stepStart = time.Now()
		if err := runSSHInputQuiet(ctx, target, remoteWriteSyncManifestNew(workdir), string(manifestData)); err != nil {
			return exit(7, "write sync manifest: %v", err)
		}
		if err := runSSHInputQuiet(ctx, target, remoteWriteSyncDeletedNew(workdir), string(manifest.DeletedNUL())); err != nil {
			return exit(7, "write sync delete manifest: %v", err)
		}
		timings.syncSteps.manifestWrite = time.Since(stepStart)
		if cfg.Sync.Delete {
			stepStart = time.Now()
			if err := runSSHQuiet(ctx, target, remotePruneSyncManifest(workdir)); err != nil {
				return exit(6, "remote sync prune failed: %v", err)
			}
			timings.syncSteps.prune = time.Since(stepStart)
		}
		stepStart = time.Now()
		if err := rsync(ctx, target, repo.Root, workdir, configuredExcludes(cfg), a.Stdout, a.Stderr, rsyncOptions{Debug: *debugSync, Delete: cfg.Sync.Delete, Checksum: cfg.Sync.Checksum, UseFilesFrom: true, FilesFrom: manifestData, Timeout: cfg.Sync.Timeout, HeartbeatInterval: 15 * time.Second}); err != nil {
			return exit(6, "rsync failed: %v", err)
		}
		timings.syncSteps.rsync = time.Since(stepStart)
		stepStart = time.Now()
		if err := runSSHQuiet(ctx, target, remoteApplySyncManifest(workdir)); err != nil {
			return exit(6, "remote sync manifest apply failed: %v", err)
		}
		timings.syncSteps.manifestApply = time.Since(stepStart)
		stepStart = time.Now()
		if out, err := runSSHCombinedOutput(ctx, target, remoteSyncSanity(workdir, os.Getenv("CRABBOX_ALLOW_MASS_DELETIONS") == "1")); err != nil {
			if out != "" {
				return exit(6, "remote sync sanity failed: %s: %v", out, err)
			}
			return exit(6, "remote sync sanity failed: %v", err)
		}
		timings.syncSteps.sanity = time.Since(stepStart)
		stepStart = time.Now()
		if err := runSSHQuiet(ctx, target, remoteGitHydrate(workdir, cfg.Sync.BaseRef)); err != nil {
			fmt.Fprintf(a.Stderr, "warning: remote git hydrate failed: %v\n", err)
		}
		timings.syncSteps.gitHydrate = time.Since(stepStart)
		if fingerprint != "" {
			stepStart = time.Now()
			if err := runSSHQuiet(ctx, target, remoteWriteSyncFingerprint(workdir, fingerprint)); err != nil {
				fmt.Fprintf(a.Stderr, "warning: write sync fingerprint failed: %v\n", err)
			}
			timings.syncSteps.fingerprintWrite = time.Since(stepStart)
		}
		timings.sync = time.Since(syncStart)
		fmt.Fprintf(a.Stderr, "sync complete in %s\n", timings.sync.Round(time.Millisecond))
	}
afterSync:
	if *syncOnly {
		fmt.Fprintf(a.Stdout, "synced %s\n", workdir)
		fmt.Fprintln(a.Stderr, formatRunSummary(timings, time.Since(timings.started), 0))
		return nil
	}

	commandStart := time.Now()
	if err := waitForSSHReady(ctx, &target, a.Stderr, "before command", 2*time.Minute); err != nil {
		return err
	}
	if *noSync {
		if err := runSSHQuiet(ctx, target, remoteMkdir(workdir)); err != nil {
			return exit(7, "create remote workdir: %v", err)
		}
	}
	if !useCoordinator {
		server = a.touchDirectLeaseBestEffort(context.Background(), cfg, server, "running")
		defer func() {
			server = a.touchDirectLeaseBestEffort(context.Background(), cfg, server, "ready")
		}()
	}
	fmt.Fprintf(a.Stderr, "running on %s %s\n", target.Host, strings.Join(command, " "))
	var runID string
	if useCoordinator && leaseID != "" && coord != nil {
		run, err := coord.CreateRun(ctx, leaseID, cfg, command)
		if err != nil {
			fmt.Fprintf(a.Stderr, "warning: run history create failed: %v\n", err)
		} else {
			runID = run.ID
			fmt.Fprintf(a.Stderr, "recording run %s\n", runID)
		}
	}
	remote := remoteCommandWithEnvFile(workdir, allowedEnv(cfg.EnvAllow), actionsEnvFile, command)
	if *shellMode || shouldUseShell(command) {
		remote = remoteShellCommandWithEnvFile(workdir, allowedEnv(cfg.EnvAllow), actionsEnvFile, strings.Join(command, " "))
	}
	var logBuffer runLogBuffer
	stdout := io.MultiWriter(a.Stdout, &logBuffer)
	stderr := io.MultiWriter(a.Stderr, &logBuffer)
	code := runSSHStream(ctx, target, remote, stdout, stderr)
	timings.command = time.Since(commandStart)
	var results *TestResultSummary
	if len(cfg.Results.JUnit) > 0 {
		results, err = collectRemoteJUnitResults(ctx, target, workdir, cfg.Results.JUnit)
		if err != nil {
			fmt.Fprintf(a.Stderr, "warning: collect test results failed: %v\n", err)
		} else if line := resultSummaryLine(results); line != "" {
			fmt.Fprintln(a.Stderr, line)
		}
	}
	if runID != "" {
		if _, err := coord.FinishRun(context.Background(), runID, code, timings.sync, timings.command, logBuffer.String(), logBuffer.Truncated(), results); err != nil {
			fmt.Fprintf(a.Stderr, "warning: run history finish failed for %s: %v\n", runID, err)
		}
	}
	total := time.Since(timings.started)
	fmt.Fprintf(a.Stderr, "command complete in %s total=%s\n", timings.command.Round(time.Millisecond), total.Round(time.Millisecond))
	fmt.Fprintln(a.Stderr, formatRunSummary(timings, total, code))
	if code != 0 {
		return ExitError{Code: code, Message: fmt.Sprintf("remote command exited %d", code)}
	}
	return nil
}

type runTimings struct {
	started     time.Time
	sync        time.Duration
	command     time.Duration
	syncSteps   syncStepTimings
	syncSkipped bool
}

type syncStepTimings struct {
	sshReady          time.Duration
	mkdir             time.Duration
	manifest          time.Duration
	preflight         time.Duration
	fingerprintLocal  time.Duration
	fingerprintRemote time.Duration
	gitSeed           time.Duration
	manifestWrite     time.Duration
	prune             time.Duration
	rsync             time.Duration
	manifestApply     time.Duration
	sanity            time.Duration
	gitHydrate        time.Duration
	fingerprintWrite  time.Duration
}

func formatRunSummary(timings runTimings, total time.Duration, exitCode int) string {
	summary := fmt.Sprintf("run summary sync=%s command=%s total=%s sync_skipped=%t exit=%d",
		timings.sync.Round(time.Millisecond),
		timings.command.Round(time.Millisecond),
		total.Round(time.Millisecond),
		timings.syncSkipped,
		exitCode,
	)
	if breakdown := formatSyncStepTimings(timings.syncSteps); breakdown != "" {
		summary += " sync_steps=" + breakdown
	}
	return summary
}

func formatSyncStepTimings(steps syncStepTimings) string {
	parts := make([]string, 0, 14)
	appendStep := func(name string, duration time.Duration) {
		if duration > 0 {
			parts = append(parts, fmt.Sprintf("%s:%s", name, duration.Round(time.Millisecond)))
		}
	}
	appendStep("ssh", steps.sshReady)
	appendStep("mkdir", steps.mkdir)
	appendStep("manifest", steps.manifest)
	appendStep("preflight", steps.preflight)
	appendStep("fingerprint", steps.fingerprintLocal)
	appendStep("fingerprint_remote", steps.fingerprintRemote)
	appendStep("git_seed", steps.gitSeed)
	appendStep("manifest_write", steps.manifestWrite)
	appendStep("prune", steps.prune)
	appendStep("rsync", steps.rsync)
	appendStep("manifest_apply", steps.manifestApply)
	appendStep("sanity", steps.sanity)
	appendStep("git_hydrate", steps.gitHydrate)
	appendStep("fingerprint_write", steps.fingerprintWrite)
	return strings.Join(parts, ",")
}

func shouldUseShell(command []string) bool {
	if len(command) == 1 {
		return strings.ContainsAny(command[0], "&|;<>*$`")
	}
	for _, word := range command {
		switch word {
		case "&&", "||", ";", "|", ">", ">>", "<", "2>", "2>>":
			return true
		}
	}
	return false
}

func (a App) acquireCoordinator(ctx context.Context, cfg Config, coord *CoordinatorClient, keep bool) (Server, SSHTarget, string, error) {
	leaseID := newLeaseID()
	slug := newLeaseSlug(leaseID)
	keyPath, publicKey, err := ensureTestboxKey(leaseID)
	if err != nil {
		return Server{}, SSHTarget{}, "", err
	}
	cfg.SSHKey = keyPath
	cfg.ProviderKey = providerKeyForLease(leaseID)
	ensureAWSSSHCIDRs(ctx, &cfg)
	fmt.Fprintf(a.Stderr, "coordinator lease class=%s preferred_type=%s keep=%v slug=%s idle_timeout=%s ttl=%s\n", cfg.Class, cfg.ServerType, keep, slug, cfg.IdleTimeout, cfg.TTL)
	lease, err := coord.CreateLease(ctx, cfg, publicKey, keep, leaseID, slug)
	if err != nil {
		return Server{}, SSHTarget{}, "", err
	}
	if lease.ID != "" && lease.ID != leaseID {
		if err := moveStoredTestboxKey(leaseID, lease.ID); err != nil {
			fmt.Fprintf(a.Stderr, "warning: could not move local key from %s to %s: %v\n", leaseID, lease.ID, err)
		}
	}
	server, target, leaseID := leaseToServerTarget(lease, cfg)
	fmt.Fprintf(a.Stderr, "leased %s slug=%s server=%d type=%s ip=%s via coordinator\n", leaseID, blank(lease.Slug, "-"), server.ID, server.ServerType.Name, target.Host)
	if err := waitForSSH(ctx, &target, a.Stderr); err != nil {
		if !keep {
			if releaseErr := releaseCoordinatorLease(context.Background(), coord, leaseID); releaseErr != nil {
				fmt.Fprintf(a.Stderr, "warning: release failed after bootstrap error for %s: %v\n", leaseID, releaseErr)
			}
		}
		return Server{}, SSHTarget{}, "", err
	}
	return server, target, leaseID, nil
}

func (a App) acquireCoordinatorWithRetry(ctx context.Context, cfg Config, coord *CoordinatorClient, keep bool) (Server, SSHTarget, string, error) {
	var lastErr error
	attempts := acquireAttempts(keep)
	for attempt := 1; attempt <= attempts; attempt++ {
		server, target, leaseID, err := a.acquireCoordinator(ctx, cfg, coord, keep)
		if err == nil {
			return server, target, leaseID, nil
		}
		lastErr = err
		if attempt == attempts || !isBootstrapWaitError(err) {
			return Server{}, SSHTarget{}, "", err
		}
		fmt.Fprintf(a.Stderr, "warning: bootstrap failed; retrying with fresh lease: %v\n", err)
	}
	return Server{}, SSHTarget{}, "", lastErr
}

func (a App) acquireWithRetry(ctx context.Context, cfg Config, keep bool) (Server, SSHTarget, string, error) {
	var lastErr error
	attempts := acquireAttempts(keep)
	for attempt := 1; attempt <= attempts; attempt++ {
		server, target, leaseID, err := a.acquire(ctx, cfg, keep)
		if err == nil {
			return server, target, leaseID, nil
		}
		lastErr = err
		if attempt == attempts || !isBootstrapWaitError(err) {
			return Server{}, SSHTarget{}, "", err
		}
		fmt.Fprintf(a.Stderr, "warning: bootstrap failed; retrying with fresh lease: %v\n", err)
	}
	return Server{}, SSHTarget{}, "", lastErr
}

func acquireAttempts(keep bool) int {
	if keep {
		return 1
	}
	return 2
}

func isBootstrapWaitError(err error) bool {
	var exitErr ExitError
	return AsExitError(err, &exitErr) &&
		exitErr.Code == 5 &&
		strings.Contains(exitErr.Message, "timed out waiting for SSH")
}

func releaseCoordinatorLease(ctx context.Context, coord *CoordinatorClient, leaseID string) error {
	var lastErr error
	for attempt := 1; attempt <= 5; attempt++ {
		releaseCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		_, err := coord.ReleaseLease(releaseCtx, leaseID, true)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		if attempt == 5 {
			break
		}
		time.Sleep(time.Duration(attempt*2) * time.Second)
	}
	return lastErr
}

func (a App) releaseAcquiredLeaseBestEffort(ctx context.Context, cfg Config, coord *CoordinatorClient, useCoordinator bool, server Server, target SSHTarget, leaseID string) {
	a.writeActionsHydrationStopBestEffort(ctx, target, leaseID)
	fmt.Fprintf(a.Stderr, "releasing %s server=%s\n", leaseID, server.DisplayID())
	if useCoordinator {
		if err := releaseCoordinatorLease(ctx, coord, leaseID); err != nil {
			fmt.Fprintf(a.Stderr, "warning: release failed for %s: %v\n", leaseID, err)
		}
	} else if err := deleteServer(ctx, cfg, server); err != nil {
		fmt.Fprintf(a.Stderr, "warning: delete failed for %s: %v\n", leaseID, err)
	}
	removeLeaseClaim(leaseID)
}

func startCoordinatorHeartbeat(ctx context.Context, coord *CoordinatorClient, leaseID string, idleTimeout time.Duration, updateIdleTimeout *time.Duration, stderr io.Writer) func() {
	rootCtx, cancel := context.WithCancel(ctx)
	interval := heartbeatInterval(idleTimeout)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			callCtx, heartbeatCancel := context.WithTimeout(rootCtx, 20*time.Second)
			var err error
			if updateIdleTimeout != nil {
				_, err = coord.UpdateLeaseIdleTimeout(callCtx, leaseID, *updateIdleTimeout)
			} else {
				_, err = coord.TouchLease(callCtx, leaseID)
			}
			heartbeatCancel()
			if err != nil && rootCtx.Err() == nil {
				fmt.Fprintf(stderr, "warning: heartbeat failed for %s: %v\n", leaseID, err)
			}
			select {
			case <-ticker.C:
			case <-rootCtx.Done():
				return
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func heartbeatInterval(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return time.Minute
	}
	interval := ttl / 3
	if interval < 5*time.Second {
		return 5 * time.Second
	}
	if interval > time.Minute {
		return time.Minute
	}
	return interval
}

func (a App) touchLeaseBestEffort(ctx context.Context, cfg Config, identifier, leaseID string) {
	if _, ok, err := newCoordinatorClient(cfg); err == nil && ok {
		if leaseID == "" {
			leaseID = identifier
		}
		a.touchCoordinatorLeaseBestEffort(ctx, cfg, leaseID)
		return
	}
	server, _, _, err := a.findLease(ctx, cfg, identifier)
	if err != nil {
		fmt.Fprintf(a.Stderr, "warning: direct touch failed for %s: %v\n", identifier, err)
		return
	}
	a.touchDirectLeaseBestEffort(ctx, cfg, server, blank(server.Labels["state"], "ready"))
}

func (a App) touchActiveLeaseBestEffort(ctx context.Context, cfg Config, server Server, leaseID string) Server {
	if _, ok, err := newCoordinatorClient(cfg); err == nil && ok {
		a.touchCoordinatorLeaseBestEffort(ctx, cfg, leaseID)
		return server
	}
	return a.touchDirectLeaseBestEffort(ctx, cfg, server, blank(server.Labels["state"], "ready"))
}

func (a App) touchDirectLeaseBestEffort(ctx context.Context, cfg Config, server Server, state string) Server {
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	server.Labels = touchDirectLeaseLabels(server.Labels, cfg, state, time.Now().UTC())
	if cfg.Provider == "aws" || server.Provider == "aws" || strings.HasPrefix(server.CloudID, "i-") {
		client, err := newAWSClient(ctx, cfg)
		if err != nil {
			fmt.Fprintf(a.Stderr, "warning: direct touch state=%s: %v\n", state, err)
			return server
		}
		if err := client.SetTags(ctx, server.CloudID, server.Labels); err != nil {
			fmt.Fprintf(a.Stderr, "warning: direct touch state=%s: %v\n", state, err)
		}
		return server
	}
	client, err := newHetznerClient()
	if err != nil {
		fmt.Fprintf(a.Stderr, "warning: direct touch state=%s: %v\n", state, err)
		return server
	}
	if err := client.SetLabels(ctx, server.ID, server.Labels); err != nil {
		fmt.Fprintf(a.Stderr, "warning: direct touch state=%s: %v\n", state, err)
	}
	return server
}

func (a App) acquire(ctx context.Context, cfg Config, keep bool) (Server, SSHTarget, string, error) {
	if cfg.Provider == "aws" {
		return a.acquireAWS(ctx, cfg, keep)
	}
	client, err := newHetznerClient()
	if err != nil {
		return Server{}, SSHTarget{}, "", err
	}
	leaseID := newLeaseID()
	servers, err := client.ListCrabboxServers(ctx)
	if err != nil {
		return Server{}, SSHTarget{}, "", err
	}
	slug := allocateDirectLeaseSlug(leaseID, servers)
	keyPath, publicKey, err := ensureTestboxKey(leaseID)
	if err != nil {
		return Server{}, SSHTarget{}, "", err
	}
	cfg.SSHKey = keyPath
	cfg.ProviderKey = providerKeyForLease(leaseID)
	if cfg.ProviderKey != "" {
		providerKey, err := client.EnsureSSHKey(ctx, cfg.ProviderKey, publicKey)
		if err != nil {
			return Server{}, SSHTarget{}, "", err
		}
		cfg.ProviderKey = providerKey.Name
	}
	fmt.Fprintf(a.Stderr, "provisioning provider=hetzner lease=%s slug=%s class=%s preferred_type=%s location=%s keep=%v\n", leaseID, slug, cfg.Class, cfg.ServerType, cfg.Location, keep)
	server, cfg, err := client.CreateServerWithFallback(ctx, cfg, publicKey, leaseID, slug, keep, func(format string, args ...any) {
		fmt.Fprintf(a.Stderr, format, args...)
	})
	if err != nil {
		return Server{}, SSHTarget{}, "", err
	}
	fmt.Fprintf(a.Stderr, "provisioned lease=%s server=%d type=%s\n", leaseID, server.ID, cfg.ServerType)
	server, err = waitForServerIP(ctx, client, server.ID)
	if err != nil {
		return Server{}, SSHTarget{}, "", err
	}
	target := SSHTarget{User: cfg.SSHUser, Host: server.PublicNet.IPv4.IP, Key: cfg.SSHKey, Port: cfg.SSHPort}
	if err := waitForSSH(ctx, &target, a.Stderr); err != nil {
		if !keep {
			_ = deleteServer(context.Background(), cfg, server)
		}
		return Server{}, SSHTarget{}, "", err
	}
	server.Labels["state"] = "ready"
	if err := client.SetLabels(ctx, server.ID, server.Labels); err != nil {
		fmt.Fprintf(a.Stderr, "warning: set labels: %v\n", err)
	}
	return server, target, leaseID, nil
}

func (a App) acquireAWS(ctx context.Context, cfg Config, keep bool) (Server, SSHTarget, string, error) {
	cfg = a.chooseAWSRegion(ctx, cfg)
	client, err := newAWSClient(ctx, cfg)
	if err != nil {
		return Server{}, SSHTarget{}, "", err
	}
	leaseID := newLeaseID()
	servers, err := client.ListCrabboxServers(ctx)
	if err != nil {
		return Server{}, SSHTarget{}, "", err
	}
	slug := allocateDirectLeaseSlug(leaseID, servers)
	keyPath, publicKey, err := ensureTestboxKey(leaseID)
	if err != nil {
		return Server{}, SSHTarget{}, "", err
	}
	cfg.SSHKey = keyPath
	cfg.ProviderKey = providerKeyForLease(leaseID)
	ensureAWSSSHCIDRs(ctx, &cfg)
	fmt.Fprintf(a.Stderr, "provisioning provider=aws lease=%s slug=%s class=%s preferred_type=%s region=%s keep=%v market=%s strategy=%s\n", leaseID, slug, cfg.Class, cfg.ServerType, cfg.AWSRegion, keep, cfg.Capacity.Market, cfg.Capacity.Strategy)
	server, cfg, err := client.CreateServerWithFallback(ctx, cfg, publicKey, leaseID, slug, keep, func(format string, args ...any) {
		fmt.Fprintf(a.Stderr, format, args...)
	})
	if err != nil {
		return Server{}, SSHTarget{}, "", err
	}
	fmt.Fprintf(a.Stderr, "provisioned lease=%s server=%s type=%s\n", leaseID, server.DisplayID(), cfg.ServerType)
	server, err = client.waitForServerIP(ctx, server.CloudID)
	if err != nil {
		return Server{}, SSHTarget{}, "", err
	}
	target := SSHTarget{User: cfg.SSHUser, Host: server.PublicNet.IPv4.IP, Key: cfg.SSHKey, Port: cfg.SSHPort}
	if err := waitForSSH(ctx, &target, a.Stderr); err != nil {
		if !keep {
			_ = client.DeleteServer(context.Background(), server.CloudID)
		}
		return Server{}, SSHTarget{}, "", err
	}
	server.Labels["state"] = "ready"
	if err := client.SetTags(ctx, server.CloudID, server.Labels); err != nil {
		fmt.Fprintf(a.Stderr, "warning: set tags: %v\n", err)
	}
	return server, target, leaseID, nil
}

func (a App) chooseAWSRegion(ctx context.Context, cfg Config) Config {
	if cfg.Provider != "aws" || cfg.Capacity.Market != "spot" || len(cfg.Capacity.Regions) < 2 {
		return cfg
	}
	client, err := newAWSClient(ctx, cfg)
	if err != nil {
		fmt.Fprintf(a.Stderr, "warning: spot placement score unavailable: %v\n", err)
		return cfg
	}
	scores, err := client.SpotPlacementScores(ctx, cfg)
	if err != nil {
		fmt.Fprintf(a.Stderr, "warning: spot placement score unavailable: %v\n", err)
		return cfg
	}
	if len(scores) == 0 {
		return cfg
	}
	best := awsString(scores[0].Region)
	score := int32(0)
	if scores[0].Score != nil {
		score = *scores[0].Score
	}
	if best != "" && best != cfg.AWSRegion {
		fmt.Fprintf(a.Stderr, "selected aws region=%s spot_score=%d previous=%s\n", best, score, cfg.AWSRegion)
		cfg.AWSRegion = best
	}
	return cfg
}

func waitForServerIP(ctx context.Context, client *HetznerClient, id int64) (Server, error) {
	deadline := time.Now().Add(5 * time.Minute)
	for {
		server, err := client.GetServer(ctx, id)
		if err != nil {
			return Server{}, err
		}
		if server.PublicNet.IPv4.IP != "" {
			return server, nil
		}
		if time.Now().After(deadline) {
			return Server{}, exit(5, "timed out waiting for server IP")
		}
		time.Sleep(3 * time.Second)
	}
}

func (a App) findLease(ctx context.Context, cfg Config, id string) (Server, SSHTarget, string, error) {
	if cfg.Provider == "aws" {
		return a.findAWSLease(ctx, cfg, id)
	}
	client, err := newHetznerClient()
	if err != nil {
		return Server{}, SSHTarget{}, "", err
	}
	if serverID, ok := parseServerID(id); ok {
		server, err := client.GetServer(ctx, serverID)
		if err != nil {
			return Server{}, SSHTarget{}, "", err
		}
		leaseID := server.Labels["lease"]
		if leaseID == "" {
			leaseID = id
		}
		target := SSHTarget{User: cfg.SSHUser, Host: server.PublicNet.IPv4.IP, Key: cfg.SSHKey, Port: cfg.SSHPort}
		useStoredTestboxKey(&target, leaseID)
		return server, target, leaseID, nil
	}
	servers, err := client.ListCrabboxServers(ctx)
	if err != nil {
		return Server{}, SSHTarget{}, "", err
	}
	if server, leaseID, err := findServerByAlias(servers, id); err != nil {
		return Server{}, SSHTarget{}, "", err
	} else if leaseID != "" {
		target := SSHTarget{User: cfg.SSHUser, Host: server.PublicNet.IPv4.IP, Key: cfg.SSHKey, Port: cfg.SSHPort}
		useStoredTestboxKey(&target, leaseID)
		return server, target, leaseID, nil
	}
	return Server{}, SSHTarget{}, "", exit(4, "lease/server not found: %s", id)
}

func (a App) findAWSLease(ctx context.Context, cfg Config, id string) (Server, SSHTarget, string, error) {
	client, err := newAWSClient(ctx, cfg)
	if err != nil {
		return Server{}, SSHTarget{}, "", err
	}
	if strings.HasPrefix(id, "i-") {
		server, err := client.GetServer(ctx, id)
		if err != nil {
			return Server{}, SSHTarget{}, "", err
		}
		leaseID := server.Labels["lease"]
		if leaseID == "" {
			leaseID = id
		}
		target := SSHTarget{User: cfg.SSHUser, Host: server.PublicNet.IPv4.IP, Key: cfg.SSHKey, Port: cfg.SSHPort}
		useStoredTestboxKey(&target, leaseID)
		return server, target, leaseID, nil
	}
	servers, err := client.ListCrabboxServers(ctx)
	if err != nil {
		return Server{}, SSHTarget{}, "", err
	}
	if server, leaseID, err := findServerByAlias(servers, id); err != nil {
		return Server{}, SSHTarget{}, "", err
	} else if leaseID != "" {
		target := SSHTarget{User: cfg.SSHUser, Host: server.PublicNet.IPv4.IP, Key: cfg.SSHKey, Port: cfg.SSHPort}
		useStoredTestboxKey(&target, leaseID)
		return server, target, leaseID, nil
	}
	return Server{}, SSHTarget{}, "", exit(4, "lease/server not found: %s", id)
}

func findServerByAlias(servers []Server, id string) (Server, string, error) {
	if isCanonicalLeaseID(id) {
		for _, server := range servers {
			if server.Labels["lease"] == id {
				return server, server.Labels["lease"], nil
			}
		}
	}
	matches := make([]Server, 0, 2)
	slug := normalizeLeaseSlug(id)
	for _, server := range servers {
		if serverSlug(server) == slug {
			matches = append(matches, server)
		}
	}
	if len(matches) > 1 {
		var b strings.Builder
		fmt.Fprintf(&b, "slug %q matches multiple active leases:\n", id)
		for _, server := range matches {
			fmt.Fprintf(&b, "  lease=%s slug=%s server=%s host=%s\n", blank(server.Labels["lease"], "-"), blank(serverSlug(server), "-"), server.DisplayID(), server.PublicNet.IPv4.IP)
		}
		return Server{}, "", exit(4, "%s", strings.TrimSpace(b.String()))
	}
	if len(matches) == 1 {
		return matches[0], matches[0].Labels["lease"], nil
	}
	for _, server := range servers {
		if server.Name == id {
			return server, server.Labels["lease"], nil
		}
	}
	return Server{}, "", nil
}

func (a App) stop(ctx context.Context, args []string) error {
	fs := newFlagSet("stop", a.Stderr)
	provider := fs.String("provider", defaultConfig().Provider, "provider: hetzner, aws, or blacksmith-testbox")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return exit(2, "usage: crabbox stop <lease-or-server-id>")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	cfg.Provider = *provider
	if isBlacksmithProvider(cfg.Provider) {
		return a.blacksmithStop(ctx, cfg, fs.Arg(0))
	}
	if coord, ok, err := newCoordinatorClient(cfg); err != nil {
		return err
	} else if ok {
		if lease, err := coord.GetLease(ctx, fs.Arg(0)); err == nil {
			_, target, leaseID := leaseToServerTarget(lease, cfg)
			a.writeActionsHydrationStopBestEffort(ctx, target, leaseID)
		} else {
			fmt.Fprintf(a.Stderr, "warning: could not inspect lease before release: %v\n", err)
		}
		released, err := coord.ReleaseLease(ctx, fs.Arg(0), true)
		if err != nil {
			return err
		}
		removeLeaseClaim(released.ID)
		fmt.Fprintf(a.Stderr, "released lease=%s server=%s\n", released.ID, leaseDisplayID(released))
		return nil
	}
	server, target, leaseID, err := a.findLease(ctx, cfg, fs.Arg(0))
	if err != nil {
		return err
	}
	a.writeActionsHydrationStopBestEffort(ctx, target, leaseID)
	fmt.Fprintf(a.Stderr, "deleting lease=%s server=%s name=%s\n", leaseID, server.DisplayID(), server.Name)
	if err := deleteServer(ctx, cfg, server); err != nil {
		return err
	}
	removeLeaseClaim(leaseID)
	return nil
}

func (a App) writeActionsHydrationStopBestEffort(ctx context.Context, target SSHTarget, leaseID string) {
	if leaseID == "" || target.Host == "" {
		return
	}
	stopCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := writeActionsHydrationStop(stopCtx, target, leaseID); err != nil {
		fmt.Fprintf(a.Stderr, "warning: could not stop GitHub Actions hydration for %s: %v\n", leaseID, err)
	}
}

func leaseDisplayID(lease CoordinatorLease) string {
	if lease.CloudID != "" {
		return lease.CloudID
	}
	return fmt.Sprint(lease.ServerID)
}

func deleteServer(ctx context.Context, cfg Config, server Server) error {
	if cfg.Provider == "aws" || server.Provider == "aws" || strings.HasPrefix(server.CloudID, "i-") {
		client, err := newAWSClient(ctx, cfg)
		if err != nil {
			return err
		}
		if err := client.DeleteServer(ctx, server.CloudID); err != nil {
			return err
		}
		if keyName := serverProviderKey(server); validCrabboxProviderKey(keyName) {
			return client.DeleteSSHKey(ctx, keyName)
		}
		return nil
	}
	client, err := newHetznerClient()
	if err != nil {
		return err
	}
	if err := client.DeleteServer(ctx, server.ID); err != nil {
		return err
	}
	if keyName := serverProviderKey(server); validCrabboxProviderKey(keyName) {
		return client.DeleteSSHKey(ctx, keyName)
	}
	return nil
}

func serverProviderKey(server Server) string {
	if server.Labels != nil && server.Labels["provider_key"] != "" {
		return server.Labels["provider_key"]
	}
	if server.Labels != nil && server.Labels["lease"] != "" {
		return providerKeyForLease(server.Labels["lease"])
	}
	return ""
}

func validCrabboxProviderKey(name string) bool {
	const prefix = "crabbox-cbx-"
	if !strings.HasPrefix(name, prefix) || len(name) != len(prefix)+12 {
		return false
	}
	for _, c := range name[len(prefix):] {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func repoExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
