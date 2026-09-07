package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	benchmarkTimingSchemaVersion = 1
	benchmarkCheckSchemaVersion  = 1
)

type BenchmarkTimingRecord struct {
	SchemaVersion int                    `json:"schemaVersion"`
	RecordedAt    time.Time              `json:"recordedAt"`
	Source        string                 `json:"source"`
	Benchmark     BenchmarkRecordContext `json:"benchmark"`
	Timing        TimingReport           `json:"timing"`
}

type BenchmarkRecordContext struct {
	CommandFingerprint string `json:"commandFingerprint,omitempty"`
	CommandDisplay     string `json:"commandDisplay,omitempty"`
	RepoFingerprint    string `json:"repoFingerprint,omitempty"`
	RepoName           string `json:"repoName,omitempty"`
	RepoHead           string `json:"repoHead,omitempty"`
	ProviderFamily     string `json:"providerFamily,omitempty"`
	ProviderKind       string `json:"providerKind,omitempty"`
	ProviderCategory   string `json:"providerCategory,omitempty"`
	ColdRun            *bool  `json:"coldRun,omitempty"`
	RepeatIndex        int    `json:"repeatIndex,omitempty"`
}

type benchmarkReport struct {
	SchemaVersion    int                    `json:"schemaVersion"`
	GeneratedAt      time.Time              `json:"generatedAt"`
	StorePath        string                 `json:"storePath"`
	Filters          benchmarkReportFilters `json:"filters"`
	ObservationCount int                    `json:"observationCount"`
	MatchedCount     int                    `json:"matchedCount"`
	Groups           []benchmarkReportGroup `json:"groups"`
	Warnings         []string               `json:"warnings,omitempty"`
}

type benchmarkReportFilters struct {
	Since              string   `json:"since,omitempty"`
	SinceTime          string   `json:"sinceTime,omitempty"`
	Providers          []string `json:"providers,omitempty"`
	CommandFingerprint string   `json:"commandFingerprint,omitempty"`
	MinSamples         int      `json:"minSamples"`
}

type benchmarkReportGroup struct {
	Source               string                        `json:"source"`
	Provider             string                        `json:"provider"`
	ProviderFamily       string                        `json:"providerFamily,omitempty"`
	ProviderKind         string                        `json:"providerKind,omitempty"`
	ProviderCategory     string                        `json:"providerCategory,omitempty"`
	MachineType          string                        `json:"machineType,omitempty"`
	CommandFingerprint   string                        `json:"commandFingerprint,omitempty"`
	ColdRun              *bool                         `json:"coldRun,omitempty"`
	N                    int                           `json:"n"`
	ObservationCount     int                           `json:"observationCount"`
	FailureCount         int                           `json:"failureCount"`
	RunnerTotalN         int                           `json:"runnerTotalN"`
	MedianTotalMs        *int64                        `json:"medianTotalMs,omitempty"`
	P95TotalMs           *int64                        `json:"p95TotalMs,omitempty"`
	MedianRunnerTotalMs  *int64                        `json:"medianRunnerTotalMs,omitempty"`
	P95RunnerTotalMs     *int64                        `json:"p95RunnerTotalMs,omitempty"`
	MedianSyncMs         *int64                        `json:"medianSyncMs,omitempty"`
	MedianCommandMs      *int64                        `json:"medianCommandMs,omitempty"`
	RunnerPhases         []benchmarkRunnerPhaseSummary `json:"runnerPhases,omitempty"`
	SyncPhases           []benchmarkSyncPhaseSummary   `json:"syncPhases,omitempty"`
	SyncSkippedCount     int                           `json:"syncSkippedCount,omitempty"`
	InsufficientEvidence bool                          `json:"insufficientEvidence"`
	Evidence             string                        `json:"evidence"`
}

type benchmarkCheckResult struct {
	SchemaVersion int                   `json:"schemaVersion"`
	Filters       benchmarkCheckFilters `json:"filters"`
	Policy        benchmarkCheckPolicy  `json:"policy"`
	MatchedCount  int                   `json:"matchedCount"`
	GroupCount    int                   `json:"groupCount"`
	Passed        bool                  `json:"passed"`
	Reasons       []string              `json:"reasons"`
	Groups        []benchmarkCheckGroup `json:"groups"`
}

type benchmarkCheckFilters struct {
	Since                 string   `json:"since,omitempty"`
	Providers             []string `json:"providers,omitempty"`
	CommandFingerprintSet bool     `json:"commandFingerprintSet"`
}

type benchmarkCheckPolicy struct {
	MinSamples                 int    `json:"minSamples"`
	RequiredRunnerTotalSamples int    `json:"requiredRunnerTotalSamples"`
	MaxFailures                int    `json:"maxFailures"`
	MaxP95RunnerTotal          string `json:"maxP95RunnerTotal"`
}

type benchmarkCheckGroup struct {
	Source            string   `json:"source"`
	Provider          string   `json:"provider"`
	ProviderFamily    string   `json:"providerFamily,omitempty"`
	ProviderKind      string   `json:"providerKind,omitempty"`
	ProviderCategory  string   `json:"providerCategory,omitempty"`
	MachineType       string   `json:"machineType,omitempty"`
	ColdRun           *bool    `json:"coldRun,omitempty"`
	ObservationCount  int      `json:"observationCount"`
	SuccessfulSamples int      `json:"successfulSamples"`
	FailureCount      int      `json:"failureCount"`
	RunnerTotalN      int      `json:"runnerTotalN"`
	P95RunnerTotalMs  *int64   `json:"p95RunnerTotalMs"`
	Passed            bool     `json:"passed"`
	Reasons           []string `json:"reasons"`
}

type benchmarkRunnerPhaseSummary struct {
	Name     string `json:"name"`
	Opaque   bool   `json:"opaque,omitempty"`
	N        int    `json:"n"`
	MedianMs *int64 `json:"medianMs,omitempty"`
	P95Ms    *int64 `json:"p95Ms,omitempty"`
}

type benchmarkSyncPhaseSummary struct {
	Name         string `json:"name"`
	N            int    `json:"n"`
	MedianMs     *int64 `json:"medianMs,omitempty"`
	P95Ms        *int64 `json:"p95Ms,omitempty"`
	SkippedCount int    `json:"skippedCount,omitempty"`
}

type benchmarkReportOptions struct {
	StorePath          string
	SinceRaw           string
	Since              time.Time
	Providers          []string
	ProviderSet        map[string]bool
	CommandFingerprint string
	MinSamples         int
}

type benchmarkRunExecutor func(context.Context, []string, benchmarkRecordContext) error

type capturedTimingReportWriter struct {
	writer io.Writer
	report *TimingReport
}

func (w *capturedTimingReportWriter) Write(p []byte) (int, error) {
	return w.writer.Write(p)
}

func (w *capturedTimingReportWriter) WriteTimingReport(report TimingReport) error {
	w.report = &report
	return nil
}

func (a App) benchRun(ctx context.Context, args []string) error {
	return a.benchRunWithExecutor(ctx, args, a.runCommandWithBenchmarkRecord)
}

func (a App) benchRunWithExecutor(ctx context.Context, args []string, execute benchmarkRunExecutor) error {
	fs := newFlagSet("bench run", a.Stderr)
	store := fs.String("store", "default", "benchmark JSONL store: default or path")
	providers := fs.String("providers", "", "comma-separated providers to run")
	provider := fs.String("provider", "", "single provider to run")
	repeats := fs.Int("repeats", 1, "number of repeats per provider")
	cold := fs.Bool("cold", false, "mark observations as cold runs")
	warm := fs.Bool("warm", false, "mark observations as warm/reused runs")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if execute == nil {
		return exit(2, "benchmark run executor is not configured")
	}
	command := fs.Args()
	if len(command) > 0 && command[0] == "--" {
		command = command[1:]
	}
	if len(command) == 0 {
		return exit(2, "usage: crabbox bench run --providers a,b [--repeats n] -- <command...>")
	}
	if *repeats < 1 {
		return exit(2, "--repeats must be >= 1")
	}
	if *cold && *warm {
		return exit(2, "--cold and --warm are mutually exclusive")
	}
	storePath, enabled, err := resolveBenchmarkTimingStore(*store)
	if err != nil {
		return err
	}
	if !enabled {
		return exit(2, "--store cannot be off for bench run")
	}
	providerValues := append(splitCommaList(*providers), splitCommaList(*provider)...)
	providerValues = normalizeBenchmarkProviderFilters(providerValues)
	if len(providerValues) == 0 {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		providerValues = normalizeBenchmarkProviderFilters([]string{cfg.Provider})
	}
	if len(providerValues) == 0 {
		return exit(2, "bench run requires --provider, --providers, or a configured default provider")
	}
	var coldRun *bool
	if *cold || *warm {
		value := *cold
		coldRun = &value
	}
	observations := 0
	var failures []string
	for _, providerName := range providerValues {
		for repeat := 1; repeat <= *repeats; repeat++ {
			fmt.Fprintf(a.Stderr, "benchmark run provider=%s repeat=%d/%d store=%s\n", providerName, repeat, *repeats, storePath)
			recordCtx := benchmarkRecordContext{
				Source:      "bench-run",
				RepeatIndex: repeat,
				ColdRun:     coldRun,
				OnRecord: func() {
					observations++
				},
			}
			if err := execute(ctx, benchmarkRunCommandArgs(providerName, storePath, command), recordCtx); err != nil {
				failures = append(failures, fmt.Sprintf("%s repeat=%d: %v", providerName, repeat, err))
				fmt.Fprintf(a.Stderr, "benchmark run failed provider=%s repeat=%d/%d error=%v\n", providerName, repeat, *repeats, err)
			}
		}
	}
	fmt.Fprintf(a.Stderr, "benchmark run completed path=%s observations=%d failures=%d\n", storePath, observations, len(failures))
	if len(failures) > 0 {
		return exit(1, "benchmark run failed: %s", strings.Join(failures, "; "))
	}
	return nil
}

func benchmarkRunCommandArgs(providerName, storePath string, command []string) []string {
	args := []string{"--provider", providerName, "--timing-record", storePath, "--"}
	args = append(args, command...)
	return args
}

func (a App) benchRecord(_ context.Context, args []string) error {
	fs := newFlagSet("bench record", a.Stderr)
	store := fs.String("store", "default", "benchmark JSONL store: default or path")
	timingJSONPath := fs.String("timing-json", "-", "TimingReport JSON input path, or - for stdin")
	source := fs.String("source", "bench-record", "record source label")
	commandDisplay := fs.String("command", "", "command display text for grouping")
	cold := fs.Bool("cold", false, "mark the observation as a cold run")
	warm := fs.Bool("warm", false, "mark the observation as a warm/reused run")
	repeatIndex := fs.Int("repeat-index", 0, "one-based repeat index when known")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	commandArgs := fs.Args()
	if len(commandArgs) > 0 && commandArgs[0] == "--" {
		commandArgs = commandArgs[1:]
	}
	if strings.TrimSpace(*commandDisplay) != "" && len(commandArgs) > 0 {
		return exit(2, "use either --command or command args after --, not both")
	}
	if *cold && *warm {
		return exit(2, "--cold and --warm are mutually exclusive")
	}
	if *repeatIndex < 0 {
		return exit(2, "--repeat-index must be >= 0")
	}
	storePath, enabled, err := resolveBenchmarkTimingStore(*store)
	if err != nil {
		return err
	}
	if !enabled {
		return exit(2, "--store cannot be off for bench record")
	}
	report, err := readTimingReportInput(a.input(), strings.TrimSpace(*timingJSONPath))
	if err != nil {
		return err
	}
	command := benchmarkCommandInput(strings.TrimSpace(*commandDisplay), commandArgs)
	var coldRun *bool
	if *cold || *warm {
		value := *cold
		coldRun = &value
	}
	record := newBenchmarkTimingRecord(time.Now().UTC(), strings.TrimSpace(*source), report, Repo{}, command, coldRun, *repeatIndex)
	if strings.TrimSpace(*commandDisplay) != "" {
		record.Benchmark.CommandDisplay = strings.TrimSpace(*commandDisplay)
	}
	if err := appendBenchmarkTimingRecord(storePath, record); err != nil {
		return err
	}
	fmt.Fprintf(a.Stderr, "benchmark timing record appended path=%s observations=1\n", storePath)
	return nil
}

func (a App) benchReport(_ context.Context, args []string) error {
	fs := newFlagSet("bench report", a.Stderr)
	store := fs.String("store", "default", "benchmark JSONL store: default or path")
	jsonOut := fs.Bool("json", false, "print JSON")
	providers := fs.String("providers", "", "comma-separated providers to include")
	provider := fs.String("provider", "", "single provider to include")
	commandFingerprint := fs.String("command-fingerprint", "", "command fingerprint to include")
	since := fs.String("since", "", "include records since a duration such as 7d or 24h")
	minSamples := fs.Int("min-samples", 2, "minimum successful samples before evidence is considered sufficient")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return exit(2, "usage: crabbox bench report [--store default|path] [--providers a,b] [--command-fingerprint sha256:...] [--since 7d] [--min-samples n] [--json]")
	}
	storePath, enabled, err := resolveBenchmarkTimingStore(*store)
	if err != nil {
		return err
	}
	if !enabled {
		return exit(2, "--store cannot be off for bench report")
	}
	if *minSamples < 1 {
		return exit(2, "--min-samples must be >= 1")
	}
	now := time.Now().UTC()
	sinceTime, sinceLabel, err := parseBenchmarkSince(*since, now)
	if err != nil {
		return err
	}
	providerValues := append(splitCommaList(*providers), splitCommaList(*provider)...)
	providerValues = normalizeBenchmarkProviderFilters(providerValues)
	opts := benchmarkReportOptions{
		StorePath:          storePath,
		SinceRaw:           sinceLabel,
		Since:              sinceTime,
		Providers:          providerValues,
		ProviderSet:        stringSet(providerValues),
		CommandFingerprint: strings.TrimSpace(*commandFingerprint),
		MinSamples:         *minSamples,
	}
	records, err := readBenchmarkTimingRecords(storePath)
	if err != nil {
		return err
	}
	report := buildBenchmarkReport(records, opts, now)
	if *jsonOut {
		encoder := json.NewEncoder(a.Stdout)
		encoder.SetEscapeHTML(false)
		return encoder.Encode(report)
	}
	printBenchmarkReport(a.Stdout, report)
	return nil
}

func (a App) benchCheck(_ context.Context, args []string) error {
	fs := newFlagSet("bench check", a.Stderr)
	store := fs.String("store", "default", "benchmark JSONL store: default or path")
	jsonOut := fs.Bool("json", false, "print JSON")
	providers := fs.String("providers", "", "comma-separated providers to include")
	provider := fs.String("provider", "", "single provider to include")
	commandFingerprint := fs.String("command-fingerprint", "", "command fingerprint to include")
	since := fs.String("since", "", "include records since a duration such as 7d or 24h")
	minSamples := fs.Int("min-samples", 3, "minimum successful samples required in every matched group")
	maxFailures := fs.Int("max-failures", 0, "maximum failed observations allowed in every matched group")
	maxP95RunnerTotalRaw := fs.String("max-p95-runner-total", "", "required maximum p95 runner total duration")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return exit(2, "usage: crabbox bench check [--store default|path] [--providers a,b] [--command-fingerprint sha256:...] [--since 7d] [--min-samples n] [--max-failures n] --max-p95-runner-total 5s [--json]")
	}
	storePath, enabled, err := resolveBenchmarkTimingStore(*store)
	if err != nil {
		return err
	}
	if !enabled {
		return exit(2, "--store cannot be off for bench check")
	}
	if *minSamples < 1 {
		return exit(2, "--min-samples must be >= 1")
	}
	if *maxFailures < 0 {
		return exit(2, "--max-failures must be >= 0")
	}
	maxP95RunnerTotalValue := strings.TrimSpace(*maxP95RunnerTotalRaw)
	if maxP95RunnerTotalValue == "" {
		return exit(2, "--max-p95-runner-total is required")
	}
	maxP95RunnerTotal, err := time.ParseDuration(maxP95RunnerTotalValue)
	if err != nil {
		return exit(2, "--max-p95-runner-total must be a positive duration such as 5s")
	}
	if maxP95RunnerTotal <= 0 {
		return exit(2, "--max-p95-runner-total must be greater than zero")
	}

	now := time.Now().UTC()
	sinceTime, sinceLabel, err := parseBenchmarkSince(*since, now)
	if err != nil {
		return err
	}
	providerValues := append(splitCommaList(*providers), splitCommaList(*provider)...)
	providerValues = normalizeBenchmarkProviderFilters(providerValues)
	commandFingerprintValue := strings.TrimSpace(*commandFingerprint)
	opts := benchmarkReportOptions{
		StorePath:          storePath,
		SinceRaw:           sinceLabel,
		Since:              sinceTime,
		Providers:          providerValues,
		ProviderSet:        stringSet(providerValues),
		CommandFingerprint: commandFingerprintValue,
		MinSamples:         *minSamples,
	}
	records, err := readBenchmarkTimingRecords(storePath)
	if err != nil {
		return err
	}
	report := buildBenchmarkReport(records, opts, now)
	result := evaluateBenchmarkCheck(report, benchmarkCheckPolicy{
		MinSamples:                 *minSamples,
		RequiredRunnerTotalSamples: max(*minSamples, 3),
		MaxFailures:                *maxFailures,
		MaxP95RunnerTotal:          maxP95RunnerTotal.String(),
	}, maxP95RunnerTotal, commandFingerprintValue != "")
	if *jsonOut {
		encoder := json.NewEncoder(a.Stdout)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(result); err != nil {
			return err
		}
	} else {
		printBenchmarkCheck(a.Stdout, result)
	}
	if !result.Passed {
		return exit(1, "benchmark check failed")
	}
	return nil
}

func evaluateBenchmarkCheck(report benchmarkReport, policy benchmarkCheckPolicy, maxP95RunnerTotal time.Duration, commandFingerprintSet bool) benchmarkCheckResult {
	result := benchmarkCheckResult{
		SchemaVersion: benchmarkCheckSchemaVersion,
		Filters: benchmarkCheckFilters{
			Since:                 report.Filters.Since,
			Providers:             append([]string(nil), report.Filters.Providers...),
			CommandFingerprintSet: commandFingerprintSet,
		},
		Policy:       policy,
		MatchedCount: report.MatchedCount,
		GroupCount:   len(report.Groups),
		Passed:       true,
		Reasons:      []string{},
		Groups:       make([]benchmarkCheckGroup, 0, len(report.Groups)),
	}
	if report.MatchedCount == 0 {
		result.Passed = false
		result.Reasons = append(result.Reasons, "no_matching_observations")
	}
	maxP95RunnerTotalMs := maxP95RunnerTotal.Milliseconds()
	for _, reportGroup := range report.Groups {
		group := benchmarkCheckGroup{
			Source:            reportGroup.Source,
			Provider:          reportGroup.Provider,
			ProviderFamily:    reportGroup.ProviderFamily,
			ProviderKind:      reportGroup.ProviderKind,
			ProviderCategory:  reportGroup.ProviderCategory,
			MachineType:       reportGroup.MachineType,
			ColdRun:           cloneBoolPtr(reportGroup.ColdRun),
			ObservationCount:  reportGroup.ObservationCount,
			SuccessfulSamples: reportGroup.N,
			FailureCount:      reportGroup.FailureCount,
			RunnerTotalN:      reportGroup.RunnerTotalN,
			P95RunnerTotalMs:  reportGroup.P95RunnerTotalMs,
			Passed:            true,
			Reasons:           []string{},
		}
		if reportGroup.N < policy.MinSamples {
			group.Reasons = append(group.Reasons, "insufficient_successful_samples")
		}
		if reportGroup.FailureCount > policy.MaxFailures {
			group.Reasons = append(group.Reasons, "max_failures_exceeded")
		}
		if reportGroup.RunnerTotalN < policy.RequiredRunnerTotalSamples {
			group.Reasons = append(group.Reasons, "insufficient_runner_total_samples")
		}
		if reportGroup.P95RunnerTotalMs == nil {
			group.Reasons = append(group.Reasons, "missing_p95_runner_total")
		} else if *reportGroup.P95RunnerTotalMs > maxP95RunnerTotalMs {
			group.Reasons = append(group.Reasons, "p95_runner_total_exceeded")
		}
		group.Passed = len(group.Reasons) == 0
		result.Passed = result.Passed && group.Passed
		result.Groups = append(result.Groups, group)
	}
	return result
}

func printBenchmarkCheck(w io.Writer, result benchmarkCheckResult) {
	fmt.Fprintf(w, "benchmark check matched=%d groups=%d min_samples=%d runner_total_samples=%d max_failures=%d max_p95_runner_total=%s passed=%t\n",
		result.MatchedCount,
		result.GroupCount,
		result.Policy.MinSamples,
		result.Policy.RequiredRunnerTotalSamples,
		result.Policy.MaxFailures,
		result.Policy.MaxP95RunnerTotal,
		result.Passed,
	)
	for _, reason := range result.Reasons {
		fmt.Fprintf(w, "reason=%s\n", reason)
	}
	for _, group := range result.Groups {
		fmt.Fprintf(w, "%s source=%s family=%s kind=%s machine=%s cold=%s observations=%d successful=%d failures=%d runner_total_n=%d p95_runner_total=%s passed=%t reasons=%s\n",
			group.Provider,
			group.Source,
			blank(group.ProviderFamily, "-"),
			blank(group.ProviderKind, "-"),
			blank(group.MachineType, "-"),
			benchmarkColdDisplay(group.ColdRun),
			group.ObservationCount,
			group.SuccessfulSamples,
			group.FailureCount,
			group.RunnerTotalN,
			formatBenchmarkMs(group.P95RunnerTotalMs),
			group.Passed,
			firstNonBlank(strings.Join(group.Reasons, ","), "-"),
		)
	}
}

func readTimingReportInput(stdin io.Reader, path string) (TimingReport, error) {
	var reader io.Reader
	var file *os.File
	switch strings.TrimSpace(path) {
	case "", "-":
		reader = stdin
	default:
		f, err := os.Open(path)
		if err != nil {
			return TimingReport{}, exit(2, "read timing JSON %s: %v", path, err)
		}
		file = f
		reader = f
	}
	if file != nil {
		defer file.Close()
	}
	var report TimingReport
	if err := json.NewDecoder(reader).Decode(&report); err != nil {
		return TimingReport{}, exit(2, "decode timing JSON: %v", err)
	}
	if strings.TrimSpace(report.Provider) == "" {
		return TimingReport{}, exit(2, "timing JSON is missing provider")
	}
	return report, nil
}

func resolveBenchmarkTimingStore(value string) (string, bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "off":
		return "", false, nil
	case "default":
		dir, err := crabboxStateDir()
		if err != nil {
			return "", false, err
		}
		return filepath.Join(dir, "timings.jsonl"), true, nil
	default:
		return value, true, nil
	}
}

func appendBenchmarkTimingRecord(path string, record BenchmarkTimingRecord) error {
	if strings.TrimSpace(path) == "" {
		return exit(2, "benchmark timing store path is empty")
	}
	if record.SchemaVersion == 0 {
		record.SchemaVersion = benchmarkTimingSchemaVersion
	}
	parent := filepath.Dir(path)
	if parent != "." && parent != "" {
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return exit(2, "create benchmark timing store directory %s: %v", parent, err)
		}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return exit(2, "open benchmark timing store %s: %v", path, err)
	}
	encoder := json.NewEncoder(file)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(record); err != nil {
		_ = file.Close()
		return exit(2, "write benchmark timing record %s: %v", path, err)
	}
	if err := file.Close(); err != nil {
		return exit(2, "close benchmark timing store %s: %v", path, err)
	}
	return nil
}

func readBenchmarkTimingRecords(path string) ([]BenchmarkTimingRecord, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, exit(2, "read benchmark timing store %s: %v", path, err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	var records []BenchmarkTimingRecord
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var record BenchmarkTimingRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			return nil, exit(2, "decode benchmark timing store %s line %d: %v", path, lineNo, err)
		}
		if record.SchemaVersion != benchmarkTimingSchemaVersion {
			return nil, exit(2, "decode benchmark timing store %s line %d: unsupported schemaVersion=%d", path, lineNo, record.SchemaVersion)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, exit(2, "read benchmark timing store %s: %v", path, err)
	}
	return records, nil
}

func newBenchmarkTimingRecord(now time.Time, source string, report TimingReport, repo Repo, command []string, coldRun *bool, repeatIndex int) BenchmarkTimingRecord {
	family, kind, category := benchmarkProviderInfo(report.Provider)
	commandDisplay := strings.Join(readableShellWords(command), " ")
	return BenchmarkTimingRecord{
		SchemaVersion: benchmarkTimingSchemaVersion,
		RecordedAt:    now.UTC(),
		Source:        firstNonBlank(strings.TrimSpace(source), "run"),
		Benchmark: BenchmarkRecordContext{
			CommandFingerprint: benchmarkCommandFingerprint(command),
			CommandDisplay:     commandDisplay,
			RepoFingerprint:    benchmarkRepoFingerprint(repo),
			RepoName:           repo.Name,
			RepoHead:           repo.Head,
			ProviderFamily:     family,
			ProviderKind:       kind,
			ProviderCategory:   category,
			ColdRun:            coldRun,
			RepeatIndex:        repeatIndex,
		},
		Timing: report,
	}
}

func benchmarkCommandInput(commandDisplay string, commandArgs []string) []string {
	if len(commandArgs) > 0 {
		return commandArgs
	}
	if commandDisplay == "" {
		return nil
	}
	return []string{commandDisplay}
}

func benchmarkCommandFingerprint(command []string) string {
	if len(command) == 0 {
		return ""
	}
	body, err := json.Marshal(command)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func benchmarkRepoFingerprint(repo Repo) string {
	if repo.Name == "" && repo.RemoteURL == "" && repo.Head == "" {
		return ""
	}
	h := sha256.New()
	fmt.Fprintf(h, "v1\nname=%s\nremote=%s\nhead=%s\n", repo.Name, repo.RemoteURL, repo.Head)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func benchmarkProviderInfo(provider string) (string, string, string) {
	normalized := normalizeProviderName(provider)
	for _, entry := range providerMatrix() {
		if normalizeProviderName(entry.Provider) == normalized {
			return entry.Family, string(entry.Kind), benchmarkProviderCategories[entry.Provider]
		}
		for _, alias := range entry.Aliases {
			if normalizeProviderName(alias) == normalized {
				return entry.Family, string(entry.Kind), benchmarkProviderCategories[entry.Provider]
			}
		}
	}
	return "", "", ""
}

func buildBenchmarkReport(records []BenchmarkTimingRecord, opts benchmarkReportOptions, now time.Time) benchmarkReport {
	groups := map[string]*benchmarkReportGroupBuilder{}
	matched := 0
	for _, record := range records {
		if !benchmarkRecordMatches(record, opts) {
			continue
		}
		matched++
		key := benchmarkGroupKeyForRecord(record)
		builder := groups[key]
		if builder == nil {
			group := benchmarkReportGroup{
				Source:             benchmarkRecordSource(record.Source),
				Provider:           record.Timing.Provider,
				ProviderFamily:     record.Benchmark.ProviderFamily,
				ProviderKind:       record.Benchmark.ProviderKind,
				ProviderCategory:   record.Benchmark.ProviderCategory,
				MachineType:        record.Timing.MachineType,
				CommandFingerprint: record.Benchmark.CommandFingerprint,
				ColdRun:            cloneBoolPtr(record.Benchmark.ColdRun),
			}
			if group.ProviderFamily == "" || group.ProviderKind == "" || group.ProviderCategory == "" {
				family, kind, category := benchmarkProviderInfo(group.Provider)
				if group.ProviderFamily == "" {
					group.ProviderFamily = family
				}
				if group.ProviderKind == "" {
					group.ProviderKind = kind
				}
				if group.ProviderCategory == "" {
					group.ProviderCategory = category
				}
			}
			builder = &benchmarkReportGroupBuilder{group: group}
			groups[key] = builder
		}
		builder.add(record)
	}
	out := benchmarkReport{
		SchemaVersion:    benchmarkTimingSchemaVersion,
		GeneratedAt:      now.UTC(),
		StorePath:        opts.StorePath,
		Filters:          benchmarkReportFilters{Since: opts.SinceRaw, Providers: opts.Providers, CommandFingerprint: opts.CommandFingerprint, MinSamples: opts.MinSamples},
		ObservationCount: len(records),
		MatchedCount:     matched,
	}
	if !opts.Since.IsZero() {
		out.Filters.SinceTime = opts.Since.UTC().Format(time.RFC3339)
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		group := groups[key].finish(opts.MinSamples)
		if group.InsufficientEvidence {
			out.Warnings = append(out.Warnings, fmt.Sprintf("insufficient evidence for provider=%s command=%s n=%d", group.Provider, blank(group.CommandFingerprint, "-"), group.N))
		}
		out.Groups = append(out.Groups, group)
	}
	if len(records) == 0 {
		out.Warnings = append(out.Warnings, "no benchmark timing records found")
	} else if matched == 0 {
		out.Warnings = append(out.Warnings, "no benchmark timing records matched the filters")
	}
	return out
}

type benchmarkReportGroupBuilder struct {
	group         benchmarkReportGroup
	totalMs       []int64
	runnerTotalMs []int64
	syncMs        []int64
	commandMs     []int64
	runnerPhases  map[benchmarkRunnerPhaseKey][]int64
	syncPhases    map[string]*benchmarkSyncPhaseBuilder
}

type benchmarkRunnerPhaseKey struct {
	Name   string
	Opaque bool
}

type benchmarkSyncPhaseBuilder struct {
	values       []int64
	skippedCount int
}

func (b *benchmarkReportGroupBuilder) add(record BenchmarkTimingRecord) {
	b.group.ObservationCount++
	if record.Timing.ExitCode != 0 {
		b.group.FailureCount++
		return
	}
	b.group.N++
	b.totalMs = append(b.totalMs, record.Timing.TotalMs)
	if record.Timing.RunnerTotalMs > 0 {
		b.runnerTotalMs = append(b.runnerTotalMs, record.Timing.RunnerTotalMs)
	}
	b.syncMs = append(b.syncMs, record.Timing.SyncMs)
	b.commandMs = append(b.commandMs, record.Timing.CommandMs)
	if record.Timing.SyncSkipped {
		b.group.SyncSkippedCount++
	}
	b.addRunnerPhases(record.Timing.RunnerPhases)
	b.addSyncPhases(record.Timing.SyncPhases)
}

func (b *benchmarkReportGroupBuilder) addRunnerPhases(phases []RunnerPhase) {
	perObservation := map[benchmarkRunnerPhaseKey]int64{}
	for _, phase := range phases {
		name := strings.TrimSpace(phase.Name)
		if name == "" || phase.Ms <= 0 {
			continue
		}
		perObservation[benchmarkRunnerPhaseKey{Name: name, Opaque: phase.Opaque}] += phase.Ms
	}
	if len(perObservation) == 0 {
		return
	}
	if b.runnerPhases == nil {
		b.runnerPhases = map[benchmarkRunnerPhaseKey][]int64{}
	}
	for key, value := range perObservation {
		b.runnerPhases[key] = append(b.runnerPhases[key], value)
	}
}

func (b *benchmarkReportGroupBuilder) addSyncPhases(phases []TimingPhase) {
	type observationPhase struct {
		ms       int64
		measured bool
		skipped  bool
	}
	perObservation := map[string]observationPhase{}
	for _, phase := range phases {
		name := strings.TrimSpace(phase.Name)
		if name == "" {
			continue
		}
		value := perObservation[name]
		if !phase.Skipped && phase.Ms >= 0 {
			value.ms += phase.Ms
			value.measured = true
		}
		value.skipped = value.skipped || phase.Skipped
		perObservation[name] = value
	}
	if len(perObservation) == 0 {
		return
	}
	if b.syncPhases == nil {
		b.syncPhases = map[string]*benchmarkSyncPhaseBuilder{}
	}
	for name, value := range perObservation {
		phase := b.syncPhases[name]
		if phase == nil {
			phase = &benchmarkSyncPhaseBuilder{}
			b.syncPhases[name] = phase
		}
		if value.measured {
			phase.values = append(phase.values, value.ms)
		}
		if value.skipped {
			phase.skippedCount++
		}
	}
}

func (b *benchmarkReportGroupBuilder) finish(minSamples int) benchmarkReportGroup {
	group := b.group
	group.RunnerTotalN = len(b.runnerTotalMs)
	group.MedianTotalMs = medianInt64(b.totalMs)
	group.MedianRunnerTotalMs = medianInt64(b.runnerTotalMs)
	group.MedianSyncMs = medianInt64(b.syncMs)
	group.MedianCommandMs = medianInt64(b.commandMs)
	if group.N >= 3 {
		group.P95TotalMs = percentileNearestRankInt64(b.totalMs, 0.95)
	}
	if len(b.runnerTotalMs) >= 3 {
		group.P95RunnerTotalMs = percentileNearestRankInt64(b.runnerTotalMs, 0.95)
	}
	group.RunnerPhases = finishBenchmarkRunnerPhases(b.runnerPhases)
	group.SyncPhases = finishBenchmarkSyncPhases(b.syncPhases)
	group.InsufficientEvidence = group.N < minSamples
	if group.N == 0 {
		group.Evidence = fmt.Sprintf("insufficient_successful_samples need=%d", minSamples)
	} else if group.InsufficientEvidence {
		group.Evidence = fmt.Sprintf("insufficient_successful_samples n=%d need=%d", group.N, minSamples)
	} else {
		group.Evidence = "sufficient_local_samples"
	}
	return group
}

func finishBenchmarkRunnerPhases(phases map[benchmarkRunnerPhaseKey][]int64) []benchmarkRunnerPhaseSummary {
	keys := make([]benchmarkRunnerPhaseKey, 0, len(phases))
	for key := range phases {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Name != keys[j].Name {
			return keys[i].Name < keys[j].Name
		}
		return !keys[i].Opaque && keys[j].Opaque
	})
	out := make([]benchmarkRunnerPhaseSummary, 0, len(keys))
	for _, key := range keys {
		values := phases[key]
		summary := benchmarkRunnerPhaseSummary{
			Name:     key.Name,
			Opaque:   key.Opaque,
			N:        len(values),
			MedianMs: medianInt64(values),
		}
		if len(values) >= 3 {
			summary.P95Ms = percentileNearestRankInt64(values, 0.95)
		}
		out = append(out, summary)
	}
	return out
}

func finishBenchmarkSyncPhases(phases map[string]*benchmarkSyncPhaseBuilder) []benchmarkSyncPhaseSummary {
	names := make([]string, 0, len(phases))
	for name := range phases {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]benchmarkSyncPhaseSummary, 0, len(names))
	for _, name := range names {
		phase := phases[name]
		summary := benchmarkSyncPhaseSummary{
			Name:         name,
			N:            len(phase.values),
			MedianMs:     medianInt64(phase.values),
			SkippedCount: phase.skippedCount,
		}
		if len(phase.values) >= 3 {
			summary.P95Ms = percentileNearestRankInt64(phase.values, 0.95)
		}
		out = append(out, summary)
	}
	return out
}

func benchmarkRecordMatches(record BenchmarkTimingRecord, opts benchmarkReportOptions) bool {
	if !opts.Since.IsZero() && record.RecordedAt.Before(opts.Since) {
		return false
	}
	if opts.CommandFingerprint != "" && record.Benchmark.CommandFingerprint != opts.CommandFingerprint {
		return false
	}
	if len(opts.ProviderSet) > 0 && !opts.ProviderSet[normalizeProviderName(record.Timing.Provider)] {
		return false
	}
	return true
}

func benchmarkGroupKeyForRecord(record BenchmarkTimingRecord) string {
	cold := "unknown"
	if record.Benchmark.ColdRun != nil {
		cold = strconv.FormatBool(*record.Benchmark.ColdRun)
	}
	parts := []string{
		benchmarkRecordSource(record.Source),
		normalizeProviderName(record.Timing.Provider),
		record.Benchmark.ProviderFamily,
		record.Benchmark.ProviderCategory,
		record.Benchmark.ProviderKind,
		record.Timing.MachineType,
		record.Benchmark.CommandFingerprint,
		cold,
	}
	return strings.Join(parts, "\x00")
}

func benchmarkRecordSource(source string) string {
	return firstNonBlank(strings.TrimSpace(source), "unknown")
}

func printBenchmarkReport(w io.Writer, report benchmarkReport) {
	if report.ObservationCount == 0 {
		fmt.Fprintf(w, "no benchmark timing records found store=%s\n", report.StorePath)
		return
	}
	if report.MatchedCount == 0 {
		fmt.Fprintf(w, "no matching benchmark timing records store=%s observations=%d\n", report.StorePath, report.ObservationCount)
		return
	}
	fmt.Fprintf(w, "benchmark report store=%s observations=%d matched=%d min_samples=%d\n", report.StorePath, report.ObservationCount, report.MatchedCount, report.Filters.MinSamples)
	for _, group := range report.Groups {
		fmt.Fprintf(w, "%s source=%s family=%s kind=%s machine=%s command=%s cold=%s n=%d median_total=%s p95_total=%s runner_total_n=%d median_runner_total=%s p95_runner_total=%s median_sync=%s median_command=%s sync_skipped=%d failures=%d evidence=%s\n",
			group.Provider,
			group.Source,
			blank(group.ProviderFamily, "-"),
			blank(group.ProviderKind, "-"),
			blank(group.MachineType, "-"),
			blank(shortBenchmarkFingerprint(group.CommandFingerprint), "-"),
			benchmarkColdDisplay(group.ColdRun),
			group.N,
			formatBenchmarkMs(group.MedianTotalMs),
			formatBenchmarkMs(group.P95TotalMs),
			group.RunnerTotalN,
			formatBenchmarkMs(group.MedianRunnerTotalMs),
			formatBenchmarkMs(group.P95RunnerTotalMs),
			formatBenchmarkMs(group.MedianSyncMs),
			formatBenchmarkMs(group.MedianCommandMs),
			group.SyncSkippedCount,
			group.FailureCount,
			group.Evidence,
		)
		for _, phase := range group.RunnerPhases {
			fmt.Fprintf(w, "  runner_phase name=%s opaque=%t n=%d median=%s p95=%s\n",
				phase.Name, phase.Opaque, phase.N, formatBenchmarkMs(phase.MedianMs), formatBenchmarkMs(phase.P95Ms))
		}
		for _, phase := range group.SyncPhases {
			fmt.Fprintf(w, "  sync_phase name=%s n=%d median=%s p95=%s skipped=%d\n",
				phase.Name, phase.N, formatBenchmarkMs(phase.MedianMs), formatBenchmarkMs(phase.P95Ms), phase.SkippedCount)
		}
	}
}

func parseBenchmarkSince(value string, now time.Time) (time.Time, string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, "", nil
	}
	if strings.HasSuffix(value, "d") {
		daysValue := strings.TrimSuffix(value, "d")
		days, err := strconv.Atoi(daysValue)
		if err != nil || days < 0 {
			return time.Time{}, "", exit(2, "--since day duration must look like 7d")
		}
		return now.Add(-time.Duration(days) * 24 * time.Hour), value, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return time.Time{}, "", exit(2, "--since must be a duration such as 7d or 24h")
	}
	if duration < 0 {
		return time.Time{}, "", exit(2, "--since must not be negative")
	}
	return now.Add(-duration), value, nil
}

func normalizeBenchmarkProviderFilters(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		normalized := normalizeProviderName(value)
		if normalized == "" || seen[normalized] {
			continue
		}
		seen[normalized] = true
		out = append(out, normalized)
	}
	sort.Strings(out)
	return out
}

func stringSet(values []string) map[string]bool {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]bool, len(values))
	for _, value := range values {
		out[value] = true
	}
	return out
}

func medianInt64(values []int64) *int64 {
	if len(values) == 0 {
		return nil
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	mid := len(sorted) / 2
	var value int64
	if len(sorted)%2 == 0 {
		value = (sorted[mid-1] + sorted[mid]) / 2
	} else {
		value = sorted[mid]
	}
	return &value
}

func percentileNearestRankInt64(values []int64, percentile float64) *int64 {
	if len(values) == 0 {
		return nil
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	rank := int(math.Ceil(percentile*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	value := sorted[rank]
	return &value
}

func cloneBoolPtr(value *bool) *bool {
	if value == nil {
		return nil
	}
	out := *value
	return &out
}

func formatBenchmarkMs(value *int64) string {
	if value == nil {
		return "-"
	}
	return (time.Duration(*value) * time.Millisecond).String()
}

func benchmarkColdDisplay(value *bool) string {
	if value == nil {
		return "unknown"
	}
	return strconv.FormatBool(*value)
}

func shortBenchmarkFingerprint(value string) string {
	if strings.HasPrefix(value, "sha256:") && len(value) > len("sha256:")+12 {
		return value[:len("sha256:")+12]
	}
	return value
}

func timingReportFromDelegatedRunResult(req RunRequest, result RunResult, provider string, runErr error) timingReport {
	exitCode := result.ExitCode
	if exitCode == 0 && runErr != nil {
		var exitErr ExitError
		if AsExitError(runErr, &exitErr) {
			exitCode = exitErr.Code
		} else {
			exitCode = 1
		}
	}
	report := timingReport{
		Provider:      firstNonBlank(result.Provider, provider),
		LeaseID:       result.LeaseID,
		Slug:          result.Slug,
		RunID:         delegatedRunID(req, result),
		SyncDelegated: result.SyncDelegated,
		CommandMs:     result.Command.Milliseconds(),
		TotalMs:       result.Total.Milliseconds(),
		ExitCode:      exitCode,
		ActionsRunURL: result.ActionsURL,
		Label:         req.Label,
		Artifacts:     result.Artifacts,
	}
	return TimingReportWithRunResult(report, result, runErr)
}
