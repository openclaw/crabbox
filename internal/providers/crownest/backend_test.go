package crownest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunUploadsArchiveStreamsLogsAndCleansUp(t *testing.T) {
	repoRoot := tempGitRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	api := &fakeCrownestClient{baseURL: "https://api.crownest.dev"}
	var stdout, stderr bytes.Buffer
	cfg := testConfig()
	b := &backend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		rt: Runtime{
			Stdout: &stdout,
			Stderr: &stderr,
		},
		newClient: func(Config, Runtime) (client, error) { return api, nil },
	}

	result, err := b.Run(context.Background(), RunRequest{
		Repo:    Repo{Root: repoRoot, Name: "demo"},
		Command: []string{"pnpm", "test"},
	})
	if err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit=%d", result.ExitCode)
	}
	if !strings.Contains(stdout.String(), "ok\n") {
		t.Fatalf("stdout=%q", stdout.String())
	}
	if !strings.Contains(result.CommandText, "pnpm") || !strings.Contains(result.CommandText, "test") {
		t.Fatalf("command=%q", result.CommandText)
	}
	if api.created.Command != result.CommandText || api.created.Template != "python-node" || api.created.Keep {
		t.Fatalf("create request=%#v", api.created)
	}
	if api.uploadBytes == 0 || api.finalized.UploadID != "upl_123" || !api.started || api.deletedSandboxID != "sbx_123" {
		t.Fatalf("fake api state upload=%d finalized=%#v started=%v deleted=%q", api.uploadBytes, api.finalized, api.started, api.deletedSandboxID)
	}
	if result.Session == nil || result.Session.Provider != providerName || result.Session.Kept {
		t.Fatalf("session=%#v", result.Session)
	}
	if strings.Contains(stderr.String(), "cn_test") {
		t.Fatalf("stderr leaked secret: %q", stderr.String())
	}
}

func TestRunRejectsWorkspaceEnvUntilCrownestSupportsIt(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	b := &backend{
		spec: Provider{}.Spec(),
		cfg:  testConfig(),
		rt:   Runtime{Stdout: io.Discard, Stderr: io.Discard},
		newClient: func(Config, Runtime) (client, error) {
			return &fakeCrownestClient{baseURL: "https://api.crownest.dev"}, nil
		},
	}
	_, err := b.Run(context.Background(), RunRequest{
		Repo:    Repo{Root: tempGitRepo(t), Name: "demo"},
		Command: []string{"printenv", "FOO"},
		Env:     map[string]string{"FOO": "bar"},
	})
	if err == nil || !strings.Contains(err.Error(), "does not support command environment forwarding") {
		t.Fatalf("err=%v", err)
	}
}

func TestRunRejectsSyncOnlyBeforeCreatingWorkspaceRun(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	calledClient := false
	b := &backend{
		spec: Provider{}.Spec(),
		cfg:  testConfig(),
		rt:   Runtime{Stdout: io.Discard, Stderr: io.Discard},
		newClient: func(Config, Runtime) (client, error) {
			calledClient = true
			return &fakeCrownestClient{baseURL: "https://api.crownest.dev"}, nil
		},
	}

	_, err := b.Run(context.Background(), RunRequest{
		Repo:     Repo{Root: tempGitRepo(t), Name: "demo"},
		Command:  []string{"echo", "should-not-run"},
		SyncOnly: true,
	})
	if err == nil || !strings.Contains(err.Error(), "--sync-only") {
		t.Fatalf("err=%v, want --sync-only rejection", err)
	}
	if calledClient {
		t.Fatalf("sync-only rejection should not create a Crownest client")
	}
}

func TestRunCancelsWorkspaceRunWhenLocalContextIsCanceled(t *testing.T) {
	repoRoot := tempGitRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	api := &fakeCrownestClient{
		baseURL: "https://api.crownest.dev",
		stream: func() (io.ReadCloser, error) {
			cancel()
			return nil, context.Canceled
		},
	}
	b := &backend{
		spec: Provider{}.Spec(),
		cfg:  testConfig(),
		rt:   Runtime{Stdout: io.Discard, Stderr: io.Discard},
		newClient: func(Config, Runtime) (client, error) {
			return api, nil
		},
	}

	_, err := b.Run(ctx, RunRequest{
		Repo:    Repo{Root: repoRoot, Name: "demo"},
		Command: []string{"pnpm", "test"},
		Keep:    true,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
	if api.canceledRunID != "wsr_123" {
		t.Fatalf("canceledRunID=%q", api.canceledRunID)
	}
}

func TestRunReusesClaimWithoutDeletingSandbox(t *testing.T) {
	repoRoot := tempGitRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := testConfig()
	api := &fakeCrownestClient{baseURL: "https://api.crownest.dev", startSandboxID: "sbx_reused"}
	leaseID := leasePrefix + "sbx_reused"
	if err := claimLeaseForRepoProviderScopePond(leaseID, "kept", providerName, claimScope(api.BaseURL(), cfg), cfg.Pond, repoRoot, cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	b := &backend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		rt:   Runtime{Stdout: io.Discard, Stderr: io.Discard},
		newClient: func(Config, Runtime) (client, error) {
			return api, nil
		},
	}

	result, err := b.Run(context.Background(), RunRequest{
		Repo:    Repo{Root: repoRoot, Name: "demo"},
		ID:      "kept",
		Command: []string{"pnpm", "test"},
	})
	if err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if !api.created.Keep || api.created.SandboxID != "sbx_reused" {
		t.Fatalf("create request=%#v, want kept reused sandbox", api.created)
	}
	if api.deletedSandboxID != "" {
		t.Fatalf("deleted reused sandbox %q", api.deletedSandboxID)
	}
	if result.Session == nil || !result.Session.Reused || !result.Session.Kept {
		t.Fatalf("session=%#v", result.Session)
	}
}

func TestRunCleansUpCreatedSandboxAfterArchiveSetupFailure(t *testing.T) {
	repoRoot := tempGitRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeCrownestClient{
		baseURL:         "https://api.crownest.dev",
		createSandboxID: "sbx_created",
		transferErr:     errors.New("transfer failed"),
	}
	b := &backend{
		spec: Provider{}.Spec(),
		cfg:  testConfig(),
		rt:   Runtime{Stdout: io.Discard, Stderr: io.Discard},
		newClient: func(Config, Runtime) (client, error) {
			return api, nil
		},
	}

	_, err := b.Run(context.Background(), RunRequest{
		Repo:    Repo{Root: repoRoot, Name: "demo"},
		Command: []string{"pnpm", "test"},
	})
	if err == nil || !strings.Contains(err.Error(), "transfer failed") {
		t.Fatalf("err=%v, want transfer failure", err)
	}
	if api.deletedSandboxID != "sbx_created" {
		t.Fatalf("deletedSandboxID=%q, want created sandbox cleanup", api.deletedSandboxID)
	}
}

func TestRunKeepOnFailureRetainsCreatedSandbox(t *testing.T) {
	repoRoot := tempGitRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeCrownestClient{
		baseURL:        "https://api.crownest.dev",
		startSandboxID: "sbx_failed",
		stream: func() (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(strings.Join([]string{
				`data: {"type":"terminal","seq":1,"workspaceRun":{"id":"wsr_123","status":"failed","failureReason":"command_exit","failureClass":"user_command","sandboxId":"sbx_failed","exitCode":7}}`,
				"",
			}, "\n"))), nil
		},
	}
	var stderr bytes.Buffer
	b := &backend{
		spec: Provider{}.Spec(),
		cfg:  testConfig(),
		rt:   Runtime{Stdout: io.Discard, Stderr: &stderr},
		newClient: func(Config, Runtime) (client, error) {
			return api, nil
		},
	}

	result, err := b.Run(context.Background(), RunRequest{
		Repo:          Repo{Root: repoRoot, Name: "demo"},
		Command:       []string{"false"},
		KeepOnFailure: true,
	})
	var exitErr ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 7 {
		t.Fatalf("err=%v, want exit 7", err)
	}
	if api.deletedSandboxID != "" {
		t.Fatalf("deletedSandboxID=%q, want retained sandbox", api.deletedSandboxID)
	}
	if !api.created.Keep {
		t.Fatalf("create request=%#v, want keepSandbox for keep-on-failure", api.created)
	}
	if result.Session == nil || !result.Session.Kept {
		t.Fatalf("session=%#v, want kept session after failure", result.Session)
	}
	if claim, err := readLeaseClaim(result.LeaseID); err != nil || claim.LeaseID != result.LeaseID {
		t.Fatalf("claim=%#v err=%v, want retained claim", claim, err)
	}
	if !strings.Contains(stderr.String(), "keep-on-failure") {
		t.Fatalf("stderr=%q, want keep-on-failure hint", stderr.String())
	}
}

func TestRunReturnsErrorForCanceledTerminalWithoutExitCode(t *testing.T) {
	repoRoot := tempGitRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeCrownestClient{
		baseURL:        "https://api.crownest.dev",
		startSandboxID: "sbx_canceled",
		stream: func() (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(strings.Join([]string{
				`data: {"type":"terminal","seq":1,"workspaceRun":{"id":"wsr_123","status":"canceled","failureReason":"timeout","failureClass":"platform","sandboxId":"sbx_canceled"}}`,
				"",
			}, "\n"))), nil
		},
	}
	b := &backend{
		spec: Provider{}.Spec(),
		cfg:  testConfig(),
		rt:   Runtime{Stdout: io.Discard, Stderr: io.Discard},
		newClient: func(Config, Runtime) (client, error) {
			return api, nil
		},
	}

	result, err := b.Run(context.Background(), RunRequest{
		Repo:    Repo{Root: repoRoot, Name: "demo"},
		Command: []string{"pnpm", "test"},
	})
	if err == nil || !strings.Contains(err.Error(), "status=canceled") {
		t.Fatalf("err=%v, want canceled terminal status error", err)
	}
	if result.ExitCode == 0 {
		t.Fatalf("exit=%d, want non-zero", result.ExitCode)
	}
	if api.deletedSandboxID != "sbx_canceled" {
		t.Fatalf("deletedSandboxID=%q, want canceled sandbox cleanup", api.deletedSandboxID)
	}
}

func TestCreateSandboxCleansUpRemoteWhenLocalClaimFails(t *testing.T) {
	repoRoot := tempGitRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := testConfig()
	api := &fakeCrownestClient{
		baseURL:         "https://api.crownest.dev",
		createSandboxID: "sbx_new",
	}
	leaseID := leasePrefix + "sbx_new"
	if err := claimLeaseForRepoProviderScopePond(leaseID, "existing", providerName, claimScope(api.BaseURL(), cfg), cfg.Pond, filepath.Join(repoRoot, "other"), cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	b := &backend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		rt:   Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}

	_, _, _, err := b.createSandbox(context.Background(), api, Repo{Root: repoRoot, Name: "demo"}, false, "")
	if err == nil || !strings.Contains(err.Error(), "claimed by repo") {
		t.Fatalf("err=%v, want claim repo conflict", err)
	}
	if api.deletedSandboxID != "sbx_new" {
		t.Fatalf("deletedSandboxID=%q, want remote cleanup", api.deletedSandboxID)
	}
}

type fakeCrownestClient struct {
	baseURL          string
	created          createWorkspaceRunRequest
	createSandboxID  string
	finalized        finalizeArchiveRequest
	uploadBytes      int
	started          bool
	startSandboxID   string
	deletedSandboxID string
	canceledRunID    string
	transferErr      error
	stream           func() (io.ReadCloser, error)
}

func (f *fakeCrownestClient) BaseURL() string { return f.baseURL }

func (f *fakeCrownestClient) CreateSandbox(context.Context, createSandboxRequest) (sandbox, error) {
	return sandbox{ID: blank(f.createSandboxID, "sbx_123"), Status: "running"}, nil
}

func (f *fakeCrownestClient) GetSandbox(context.Context, string) (sandbox, error) {
	return sandbox{ID: "sbx_123", Status: "running"}, nil
}

func (f *fakeCrownestClient) DeleteSandbox(_ context.Context, id string) error {
	f.deletedSandboxID = id
	return nil
}

func (f *fakeCrownestClient) CreateWorkspaceRun(_ context.Context, req createWorkspaceRunRequest, _ string) (workspaceRun, error) {
	f.created = req
	return workspaceRun{ID: "wsr_123", Status: "awaiting_archive", SandboxID: f.createSandboxID}, nil
}

func (f *fakeCrownestClient) CreateArchiveTransfer(_ context.Context, id string, _ createArchiveTransferRequest, _ string) (archiveTransfer, error) {
	if f.transferErr != nil {
		return archiveTransfer{}, f.transferErr
	}
	return archiveTransfer{ID: "upl_123", Method: "PUT", UploadURL: "/upload", MaxSizeBytes: 1 << 30, WorkspaceRunID: id}, nil
}

func (f *fakeCrownestClient) UploadArchive(_ context.Context, _ archiveTransfer, body io.Reader) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	f.uploadBytes = len(data)
	return nil
}

func (f *fakeCrownestClient) FinalizeArchive(_ context.Context, _ string, req finalizeArchiveRequest, _ string) (workspaceRun, error) {
	f.finalized = req
	return workspaceRun{ID: "wsr_123", Status: "archive_uploaded"}, nil
}

func (f *fakeCrownestClient) StartWorkspaceRun(context.Context, string, string) (workspaceRun, error) {
	f.started = true
	sandboxID := blank(f.startSandboxID, "sbx_123")
	return workspaceRun{ID: "wsr_123", Status: "running", SandboxID: sandboxID}, nil
}

func (f *fakeCrownestClient) CancelWorkspaceRun(_ context.Context, id string, _ string) (workspaceRun, error) {
	f.canceledRunID = id
	return workspaceRun{ID: "wsr_123", Status: "canceled"}, nil
}

func (f *fakeCrownestClient) GetWorkspaceRun(context.Context, string) (workspaceRun, error) {
	code := 0
	return workspaceRun{ID: "wsr_123", Status: "succeeded", SandboxID: "sbx_123", ExitCode: &code}, nil
}

func (f *fakeCrownestClient) StreamWorkspaceRunEvents(context.Context, string, int64) (io.ReadCloser, error) {
	if f.stream != nil {
		return f.stream()
	}
	return io.NopCloser(strings.NewReader(strings.Join([]string{
		`data: {"type":"stdout","seq":1,"data":"ok\n"}`,
		"",
		`data: {"type":"terminal","seq":2,"workspaceRun":{"id":"wsr_123","status":"succeeded","sandboxId":"sbx_123","exitCode":0}}`,
		"",
	}, "\n"))), nil
}

func (f *fakeCrownestClient) Probe(context.Context) error { return nil }

func tempGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"scripts":{"test":"echo ok"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "package.json")
	runGit(t, dir, "commit", "-m", "init")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
