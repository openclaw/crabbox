package superserve

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
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type fakeSuperserveClient struct {
	mu          sync.Mutex
	baseURL     string
	sandbox     superserveSandbox
	accessToken string
	created     []createSandboxRequest
	updates     []map[string]string
	listFilters []map[string]string
	deleted     []string
	activated   []string
	uploads     []fakeSuperserveUpload
	execs       []execRequest
	execResults []execResult
	execErr     error
	activateErr error
	onExec      func(count int, req execRequest)
	probes      int
	getErr      error
	deleteErr   error
	updateErr   error
}

type fakeSuperserveUpload struct {
	sandboxID string
	path      string
	size      int
}

func newFakeSuperserveClient() *fakeSuperserveClient {
	return &fakeSuperserveClient{
		baseURL:     "https://api.superserve.test",
		accessToken: "ss_test_token",
		sandbox: superserveSandbox{
			ID:     "sb_test01",
			Status: "running",
			Metadata: map[string]string{
				metadataProviderKey: providerName,
			},
		},
	}
}

func (f *fakeSuperserveClient) BaseURL() string { return f.baseURL }

func (f *fakeSuperserveClient) CreateSandbox(_ context.Context, req createSandboxRequest) (superserveSandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, req)
	sb := f.sandbox
	sb.Metadata = cloneMap(req.Metadata)
	f.sandbox = sb
	return sb, nil
}

func (f *fakeSuperserveClient) ListSandboxes(_ context.Context, filter map[string]string) ([]superserveSandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listFilters = append(f.listFilters, cloneMap(filter))
	return []superserveSandbox{cloneSandbox(f.sandbox)}, nil
}

func (f *fakeSuperserveClient) GetSandbox(context.Context, string) (superserveSandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return superserveSandbox{}, f.getErr
	}
	return cloneSandbox(f.sandbox), nil
}

func (f *fakeSuperserveClient) ActivateSandbox(context.Context, string) (sandboxAccess, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.activated = append(f.activated, f.sandbox.ID)
	if f.activateErr != nil {
		return sandboxAccess{}, f.activateErr
	}
	return sandboxAccess{Sandbox: cloneSandbox(f.sandbox), AccessToken: f.accessToken}, nil
}

func (f *fakeSuperserveClient) UpdateSandboxMetadata(_ context.Context, _ string, metadata map[string]string) (superserveSandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updateErr != nil {
		return superserveSandbox{}, f.updateErr
	}
	f.updates = append(f.updates, cloneMap(metadata))
	f.sandbox.Metadata = cloneMap(metadata)
	return cloneSandbox(f.sandbox), nil
}

func (f *fakeSuperserveClient) PauseSandbox(context.Context, string) (superserveSandbox, error) {
	return cloneSandbox(f.sandbox), nil
}

func (f *fakeSuperserveClient) ResumeSandbox(context.Context, string) (sandboxAccess, error) {
	return sandboxAccess{Sandbox: cloneSandbox(f.sandbox), AccessToken: "ss_test_token"}, nil
}

func (f *fakeSuperserveClient) DeleteSandbox(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeSuperserveClient) UploadFile(_ context.Context, access *sandboxAccess, remotePath string, content io.Reader) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, err := io.ReadAll(content)
	if err != nil {
		return err
	}
	f.uploads = append(f.uploads, fakeSuperserveUpload{sandboxID: access.Sandbox.ID, path: remotePath, size: len(data)})
	return nil
}

func (f *fakeSuperserveClient) Exec(_ context.Context, _ *sandboxAccess, req execRequest, stdout, stderr io.Writer) (execResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execs = append(f.execs, req)
	execCount := len(f.execs)
	if f.execErr != nil {
		return execResult{}, f.execErr
	}
	result := execResult{}
	if len(f.execResults) > 0 {
		result = f.execResults[0]
		f.execResults = f.execResults[1:]
	}
	if result.Stdout != "" {
		_, _ = io.WriteString(stdout, result.Stdout)
	}
	if result.Stderr != "" {
		_, _ = io.WriteString(stderr, result.Stderr)
	}
	if f.onExec != nil {
		f.onExec(execCount, req)
	}
	return result, nil
}

func (f *fakeSuperserveClient) Probe(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probes++
	return nil
}

func TestWarmupCreatesClaimAndOwnershipMetadataWithoutToken(t *testing.T) {
	fake := newFakeSuperserveClient()
	backend := newSuperserveTestBackend(t, fake)
	var stdout bytes.Buffer
	backend.rt.Stdout = &stdout

	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: Repo{Name: "my-app", Root: "/repo"}, Keep: true}); err != nil {
		t.Fatalf("Warmup err=%v", err)
	}
	leaseID := leasePrefix + fake.sandbox.ID
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.LeaseID != leaseID || claim.Provider != providerName || claim.ProviderScope == "" {
		t.Fatalf("claim=%#v", claim)
	}
	if strings.Contains(stdout.String(), "ss_test_token") {
		t.Fatalf("warmup output leaked token: %q", stdout.String())
	}
	if strings.Contains(mustReadClaimJSON(t, leaseID), "access_token") || strings.Contains(mustReadClaimJSON(t, leaseID), "ss_test_token") {
		t.Fatalf("claim persisted token: %s", mustReadClaimJSON(t, leaseID))
	}
	if len(fake.created) != 1 || fake.created[0].Metadata[metadataProviderKey] != providerName || fake.created[0].Metadata[metadataScopeKey] == "" {
		t.Fatalf("create metadata=%#v", fake.created)
	}
	if !strings.HasPrefix(fake.created[0].Name, "crabbox-my-app-") || fake.created[0].FromTemplate != backend.cfg.Superserve.Template || fake.created[0].TimeoutSeconds != backend.sandboxTimeoutSecs() {
		t.Fatalf("create request did not use Superserve API fields: %#v", fake.created[0])
	}
	if len(fake.updates) != 1 || fake.updates[0][metadataClaimKey] != leaseID || fake.updates[0][metadataSlugKey] == "" {
		t.Fatalf("update metadata=%#v", fake.updates)
	}
}

func TestWarmupSnapshotClearsDefaultTemplate(t *testing.T) {
	fake := newFakeSuperserveClient()
	backend := newSuperserveTestBackend(t, fake)
	backend.cfg.Superserve.Template = "superserve/base"
	backend.cfg.Superserve.Snapshot = "snap-123"

	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: Repo{Name: "my-app", Root: "/repo"}, Keep: true}); err != nil {
		t.Fatalf("Warmup err=%v", err)
	}
	if len(fake.created) != 1 {
		t.Fatalf("created=%#v", fake.created)
	}
	if fake.created[0].FromTemplate != "" || fake.created[0].FromSnapshot != "snap-123" {
		t.Fatalf("snapshot create sent wrong source: %#v", fake.created[0])
	}
}

func TestListRequiresLocalClaimAndMatchingRemoteMetadata(t *testing.T) {
	fake := newFakeSuperserveClient()
	backend := newSuperserveTestBackend(t, fake)
	leaseID, scope := createSuperserveClaim(t, backend, fake, "listed")
	fake.sandbox.Metadata = ownedMetadata(fake.baseURL, scope, leaseID, "listed")

	views, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatalf("List err=%v", err)
	}
	if len(views) != 1 || views[0].CloudID != fake.sandbox.ID || views[0].Labels["lease"] != leaseID {
		t.Fatalf("views=%#v", views)
	}
	if len(fake.listFilters) != 1 || fake.listFilters[0][metadataProviderKey] != providerName || fake.listFilters[0][metadataEndpointKey] == "" {
		t.Fatalf("list filters=%#v", fake.listFilters)
	}

	fake.sandbox.Metadata[metadataScopeKey] = "different"
	if _, err := backend.List(context.Background(), ListRequest{}); err == nil || !strings.Contains(err.Error(), "ownership metadata") {
		t.Fatalf("List err=%v, want ownership mismatch", err)
	}
}

func TestStatusAndStopRequireOwnershipBeforeDelete(t *testing.T) {
	fake := newFakeSuperserveClient()
	backend := newSuperserveTestBackend(t, fake)
	leaseID, scope := createSuperserveClaim(t, backend, fake, "owned")
	fake.sandbox.Metadata = ownedMetadata(fake.baseURL, scope, leaseID, "owned")

	status, err := backend.Status(context.Background(), StatusRequest{ID: "owned"})
	if err != nil {
		t.Fatalf("Status err=%v", err)
	}
	if status.ID != leaseID || !status.Ready || status.ServerID != fake.sandbox.ID {
		t.Fatalf("status=%#v", status)
	}

	fake.sandbox.Metadata[metadataScopeKey] = "foreign"
	if err := backend.Stop(context.Background(), StopRequest{ID: leaseID}); err == nil || !strings.Contains(err.Error(), "ownership metadata") {
		t.Fatalf("Stop err=%v, want ownership mismatch", err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("deleted despite ownership mismatch: %#v", fake.deleted)
	}
}

func TestStatusMissingRemoteStateIsUnknownAndNotReady(t *testing.T) {
	fake := newFakeSuperserveClient()
	backend := newSuperserveTestBackend(t, fake)
	leaseID, scope := createSuperserveClaim(t, backend, fake, "unknown")
	fake.sandbox.Metadata = ownedMetadata(fake.baseURL, scope, leaseID, "unknown")
	fake.sandbox.Status = ""
	fake.sandbox.State = ""

	status, err := backend.Status(context.Background(), StatusRequest{ID: "unknown"})
	if err != nil {
		t.Fatalf("Status err=%v", err)
	}
	if status.State != "unknown" || status.Ready {
		t.Fatalf("status=%#v, want unknown and not ready", status)
	}
}

func TestStopForgetMissingRequiresExplicitFlag(t *testing.T) {
	fake := newFakeSuperserveClient()
	backend := newSuperserveTestBackend(t, fake)
	leaseID, scope := createSuperserveClaim(t, backend, fake, "missing")
	fake.sandbox.Metadata = ownedMetadata(fake.baseURL, scope, leaseID, "missing")
	fake.getErr = notFoundErr()

	err := backend.Stop(context.Background(), StopRequest{ID: leaseID})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("Stop err=%v, want missing error", err)
	}
	if claim, err := readLeaseClaim(leaseID); err != nil || claim.LeaseID != leaseID {
		t.Fatalf("claim should remain: %#v err=%v", claim, err)
	}

	backend.cfg.Superserve.ForgetMissing = true
	if err := backend.Stop(context.Background(), StopRequest{ID: leaseID}); err != nil {
		t.Fatalf("Stop forget missing err=%v", err)
	}
	if claim, err := readLeaseClaim(leaseID); err != nil || claim.LeaseID != "" {
		t.Fatalf("claim should be removed: %#v err=%v", claim, err)
	}
}

func TestCleanupDryRunSkipsFreshAndDeletesExpiredOwnedOnly(t *testing.T) {
	fake := newFakeSuperserveClient()
	backend := newSuperserveTestBackend(t, fake)
	backend.cfg.IdleTimeout = time.Minute
	leaseID, scope := createSuperserveClaim(t, backend, fake, "expired")
	fake.sandbox.Metadata = ownedMetadata(fake.baseURL, scope, leaseID, "expired")
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	claim.LastUsedAt = time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	claim.IdleTimeoutSeconds = 60
	writeClaimFixture(t, claim)
	var stdout bytes.Buffer
	backend.rt.Stdout = &stdout

	if err := backend.Cleanup(context.Background(), CleanupRequest{DryRun: true}); err != nil {
		t.Fatalf("Cleanup dry-run err=%v", err)
	}
	if !strings.Contains(stdout.String(), "would delete sandbox="+fake.sandbox.ID) {
		t.Fatalf("stdout=%q, want dry-run delete", stdout.String())
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("dry-run deleted: %#v", fake.deleted)
	}

	stdout.Reset()
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatalf("Cleanup err=%v", err)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != fake.sandbox.ID {
		t.Fatalf("deleted=%#v", fake.deleted)
	}
	if claim, err := readLeaseClaim(leaseID); err != nil || claim.LeaseID != "" {
		t.Fatalf("claim should be gone: %#v err=%v", claim, err)
	}
}

func TestCleanupRejectsNonOwnedRemote(t *testing.T) {
	fake := newFakeSuperserveClient()
	backend := newSuperserveTestBackend(t, fake)
	leaseID, scope := createSuperserveClaim(t, backend, fake, "foreign")
	fake.sandbox.Metadata = ownedMetadata(fake.baseURL, scope, leaseID, "foreign")
	fake.sandbox.Metadata[metadataClaimKey] = leasePrefix + "other"
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	claim.LastUsedAt = time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	claim.IdleTimeoutSeconds = 60
	writeClaimFixture(t, claim)

	err = backend.Cleanup(context.Background(), CleanupRequest{})
	if err == nil || !strings.Contains(err.Error(), "ownership metadata") {
		t.Fatalf("Cleanup err=%v, want ownership mismatch", err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("cleanup deleted non-owned remote: %#v", fake.deleted)
	}
}

func TestDoctorIsNonMutating(t *testing.T) {
	fake := newFakeSuperserveClient()
	backend := newSuperserveTestBackend(t, fake)
	result, err := backend.Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatalf("Doctor err=%v", err)
	}
	if result.Provider != providerName || !strings.Contains(result.Message, "mutation=false") {
		t.Fatalf("doctor result=%#v", result)
	}
	if fake.probes != 1 || len(fake.created) != 0 || len(fake.deleted) != 0 {
		t.Fatalf("doctor mutated: probes=%d created=%d deleted=%d", fake.probes, len(fake.created), len(fake.deleted))
	}
}

func TestRunNoSyncExecForwardsEnvInRequestBodyAndMirrorsOutput(t *testing.T) {
	fake := newFakeSuperserveClient()
	fake.execResults = []execResult{
		{},
		{Stdout: "ok\n", Stderr: "warn\n", ExitCode: 0},
	}
	backend := newSuperserveTestBackend(t, fake)
	var stdout, stderr bytes.Buffer
	backend.rt.Stdout = &stdout
	backend.rt.Stderr = &stderr

	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "my-app", Root: t.TempDir()},
		NoSync:  true,
		Command: []string{"go", "test", "./..."},
		Env: map[string]string{
			"CRABBOX_SUPERSERVE_API_KEY": "crabbox_ss_test_not_real",
			"SUPERSERVE_API_KEY":         "ss_test_not_real",
			"PROJECT_TOKEN":              "project_test_not_real",
			"CI":                         "1",
		},
		EnvSummary: true,
	})
	if err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if result.ExitCode != 0 || result.Provider != providerName || result.LeaseID == "" || !result.SyncDelegated {
		t.Fatalf("result=%#v", result)
	}
	if result.Session == nil {
		t.Fatal("session=nil")
	}
	if result.Session.Provider != providerName || result.Session.LeaseID != result.LeaseID || result.Session.Reused || result.Session.Kept {
		t.Fatalf("session=%#v result=%#v", result.Session, result)
	}
	if result.Session.CleanupCommand != "crabbox stop --provider superserve --id "+shellQuote(result.LeaseID) {
		t.Fatalf("cleanup command=%q", result.Session.CleanupCommand)
	}
	if stdout.String() != "ok\n" || !strings.Contains(stderr.String(), "warn\n") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "ss_test_not_real") || strings.Contains(stderr.String(), "project_test_not_real") {
		t.Fatalf("env summary leaked value: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "CRABBOX_SUPERSERVE_API_KEY,SUPERSERVE_API_KEY") {
		t.Fatalf("stderr=%q, want stripped provider auth warning", stderr.String())
	}
	if len(fake.execs) != 2 {
		t.Fatalf("execs=%#v", fake.execs)
	}
	commandReq := fake.execs[1]
	if commandReq.Command != "'go' 'test' './...'" || commandReq.WorkingDir != defaultWorkdir || commandReq.TimeoutSecs != backend.cfg.Superserve.ExecTimeoutSecs {
		t.Fatalf("command req=%#v", commandReq)
	}
	if commandReq.Env["PROJECT_TOKEN"] != "project_test_not_real" || commandReq.Env["CI"] != "1" {
		t.Fatalf("safe env not forwarded in body: %#v", commandReq.Env)
	}
	if _, ok := commandReq.Env["SUPERSERVE_API_KEY"]; ok {
		t.Fatalf("provider auth forwarded in body: %#v", commandReq.Env)
	}
	if _, ok := commandReq.Env["CRABBOX_SUPERSERVE_API_KEY"]; ok {
		t.Fatalf("provider auth forwarded in body: %#v", commandReq.Env)
	}
	if strings.Contains(commandReq.Command, "ss_test_not_real") {
		t.Fatalf("secret env value appeared in command argv: %q", commandReq.Command)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != fake.sandbox.ID {
		t.Fatalf("one-shot run should delete sandbox: %#v", fake.deleted)
	}
}

func TestRunKeepOnFailureRetainsCreatedSandboxAndExitCode(t *testing.T) {
	fake := newFakeSuperserveClient()
	fake.execResults = []execResult{
		{},
		{Stderr: "boom\n", ExitCode: 7},
	}
	backend := newSuperserveTestBackend(t, fake)
	var stderr bytes.Buffer
	backend.rt.Stderr = &stderr

	result, err := backend.Run(context.Background(), RunRequest{
		Repo:          Repo{Name: "my-app", Root: t.TempDir()},
		NoSync:        true,
		Command:       []string{"false"},
		KeepOnFailure: true,
		TimingJSON:    true,
	})
	if err == nil {
		t.Fatal("expected nonzero run error")
	}
	var exitErr ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 7 {
		t.Fatalf("err=%T %[1]v, want ExitError 7", err)
	}
	if result.ExitCode != 7 {
		t.Fatalf("result=%#v", result)
	}
	if result.Session == nil {
		t.Fatal("session=nil")
	}
	if result.Session.Provider != providerName || result.Session.LeaseID != result.LeaseID || result.Session.Reused || !result.Session.Kept {
		t.Fatalf("session=%#v result=%#v", result.Session, result)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("keep-on-failure deleted sandbox: %#v", fake.deleted)
	}
	if !strings.Contains(stderr.String(), "keep-on-failure: kept lease=") {
		t.Fatalf("stderr=%q, want keep-on-failure hint", stderr.String())
	}
	var report map[string]any
	for _, line := range strings.Split(strings.TrimSpace(stderr.String()), "\n") {
		var candidate map[string]any
		if err := json.Unmarshal([]byte(line), &candidate); err == nil {
			report = candidate
		}
	}
	if report == nil {
		t.Fatalf("stderr does not contain timing JSON: %q", stderr.String())
	}
	if report["runStatus"] != "failed" || report["errorKind"] != "command-exit" {
		t.Fatalf("timing outcome status=%v kind=%v", report["runStatus"], report["errorKind"])
	}
}

func TestRunActivationFailureHonorsRetentionFlags(t *testing.T) {
	for _, tt := range []struct {
		name            string
		keep            bool
		keepOnFailure   bool
		wantDeleted     bool
		wantFailureHint bool
	}{
		{name: "default cleanup", wantDeleted: true},
		{name: "keep", keep: true},
		{name: "keep on failure", keepOnFailure: true, wantFailureHint: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeSuperserveClient()
			fake.activateErr = errors.New("activation unavailable")
			backend := newSuperserveTestBackend(t, fake)
			var stderr bytes.Buffer
			backend.rt.Stderr = &stderr

			_, err := backend.Run(context.Background(), RunRequest{
				Repo:          Repo{Name: "my-app", Root: t.TempDir()},
				NoSync:        true,
				Command:       []string{"true"},
				Keep:          tt.keep,
				KeepOnFailure: tt.keepOnFailure,
			})
			if err == nil || !strings.Contains(err.Error(), "activation unavailable") {
				t.Fatalf("Run err=%v, want activation failure", err)
			}
			if got := len(fake.deleted) == 1; got != tt.wantDeleted {
				t.Fatalf("deleted=%#v, wantDeleted=%t", fake.deleted, tt.wantDeleted)
			}
			leaseID := leasePrefix + fake.sandbox.ID
			claim, claimErr := readLeaseClaim(leaseID)
			if tt.wantDeleted {
				if claimErr != nil || claim.LeaseID != "" {
					t.Fatalf("claim=%#v err=%v, want removed claim", claim, claimErr)
				}
			} else if claimErr != nil || claim.LeaseID != leaseID {
				t.Fatalf("claim=%#v err=%v, want retained claim", claim, claimErr)
			}
			if got := strings.Contains(stderr.String(), "keep-on-failure: kept lease="); got != tt.wantFailureHint {
				t.Fatalf("stderr=%q, wantFailureHint=%t", stderr.String(), tt.wantFailureHint)
			}
		})
	}
}

func TestRunPropagatesOneShotDeleteFailureAndPreservesClaim(t *testing.T) {
	fake := newFakeSuperserveClient()
	fake.execResults = []execResult{{}, {ExitCode: 0}}
	fake.deleteErr = errors.New("delete denied")
	backend := newSuperserveTestBackend(t, fake)
	var stderr bytes.Buffer
	backend.rt.Stderr = &stderr

	result, err := backend.Run(context.Background(), RunRequest{
		Repo:       Repo{Name: "my-app", Root: t.TempDir()},
		NoSync:     true,
		Command:    []string{"true"},
		TimingJSON: true,
	})
	if err == nil || !strings.Contains(err.Error(), "delete denied") {
		t.Fatalf("Run err=%v, want delete failure", err)
	}
	if result.ExitCode != 1 {
		t.Fatalf("result=%#v, want cleanup failure exit 1", result)
	}
	if result.Session == nil {
		t.Fatal("session=nil")
	}
	if result.Session.Provider != providerName || result.Session.LeaseID != result.LeaseID || result.Session.Reused || !result.Session.Kept {
		t.Fatalf("session=%#v result=%#v", result.Session, result)
	}
	leaseID := leasePrefix + fake.sandbox.ID
	if claim, claimErr := readLeaseClaim(leaseID); claimErr != nil || claim.LeaseID != leaseID {
		t.Fatalf("claim should remain for cleanup retry: %#v err=%v", claim, claimErr)
	}
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	var report struct {
		RunStatus string `json:"runStatus"`
		ErrorKind string `json:"errorKind"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &report); err != nil {
		t.Fatalf("timing json: %v\nstderr=%s", err, stderr.String())
	}
	if report.RunStatus != "failed" || report.ErrorKind != "provider-error" {
		t.Fatalf("timing outcome status=%q kind=%q", report.RunStatus, report.ErrorKind)
	}
}

func TestRunPreservesCommandExitCodeWhenDeleteFails(t *testing.T) {
	fake := newFakeSuperserveClient()
	fake.execResults = []execResult{{}, {ExitCode: 23}}
	fake.deleteErr = errors.New("delete denied")
	backend := newSuperserveTestBackend(t, fake)

	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Name: "my-app", Root: t.TempDir()},
		NoSync:  true,
		Command: []string{"false"},
	})
	var exitErr ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 23 {
		t.Fatalf("Run err=%T %[1]v, want wrapped ExitError 23", err)
	}
	if !strings.Contains(err.Error(), "delete denied") || result.ExitCode != 23 {
		t.Fatalf("Run result=%#v err=%v, want exit 23 and cleanup failure", result, err)
	}
}

func TestRunRefreshesRetainedClaimActivityAfterSuccessfulCommand(t *testing.T) {
	fake := newFakeSuperserveClient()
	fake.execResults = []execResult{
		{},
		{ExitCode: 0},
	}
	backend := newSuperserveTestBackend(t, fake)
	leaseID, scope := createSuperserveClaim(t, backend, fake, "retained")
	fake.sandbox.Metadata = ownedMetadata(fake.baseURL, scope, leaseID, "retained")
	stale := time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	fake.onExec = func(count int, _ execRequest) {
		if count != 2 {
			return
		}
		claim, err := readLeaseClaim(leaseID)
		if err != nil {
			t.Fatal(err)
		}
		claim.LastUsedAt = stale
		writeClaimFixture(t, claim)
	}

	result, err := backend.Run(context.Background(), RunRequest{
		ID:      "retained",
		Repo:    Repo{Name: "my-app", Root: "/repo"},
		NoSync:  true,
		Keep:    true,
		Command: []string{"true"},
	})
	if err != nil {
		t.Fatalf("Run err=%v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("result=%#v", result)
	}
	if result.Session == nil {
		t.Fatal("session=nil")
	}
	if result.Session.Provider != providerName || result.Session.LeaseID != leaseID || result.Session.Slug != "retained" || !result.Session.Reused || !result.Session.Kept {
		t.Fatalf("session=%#v", result.Session)
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.LastUsedAt == stale {
		t.Fatalf("claim activity was not refreshed after command: %#v", claim)
	}
	refreshed, err := time.Parse(time.RFC3339, claim.LastUsedAt)
	if err != nil {
		t.Fatal(err)
	}
	staleTime, err := time.Parse(time.RFC3339, stale)
	if err != nil {
		t.Fatal(err)
	}
	if !refreshed.After(staleTime) {
		t.Fatalf("last_used_at=%s should be after stale marker %s", claim.LastUsedAt, stale)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("retained run deleted sandbox: %#v", fake.deleted)
	}
}

func TestCleanupSerializesAndRechecksLeaseActivity(t *testing.T) {
	fake := newFakeSuperserveClient()
	backend := newSuperserveTestBackend(t, fake)
	leaseID, scope := createSuperserveClaim(t, backend, fake, "active")
	fake.sandbox.Metadata = ownedMetadata(fake.baseURL, scope, leaseID, "active")
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	claim.LastUsedAt = time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	claim.IdleTimeoutSeconds = 60
	writeClaimFixture(t, claim)

	unlock, err := lockSuperserveLeaseOperation(context.Background(), leaseID)
	if err != nil {
		t.Fatal(err)
	}
	cleanupDone := make(chan error, 1)
	go func() {
		cleanupDone <- backend.Cleanup(context.Background(), CleanupRequest{})
	}()
	select {
	case err := <-cleanupDone:
		t.Fatalf("cleanup completed while lease operation lock was held: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	claim.LastUsedAt = time.Now().UTC().Format(time.RFC3339)
	writeClaimFixture(t, claim)
	unlock()
	if err := <-cleanupDone; err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("cleanup deleted refreshed active sandbox: %#v", fake.deleted)
	}
}

func TestSuperserveOperationLockHonorsContextCancellation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	unlock, err := lockSuperserveLeaseOperation(context.Background(), leasePrefix+"lock-test")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := lockSuperserveLeaseOperation(ctx, leasePrefix+"lock-test"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want context deadline", err)
	}
}

func TestRunSyncOnlyUploadsArchiveExtractsAndCleansRemoteArchive(t *testing.T) {
	repo := tempSuperserveGitRepo(t)
	fake := newFakeSuperserveClient()
	backend := newSuperserveTestBackend(t, fake)
	var stdout bytes.Buffer
	backend.rt.Stdout = &stdout

	result, err := backend.Run(context.Background(), RunRequest{
		Repo:     Repo{Name: "my-app", Root: repo},
		SyncOnly: true,
		Keep:     true,
	})
	if err != nil {
		t.Fatalf("Run sync-only err=%v", err)
	}
	if result.ExitCode != 0 || !strings.Contains(stdout.String(), "synced "+defaultWorkdir) {
		t.Fatalf("result=%#v stdout=%q", result, stdout.String())
	}
	if result.Session == nil {
		t.Fatal("session=nil")
	}
	if result.Session.Provider != providerName || result.Session.LeaseID != result.LeaseID || result.Session.Reused || !result.Session.Kept {
		t.Fatalf("session=%#v result=%#v", result.Session, result)
	}
	if len(fake.uploads) != 1 || !strings.HasPrefix(fake.uploads[0].path, "/tmp/crabbox-sync-") || fake.uploads[0].size == 0 {
		t.Fatalf("uploads=%#v", fake.uploads)
	}
	commands := execCommands(fake.execs)
	if !containsCommandAll(commands, "mkdir -p", defaultWorkdir) {
		t.Fatalf("commands=%#v, want workdir prepare", commands)
	}
	if !containsCommand(commands, "tar -xzf ") {
		t.Fatalf("commands=%#v, want tar extraction", commands)
	}
	if !containsCommandAll(commands, "rm -f", fake.uploads[0].path) {
		t.Fatalf("commands=%#v, want remote archive cleanup", commands)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("--keep sync-only deleted sandbox: %#v", fake.deleted)
	}
}

func TestRunRejectsTailscaleBeforeCreate(t *testing.T) {
	fake := newFakeSuperserveClient()
	backend := newSuperserveTestBackend(t, fake)
	_, err := backend.Run(context.Background(), RunRequest{
		Repo: Repo{Name: "my-app", Root: "/repo"},
		Options: core.LeaseOptions{
			Tailscale: core.TailscaleConfig{Enabled: true},
		},
		Command: []string{"true"},
	})
	if err == nil || !strings.Contains(err.Error(), "does not support Tailscale") {
		t.Fatalf("Run err=%v, want Tailscale rejection", err)
	}
	if len(fake.created) != 0 {
		t.Fatalf("created despite guardrail: %#v", fake.created)
	}
}

func TestProviderDoesNotAdvertisePauseResume(t *testing.T) {
	if (Provider{}).Spec().Features.Has(core.FeaturePauseResume) {
		t.Fatal("superserve must not advertise pause/resume until backend methods are implemented")
	}
	if !(Provider{}).Spec().Features.Has(core.FeatureRunSession) {
		t.Fatal("superserve should advertise run-session")
	}
}

func newSuperserveTestBackend(t *testing.T, fake *fakeSuperserveClient) *backend {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := testConfig()
	cfg.Superserve.BaseURL = fake.baseURL
	cfg.IdleTimeout = time.Minute
	rt := Runtime{Stdout: io.Discard, Stderr: io.Discard}
	b := NewSuperserveBackend((Provider{}).Spec(), cfg, rt).(*backend)
	b.newClient = func(Config, Runtime) (superserveClient, error) { return fake, nil }
	return b
}

func createSuperserveClaim(t *testing.T, b *backend, fake *fakeSuperserveClient, slug string) (string, string) {
	t.Helper()
	scope, err := newSuperserveClaimScope(fake.baseURL)
	if err != nil {
		t.Fatal(err)
	}
	leaseID := leasePrefix + fake.sandbox.ID
	if err := claimLeaseForRepoProviderScopePond(leaseID, slug, providerName, scope, "", "/repo", b.cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	return leaseID, scope
}

func ownedMetadata(baseURL, scope, leaseID, slug string) map[string]string {
	return map[string]string{
		metadataProviderKey: providerName,
		metadataEndpointKey: superserveEndpointScope(baseURL),
		metadataScopeKey:    scope,
		metadataClaimKey:    leaseID,
		metadataSlugKey:     slug,
	}
}

func notFoundErr() error {
	return &superserveAPIError{StatusCode: http.StatusNotFound, err: errors.New("not found")}
}

func mustReadClaimJSON(t *testing.T, leaseID string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(os.Getenv("XDG_STATE_HOME"), "crabbox", "claims", leaseID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func writeClaimFixture(t *testing.T, claim LeaseClaim) {
	t.Helper()
	data, err := json.MarshalIndent(claim, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("XDG_STATE_HOME"), "crabbox", "claims", claim.LeaseID+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func tempSuperserveGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.test/my-app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "init")
	runGit(t, dir, "add", ".")
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

func execCommands(reqs []execRequest) []string {
	out := make([]string, 0, len(reqs))
	for _, req := range reqs {
		out = append(out, req.Command)
	}
	return out
}

func containsCommand(commands []string, needle string) bool {
	for _, command := range commands {
		if strings.Contains(command, needle) {
			return true
		}
	}
	return false
}

func containsCommandAll(commands []string, needles ...string) bool {
	for _, command := range commands {
		ok := true
		for _, needle := range needles {
			if !strings.Contains(command, needle) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func cloneMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneSandbox(in superserveSandbox) superserveSandbox {
	in.Metadata = cloneMap(in.Metadata)
	return in
}
