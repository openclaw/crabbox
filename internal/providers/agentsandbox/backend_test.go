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
	kubeexec "k8s.io/client-go/util/exec"
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
	if !strings.HasPrefix(claim.Labels[claimLabelClaimName], "crabbox-feature-branch-") || claim.Labels[claimLabelPodName] == "" {
		t.Fatalf("claim labels=%#v", claim.Labels)
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
		kubeexec.CodeExitError{Err: errors.New("remote exited"), Code: 42},
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
	live.SetLabels(map[string]string{labelProvider: providerName, labelLeaseID: "asbx_other"})
	_, err = backend.Run(context.Background(), RunRequest{Repo: repo, ID: claim.LeaseID, NoSync: true, Command: []string{"true"}})
	if err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("expected live ownership error, got %v", err)
	}
	if len(fake.execs) != 0 {
		t.Fatalf("command executed despite ownership mismatch: %#v", fake.execs)
	}
}

func TestRunExistingLeaseReadinessIsBounded(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	cfg.AgentSandbox.SandboxReadyTimeout = time.Millisecond
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
	t.Setenv("CRABBOX_STATE_DIR", t.TempDir())
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
