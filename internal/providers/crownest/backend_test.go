package crownest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if api.uploadBytes == 0 || api.finalized.UploadID != "upl_123" || !api.started || api.deletedSandboxID != "" {
		t.Fatalf("fake api state upload=%d finalized=%#v started=%v deleted=%q", api.uploadBytes, api.finalized, api.started, api.deletedSandboxID)
	}
	if result.Session == nil || result.Session.Provider != providerName || result.Session.Kept {
		t.Fatalf("session=%#v", result.Session)
	}
	for _, want := range []string{"--provider crownest", "--crownest-url 'https://api.crownest.dev'", "--crownest-template 'python-node'", "--id "} {
		if !strings.Contains(result.Session.CleanupCommand, want) {
			t.Fatalf("cleanup command=%q, want %q", result.Session.CleanupCommand, want)
		}
	}
	if strings.Contains(stderr.String(), "cn_test") {
		t.Fatalf("stderr leaked secret: %q", stderr.String())
	}
	if claim, err := readLeaseClaim(result.LeaseID); err != nil || claim.LeaseID != "" {
		t.Fatalf("claim=%#v err=%v, want one-shot local claim removed without sandbox delete", claim, err)
	}
}

func TestRunCleanupCommandIncludesCrownestScopeFlags(t *testing.T) {
	repoRoot := tempGitRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeCrownestClient{baseURL: "https://crownest.internal.example/api"}
	cfg := testConfig()
	cfg.Crownest.APIURL = api.BaseURL()
	cfg.Crownest.ProjectID = "proj_custom"
	cfg.Crownest.Template = "node-22"
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
		Command: []string{"pnpm", "test"},
		Keep:    true,
	})
	if err != nil {
		t.Fatalf("Run err=%v", err)
	}
	for _, want := range []string{
		"--provider crownest",
		"--crownest-url 'https://crownest.internal.example/api'",
		"--crownest-project-id 'proj_custom'",
		"--crownest-template 'node-22'",
		"--id ",
	} {
		if !strings.Contains(result.Session.CleanupCommand, want) {
			t.Fatalf("cleanup command=%q, want %q", result.Session.CleanupCommand, want)
		}
	}
}

func TestRunMarksSessionKeptWhenRetainedCleanupFails(t *testing.T) {
	repoRoot := tempGitRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeCrownestClient{
		baseURL:   "https://api.crownest.dev",
		deleteErr: errors.New("delete failed"),
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
		Repo:          Repo{Root: repoRoot, Name: "demo"},
		Command:       []string{"pnpm", "test"},
		KeepOnFailure: true,
	})
	if err == nil || !strings.Contains(err.Error(), "delete failed") {
		t.Fatalf("err=%v, want cleanup failure", err)
	}
	if !api.created.Keep {
		t.Fatalf("create request=%#v, want retained sandbox for keep-on-failure cleanup", api.created)
	}
	if result.Session == nil || !result.Session.Kept {
		t.Fatalf("session=%#v, want kept session after cleanup failure", result.Session)
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

func TestRunCancelsWorkspaceRunWhenStreamFailsBeforeTerminal(t *testing.T) {
	repoRoot := tempGitRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeCrownestClient{
		baseURL:        "https://api.crownest.dev",
		startSandboxID: "sbx_stream_failed",
		latestRun:      workspaceRun{ID: "wsr_123", Status: "running", SandboxID: "sbx_stream_failed"},
		stream: func() (io.ReadCloser, error) {
			return nil, errors.New("event stream disconnected")
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
		Repo:          Repo{Root: repoRoot, Name: "demo"},
		Command:       []string{"pnpm", "test"},
		KeepOnFailure: true,
	})
	if err == nil || !strings.Contains(err.Error(), "crownest stream failed") {
		t.Fatalf("err=%v, want stream failure", err)
	}
	if api.canceledRunID != "wsr_123" {
		t.Fatalf("canceledRunID=%q, want active run cancellation", api.canceledRunID)
	}
	if api.deletedSandboxID != "" {
		t.Fatalf("deletedSandboxID=%q, want retained sandbox", api.deletedSandboxID)
	}
	if result.Session == nil || !result.Session.Kept {
		t.Fatalf("session=%#v, want kept session after stream failure", result.Session)
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

func TestRunReuseHonorsOperationLock(t *testing.T) {
	repoRoot := tempGitRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := testConfig()
	api := &fakeCrownestClient{baseURL: "https://api.crownest.dev", startSandboxID: "sbx_reused"}
	leaseID := leasePrefix + "sbx_reused"
	if err := claimLeaseForRepoProviderScopePond(leaseID, "kept", providerName, claimScope(api.BaseURL(), cfg), cfg.Pond, repoRoot, cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockCrownestLeaseOperation(context.Background(), leaseID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	b := &backend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		rt:   Runtime{Stdout: io.Discard, Stderr: io.Discard},
		newClient: func(Config, Runtime) (client, error) {
			return api, nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = b.Run(ctx, RunRequest{
		Repo:    Repo{Root: repoRoot, Name: "demo"},
		ID:      "kept",
		Command: []string{"pnpm", "test"},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want context deadline while waiting for operation lock", err)
	}
	if api.getSandboxCalls != 0 || api.created.Command != "" {
		t.Fatalf("run touched remote before lock: getSandboxCalls=%d created=%#v", api.getSandboxCalls, api.created)
	}
}

func TestStatusWaitPollsUntilSandboxReady(t *testing.T) {
	repoRoot := tempGitRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := testConfig()
	api := &fakeCrownestClient{
		baseURL:          "https://api.crownest.dev",
		getSandboxStates: []string{"starting", "running"},
	}
	leaseID := leasePrefix + "sbx_123"
	if err := claimLeaseForRepoProviderScopePond(leaseID, "status-wait", providerName, claimScope(api.BaseURL(), cfg), cfg.Pond, repoRoot, cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeLeaseClaim(leaseID) })
	b := &backend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		rt:   Runtime{Stdout: io.Discard, Stderr: io.Discard},
		newClient: func(Config, Runtime) (client, error) {
			return api, nil
		},
	}

	view, err := b.Status(context.Background(), StatusRequest{ID: "status-wait", Wait: true, WaitTimeout: time.Second})
	if err != nil {
		t.Fatalf("Status err=%v", err)
	}
	if !view.Ready || view.State != "running" {
		t.Fatalf("view=%#v, want ready running", view)
	}
	if api.getSandboxCalls < 2 {
		t.Fatalf("getSandboxCalls=%d, want polling", api.getSandboxCalls)
	}
}

func TestStopHonorsOperationLock(t *testing.T) {
	repoRoot := tempGitRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := testConfig()
	api := &fakeCrownestClient{baseURL: "https://api.crownest.dev"}
	leaseID := leasePrefix + "sbx_stop_locked"
	if err := claimLeaseForRepoProviderScopePond(leaseID, "stop-locked", providerName, claimScope(api.BaseURL(), cfg), cfg.Pond, repoRoot, cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockCrownestLeaseOperation(context.Background(), leaseID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	b := &backend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		rt:   Runtime{Stdout: io.Discard, Stderr: io.Discard},
		newClient: func(Config, Runtime) (client, error) {
			return api, nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err = b.Stop(ctx, StopRequest{ID: "stop-locked"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want context deadline while waiting for operation lock", err)
	}
	if api.deletedSandboxID != "" {
		t.Fatalf("stop deleted sandbox before lock: %q", api.deletedSandboxID)
	}
}

func TestCleanupSerializesAndRechecksLeaseActivity(t *testing.T) {
	repoRoot := tempGitRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := testConfig()
	cfg.IdleTimeout = time.Minute
	api := &fakeCrownestClient{baseURL: "https://api.crownest.dev"}
	leaseID := leasePrefix + "sbx_cleanup_locked"
	if err := claimLeaseForRepoProviderScopePond(leaseID, "cleanup-locked", providerName, claimScope(api.BaseURL(), cfg), cfg.Pond, repoRoot, cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	claim.LastUsedAt = time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	claim.IdleTimeoutSeconds = 60
	writeCrownestClaimFixture(t, claim)
	unlock, err := lockCrownestLeaseOperation(context.Background(), leaseID)
	if err != nil {
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

	cleanupDone := make(chan error, 1)
	go func() {
		cleanupDone <- b.Cleanup(context.Background(), CleanupRequest{})
	}()
	select {
	case err := <-cleanupDone:
		t.Fatalf("cleanup completed while operation lock was held: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	claim.LastUsedAt = time.Now().UTC().Format(time.RFC3339)
	writeCrownestClaimFixture(t, claim)
	unlock()
	if err := <-cleanupDone; err != nil {
		t.Fatal(err)
	}
	if api.deletedSandboxID != "" {
		t.Fatalf("cleanup deleted refreshed active sandbox: %q", api.deletedSandboxID)
	}
}

func TestCrownestOperationLockHonorsContextCancellation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	unlock, err := lockCrownestLeaseOperation(context.Background(), leasePrefix+"lock-test")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := lockCrownestLeaseOperation(ctx, leasePrefix+"lock-test"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want context deadline", err)
	}
}

func TestRunRemovesOneShotClaimAfterArchiveSetupFailure(t *testing.T) {
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
	if api.deletedSandboxID != "" {
		t.Fatalf("deletedSandboxID=%q, want Crownest-owned one-shot cleanup", api.deletedSandboxID)
	}
	leaseID := leasePrefix + "sbx_created"
	if claim, err := readLeaseClaim(leaseID); err != nil || claim.LeaseID != "" {
		t.Fatalf("claim=%#v err=%v, want one-shot setup-failure claim removed", claim, err)
	}
}

func TestRunDeletesPartialCreateSandboxWhenWorkspaceRunIDIsMissing(t *testing.T) {
	repoRoot := tempGitRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeCrownestClient{
		baseURL:         "https://api.crownest.dev",
		createRunResult: workspaceRun{SandboxID: "sbx_partial"},
		createRunErr:    errors.New("crownest create workspace run returned no id"),
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
	if err == nil || !strings.Contains(err.Error(), "returned no id") {
		t.Fatalf("err=%v, want missing workspace run id", err)
	}
	if api.deletedSandboxID != "sbx_partial" {
		t.Fatalf("deletedSandboxID=%q, want partial sandbox cleanup", api.deletedSandboxID)
	}
	leaseID := leasePrefix + "sbx_partial"
	if claim, err := readLeaseClaim(leaseID); err != nil || claim.LeaseID != "" {
		t.Fatalf("claim=%#v err=%v, want no partial-create claim", claim, err)
	}
}

func TestRunKeepOnFailureRetainsCreatedSandboxAfterArchiveSetupFailure(t *testing.T) {
	repoRoot := tempGitRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeCrownestClient{
		baseURL:         "https://api.crownest.dev",
		createSandboxID: "sbx_kept_setup",
		transferErr:     errors.New("transfer failed"),
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
		Command:       []string{"pnpm", "test"},
		KeepOnFailure: true,
	})
	if err == nil || !strings.Contains(err.Error(), "transfer failed") {
		t.Fatalf("err=%v, want transfer failure", err)
	}
	if api.deletedSandboxID != "" {
		t.Fatalf("deletedSandboxID=%q, want retained sandbox", api.deletedSandboxID)
	}
	leaseID := leasePrefix + "sbx_kept_setup"
	t.Cleanup(func() { removeLeaseClaim(leaseID) })
	if claim, err := readLeaseClaim(leaseID); err != nil || claim.LeaseID != leaseID {
		t.Fatalf("claim=%#v err=%v, want retained claim", claim, err)
	}
	if result.Session == nil || !result.Session.Kept || result.Session.LeaseID != leaseID {
		t.Fatalf("session=%#v, want retained setup-failure session", result.Session)
	}
	if !strings.Contains(stderr.String(), "keep-on-failure: kept lease="+leaseID) {
		t.Fatalf("stderr=%q, want keep-on-failure hint", stderr.String())
	}
}

func TestRunKeepDeletesCreatedSandboxWhenLocalClaimFails(t *testing.T) {
	repoRoot := tempGitRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := testConfig()
	api := &fakeCrownestClient{
		baseURL:         "https://api.crownest.dev",
		createSandboxID: "sbx_unclaimable",
	}
	leaseID := leasePrefix + "sbx_unclaimable"
	if err := claimLeaseForRepoProviderScopePond(leaseID, "existing", providerName, claimScope(api.BaseURL(), cfg), cfg.Pond, filepath.Join(repoRoot, "other"), cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeLeaseClaim(leaseID) })
	b := &backend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		rt:   Runtime{Stdout: io.Discard, Stderr: io.Discard},
		newClient: func(Config, Runtime) (client, error) {
			return api, nil
		},
	}

	_, err := b.Run(context.Background(), RunRequest{
		Repo:    Repo{Root: repoRoot, Name: "demo"},
		Command: []string{"pnpm", "test"},
		Keep:    true,
	})
	if err == nil || !strings.Contains(err.Error(), "claimed by repo") {
		t.Fatalf("err=%v, want local claim failure", err)
	}
	if api.deletedSandboxID != "sbx_unclaimable" {
		t.Fatalf("deletedSandboxID=%q, want unclaimed sandbox cleanup", api.deletedSandboxID)
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
	if api.deletedSandboxID != "" {
		t.Fatalf("deletedSandboxID=%q, want Crownest-owned canceled sandbox cleanup", api.deletedSandboxID)
	}
	if claim, err := readLeaseClaim(result.LeaseID); err != nil || claim.LeaseID != "" {
		t.Fatalf("claim=%#v err=%v, want one-shot terminal-failure claim removed", claim, err)
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
	createRunResult  workspaceRun
	createRunErr     error
	finalized        finalizeArchiveRequest
	uploadBytes      int
	started          bool
	startSandboxID   string
	latestRun        workspaceRun
	getSandboxStates []string
	getSandboxCalls  int
	deletedSandboxID string
	canceledRunID    string
	deleteErr        error
	transferErr      error
	stream           func() (io.ReadCloser, error)
}

func (f *fakeCrownestClient) BaseURL() string { return f.baseURL }

func (f *fakeCrownestClient) CreateSandbox(context.Context, createSandboxRequest) (sandbox, error) {
	return sandbox{ID: blank(f.createSandboxID, "sbx_123"), Status: "running"}, nil
}

func (f *fakeCrownestClient) GetSandbox(context.Context, string) (sandbox, error) {
	f.getSandboxCalls++
	if len(f.getSandboxStates) > 0 {
		idx := f.getSandboxCalls - 1
		if idx >= len(f.getSandboxStates) {
			idx = len(f.getSandboxStates) - 1
		}
		return sandbox{ID: "sbx_123", Status: f.getSandboxStates[idx]}, nil
	}
	return sandbox{ID: "sbx_123", Status: "running"}, nil
}

func (f *fakeCrownestClient) DeleteSandbox(_ context.Context, id string) error {
	f.deletedSandboxID = id
	return f.deleteErr
}

func (f *fakeCrownestClient) CreateWorkspaceRun(_ context.Context, req createWorkspaceRunRequest, _ string) (workspaceRun, error) {
	f.created = req
	if f.createRunErr != nil || f.createRunResult.ID != "" || f.createRunResult.SandboxID != "" {
		return f.createRunResult, f.createRunErr
	}
	return workspaceRun{ID: "wsr_123", Status: "awaiting_archive", SandboxID: f.createSandboxID}, nil
}

func (f *fakeCrownestClient) CreateArchiveTransfer(_ context.Context, id string, _ createArchiveTransferRequest, _ string) (archiveTransfer, error) {
	if f.transferErr != nil {
		return archiveTransfer{}, f.transferErr
	}
	return archiveTransfer{ID: "upl_123", Method: "PUT", UploadURL: "/upload", MaxSizeBytes: 1 << 30}, nil
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
	if f.latestRun.ID != "" {
		return f.latestRun, nil
	}
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

func writeCrownestClaimFixture(t *testing.T, claim LeaseClaim) {
	t.Helper()
	data, err := json.MarshalIndent(claim, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("XDG_STATE_HOME"), "crabbox", "claims", claim.LeaseID+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

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
