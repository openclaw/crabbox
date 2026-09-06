package azuredynamicsessions

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

	core "github.com/openclaw/crabbox/internal/cli"
)

const testAzureDynamicSessionsEndpoint = "http://127.0.0.1:8787"

func TestProviderSpec(t *testing.T) {
	spec := Provider{}.Spec()
	if spec.Kind != core.ProviderKindDelegatedRun {
		t.Fatalf("kind=%v want delegated-run", spec.Kind)
	}
	if !spec.Features.Has(core.FeatureArchiveSync) {
		t.Fatalf("features=%#v want archive-sync", spec.Features)
	}
	if !spec.Features.Has(core.FeatureRunSession) {
		t.Fatalf("features=%#v want run-session", spec.Features)
	}
}

func TestRunStopsNewSessionByDefault(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := t.TempDir()
	fake := &recordingAzureDynamicSessionsAPI{}
	restoreAzureDynamicSessionsClient(t, fake)
	backend := testAzureDynamicSessionsBackend()

	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Root: repo, Name: "repo"},
		NoSync:  true,
		Command: []string{"printf", "ok"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || result.Provider != providerName || result.LeaseID == "" {
		t.Fatalf("result = %#v", result)
	}
	if result.Session == nil {
		t.Fatal("result.Session is nil")
	}
	if result.Session.Provider != providerName || result.Session.LeaseID != result.LeaseID || result.Session.Slug != result.Slug {
		t.Fatalf("session=%#v result=%#v", result.Session, result)
	}
	if result.Session.Reused {
		t.Fatalf("session.Reused=true, want false")
	}
	if result.Session.Kept {
		t.Fatalf("session.Kept=true, want false after cleanup")
	}
	if !strings.Contains(result.Session.CleanupCommand, "crabbox stop --provider azure-dynamic-sessions") {
		t.Fatalf("cleanup command=%q", result.Session.CleanupCommand)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != result.LeaseID {
		t.Fatalf("deleted sessions = %#v, want %s", fake.deleted, result.LeaseID)
	}
	if _, ok, err := resolveLeaseClaimForProvider(result.LeaseID, providerName); err != nil || ok {
		t.Fatalf("claim after cleanup ok=%t err=%v", ok, err)
	}
}

func TestRunCleanupUsesBoundedContext(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	oldTimeout := azureDynamicSessionsDeleteTimeout
	azureDynamicSessionsDeleteTimeout = 200 * time.Millisecond
	t.Cleanup(func() { azureDynamicSessionsDeleteTimeout = oldTimeout })
	fake := &recordingAzureDynamicSessionsAPI{deleteWaitForCancel: true}
	restoreAzureDynamicSessionsClient(t, fake)
	backend := testAzureDynamicSessionsBackend()

	started := time.Now()
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:       Repo{Root: t.TempDir(), Name: "repo"},
		NoSync:     true,
		Command:    []string{"printf", "ok"},
		TimingJSON: true,
	})
	if err == nil || result.ExitCode != 1 || result.ErrorKind != core.RunErrorProvider {
		t.Fatalf("cleanup failure reported success: result=%#v err=%v", result, err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("bounded cleanup did not return promptly")
	}
	if result.Session == nil || !result.Session.Kept {
		t.Fatalf("session=%#v, want retained session after cleanup failure", result.Session)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != result.LeaseID {
		t.Fatalf("deleted sessions = %#v, want %s", fake.deleted, result.LeaseID)
	}
	var report timingReport
	lines := strings.Split(strings.TrimSpace(backend.rt.Stderr.(*bytes.Buffer).String()), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &report); err != nil || report.ExitCode != 1 || report.ErrorKind != core.RunErrorProvider {
		t.Fatalf("final cleanup timing=%#v err=%v", report, err)
	}
}

func TestRunCleanupPreservesReplacedSessionClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	replacementRepo := t.TempDir()
	fake := &recordingAzureDynamicSessionsAPI{}
	fake.onExec = func(req azureDynamicSessionsExecRequest) {
		if strings.HasPrefix(req.Command, "mkdir -p ") {
			return
		}
		claims, err := listLeaseClaims()
		if err != nil || len(claims) != 1 {
			t.Fatalf("claims=%#v err=%v", claims, err)
		}
		claim := claims[0]
		if err := claimLeaseForRepoProviderScope(claim.LeaseID, claim.Slug, providerName, claim.ProviderScope, replacementRepo, time.Minute, true); err != nil {
			t.Fatal(err)
		}
	}
	restoreAzureDynamicSessionsClient(t, fake)
	backend := testAzureDynamicSessionsBackend()
	result, err := backend.Run(t.Context(), RunRequest{
		Repo:    Repo{Root: t.TempDir(), Name: "repo"},
		NoSync:  true,
		Command: []string{"printf", "ok"},
	})
	if err == nil || result.ExitCode != 1 || !strings.Contains(err.Error(), "claim changed; retry") {
		t.Fatalf("cleanup refusal reported success: result=%#v err=%v", result, err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("deleted=%#v, want replaced session claim protected", fake.deleted)
	}
	claim, ok, err := resolveLeaseClaimForProvider(result.LeaseID, providerName)
	if err != nil || !ok || claim.RepoRoot != replacementRepo {
		t.Fatalf("replacement claim=%#v ok=%t err=%v", claim, ok, err)
	}
	if result.Session == nil || !result.Session.Kept {
		t.Fatalf("session=%#v, want replacement retained after cleanup refusal", result.Session)
	}
}

func TestCreateSessionRollbackReportsDeleteFailure(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &recordingAzureDynamicSessionsAPI{deleteErr: errors.New("delete failed")}
	backend := testAzureDynamicSessionsBackend()
	backend.cfg.AzureDynamicSessions.Endpoint = ""

	_, _, err := backend.createSession(context.Background(), fake, Repo{Root: t.TempDir(), Name: "repo"}, false, "")
	if err == nil || !strings.Contains(err.Error(), "requires azureDynamicSessions.endpoint") || !strings.Contains(err.Error(), "cleanup azure-dynamic-sessions session") || !strings.Contains(err.Error(), "delete failed") {
		t.Fatalf("err = %v, want claim-scope and cleanup errors", err)
	}
	if len(fake.deleted) != 1 {
		t.Fatalf("deleted sessions = %#v, want rollback delete attempt", fake.deleted)
	}
}

func TestRunKeepOnFailureRetainsNewSession(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := t.TempDir()
	fake := &recordingAzureDynamicSessionsAPI{commandExit: 7}
	restoreAzureDynamicSessionsClient(t, fake)
	backend := testAzureDynamicSessionsBackend()

	result, err := backend.Run(context.Background(), RunRequest{
		Repo:          Repo{Root: repo, Name: "repo"},
		NoSync:        true,
		KeepOnFailure: true,
		Command:       []string{"false"},
		TimingJSON:    true,
	})
	var exitErr ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 7 {
		t.Fatalf("err = %v, want exit 7", err)
	}
	if result.LeaseID == "" {
		t.Fatalf("result missing lease: %#v", result)
	}
	if result.Session == nil || !result.Session.Kept || result.Session.Reused {
		t.Fatalf("session=%#v, want retained new session", result.Session)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("deleted sessions = %#v, want retained session", fake.deleted)
	}
	if claim, ok, err := resolveLeaseClaimForProvider(result.LeaseID, providerName); err != nil || !ok || claim.RepoRoot != repo {
		t.Fatalf("retained claim ok=%t claim=%#v err=%v", ok, claim, err)
	}
	var report timingReport
	found := false
	for _, line := range strings.Split(strings.TrimSpace(backend.rt.Stderr.(*bytes.Buffer).String()), "\n") {
		if !strings.HasPrefix(line, "{") {
			continue
		}
		if err := json.Unmarshal([]byte(line), &report); err != nil {
			t.Fatalf("timing json: %v\nstderr=%s", err, backend.rt.Stderr)
		}
		found = true
		break
	}
	if !found {
		t.Fatalf("stderr does not contain timing JSON: %s", backend.rt.Stderr)
	}
	if report.RunStatus != "failed" || report.ErrorKind != "command-exit" {
		t.Fatalf("timing outcome status=%q kind=%q", report.RunStatus, report.ErrorKind)
	}
}

func TestRunKeepOnFailureRetainsNewSessionAfterSetupFailure(t *testing.T) {
	for _, tc := range []struct {
		name          string
		noSync        bool
		uploadErr     error
		failWorkspace bool
	}{
		{name: "archive sync", uploadErr: errors.New("upload failed")},
		{name: "workspace setup", noSync: true, failWorkspace: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			fake := &recordingAzureDynamicSessionsAPI{
				uploadErr:     tc.uploadErr,
				failWorkspace: tc.failWorkspace,
			}
			restoreAzureDynamicSessionsClient(t, fake)
			backend := testAzureDynamicSessionsBackend()
			repo := newAzureDynamicSessionsSyncTestRepo(t)

			result, err := backend.Run(t.Context(), RunRequest{
				Repo:          Repo{Root: repo, Name: "repo"},
				NoSync:        tc.noSync,
				KeepOnFailure: true,
				Command:       []string{"true"},
			})
			if err == nil {
				t.Fatal("run unexpectedly succeeded")
			}
			if result.Session == nil || !result.Session.Kept || result.Session.Reused {
				t.Fatalf("session=%#v, want retained new session", result.Session)
			}
			if len(fake.deleted) != 0 {
				t.Fatalf("deleted sessions=%v, want retained session", fake.deleted)
			}
			if claim, ok, claimErr := resolveLeaseClaimForProvider(result.LeaseID, providerName); claimErr != nil || !ok || claim.RepoRoot != repo {
				t.Fatalf("retained claim ok=%t claim=%#v err=%v", ok, claim, claimErr)
			}
			if !strings.Contains(backend.rt.Stderr.(*bytes.Buffer).String(), "keep-on-failure") {
				t.Fatalf("stderr=%q, want retention guidance", backend.rt.Stderr)
			}
		})
	}
}

func TestSyncDeletePreservesWorkspaceWhenReplacementFails(t *testing.T) {
	for _, tc := range []struct {
		name        string
		uploadErr   error
		failExtract bool
	}{
		{name: "upload", uploadErr: errors.New("upload failed")},
		{name: "extract", failExtract: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := filepath.Join(t.TempDir(), "workspace")
			if err := os.Mkdir(workspace, 0o755); err != nil {
				t.Fatal(err)
			}
			previous := filepath.Join(workspace, "previous.txt")
			if err := os.WriteFile(previous, []byte("keep me"), 0o644); err != nil {
				t.Fatal(err)
			}
			fake := &recordingAzureDynamicSessionsAPI{
				uploadErr:    tc.uploadErr,
				failExtract:  tc.failExtract,
				executeShell: true,
			}
			backend := testAzureDynamicSessionsBackend()
			backend.cfg.Sync.Delete = true

			_, _, err := backend.syncWorkspace(t.Context(), fake, "azds-session", RunRequest{
				Repo: Repo{Root: newAzureDynamicSessionsSyncTestRepo(t), Name: "repo"},
			}, workspace)
			if err == nil {
				t.Fatal("sync unexpectedly succeeded")
			}
			content, err := os.ReadFile(previous)
			if err != nil || string(content) != "keep me" {
				t.Fatalf("previous workspace content=%q err=%v", content, err)
			}
			if matches, err := filepath.Glob(filepath.Join(filepath.Dir(workspace), ".workspace.crabbox-sync-*")); err != nil || len(matches) != 0 {
				t.Fatalf("staging directories=%v err=%v, want none", matches, err)
			}
		})
	}
}

func TestStatusWaitBoundsInFlightSessionLookup(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	claimAzureDynamicSessionsLease(t, "azds-wait", "waiting-session", t.TempDir(), time.Minute)
	fake := &recordingAzureDynamicSessionsAPI{getWaitForCancel: true}
	restoreAzureDynamicSessionsClient(t, fake)
	backend := testAzureDynamicSessionsBackend()
	started := time.Now()

	_, err := backend.Status(t.Context(), StatusRequest{
		ID: "waiting-session", Wait: true, WaitTimeout: 30 * time.Millisecond,
	})
	var exitErr ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 5 || !strings.Contains(err.Error(), "timed out waiting for session azds-wait") {
		t.Fatalf("status err=%v, want session wait timeout with exit code 5", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("in-flight session lookup returned after %s", elapsed)
	}
}

func TestRunReusesClaimWithoutStoppingSession(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := t.TempDir()
	claimAzureDynamicSessionsLease(t, "azds-kept", "kept-session", repo, time.Minute)
	fake := &recordingAzureDynamicSessionsAPI{}
	restoreAzureDynamicSessionsClient(t, fake)
	backend := testAzureDynamicSessionsBackend()

	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Root: repo, Name: "repo"},
		ID:      "kept-session",
		NoSync:  true,
		Command: []string{"printf", "ok"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.LeaseID != "azds-kept" || result.Slug != "kept-session" {
		t.Fatalf("result = %#v", result)
	}
	if result.Session == nil || !result.Session.Reused || !result.Session.Kept {
		t.Fatalf("session=%#v, want retained reused session", result.Session)
	}
	if result.Session.LeaseID != "azds-kept" || result.Session.Slug != "kept-session" {
		t.Fatalf("session=%#v", result.Session)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("deleted reused session: %#v", fake.deleted)
	}
}

func TestWarmupRejectsActionsRunner(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &recordingAzureDynamicSessionsAPI{}
	restoreAzureDynamicSessionsClient(t, fake)
	backend := testAzureDynamicSessionsBackend()

	err := backend.Warmup(context.Background(), WarmupRequest{
		Repo:          Repo{Root: t.TempDir(), Name: "repo"},
		ActionsRunner: true,
	})
	if err == nil || !strings.Contains(err.Error(), "--actions-runner is not supported") {
		t.Fatalf("err = %v, want actions-runner rejection", err)
	}
	if fake.checkRunnerCalls != 0 {
		t.Fatalf("CheckRunner calls = %d, want 0", fake.checkRunnerCalls)
	}
}

func TestStopRemovesStaleClaimWhenSessionIsGone(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	claimAzureDynamicSessionsLease(t, "azds-stale", "stale-session", t.TempDir(), time.Minute)
	fake := &recordingAzureDynamicSessionsAPI{
		deleteErr: &azureDynamicSessionsAPIError{StatusCode: 404, Status: "404 Not Found"},
	}
	restoreAzureDynamicSessionsClient(t, fake)
	backend := testAzureDynamicSessionsBackend()

	if err := backend.Stop(context.Background(), StopRequest{ID: "stale-session"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := resolveLeaseClaimForProvider("stale-session", providerName); err != nil || ok {
		t.Fatalf("claim after stale stop ok=%t err=%v", ok, err)
	}
}

func TestStopRemovesStaleClaimOnAzureMissingSessionCode(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	claimAzureDynamicSessionsLease(t, "azds-stale-400", "stale-session-400", t.TempDir(), time.Minute)
	fake := &recordingAzureDynamicSessionsAPI{
		deleteErr: &azureDynamicSessionsAPIError{
			StatusCode: 400,
			Status:     "400 Bad Request",
			Body:       `{"error":{"code":"SessionWithIdentifierNotFound","message":"session not found"}}`,
		},
	}
	restoreAzureDynamicSessionsClient(t, fake)
	backend := testAzureDynamicSessionsBackend()

	if err := backend.Stop(context.Background(), StopRequest{ID: "stale-session-400"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := resolveLeaseClaimForProvider("stale-session-400", providerName); err != nil || ok {
		t.Fatalf("claim after stale stop ok=%t err=%v", ok, err)
	}
}

func TestResolveSessionIDRequiresLocalClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	backend := testAzureDynamicSessionsBackend()
	client := &recordingAzureDynamicSessionsAPI{}

	_, _, err := backend.resolveSessionID(context.Background(), client, "azds-external", t.TempDir(), false)
	if err == nil || !strings.Contains(err.Error(), "not claimed by Crabbox") {
		t.Fatalf("resolve unclaimed session err=%v, want claim boundary error", err)
	}
	if client.getSessionCalls != 0 {
		t.Fatalf("GetSession calls = %d, want 0", client.getSessionCalls)
	}
}

func TestResolveSessionIDUsesClaimedSlug(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repoA := t.TempDir()
	repoB := t.TempDir()
	claimAzureDynamicSessionsLease(t, "azds-claimed", "claimed-session", repoA, time.Minute)
	backend := testAzureDynamicSessionsBackend()
	client := &recordingAzureDynamicSessionsAPI{}

	if _, _, err := backend.resolveSessionID(context.Background(), client, "claimed-session", repoB, false); err == nil || !strings.Contains(err.Error(), "use --reclaim") {
		t.Fatalf("resolve without reclaim err=%v, want reclaim guard", err)
	}
	leaseID, slug, err := backend.resolveSessionID(context.Background(), client, "claimed-session", repoB, true)
	if err != nil {
		t.Fatal(err)
	}
	if leaseID != "azds-claimed" || slug != "claimed-session" {
		t.Fatalf("resolved lease=%q slug=%q", leaseID, slug)
	}
	if client.getSessionCalls != 0 {
		t.Fatalf("GetSession calls = %d, want 0", client.getSessionCalls)
	}
}

func TestResolveSessionIDRejectsClaimFromDifferentEndpoint(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := claimLeaseForRepoProviderScope("azds-other-pool", "other-pool", providerName, "endpoint:http://127.0.0.1:8788", t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	backend := testAzureDynamicSessionsBackend()
	client := &recordingAzureDynamicSessionsAPI{}

	if _, _, err := backend.resolveSessionID(context.Background(), client, "other-pool", t.TempDir(), false); err == nil || !strings.Contains(err.Error(), "not claimed by Crabbox") {
		t.Fatalf("resolve wrong-scope err=%v, want claim boundary error", err)
	}
	if client.getSessionCalls != 0 {
		t.Fatalf("GetSession calls = %d, want 0", client.getSessionCalls)
	}
}

func claimAzureDynamicSessionsLease(t *testing.T, leaseID, slug, repoRoot string, idleTimeout time.Duration) {
	t.Helper()
	scope := "endpoint:" + testAzureDynamicSessionsEndpoint
	if err := claimLeaseForRepoProviderScope(leaseID, slug, providerName, scope, repoRoot, idleTimeout, false); err != nil {
		t.Fatal(err)
	}
}

type recordingAzureDynamicSessionsAPI struct {
	checkRunnerCalls    int
	getSessionCalls     int
	deleted             []string
	execs               []azureDynamicSessionsExecRequest
	commandExit         int
	commandErr          error
	deleteErr           error
	deleteWaitForCancel bool
	uploadErr           error
	failWorkspace       bool
	failExtract         bool
	executeShell        bool
	getWaitForCancel    bool
	onExec              func(azureDynamicSessionsExecRequest)
}

func (r *recordingAzureDynamicSessionsAPI) CheckRunner(context.Context, string) error {
	r.checkRunnerCalls++
	return nil
}

func (r *recordingAzureDynamicSessionsAPI) UploadFile(_ context.Context, _ string, localPath, remotePath string) error {
	if r.uploadErr != nil {
		return r.uploadErr
	}
	if !r.executeShell {
		return nil
	}
	archive, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	return os.WriteFile(remotePath, archive, 0o600)
}

func (r *recordingAzureDynamicSessionsAPI) ExecStream(ctx context.Context, _ string, req azureDynamicSessionsExecRequest, _ io.Writer, _ io.Writer) (int, error) {
	r.execs = append(r.execs, req)
	if r.onExec != nil {
		r.onExec(req)
	}
	if r.failWorkspace && strings.HasPrefix(req.Command, "mkdir -p ") {
		return 7, nil
	}
	if r.failExtract && strings.HasPrefix(req.Command, "tar -xzf ") {
		return 7, nil
	}
	if (r.commandExit != 0 || r.commandErr != nil) && !strings.HasPrefix(req.Command, "mkdir -p ") {
		return r.commandExit, r.commandErr
	}
	if r.executeShell {
		if err := exec.CommandContext(ctx, "sh", "-c", req.Command).Run(); err != nil {
			return 1, err
		}
	}
	return 0, nil
}

func (r *recordingAzureDynamicSessionsAPI) GetSession(ctx context.Context, _ string) (azureDynamicSessionsSession, error) {
	r.getSessionCalls++
	if r.getWaitForCancel {
		<-ctx.Done()
		return azureDynamicSessionsSession{}, ctx.Err()
	}
	return azureDynamicSessionsSession{}, nil
}

func TestRunPreservesPrimaryFailureThroughCleanupAndTimingWrite(t *testing.T) {
	for _, transport := range []bool{false, true} {
		t.Run(map[bool]string{false: "command", true: "transport"}[transport], func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			cleanupErr := errors.New("synthetic cleanup failure")
			writerErr := errors.New("synthetic writer failure")
			fake := &recordingAzureDynamicSessionsAPI{commandExit: 23, deleteErr: cleanupErr}
			wantCode := 23
			if transport {
				fake.commandErr = context.Canceled
				wantCode = 1
			}
			restoreAzureDynamicSessionsClient(t, fake)
			b := testAzureDynamicSessionsBackend()
			b.rt.Stderr = azureLifecycleFailingWriter{writerErr}
			result, err := b.Run(t.Context(), RunRequest{Repo: Repo{Root: t.TempDir(), Name: "fixture"}, NoSync: true, Command: []string{"workload"}, TimingJSON: true})
			var exitErr ExitError
			if !errors.As(err, &exitErr) || exitErr.Code != wantCode || result.ExitCode != wantCode {
				t.Fatalf("primary exit lost: result=%#v err=%v", result, err)
			}
			if !errors.Is(err, cleanupErr) || !errors.Is(err, writerErr) {
				t.Fatalf("secondary errors lost: %v", err)
			}
			if transport && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
			if len(fake.deleted) != 1 || result.Session == nil || !result.Session.Kept {
				t.Fatalf("cleanup/session=%v/%#v", fake.deleted, result.Session)
			}
			if _, ok, err := resolveLeaseClaimForProvider(result.LeaseID, providerName); err != nil || !ok {
				t.Fatalf("failed cleanup removed claim: %v", err)
			}
		})
	}
}

type azureLifecycleFailingWriter struct{ err error }

func (w azureLifecycleFailingWriter) Write([]byte) (int, error) { return 0, w.err }

func (r *recordingAzureDynamicSessionsAPI) ListSessions(context.Context) ([]azureDynamicSessionsSession, error) {
	return nil, nil
}

func (r *recordingAzureDynamicSessionsAPI) DeleteSession(ctx context.Context, identifier string) error {
	r.deleted = append(r.deleted, identifier)
	if r.deleteWaitForCancel {
		<-ctx.Done()
		return ctx.Err()
	}
	if r.deleteErr != nil {
		return r.deleteErr
	}
	return nil
}

func restoreAzureDynamicSessionsClient(t *testing.T, api azureDynamicSessionsAPI) {
	t.Helper()
	previous := newAzureDynamicSessionsClient
	newAzureDynamicSessionsClient = func(context.Context, Config, Runtime) (azureDynamicSessionsAPI, error) {
		return api, nil
	}
	t.Cleanup(func() {
		newAzureDynamicSessionsClient = previous
	})
}

func testAzureDynamicSessionsBackend() *azureDynamicSessionsBackend {
	return &azureDynamicSessionsBackend{
		spec: Provider{}.Spec(),
		cfg:  Config{AzureDynamicSessions: AzureDynamicSessionsConfig{Endpoint: testAzureDynamicSessionsEndpoint}},
		rt: Runtime{
			Stdout: &bytes.Buffer{},
			Stderr: &bytes.Buffer{},
		},
	}
}

func newAzureDynamicSessionsSyncTestRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar not available")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test/repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("git", "init")
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, output)
	}
	return root
}

func TestRunCleanupBoundsClaimFenceWait(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	oldTimeout := azureDynamicSessionsDeleteTimeout
	azureDynamicSessionsDeleteTimeout = 200 * time.Millisecond
	t.Cleanup(func() { azureDynamicSessionsDeleteTimeout = oldTimeout })
	fake := &recordingAzureDynamicSessionsAPI{}
	restoreAzureDynamicSessionsClient(t, fake)
	backend := testAzureDynamicSessionsBackend()
	release, entered, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	defer close(release)
	holderCtx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	fake.onExec = func(req azureDynamicSessionsExecRequest) {
		if strings.HasPrefix(req.Command, "mkdir -p ") {
			return
		}
		claims, err := listLeaseClaims()
		if err != nil || len(claims) != 1 {
			t.Fatalf("claims=%v err=%v", claims, err)
		}
		go func() {
			done <- core.WithDurableLeaseClaimLockContext(holderCtx, claims[0].LeaseID, func(*core.LeaseClaim, bool, func() error) error {
				close(entered)
				select {
				case <-release:
					return nil
				case <-holderCtx.Done():
					return holderCtx.Err()
				}
			})
		}()
		select {
		case <-entered:
		case err := <-done:
			t.Fatalf("claim fence failed: %v", err)
		case <-holderCtx.Done():
			t.Fatal("claim fence was not acquired")
		}
	}
	result, err := backend.Run(t.Context(), RunRequest{Repo: Repo{Root: t.TempDir(), Name: "fixture"}, NoSync: true, Command: []string{"true"}, TimingJSON: true})
	if !errors.Is(err, context.DeadlineExceeded) || result.ExitCode != 1 || result.ErrorKind != core.RunErrorProvider || len(fake.deleted) != 0 {
		t.Fatalf("cleanup escaped bounded claim fence: result=%#v err=%v deletes=%v", result, err, fake.deleted)
	}
	if result.Session == nil || !result.Session.Kept {
		t.Fatalf("unconfirmed session not retained: %#v", result.Session)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("claim fence holder did not finish")
	}
}
