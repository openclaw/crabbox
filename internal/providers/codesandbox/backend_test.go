package codesandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWarmupCreatesSandboxClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeCodeSandboxAPI()
	backend, stdout, stderr := newFakeBackend(t, fake)

	if err := backend.Warmup(context.Background(), WarmupRequest{
		Repo: Repo{Name: "my-app", Root: "/repo"},
		RequestedSlug: "codesandbox-blue",
		TimingJSON: true,
	}); err != nil {
		t.Fatalf("Warmup err=%v", err)
	}
	if len(fake.created) != 1 {
		t.Fatalf("create calls=%d want 1", len(fake.created))
	}
	if got := fake.created[0].Tags; !contains(got, codeSandboxClaimTag) || !hasPrefix(got, codeSandboxScopeTagPrefix+"codesandbox/ownership:") {
		t.Fatalf("create tags=%v missing ownership tags", got)
	}
	leaseID := leasePrefix + fake.sandboxID
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatalf("read claim: %v", err)
	}
	if claim.Provider != providerName || claim.Slug != "codesandbox-blue" || claim.RepoRoot != "/repo" || !strings.HasPrefix(claim.ProviderScope, "codesandbox/ownership:") {
		t.Fatalf("claim=%#v", claim)
	}
	if !strings.Contains(stdout.String(), "leased "+leaseID) || !strings.Contains(stderr.String(), `"provider":"codesandbox"`) {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestResolveLeaseIDRequiresLocalClaimForRawIDs(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if _, _, _, _, err := resolveLeaseID("sb-raw"); err == nil || !strings.Contains(err.Error(), "not claimed by Crabbox") {
		t.Fatalf("resolve err=%v, want unclaimed rejection", err)
	}
}

func TestStopRejectsOwnershipMismatchBeforeDelete(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeCodeSandboxAPI()
	backend, _, _ := newFakeBackend(t, fake)
	leaseID := leasePrefix + fake.sandboxID
	if err := claimLeaseForRepoProviderScopePond(leaseID, "mine", providerName, "codesandbox/ownership:local", "", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	fake.sandbox.Tags = []string{codeSandboxClaimTag, codeSandboxScopeTagPrefix + "codesandbox/ownership:remote"}

	err := backend.Stop(context.Background(), StopRequest{ID: leaseID})
	if err == nil || !strings.Contains(err.Error(), "ownership tag") {
		t.Fatalf("Stop err=%v, want ownership rejection", err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("deleted=%v, want no delete", fake.deleted)
	}
}

func TestListAndStatusUseOwnedClaims(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeCodeSandboxAPI()
	backend, _, _ := newFakeBackend(t, fake)
	leaseID := leasePrefix + fake.sandboxID
	if err := claimLeaseForRepoProviderScopePond(leaseID, "listed", providerName, fake.scope, "pond-a", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := claimLeaseForRepoProviderScopePond("csbx_other", "other", "other-provider", fake.scope, "", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}

	leases, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatalf("List err=%v", err)
	}
	if len(leases) != 1 || leases[0].CloudID != fake.sandboxID || leases[0].Labels["slug"] != "listed" {
		t.Fatalf("leases=%#v", leases)
	}
	status, err := backend.Status(context.Background(), StatusRequest{ID: "listed"})
	if err != nil {
		t.Fatalf("Status err=%v", err)
	}
	if status.ID != leaseID || status.ServerID != fake.sandboxID || !status.Ready || status.Pond != "pond-a" {
		t.Fatalf("status=%#v", status)
	}
}

func TestRunStreamsOutputAndCleansUpOneShot(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeCodeSandboxAPI()
	fake.commandResults = []CommandResult{
		{ExitCode: 0}, // mkdir for --no-sync
		{ExitCode: 0, Stdout: "hello\n", Stderr: "note\n"},
	}
	backend, stdout, stderr := newFakeBackend(t, fake)

	result, err := backend.Run(context.Background(), RunRequest{
		Repo: Repo{Name: "my-app", Root: "/repo"},
		NoSync: true,
		Command: []string{"echo", "hello"},
		Env: map[string]string{"SECRET_TOKEN": "super-secret"},
		EnvSummary: true,
	})
	if err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if result.ExitCode != 0 || !result.SyncDelegated || result.Provider != providerName {
		t.Fatalf("result=%#v", result)
	}
	if !strings.Contains(stdout.String(), "hello\n") || !strings.Contains(stderr.String(), "note\n") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "super-secret") || !strings.Contains(stderr.String(), "SECRET_TOKEN=set len=12 secret=true") {
		t.Fatalf("env summary leaked or missing metadata: %q", stderr.String())
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != fake.sandboxID {
		t.Fatalf("deleted=%v", fake.deleted)
	}
	if _, ok, err := resolveCodeSandboxLeaseClaim(leasePrefix + fake.sandboxID); err != nil || ok {
		t.Fatalf("claim exists after cleanup ok=%v err=%v", ok, err)
	}
}

func TestRunPropagatesExitAndKeepOnFailureRetainsSandbox(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeCodeSandboxAPI()
	fake.commandResults = []CommandResult{
		{ExitCode: 0},
		{ExitCode: 7, Stderr: "boom\n"},
	}
	backend, _, stderr := newFakeBackend(t, fake)

	result, err := backend.Run(context.Background(), RunRequest{
		Repo: Repo{Name: "my-app", Root: "/repo"},
		NoSync: true,
		Command: []string{"false"},
		KeepOnFailure: true,
	})
	if err == nil {
		t.Fatal("expected non-zero exit error")
	}
	var exitErr ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 7 || result.ExitCode != 7 {
		t.Fatalf("err=%v result=%#v", err, result)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("deleted=%v, want retained", fake.deleted)
	}
	if !strings.Contains(stderr.String(), "stop: crabbox stop --provider codesandbox") {
		t.Fatalf("missing keep-on-failure stop hint: %q", stderr.String())
	}
}

func TestRunCancellationPropagates(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newFakeCodeSandboxAPI()
	fake.blockRun = true
	backend, _, _ := newFakeBackend(t, fake)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := backend.Run(ctx, RunRequest{
		Repo: Repo{Name: "my-app", Root: "/repo"},
		NoSync: true,
		Command: []string{"sleep", "10"},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run err=%v, want context.Canceled", err)
	}
}

func TestRunSyncOnlyUploadsArchiveAndExtracts(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, "go.mod"), []byte("module example.test/app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, repoRoot)
	fake := newFakeCodeSandboxAPI()
	fake.commandResults = []CommandResult{{ExitCode: 0}, {ExitCode: 0}, {ExitCode: 0}, {ExitCode: 0}}
	backend, stdout, _ := newFakeBackend(t, fake)
	backend.cfg.CodeSandbox.Workdir = "/project/workspace/my-app"

	result, err := backend.Run(context.Background(), RunRequest{
		Repo: Repo{Name: "my-app", Root: repoRoot},
		SyncOnly: true,
		ForceSyncLarge: true,
	})
	if err != nil {
		t.Fatalf("Run sync-only err=%v", err)
	}
	if !result.SyncDelegated || !strings.Contains(stdout.String(), "synced /project/workspace/my-app") {
		t.Fatalf("result=%#v stdout=%q", result, stdout.String())
	}
	if len(fake.uploads) != 1 || !strings.HasPrefix(fake.uploads[0].Path, "/tmp/crabbox-codesandbox-sync-") || len(fake.uploads[0].Data) == 0 {
		t.Fatalf("uploads=%#v", fake.uploads)
	}
	if !fake.hasCommandContaining("tar -xzf") || !fake.hasCommandContaining("/project/workspace/my-app") {
		t.Fatalf("commands=%#v", fake.commands)
	}
}

func newFakeBackend(t *testing.T, api *fakeCodeSandboxAPI) (*codeSandboxBackend, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	cfg := newTestConfig()
	var stdout, stderr bytes.Buffer
	restore := replaceClientFactory(func(Config, Runtime) (codeSandboxAPI, error) {
		return api, nil
	})
	t.Cleanup(restore)
	return &codeSandboxBackend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		rt: Runtime{
			Stdout: &stdout,
			Stderr: &stderr,
		},
	}, &stdout, &stderr
}

type uploadCall struct {
	SandboxID string
	Path      string
	Data      []byte
}

type fakeCodeSandboxAPI struct {
	sandboxID      string
	scope          string
	sandbox        SandboxSummary
	created        []CreateSandboxRequest
	deleted        []string
	commands       []CommandRequest
	uploads        []uploadCall
	commandResults []CommandResult
	blockRun       bool
}

func newFakeCodeSandboxAPI() *fakeCodeSandboxAPI {
	scope := "codesandbox/ownership:local"
	return &fakeCodeSandboxAPI{
		sandboxID: "sb-test01",
		scope:     scope,
		sandbox: SandboxSummary{
			ID:    "sb-test01",
			Title: "crabbox-my-app",
			State: "running",
			URL:   "https://sb-test01.csb.app",
			Tags:  []string{codeSandboxClaimTag, codeSandboxScopeTagPrefix + scope},
		},
	}
}

func (f *fakeCodeSandboxAPI) ListSandboxes(context.Context, ListSandboxesRequest) (ListSandboxesResult, error) {
	return ListSandboxesResult{Sandboxes: []SandboxSummary{f.sandbox}, TotalCount: 1}, nil
}

func (f *fakeCodeSandboxAPI) CreateSandbox(_ context.Context, req CreateSandboxRequest) (SandboxSummary, error) {
	f.created = append(f.created, req)
	f.sandbox.Tags = append([]string(nil), req.Tags...)
	for _, tag := range req.Tags {
		if strings.HasPrefix(tag, codeSandboxScopeTagPrefix) {
			f.scope = strings.TrimPrefix(tag, codeSandboxScopeTagPrefix)
		}
	}
	return f.sandbox, nil
}

func (f *fakeCodeSandboxAPI) GetSandbox(context.Context, string) (SandboxSummary, error) {
	return f.sandbox, nil
}

func (f *fakeCodeSandboxAPI) DeleteSandbox(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeCodeSandboxAPI) RunCommand(ctx context.Context, _ string, req CommandRequest) (CommandResult, error) {
	f.commands = append(f.commands, req)
	if f.blockRun {
		<-ctx.Done()
		return CommandResult{}, ctx.Err()
	}
	if len(f.commandResults) == 0 {
		return CommandResult{ExitCode: 0}, nil
	}
	result := f.commandResults[0]
	if len(f.commandResults) > 1 {
		f.commandResults = f.commandResults[1:]
	}
	return result, nil
}

func (f *fakeCodeSandboxAPI) UploadFile(_ context.Context, sandboxID, remotePath string, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.uploads = append(f.uploads, uploadCall{SandboxID: sandboxID, Path: remotePath, Data: data})
	return nil
}

func (f *fakeCodeSandboxAPI) hasCommandContaining(fragment string) bool {
	for _, command := range f.commands {
		if strings.Contains(strings.Join(command.Command, " "), fragment) {
			return true
		}
	}
	return false
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func hasPrefix(values []string, prefix string) bool {
	for _, value := range values {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"add", "."},
		{"-c", "user.name=Crabbox Test", "-c", "user.email=test@example.com", "commit", "-m", "init"},
	} {
		cmd := osexec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}
