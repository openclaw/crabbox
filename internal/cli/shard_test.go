package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	app := App{Stdout: &stdout, Stderr: io.Discard}
	done := make(chan error, 1)
	go func() {
		done <- app.shardRun(context.Background(), opts, &shardOutputMux{}, provisioner.provision, executor.run)
	}()
	select {
	case <-executor.started:
	case <-time.After(5 * time.Second):
		t.Fatal("first shard did not start")
	}
	close(executor.unblock)
	err := <-done
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 1 {
		t.Fatalf("err=%v, want exit 1", err)
	}
	if !strings.Contains(stdout.String(), "canceled=") {
		t.Fatalf("verdict missing canceled shards:\n%s", stdout.String())
	}
	provisions, releases := provisioner.stats()
	if provisions != releases {
		t.Fatalf("provisions=%d releases=%d, want matching cleanup", provisions, releases)
	}
	if provisions != 1 {
		t.Fatalf("provisions=%d, want exactly one under concurrency 1 fail-fast", provisions)
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
	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
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
	out := stdout.String()
	if !strings.Contains(out, "provision failures:") || !strings.Contains(out, "[2/3] error=acquire failed") {
		t.Fatalf("stdout missing clear provision failure report:\n%s", out)
	}
}

func TestShardCreateFailureMidFanOutUnderConcurrency(t *testing.T) {
	// Non-capacity create refusal mid fan-out: siblings still run and release.
	provisioner := &shardTestProvisioner{
		errs: map[int]error{
			2: errors.New("image not found"),
		},
	}
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
	app := App{Stdout: &stdout, Stderr: &stderr}
	done := make(chan error, 1)
	go func() {
		done <- app.shardRun(context.Background(), opts, &shardOutputMux{}, slowProvision, executor.run)
	}()
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
	if executor.callCount() != 3 {
		t.Fatalf("calls=%d, want 3 successful siblings", executor.callCount())
	}
	if executor.call(2) != nil {
		t.Fatal("executor ran for the shard whose create was refused")
	}
	if _, releases := provisioner.stats(); releases != 3 {
		t.Fatalf("releases=%d, want 3 successful leases released", releases)
	}
	if !strings.Contains(stderr.String(), `shard 2/4 failed error="image not found"`) {
		t.Fatalf("stderr missing create refusal:\n%s", stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "provision failures:") || !strings.Contains(out, "[2/4] error=image not found") {
		t.Fatalf("stdout missing provision failure summary:\n%s", out)
	}
	if !strings.Contains(out, "failed_shards=1") {
		t.Fatalf("stdout missing failed_shards tally:\n%s", out)
	}
}

func TestShardCapacityRetry(t *testing.T) {
	costLimit := coordinatorCreationRejectedError{
		cause:            CoordinatorHTTPError{StatusCode: http.StatusTooManyRequests, Message: `{"error":"cost_limit_exceeded"}`},
		provider:         "aws",
		requestedLeaseID: "cbx_costlimit01",
	}
	cases := []struct {
		name         string
		wait         time.Duration
		err          error
		wantCalls    int
		wantCapacity bool
		wantSubstr   string
	}{
		{name: "default 0 means no retry", wait: 0, err: MarkCapacityRefusal(errors.New("resource_limit_exceeded")), wantCalls: 1, wantCapacity: true},
		{name: "cost_limit not retried", wait: time.Minute, err: costLimit, wantCalls: 1, wantSubstr: "cost_limit_exceeded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			provision := func(context.Context, int, string) (checkpointForkProvision, error) {
				calls++
				return checkpointForkProvision{}, tc.err
			}
			opts := shardTestOptions(1)
			opts.CapacityWait = tc.wait
			opts.capacityBackoff = func(int, error) time.Duration { return time.Millisecond }
			_, err := App{Stdout: io.Discard, Stderr: io.Discard}.provisionShardWithCapacityRetry(context.Background(), &shardOutputMux{}, opts, 1, "slug", provision)
			if err == nil {
				t.Fatal("expected error")
			}
			if tc.wantSubstr != "" && !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("err=%v, want %q", err, tc.wantSubstr)
			}
			if IsCapacityRefusal(err) != tc.wantCapacity {
				t.Fatalf("IsCapacityRefusal=%v want %v", IsCapacityRefusal(err), tc.wantCapacity)
			}
			if calls != tc.wantCalls {
				t.Fatalf("calls=%d, want %d", calls, tc.wantCalls)
			}
		})
	}
}

func TestShardCapacityWaitThenSucceedAndFreshLease(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	var seen []string
	provision := func(context.Context, int, string) (checkpointForkProvision, error) {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		id := NewLeaseID()
		seen = append(seen, id)
		if attempts < 3 {
			return checkpointForkProvision{}, MarkCapacityRefusal(errors.New("InsufficientInstanceCapacity"))
		}
		return checkpointForkProvision{Lease: LeaseTarget{LeaseID: id}, Release: func(context.Context) {}}, nil
	}
	var stderr bytes.Buffer
	opts := shardTestOptions(1)
	opts.CapacityWait = time.Minute
	opts.capacityBackoff = func(int, error) time.Duration { return time.Millisecond }
	lease, err := App{Stdout: io.Discard, Stderr: &stderr}.provisionShardWithCapacityRetry(context.Background(), &shardOutputMux{}, opts, 1, "slug", provision)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != 3 || len(seen) != 3 || seen[0] == seen[1] || seen[1] == seen[2] {
		t.Fatalf("attempts=%d leases=%v", attempts, seen)
	}
	if lease.Lease.LeaseID != seen[2] {
		t.Fatalf("lease=%s, want %s", lease.Lease.LeaseID, seen[2])
	}
	if !strings.Contains(stderr.String(), "waiting for capacity") {
		t.Fatalf("stderr missing wait log:\n%s", stderr.String())
	}
}

func TestShardCapacityWaitExhausted(t *testing.T) {
	provision := func(context.Context, int, string) (checkpointForkProvision, error) {
		return checkpointForkProvision{}, MarkCapacityRefusal(errors.New("AllocationFailed"))
	}
	var stdout, stderr bytes.Buffer
	opts := shardTestOptions(1)
	opts.CapacityWait = 20 * time.Millisecond
	opts.capacityBackoff = func(int, error) time.Duration { return 5 * time.Millisecond }
	wrapped := func(ctx context.Context, index int, slug string) (checkpointForkProvision, error) {
		return App{Stdout: &stdout, Stderr: &stderr}.provisionShardWithCapacityRetry(ctx, &shardOutputMux{}, opts, index, slug, provision)
	}
	err := App{Stdout: &stdout, Stderr: &stderr}.shardRun(context.Background(), opts, &shardOutputMux{}, wrapped, newShardTestExecutor().run)
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 7 {
		t.Fatalf("err=%v, want exit 7", err)
	}
	if !strings.Contains(stdout.String(), "provision failures:") || !strings.Contains(stdout.String(), "capacity wait exhausted") {
		t.Fatalf("stdout missing exhausted provision failure:\n%s", stdout.String())
	}
}

func TestShardCapacityFailFastCancelsWaiter(t *testing.T) {
	enteredWait := make(chan struct{})
	base := newShardTestProvisioner()
	releaseOK := make(chan struct{})
	provision := func(ctx context.Context, index int, slug string) (checkpointForkProvision, error) {
		if index == 2 {
			return checkpointForkProvision{}, MarkCapacityRefusal(errors.New("resource_limit_exceeded"))
		}
		select {
		case <-releaseOK:
		case <-ctx.Done():
			return checkpointForkProvision{}, ctx.Err()
		}
		return base.provision(ctx, index, slug)
	}
	var stderr bytes.Buffer
	opts := shardTestOptions(2)
	opts.Concurrency = 2
	opts.FailFast = true
	opts.CapacityWait = time.Minute
	opts.capacityBackoff = func(int, error) time.Duration {
		select {
		case <-enteredWait:
		default:
			close(enteredWait)
		}
		return time.Hour
	}
	app := App{Stdout: io.Discard, Stderr: &stderr}
	wrapped := func(ctx context.Context, index int, slug string) (checkpointForkProvision, error) {
		return app.provisionShardWithCapacityRetry(ctx, &shardOutputMux{}, opts, index, slug, provision)
	}
	executor := newShardTestExecutor()
	executor.outcomes[1] = recordedOutcome(4, nil)
	done := make(chan error, 1)
	go func() { done <- app.shardRun(context.Background(), opts, &shardOutputMux{}, wrapped, executor.run) }()
	select {
	case <-enteredWait:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for capacity backoff")
	}
	close(releaseOK)
	if err := <-done; err == nil {
		t.Fatal("expected fail-fast error")
	}
	if !strings.Contains(stderr.String(), "waiting for capacity") || !strings.Contains(stderr.String(), "canceled") {
		t.Fatalf("stderr missing wait/cancel:\n%s", stderr.String())
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
		{name: "capacity-wait stays local", args: []string{"--count", "2", "--from", "chk_1", "--capacity-wait", "30s", "--concurrency", "2"}, wantOwn: "--count 2 --from chk_1 --capacity-wait 30s --concurrency 2"},
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
	record, _, err := store.Reserve(checkpointRecord{ID: "chk_shard_cap", Kind: checkpointKindArchive, CreatedAt: time.Now().UTC().Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	err = app.shard(context.Background(), []string{"--count", "3", "--from", record.ID, "--", "pnpm", "test"})
	if err == nil || !strings.Contains(err.Error(), "--count 3 exceeds shard.maxCount 2") {
		t.Fatalf("err=%v", err)
	}
}

func TestShardMaxConcurrencyCap(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "crabbox.yaml")
	if err := os.WriteFile(configPath, []byte("shard:\n  maxConcurrency: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", configPath)
	store, err := defaultCheckpointStore()
	if err != nil {
		t.Fatal(err)
	}
	record, _, err := store.Reserve(checkpointRecord{ID: "chk_shard_conc_cap", Kind: checkpointKindArchive, CreatedAt: time.Now().UTC().Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	err = app.shard(context.Background(), []string{"--count", "4", "--concurrency", "3", "--from", record.ID, "--", "pnpm", "test"})
	if err == nil || !strings.Contains(err.Error(), "--concurrency 3 exceeds shard.maxConcurrency 2") {
		t.Fatalf("err=%v", err)
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

func TestShardBoundsInFlightConcurrency(t *testing.T) {
	provisioner := newShardTestProvisioner()
	executor := newShardTestExecutor()
	executor.started = make(chan int, 8)
	executor.unblock = make(chan struct{})
	var (
		mu       sync.Mutex
		inFlight int
		peak     int
	)
	track := func(delta int) {
		mu.Lock()
		defer mu.Unlock()
		inFlight += delta
		if inFlight > peak {
			peak = inFlight
		}
	}
	provision := func(ctx context.Context, index int, slug string) (checkpointForkProvision, error) {
		track(1)
		defer track(-1)
		return provisioner.provision(ctx, index, slug)
	}
	run := func(ctx context.Context, index int, runArgs []string, outcome *shardRunOutcome) error {
		track(1)
		defer track(-1)
		return executor.run(ctx, index, runArgs, outcome)
	}
	for i := 1; i <= 6; i++ {
		executor.block[i] = true
		executor.outcomes[i] = recordedOutcome(0, nil)
	}
	opts := shardTestOptions(6)
	opts.Concurrency = 2
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	done := make(chan error, 1)
	go func() {
		done <- app.shardRun(context.Background(), opts, &shardOutputMux{}, provision, run)
	}()
	seen := map[int]bool{}
	for len(seen) < 2 {
		select {
		case index := <-executor.started:
			seen[index] = true
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of first 2 shards started", len(seen))
		}
	}
	select {
	case index := <-executor.started:
		t.Fatalf("third shard %d started before a slot was free", index)
	case <-time.After(100 * time.Millisecond):
	}
	close(executor.unblock)
	if err := <-done; err != nil {
		t.Fatalf("shardRun: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if peak > 2 {
		t.Fatalf("peak in-flight=%d, want at most 2", peak)
	}
	if peak < 2 {
		t.Fatalf("peak in-flight=%d, want to observe concurrency 2", peak)
	}
	provisions, releases := provisioner.stats()
	if provisions != 6 || releases != 6 {
		t.Fatalf("provisions=%d releases=%d, want 6/6", provisions, releases)
	}
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
