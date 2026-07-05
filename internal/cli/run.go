package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
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
	if cfg.ServerType == "" || flagWasSet(fs, "provider") || flagWasSet(fs, "class") || flagWasSet(fs, "target") || flagWasSet(fs, "windows-mode") || flagWasSet(fs, "arch") {
		cfg.ServerType = serverTypeForConfig(*cfg)
	}
}

func (a App) warmup(ctx context.Context, args []string) error {
	started := time.Now()
	defaults := defaultConfig()
	fs := newFlagSet("warmup", a.Stderr)
	leaseFlags := registerLeaseCreateFlags(fs, defaults)
	requestedLeaseID := fs.String("lease-id", "", "fixed lease ID for idempotent external-provider orchestration")
	keep := fs.Bool("keep", true, "keep server after warmup")
	actionsRunner := fs.Bool("actions-runner", false, "register this box as an ephemeral GitHub Actions runner")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	timingJSON := fs.Bool("timing-json", false, "print final timing as JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	requestedSlug, err := requestedLeaseSlug(*leaseFlags.Slug)
	if err != nil {
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
	if strings.TrimSpace(*requestedLeaseID) != "" {
		if !canonicalLeaseIDPattern.MatchString(strings.TrimSpace(*requestedLeaseID)) {
			return exit(2, "--lease-id must match cbx_<12 lowercase hex characters>")
		}
		capable, ok := backend.(IdempotentLeaseIDBackend)
		if !ok || !capable.SupportsRequestedLeaseID() {
			return exit(2, "provider=%s does not support fixed idempotent lease IDs", backend.Spec().Name)
		}
	}
	options := leaseOptionsFromConfig(cfg)
	if delegated, ok := backend.(DelegatedRunBackend); ok {
		if err := delegated.Warmup(ctx, WarmupRequest{Repo: repo, Options: options, Keep: *keep, Reclaim: *reclaim, ActionsRunner: *actionsRunner, RequestedSlug: requestedSlug, TimingJSON: *timingJSON}); err != nil {
			return err
		}
		a.syncExternalRunnersBestEffort(ctx, cfg, backend)
		return nil
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
	providerName := backend.Spec().Name
	var controllerOwnsCleanup atomic.Bool
	lease, err := sshBackend.Acquire(ctx, AcquireRequest{
		Repo: repo, Options: options, Keep: *keep, Reclaim: *reclaim,
		RequestedLeaseID: strings.TrimSpace(*requestedLeaseID), RequestedSlug: requestedSlug,
		OnAcquired: func(acquired LeaseTarget) error {
			err := acknowledgeControllerAcquireIdentity(ctx, controllerAcquireIdentityFromLease(providerName, acquired))
			if err == nil && controllerAcquireIdentityAcknowledgmentConfigured() {
				controllerOwnsCleanup.Store(true)
			}
			return err
		},
	})
	if err != nil {
		return err
	}
	server, target, leaseID := lease.Server, lease.SSH, lease.LeaseID
	applyResolvedServerConfig(&cfg, server)
	if err := a.claimLeaseTargetForRepoAndRegister(ctx, leaseID, serverSlug(server), cfg, server, target, repo.Root, *reclaim); err != nil {
		a.releaseWarmupLeaseAfterFailure(ctx, sshBackend, cfg, LeaseTarget{Server: server, SSH: target, LeaseID: leaseID, Coordinator: lease.Coordinator}, controllerOwnsCleanup.Load())
		return err
	}
	if serverTailscaleMetadata(server).Enabled {
		target = bootstrapNetworkTarget(cfg, server, target)
		if err := waitForSSHReady(ctx, &target, a.Stderr, "tailscale metadata", 2*time.Minute); err == nil {
			a.refreshTailscaleMetadata(ctx, cfg, sshBackend, lease.Coordinator, lease.Coordinator != nil, &server, target, leaseID)
			_ = updateLeaseClaimEndpoint(leaseID, server, target)
		} else {
			fmt.Fprintf(a.Stderr, "warning: tailscale metadata wait failed: %v\n", err)
		}
	}
	if resolved, err := resolveNetworkTarget(ctx, cfg, server, target); err != nil {
		a.releaseWarmupLeaseAfterFailure(ctx, sshBackend, cfg, LeaseTarget{Server: server, SSH: target, LeaseID: leaseID, Coordinator: lease.Coordinator}, controllerOwnsCleanup.Load())
		return err
	} else {
		target = resolved.Target
		_ = updateLeaseClaimEndpoint(leaseID, server, target)
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
	a.startRegisteredWebVNCDaemonBestEffort(cfg, target, leaseID, *keep)
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

func (a App) releaseWarmupLeaseAfterFailure(ctx context.Context, backend SSHLeaseBackend, cfg Config, lease LeaseTarget, controllerOwnsCleanup bool) {
	if controllerOwnsCleanup {
		return
	}
	a.releaseBackendLeaseBestEffort(ctx, backend, cfg, lease)
}

type benchmarkRecordContext struct {
	Source      string
	RepeatIndex int
	ColdRun     *bool
	OnRecord    func()
}

func (a App) runCommand(ctx context.Context, args []string) error {
	return a.runCommandWithBenchmarkRecord(ctx, args, benchmarkRecordContext{})
}

func (a App) runCommandWithBenchmarkRecord(ctx context.Context, args []string, benchmarkCtx benchmarkRecordContext) (err error) {
	defaults := defaultConfig()
	fs := newFlagSet("run", a.Stderr)
	leaseFlags := registerLeaseCreateFlags(fs, defaults)
	leaseIDFlag := fs.String("id", "", "existing lease or server id")
	keep := fs.Bool("keep", false, "keep server after command")
	keepOnFailure := fs.Bool("keep-on-failure", false, "keep a newly acquired lease when the remote command exits non-zero")
	noSync := fs.Bool("no-sync", false, "skip rsync")
	syncOnly := fs.Bool("sync-only", false, "sync and exit")
	noHydrate := fs.Bool("no-hydrate", false, "skip configured Actions hydration")
	debugSync := fs.Bool("debug", false, "print detailed sync timing")
	shellMode := fs.Bool("shell", false, "run command through the remote shell")
	checksumSync := fs.Bool("checksum", false, "use checksum rsync instead of size/time")
	forceSyncLarge := fs.Bool("force-sync-large", false, "allow unusually large sync candidates")
	fullResync := fs.Bool("full-resync", false, "reset the remote workdir and force a complete sync")
	freshSync := fs.Bool("fresh-sync", false, "alias for --full-resync")
	junitResults := fs.String("junit", "", "comma-separated remote JUnit XML paths to record")
	resultsAuto := fs.Bool("results-auto", false, "scan common remote JUnit XML paths after the command")
	failOnTestFailures := fs.Bool("fail-on-test-failures", false, "exit non-zero when collected JUnit reports contain failures or errors")
	captureStdout := fs.String("capture-stdout", "", "write remote stdout to a local file instead of the terminal")
	captureStderr := fs.String("capture-stderr", "", "write remote stderr to a local file instead of the terminal")
	captureOnFail := fs.Bool("capture-on-fail", false, "compatibility alias; failure bundles are saved by default on non-zero exit")
	preflight := fs.Bool("preflight", false, "print remote capability preflight before running the command")
	preflightTools := fs.String("preflight-tools", "", "comma-separated preflight tools to probe; overrides run.preflightTools")
	scriptPath := fs.String("script", "", "upload and run a local script file")
	scriptStdin := fs.Bool("script-stdin", false, "read a script from stdin, upload it, and run it")
	freshPRValue := fs.String("fresh-pr", "", "run from a fresh remote checkout of a GitHub PR: owner/repo#123, URL, or number")
	applyLocalPatch := fs.Bool("apply-local-patch", false, "apply the local git diff on top of --fresh-pr checkout")
	envHelper := fs.String("env-helper", "", "persist profile env as a reusable remote helper name under .crabbox/env/")
	runLabel := fs.String("label", "", "human-readable label for this run")
	presetName := fs.String("preset", "", "configured profile preset to expand into a command")
	scenario := fs.String("scenario", "", "preset variable shorthand for --preset-var scenario=<value>")
	emitProof := fs.String("emit-proof", "", "write a generated proof block after a successful run")
	proofTemplate := fs.String("proof-template", "", "proof template name from the selected profile")
	stopAfter := fs.String("stop-after", "", "stop policy for the lease: success, always, failure, or never")
	leaseOutput := fs.String("lease-output", "", "write a small JSON lease handle for orchestrators")
	readyPool := fs.String("pool", "", "borrow a broker ready-pool lease")
	readyPoolReturn := fs.String("pool-return", "auto", "ready-pool return policy: auto, ready, drain, release")
	var downloads stringListFlag
	var allowEnvFlags stringListFlag
	var envProfileFlags stringListFlag
	var presetVars stringListFlag
	var artifactGlobs stringListFlag
	var requiredArtifactGlobs stringListFlag
	fs.Var(&downloads, "download", "download a remote file after command success: remote=local; repeatable")
	fs.Var(&allowEnvFlags, "allow-env", "allow an environment variable for this run; repeatable or comma-separated")
	fs.Var(&envProfileFlags, "env-from-profile", "load allowed environment values from a local profile file; repeatable")
	fs.Var(&presetVars, "preset-var", "preset template variable name=value; repeatable or comma-separated")
	fs.Var(&artifactGlobs, "artifact-glob", "collect remote files matching a safe glob into a local run artifact tarball; repeatable")
	fs.Var(&requiredArtifactGlobs, "require-artifact", "require a remote file matching a safe glob after command success; repeatable")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	timingJSON := fs.Bool("timing-json", false, "print final timing as JSON")
	timingRecord := fs.String("timing-record", "", "append final timing to benchmark JSONL store: default, off, or path")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	timingRecordPath, timingRecordEnabled, err := resolveBenchmarkTimingStore(*timingRecord)
	if err != nil {
		return err
	}
	var cleanup leaseCleanupResult
	var finalTimingReport *timingReport
	var timingRecordRepo Repo
	var timingRecordCommand []string
	var timingRecordColdRun *bool
	var delegatedTimingCapture *capturedTimingReportWriter
	defer func() {
		if finalTimingReport == nil {
			return
		}
		report := *finalTimingReport
		cleanup.apply(&report)
		if timingRecordEnabled {
			recordColdRun := timingRecordColdRun
			if benchmarkCtx.ColdRun != nil {
				recordColdRun = benchmarkCtx.ColdRun
			}
			record := newBenchmarkTimingRecord(time.Now().UTC(), firstNonBlank(strings.TrimSpace(benchmarkCtx.Source), "run"), report, timingRecordRepo, timingRecordCommand, recordColdRun, benchmarkCtx.RepeatIndex)
			if writeErr := appendBenchmarkTimingRecord(timingRecordPath, record); writeErr != nil {
				if err == nil {
					err = writeErr
				} else {
					fmt.Fprintf(a.Stderr, "warning: benchmark timing record skipped: %v\n", writeErr)
				}
			} else {
				if benchmarkCtx.OnRecord != nil {
					benchmarkCtx.OnRecord()
				}
				fmt.Fprintf(a.Stderr, "benchmark timing record appended path=%s observations=1\n", timingRecordPath)
			}
		}
		if !*timingJSON {
			return
		}
		if writeErr := writeTimingJSON(a.Stderr, report); writeErr != nil && err == nil {
			err = writeErr
		}
	}()
	command := fs.Args()
	if len(command) > 0 && command[0] == "--" {
		command = command[1:]
	}
	runLabelValue := strings.TrimSpace(*runLabel)
	requestedSlug, err := requestedLeaseSlug(*leaseFlags.Slug)
	if err != nil {
		return err
	}
	if requestedSlug != "" && strings.TrimSpace(*leaseIDFlag) != "" {
		return exit(2, "--slug only applies when creating a new lease; omit --id or use the existing slug")
	}
	if strings.TrimSpace(*readyPool) != "" && strings.TrimSpace(*leaseIDFlag) != "" {
		return exit(2, "--pool borrows the lease id; omit --id")
	}
	if strings.TrimSpace(*readyPool) != "" && strings.TrimSpace(*stopAfter) != "" {
		return exit(2, "--pool uses --pool-return for cleanup policy; omit --stop-after")
	}
	if strings.TrimSpace(*readyPool) != "" && (*keep || *keepOnFailure) {
		return exit(2, "--pool uses --pool-return for lifecycle; omit --keep and --keep-on-failure")
	}
	if err := validateReadyPoolRunReturnPolicy(*readyPoolReturn); err != nil {
		return err
	}
	fullResyncRequested := *fullResync || *freshSync
	if strings.TrimSpace(*readyPool) != "" && fullResyncRequested {
		return exit(2, "--pool cannot be combined with --full-resync or --fresh-sync")
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	cfg.Profile = *leaseFlags.Profile
	if err := applySelectedProfileConfig(&cfg); err != nil {
		return err
	}
	if err := applyLeaseCreateFlagsForLease(&cfg, fs, leaseFlags, *leaseIDFlag); err != nil {
		return err
	}
	expansion, err := expandRunProfile(cfg, *presetName, *scenario, presetVars, command, *shellMode, *preflight, artifactGlobs, *proofTemplate)
	if err != nil {
		return err
	}
	command = expansion.Command
	*shellMode = expansion.Shell
	*preflight = expansion.Preflight
	if expansion.ProofTemplate != "" {
		*proofTemplate = expansion.ProofTemplate
	}
	if expansion.PresetName != "" {
		fmt.Fprintln(a.Stderr, formatExpandedPresetCommand(expansion.PresetName, command, *shellMode, expansion.Env, expansion.LiteralArgs))
	}
	if len(command) == 0 && *scriptPath == "" && !*scriptStdin && !*syncOnly {
		return exit(2, "usage: crabbox run [flags] -- <command...>")
	}
	if err := validateRunStopAfterPolicy(*stopAfter); err != nil {
		return err
	}
	if err := validateRunArtifactGlobs(expansion.ArtifactGlobs); err != nil {
		return err
	}
	requiredArtifactGlobs = appendUniqueStrings(nil, requiredArtifactGlobs...)
	if err := validateRequiredRunArtifactGlobs(requiredArtifactGlobs); err != nil {
		return err
	}
	runArtifactGlobs := appendUniqueStrings(append([]string{}, expansion.ArtifactGlobs...), requiredArtifactGlobs...)
	if *syncOnly {
		if len(expansion.ArtifactGlobs) > 0 {
			return exit(2, "--artifact-glob cannot be combined with --sync-only")
		}
		if len(requiredArtifactGlobs) > 0 {
			return exit(2, "--require-artifact cannot be combined with --sync-only")
		}
		if strings.TrimSpace(*emitProof) != "" {
			return exit(2, "--emit-proof cannot be combined with --sync-only")
		}
	}
	if err := preflightRunLocalOutputs(*captureStdout, *captureStderr, downloads); err != nil {
		return err
	}
	if strings.TrimSpace(*leaseOutput) != "" {
		if err := preflightLocalOutputPath("lease output", strings.TrimSpace(*leaseOutput), false, false); err != nil {
			return err
		}
	}
	if strings.TrimSpace(*emitProof) != "" {
		if strings.TrimSpace(*leaseOutput) != "" {
			samePath, err := sameLocalOutputPath(strings.TrimSpace(*leaseOutput), strings.TrimSpace(*emitProof))
			if err != nil {
				return err
			}
			if samePath {
				return exit(2, "lease output and emit proof paths must be different")
			}
		}
		if err := preflightProofOutputPath(strings.TrimSpace(*emitProof), *captureStdout, *captureStderr, downloads); err != nil {
			return err
		}
		if strings.TrimSpace(*proofTemplate) != "" {
			if _, ok := cfg.ProofTemplates[strings.TrimSpace(*proofTemplate)]; !ok {
				return exit(2, "proof template %q is not configured for profile %q", strings.TrimSpace(*proofTemplate), cfg.Profile)
			}
		}
		if err := preflightLocalOutputPath("emit proof", strings.TrimSpace(*emitProof), true, true); err != nil {
			return err
		}
	}
	applyRunEnvAllowFlags(&cfg, allowEnvFlags)
	if *preflightTools != "" {
		cfg.Run.PreflightTools = normalizePreflightToolNames(splitCommaList(*preflightTools))
	}
	if *preflight {
		if err := validatePreflightTools(cfg.Run.PreflightTools); err != nil {
			return err
		}
	}
	if flagWasSet(fs, "checksum") {
		cfg.Sync.Checksum = *checksumSync
	}
	if *junitResults != "" {
		cfg.Results.JUnit = splitCommaList(*junitResults)
	}
	if flagWasSet(fs, "results-auto") {
		cfg.Results.Auto = *resultsAuto
	}
	if flagWasSet(fs, "fail-on-test-failures") {
		cfg.Results.FailOnFailures = *failOnTestFailures
	}
	repo, err := findRepo()
	if err != nil {
		return err
	}
	timingRecordRepo = repo
	freshPR, err := parseFreshPRSpec(*freshPRValue, repo)
	if err != nil {
		return err
	}
	if !freshPR.Empty() {
		if *noSync {
			return exit(2, "--fresh-pr cannot be combined with --no-sync")
		}
		if *syncOnly {
			return exit(2, "--fresh-pr cannot be combined with --sync-only")
		}
		if fullResyncRequested {
			return exit(2, "--full-resync is redundant with --fresh-pr")
		}
	} else if *applyLocalPatch {
		return exit(2, "--apply-local-patch requires --fresh-pr")
	}
	if fullResyncRequested && *noSync {
		return exit(2, "--full-resync cannot be combined with --no-sync")
	}
	if (*scriptPath != "" || *scriptStdin) && *syncOnly {
		return exit(2, "--script cannot be combined with --sync-only")
	}
	envSelection, err := selectRunEnv(cfg.EnvAllow, envProfileFlags, len(allowEnvFlags) > 0)
	if err != nil {
		return err
	}
	envSelection.Inline = mergeEnv(envSelection.Inline, expansion.Env)
	envSelection.Effective = mergeEnv(envSelection.Effective, expansion.Env)
	envHelperName := strings.TrimSpace(*envHelper)
	if envHelperName != "" && len(envSelection.Profile) == 0 {
		return exit(2, "--env-helper requires --env-from-profile values selected by --allow-env")
	}
	if envHelperName != "" {
		if *syncOnly {
			return exit(2, "--env-helper cannot be combined with --sync-only")
		}
		if _, err := safeEnvHelperName(envHelperName); err != nil {
			return err
		}
	}
	if *leaseIDFlag == "" {
		if err := validateRunArtifactGlobTarget(SSHTarget{TargetOS: cfg.TargetOS, WindowsMode: cfg.WindowsMode}, expansion.ArtifactGlobs); err != nil {
			return err
		}
		if err := validateRequiredRunArtifactGlobTarget(SSHTarget{TargetOS: cfg.TargetOS, WindowsMode: cfg.WindowsMode}, requiredArtifactGlobs); err != nil {
			return err
		}
		if envHelperName != "" {
			if err := validateRunEnvHelperTarget(SSHTarget{TargetOS: cfg.TargetOS, WindowsMode: cfg.WindowsMode}, runEnvHelperPath(envHelperName)); err != nil {
				return err
			}
		}
		if expansion.Profile.Doctor.Enabled && cfg.TargetOS == targetWindows && cfg.WindowsMode == windowsModeNormal {
			return exit(2, "profile doctor is not supported for native Windows targets")
		}
	}
	backendRuntime := runtimeForApp(a)
	if timingRecordEnabled {
		delegatedTimingCapture = &capturedTimingReportWriter{writer: a.Stderr}
		backendRuntime.Stderr = delegatedTimingCapture
	}
	backend, err := loadBackend(cfg, backendRuntime)
	if err != nil {
		return err
	}
	var server Server
	var target SSHTarget
	var leaseID string
	var borrowedPool *CoordinatorReadyPoolResponse
	var runFailure error
	defer func() {
		if borrowedPool == nil {
			return
		}
		failure := runFailure
		if failure == nil {
			failure = err
		}
		result := readyPoolRunReturnResult(*readyPoolReturn, failure)
		coord, coordErr := readyPoolCoordinatorFromConfig(cfg)
		if coordErr != nil {
			fmt.Fprintf(a.Stderr, "warning: ready-pool return skipped for %s: %v\n", borrowedPool.Entry.LeaseID, coordErr)
			if failure == nil {
				err = coordErr
			}
			return
		}
		if readyPoolReturnNeedsHydrationStop(result) {
			a.writeActionsHydrationStopBestEffort(context.Background(), target, borrowedPool.Entry.LeaseID)
		}
		if _, returnErr := coord.ReturnReadyPoolLease(context.Background(), borrowedPool.Entry.Key, borrowedPool.Entry.LeaseID, result, readyPoolRunReturnReason(failure), borrowedPool.Entry.BorrowToken); returnErr != nil {
			fmt.Fprintf(a.Stderr, "warning: ready-pool return failed for %s: %v\n", borrowedPool.Entry.LeaseID, returnErr)
			if failure == nil {
				err = returnErr
			}
			return
		}
		fmt.Fprintf(a.Stderr, "returned pool=%s lease=%s result=%s\n", borrowedPool.Entry.Key, borrowedPool.Entry.LeaseID, result)
	}()
	if strings.TrimSpace(*leaseOutput) != "" && !backend.Spec().Features.Has(FeatureRunSession) {
		// TODO: Let other reusable delegated providers populate RunResult.Session
		// and advertise FeatureRunSession once their lifecycle contract is covered.
		return exit(2, "--lease-output is not supported for provider=%s yet", backend.Spec().Name)
	}
	options := leaseOptionsFromConfig(cfg)
	scriptRequested := *scriptPath != "" || *scriptStdin
	var script *RunScriptSpec
	runReq := RunRequest{
		Repo:                  repo,
		ID:                    *leaseIDFlag,
		Options:               options,
		Keep:                  *keep,
		KeepOnFailure:         *keepOnFailure,
		Reclaim:               *reclaim,
		NoSync:                *noSync,
		SyncOnly:              *syncOnly,
		DebugSync:             *debugSync,
		ShellMode:             *shellMode,
		ChecksumSync:          *checksumSync,
		ForceSyncLarge:        *forceSyncLarge,
		FullResync:            fullResyncRequested,
		EnvHelper:             envHelperName,
		CaptureStdout:         *captureStdout,
		CaptureStderr:         *captureStderr,
		CaptureOnFail:         *captureOnFail,
		Preflight:             *preflight,
		Downloads:             downloads,
		Env:                   envSelection.Effective,
		EnvSummary:            envSelection.SummaryRequested,
		ScriptRequested:       scriptRequested,
		FreshPR:               freshPR,
		ApplyLocalPatch:       *applyLocalPatch,
		Command:               command,
		Label:                 runLabelValue,
		RequestedSlug:         requestedSlug,
		TimingJSON:            *timingJSON || timingRecordEnabled,
		ArtifactGlobs:         expansion.ArtifactGlobs,
		RequiredArtifactGlobs: requiredArtifactGlobs,
		EmitProof:             strings.TrimSpace(*emitProof),
		ProofTemplate:         strings.TrimSpace(*proofTemplate),
		ProfileVariables:      expansion.Variables,
		StopAfter:             strings.TrimSpace(*stopAfter),
	}
	if delegated, ok := backend.(DelegatedRunBackend); ok {
		if strings.TrimSpace(*readyPool) != "" {
			return exit(2, "--pool requires a brokered SSH lease provider")
		}
		if expansion.Profile.Doctor.Enabled {
			return exit(2, "%s delegates run execution; profile doctor is not supported", backend.Spec().Name)
		}
		if err := RejectDelegatedSyncOptionsForSpec(backend.Spec(), runReq); err != nil {
			return err
		}
		if scriptRequested && backend.Spec().Features.Has(FeatureModuleRun) {
			script, err = loadRunScript(*scriptPath, *scriptStdin, a.Stdin)
			if err != nil {
				return err
			}
			runReq.Script = script
		}
		timingRecordCommand = runScriptRecordCommand(script, command)
		if runReq.Preflight {
			printDelegatedPreflightUnsupported(a.Stderr, backend.Spec().Name)
		}
		result, runErr := delegated.Run(ctx, runReq)
		if runErr == nil || result.Command > 0 || result.Total > 0 {
			a.syncExternalRunnersBestEffort(ctx, cfg, backend)
		}
		if sessionErr := ValidateRunSessionForSpec(backend.Spec(), result); sessionErr != nil {
			if runErr == nil {
				return sessionErr
			}
			fmt.Fprintf(a.Stderr, "warning: ignoring invalid delegated run session: %v\n", sessionErr)
			result.Session = nil
		}
		if err := writeRunLeaseOutput(strings.TrimSpace(*leaseOutput), result.Session); err != nil {
			if runErr == nil {
				return err
			}
			fmt.Fprintf(a.Stderr, "warning: lease output failed: %v\n", err)
		}
		if runErr == nil && strings.TrimSpace(*emitProof) != "" {
			proof, err := writeDelegatedRunProof(strings.TrimSpace(*emitProof), strings.TrimSpace(*proofTemplate), cfg, result, runReq)
			if err != nil {
				return err
			}
			result.Artifacts = append(result.Artifacts, proof)
			fmt.Fprintf(a.Stderr, "artifact kind=proof path=%s bytes=%d template=%s\n", proof.Path, proof.Bytes, blank(proof.Template, "default"))
		}
		if result.Session != nil {
			coldRun := !result.Session.Reused
			timingRecordColdRun = &coldRun
		}
		if timingRecordEnabled {
			report := timingReportFromDelegatedRunResult(runReq, result, backend.Spec().Name, runErr)
			if delegatedTimingCapture != nil && delegatedTimingCapture.report != nil {
				report = *delegatedTimingCapture.report
			}
			report.Artifacts = result.Artifacts
			finalTimingReport = &report
		}
		return runErr
	}
	sshBackend, ok := backend.(SSHLeaseBackend)
	if !ok {
		return exit(2, "provider=%s does not support run", backend.Spec().Name)
	}
	coord := backendCoordinator(backend)
	var registrationCoord *CoordinatorClient
	if shouldRegisterCoordinatorLease(cfg) {
		if client, configured, coordErr := newCoordinatorClient(cfg); coordErr != nil {
			fmt.Fprintf(a.Stderr, "warning: registered coordinator heartbeat unavailable: %v\n", coordErr)
		} else if configured {
			registrationCoord = client
		}
	}
	if scriptRequested {
		script, err = loadRunScript(*scriptPath, *scriptStdin, a.Stdin)
		if err != nil {
			return err
		}
		runReq.Script = script
	}
	if strings.TrimSpace(*readyPool) != "" {
		if coord == nil {
			return exit(2, "--pool requires a coordinator-backed SSH lease provider")
		}
		repoSlug := cfg.Actions.Repo
		if repoSlug == "" {
			repoSlug = bestEffortGitHubRepoSlug(repo, cfg)
		}
		borrowInput, err := readyPoolRunBorrowInputForRun(cfg, repo, repoSlug, *noSync)
		if err != nil {
			return err
		}
		res, err := coord.BorrowReadyPoolLease(ctx, strings.TrimSpace(*readyPool), borrowInput)
		if err != nil {
			return err
		}
		borrowedPool = &res
		*leaseIDFlag = res.Entry.LeaseID
		fmt.Fprintf(a.Stderr, "borrowed pool=%s lease=%s\n", res.Entry.Key, res.Entry.LeaseID)
	}

	acquired := false
	useCoordinator := coord != nil
	recorder := &runRecorder{}
	failureClassificationPrinted := false
	recordFailure := func(failure error) error {
		if failure != nil && !failureClassificationPrinted {
			classification := ClassifyRunFailure(1, failure.Error(), nil)
			if classification.BlockedStage != "" && classification.BlockedStage != "unknown" {
				fmt.Fprintf(a.Stderr, "failure classification%s\n", FormatFailureClassificationFields(classification))
				failureClassificationPrinted = true
			}
		}
		return recordRunFailure(&runFailure, failure)
	}
	recordCommand := runScriptRecordCommand(script, command)
	timingRecordCommand = recordCommand
	if useCoordinator {
		recorder = newRunRecorder(ctx, coord, cfg, recordCommand, runLabelValue, a.Stderr, strings.TrimSpace(*leaseIDFlag) != "")
		defer func() {
			recorder.Failed(runFailure)
		}()
		recorder.Event("leasing.started", "leasing", "")
	}
	if *leaseIDFlag != "" {
		var lease LeaseTarget
		lease, err = resolveSSHLeaseTarget(ctx, sshBackend, ResolveRequest{Repo: repo, Options: options, ID: *leaseIDFlag, Reclaim: *reclaim, Prepare: true})
		if err == nil {
			server, target, leaseID = lease.Server, lease.SSH, lease.LeaseID
			if lease.Coordinator != nil {
				coord = lease.Coordinator
				useCoordinator = true
				recorder.UseCoordinator(coord)
			}
			applyResolvedLeaseConfig(&cfg, server, &target)
			if borrowedPool != nil {
				target = applyReadyPoolEndpoint(target, borrowedPool.Entry)
			}
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
		lease, err = sshBackend.Acquire(ctx, AcquireRequest{Repo: repo, Options: options, Keep: *keep, Reclaim: *reclaim, RequestedSlug: requestedSlug})
		if err == nil {
			server, target, leaseID = lease.Server, lease.SSH, lease.LeaseID
		}
		acquired = true
	}
	if err != nil {
		return recordFailure(err)
	}
	if timingRecordEnabled {
		coldRun := acquired && strings.TrimSpace(*leaseIDFlag) == "" && borrowedPool == nil
		timingRecordColdRun = &coldRun
	}
	applyResolvedServerConfig(&cfg, server)
	if borrowedPool != nil && strings.TrimSpace(borrowedPool.Entry.WorkRoot) != "" {
		cfg.WorkRoot = strings.TrimSpace(borrowedPool.Entry.WorkRoot)
	}
	if err := enforceManagedLeaseCapabilities(cfg, server, leaseID); err != nil {
		return recordFailure(err)
	}
	if err := validateRunArtifactGlobTarget(target, expansion.ArtifactGlobs); err != nil {
		return recordFailure(err)
	}
	if err := validateRequiredRunArtifactGlobTarget(target, requiredArtifactGlobs); err != nil {
		return recordFailure(err)
	}
	if expansion.Profile.Doctor.Enabled && isWindowsNativeTarget(target) {
		return recordFailure(exit(2, "profile doctor is not supported for native Windows targets"))
	}
	if useCoordinator {
		recorder.AttachLease(leaseID, serverSlug(server), cfg)
	}
	keepFailedLease := false
	defer func() {
		if !shouldReleaseRunLease(acquired, *keep, keepFailedLease, *stopAfter, runFailure) {
			return
		}
		releaseApp := a
		if *timingJSON {
			releaseApp.Stderr = io.Discard
		}
		cleanup.Attempted = true
		cleanup.Err = releaseApp.releaseBackendLeaseBestEffort(context.Background(), sshBackend, cfg, LeaseTarget{Server: server, SSH: target, LeaseID: leaseID, Coordinator: coord})
		cleanup.Stopped = cleanup.Err == nil
		if cleanup.Err == nil {
			recorder.Event("lease.released", "released", "")
		}
		if !*timingJSON {
			if cleanup.Err != nil {
				fmt.Fprintf(a.Stderr, "lease cleanup stopped=false policy=%s lease=%s slug=%s error=%q\n", blank(*stopAfter, "auto"), leaseID, blank(serverSlug(server), "-"), cleanup.Err.Error())
				if err == nil {
					err = exit(7, "lease cleanup failed for %s: %v", leaseID, cleanup.Err)
				}
				return
			}
			fmt.Fprintf(a.Stderr, "lease cleanup stopped=true policy=%s lease=%s slug=%s\n", blank(*stopAfter, "auto"), leaseID, blank(serverSlug(server), "-"))
		}
	}()
	claimLease := a.claimLeaseTargetForRepoAndRegister
	if *leaseIDFlag != "" {
		claimLease = a.claimResolvedLeaseTargetForRepoAndRegister
	}
	if err := claimLease(ctx, leaseID, serverSlug(server), cfg, server, target, repo.Root, *reclaim || borrowedPool != nil); err != nil {
		return recordFailure(err)
	}
	a.startRegisteredWebVNCDaemonBestEffort(cfg, target, leaseID, acquired && *keep)
	if !useCoordinator && leaseID != "" {
		if touched, touchErr := sshBackend.Touch(ctx, TouchRequest{Lease: LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, State: blank(server.Labels["state"], "ready"), IdleTimeout: cfg.IdleTimeout}); touchErr == nil {
			server = touched
		} else {
			fmt.Fprintf(a.Stderr, "warning: direct touch failed for %s: %v\n", leaseID, touchErr)
		}
	}
	if envHelperName != "" {
		// Reject target-specific helper gaps before SSH wait or sync mutates the remote.
		if err := validateRunEnvHelperTarget(target, runEnvHelperPath(envHelperName)); err != nil {
			return recordFailure(err)
		}
	}
	var stopHeartbeat func()
	stopRunHeartbeat := func() {
		if stopHeartbeat == nil {
			return
		}
		stopHeartbeat()
		stopHeartbeat = nil
	}
	defer stopRunHeartbeat()
	startRunHeartbeat := func(updateIdleTimeout *time.Duration) {
		stopRunHeartbeat()
		heartbeatCoord := coord
		if heartbeatCoord == nil {
			heartbeatCoord = registrationCoord
		}
		if heartbeatCoord != nil {
			stopHeartbeat = startCoordinatorHeartbeat(ctx, heartbeatCoord, leaseID, cfg.IdleTimeout, updateIdleTimeout, leaseTelemetryCollectorForTarget(target), a.Stderr)
		}
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
		startRunHeartbeat(heartbeatIdleTimeout)
	} else if registrationCoord != nil && leaseID != "" {
		startRunHeartbeat(nil)
	}

	if cfg.Sync.BaseRef == "" {
		cfg.Sync.BaseRef = repo.BaseRef
	}
	timings := runTimings{started: time.Now()}
	exitNodeEgressChecked := false
	workdir := remoteJoin(cfg, leaseID, repo.Name)
	actionsEnvFile := ""
	profileEnvFile := ""
	actionsURL := ""
	hydratedByActions := false
	autoHydrateActions := shouldAutoHydrateActions(cfg, *noHydrate, *noSync, freshPR, *syncOnly)
	if !freshPR.Empty() {
		workdir = remoteJoin(cfg, leaseID, freshPR.WorkdirName())
	} else if state, err := readActionsHydrationState(ctx, target, leaseID); err == nil && state.Workspace != "" {
		workdir = state.Workspace
		actionsEnvFile = state.EnvFile
		if state.RunID != "" {
			if ghRepo, err := resolveGitHubRepo(repo, cfg.Actions.Repo); err == nil {
				actionsURL = actionsRunURL(ghRepo, state.RunID)
			}
		}
		hydratedByActions = true
		fmt.Fprintf(a.Stderr, "using Actions workspace %s\n", workdir)
	} else if commandNeedsHydrationHint(command, *shellMode) && cfg.Actions.Workflow != "" && !autoHydrateActions {
		fmt.Fprintf(a.Stderr, "warning: no Actions hydration marker found for %s; JS package commands may fail on a raw box. Run \"crabbox actions hydrate --id %s\" first, omit --no-hydrate, or include runtime setup in the command.\n", leaseID, leaseID)
	}
	contextPrinted := false
	preflightPrinted := false
	rawJSRuntimePreflightDone := false
	printContext := func(currentTarget SSHTarget) {
		if contextPrinted {
			return
		}
		printRunContextSummary(a.Stderr, coord, cfg, server, currentTarget, leaseID, workdir, hydratedByActions, actionsURL, recorder)
		contextPrinted = true
	}
	printPreflight := func(currentTarget SSHTarget) {
		if !*preflight || preflightPrinted {
			return
		}
		hydrateTarget := currentTarget
		if hydrateTarget.TargetOS == "" {
			hydrateTarget.TargetOS = cfg.TargetOS
		}
		if hydrateTarget.WindowsMode == "" {
			hydrateTarget.WindowsMode = cfg.WindowsMode
		}
		hydrateSupported := supportsLocalActionsHydrateTarget(hydrateTarget) || supportsGitHubActionsRunnerTarget(hydrateTarget)
		printRemoteCapabilityPreflight(ctx, a.Stderr, cfg, currentTarget, leaseID, workdir, remoteRunEnvFiles(actionsEnvFile, profileEnvFile), hydratedByActions, actionsURL, hydrateSupported, envSelection.Inline)
		preflightPrinted = true
	}
	preflightRawJSRuntime := func(currentTarget SSHTarget) error {
		if rawJSRuntimePreflightDone {
			return nil
		}
		if hydratedByActions || script != nil || *syncOnly {
			rawJSRuntimePreflightDone = true
			return nil
		}
		if runEnvProvidesPath(envSelection.Effective, currentTarget) {
			rawJSRuntimePreflightDone = true
			return nil
		}
		tools := commandRuntimePreflightTools(command, *shellMode)
		if len(tools) == 0 {
			rawJSRuntimePreflightDone = true
			return nil
		}
		hydrateTarget := currentTarget
		if hydrateTarget.TargetOS == "" {
			hydrateTarget.TargetOS = cfg.TargetOS
		}
		if hydrateTarget.WindowsMode == "" {
			hydrateTarget.WindowsMode = cfg.WindowsMode
		}
		if autoHydrateActions && supportsLocalActionsHydrateTarget(hydrateTarget) {
			rawJSRuntimePreflightDone = true
			return nil
		}
		missing, err := probeMissingRemoteTools(ctx, currentTarget, tools)
		if err != nil {
			return exit(5, "remote JS runtime preflight failed before sync: %v", err)
		}
		if len(missing) == 0 {
			rawJSRuntimePreflightDone = true
			return nil
		}
		if *keepOnFailure {
			if acquired && !*keep {
				keepFailedLease = true
			}
			printKeepOnFailureSSHHint(a.Stderr, cfg, leaseID, server, currentTarget)
		}
		suggestion := rawJSRuntimeHydrateSuggestion(cfg, hydrateTarget, leaseID, acquired, *keep, *keepOnFailure)
		return rawJSRuntimeMissingError(cfg, missing, command, *shellMode, suggestion)
	}
	autoHydrateActionsIfNeeded := func(currentTarget SSHTarget) error {
		if !autoHydrateActions || hydratedByActions {
			return nil
		}
		hydrateTarget := currentTarget
		if hydrateTarget.TargetOS == "" {
			hydrateTarget.TargetOS = cfg.TargetOS
		}
		if hydrateTarget.WindowsMode == "" {
			hydrateTarget.WindowsMode = cfg.WindowsMode
		}
		if !supportsLocalActionsHydrateTarget(hydrateTarget) {
			return nil
		}
		fields := actionsHydrateFields(leaseID, githubActionsLeaseLabel(leaseID), cfg.Actions.Job, 0, cfg.Actions.Fields)
		recorder.Event("actions.hydrate.started", "hydrate", cfg.Actions.Workflow)
		state, err := a.hydrateActionsLocally(ctx, cfg, repo, currentTarget, leaseID, cfg.Actions.Job, fields, 20*time.Minute, false, false)
		if err != nil {
			recorder.Event("actions.hydrate.failed", "hydrate", err.Error())
			if *keepOnFailure {
				if acquired && !*keep {
					keepFailedLease = true
				}
				printKeepOnFailureSSHHint(a.Stderr, cfg, leaseID, server, currentTarget)
			}
			return err
		}
		workdir = state.Workspace
		actionsEnvFile = state.EnvFile
		actionsURL = ""
		hydratedByActions = true
		rawJSRuntimePreflightDone = true
		recorder.Event("actions.hydrate.finished", "hydrate", workdir)
		fmt.Fprintf(a.Stderr, "using local Actions workspace %s\n", workdir)
		return nil
	}
	beforeCommandLeaseReplacementAttempted := false
	replaceLeaseAfterBeforeCommandSSHFailure := func(waitErr error) (bool, error) {
		if beforeCommandLeaseReplacementAttempted ||
			!shouldReplaceLeaseAfterBeforeCommandSSHFailure(waitErr, acquired, useCoordinator, *leaseIDFlag != "", *keep, *keepOnFailure, *noSync, *syncOnly, *stopAfter, requestedSlug) {
			return false, nil
		}
		beforeCommandLeaseReplacementAttempted = true
		oldLease := LeaseTarget{Server: server, SSH: target, LeaseID: leaseID, Coordinator: coord}
		oldLeaseID := leaseID
		oldSlug := serverSlug(server)
		fmt.Fprintf(a.Stderr, "warning: SSH became unavailable after sync on lease=%s slug=%s; replacing lease once and retrying sync\n", oldLeaseID, blank(oldSlug, "-"))
		recorder.Event("lease.replace.started", "leasing", fmt.Sprintf("old_lease=%s old_slug=%s reason=ssh_before_command", oldLeaseID, blank(oldSlug, "-")))

		stopRunHeartbeat()
		recorder.resetTelemetryForLeaseReplacement()
		releaseApp := a
		if *timingJSON {
			releaseApp.Stderr = io.Discard
		}
		if err := releaseApp.releaseBackendLeaseBestEffort(context.Background(), sshBackend, cfg, oldLease); err != nil {
			recorder.Event("lease.replace.failed", "leasing", err.Error())
			return true, exit(7, "replace stale lease %s: release failed: %v", oldLeaseID, err)
		}
		acquired = false

		newLease, err := sshBackend.Acquire(ctx, AcquireRequest{Repo: repo, Options: options, Keep: *keep, Reclaim: *reclaim})
		if err != nil {
			recorder.Event("lease.replace.failed", "leasing", err.Error())
			return true, err
		}

		server, target, leaseID = newLease.Server, newLease.SSH, newLease.LeaseID
		acquired = true
		coord = newLease.Coordinator
		useCoordinator = coord != nil
		recorder.UseCoordinator(coord)
		applyResolvedServerConfig(&cfg, server)
		if err := enforceManagedLeaseCapabilities(cfg, server, leaseID); err != nil {
			return true, err
		}
		if err := validateRunArtifactGlobTarget(target, expansion.ArtifactGlobs); err != nil {
			return true, err
		}
		if err := validateRequiredRunArtifactGlobTarget(target, requiredArtifactGlobs); err != nil {
			return true, err
		}
		if expansion.Profile.Doctor.Enabled && isWindowsNativeTarget(target) {
			return true, exit(2, "profile doctor is not supported for native Windows targets")
		}
		if useCoordinator {
			recorder.AttachLease(leaseID, serverSlug(server), cfg)
			startRunHeartbeat(nil)
		}
		if err := a.claimLeaseTargetForRepoAndRegister(ctx, leaseID, serverSlug(server), cfg, server, target, repo.Root, *reclaim); err != nil {
			return true, err
		}
		workdir = remoteJoin(cfg, leaseID, repo.Name)
		if !freshPR.Empty() {
			workdir = remoteJoin(cfg, leaseID, freshPR.WorkdirName())
		}
		actionsEnvFile = ""
		profileEnvFile = ""
		actionsURL = ""
		hydratedByActions = false
		contextPrinted = false
		preflightPrinted = false
		rawJSRuntimePreflightDone = false
		exitNodeEgressChecked = false
		timings.sync = 0
		timings.syncSteps = syncStepTimings{}
		timings.syncSkipped = false
		fmt.Fprintf(a.Stderr, "retrying sync on replacement lease=%s slug=%s\n", leaseID, blank(serverSlug(server), "-"))
		recorder.Event("lease.replace.finished", "leasing", fmt.Sprintf("lease=%s slug=%s", leaseID, blank(serverSlug(server), "-")))
		return true, nil
	}
retrySync:
	if !*noSync {
		syncStart := time.Now()
		if freshPR.Empty() {
			fmt.Fprintf(a.Stderr, "syncing %s -> %s:%s\n", repo.Root, target.Host, workdir)
		} else {
			fmt.Fprintf(a.Stderr, "fresh-pr checkout %s -> %s:%s\n", freshPR.Slug(), target.Host, workdir)
		}
		stepStart := time.Now()
		recorder.Event("bootstrap.waiting", "bootstrap", "waiting for SSH before sync")
		target = bootstrapNetworkTarget(cfg, server, target)
		if err := waitForSSHReady(ctx, &target, a.Stderr, "before sync", 2*time.Minute); err != nil {
			return recordFailure(err)
		}
		a.refreshTailscaleMetadata(ctx, cfg, sshBackend, coord, useCoordinator, &server, target, leaseID)
		_ = updateLeaseClaimEndpoint(leaseID, server, target)
		if resolved, err := resolveNetworkTarget(ctx, cfg, server, target); err != nil {
			return recordFailure(err)
		} else {
			target = resolved.Target
			_ = updateLeaseClaimEndpoint(leaseID, server, target)
			if resolved.FallbackReason != "" {
				fmt.Fprintf(a.Stderr, "network fallback %s\n", resolved.FallbackReason)
			}
		}
		printContext(target)
		if !exitNodeEgressChecked {
			if err := validateTailscaleExitNodeEgress(ctx, server, target); err != nil {
				return recordFailure(err)
			}
			exitNodeEgressChecked = true
		}
		if err := preflightRawJSRuntime(target); err != nil {
			return recordFailure(err)
		}
		recorder.CaptureTelemetryStart(ctx, target)
		recorder.StartTelemetrySampler(ctx, target)
		recorder.Event("sync.started", "sync", "")
		timings.syncSteps.sshReady = time.Since(stepStart)
		if !freshPR.Empty() {
			stepStart = time.Now()
			checkoutCommand := remoteFreshPRCheckoutCommandForTarget(workdir, freshPR, target)
			out, err := runSSHCombinedOutput(ctx, target, checkoutCommand)
			if err != nil && isWindowsNativeTarget(target) {
				fmt.Fprintf(a.Stderr, "warning: fresh-pr checkout SSH failed on native Windows; refreshing SSH port and retrying once: %v\n", err)
				target.Port = cfg.SSHPort
				target.FallbackPorts = cfg.SSHFallbackPorts
				target = bootstrapNetworkTarget(cfg, server, target)
				if waitErr := waitForSSHReady(ctx, &target, a.Stderr, "before sync", 2*time.Minute); waitErr != nil {
					return recordFailure(waitErr)
				}
				if resolved, resolveErr := resolveNetworkTarget(ctx, cfg, server, target); resolveErr != nil {
					return recordFailure(resolveErr)
				} else {
					target = resolved.Target
					if resolved.FallbackReason != "" {
						fmt.Fprintf(a.Stderr, "network fallback %s\n", resolved.FallbackReason)
					}
				}
				checkoutCommand = remoteFreshPRCheckoutCommandForTarget(workdir, freshPR, target)
				out, err = runSSHCombinedOutput(ctx, target, checkoutCommand)
			}
			if err != nil {
				return recordFailure(exit(6, "fresh-pr checkout failed: %v: %s", err, strings.TrimSpace(out)))
			}
			timings.syncSteps.gitSeed = time.Since(stepStart)
			if *applyLocalPatch {
				stepStart = time.Now()
				applied, err := applyLocalPatchToFreshPR(ctx, target, workdir, repo)
				if err != nil {
					return recordFailure(err)
				}
				timings.syncSteps.finalize = time.Since(stepStart)
				if applied {
					fmt.Fprintln(a.Stderr, "fresh-pr local_patch=applied")
				} else {
					fmt.Fprintln(a.Stderr, "fresh-pr local_patch=none")
				}
			}
			timings.sync = time.Since(syncStart)
			fmt.Fprintf(a.Stderr, "fresh-pr checkout complete in %s\n", timings.sync.Round(time.Millisecond))
			recorder.Event("sync.finished", "synced", fmt.Sprintf("duration=%s fresh_pr=%s", timings.sync.Round(time.Millisecond), freshPR.Slug()))
			goto afterSync
		}
		excludes, err := syncExcludes(repo.Root, cfg)
		if err != nil {
			return recordFailure(err)
		}
		stepStart = time.Now()
		manifest, err := syncManifestFiltered(repo.Root, excludes, syncIncludes(cfg))
		if err != nil {
			return recordFailure(exit(6, "build sync file list: %v", err))
		}
		timings.syncSteps.manifest = time.Since(stepStart)
		stepStart = time.Now()
		if err := checkSyncPreflight(manifest, cfg, *forceSyncLarge, a.Stderr); err != nil {
			return recordFailure(err)
		}
		timings.syncSteps.preflight = time.Since(stepStart)
		fingerprint := ""
		if cfg.Sync.Fingerprint && !isWindowsNativeTarget(target) {
			stepStart = time.Now()
			fingerprint, err = syncFingerprintForManifest(repo, cfg, manifest, excludes)
			timings.syncSteps.fingerprintLocal = time.Since(stepStart)
			if err != nil {
				fmt.Fprintf(a.Stderr, "warning: sync fingerprint failed: %v\n", err)
			} else if !fullResyncRequested && fingerprint != "" {
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
		if fullResyncRequested {
			stepStart = time.Now()
			fmt.Fprintf(a.Stderr, "full-resync resetting remote workdir %s\n", workdir)
			resetCommand := remoteResetWorkdir(workdir)
			if isWindowsNativeTarget(target) {
				resetCommand = windowsRemoteResetWorkdir(workdir)
			}
			if err := runSSHQuiet(ctx, target, resetCommand); err != nil {
				return recordFailure(exit(7, "reset remote workdir: %v", err))
			}
			timings.syncSteps.reset = time.Since(stepStart)
		} else if isWindowsNativeTarget(target) {
			stepStart = time.Now()
			if err := runSSHQuiet(ctx, target, windowsRemoteMkdir(workdir)); err != nil {
				return recordFailure(exit(7, "create remote workdir: %v", err))
			}
			timings.syncSteps.mkdir = time.Since(stepStart)
		}
		if isWindowsNativeTarget(target) {
			stepStart = time.Now()
			if err := syncWindowsNative(ctx, target, repo, cfg, workdir, manifest, a.Stdout, a.Stderr, rsyncOptions{Debug: *debugSync, Delete: cfg.Sync.Delete, Checksum: cfg.Sync.Checksum, FullResync: fullResyncRequested, Timeout: cfg.Sync.Timeout, HeartbeatInterval: 15 * time.Second}); err != nil {
				return recordFailure(err)
			}
			timings.syncSteps.rsync = time.Since(stepStart)
			timings.sync = time.Since(syncStart)
			fmt.Fprintf(a.Stderr, "sync complete in %s\n", timings.sync.Round(time.Millisecond))
			recorder.Event("sync.finished", "synced", fmt.Sprintf("duration=%s mode=archive", timings.sync.Round(time.Millisecond)))
			goto afterSync
		}
		gitSeed, credentialBlocked := syncGitSeedDecision(cfg, repo)
		if credentialBlocked {
			warnCredentialBearingGitSeed(a.Stderr)
		}
		if gitSeed {
			stepStart = time.Now()
			if err := runSSHQuiet(ctx, target, remoteGitSeed(workdir, repo.RemoteURL, repo.Head)); err != nil {
				fmt.Fprintf(a.Stderr, "warning: remote git seed failed: %v\n", err)
			}
			timings.syncSteps.gitSeed = time.Since(stepStart)
		}
		manifestData := manifest.NUL()
		deletedData := manifest.DeletedNUL()
		stepStart = time.Now()
		manifestInput := syncManifestInputForTarget(target, manifestData, deletedData)
		manifestCtx := ctx
		var cancelManifest context.CancelFunc
		if cfg.Sync.Timeout > 0 {
			manifestCtx, cancelManifest = context.WithTimeout(ctx, cfg.Sync.Timeout)
		}
		stopManifestHeartbeat := startSyncHeartbeat(a.Stderr, stepStart, 15*time.Second)
		manifestErr := runSSHInput(manifestCtx, target, remoteWriteSyncManifestsNewForTarget(target, workdir), strings.NewReader(manifestInput), io.Discard, a.Stderr)
		stopManifestHeartbeat()
		if cancelManifest != nil {
			cancelManifest()
		}
		if manifestCtx.Err() == context.DeadlineExceeded {
			return recordFailure(exit(6, "write sync manifests timed out after %s", cfg.Sync.Timeout))
		}
		if manifestErr != nil {
			return recordFailure(exit(7, "write sync manifests: %v", manifestErr))
		}
		timings.syncSteps.manifestWrite = time.Since(stepStart)
		if shouldPruneRemoteSync(cfg.Sync.Delete, fullResyncRequested) {
			// Full resync can git-seed files that are absent from the local manifest.
			// Seed the old manifest from git so prune removes those resurrected paths.
			if shouldSeedRemotePruneManifest(hydratedByActions, fullResyncRequested) {
				if err := runSSHQuiet(ctx, target, remoteSeedSyncManifestFromGit(workdir)); err != nil {
					return recordFailure(exit(6, "remote sync seed manifest failed: %v", err))
				}
			}
			stepStart = time.Now()
			if err := runSSHQuiet(ctx, target, remotePruneSyncManifestForTarget(target, workdir)); err != nil {
				return recordFailure(exit(6, "remote sync prune failed: %v", err))
			}
			timings.syncSteps.prune = time.Since(stepStart)
		}
		stepStart = time.Now()
		if err := rsync(ctx, target, repo.Root, workdir, excludes, a.Stdout, a.Stderr, rsyncOptions{Debug: *debugSync, Delete: cfg.Sync.Delete, Checksum: cfg.Sync.Checksum, UseFilesFrom: true, FilesFrom: manifestData, NoTimes: localContainerDockerSocketSync(cfg, server), Timeout: cfg.Sync.Timeout, HeartbeatInterval: 15 * time.Second}); err != nil {
			return recordFailure(exit(6, "rsync failed: %v", err))
		}
		timings.syncSteps.rsync = time.Since(stepStart)
		baseSHA := gitHydrateBaseSHA(repo, cfg.Sync.BaseRef)
		hydrateGit := true
		if hydratedByActions {
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
			AllowMassDeletions: allowRemoteSyncMassDeletions(cfg, hydratedByActions),
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
	if !*noSync {
		if err := autoHydrateActionsIfNeeded(target); err != nil {
			return recordFailure(err)
		}
	}
	if *syncOnly {
		printPreflight(target)
		fmt.Fprintf(a.Stdout, "synced %s\n", workdir)
		fmt.Fprintln(a.Stderr, formatRunSummary(timings, time.Since(timings.started), 0))
		if *timingJSON || timingRecordEnabled {
			total := time.Since(timings.started)
			report := timingReportFromRunWithActionsURL(cfg.Provider, leaseID, serverSlug(server), timings, total, 0, actionsURL)
			populateRunTimingMetadata(&report, cfg, repo, server, leaseID, recorder.runID, workdir, nil)
			report.Label = runLabelValue
			finalTimingReport = &report
		}
		recorder.Finish(ctx, target, 0, timings.sync, 0, "", false, nil, FailureClassification{})
		return nil
	}

	commandStart := time.Now()
	recorder.Event("bootstrap.waiting", "bootstrap", "waiting for SSH before command")
	target = bootstrapNetworkTarget(cfg, server, target)
	if err := waitForSSHReady(ctx, &target, a.Stderr, "before command", 2*time.Minute); err != nil {
		replaced, replaceErr := replaceLeaseAfterBeforeCommandSSHFailure(err)
		if replaceErr != nil {
			return recordFailure(replaceErr)
		}
		if replaced {
			goto retrySync
		}
		return recordFailure(err)
	}
	a.refreshTailscaleMetadata(ctx, cfg, sshBackend, coord, useCoordinator, &server, target, leaseID)
	_ = updateLeaseClaimEndpoint(leaseID, server, target)
	if resolved, err := resolveNetworkTarget(ctx, cfg, server, target); err != nil {
		return recordFailure(err)
	} else {
		target = resolved.Target
		_ = updateLeaseClaimEndpoint(leaseID, server, target)
		if resolved.FallbackReason != "" {
			fmt.Fprintf(a.Stderr, "network fallback %s\n", resolved.FallbackReason)
		}
	}
	printContext(target)
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
	if err := preflightRawJSRuntime(target); err != nil {
		return recordFailure(err)
	}
	if len(envSelection.Profile) > 0 {
		profileEnvFile = runEnvProfilePath(firstNonBlank(recorder.runID, leaseID, "run"))
		envHelperPath := ""
		if envHelperName != "" {
			safeName, _ := safeEnvHelperName(envHelperName)
			profileEnvFile = runEnvProfilePath(safeName)
			envHelperPath = runEnvHelperPath(safeName)
		}
		if err := validateRunEnvHelperTarget(target, envHelperPath); err != nil {
			return recordFailure(err)
		}
		if err := uploadRunEnvProfile(ctx, target, workdir, profileEnvFile, envSelection.Profile); err != nil {
			return recordFailure(err)
		}
		persistEnvProfile := false
		defer func() {
			// Helper mode intentionally keeps the profile; all failure paths clean it up.
			if persistEnvProfile {
				return
			}
			if out, cleanupErr := runSSHCombinedOutput(context.Background(), target, removeRunEnvProfileCommand(target, workdir, profileEnvFile)); cleanupErr != nil {
				fmt.Fprintf(a.Stderr, "warning: remote env profile cleanup failed: %v: %s\n", cleanupErr, strings.TrimSpace(out))
			}
		}()
		if err := probeRunEnvProfile(ctx, target, workdir, profileEnvFile, envSelection.Profile, a.Stderr); err != nil {
			return recordFailure(err)
		}
		if envHelperPath != "" {
			if err := uploadRunEnvHelper(ctx, target, workdir, envHelperPath, profileEnvFile); err != nil {
				return recordFailure(err)
			}
			persistEnvProfile = true
			fmt.Fprintf(a.Stderr, "env helper remote=%s usage=%s\n", envHelperPath, shellQuote("./"+envHelperPath+" <command>"))
		}
	}
	printPreflight(target)
	if expansion.Profile.Doctor.Enabled {
		fmt.Fprintf(a.Stderr, "profile doctor profile=%s\n", cfg.Profile)
		out, err := runSSHCombinedOutput(ctx, target, remoteProfileDoctorCommand(cfg.Profile, expansion.Profile.Doctor, workdir))
		if strings.TrimSpace(out) != "" {
			fmt.Fprintln(a.Stderr, strings.TrimSpace(out))
		}
		if err != nil {
			failure := exit(7, "profile doctor failed for %s: image_prereq_missing", cfg.Profile)
			if shouldReleaseRunLease(acquired, *keep, keepFailedLease, *stopAfter, failure) {
				return recordFailure(exit(7, "%s; fix the profile image prerequisites, then rerun the command; use --keep or --stop-after never to inspect the failed lease", failure.Error()))
			}
			return recordFailure(exit(7, "%s; rerun crabbox doctor --profile %s --id %s", failure.Error(), cfg.Profile, firstNonBlank(serverSlug(server), leaseID)))
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
	commandDisplay := runCommandDisplayWithLiteralArgs(command, *shellMode, expansion.LiteralArgs)
	if script != nil {
		script = runScriptForTarget(script, target)
		if err := uploadRunScript(ctx, target, workdir, script); err != nil {
			return recordFailure(err)
		}
		fmt.Fprintf(a.Stderr, "uploaded script %s -> %s\n", script.Source, script.RemotePath)
		recorder.Event("script.uploaded", "script", script.RemotePath)
		commandDisplay = runScriptDisplay(script, command)
	}
	fmt.Fprintf(a.Stderr, "running on %s %s\n", target.Host, commandDisplay)
	recorder.Event("command.started", "command", commandDisplay)
	capabilityEnv, err := requestedCapabilityEnv(ctx, cfg, target)
	if err != nil {
		return recordFailure(err)
	}
	if envSelection.SummaryRequested {
		printEnvForwardingSummary(a.Stderr, cfg.Provider, "forwarded", cfg.EnvAllow, envSelection.Effective)
	} else {
		maybePrintEnvForwardingSummary(a.Stderr, cfg.Provider, "forwarded", cfg.EnvAllow, envSelection.Effective)
	}
	runEnv := mergeEnv(envSelection.Inline, capabilityEnv)
	envFiles := remoteRunEnvFiles(actionsEnvFile, profileEnvFile)
	useShell := shouldUseShellWithLiteralArgs(command, expansion.LiteralArgs)
	remote := remoteCommandWithEnvFiles(workdir, runEnv, envFiles, command)
	if script != nil {
		remote = remoteRunScriptCommandWithEnvFiles(workdir, runEnv, envFiles, script, command)
	} else if *shellMode {
		remote = remoteShellCommandWithEnvFiles(workdir, runEnv, envFiles, strings.Join(command, " "))
	} else if useShell {
		remote = remoteShellCommandWithEnvFiles(workdir, runEnv, envFiles, runCommandShellStringWithLiteralArgs(command, false, expansion.LiteralArgs))
	}
	if isWindowsNativeTarget(target) {
		remote = windowsRemoteCommandWithEnvFiles(workdir, runEnv, envFiles, command)
		if script != nil {
			remote = windowsRemoteRunScriptCommandWithEnvFiles(workdir, runEnv, envFiles, script, command)
		} else if *shellMode {
			remote = windowsRemoteShellCommandWithEnvFiles(workdir, runEnv, envFiles, strings.Join(command, " "))
		} else if useShell {
			remote = windowsRemoteShellCommandWithEnvFiles(workdir, runEnv, envFiles, runCommandShellStringWithLiteralArgs(command, false, expansion.LiteralArgs))
		}
	}
	var logBuffer runLogBuffer
	stdoutEvents := recorder.StreamWriter("stdout")
	stderrEvents := recorder.StreamWriter("stderr")
	stdoutTail := newStreamTailBuffer(failureTailLines)
	stderrTail := newStreamTailBuffer(failureTailLines)
	streamCaptures, err := openFailureStreamCaptures(*captureStdout, *captureStderr)
	if err != nil {
		return recordFailure(err)
	}
	defer streamCaptures.cleanup()
	phaseTracker := newCommandPhaseTracker(commandStart)
	stdoutPhaseWriter := &phaseMarkerWriter{tracker: phaseTracker}
	stderrPhaseWriter := &phaseMarkerWriter{tracker: phaseTracker}
	stdout := io.MultiWriter(a.Stdout, &logBuffer, stdoutEvents, stdoutTail, stdoutPhaseWriter)
	stderr := io.MultiWriter(a.Stderr, &logBuffer, stderrEvents, stderrTail, stderrPhaseWriter)
	stdout, stdoutCaptured, err := streamCaptures.stdout.writer(stdout, stdoutPhaseWriter, a.Stderr)
	if err != nil {
		return recordFailure(err)
	}
	if stdoutCaptured {
		stdoutEvents = nil
	}
	stderr, stderrCaptured, err := streamCaptures.stderr.writer(stderr, stderrPhaseWriter, a.Stderr)
	if err != nil {
		return recordFailure(err)
	}
	if stderrCaptured {
		stderrEvents = nil
	}
	resultsMarker := ""
	if cfg.Results.Auto {
		resultsMarker = remoteResultsMarker
		markerCommand := remoteTouchResultsMarker(workdir)
		if isWindowsNativeTarget(target) {
			markerCommand = windowsRemoteTouchResultsMarker(workdir)
		}
		if err := runSSHQuiet(ctx, target, markerCommand); err != nil {
			return recordFailure(exit(7, "prepare test result freshness marker: %v", err))
		}
	}
	code, streamErr := runSSHStreamResult(ctx, target, remote, stdout, stderr)
	if err := streamCaptures.closeAfterStream(streamErr, code, a.Stderr); err != nil {
		return recordFailure(err)
	}
	if !stdoutCaptured {
		stdoutEvents.Flush()
	}
	if !stderrCaptured {
		stderrEvents.Flush()
	}
	stdoutPhaseWriter.Flush()
	stderrPhaseWriter.Flush()
	timings.command = time.Since(commandStart)
	timings.commandPhases = phaseTracker.Finish(time.Now())
	var results *TestResultSummary
	if cfg.Results.Auto || len(cfg.Results.JUnit) > 0 {
		results, err = collectRemoteJUnitResults(ctx, target, workdir, cfg.Results, resultsMarker)
		if err != nil {
			fmt.Fprintf(a.Stderr, "warning: collect test results incomplete: %v\n", err)
		}
		if line := resultSummaryLine(results); line != "" {
			fmt.Fprintln(a.Stderr, line)
		}
	}
	var artifactFailure error
	if code == 0 && len(requiredArtifactGlobs) > 0 {
		requireOutput, err := requireRunArtifactGlobs(ctx, target, workdir, requiredArtifactGlobs)
		if err != nil {
			artifactFailure = err
			code = 7
		}
		if strings.TrimSpace(requireOutput) != "" {
			fmt.Fprintln(a.Stderr, strings.TrimSpace(requireOutput))
		}
	}
	if code == 0 {
		for _, spec := range downloads {
			bytes, local, err := downloadRemoteFile(ctx, target, workdir, spec)
			if err != nil {
				return recordFailure(err)
			}
			fmt.Fprintf(a.Stderr, "downloaded %s bytes=%d\n", local, bytes)
		}
	}
	var runArtifacts []runArtifact
	if code == 0 && len(runArtifactGlobs) > 0 {
		collected, artifactOutput, err := collectRunArtifactGlobs(ctx, target, workdir, repo.Root, recorder.runID, leaseID, runArtifactGlobs)
		if err != nil {
			return recordFailure(err)
		}
		if strings.TrimSpace(artifactOutput) != "" {
			fmt.Fprintln(a.Stderr, strings.TrimSpace(artifactOutput))
		}
		runArtifacts = append(runArtifacts, collected...)
		for _, artifact := range collected {
			fmt.Fprintf(a.Stderr, "artifact kind=%s path=%s bytes=%d\n", artifact.Kind, artifact.Path, artifact.Bytes)
		}
	}
	var testResultsFailure error
	if failRunForTestResults(code, cfg.Results, results) {
		code = 1
		testResultsFailure = ExitError{Code: code, Message: fmt.Sprintf("JUnit results contain %d failures and %d errors", results.Failures, results.Errors)}
		fmt.Fprintf(a.Stderr, "test results policy: failing run because collected JUnit reports contain failures=%d errors=%d\n", results.Failures, results.Errors)
	}
	total := time.Since(timings.started)
	classification := FailureClassification{}
	if code != 0 {
		classificationLog := logBuffer.String()
		if artifactFailure != nil {
			classificationLog = strings.TrimSpace(classificationLog + "\n" + artifactFailure.Error())
		}
		classification = ClassifyRunFailure(code, classificationLog, timings.commandPhases)
		if testResultsFailure != nil {
			classification = FailureClassification{BlockedStage: "test", RetryLikely: "false"}
		}
		timings.blockedStage = classification.BlockedStage
		timings.retryLikely = classification.RetryLikely
		failureClassificationPrinted = true
	}
	report := timingReportFromRunWithActionsURL(cfg.Provider, leaseID, serverSlug(server), timings, total, code, actionsURL)
	populateRunTimingMetadata(&report, cfg, repo, server, leaseID, recorder.runID, workdir, runArtifacts)
	report.Label = runLabelValue
	if strings.TrimSpace(*emitProof) != "" && code == 0 {
		template := cfg.ProofTemplates[strings.TrimSpace(*proofTemplate)]
		proof, err := writeRunProof(strings.TrimSpace(*emitProof), strings.TrimSpace(*proofTemplate), proofRenderInput{
			Template:    template,
			Provider:    cfg.Provider,
			LeaseID:     leaseID,
			Slug:        serverSlug(server),
			RunID:       recorder.runID,
			Command:     commandDisplay,
			LogExcerpt:  selectProofLogExcerpt(logBuffer.String()),
			ActionsURL:  actionsURL,
			Artifacts:   runArtifacts,
			Variables:   expansion.Variables,
			CommandMs:   report.CommandMs,
			ExitCode:    code,
			GeneratedAt: time.Now(),
		})
		if err != nil {
			return recordFailure(err)
		}
		runArtifacts = append(runArtifacts, proof)
		report.Artifacts = runArtifacts
		fmt.Fprintf(a.Stderr, "artifact kind=proof path=%s bytes=%d template=%s\n", proof.Path, proof.Bytes, blank(proof.Template, "default"))
	}
	recorder.Finish(ctx, target, code, timings.sync, timings.command, logBuffer.String(), logBuffer.Truncated(), results, classification)
	if a.runOutcome != nil {
		a.runOutcome.Recorded = true
		a.runOutcome.ExitCode = code
		a.runOutcome.RunID = recorder.runID
		a.runOutcome.Results = results
	}
	fmt.Fprintf(a.Stderr, "command complete in %s total=%s\n", timings.command.Round(time.Millisecond), total.Round(time.Millisecond))
	fmt.Fprintln(a.Stderr, formatRunSummary(timings, total, code))
	labelField := ""
	if runLabelValue != "" {
		labelField = fmt.Sprintf(" label=%q", runLabelValue)
	}
	fmt.Fprintf(a.Stderr, "run details provider=%s lease=%s slug=%s run=%s%s type=%s repo=%s workdir=%s actions=%s stop_command=%q idle_timeout=%s\n", cfg.Provider, leaseID, blank(serverSlug(server), "-"), blank(recorder.runID, "-"), labelField, blank(server.ServerType.Name, "-"), repo.Root, workdir, blank(actionsURL, "-"), report.StopCommand, cfg.IdleTimeout)
	if *timingJSON || timingRecordEnabled {
		finalTimingReport = &report
	}
	if code != 0 {
		printRunFailureDigest(a.Stderr, runFailureDigestInput{
			Provider:              cfg.Provider,
			TargetOS:              cfg.TargetOS,
			WindowsMode:           cfg.WindowsMode,
			LeaseID:               leaseID,
			Slug:                  serverSlug(server),
			RunID:                 recorder.runID,
			RunHistoryUnavailable: recorder.historyUnavailable,
			CommandDisplay:        commandDisplay,
			ShellMode:             *shellMode || useShell,
			ScriptMode:            script != nil,
			RoutingArgs:           runFailureDigestRoutingArgs(cfg, leaseID),
			SSHRoutingArgs:        runFailureDigestSSHRoutingArgs(cfg, leaseID),
			StopCommand:           report.StopCommand,
			Classification:        classification,
			Phases:                timings.commandPhases,
			Results:               results,
		}, stdoutTail, stderrTail, *captureStdout, *captureStderr)
		capture := FailureCaptureMetadata{
			Provider:       cfg.Provider,
			LeaseID:        leaseID,
			Slug:           serverSlug(server),
			RunID:          recorder.runID,
			Workdir:        workdir,
			ExitCode:       code,
			ActionsRunURL:  actionsURL,
			Timing:         report,
			EnvAllow:       cfg.EnvAllow,
			Env:            envSelection.Effective,
			Config:         cfg,
			StdoutPath:     streamCaptures.stdout.path(),
			StderrPath:     streamCaptures.stderr.path(),
			CaptureFlagSet: *captureOnFail,
		}
		if local, bytes, captureErr := captureFailureBundle(ctx, target, workdir, leaseID, recorder.runID, capture); captureErr != nil {
			fmt.Fprintf(a.Stderr, "warning: failure bundle failed: %v\n", captureErr)
			if local != "" {
				fmt.Fprintf(a.Stderr, "failure-bundle local=%s bytes=%d secret_risk=caller-redacts-before-sharing\n", local, bytes)
			}
		} else {
			fmt.Fprintf(a.Stderr, "failure-bundle local=%s bytes=%d secret_risk=caller-redacts-before-sharing\n", local, bytes)
		}
		if *keepOnFailure {
			if acquired && !*keep {
				keepFailedLease = true
			}
			printKeepOnFailureSSHHint(a.Stderr, cfg, leaseID, server, target)
		}
		hydrateSuggestion := rawJSRuntimeHydrateSuggestion(cfg, target, leaseID, acquired, *keep, *keepOnFailure)
		printCommandNotFoundHint(a.Stderr, cfg, target, leaseID, command, *shellMode, code, hydratedByActions, hydrateSuggestion)
		printFailureTail(a.Stderr, "stdout", stdoutTail, *captureStdout)
		printFailureTail(a.Stderr, "stderr", stderrTail, *captureStderr)
		if artifactFailure != nil {
			return recordFailure(artifactFailure)
		}
		if testResultsFailure != nil {
			return recordFailure(testResultsFailure)
		}
		return recordFailure(ExitError{Code: code, Message: fmt.Sprintf("remote command exited %d", code)})
	}
	return nil
}

func applyRunEnvAllowFlags(cfg *Config, values []string) {
	for _, value := range values {
		cfg.EnvAllow = appendUniqueStrings(cfg.EnvAllow, splitCommaList(value)...)
	}
}

func writeRunLeaseOutput(path string, session *RunSessionHandle) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if session == nil {
		return exit(2, "--lease-output was requested but provider did not return a session handle")
	}
	return writeJSONFile(path, session)
}

type countingWriteCloser struct {
	io.WriteCloser
	N int64
}

func (w *countingWriteCloser) Write(p []byte) (int, error) {
	n, err := w.WriteCloser.Write(p)
	w.N += int64(n)
	return n, err
}

func recordRunFailure(dst *error, failure error) error {
	if dst != nil && failure != nil {
		*dst = failure
	}
	return failure
}

func validateRunStopAfterPolicy(policy string) error {
	switch strings.ToLower(strings.TrimSpace(policy)) {
	case "", "success", "always", "failure", "never":
		return nil
	default:
		return exit(2, "--stop-after must be success, always, failure, or never")
	}
}

func shouldReleaseRunLease(acquired, keep, keepFailedLease bool, stopAfter string, runFailure error) bool {
	switch strings.ToLower(strings.TrimSpace(stopAfter)) {
	case "never":
		return false
	case "always":
		return true
	case "success":
		return runFailure == nil
	case "failure":
		return runFailure != nil
	default:
		return acquired && !keep && !keepFailedLease
	}
}

func populateRunTimingMetadata(report *timingReport, cfg Config, repo Repo, server Server, leaseID, runID, workdir string, artifacts []runArtifact) {
	report.RunID = runID
	report.MachineType = server.ServerType.Name
	report.RepoPath = repo.Root
	report.Workdir = workdir
	stopID := firstNonBlank(serverSlug(server), leaseID)
	if normalizeProviderName(cfg.Provider) == "external" {
		stopID = leaseID
	}
	report.StopCommand = runStopCommand(cfg, stopID)
	report.IdleTimeout = cfg.IdleTimeout.String()
	report.Artifacts = artifacts
}

func writeDelegatedRunProof(path, templateName string, cfg Config, result RunResult, req RunRequest) (runArtifact, error) {
	template := cfg.ProofTemplates[strings.TrimSpace(templateName)]
	provider := firstNonBlank(result.Provider, cfg.Provider)
	leaseID := result.LeaseID
	slug := result.Slug
	command := strings.TrimSpace(result.CommandText)
	if command == "" {
		command = runCommandDisplay(req.Command, req.ShellMode)
	}
	logExcerpt := strings.TrimSpace(result.LogExcerpt)
	if logExcerpt == "" {
		logExcerpt = "(no console output captured)"
	}
	return writeRunProof(path, templateName, proofRenderInput{
		Template:    template,
		Provider:    provider,
		LeaseID:     leaseID,
		Slug:        slug,
		Command:     command,
		LogExcerpt:  logExcerpt,
		ActionsURL:  result.ActionsURL,
		Artifacts:   result.Artifacts,
		Variables:   req.ProfileVariables,
		CommandMs:   result.Command.Milliseconds(),
		ExitCode:    result.ExitCode,
		GeneratedAt: time.Now(),
	})
}

func runCommandDisplay(command []string, shellMode bool) string {
	return runCommandDisplayWithLiteralArgs(command, shellMode, nil)
}

func runCommandDisplayWithLiteralArgs(command []string, shellMode bool, literalArgs map[int]bool) string {
	if shellMode || shouldUseShellWithLiteralArgs(command, literalArgs) {
		return runCommandShellStringWithLiteralArgs(command, shellMode, literalArgs)
	}
	return strings.Join(readableShellWords(command), " ")
}

func runCommandShellString(command []string, shellMode bool) string {
	return runCommandShellStringWithLiteralArgs(command, shellMode, nil)
}

func runCommandShellStringWithLiteralArgs(command []string, shellMode bool, literalArgs map[int]bool) string {
	if shellMode {
		return strings.Join(command, " ")
	}
	if len(command) == 1 && !literalArgs[0] {
		return command[0]
	}
	return shellScriptFromArgvWithLiteralArgs(command, literalArgs)
}

func runStopCommand(cfg Config, id string) string {
	args := []string{"crabbox", "stop", "--provider", cfg.Provider}
	if strings.TrimSpace(cfg.TargetOS) != "" {
		args = append(args, "--target", cfg.TargetOS)
	}
	if cfg.TargetOS == targetWindows && strings.TrimSpace(cfg.WindowsMode) != "" {
		args = append(args, "--windows-mode", cfg.WindowsMode)
	}
	if strings.TrimSpace(cfg.Static.Host) != "" {
		args = append(args, "--static-host", cfg.Static.Host)
	}
	if strings.TrimSpace(cfg.Static.User) != "" {
		args = append(args, "--static-user", cfg.Static.User)
	}
	if strings.TrimSpace(cfg.Static.Port) != "" {
		args = append(args, "--static-port", cfg.Static.Port)
	}
	if strings.TrimSpace(cfg.Static.WorkRoot) != "" {
		args = append(args, "--static-work-root", cfg.Static.WorkRoot)
	}
	args = appendProviderStopRoutingArgs(args, cfg, id)
	if strings.TrimSpace(id) != "" {
		args = append(args, "--id", id)
	}
	return readableShellCommand(args)
}

func appendProviderStopRoutingArgs(args []string, cfg Config, id string) []string {
	switch normalizeProviderName(cfg.Provider) {
	case "namespace-instance":
		if strings.TrimSpace(cfg.NamespaceInstance.CLIPath) != "" && cfg.NamespaceInstance.CLIPath != "nsc" {
			args = append(args, "--namespace-instance-cli", cfg.NamespaceInstance.CLIPath)
		}
		if strings.TrimSpace(cfg.NamespaceInstance.Endpoint) != "" {
			args = append(args, "--namespace-instance-endpoint", routingSafeURL(cfg.NamespaceInstance.Endpoint))
		}
		if strings.TrimSpace(cfg.NamespaceInstance.Region) != "" {
			args = append(args, "--namespace-instance-region", cfg.NamespaceInstance.Region)
		}
		if strings.TrimSpace(cfg.NamespaceInstance.Keychain) != "" {
			args = append(args, "--namespace-instance-keychain", cfg.NamespaceInstance.Keychain)
		}
	case "proxmox":
		if strings.TrimSpace(cfg.Proxmox.APIURL) != "" {
			args = append(args, "--proxmox-api-url", routingSafeURL(cfg.Proxmox.APIURL))
		}
		if strings.TrimSpace(cfg.Proxmox.Node) != "" {
			args = append(args, "--proxmox-node", cfg.Proxmox.Node)
		}
		if cfg.Proxmox.InsecureTLS {
			args = append(args, "--proxmox-insecure-tls")
		}
	case "xcp-ng":
		if strings.TrimSpace(cfg.XCPNg.APIURL) != "" {
			args = append(args, "--xcp-ng-api-url", routingSafeURL(cfg.XCPNg.APIURL))
		}
		if strings.TrimSpace(cfg.XCPNg.Username) != "" {
			args = append(args, "--xcp-ng-username", cfg.XCPNg.Username)
		}
		if strings.TrimSpace(cfg.XCPNg.Template) != "" {
			args = append(args, "--xcp-ng-template", cfg.XCPNg.Template)
		}
		if strings.TrimSpace(cfg.XCPNg.TemplateUUID) != "" {
			args = append(args, "--xcp-ng-template-uuid", cfg.XCPNg.TemplateUUID)
		}
		if strings.TrimSpace(cfg.XCPNg.SR) != "" {
			args = append(args, "--xcp-ng-sr", cfg.XCPNg.SR)
		}
		if strings.TrimSpace(cfg.XCPNg.SRUUID) != "" {
			args = append(args, "--xcp-ng-sr-uuid", cfg.XCPNg.SRUUID)
		}
		if strings.TrimSpace(cfg.XCPNg.Network) != "" {
			args = append(args, "--xcp-ng-network", cfg.XCPNg.Network)
		}
		if strings.TrimSpace(cfg.XCPNg.NetworkUUID) != "" {
			args = append(args, "--xcp-ng-network-uuid", cfg.XCPNg.NetworkUUID)
		}
		if strings.TrimSpace(cfg.XCPNg.Host) != "" {
			args = append(args, "--xcp-ng-host", cfg.XCPNg.Host)
		}
		if strings.TrimSpace(cfg.XCPNg.User) != "" {
			args = append(args, "--xcp-ng-user", cfg.XCPNg.User)
		}
		if strings.TrimSpace(cfg.XCPNg.WorkRoot) != "" {
			args = append(args, "--xcp-ng-work-root", cfg.XCPNg.WorkRoot)
		}
		if cfg.XCPNg.InsecureTLS {
			args = append(args, "--xcp-ng-insecure-tls")
		}
	case "namespace", "namespace-devbox":
		if strings.TrimSpace(cfg.Namespace.Site) != "" {
			args = append(args, "--namespace-site", cfg.Namespace.Site)
		}
		if strings.TrimSpace(cfg.Namespace.WorkRoot) != "" {
			args = append(args, "--namespace-work-root", cfg.Namespace.WorkRoot)
		}
		if DeleteOnReleaseExplicit(cfg, "namespace-devbox") {
			args = append(args, fmt.Sprintf("--namespace-delete-on-release=%t", cfg.Namespace.DeleteOnRelease))
		}
	case "coder":
		args = append(args, fmt.Sprintf("--coder-delete-on-release=%t", cfg.Coder.DeleteOnRelease))
	case "daytona":
		if strings.TrimSpace(cfg.Daytona.APIURL) != "" {
			args = append(args, "--daytona-api-url", routingSafeURL(cfg.Daytona.APIURL))
		}
		if strings.TrimSpace(cfg.Daytona.Target) != "" {
			args = append(args, "--daytona-target", cfg.Daytona.Target)
		}
		if strings.TrimSpace(cfg.Daytona.User) != "" {
			args = append(args, "--daytona-user", cfg.Daytona.User)
		}
	case "sprites":
		if strings.TrimSpace(cfg.Sprites.APIURL) != "" {
			args = append(args, "--sprites-api-url", routingSafeURL(cfg.Sprites.APIURL))
		}
	case "semaphore":
		if strings.TrimSpace(cfg.Semaphore.Host) != "" {
			args = append(args, "--semaphore-host", cfg.Semaphore.Host)
		}
	case "exe-dev":
		if strings.TrimSpace(cfg.ExeDev.ControlHost) != "" {
			args = append(args, "--exe-dev-control-host", cfg.ExeDev.ControlHost)
		}
	case "morph":
		if strings.TrimSpace(cfg.Morph.APIURL) != "" {
			args = append(args, "--morph-api-url", routingSafeURL(cfg.Morph.APIURL))
		}
		if DeleteOnReleaseExplicit(cfg, "morph") {
			args = append(args, fmt.Sprintf("--morph-delete-on-release=%t", cfg.Morph.DeleteOnRelease))
		}
	case "hostinger":
		if strings.TrimSpace(cfg.Hostinger.APIURL) != "" {
			args = append(args, "--hostinger-url", routingSafeURL(cfg.Hostinger.APIURL))
		}
	case "vast", "vast-ai", "vastai":
		if apiURL := strings.TrimSpace(cfg.Vast.APIURL); apiURL != "" {
			args = append(args, "--vast-api-url", routingSafeURL(apiURL))
		}
		if DeleteOnReleaseExplicit(cfg, "vast") {
			args = append(args, "--vast-release-action", cfg.Vast.ReleaseAction)
		}
	case "nvidia-brev":
		if cli := strings.TrimSpace(cfg.NvidiaBrev.CLI); cli != "" {
			args = append(args, "--nvidia-brev-cli", cli)
		}
		if target := strings.TrimSpace(cfg.NvidiaBrev.Target); target != "" && target != "container" {
			args = append(args, "--nvidia-brev-target", target)
		}
		if user := strings.TrimSpace(cfg.NvidiaBrev.User); user != "" {
			args = append(args, "--nvidia-brev-user", user)
		}
		if DeleteOnReleaseExplicit(cfg, "nvidia-brev") {
			args = append(args, "--nvidia-brev-release-action", cfg.NvidiaBrev.ReleaseAction)
		}
	case "kubevirt":
		if strings.TrimSpace(cfg.KubeVirt.Kubectl) != "" {
			args = append(args, "--kubevirt-kubectl", cfg.KubeVirt.Kubectl)
		}
		if strings.TrimSpace(cfg.KubeVirt.Virtctl) != "" {
			args = append(args, "--kubevirt-virtctl", cfg.KubeVirt.Virtctl)
		}
		if strings.TrimSpace(cfg.KubeVirt.Kubeconfig) != "" {
			args = append(args, "--kubevirt-kubeconfig", cfg.KubeVirt.Kubeconfig)
		} else if value := strings.TrimSpace(os.Getenv("KUBECONFIG")); value != "" {
			args = append([]string{"KUBECONFIG=" + value}, args...)
		}
		if strings.TrimSpace(cfg.KubeVirt.Context) != "" {
			args = append(args, "--kubevirt-context", cfg.KubeVirt.Context)
		}
		if strings.TrimSpace(cfg.KubeVirt.Namespace) != "" {
			args = append(args, "--kubevirt-namespace", cfg.KubeVirt.Namespace)
		}
		if strings.TrimSpace(cfg.KubeVirt.Template) != "" {
			args = append(args, "--kubevirt-template", cfg.KubeVirt.Template)
		}
		if DeleteOnReleaseExplicit(cfg, "kubevirt") {
			args = append(args, fmt.Sprintf("--kubevirt-delete-on-release=%t", cfg.KubeVirt.DeleteOnRelease))
		}
	case "incus":
		if DeleteOnReleaseExplicit(cfg, "incus") {
			args = append(args, fmt.Sprintf("--incus-delete-on-release=%t", cfg.Incus.DeleteOnRelease))
		}
	case "external":
		if path, err := ExternalRoutingPath(id); err == nil {
			args = append(args, "--external-routing-file", path)
		} else {
			if strings.TrimSpace(cfg.External.Command) != "" {
				args = append(args, "--external-command", cfg.External.Command)
			}
			if strings.TrimSpace(cfg.External.WorkRoot) != "" {
				args = append(args, "--external-work-root", cfg.External.WorkRoot)
			}
		}
	}
	return args
}

func routingSafeURL(value string) string {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return value
	}
	addedScheme := false
	parseValue := raw
	if !strings.Contains(parseValue, "://") {
		parseValue = "https://" + parseValue
		addedScheme = true
	}
	u, err := url.Parse(parseValue)
	if err != nil {
		return sanitizedMalformedConfigURL(parseValue, addedScheme)
	}
	if u.User == nil {
		return value
	}
	safe := *u
	safe.User = nil
	out := safe.String()
	if addedScheme {
		out = strings.TrimPrefix(out, "https://")
	}
	return out
}

type runTimings struct {
	started       time.Time
	sync          time.Duration
	command       time.Duration
	syncSteps     syncStepTimings
	commandPhases []timingPhase
	syncSkipped   bool
	blockedStage  string
	retryLikely   string
}

type syncStepTimings struct {
	sshReady             time.Duration
	mkdir                time.Duration
	manifest             time.Duration
	preflight            time.Duration
	reset                time.Duration
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
	if breakdown := formatCommandPhaseTimings(timings.commandPhases); breakdown != "" {
		summary += " command_phases=" + breakdown
	}
	summary += FormatFailureClassificationFields(FailureClassification{BlockedStage: timings.blockedStage, RetryLikely: timings.retryLikely})
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
	appendStep("reset", steps.reset)
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

func shouldPruneRemoteSync(deleteEnabled, fullResync bool) bool {
	return deleteEnabled || fullResync
}

func shouldSeedRemotePruneManifest(hydratedByActions, fullResync bool) bool {
	return hydratedByActions || fullResync
}

func allowRemoteSyncMassDeletions(cfg Config, hydratedByActions bool) bool {
	return hydratedByActions || len(syncIncludes(cfg)) > 0 || os.Getenv("CRABBOX_ALLOW_MASS_DELETIONS") == "1"
}

func commandNeedsHydrationHint(command []string, shellMode bool) bool {
	return len(commandRuntimePreflightTools(command, shellMode)) > 0
}

func shouldAutoHydrateActions(cfg Config, noHydrate, noSync bool, freshPR FreshPRSpec, syncOnly bool) bool {
	return strings.TrimSpace(cfg.Actions.Workflow) != "" && !noHydrate && !noSync && freshPR.Empty() && !syncOnly
}

func rawJSRuntimeHydrateSuggestion(cfg Config, target SSHTarget, leaseID string, acquired, keep, keepOnFailure bool) string {
	if strings.TrimSpace(cfg.Actions.Workflow) == "" {
		return ""
	}
	if !acquired || keep || keepOnFailure {
		return hydrateCommandSuggestion(cfg, target, leaseID, supportsActionsRunnerTarget(target))
	}
	return "rerun with --keep and then hydrate the kept lease"
}

func commandRuntimePreflightTools(command []string, shellMode bool) []string {
	words := commandWords(command, shellMode)
	if shellWordsContainFailureFallback(words) {
		return nil
	}
	var tools []string
	for len(words) > 0 {
		segment, rest := nextShellCommandSegment(words)
		tool, skip := commandSegmentRuntimePreflightTool(segment)
		if skip {
			if len(tools) == 0 {
				return nil
			}
			return tools
		}
		if tool != "" {
			tools = appendUniqueStrings(tools, tool)
		}
		words = rest
	}
	return tools
}

func commandSegmentRuntimePreflightTool(words []string) (tool string, skip bool) {
	var customPath bool
	words, customPath = stripCommandEnvPrefixes(words)
	if customPath {
		return "", true
	}
	words = stripSudoCommandPrefix(words)
	words, customPath = stripCommandEnvPrefixes(words)
	if customPath {
		return "", true
	}
	if len(words) == 0 {
		return "", false
	}
	first := cleanCommandWord(words[0])
	if strings.Contains(first, "/") && !strings.HasPrefix(first, "/") {
		return "", true
	}
	base := commandBase(first)
	if commandRunsForeignShell(base) {
		return "", true
	}
	if commandSegmentSetsPath(base, words[1:]) {
		return "", true
	}
	if commandSegmentSetsUpJSRuntime(base, words[1:]) {
		return "", true
	}
	switch base {
	case "pnpm", "npm", "npx", "corepack", "yarn", "bun":
		return first, false
	case "node":
		return first, false
	}
	if commandMayInstallRuntime(base) {
		return "", true
	}
	return "", false
}

func commandRunsForeignShell(base string) bool {
	switch strings.ToLower(base) {
	case "powershell", "powershell.exe", "pwsh", "pwsh.exe":
		return true
	}
	return false
}

func runEnvProvidesPath(env map[string]string, target SSHTarget) bool {
	for key := range env {
		if key == "PATH" || isWindowsNativeTarget(target) && strings.EqualFold(key, "PATH") {
			return true
		}
	}
	return false
}

func stripCommandEnvPrefixes(words []string) ([]string, bool) {
	for len(words) > 0 && commandBase(cleanCommandWord(words[0])) == "env" {
		var customPath bool
		words, customPath = stripEnvCommandPrefix(words[1:])
		if customPath {
			return nil, true
		}
	}
	for len(words) > 0 && shellAssignmentWord(words[0]) {
		if shellAssignmentKey(words[0]) == "PATH" {
			return nil, true
		}
		words = words[1:]
	}
	return words, false
}

func commandSegmentSetsPath(base string, args []string) bool {
	if base != "export" {
		return false
	}
	for _, arg := range args {
		if shellAssignmentWord(cleanCommandWord(arg)) && shellAssignmentKey(cleanCommandWord(arg)) == "PATH" {
			return true
		}
	}
	return false
}

func commandSegmentSetsUpJSRuntime(base string, args []string) bool {
	switch base {
	case "corepack":
		return len(args) > 0 && (cleanCommandWord(args[0]) == "enable" || cleanCommandWord(args[0]) == "prepare")
	case "npm":
		if len(args) == 0 {
			return false
		}
		action := cleanCommandWord(args[0])
		if action != "install" && action != "i" && action != "add" {
			return false
		}
		for _, arg := range args[1:] {
			arg = cleanCommandWord(arg)
			if arg == "-g" || arg == "--global" || strings.HasPrefix(arg, "--location=global") {
				return true
			}
		}
	case "yarn":
		return len(args) >= 2 && cleanCommandWord(args[0]) == "global" && cleanCommandWord(args[1]) == "add"
	}
	return false
}

func stripSudoCommandPrefix(words []string) []string {
	if len(words) == 0 || commandBase(cleanCommandWord(words[0])) != "sudo" {
		return words
	}
	words = words[1:]
	for len(words) > 0 {
		word := cleanCommandWord(words[0])
		if word == "--" {
			return words[1:]
		}
		if word == "-E" || word == "-n" || word == "-S" || word == "-H" || word == "-k" || word == "-v" {
			words = words[1:]
			continue
		}
		if word == "-u" || word == "-g" || word == "-C" || word == "-p" || word == "-h" {
			if len(words) < 2 {
				return nil
			}
			words = words[2:]
			continue
		}
		if strings.HasPrefix(word, "-u") || strings.HasPrefix(word, "-g") || strings.HasPrefix(word, "-C") || strings.HasPrefix(word, "-p") || strings.HasPrefix(word, "-h") {
			words = words[1:]
			continue
		}
		if strings.HasPrefix(word, "-") {
			return nil
		}
		return words
	}
	return nil
}

func nextShellCommandSegment(words []string) ([]string, []string) {
	for i, word := range words {
		if isShellCommandSeparator(word) {
			return words[:i], words[i+1:]
		}
	}
	return words, nil
}

func isShellCommandSeparator(word string) bool {
	switch strings.TrimSpace(word) {
	case "&&", ";", "|":
		return true
	default:
		return false
	}
}

func commandMayInstallRuntime(base string) bool {
	switch base {
	case "apt", "apt-get", "apk", "brew", "dnf", "yum", "curl", "wget", "mise", "asdf", "volta", "nvm", "source", ".", "bash", "sh", "zsh":
		return true
	default:
		return false
	}
}

func shellWordsContainFailureFallback(words []string) bool {
	for _, word := range words {
		if strings.TrimSpace(word) == "||" {
			return true
		}
	}
	return false
}

func stripEnvCommandPrefix(words []string) ([]string, bool) {
	for len(words) > 0 {
		word := cleanCommandWord(words[0])
		if shellAssignmentWord(word) {
			if shellAssignmentKey(word) == "PATH" {
				return nil, true
			}
			words = words[1:]
			continue
		}
		if word == "--" {
			return words[1:], false
		}
		if word == "-u" || word == "--unset" || word == "-C" || word == "--chdir" {
			if len(words) < 2 {
				return nil, false
			}
			words = words[2:]
			continue
		}
		if word == "-S" || word == "--split-string" {
			return nil, false
		}
		if strings.HasPrefix(word, "--unset=") || strings.HasPrefix(word, "--chdir=") {
			words = words[1:]
			continue
		}
		if word == "-i" || word == "-" || word == "--ignore-environment" || word == "-0" || word == "--null" {
			words = words[1:]
			continue
		}
		if strings.HasPrefix(word, "-") {
			return nil, false
		}
		return words, false
	}
	return nil, false
}

func commandWords(command []string, shellMode bool) []string {
	if len(command) == 0 {
		return nil
	}
	if shellMode || len(command) == 1 {
		return shellCommandWords(strings.Join(command, " "))
	}
	return append([]string(nil), command...)
}

func shellCommandWords(value string) []string {
	var words []string
	var current strings.Builder
	var quote rune
	flush := func() {
		if current.Len() == 0 {
			return
		}
		words = append(words, current.String())
		current.Reset()
	}
	for i, r := range value {
		if quote != 0 {
			if r == quote {
				quote = 0
				continue
			}
			current.WriteRune(r)
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			continue
		}
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			flush()
			continue
		}
		if i > 0 && ((value[i-1] == '&' && r == '&') || (value[i-1] == '|' && r == '|')) {
			continue
		}
		if r == ';' || r == '|' {
			flush()
			if r == '|' && i+1 < len(value) && value[i+1] == '|' {
				words = append(words, "||")
				continue
			}
			words = append(words, string(r))
			continue
		}
		if r == '&' && i+1 < len(value) && value[i+1] == '&' {
			flush()
			words = append(words, "&&")
			continue
		}
		current.WriteRune(r)
	}
	flush()
	return words
}

func shellAssignmentWord(word string) bool {
	if strings.HasPrefix(word, "-") {
		return false
	}
	idx := strings.Index(word, "=")
	if idx <= 0 {
		return false
	}
	name := word[:idx]
	for i, r := range name {
		if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func shellAssignmentKey(word string) string {
	key, _, _ := strings.Cut(word, "=")
	return key
}

func cleanCommandWord(word string) string {
	word = strings.TrimSpace(word)
	return strings.Trim(word, "'\";|&()")
}

func commandBase(word string) string {
	if idx := strings.LastIndex(word, "/"); idx >= 0 {
		word = word[idx+1:]
	}
	return word
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
	return shouldUseShellWithLiteralArgs(command, nil)
}

func shouldUseShellWithLiteralArgs(command []string, literalArgs map[int]bool) bool {
	if len(command) == 1 {
		if literalArgs[0] {
			return false
		}
		return strings.ContainsAny(command[0], " \t\r\n&|;<>*$`()")
	}
	if leadingEnvAssignment(command) {
		return true
	}
	for idx, word := range command {
		if literalArgs[idx] {
			continue
		}
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
	return len(command) > 1 && isShellEnvAssignment(command[0])
}

func LeadingEnvAssignment(command []string) bool {
	return leadingEnvAssignment(command)
}

func shellScriptFromArgvWithLiteralArgs(command []string, literalArgs map[int]bool) string {
	parts := make([]string, 0, len(command))
	seenCommand := false
	for idx, word := range command {
		if !literalArgs[idx] && isShellControlOperator(word) {
			parts = append(parts, word)
			if resetsShellCommandPosition(word) {
				seenCommand = false
			}
			continue
		}
		if !literalArgs[idx] && !seenCommand && isShellEnvAssignment(word) {
			key, value, _ := strings.Cut(word, "=")
			parts = append(parts, key+"="+shellQuote(value))
			continue
		}
		seenCommand = true
		parts = append(parts, shellQuote(word))
	}
	return strings.Join(parts, " ")
}

func validateCoordinatorLeaseCapabilities(cfg Config, lease CoordinatorLease) error {
	if cfg.Desktop && !lease.Desktop {
		return exit(5, "coordinator did not provision desktop=true for lease %s; deploy the coordinator with desktop/VNC support", blank(lease.ID, "-"))
	}
	if cfg.Desktop {
		requestedDesktopEnv := normalizedDesktopEnv(cfg.DesktopEnv)
		if requestedDesktopEnv != desktopEnvXFCE && normalizedDesktopEnv(lease.DesktopEnv) != requestedDesktopEnv {
			return exit(5, "coordinator did not provision desktopEnv=%s for lease %s; deploy the coordinator with desktop environment support", requestedDesktopEnv, blank(lease.ID, "-"))
		}
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
	if targetOS := strings.TrimSpace(server.Labels["target"]); targetOS != "" {
		cfg.TargetOS = targetOS
	}
	if windowsMode := strings.TrimSpace(server.Labels["windows_mode"]); windowsMode != "" {
		cfg.WindowsMode = windowsMode
	} else if cfg.TargetOS != targetWindows {
		cfg.WindowsMode = ""
	}
	normalizeTargetConfig(cfg)
	if cfg.Provider == "local-container" || server.Provider == "local-container" {
		if root := server.Labels["work_root"]; root != "" {
			cfg.LocalContainer.WorkRoot = root
		}
		if labelBool(server.Labels["docker_socket"]) {
			cfg.LocalContainer.DockerSocket = true
		}
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
		(strings.Contains(exitErr.Message, "timed out waiting for SSH") ||
			strings.Contains(exitErr.Message, "timed out waiting for XCP-ng guest IPv4"))
}

func IsBootstrapWaitError(err error) bool {
	return isBootstrapWaitError(err)
}

func shouldReplaceLeaseAfterBeforeCommandSSHFailure(err error, acquired, useCoordinator, explicitLeaseID, keep, keepOnFailure, noSync, syncOnly bool, stopAfter, requestedSlug string) bool {
	if !isBootstrapWaitError(err) ||
		!acquired ||
		!useCoordinator ||
		explicitLeaseID ||
		noSync ||
		syncOnly ||
		strings.TrimSpace(requestedSlug) != "" {
		return false
	}
	return shouldReleaseRunLease(acquired, keep, keepOnFailure, stopAfter, err)
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

type leaseCleanupResult struct {
	Attempted bool
	Stopped   bool
	Err       error
}

func (result leaseCleanupResult) apply(report *timingReport) {
	if !result.Attempted || report == nil {
		return
	}
	stopped := result.Stopped
	report.LeaseStopped = &stopped
	if result.Err != nil {
		report.LeaseStopErr = result.Err.Error()
	}
}

func (a App) releaseBackendLeaseBestEffort(ctx context.Context, backend SSHLeaseBackend, cfg Config, lease LeaseTarget) error {
	a.cleanupBackendLeaseConnectionsBestEffort(ctx, lease)
	return a.releaseBackendLease(ctx, backend, cfg, lease)
}

func (a App) cleanupBackendLeaseConnectionsBestEffort(ctx context.Context, lease LeaseTarget) {
	a.writeActionsHydrationStopBestEffort(ctx, lease.SSH, lease.LeaseID)
	a.cleanupMediatedEgressBestEffort(ctx, lease.LeaseID, lease)
	a.logoutRemoteTailscaleBestEffort(ctx, lease)
}

func (a App) releaseBackendLease(ctx context.Context, backend SSHLeaseBackend, cfg Config, lease LeaseTarget) error {
	fmt.Fprintf(a.Stderr, "releasing %s server=%s\n", lease.LeaseID, lease.Server.DisplayID())
	if err := backend.ReleaseLease(ctx, ReleaseLeaseRequest{Lease: lease, Force: true}); err != nil {
		fmt.Fprintf(a.Stderr, "warning: release failed for %s: %v\n", lease.LeaseID, err)
		return err
	}
	a.releaseRegisteredCoordinatorLeaseBestEffort(ctx, cfg, lease.LeaseID)
	return nil
}

func startCoordinatorHeartbeat(ctx context.Context, coord *CoordinatorClient, leaseID string, idleTimeout time.Duration, updateIdleTimeout *time.Duration, telemetryCollector leaseTelemetryCollector, stderr io.Writer) func() {
	rootCtx, cancel := context.WithCancel(ctx)
	interval := heartbeatInterval(idleTimeout)
	done := make(chan struct{})
	go func() {
		defer close(done)
		var control *coordinatorControlConn
		defer func() {
			if control != nil {
				control.close()
			}
		}()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			telemetry := collectLeaseTelemetryBestEffort(rootCtx, telemetryCollector)
			callCtx, heartbeatCancel := context.WithTimeout(rootCtx, 20*time.Second)
			var err error
			var idleTimeoutOverride *time.Duration
			if updateIdleTimeout != nil {
				idleTimeoutOverride = updateIdleTimeout
			}
			if control == nil {
				control, _ = dialCoordinatorControl(callCtx, coord)
			}
			if control != nil {
				err = control.heartbeat(callCtx, leaseID, idleTimeoutOverride, telemetry)
				if err != nil {
					control.close()
					control = nil
				}
			}
			if control == nil {
				if updateIdleTimeout != nil {
					_, err = coord.UpdateLeaseIdleTimeoutWithTelemetry(callCtx, leaseID, *updateIdleTimeout, telemetry)
				} else {
					_, err = coord.TouchLeaseWithTelemetry(callCtx, leaseID, telemetry)
				}
			} else {
				err = nil
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
		// Canonical IDs are exact identities, never aliases. Falling through to
		// slug or provider-name matching could retarget a destructive operation.
		return Server{}, "", nil
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
	id := fs.String("id", "", "lease id or slug")
	reclaim := fs.Bool("reclaim", false, "adopt an unclaimed provider resource before stopping it")
	expectedLeaseID := fs.String("expected-provider-lease-id", "", "internal: immutable provider lease identity")
	expectedAttemptLeaseID := fs.String("expected-provider-attempt-lease-id", "", "internal: immutable provider attempt identity")
	expectedSlug := fs.String("expected-provider-slug", "", "internal: immutable provider slug identity")
	expectedResourceID := fs.String("expected-provider-resource-id", "", "internal: immutable provider resource identity")
	expectedProviderScope := fs.String("expected-provider-scope", "", "internal: immutable provider configuration scope")
	expectedCoordinatorRegistrationURL := fs.String("expected-coordinator-registration-url", "", "internal: immutable coordinator registration binding")
	confirmedAbsentLocalCleanup := fs.Bool("confirmed-absent-local-cleanup", false, "internal: remove local state after complete provider absence proof")
	providerFlags := registerProviderFlags(fs, defaults)
	targetFlags := registerTargetFlags(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	idFlagSet := flagWasSet(fs, "id")
	setIDFromFirstArg(fs, id)
	if strings.TrimSpace(*id) == "" || fs.NArg() > 1 || (idFlagSet && fs.NArg() > 0) {
		return exit(2, "usage: crabbox stop --id <lease-or-server-id>")
	}
	expectedFlagNames := []string{
		"expected-provider-lease-id",
		"expected-provider-attempt-lease-id",
		"expected-provider-slug",
		"expected-provider-resource-id",
	}
	expectedFlagCount := 0
	for _, name := range expectedFlagNames {
		if flagWasSet(fs, name) {
			expectedFlagCount++
		}
	}
	if expectedFlagCount != 0 && expectedFlagCount != len(expectedFlagNames) {
		return exit(2, "internal provider release requires the complete expected identity set")
	}
	if *confirmedAbsentLocalCleanup && (expectedFlagCount != len(expectedFlagNames) || !flagWasSet(fs, "expected-provider-scope") || !flagWasSet(fs, "expected-coordinator-registration-url") || !flagWasSet(fs, "provider")) {
		return exit(2, "confirmed-absence local cleanup requires explicit provider, scope, coordinator binding, and complete expected identity set")
	}
	if flagWasSet(fs, "expected-coordinator-registration-url") {
		if !*confirmedAbsentLocalCleanup {
			return exit(2, "expected coordinator registration binding is only valid for confirmed-absence cleanup")
		}
		if err := validateControllerCoordinatorRegistrationURL(*expectedCoordinatorRegistrationURL); err != nil {
			return exit(2, "invalid expected coordinator registration binding: %v", err)
		}
	}
	if flagWasSet(fs, "expected-provider-scope") {
		scope := strings.TrimSpace(*expectedProviderScope)
		if scope == "" || scope != *expectedProviderScope || !validControllerInventoryIdentity(scope) {
			return exit(2, "invalid expected provider scope")
		}
	}
	expectedIdentity := ProviderIdentityExpectation{
		LeaseID:        *expectedLeaseID,
		AttemptLeaseID: *expectedAttemptLeaseID,
		Slug:           *expectedSlug,
		ResourceID:     *expectedResourceID,
	}
	if expectedFlagCount != 0 {
		if err := ValidateProviderIdentityExpectation(expectedIdentity); err != nil {
			return err
		}
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if err := prepareProviderSelection(&cfg, *provider); err != nil {
		return err
	}
	if *confirmedAbsentLocalCleanup {
		resolvedProvider, err := ProviderFor(cfg.Provider)
		if err != nil {
			return err
		}
		if resolvedProvider.Name() == "external" {
			leaseID := firstNonBlank(expectedIdentity.LeaseID, expectedIdentity.AttemptLeaseID)
			path, err := ExternalRoutingPath(leaseID)
			if err != nil {
				return err
			}
			if _, err := os.Stat(path); err == nil {
				cfg.External.RoutingFile = path
				cfg.External.routingLoaded = false
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	if err := autoRouteStaticLease(&cfg, fs, *id); err != nil {
		return err
	}
	if err := autoRouteExternalLease(&cfg, fs, *id); err != nil {
		return err
	}
	if err := applyProviderFlags(&cfg, fs, providerFlags); err != nil {
		return err
	}
	if err := applyTargetFlagOverrides(&cfg, fs, targetFlags); err != nil {
		return err
	}
	if err := finalizeProviderSelection(&cfg); err != nil {
		return err
	}
	if flagWasSet(fs, "expected-provider-scope") {
		_, actualScope, _, err := controllerProviderIdentityForConfig(cfg)
		if err != nil {
			return err
		}
		if actualScope != *expectedProviderScope {
			return exit(4, "provider configuration scope changed before lifecycle operation")
		}
	}
	if *confirmedAbsentLocalCleanup {
		actualCoordinatorRegistrationURL, err := coordinatorRegistrationURLForConfig(cfg)
		if err != nil {
			return err
		}
		if actualCoordinatorRegistrationURL != *expectedCoordinatorRegistrationURL {
			return exit(4, "coordinator registration binding changed before confirmed-absence cleanup")
		}
	}
	backend, err := loadBackend(cfg, runtimeForApp(a))
	if err != nil {
		return err
	}
	if *confirmedAbsentLocalCleanup {
		// Validate the immutable local identity before the network mutation, but
		// retain its route and claim until coordinator deregistration succeeds.
		// A failed deregistration must remain retryable with the persisted route.
		if _, err := confirmedAbsentLocalStateSnapshot(ctx, backend, expectedIdentity, *expectedProviderScope); err != nil {
			return err
		}
		if err := a.releaseRegisteredCoordinatorLeaseAfterConfirmedAbsence(ctx, cfg, expectedIdentity.LeaseID); err != nil {
			return fmt.Errorf("deregister coordinator lease after confirmed provider absence: %w", err)
		}
		if err := cleanupConfirmedAbsentLocalState(ctx, backend, expectedIdentity, *expectedProviderScope); err != nil {
			return err
		}
		return nil
	}
	if delegated, ok := backend.(DelegatedRunBackend); ok {
		if !expectedIdentity.empty() {
			return exit(2, "provider=%s cannot validate an expected release identity", backend.Spec().Name)
		}
		if *reclaim {
			reclaimer, ok := backend.(StopReclaimBackend)
			if !ok {
				return exit(2, "provider=%s does not support stop --reclaim", backend.Spec().Name)
			}
			return reclaimer.ReclaimAndStop(ctx, StopRequest{Options: leaseOptionsFromConfig(cfg), ID: *id})
		}
		return delegated.Stop(ctx, StopRequest{Options: leaseOptionsFromConfig(cfg), ID: *id})
	}
	if *reclaim {
		return exit(2, "provider=%s does not support stop --reclaim", backend.Spec().Name)
	}
	sshBackend, ok := backend.(SSHLeaseBackend)
	if !ok {
		return exit(2, "provider=%s does not support stop", backend.Spec().Name)
	}
	lease, err := sshBackend.Resolve(ctx, ResolveRequest{
		Options:                  leaseOptionsFromConfig(cfg),
		ID:                       *id,
		ReleaseOnly:              true,
		ExpectedProviderIdentity: expectedIdentity,
	})
	if err != nil {
		if backendCoordinator(backend) != nil {
			fmt.Fprintf(a.Stderr, "warning: could not inspect lease before release: %v\n", err)
			lease = LeaseTarget{LeaseID: *id}
		} else {
			return err
		}
	}
	if err := ValidateLeaseTargetProviderIdentity(lease, expectedIdentity); err != nil {
		return err
	}
	if lease.SSH.Host != "" {
		a.writeActionsHydrationStopBestEffort(ctx, lease.SSH, lease.LeaseID)
	}
	a.cleanupMediatedEgressBestEffort(ctx, *id, lease)
	a.logoutRemoteTailscaleBestEffort(ctx, lease)
	if err := sshBackend.ReleaseLease(ctx, ReleaseLeaseRequest{
		Lease:                    lease,
		Force:                    true,
		ExpectedProviderIdentity: expectedIdentity,
	}); err != nil {
		return err
	}
	a.releaseRegisteredCoordinatorLeaseBestEffort(ctx, cfg, lease.LeaseID)
	if backendCoordinator(backend) != nil {
		fmt.Fprintf(a.Stderr, "released lease=%s server=%s\n", lease.LeaseID, lease.Server.DisplayID())
		return nil
	}
	if reporter, ok := backend.(ReleaseLeaseReporter); ok {
		fmt.Fprintln(a.Stderr, reporter.ReleaseLeaseMessage(lease))
		return nil
	}
	fmt.Fprintf(a.Stderr, "deleted lease=%s server=%s name=%s\n", lease.LeaseID, lease.Server.DisplayID(), lease.Server.Name)
	return nil
}

type confirmedAbsentLocalState struct {
	leaseID     string
	claim       leaseClaim
	claimExists bool
}

func confirmedAbsentLocalStateSnapshot(ctx context.Context, backend Backend, expected ProviderIdentityExpectation, providerScope string) (confirmedAbsentLocalState, error) {
	if err := ctx.Err(); err != nil {
		return confirmedAbsentLocalState{}, err
	}
	if err := ValidateProviderIdentityExpectation(expected); err != nil {
		return confirmedAbsentLocalState{}, err
	}
	leaseID := firstNonBlank(expected.LeaseID, expected.AttemptLeaseID)
	if expected.LeaseID != "" && expected.AttemptLeaseID != "" && expected.LeaseID != expected.AttemptLeaseID {
		return confirmedAbsentLocalState{}, exit(4, "provider lease identity changed before confirmed-absence cleanup")
	}
	provider := backend.Spec().Name
	claim, claimExists, err := readLeaseClaimWithPresence(leaseID)
	if err != nil {
		return confirmedAbsentLocalState{}, err
	}
	if claimExists {
		if claim.Provider != provider {
			return confirmedAbsentLocalState{}, exit(4, "lease claim provider changed before confirmed-absence cleanup")
		}
		if claim.ProviderScope != providerScope {
			return confirmedAbsentLocalState{}, exit(4, "lease claim provider scope changed before confirmed-absence cleanup")
		}
		for _, identity := range []string{expected.LeaseID, expected.AttemptLeaseID} {
			if identity != "" && claim.LeaseID != identity {
				return confirmedAbsentLocalState{}, exit(4, "lease claim identity changed before confirmed-absence cleanup")
			}
		}
		if expected.Slug != "" && claim.Slug != expected.Slug {
			return confirmedAbsentLocalState{}, exit(4, "lease claim slug changed before confirmed-absence cleanup")
		}
		if expected.ResourceID != "" && claim.CloudID != expected.ResourceID {
			return confirmedAbsentLocalState{}, exit(4, "lease claim resource identity changed before confirmed-absence cleanup")
		}
	}
	return confirmedAbsentLocalState{leaseID: leaseID, claim: claim, claimExists: claimExists}, nil
}

func cleanupConfirmedAbsentLocalState(ctx context.Context, backend Backend, expected ProviderIdentityExpectation, providerScope string) error {
	state, err := confirmedAbsentLocalStateSnapshot(ctx, backend, expected, providerScope)
	if err != nil {
		return err
	}
	cleanupSidecars := func() error {
		cleaner, ok := backend.(ConfirmedAbsentLocalStateCleaner)
		if !ok {
			return nil
		}
		return cleaner.CleanupConfirmedAbsentLocalState(ctx, ConfirmedAbsentLocalCleanupRequest{
			ExpectedProviderIdentity: expected,
			ProviderScope:            providerScope,
		})
	}
	return cleanupLeaseClaimIfUnchangedAfter(state.leaseID, state.claim, state.claimExists, cleanupSidecars)
}

func (a App) writeActionsHydrationStopBestEffort(ctx context.Context, target SSHTarget, leaseID string) {
	if !shouldWriteActionsHydrationStop(leaseID, target) {
		return
	}
	stopCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	hydrated := false
	if state, err := readActionsHydrationState(stopCtx, target, leaseID); err == nil && state.Workspace != "" {
		hydrated = true
	}
	if err := writeActionsHydrationStop(stopCtx, target, leaseID); err != nil {
		fmt.Fprintf(a.Stderr, "warning: could not stop GitHub Actions hydration for %s: %v\n", leaseID, err)
		return
	}
	if hydrated {
		time.Sleep(actionsHydrationStopSettleDelay)
	}
}

func shouldWriteActionsHydrationStop(leaseID string, target SSHTarget) bool {
	return leaseID != "" && target.Host != ""
}

const actionsHydrationStopSettleDelay = 20 * time.Second

func leaseDisplayID(lease CoordinatorLease) string {
	if lease.CloudID != "" {
		return lease.CloudID
	}
	return fmt.Sprint(lease.ServerID)
}

func localContainerDockerSocketSync(cfg Config, server Server) bool {
	if cfg.Provider != "local-container" && server.Provider != "local-container" {
		return false
	}
	return cfg.LocalContainer.DockerSocket || labelBool(server.Labels["docker_socket"])
}

func localContainerDockerSocketConfig(cfg Config) bool {
	return cfg.Provider == "local-container" && cfg.LocalContainer.DockerSocket
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
