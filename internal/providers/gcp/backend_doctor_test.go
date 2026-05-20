package gcp

import (
	"bytes"
	"context"
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
	return Server{}, Config{}, nil
}

func (c *fakeGCPDoctorClient) WaitForServerIP(context.Context, string) (Server, error) {
	return Server{}, nil
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

func TestGCPCleanupRemovesDeletedAndStaleClaims(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := t.TempDir()
	if err := core.ClaimLeaseForRepoProviderScope("cbx_expired", "expired-box", "gcp", "project:project-a", repo, time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := core.ClaimLeaseForRepoProviderScope("cbx_stale", "stale-box", "gcp", "project:project-a", repo, time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := core.ClaimLeaseForRepoProviderScope("cbx_other_project", "other-box", "gcp", "project:project-b", repo, time.Minute, false); err != nil {
		t.Fatal(err)
	}
	fake := &fakeGCPDoctorClient{servers: []Server{{
		CloudID: "crabbox-expired",
		Name:    "crabbox-expired",
		Labels: map[string]string{
			"lease":      "cbx_expired",
			"state":      "ready",
			"expires_at": core.LeaseLabelTime(time.Now().Add(-time.Hour)),
		},
	}}}
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
	if len(fake.deleted) != 1 {
		t.Fatalf("deleted=%v want one deleted server", fake.deleted)
	}
	for _, leaseID := range []string{"cbx_expired", "cbx_stale"} {
		claim, err := core.ReadLeaseClaim(leaseID)
		if err != nil {
			t.Fatal(err)
		}
		if claim.LeaseID != "" {
			t.Fatalf("claim %s still present: %#v", leaseID, claim)
		}
	}
	claim, err := core.ReadLeaseClaim("cbx_other_project")
	if err != nil {
		t.Fatal(err)
	}
	if claim.LeaseID == "" {
		t.Fatal("other project claim was removed")
	}
	out := stderr.String()
	if !strings.Contains(out, "delete server id=crabbox-expired") || !strings.Contains(out, "remove stale claim lease=cbx_stale") {
		t.Fatalf("cleanup output=%q", out)
	}
}

func TestGCPDoctorListsInventoryOnly(t *testing.T) {
	fake := &fakeGCPDoctorClient{servers: []Server{{CloudID: "crabbox-one"}, {CloudID: "crabbox-two"}}}
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
