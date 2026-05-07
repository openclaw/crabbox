package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

func applyCapacityMarketFlag(cfg *Config, fs *flag.FlagSet, market string) error {
	if !flagWasSet(fs, "market") {
		return nil
	}
	switch market {
	case "spot", "on-demand":
		cfg.Capacity.Market = market
		return nil
	default:
		return exit(2, "--market must be spot or on-demand")
	}
}

func applyServerTypeFlagOverrides(cfg *Config, fs *flag.FlagSet, serverType string) {
	if flagWasSet(fs, "type") {
		cfg.ServerType = serverType
		cfg.ServerTypeExplicit = true
		return
	}
	if cfg.ServerTypeExplicit {
		return
	}
	if cfg.ServerType == "" || flagWasSet(fs, "provider") || flagWasSet(fs, "class") || flagWasSet(fs, "target") || flagWasSet(fs, "windows-mode") {
		cfg.ServerType = serverTypeForConfig(*cfg)
	}
}

func (a App) warmup(ctx context.Context, args []string) error {
	started := time.Now()
	defaults := defaultConfig()
	fs := newFlagSet("warmup", a.Stderr)
	leaseFlags := registerLeaseCreateFlags(fs, defaults)
	keep := fs.Bool("keep", true, "keep server after warmup")
	actionsRunner := fs.Bool("actions-runner", false, "register this box as an ephemeral GitHub Actions runner")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	timingJSON := fs.Bool("timing-json", false, "print final timing as JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if err := applyLeaseCreateFlags(&cfg, fs, leaseFlags); err != nil {
		return err
	}
	repo, err := findRepo()
	if err != nil {
		return err
	}
	backend, err := loadBackend(cfg, runtimeForApp(a))
	if err != nil {
		return err
	}
	options := leaseOptionsFromConfig(cfg)
	if delegated, ok := backend.(DelegatedRunBackend); ok {
		return delegated.Warmup(ctx, WarmupRequest{Repo: repo, Options: options, Keep: *keep, Reclaim: *reclaim, ActionsRunner: *actionsRunner, TimingJSON: *timingJSON})
	}
	sshBackend, ok := backend.(SSHLeaseBackend)
	if !ok {
		return exit(2, "provider=%s does not support warmup", backend.Spec().Name)
	}
	if *actionsRunner {
		if err := validateActionsRunnerCapability(backend, cfg); err != nil {
			return err
		}
	}
	lease, err := sshBackend.Acquire(ctx, AcquireRequest{Repo: repo, Options: options, Keep: *keep, Reclaim: *reclaim})
	if err != nil {
		return err
	}
	server, target, leaseID := lease.Server, lease.SSH, lease.LeaseID
	applyResolvedServerConfig(&cfg, server)
	if err := claimLeaseForRepoConfig(leaseID, serverSlug(server), cfg, repo.Root, cfg.IdleTimeout, *reclaim); err != nil {
		a.releaseBackendLeaseBestEffort(ctx, sshBackend, LeaseTarget{Server: server, SSH: target, LeaseID: leaseID, Coordinator: lease.Coordinator})
		return err
	}
	if serverTailscaleMetadata(server).Enabled {
		target = bootstrapNetworkTarget(cfg, server, target)
		if err := waitForSSHReady(ctx, &target, a.Stderr, "tailscale metadata", 2*time.Minute); err == nil {
			a.refreshTailscaleMetadata(ctx, cfg, lease.Coordinator, lease.Coordinator != nil, &server, target, leaseID)
		} else {
			fmt.Fprintf(a.Stderr, "warning: tailscale metadata wait failed: %v\n", err)
		}
	}
	if resolved, err := resolveNetworkTarget(ctx, cfg, server, target); err != nil {
		a.releaseBackendLeaseBestEffort(ctx, sshBackend, LeaseTarget{Server: server, SSH: target, LeaseID: leaseID, Coordinator: lease.Coordinator})
		return err
	} else {
		target = resolved.Target
		if resolved.FallbackReason != "" {
			fmt.Fprintf(a.Stderr, "network fallback %s\n", resolved.FallbackReason)
		}
	}
	network := readyNetworkDisplay(cfg, server, target)
	meta := serverTailscaleMetadata(server)
	tailscaleSummary := ""
	if meta.Enabled {
		tailscaleSummary = " tailscale=" + blank(tailscaleTargetHost(meta), blank(meta.State, "requested"))
	}
	fmt.Fprintf(a.Stdout, "leased %s slug=%s provider=%s server=%s type=%s ip=%s%s idle_timeout=%s expires=%s\n", leaseID, blank(serverSlug(server), "-"), cfg.Provider, server.DisplayID(), server.ServerType.Name, server.PublicNet.IPv4.IP, tailscaleSummary, cfg.IdleTimeout, blank(leaseLabelTimeDisplay(server.Labels["expires_at"]), server.Labels["expires_at"]))
	fmt.Fprintf(a.Stdout, "ready ssh=%s@%s:%s network=%s workroot=%s\n", redactedSSHUser(cfg, server, target), target.Host, target.Port, network, cfg.WorkRoot)
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
	if *timingJSON {
		total := time.Since(started)
		if err := writeTimingJSON(a.Stderr, timingReport{
			Provider: cfg.Provider,
			LeaseID:  leaseID,
			Slug:     serverSlug(server),
			TotalMs:  total.Milliseconds(),
			ExitCode: 0,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (a App) runCommand(ctx context.Context, args []string) (err error) {
	defaults := defaultConfig()
	fs := newFlagSet("run", a.Stderr)
	leaseFlags := registerLeaseCreateFlags(fs, defaults)
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
	timingJSON := fs.Bool("timing-json", false, "print final timing as JSON")
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
	if err := applyLeaseCreateFlags(&cfg, fs, leaseFlags); err != nil {
		return err
	}
	if flagWasSet(fs, "checksum") {
		cfg.Sync.Checksum = *checksumSync
	}
	if *junitResults != "" {
		cfg.Results.JUnit = splitCommaList(*junitResults)
	}
	repo, err := findRepo()
	if err != nil {
		return err
	}
	backend, err := loadBackend(cfg, runtimeForApp(a))
	if err != nil {
		return err
	}
	options := leaseOptionsFromConfig(cfg)
	if delegated, ok := backend.(DelegatedRunBackend); ok {
		_, err := delegated.Run(ctx, RunRequest{
			Repo:           repo,
			ID:             *leaseIDFlag,
			Options:        options,
			Keep:           *keep,
			Reclaim:        *reclaim,
			NoSync:         *noSync,
			SyncOnly:       *syncOnly,
			DebugSync:      *debugSync,
			ShellMode:      *shellMode,
			ChecksumSync:   *checksumSync,
			ForceSyncLarge: *forceSyncLarge,
			Command:        command,
			TimingJSON:     *timingJSON,
		})
		return err
	}
	sshBackend, ok := backend.(SSHLeaseBackend)
	if !ok {
		return exit(2, "provider=%s does not support run", backend.Spec().Name)
	}

	var server Server
	var target SSHTarget
	var leaseID string
	acquired := false
	coord := backendCoordinator(backend)
	useCoordinator := coord != nil
	recorder := &runRecorder{}
	var runFailure error
	recordFailure := func(failure error) error {
		return recordRunFailure(&runFailure, failure)
	}
	if useCoordinator {
		recorder = newRunRecorder(ctx, coord, cfg, command, a.Stderr)
		defer func() {
			recorder.Failed(runFailure)
		}()
		recorder.Event("leasing.started", "leasing", "")
	}
	if *leaseIDFlag != "" {
		var lease LeaseTarget
		lease, err = sshBackend.Resolve(ctx, ResolveRequest{Repo: repo, Options: options, ID: *leaseIDFlag, Reclaim: *reclaim})
		if err == nil {
			server, target, leaseID = lease.Server, lease.SSH, lease.LeaseID
			if resolved, resolveErr := resolveNetworkTarget(ctx, cfg, server, target); resolveErr != nil {
				err = resolveErr
			} else {
				target = resolved.Target
				if resolved.FallbackReason != "" {
					fmt.Fprintf(a.Stderr, "network fallback %s\n", resolved.FallbackReason)
				}
			}
		}
		if err == nil && !flagWasSet(fs, "idle-timeout") {
			if useCoordinator {
				if duration, ok := parseDurationSecondsLabel(server.Labels["idle_timeout_secs"]); ok {
					cfg.IdleTimeout = duration
				}
			} else if duration, ok := parseDurationSecondsLabel(server.Labels["idle_timeout_secs"]); ok {
				cfg.IdleTimeout = duration
			} else if duration, ok := parseDurationSecondsLabel(server.Labels["idle_timeout"]); ok {
				cfg.IdleTimeout = duration
			}
		}
	} else {
		var lease LeaseTarget
		lease, err = sshBackend.Acquire(ctx, AcquireRequest{Repo: repo, Options: options, Keep: *keep, Reclaim: *reclaim})
		if err == nil {
			server, target, leaseID = lease.Server, lease.SSH, lease.LeaseID
		}
		acquired = true
	}
	if err != nil {
		return recordFailure(err)
	}
	applyResolvedServerConfig(&cfg, server)
	if err := enforceManagedLeaseCapabilities(cfg, server, leaseID); err != nil {
		return recordFailure(err)
	}
	if useCoordinator {
		recorder.AttachLease(leaseID, serverSlug(server), cfg)
	}
	if err := claimLeaseForRepoConfig(leaseID, serverSlug(server), cfg, repo.Root, cfg.IdleTimeout, *reclaim); err != nil {
		if acquired && !*keep {
			a.releaseBackendLeaseBestEffort(ctx, sshBackend, LeaseTarget{Server: server, SSH: target, LeaseID: leaseID, Coordinator: coord})
		}
		return recordFailure(err)
	}
	if !useCoordinator && leaseID != "" {
		if touched, touchErr := sshBackend.Touch(ctx, TouchRequest{Lease: LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, State: blank(server.Labels["state"], "ready"), IdleTimeout: cfg.IdleTimeout}); touchErr == nil {
			server = touched
		} else {
			fmt.Fprintf(a.Stderr, "warning: direct touch failed for %s: %v\n", leaseID, touchErr)
		}
	}
	if acquired {
		defer func() {
			if !*keep {
				a.releaseBackendLeaseBestEffort(context.Background(), sshBackend, LeaseTarget{Server: server, SSH: target, LeaseID: leaseID, Coordinator: coord})
				recorder.Event("lease.released", "released", "")
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
				return recordFailure(err)
			}
		}
		stopHeartbeat := startCoordinatorHeartbeat(ctx, coord, leaseID, cfg.IdleTimeout, heartbeatIdleTimeout, leaseTelemetryCollectorForTarget(target), a.Stderr)
		defer stopHeartbeat()
	}

	if cfg.Sync.BaseRef == "" {
		cfg.Sync.BaseRef = repo.BaseRef
	}
	timings := runTimings{started: time.Now()}
	exitNodeEgressChecked := false
	workdir := remoteJoin(cfg, leaseID, repo.Name)
	actionsEnvFile := ""
	actionsURL := ""
	hydratedByActions := false
	if state, err := readActionsHydrationState(ctx, target, leaseID); err == nil && state.Workspace != "" {
		workdir = state.Workspace
		actionsEnvFile = state.EnvFile
		if state.RunID != "" {
			if ghRepo, err := resolveGitHubRepo(repo, cfg.Actions.Repo); err == nil {
				actionsURL = actionsRunURL(ghRepo, state.RunID)
			}
		}
		hydratedByActions = true
		fmt.Fprintf(a.Stderr, "using GitHub Actions workspace %s\n", workdir)
	} else if commandNeedsHydrationHint(command, *shellMode) && cfg.Actions.Workflow != "" {
		fmt.Fprintf(a.Stderr, "warning: no GitHub Actions hydration marker found for %s; JS package commands may fail on a raw box. Run \"crabbox actions hydrate --id %s\" first, or include runtime setup in the command.\n", leaseID, leaseID)
	}
	if !*noSync {
		syncStart := time.Now()
		fmt.Fprintf(a.Stderr, "syncing %s -> %s:%s\n", repo.Root, target.Host, workdir)
		stepStart := time.Now()
		recorder.Event("bootstrap.waiting", "bootstrap", "waiting for SSH before sync")
		target = bootstrapNetworkTarget(cfg, server, target)
		if err := waitForSSHReady(ctx, &target, a.Stderr, "before sync", 2*time.Minute); err != nil {
			return recordFailure(err)
		}
		a.refreshTailscaleMetadata(ctx, cfg, coord, useCoordinator, &server, target, leaseID)
		if resolved, err := resolveNetworkTarget(ctx, cfg, server, target); err != nil {
			return recordFailure(err)
		} else {
			target = resolved.Target
			if resolved.FallbackReason != "" {
				fmt.Fprintf(a.Stderr, "network fallback %s\n", resolved.FallbackReason)
			}
		}
		if !exitNodeEgressChecked {
			if err := validateTailscaleExitNodeEgress(ctx, server, target); err != nil {
				return recordFailure(err)
			}
			exitNodeEgressChecked = true
		}
		recorder.CaptureTelemetryStart(ctx, target)
		recorder.StartTelemetrySampler(ctx, target)
		recorder.Event("sync.started", "sync", "")
		timings.syncSteps.sshReady = time.Since(stepStart)
		excludes, err := syncExcludes(repo.Root, cfg)
		if err != nil {
			return recordFailure(err)
		}
		if isWindowsNativeTarget(target) {
			stepStart = time.Now()
			if err := runSSHQuiet(ctx, target, windowsRemoteMkdir(workdir)); err != nil {
				return recordFailure(exit(7, "create remote workdir: %v", err))
			}
			timings.syncSteps.mkdir = time.Since(stepStart)
		}
		stepStart = time.Now()
		manifest, err := syncManifest(repo.Root, excludes)
		if err != nil {
			return recordFailure(exit(6, "build sync file list: %v", err))
		}
		timings.syncSteps.manifest = time.Since(stepStart)
		stepStart = time.Now()
		if err := checkSyncPreflight(manifest, cfg, *forceSyncLarge, a.Stderr); err != nil {
			return recordFailure(err)
		}
		timings.syncSteps.preflight = time.Since(stepStart)
		if isWindowsNativeTarget(target) {
			stepStart = time.Now()
			if err := syncWindowsNative(ctx, target, repo, cfg, workdir, manifest, a.Stdout, a.Stderr, rsyncOptions{Debug: *debugSync, Delete: cfg.Sync.Delete, Checksum: cfg.Sync.Checksum, Timeout: cfg.Sync.Timeout, HeartbeatInterval: 15 * time.Second}); err != nil {
				return recordFailure(err)
			}
			timings.syncSteps.rsync = time.Since(stepStart)
			timings.sync = time.Since(syncStart)
			fmt.Fprintf(a.Stderr, "sync complete in %s\n", timings.sync.Round(time.Millisecond))
			recorder.Event("sync.finished", "synced", fmt.Sprintf("duration=%s mode=archive", timings.sync.Round(time.Millisecond)))
			goto afterSync
		}
		fingerprint := ""
		if cfg.Sync.Fingerprint {
			stepStart = time.Now()
			fingerprint, err = syncFingerprintForManifest(repo, cfg, manifest, excludes)
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
					recorder.Event("sync.finished", "synced", fmt.Sprintf("duration=%s skipped=true", timings.sync.Round(time.Millisecond)))
					goto afterSync
				}
			}
		}
		if cfg.Sync.GitSeed && remoteGitSeedCandidate(repo) {
			stepStart = time.Now()
			if err := runSSHQuiet(ctx, target, remoteGitSeed(workdir, repo.RemoteURL, repo.Head)); err != nil {
				fmt.Fprintf(a.Stderr, "warning: remote git seed failed: %v\n", err)
			}
			timings.syncSteps.gitSeed = time.Since(stepStart)
		}
		manifestData := manifest.NUL()
		stepStart = time.Now()
		manifestInput := fmt.Sprintf("%d\n", len(manifestData)) + string(manifestData) + string(manifest.DeletedNUL())
		if err := runSSHInputQuiet(ctx, target, remoteWriteSyncManifestsNew(workdir), manifestInput); err != nil {
			return recordFailure(exit(7, "write sync manifests: %v", err))
		}
		timings.syncSteps.manifestWrite = time.Since(stepStart)
		if cfg.Sync.Delete {
			stepStart = time.Now()
			if err := runSSHQuiet(ctx, target, remotePruneSyncManifest(workdir)); err != nil {
				return recordFailure(exit(6, "remote sync prune failed: %v", err))
			}
			timings.syncSteps.prune = time.Since(stepStart)
		}
		stepStart = time.Now()
		if err := rsync(ctx, target, repo.Root, workdir, excludes, a.Stdout, a.Stderr, rsyncOptions{Debug: *debugSync, Delete: cfg.Sync.Delete, Checksum: cfg.Sync.Checksum, UseFilesFrom: true, FilesFrom: manifestData, Timeout: cfg.Sync.Timeout, HeartbeatInterval: 15 * time.Second}); err != nil {
			return recordFailure(exit(6, "rsync failed: %v", err))
		}
		timings.syncSteps.rsync = time.Since(stepStart)
		baseSHA := gitHydrateBaseSHA(repo, cfg.Sync.BaseRef)
		hydrateGit := true
		if hydratedByActions {
			stepStart = time.Now()
			reason, err := runSSHOutput(ctx, target, remoteGitHydrateStatus(workdir, cfg.Sync.BaseRef, baseSHA))
			if err == nil && reason != "" {
				timings.syncSteps.gitHydrateSkipped = true
				timings.syncSteps.gitHydrateSkipReason = reason
				hydrateGit = false
				fmt.Fprintf(a.Stderr, "skipping git hydrate: %s\n", reason)
			}
		}
		stepStart = time.Now()
		finalizeCommand := remoteFinalizeSync(workdir, remoteSyncFinalizeOptions{
			AllowMassDeletions: os.Getenv("CRABBOX_ALLOW_MASS_DELETIONS") == "1",
			HydrateGit:         hydrateGit,
			BaseRef:            cfg.Sync.BaseRef,
			BaseSHA:            baseSHA,
			Fingerprint:        fingerprint,
		})
		if out, err := runSSHCombinedOutput(ctx, target, finalizeCommand); err != nil {
			if out != "" {
				return recordFailure(exit(6, "remote sync finalize failed: %s: %v", out, err))
			}
			return recordFailure(exit(6, "remote sync finalize failed: %v", err))
		}
		timings.syncSteps.finalize = time.Since(stepStart)
		timings.sync = time.Since(syncStart)
		fmt.Fprintf(a.Stderr, "sync complete in %s\n", timings.sync.Round(time.Millisecond))
		recorder.Event("sync.finished", "synced", fmt.Sprintf("duration=%s skipped=false", timings.sync.Round(time.Millisecond)))
	} else {
		timings.syncSkipped = true
		recorder.Event("sync.finished", "synced", "skipped by --no-sync")
	}
afterSync:
	if *syncOnly {
		fmt.Fprintf(a.Stdout, "synced %s\n", workdir)
		fmt.Fprintln(a.Stderr, formatRunSummary(timings, time.Since(timings.started), 0))
		if *timingJSON {
			total := time.Since(timings.started)
			if err := writeTimingJSON(a.Stderr, timingReportFromRunWithActionsURL(cfg.Provider, leaseID, serverSlug(server), timings, total, 0, actionsURL)); err != nil {
				return recordFailure(err)
			}
		}
		recorder.Finish(ctx, target, 0, timings.sync, 0, "", false, nil)
		return nil
	}

	commandStart := time.Now()
	recorder.Event("bootstrap.waiting", "bootstrap", "waiting for SSH before command")
	target = bootstrapNetworkTarget(cfg, server, target)
	if err := waitForSSHReady(ctx, &target, a.Stderr, "before command", 2*time.Minute); err != nil {
		return recordFailure(err)
	}
	a.refreshTailscaleMetadata(ctx, cfg, coord, useCoordinator, &server, target, leaseID)
	if resolved, err := resolveNetworkTarget(ctx, cfg, server, target); err != nil {
		return recordFailure(err)
	} else {
		target = resolved.Target
		if resolved.FallbackReason != "" {
			fmt.Fprintf(a.Stderr, "network fallback %s\n", resolved.FallbackReason)
		}
	}
	if !exitNodeEgressChecked {
		if err := validateTailscaleExitNodeEgress(ctx, server, target); err != nil {
			return recordFailure(err)
		}
		exitNodeEgressChecked = true
	}
	recorder.CaptureTelemetryStart(ctx, target)
	recorder.StartTelemetrySampler(ctx, target)
	if *noSync {
		mkdirCommand := remoteMkdir(workdir)
		if isWindowsNativeTarget(target) {
			mkdirCommand = windowsRemoteMkdir(workdir)
		}
		if err := runSSHQuiet(ctx, target, mkdirCommand); err != nil {
			return recordFailure(exit(7, "create remote workdir: %v", err))
		}
	}
	if !useCoordinator {
		if touched, touchErr := sshBackend.Touch(context.Background(), TouchRequest{Lease: LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, State: "running", IdleTimeout: cfg.IdleTimeout}); touchErr == nil {
			server = touched
		} else {
			fmt.Fprintf(a.Stderr, "warning: direct touch state=running: %v\n", touchErr)
		}
		defer func() {
			if touched, touchErr := sshBackend.Touch(context.Background(), TouchRequest{Lease: LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, State: "ready", IdleTimeout: cfg.IdleTimeout}); touchErr == nil {
				server = touched
			} else {
				fmt.Fprintf(a.Stderr, "warning: direct touch state=ready: %v\n", touchErr)
			}
		}()
	}
	fmt.Fprintf(a.Stderr, "running on %s %s\n", target.Host, strings.Join(command, " "))
	recorder.Event("command.started", "command", strings.Join(command, " "))
	capabilityEnv, err := requestedCapabilityEnv(ctx, cfg, target)
	if err != nil {
		return recordFailure(err)
	}
	runEnv := mergeEnv(allowedEnv(cfg.EnvAllow), capabilityEnv)
	remote := remoteCommandWithEnvFile(workdir, runEnv, actionsEnvFile, command)
	if *shellMode {
		remote = remoteShellCommandWithEnvFile(workdir, runEnv, actionsEnvFile, strings.Join(command, " "))
	} else if shouldUseShell(command) {
		remote = remoteShellCommandWithEnvFile(workdir, runEnv, actionsEnvFile, shellScriptFromArgv(command))
	}
	if isWindowsNativeTarget(target) {
		remote = windowsRemoteCommandWithEnvFile(workdir, runEnv, actionsEnvFile, command)
		if *shellMode || shouldUseShell(command) {
			remote = windowsRemoteShellCommandWithEnvFile(workdir, runEnv, actionsEnvFile, strings.Join(command, " "))
		}
	}
	var logBuffer runLogBuffer
	stdoutEvents := recorder.StreamWriter("stdout")
	stderrEvents := recorder.StreamWriter("stderr")
	stdout := io.MultiWriter(a.Stdout, &logBuffer, stdoutEvents)
	stderr := io.MultiWriter(a.Stderr, &logBuffer, stderrEvents)
	code := runSSHStream(ctx, target, remote, stdout, stderr)
	stdoutEvents.Flush()
	stderrEvents.Flush()
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
	recorder.Finish(ctx, target, code, timings.sync, timings.command, logBuffer.String(), logBuffer.Truncated(), results)
	total := time.Since(timings.started)
	fmt.Fprintf(a.Stderr, "command complete in %s total=%s\n", timings.command.Round(time.Millisecond), total.Round(time.Millisecond))
	fmt.Fprintln(a.Stderr, formatRunSummary(timings, total, code))
	if *timingJSON {
		if err := writeTimingJSON(a.Stderr, timingReportFromRunWithActionsURL(cfg.Provider, leaseID, serverSlug(server), timings, total, code, actionsURL)); err != nil {
			return recordFailure(err)
		}
	}
	if code != 0 {
		return recordFailure(ExitError{Code: code, Message: fmt.Sprintf("remote command exited %d", code)})
	}
	return nil
}

func recordRunFailure(dst *error, failure error) error {
	if dst != nil && failure != nil {
		*dst = failure
	}
	return failure
}

type runTimings struct {
	started     time.Time
	sync        time.Duration
	command     time.Duration
	syncSteps   syncStepTimings
	syncSkipped bool
}

type syncStepTimings struct {
	sshReady             time.Duration
	mkdir                time.Duration
	manifest             time.Duration
	preflight            time.Duration
	fingerprintLocal     time.Duration
	fingerprintRemote    time.Duration
	gitSeed              time.Duration
	manifestWrite        time.Duration
	prune                time.Duration
	rsync                time.Duration
	manifestApply        time.Duration
	sanity               time.Duration
	gitHydrate           time.Duration
	finalize             time.Duration
	gitHydrateSkipped    bool
	gitHydrateSkipReason string
	fingerprintWrite     time.Duration
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
	if steps.gitHydrateSkipped {
		parts = append(parts, "git_hydrate:skipped_"+strings.ReplaceAll(steps.gitHydrateSkipReason, " ", "_"))
	} else {
		appendStep("git_hydrate", steps.gitHydrate)
	}
	appendStep("finalize", steps.finalize)
	appendStep("fingerprint_write", steps.fingerprintWrite)
	return strings.Join(parts, ",")
}

func commandNeedsHydrationHint(command []string, shellMode bool) bool {
	if len(command) == 0 {
		return false
	}
	words := command
	if shellMode || len(command) == 1 {
		words = strings.Fields(strings.Join(command, " "))
	}
	for len(words) > 0 && words[0] == "env" {
		words = words[1:]
		for len(words) > 0 && strings.Contains(words[0], "=") {
			words = words[1:]
		}
	}
	for _, word := range words {
		word = strings.Trim(word, "'\";|&()")
		switch word {
		case "pnpm", "npm", "node", "npx", "corepack", "yarn", "bun":
			return true
		}
	}
	return false
}

func gitHydrateBaseSHA(repo Repo, baseRef string) string {
	if baseRef == "" {
		return ""
	}
	if sha := gitOutput(repo.Root, "rev-parse", "--verify", "refs/remotes/origin/"+baseRef+"^{commit}"); sha != "" {
		return sha
	}
	return gitOutput(repo.Root, "rev-parse", "--verify", baseRef+"^{commit}")
}

func shouldUseShell(command []string) bool {
	if len(command) == 1 {
		return strings.ContainsAny(command[0], " \t\r\n&|;<>*$`()")
	}
	for _, word := range command {
		if isShellControlOperator(word) {
			return true
		}
	}
	return false
}

func ShouldUseShell(command []string) bool {
	return shouldUseShell(command)
}

func leadingEnvAssignment(command []string) bool {
	return len(command) > 1 && strings.Contains(command[0], "=") && !strings.HasPrefix(command[0], "-")
}

func LeadingEnvAssignment(command []string) bool {
	return leadingEnvAssignment(command)
}

func validateCoordinatorLeaseCapabilities(cfg Config, lease CoordinatorLease) error {
	if cfg.Desktop && !lease.Desktop {
		return exit(5, "coordinator did not provision desktop=true for lease %s; deploy the coordinator with desktop/VNC support", blank(lease.ID, "-"))
	}
	if cfg.Browser && !lease.Browser {
		return exit(5, "coordinator did not provision browser=true for lease %s; deploy the coordinator with browser support", blank(lease.ID, "-"))
	}
	if cfg.Code && !lease.Code {
		return exit(5, "coordinator did not provision code=true for lease %s; deploy the coordinator with web code support", blank(lease.ID, "-"))
	}
	if cfg.Tailscale.Enabled && (lease.Tailscale == nil || !lease.Tailscale.Enabled) {
		return exit(5, "coordinator did not provision tailscale=true for lease %s; deploy the coordinator with Tailscale support", blank(lease.ID, "-"))
	}
	return nil
}

func applyResolvedServerConfig(cfg *Config, server Server) {
	if server.Provider != "" {
		cfg.Provider = server.Provider
	}
	if server.ServerType.Name != "" {
		cfg.ServerType = server.ServerType.Name
	}
	if root := server.Labels["work_root"]; root != "" {
		cfg.WorkRoot = root
	}
}

func readyNetworkDisplay(cfg Config, server Server, target SSHTarget) NetworkMode {
	if target.NetworkKind != "" {
		return target.NetworkKind
	}
	if cfg.Provider == "daytona" || server.Provider == "daytona" {
		return NetworkPublic
	}
	if target.Host != server.PublicNet.IPv4.IP && target.Host != "" {
		return NetworkTailscale
	}
	return NetworkPublic
}

func coordinatorFallbackSummary(lease CoordinatorLease) string {
	if lease.RequestedServerType == "" {
		return ""
	}
	if lease.RequestedServerType == lease.ServerType && len(lease.ProvisioningAttempts) == 0 {
		return ""
	}
	attempts := make([]string, 0, len(lease.ProvisioningAttempts))
	for _, attempt := range lease.ProvisioningAttempts {
		label := attempt.ServerType
		if attempt.Region != "" {
			label = attempt.Region + "/" + label
		}
		if attempt.Market != "" && attempt.Market != "spot" {
			label = attempt.Market + "/" + label
		}
		if attempt.Category != "" {
			label += ":" + attempt.Category
		}
		attempts = append(attempts, label)
	}
	return fmt.Sprintf("requested_type=%s actual_type=%s attempts=%s", lease.RequestedServerType, lease.ServerType, blank(strings.Join(attempts, ","), "-"))
}

func coordinatorCapacityHintLines(lease CoordinatorLease) []string {
	lines := make([]string, 0, len(lease.CapacityHints))
	for _, hint := range lease.CapacityHints {
		if hint.Code == "" && hint.Message == "" {
			continue
		}
		line := hint.Code
		if hint.Message != "" {
			if line != "" {
				line += ": "
			}
			line += hint.Message
		}
		if hint.Action != "" {
			line += " action=" + hint.Action
		}
		lines = append(lines, line)
	}
	return lines
}

func acquireAttempts(bool) int {
	return 2
}

func AcquireAttempts(keep bool) int {
	return acquireAttempts(keep)
}

func isBootstrapWaitError(err error) bool {
	var exitErr ExitError
	return AsExitError(err, &exitErr) &&
		exitErr.Code == 5 &&
		strings.Contains(exitErr.Message, "timed out waiting for SSH")
}

func IsBootstrapWaitError(err error) bool {
	return isBootstrapWaitError(err)
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

func (a App) releaseBackendLeaseBestEffort(ctx context.Context, backend SSHLeaseBackend, lease LeaseTarget) {
	a.writeActionsHydrationStopBestEffort(ctx, lease.SSH, lease.LeaseID)
	fmt.Fprintf(a.Stderr, "releasing %s server=%s\n", lease.LeaseID, lease.Server.DisplayID())
	if err := backend.ReleaseLease(ctx, ReleaseLeaseRequest{Lease: lease, Force: true}); err != nil {
		fmt.Fprintf(a.Stderr, "warning: release failed for %s: %v\n", lease.LeaseID, err)
	}
}

func startCoordinatorHeartbeat(ctx context.Context, coord *CoordinatorClient, leaseID string, idleTimeout time.Duration, updateIdleTimeout *time.Duration, telemetryCollector leaseTelemetryCollector, stderr io.Writer) func() {
	rootCtx, cancel := context.WithCancel(ctx)
	interval := heartbeatInterval(idleTimeout)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			telemetry := collectLeaseTelemetryBestEffort(rootCtx, telemetryCollector)
			callCtx, heartbeatCancel := context.WithTimeout(rootCtx, 20*time.Second)
			var err error
			if updateIdleTimeout != nil {
				_, err = coord.UpdateLeaseIdleTimeoutWithTelemetry(callCtx, leaseID, *updateIdleTimeout, telemetry)
			} else {
				_, err = coord.TouchLeaseWithTelemetry(callCtx, leaseID, telemetry)
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

var coordinatorLeaseWatchInterval = 10 * time.Second

func startCoordinatorLeaseWatch(ctx context.Context, coord *CoordinatorClient, leaseID string, cancel context.CancelCauseFunc, stderr io.Writer) func() {
	watchCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(coordinatorLeaseWatchInterval)
		defer ticker.Stop()
		for {
			if !coordinatorLeaseStillActive(watchCtx, coord, leaseID, cancel, stderr) {
				return
			}
			select {
			case <-ticker.C:
			case <-watchCtx.Done():
				return
			}
		}
	}()
	return func() {
		stop()
		<-done
	}
}

func coordinatorLeaseStillActive(ctx context.Context, coord *CoordinatorClient, leaseID string, cancel context.CancelCauseFunc, stderr io.Writer) bool {
	if ctx.Err() != nil {
		return false
	}
	callCtx, callCancel := context.WithTimeout(ctx, 10*time.Second)
	lease, err := coord.GetLease(callCtx, leaseID)
	callCancel()
	if err != nil {
		if isCoordinatorNotFoundError(err) {
			cancel(exit(5, "lease %s disappeared while waiting for SSH; another process may have released it", leaseID))
			return false
		}
		if ctx.Err() == nil {
			fmt.Fprintf(stderr, "warning: lease watch failed for %s: %v\n", leaseID, err)
		}
		return true
	}
	if lease.State != "" && lease.State != "active" {
		cancel(exit(5, "lease %s became %s while waiting for SSH; another process may have released it", leaseID, lease.State))
		return false
	}
	return true
}

func isCoordinatorNotFoundError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "http 404") || strings.Contains(msg, "not_found")
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

func WaitForServerIP(ctx context.Context, client *HetznerClient, id int64) (Server, error) {
	return waitForServerIP(ctx, client, id)
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

func FindServerByAlias(servers []Server, id string) (Server, string, error) {
	return findServerByAlias(servers, id)
}

func (a App) stop(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("stop", a.Stderr)
	provider := fs.String("provider", defaults.Provider, providerHelpAll())
	providerFlags := registerProviderFlags(fs, defaults)
	targetFlags := registerTargetFlags(fs, defaults)
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
	if err := applyProviderFlags(&cfg, fs, providerFlags); err != nil {
		return err
	}
	if err := applyTargetFlagOverrides(&cfg, fs, targetFlags); err != nil {
		return err
	}
	backend, err := loadBackend(cfg, runtimeForApp(a))
	if err != nil {
		return err
	}
	if delegated, ok := backend.(DelegatedRunBackend); ok {
		return delegated.Stop(ctx, StopRequest{Options: leaseOptionsFromConfig(cfg), ID: fs.Arg(0)})
	}
	sshBackend, ok := backend.(SSHLeaseBackend)
	if !ok {
		return exit(2, "provider=%s does not support stop", backend.Spec().Name)
	}
	lease, err := sshBackend.Resolve(ctx, ResolveRequest{Options: leaseOptionsFromConfig(cfg), ID: fs.Arg(0)})
	if err != nil {
		if backendCoordinator(backend) != nil {
			fmt.Fprintf(a.Stderr, "warning: could not inspect lease before release: %v\n", err)
			lease = LeaseTarget{LeaseID: fs.Arg(0)}
		} else {
			return err
		}
	}
	if lease.SSH.Host != "" {
		a.writeActionsHydrationStopBestEffort(ctx, lease.SSH, lease.LeaseID)
	}
	if err := sshBackend.ReleaseLease(ctx, ReleaseLeaseRequest{Lease: lease, Force: true}); err != nil {
		return err
	}
	if backendCoordinator(backend) != nil {
		fmt.Fprintf(a.Stderr, "released lease=%s server=%s\n", lease.LeaseID, lease.Server.DisplayID())
		return nil
	}
	if isStaticProvider(cfg.Provider) || lease.Server.Provider == staticProvider {
		fmt.Fprintf(a.Stderr, "released static lease=%s host=%s\n", lease.LeaseID, lease.SSH.Host)
		return nil
	}
	fmt.Fprintf(a.Stderr, "deleted lease=%s server=%s name=%s\n", lease.LeaseID, lease.Server.DisplayID(), lease.Server.Name)
	return nil
}

func (a App) writeActionsHydrationStopBestEffort(ctx context.Context, target SSHTarget, leaseID string) {
	if leaseID == "" || target.Host == "" {
		return
	}
	if isWindowsNativeTarget(target) {
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
	if isStaticProvider(cfg.Provider) || server.Provider == staticProvider {
		return nil
	}
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
	if cfg.Provider == "azure" || server.Provider == "azure" {
		return deleteAzureServer(ctx, cfg, server)
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

func DeleteServer(ctx context.Context, cfg Config, server Server) error {
	return deleteServer(ctx, cfg, server)
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
