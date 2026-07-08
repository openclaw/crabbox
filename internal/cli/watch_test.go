package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

const watchTestLeaseID = "cbx_abcdef123456"

type watchTestBackend struct {
	mu           sync.Mutex
	acquireCalls int
	resolveCalls int
	releaseCalls int
	releaseCtx   context.Context
	releaseLease LeaseTarget
	resolveReq   ResolveRequest
	acquireErr   error
	resolveErr   error
	labels       map[string]string
	resolveSSH   string
	features     FeatureSet
}

func newWatchTestBackend() *watchTestBackend {
	return &watchTestBackend{features: FeatureSet{FeatureSSH, FeatureCrabboxSync}}
}

func (b *watchTestBackend) Spec() ProviderSpec {
	return ProviderSpec{Name: "watch-test", Kind: ProviderKindSSHLease, Features: b.features}
}

func (b *watchTestBackend) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.acquireCalls++
	if b.acquireErr != nil {
		return LeaseTarget{}, b.acquireErr
	}
	server := Server{Provider: "watch-test", Labels: map[string]string{"lease": watchTestLeaseID}}
	server.ServerType.Name = "test"
	return LeaseTarget{LeaseID: watchTestLeaseID, Server: server, SSH: SSHTarget{Host: "acquired.example"}}, nil
}

func (b *watchTestBackend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.resolveCalls++
	b.resolveReq = req
	if b.resolveErr != nil {
		return LeaseTarget{}, b.resolveErr
	}
	labels := map[string]string{"lease": req.ID}
	for key, value := range b.labels {
		labels[key] = value
	}
	server := Server{Provider: "watch-test", Labels: labels}
	server.ServerType.Name = "test"
	return LeaseTarget{LeaseID: req.ID, Server: server, SSH: SSHTarget{Host: b.resolveSSH}}, nil
}

func (b *watchTestBackend) RefreshReleaseLeaseTarget(ctx context.Context, lease LeaseTarget) (LeaseTarget, error) {
	return b.Resolve(ctx, ResolveRequest{ID: lease.LeaseID, ReleaseOnly: true})
}

func (b *watchTestBackend) ReleaseLeaseConnectionCleanupSafe() bool { return true }

func (b *watchTestBackend) List(context.Context, ListRequest) ([]LeaseView, error) {
	return nil, nil
}

func (b *watchTestBackend) ReleaseLease(ctx context.Context, req ReleaseLeaseRequest) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.releaseCalls++
	b.releaseCtx = ctx
	b.releaseLease = req.Lease
	return nil
}

func (b *watchTestBackend) Touch(context.Context, TouchRequest) (Server, error) {
	return Server{}, nil
}

func (b *watchTestBackend) counts() (int, int, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.acquireCalls, b.resolveCalls, b.releaseCalls
}

type watchTestExecutor struct {
	mu      sync.Mutex
	calls   [][]string
	started chan struct{}
	release chan struct{}
	errs    map[int]error
}

func newWatchTestExecutor() *watchTestExecutor {
	return &watchTestExecutor{started: make(chan struct{}, 64), errs: map[int]error{}}
}

func (e *watchTestExecutor) run(ctx context.Context, args []string) error {
	e.mu.Lock()
	e.calls = append(e.calls, append([]string{}, args...))
	iteration := len(e.calls)
	err := e.errs[iteration]
	e.mu.Unlock()
	select {
	case e.started <- struct{}{}:
	default:
	}
	if e.release != nil {
		select {
		case <-e.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err != nil {
		return err
	}
	return ctx.Err()
}

func (e *watchTestExecutor) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.calls)
}

func (e *watchTestExecutor) call(index int) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if index >= len(e.calls) {
		return nil
	}
	return e.calls[index]
}

func (e *watchTestExecutor) awaitStart(t *testing.T) {
	t.Helper()
	select {
	case <-e.started:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a watch iteration to start")
	}
}

func watchTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func watchTestWrite(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newWatchGitRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	watchTestGit(t, root, "init", "-q")
	watchTestWrite(t, root, "main.go", "package main\n")
	watchTestWrite(t, root, ".gitignore", "*.log\nignored.txt\n")
	watchTestGit(t, root, "add", ".")
	watchTestGit(t, root, "commit", "-q", "-m", "seed")
	return root
}

func newWatchTestSession(root string, execute watchRunExecutor, debounce, idleExit time.Duration, stderr io.Writer) *watchSession {
	if stderr == nil {
		stderr = io.Discard
	}
	return &watchSession{
		root:     root,
		leaseID:  watchTestLeaseID,
		command:  []string{"echo", "ok"},
		debounce: debounce,
		idleExit: idleExit,
		cfg:      Config{},
		execute:  execute,
		stderr:   stderr,
	}
}

func TestWatchRunsInitialIterationImmediately(t *testing.T) {
	root := newWatchGitRepo(t)
	executor := newWatchTestExecutor()
	session := newWatchTestSession(root, executor.run, 25*time.Millisecond, 300*time.Millisecond, nil)
	session.runArgs = []string{"--shell"}
	if err := session.run(context.Background()); err != nil {
		t.Fatalf("session.run: %v", err)
	}
	if executor.callCount() != 1 {
		t.Fatalf("iterations=%d, want 1", executor.callCount())
	}
	want := []string{"--id", watchTestLeaseID, "--label", "watch #1", "--shell", "--", "echo", "ok"}
	got := executor.call(0)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("iteration args=%v, want %v", got, want)
	}
}

func TestWatchIdleExitAfterQuietPeriod(t *testing.T) {
	root := newWatchGitRepo(t)
	executor := newWatchTestExecutor()
	var stderr bytes.Buffer
	session := newWatchTestSession(root, executor.run, 25*time.Millisecond, 250*time.Millisecond, &stderr)
	start := time.Now()
	if err := session.run(context.Background()); err != nil {
		t.Fatalf("session.run: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Fatalf("idle exit fired too early: %s", elapsed)
	}
	if !strings.Contains(stderr.String(), "idle_exit") {
		t.Fatalf("stderr missing idle_exit line:\n%s", stderr.String())
	}
}

func TestWatchIdleExitWaitsForActiveRun(t *testing.T) {
	root := newWatchGitRepo(t)
	executor := newWatchTestExecutor()
	executor.release = make(chan struct{})
	session := newWatchTestSession(root, executor.run, 25*time.Millisecond, 150*time.Millisecond, nil)
	done := make(chan error, 1)
	go func() { done <- session.run(context.Background()) }()
	executor.awaitStart(t)
	time.Sleep(300 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("session exited before the active run finished: %v", err)
	default:
	}
	close(executor.release)
	if err := <-done; err != nil {
		t.Fatalf("session.run: %v", err)
	}
	if executor.callCount() != 1 {
		t.Fatalf("iterations=%d, want 1", executor.callCount())
	}
}

func TestWatchRerunsOnQualifyingChange(t *testing.T) {
	root := newWatchGitRepo(t)
	executor := newWatchTestExecutor()
	session := newWatchTestSession(root, executor.run, 25*time.Millisecond, 2*time.Second, nil)
	done := make(chan error, 1)
	go func() { done <- session.run(context.Background()) }()
	executor.awaitStart(t)
	watchTestWrite(t, root, "main.go", "package main\nvar x = 1\n")
	executor.awaitStart(t)
	if got := executor.call(1); !strings.Contains(strings.Join(got, " "), "watch #2") {
		t.Fatalf("second iteration args=%v, want watch #2 label", got)
	}
	watchTestWrite(t, root, "main.go", "package main\nvar x = 2\n")
	executor.awaitStart(t)
	if got := executor.call(2); !strings.Contains(strings.Join(got, " "), "watch #3") {
		t.Fatalf("third iteration args=%v, want watch #3 label", got)
	}
	if err := <-done; err != nil {
		t.Fatalf("session.run: %v", err)
	}
}

func TestWatchIgnoredChurnDoesNotTriggerRunOrResetIdle(t *testing.T) {
	root := newWatchGitRepo(t)
	watchTestWrite(t, root, ".crabboxignore", "scratch\n")
	executor := newWatchTestExecutor()
	session := newWatchTestSession(root, executor.run, 25*time.Millisecond, 600*time.Millisecond, nil)
	stop := make(chan struct{})
	var churn sync.WaitGroup
	churn.Add(1)
	go func() {
		defer churn.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
			}
			watchTestWrite(t, root, "node_modules/dep.js", fmt.Sprintf("// %d", i))
			watchTestWrite(t, root, "ignored.txt", fmt.Sprintf("%d", i))
			watchTestWrite(t, root, "scratch/tmp.txt", fmt.Sprintf("%d", i))
		}
	}()
	start := time.Now()
	err := session.run(context.Background())
	elapsed := time.Since(start)
	close(stop)
	churn.Wait()
	if err != nil {
		t.Fatalf("session.run: %v", err)
	}
	if executor.callCount() != 1 {
		t.Fatalf("iterations=%d, want 1", executor.callCount())
	}
	if elapsed > 3*time.Second {
		t.Fatalf("ignored churn kept the session alive for %s", elapsed)
	}
}

func TestWatchWatchesNewlyCreatedDirectories(t *testing.T) {
	root := newWatchGitRepo(t)
	executor := newWatchTestExecutor()
	session := newWatchTestSession(root, executor.run, 25*time.Millisecond, 3*time.Second, nil)
	done := make(chan error, 1)
	go func() { done <- session.run(context.Background()) }()
	executor.awaitStart(t)
	if err := os.MkdirAll(filepath.Join(root, "pkg", "util"), 0o755); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	watchTestWrite(t, root, "pkg/util/util.go", "package util\n")
	executor.awaitStart(t)
	if executor.callCount() < 2 {
		t.Fatalf("iterations=%d, want at least 2", executor.callCount())
	}
	if err := <-done; err != nil {
		t.Fatalf("session.run: %v", err)
	}
}

func TestWatchDebounceCoalescesBurstIntoOneRun(t *testing.T) {
	root := newWatchGitRepo(t)
	executor := newWatchTestExecutor()
	session := newWatchTestSession(root, executor.run, 150*time.Millisecond, 2*time.Second, nil)
	done := make(chan error, 1)
	go func() { done <- session.run(context.Background()) }()
	executor.awaitStart(t)
	time.Sleep(200 * time.Millisecond)
	for i := 0; i < 5; i++ {
		watchTestWrite(t, root, fmt.Sprintf("burst%d.txt", i), "x")
		time.Sleep(10 * time.Millisecond)
	}
	executor.awaitStart(t)
	if err := <-done; err != nil {
		t.Fatalf("session.run: %v", err)
	}
	if executor.callCount() != 2 {
		t.Fatalf("iterations=%d, want 2", executor.callCount())
	}
}

func TestWatchChangeDuringRunQueuesExactlyOnePendingRerun(t *testing.T) {
	root := newWatchGitRepo(t)
	executor := newWatchTestExecutor()
	executor.release = make(chan struct{})
	session := newWatchTestSession(root, executor.run, 25*time.Millisecond, 2*time.Second, nil)
	done := make(chan error, 1)
	go func() { done <- session.run(context.Background()) }()
	executor.awaitStart(t)
	watchTestWrite(t, root, "first.txt", "1")
	time.Sleep(150 * time.Millisecond)
	watchTestWrite(t, root, "second.txt", "2")
	time.Sleep(150 * time.Millisecond)
	executor.release <- struct{}{}
	executor.awaitStart(t)
	executor.release <- struct{}{}
	if err := <-done; err != nil {
		t.Fatalf("session.run: %v", err)
	}
	if executor.callCount() != 2 {
		t.Fatalf("iterations=%d, want 2", executor.callCount())
	}
	if got := executor.call(1); !strings.Contains(strings.Join(got, " "), "watch #2") {
		t.Fatalf("queued iteration args=%v, want watch #2 label", got)
	}
}

func TestWatchRunsQueuedRerunBeforeIdleExit(t *testing.T) {
	root := newWatchGitRepo(t)
	executor := newWatchTestExecutor()
	executor.release = make(chan struct{}, 1)
	session := newWatchTestSession(root, executor.run, 25*time.Millisecond, 250*time.Millisecond, nil)
	done := make(chan error, 1)
	go func() { done <- session.run(context.Background()) }()
	executor.awaitStart(t)
	watchTestWrite(t, root, "queued.txt", "queued while running")
	time.Sleep(600 * time.Millisecond)
	executor.release <- struct{}{}
	executor.awaitStart(t)
	executor.release <- struct{}{}
	if err := <-done; err != nil {
		t.Fatalf("session.run: %v", err)
	}
	if executor.callCount() != 2 {
		t.Fatalf("iterations=%d, want 2 (queued rerun must run before idle exit)", executor.callCount())
	}
	if got := executor.call(1); !strings.Contains(strings.Join(got, " "), "watch #2") {
		t.Fatalf("queued iteration args=%v, want watch #2 label", got)
	}
}

func TestWatchContinuesAfterNonzeroRemoteExit(t *testing.T) {
	root := newWatchGitRepo(t)
	executor := newWatchTestExecutor()
	executor.errs[1] = ExitError{Code: 1, Message: "remote command exited 1"}
	var stderr bytes.Buffer
	session := newWatchTestSession(root, executor.run, 25*time.Millisecond, 2*time.Second, &stderr)
	done := make(chan error, 1)
	go func() { done <- session.run(context.Background()) }()
	executor.awaitStart(t)
	time.Sleep(100 * time.Millisecond)
	watchTestWrite(t, root, "main.go", "package main\nvar y = 1\n")
	executor.awaitStart(t)
	if err := <-done; err != nil {
		t.Fatalf("session.run: %v", err)
	}
	if executor.callCount() != 2 {
		t.Fatalf("iterations=%d, want 2", executor.callCount())
	}
	if !strings.Contains(stderr.String(), "result=nonzero") {
		t.Fatalf("stderr missing nonzero result line:\n%s", stderr.String())
	}
}

func TestWatchStopsOnFatalIterationError(t *testing.T) {
	root := newWatchGitRepo(t)
	executor := newWatchTestExecutor()
	fatal := errors.New("ssh transport failed")
	executor.errs[1] = fatal
	session := newWatchTestSession(root, executor.run, 25*time.Millisecond, 2*time.Second, nil)
	if err := session.run(context.Background()); !errors.Is(err, fatal) {
		t.Fatalf("session.run error=%v, want %v", err, fatal)
	}
	if executor.callCount() != 1 {
		t.Fatalf("iterations=%d, want 1", executor.callCount())
	}
}

func TestWatchReloadsCrabboxIgnoreMidSession(t *testing.T) {
	root := newWatchGitRepo(t)
	executor := newWatchTestExecutor()
	session := newWatchTestSession(root, executor.run, 25*time.Millisecond, 700*time.Millisecond, nil)
	done := make(chan error, 1)
	go func() { done <- session.run(context.Background()) }()
	executor.awaitStart(t)
	watchTestWrite(t, root, ".crabboxignore", "noise.txt\n")
	executor.awaitStart(t)
	stop := make(chan struct{})
	var churn sync.WaitGroup
	churn.Add(1)
	go func() {
		defer churn.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
			}
			watchTestWrite(t, root, "noise.txt", fmt.Sprintf("%d", i))
		}
	}()
	err := <-done
	close(stop)
	churn.Wait()
	if err != nil {
		t.Fatalf("session.run: %v", err)
	}
	if executor.callCount() != 2 {
		t.Fatalf("iterations=%d, want 2", executor.callCount())
	}
}

func TestWatchRewatchesDirectoriesWhenExclusionsRelax(t *testing.T) {
	root := newWatchGitRepo(t)
	watchTestWrite(t, root, ".crabboxignore", "generated\n")
	watchTestWrite(t, root, "generated/seed.txt", "seeded before start")
	executor := newWatchTestExecutor()
	session := newWatchTestSession(root, executor.run, 25*time.Millisecond, 3*time.Second, nil)
	done := make(chan error, 1)
	go func() { done <- session.run(context.Background()) }()
	executor.awaitStart(t)
	time.Sleep(100 * time.Millisecond)
	watchTestWrite(t, root, ".crabboxignore", "")
	executor.awaitStart(t)
	time.Sleep(100 * time.Millisecond)
	watchTestWrite(t, root, "generated/data.txt", "now visible")
	executor.awaitStart(t)
	if err := <-done; err != nil {
		t.Fatalf("session.run: %v", err)
	}
	if executor.callCount() != 3 {
		t.Fatalf("iterations=%d, want 3 (relaxed exclusion must re-attach watches)", executor.callCount())
	}
}

func TestQualifyWatchBatch(t *testing.T) {
	root := newWatchGitRepo(t)
	watchTestWrite(t, root, "keep.log", "tracked but gitignored")
	watchTestGit(t, root, "add", "-f", "keep.log")
	watchTestGit(t, root, "commit", "-q", "-m", "add keep.log")
	watchTestWrite(t, root, ".crabboxignore", "vendor-notes.md\n")
	watchTestWrite(t, root, "untracked.txt", "new")
	watchTestWrite(t, root, "ignored.txt", "ignored")
	watchTestWrite(t, root, "vendor-notes.md", "crabboxignored")
	watchTestWrite(t, root, "deleted.txt", "to delete")
	watchTestGit(t, root, "add", "deleted.txt")
	watchTestGit(t, root, "commit", "-q", "-m", "add deleted.txt")
	if err := os.Remove(filepath.Join(root, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	session := newWatchTestSession(root, nil, time.Millisecond, time.Second, nil)
	for _, tc := range []struct {
		name string
		path string
		want bool
	}{
		{name: "tracked modified", path: "main.go", want: true},
		{name: "untracked new", path: "untracked.txt", want: true},
		{name: "gitignored", path: "ignored.txt", want: false},
		{name: "tracked but gitignored", path: "keep.log", want: true},
		{name: "crabboxignored", path: "vendor-notes.md", want: false},
		{name: "deleted tracked", path: "deleted.txt", want: true},
		{name: "deleted untracked ignored", path: "gone.log", want: false},
		{name: "deleted untracked nonignored", path: "gone.txt", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			qualified, err := session.qualifyBatch([]string{tc.path})
			if err != nil {
				t.Fatalf("qualifyBatch(%q): %v", tc.path, err)
			}
			got := len(qualified) > 0
			if got != tc.want {
				t.Fatalf("qualifyBatch(%q)=%v, want qualified=%v", tc.path, qualified, tc.want)
			}
		})
	}
}

func TestQualifyWatchBatchHonorsSyncIncludes(t *testing.T) {
	root := newWatchGitRepo(t)
	watchTestWrite(t, root, "src/app.go", "package app\n")
	watchTestWrite(t, root, "docs/readme.md", "docs\n")
	watchTestWrite(t, root, "src/gone.go", "package app\n")
	watchTestWrite(t, root, "docs/gone.md", "docs\n")
	watchTestGit(t, root, "add", "src", "docs")
	watchTestGit(t, root, "commit", "-q", "-m", "add src and docs")
	for _, rel := range []string{"src/gone.go", "docs/gone.md"} {
		if err := os.Remove(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Fatal(err)
		}
	}
	watchTestWrite(t, root, "src/new.txt", "new")
	watchTestWrite(t, root, "notes.txt", "outside")
	session := newWatchTestSession(root, nil, time.Millisecond, time.Second, nil)
	session.cfg.Sync.Includes = []string{"src"}
	for _, tc := range []struct {
		name string
		path string
		want bool
	}{
		{name: "tracked inside include", path: "src/app.go", want: true},
		{name: "untracked inside include", path: "src/new.txt", want: true},
		{name: "tracked outside include", path: "docs/readme.md", want: false},
		{name: "untracked outside include", path: "notes.txt", want: false},
		{name: "root file outside include", path: "main.go", want: false},
		{name: "deleted tracked inside include", path: "src/gone.go", want: true},
		{name: "deleted tracked outside include", path: "docs/gone.md", want: false},
		{name: "deleted untracked outside include", path: "gone.txt", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			qualified, err := session.qualifyBatch([]string{tc.path})
			if err != nil {
				t.Fatalf("qualifyBatch(%q): %v", tc.path, err)
			}
			got := len(qualified) > 0
			if got != tc.want {
				t.Fatalf("qualifyBatch(%q)=%v, want qualified=%v", tc.path, qualified, tc.want)
			}
		})
	}
}

func TestWatchGitPathsTreatsEventNamesLiterally(t *testing.T) {
	root := newWatchGitRepo(t)
	watchTestWrite(t, root, "*.go", "literal filename\n")

	got, err := watchGitPaths(root, []string{"*.go"}, "ls-files", "--cached", "--others", "--exclude-standard", "-z", "--")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "*.go" {
		t.Fatalf("paths=%q, want only the literal event path", got)
	}
}

func TestWatchIgnoresUnchangedMetadataEvents(t *testing.T) {
	root := newWatchGitRepo(t)
	path := filepath.Join(root, "main.go")
	session := &watchSession{root: root}
	session.rememberPath("main.go", path)

	batch := map[string]struct{}{}
	if session.observeEvent(nil, fsnotify.Event{Name: path, Op: fsnotify.Chmod}, batch) {
		t.Fatal("unchanged metadata event qualified")
	}
	if len(batch) != 0 {
		t.Fatalf("batch=%v, want empty", batch)
	}
}

func TestWatchKeepsExecutableModeChanges(t *testing.T) {
	root := newWatchGitRepo(t)
	path := filepath.Join(root, "main.go")
	session := &watchSession{root: root}
	session.rememberPath("main.go", path)

	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	batch := map[string]struct{}{}
	if !session.observeEvent(nil, fsnotify.Event{Name: path, Op: fsnotify.Chmod}, batch) {
		t.Fatal("executable mode change did not qualify")
	}
	if _, ok := batch["main.go"]; !ok {
		t.Fatalf("batch=%v, want main.go", batch)
	}
}

func TestWatchIncludeWhitelistFiltersReruns(t *testing.T) {
	root := newWatchGitRepo(t)
	watchTestWrite(t, root, "src/app.go", "package app\n")
	watchTestGit(t, root, "add", "src")
	watchTestGit(t, root, "commit", "-q", "-m", "add src")
	executor := newWatchTestExecutor()
	session := newWatchTestSession(root, executor.run, 25*time.Millisecond, 900*time.Millisecond, nil)
	session.cfg.Sync.Includes = []string{"src"}
	done := make(chan error, 1)
	go func() { done <- session.run(context.Background()) }()
	executor.awaitStart(t)
	time.Sleep(100 * time.Millisecond)
	watchTestWrite(t, root, "outside.txt", "not synced")
	time.Sleep(200 * time.Millisecond)
	if executor.callCount() != 1 {
		t.Fatalf("iterations=%d after outside-include churn, want 1", executor.callCount())
	}
	watchTestWrite(t, root, "src/app.go", "package app\nvar x = 1\n")
	executor.awaitStart(t)
	if err := <-done; err != nil {
		t.Fatalf("session.run: %v", err)
	}
	if executor.callCount() != 2 {
		t.Fatalf("iterations=%d, want 2", executor.callCount())
	}
}

func TestWatchAcquiresLeaseWhenNoID(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_COORDINATOR", "")
	root := newWatchGitRepo(t)
	backend := newWatchTestBackend()
	executor := newWatchTestExecutor()
	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	opts := watchOptions{Debounce: 25 * time.Millisecond, IdleExit: 250 * time.Millisecond, IdleExitSet: true, Command: []string{"echo", "ok"}}
	cfg := Config{Provider: "watch-test", IdleTimeout: time.Minute, TTL: time.Hour}
	err := app.watchWithBackend(context.Background(), opts, Repo{Root: root, Name: "my-app"}, cfg, backend, executor.run)
	if err != nil {
		t.Fatalf("watchWithBackend: %v", err)
	}
	acquires, resolves, releases := backend.counts()
	if acquires != 1 || resolves != 1 || releases != 1 {
		t.Fatalf("acquire=%d resolve=%d release=%d, want 1/1/1", acquires, resolves, releases)
	}
	if !strings.Contains(stdout.String(), "leased "+watchTestLeaseID) {
		t.Fatalf("stdout missing leased line:\n%s", stdout.String())
	}
	if got := executor.call(0); !strings.Contains(strings.Join(got, " "), "--id "+watchTestLeaseID) {
		t.Fatalf("iteration args=%v, want injected --id", got)
	}
}

func TestWatchRefreshesReleaseAuthorizationAfterIterations(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_COORDINATOR", "")
	root := newWatchGitRepo(t)
	backend := newWatchTestBackend()
	backend.labels = map[string]string{"claim_revision": "after-run"}
	backend.resolveSSH = "refreshed.example"
	executor := newWatchTestExecutor()
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	opts := watchOptions{Debounce: 25 * time.Millisecond, IdleExit: 250 * time.Millisecond, IdleExitSet: true, Command: []string{"echo", "ok"}}
	cfg := Config{Provider: "watch-test", IdleTimeout: time.Minute, TTL: time.Hour}

	if err := app.watchWithBackend(context.Background(), opts, Repo{Root: root, Name: "my-app"}, cfg, backend, executor.run); err != nil {
		t.Fatalf("watchWithBackend: %v", err)
	}

	backend.mu.Lock()
	defer backend.mu.Unlock()
	if !backend.resolveReq.ReleaseOnly || backend.resolveReq.ID != watchTestLeaseID {
		t.Fatalf("resolve request=%+v, want release-only refresh", backend.resolveReq)
	}
	if got := backend.releaseLease.Server.Labels["claim_revision"]; got != "after-run" {
		t.Fatalf("released claim revision=%q, want refreshed snapshot", got)
	}
	if got := backend.releaseLease.SSH.Host; got != "refreshed.example" {
		t.Fatalf("released SSH host=%q, want refreshed connection metadata", got)
	}
}

func TestWatchAttemptsReleaseWithAcquiredTargetWhenRefreshFails(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_COORDINATOR", "")
	root := newWatchGitRepo(t)
	backend := newWatchTestBackend()
	backend.resolveErr = errors.New("temporary inventory failure")
	executor := newWatchTestExecutor()
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	opts := watchOptions{Debounce: 25 * time.Millisecond, IdleExit: 250 * time.Millisecond, IdleExitSet: true, Command: []string{"echo", "ok"}}
	cfg := Config{Provider: "watch-test", IdleTimeout: time.Minute, TTL: time.Hour}

	if err := app.watchWithBackend(context.Background(), opts, Repo{Root: root, Name: "my-app"}, cfg, backend, executor.run); err != nil {
		t.Fatalf("watchWithBackend: %v", err)
	}

	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.releaseCalls != 1 || backend.releaseLease.LeaseID != watchTestLeaseID || backend.releaseLease.SSH.Host != "acquired.example" {
		t.Fatalf("release calls=%d lease=%+v, want acquired target release attempt", backend.releaseCalls, backend.releaseLease)
	}
}

func TestWatchWithIDResolvesAndNeverReleases(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_COORDINATOR", "")
	root := newWatchGitRepo(t)
	backend := newWatchTestBackend()
	backend.labels = map[string]string{"idle_timeout_secs": "1800"}
	executor := newWatchTestExecutor()
	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	opts := watchOptions{LeaseID: watchTestLeaseID, Debounce: 25 * time.Millisecond, IdleExit: 250 * time.Millisecond, IdleExitSet: true, Command: []string{"echo", "ok"}}
	cfg := Config{Provider: "watch-test", IdleTimeout: time.Minute, TTL: time.Hour}
	err := app.watchWithBackend(context.Background(), opts, Repo{Root: root, Name: "my-app"}, cfg, backend, executor.run)
	if err != nil {
		t.Fatalf("watchWithBackend: %v", err)
	}
	acquires, resolves, releases := backend.counts()
	if acquires != 0 || resolves != 1 || releases != 0 {
		t.Fatalf("acquire=%d resolve=%d release=%d, want 0/1/0", acquires, resolves, releases)
	}
	if !strings.Contains(stdout.String(), "idle_timeout=30m0s") {
		t.Fatalf("stdout missing label-derived idle timeout:\n%s", stdout.String())
	}
}

func TestWatchKeepSkipsRelease(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_COORDINATOR", "")
	root := newWatchGitRepo(t)
	backend := newWatchTestBackend()
	executor := newWatchTestExecutor()
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	opts := watchOptions{Keep: true, Debounce: 25 * time.Millisecond, IdleExit: 250 * time.Millisecond, IdleExitSet: true, Command: []string{"echo", "ok"}}
	cfg := Config{Provider: "watch-test", IdleTimeout: time.Minute, TTL: time.Hour}
	if err := app.watchWithBackend(context.Background(), opts, Repo{Root: root, Name: "my-app"}, cfg, backend, executor.run); err != nil {
		t.Fatalf("watchWithBackend: %v", err)
	}
	acquires, _, releases := backend.counts()
	if acquires != 1 || releases != 0 {
		t.Fatalf("acquire=%d release=%d, want 1/0", acquires, releases)
	}
}

func TestWatchCtrlCReleasesFreshLeaseWithBackgroundContext(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_COORDINATOR", "")
	root := newWatchGitRepo(t)
	backend := newWatchTestBackend()
	executor := newWatchTestExecutor()
	executor.release = make(chan struct{})
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	opts := watchOptions{Debounce: 25 * time.Millisecond, IdleExit: 10 * time.Second, IdleExitSet: true, Command: []string{"echo", "ok"}}
	cfg := Config{Provider: "watch-test", IdleTimeout: time.Minute, TTL: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- app.watchWithBackend(ctx, opts, Repo{Root: root, Name: "my-app"}, cfg, backend, executor.run)
	}()
	executor.awaitStart(t)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("watchWithBackend: %v", err)
	}
	_, _, releases := backend.counts()
	if releases != 1 {
		t.Fatalf("release=%d, want 1", releases)
	}
	if backend.releaseCtx == nil || backend.releaseCtx.Err() != nil {
		t.Fatalf("release context canceled: %v", backend.releaseCtx)
	}
}

func TestWatchReleasesLeaseWhenWatcherSetupFails(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_COORDINATOR", "")
	root := newWatchGitRepo(t)
	if err := os.MkdirAll(filepath.Join(root, ".crabboxignore"), 0o755); err != nil {
		t.Fatal(err)
	}
	backend := newWatchTestBackend()
	executor := newWatchTestExecutor()
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	opts := watchOptions{Debounce: 25 * time.Millisecond, IdleExit: 250 * time.Millisecond, IdleExitSet: true, Command: []string{"echo", "ok"}}
	cfg := Config{Provider: "watch-test", IdleTimeout: time.Minute, TTL: time.Hour}
	err := app.watchWithBackend(context.Background(), opts, Repo{Root: root, Name: "my-app"}, cfg, backend, executor.run)
	if err == nil {
		t.Fatal("watchWithBackend succeeded, want setup failure")
	}
	if executor.callCount() != 0 {
		t.Fatalf("iterations=%d, want 0", executor.callCount())
	}
	_, _, releases := backend.counts()
	if releases != 1 {
		t.Fatalf("release=%d, want 1", releases)
	}
}

func TestWatchReleasesLeaseOnFatalIterationError(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_COORDINATOR", "")
	root := newWatchGitRepo(t)
	backend := newWatchTestBackend()
	executor := newWatchTestExecutor()
	fatal := errors.New("sync failed")
	executor.errs[1] = fatal
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	opts := watchOptions{Debounce: 25 * time.Millisecond, IdleExit: 250 * time.Millisecond, IdleExitSet: true, Command: []string{"echo", "ok"}}
	cfg := Config{Provider: "watch-test", IdleTimeout: time.Minute, TTL: time.Hour}
	err := app.watchWithBackend(context.Background(), opts, Repo{Root: root, Name: "my-app"}, cfg, backend, executor.run)
	if !errors.Is(err, fatal) {
		t.Fatalf("watchWithBackend error=%v, want %v", err, fatal)
	}
	_, _, releases := backend.counts()
	if releases != 1 {
		t.Fatalf("release=%d, want 1", releases)
	}
}

func TestWatchBackendGate(t *testing.T) {
	if _, err := watchBackendGate(newWatchTestBackend()); err != nil {
		t.Fatalf("crabbox-sync ssh lease backend rejected: %v", err)
	}
	noSync := newWatchTestBackend()
	noSync.features = FeatureSet{FeatureSSH}
	var exitErr ExitError
	if _, err := watchBackendGate(noSync); !AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("backend without crabbox-sync: err=%v, want exit code 2", err)
	}
	if _, err := watchBackendGate(prewarmDelegatedTestBackend{}); !AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("delegated backend: err=%v, want exit code 2", err)
	}
}

func TestWatchEffectiveIdleExit(t *testing.T) {
	for _, tc := range []struct {
		name        string
		opts        watchOptions
		idleTimeout time.Duration
		want        time.Duration
		wantErr     bool
	}{
		{name: "defaults to idle timeout", opts: watchOptions{}, idleTimeout: 30 * time.Minute, want: 30 * time.Minute},
		{name: "explicit below timeout", opts: watchOptions{IdleExit: 5 * time.Minute, IdleExitSet: true}, idleTimeout: 30 * time.Minute, want: 5 * time.Minute},
		{name: "explicit equal to timeout", opts: watchOptions{IdleExit: 30 * time.Minute, IdleExitSet: true}, idleTimeout: 30 * time.Minute, want: 30 * time.Minute},
		{name: "explicit above timeout", opts: watchOptions{IdleExit: time.Hour, IdleExitSet: true}, idleTimeout: 30 * time.Minute, wantErr: true},
		{name: "explicit zero", opts: watchOptions{IdleExit: 0, IdleExitSet: true}, idleTimeout: 30 * time.Minute, wantErr: true},
		{name: "explicit negative", opts: watchOptions{IdleExit: -time.Minute, IdleExitSet: true}, idleTimeout: 30 * time.Minute, wantErr: true},
		{name: "nonpositive idle timeout", opts: watchOptions{}, idleTimeout: 0, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := watchEffectiveIdleExit(tc.opts, tc.idleTimeout)
			if tc.wantErr {
				var exitErr ExitError
				if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
					t.Fatalf("err=%v, want exit code 2", err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %s err=%v, want %s", got, err, tc.want)
			}
		})
	}
}

func TestWatchRejectsConflictingRunFlags(t *testing.T) {
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	for _, args := range [][]string{
		{"--pool", "ready", "--", "echo", "ok"},
		{"--pool-return", "auto", "--", "echo", "ok"},
		{"--sync-only", "--", "echo", "ok"},
		{"--no-sync", "--", "echo", "ok"},
		{"--apply-local-patch", "--", "echo", "ok"},
		{"--fresh-pr=123", "--", "echo", "ok"},
		{"--script", "run.sh", "--", "echo", "ok"},
		{"--script-stdin", "--", "echo", "ok"},
		{"--stop-after", "success", "--", "echo", "ok"},
		{"--lease-output", "lease.json", "--", "echo", "ok"},
		{"--keep-on-failure", "--", "echo", "ok"},
		{"--capture-stdout=out.log", "--", "echo", "ok"},
		{"--capture-stderr", "err.log", "--", "echo", "ok"},
		{"--download", "remote=local", "--", "echo", "ok"},
		{"--emit-proof", "proof.md", "--", "echo", "ok"},
		{"--proof-template", "default", "--", "echo", "ok"},
		{"-label", "custom", "--", "echo", "ok"},
	} {
		t.Run(args[0], func(t *testing.T) {
			err := app.watch(context.Background(), args)
			var exitErr ExitError
			if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
				t.Fatalf("watch(%v) err=%v, want exit code 2", args, err)
			}
			flagName := strings.TrimLeft(strings.SplitN(args[0], "=", 2)[0], "-")
			if !strings.Contains(exitErr.Message, "--"+flagName) {
				t.Fatalf("error %q does not name flag %q", exitErr.Message, flagName)
			}
		})
	}
}

func TestWatchUsageValidation(t *testing.T) {
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "missing command", args: []string{}, want: "usage: crabbox watch"},
		{name: "missing command after flags", args: []string{"--debounce", "1s"}, want: "usage: crabbox watch"},
		{name: "empty command", args: []string{"--"}, want: "usage: crabbox watch"},
		{name: "positional before separator", args: []string{"echo", "ok"}, want: "place the command after --"},
		{name: "zero debounce", args: []string{"--debounce", "0s", "--", "echo", "ok"}, want: "--debounce must be positive"},
		{name: "negative debounce", args: []string{"--debounce=-1s", "--", "echo", "ok"}, want: "--debounce must be positive"},
		{name: "slug with id", args: []string{"--id", watchTestLeaseID, "--slug", "my-slug", "--", "echo", "ok"}, want: "--slug only applies when creating a new lease"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := app.watch(context.Background(), tc.args)
			var exitErr ExitError
			if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
				t.Fatalf("watch(%v) err=%v, want exit code 2", tc.args, err)
			}
			if !strings.Contains(exitErr.Message, tc.want) {
				t.Fatalf("error %q missing %q", exitErr.Message, tc.want)
			}
		})
	}
}

func TestWatchAllowsCommandlessPreset(t *testing.T) {
	err := (App{Stdout: io.Discard, Stderr: io.Discard}).watch(context.Background(), []string{
		"--provider", "e2b",
		"--preset", "qa",
		"--",
	})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 || !strings.Contains(exitErr.Message, "does not support watch") {
		t.Fatalf("watch error=%v, want commandless preset to pass usage validation", err)
	}
}

func TestWatchValidatesSelectedProfileBeforeLoadingProvider(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), ".crabbox.yaml")
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	if err := os.WriteFile(cfgPath, []byte(`
profiles:
  invalid:
    doctor:
      enabled: true
      tools: [not-a-real-tool]
`), 0o600); err != nil {
		t.Fatal(err)
	}

	err := (App{Stdout: io.Discard, Stderr: io.Discard}).watch(context.Background(), []string{
		"--provider", "e2b",
		"--profile", "invalid",
		"--", "true",
	})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 || !strings.Contains(exitErr.Message, "unknown preflight tool") {
		t.Fatalf("watch error=%v, want profile validation before provider loading", err)
	}
}

func TestPartitionWatchRunArgs(t *testing.T) {
	newFS := func() *flag.FlagSet {
		fs := newFlagSet("watch", io.Discard)
		fs.String("id", "", "")
		fs.Bool("keep", false, "")
		fs.Duration("debounce", 0, "")
		fs.Duration("idle-exit", 0, "")
		fs.String("slug", "", "")
		fs.String("preset", "", "")
		fs.String("provider", "", "")
		fs.Bool("desktop", false, "")
		return fs
	}
	for _, tc := range []struct {
		name      string
		args      []string
		wantWatch string
		wantRun   string
		wantErr   string
	}{
		{name: "empty", args: nil, wantWatch: "", wantRun: ""},
		{name: "owned separated value", args: []string{"--id", "cbx_x"}, wantWatch: "--id cbx_x", wantRun: ""},
		{name: "owned equals value", args: []string{"--debounce=1s"}, wantWatch: "--debounce=1s", wantRun: ""},
		{name: "owned bool", args: []string{"--keep"}, wantWatch: "--keep", wantRun: ""},
		{name: "slug never forwarded", args: []string{"--slug", "fast-crab"}, wantWatch: "--slug fast-crab", wantRun: ""},
		{name: "preset forwarded and parsed", args: []string{"--preset", "qa"}, wantWatch: "--preset qa", wantRun: "--preset qa"},
		{name: "shared goes to both", args: []string{"--provider", "hetzner"}, wantWatch: "--provider hetzner", wantRun: "--provider hetzner"},
		{name: "shared bool goes to both", args: []string{"--desktop"}, wantWatch: "--desktop", wantRun: "--desktop"},
		{name: "unknown forwarded with value", args: []string{"--junit", "results.xml"}, wantWatch: "", wantRun: "--junit results.xml"},
		{name: "unknown bool then flag", args: []string{"--results-auto", "--provider", "aws"}, wantWatch: "--provider aws", wantRun: "--results-auto --provider aws"},
		{name: "order preserved", args: []string{"--shell", "--id", "cbx_x", "--allow-env", "CI"}, wantWatch: "--id cbx_x", wantRun: "--shell --allow-env CI"},
		{name: "single dash owned", args: []string{"-id", "cbx_x"}, wantWatch: "-id cbx_x", wantRun: ""},
		{name: "forbidden", args: []string{"--sync-only"}, wantErr: "--sync-only cannot be used with watch"},
		{name: "forbidden equals", args: []string{"--label=x"}, wantErr: "--label cannot be used with watch"},
		{name: "forbidden after unknown bool", args: []string{"--results-auto", "--no-sync"}, wantErr: "--no-sync cannot be used with watch"},
		{name: "positional", args: []string{"echo"}, wantErr: "place the command after --"},
		{name: "owned missing value", args: []string{"--id"}, wantErr: "flag needs an argument"},
		{name: "bare dashes", args: []string{"---"}, wantErr: "invalid flag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			watchArgs, runArgs, err := partitionWatchRunArgs(newFS(), tc.args)
			if tc.wantErr != "" {
				var exitErr ExitError
				if !AsExitError(err, &exitErr) || exitErr.Code != 2 || !strings.Contains(exitErr.Message, tc.wantErr) {
					t.Fatalf("err=%v, want exit code 2 containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("partition: %v", err)
			}
			if got := strings.Join(watchArgs, " "); got != tc.wantWatch {
				t.Fatalf("watchArgs=%q, want %q", got, tc.wantWatch)
			}
			if got := strings.Join(runArgs, " "); got != tc.wantRun {
				t.Fatalf("runArgs=%q, want %q", got, tc.wantRun)
			}
		})
	}
}

func TestIsWatchIterationResult(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "remote nonzero", err: ExitError{Code: 1, Message: "remote command exited 1"}, want: true},
		{name: "junit failures", err: ExitError{Code: 1, Message: "JUnit results contain 2 failures and 0 errors"}, want: true},
		{name: "sync too large", err: ExitError{Code: 6, Message: "sync manifest too large: 12 files >= limit 10"}, want: false},
		{name: "plain error", err: errors.New("ssh transport failed"), want: false},
		{name: "nil", err: nil, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isWatchIterationResult(tc.err); got != tc.want {
				t.Fatalf("isWatchIterationResult=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestWatchHelpExitsZero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	err := app.Run(context.Background(), []string{"watch", "--help"})
	var exitErr ExitError
	if err != nil && (!AsExitError(err, &exitErr) || exitErr.Code != 0) {
		t.Fatalf("watch --help err=%v, want exit code 0", err)
	}
	combined := stdout.String() + stderr.String()
	if !strings.Contains(combined, "Usage:") || !strings.Contains(combined, "crabbox watch [flags] -- <command...>") {
		t.Fatalf("help output missing usage:\n%s", combined)
	}
}

func TestTopLevelHelpListsWatch(t *testing.T) {
	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	app.printHelp()
	if !strings.Contains(stdout.String(), "\n  watch       ") {
		t.Fatal("top-level help does not list watch")
	}
}
