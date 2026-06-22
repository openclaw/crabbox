package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func init() {
	RegisterProvider(benchmarkTimingTestProvider{})
}

type benchmarkTimingTestProvider struct{}

func (benchmarkTimingTestProvider) Name() string      { return "benchmark-timing-test" }
func (benchmarkTimingTestProvider) Aliases() []string { return nil }
func (benchmarkTimingTestProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        "benchmark-timing-test",
		Family:      "benchmark-timing-test",
		Kind:        ProviderKindDelegatedRun,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Coordinator: CoordinatorNever,
	}
}
func (benchmarkTimingTestProvider) RegisterFlags(*flag.FlagSet, Config) any { return nil }
func (benchmarkTimingTestProvider) ApplyFlags(*Config, *flag.FlagSet, any) error {
	return nil
}
func (p benchmarkTimingTestProvider) Configure(_ Config, rt Runtime) (Backend, error) {
	return benchmarkTimingTestBackend{spec: p.Spec(), stderr: rt.Stderr}, nil
}

type benchmarkTimingTestBackend struct {
	spec   ProviderSpec
	stderr io.Writer
}

func (b benchmarkTimingTestBackend) Spec() ProviderSpec { return b.spec }
func (b benchmarkTimingTestBackend) Warmup(context.Context, WarmupRequest) error {
	return nil
}
func (b benchmarkTimingTestBackend) Run(_ context.Context, req RunRequest) (RunResult, error) {
	result := RunResult{
		Provider:      b.spec.Name,
		LeaseID:       "bench_test",
		Slug:          "benchmark-timing-test",
		SyncDelegated: true,
		Command:       250 * time.Millisecond,
		Total:         time.Second,
	}
	if req.TimingJSON {
		report := timingReportFromDelegatedRunResult(req, result, b.spec.Name, nil)
		report.SyncMs = 400
		report.SyncPhases = []TimingPhase{{Name: "archive", Ms: 400}}
		report.MachineType = "test-medium"
		if err := writeTimingJSON(b.stderr, report); err != nil {
			return RunResult{}, err
		}
	}
	return result, nil
}
func (b benchmarkTimingTestBackend) List(context.Context, ListRequest) ([]LeaseView, error) {
	return nil, nil
}
func (b benchmarkTimingTestBackend) Status(context.Context, StatusRequest) (StatusView, error) {
	return StatusView{}, nil
}
func (b benchmarkTimingTestBackend) Stop(context.Context, StopRequest) error { return nil }

func TestTimingReportFromDelegatedRunResultClassifiesRunError(t *testing.T) {
	report := timingReportFromDelegatedRunResult(RunRequest{}, RunResult{
		Provider:      "sandbox-test",
		SyncDelegated: true,
		Command:       250 * time.Millisecond,
		Total:         time.Second,
	}, "fallback", context.DeadlineExceeded)
	if report.ExitCode != 1 {
		t.Fatalf("ExitCode=%d, want 1", report.ExitCode)
	}
	if report.RunStatus != RunStatusTimedOut || report.ErrorKind != RunErrorTimeout {
		t.Fatalf("runStatus/errorKind=%q/%q", report.RunStatus, report.ErrorKind)
	}
}

func TestBenchRecordAppendsTimingJSONRecord(t *testing.T) {
	dir := t.TempDir()
	inputPath := filepath.Join(dir, "timing.json")
	storePath := filepath.Join(dir, "timings.jsonl")
	report := TimingReport{
		Provider:    "aws",
		LeaseID:     "cbx_123",
		SyncMs:      100,
		CommandMs:   900,
		TotalMs:     1100,
		ExitCode:    0,
		MachineType: "c7a.large",
	}
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inputPath, body, 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	err = app.benchRecord(context.Background(), []string{"--store", storePath, "--timing-json", inputPath, "--command", "pnpm test", "--cold", "--repeat-index", "1"})
	if err != nil {
		t.Fatalf("bench record error=%v stderr=%q", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "benchmark timing record appended") {
		t.Fatalf("bench record did not print append destination: %q", stderr.String())
	}

	records, err := readBenchmarkTimingRecords(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("records=%d want 1", len(records))
	}
	record := records[0]
	if record.SchemaVersion != benchmarkTimingSchemaVersion {
		t.Fatalf("schemaVersion=%d", record.SchemaVersion)
	}
	if record.Timing.Provider != "aws" || record.Timing.TotalMs != 1100 {
		t.Fatalf("timing=%#v", record.Timing)
	}
	if record.Benchmark.CommandDisplay != "pnpm test" {
		t.Fatalf("commandDisplay=%q", record.Benchmark.CommandDisplay)
	}
	if record.Benchmark.CommandFingerprint == "" {
		t.Fatal("command fingerprint was empty")
	}
	if record.Benchmark.ColdRun == nil || !*record.Benchmark.ColdRun {
		t.Fatalf("coldRun=%v", record.Benchmark.ColdRun)
	}
	if record.Benchmark.RepeatIndex != 1 {
		t.Fatalf("repeatIndex=%d", record.Benchmark.RepeatIndex)
	}
	if record.Benchmark.ProviderCategory != "brokerable-cloud" {
		t.Fatalf("providerCategory=%q", record.Benchmark.ProviderCategory)
	}
}

func TestRunDelegatedTimingJSONEmittedOnceWhileRecording(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "timings.jsonl")
	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	err := app.runCommand(context.Background(), []string{
		"--provider", "benchmark-timing-test",
		"--timing-json",
		"--timing-record", storePath,
		"--", "true",
	})
	if err != nil {
		t.Fatalf("run error=%v stderr=%q", err, stderr.String())
	}
	if count := strings.Count(stderr.String(), `"provider":"benchmark-timing-test"`); count != 1 {
		t.Fatalf("delegated timing JSON count=%d want 1; stderr=%q", count, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	var emitted TimingReport
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &emitted); err != nil || emitted.Provider != "benchmark-timing-test" {
		t.Fatalf("final stderr line is not delegated timing JSON: line=%q error=%v", lines[len(lines)-1], err)
	}
	records, err := readBenchmarkTimingRecords(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Timing.Provider != "benchmark-timing-test" {
		t.Fatalf("records=%#v, want one delegated timing record", records)
	}
	if timing := records[0].Timing; timing.SyncMs != 400 || len(timing.SyncPhases) != 1 || timing.SyncPhases[0].Name != "archive" || timing.MachineType != "test-medium" {
		t.Fatalf("delegated timing metadata=%#v", timing)
	}
	if records[0].Timing.RunStatus != RunStatusSucceeded {
		t.Fatalf("delegated timing runStatus=%q", records[0].Timing.RunStatus)
	}
}

func TestRunDelegatedTimingRecordDoesNotPrintTimingJSON(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "timings.jsonl")
	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	err := app.runCommand(context.Background(), []string{
		"--provider", "benchmark-timing-test",
		"--timing-record", storePath,
		"--", "true",
	})
	if err != nil {
		t.Fatalf("run error=%v stderr=%q", err, stderr.String())
	}
	if strings.Contains(stderr.String(), `"provider":"benchmark-timing-test"`) {
		t.Fatalf("timing JSON leaked without --timing-json: %q", stderr.String())
	}
	records, err := readBenchmarkTimingRecords(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Timing.SyncMs != 400 {
		t.Fatalf("records=%#v, want one complete delegated timing record", records)
	}
}

func TestBenchRunFansOutProvidersAndRepeats(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "timings.jsonl")
	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	type call struct {
		args   []string
		record benchmarkRecordContext
	}
	var calls []call
	err := app.benchRunWithExecutor(context.Background(), []string{"--store", storePath, "--providers", "hetzner,aws", "--repeats", "2", "--cold", "--", "go", "test", "./..."}, func(_ context.Context, args []string, record benchmarkRecordContext) error {
		copiedArgs := append([]string(nil), args...)
		calls = append(calls, call{args: copiedArgs, record: record})
		if record.OnRecord != nil {
			record.OnRecord()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("bench run error=%v stderr=%q", err, stderr.String())
	}
	if len(calls) != 4 {
		t.Fatalf("calls=%d want 4", len(calls))
	}
	wantArgs := [][]string{
		{"--provider", "aws", "--timing-record", storePath, "--", "go", "test", "./..."},
		{"--provider", "aws", "--timing-record", storePath, "--", "go", "test", "./..."},
		{"--provider", "hetzner", "--timing-record", storePath, "--", "go", "test", "./..."},
		{"--provider", "hetzner", "--timing-record", storePath, "--", "go", "test", "./..."},
	}
	for i, got := range calls {
		if strings.Join(got.args, "\x00") != strings.Join(wantArgs[i], "\x00") {
			t.Fatalf("call %d args=%q want %q", i, got.args, wantArgs[i])
		}
		if got.record.Source != "bench-run" {
			t.Fatalf("call %d source=%q", i, got.record.Source)
		}
		wantRepeat := i%2 + 1
		if got.record.RepeatIndex != wantRepeat {
			t.Fatalf("call %d repeat=%d want %d", i, got.record.RepeatIndex, wantRepeat)
		}
		if got.record.ColdRun == nil || !*got.record.ColdRun {
			t.Fatalf("call %d coldRun=%v", i, got.record.ColdRun)
		}
	}
	if !strings.Contains(stderr.String(), "benchmark run completed path="+storePath+" observations=4 failures=0") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestBenchmarkReportAggregatesAndMarksInsufficientEvidence(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	cold := true
	command := []string{"pnpm", "test"}
	records := []BenchmarkTimingRecord{
		newBenchmarkTimingRecord(now.Add(-3*time.Hour), "test", TimingReport{Provider: "aws", MachineType: "c7a.large", SyncMs: 100, CommandMs: 900, TotalMs: 1100, ExitCode: 0}, Repo{Name: "my-app", Head: "abc123"}, command, &cold, 1),
		newBenchmarkTimingRecord(now.Add(-2*time.Hour), "test", TimingReport{Provider: "aws", MachineType: "c7a.large", SyncMs: 100, CommandMs: 400, TotalMs: 700, ExitCode: 1}, Repo{Name: "my-app", Head: "abc123"}, command, &cold, 2),
		newBenchmarkTimingRecord(now.Add(-90*time.Minute), "test", TimingReport{Provider: "hetzner", MachineType: "cx22", SyncMs: 200, CommandMs: 1800, TotalMs: 2100, ExitCode: 0}, Repo{Name: "my-app", Head: "abc123"}, command, &cold, 1),
		newBenchmarkTimingRecord(now.Add(-30*time.Minute), "test", TimingReport{Provider: "hetzner", MachineType: "cx22", SyncMs: 300, CommandMs: 1900, TotalMs: 2300, ExitCode: 0}, Repo{Name: "my-app", Head: "abc123"}, command, &cold, 2),
	}

	report := buildBenchmarkReport(records, benchmarkReportOptions{StorePath: "timings.jsonl", MinSamples: 2}, now)
	if report.ObservationCount != 4 || report.MatchedCount != 4 {
		t.Fatalf("counts observation=%d matched=%d", report.ObservationCount, report.MatchedCount)
	}
	if len(report.Groups) != 2 {
		t.Fatalf("groups=%d want 2: %#v", len(report.Groups), report.Groups)
	}
	groups := map[string]benchmarkReportGroup{}
	for _, group := range report.Groups {
		groups[group.Provider] = group
	}
	aws := groups["aws"]
	if aws.N != 1 || aws.FailureCount != 1 {
		t.Fatalf("aws counts n=%d failures=%d", aws.N, aws.FailureCount)
	}
	if !aws.InsufficientEvidence || !strings.Contains(aws.Evidence, "insufficient_successful_samples") {
		t.Fatalf("aws evidence=%q insufficient=%t", aws.Evidence, aws.InsufficientEvidence)
	}
	if aws.MedianTotalMs == nil || *aws.MedianTotalMs != 1100 {
		t.Fatalf("aws median total=%v", aws.MedianTotalMs)
	}
	hetzner := groups["hetzner"]
	if hetzner.N != 2 || hetzner.FailureCount != 0 {
		t.Fatalf("hetzner counts n=%d failures=%d", hetzner.N, hetzner.FailureCount)
	}
	if hetzner.InsufficientEvidence {
		t.Fatalf("hetzner should have sufficient evidence: %q", hetzner.Evidence)
	}
	if hetzner.MedianTotalMs == nil || *hetzner.MedianTotalMs != 2200 {
		t.Fatalf("hetzner median total=%v", hetzner.MedianTotalMs)
	}
}

func TestBenchReportJSONFiltersStoreRows(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "timings.jsonl")
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	cold := false
	command := []string{"go", "test", "./..."}
	for _, record := range []BenchmarkTimingRecord{
		newBenchmarkTimingRecord(now.Add(-time.Hour), "test", TimingReport{Provider: "aws", MachineType: "c7a.large", SyncMs: 100, CommandMs: 1000, TotalMs: 1200, ExitCode: 0}, Repo{Name: "my-app"}, command, &cold, 1),
		newBenchmarkTimingRecord(now.Add(-time.Hour), "test", TimingReport{Provider: "hetzner", MachineType: "cx22", SyncMs: 100, CommandMs: 1500, TotalMs: 1700, ExitCode: 0}, Repo{Name: "my-app"}, command, &cold, 1),
	} {
		if err := appendBenchmarkTimingRecord(storePath, record); err != nil {
			t.Fatal(err)
		}
	}

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	err := app.benchReport(context.Background(), []string{"--store", storePath, "--provider", "aws", "--json"})
	if err != nil {
		t.Fatalf("bench report error=%v stderr=%q", err, stderr.String())
	}
	var report benchmarkReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, stdout.String())
	}
	if report.ObservationCount != 2 || report.MatchedCount != 1 {
		t.Fatalf("counts observation=%d matched=%d", report.ObservationCount, report.MatchedCount)
	}
	if len(report.Groups) != 1 || report.Groups[0].Provider != "aws" {
		t.Fatalf("groups=%#v", report.Groups)
	}
	if report.Groups[0].InsufficientEvidence != true {
		t.Fatalf("single sample should be insufficient by default: %#v", report.Groups[0])
	}
}
