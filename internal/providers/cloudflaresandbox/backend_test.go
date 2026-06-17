package cloudflaresandbox

import (
	"bytes"
	"context"
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
	sandboxes          map[string]sandboxSummary
	nextID             int
	calls              []string
	uploads            []string
	execs              []execRequest
	deleteErr          error
	execErr            error
	exitCode           int
	stdout             string
	stderr             string
	creates            []createSandboxRequest
	generateIDs        bool
	healthOK           bool
	omitCreateMetadata bool
	dropMetadata       bool
	openAPIErr         error
}

func newLifecycleFakeClient() *lifecycleFakeClient {
	return &lifecycleFakeClient{sandboxes: map[string]sandboxSummary{}, healthOK: true}
}

func (f *lifecycleFakeClient) Health(context.Context) (healthResponse, error) {
	status := "ready"
	if !f.healthOK {
		status = "degraded"
	}
	return healthResponse{OK: f.healthOK, Status: status}, nil
}

func (f *lifecycleFakeClient) OpenAPI(context.Context) (openAPIResponse, error) {
	if f.openAPIErr != nil {
		return openAPIResponse{}, f.openAPIErr
	}
	var out openAPIResponse
	out.Info.Title = "fake"
	return out, nil
}

func (f *lifecycleFakeClient) ListSandboxes(context.Context) ([]sandboxSummary, error) {
	f.calls = append(f.calls, "list")
	out := make([]sandboxSummary, 0, len(f.sandboxes))
	for _, sb := range f.sandboxes {
		out = append(out, sb)
	}
	return out, nil
}

func (f *lifecycleFakeClient) ListRunning(ctx context.Context) ([]sandboxSummary, error) {
	return f.ListSandboxes(ctx)
}

func (f *lifecycleFakeClient) CreateSandbox(_ context.Context, req createSandboxRequest) (sandboxSummary, error) {
	f.calls = append(f.calls, "create")
	f.creates = append(f.creates, req)
	id := strings.TrimSpace(req.Name)
	if id == "" || f.generateIDs {
		f.nextID++
		id = "cf-sandbox-" + string(rune('a'+f.nextID-1))
	}
	metadata := cloneMap(req.Metadata)
	if f.dropMetadata {
		metadata = nil
	}
	sb := sandboxSummary{ID: id, Name: req.Name, Status: "running", Metadata: metadata}
	f.sandboxes[id] = sb
	if f.omitCreateMetadata {
		sb.Metadata = nil
	}
	return sb, nil
}

func (f *lifecycleFakeClient) GetSandbox(_ context.Context, id string) (sandboxSummary, error) {
	f.calls = append(f.calls, "get:"+id)
	sb, ok := f.sandboxes[id]
	if !ok {
		return sandboxSummary{}, &cloudflareSandboxNotFoundError{err: errors.New("not found")}
	}
	return sb, nil
}

func (f *lifecycleFakeClient) DeleteSandbox(_ context.Context, id string) error {
	f.calls = append(f.calls, "delete:"+id)
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if _, ok := f.sandboxes[id]; !ok {
		return &cloudflareSandboxNotFoundError{err: errors.New("not found")}
	}
	delete(f.sandboxes, id)
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
	return execResult{ExitCode: f.exitCode, Stdout: f.stdout, Stderr: f.stderr}, nil
}

func (f *lifecycleFakeClient) UploadFile(_ context.Context, id, remotePath string, content io.Reader) error {
	f.calls = append(f.calls, "upload:"+id)
	_, _ = io.Copy(io.Discard, content)
	f.uploads = append(f.uploads, remotePath)
	return nil
}

func (f *lifecycleFakeClient) Persist(context.Context, string, persistRequest) (persistResponse, error) {
	return persistResponse{}, nil
}

func (f *lifecycleFakeClient) Hydrate(context.Context, string, hydrateRequest) error { return nil }

func (f *lifecycleFakeClient) WarmPool(context.Context) (warmPoolResponse, error) {
	return warmPoolResponse{Ready: 1, Total: 1}, nil
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
	if !strings.Contains(stdout.String(), "provider=cloudflare-sandbox") || strings.Contains(stdout.String(), "secret") {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestCreateMetadataRedactsRemoteURLCredentials(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newLifecycleFakeClient()
	backend := testBackend(fake, io.Discard, io.Discard)
	repo := Repo{
		Name:      "my-app",
		Root:      "/repo",
		RemoteURL: "https://oauth2:secret-token@github.com/openclaw/crabbox.git",
	}
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repo, Keep: true}); err != nil {
		t.Fatal(err)
	}
	if len(fake.creates) != 1 {
		t.Fatalf("creates=%#v", fake.creates)
	}
	repoMetadata := fake.creates[0].Metadata[metadataRepoKey]
	if repoMetadata != "https://github.com/openclaw/crabbox.git" {
		t.Fatalf("metadata repo=%q", repoMetadata)
	}
	if strings.Contains(repoMetadata, "secret-token") || strings.Contains(repoMetadata, "oauth2") {
		t.Fatalf("metadata leaked credentials: %q", repoMetadata)
	}
}

func TestDoctorFailsUnhealthyBridgeResponse(t *testing.T) {
	fake := newLifecycleFakeClient()
	fake.healthOK = false
	backend := testBackend(fake, io.Discard, io.Discard)
	result, err := backend.Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "failed" {
		t.Fatalf("doctor status=%q checks=%#v", result.Status, result.Checks)
	}
	if len(result.Checks) == 0 || result.Checks[0].Check != "health" || result.Checks[0].Status != "failed" {
		t.Fatalf("checks=%#v", result.Checks)
	}
}

func TestDoctorFailsWhenOpenAPIReadinessFails(t *testing.T) {
	fake := newLifecycleFakeClient()
	fake.openAPIErr = errors.New("401 Unauthorized")
	backend := testBackend(fake, io.Discard, io.Discard)
	result, err := backend.Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "failed" {
		t.Fatalf("doctor status=%q checks=%#v", result.Status, result.Checks)
	}
	if len(result.Checks) < 2 || result.Checks[1].Check != "openapi" || result.Checks[1].Status != "failed" {
		t.Fatalf("checks=%#v", result.Checks)
	}
}

func TestWarmupAcceptsGeneratedSandboxIDWithEchoedMetadata(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	fake := newLifecycleFakeClient()
	fake.generateIDs = true
	backend := testBackend(fake, &stdout, &stderr)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: Repo{Name: "my-app", Root: "/repo"}, Keep: true}); err != nil {
		t.Fatal(err)
	}
	if len(fake.creates) != 1 {
		t.Fatalf("creates=%#v", fake.creates)
	}
	leaseID := fake.creates[0].Metadata[metadataClaimKey]
	if leaseID == "" {
		t.Fatalf("request metadata=%#v", fake.creates[0].Metadata)
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.CloudID != "cf-sandbox-a" {
		t.Fatalf("claim CloudID=%q want generated sandbox id", claim.CloudID)
	}
	status, err := backend.Status(context.Background(), StatusRequest{ID: claim.Slug})
	if err != nil {
		t.Fatal(err)
	}
	if status.ID != leaseID || status.ServerID != claim.CloudID {
		t.Fatalf("status=%#v claim=%#v", status, claim)
	}
	views, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].CloudID != claim.CloudID || views[0].Labels["lease"] != leaseID {
		t.Fatalf("views=%#v claim=%#v", views, claim)
	}
}

func TestWarmupVerifiesOwnershipMetadataWhenCreateOmitsIt(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newLifecycleFakeClient()
	fake.omitCreateMetadata = true
	backend := testBackend(fake, io.Discard, io.Discard)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: Repo{Name: "my-app", Root: "/repo"}, Keep: true}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(fake.calls, "get:"+fake.creates[0].Name) {
		t.Fatalf("calls=%#v, want create metadata verification fetch", fake.calls)
	}
	leaseID := fake.creates[0].Metadata[metadataClaimKey]
	if _, err := readLeaseClaim(leaseID); err != nil {
		t.Fatalf("claim not created after verified metadata fetch: %v", err)
	}
}

func TestWarmupRejectsCreateWhenRemoteMetadataMissing(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newLifecycleFakeClient()
	fake.omitCreateMetadata = true
	fake.dropMetadata = true
	backend := testBackend(fake, io.Discard, io.Discard)
	err := backend.Warmup(context.Background(), WarmupRequest{Repo: Repo{Name: "my-app", Root: "/repo"}, Keep: true})
	if err == nil || !strings.Contains(err.Error(), "ownership metadata") {
		t.Fatalf("err=%v, want ownership metadata failure", err)
	}
	if len(fake.sandboxes) != 0 {
		t.Fatalf("sandbox not cleaned after metadata verification failure: %#v", fake.sandboxes)
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
		Repo:    Repo{Name: "my-app", Root: repo},
		Command: []string{"echo", "ok"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || !result.SyncDelegated || result.Provider != providerName {
		t.Fatalf("result=%#v", result)
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
	if _, err := readLeaseClaim(leaseID); err != nil {
		t.Fatalf("claim not retained: %v", err)
	}
	if len(fake.sandboxes) != 1 {
		t.Fatalf("retained sandbox deleted: %#v", fake.sandboxes)
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

func TestStopHonorsLeaseOperationLock(t *testing.T) {
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
	unlock, err := lockCloudflareSandboxLeaseOperation(context.Background(), leaseID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	err = backend.Stop(ctx, StopRequest{ID: leaseID})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop err=%v, want context deadline", err)
	}
	if len(fake.sandboxes) != 1 {
		t.Fatalf("stop deleted sandbox while lock was held: %#v", fake.sandboxes)
	}
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
	backend.cfg.CloudflareSandbox.ForgetMissing = true
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
			"PUBLIC_VALUE":                     "visible",
			"CRABBOX_CLOUDFLARE_SANDBOX_TOKEN": secretValue,
			"CLOUDFLARE_API_TOKEN":             "another-secret",
			"CLOUDFLARE_API_KEY":               "legacy-secret",
			"CLOUDFLARE_EMAIL":                 "user@example.test",
			"CF_API_KEY":                       "cf-legacy-secret",
			"CF_API_EMAIL":                     "cf-user@example.test",
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
	if _, ok := last.Env["CRABBOX_CLOUDFLARE_SANDBOX_TOKEN"]; ok {
		t.Fatalf("provider secret forwarded to command env: %#v", last.Env)
	}
	for _, name := range []string{"CLOUDFLARE_API_TOKEN", "CLOUDFLARE_API_KEY", "CLOUDFLARE_EMAIL", "CF_API_KEY", "CF_API_EMAIL"} {
		if _, ok := last.Env[name]; ok {
			t.Fatalf("provider credential %s forwarded to command env: %#v", name, last.Env)
		}
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
	if !sawExecContaining(fake.execs, ".crabbox-sync-") || !sawExecContaining(fake.execs, "mv") || !sawExecContaining(fake.execs, "/workspace/crabbox") {
		t.Fatalf("staging replace command missing: %#v", fake.execs)
	}
}

func TestRunKeepOnFailureRetainsClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newLifecycleFakeClient()
	fake.exitCode = 7
	backend := testBackend(fake, io.Discard, io.Discard)
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:          Repo{Name: "my-app", Root: tempRepo(t)},
		NoSync:        true,
		KeepOnFailure: true,
		Command:       []string{"false"},
	})
	if err == nil || result.ExitCode != 7 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if len(fake.sandboxes) != 1 {
		t.Fatalf("sandbox should be retained on failure: %#v", fake.sandboxes)
	}
	var leaseID string
	for id := range fake.sandboxes {
		leaseID = leasePrefix + id
	}
	if claim, err := readLeaseClaim(leaseID); err != nil || claim.LeaseID == "" {
		t.Fatalf("claim should be retained: claim=%#v err=%v", claim, err)
	}
}

func TestRunKeepOnFailureStillDeletesSuccessfulOneShot(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newLifecycleFakeClient()
	backend := testBackend(fake, io.Discard, io.Discard)
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:          Repo{Name: "my-app", Root: tempRepo(t)},
		NoSync:        true,
		KeepOnFailure: true,
		Command:       []string{"true"},
	})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if len(fake.sandboxes) != 0 {
		t.Fatalf("successful keep-on-failure run should delete sandbox: %#v", fake.sandboxes)
	}
}

func TestListReturnsOwnedSandboxesOnly(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := newLifecycleFakeClient()
	backend := testBackend(fake, io.Discard, io.Discard)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: Repo{Name: "my-app", Root: "/repo"}, Keep: true}); err != nil {
		t.Fatal(err)
	}
	fake.sandboxes["foreign"] = sandboxSummary{ID: "foreign", Status: "running"}
	views, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Labels["provider"] != providerName {
		t.Fatalf("views=%#v", views)
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
	cfg.CloudflareSandbox.BridgeURL = "https://bridge.example.test"
	cfg.CloudflareSandbox.Workdir = defaultWorkdir
	cfg.CloudflareSandbox.ExecTimeoutSecs = 600
	cfg.IdleTimeout = time.Hour
	return &backend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		rt: Runtime{
			Stdout: stdout,
			Stderr: stderr,
		},
		newClient: func(Config, Runtime) (bridgeClient, error) {
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
