package blaxel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type lifecycleFakeClient struct {
	baseURL       string
	sandboxes     map[string]Sandbox
	createReqs    []CreateSandboxRequest
	updateLabels  []map[string]string
	deleted       []string
	execReqs      []ExecuteProcessRequest
	uploads       []string
	logs          ProcessLogs
	exitCode      int
	omitExitCode  bool
	processStatus string
	getErr        error
	deleteErr     error
	updateErr     error
	updateEmptyID bool
	processErr    error
	nextSandboxID string
	createStatus  string
	listPages     map[string]ListSandboxesResult
	listReqs      []ListSandboxesRequest
	stopped       []string
	stopDeadline  bool
}

func newLifecycleFakeClient() *lifecycleFakeClient {
	return &lifecycleFakeClient{
		baseURL:       defaultAPIURL,
		sandboxes:     map[string]Sandbox{},
		nextSandboxID: "sbx_1",
		logs:          ProcessLogs{Stdout: "ok\n", Stderr: "warn\n"},
	}
}

func (f *lifecycleFakeClient) BaseURL() string { return f.baseURL }
func (f *lifecycleFakeClient) Probe(context.Context) error {
	return nil
}
func (f *lifecycleFakeClient) CreateSandbox(_ context.Context, req CreateSandboxRequest) (Sandbox, error) {
	f.createReqs = append(f.createReqs, req)
	id := f.nextSandboxID
	if id == "" {
		id = "sbx_1"
	}
	status := f.createStatus
	if status == "" {
		status = "running"
	}
	sb := Sandbox{ID: id, Name: req.Name, Status: status, Region: req.Region, Image: req.Image, Labels: cloneLabels(req.Labels)}
	f.sandboxes[id] = sb
	return sb, nil
}
func (f *lifecycleFakeClient) GetSandbox(_ context.Context, id string) (Sandbox, error) {
	if f.getErr != nil {
		return Sandbox{}, f.getErr
	}
	sb, ok := f.sandboxes[id]
	if !ok {
		return Sandbox{}, apiError{StatusCode: http.StatusNotFound}
	}
	return sb, nil
}
func (f *lifecycleFakeClient) ListSandboxes(_ context.Context, req ListSandboxesRequest) (ListSandboxesResult, error) {
	f.listReqs = append(f.listReqs, req)
	if f.listPages != nil {
		return f.listPages[req.Cursor], nil
	}
	out := make([]Sandbox, 0, len(f.sandboxes))
	for _, sb := range f.sandboxes {
		out = append(out, sb)
	}
	return ListSandboxesResult{Sandboxes: out}, nil
}
func (f *lifecycleFakeClient) UpdateSandboxLabels(_ context.Context, id string, labels map[string]string) (Sandbox, error) {
	if f.updateErr != nil {
		return Sandbox{}, f.updateErr
	}
	f.updateLabels = append(f.updateLabels, cloneLabels(labels))
	sb := f.sandboxes[id]
	sb.Labels = cloneLabels(labels)
	f.sandboxes[id] = sb
	if f.updateEmptyID {
		sb.ID = ""
	}
	return sb, nil
}
func (f *lifecycleFakeClient) DeleteSandbox(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.sandboxes, id)
	return nil
}
func (f *lifecycleFakeClient) ExecuteProcess(_ context.Context, _ string, req ExecuteProcessRequest) (Process, error) {
	if f.processErr != nil {
		return Process{}, f.processErr
	}
	f.execReqs = append(f.execReqs, req)
	return Process{ID: "proc_1", Status: f.effectiveProcessStatus(), ExitCode: f.processExitCode()}, nil
}
func (f *lifecycleFakeClient) GetProcess(context.Context, string, string) (Process, error) {
	return Process{ID: "proc_1", Status: f.effectiveProcessStatus(), ExitCode: f.processExitCode()}, nil
}
func (f *lifecycleFakeClient) GetProcessLogs(context.Context, string, string) (ProcessLogs, error) {
	return f.logs, nil
}
func (f *lifecycleFakeClient) StopProcess(ctx context.Context, _ string, process string) error {
	_, f.stopDeadline = ctx.Deadline()
	f.stopped = append(f.stopped, process)
	return nil
}
func (f *lifecycleFakeClient) WriteFile(context.Context, string, WriteFileRequest) error {
	return nil
}
func (f *lifecycleFakeClient) UploadFile(_ context.Context, _ string, remotePath string, r io.Reader) error {
	_, _ = io.ReadAll(r)
	f.uploads = append(f.uploads, remotePath)
	return nil
}
func (f *lifecycleFakeClient) GetDirectoryTree(context.Context, string, string) (DirectoryTree, error) {
	return DirectoryTree{}, nil
}

func (f *lifecycleFakeClient) processExitCode() *int {
	if f.omitExitCode {
		return nil
	}
	return intPtr(f.exitCode)
}

func (f *lifecycleFakeClient) effectiveProcessStatus() string {
	if f.processStatus != "" {
		return f.processStatus
	}
	return "completed"
}

func TestWarmupCreatesClaimAndCompletesRemoteLabels(t *testing.T) {
	backend, fake, _, stdout, _ := newLifecycleBackend(t)
	err := backend.Warmup(context.Background(), WarmupRequest{Repo: testRepo(t), RequestedSlug: "warm-one"})
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.createReqs) != 1 || fake.createReqs[0].Labels[blaxelClaimKey] == "" {
		t.Fatalf("create labels=%#v", fake.createReqs)
	}
	if len(fake.updateLabels) != 1 {
		t.Fatalf("label updates=%#v", fake.updateLabels)
	}
	labels := fake.updateLabels[0]
	if labels["crabbox"] != "true" || labels["crabbox.provider"] != providerName ||
		labels["crabbox.lease"] != leasePrefix+"sbx_1" || labels["crabbox.slug"] != "warm-one" ||
		labels[blaxelClaimKey] == "" || labels["crabbox.repo"] == "" {
		t.Fatalf("labels=%#v", labels)
	}
	claim, err := readLeaseClaim(leasePrefix + "sbx_1")
	if err != nil {
		t.Fatal(err)
	}
	if claim.Provider != providerName || claim.ProviderScope != labels[blaxelClaimKey] || claim.Slug != "warm-one" {
		t.Fatalf("claim=%#v labels=%#v", claim, labels)
	}
	if !strings.Contains(stdout.String(), "leased blx_sbx_1 slug=warm-one provider=blaxel") {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestWarmupPreservesCreateIDWhenLabelUpdateOmitsID(t *testing.T) {
	backend, fake, _, stdout, _ := newLifecycleBackend(t)
	fake.updateEmptyID = true
	err := backend.Warmup(context.Background(), WarmupRequest{Repo: testRepo(t), RequestedSlug: "empty-update"})
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("deleted=%#v", fake.deleted)
	}
	if claim, err := readLeaseClaim(leasePrefix + "sbx_1"); err != nil || claim.LeaseID != leasePrefix+"sbx_1" {
		t.Fatalf("claim=%#v err=%v", claim, err)
	}
	if !strings.Contains(stdout.String(), "sandbox=sbx_1") {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestBlaxelWorkdirRejectsBroadPaths(t *testing.T) {
	for _, workdir := range []string{"/", "/tmp", "/workspace", "/home", "/root", "/usr", "/var"} {
		cfg := Config{Blaxel: BlaxelConfig{Workdir: workdir}}
		if _, err := blaxelWorkdir(cfg); err == nil || !strings.Contains(err.Error(), "too broad") {
			t.Fatalf("blaxelWorkdir(%q) err=%v, want too broad", workdir, err)
		}
		if err := validateBlaxelConfig(cfg); err == nil || !strings.Contains(err.Error(), "too broad") {
			t.Fatalf("validateBlaxelConfig(%q) err=%v, want too broad", workdir, err)
		}
	}
	cfg := Config{Blaxel: BlaxelConfig{Workdir: " /workspace/crabbox/../project "}}
	if got, err := blaxelWorkdir(cfg); err != nil || got != "/workspace/project" {
		t.Fatalf("blaxelWorkdir cleaned=%q err=%v", got, err)
	}
}

func TestRunForwardsEnvInProcessBodyAndReturnsRemoteExit(t *testing.T) {
	backend, fake, _, stdout, stderr := newLifecycleBackend(t)
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:       testRepo(t),
		NoSync:     true,
		Keep:       true,
		Command:    []string{"go", "test", "./..."},
		Env:        map[string]string{"TOKEN": "secret-value"},
		EnvSummary: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || result.Provider != providerName || result.LeaseID != leasePrefix+"sbx_1" {
		t.Fatalf("result=%#v", result)
	}
	if result.Session == nil ||
		result.Session.Provider != providerName ||
		result.Session.LeaseID != leasePrefix+"sbx_1" ||
		result.Session.Slug == "" ||
		result.Session.Reused ||
		!result.Session.Kept ||
		result.Session.CleanupCommand != "crabbox stop --provider blaxel "+leasePrefix+"sbx_1" {
		t.Fatalf("session=%#v", result.Session)
	}
	if len(fake.execReqs) < 2 {
		t.Fatalf("execReqs=%#v, want ensure workspace and command", fake.execReqs)
	}
	runReq := fake.execReqs[len(fake.execReqs)-1]
	if runReq.Command != "go" || strings.Join(runReq.Args, " ") != "test ./..." || runReq.Env["TOKEN"] != "secret-value" {
		t.Fatalf("run exec=%#v", runReq)
	}
	if strings.Contains(stderr.String(), "secret-value") {
		t.Fatalf("stderr leaked env value: %q", stderr.String())
	}
	if stdout.String() != "ok\n" || !strings.Contains(stderr.String(), "warn\n") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestRunSyncOnlyUploadsArchiveAndSkipsUserCommand(t *testing.T) {
	backend, fake, _, stdout, _ := newLifecycleBackend(t)
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:     testRepo(t),
		SyncOnly: true,
		Keep:     true,
		Command:  []string{"echo", "unused"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.uploads) != 1 {
		t.Fatalf("uploads=%#v", fake.uploads)
	}
	if result.Session == nil || result.Session.LeaseID != leasePrefix+"sbx_1" || !result.Session.Kept {
		t.Fatalf("session=%#v", result.Session)
	}
	if got := len(fake.execReqs); got < 2 {
		t.Fatalf("execReqs=%#v, want sync shell helpers", fake.execReqs)
	}
	for _, req := range fake.execReqs {
		if req.Command == "echo" {
			t.Fatalf("sync-only ran user command: %#v", fake.execReqs)
		}
	}
	if !strings.Contains(stdout.String(), "synced /workspace/crabbox") {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestExecCommandReturnsWhenTerminalProcessOmitsExitCode(t *testing.T) {
	backend, fake, _, _, _ := newLifecycleBackend(t)
	fake.omitExitCode = true
	code, err := backend.execCommand(context.Background(), fake, "sbx_1", "/workspace/crabbox", []string{"true"}, nil)
	if err == nil || !strings.Contains(err.Error(), "without an exit code") {
		t.Fatalf("execCommand code=%d err=%v, want missing exit code error", code, err)
	}
	if code != 1 {
		t.Fatalf("code=%d, want 1", code)
	}
}

func TestWaitProcessTreatsStoppedAsTerminal(t *testing.T) {
	backend, fake, _, _, _ := newLifecycleBackend(t)
	fake.processStatus = "stopped"
	got, err := backend.waitProcess(context.Background(), fake, "sbx_1", Process{ID: "proc_1", Status: "running"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "stopped" {
		t.Fatalf("status=%q", got.Status)
	}
	if len(fake.stopped) != 0 {
		t.Fatalf("stopped=%#v", fake.stopped)
	}
}

func TestExecCommandEnforcesLocalProcessWaitTimeout(t *testing.T) {
	backend, fake, _, _, _ := newLifecycleBackend(t)
	backend.cfg.Blaxel.ExecTimeoutSecs = 1
	fake.processStatus = "running"
	fake.omitExitCode = true
	code, err := backend.execCommand(context.Background(), fake, "sbx_1", "/workspace/crabbox", []string{"sleep", "600"}, nil)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("execCommand code=%d err=%v, want deadline exceeded", code, err)
	}
	if len(fake.stopped) != 1 || fake.stopped[0] != "proc_1" {
		t.Fatalf("stopped=%#v", fake.stopped)
	}
	if !fake.stopDeadline {
		t.Fatal("StopProcess context had no deadline")
	}
}

func TestBuildCommandPreservesExplicitShellScript(t *testing.T) {
	got, err := buildCommand([]string{"python3 --version && pytest"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "\x00") != "bash\x00-lc\x00python3 --version && pytest" {
		t.Fatalf("shell command=%#v", got)
	}
	auto, err := buildCommand([]string{"KEY=value", "pytest"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(auto, "\x00") != "bash\x00-lc\x00KEY='value' 'pytest'" {
		t.Fatalf("auto-shell command=%#v", auto)
	}
}

func TestStopRequiresMatchingRemoteOwnershipLabels(t *testing.T) {
	backend, fake, _, _, _ := newLifecycleBackend(t)
	err := backend.Warmup(context.Background(), WarmupRequest{Repo: testRepo(t), RequestedSlug: "owned"})
	if err != nil {
		t.Fatal(err)
	}
	sb := fake.sandboxes["sbx_1"]
	sb.Labels[blaxelClaimKey] = "foreign"
	fake.sandboxes["sbx_1"] = sb
	err = backend.Stop(context.Background(), StopRequest{ID: "owned"})
	if err == nil || !strings.Contains(err.Error(), "ownership labels") {
		t.Fatalf("Stop err=%v, want ownership mismatch", err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("deleted foreign sandbox: %#v", fake.deleted)
	}
}

func TestCleanupDryRunSkipsFreshAndDoesNotDelete(t *testing.T) {
	backend, fake, _, stdout, stderr := newLifecycleBackend(t)
	err := backend.Warmup(context.Background(), WarmupRequest{Repo: testRepo(t), RequestedSlug: "fresh"})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Cleanup(context.Background(), CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("dry-run deleted=%#v", fake.deleted)
	}
	if !strings.Contains(stderr.String(), "idle timeout not reached") {
		t.Fatalf("stderr=%q", stderr.String())
	}
	if strings.Contains(stdout.String(), "delete sandbox") {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestCleanupDeletesDueOwnedClaimOnly(t *testing.T) {
	backend, fake, _, _, _ := newLifecycleBackend(t)
	err := backend.Warmup(context.Background(), WarmupRequest{Repo: testRepo(t), RequestedSlug: "due"})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := readLeaseClaim(leasePrefix + "sbx_1")
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-2 * time.Hour)
	if err := claimLeaseForRepoProviderScopePond(claim.LeaseID, claim.Slug, providerName, claim.ProviderScope, "", testRepo(t).Root, time.Second, true); err != nil {
		t.Fatal(err)
	}
	claim, err = readLeaseClaim(claim.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	claim.LastUsedAt = old.Format(time.RFC3339)
	writeClaimForTest(t, claim)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != "sbx_1" {
		t.Fatalf("deleted=%#v", fake.deleted)
	}
	if claim, err := readLeaseClaim(leasePrefix + "sbx_1"); err != nil || claim.LeaseID != "" {
		t.Fatalf("claim=%#v err=%v, want removed", claim, err)
	}
}

func TestStopPreservesMissingClaimUnlessForgetMissing(t *testing.T) {
	backend, fake, _, _, _ := newLifecycleBackend(t)
	err := backend.Warmup(context.Background(), WarmupRequest{Repo: testRepo(t), RequestedSlug: "missing"})
	if err != nil {
		t.Fatal(err)
	}
	delete(fake.sandboxes, "sbx_1")
	err = backend.Stop(context.Background(), StopRequest{ID: "missing"})
	if err == nil || !strings.Contains(err.Error(), "status=404") {
		t.Fatalf("Stop err=%v, want preserved 404", err)
	}
	if _, err := readLeaseClaim(leasePrefix + "sbx_1"); err != nil {
		t.Fatalf("claim should be preserved: %v", err)
	}
	backend.cfg.Blaxel.ForgetMissing = true
	if err := backend.Stop(context.Background(), StopRequest{ID: "missing"}); err != nil {
		t.Fatal(err)
	}
	if claim, err := readLeaseClaim(leasePrefix + "sbx_1"); err != nil || claim.LeaseID != "" {
		t.Fatalf("claim=%#v err=%v, want removed", claim, err)
	}
}

func TestCreateLabelUpdateFailureCleansRemote(t *testing.T) {
	backend, fake, _, _, _ := newLifecycleBackend(t)
	fake.updateErr = errors.New("label denied")
	err := backend.Warmup(context.Background(), WarmupRequest{Repo: testRepo(t)})
	if err == nil || !strings.Contains(err.Error(), "label denied") {
		t.Fatalf("Warmup err=%v", err)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != "sbx_1" {
		t.Fatalf("deleted=%#v, want cleanup of ambiguous labeled create", fake.deleted)
	}
}

func TestCreateCleanupFailureWritesRecoveryClaimAndCleanupDeletesMatch(t *testing.T) {
	backend, fake, _, stdout, _ := newLifecycleBackend(t)
	fake.updateErr = errors.New("label denied")
	fake.deleteErr = errors.New("delete denied")
	err := backend.Warmup(context.Background(), WarmupRequest{Repo: testRepo(t)})
	if err == nil || !strings.Contains(err.Error(), "recovery") {
		t.Fatalf("Warmup err=%v, want recovery claim failure context", err)
	}
	recoveries, err := listBlaxelCleanupClaims()
	if err != nil {
		t.Fatal(err)
	}
	var recovery LeaseClaim
	for _, claim := range recoveries {
		if strings.HasPrefix(claim.LeaseID, recoveryPrefix) {
			recovery = claim
		}
	}
	if recovery.LeaseID == "" || recovery.ProviderScope == "" {
		t.Fatalf("recoveries=%#v", recoveries)
	}
	sb := fake.sandboxes["sbx_1"]
	sb.Labels = map[string]string{
		"crabbox":          "true",
		"crabbox.provider": providerName,
		blaxelClaimKey:     recovery.ProviderScope,
	}
	fake.sandboxes["sbx_1"] = sb
	fake.updateErr = nil
	fake.deleteErr = nil
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) < 2 || fake.deleted[len(fake.deleted)-1] != "sbx_1" {
		t.Fatalf("deleted=%#v", fake.deleted)
	}
	if !strings.Contains(stdout.String(), "reason=ambiguous create") {
		t.Fatalf("stdout=%q", stdout.String())
	}
	if claim, err := readLeaseClaim(recovery.LeaseID); err != nil || claim.LeaseID != "" {
		t.Fatalf("recovery claim=%#v err=%v, want removed", claim, err)
	}
}

func TestRecoveryCleanupPaginatesBeforeRemovingRecoveryClaim(t *testing.T) {
	backend, fake, _, _, _ := newLifecycleBackend(t)
	scope, err := newBlaxelClaimScope(fake.baseURL, backend.cfg.Blaxel.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	recovery := LeaseClaim{
		LeaseID:            recoveryPrefix + "abc123",
		Provider:           providerName,
		ProviderScope:      scope,
		ClaimedAt:          time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339),
		IdleTimeoutSeconds: 1,
	}
	writeClaimForTest(t, recovery)
	fake.listPages = map[string]ListSandboxesResult{
		"": {Sandboxes: []Sandbox{{ID: "foreign", Labels: map[string]string{"crabbox": "true"}}}, Next: "page-2"},
		"page-2": {Sandboxes: []Sandbox{{
			ID: "sbx_2",
			Labels: map[string]string{
				"crabbox":          "true",
				"crabbox.provider": providerName,
				blaxelClaimKey:     recovery.ProviderScope,
			},
		}}},
	}
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.listReqs) != 2 || fake.listReqs[0].Limit != 200 || fake.listReqs[1].Cursor != "page-2" {
		t.Fatalf("listReqs=%#v", fake.listReqs)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != "sbx_2" {
		t.Fatalf("deleted=%#v", fake.deleted)
	}
	if claim, err := readLeaseClaim(recovery.LeaseID); err != nil || claim.LeaseID != "" {
		t.Fatalf("recovery claim=%#v err=%v, want removed", claim, err)
	}
}

func TestStandbySandboxStateIsReady(t *testing.T) {
	if !isReadyState("STANDBY") {
		t.Fatal("standby sandbox should be ready because Blaxel resumes it on use")
	}
}

func TestCreateReadinessFailureDeletesOneShotSandboxAndClaim(t *testing.T) {
	backend, fake, _, _, _ := newLifecycleBackend(t)
	fake.createStatus = "failed"
	_, err := backend.Run(context.Background(), RunRequest{
		Repo:    testRepo(t),
		NoSync:  true,
		Command: []string{"true"},
	})
	if err == nil || !strings.Contains(err.Error(), "entered terminal state") {
		t.Fatalf("Run err=%v, want readiness failure", err)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != "sbx_1" {
		t.Fatalf("deleted=%#v", fake.deleted)
	}
	if claim, err := readLeaseClaim(leasePrefix + "sbx_1"); err != nil || claim.LeaseID != "" {
		t.Fatalf("claim=%#v err=%v, want removed", claim, err)
	}
}

func newLifecycleBackend(t *testing.T) (*backend, *lifecycleFakeClient, string, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("HOME", t.TempDir())
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	fake := newLifecycleFakeClient()
	backend := &backend{
		spec: Provider{}.Spec(),
		cfg: core.Config{
			Provider:    providerName,
			IdleTimeout: time.Hour,
			Blaxel: core.BlaxelConfig{
				APIURL:    fake.baseURL,
				APIKey:    "test-key",
				Workspace: "workspace-test",
			},
		},
		rt: Runtime{Stdout: stdout, Stderr: stderr},
		clientFactory: func(Config, Runtime) (Client, error) {
			return fake, nil
		},
	}
	return backend, fake, state, stdout, stderr
}

func testRepo(t *testing.T) Repo {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.org/repo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "init")
	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "init")
	return Repo{Root: root, Name: "my-app", Head: "abc123"}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}

func writeClaimForTest(t *testing.T, claim LeaseClaim) {
	t.Helper()
	claimsDir := filepath.Join(os.Getenv("XDG_STATE_HOME"), "crabbox", "claims")
	if err := os.MkdirAll(claimsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(claim)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claimsDir, claim.LeaseID+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func cloneLabels(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

func intPtr(v int) *int { return &v }
