package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
