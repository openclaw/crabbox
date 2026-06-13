package superserve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
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
	created     []createSandboxRequest
	updates     []map[string]string
	listFilters []map[string]string
	deleted     []string
	probes      int
	getErr      error
	deleteErr   error
	updateErr   error
}

func newFakeSuperserveClient() *fakeSuperserveClient {
	return &fakeSuperserveClient{
		baseURL: "https://api.superserve.test",
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
	return sandboxAccess{Sandbox: cloneSandbox(f.sandbox), AccessToken: "ss_test_token"}, nil
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
	if len(fake.updates) != 1 || fake.updates[0][metadataClaimKey] != leaseID || fake.updates[0][metadataSlugKey] == "" {
		t.Fatalf("update metadata=%#v", fake.updates)
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

func TestRunRemainsDeferredToPlan03(t *testing.T) {
	backend := newSuperserveTestBackend(t, newFakeSuperserveClient())
	_, err := backend.Run(context.Background(), RunRequest{})
	if err == nil || !strings.Contains(err.Error(), "run is not implemented yet") {
		t.Fatalf("Run err=%v", err)
	}
	if (Provider{}).Spec().Features.Has(core.FeaturePauseResume) {
		t.Fatal("superserve must not advertise pause/resume until backend methods are implemented")
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
