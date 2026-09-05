package vercelsandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type lifecycleFakeClient struct {
	sandboxes   map[string]sandboxSummary
	scope       projectScope
	nextID      int
	calls       []string
	uploads     []string
	execs       []execRequest
	deleteErr   error
	execErr     error
	workloadErr error
	exitCode    int
	stdout      string
	stderr      string
	useNameID   bool
	creates     []createSandboxRequest
}

func newLifecycleFakeClient() *lifecycleFakeClient {
	return &lifecycleFakeClient{
		sandboxes: map[string]sandboxSummary{},
		scope: projectScope{
			ProjectID: "prj_test",
			TeamID:    "team_test",
		},
	}
}

func (f *lifecycleFakeClient) CheckSDK(context.Context) error           { return nil }
func (f *lifecycleFakeClient) CheckCLI(context.Context) (string, error) { return "/bin/sandbox", nil }
func (f *lifecycleFakeClient) CheckAuth(context.Context) error          { return nil }
func (f *lifecycleFakeClient) CheckProject(context.Context) error       { return nil }
func (f *lifecycleFakeClient) ResolveProjectScope(context.Context, bool) (projectScope, error) {
	return f.scope, nil
}
func (f *lifecycleFakeClient) ListSandboxes(context.Context) ([]sandboxSummary, error) {
	f.calls = append(f.calls, "list")
	out := make([]sandboxSummary, 0, len(f.sandboxes))
	for _, sb := range f.sandboxes {
		out = append(out, sb)
	}
	return out, nil
}

func (f *lifecycleFakeClient) CreateSandbox(_ context.Context, req createSandboxRequest) (sandboxSummary, error) {
	f.calls = append(f.calls, "create")
	f.creates = append(f.creates, req)
	f.nextID++
	id := "sbx_" + string(rune('a'+f.nextID-1))
	if f.useNameID && strings.TrimSpace(req.Name) != "" {
		id = req.Name
	}
	sb := sandboxSummary{ID: id, Name: req.Name, State: "running", Metadata: cloneMap(req.Metadata)}
	f.sandboxes[id] = sb
	return sb, nil
}

func (f *lifecycleFakeClient) GetSandbox(_ context.Context, id string) (sandboxSummary, error) {
	f.calls = append(f.calls, "get:"+id)
	sb, ok := f.sandboxes[id]
	if !ok {
		return sandboxSummary{}, &vercelSandboxNotFoundError{err: errors.New("not found")}
	}
	return sb, nil
}

func (f *lifecycleFakeClient) UpdateSandboxMetadata(_ context.Context, id string, metadata map[string]string) (sandboxSummary, error) {
	f.calls = append(f.calls, "metadata:"+id)
	sb, ok := f.sandboxes[id]
	if !ok {
		return sandboxSummary{}, &vercelSandboxNotFoundError{err: errors.New("not found")}
	}
	sb.Metadata = cloneMap(metadata)
	f.sandboxes[id] = sb
	return sb, nil
}

func (f *lifecycleFakeClient) DeleteSandbox(_ context.Context, id string) error {
	f.calls = append(f.calls, "delete:"+id)
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if _, ok := f.sandboxes[id]; !ok {
		return &vercelSandboxNotFoundError{err: errors.New("not found")}
	}
	delete(f.sandboxes, id)
	return nil
}

func (f *lifecycleFakeClient) UploadFile(_ context.Context, id, remotePath string, content io.Reader) error {
	f.calls = append(f.calls, "upload:"+id)
	_, _ = io.Copy(io.Discard, content)
	f.uploads = append(f.uploads, remotePath)
	return nil
}

func (f *lifecycleFakeClient) Exec(_ context.Context, id string, req execRequest, stdout, stderr io.Writer) (execResult, error) {
	f.calls = append(f.calls, "exec:"+id)
	f.execs = append(f.execs, req)
	if f.stdout != "" {
		_, _ = io.WriteString(stdout, f.stdout)
	}
	if f.stderr != "" {
		_, _ = io.WriteString(stderr, f.stderr)
	}
	if f.execErr != nil {
		return execResult{}, f.execErr
	}
	if req.WorkingDir == "" {
		return execResult{ExitCode: 0}, nil
	}
	if f.workloadErr != nil {
		return execResult{}, f.workloadErr
	}
	return execResult{ExitCode: f.exitCode, Stdout: f.stdout, Stderr: f.stderr}, nil
}

func TestWarmupCreatesOwnedClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	fake := newLifecycleFakeClient()
	backend := testBackend(fake, &stdout, &stderr)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: Repo{Name: "my-app", Root: "/repo"}, Keep: true}); err != nil {
		t.Fatal(err)
	}
	if len(fake.sandboxes) != 1 {
		t.Fatalf("sandboxes=%#v", fake.sandboxes)
	}
	var sb sandboxSummary
	for _, value := range fake.sandboxes {
		sb = value
	}
	leaseID := leasePrefix + sb.ID
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Provider != providerName || claim.LeaseID != leaseID || claim.Slug == "" {
		t.Fatalf("claim=%#v", claim)
	}
	if sb.Metadata[metadataProviderKey] != providerName || sb.Metadata[metadataClaimKey] != leaseID || sb.Metadata[metadataScopeKey] != claim.ProviderScope {
		t.Fatalf("metadata=%#v claim=%#v", sb.Metadata, claim)
	}
	if !strings.Contains(stdout.String(), "provider=vercel-sandbox") || strings.Contains(stdout.String(), "secret") {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestWarmupStampsClaimMetadataAtCreateWhenNameIsRemoteID(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newLifecycleFakeClient()
	fake.useNameID = true
	backend := testBackend(fake, io.Discard, io.Discard)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: Repo{Name: "my-app", Root: "/repo"}, Keep: true}); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(fake.calls, func(call string) bool { return strings.HasPrefix(call, "metadata:") }) {
		t.Fatalf("name-id create should not require post-create metadata update: %v", fake.calls)
	}
	for id, sb := range fake.sandboxes {
		leaseID := leasePrefix + id
		if sb.Metadata[metadataClaimKey] != leaseID || sb.Metadata[metadataSlugKey] == "" {
			t.Fatalf("metadata not stamped at create: id=%s metadata=%#v", id, sb.Metadata)
		}
		if len(sb.Metadata) > 5 {
			t.Fatalf("metadata exceeds Vercel's five-tag limit: %#v", sb.Metadata)
		}
	}
}

func TestRunOneShotSyncsExecutesAndDeletes(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := tempRepo(t)
	var stdout, stderr bytes.Buffer
	fake := newLifecycleFakeClient()
	fake.stdout = "ok\n"
	backend := testBackend(fake, &stdout, &stderr)
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:               Repo{Name: "my-app", Root: repo},
		Command:            []string{"echo", "ok", "&&"},
		CommandLiteralArgs: map[int]bool{2: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || !result.SyncDelegated || result.Provider != providerName {
		t.Fatalf("result=%#v", result)
	}
	if result.Session == nil || result.Session.Provider != providerName || result.Session.Reused || result.Session.Kept || !strings.Contains(result.Session.CleanupCommand, "crabbox stop --provider vercel-sandbox") {
		t.Fatalf("unexpected one-shot session handle: %#v", result.Session)
	}
	if len(fake.sandboxes) != 0 {
		t.Fatalf("one-shot sandbox not deleted: %#v", fake.sandboxes)
	}
	if len(fake.uploads) != 1 {
		t.Fatalf("uploads=%v", fake.uploads)
	}
	if !sawExecContaining(fake.execs, "tar -xzf") {
		t.Fatalf("sync extract command missing: %#v", fake.execs)
	}
	if !sawExecContaining(fake.execs, "'echo' 'ok'") {
		t.Fatalf("run command missing: %#v", fake.execs)
	}
	if !strings.Contains(stdout.String(), "ok\n") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if result.CommandText != "'echo' 'ok' '&&'" {
		t.Fatalf("literal intent lost in final payload: %q", result.CommandText)
	}
}

func TestRunRejectsOversizedWorkspaceBeforeProviderCalls(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newLifecycleFakeClient()
	backend := testBackend(fake, io.Discard, io.Discard)
	backend.cfg.Sync.FailFiles = 2
	backend.cfg.Sync.FailBytes = 0

	_, err := backend.Run(context.Background(), RunRequest{
		Repo: Repo{Name: "my-app", Root: tempRepo(t)}, Command: []string{"true"},
	})
	if err == nil || !strings.Contains(err.Error(), "sync candidate too large: 2 files >= limit 2") {
		t.Fatalf("err=%v", err)
	}
	if len(fake.calls) != 0 || len(fake.creates) != 0 || len(fake.sandboxes) != 0 {
		t.Fatalf("provider calls=%v creates=%v sandboxes=%v", fake.calls, fake.creates, fake.sandboxes)
	}
}

func TestRetainedRunByIDVerifiesOwnershipAndKeepsClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := tempRepo(t)
	fake := newLifecycleFakeClient()
	backend := testBackend(fake, io.Discard, io.Discard)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: Repo{Name: "my-app", Root: repo}, Keep: true}); err != nil {
		t.Fatal(err)
	}
	var leaseID string
	for id := range fake.sandboxes {
		leaseID = leasePrefix + id
	}
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "my-app", Root: repo},
		ID:      leaseID,
		NoSync:  true,
		Keep:    true,
		Command: []string{"pwd"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.LeaseID != leaseID {
		t.Fatalf("lease=%q want %q", result.LeaseID, leaseID)
	}
	if result.Session == nil || result.Session.LeaseID != leaseID || !result.Session.Reused || !result.Session.Kept {
		t.Fatalf("unexpected retained session handle: %#v", result.Session)
	}
	if _, err := readLeaseClaim(leaseID); err != nil {
		t.Fatalf("claim not retained: %v", err)
	}
	if len(fake.sandboxes) != 1 {
		t.Fatalf("retained sandbox deleted: %#v", fake.sandboxes)
	}
}

func TestListHydratesClaimOnlyInventoryBeforeOwnershipFilter(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newLifecycleFakeClient()
	backend := testBackend(fake, io.Discard, io.Discard)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: Repo{Name: "my-app", Root: "/repo"}, Keep: true}); err != nil {
		t.Fatal(err)
	}
	var leaseID string
	for id := range fake.sandboxes {
		leaseID = leasePrefix + id
	}
	original := fake.sandboxes
	fake.sandboxes = map[string]sandboxSummary{
		strings.TrimPrefix(leaseID, leasePrefix): original[strings.TrimPrefix(leaseID, leasePrefix)],
	}
	fakeList := []sandboxSummary{{ID: leaseID}}
	listingFake := &claimOnlyListFakeClient{lifecycleFakeClient: fake, list: fakeList}
	backend.newClient = func(Config, Runtime) (vercelSandboxClient, error) {
		return listingFake, nil
	}
	views, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Labels["lease"] != leaseID {
		t.Fatalf("views=%#v", views)
	}
	if !slices.Contains(listingFake.calls, "get:"+strings.TrimPrefix(leaseID, leasePrefix)) {
		t.Fatalf("claim-only inventory was not hydrated: %v", listingFake.calls)
	}
}

func TestListSkipsMissingClaimAndReturnsRemainingSandbox(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newLifecycleFakeClient()
	backend := testBackend(fake, io.Discard, io.Discard)
	for _, name := range []string{"first", "second"} {
		if err := backend.Warmup(context.Background(), WarmupRequest{Repo: Repo{Name: name, Root: "/repo"}, Keep: true}); err != nil {
			t.Fatal(err)
		}
	}
	leaseIDs := make([]string, 0, len(fake.sandboxes))
	for id := range fake.sandboxes {
		leaseIDs = append(leaseIDs, leasePrefix+id)
	}
	delete(fake.sandboxes, strings.TrimPrefix(leaseIDs[0], leasePrefix))
	listingFake := &claimOnlyListFakeClient{
		lifecycleFakeClient: fake,
		list: []sandboxSummary{
			{ID: leaseIDs[0]},
			{ID: leaseIDs[1]},
		},
	}
	backend.newClient = func(Config, Runtime) (vercelSandboxClient, error) {
		return listingFake, nil
	}
	views, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Labels["lease"] != leaseIDs[1] {
		t.Fatalf("views=%#v leaseIDs=%v", views, leaseIDs)
	}
}

func TestEmptyListAndCleanupDoNotResolveProjectScope(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newLifecycleFakeClient()
	fake.scope = projectScope{}
	backend := testBackend(fake, io.Discard, io.Discard)
	if views, err := backend.List(context.Background(), ListRequest{}); err != nil || views == nil || len(views) != 0 {
		t.Fatalf("views=%#v err=%v", views, err)
	}
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyScopeClaimRemainsManageableAfterBinding(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newLifecycleFakeClient()
	backend := testBackend(fake, io.Discard, io.Discard)
	backend.cfg.VercelSandbox.ProjectID = "vercel-sandbox-default-project"
	backend.cfg.VercelSandbox.TeamID = ""
	backend.cfg.VercelSandbox.Scope = "example-org"
	fake.scope = projectScope{ProjectID: "vercel-sandbox-default-project", TeamID: "example-org"}
	legacyScope := backend.providerScopeBase() + "/ownership:legacy"
	leaseID := leasePrefix + "legacy"
	slug := "legacy-box"
	if err := claimLeaseForRepoProviderScopePond(leaseID, slug, providerName, legacyScope, "", "/repo", time.Hour, false); err != nil {
		t.Fatal(err)
	}
	fake.sandboxes["legacy"] = sandboxSummary{
		ID:    "legacy",
		State: "running",
		Metadata: map[string]string{
			metadataProviderKey: providerName,
			metadataScopeKey:    legacyScope,
			metadataClaimKey:    leaseID,
			metadataSlugKey:     slug,
		},
	}
	views, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Labels["lease"] != leaseID {
		t.Fatalf("views=%#v", views)
	}
	if err := backend.Stop(context.Background(), StopRequest{ID: slug}); err != nil {
		t.Fatal(err)
	}
	if len(fake.sandboxes) != 0 {
		t.Fatalf("legacy sandbox was not deleted: %#v", fake.sandboxes)
	}
	if claim, err := readLeaseClaim(leaseID); err != nil || claim.LeaseID != "" {
		t.Fatalf("legacy claim was not removed: claim=%#v err=%v", claim, err)
	}
}

func TestStopRejectsOwnershipMismatch(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newLifecycleFakeClient()
	backend := testBackend(fake, io.Discard, io.Discard)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: Repo{Name: "my-app", Root: "/repo"}, Keep: true}); err != nil {
		t.Fatal(err)
	}
	var leaseID, sandboxID string
	for id, sb := range fake.sandboxes {
		sandboxID = id
		leaseID = leasePrefix + id
		sb.Metadata[metadataProviderKey] = "foreign"
		fake.sandboxes[id] = sb
	}
	err := backend.Stop(context.Background(), StopRequest{ID: leaseID})
	if err == nil || !strings.Contains(err.Error(), "ownership metadata") {
		t.Fatalf("err=%v", err)
	}
	if _, ok := fake.sandboxes[sandboxID]; !ok {
		t.Fatalf("foreign sandbox was deleted")
	}
}

type claimOnlyListFakeClient struct {
	*lifecycleFakeClient
	list []sandboxSummary
}

func (f *claimOnlyListFakeClient) ListSandboxes(context.Context) ([]sandboxSummary, error) {
	f.calls = append(f.calls, "list")
	return f.list, nil
}

func TestCleanupPreservesMissingClaimUnlessForgetMissing(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newLifecycleFakeClient()
	backend := testBackend(fake, io.Discard, io.Discard)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: Repo{Name: "my-app", Root: "/repo"}, Keep: true}); err != nil {
		t.Fatal(err)
	}
	var leaseID string
	for id := range fake.sandboxes {
		leaseID = leasePrefix + id
		delete(fake.sandboxes, id)
	}
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := readLeaseClaim(leaseID); err != nil {
		t.Fatalf("claim should be preserved without forget-missing: %v", err)
	}
	backend.cfg.VercelSandbox.ForgetMissing = true
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.LeaseID != "" {
		t.Fatalf("claim should be removed with forget-missing: %#v", claim)
	}
}

func TestRunForwardsAllowedEnvOffArgvAndStripsProviderSecrets(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newLifecycleFakeClient()
	backend := testBackend(fake, io.Discard, io.Discard)
	secretValue := "secret-token-value"
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "my-app", Root: tempRepo(t)},
		Keep:    true,
		NoSync:  true,
		Command: []string{"env"},
		Env: map[string]string{
			"PUBLIC_VALUE":         "visible",
			"VERCEL_AUTH_TOKEN":    secretValue,
			"CRABBOX_VERCEL_TOKEN": "another-secret",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("result=%#v", result)
	}
	last := fake.execs[len(fake.execs)-1]
	if last.Env["PUBLIC_VALUE"] != "visible" {
		t.Fatalf("allowed env not forwarded: %#v", last.Env)
	}
	if _, ok := last.Env["VERCEL_AUTH_TOKEN"]; ok {
		t.Fatalf("provider secret forwarded to command env: %#v", last.Env)
	}
	if strings.Contains(strings.Join(fake.calls, " "), secretValue) {
		t.Fatalf("secret leaked through fake call log: %v", fake.calls)
	}
}

func TestSyncDeleteUsesStagingReplace(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := tempRepo(t)
	fake := newLifecycleFakeClient()
	backend := testBackend(fake, io.Discard, io.Discard)
	backend.cfg.Sync.Delete = true
	if _, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "my-app", Root: repo},
		Command: []string{"true"},
	}); err != nil {
		t.Fatal(err)
	}
	if !sawExecContaining(fake.execs, ".crabbox-sync-") || !sawExecContaining(fake.execs, "mv") || !sawExecContaining(fake.execs, "/vercel/sandbox/crabbox") {
		t.Fatalf("staging replace command missing: %#v", fake.execs)
	}
}

func TestRunKeepOnFailureRetainsClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newLifecycleFakeClient()
	fake.exitCode = 7
	var stderr bytes.Buffer
	backend := testBackend(fake, io.Discard, &stderr)
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:          Repo{Name: "my-app", Root: tempRepo(t)},
		NoSync:        true,
		KeepOnFailure: true,
		Command:       []string{"false"},
		TimingJSON:    true,
	})
	if err == nil || result.ExitCode != 7 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if result.Session == nil || !result.Session.Kept || result.Session.CleanupCommand == "" {
		t.Fatalf("keep-on-failure should return retained session handle: %#v", result.Session)
	}
	if len(fake.sandboxes) != 1 {
		t.Fatalf("sandbox should be retained on failure: %#v", fake.sandboxes)
	}
	if len(fake.creates) != 1 || !fake.creates[0].Persistent {
		t.Fatalf("keep-on-failure sandbox is not persistent: %#v", fake.creates)
	}
	var leaseID string
	for id := range fake.sandboxes {
		leaseID = leasePrefix + id
	}
	if claim, err := readLeaseClaim(leaseID); err != nil || claim.LeaseID == "" {
		t.Fatalf("claim should be retained: claim=%#v err=%v", claim, err)
	}
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	var report map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &report); err != nil {
		t.Fatalf("final stderr line is not timing JSON: %q: %v", lines[len(lines)-1], err)
	}
	if report["runStatus"] != "failed" || report["errorKind"] != "command-exit" {
		t.Fatalf("timing outcome status=%v kind=%v", report["runStatus"], report["errorKind"])
	}
}

func TestRunEarlyFailureKeepsAccurateRecoveryState(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		workspace, keep, deleteFails bool
	}{
		{name: "workspace delete failure", workspace: true, deleteFails: true},
		{name: "kept workspace failure", workspace: true, keep: true},
		{name: "kept command preparation failure", keep: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			fake := newLifecycleFakeClient()
			primary, cleanup := errors.New("workspace failed"), errors.New("delete unavailable")
			req := RunRequest{Repo: Repo{Root: t.TempDir(), Name: "fixture"}, NoSync: true, KeepOnFailure: tc.keep, TimingJSON: true}
			if tc.workspace {
				fake.execErr = primary
				req.Command = []string{"true"}
			}
			if tc.deleteFails {
				fake.deleteErr = cleanup
			}
			var stderr bytes.Buffer
			b := testBackend(fake, io.Discard, &stderr)
			result, err := b.Run(t.Context(), req)
			if err == nil || result.ExitCode != 1 || result.Status != core.RunStatusFailed || result.ErrorKind != core.RunErrorProvider || result.Session == nil || !result.Session.Kept || result.Session.Reused {
				t.Fatalf("early failure result=%#v session=%+v err=%v", result, result.Session, err)
			}
			if tc.workspace && !errors.Is(err, primary) || tc.deleteFails && !errors.Is(err, cleanup) {
				t.Fatalf("failure cause lost: %v", err)
			}
			deletes := 0
			for _, call := range fake.calls {
				if strings.HasPrefix(call, "delete:") {
					deletes++
				}
			}
			if (deletes == 1) != tc.deleteFails || len(fake.sandboxes) != 1 {
				t.Fatalf("cleanup calls=%v sandboxes=%v", fake.calls, fake.sandboxes)
			}
			if claim, err := readLeaseClaim(result.LeaseID); err != nil || claim.LeaseID != result.LeaseID {
				t.Fatalf("recovery claim=%#v err=%v", claim, err)
			}
			var report core.TimingReport
			lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
			if err := json.Unmarshal([]byte(lines[len(lines)-1]), &report); err != nil || report.ExitCode != result.ExitCode || report.RunStatus != result.Status || report.ErrorKind != result.ErrorKind {
				t.Fatalf("final timing=%#v err=%v diagnostics=%s", report, err, stderr.String())
			}
		})
	}
}

type failingTimingWriter struct{ cause error }

func (w failingTimingWriter) Write(data []byte) (int, error) {
	if bytes.HasPrefix(data, []byte("{")) {
		return 0, w.cause
	}
	return len(data), nil
}

func TestRunTimingFailurePreservesPrimaryExit(t *testing.T) {
	for _, keep := range []bool{false, true} {
		t.Run(map[bool]string{false: "cleanup fails", true: "kept"}[keep], func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			fake := newLifecycleFakeClient()
			fake.exitCode = 23
			if !keep {
				fake.deleteErr = errors.New("delete unavailable")
			}
			writeFailure := errors.New("timing output unavailable")
			b := testBackend(fake, io.Discard, failingTimingWriter{writeFailure})
			result, err := b.Run(t.Context(), RunRequest{Repo: Repo{Root: t.TempDir(), Name: "fixture"}, NoSync: true, KeepOnFailure: keep, TimingJSON: true, Command: []string{"false"}})
			var exitErr ExitError
			if !errors.Is(err, writeFailure) || !errors.As(err, &exitErr) || exitErr.Code != 23 || result.ExitCode != 23 || result.ErrorKind != core.RunErrorCommandExit || result.Session == nil || !result.Session.Kept {
				t.Fatalf("timing masked outcome: result=%#v err=%v", result, err)
			}
			if fake.deleteErr != nil && !errors.Is(err, fake.deleteErr) {
				t.Fatalf("cleanup diagnostic lost: %v", err)
			}
		})
	}
}

func TestRunRejectedReuseReleasesOperationLock(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newLifecycleFakeClient()
	b := testBackend(fake, io.Discard, io.Discard)
	repo := Repo{Root: t.TempDir(), Name: "fixture"}
	if err := b.Warmup(t.Context(), WarmupRequest{Repo: repo, Keep: true}); err != nil {
		t.Fatal(err)
	}
	leaseID := leasePrefix + "sbx_a"
	fake.sandboxes["sbx_a"].Metadata[metadataClaimKey] = "changed"
	result, err := b.Run(t.Context(), RunRequest{ID: leaseID, Repo: repo, NoSync: true, Command: []string{"true"}})
	if err == nil || result.Session != nil {
		t.Fatalf("rejected reuse result=%#v err=%v", result, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	unlock, err := lockVercelSandboxLeaseOperation(ctx, leaseID)
	if err != nil {
		t.Fatalf("failed reuse leaked operation lock: %v", err)
	}
	unlock()
	if claim, err := readLeaseClaim(leaseID); err != nil || claim.RepoRoot != repo.Root || len(fake.sandboxes) != 1 {
		t.Fatalf("rejected reuse changed custody: claim=%#v err=%v", claim, err)
	}
}

func TestRunRedactsProviderTokenAndPreservesCause(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	token := "synthetic-lifecycle-token"
	t.Setenv("VERCEL_TOKEN", token)
	fake := newLifecycleFakeClient()
	fake.workloadErr = errors.Join(errors.New("backend rejected "+token), context.DeadlineExceeded)
	b := testBackend(fake, io.Discard, io.Discard)
	result, err := b.Run(t.Context(), RunRequest{Repo: Repo{Root: t.TempDir(), Name: "fixture"}, NoSync: true, KeepOnFailure: true, Command: []string{"true"}})
	if err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("provider display redaction lost: %v", err)
	}
	if !errors.Is(err, fake.workloadErr) || !errors.Is(err, context.DeadlineExceeded) || result.Status != core.RunStatusTimedOut || result.Session == nil || !result.Session.Kept {
		t.Fatalf("primary cause or retention lost: result=%#v err=%v", result, err)
	}
}

func TestAbortedSandboxStateIsTerminal(t *testing.T) {
	if !isTerminalState("aborted") {
		t.Fatal("aborted sandbox state should be terminal")
	}
}

func testBackend(fake *lifecycleFakeClient, stdout, stderr io.Writer) *backend {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.VercelSandbox.Runtime = defaultRuntime
	cfg.VercelSandbox.Workdir = defaultWorkdir
	cfg.VercelSandbox.ProjectID = "prj_test"
	cfg.VercelSandbox.TeamID = "team_test"
	cfg.IdleTimeout = time.Hour
	return &backend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		rt: Runtime{
			Stdout: stdout,
			Stderr: stderr,
		},
		newClient: func(Config, Runtime) (vercelSandboxClient, error) {
			return fake, nil
		},
	}
}

func tempRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.txt"), []byte("app\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "-C", dir, "init").Run(); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "-C", dir, "add", ".").Run(); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "-C", dir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "init").Run(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func cloneMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func sawExecContaining(execReqs []execRequest, needle string) bool {
	return slices.ContainsFunc(execReqs, func(req execRequest) bool {
		return strings.Contains(req.Command, needle)
	})
}
