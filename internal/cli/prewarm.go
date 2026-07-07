package cli

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"
)

const prewarmFailureCleanupTimeout = 2 * time.Minute

func (a App) prewarm(ctx context.Context, args []string) error {
	started := time.Now()
	defaults := defaultConfig()
	fs := newFlagSet("prewarm", a.Stderr)
	leaseFlags := registerLeaseCreateFlags(fs, defaults)
	noHydrate := fs.Bool("no-hydrate", false, "skip configured Actions hydration")
	githubRunner := fs.Bool("github-runner", false, "hydrate by registering a GitHub self-hosted runner")
	repoFlag := fs.String("repo", "", "GitHub repository owner/name for hydration")
	workflowFlag := fs.String("workflow", "", "workflow file/name/id for hydration")
	jobFlag := fs.String("job", "", "expected hydrate workflow job/input name")
	refFlag := fs.String("ref", "", "workflow ref for hydration")
	waitTimeout := fs.Duration("wait-timeout", 20*time.Minute, "time to wait for Actions hydration")
	keepAliveMinutes := fs.Int("keep-alive-minutes", 90, "minutes for workflow to keep a GitHub runner job alive")
	probeCommand := fs.String("probe-command", "", "optional shell command to prove the hydrated box is test-ready")
	poolKey := fs.String("pool", "", "register the hydrated lease in a broker ready pool")
	dryRun := fs.Bool("dry-run", false, "print the planned Crabbox commands without running them")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	timingJSON := fs.Bool("timing-json", false, "print final timing as JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	_ = reclaim
	requestedSlug, err := requestedLeaseSlug(*leaseFlags.Slug)
	if err != nil {
		return err
	}
	if requestedSlug != "" {
		*leaseFlags.Slug = requestedSlug
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	mutateExternal := !*dryRun
	if err := applyLeaseCreateFlagsForLeaseMode(&cfg, fs, leaseFlags, "", mutateExternal); err != nil {
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
	backend, err := loadBackend(cfg, runtimeForApp(a))
	if err != nil {
		return err
	}
	if backend.Spec().Kind == ProviderKindServiceControl {
		return exit(2, "prewarm is not supported for provider=%s; it does not provide a lease or run surface", backend.Spec().Name)
	}
	readyPoolKey := strings.TrimSpace(*poolKey)
	if readyPoolKey != "" && backendCoordinator(backend) == nil {
		return exit(2, "--pool requires a coordinator-backed SSH lease provider")
	}
	leaseArgs := prewarmWarmupArgs(args)
	if backend.Spec().Kind == ProviderKindDelegatedRun && isBlacksmithProvider(cfg.Provider) {
		leaseArgs = prewarmBlacksmithHydrationArgs(fs, leaseArgs, *workflowFlag, *jobFlag, *refFlag)
	}
	followupArgs := prewarmProviderPassthroughArgs(args, defaults)
	hydrateArgs := prewarmHydrateArgs(cfg, "<lease>", *githubRunner, *waitTimeout, *keepAliveMinutes, followupArgs)
	probeArgs := prewarmProbeArgs(cfg, "<lease>", *probeCommand, followupArgs)
	if *dryRun {
		fmt.Fprintln(a.Stdout, "crabbox "+strings.Join(readableShellWords(append([]string{"warmup"}, leaseArgs...)), " "))
		if !*noHydrate && cfg.Actions.Workflow != "" && backend.Spec().Kind != ProviderKindDelegatedRun {
			fmt.Fprintln(a.Stdout, "crabbox "+strings.Join(readableShellWords(append([]string{"actions", "hydrate"}, hydrateArgs...)), " "))
		}
		if strings.TrimSpace(*probeCommand) != "" {
			fmt.Fprintln(a.Stdout, "crabbox "+strings.Join(readableShellWords(append([]string{"run"}, probeArgs...)), " "))
		}
		return nil
	}
	var out bytes.Buffer
	var acquiredLease LeaseTarget
	warmupStarted := time.Now()
	warmupApp := App{Stdout: io.MultiWriter(a.Stdout, &out), Stderr: a.Stderr}
	if err := warmupApp.warmupWithLeaseObserver(ctx, leaseArgs, func(lease LeaseTarget) { acquiredLease = lease }); err != nil {
		return err
	}
	leaseID := parseWarmupLeaseID(out.String())
	if leaseID == "" {
		return exit(2, "prewarm could not parse warmup lease id")
	}
	if backend.Spec().Kind != ProviderKindDelegatedRun && acquiredLease.LeaseID != leaseID {
		return exit(2, "prewarm warmup lease identity was not preserved")
	}
	warmupMs := time.Since(warmupStarted).Milliseconds()
	hydration := "skipped"
	hydrateMs := int64(0)
	if backend.Spec().Kind == ProviderKindDelegatedRun {
		hydration = "provider-owned"
		fmt.Fprintf(a.Stdout, "prewarm hydration=%s provider=%s id=%s\n", hydration, backend.Spec().Name, leaseID)
	} else if !*noHydrate && cfg.Actions.Workflow != "" {
		hydration = "actions"
		hydrateStarted := time.Now()
		hydrateArgs = prewarmHydrateArgs(cfg, leaseID, *githubRunner, *waitTimeout, *keepAliveMinutes, followupArgs)
		if err := a.runPrewarmPostWarmupStep(ctx, backend, cfg, acquiredLease, "actions hydration", func() error {
			return a.actionsHydrate(ctx, hydrateArgs)
		}); err != nil {
			return err
		}
		hydrateMs = time.Since(hydrateStarted).Milliseconds()
	} else if !*noHydrate {
		fmt.Fprintf(a.Stdout, "prewarm hydration=skipped reason=no_actions_workflow id=%s\n", leaseID)
	}
	probeMs := int64(0)
	if strings.TrimSpace(*probeCommand) != "" {
		probeStarted := time.Now()
		if err := a.runPrewarmPostWarmupStep(ctx, backend, cfg, acquiredLease, "probe", func() error {
			return a.runCommand(ctx, prewarmProbeArgs(cfg, leaseID, *probeCommand, followupArgs))
		}); err != nil {
			return err
		}
		probeMs = time.Since(probeStarted).Milliseconds()
	}
	if readyPoolKey != "" {
		if err := a.runPrewarmPostWarmupStep(ctx, backend, cfg, acquiredLease, "pool registration", func() error {
			return a.registerPrewarmedLeaseInReadyPool(ctx, cfg, leaseID, readyPoolKey, *githubRunner)
		}); err != nil {
			return err
		}
	}
	total := time.Since(started)
	fmt.Fprintf(a.Stdout, "prewarm complete id=%s provider=%s hydration=%s warmup=%dms hydrate=%dms probe=%dms total=%s\n", leaseID, cfg.Provider, hydration, warmupMs, hydrateMs, probeMs, total.Round(time.Millisecond))
	if *timingJSON {
		return writeTimingJSON(a.Stderr, timingReport{
			Provider:  cfg.Provider,
			LeaseID:   leaseID,
			HydrateMs: hydrateMs,
			ProbeMs:   probeMs,
			TotalMs:   total.Milliseconds(),
			ExitCode:  0,
		})
	}
	return nil
}

func prewarmHydrateArgs(cfg Config, leaseID string, githubRunner bool, waitTimeout time.Duration, keepAliveMinutes int, passthrough []string) []string {
	args := append([]string{}, passthrough...)
	args = append(args, "--provider", cfg.Provider, "--target", cfg.TargetOS, "--network", string(cfg.Network), "--id", leaseID)
	if cfg.TargetOS == targetWindows && cfg.WindowsMode != "" {
		args = append(args, "--windows-mode", cfg.WindowsMode)
	}
	if githubRunner {
		args = append(args, "--github-runner")
	}
	if cfg.Actions.Repo != "" {
		args = append(args, "--repo", cfg.Actions.Repo)
	}
	if cfg.Actions.Workflow != "" {
		args = append(args, "--workflow", cfg.Actions.Workflow)
	}
	if cfg.Actions.Job != "" {
		args = append(args, "--job", cfg.Actions.Job)
	}
	if cfg.Actions.Ref != "" {
		args = append(args, "--ref", cfg.Actions.Ref)
	}
	if waitTimeout > 0 {
		args = append(args, "--wait-timeout", waitTimeout.String())
	}
	if keepAliveMinutes > 0 {
		args = append(args, "--keep-alive-minutes", fmt.Sprint(keepAliveMinutes))
	}
	for _, field := range cfg.Actions.Fields {
		args = append(args, "--field", field)
	}
	return args
}

func prewarmWarmupArgs(args []string) []string {
	out := make([]string, 0, len(args)+1)
	valueFlags := map[string]struct{}{
		"repo": {}, "workflow": {}, "job": {}, "ref": {},
		"wait-timeout": {}, "keep-alive-minutes": {}, "probe-command": {}, "pool": {},
	}
	boolFlags := map[string]struct{}{
		"no-hydrate": {}, "github-runner": {}, "dry-run": {}, "timing-json": {},
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			out = append(out, arg)
			continue
		}
		name := strings.TrimLeft(arg, "-")
		if before, _, ok := strings.Cut(name, "="); ok {
			name = before
		}
		if _, ok := boolFlags[name]; ok {
			continue
		}
		if _, ok := valueFlags[name]; ok {
			if !strings.Contains(arg, "=") && i+1 < len(args) {
				i++
			}
			continue
		}
		out = append(out, arg)
	}
	out = append(out, "--keep=true")
	return out
}

func (a App) registerPrewarmedLeaseInReadyPool(ctx context.Context, cfg Config, leaseID, poolKey string, githubRunner bool) error {
	repo, _ := findRepo()
	input := map[string]any{"leaseID": leaseID}
	if repoValue := firstNonBlank(cfg.Actions.Repo, bestEffortGitHubRepoSlug(repo, cfg)); repoValue != "" {
		input["repo"] = repoValue
	}
	if refValue := firstNonBlank(cfg.Actions.Ref, repo.BaseRef); refValue != "" {
		input["ref"] = refValue
	}
	if commit := prewarmReadyPoolCommit(cfg, repo, githubRunner); commit != "" {
		input["commit"] = commit
	}
	addStringInput(input, "sshHost", readyPoolClaimSSHHost(leaseID))
	addStringInput(input, "sshPort", readyPoolClaimSSHPort(leaseID))
	addStringInput(input, "workRoot", readyPoolClaimWorkRoot(leaseID))
	coord, err := readyPoolCoordinatorFromConfig(cfg)
	if err != nil {
		return err
	}
	res, err := coord.RegisterReadyPoolLease(ctx, poolKey, input)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "pool registered key=%s lease=%s state=%s\n", res.Entry.Key, res.Entry.LeaseID, res.Entry.State)
	return nil
}

func (a App) runPrewarmPostWarmupStep(ctx context.Context, backend Backend, cfg Config, lease LeaseTarget, stage string, step func() error) error {
	err := step()
	if err == nil {
		return nil
	}
	a.releasePrewarmLeaseAfterFailure(ctx, backend, cfg, lease, stage)
	return err
}

func (a App) releasePrewarmLeaseAfterFailure(ctx context.Context, backend Backend, cfg Config, acquired LeaseTarget, stage string) {
	sshBackend, ok := backend.(SSHLeaseBackend)
	if !ok {
		return
	}
	leaseID := acquired.LeaseID
	if _, guarded := backend.(ReleaseLeaseTargetRefresher); guarded {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), prewarmFailureCleanupTimeout)
		defer cancel()
		fmt.Fprintf(a.Stderr, "prewarm cleanup: releasing id=%s after %s failure\n", leaseID, stage)
		if err := a.releaseBackendLeaseBestEffort(releaseCtx, sshBackend, cfg, acquired); err != nil {
			fmt.Fprintf(a.Stderr, "warning: prewarm %s failed; automatic release of %s failed: %v; next: crabbox stop --provider %s --id %s\n", stage, leaseID, err, cfg.Provider, leaseID)
			return
		}
		fmt.Fprintf(a.Stderr, "prewarm cleanup: released id=%s after %s failure\n", leaseID, stage)
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), prewarmFailureCleanupTimeout)
	defer cancel()
	lease, err := sshBackend.Resolve(cleanupCtx, ResolveRequest{
		Repo:        Repo{},
		Options:     leaseOptionsFromConfig(cfg),
		ID:          leaseID,
		ReleaseOnly: true,
	})
	if err != nil {
		if backendCoordinator(backend) == nil {
			fmt.Fprintf(a.Stderr, "warning: prewarm %s failed; automatic release of %s skipped: %v; next: crabbox stop --provider %s --id %s\n", stage, leaseID, err, cfg.Provider, leaseID)
			return
		}
		fmt.Fprintf(a.Stderr, "warning: could not inspect lease %s before prewarm cleanup: %v; releasing by lease ID\n", leaseID, err)
		lease = LeaseTarget{LeaseID: leaseID}
	}
	fmt.Fprintf(a.Stderr, "prewarm cleanup: releasing id=%s after %s failure\n", leaseID, stage)
	a.cleanupBackendLeaseConnectionsBestEffort(cleanupCtx, lease)
	releaseCtx, releaseCancel := context.WithTimeout(context.WithoutCancel(ctx), prewarmFailureCleanupTimeout)
	defer releaseCancel()
	if err := a.releaseBackendLease(releaseCtx, sshBackend, cfg, lease); err != nil {
		fmt.Fprintf(a.Stderr, "warning: prewarm %s failed; automatic release of %s failed: %v; next: crabbox stop --provider %s --id %s\n", stage, leaseID, err, cfg.Provider, leaseID)
		return
	}
	fmt.Fprintf(a.Stderr, "prewarm cleanup: released id=%s after %s failure\n", leaseID, stage)
}

func prewarmReadyPoolCommit(cfg Config, repo Repo, githubRunner bool) string {
	ref := strings.TrimSpace(cfg.Actions.Ref)
	if ref == "" {
		if githubRunner {
			return ""
		}
		return strings.TrimSpace(repo.Head)
	}
	if isGitCommitSHA(ref) {
		if githubRunner || ref == strings.TrimSpace(repo.Head) {
			return ref
		}
		return ""
	}
	if githubRunner {
		return ""
	}
	if repo.Root == "" {
		return ""
	}
	branch := strings.TrimSpace(gitOutput(repo.Root, "rev-parse", "--abbrev-ref", "HEAD"))
	if branch != "" && (ref == branch || ref == "refs/heads/"+branch) {
		return strings.TrimSpace(repo.Head)
	}
	return ""
}

func isGitCommitSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, ch := range value {
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')) {
			return false
		}
	}
	return true
}

func prewarmBlacksmithHydrationArgs(fs *flag.FlagSet, args []string, workflow, job, ref string) []string {
	if workflow != "" && !flagWasSet(fs, "blacksmith-workflow") {
		args = append(args, "--blacksmith-workflow", workflow)
	}
	if job != "" && !flagWasSet(fs, "blacksmith-job") {
		args = append(args, "--blacksmith-job", job)
	}
	if ref != "" && !flagWasSet(fs, "blacksmith-ref") {
		args = append(args, "--blacksmith-ref", ref)
	}
	return args
}

func prewarmProbeArgs(cfg Config, leaseID, command string, passthrough []string) []string {
	args := append([]string{}, passthrough...)
	args = append(args, "--provider", cfg.Provider, "--target", cfg.TargetOS)
	if cfg.TargetOS == targetWindows && cfg.WindowsMode != "" {
		args = append(args, "--windows-mode", cfg.WindowsMode)
	}
	args = append(args, "--network", string(cfg.Network), "--id", leaseID, "--no-sync", "--no-hydrate", "--shell", "--", command)
	return args
}

func prewarmProviderPassthroughArgs(args []string, defaults Config) []string {
	allowed := prewarmProviderPassthroughFlags(defaults)
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		name := strings.TrimLeft(arg, "-")
		if before, _, ok := strings.Cut(name, "="); ok {
			name = before
		}
		takesValue, ok := allowed[name]
		if !ok {
			continue
		}
		out = append(out, arg)
		if takesValue && !strings.Contains(arg, "=") && i+1 < len(args) {
			i++
			out = append(out, args[i])
		}
	}
	return out
}

func prewarmProviderPassthroughFlags(defaults Config) map[string]bool {
	fs := flag.NewFlagSet("prewarm-followup", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	_ = registerProviderFlags(fs, defaults)
	flags := map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) {
		takesValue := true
		if boolValue, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && boolValue.IsBoolFlag() {
			takesValue = false
		}
		flags[f.Name] = takesValue
	})
	for _, provider := range registeredProviders() {
		if creationOnly, ok := provider.(ProviderCreationOnlyFlagProvider); ok {
			for _, name := range creationOnly.CreationOnlyFlagNames() {
				delete(flags, name)
			}
		}
	}
	return flags
}
