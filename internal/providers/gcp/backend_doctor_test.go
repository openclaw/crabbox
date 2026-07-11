package gcp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"google.golang.org/api/googleapi"
)

type fakeGCPDoctorClient struct {
	listCalls int
	deleted   []string
	mutated   bool
	servers   []Server
	complete  []Server
	get       map[string]Server
	getErr    error
	created   Server
	createCfg Config
	createErr error
	waitErr   error
}

func (c *fakeGCPDoctorClient) ListCrabboxServers(context.Context) ([]Server, error) {
	c.listCalls++
	return c.servers, nil
}

func (c *fakeGCPDoctorClient) ListCrabboxServersComplete(context.Context) ([]Server, error) {
	c.listCalls++
	if c.complete != nil {
		return c.complete, nil
	}
	return c.servers, nil
}

func (c *fakeGCPDoctorClient) CreateServerWithFallback(context.Context, Config, string, string, string, bool, func(string, ...any)) (Server, Config, error) {
	c.mutated = true
	if c.createErr != nil {
		return Server{}, Config{}, c.createErr
	}
	if c.created.CloudID == "" {
		c.created = Server{CloudID: "crabbox-created", Name: "crabbox-created", Labels: map[string]string{}}
	}
	return c.created, c.createCfg, nil
}

func (c *fakeGCPDoctorClient) WaitForServerIP(context.Context, string) (Server, error) {
	if c.waitErr != nil {
		return Server{}, c.waitErr
	}
	return c.created, nil
}

func (c *fakeGCPDoctorClient) GetServer(_ context.Context, name string) (Server, error) {
	if c.getErr != nil {
		return Server{}, c.getErr
	}
	if c.get != nil {
		if server, ok := c.get[name]; ok {
			return server, nil
		}
	}
	return Server{}, errors.New("gcp server not found: " + name)
}

func (c *fakeGCPDoctorClient) DeleteServer(_ context.Context, name string) error {
	c.deleted = append(c.deleted, name)
	c.mutated = true
	return nil
}

func (c *fakeGCPDoctorClient) SetLabels(context.Context, string, map[string]string) error {
	c.mutated = true
	return nil
}

func canonicalGCPTestServer(leaseID, slug string) Server {
	name := core.LeaseProviderName(leaseID, slug)
	return Server{
		Provider: "gcp",
		CloudID:  name,
		ID:       42,
		Name:     name,
		Labels: map[string]string{
			"crabbox":      "true",
			"created_by":   "crabbox",
			"provider":     "gcp",
			"provider_key": "crabbox-test",
			"lease":        leaseID,
			"slug":         slug,
			"zone":         "us-central1-b",
		},
	}
}

func claimGCPTestServer(t *testing.T, cfg Config, server Server) {
	t.Helper()
	if err := core.ClaimLeaseTargetForConfig(server.Labels["lease"], server.Labels["slug"], cfg, server, core.SSHTarget{}, time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestValidateExactGCPClaimBindsProviderResourceAndLease(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := Config{Provider: "gcp", GCPProject: "project-a", GCPZone: "us-central1-b"}
	server := canonicalGCPTestServer("cbx_111111111111", "owned")
	claimGCPTestServer(t, cfg, server)
	claim, err := core.ReadLeaseClaim(server.Labels["lease"])
	if err != nil {
		t.Fatal(err)
	}
	if err := validateExactGCPClaim(claim, server, server.Labels["lease"], cfg); err != nil {
		t.Fatalf("valid exact claim rejected: %v", err)
	}

	tests := map[string]func(*core.LeaseClaim, *Server, *Config){
		"project scope": func(_ *core.LeaseClaim, _ *Server, cfg *Config) { cfg.GCPProject = "project-b" },
		"cloud name":    func(_ *core.LeaseClaim, server *Server, _ *Config) { server.CloudID += "-other" },
		"numeric id":    func(_ *core.LeaseClaim, server *Server, _ *Config) { server.ID++ },
		"slug": func(_ *core.LeaseClaim, server *Server, _ *Config) {
			server.Labels["slug"] = "other"
			server.Name = core.LeaseProviderName(server.Labels["lease"], "other")
		},
		"zone":         func(_ *core.LeaseClaim, server *Server, _ *Config) { server.Labels["zone"] = "us-central1-c" },
		"provider key": func(_ *core.LeaseClaim, server *Server, _ *Config) { server.Labels["provider_key"] = "crabbox-other" },
		"claim labels": func(claim *core.LeaseClaim, _ *Server, _ *Config) { delete(claim.Labels, "zone") },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			changedClaim := claim
			changedClaim.Labels = maps.Clone(claim.Labels)
			changedServer := server
			changedServer.Labels = maps.Clone(server.Labels)
			changedCfg := cfg
			mutate(&changedClaim, &changedServer, &changedCfg)
			if err := validateExactGCPClaim(changedClaim, changedServer, server.Labels["lease"], changedCfg); err == nil {
				t.Fatal("mismatched claim was accepted")
			}
		})
	}
}

func TestGCPReleaseLeaseRejectsMissingExactClaimBeforeDelete(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	server := canonicalGCPTestServer("cbx_222222222222", "claimless")
	fake := &fakeGCPDoctorClient{get: map[string]Server{server.CloudID: server}}
	old := newGCPClient
	newGCPClient = func(context.Context, Config) (gcpClient, error) { return fake, nil }
	t.Cleanup(func() { newGCPClient = old })

	backend := NewGCPLeaseBackend(core.ProviderSpec{}, Config{Provider: "gcp", GCPProject: "project-a", GCPZone: "us-central1-b"}, Runtime{Stderr: io.Discard}).(*gcpLeaseBackend)
	err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: server.Labels["lease"], Server: server}})
	if err == nil || !strings.Contains(err.Error(), "no exact local claim") {
		t.Fatalf("ReleaseLease() error=%v, want missing-claim refusal", err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("deleted=%v, want no destructive call", fake.deleted)
	}
}

func TestValidateGCPCleanupLiveServerRejectsReplacementInstance(t *testing.T) {
	expected := canonicalGCPTestServer("cbx_111111111111", "stale")
	live := expected
	live.ID++
	if err := validateGCPCleanupLiveServer(expected, live); err == nil || !strings.Contains(err.Error(), "instance id") {
		t.Fatalf("replacement instance error=%v", err)
	}
}

func TestGCPAcquireCleansUpCreatedServerOnIPFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ipErr := errors.New("ip unavailable")
	fake := &fakeGCPDoctorClient{
		created:   Server{CloudID: "crabbox-created", Name: "crabbox-created", Labels: map[string]string{"lease": "cbx_created"}},
		createCfg: Config{Provider: "gcp", GCPProject: "project-a", GCPZone: "us-central1-b"},
		waitErr:   ipErr,
	}
	old := newGCPClient
	newGCPClient = func(context.Context, Config) (gcpClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newGCPClient = old })

	backend := NewGCPLeaseBackend(core.ProviderSpec{}, Config{Provider: "gcp", GCPProject: "project-a"}, Runtime{Stderr: io.Discard}).(*gcpLeaseBackend)
	_, err := backend.acquireOnce(context.Background(), false, "")
	if !errors.Is(err, ipErr) {
		t.Fatalf("err=%v, want IP failure", err)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != "crabbox-created" {
		t.Fatalf("deleted=%v, want created server cleanup", fake.deleted)
	}
}

func TestGCPAcquireCleansUpCreatedServerOnFallbackClientFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rebuildErr := errors.New("fallback auth failed")
	fake := &fakeGCPDoctorClient{
		created:   Server{CloudID: "crabbox-fallback", Name: "crabbox-fallback", Labels: map[string]string{"lease": "cbx_created"}},
		createCfg: Config{Provider: "gcp", GCPProject: "project-a", GCPZone: "us-central1-c"},
	}
	calls := 0
	old := newGCPClient
	newGCPClient = func(context.Context, Config) (gcpClient, error) {
		calls++
		if calls >= 2 {
			return nil, rebuildErr
		}
		return fake, nil
	}
	t.Cleanup(func() { newGCPClient = old })

	backend := NewGCPLeaseBackend(core.ProviderSpec{}, Config{Provider: "gcp", GCPProject: "project-a"}, Runtime{Stderr: io.Discard}).(*gcpLeaseBackend)
	_, err := backend.acquireOnce(context.Background(), false, "")
	if !errors.Is(err, rebuildErr) {
		t.Fatalf("err=%v, want fallback client failure", err)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != "crabbox-fallback" {
		t.Fatalf("deleted=%v, want created server cleanup", fake.deleted)
	}
	if calls < 3 {
		t.Fatalf("newGCPClient calls=%d, want cleanup client rebuild attempt", calls)
	}
}

func TestGCPCleanupRemovesDeletedAndStaleClaims(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := t.TempDir()
	expiredLeaseID := "cbx_111111111111"
	staleLeaseID := "cbx_222222222222"
	otherProjectLeaseID := "cbx_333333333333"
	if err := core.ClaimLeaseForRepoProviderScope(staleLeaseID, "stale-box", "gcp", "project:project-a", repo, time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := core.ClaimLeaseForRepoProviderScope(otherProjectLeaseID, "other-box", "gcp", "project:project-b", repo, time.Minute, false); err != nil {
		t.Fatal(err)
	}
	expired := Server{
		CloudID: core.LeaseProviderName(expiredLeaseID, "expired-box"),
		Name:    core.LeaseProviderName(expiredLeaseID, "expired-box"),
		ID:      42,
		Labels: map[string]string{
			"crabbox": "true", "created_by": "crabbox", "provider": "gcp",
			"provider_key": "crabbox-test", "lease": expiredLeaseID, "slug": "expired-box", "zone": "us-central1-b",
			"state": "ready", "expires_at": core.LeaseLabelTime(time.Now().Add(-time.Hour)),
		},
	}
	claimGCPTestServer(t, Config{Provider: "gcp", GCPProject: "project-a", GCPZone: "us-central1-b"}, expired)
	fake := &fakeGCPDoctorClient{
		servers: []Server{expired,
			{
				CloudID: "crabbox-forged",
				Name:    "crabbox-forged",
				ID:      43,
				Labels: map[string]string{
					"crabbox": "true", "created_by": "crabbox", "provider": "gcp",
					"lease": "cbx_444444444444", "slug": "forged",
					"state": "ready", "expires_at": core.LeaseLabelTime(time.Now().Add(-time.Hour)),
				},
			}},
		get: map[string]Server{expired.CloudID: expired},
	}
	old := newGCPClient
	newGCPClient = func(context.Context, Config) (gcpClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newGCPClient = old })

	var stderr bytes.Buffer
	backend := NewGCPLeaseBackend(core.ProviderSpec{}, Config{Provider: "gcp", GCPProject: "project-a"}, Runtime{Stderr: &stderr})
	cleaner, ok := backend.(core.CleanupBackend)
	if !ok {
		t.Fatal("gcp backend missing cleanup")
	}
	if err := cleaner.Cleanup(context.Background(), core.CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("dry-run deleted=%v, want no mutation", fake.deleted)
	}
	if claim, err := core.ReadLeaseClaim(expiredLeaseID); err != nil || claim.LeaseID == "" {
		t.Fatalf("dry-run exact claim=%+v err=%v, want retained claim", claim, err)
	}
	stderr.Reset()
	if err := cleaner.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != core.LeaseProviderName(expiredLeaseID, "expired-box") {
		t.Fatalf("deleted=%v want canonical expired server", fake.deleted)
	}
	for _, leaseID := range []string{expiredLeaseID, staleLeaseID} {
		claim, err := core.ReadLeaseClaim(leaseID)
		if err != nil {
			t.Fatal(err)
		}
		if claim.LeaseID != "" {
			t.Fatalf("claim %s still present: %#v", leaseID, claim)
		}
	}
	claim, err := core.ReadLeaseClaim(otherProjectLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.LeaseID == "" {
		t.Fatal("other project claim was removed")
	}
	out := stderr.String()
	if !strings.Contains(out, "delete server id="+core.LeaseProviderName(expiredLeaseID, "expired-box")) ||
		!strings.Contains(out, "remove stale claim lease="+staleLeaseID) {
		t.Fatalf("cleanup output=%q", out)
	}
}

func TestGCPCleanupRevalidatesLiveOwnershipBeforeDelete(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	snapshot := canonicalGCPTestServer("cbx_111111111111", "stale")
	snapshot.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	cfg := Config{Provider: "gcp", GCPProject: "project-a", GCPZone: snapshot.Labels["zone"]}
	claimGCPTestServer(t, cfg, snapshot)
	live := snapshot
	live.Labels = maps.Clone(snapshot.Labels)
	delete(live.Labels, "created_by")
	fake := &fakeGCPDoctorClient{
		servers:  []Server{snapshot},
		complete: []Server{live},
		get:      map[string]Server{snapshot.CloudID: live},
	}
	old := newGCPClient
	newGCPClient = func(context.Context, Config) (gcpClient, error) { return fake, nil }
	t.Cleanup(func() { newGCPClient = old })

	var stderr bytes.Buffer
	backend := NewGCPLeaseBackend(core.ProviderSpec{}, cfg, Runtime{Stderr: &stderr}).(*gcpLeaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("cleanup crossed changed ownership: deleted=%v", fake.deleted)
	}
	if !strings.Contains(stderr.String(), "live instance no longer has canonical Crabbox ownership labels") {
		t.Fatalf("stderr=%q, want changed-ownership skip", stderr.String())
	}
}

func TestGCPCleanupSkipsClaimlessAndStaleExactClaims(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := Config{Provider: "gcp", GCPProject: "project-a", GCPZone: "us-central1-b"}
	claimless := canonicalGCPTestServer("cbx_333333333333", "claimless")
	claimless.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	stale := canonicalGCPTestServer("cbx_444444444444", "stale")
	stale.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	claimGCPTestServer(t, cfg, stale)
	stale.ID++
	fake := &fakeGCPDoctorClient{
		servers: []Server{claimless, stale},
		get:     map[string]Server{claimless.CloudID: claimless, stale.CloudID: stale},
	}
	old := newGCPClient
	newGCPClient = func(context.Context, Config) (gcpClient, error) { return fake, nil }
	t.Cleanup(func() { newGCPClient = old })

	var stderr bytes.Buffer
	backend := NewGCPLeaseBackend(core.ProviderSpec{}, cfg, Runtime{Stderr: &stderr}).(*gcpLeaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("cleanup deleted without exact authority: %v", fake.deleted)
	}
	if got := strings.Count(stderr.String(), "reason=exact local claim missing or stale"); got != 2 {
		t.Fatalf("stderr=%q, missing/stale skips=%d want 2", stderr.String(), got)
	}
}

func TestGCPCleanupRetainsClaimWhenOwnershipLabelsDrift(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	snapshot := canonicalGCPTestServer("cbx_555555555555", "drifted")
	snapshot.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	live := snapshot
	live.Labels = maps.Clone(snapshot.Labels)
	live.Labels["created_by"] = "external"
	cfg := Config{Provider: "gcp", GCPProject: "project-a", GCPZone: snapshot.Labels["zone"]}
	if err := core.ClaimLeaseTargetForConfig(snapshot.Labels["lease"], snapshot.Labels["slug"], cfg, snapshot, core.SSHTarget{}, time.Hour); err != nil {
		t.Fatal(err)
	}
	fake := &fakeGCPDoctorClient{
		servers:  []Server{snapshot},
		complete: []Server{live},
		get:      map[string]Server{snapshot.CloudID: live},
	}
	old := newGCPClient
	newGCPClient = func(context.Context, Config) (gcpClient, error) { return fake, nil }
	t.Cleanup(func() { newGCPClient = old })

	var stderr bytes.Buffer
	backend := NewGCPLeaseBackend(core.ProviderSpec{}, cfg, Runtime{Stderr: &stderr}).(*gcpLeaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := core.ResolveLeaseClaim(snapshot.Labels["lease"])
	if err != nil || !ok || claim.CloudID != snapshot.CloudID {
		t.Fatalf("claim=%+v ok=%v err=%v, want retained exact claim", claim, ok, err)
	}
	if !strings.Contains(stderr.String(), "cloud resource still exists") {
		t.Fatalf("stderr=%q, want retained-claim diagnostic", stderr.String())
	}
}

func TestGCPCleanupRevalidatesLiveEligibilityBeforeDelete(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	snapshot := canonicalGCPTestServer("cbx_111111111111", "renewed")
	snapshot.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	cfg := Config{Provider: "gcp", GCPProject: "project-a", GCPZone: snapshot.Labels["zone"]}
	claimGCPTestServer(t, cfg, snapshot)
	live := snapshot
	live.Labels = maps.Clone(snapshot.Labels)
	live.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(time.Hour))
	fake := &fakeGCPDoctorClient{servers: []Server{snapshot}, get: map[string]Server{snapshot.CloudID: live}}
	old := newGCPClient
	newGCPClient = func(context.Context, Config) (gcpClient, error) { return fake, nil }
	t.Cleanup(func() { newGCPClient = old })

	var stderr bytes.Buffer
	backend := NewGCPLeaseBackend(core.ProviderSpec{}, cfg, Runtime{Stderr: &stderr}).(*gcpLeaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("cleanup deleted renewed instance: %v", fake.deleted)
	}
	if !strings.Contains(stderr.String(), "reason=live instance") {
		t.Fatalf("stderr=%q, want renewed-live skip", stderr.String())
	}
}

func TestGCPCleanupTreatsMissingLiveInstanceAsAlreadyDeleted(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_111111111111"
	slug := "gone"
	snapshot := canonicalGCPTestServer(leaseID, slug)
	snapshot.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	cfg := Config{Provider: "gcp", GCPProject: "project-a", GCPZone: snapshot.Labels["zone"]}
	claimGCPTestServer(t, cfg, snapshot)
	fake := &fakeGCPDoctorClient{
		servers: []Server{snapshot},
		getErr:  &googleapi.Error{Code: http.StatusNotFound, Message: "instance gone"},
	}
	old := newGCPClient
	newGCPClient = func(context.Context, Config) (gcpClient, error) { return fake, nil }
	t.Cleanup(func() { newGCPClient = old })

	var stderr bytes.Buffer
	backend := NewGCPLeaseBackend(core.ProviderSpec{}, cfg, Runtime{Stderr: &stderr}).(*gcpLeaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("cleanup retried delete for missing instance: %v", fake.deleted)
	}
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.LeaseID != "" {
		t.Fatalf("missing-instance claim was not removed: %#v", claim)
	}
	if !strings.Contains(stderr.String(), "live instance no longer exists") {
		t.Fatalf("stderr=%q, want already-deleted diagnostic", stderr.String())
	}
}

func TestGCPReleaseLeaseRequiresCanonicalLiveOwnership(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_777777777777"
	slug := "release-box"
	live := canonicalGCPTestServer(leaseID, slug)
	claimGCPTestServer(t, Config{Provider: "gcp", GCPProject: "project-a", GCPZone: "us-central1-b"}, live)
	fake := &fakeGCPDoctorClient{get: map[string]Server{live.CloudID: live}}
	old := newGCPClient
	newGCPClient = func(context.Context, Config) (gcpClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newGCPClient = old })

	backend := NewGCPLeaseBackend(core.ProviderSpec{}, Config{Provider: "gcp", GCPProject: "project-a", GCPZone: "us-central1-b"}, Runtime{Stderr: io.Discard}).(*gcpLeaseBackend)
	err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{
		Lease: LeaseTarget{
			LeaseID: leaseID,
			Server:  live,
		},
	})
	if err != nil {
		t.Fatalf("ReleaseLease() error=%v", err)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != live.CloudID {
		t.Fatalf("deleted=%v want %s", fake.deleted, live.CloudID)
	}
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.LeaseID != "" {
		t.Fatalf("claim still present after verified release: %#v", claim)
	}
}

func TestGCPReleaseLeaseRefusesWrongLiveLease(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_888888888888"
	claimed := canonicalGCPTestServer(leaseID, "release-box")
	claimGCPTestServer(t, Config{Provider: "gcp", GCPProject: "project-a", GCPZone: "us-central1-b"}, claimed)
	live := claimed
	live.Labels = maps.Clone(claimed.Labels)
	live.Labels["lease"] = "cbx_999999999999"
	live.Labels["slug"] = "other-box"
	live.Name = core.LeaseProviderName(live.Labels["lease"], live.Labels["slug"])
	fake := &fakeGCPDoctorClient{get: map[string]Server{claimed.CloudID: live}}
	old := newGCPClient
	newGCPClient = func(context.Context, Config) (gcpClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newGCPClient = old })

	backend := NewGCPLeaseBackend(core.ProviderSpec{}, Config{Provider: "gcp", GCPProject: "project-a", GCPZone: "us-central1-b"}, Runtime{Stderr: io.Discard}).(*gcpLeaseBackend)
	err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{
		Lease: LeaseTarget{
			LeaseID: leaseID,
			Server:  claimed,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "live instance belongs to lease=cbx_999999999999") {
		t.Fatalf("ReleaseLease() error=%v, want wrong-lease refusal", err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("deleted=%v want no delete", fake.deleted)
	}
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.LeaseID == "" {
		t.Fatal("claim was removed after refused release")
	}
}

func TestGCPReleaseLeaseRefusesNonCanonicalLiveInstance(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_aaaaaaaaaaaa"
	claimed := canonicalGCPTestServer(leaseID, "release-box")
	cloudID := claimed.CloudID
	claimGCPTestServer(t, Config{Provider: "gcp", GCPProject: "project-a", GCPZone: "us-central1-b"}, claimed)
	live := claimed
	live.Labels = maps.Clone(claimed.Labels)
	delete(live.Labels, "created_by")
	fake := &fakeGCPDoctorClient{get: map[string]Server{cloudID: live}}
	old := newGCPClient
	newGCPClient = func(context.Context, Config) (gcpClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newGCPClient = old })

	backend := NewGCPLeaseBackend(core.ProviderSpec{}, Config{Provider: "gcp", GCPProject: "project-a", GCPZone: "us-central1-b"}, Runtime{Stderr: io.Discard}).(*gcpLeaseBackend)
	err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{
		Lease: LeaseTarget{
			LeaseID: leaseID,
			Server:  claimed,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "not canonical Crabbox-owned") {
		t.Fatalf("ReleaseLease() error=%v, want noncanonical refusal", err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("deleted=%v want no delete", fake.deleted)
	}
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.LeaseID == "" {
		t.Fatal("claim was removed after refused release")
	}
}

func TestGCPDoctorListsInventoryOnly(t *testing.T) {
	server := func(leaseID, slug string) Server {
		name := core.LeaseProviderName(leaseID, slug)
		return Server{
			CloudID: name,
			Name:    name,
			Labels: map[string]string{
				"crabbox": "true", "created_by": "crabbox", "provider": "gcp",
				"lease": leaseID, "slug": slug,
			},
		}
	}
	fake := &fakeGCPDoctorClient{servers: []Server{
		server("cbx_555555555555", "one"),
		server("cbx_666666666666", "two"),
		{CloudID: "crabbox-forged", Name: "crabbox-forged", Labels: map[string]string{"crabbox": "true"}},
	}}
	old := newGCPClient
	newGCPClient = func(context.Context, Config) (gcpClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newGCPClient = old })

	doctor, err := Provider{}.ConfigureDoctor(Config{}, Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := doctor.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider != "gcp" || !strings.Contains(result.Message, "inventory=ready api=list mutation=false leases=2 runtime=unchecked") || !strings.Contains(result.Message, "zone=aggregated") {
		t.Fatalf("result=%#v", result)
	}
	if fake.listCalls != 1 {
		t.Fatalf("list calls=%d, want 1", fake.listCalls)
	}
	if fake.mutated {
		t.Fatal("doctor called a mutating GCP method")
	}
}
