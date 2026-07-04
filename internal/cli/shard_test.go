package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type shardTestProvisioner struct {
	mu          sync.Mutex
	calls       []int
	errs        map[int]error
	releases    int
	releaseErrs []error
}

func newShardTestProvisioner() *shardTestProvisioner {
	return &shardTestProvisioner{errs: map[int]error{}}
}

func (p *shardTestProvisioner) provision(ctx context.Context, index int, slug string) (checkpointForkProvision, error) {
	p.mu.Lock()
	p.calls = append(p.calls, index)
	err := p.errs[index]
	p.mu.Unlock()
	if err != nil {
		return checkpointForkProvision{}, err
	}
	lease := LeaseTarget{LeaseID: fmt.Sprintf("cbx_shard_%d", index), Server: Server{Labels: map[string]string{"slug": fmt.Sprintf("shard-%d", index)}}}
	release := func(releaseCtx context.Context) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.releases++
		p.releaseErrs = append(p.releaseErrs, releaseCtx.Err())
	}
	return checkpointForkProvision{Lease: lease, Workdir: "/work/my-app", Release: release}, nil
}

func (p *shardTestProvisioner) stats() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls), p.releases
}

type shardTestExecutor struct {
	mu       sync.Mutex
	calls    map[int][]string
	started  chan int
	unblock  chan struct{}
	block    map[int]bool
	errs     map[int]error
	outcomes map[int]shardRunOutcome
}

func newShardTestExecutor() *shardTestExecutor {
	return &shardTestExecutor{calls: map[int][]string{}, block: map[int]bool{}, errs: map[int]error{}, outcomes: map[int]shardRunOutcome{}}
}

func (e *shardTestExecutor) run(ctx context.Context, index int, runArgs []string, outcome *shardRunOutcome) error {
	e.mu.Lock()
	e.calls[index] = append([]string{}, runArgs...)
	blocked := e.block[index]
	e.mu.Unlock()
	if e.started != nil {
		e.started <- index
	}
	if blocked {
		select {
		case <-e.unblock:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if result, ok := e.outcomes[index]; ok {
		*outcome = result
	}
	return e.errs[index]
}

func (e *shardTestExecutor) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.calls)
}

func (e *shardTestExecutor) call(index int) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls[index]
}

func shardTestOptions(count int) shardOptions {
	return shardOptions{Checkpoint: "chk_test", Count: count, ResultsWired: true, Command: []string{"pnpm", "test"}}
}

func recordedOutcome(exitCode int, results *TestResultSummary) shardRunOutcome {
	return shardRunOutcome{Recorded: true, ExitCode: exitCode, RunID: "run_test", Results: results}
}

func TestShardRunsAllShardsConcurrently(t *testing.T) {
	provisioner := newShardTestProvisioner()
	executor := newShardTestExecutor()
	executor.started = make(chan int, 3)
	executor.unblock = make(chan struct{})
	for i := 1; i <= 3; i++ {
		executor.block[i] = true
		executor.outcomes[i] = recordedOutcome(0, nil)
	}
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	done := make(chan error, 1)
	go func() {
		done <- app.shardRun(context.Background(), shardTestOptions(3), &shardOutputMux{}, provisioner.provision, executor.run)
	}()
	seen := map[int]bool{}
	for len(seen) < 3 {
		select {
		case index := <-executor.started:
			seen[index] = true
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of 3 shards started concurrently", len(seen))
		}
	}
	close(executor.unblock)
	if err := <-done; err != nil {
		t.Fatalf("shardRun: %v", err)
	}
	provisions, releases := provisioner.stats()
	if provisions != 3 || releases != 3 {
		t.Fatalf("provisions=%d releases=%d, want 3/3", provisions, releases)
	}
}

func TestShardContinuesAfterShardFailure(t *testing.T) {
	provisioner := newShardTestProvisioner()
	executor := newShardTestExecutor()
	executor.outcomes[1] = recordedOutcome(0, nil)
	executor.outcomes[2] = recordedOutcome(4, nil)
	executor.outcomes[3] = recordedOutcome(0, nil)
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	err := app.shardRun(context.Background(), shardTestOptions(3), &shardOutputMux{}, provisioner.provision, executor.run)
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 4 || exitErr.Message != "1 of 3 shards failed" {
		t.Fatalf("err=%v, want code 4 with 1 of 3 shards failed", err)
	}
	if executor.callCount() != 3 {
		t.Fatalf("calls=%d, want all 3 shards to run", executor.callCount())
	}
	if _, releases := provisioner.stats(); releases != 3 {
		t.Fatalf("releases=%d, want 3", releases)
	}
}

func TestShardMixedFailureCodesAggregateToOne(t *testing.T) {
	provisioner := newShardTestProvisioner()
	executor := newShardTestExecutor()
	executor.outcomes[1] = recordedOutcome(3, nil)
	executor.outcomes[2] = recordedOutcome(4, nil)
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	err := app.shardRun(context.Background(), shardTestOptions(2), &shardOutputMux{}, provisioner.provision, executor.run)
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 1 || exitErr.Message != "2 of 2 shards failed" {
		t.Fatalf("err=%v, want code 1 with 2 of 2 shards failed", err)
	}
}

func TestShardFailFastCancelsSiblings(t *testing.T) {
	provisioner := newShardTestProvisioner()
	executor := newShardTestExecutor()
	executor.started = make(chan int, 3)
	executor.unblock = make(chan struct{})
	executor.block[2] = true
	executor.block[3] = true
	executor.outcomes[1] = recordedOutcome(1, nil)
	opts := shardTestOptions(3)
	opts.FailFast = true
	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	err := app.shardRun(context.Background(), opts, &shardOutputMux{}, provisioner.provision, executor.run)
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 1 || exitErr.Message != "1 of 3 shards failed" {
		t.Fatalf("err=%v, want code 1 with only the real failure counted", err)
	}
	if !strings.Contains(stdout.String(), "canceled=2") {
		t.Fatalf("verdict missing canceled count:\n%s", stdout.String())
	}
	if _, releases := provisioner.stats(); releases != 3 {
		t.Fatalf("releases=%d, want every provisioned shard released", releases)
	}
}

func TestShardCancelReleasesWithBackgroundContext(t *testing.T) {
	provisioner := newShardTestProvisioner()
	executor := newShardTestExecutor()
	executor.started = make(chan int, 2)
	executor.unblock = make(chan struct{})
	executor.block[1] = true
	executor.block[2] = true
	ctx, cancel := context.WithCancel(context.Background())
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	done := make(chan error, 1)
	go func() {
		done <- app.shardRun(ctx, shardTestOptions(2), &shardOutputMux{}, provisioner.provision, executor.run)
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-executor.started:
		case <-time.After(5 * time.Second):
			t.Fatal("shards did not start")
		}
	}
	cancel()
	err := <-done
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 130 || exitErr.Message != "shard interrupted; 2 of 2 shards canceled" {
		t.Fatalf("err=%v, want exit 130 interrupt", err)
	}
	provisioner.mu.Lock()
	defer provisioner.mu.Unlock()
	if provisioner.releases != 2 {
		t.Fatalf("releases=%d, want 2", provisioner.releases)
	}
	for _, releaseErr := range provisioner.releaseErrs {
		if releaseErr != nil {
			t.Fatalf("release context canceled: %v", releaseErr)
		}
	}
}

func TestShardKeepSkipsRelease(t *testing.T) {
	provisioner := newShardTestProvisioner()
	executor := newShardTestExecutor()
	executor.outcomes[1] = recordedOutcome(0, nil)
	executor.outcomes[2] = recordedOutcome(0, nil)
	opts := shardTestOptions(2)
	opts.Keep = true
	var stderr bytes.Buffer
	app := App{Stdout: io.Discard, Stderr: &stderr}
	if err := app.shardRun(context.Background(), opts, &shardOutputMux{}, provisioner.provision, executor.run); err != nil {
		t.Fatalf("shardRun: %v", err)
	}
	if _, releases := provisioner.stats(); releases != 0 {
		t.Fatalf("releases=%d, want 0 with --keep", releases)
	}
	if !strings.Contains(stderr.String(), "kept lease=cbx_shard_1") {
		t.Fatalf("stderr missing kept line:\n%s", stderr.String())
	}
}

func TestShardProvisionFailureDoesNotOrphanSiblings(t *testing.T) {
	provisioner := newShardTestProvisioner()
	provisioner.errs[2] = errors.New("acquire failed")
	executor := newShardTestExecutor()
	executor.outcomes[1] = recordedOutcome(0, nil)
	executor.outcomes[3] = recordedOutcome(0, nil)
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	err := app.shardRun(context.Background(), shardTestOptions(3), &shardOutputMux{}, provisioner.provision, executor.run)
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 7 || exitErr.Message != "1 of 3 shards failed to provision or run" {
		t.Fatalf("err=%v, want exit 7 provision failure", err)
	}
	if executor.callCount() != 2 {
		t.Fatalf("calls=%d, want the failed shard skipped and siblings run", executor.callCount())
	}
	if executor.call(2) != nil {
		t.Fatal("executor ran for the shard that failed to provision")
	}
	if _, releases := provisioner.stats(); releases != 2 {
		t.Fatalf("releases=%d, want 2", releases)
	}
}

func TestShardMergedVerdict(t *testing.T) {
	provisioner := newShardTestProvisioner()
	executor := newShardTestExecutor()
	executor.outcomes[1] = recordedOutcome(0, &TestResultSummary{Format: "junit", Suites: 1, Tests: 10, Failures: 1, TimeSeconds: 2.5, Failed: []TestFailure{{Suite: "pkg", Name: "TestA", Kind: "failure", Message: "boom"}}})
	executor.outcomes[2] = recordedOutcome(0, &TestResultSummary{Format: "junit", Suites: 1, Tests: 20, Skipped: 2, TimeSeconds: 3.5})
	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	if err := app.shardRun(context.Background(), shardTestOptions(2), &shardOutputMux{}, provisioner.provision, executor.run); err != nil {
		t.Fatalf("shardRun: %v", err)
	}
	out := stdout.String()
	for _, want := range []string{
		"shard results",
		"shard verdict shards=2 failed_shards=0 tests=30 failures=1 errors=0 skipped=2 suite_time=6.000s",
		"failed:",
		"[1/2]",
		"TestA",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q:\n%s", want, out)
		}
	}
}

func TestShardFailOnTestFailuresAppliesToMergedSummary(t *testing.T) {
	failing := &TestResultSummary{Format: "junit", Tests: 5, Failures: 1, Failed: []TestFailure{{Name: "TestB", Kind: "failure"}}}
	cases := []struct {
		name        string
		exitCode    int
		policy      bool
		wantCode    int
		wantMessage string
	}{
		{name: "policy fires on merged failures", exitCode: 0, policy: true, wantCode: 1, wantMessage: "JUnit results contain 1 failures and 0 errors"},
		{name: "policy off keeps success", exitCode: 0, policy: false, wantCode: 0},
		{name: "command failure takes precedence", exitCode: 2, policy: true, wantCode: 2, wantMessage: "1 of 1 shards failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provisioner := newShardTestProvisioner()
			executor := newShardTestExecutor()
			executor.outcomes[1] = recordedOutcome(tc.exitCode, failing)
			opts := shardTestOptions(1)
			opts.FailOnTestFailures = tc.policy
			app := App{Stdout: io.Discard, Stderr: io.Discard}
			err := app.shardRun(context.Background(), opts, &shardOutputMux{}, provisioner.provision, executor.run)
			if tc.wantCode == 0 {
				if err != nil {
					t.Fatalf("shardRun: %v", err)
				}
				return
			}
			var exitErr ExitError
			if !AsExitError(err, &exitErr) || exitErr.Code != tc.wantCode || exitErr.Message != tc.wantMessage {
				t.Fatalf("err=%v, want code=%d message=%q", err, tc.wantCode, tc.wantMessage)
			}
		})
	}
}

func TestShardJSONReport(t *testing.T) {
	provisioner := newShardTestProvisioner()
	executor := newShardTestExecutor()
	executor.outcomes[1] = recordedOutcome(0, &TestResultSummary{Format: "junit", Tests: 3, TimeSeconds: 1.5})
	executor.outcomes[2] = recordedOutcome(2, nil)
	opts := shardTestOptions(2)
	opts.JSON = true
	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	err := app.shardRun(context.Background(), opts, &shardOutputMux{}, provisioner.provision, executor.run)
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("err=%v, want code 2", err)
	}
	var report shardJSONReport
	if decodeErr := json.Unmarshal(stdout.Bytes(), &report); decodeErr != nil {
		t.Fatalf("decode: %v\n%s", decodeErr, stdout.String())
	}
	if report.Checkpoint != "chk_test" || report.Count != 2 || len(report.Shards) != 2 || report.ExitCode != 2 {
		t.Fatalf("report=%+v", report)
	}
	if report.Merged == nil || report.Merged.Tests != 3 {
		t.Fatalf("merged=%+v", report.Merged)
	}
	if report.Failed == nil {
		t.Fatal("failed must encode as [] not null")
	}
	if report.Shards[0].LeaseID != "cbx_shard_1" || report.Shards[1].ExitCode != 2 {
		t.Fatalf("shards=%+v", report.Shards)
	}
}

func TestShardRunArgsInjection(t *testing.T) {
	args := shardRunArgs("cbx_123", 2, 4, []string{"--results-auto"}, []string{"pnpm", "test", "--", "--shard", "2/4"})
	want := []string{"--id", "cbx_123", "--keep", "--label", "shard 2/4", "--fail-on-test-failures=false", "--results-auto", "--", "pnpm", "test", "--", "--shard", "2/4"}
	if strings.Join(args, " ") != strings.Join(want, " ") {
		t.Fatalf("args=%v\nwant=%v", args, want)
	}
}

func TestPartitionShardRunArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantOwn string
		wantRun string
		wantErr string
	}{
		{name: "owned only stays local", args: []string{"--count", "3", "--from", "chk_1", "--fail-fast"}, wantOwn: "--count 3 --from chk_1 --fail-fast"},
		{name: "registered flags forward to both", args: []string{"--count", "2", "--junit", "junit.xml"}, wantOwn: "--count 2 --junit junit.xml", wantRun: "--junit junit.xml"},
		{name: "unknown flags forward to run", args: []string{"--count", "2", "--no-hydrate"}, wantOwn: "--count 2", wantRun: "--no-hydrate"},
		{name: "id is forbidden", args: []string{"--id", "cbx_1"}, wantErr: "--id cannot be used with shard"},
		{name: "pool is forbidden", args: []string{"--pool", "ready"}, wantErr: "--pool cannot be used with shard"},
		{name: "timing-record is forbidden", args: []string{"--timing-record", "default"}, wantErr: "--timing-record cannot be used with shard"},
		{name: "capture-stdout is forbidden", args: []string{"--capture-stdout", "out.log"}, wantErr: "--capture-stdout cannot be used with shard"},
		{name: "label is forbidden", args: []string{"--label", "mine"}, wantErr: "--label cannot be used with shard"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFlagSet("shard", io.Discard)
			fs.Int("count", 0, "")
			fs.String("from", "", "")
			fs.Bool("fail-fast", false, "")
			fs.String("junit", "", "")
			own, run, err := partitionForwardedRunArgs(fs, tc.args, shardOwnedOnlyFlags, shardForbiddenRunFlags, "shard: it conflicts with parallel shard runs")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err=%v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("partition: %v", err)
			}
			if strings.Join(own, " ") != tc.wantOwn || strings.Join(run, " ") != tc.wantRun {
				t.Fatalf("own=%v run=%v, want own=%q run=%q", own, run, tc.wantOwn, tc.wantRun)
			}
		})
	}
}

func TestPrefixLineWriter(t *testing.T) {
	cases := []struct {
		name   string
		writes []string
		flush  bool
		want   string
	}{
		{name: "single line", writes: []string{"hello\n"}, want: "[1/3] hello\n"},
		{name: "multiple lines in one write", writes: []string{"a\nb\n"}, want: "[1/3] a\n[1/3] b\n"},
		{name: "partial line buffers until newline", writes: []string{"par", "tial\n"}, want: "[1/3] partial\n"},
		{name: "flush emits trailing partial", writes: []string{"tail"}, flush: true, want: "[1/3] tail\n"},
		{name: "flush with empty buffer is silent", writes: []string{"done\n"}, flush: true, want: "[1/3] done\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			writer := &prefixLineWriter{mux: &shardOutputMux{}, out: &out, prefix: shardLinePrefix(1, 3)}
			for _, chunk := range tc.writes {
				if _, err := writer.Write([]byte(chunk)); err != nil {
					t.Fatalf("write: %v", err)
				}
			}
			if tc.flush {
				if err := writer.Flush(); err != nil {
					t.Fatalf("flush: %v", err)
				}
			}
			if out.String() != tc.want {
				t.Fatalf("out=%q, want %q", out.String(), tc.want)
			}
		})
	}
}

func TestShardLinePrefixWidth(t *testing.T) {
	if got := shardLinePrefix(3, 12); got != "[ 3/12] " {
		t.Fatalf("prefix=%q", got)
	}
	if got := shardLinePrefix(11, 12); got != "[11/12] " {
		t.Fatalf("prefix=%q", got)
	}
}

func TestPrefixLineWriterOversizedPartialFlushes(t *testing.T) {
	var out bytes.Buffer
	writer := &prefixLineWriter{mux: &shardOutputMux{}, out: &out, prefix: "[1/2] "}
	big := strings.Repeat("x", shardPartialLineLimit+1)
	if _, err := writer.Write([]byte(big)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !strings.HasPrefix(out.String(), "[1/2] xxx") || !strings.HasSuffix(out.String(), "x\n") {
		t.Fatalf("oversized partial not flushed: len=%d", out.Len())
	}
}

func TestPrefixLineWriterConcurrentLineIntegrity(t *testing.T) {
	var out bytes.Buffer
	mux := &shardOutputMux{}
	var wg sync.WaitGroup
	for i := 1; i <= 8; i++ {
		writer := &prefixLineWriter{mux: mux, out: &out, prefix: shardLinePrefix(i, 8)}
		wg.Add(1)
		go func(index int, w *prefixLineWriter) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				payload := fmt.Sprintf("shard%d-line%d", index, j)
				w.Write([]byte(payload[:4]))
				w.Write([]byte(payload[4:] + "\n"))
			}
		}(i, writer)
	}
	wg.Wait()
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 400 {
		t.Fatalf("lines=%d, want 400", len(lines))
	}
	for _, line := range lines {
		rest := line
		if !strings.HasPrefix(rest, "[") {
			t.Fatalf("line missing prefix: %q", line)
		}
		end := strings.Index(rest, "] ")
		if end < 0 {
			t.Fatalf("line missing prefix close: %q", line)
		}
		payload := rest[end+2:]
		if !strings.HasPrefix(payload, "shard") || !strings.Contains(payload, "-line") {
			t.Fatalf("payload corrupted: %q", line)
		}
	}
}

func TestShardRejectsInvalidArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing count", args: []string{"--from", "chk_1", "--", "pnpm", "test"}, want: "--count must be at least 1"},
		{name: "zero count", args: []string{"--count", "0", "--from", "chk_1", "--", "pnpm", "test"}, want: "--count must be at least 1"},
		{name: "missing from", args: []string{"--count", "2", "--", "pnpm", "test"}, want: "usage: crabbox shard"},
		{name: "missing command", args: []string{"--count", "2", "--from", "chk_1"}, want: "usage: crabbox shard"},
		{name: "positional argument", args: []string{"--count", "2", "--from", "chk_1", "stray", "--", "pnpm", "test"}, want: "place the command after --"},
		{name: "forbidden run flag", args: []string{"--count", "2", "--from", "chk_1", "--pool", "ready", "--", "pnpm", "test"}, want: "--pool cannot be used with shard"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
			app := App{Stdout: io.Discard, Stderr: io.Discard}
			err := app.shard(context.Background(), tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
		})
	}
}

func TestShardMaxCountCap(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "crabbox.yaml")
	if err := os.WriteFile(configPath, []byte("shard:\n  maxCount: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", configPath)
	store, err := defaultCheckpointStore()
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Create(checkpointRecord{ID: "chk_shard_cap", Kind: checkpointKindArchive, CreatedAt: time.Now().UTC().Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	err = app.shard(context.Background(), []string{"--count", "3", "--from", record.ID, "--", "pnpm", "test"})
	if err == nil || !strings.Contains(err.Error(), "--count 3 exceeds shard.maxCount 2") {
		t.Fatalf("err=%v", err)
	}
}

func TestShardDryRunPrintsPlan(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	store, err := defaultCheckpointStore()
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Create(checkpointRecord{ID: "chk_shard_dry", Kind: checkpointKindArchive, CreatedAt: time.Now().UTC().Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	err = app.shard(context.Background(), []string{
		"--count", "2", "--from", record.ID, "--dry-run", "--slug", "Suite",
		"--", "pnpm", "test", "--", "--shard", "{{index}}/{{total}}",
	})
	if err != nil {
		t.Fatal(err)
	}
	out := stdout.String()
	for _, want := range []string{
		`would shard checkpoint id=chk_shard_dry`,
		`slug=suite-1 keep=false index=1/2 command="pnpm test -- --shard 1/2"`,
		`slug=suite-2 keep=false index=2/2 command="pnpm test -- --shard 2/2"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q:\n%s", want, out)
		}
	}
}

func TestShardHelpPrintsUsage(t *testing.T) {
	var stderr bytes.Buffer
	app := App{Stdout: io.Discard, Stderr: &stderr}
	err := app.shard(context.Background(), []string{"--help"})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 0 {
		t.Fatalf("err=%v, want ExitError code 0", err)
	}
	if !strings.Contains(stderr.String(), "Usage:") {
		t.Fatalf("help missing Usage block:\n%s", stderr.String())
	}
}
