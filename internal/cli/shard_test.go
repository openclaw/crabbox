package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
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
	return shardOptions{Checkpoint: "chk_test", Count: count, Concurrency: count, ResultsWired: true, Command: []string{"pnpm", "test"}}
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

func TestShardFailFastSkipsWaitersWithoutProvisioning(t *testing.T) {
	provisioner := newShardTestProvisioner()
	executor := newShardTestExecutor()
	executor.started = make(chan int, 4)
	executor.unblock = make(chan struct{})
	for i := 1; i <= 4; i++ {
		executor.block[i] = true
		executor.outcomes[i] = recordedOutcome(1, nil)
	}
	opts := shardTestOptions(4)
	opts.Concurrency = 1
	opts.FailFast = true
	var stdout bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- App{Stdout: &stdout, Stderr: io.Discard}.shardRun(context.Background(), opts, &shardOutputMux{}, provisioner.provision, executor.run)
	}()
	select {
	case <-executor.started:
	case <-time.After(5 * time.Second):
		t.Fatal("first shard did not start")
	}
	close(executor.unblock)
	err := <-done
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 1 || !strings.Contains(stdout.String(), "canceled=") {
		t.Fatalf("err=%v stdout=%s, want exit 1 with canceled=", err, stdout.String())
	}
	if provisions, releases := provisioner.stats(); provisions != 1 || releases != 1 {
		t.Fatalf("provisions=%d releases=%d, want 1/1 under concurrency 1 fail-fast", provisions, releases)
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
	opts := shardTestOptions(4)
	opts.Concurrency = 2
	go func() {
		done <- app.shardRun(ctx, opts, &shardOutputMux{}, provisioner.provision, executor.run)
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
	if !AsExitError(err, &exitErr) || exitErr.Code != 130 || exitErr.Message != "shard interrupted; 4 of 4 shards canceled" {
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

func TestShardCreateFailureMidFanOutUnderConcurrency(t *testing.T) {
	// Create refusal mid fan-out: siblings still run and release, and in-flight
	// provisions stay within --concurrency.
	provisioner := &shardTestProvisioner{errs: map[int]error{2: errors.New("image not found")}}
	var (
		mu       sync.Mutex
		inflight int
		maxSeen  int
		gate     = make(chan struct{})
		started  = make(chan int, 4)
	)
	slowProvision := func(ctx context.Context, index int, slug string) (checkpointForkProvision, error) {
		mu.Lock()
		inflight++
		if inflight > maxSeen {
			maxSeen = inflight
		}
		mu.Unlock()
		started <- index
		defer func() {
			mu.Lock()
			inflight--
			mu.Unlock()
		}()
		if index == 1 || index == 2 {
			<-gate
		}
		return provisioner.provision(ctx, index, slug)
	}
	executor := newShardTestExecutor()
	for _, i := range []int{1, 3, 4} {
		executor.outcomes[i] = recordedOutcome(0, nil)
	}
	opts := shardTestOptions(4)
	opts.Concurrency = 2
	var stdout, stderr bytes.Buffer
	mux := &shardOutputMux{}
	app := App{Stdout: &stdout, Stderr: &stderr}
	done := make(chan error, 1)
	go func() { done <- app.shardRun(context.Background(), opts, mux, slowProvision, executor.run) }()
	first := map[int]bool{}
	for len(first) < 2 {
		select {
		case idx := <-started:
			first[idx] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for first concurrency wave; started=%v", first)
		}
	}
	close(gate)
	err := <-done
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 7 || exitErr.Message != "1 of 4 shards failed to provision or run" {
		t.Fatalf("err=%v, want exit 7 create failure mid fan-out", err)
	}
	mu.Lock()
	seen := maxSeen
	mu.Unlock()
	if seen > 2 {
		t.Fatalf("max in-flight provisions=%d, want <= 2", seen)
	}
	if executor.callCount() != 3 || executor.call(2) != nil {
		t.Fatalf("calls=%d call2=%v, want 3 siblings and skipped refused shard", executor.callCount(), executor.call(2))
	}
	if _, releases := provisioner.stats(); releases != 3 {
		t.Fatalf("releases=%d, want 3", releases)
	}
	out := stdout.String()
	if !strings.Contains(out, "provision failures:") || !strings.Contains(out, "[2/4] error=image not found") || !strings.Contains(out, "failed_shards=1") {
		t.Fatalf("stdout missing provision failure report:\n%s", out)
	}
}

func TestShardInfraFailureTakesPrecedenceOverCommandFailure(t *testing.T) {
	provisioner := newShardTestProvisioner()
	provisioner.errs[3] = errors.New("acquire failed")
	executor := newShardTestExecutor()
	executor.outcomes[1] = recordedOutcome(4, nil)
	executor.outcomes[2] = recordedOutcome(0, nil)
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	err := app.shardRun(context.Background(), shardTestOptions(3), &shardOutputMux{}, provisioner.provision, executor.run)
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 7 || exitErr.Message != "1 of 3 shards failed to provision or run" {
		t.Fatalf("err=%v, want infra exit 7 to take precedence over command exit 4", err)
	}
}

func TestShardMergedVerdict(t *testing.T) {
	provisioner := newShardTestProvisioner()
	executor := newShardTestExecutor()
	executor.outcomes[1] = recordedOutcome(0, &TestResultSummary{Format: "junit", Suites: 1, Tests: 10, Failures: 1, TimeSeconds: 2.5, Failed: []TestFailure{{Suite: "pkg\x1b]0;suite\x07", Name: "TestA\rhidden", Kind: "failure", Message: "boom\x1b]8;;https://example.test\x07link"}}})
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
		"[1/2] run=run_test",
		`pkg\u001B]0;suite\u0007`,
		`TestA\u000Dhidden`,
		`boom\u001B]8;;https://example.test\u0007link`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q:\n%s", want, out)
		}
	}
	assertTerminalSafe(t, out)
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
		{name: "concurrency stays local", args: []string{"--count", "2", "--from", "chk_1", "--concurrency", "2"}, wantOwn: "--count 2 --from chk_1 --concurrency 2"},
		{name: "provider stays local", args: []string{"--count", "2", "--from", "chk_1", "--provider", "local-container"}, wantOwn: "--count 2 --from chk_1 --provider local-container"},
		{name: "registered flags forward to both", args: []string{"--count", "2", "--junit", "junit.xml"}, wantOwn: "--count 2 --junit junit.xml", wantRun: "--junit junit.xml"},
		{name: "unknown flags forward to run", args: []string{"--count", "2", "--no-hydrate"}, wantOwn: "--count 2", wantRun: "--no-hydrate"},
		{name: "id is forbidden", args: []string{"--id", "cbx_1"}, wantErr: "--id cannot be used with shard"},
		{name: "pool is forbidden", args: []string{"--pool", "ready"}, wantErr: "--pool cannot be used with shard"},
		{name: "timing-record is forbidden", args: []string{"--timing-record", "default"}, wantErr: "--timing-record cannot be used with shard"},
		{name: "capture-stdout is forbidden", args: []string{"--capture-stdout", "out.log"}, wantErr: "--capture-stdout cannot be used with shard"},
		{name: "label is forbidden", args: []string{"--label", "mine"}, wantErr: "--label cannot be used with shard"},
		{name: "attest is forbidden", args: []string{"--attest", "receipt.json"}, wantErr: "--attest cannot be used with shard"},
		{name: "attest-key is forbidden", args: []string{"--attest-key", "key.pem"}, wantErr: "--attest-key cannot be used with shard"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFlagSet("shard", io.Discard)
			fs.Int("count", 0, "")
			fs.String("from", "", "")
			fs.Bool("fail-fast", false, "")
			fs.String("junit", "", "")
			fs.String("provider", "", "")
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
		{name: "zero concurrency", args: []string{"--count", "2", "--concurrency", "0", "--from", "chk_1", "--", "pnpm", "test"}, want: "--concurrency must be at least 1"},
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

func TestShardConfigCaps(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		args []string
		want string
		id   string
	}{
		{name: "maxCount", yaml: "shard:\n  maxCount: 2\n", args: []string{"--count", "3", "--from", "chk_shard_cap", "--", "pnpm", "test"}, want: "--count 3 exceeds shard.maxCount 2", id: "chk_shard_cap"},
		{name: "maxConcurrency", yaml: "shard:\n  maxConcurrency: 2\n", args: []string{"--count", "4", "--concurrency", "3", "--from", "chk_shard_conc_cap", "--", "pnpm", "test"}, want: "--concurrency 3 exceeds shard.maxConcurrency 2", id: "chk_shard_conc_cap"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			configPath := filepath.Join(t.TempDir(), "crabbox.yaml")
			if err := os.WriteFile(configPath, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CRABBOX_CONFIG", configPath)
			store, err := defaultCheckpointStore()
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.Reserve(checkpointRecord{ID: tc.id, Kind: checkpointKindArchive, CreatedAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
				t.Fatal(err)
			}
			err = App{Stdout: io.Discard, Stderr: io.Discard}.shard(context.Background(), tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
		})
	}
}

func TestResolveShardConcurrency(t *testing.T) {
	cases := []struct {
		name           string
		count          int
		flag           int
		flagSet        bool
		maxConcurrency int
		want           int
		wantErr        string
	}{
		{name: "default all", count: 8, want: 8},
		{name: "flag bounds", count: 8, flag: 3, flagSet: true, want: 3},
		{name: "flag above count clamps", count: 4, flag: 9, flagSet: true, want: 4},
		{name: "config bounds default", count: 8, maxConcurrency: 2, want: 2},
		{name: "flag within config", count: 8, flag: 2, flagSet: true, maxConcurrency: 4, want: 2},
		{name: "flag exceeds config", count: 8, flag: 5, flagSet: true, maxConcurrency: 2, wantErr: "--concurrency 5 exceeds shard.maxConcurrency 2"},
		{name: "zero flag", count: 4, flag: 0, flagSet: true, wantErr: "--concurrency must be at least 1"},
		{name: "negative flag", count: 4, flag: -1, flagSet: true, wantErr: "--concurrency must be at least 1"},
		{name: "excessive flag", count: 4, flag: 1025, flagSet: true, wantErr: "--concurrency must be at most 1024"},
		{name: "maximum flag", count: 2048, flag: 1024, flagSet: true, want: 1024},
		{name: "negative config", count: 4, maxConcurrency: -1, wantErr: "shard.maxConcurrency must be between 0 and 1024"},
		{name: "excessive config", count: 4, maxConcurrency: 1025, wantErr: "shard.maxConcurrency must be between 0 and 1024"},
		{name: "default count unchanged", count: 2048, want: 2048},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveShardConcurrency(tc.count, tc.flag, tc.flagSet, tc.maxConcurrency)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err=%v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveShardConcurrency: %v", err)
			}
			if got != tc.want {
				t.Fatalf("concurrency=%d, want %d", got, tc.want)
			}
		})
	}
}

func TestShardConcurrencyUpgradeFromV0793(t *testing.T) {
	// v0.79.3 configs used shard.maxCount only (or omitted shard entirely).
	// Loading them must leave MaxConcurrency at 0 so fan-out stays unbounded.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Run("maxCount only", func(t *testing.T) {
		cfg := defaultConfig()
		maxCount := 8
		if err := applyFileConfig(&cfg, fileConfig{Shard: &fileShardConfig{MaxCount: &maxCount}}); err != nil {
			t.Fatal(err)
		}
		if cfg.Shard.MaxCount != 8 || cfg.Shard.MaxConcurrency != 0 {
			t.Fatalf("shard=%#v, want maxCount=8 maxConcurrency=0", cfg.Shard)
		}
		got, err := resolveShardConcurrency(8, 0, false, cfg.Shard.MaxConcurrency)
		if err != nil {
			t.Fatal(err)
		}
		if got != 8 {
			t.Fatalf("concurrency=%d, want unbounded 8", got)
		}
	})
	t.Run("no shard key", func(t *testing.T) {
		cfg := defaultConfig()
		if err := applyFileConfig(&cfg, fileConfig{}); err != nil {
			t.Fatal(err)
		}
		if cfg.Shard.MaxConcurrency != 0 {
			t.Fatalf("MaxConcurrency=%d, want 0", cfg.Shard.MaxConcurrency)
		}
		got, err := resolveShardConcurrency(4, 0, false, cfg.Shard.MaxConcurrency)
		if err != nil || got != 4 {
			t.Fatalf("concurrency=%d err=%v, want 4", got, err)
		}
	})
}

func TestRejectShardProviderMismatch(t *testing.T) {
	fs := newFlagSet("shard", io.Discard)
	provider := fs.String("provider", "", "")
	record := checkpointRecord{ID: "chk_mismatch", Provider: "local-container"}
	record.Native.Provider = "local-container"
	if err := parseFlags(fs, []string{"--provider", "hetzner"}); err != nil {
		t.Fatal(err)
	}
	err := rejectShardProviderMismatch(fs, leaseCreateFlagValues{Provider: provider}, record)
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 || !strings.Contains(exitErr.Message, "does not match the recorded native checkpoint provider") {
		t.Fatalf("err=%v, want exit 2 mismatch", err)
	}
	fsOK := newFlagSet("shard", io.Discard)
	providerOK := fsOK.String("provider", "", "")
	if err := parseFlags(fsOK, []string{"--provider", "local-container"}); err != nil {
		t.Fatal(err)
	}
	if err := rejectShardProviderMismatch(fsOK, leaseCreateFlagValues{Provider: providerOK}, record); err != nil {
		t.Fatalf("matching provider: %v", err)
	}
}

func TestShardDryRunPrintsPlan(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	store, err := defaultCheckpointStore()
	if err != nil {
		t.Fatal(err)
	}
	record, _, err := store.Reserve(checkpointRecord{ID: "chk_shard_dry", Kind: checkpointKindArchive, CreatedAt: time.Now().UTC().Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	err = app.shard(context.Background(), []string{
		"--count", "2", "--from", record.ID, "--provider", "local-container", "--dry-run", "--slug", "Suite",
		"--", "pnpm", "test", "--", "--shard", "{{index}}/{{total}}",
	})
	if err != nil {
		t.Fatal(err)
	}
	out := stdout.String()
	for _, want := range []string{
		`would shard checkpoint id=chk_shard_dry provider=local-container count=2 concurrency=2 keep=false`,
		`slug=suite-1 keep=false index=1/2 command="pnpm test -- --shard 1/2"`,
		`slug=suite-2 keep=false index=2/2 command="pnpm test -- --shard 2/2"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q:\n%s", want, out)
		}
	}
}

func TestShardArchiveDryRunRequiresProviderIntent(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	store, err := defaultCheckpointStore()
	if err != nil {
		t.Fatal(err)
	}
	record, _, err := store.Reserve(checkpointRecord{ID: "chk_shard_no_provider", Kind: checkpointKindArchive, CreatedAt: time.Now().UTC().Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	err = (App{Stdout: io.Discard, Stderr: io.Discard}).shard(context.Background(), []string{
		"--count", "2", "--from", record.ID, "--dry-run", "--", "go", "test", "./...",
	})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 || exitErr.Message != providerSelectionRequiredDiagnostic {
		t.Fatalf("error=%v, want exit 2 %q", err, providerSelectionRequiredDiagnostic)
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

func TestShardProvisionFailureSummarySanitizesControlBytes(t *testing.T) {
	// LocalCommandError-style stderr can include ESC/CSI; verdict output must not.
	raw := "provision failed: \x1b[31mboom\x1b[0m\x00\x7f"
	provisioner := &shardTestProvisioner{errs: map[int]error{1: errors.New(raw)}}
	executor := newShardTestExecutor()
	opts := shardTestOptions(1)
	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	err := app.shardRun(context.Background(), opts, &shardOutputMux{}, provisioner.provision, executor.run)
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 7 {
		t.Fatalf("err=%v, want exit 7", err)
	}
	out := stdout.String() + stderr.String()
	assertTerminalSafe(t, out)
	want := terminalSafeResultField(raw)
	if !strings.Contains(out, want) {
		t.Fatalf("sanitized error missing from output want=%q\n%s", want, out)
	}
	if strings.Contains(out, "\x1b") || strings.Contains(out, "\x00") {
		t.Fatalf("raw control bytes leaked into shard summary:\n%q", out)
	}
	opts.JSON = true
	stdout.Reset()
	stderr.Reset()
	err = app.shardRun(context.Background(), opts, &shardOutputMux{}, provisioner.provision, executor.run)
	if !AsExitError(err, &exitErr) || exitErr.Code != 7 {
		t.Fatalf("json err=%v, want exit 7", err)
	}
	var report shardJSONReport
	if decodeErr := json.Unmarshal(stdout.Bytes(), &report); decodeErr != nil {
		t.Fatalf("decode: %v\n%s", decodeErr, stdout.String())
	}
	if len(report.Shards) != 1 || report.Shards[0].Error != want {
		t.Fatalf("json error=%q want %q report=%#v", report.Shards[0].Error, want, report)
	}
	assertTerminalSafe(t, report.Shards[0].Error)
}

const shardCarryProviderName = "shard-carry-test"

type shardCarryTestProvider struct{}

func (shardCarryTestProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        shardCarryProviderName,
		Kind:        ProviderKindSSHLease,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Features:    FeatureSet{FeatureSSH, FeatureCrabboxSync},
		Coordinator: CoordinatorNever,
	}
}
func (shardCarryTestProvider) RegisterFlags(*flag.FlagSet, Config) any { return noProviderFlags{} }
func (shardCarryTestProvider) ApplyFlags(*Config, *flag.FlagSet, any) error {
	return nil
}
func (p shardCarryTestProvider) Configure(Config, Runtime) (Backend, error) {
	if shardCarryTestBackend == nil {
		return nil, errors.New("shard carry test backend not installed")
	}
	return shardCarryTestBackend, nil
}

// shardCarryBackend is a minimal SSH lease backend for proving runCommand hands
// AuthorizedClaim back to runOneShard. Release-authority (allow + stale refuse)
// is covered by TestShardCleanupHetznerReleaseAuthority against the real
// Hetzner ReleaseLease path.
type shardCarryBackend struct {
	spec  ProviderSpec
	lease LeaseTarget
}

var shardCarryTestBackend *shardCarryBackend

func (b *shardCarryBackend) Spec() ProviderSpec { return b.spec }
func (b *shardCarryBackend) Acquire(context.Context, AcquireRequest) (LeaseTarget, error) {
	return b.lease, nil
}
func (b *shardCarryBackend) Resolve(context.Context, ResolveRequest) (LeaseTarget, error) {
	return b.lease, nil
}
func (b *shardCarryBackend) List(context.Context, ListRequest) ([]LeaseView, error) {
	return nil, nil
}
func (b *shardCarryBackend) Touch(_ context.Context, req TouchRequest) (Server, error) {
	server := req.Lease.Server
	server.Provider = b.spec.Name
	return server, nil
}
func (b *shardCarryBackend) ReleaseLeaseConnectionCleanupSafe() bool { return false }
func (b *shardCarryBackend) ReleaseLease(context.Context, ReleaseLeaseRequest) error {
	return nil
}

// TestRunOneShardCarriesAuthorizedClaimOnly drives real runOneShard → runCommand
// and asserts the post-run AuthorizedClaim is handed to carryReleaseClaim.
// Provider delete fencing lives in TestShardCleanupHetznerReleaseAuthority.
func TestRunOneShardCarriesAuthorizedClaimOnly(t *testing.T) {
	RegisterProvider(shardCarryTestProvider{})
	t.Cleanup(func() { delete(providerRegistry, shardCarryProviderName) })

	clearConfigEnv(t)
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	installRecordingSSH(t, dir)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, "missing.yaml"))
	t.Setenv("CRABBOX_FAKE_SSH_PORT", "22")
	t.Setenv("CRABBOX_FAKE_SSH_PROXY", "1")

	repo, err := findRepo()
	if err != nil {
		t.Fatal(err)
	}
	const leaseID = "cbx_shardcarry01"
	cfg := baseConfig()
	cfg.Provider = shardCarryProviderName
	server := Server{
		CloudID:  "i-shard-carry",
		Provider: cfg.Provider,
		Labels: map[string]string{
			"lease":    leaseID,
			"provider": cfg.Provider,
			"slug":     "shard-carry",
			"state":    "ready",
		},
	}
	target := SSHTarget{User: "crabbox", Host: "127.0.0.1", Port: "22", TargetOS: targetLinux, SSHConfigProxy: true}
	if err := ClaimLeaseTargetForRepoConfig(leaseID, "shard-carry", cfg, server, target, repo.Root, cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	acquired, err := ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	SetServerLeaseClaimSnapshot(&server, acquired, true)
	lease := LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}
	backend := &shardCarryBackend{spec: shardCarryTestProvider{}.Spec(), lease: lease}
	shardCarryTestBackend = backend
	t.Cleanup(func() {
		shardCarryTestBackend = nil
		RemoveLeaseClaim(leaseID)
	})

	var stderr bytes.Buffer
	var carried *leaseClaim
	fakeOwner := func(ctx context.Context, target SSHTarget, id string, _ io.Writer) (*workspaceOwner, error) {
		ownerCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		close(done)
		return &workspaceOwner{
			target: target, transport: runRecoveryOwnerTransport{},
			key: workspaceOwnerKey(id), token: strings.Repeat("a", 64), ttl: time.Minute,
			ctx: ownerCtx, cancel: cancel, stop: make(chan struct{}), done: done,
		}, nil
	}
	app := App{Stdout: io.Discard, Stderr: &stderr, workspaceOwnerAcquirer: fakeOwner}
	releaseCFG := cfg
	release, carryReleaseClaim := app.checkpointForkReleaseBindings(&server, &target, leaseID, nil, backend, &releaseCFG)
	provision := func(context.Context, int, string) (checkpointForkProvision, error) {
		return checkpointForkProvision{
			Lease: lease,
			Release: func(ctx context.Context) {
				release(ctx)
			},
			carryReleaseClaim: func(claim leaseClaim) {
				cloned := claim
				carried = &cloned
				carryReleaseClaim(claim)
			},
		}, nil
	}
	execute := func(ctx context.Context, _ int, runArgs []string, outcome *shardRunOutcome) error {
		shardApp := App{Stdout: io.Discard, Stderr: &stderr, runOutcome: outcome, workspaceOwnerAcquirer: fakeOwner}
		args := append([]string{
			"--provider", shardCarryProviderName,
			"--no-sync", "--no-hydrate", "--timing-record", "off",
		}, runArgs...)
		if err := shardApp.runCommand(ctx, args); err != nil {
			return err
		}
		if outcome.AuthorizedClaim == nil {
			return errors.New("runCommand did not return AuthorizedClaim")
		}
		return nil
	}
	opts := shardTestOptions(1)
	opts.Keep = false
	opts.Command = []string{"true"}
	result := app.runOneShard(context.Background(), func() {}, opts, &shardOutputMux{}, provision, execute, 1)
	if !result.Recorded || result.ExitCode != 0 {
		t.Fatalf("result=%#v stderr=%s", result, stderr.String())
	}
	if carried == nil || carried.LeaseID != leaseID {
		t.Fatalf("carryReleaseClaim got %#v, want lease %s", carried, leaseID)
	}
	snapshot, exists, set := ServerLeaseClaimSnapshot(server)
	if !set || !exists || !reflect.DeepEqual(snapshot, *carried) {
		t.Fatalf("server snapshot set=%v exists=%v claim=%#v carried=%#v", set, exists, snapshot, carried)
	}
}

// TestShardV0793OnDiskSettingsAndClaimIntact loads the v0.79.3 on-disk shapes
// (shard.maxCount-only config + claim JSON) with the current code and asserts
// settings/claims stay intact for the shard path.
func TestShardV0793OnDiskSettingsAndClaimIntact(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	configPath := filepath.Join(dir, "crabbox.yaml")
	// Shape recorded at tag v0.79.3: shard.maxCount only, no maxConcurrency.
	const v0793YAML = "shard:\n  maxCount: 12\n"
	if err := os.WriteFile(configPath, []byte(v0793YAML), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", configPath)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Shard != (ShardConfig{MaxCount: 12}) {
		t.Fatalf("loaded shard=%#v, want maxCount=12 maxConcurrency=0 (v0.79.3 default)", cfg.Shard)
	}
	got, err := resolveShardConcurrency(12, 0, false, cfg.Shard.MaxConcurrency)
	if err != nil || got != 12 {
		t.Fatalf("concurrency=%d err=%v, want unbounded 12", got, err)
	}

	const leaseID = "cbx_v0793claim01"
	// Minimal claim JSON matching v0.79.3's persisted fields.
	seed := map[string]any{
		"leaseID": leaseID, "revision": "gen-v0793", "slug": "upgrade-claim",
		"provider": "aws", "cloudID": "i-v0793", "providerScope": "us-east-1",
		"repoRoot": dir, "claimedAt": "2026-01-02T03:04:05Z", "lastUsedAt": "2026-01-02T03:04:05Z",
	}
	claimBytes, err := json.MarshalIndent(seed, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path, err := leaseClaimPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(claimBytes, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	claim, err := ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.LeaseID != leaseID || claim.Revision != "gen-v0793" || claim.CloudID != "i-v0793" || claim.Provider != "aws" {
		t.Fatalf("claim not intact after load: %#v", claim)
	}
}
