package cli

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type GitHubRepo struct {
	Owner string
	Name  string
}

func (r GitHubRepo) Slug() string {
	if r.Owner == "" || r.Name == "" {
		return ""
	}
	return r.Owner + "/" + r.Name
}

func (a App) actionsHydrate(ctx context.Context, args []string) error {
	started := time.Now()
	defaults := defaultConfig()
	fs := newFlagSet("actions hydrate", a.Stderr)
	provider := fs.String("provider", defaults.Provider, providerHelpAll())
	providerFlags := registerProviderFlags(fs, defaults)
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	leaseIDFlag := fs.String("id", "", "existing lease id or slug")
	repoFlag := fs.String("repo", "", "GitHub repository owner/name")
	workflowFlag := fs.String("workflow", "", "workflow file/name/id")
	jobFlag := fs.String("job", "", "expected hydrate workflow job/input name")
	refFlag := fs.String("ref", "", "workflow ref")
	waitTimeout := fs.Duration("wait-timeout", 20*time.Minute, "time to wait for Actions hydration")
	keepAliveMinutes := fs.Int("keep-alive-minutes", 90, "minutes for workflow to keep the job alive")
	githubRunner := fs.Bool("github-runner", false, "hydrate by registering a GitHub self-hosted runner instead of local SSH execution")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	timingJSON := fs.Bool("timing-json", false, "print final timing as JSON")
	fieldFlags := stringListFlag{}
	fs.Var(&fieldFlags, "f", "workflow input key=value")
	fs.Var(&fieldFlags, "field", "workflow input key=value")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *leaseIDFlag == "" {
		return exit(2, "actions hydrate requires --id")
	}
	if skipped, skippedID, err := shouldSkipBlacksmithActionsHydrate(*leaseIDFlag, *provider); err != nil {
		return err
	} else if skipped {
		fmt.Fprintf(a.Stdout, "actions hydrate skipped id=%s provider=blacksmith-testbox reason=provider-owned\n", skippedID)
		fmt.Fprintf(a.Stdout, "actions hydrate complete total=%s\n", time.Since(started).Round(time.Millisecond))
		if *timingJSON {
			total := time.Since(started)
			if err := writeTimingJSON(a.Stderr, timingReport{
				Provider: "blacksmith-testbox",
				LeaseID:  skippedID,
				TotalMs:  total.Milliseconds(),
				ExitCode: 0,
			}); err != nil {
				return err
			}
		}
		return nil
	}
	cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{LeaseID: *leaseIDFlag})
	if err != nil {
		return err
	}
	if err := applyProviderFlags(&cfg, fs, providerFlags); err != nil {
		return err
	}
	repo, err := findRepo()
	if err != nil {
		return err
	}
	if *repoFlag != "" {
		cfg.Actions.Repo = *repoFlag
	}
	if *workflowFlag != "" {
		cfg.Actions.Workflow = *workflowFlag
	}
	if *jobFlag != "" {
		cfg.Actions.Job = *jobFlag
	}
	if *refFlag != "" {
		cfg.Actions.Ref = *refFlag
	}
	if cfg.Actions.Workflow == "" {
		return exit(2, "actions hydrate requires --workflow or actions.workflow")
	}
	server, target, leaseID, slug, err := a.resolveLeaseTargetForActions(ctx, cfg, *leaseIDFlag, repo, *reclaim)
	if err != nil {
		return err
	}
	applyResolvedServerConfig(&cfg, server)
	target = targetWithConfigDefaults(target, cfg)
	expectedClaim, expectedClaimExists, err := resolvedLeaseClaimSnapshot(leaseID, server)
	if err != nil {
		return err
	}
	ownedClaim, err := claimLeaseForRepoConfigIfUnchanged(
		leaseID,
		slug,
		cfg,
		repo.Root,
		cfg.IdleTimeout,
		*reclaim,
		expectedClaim,
		expectedClaimExists,
	)
	if err != nil {
		return err
	}
	if resolved, err := resolveNetworkTarget(ctx, cfg, server, target); err != nil {
		return err
	} else {
		target = resolved.Target
		if resolved.FallbackReason != "" {
			fmt.Fprintf(a.Stderr, "network fallback %s\n", resolved.FallbackReason)
		}
	}
	if err := waitForSSHReady(ctx, &target, a.Stderr, "actions hydrate", 2*time.Minute); err != nil {
		return err
	}
	if _, err := updateLeaseClaimEndpointIfUnchanged(leaseID, ownedClaim, server, target); err != nil {
		return err
	}
	a.registerCoordinatorLeaseBestEffort(ctx, cfg, LeaseTarget{Server: server, SSH: target, LeaseID: leaseID})
	backend, err := loadBackend(cfg, runtimeForApp(a))
	if err != nil {
		return err
	}
	if coord := backendCoordinator(backend); coord != nil {
		stopHeartbeat := startCoordinatorHeartbeat(ctx, coord, leaseID, cfg.IdleTimeout, nil, leaseTelemetryCollectorForTarget(target), a.Stderr)
		defer stopHeartbeat()
	} else if sshBackend, ok := backend.(SSHLeaseBackend); ok {
		_, err := sshBackend.Touch(ctx, TouchRequest{Lease: LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, State: blank(server.Labels["state"], "ready"), IdleTimeout: cfg.IdleTimeout})
		if err != nil {
			fmt.Fprintf(a.Stderr, "warning: touch failed for %s: %v\n", leaseID, err)
		}
	}
	label := githubActionsLeaseLabel(leaseID)
	ref := actionsRef(cfg, repo)
	extraFields := mergeWorkflowInputFields(cfg.Actions.Fields, fieldFlags)
	fields := actionsHydrateFields(leaseID, label, cfg.Actions.Job, *keepAliveMinutes, extraFields)
	if !*githubRunner {
		localFields := actionsHydrateFields(leaseID, label, cfg.Actions.Job, 0, extraFields)
		if state, err := a.hydrateActionsLocally(ctx, cfg, repo, target, leaseID, cfg.Actions.Job, localFields, *waitTimeout, true, true); err == nil {
			fmt.Fprintf(a.Stdout, "actions hydrated local id=%s slug=%s workspace=%s run_id=%s\n", leaseID, blank(slug, "-"), state.Workspace, blank(state.RunID, "-"))
			fmt.Fprintf(a.Stdout, "actions hydrate complete total=%s\n", time.Since(started).Round(time.Millisecond))
			if *timingJSON {
				total := time.Since(started)
				if err := writeTimingJSON(a.Stderr, timingReport{
					Provider: cfg.Provider,
					LeaseID:  leaseID,
					Slug:     slug,
					TotalMs:  total.Milliseconds(),
					ExitCode: 0,
				}); err != nil {
					return err
				}
			}
			return nil
		} else {
			return exit(exitCodeForError(err, 7), "local Actions hydration failed for %s: %v; rerun with --github-runner when the workflow needs full GitHub Actions semantics", leaseID, err)
		}
	}
	ghRepo, err := resolveGitHubRepo(repo, cfg.Actions.Repo)
	if err != nil {
		return err
	}
	state, err := a.hydrateActionsWithGitHubRunner(ctx, cfg, repo, target, leaseID, slug, ghRepo, label, ref, fields, *waitTimeout)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "actions hydrated id=%s slug=%s workspace=%s run_id=%s\n", leaseID, blank(slug, "-"), state.Workspace, blank(state.RunID, "-"))
	fmt.Fprintf(a.Stdout, "actions hydrate complete total=%s\n", time.Since(started).Round(time.Millisecond))
	if *timingJSON {
		total := time.Since(started)
		if err := writeTimingJSON(a.Stderr, timingReport{
			Provider:      cfg.Provider,
			LeaseID:       leaseID,
			Slug:          slug,
			TotalMs:       total.Milliseconds(),
			ExitCode:      0,
			ActionsRunURL: actionsRunURL(ghRepo, state.RunID),
		}); err != nil {
			return err
		}
	}
	return nil
}

func (a App) hydrateActionsWithGitHubRunner(ctx context.Context, cfg Config, repo Repo, target SSHTarget, leaseID, slug string, ghRepo GitHubRepo, label, ref string, fields []string, waitTimeout time.Duration) (actionsHydrationState, error) {
	if err := a.registerGitHubActionsRunner(ctx, cfg, target, leaseID, slug, ghRepo, "", nil); err != nil {
		return actionsHydrationState{}, err
	}
	if err := clearActionsHydrationState(ctx, target, leaseID); err != nil {
		return actionsHydrationState{}, err
	}
	if inputs, ok, err := githubWorkflowDispatchInputs(ctx, repo.Root, ghRepo, cfg.Actions.Workflow, ref); err != nil {
		fmt.Fprintf(a.Stderr, "warning: inspect workflow inputs failed: %v\n", err)
	} else if ok {
		filtered, dropped := filterWorkflowInputs(fields, inputs)
		for _, field := range dropped {
			fmt.Fprintf(a.Stderr, "warning: workflow %s does not declare input %s; omitting it\n", cfg.Actions.Workflow, fieldName(field))
		}
		fields = filtered
		for _, required := range []string{"crabbox_id", "crabbox_runner_label", "crabbox_keep_alive_minutes"} {
			if !inputs[required] {
				return actionsHydrationState{}, exit(2, "workflow %s at %s does not declare required hydrate input %s", cfg.Actions.Workflow, ref, required)
			}
		}
	}
	expectedJob := cfg.Actions.Job
	if !workflowFieldsContain(fields, "crabbox_job") {
		expectedJob = ""
	}
	if err := dispatchGitHubActionsWorkflow(ctx, repo.Root, ghRepo, cfg.Actions.Workflow, ref, fields); err != nil {
		if expectedJob != "" && strings.Contains(err.Error(), "Unexpected input") {
			fields = dropWorkflowField(fields, "crabbox_job")
			expectedJob = ""
			fmt.Fprintf(a.Stderr, "warning: retrying workflow dispatch without crabbox_job for compatibility\n")
			if retryErr := dispatchGitHubActionsWorkflow(ctx, repo.Root, ghRepo, cfg.Actions.Workflow, ref, fields); retryErr != nil {
				return actionsHydrationState{}, retryErr
			}
		} else {
			return actionsHydrationState{}, err
		}
	}
	fmt.Fprintf(a.Stdout, "dispatched workflow=%s repo=%s ref=%s runner_label=%s\n", cfg.Actions.Workflow, ghRepo.Slug(), ref, label)
	return waitForActionsHydration(ctx, target, leaseID, expectedJob, waitTimeout, a.Stderr)
}

func (a App) actionsRegister(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("actions register", a.Stderr)
	provider := fs.String("provider", defaults.Provider, providerHelpAll())
	providerFlags := registerProviderFlags(fs, defaults)
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	leaseIDFlag := fs.String("id", "", "existing lease id or slug")
	repoFlag := fs.String("repo", "", "GitHub repository owner/name")
	nameFlag := fs.String("name", "", "runner name")
	labelsFlag := fs.String("labels", "", "comma-separated extra runner labels")
	versionFlag := fs.String("version", "", "actions/runner version or latest")
	ephemeralFlag := fs.Bool("ephemeral", true, "register runner as ephemeral")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *leaseIDFlag == "" {
		return exit(2, "actions register requires --id")
	}
	cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{LeaseID: *leaseIDFlag})
	if err != nil {
		return err
	}
	if err := applyProviderFlags(&cfg, fs, providerFlags); err != nil {
		return err
	}
	repo, err := findRepo()
	if err != nil {
		return err
	}
	if *repoFlag != "" {
		cfg.Actions.Repo = *repoFlag
	}
	if *versionFlag != "" {
		cfg.Actions.RunnerVersion = *versionFlag
	}
	if flagWasSet(fs, "ephemeral") {
		cfg.Actions.Ephemeral = *ephemeralFlag
	}
	extraLabels := splitCommaList(*labelsFlag)
	ghRepo, err := resolveGitHubRepo(repo, cfg.Actions.Repo)
	if err != nil {
		return err
	}
	server, target, leaseID, slug, err := a.resolveLeaseTargetForActions(ctx, cfg, *leaseIDFlag, repo, *reclaim)
	if err != nil {
		return err
	}
	applyResolvedServerConfig(&cfg, server)
	target = targetWithConfigDefaults(target, cfg)
	if err := a.claimResolvedLeaseTargetForRepoAndRegister(ctx, leaseID, slug, cfg, server, target, repo.Root, *reclaim); err != nil {
		return err
	}
	a.touchLeaseTargetBestEffort(ctx, cfg, LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, "")
	return a.registerGitHubActionsRunner(ctx, cfg, target, leaseID, slug, ghRepo, *nameFlag, extraLabels)
}

func (a App) actionsDispatch(ctx context.Context, args []string) error {
	fs := newFlagSet("actions dispatch", a.Stderr)
	repoFlag := fs.String("repo", "", "GitHub repository owner/name")
	workflowFlag := fs.String("workflow", "", "workflow file/name/id")
	refFlag := fs.String("ref", "", "workflow ref")
	fieldFlags := stringListFlag{}
	fs.Var(&fieldFlags, "f", "workflow input key=value")
	fs.Var(&fieldFlags, "field", "workflow input key=value")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	repo, err := findRepo()
	if err != nil {
		return err
	}
	if *repoFlag != "" {
		cfg.Actions.Repo = *repoFlag
	}
	if *workflowFlag != "" {
		cfg.Actions.Workflow = *workflowFlag
	}
	if *refFlag != "" {
		cfg.Actions.Ref = *refFlag
	}
	ghRepo, err := resolveGitHubRepo(repo, cfg.Actions.Repo)
	if err != nil {
		return err
	}
	if cfg.Actions.Workflow == "" {
		return exit(2, "actions dispatch requires --workflow or actions.workflow")
	}
	ref := actionsRef(cfg, repo)
	if err := dispatchGitHubActionsWorkflow(ctx, repo.Root, ghRepo, cfg.Actions.Workflow, ref, fieldFlags); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "dispatched workflow=%s repo=%s ref=%s\n", cfg.Actions.Workflow, ghRepo.Slug(), ref)
	return nil
}

func (a App) registerGitHubActionsRunner(ctx context.Context, cfg Config, target SSHTarget, leaseID, slug string, ghRepo GitHubRepo, nameOverride string, extraLabels []string) error {
	if !supportsGitHubActionsRunnerTarget(target) {
		return exit(2, "actions runner registration currently supports Linux and Windows targets only")
	}
	token, err := githubActionsRegistrationToken(ctx, ghRepo)
	if err != nil {
		return err
	}
	name := nameOverride
	if name == "" {
		name = leaseProviderName(leaseID, slug)
	}
	labels := githubActionsRunnerLabels(cfg, leaseID, slug, extraLabels)
	script := githubActionsRunnerInstallScriptForTarget(cfg.Actions.RunnerVersion, cfg.Actions.Ephemeral, target)
	remote := githubActionsRunnerInstallRemoteCommand(target)
	input := githubActionsRunnerInstallInput(ghRepo.Slug(), name, strings.Join(labels, ","), token, script)
	if err := runSSHInputQuiet(ctx, target, remote, input); err != nil {
		return exit(7, "register GitHub Actions runner on %s: %v", target.Host, err)
	}
	fmt.Fprintf(a.Stdout, "actions runner registered repo=%s name=%s labels=%s ephemeral=%t\n", ghRepo.Slug(), name, strings.Join(labels, ","), cfg.Actions.Ephemeral)
	return nil
}

func supportsActionsRunnerTarget(target SSHTarget) bool {
	return supportsGitHubActionsRunnerTarget(target)
}

func supportsLocalActionsHydrateTarget(target SSHTarget) bool {
	return target.TargetOS == "" || target.TargetOS == targetLinux || isWindowsWSL2Target(target)
}

func supportsGitHubActionsRunnerTarget(target SSHTarget) bool {
	return target.TargetOS == "" || target.TargetOS == targetLinux || target.TargetOS == targetWindows || isWindowsWSL2Target(target)
}

func targetWithConfigDefaults(target SSHTarget, cfg Config) SSHTarget {
	if target.TargetOS == "" {
		target.TargetOS = cfg.TargetOS
	}
	if target.WindowsMode == "" {
		target.WindowsMode = cfg.WindowsMode
	}
	return target
}

func (a App) resolveLeaseTargetForActions(ctx context.Context, cfg Config, id string, repo Repo, reclaim bool) (Server, SSHTarget, string, string, error) {
	server, target, leaseID, err := a.resolveLeaseTargetForRepo(ctx, cfg, id, repo, reclaim)
	return server, target, leaseID, serverSlug(server), err
}

func shouldSkipBlacksmithActionsHydrate(identifier, provider string) (bool, string, error) {
	if isBlacksmithProvider(provider) || strings.HasPrefix(identifier, "tbx_") {
		return true, identifier, nil
	}
	claim, ok, err := resolveLeaseClaim(identifier)
	if err != nil || !ok {
		return false, "", err
	}
	if isBlacksmithProvider(claim.Provider) {
		return true, claim.LeaseID, nil
	}
	return false, "", nil
}

func dispatchGitHubActionsWorkflow(ctx context.Context, dir string, repo GitHubRepo, workflow, ref string, fields []string) error {
	cmdArgs := []string{"workflow", "run", workflow, "--repo", repo.Slug(), "--ref", ref}
	if err := validateWorkflowInputFields(fields); err != nil {
		return err
	}
	for _, field := range fields {
		cmdArgs = append(cmdArgs, "-f", field)
	}
	return runGH(ctx, dir, cmdArgs...)
}

func exitCodeForError(err error, fallback int) int {
	var exitErr ExitError
	if AsExitError(err, &exitErr) && exitErr.Code != 0 {
		return exitErr.Code
	}
	return fallback
}

func (a App) hydrateActionsLocally(ctx context.Context, cfg Config, repo Repo, target SSHTarget, leaseID, expectedJob string, fields []string, waitTimeout time.Duration, streamOutput bool, syncBefore bool) (actionsHydrationState, error) {
	if !supportsLocalActionsHydrateTarget(target) {
		return actionsHydrationState{}, exit(2, "local Actions hydration currently supports Linux and Windows WSL2 targets only")
	}
	if err := validateWorkflowInputFields(fields); err != nil {
		return actionsHydrationState{}, err
	}
	workflowPath, err := localActionsWorkflowPath(repo.Root, cfg.Actions.Workflow)
	if err != nil {
		return actionsHydrationState{}, err
	}
	workflow, err := readLocalHydrateWorkflow(workflowPath)
	if err != nil {
		return actionsHydrationState{}, err
	}
	jobName, job, err := selectLocalHydrateJob(workflow, cfg.Actions.Job)
	if err != nil {
		return actionsHydrationState{}, exit(2, "workflow %s %v", cfg.Actions.Workflow, err)
	}
	if inputs, defaults, required, ok, err := parseWorkflowDispatchInputSpecFromFile(workflowPath); err != nil {
		return actionsHydrationState{}, err
	} else if ok {
		fields = applyWorkflowInputDefaults(fields, defaults)
		if missing := missingRequiredWorkflowInputs(fields, required); len(missing) > 0 {
			return actionsHydrationState{}, exit(2, "workflow %s requires hydrate input(s) %s; pass them with -f key=value or define defaults", cfg.Actions.Workflow, strings.Join(missing, ","))
		}
		filtered, dropped := filterWorkflowInputs(fields, inputs)
		for _, field := range dropped {
			fmt.Fprintf(a.Stderr, "warning: workflow %s does not declare input %s; omitting it\n", cfg.Actions.Workflow, fieldName(field))
		}
		fields = filtered
		if !workflowFieldsContain(fields, "crabbox_job") {
			expectedJob = ""
		}
		for _, required := range []string{"crabbox_id", "crabbox_runner_label", "crabbox_keep_alive_minutes"} {
			if !inputs[required] {
				return actionsHydrationState{}, exit(2, "workflow %s does not declare required hydrate input %s", cfg.Actions.Workflow, required)
			}
		}
	}
	workdir := remoteJoin(cfg, leaseID, repo.Name)
	if streamOutput {
		fmt.Fprintf(a.Stdout, "local actions hydrate workflow=%s job=%s workspace=%s\n", cfg.Actions.Workflow, jobName, workdir)
	}
	if err := clearActionsHydrationState(ctx, target, leaseID); err != nil {
		return actionsHydrationState{}, err
	}
	if syncBefore {
		if err := a.syncLocalActionsWorkspace(ctx, cfg, repo, target, workdir); err != nil {
			return actionsHydrationState{}, err
		}
	}
	script, err := localActionsHydrateScript(cfg, repo, workflow, job, jobName, leaseID, fields, workdir)
	if err != nil {
		return actionsHydrationState{}, err
	}
	stdout := io.Discard
	stderr := io.Discard
	if streamOutput {
		stdout = a.Stdout
		stderr = a.Stderr
	}
	if err := runSSHInput(ctx, target, remoteInstallLocalActionsHydrateScript(leaseID), strings.NewReader(script), stdout, stderr); err != nil {
		return actionsHydrationState{}, exit(7, "install local Actions hydration script on %s: %v", target.Host, err)
	}
	if isWindowsWSL2Target(target) {
		if err := runSSHInput(ctx, target, remoteRunLocalActionsHydrateScriptForeground(leaseID, waitTimeout), nil, stdout, stderr); err != nil {
			return actionsHydrationState{}, exit(7, "run local Actions hydration on %s: %v", target.Host, err)
		}
		state, err := readActionsHydrationState(ctx, target, leaseID)
		if err != nil || state.Workspace == "" {
			if err != nil {
				return actionsHydrationState{}, exit(7, "read local Actions hydration marker for %s: %v", leaseID, err)
			}
			return actionsHydrationState{}, exit(7, "local Actions hydration completed without marker for %s", leaseID)
		}
		if expectedJob != "" && state.Job != "" && state.Job != expectedJob {
			return actionsHydrationState{}, exit(5, "local Actions hydration marker for %s came from job %q, expected %q", leaseID, state.Job, expectedJob)
		}
		if err := ensureLocalActionsRunEnv(ctx, target, leaseID, state); err != nil {
			return actionsHydrationState{}, err
		}
		return state, nil
	}
	pid, err := runSSHOutput(ctx, target, remoteStartLocalActionsHydrateScript(leaseID))
	if err != nil {
		return actionsHydrationState{}, exit(7, "start local Actions hydration on %s: %v", target.Host, err)
	}
	return waitForLocalActionsHydration(ctx, target, leaseID, expectedJob, strings.TrimSpace(pid), waitTimeout, stderr)
}

func (a App) syncLocalActionsWorkspace(ctx context.Context, cfg Config, repo Repo, target SSHTarget, workdir string) error {
	if cfg.Sync.BaseRef == "" {
		cfg.Sync.BaseRef = repo.BaseRef
	}
	excludes, err := syncExcludes(repo.Root, cfg)
	if err != nil {
		return err
	}
	manifest, err := syncManifestFiltered(repo.Root, excludes, syncIncludes(cfg))
	if err != nil {
		return exit(6, "build sync file list: %v", err)
	}
	if err := checkSyncPreflight(manifest, cfg, false, a.Stderr); err != nil {
		return err
	}
	if err := runSSHQuiet(ctx, target, remoteMkdir(workdir)); err != nil {
		return exit(7, "create remote workdir: %v", err)
	}
	gitSeed, credentialBlocked := syncGitSeedDecision(cfg, repo)
	if credentialBlocked {
		warnCredentialBearingGitSeed(a.Stderr)
	}
	if gitSeed {
		if err := runSSHQuiet(ctx, target, remoteGitSeed(workdir, repo.RemoteURL, repo.Head)); err != nil {
			fmt.Fprintf(a.Stderr, "warning: remote git seed failed: %v\n", err)
		}
	}
	manifestData := manifest.NUL()
	deletedData := manifest.DeletedNUL()
	manifestInput := syncManifestInputForTarget(target, manifestData, deletedData)
	if err := runSSHInput(ctx, target, remoteWriteSyncManifestsNewForTarget(target, workdir), strings.NewReader(manifestInput), io.Discard, a.Stderr); err != nil {
		return exit(7, "write sync manifests: %v", err)
	}
	if shouldPruneRemoteSync(cfg.Sync.Delete, false) {
		if err := runSSHQuiet(ctx, target, remoteSeedSyncManifestFromGit(workdir)); err != nil {
			return exit(6, "remote sync seed manifest failed: %v", err)
		}
		if err := runSSHQuiet(ctx, target, remotePruneSyncManifestForTarget(target, workdir)); err != nil {
			return exit(6, "remote sync prune failed: %v", err)
		}
	}
	fmt.Fprintf(a.Stderr, "syncing %s -> %s:%s for local actions hydrate\n", repo.Root, target.Host, workdir)
	if err := rsync(ctx, target, repo.Root, workdir, excludes, a.Stdout, a.Stderr, rsyncOptions{Checksum: cfg.Sync.Checksum, UseFilesFrom: true, FilesFrom: manifestData, NoTimes: localContainerDockerSocketConfig(cfg), Timeout: cfg.Sync.Timeout, HeartbeatInterval: 15 * time.Second}); err != nil {
		return exit(6, "rsync failed: %v", err)
	}
	fingerprint := ""
	if cfg.Sync.Fingerprint {
		if value, err := syncFingerprintForManifest(repo, cfg, manifest, excludes); err == nil {
			fingerprint = value
		} else {
			fmt.Fprintf(a.Stderr, "warning: sync fingerprint failed: %v\n", err)
		}
	}
	finalizeCommand := remoteFinalizeSync(workdir, remoteSyncFinalizeOptions{
		AllowMassDeletions: true,
		HydrateGit:         true,
		BaseRef:            cfg.Sync.BaseRef,
		BaseSHA:            gitHydrateBaseSHA(repo, cfg.Sync.BaseRef),
		Fingerprint:        fingerprint,
	})
	if out, err := runSSHCombinedOutput(ctx, target, finalizeCommand); err != nil {
		if out != "" {
			return exit(6, "remote sync finalize failed: %s: %v", out, err)
		}
		return exit(6, "remote sync finalize failed: %v", err)
	}
	return nil
}

func parseWorkflowDispatchInputSpecFromFile(path string) (map[string]bool, map[string]string, map[string]bool, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, nil, false, err
	}
	return parseWorkflowDispatchInputSpec(data)
}

func localActionsWorkflowPath(root, workflow string) (string, error) {
	workflow = strings.TrimSpace(strings.TrimPrefix(workflow, "/"))
	if workflow == "" {
		return "", exit(2, "actions hydrate requires actions.workflow")
	}
	candidates := []string{workflow}
	if !strings.Contains(workflow, "/") {
		candidates = []string{".github/workflows/" + workflow}
		if filepath.Ext(workflow) == "" {
			candidates = append(candidates, ".github/workflows/"+workflow+".yml", ".github/workflows/"+workflow+".yaml")
		}
	}
	workflowRoot := filepath.Join(root, ".github", "workflows")
	var lastErr error
	for _, candidate := range candidates {
		if !strings.HasPrefix(candidate, ".github/workflows/") {
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(candidate))
		rel, err := filepath.Rel(workflowRoot, path)
		if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.IsAbs(rel) {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			lastErr = err
			continue
		}
		if info.IsDir() {
			return "", exit(2, "local Actions hydration workflow %s is a directory", candidate)
		}
		return path, nil
	}
	if lastErr != nil {
		return "", exit(2, "local Actions hydration workflow %s is not readable: %v", workflow, lastErr)
	}
	return "", exit(2, "local Actions hydration requires a repo-local workflow path under .github/workflows")
}

type localHydrateWorkflow struct {
	Name     string            `yaml:"name"`
	Env      map[string]string `yaml:"env"`
	Defaults localHydrateDefaults
	Jobs     map[string]localHydrateJob `yaml:"jobs"`
}

type localHydrateDefaults struct {
	Run localHydrateRunDefaults `yaml:"run"`
}

type localHydrateRunDefaults struct {
	Shell            string `yaml:"shell"`
	WorkingDirectory string `yaml:"working-directory"`
}

type localHydrateJob struct {
	Name      string            `yaml:"name"`
	Env       map[string]string `yaml:"env"`
	Defaults  localHydrateDefaults
	Container yaml.Node            `yaml:"container"`
	Services  map[string]yaml.Node `yaml:"services"`
	Steps     []localHydrateStep   `yaml:"steps"`
}

type localHydrateStep struct {
	ID               string            `yaml:"id"`
	Name             string            `yaml:"name"`
	If               string            `yaml:"if"`
	Uses             string            `yaml:"uses"`
	Run              string            `yaml:"run"`
	Shell            string            `yaml:"shell"`
	WorkingDirectory string            `yaml:"working-directory"`
	Env              map[string]string `yaml:"env"`
	With             map[string]string `yaml:"with"`
}

type localCompositeAction struct {
	Name    string                          `yaml:"name"`
	Inputs  map[string]localCompositeInput  `yaml:"inputs"`
	Outputs map[string]localCompositeOutput `yaml:"outputs"`
	Runs    localCompositeRuns              `yaml:"runs"`
}

type localCompositeInput struct {
	Description string `yaml:"description"`
	Required    bool   `yaml:"required"`
	Default     string `yaml:"default"`
}

type localCompositeOutput struct {
	Description string `yaml:"description"`
	Value       string `yaml:"value"`
}

type localCompositeRuns struct {
	Using string             `yaml:"using"`
	Steps []localHydrateStep `yaml:"steps"`
}

func readLocalHydrateWorkflow(path string) (localHydrateWorkflow, error) {
	var workflow localHydrateWorkflow
	data, err := os.ReadFile(path)
	if err != nil {
		return workflow, err
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		return workflow, err
	}
	if len(workflow.Jobs) == 0 {
		return workflow, exit(2, "workflow %s does not define jobs", path)
	}
	return workflow, nil
}

func selectLocalHydrateJob(workflow localHydrateWorkflow, legacyJob string) (string, localHydrateJob, error) {
	if job, ok := workflow.Jobs["hydrate"]; ok {
		return "hydrate", job, nil
	}
	if legacyJob != "" {
		if job, ok := workflow.Jobs[legacyJob]; ok {
			return legacyJob, job, nil
		}
	}
	if len(workflow.Jobs) == 1 {
		for name, job := range workflow.Jobs {
			return name, job, nil
		}
	}
	return "", localHydrateJob{}, fmt.Errorf("does not define a hydrate job; add a job named %q or use a single-job workflow", "hydrate")
}

func validateLocalHydrateJob(job localHydrateJob) error {
	if job.Container.Kind != 0 {
		return exit(2, "local Actions hydration does not support job containers; rerun with --github-runner when the workflow needs full GitHub Actions semantics")
	}
	if len(job.Services) > 0 {
		return exit(2, "local Actions hydration does not support service containers; rerun with --github-runner when the workflow needs full GitHub Actions semantics")
	}
	return nil
}

func localActionsHydrateScript(cfg Config, repo Repo, workflow localHydrateWorkflow, job localHydrateJob, jobName, leaseID string, fields []string, workdir string) (string, error) {
	if err := validateLocalHydrateJob(job); err != nil {
		return "", err
	}
	inputs, err := fieldsMap(fields)
	if err != nil {
		return "", err
	}
	runnerRoot := path.Join(path.Dir(workdir), ".crabbox-local-actions", localActionsRunnerRootName(leaseID))
	runnerTemp := path.Join(runnerRoot, "tmp")
	runnerToolCache := path.Join(runnerRoot, "tools")
	githubRef := actionsFullRef(cfg, repo)
	env := map[string]string{
		"CI":                    "true",
		"GITHUB_ACTIONS":        "true",
		"GITHUB_WORKSPACE":      workdir,
		"GITHUB_REPOSITORY":     repoSlugForActions(cfg, repo),
		"GITHUB_RUN_ID":         "local-" + leaseID,
		"GITHUB_RUN_NUMBER":     "1",
		"GITHUB_RUN_ATTEMPT":    "1",
		"GITHUB_REF":            githubRef,
		"GITHUB_REF_NAME":       actionsRefName(githubRef),
		"GITHUB_SHA":            repo.Head,
		"GITHUB_EVENT_NAME":     "workflow_dispatch",
		"GITHUB_ACTOR":          "crabbox",
		"GITHUB_JOB":            jobName,
		"RUNNER_OS":             "Linux",
		"RUNNER_TEMP":           runnerTemp,
		"RUNNER_TOOL_CACHE":     runnerToolCache,
		"CRABBOX_LOCAL_ACTIONS": "1",
	}
	if err := mergeInterpolatedEnv(env, workflow.Env, inputs, workdir, repo.Root, nil); err != nil {
		return "", err
	}
	if err := mergeInterpolatedEnv(env, job.Env, inputs, workdir, repo.Root, nil); err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("set -euo pipefail\n")
	b.WriteString("rm -rf " + shellQuote(runnerRoot) + "\n")
	b.WriteString("mkdir -p " + shellQuote(runnerRoot) + " " + shellQuote(workdir) + "\n")
	b.WriteString("chmod 700 " + shellQuote(runnerRoot) + "\n")
	b.WriteString("[ \"$(stat -c %u " + shellQuote(runnerRoot) + ")\" = \"$(id -u)\" ] || { echo 'crabbox local Actions runner root is not owned by the current user' >&2; exit 2; }\n")
	b.WriteString("mkdir -p " + shellQuote(runnerTemp) + " " + shellQuote(runnerToolCache) + "\n")
	b.WriteString("export PATH=" + shellQuote(runnerToolCache+"/node/bin") + ":\"$PATH\"\n")
	b.WriteString("case \"$(uname -m)\" in\n")
	b.WriteString("  x86_64|amd64) export RUNNER_ARCH='X64' ;;\n")
	b.WriteString("  aarch64|arm64) export RUNNER_ARCH='ARM64' ;;\n")
	b.WriteString("  *) export RUNNER_ARCH=\"$(uname -m)\" ;;\n")
	b.WriteString("esac\n")
	for _, key := range sortedKeys(env) {
		value, err := interpolateLocalActionsValue(env[key], inputs, env, workdir, repo.Root, nil)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "export %s=%s\n", key, shellQuote(value))
	}
	b.WriteString("mkdir -p \"$RUNNER_TEMP\" \"$RUNNER_TOOL_CACHE\"\n")
	for key, value := range inputs {
		envName := "INPUT_" + actionInputEnvName(key)
		env[envName] = value
		fmt.Fprintf(&b, "export %s=%s\n", envName, shellQuote(value))
	}
	b.WriteString(localActionsRuntimeShell())
	ctx := localHydrateScriptContext{
		RepoRoot:         repo.Root,
		Workdir:          workdir,
		Inputs:           inputs,
		Env:              env,
		WorkflowDefaults: workflow.Defaults,
		JobDefaults:      job.Defaults,
		StepOutputs:      map[string]map[string]string{},
	}
	if err := appendLocalHydrateSteps(&b, job.Steps, ctx); err != nil {
		return "", err
	}
	return b.String(), nil
}

func localActionsRunnerRootName(leaseID string) string {
	slug := normalizeLeaseSlug(leaseID)
	if slug == "" {
		slug = "lease"
	} else if len(slug) > 32 {
		slug = strings.Trim(slug[:32], "-")
		if slug == "" {
			slug = "lease"
		}
	}
	sum := sha256.Sum256([]byte(leaseID))
	return "crabbox-local-actions-" + slug + "-" + hex.EncodeToString(sum[:8])
}

type localHydrateScriptContext struct {
	RepoRoot         string
	Workdir          string
	Inputs           map[string]string
	Env              map[string]string
	WorkflowDefaults localHydrateDefaults
	JobDefaults      localHydrateDefaults
	StepOutputs      map[string]map[string]string
	Depth            int
}

func appendLocalHydrateSteps(b *strings.Builder, steps []localHydrateStep, ctx localHydrateScriptContext) error {
	if ctx.Depth > 8 {
		return exit(2, "local Actions hydration composite action nesting is too deep")
	}
	for i, step := range steps {
		skip, err := shouldSkipLocalHydrateStep(step.If, ctx.Inputs, ctx.Env, ctx.StepOutputs)
		if err != nil {
			return err
		}
		if skip {
			continue
		}
		label := step.Name
		if label == "" {
			label = firstNonBlank(step.Uses, fmt.Sprintf("step %d", i+1))
		}
		fmt.Fprintf(b, "echo %s\n", shellQuote("local actions: "+label))
		stepEnv := copyStringMap(ctx.Env)
		if err := mergeInterpolatedEnv(stepEnv, step.Env, ctx.Inputs, ctx.Workdir, ctx.RepoRoot, ctx.StepOutputs); err != nil {
			return err
		}
		for _, key := range sortedKeys(step.Env) {
			if !validShellEnvName(key) {
				return exit(2, "local Actions hydration does not support env name %q", key)
			}
			value, err := interpolateLocalActionsValue(stepEnv[key], ctx.Inputs, stepEnv, ctx.Workdir, ctx.RepoRoot, ctx.StepOutputs)
			if err != nil {
				return err
			}
			fmt.Fprintf(b, "__crabbox_save_step_env %s\n", shellQuote(key))
			fmt.Fprintf(b, "export %s=%s\n", key, shellQuote(value))
		}
		b.WriteString("__crabbox_prepare_step_files\n")
		if step.Uses != "" {
			usesScript, outputs, err := localHydrateUsesScript(step, ctx, stepEnv)
			if err != nil {
				return err
			}
			b.WriteString(usesScript)
			if step.ID != "" && len(outputs) > 0 {
				ctx.StepOutputs[step.ID] = outputs
			}
		}
		if step.Run != "" {
			shellName := firstNonBlank(step.Shell, ctx.JobDefaults.Run.Shell, ctx.WorkflowDefaults.Run.Shell, "bash")
			wd := firstNonBlank(step.WorkingDirectory, ctx.JobDefaults.Run.WorkingDirectory, ctx.WorkflowDefaults.Run.WorkingDirectory)
			wd, err = interpolateLocalActionsValue(wd, ctx.Inputs, stepEnv, ctx.Workdir, ctx.RepoRoot, ctx.StepOutputs)
			if err != nil {
				return err
			}
			if wd == "" {
				wd = ctx.Workdir
			} else if !strings.HasPrefix(wd, "/") {
				wd = path.Join(ctx.Workdir, wd)
			}
			run, err := interpolateLocalActionsValue(step.Run, ctx.Inputs, stepEnv, ctx.Workdir, ctx.RepoRoot, ctx.StepOutputs)
			if err != nil {
				return err
			}
			if err := appendLocalHydrateRunStep(b, shellName, wd, run); err != nil {
				return err
			}
			if step.ID != "" {
				if outputs := inferLocalRunStepOutputs(run, stepEnv); len(outputs) > 0 {
					ctx.StepOutputs[step.ID] = outputs
				}
			}
		}
		for _, key := range sortedKeys(step.Env) {
			fmt.Fprintf(b, "__crabbox_restore_step_env %s\n", shellQuote(key))
		}
		b.WriteString("__crabbox_apply_step_files\n")
	}
	return nil
}

func fieldsMap(fields []string) (map[string]string, error) {
	out := map[string]string{}
	for _, field := range fields {
		key, value, ok := strings.Cut(field, "=")
		if !ok || key == "" {
			return nil, exit(2, "workflow input must be key=value: %s", field)
		}
		out[key] = value
	}
	return out, nil
}

func repoSlugForActions(cfg Config, repo Repo) string {
	if ghRepo, err := parseGitHubRepo(cfg.Actions.Repo); err == nil && ghRepo.Slug() != "" {
		return ghRepo.Slug()
	}
	ghRepo, err := resolveGitHubRepo(repo, "")
	if err == nil && ghRepo.Slug() != "" {
		return ghRepo.Slug()
	}
	return repo.Name + "/" + repo.Name
}

func mergeInterpolatedEnv(dst map[string]string, src map[string]string, inputs map[string]string, workdir, repoRoot string, stepOutputs map[string]map[string]string) error {
	for key, value := range src {
		interpolated, err := interpolateLocalActionsValue(value, inputs, dst, workdir, repoRoot, stepOutputs)
		if err != nil {
			return err
		}
		dst[key] = interpolated
	}
	return nil
}

func interpolateLocalActionsValue(value string, inputs, env map[string]string, workdir, repoRoot string, stepOutputs map[string]map[string]string) (string, error) {
	replacements := map[string]string{
		"github.workspace":  workdir,
		"github.ref":        env["GITHUB_REF"],
		"github.ref_name":   env["GITHUB_REF_NAME"],
		"github.sha":        env["GITHUB_SHA"],
		"github.run_id":     env["GITHUB_RUN_ID"],
		"runner.temp":       env["RUNNER_TEMP"],
		"runner.tool_cache": env["RUNNER_TOOL_CACHE"],
	}
	if actionPath := env["GITHUB_ACTION_PATH"]; actionPath != "" {
		replacements["github.action_path"] = actionPath
	}
	var unsupported string
	out := localActionsExpressionPattern.ReplaceAllStringFunc(value, func(match string) string {
		parts := localActionsExpressionPattern.FindStringSubmatch(match)
		if len(parts) != 2 {
			return match
		}
		expr := strings.TrimSpace(parts[1])
		if replacement, ok := replacements[expr]; ok {
			return replacement
		}
		if hash, ok := localActionsHashFiles(expr, repoRoot); ok {
			return hash
		}
		if key, ok := strings.CutPrefix(expr, "inputs."); ok {
			if input, ok := inputs[key]; ok {
				return input
			}
			return ""
		}
		if key, ok := strings.CutPrefix(expr, "env."); ok {
			if envValue, ok := env[key]; ok {
				return envValue
			}
			return ""
		}
		if localActionsDirectSecretPattern.MatchString(expr) {
			return ""
		}
		if output, ok := localActionsStepOutput(expr, stepOutputs); ok {
			return output
		}
		unsupported = expr
		return match
	})
	if unsupported != "" {
		return "", exit(2, "local Actions hydration does not support expression %q; rerun with --github-runner when the workflow needs full GitHub Actions semantics", unsupported)
	}
	if strings.Contains(out, "${{") {
		return "", exit(2, "local Actions hydration does not support complex Actions expressions in %q; rerun with --github-runner when the workflow needs full GitHub Actions semantics", value)
	}
	return out, nil
}

var localActionsExpressionPattern = regexp.MustCompile(`\$\{\{\s*([^}]+?)\s*\}\}`)
var localActionsDirectSecretPattern = regexp.MustCompile(`^secrets\.[A-Za-z_][A-Za-z0-9_]*$`)

func shouldSkipLocalHydrateStep(expr string, inputs, env map[string]string, stepOutputs map[string]map[string]string) (bool, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return false, nil
	}
	expr = strings.TrimPrefix(strings.TrimSuffix(expr, "}}"), "${{")
	expr = strings.TrimSpace(expr)
	switch strings.ToLower(expr) {
	case "", "true", "success()", "always()":
		return false, nil
	case "false", "cancelled()", "failure()":
		return true, nil
	default:
		result, ok := evalLocalActionsIf(expr, inputs, env, stepOutputs)
		if ok {
			return !result, nil
		}
		return false, exit(2, "local Actions hydration does not support if expression %q", expr)
	}
}

func localHydrateUsesScript(step localHydrateStep, ctx localHydrateScriptContext, env map[string]string) (string, map[string]string, error) {
	uses := strings.ToLower(strings.TrimSpace(step.Uses))
	switch {
	case strings.HasPrefix(uses, "actions/checkout@"):
		if err := validateLocalCheckoutStep(step, ctx.Inputs, env, ctx.Workdir, ctx.RepoRoot); err != nil {
			return "", nil, err
		}
		return "# actions/checkout handled by Crabbox sync/git seed\n", nil, nil
	case strings.HasPrefix(uses, "actions/setup-node@"):
		if err := validateLocalActionWithKeys("actions/setup-node", step.With, "node-version", "node-version-file", "check-latest"); err != nil {
			return "", nil, err
		}
		version, err := localHydrateWithInput(step, []string{"node-version", "node-version-file"}, "", ctx.Inputs, env, ctx.Workdir, ctx.RepoRoot, ctx.StepOutputs)
		if err != nil {
			return "", nil, err
		}
		version = normalizeLocalNodeVersionSpec(version)
		if strings.TrimSpace(step.With["node-version"]) != "" && !supportedLocalNodeVersionSpec(version) {
			return "", nil, exit(2, "local Actions hydration does not support actions/setup-node version %q; rerun with --github-runner when the workflow needs full GitHub Actions semantics", version)
		}
		return "__crabbox_setup_node " + shellQuote(version) + "\n", nil, nil
	case strings.HasPrefix(uses, "actions/setup-go@"):
		if err := validateLocalActionWithKeys("actions/setup-go", step.With, "go-version", "go-version-file"); err != nil {
			return "", nil, err
		}
		version, err := localHydrateWithInput(step, []string{"go-version", "go-version-file"}, "", ctx.Inputs, env, ctx.Workdir, ctx.RepoRoot, ctx.StepOutputs)
		if err != nil {
			return "", nil, err
		}
		return "__crabbox_setup_go " + shellQuote(version) + "\n", nil, nil
	case strings.HasPrefix(uses, "actions/setup-python@"):
		if err := validateLocalActionWithKeys("actions/setup-python", step.With, "python-version", "python-version-file"); err != nil {
			return "", nil, err
		}
		version, err := localHydrateWithInput(step, []string{"python-version", "python-version-file"}, "", ctx.Inputs, env, ctx.Workdir, ctx.RepoRoot, ctx.StepOutputs)
		if err != nil {
			return "", nil, err
		}
		return "__crabbox_setup_python " + shellQuote(version) + "\n", nil, nil
	case strings.HasPrefix(uses, "actions/cache/restore@"):
		return "printf 'cache-hit=false\\ncache-matched-key=\\n' >> \"$GITHUB_OUTPUT\"\n", map[string]string{"cache-hit": "false", "cache-matched-key": ""}, nil
	case strings.HasPrefix(uses, "actions/cache/save@"):
		return "# actions/cache/save skipped by local Actions hydration\n", nil, nil
	case isLocalCompositeActionUse(step.Uses):
		return localCompositeActionScript(step, ctx, env)
	default:
		return "", nil, exit(2, "local Actions hydration does not support uses step %q", step.Uses)
	}
}

func validateLocalActionWithKeys(action string, with map[string]string, allowed ...string) error {
	if len(with) == 0 {
		return nil
	}
	allowedSet := map[string]bool{}
	for _, key := range allowed {
		allowedSet[strings.ToLower(strings.TrimSpace(key))] = true
	}
	for key := range with {
		normalized := strings.ToLower(strings.TrimSpace(key))
		if !allowedSet[normalized] {
			return exit(2, "local Actions hydration does not support %s option %q; rerun with --github-runner when the workflow needs full GitHub Actions semantics", action, key)
		}
	}
	return nil
}

func localHydrateWithInput(step localHydrateStep, names []string, fallback string, inputs, env map[string]string, workdir, repoRoot string, stepOutputs map[string]map[string]string) (string, error) {
	value := fallback
	for _, name := range names {
		if raw := strings.TrimSpace(step.With[name]); raw != "" {
			value = raw
			break
		}
	}
	return interpolateLocalActionsValue(value, inputs, env, workdir, repoRoot, stepOutputs)
}

func validateLocalCheckoutStep(step localHydrateStep, inputs, env map[string]string, workdir, repoRoot string) error {
	for key, value := range step.With {
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "ref", "fetch-depth", "persist-credentials", "set-safe-directory":
			continue
		case "path":
			resolved, err := interpolateLocalActionsValue(value, inputs, env, workdir, repoRoot, nil)
			if err != nil {
				return err
			}
			if strings.TrimSpace(resolved) == "" || strings.TrimSpace(resolved) == "." {
				continue
			}
		case "submodules", "lfs":
			resolved, err := interpolateLocalActionsValue(value, inputs, env, workdir, repoRoot, nil)
			if err != nil {
				return err
			}
			if isFalseActionsInput(resolved) {
				continue
			}
		}
		return exit(2, "local Actions hydration does not support actions/checkout option %q; rerun with --github-runner when the workflow needs full GitHub Actions semantics", key)
	}
	return nil
}

func isLocalCompositeActionUse(uses string) bool {
	uses = strings.TrimSpace(uses)
	return strings.HasPrefix(uses, "./") || uses == "." || strings.HasPrefix(uses, "../")
}

func localCompositeActionScript(step localHydrateStep, ctx localHydrateScriptContext, env map[string]string) (string, map[string]string, error) {
	actionPath, err := localCompositeActionPath(ctx.RepoRoot, step.Uses)
	if err != nil {
		return "", nil, err
	}
	action, err := readLocalCompositeAction(actionPath)
	if err != nil {
		return "", nil, err
	}
	if !strings.EqualFold(strings.TrimSpace(action.Runs.Using), "composite") {
		return "", nil, exit(2, "local Actions hydration only supports repo-local composite actions; %s uses %q", step.Uses, action.Runs.Using)
	}
	inputs := map[string]string{}
	for _, name := range sortedCompositeInputKeys(action.Inputs) {
		inputs[name] = action.Inputs[name].Default
	}
	for _, name := range sortedKeys(step.With) {
		value, err := interpolateLocalActionsValue(step.With[name], ctx.Inputs, env, ctx.Workdir, ctx.RepoRoot, ctx.StepOutputs)
		if err != nil {
			return "", nil, err
		}
		inputs[name] = value
	}
	for name, spec := range action.Inputs {
		if spec.Required && strings.TrimSpace(inputs[name]) == "" {
			return "", nil, exit(2, "local composite action %s requires input %s", step.Uses, name)
		}
	}
	actionTargetPath, err := localCompositeActionTargetPath(ctx.Workdir, step.Uses)
	if err != nil {
		return "", nil, err
	}
	next := ctx
	next.Inputs = inputs
	next.Env = copyStringMap(env)
	next.Env["GITHUB_ACTION_PATH"] = actionTargetPath
	next.StepOutputs = map[string]map[string]string{}
	next.Depth++
	var b strings.Builder
	fmt.Fprintf(&b, "__crabbox_save_step_env %s\n", shellQuote("GITHUB_ACTION_PATH"))
	fmt.Fprintf(&b, "export GITHUB_ACTION_PATH=%s\n", shellQuote(actionTargetPath))
	for _, name := range sortedKeys(inputs) {
		envName := "INPUT_" + actionInputEnvName(name)
		next.Env[envName] = inputs[name]
		fmt.Fprintf(&b, "__crabbox_save_step_env %s\n", shellQuote(envName))
		fmt.Fprintf(&b, "export %s=%s\n", envName, shellQuote(inputs[name]))
	}
	if err := appendLocalHydrateSteps(&b, action.Runs.Steps, next); err != nil {
		return "", nil, err
	}
	for _, name := range sortedKeys(inputs) {
		fmt.Fprintf(&b, "__crabbox_restore_step_env %s\n", shellQuote("INPUT_"+actionInputEnvName(name)))
	}
	fmt.Fprintf(&b, "__crabbox_restore_step_env %s\n", shellQuote("GITHUB_ACTION_PATH"))
	outputs, err := localCompositeActionOutputs(action.Outputs, next, actionTargetPath)
	if err != nil {
		return "", nil, err
	}
	return b.String(), outputs, nil
}

func localCompositeActionPath(repoRoot, uses string) (string, error) {
	if repoRoot == "" {
		return "", exit(2, "local Actions hydration cannot resolve repo-local action %q without a repository root", uses)
	}
	uses = strings.TrimSpace(uses)
	if at := strings.IndexByte(uses, '@'); at >= 0 {
		return "", exit(2, "local Actions hydration does not support versioned repo-local action %q", uses)
	}
	clean := filepath.Clean(uses)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", exit(2, "local Actions hydration repo-local action must stay inside the repository: %q", uses)
	}
	return filepath.Join(repoRoot, clean), nil
}

func localCompositeActionTargetPath(workdir, uses string) (string, error) {
	uses = strings.TrimSpace(uses)
	if at := strings.IndexByte(uses, '@'); at >= 0 {
		return "", exit(2, "local Actions hydration does not support versioned repo-local action %q", uses)
	}
	clean := filepath.Clean(uses)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", exit(2, "local Actions hydration repo-local action must stay inside the repository: %q", uses)
	}
	return path.Join(workdir, filepath.ToSlash(clean)), nil
}

func readLocalCompositeAction(dir string) (localCompositeAction, error) {
	var lastErr error
	for _, name := range []string{"action.yml", "action.yaml"} {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			lastErr = err
			continue
		}
		var action localCompositeAction
		if err := yaml.Unmarshal(data, &action); err != nil {
			return action, err
		}
		return action, nil
	}
	return localCompositeAction{}, exit(2, "local composite action %s is not readable: %v", dir, lastErr)
}

func sortedCompositeInputKeys(values map[string]localCompositeInput) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func localCompositeActionOutputs(specs map[string]localCompositeOutput, ctx localHydrateScriptContext, actionTargetPath string) (map[string]string, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	env := copyStringMap(ctx.Env)
	env["GITHUB_ACTION_PATH"] = actionTargetPath
	out := map[string]string{}
	for _, name := range sortedCompositeOutputKeys(specs) {
		value, err := interpolateLocalActionsValue(specs[name].Value, ctx.Inputs, env, ctx.Workdir, ctx.RepoRoot, ctx.StepOutputs)
		if err != nil {
			return nil, err
		}
		out[name] = value
	}
	return out, nil
}

func sortedCompositeOutputKeys(values map[string]localCompositeOutput) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func validateWorkflowInputFields(fields []string) error {
	for _, field := range fields {
		key, _, ok := strings.Cut(field, "=")
		if !ok || key == "" {
			return exit(2, "workflow input must be key=value: %s", field)
		}
	}
	return nil
}

func applyWorkflowInputDefaults(fields []string, defaults map[string]string) []string {
	if len(defaults) == 0 {
		return fields
	}
	out := append([]string{}, fields...)
	seen := map[string]bool{}
	for _, field := range out {
		if name := fieldName(field); name != "" {
			seen[name] = true
		}
	}
	for _, name := range sortedKeys(defaults) {
		if !seen[name] {
			out = append(out, name+"="+defaults[name])
		}
	}
	return out
}

func missingRequiredWorkflowInputs(fields []string, required map[string]bool) []string {
	if len(required) == 0 {
		return nil
	}
	present := map[string]bool{}
	for _, field := range fields {
		if name := fieldName(field); name != "" {
			present[name] = true
		}
	}
	var missing []string
	for name := range required {
		if !present[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

func isFalseActionsInput(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "false", "0", "no":
		return true
	default:
		return false
	}
}

func supportedLocalNodeVersionSpec(value string) bool {
	value = strings.TrimSpace(strings.TrimPrefix(value, "v"))
	if value == "" {
		return true
	}
	for _, part := range strings.Split(value, ".") {
		if part == "" {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

func normalizeLocalNodeVersionSpec(value string) string {
	value = strings.TrimSpace(strings.TrimPrefix(value, "v"))
	value = strings.TrimSuffix(value, ".x")
	value = strings.TrimSuffix(value, ".X")
	return value
}

func localActionsHashFiles(expr, repoRoot string) (string, bool) {
	if !strings.HasPrefix(expr, "hashFiles(") || !strings.HasSuffix(expr, ")") {
		return "", false
	}
	if repoRoot == "" {
		return "", true
	}
	arg := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(expr, "hashFiles("), ")"))
	arg = strings.Trim(arg, `'"`)
	if !validLocalActionsHashPattern(arg) {
		return "", true
	}
	matches, err := localActionsHashMatches(repoRoot, arg)
	if err != nil || len(matches) == 0 {
		return "", true
	}
	sort.Strings(matches)
	h := sha256.New()
	wrote := false
	for _, match := range matches {
		rel, err := filepath.Rel(repoRoot, match)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			continue
		}
		info, err := os.Lstat(match)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(match)
		if err != nil {
			continue
		}
		_, _ = h.Write([]byte(filepath.ToSlash(strings.TrimPrefix(match, repoRoot+string(filepath.Separator)))))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(data)
		_, _ = h.Write([]byte{0})
		wrote = true
	}
	if !wrote {
		return "", true
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

func validLocalActionsHashPattern(pattern string) bool {
	if pattern == "" || filepath.IsAbs(pattern) {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(pattern), "/") {
		if part == ".." {
			return false
		}
	}
	return true
}

func localActionsHashMatches(repoRoot, pattern string) ([]string, error) {
	pattern = filepath.ToSlash(pattern)
	var matches []string
	err := filepath.WalkDir(repoRoot, func(filePath string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		rel, err := filepath.Rel(repoRoot, filePath)
		if err != nil {
			return err
		}
		if localActionsPathMatch(pattern, filepath.ToSlash(rel)) {
			matches = append(matches, filePath)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	return matches, nil
}

func localActionsPathMatch(pattern, name string) bool {
	return localActionsPathMatchParts(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func localActionsPathMatchParts(pattern, name []string) bool {
	if len(pattern) == 0 {
		return len(name) == 0
	}
	if pattern[0] == "**" {
		if localActionsPathMatchParts(pattern[1:], name) {
			return true
		}
		return len(name) > 0 && localActionsPathMatchParts(pattern, name[1:])
	}
	if len(name) == 0 {
		return false
	}
	matched, err := path.Match(pattern[0], name[0])
	if err != nil || !matched {
		return false
	}
	return localActionsPathMatchParts(pattern[1:], name[1:])
}

func localActionsStepOutput(expr string, stepOutputs map[string]map[string]string) (string, bool) {
	if !strings.HasPrefix(expr, "steps.") {
		return "", false
	}
	parts := strings.Split(expr, ".")
	if len(parts) == 4 && parts[2] == "outputs" && parts[1] != "" && parts[3] != "" {
		if outputs := stepOutputs[parts[1]]; outputs != nil {
			value, ok := outputs[parts[3]]
			return value, ok
		}
	}
	return "", false
}

var localRunStepOutputEchoPattern = regexp.MustCompile(`^echo\s+(?:"([^"]*)"|'([^']*)')\s*>>\s*"?\$GITHUB_OUTPUT"?\s*$`)
var localShellVariablePattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

func inferLocalRunStepOutputs(script string, env map[string]string) map[string]string {
	outputs := map[string]string{}
	for _, line := range strings.Split(script, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		match := localRunStepOutputEchoPattern.FindStringSubmatch(line)
		if len(match) != 3 {
			return nil
		}
		content := firstNonBlank(match[1], match[2])
		singleQuoted := match[2] != ""
		key, value, ok := strings.Cut(content, "=")
		if !ok || !validLocalActionsOutputName(key) {
			continue
		}
		if singleQuoted {
			outputs[key] = value
			continue
		}
		expanded, ok := expandLocalShellVariables(value, env)
		if !ok {
			return nil
		}
		outputs[key] = expanded
	}
	if len(outputs) == 0 {
		return nil
	}
	return outputs
}

func validLocalActionsOutputName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r == '_', r == '-':
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

func expandLocalShellVariables(value string, env map[string]string) (string, bool) {
	if strings.Contains(value, `\$`) {
		return "", false
	}
	ok := true
	out := localShellVariablePattern.ReplaceAllStringFunc(value, func(match string) string {
		parts := localShellVariablePattern.FindStringSubmatch(match)
		for _, key := range parts[1:] {
			if key != "" {
				value, exists := env[key]
				if !exists {
					ok = false
					return match
				}
				return value
			}
		}
		return ""
	})
	if !ok || strings.Contains(out, "$") || strings.Contains(out, "`") {
		return "", false
	}
	return out, true
}

func evalLocalActionsIf(expr string, inputs, env map[string]string, stepOutputs map[string]map[string]string) (bool, bool) {
	parts := strings.Split(expr, "&&")
	if len(parts) == 0 {
		return false, false
	}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		var op string
		if strings.Contains(part, "==") {
			op = "=="
		} else if strings.Contains(part, "!=") {
			op = "!="
		} else {
			return false, false
		}
		left, right, ok := strings.Cut(part, op)
		if !ok {
			return false, false
		}
		leftValue, ok := localActionsIfValue(strings.TrimSpace(left), inputs, env, stepOutputs)
		if !ok {
			return false, false
		}
		rightValue, ok := localActionsIfValue(strings.TrimSpace(right), inputs, env, stepOutputs)
		if !ok {
			return false, false
		}
		matched := leftValue == rightValue
		if op == "!=" {
			matched = !matched
		}
		if !matched {
			return false, true
		}
	}
	return true, true
}

func localActionsIfValue(value string, inputs, env map[string]string, stepOutputs map[string]map[string]string) (string, bool) {
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		if value[0] == '\'' && value[len(value)-1] == '\'' || value[0] == '"' && value[len(value)-1] == '"' {
			return value[1 : len(value)-1], true
		}
	}
	if key, ok := strings.CutPrefix(value, "inputs."); ok {
		return inputs[key], true
	}
	if _, ok := strings.CutPrefix(value, "env."); ok {
		return "", false
	}
	if output, ok := localActionsStepOutput(value, stepOutputs); ok {
		return output, true
	}
	return "", false
}

func appendLocalHydrateRunStep(b *strings.Builder, shellName, workdir, script string) error {
	shellName = strings.TrimSpace(shellName)
	delimiter := localHydrateHeredocDelimiter(script)
	switch {
	case shellName == "", shellName == "bash":
		fmt.Fprintf(b, "__crabbox_run_bash %s <<'%s'\n%s\n%s\n", shellQuote(workdir), delimiter, script, delimiter)
	case shellName == "sh":
		fmt.Fprintf(b, "__crabbox_run_sh %s <<'%s'\n%s\n%s\n", shellQuote(workdir), delimiter, script, delimiter)
	default:
		return exit(2, "local Actions hydration does not support shell %q", shellName)
	}
	return nil
}

func localHydrateHeredocDelimiter(script string) string {
	const base = "CRABBOX_STEP"
	delimiter := base
	for i := 2; strings.Contains("\n"+script+"\n", "\n"+delimiter+"\n"); i++ {
		delimiter = fmt.Sprintf("%s_%d", base, i)
	}
	return delimiter
}

func actionInputEnvName(name string) string {
	name = strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
	var b strings.Builder
	for _, r := range name {
		if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func copyStringMap(values map[string]string) map[string]string {
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func validShellEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r == '_':
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

func localActionsRuntimeShell() string {
	return `__crabbox_prepare_step_files() {
  export GITHUB_ENV="$RUNNER_TEMP/step-env"
  export GITHUB_PATH="$RUNNER_TEMP/step-path"
  export GITHUB_OUTPUT="$RUNNER_TEMP/step-output"
  export GITHUB_STATE="$RUNNER_TEMP/step-state"
  export GITHUB_STEP_SUMMARY="$RUNNER_TEMP/step-summary"
  : >"$GITHUB_ENV"
  : >"$GITHUB_PATH"
  : >"$GITHUB_OUTPUT"
  : >"$GITHUB_STATE"
  : >"$GITHUB_STEP_SUMMARY"
}
__crabbox_apply_env_file() {
  [ -s "$GITHUB_ENV" ] || return 0
  local line key value
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in
      *=*)
        key="${line%%=*}"
        value="${line#*=}"
        case "$key" in
          ''|*[!A-Za-z0-9_]*|[0-9]*) continue ;;
        esac
        export "$key=$value"
        ;;
    esac
  done <"$GITHUB_ENV"
}
__crabbox_apply_path_file() {
  [ -s "$GITHUB_PATH" ] || return 0
  local line
  while IFS= read -r line || [ -n "$line" ]; do
    [ -n "$line" ] && export PATH="$line:$PATH"
  done <"$GITHUB_PATH"
}
__crabbox_apply_step_files() {
  __crabbox_apply_env_file
  __crabbox_apply_path_file
}
__crabbox_save_step_env() {
  local key="$1"
  eval "case \"\${${key}+set}\" in set) export __CRABBOX_SAVED_${key}=\"\${${key}}\"; export __CRABBOX_SAVED_${key}_SET=1 ;; *) unset __CRABBOX_SAVED_${key}; export __CRABBOX_SAVED_${key}_SET=0 ;; esac"
}
__crabbox_restore_step_env() {
  local key="$1"
  eval "case \"\${__CRABBOX_SAVED_${key}_SET:-0}\" in 1) export ${key}=\"\${__CRABBOX_SAVED_${key}}\" ;; *) unset ${key} ;; esac"
  unset "__CRABBOX_SAVED_${key}" "__CRABBOX_SAVED_${key}_SET"
}
__crabbox_run_bash() {
	  local wd="$1"
	  shift
	  mkdir -p "$wd"
	  local script="$RUNNER_TEMP/step.sh"
	  cat >"$script"
	  (cd "$wd" && bash --noprofile --norc -e -o pipefail "$script")
	}
__crabbox_run_sh() {
	  local wd="$1"
	  shift
	  mkdir -p "$wd"
	  local script="$RUNNER_TEMP/step.sh"
	  cat >"$script"
	  (cd "$wd" && sh -e "$script")
	}
__crabbox_arch() {
  case "$(uname -m)" in
    x86_64|amd64) printf x64 ;;
    aarch64|arm64) printf arm64 ;;
    *) uname -m ;;
  esac
}
__crabbox_sudo() {
  if [ "$(id -u)" = "0" ]; then
    "$@"
  elif command -v sudo >/dev/null 2>&1; then
    sudo "$@"
  else
    "$@"
  fi
}
__crabbox_ensure_xz() {
  if command -v xz >/dev/null 2>&1; then
    return 0
  fi
  if command -v apt-get >/dev/null 2>&1; then
    __crabbox_sudo apt-get update >/tmp/crabbox-actions-apt-update.log 2>&1 || true
    __crabbox_sudo apt-get install -y --no-install-recommends xz-utils
  fi
  command -v xz >/dev/null 2>&1
}
__crabbox_setup_node() {
  local requested="${1:-}"
  if [ -n "$requested" ] && [ -f "$GITHUB_WORKSPACE/$requested" ]; then
    if [ "$(basename "$requested")" = "package.json" ]; then
      requested="$(sed -nE 's/.*"node"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/p' "$GITHUB_WORKSPACE/$requested" | head -n 1 | tr -d '[:space:]')"
    else
      requested="$(head -n 1 "$GITHUB_WORKSPACE/$requested" | tr -d '[:space:]')"
    fi
  fi
  if [ -z "$requested" ]; then
    if command -v node >/dev/null 2>&1; then
      corepack enable >/dev/null 2>&1 || true
      return 0
    fi
    echo "actions/setup-node did not request a Node version, and Node is not installed on this image; add node-version or rerun with --github-runner" >&2
    return 2
  fi
  local major="${requested%%.*}"
  major="${major#v}"
  case "$major" in
    ''|*[!0-9]*)
      echo "local Actions hydration does not support actions/setup-node version ${requested}; use a numeric major version or rerun with --github-runner" >&2
      return 2
      ;;
  esac
  local want dots
  want="${requested#v}"
  dots="$(printf '%s' "$want" | tr -cd '.' | wc -c | tr -d ' ')"
  if command -v node >/dev/null 2>&1; then
    local actual
    actual="$(node -p 'process.versions.node' 2>/dev/null || true)"
    case "$dots" in
      0) case "${actual%%.*}" in "$major") corepack enable >/dev/null 2>&1 || true; return 0 ;; esac ;;
      1) case "$actual" in "$want"|"$want".*) corepack enable >/dev/null 2>&1 || true; return 0 ;; esac ;;
      *) case "$actual" in "$want") corepack enable >/dev/null 2>&1 || true; return 0 ;; esac ;;
    esac
  fi
  local arch version archive checksums expected actual dir marker tmp extract selector
  arch="$(__crabbox_arch)"
  case "$dots" in
    0) selector="v${major}." ;;
    1) selector="v${want}." ;;
    *) selector="v${want}" ;;
  esac
  version="$(curl -fsSL https://nodejs.org/dist/index.tab | awk -v selector="$selector" 'NR>1 && found == "" && ($1 == selector || index($1, selector)==1) { found=$1 } END { if (found != "") print found }')"
  if [ -z "$version" ]; then
    echo "unable to resolve Node $major" >&2
    return 2
  fi
  archive="node-${version}-linux-${arch}.tar.xz"
  dir="$RUNNER_TOOL_CACHE/node-${version}-linux-${arch}"
  marker="$dir/.crabbox-node-sha256"
  tmp="$RUNNER_TEMP/$archive"
  checksums="$RUNNER_TEMP/node-${version}-SHASUMS256.txt"
  mkdir -p "$RUNNER_TOOL_CACHE" "$RUNNER_TEMP"
  curl -fsSL -o "$checksums" "https://nodejs.org/dist/${version}/SHASUMS256.txt"
  expected="$(awk -v archive="$archive" '$2 == archive || $2 == "*" archive { print $1; exit }' "$checksums")"
  case "$expected" in ''|*[!0-9a-fA-F]*) expected= ;; esac
  if [ "${#expected}" -ne 64 ]; then
    echo "Node release checksums did not contain a valid digest for $archive" >&2
    return 2
  fi
  if [ ! -x "$dir/bin/node" ] || [ ! -f "$marker" ] || [ "$(cat "$marker" 2>/dev/null || true)" != "$expected" ]; then
    curl -fsSL -o "$tmp" "https://nodejs.org/dist/${version}/${archive}"
    if command -v sha256sum >/dev/null 2>&1; then
      actual="$(sha256sum "$tmp" | awk '{ print $1 }')"
    elif command -v shasum >/dev/null 2>&1; then
      actual="$(shasum -a 256 "$tmp" | awk '{ print $1 }')"
    else
      echo "sha256sum or shasum is required to verify Node archives" >&2
      return 2
    fi
    actual="$(printf '%s' "$actual" | tr 'A-F' 'a-f')"
    if [ "$actual" != "$expected" ]; then
      echo "Node archive checksum mismatch for $archive" >&2
      return 2
    fi
    __crabbox_ensure_xz || { echo "xz is required to extract Node archives; install xz-utils or use a fuller local-container image" >&2; return 2; }
    extract="$RUNNER_TEMP/node-${version}-extract"
    rm -rf "$extract"
    mkdir -p "$extract"
    tar -xJf "$tmp" -C "$extract"
    [ -x "$extract/node-${version}-linux-${arch}/bin/node" ] || { echo "Node archive has an unexpected layout" >&2; return 2; }
    rm -rf "$dir"
    mv "$extract/node-${version}-linux-${arch}" "$dir"
    printf '%s\n' "$expected" >"$marker"
    rm -rf "$extract"
  fi
  rm -f "$RUNNER_TOOL_CACHE/node"
  ln -s "$dir" "$RUNNER_TOOL_CACHE/node"
  export PATH="$RUNNER_TOOL_CACHE/node/bin:$PATH"
  corepack enable >/dev/null 2>&1 || true
}
__crabbox_setup_go() {
  local requested="${1:-}"
  if [ -n "$requested" ] && [ -f "$GITHUB_WORKSPACE/$requested" ]; then
    if [ "$(basename "$requested")" = "go.mod" ]; then
      requested="$(awk '$1 == "go" { print $2; exit }' "$GITHUB_WORKSPACE/$requested" | tr -d '[:space:]')"
    else
      requested="$(head -n 1 "$GITHUB_WORKSPACE/$requested" | tr -d '[:space:]')"
    fi
  fi
  if command -v go >/dev/null 2>&1; then
    if [ -z "$requested" ]; then
      return 0
    fi
    local want major rest minor patch prefix actual
    want="${requested#go}"
    want="${want#v}"
    major="${want%%.*}"
    actual="$(go version | awk '{ print $3 }')"
    case "$major" in
      ''|*[!0-9]*) ;;
      *)
        rest="${want#*.}"
        if [ "$rest" != "$want" ]; then
          minor="${rest%%.*}"
          patch="${rest#*.}"
          if [ "$patch" != "$rest" ]; then
            case "$actual" in "go${major}.${minor}.${patch}") return 0 ;; esac
          else
            prefix="go${major}.${minor}"
            case "$actual" in "$prefix"|"$prefix".*) return 0 ;; esac
          fi
        else
          case "$actual" in go"$major"|go"$major".*) return 0 ;; esac
        fi
        ;;
    esac
    echo "actions/setup-go requested ${requested}, but installed ${actual:-unknown}; install Go in a run step or rerun with --github-runner" >&2
    return 2
  fi
  echo "actions/setup-go requested ${requested:-default}, but Go is not installed on this image; install Go in a run step or prebuild the Crabbox image" >&2
  return 2
}
__crabbox_setup_python() {
  local requested="${1:-}"
  if [ -n "$requested" ] && [ -f "$GITHUB_WORKSPACE/$requested" ]; then
    requested="$(head -n 1 "$GITHUB_WORKSPACE/$requested" | tr -d '[:space:]')"
  fi
  local py
  py="$(command -v python3 || command -v python || true)"
  if [ -n "$py" ]; then
    if [ -z "$requested" ]; then
      return 0
    fi
    local want actual
    want="${requested#v}"
    want="${want%%-*}"
    actual="$("$py" -c 'import sys; print(".".join(map(str, sys.version_info[:3])))' 2>/dev/null || true)"
    case "$want" in
      ''|*[!0-9.]*) ;;
      *.*) case "$actual" in "$want"|"$want".*) return 0 ;; esac ;;
      *) case "$actual" in "$want"|"$want".*) return 0 ;; esac ;;
    esac
    echo "actions/setup-python requested ${requested}, but installed ${actual:-unknown}; install Python in a run step or rerun with --github-runner" >&2
    return 2
  fi
  echo "actions/setup-python requested ${requested:-default}, but Python is not installed on this image; install Python in a run step or prebuild the Crabbox image" >&2
  return 2
}
`
}

func actionsHydrateFields(leaseID, label, job string, keepAliveMinutes int, extra []string) []string {
	fields := []string{
		"crabbox_id=" + leaseID,
		"crabbox_runner_label=" + label,
		fmt.Sprintf("crabbox_keep_alive_minutes=%d", keepAliveMinutes),
	}
	if job != "" {
		fields = append(fields, "crabbox_job="+job)
	}
	fields = append(fields, extra...)
	return fields
}

func mergeWorkflowInputFields(base, override []string) []string {
	fields := append([]string{}, base...)
	index := map[string]int{}
	for i, field := range fields {
		if name := fieldName(field); name != "" {
			index[name] = i
		}
	}
	for _, field := range override {
		name := fieldName(field)
		if name != "" {
			if existing, ok := index[name]; ok {
				fields[existing] = field
				continue
			}
			index[name] = len(fields)
		}
		fields = append(fields, field)
	}
	return fields
}

func githubWorkflowDispatchInputs(ctx context.Context, dir string, repo GitHubRepo, workflow, ref string) (map[string]bool, bool, error) {
	workflow = strings.TrimPrefix(workflow, "/")
	if !strings.HasPrefix(workflow, ".github/workflows/") {
		return nil, false, nil
	}
	out, err := ghOutput(ctx, dir, "api", "repos/"+repo.Slug()+"/contents/"+workflow+"?ref="+url.QueryEscape(ref), "--jq", ".content")
	if err != nil {
		return nil, false, err
	}
	encoded := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, out)
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, false, err
	}
	inputs, ok, err := parseWorkflowDispatchInputs(data)
	return inputs, ok, err
}

func parseWorkflowDispatchInputs(data []byte) (map[string]bool, bool, error) {
	inputs, _, _, ok, err := parseWorkflowDispatchInputSpec(data)
	return inputs, ok, err
}

func parseWorkflowDispatchInputSpec(data []byte) (map[string]bool, map[string]string, map[string]bool, bool, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, nil, nil, false, err
	}
	root := mappingValue(&doc, "")
	if root == nil {
		return nil, nil, nil, false, nil
	}
	on := mappingValue(root, "on")
	if on == nil {
		return nil, nil, nil, false, nil
	}
	dispatch := mappingValue(on, "workflow_dispatch")
	if dispatch == nil {
		return nil, nil, nil, false, nil
	}
	inputsNode := mappingValue(dispatch, "inputs")
	if inputsNode == nil || inputsNode.Kind != yaml.MappingNode {
		return map[string]bool{}, map[string]string{}, map[string]bool{}, true, nil
	}
	inputs := map[string]bool{}
	defaults := map[string]string{}
	required := map[string]bool{}
	for i := 0; i+1 < len(inputsNode.Content); i += 2 {
		name := inputsNode.Content[i].Value
		inputs[name] = true
		valueNode := inputsNode.Content[i+1]
		if defaultNode := mappingValue(valueNode, "default"); defaultNode != nil && defaultNode.Kind == yaml.ScalarNode {
			defaults[name] = defaultNode.Value
		}
		if requiredNode := mappingValue(valueNode, "required"); requiredNode != nil && requiredNode.Kind == yaml.ScalarNode && strings.EqualFold(requiredNode.Value, "true") {
			if _, hasDefault := defaults[name]; !hasDefault {
				required[name] = true
			}
		}
	}
	return inputs, defaults, required, true, nil
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		if key == "" {
			return node.Content[0]
		}
		return mappingValue(node.Content[0], key)
	}
	if key == "" {
		if node.Kind == yaml.MappingNode {
			return node
		}
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func filterWorkflowInputs(fields []string, inputs map[string]bool) ([]string, []string) {
	filtered := make([]string, 0, len(fields))
	dropped := []string{}
	for _, field := range fields {
		name := fieldName(field)
		if inputs[name] {
			filtered = append(filtered, field)
		} else {
			dropped = append(dropped, field)
		}
	}
	return filtered, dropped
}

func workflowFieldsContain(fields []string, name string) bool {
	for _, field := range fields {
		if fieldName(field) == name {
			return true
		}
	}
	return false
}

func dropWorkflowField(fields []string, name string) []string {
	out := fields[:0]
	for _, field := range fields {
		if fieldName(field) != name {
			out = append(out, field)
		}
	}
	return out
}

func fieldName(field string) string {
	name, _, _ := strings.Cut(field, "=")
	return name
}

func actionsRef(cfg Config, repo Repo) string {
	if cfg.Actions.Ref != "" {
		return cfg.Actions.Ref
	}
	if repo.BaseRef != "" {
		return repo.BaseRef
	}
	return "main"
}

func actionsFullRef(cfg Config, repo Repo) string {
	ref := actionsRef(cfg, repo)
	if strings.HasPrefix(ref, "refs/") {
		return ref
	}
	return "refs/heads/" + ref
}

func actionsRefName(ref string) string {
	if strings.HasPrefix(ref, "refs/heads/") {
		return strings.TrimPrefix(ref, "refs/heads/")
	}
	if strings.HasPrefix(ref, "refs/tags/") {
		return strings.TrimPrefix(ref, "refs/tags/")
	}
	return path.Base(ref)
}

func githubActionsRunnerLabels(cfg Config, leaseID, slug string, extra []string) []string {
	labels := []string{
		"crabbox",
		githubActionsLeaseLabel(leaseID),
		"crabbox-profile-" + sanitizeGitHubRunnerLabel(cfg.Profile),
		"crabbox-class-" + sanitizeGitHubRunnerLabel(cfg.Class),
	}
	if slug = normalizeLeaseSlug(slug); slug != "" {
		labels = append(labels, "crabbox-"+sanitizeGitHubRunnerLabel(slug))
	}
	labels = append(labels, cfg.Actions.RunnerLabels...)
	labels = append(labels, extra...)
	return appendUniqueStrings(nil, labels...)
}

func githubActionsLeaseLabel(leaseID string) string {
	return "crabbox-" + sanitizeGitHubRunnerLabel(leaseID)
}

type actionsHydrationState struct {
	Workspace    string
	RunID        string
	ReadyAt      string
	Job          string
	EnvFile      string
	ServicesFile string
}

func waitForActionsHydration(ctx context.Context, target SSHTarget, leaseID, expectedJob string, timeout time.Duration, stderr io.Writer) (actionsHydrationState, error) {
	deadline := time.Now().Add(timeout)
	for {
		state, err := readActionsHydrationState(ctx, target, leaseID)
		if err == nil && state.Workspace != "" {
			if expectedJob != "" && state.Job != "" && state.Job != expectedJob {
				return actionsHydrationState{}, exit(5, "GitHub Actions hydration marker for %s came from job %q, expected %q", leaseID, state.Job, expectedJob)
			}
			return state, nil
		}
		if ctx.Err() != nil {
			return actionsHydrationState{}, ctx.Err()
		}
		if time.Now().After(deadline) {
			return actionsHydrationState{}, exit(5, "timed out waiting for GitHub Actions hydration marker for %s", leaseID)
		}
		fmt.Fprintf(stderr, "waiting for GitHub Actions hydration marker id=%s...\n", leaseID)
		time.Sleep(10 * time.Second)
	}
}

func readActionsHydrationState(ctx context.Context, target SSHTarget, leaseID string) (actionsHydrationState, error) {
	out, err := runActionsHydrationOutput(ctx, target, remoteReadActionsHydrationStateForTarget(target, leaseID))
	if err != nil {
		return actionsHydrationState{}, err
	}
	return normalizeActionsHydrationStateForTarget(target, parseActionsHydrationState(out)), nil
}

func waitForLocalActionsHydration(ctx context.Context, target SSHTarget, leaseID, expectedJob, pid string, timeout time.Duration, stderr io.Writer) (actionsHydrationState, error) {
	if timeout <= 0 {
		timeout = 20 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	for {
		state, err := readActionsHydrationState(ctx, target, leaseID)
		if err == nil && state.Workspace != "" {
			if expectedJob != "" && state.Job != "" && state.Job != expectedJob {
				return actionsHydrationState{}, exit(5, "local Actions hydration marker for %s came from job %q, expected %q", leaseID, state.Job, expectedJob)
			}
			if err := ensureLocalActionsRunEnv(ctx, target, leaseID, state); err != nil {
				return actionsHydrationState{}, err
			}
			return state, nil
		}
		status, statusErr := runActionsHydrationOutput(ctx, target, remoteLocalActionsHydrateStatus(leaseID, pid))
		if statusErr == nil && strings.HasPrefix(status, "exit=") {
			if state, err := readActionsHydrationState(ctx, target, leaseID); err == nil && state.Workspace != "" {
				if expectedJob != "" && state.Job != "" && state.Job != expectedJob {
					return actionsHydrationState{}, exit(5, "local Actions hydration marker for %s came from job %q, expected %q", leaseID, state.Job, expectedJob)
				}
				if err := ensureLocalActionsRunEnv(ctx, target, leaseID, state); err != nil {
					return actionsHydrationState{}, err
				}
				return state, nil
			}
			return actionsHydrationState{}, exit(7, "local Actions hydration exited before writing marker for %s: %s", leaseID, strings.TrimSpace(status))
		}
		if ctx.Err() != nil {
			return actionsHydrationState{}, ctx.Err()
		}
		if time.Now().After(deadline) {
			return actionsHydrationState{}, exit(5, "timed out waiting for local Actions hydration marker for %s", leaseID)
		}
		fmt.Fprintf(stderr, "waiting for local Actions hydration marker id=%s...\n", leaseID)
		time.Sleep(5 * time.Second)
	}
}

func ensureLocalActionsRunEnv(ctx context.Context, target SSHTarget, leaseID string, state actionsHydrationState) error {
	if err := runActionsHydrationQuiet(ctx, target, remoteEnsureLocalActionsRunEnv(leaseID, state.EnvFile)); err != nil {
		return exit(7, "update local Actions env handoff on %s: %v", target.Host, err)
	}
	return nil
}

func runActionsHydrationOutput(ctx context.Context, target SSHTarget, remote string) (string, error) {
	commandCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := runSSHOutputWithRemoteWaitTimeout(commandCtx, target, remote, 15*time.Second, "2", "1")
	if commandCtx.Err() == context.DeadlineExceeded {
		return "", exit(7, "Actions hydration SSH probe timed out after 30s")
	}
	return out, err
}

func runActionsHydrationQuiet(ctx context.Context, target SSHTarget, remote string) error {
	commandCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := runSSHQuietWithRemoteWaitTimeout(commandCtx, target, remote, 15*time.Second, "2", "1")
	if commandCtx.Err() == context.DeadlineExceeded {
		return exit(7, "Actions hydration SSH probe timed out after 30s")
	}
	return err
}

func clearActionsHydrationState(ctx context.Context, target SSHTarget, leaseID string) error {
	if err := runSSHQuiet(ctx, target, remoteClearActionsHydrationStateForTarget(target, leaseID)); err != nil {
		return exit(7, "clear GitHub Actions hydration marker on %s: %v", target.Host, err)
	}
	return nil
}

func writeActionsHydrationStop(ctx context.Context, target SSHTarget, leaseID string) error {
	if err := runSSHQuiet(ctx, target, remoteWriteActionsHydrationStopForTarget(target, leaseID)); err != nil {
		return exit(7, "write GitHub Actions hydration stop marker on %s: %v", target.Host, err)
	}
	return nil
}

func parseActionsHydrationState(value string) actionsHydrationState {
	state := actionsHydrationState{}
	for _, line := range strings.Split(value, "\n") {
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "WORKSPACE":
			state.Workspace = strings.TrimSpace(val)
		case "RUN_ID":
			state.RunID = strings.TrimSpace(val)
		case "READY_AT":
			state.ReadyAt = strings.TrimSpace(val)
		case "JOB":
			state.Job = strings.TrimSpace(val)
		case "ENV_FILE":
			state.EnvFile = strings.TrimSpace(val)
		case "SERVICES_FILE":
			state.ServicesFile = strings.TrimSpace(val)
		}
	}
	return state
}

func normalizeActionsHydrationStateForTarget(target SSHTarget, state actionsHydrationState) actionsHydrationState {
	if !isWindowsNativeTarget(target) {
		return state
	}
	state.Workspace = windowsNativePathFromMSYSPath(state.Workspace)
	state.EnvFile = windowsNativePathFromMSYSPath(state.EnvFile)
	state.ServicesFile = windowsNativePathFromMSYSPath(state.ServicesFile)
	return state
}

func windowsNativePathFromMSYSPath(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 3 && value[0] == '/' && value[2] == '/' {
		drive := value[1]
		if (drive >= 'a' && drive <= 'z') || (drive >= 'A' && drive <= 'Z') {
			return strings.ToUpper(string(drive)) + `:\` + strings.ReplaceAll(value[3:], "/", `\`)
		}
	}
	return value
}

func remoteReadActionsHydrationState(leaseID string) string {
	return "cat \"$HOME\"/" + shellQuote(actionsHydrationStatePath(leaseID)) + " 2>/dev/null || true"
}

func remoteReadActionsHydrationStateForTarget(target SSHTarget, leaseID string) string {
	if isWindowsNativeTarget(target) {
		return powershellCommand(`$path = ` + psQuote(windowsActionsHydrationPath(actionsHydrationStatePath(leaseID))) + `
if (Test-Path -LiteralPath $path) { Get-Content -Raw -LiteralPath $path }
exit 0
`)
	}
	return remoteReadActionsHydrationState(leaseID)
}

func remoteInstallLocalActionsHydrateScript(leaseID string) string {
	path := "\"$HOME\"/" + shellQuote(actionsHydrationLocalScriptPath(leaseID))
	return "mkdir -p \"$HOME\"/" + shellQuote(actionsHydrationDir()) + " && cat > " + path + " && chmod 700 " + path
}

func remoteStartLocalActionsHydrateScript(leaseID string) string {
	script := "\"$HOME\"/" + shellQuote(actionsHydrationLocalScriptPath(leaseID))
	logPath := "\"$HOME\"/" + shellQuote(actionsHydrationLocalLogPath(leaseID))
	exitPath := "\"$HOME\"/" + shellQuote(actionsHydrationLocalExitPath(leaseID))
	wrapper := `bash "$1" >"$2" 2>&1; printf '%s\n' "$?" >"$3"`
	return "rm -f " + exitPath + " " + logPath + " && nohup bash -c " + shellQuote(wrapper) + " sh " + script + " " + logPath + " " + exitPath + " >/dev/null 2>&1 < /dev/null & printf '%s\\n' \"$!\""
}

func remoteRunLocalActionsHydrateScriptForeground(leaseID string, timeout time.Duration) string {
	if timeout <= 0 {
		timeout = 20 * time.Minute
	}
	seconds := int((timeout + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	script := "\"$HOME\"/" + shellQuote(actionsHydrationLocalScriptPath(leaseID))
	logPath := "\"$HOME\"/" + shellQuote(actionsHydrationLocalLogPath(leaseID))
	exitPath := "\"$HOME\"/" + shellQuote(actionsHydrationLocalExitPath(leaseID))
	wrapper := `set -o pipefail
rm -f "$3" "$4"
timeout --signal=TERM --kill-after=10s "$1" bash "$2" 2>&1 | tee "$3"
code=${PIPESTATUS[0]}
printf '%s\n' "$code" >"$4"
exit "$code"`
	return "bash -c " + shellQuote(wrapper) + " sh " + shellQuote(fmt.Sprintf("%ds", seconds)) + " " + script + " " + logPath + " " + exitPath
}

func remoteLocalActionsHydrateStatus(leaseID, pid string) string {
	exitPath := "\"$HOME\"/" + shellQuote(actionsHydrationLocalExitPath(leaseID))
	logPath := "\"$HOME\"/" + shellQuote(actionsHydrationLocalLogPath(leaseID))
	pid = strings.TrimSpace(pid)
	if pid == "" {
		pid = "-"
	}
	return "if [ -f " + exitPath + " ]; then printf 'exit=%s\\n' \"$(cat " + exitPath + " 2>/dev/null)\"; tail -80 " + logPath + " 2>/dev/null || true; elif kill -0 " + shellQuote(pid) + " 2>/dev/null; then printf running; else printf 'exit=unknown\\n'; tail -80 " + logPath + " 2>/dev/null || true; fi"
}

func remoteEnsureLocalActionsRunEnv(leaseID, envFile string) string {
	envPath := "\"$HOME\"/" + shellQuote(actionsHydrationEnvPath(leaseID))
	if strings.TrimSpace(envFile) != "" {
		envPath = shellQuote(envFile)
	}
	script := `set -e
env_file=` + envPath + `
if [ -f "$env_file" ]; then
  set +u
  . "$env_file" >/dev/null 2>&1 || true
  set -u
  if [ -n "${RUNNER_TOOL_CACHE:-}" ] && [ -x "$RUNNER_TOOL_CACHE/node/bin/node" ] && ! grep -q '^# CRABBOX_LOCAL_ACTIONS_NODE_PATH$' "$env_file"; then
    {
      printf '%s\n' '# CRABBOX_LOCAL_ACTIONS_NODE_PATH'
      printf '%s\n' 'export PATH="${RUNNER_TOOL_CACHE}/node/bin:$PATH"'
    } >> "$env_file"
  fi
fi
`
	return "bash -lc " + shellQuote(script)
}

func remoteClearActionsHydrationState(leaseID string) string {
	return "rm -f \"$HOME\"/" + shellQuote(actionsHydrationStatePath(leaseID)) + " \"$HOME\"/" + shellQuote(actionsHydrationEnvPath(leaseID)) + " \"$HOME\"/" + shellQuote(actionsHydrationServicesPath(leaseID)) + " \"$HOME\"/" + shellQuote(actionsHydrationStopPath(leaseID)) + " \"$HOME\"/" + shellQuote(actionsHydrationLocalScriptPath(leaseID)) + " \"$HOME\"/" + shellQuote(actionsHydrationLocalLogPath(leaseID)) + " \"$HOME\"/" + shellQuote(actionsHydrationLocalExitPath(leaseID))
}

func remoteClearActionsHydrationStateForTarget(target SSHTarget, leaseID string) string {
	if isWindowsNativeTarget(target) {
		paths := []string{
			windowsActionsHydrationPath(actionsHydrationStatePath(leaseID)),
			windowsActionsHydrationPath(actionsHydrationEnvPath(leaseID)),
			windowsActionsHydrationPath(actionsHydrationServicesPath(leaseID)),
			windowsActionsHydrationPath(actionsHydrationStopPath(leaseID)),
			windowsActionsHydrationPath(actionsHydrationLocalScriptPath(leaseID)),
			windowsActionsHydrationPath(actionsHydrationLocalLogPath(leaseID)),
			windowsActionsHydrationPath(actionsHydrationLocalExitPath(leaseID)),
		}
		var b strings.Builder
		b.WriteString(`$ErrorActionPreference = "SilentlyContinue"` + "\n")
		foreach := "$paths = @("
		for i, value := range paths {
			if i > 0 {
				foreach += ", "
			}
			foreach += psQuote(value)
		}
		foreach += ")\n"
		b.WriteString(foreach)
		b.WriteString(`foreach ($rel in $paths) {
  Remove-Item -LiteralPath $rel -Force -ErrorAction SilentlyContinue
}
exit 0
`)
		return powershellCommand(b.String())
	}
	return remoteClearActionsHydrationState(leaseID)
}

func actionsHydrationStatePath(leaseID string) string {
	return actionsHydrationDir() + "/" + leaseID + ".env"
}

func actionsHydrationEnvPath(leaseID string) string {
	return actionsHydrationDir() + "/" + leaseID + ".env.sh"
}

func actionsHydrationServicesPath(leaseID string) string {
	return actionsHydrationDir() + "/" + leaseID + ".services"
}

func actionsHydrationStopPath(leaseID string) string {
	return actionsHydrationDir() + "/" + leaseID + ".stop"
}

func actionsHydrationLocalScriptPath(leaseID string) string {
	return actionsHydrationDir() + "/" + leaseID + ".local.sh"
}

func actionsHydrationLocalLogPath(leaseID string) string {
	return actionsHydrationDir() + "/" + leaseID + ".local.log"
}

func actionsHydrationLocalExitPath(leaseID string) string {
	return actionsHydrationDir() + "/" + leaseID + ".local.exit"
}

func actionsHydrationDir() string {
	return ".crabbox/actions"
}

func windowsActionsHydrationPath(value string) string {
	return windowsPathJoin(windowsActionsHydrationRoot(), strings.TrimPrefix(value, actionsHydrationDir()+"/"))
}

func windowsActionsHydrationRoot() string {
	return `C:\ProgramData\crabbox\actions`
}

func actionsRunURL(repo GitHubRepo, runID string) string {
	if repo.Slug() == "" || runID == "" || strings.HasPrefix(runID, "local-") {
		return ""
	}
	return "https://github.com/" + repo.Slug() + "/actions/runs/" + runID
}

func remoteWriteActionsHydrationStop(leaseID string) string {
	return "mkdir -p \"$HOME\"/" + shellQuote(actionsHydrationDir()) + " && touch \"$HOME\"/" + shellQuote(actionsHydrationStopPath(leaseID))
}

func remoteWriteActionsHydrationStopForTarget(target SSHTarget, leaseID string) string {
	if isWindowsNativeTarget(target) {
		return powershellCommand(`$ErrorActionPreference = "Stop"
$dir = ` + psQuote(windowsActionsHydrationRoot()) + `
New-Item -ItemType Directory -Force -Path $dir | Out-Null
New-Item -ItemType File -Force -Path ` + psQuote(windowsActionsHydrationPath(actionsHydrationStopPath(leaseID))) + ` | Out-Null
exit 0
`)
	}
	return remoteWriteActionsHydrationStop(leaseID)
}

func githubActionsRunnerInstallScript(version string, ephemeral bool) string {
	if version == "" {
		version = "latest"
	}
	ephemeralArg := ""
	if ephemeral {
		ephemeralArg = "--ephemeral"
	}
	return fmt.Sprintf(`set -euo pipefail
if [ -z "${RUNNER_REPO:-}" ] || [ -z "${RUNNER_NAME:-}" ] || [ -z "${RUNNER_TOKEN:-}" ]; then
  echo "missing runner env" >&2
  exit 2
fi
version=%s
arch="$(uname -m)"
case "$arch" in
  x86_64|amd64) runner_arch=x64 ;;
  aarch64|arm64) runner_arch=arm64 ;;
  *) echo "unsupported runner arch: $arch" >&2; exit 2 ;;
esac
if [ "$(id -u)" = 0 ]; then
  export RUNNER_ALLOW_RUNASROOT=1
fi
if [ "$version" = latest ]; then
  release_url=https://api.github.com/repos/actions/runner/releases/latest
else
  release_url="https://api.github.com/repos/actions/runner/releases/tags/v${version}"
fi
release_json="$(curl -fsSL "$release_url")"
resolved_version="$(jq -er '.tag_name | strings | sub("^v"; "")' <<<"$release_json")"
if [ "$version" != latest ] && [ "$resolved_version" != "$version" ]; then
  echo "runner release metadata resolved unexpected version: wanted=$version got=$resolved_version" >&2
  exit 1
fi
version="$resolved_version"
archive_name="actions-runner-linux-${runner_arch}-${version}.tar.gz"
expected_sha="$(jq -er --arg name "$archive_name" '.assets[] | select(.name == $name) | .digest | strings | select(test("^sha256:[0-9a-fA-F]{64}$")) | sub("^sha256:"; "") | ascii_downcase' <<<"$release_json")"
runner_dir="$HOME/actions-runner"
mkdir -p "$runner_dir"
cd "$runner_dir"
version_marker=".crabbox-runner-version-$version-$runner_arch-sha256-$expected_sha"
if [ ! -x ./config.sh ] || [ ! -f "$version_marker" ]; then
  archive="$(mktemp "${TMPDIR:-/tmp}/crabbox-actions-runner.XXXXXX.tar.gz")"
  trap 'rm -f "$archive"' EXIT
  curl -fsSL -o "$archive" "https://github.com/actions/runner/releases/download/v${version}/${archive_name}"
  actual_sha="$(sha256sum "$archive" | cut -d' ' -f1 | tr '[:upper:]' '[:lower:]')"
  if [ "$actual_sha" != "$expected_sha" ]; then
    echo "runner archive checksum mismatch: expected=$expected_sha actual=$actual_sha" >&2
    exit 1
  fi
  rm -rf ./*
  tar xzf "$archive"
  rm -f "$archive"
  trap - EXIT
  touch "$version_marker"
fi
if [ -f .runner ]; then
  ./config.sh remove --unattended --token "$RUNNER_TOKEN" || true
fi
if command -v apt-get >/dev/null 2>&1 && grep -qi microsoft /proc/version 2>/dev/null; then
  sudo rm -rf /var/lib/apt/lists/*
  sudo apt-get update >/tmp/crabbox-actions-runner-apt-update.log 2>&1
fi
sudo ./bin/installdependencies.sh >/tmp/crabbox-actions-runner-deps.log 2>&1 || true
sudo mkdir -p "$HOME/.cache/node/corepack/v1"
sudo chown -R "$(id -u):$(id -g)" "$HOME/.cache" 2>/dev/null || true
./config.sh --unattended --replace %s --url "https://github.com/${RUNNER_REPO}" --token "$RUNNER_TOKEN" --name "$RUNNER_NAME" --labels "$RUNNER_LABELS"
cat >"$HOME/actions-runner/run-crabbox.sh" <<'RUNNER'
#!/usr/bin/env bash
set -euo pipefail
if [ "$(id -u)" = 0 ]; then
  export RUNNER_ALLOW_RUNASROOT=1
fi
cd "$HOME/actions-runner"
exec ./run.sh
RUNNER
chmod +x "$HOME/actions-runner/run-crabbox.sh"
sudo tee /etc/systemd/system/crabbox-actions-runner.service >/dev/null <<SERVICE
[Unit]
Description=Crabbox GitHub Actions runner
After=network-online.target docker.service
Wants=network-online.target

[Service]
User=$(id -un)
WorkingDirectory=$HOME/actions-runner
ExecStart=$HOME/actions-runner/run-crabbox.sh
Restart=no

[Install]
WantedBy=multi-user.target
SERVICE
sudo systemctl daemon-reload
sudo systemctl enable --now crabbox-actions-runner.service
`, shellQuote(version), ephemeralArg)
}

func githubActionsRunnerInstallScriptForTarget(version string, ephemeral bool, target SSHTarget) string {
	if isWindowsNativeTarget(target) {
		return githubActionsRunnerInstallPowerShellScript(version, ephemeral)
	}
	return githubActionsRunnerInstallScript(version, ephemeral)
}

func githubActionsRunnerInstallRemoteCommand(target SSHTarget) string {
	if isWindowsNativeTarget(target) {
		return powershellCommand(`$ErrorActionPreference = "Stop"
function Read-CrabboxRunnerValue {
  $line = [Console]::In.ReadLine()
  if ($null -eq $line) { throw "missing runner registration input" }
  [System.Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($line))
}
$env:RUNNER_REPO = Read-CrabboxRunnerValue
$env:RUNNER_NAME = Read-CrabboxRunnerValue
$env:RUNNER_LABELS = Read-CrabboxRunnerValue
$env:RUNNER_TOKEN = Read-CrabboxRunnerValue
$script = [Console]::In.ReadToEnd()
$path = Join-Path $env:TEMP ("crabbox-actions-runner-" + [Guid]::NewGuid().ToString("N") + ".ps1")
[System.IO.File]::WriteAllText($path, $script, [System.Text.UTF8Encoding]::new($false))
try {
  & powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File $path
  exit $LASTEXITCODE
} finally {
  Remove-Item -LiteralPath $path -Force -ErrorAction SilentlyContinue
}
`)
	}
	return `set -eu
IFS= read -r crabbox_runner_repo
IFS= read -r crabbox_runner_name
IFS= read -r crabbox_runner_labels
IFS= read -r crabbox_runner_token
export RUNNER_REPO="$(printf '%s' "$crabbox_runner_repo" | base64 -d)"
export RUNNER_NAME="$(printf '%s' "$crabbox_runner_name" | base64 -d)"
export RUNNER_LABELS="$(printf '%s' "$crabbox_runner_labels" | base64 -d)"
export RUNNER_TOKEN="$(printf '%s' "$crabbox_runner_token" | base64 -d)"
unset crabbox_runner_repo crabbox_runner_name crabbox_runner_labels crabbox_runner_token
exec bash -s`
}

func githubActionsRunnerInstallInput(repo, name, labels, token, script string) string {
	var input strings.Builder
	for _, value := range []string{repo, name, labels, token} {
		input.WriteString(base64.StdEncoding.EncodeToString([]byte(value)))
		input.WriteByte('\n')
	}
	input.WriteString(script)
	return input.String()
}

func githubActionsRunnerInstallPowerShellScript(version string, ephemeral bool) string {
	if version == "" {
		version = "latest"
	}
	ephemeralArg := ""
	if ephemeral {
		ephemeralArg = `, "--ephemeral"`
	}
	return fmt.Sprintf(`$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
if ([string]::IsNullOrWhiteSpace($env:RUNNER_REPO) -or [string]::IsNullOrWhiteSpace($env:RUNNER_NAME) -or [string]::IsNullOrWhiteSpace($env:RUNNER_TOKEN)) {
  throw "missing runner env"
}
$version = %s
$arch = $env:PROCESSOR_ARCHITECTURE
switch -Regex ($arch) {
  "^(AMD64|x86_64)$" { $runnerArch = "x64"; break }
  "^ARM64$" { $runnerArch = "arm64"; break }
  default { throw "unsupported runner arch: $arch" }
}
if ($version -eq "latest") {
  $releaseUri = "https://api.github.com/repos/actions/runner/releases/latest"
} else {
  $releaseUri = "https://api.github.com/repos/actions/runner/releases/tags/v$version"
}
$release = Invoke-RestMethod -Uri $releaseUri -UseBasicParsing
$resolvedVersion = ($release.tag_name -replace "^v", "")
if ($version -ne "latest" -and $resolvedVersion -ne $version) {
  throw "runner release metadata resolved unexpected version: wanted=$version got=$resolvedVersion"
}
$version = $resolvedVersion
$archiveName = "actions-runner-win-$runnerArch-$version.zip"
$asset = @($release.assets | Where-Object { $_.name -eq $archiveName })
if ($asset.Count -ne 1) {
  throw "runner release metadata has no unique asset named $archiveName"
}
$digestMatch = [Regex]::Match([string]$asset[0].digest, "^sha256:(?<sha>[0-9a-fA-F]{64})$")
if (-not $digestMatch.Success) {
  throw "runner release asset has no valid SHA-256 digest for $archiveName"
}
$expectedSha = $digestMatch.Groups["sha"].Value.ToLowerInvariant()
$runnerDir = Join-Path $HOME "actions-runner"
New-Item -ItemType Directory -Force -Path $runnerDir | Out-Null
Set-Location -LiteralPath $runnerDir
$versionMarker = ".crabbox-runner-version-$version-$runnerArch-sha256-$expectedSha"
if (-not (Test-Path -LiteralPath ".\config.cmd") -or -not (Test-Path -LiteralPath $versionMarker)) {
  $zip = Join-Path ([IO.Path]::GetTempPath()) ("crabbox-actions-runner-" + [Guid]::NewGuid().ToString("N") + ".zip")
  try {
    Invoke-WebRequest -Uri "https://github.com/actions/runner/releases/download/v$version/$archiveName" -OutFile $zip -UseBasicParsing
    $actualSha = (Get-FileHash -LiteralPath $zip -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actualSha -ne $expectedSha) {
      throw "runner archive checksum mismatch: expected=$expectedSha actual=$actualSha"
    }
    Get-ChildItem -Force -LiteralPath $runnerDir | Remove-Item -Recurse -Force
    Expand-Archive -LiteralPath $zip -DestinationPath $runnerDir -Force
    New-Item -ItemType File -Path $versionMarker -Force | Out-Null
  } finally {
    Remove-Item -LiteralPath $zip -Force -ErrorAction SilentlyContinue
  }
}
if (Test-Path -LiteralPath ".\.runner") {
  & .\config.cmd remove --unattended --token $env:RUNNER_TOKEN
  if ($LASTEXITCODE -ne 0) { Write-Warning "previous runner removal failed; continuing with replace"; $global:LASTEXITCODE = 0 }
}
$configArgs = @("--unattended", "--replace"%s, "--url", "https://github.com/$env:RUNNER_REPO", "--token", $env:RUNNER_TOKEN, "--name", $env:RUNNER_NAME, "--labels", $env:RUNNER_LABELS)
& .\config.cmd @configArgs
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
$runScript = Join-Path $runnerDir "run-crabbox.ps1"
Set-Content -Encoding UTF8 -LiteralPath $runScript -Value @'
$ErrorActionPreference = "Stop"
Set-Location -LiteralPath $PSScriptRoot
& .\run.cmd
exit $LASTEXITCODE
'@
$log = Join-Path $runnerDir "crabbox-runner.log"
$err = Join-Path $runnerDir "crabbox-runner.err.log"
$taskName = ("crabbox-actions-runner-" + ($env:RUNNER_NAME -replace "[^A-Za-z0-9_.-]", "-"))
$passwordPath = "C:\ProgramData\crabbox\windows.password"
if (Test-Path -LiteralPath $passwordPath) {
  Unregister-ScheduledTask -TaskName $taskName -Confirm:$false -ErrorAction SilentlyContinue
  $argument = '-NoLogo -NoProfile -ExecutionPolicy Bypass -File "' + $runScript + '"'
  $action = New-ScheduledTaskAction -Execute "powershell.exe" -Argument $argument
  $trigger = New-ScheduledTaskTrigger -Once -At ((Get-Date).AddMinutes(5))
  $password = (Get-Content -Raw -LiteralPath $passwordPath).Trim()
  Register-ScheduledTask -TaskName $taskName -Action $action -Trigger $trigger -User (whoami) -Password $password -RunLevel Highest -Force | Out-Null
  Start-ScheduledTask -TaskName $taskName
  Write-Output ("started runner task=" + $taskName)
} else {
  $process = Start-Process -FilePath "powershell.exe" -ArgumentList @("-NoLogo", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", $runScript) -WorkingDirectory $runnerDir -RedirectStandardOutput $log -RedirectStandardError $err -WindowStyle Hidden -PassThru
  Write-Output ("started runner pid=" + $process.Id)
}
`, psQuote(version), ephemeralArg)
}

func githubActionsRegistrationToken(ctx context.Context, repo GitHubRepo) (string, error) {
	out, err := ghOutput(ctx, "", "api", "-X", "POST", "repos/"+repo.Slug()+"/actions/runners/registration-token", "--jq", ".token")
	if err != nil {
		if isGitHubRunnerRegistrationPermissionError(err) {
			return "", exit(3, "GitHub Actions runner registration for %s requires repository write access or fine-grained Self-hosted runners write permission. If this is a Blacksmith Testbox tbx_... id, skip actions hydrate and run with --provider blacksmith-testbox.", repo.Slug())
		}
		return "", err
	}
	token := strings.TrimSpace(out)
	if token == "" {
		return "", exit(3, "GitHub returned an empty runner registration token for %s", repo.Slug())
	}
	return token, nil
}

func isGitHubRunnerRegistrationPermissionError(err error) bool {
	text := err.Error()
	return strings.Contains(text, "repository write permissions") ||
		strings.Contains(text, "repository runners fine-grained permission") ||
		strings.Contains(text, "HTTP 403")
}

func resolveGitHubRepo(repo Repo, override string) (GitHubRepo, error) {
	if override != "" {
		return parseGitHubRepo(override)
	}
	return parseGitHubRepo(repo.RemoteURL)
}

var scpLikeGitHubRemote = regexp.MustCompile(`^[^@]+@github\.com:([^/]+)/(.+)$`)

func parseGitHubRepo(value string) (GitHubRepo, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return GitHubRepo{}, exit(2, "GitHub repo is unknown; set actions.repo or pass --repo owner/name")
	}
	if !strings.Contains(value, "://") {
		if match := scpLikeGitHubRemote.FindStringSubmatch(value); match != nil {
			return cleanGitHubRepo(match[1], match[2])
		}
		parts := strings.Split(strings.TrimSuffix(value, ".git"), "/")
		if len(parts) == 2 {
			return cleanGitHubRepo(parts[0], parts[1])
		}
	}
	u, err := url.Parse(value)
	if err == nil && strings.EqualFold(u.Host, "github.com") {
		parts := strings.Split(strings.Trim(path.Clean(u.Path), "/"), "/")
		if len(parts) >= 2 {
			return cleanGitHubRepo(parts[0], parts[1])
		}
	}
	return GitHubRepo{}, exit(2, "unsupported GitHub repo %q; expected owner/name or github.com remote", value)
}

func cleanGitHubRepo(owner, name string) (GitHubRepo, error) {
	owner = strings.TrimSpace(owner)
	name = strings.TrimSuffix(strings.TrimSpace(name), ".git")
	if owner == "" || name == "" {
		return GitHubRepo{}, exit(2, "invalid GitHub repo owner/name")
	}
	return GitHubRepo{Owner: owner, Name: name}, nil
}

func sanitizeGitHubRunnerLabel(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "unknown"
	}
	return out
}

func ghOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", exit(3, "gh %s: %v\n%s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func runGH(ctx context.Context, dir string, args ...string) error {
	_, err := ghOutput(ctx, dir, args...)
	return err
}

type stringListFlag []string

func (f *stringListFlag) String() string {
	return strings.Join(*f, ",")
}

func (f *stringListFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}
