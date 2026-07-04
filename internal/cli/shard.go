package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
)

const shardPartialLineLimit = 64 * 1024

type shardRunOutcome struct {
	Recorded bool
	ExitCode int
	RunID    string
	Results  *TestResultSummary
}

var shardForbiddenRunFlags = map[string]bool{
	"pool":              true,
	"pool-return":       true,
	"sync-only":         true,
	"no-sync":           true,
	"apply-local-patch": true,
	"fresh-pr":          true,
	"script":            true,
	"script-stdin":      true,
	"stop-after":        true,
	"lease-output":      true,
	"keep-on-failure":   true,
	"capture-stdout":    true,
	"capture-stderr":    true,
	"download":          true,
	"emit-proof":        true,
	"proof-template":    true,
	"label":             true,
	"id":                true,
	"timing-json":       true,
	"timing-record":     true,
}

var shardOwnedOnlyFlags = map[string]bool{
	"count":                 true,
	"from":                  true,
	"fail-fast":             true,
	"keep":                  true,
	"quiet":                 true,
	"json":                  true,
	"dry-run":               true,
	"workdir":               true,
	"clear":                 true,
	"reclaim":               true,
	"slug":                  true,
	"fail-on-test-failures": true,
}

type shardOptions struct {
	Checkpoint         string
	Count              int
	Keep               bool
	FailFast           bool
	Quiet              bool
	JSON               bool
	FailOnTestFailures bool
	ResultsWired       bool
	RequestedSlug      string
	RunArgs            []string
	Command            []string
}

type shardResult struct {
	Index    int
	LeaseID  string
	Slug     string
	RunID    string
	ExitCode int
	Recorded bool
	Canceled bool
	Results  *TestResultSummary
	Err      error
}

func (r shardResult) commandFailed() bool {
	return r.Recorded && r.ExitCode != 0 && !r.Canceled
}

func (r shardResult) infraFailed() bool {
	return !r.Recorded && !r.Canceled && (r.Err != nil || r.LeaseID == "")
}

func (r shardResult) failed() bool {
	return r.commandFailed() || r.infraFailed()
}

type shardProvisioner func(ctx context.Context, index int, slug string) (checkpointForkProvision, error)

type shardRunExecutor func(ctx context.Context, index int, runArgs []string, outcome *shardRunOutcome) error

func (a App) shard(ctx context.Context, args []string) error {
	shardArgs, command := splitCheckpointForkRunArgs(args)
	defaults := defaultConfig()
	fs := newFlagSet("shard", a.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage:")
		fmt.Fprintln(fs.Output(), "  crabbox shard --count <n> --from <checkpoint-id> [flags] -- <command...>")
		fmt.Fprintln(fs.Output(), "")
		fmt.Fprintln(fs.Output(), "Forks --count leases from a checkpoint concurrently, runs the command on")
		fmt.Fprintln(fs.Output(), "every fork in parallel through the normal run pipeline, and merges the")
		fmt.Fprintln(fs.Output(), "collected JUnit results into one suite verdict. The command may use the")
		fmt.Fprintln(fs.Output(), "{{index}}, {{total}}, {{lease}}, and {{slug}} placeholders. Other run flags")
		fmt.Fprintln(fs.Output(), "pass through to each shard's run.")
		fmt.Fprintln(fs.Output(), "")
		fmt.Fprintln(fs.Output(), "Flags:")
		fs.PrintDefaults()
	}
	leaseFlags := registerLeaseCreateFlags(fs, defaults)
	count := fs.Int("count", 0, "number of parallel shards to run")
	from := fs.String("from", "", "checkpoint id to fork each shard from")
	failFast := fs.Bool("fail-fast", false, "cancel remaining shards after the first failing shard")
	keep := fs.Bool("keep", false, "keep forked leases after their shard finishes")
	quiet := fs.Bool("quiet", false, "suppress shard output streams and print per-shard status lines only")
	jsonOut := fs.Bool("json", false, "print the merged verdict as JSON")
	dryRun := fs.Bool("dry-run", false, "show the shard plan without provisioning")
	workdirOverride := fs.String("workdir", "", "remote restore workdir")
	clear := fs.Bool("clear", true, "clear the remote workdir before restoring")
	reclaim := fs.Bool("reclaim", false, "claim these leases for the current repo")
	failOnTestFailures := fs.Bool("fail-on-test-failures", false, "exit non-zero when the merged JUnit results contain failures or errors")
	junitResults := fs.String("junit", "", "comma-separated remote JUnit XML paths to record")
	resultsAuto := fs.Bool("results-auto", false, "scan common remote JUnit XML paths after the command")
	ownArgs, runArgs, err := partitionForwardedRunArgs(fs, shardArgs, shardOwnedOnlyFlags, shardForbiddenRunFlags, "shard: it conflicts with parallel shard runs")
	if err != nil {
		return err
	}
	if err := parseFlags(fs, ownArgs); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return exit(2, "unexpected argument %q; place the command after --", fs.Arg(0))
	}
	if *count < 1 {
		return exit(2, "--count must be at least 1")
	}
	if strings.TrimSpace(*from) == "" {
		return exit(2, "usage: crabbox shard --count <n> --from <checkpoint-id> [flags] -- <command...>")
	}
	if len(command) == 0 {
		return exit(2, "usage: crabbox shard --count <n> --from <checkpoint-id> [flags] -- <command...>")
	}
	requestedSlug, err := requestedLeaseSlug(*leaseFlags.Slug)
	if err != nil {
		return err
	}
	store, err := defaultCheckpointStore()
	if err != nil {
		return err
	}
	record, paths, err := store.Read(strings.TrimSpace(*from))
	if err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if cfg.Shard.MaxCount > 0 && *count > cfg.Shard.MaxCount {
		return exit(2, "--count %d exceeds shard.maxCount %d", *count, cfg.Shard.MaxCount)
	}
	nativeCheckpoint := isNativeCheckpointKind(record.Kind)
	if nativeCheckpoint && record.TargetOS == targetMacOS && !flagWasSet(fs, "market") {
		cfg.Capacity.Market = "on-demand"
	}
	if err := applyLeaseCreateFlags(&cfg, fs, leaseFlags); err != nil {
		return err
	}
	if record.Kind != checkpointKindArchive && !nativeCheckpoint {
		return exit(2, "checkpoint %s has kind=%s; shard requires %s or a native image checkpoint", record.ID, record.Kind, checkpointKindArchive)
	}
	if nativeCheckpoint {
		if nativeCheckpointResourceID(record) == "" {
			return exit(2, "checkpoint %s is pending; native provider resource is not recorded yet", record.ID)
		}
		if err := applyNativeCheckpointForkConfigAndFlags(&cfg, fs, record, leaseFlags.ProviderFlags); err != nil {
			return err
		}
	}
	if *junitResults != "" {
		cfg.Results.JUnit = splitCommaList(*junitResults)
	}
	if flagWasSet(fs, "results-auto") {
		cfg.Results.Auto = *resultsAuto
	}
	mergedPolicy := cfg.Results.FailOnFailures
	if flagWasSet(fs, "fail-on-test-failures") {
		mergedPolicy = *failOnTestFailures
	}
	if *dryRun {
		for i := 1; i <= *count; i++ {
			slug := checkpointForkFanoutSlug(requestedSlug, i, *count)
			expandedCommand := checkpointForkRunCommand(command, checkpointForkRunContext{Index: i, Total: *count, Slug: slug})
			fmt.Fprintf(a.Stdout, "would shard checkpoint id=%s provider=%s slug=%s keep=%t index=%d/%d command=%s\n", record.ID, cfg.Provider, blank(slug, "-"), *keep, i, *count, strconv.Quote(runCommandDisplay(expandedCommand, false)))
		}
		return nil
	}
	repo, err := findRepo()
	if err != nil {
		return err
	}
	backend, err := loadBackend(cfg, runtimeForApp(a))
	if err != nil {
		return err
	}
	sshBackend, ok := backend.(SSHLeaseBackend)
	if !ok {
		return exit(2, "provider=%s does not support shard: it requires an SSH lease provider that supports checkpoint fork", backend.Spec().Name)
	}
	opts := shardOptions{
		Checkpoint:         record.ID,
		Count:              *count,
		Keep:               *keep,
		FailFast:           *failFast,
		Quiet:              *quiet,
		JSON:               *jsonOut,
		FailOnTestFailures: mergedPolicy,
		ResultsWired:       len(cfg.Results.JUnit) > 0 || cfg.Results.Auto,
		RequestedSlug:      requestedSlug,
		RunArgs:            runArgs,
		Command:            command,
	}
	fmt.Fprintf(a.Stderr, "shard plan checkpoint=%s provider=%s count=%d keep=%t fail_fast=%t command=%s\n", record.ID, cfg.Provider, opts.Count, opts.Keep, opts.FailFast, strconv.Quote(runCommandDisplay(command, false)))
	mux := &shardOutputMux{}
	workdir := strings.TrimSpace(*workdirOverride)
	provision := func(provisionCtx context.Context, index int, slug string) (checkpointForkProvision, error) {
		shardApp, flush := a.forShard(mux, index, opts.Count, opts.Quiet, nil)
		defer flush()
		return shardApp.provisionCheckpointFork(provisionCtx, cfg, backend, sshBackend, repo, record, paths, opts.Keep, *reclaim, slug, workdir, *clear)
	}
	execute := func(runCtx context.Context, index int, shardRunArgs []string, outcome *shardRunOutcome) error {
		shardApp, flush := a.forShard(mux, index, opts.Count, opts.Quiet, outcome)
		defer flush()
		return shardApp.runCommand(runCtx, shardRunArgs)
	}
	return a.shardRun(ctx, opts, mux, provision, execute)
}

func (a App) forShard(mux *shardOutputMux, index, total int, quiet bool, outcome *shardRunOutcome) (App, func()) {
	shardApp := a
	shardApp.runOutcome = outcome
	if quiet {
		shardApp.Stdout = io.Discard
		shardApp.Stderr = io.Discard
		return shardApp, func() {}
	}
	prefix := shardLinePrefix(index, total)
	stdout := &prefixLineWriter{mux: mux, out: a.Stdout, prefix: prefix}
	stderr := &prefixLineWriter{mux: mux, out: a.Stderr, prefix: prefix}
	shardApp.Stdout = stdout
	shardApp.Stderr = stderr
	return shardApp, func() {
		stdout.Flush()
		stderr.Flush()
	}
}

func shardLinePrefix(index, total int) string {
	width := len(strconv.Itoa(total))
	return fmt.Sprintf("[%*d/%d] ", width, index, total)
}

func (a App) shardRun(ctx context.Context, opts shardOptions, mux *shardOutputMux, provision shardProvisioner, execute shardRunExecutor) error {
	results := make([]shardResult, opts.Count)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	start := time.Now()
	for i := 1; i <= opts.Count; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			slug := checkpointForkFanoutSlug(opts.RequestedSlug, index, opts.Count)
			lease, err := provision(runCtx, index, slug)
			if err != nil {
				if runCtx.Err() != nil {
					results[index-1] = shardResult{Index: index, Slug: slug, Canceled: true, Err: err}
					mux.printf(a.Stderr, "shard %d/%d canceled\n", index, opts.Count)
					return
				}
				results[index-1] = shardResult{Index: index, Slug: slug, ExitCode: shardErrorExitCode(err), Err: err}
				mux.printf(a.Stderr, "shard %d/%d failed error=%q\n", index, opts.Count, err.Error())
				if opts.FailFast {
					cancel()
				}
				return
			}
			leaseID := lease.Lease.LeaseID
			leaseSlug := serverSlug(lease.Lease.Server)
			if !opts.Keep {
				defer lease.Release(context.Background())
			}
			mux.printf(a.Stderr, "shard %d/%d provisioned lease=%s slug=%s\n", index, opts.Count, leaseID, blank(leaseSlug, "-"))
			expanded := checkpointForkRunCommand(opts.Command, checkpointForkRunContext{Index: index, Total: opts.Count, LeaseID: leaseID, Slug: leaseSlug})
			var outcome shardRunOutcome
			err = execute(runCtx, index, shardRunArgs(leaseID, index, opts.Count, opts.RunArgs, expanded), &outcome)
			result := shardResult{Index: index, LeaseID: leaseID, Slug: leaseSlug, RunID: outcome.RunID, Recorded: outcome.Recorded, Results: outcome.Results}
			if outcome.Recorded {
				result.ExitCode = outcome.ExitCode
			} else {
				result.ExitCode = shardErrorExitCode(err)
				if err == nil {
					err = errors.New("run finished without recording an outcome")
				}
				result.Err = err
			}
			if runCtx.Err() != nil && (!outcome.Recorded || outcome.ExitCode < 0) {
				result.Canceled = true
				result.ExitCode = 0
			}
			results[index-1] = result
			a.printShardStatus(mux, opts, result)
			if opts.Keep {
				mux.printf(a.Stderr, "shard %d/%d kept lease=%s slug=%s\n", index, opts.Count, leaseID, blank(leaseSlug, "-"))
			}
			if result.failed() && opts.FailFast {
				cancel()
			}
		}(i)
	}
	wg.Wait()
	wall := time.Since(start)
	return a.shardVerdict(opts, results, wall)
}

func (a App) printShardStatus(mux *shardOutputMux, opts shardOptions, result shardResult) {
	if result.Canceled {
		mux.printf(a.Stderr, "shard %d/%d canceled\n", result.Index, opts.Count)
		return
	}
	if result.infraFailed() {
		mux.printf(a.Stderr, "shard %d/%d failed error=%q\n", result.Index, opts.Count, result.Err.Error())
		return
	}
	if result.Results != nil {
		mux.printf(a.Stderr, "shard %d/%d done exit=%d tests=%d failures=%d errors=%d\n", result.Index, opts.Count, result.ExitCode, result.Results.Tests, result.Results.Failures, result.Results.Errors)
		return
	}
	mux.printf(a.Stderr, "shard %d/%d done exit=%d\n", result.Index, opts.Count, result.ExitCode)
}

func shardErrorExitCode(err error) int {
	var exitErr ExitError
	if AsExitError(err, &exitErr) && exitErr.Code != 0 {
		return exitErr.Code
	}
	return 1
}

func shardRunArgs(leaseID string, index, total int, runArgs, command []string) []string {
	args := make([]string, 0, len(runArgs)+len(command)+7)
	args = append(args, "--id", leaseID, "--keep", "--label", fmt.Sprintf("shard %d/%d", index, total), "--fail-on-test-failures=false")
	args = append(args, runArgs...)
	args = append(args, "--")
	args = append(args, command...)
	return args
}

func (a App) shardVerdict(opts shardOptions, results []shardResult, wall time.Duration) error {
	merged := &TestResultSummary{Format: "junit", Files: []string{}, Failed: []TestFailure{}}
	summaries := 0
	failedShards := 0
	infraFailures := 0
	canceledShards := 0
	commandCodes := map[int]bool{}
	failedCases := []shardTestFailure{}
	for _, result := range results {
		if result.failed() {
			failedShards++
		}
		if result.infraFailed() {
			infraFailures++
		}
		if result.Canceled {
			canceledShards++
		}
		if result.commandFailed() {
			commandCodes[result.ExitCode] = true
		}
		if result.Results == nil {
			continue
		}
		summaries++
		mergeJUnitSummary(merged, result.Results)
		for _, failure := range result.Results.Failed {
			failedCases = append(failedCases, shardTestFailure{Shard: result.Index, RunID: result.RunID, TestFailure: failure})
		}
	}
	if summaries == 0 {
		merged = nil
	}
	err := shardExitError(opts, results, merged, commandCodes, failedShards, infraFailures, canceledShards)
	if opts.JSON {
		report := shardJSONReport{
			Checkpoint: opts.Checkpoint,
			Count:      opts.Count,
			Shards:     shardJSONResults(results),
			Merged:     merged,
			Failed:     failedCases,
			WallMs:     wall.Milliseconds(),
		}
		if err != nil {
			var exitErr ExitError
			if AsExitError(err, &exitErr) {
				report.ExitCode = exitErr.Code
			} else {
				report.ExitCode = 1
			}
		}
		if encodeErr := json.NewEncoder(a.Stdout).Encode(report); encodeErr != nil {
			return encodeErr
		}
		return err
	}
	a.printShardTable(opts, results)
	mergedLine := ""
	if merged != nil {
		mergedLine = fmt.Sprintf(" tests=%d failures=%d errors=%d skipped=%d suite_time=%.3fs", merged.Tests, merged.Failures, merged.Errors, merged.Skipped, merged.TimeSeconds)
	}
	canceledLine := ""
	if canceledShards > 0 {
		canceledLine = fmt.Sprintf(" canceled=%d", canceledShards)
	}
	fmt.Fprintf(a.Stdout, "shard verdict shards=%d failed_shards=%d%s%s wall=%.3fs\n", opts.Count, failedShards, canceledLine, mergedLine, wall.Seconds())
	if len(failedCases) > 0 {
		fmt.Fprintln(a.Stdout, "failed:")
		for _, failure := range failedCases {
			fmt.Fprintf(a.Stdout, "  [%d/%d] run=%s", failure.Shard, opts.Count, blank(failure.RunID, "-"))
			printTestFailure(a.Stdout, failure.TestFailure)
		}
	}
	return err
}

func (a App) printShardTable(opts shardOptions, results []shardResult) {
	leaseWidth := len("LEASE")
	runWidth := len("RUN")
	for _, result := range results {
		if len(result.LeaseID) > leaseWidth {
			leaseWidth = len(result.LeaseID)
		}
		if len(result.RunID) > runWidth {
			runWidth = len(result.RunID)
		}
	}
	fmt.Fprintln(a.Stdout, "shard results")
	fmt.Fprintf(a.Stdout, "SHARD  %-*s  %-*s  EXIT  TESTS  FAIL  ERR  SKIP  TIME\n", leaseWidth, "LEASE", runWidth, "RUN")
	for _, result := range results {
		tests, failures, errCount, skipped, elapsed := "-", "-", "-", "-", "-"
		if result.Results != nil {
			tests = strconv.Itoa(result.Results.Tests)
			failures = strconv.Itoa(result.Results.Failures)
			errCount = strconv.Itoa(result.Results.Errors)
			skipped = strconv.Itoa(result.Results.Skipped)
			elapsed = fmt.Sprintf("%.1fs", result.Results.TimeSeconds)
		}
		exitValue := strconv.Itoa(result.ExitCode)
		if result.infraFailed() || result.Canceled {
			exitValue = "-"
		}
		fmt.Fprintf(a.Stdout, "%-5s  %-*s  %-*s  %-4s  %-5s  %-4s  %-3s  %-4s  %s\n", fmt.Sprintf("%d/%d", result.Index, opts.Count), leaseWidth, blank(result.LeaseID, "-"), runWidth, blank(result.RunID, "-"), exitValue, tests, failures, errCount, skipped, elapsed)
	}
}

func shardExitError(opts shardOptions, results []shardResult, merged *TestResultSummary, commandCodes map[int]bool, failedShards, infraFailures, canceledShards int) error {
	commandFailures := 0
	for _, result := range results {
		if result.commandFailed() {
			commandFailures++
		}
	}
	if infraFailures > 0 {
		return exit(7, "%d of %d shards failed to provision or run", infraFailures, opts.Count)
	}
	if commandFailures > 0 {
		code := 1
		if len(commandCodes) == 1 {
			for value := range commandCodes {
				if value > 0 && value < 256 {
					code = value
				}
			}
		}
		return ExitError{Code: code, Message: fmt.Sprintf("%d of %d shards failed", failedShards, opts.Count)}
	}
	if canceledShards > 0 {
		return ExitError{Code: 130, Message: fmt.Sprintf("shard interrupted; %d of %d shards canceled", canceledShards, opts.Count)}
	}
	if opts.FailOnTestFailures && merged != nil && (merged.Failures > 0 || merged.Errors > 0 || len(merged.Failed) > 0) {
		return ExitError{Code: 1, Message: fmt.Sprintf("JUnit results contain %d failures and %d errors", merged.Failures, merged.Errors)}
	}
	return nil
}

type shardTestFailure struct {
	Shard int    `json:"shard"`
	RunID string `json:"runId,omitempty"`
	TestFailure
}

type shardJSONReport struct {
	Checkpoint string             `json:"checkpoint"`
	Count      int                `json:"count"`
	Shards     []shardJSONResult  `json:"shards"`
	Merged     *TestResultSummary `json:"merged"`
	Failed     []shardTestFailure `json:"failed"`
	WallMs     int64              `json:"wallMs"`
	ExitCode   int                `json:"exitCode"`
}

type shardJSONResult struct {
	Index    int                `json:"index"`
	LeaseID  string             `json:"leaseId,omitempty"`
	Slug     string             `json:"slug,omitempty"`
	RunID    string             `json:"runId,omitempty"`
	ExitCode int                `json:"exitCode"`
	Canceled bool               `json:"canceled,omitempty"`
	Error    string             `json:"error,omitempty"`
	Results  *TestResultSummary `json:"results"`
}

func shardJSONResults(results []shardResult) []shardJSONResult {
	out := make([]shardJSONResult, 0, len(results))
	for _, result := range results {
		item := shardJSONResult{Index: result.Index, LeaseID: result.LeaseID, Slug: result.Slug, RunID: result.RunID, ExitCode: result.ExitCode, Canceled: result.Canceled, Results: result.Results}
		if result.Err != nil {
			item.Error = result.Err.Error()
		}
		out = append(out, item)
	}
	return out
}

type shardOutputMux struct {
	mu sync.Mutex
}

func (m *shardOutputMux) printf(out io.Writer, format string, args ...any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fmt.Fprintf(out, format, args...)
}

type prefixLineWriter struct {
	mux    *shardOutputMux
	out    io.Writer
	prefix string
	buf    []byte
}

func (w *prefixLineWriter) Write(p []byte) (int, error) {
	w.mux.mu.Lock()
	defer w.mux.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		idx := -1
		for i, b := range w.buf {
			if b == '\n' {
				idx = i
				break
			}
		}
		if idx < 0 {
			break
		}
		if err := w.emitLocked(w.buf[:idx+1]); err != nil {
			return len(p), err
		}
		w.buf = w.buf[idx+1:]
	}
	if len(w.buf) > shardPartialLineLimit {
		line := append(w.buf, '\n')
		w.buf = nil
		if err := w.emitLocked(line); err != nil {
			return len(p), err
		}
	}
	return len(p), nil
}

func (w *prefixLineWriter) Flush() error {
	w.mux.mu.Lock()
	defer w.mux.mu.Unlock()
	if len(w.buf) == 0 {
		return nil
	}
	line := append(w.buf, '\n')
	w.buf = nil
	return w.emitLocked(line)
}

func (w *prefixLineWriter) emitLocked(line []byte) error {
	if _, err := io.WriteString(w.out, w.prefix); err != nil {
		return err
	}
	_, err := w.out.Write(line)
	return err
}
