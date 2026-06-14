package agentsandbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestWarmupCreatesClaimAndPersistsLocalLease(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)

	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: t.TempDir()}, RequestedSlug: "feature-branch"}); err != nil {
		t.Fatal(err)
	}
	if fake.creates != 1 {
		t.Fatalf("creates=%d", fake.creates)
	}
	if !strings.Contains(backend.rt.Stdout.(*bytes.Buffer).String(), "claim=crabbox-feature-branch-") {
		t.Fatalf("stdout=%q", backend.rt.Stdout)
	}
	if !strings.Contains(backend.rt.Stderr.(*bytes.Buffer).String(), "warmup keeps the claim") {
		t.Fatalf("stderr=%q", backend.rt.Stderr)
	}
	claims, err := listAgentSandboxLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	claim, ok := claimBySlug(claims, "feature-branch")
	if !ok {
		t.Fatalf("claims=%#v", claims)
	}
	if claim.Provider != providerName || claim.Slug != "feature-branch" || claim.ProviderScope != claimScope(cfg) {
		t.Fatalf("claim=%#v", claim)
	}
	if !strings.HasPrefix(claim.Labels[claimLabelClaimName], "crabbox-feature-branch-") || claim.Labels[claimLabelClaimUID] == "" || claim.Labels[claimLabelPodName] == "" {
		t.Fatalf("claim labels=%#v", claim.Labels)
	}
}

func TestListReportsPendingClaimAsNotReady(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repo, RequestedSlug: "pending"}); err != nil {
		t.Fatal(err)
	}
	claims, err := listAgentSandboxLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	claim, ok := claimBySlug(claims, "pending")
	if !ok {
		t.Fatalf("claims=%#v", claims)
	}
	sandboxName := claim.Labels[claimLabelSandboxName]
	sandbox := fake.objects[sandboxResource+"/"+cfg.AgentSandbox.Namespace+"/"+sandboxName]
	sandbox.Status.Conditions = []conditionState{{Type: "Ready", Status: "False", Reason: "Starting"}}

	views, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range views {
		if view.Labels["slug"] == "pending" {
			if view.Status != "not-ready" && view.Labels["state"] != "not-ready" {
				t.Fatalf("pending claim view=%#v", view)
			}
			return
		}
	}
	t.Fatalf("pending claim absent from views=%#v", views)
}

func TestListDisplayedClaimIDResolvesForStatus(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repo, RequestedSlug: "listed"}); err != nil {
		t.Fatal(err)
	}
	views, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var view LeaseView
	for _, candidate := range views {
		if candidate.Labels["slug"] == "listed" {
			view = candidate
			break
		}
	}
	if !strings.HasPrefix(view.CloudID, "crabbox-listed-") {
		t.Fatalf("views=%#v", views)
	}
	status, err := backend.Status(context.Background(), StatusRequest{ID: view.CloudID})
	if err != nil {
		t.Fatal(err)
	}
	if status.ServerID != view.CloudID || !status.Ready {
		t.Fatalf("status=%#v view=%#v", status, view)
	}
}

func TestRunSyncOnlyUploadsArchiveThroughPodExecTar(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)

	result, err := backend.Run(context.Background(), RunRequest{Repo: repo, Keep: true, SyncOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || !result.SyncDelegated {
		t.Fatalf("result=%#v", result)
	}
	if fake.deletes != 0 {
		t.Fatalf("kept run deleted claim")
	}
	var tarExec *podExecRequest
	for i := range fake.execs {
		if len(fake.execs[i].Command) >= 1 && fake.execs[i].Command[0] == "tar" {
			tarExec = &fake.execs[i]
			break
		}
	}
	if tarExec == nil {
		t.Fatalf("no tar exec recorded: %#v", fake.execs)
	}
	commandText := strings.Join(tarExec.Command, " ")
	if tarExec.Pod == "" || tarExec.Namespace != cfg.AgentSandbox.Namespace || !strings.HasPrefix(commandText, "tar -xzf - -C /workspace/") {
		t.Fatalf("tar exec=%#v", *tarExec)
	}
	files := tarFiles(t, fake.execInput[len(fake.execInput)-1])
	if !files["go.mod"] || !files["pkg/example.txt"] {
		t.Fatalf("archive files=%#v", files)
	}
}

func TestRunMapsRemoteExitStatus(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	fake.execErrs = []error{
		nil,
		testExitError{code: 42},
	}
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)

	result, err := backend.Run(context.Background(), RunRequest{Repo: repo, Keep: true, NoSync: true, Command: []string{"false"}})
	if err == nil {
		t.Fatal("nonzero remote exit returned nil error")
	}
	if result.ExitCode != 42 {
		t.Fatalf("exit=%d err=%v", result.ExitCode, err)
	}
	if !strings.Contains(err.Error(), "exited 42") {
		t.Fatalf("err=%v", err)
	}
}

func TestRunKeepOnFailureHintsPreserveProviderRoute(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	cfg.AgentSandbox.Kubectl = "/opt/bin/kubectl"
	cfg.AgentSandbox.Kubeconfig = "/tmp/cluster config"
	cfg.AgentSandbox.Context = "agent context"
	cfg.AgentSandbox.Namespace = "sandbox-ns"
	cfg.AgentSandbox.WarmPool = "linux-pool"
	cfg.AgentSandbox.Container = "worker"
	cfg.AgentSandbox.Workdir = "/workspace/my app"
	fake := readyFakeClient(cfg)
	fake.execErrs = []error{nil, testExitError{code: 42}}
	backend := testBackend(cfg, fake, nil, nil)

	result, err := backend.Run(context.Background(), RunRequest{
		Repo:          testGitRepo(t),
		KeepOnFailure: true,
		NoSync:        true,
		Command:       []string{"false"},
	})
	if err == nil || result.ExitCode != 42 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	output := backend.rt.Stderr.(*bytes.Buffer).String()
	for _, want := range []string{
		"rerun:",
		"stop:",
		"'--agent-sandbox-kubectl' '/opt/bin/kubectl'",
		"'--agent-sandbox-kubeconfig' '/tmp/cluster config'",
		"'--agent-sandbox-context' 'agent context'",
		"'--agent-sandbox-namespace' 'sandbox-ns'",
		"'--agent-sandbox-warm-pool' 'linux-pool'",
		"'--agent-sandbox-container' 'worker'",
		"'--agent-sandbox-workdir' '/workspace/my app'",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
}

func TestRunFailsWhenDefaultOneShotCleanupFails(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	fake.deleteErrs = []error{errors.New("delete failed")}
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)

	result, err := backend.Run(context.Background(), RunRequest{Repo: repo, NoSync: true, TimingJSON: true, Command: []string{"true"}})
	if err == nil || !strings.Contains(err.Error(), "delete failed") {
		t.Fatalf("expected cleanup error, got result=%#v err=%v", result, err)
	}
	if result.ExitCode != 1 {
		t.Fatalf("cleanup failure should mark nonzero result: %#v", result)
	}
	if got := backend.rt.Stderr.(*bytes.Buffer).String(); !strings.Contains(got, `"exitCode":1`) {
		t.Fatalf("timing JSON did not report cleanup failure: %s", got)
	}
}

func TestRunForwardsEnvThroughStdinNotArgv(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)
	secret := "super-secret-token"
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    repo,
		Keep:    true,
		NoSync:  true,
		Command: []string{"printenv", "API_TOKEN"},
		Env:     map[string]string{"API_TOKEN": secret},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("result=%#v", result)
	}
	if len(fake.execs) < 2 {
		t.Fatalf("execs=%#v", fake.execs)
	}
	commandExec := fake.execs[len(fake.execs)-1]
	if strings.Contains(strings.Join(commandExec.Command, " "), secret) {
		t.Fatalf("secret leaked into argv: %#v", commandExec.Command)
	}
	if len(fake.execInput) == 0 || !bytes.Contains(fake.execInput[len(fake.execInput)-1], []byte(secret)) {
		t.Fatalf("script was not streamed on stdin")
	}
}

func TestRunExistingLeaseRejectsDifferentRepoWithoutReclaim(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repoA := testGitRepo(t)
	repoB := testGitRepo(t)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repoA, RequestedSlug: "repo-a"}); err != nil {
		t.Fatal(err)
	}
	claims, err := listAgentSandboxLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	claim, ok := claimBySlug(claims, "repo-a")
	if !ok {
		t.Fatalf("claims=%#v", claims)
	}
	_, err = backend.Run(context.Background(), RunRequest{Repo: repoB, ID: claim.LeaseID, NoSync: true, Command: []string{"true"}})
	if err == nil || !strings.Contains(err.Error(), "use --reclaim") {
		t.Fatalf("expected repo ownership error, got %v", err)
	}
}

func TestRunExistingLeaseReclaimRefreshesNewRepo(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repoA := testGitRepo(t)
	repoB := testGitRepo(t)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repoA, RequestedSlug: "reclaim-me"}); err != nil {
		t.Fatal(err)
	}
	claims, err := listAgentSandboxLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	claim, ok := claimBySlug(claims, "reclaim-me")
	if !ok {
		t.Fatalf("claims=%#v", claims)
	}
	result, err := backend.Run(context.Background(), RunRequest{Repo: repoB, ID: claim.LeaseID, Reclaim: true, Keep: true, NoSync: true, Command: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("result=%#v", result)
	}
	updated, err := readLeaseClaim(claim.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.RepoRoot != repoB.Root {
		t.Fatalf("repo root=%q want %q", updated.RepoRoot, repoB.Root)
	}
}

func TestRunFailedValidationDoesNotPersistReclaim(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repoA := testGitRepo(t)
	repoB := testGitRepo(t)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repoA, RequestedSlug: "failed-reclaim"}); err != nil {
		t.Fatal(err)
	}
	claims, err := listAgentSandboxLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	claim, ok := claimBySlug(claims, "failed-reclaim")
	if !ok {
		t.Fatalf("claims=%#v", claims)
	}
	claimName := claim.Labels[claimLabelClaimName]
	fake.objects[sandboxClaimResource+"/"+cfg.AgentSandbox.Namespace+"/"+claimName].Metadata.UID = "uid-replacement"

	_, err = backend.Run(context.Background(), RunRequest{
		Repo: repoB, ID: claim.LeaseID, Reclaim: true, Keep: true, NoSync: true, Command: []string{"true"},
	})
	if err == nil || !strings.Contains(err.Error(), "UID changed") {
		t.Fatalf("failed reclaim err=%v", err)
	}
	updated, err := readLeaseClaim(claim.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.RepoRoot != repoA.Root || updated.LastUsedAt != claim.LastUsedAt {
		t.Fatalf("failed reclaim mutated claim: before=%#v after=%#v", claim, updated)
	}
}

func TestRunExistingLeaseValidatesLiveClaimOwnership(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repo, RequestedSlug: "tamper"}); err != nil {
		t.Fatal(err)
	}
	claims, err := listAgentSandboxLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	claim, ok := claimBySlug(claims, "tamper")
	if !ok {
		t.Fatalf("claims=%#v", claims)
	}
	claimName := claim.Labels[claimLabelClaimName]
	live := fake.objects[sandboxClaimResource+"/"+cfg.AgentSandbox.Namespace+"/"+claimName]
	live.Metadata.Labels = map[string]string{labelProvider: providerName, labelLeaseID: "asbx_other"}
	_, err = backend.Run(context.Background(), RunRequest{Repo: repo, ID: claim.LeaseID, NoSync: true, Command: []string{"true"}})
	if err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("expected live ownership error, got %v", err)
	}
	if len(fake.execs) != 0 {
		t.Fatalf("command executed despite ownership mismatch: %#v", fake.execs)
	}
}

func TestStopRejectsReplacedClaimUID(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repo, RequestedSlug: "replace"}); err != nil {
		t.Fatal(err)
	}
	claims, err := listAgentSandboxLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	claim, ok := claimBySlug(claims, "replace")
	if !ok {
		t.Fatalf("claims=%#v", claims)
	}
	claimName := claim.Labels[claimLabelClaimName]
	fake.objects[sandboxClaimResource+"/"+cfg.AgentSandbox.Namespace+"/"+claimName].Metadata.UID = "uid-replacement"

	err = backend.Stop(context.Background(), StopRequest{ID: claim.LeaseID})
	if err == nil || !strings.Contains(err.Error(), "UID changed") {
		t.Fatalf("replacement claim stop err=%v", err)
	}
	if fake.deletes != 0 {
		t.Fatalf("replacement claim was deleted: deletes=%d", fake.deletes)
	}
}

type testExitError struct {
	code int
}

type fixedClock struct {
	now time.Time
}

func (c fixedClock) Now() time.Time {
	return c.now
}

func (e testExitError) Error() string {
	return "remote exited"
}

func (e testExitError) ExitStatus() int {
	return e.code
}

func TestRunExistingLeaseReadinessIsBounded(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	cfg.AgentSandbox.SandboxReadyTimeout = time.Minute
	cfg.AgentSandbox.PodReadyTimeout = time.Millisecond
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repo, RequestedSlug: "slow"}); err != nil {
		t.Fatal(err)
	}
	claims, err := listAgentSandboxLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	claim, ok := claimBySlug(claims, "slow")
	if !ok {
		t.Fatalf("claims=%#v", claims)
	}
	claimName := claim.Labels[claimLabelClaimName]
	fake.pods[cfg.AgentSandbox.Namespace+"/claim="+claimName] = []podState{{Name: claimName + "-pod", Phase: "Pending", Ready: false}}
	start := time.Now()
	_, err = backend.Run(context.Background(), RunRequest{Repo: repo, ID: claim.LeaseID, NoSync: true, Command: []string{"true"}})
	if err == nil || !strings.Contains(err.Error(), "readiness timed out") {
		t.Fatalf("expected bounded readiness error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("readiness wait was not bounded: %s", elapsed)
	}
}

func TestWarmupRetainsRecoverableClaimWhenReadinessCleanupFails(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	cfg.AgentSandbox.SandboxReadyTimeout = time.Millisecond
	fake := readyFakeClient(cfg)
	fake.createPending = true
	fake.deleteErrs = []error{errors.New("delete failed")}
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)

	err := backend.Warmup(context.Background(), WarmupRequest{Repo: repo, RequestedSlug: "recoverable"})
	if err == nil || !strings.Contains(err.Error(), "local lease") || !strings.Contains(err.Error(), "retained") {
		t.Fatalf("warmup err=%v", err)
	}
	claims, listErr := listAgentSandboxLeaseClaims()
	if listErr != nil {
		t.Fatal(listErr)
	}
	claim, ok := claimBySlug(claims, "recoverable")
	if !ok {
		t.Fatalf("recoverable claim missing: %#v", claims)
	}
	if claim.Labels[claimLabelClaimUID] == "" || claim.Labels["state"] != "not-ready" {
		t.Fatalf("recoverable claim labels=%#v", claim.Labels)
	}
	if stopErr := backend.Stop(context.Background(), StopRequest{ID: claim.LeaseID}); stopErr != nil {
		t.Fatal(stopErr)
	}
	if retained, readErr := readLeaseClaim(claim.LeaseID); readErr != nil || retained.LeaseID != "" {
		t.Fatalf("recovery stop retained claim=%#v err=%v", retained, readErr)
	}
}

func TestRunExistingLeaseForgetMissingStillFailsRun(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	cfg.AgentSandbox.ForgetMissing = true
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repo, RequestedSlug: "gone"}); err != nil {
		t.Fatal(err)
	}
	claims, err := listAgentSandboxLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	claim, ok := claimBySlug(claims, "gone")
	if !ok {
		t.Fatalf("claims=%#v", claims)
	}
	claimName := claim.Labels[claimLabelClaimName]
	delete(fake.objects, sandboxClaimResource+"/"+cfg.AgentSandbox.Namespace+"/"+claimName)
	result, err := backend.Run(context.Background(), RunRequest{Repo: repo, ID: claim.LeaseID, NoSync: true, Command: []string{"true"}})
	if err == nil || !strings.Contains(err.Error(), "command not run") {
		t.Fatalf("expected stale run failure, result=%#v err=%v", result, err)
	}
	forgotten, readErr := readLeaseClaim(claim.LeaseID)
	if readErr != nil || forgotten.LeaseID != "" {
		t.Fatalf("missing claim was not forgotten: claim=%#v err=%v", forgotten, readErr)
	}
	if len(fake.execs) != 0 {
		t.Fatalf("command executed despite missing claim: %#v", fake.execs)
	}
}

func TestStopSerializesWithActiveLeaseRun(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repo, RequestedSlug: "active"}); err != nil {
		t.Fatal(err)
	}
	claims, err := listAgentSandboxLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	claim, ok := claimBySlug(claims, "active")
	if !ok {
		t.Fatalf("claims=%#v", claims)
	}
	fake.execStarted = make(chan struct{}, 1)
	fake.execRelease = make(chan struct{})

	runDone := make(chan error, 1)
	go func() {
		_, err := backend.Run(context.Background(), RunRequest{
			ID: claim.LeaseID, Repo: repo, Keep: true, NoSync: true, Command: []string{"true"},
		})
		runDone <- err
	}()
	select {
	case <-fake.execStarted:
	case <-time.After(time.Second):
		t.Fatal("run did not reach user command")
	}

	stopDone := make(chan error, 1)
	go func() {
		stopDone <- backend.Stop(context.Background(), StopRequest{ID: claim.LeaseID})
	}()
	select {
	case err := <-stopDone:
		t.Fatalf("stop completed while run held operation lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(fake.execRelease)
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
	if err := <-stopDone; err != nil {
		t.Fatal(err)
	}
	if fake.deletes != 1 {
		t.Fatalf("deletes=%d want=1", fake.deletes)
	}
}

func TestCleanupSerializesWithLeaseReuse(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	backend.rt.Clock = fixedClock{now: time.Now().Add(2 * time.Hour)}
	repo := testGitRepo(t)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repo, RequestedSlug: "idle"}); err != nil {
		t.Fatal(err)
	}
	claims, err := listAgentSandboxLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	claim, ok := claimBySlug(claims, "idle")
	if !ok {
		t.Fatalf("claims=%#v", claims)
	}
	unlock, err := lockAgentSandboxLeaseOperation(context.Background(), claim.LeaseID)
	if err != nil {
		t.Fatal(err)
	}

	cleanupDone := make(chan error, 1)
	go func() {
		cleanupDone <- backend.Cleanup(context.Background(), CleanupRequest{})
	}()
	select {
	case err := <-cleanupDone:
		unlock()
		t.Fatalf("cleanup completed while lease lock was held: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	if err := <-cleanupDone; err != nil {
		t.Fatal(err)
	}
	if fake.deletes != 1 {
		t.Fatalf("deletes=%d want=1", fake.deletes)
	}
}

func TestAgentSandboxOperationLockHonorsContextCancellation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	unlock, err := lockAgentSandboxLeaseOperation(context.Background(), leasePrefix+"lock-test")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := lockAgentSandboxLeaseOperation(ctx, leasePrefix+"lock-test"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want context deadline", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cancellation took %s", elapsed)
	}
}

func TestCleanupContextIgnoresCancelledParent(t *testing.T) {
	backend := &backend{}
	parent, cancelParent := context.WithCancel(context.Background())
	cancelParent()
	cleanupCtx, cancelCleanup := backend.cleanupContext(parent)
	defer cancelCleanup()
	if err := cleanupCtx.Err(); err != nil {
		t.Fatalf("cleanup context inherited cancellation: %v", err)
	}
}

func TestStatusMissingClaimDoesNotWaitAsNotReady(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repo, RequestedSlug: "status-gone"}); err != nil {
		t.Fatal(err)
	}
	claims, err := listAgentSandboxLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	claim, ok := claimBySlug(claims, "status-gone")
	if !ok {
		t.Fatalf("claims=%#v", claims)
	}
	claimName := claim.Labels[claimLabelClaimName]
	delete(fake.objects, sandboxClaimResource+"/"+cfg.AgentSandbox.Namespace+"/"+claimName)
	view, err := backend.Status(context.Background(), StatusRequest{ID: claim.LeaseID})
	if err != nil {
		t.Fatal(err)
	}
	if view.State != "missing-or-inaccessible" {
		t.Fatalf("view=%#v", view)
	}
	start := time.Now()
	_, err = backend.Status(context.Background(), StatusRequest{ID: claim.LeaseID, Wait: true, WaitTimeout: time.Second})
	if err == nil || !strings.Contains(err.Error(), "missing in Kubernetes") {
		t.Fatalf("expected missing wait error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("status waited despite missing claim: %s", elapsed)
	}
}

func TestStatusWaitTimeoutBoundsInitialLookup(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repo, RequestedSlug: "status-timeout"}); err != nil {
		t.Fatal(err)
	}
	fake.getStarted = make(chan struct{}, 1)
	fake.getRelease = make(chan struct{})

	start := time.Now()
	_, err := backend.Status(context.Background(), StatusRequest{ID: "status-timeout", Wait: true, WaitTimeout: 20 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("status err=%v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("initial lookup ignored wait timeout: %s", elapsed)
	}
}

func TestCleanupRetainsMissingClaimsUnlessForgetMissing(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	repo := t.TempDir()
	leaseID := "asbx_missing"
	if err := claimLeaseForRepo(cfg, leaseID, "missing-claim", Repo{Root: repo}, false); err != nil {
		t.Fatal(err)
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = updateLeaseClaimLabelsIfUnchanged(leaseID, claim, map[string]string{claimLabelClaimName: "missing-claim"})
	if err != nil {
		t.Fatal(err)
	}
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)

	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := readLeaseClaim(leaseID); err != nil {
		t.Fatalf("claim removed without forgetMissing: %v", err)
	}

	cfg.AgentSandbox.ForgetMissing = true
	backend = testBackend(cfg, fake, nil, nil)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if claim, err := readLeaseClaim(leaseID); err != nil || claim.LeaseID != "" {
		t.Fatalf("claim not forgotten: claim=%#v err=%v", claim, err)
	}
}

func testAgentSandboxConfig(t *testing.T) Config {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := core.BaseConfig()
	cfg.AgentSandbox.Context = "agent-context"
	cfg.AgentSandbox.Namespace = "sandboxes"
	cfg.AgentSandbox.WarmPool = "linux-pool"
	cfg.AgentSandbox.Workdir = "/workspace/crabbox"
	cfg.AgentSandbox.DeleteOnRelease = true
	cfg.IdleTimeout = time.Hour
	return cfg
}

func testBackend(cfg Config, fake *fakeKubernetesClient, stdout, stderr *bytes.Buffer) *backend {
	if stdout == nil {
		stdout = &bytes.Buffer{}
	}
	if stderr == nil {
		stderr = &bytes.Buffer{}
	}
	return &backend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		rt:   Runtime{Stdout: stdout, Stderr: stderr},
		newClient: func(context.Context, Config, Runtime) (kubernetesClient, error) {
			return fake, nil
		},
	}
}

func claimBySlug(claims []LeaseClaim, slug string) (LeaseClaim, bool) {
	for _, claim := range claims {
		if claim.Slug == slug {
			return claim, true
		}
	}
	return LeaseClaim{}, false
}

func testGitRepo(t *testing.T) Repo {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test/repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pkg", "example.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init"}, {"add", "."}, {"commit", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return Repo{Root: root, Name: "repo"}
}

func tarFiles(t *testing.T, data []byte) map[string]bool {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := map[string]bool{}
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		files[header.Name] = true
	}
	return files
}
