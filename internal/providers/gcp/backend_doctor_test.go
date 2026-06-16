package gcp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type fakeGCPDoctorClient struct {
	listCalls int
	deleted   []string
	mutated   bool
	servers   []Server
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

func (c *fakeGCPDoctorClient) GetServer(context.Context, string) (Server, error) {
	return Server{}, nil
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
	if err := core.ClaimLeaseForRepoProviderScope(expiredLeaseID, "expired-box", "gcp", "project:project-a", repo, time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := core.ClaimLeaseForRepoProviderScope(staleLeaseID, "stale-box", "gcp", "project:project-a", repo, time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := core.ClaimLeaseForRepoProviderScope(otherProjectLeaseID, "other-box", "gcp", "project:project-b", repo, time.Minute, false); err != nil {
		t.Fatal(err)
	}
	fake := &fakeGCPDoctorClient{servers: []Server{
		{
			CloudID: core.LeaseProviderName(expiredLeaseID, "expired-box"),
			Name:    core.LeaseProviderName(expiredLeaseID, "expired-box"),
			Labels: map[string]string{
				"crabbox": "true", "created_by": "crabbox", "provider": "gcp",
				"lease": expiredLeaseID, "slug": "expired-box",
				"state": "ready", "expires_at": core.LeaseLabelTime(time.Now().Add(-time.Hour)),
			},
		},
		{
			CloudID: "crabbox-forged",
			Name:    "crabbox-forged",
			Labels: map[string]string{
				"crabbox": "true", "created_by": "crabbox", "provider": "gcp",
				"lease": "cbx_444444444444", "slug": "forged",
				"state": "ready", "expires_at": core.LeaseLabelTime(time.Now().Add(-time.Hour)),
			},
		},
	}}
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
