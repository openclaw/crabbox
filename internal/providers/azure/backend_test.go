package azure

import (
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type fakeAzureClient struct {
	deleted         []string
	cleanupExpected []Server
	tagged          []string
	servers         []Server
	listErr         error
	created         Server
	createCfg       Config
	createErr       error
	waitErr         error
	getErr          error
	get             map[string]Server
	getErrs         map[string]error
	getIDs          []string
}

func (c *fakeAzureClient) ListCrabboxServers(context.Context) ([]Server, error) {
	if c.listErr != nil {
		return nil, c.listErr
	}
	return c.servers, nil
}

func (c *fakeAzureClient) CreateServerWithFallback(context.Context, Config, string, string, string, bool, func(string, ...any)) (Server, Config, error) {
	if c.createErr != nil {
		return Server{}, Config{}, c.createErr
	}
	if c.created.CloudID == "" {
		c.created = Server{CloudID: "crabbox-created", Name: "crabbox-created", Labels: map[string]string{}}
	}
	return c.created, c.createCfg, nil
}

func (c *fakeAzureClient) WaitForServerIP(context.Context, string) (Server, error) {
	if c.waitErr != nil {
		return Server{}, c.waitErr
	}
	return c.created, nil
}

func (c *fakeAzureClient) GetServer(_ context.Context, id string) (Server, error) {
	c.getIDs = append(c.getIDs, id)
	if err := c.getErrs[id]; err != nil {
		return Server{}, err
	}
	if c.getErr != nil {
		return Server{}, c.getErr
	}
	if c.get != nil {
		if server, ok := c.get[id]; ok {
			return server, nil
		}
	}
	for _, server := range c.servers {
		if server.CloudID == id || server.Name == id {
			return server, nil
		}
	}
	return Server{}, core.Exit(4, "azure vm not found: %s", id)
}

func (c *fakeAzureClient) DeleteServer(_ context.Context, name string) error {
	c.deleted = append(c.deleted, name)
	return nil
}

func (c *fakeAzureClient) DeleteCleanupServer(_ context.Context, server Server, _ time.Time) error {
	c.cleanupExpected = append(c.cleanupExpected, server)
	c.deleted = append(c.deleted, server.CloudID)
	return nil
}

func (c *fakeAzureClient) SetTags(_ context.Context, name string, _ map[string]string) error {
	c.tagged = append(c.tagged, name)
	return nil
}

func TestAzureAcquireCleansUpCreatedServerOnIPFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ipErr := errors.New("ip unavailable")
	fake := &fakeAzureClient{
		created:   Server{CloudID: "crabbox-created", Name: "crabbox-created", Labels: map[string]string{"lease": "cbx_created"}},
		createCfg: azureAcquireTestConfig(),
		waitErr:   ipErr,
	}
	oldClient := newAzureClient
	newAzureClient = func(context.Context, Config) (azureClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newAzureClient = oldClient })

	backend := NewAzureLeaseBackend(ProviderSpec{}, azureAcquireTestConfig(), Runtime{Stderr: io.Discard}).(*azureLeaseBackend)
	_, err := backend.acquireOnce(context.Background(), false, "")
	if !errors.Is(err, ipErr) {
		t.Fatalf("err=%v, want IP failure", err)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != "crabbox-created" {
		t.Fatalf("deleted=%v, want created server cleanup", fake.deleted)
	}
}

func TestAzureAcquireValidatesSSHCIDRsBeforeClient(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	listErr := errors.New("stop before provision")
	fake := &fakeAzureClient{listErr: listErr}
	oldValidate := validateAzureSSHCIDRsForAcquire
	validated := false
	validateAzureSSHCIDRsForAcquire = func(_ context.Context, cfg Config) error {
		validated = true
		if len(cfg.AzureSSHCIDRs) != 0 {
			t.Fatalf("AzureSSHCIDRs=%v before validation, want non-explicit empty config", cfg.AzureSSHCIDRs)
		}
		return nil
	}
	t.Cleanup(func() { validateAzureSSHCIDRsForAcquire = oldValidate })
	var clientCfg Config
	oldClient := newAzureClient
	newAzureClient = func(_ context.Context, cfg Config) (azureClient, error) {
		if !validated {
			t.Fatal("newAzureClient ran before SSH CIDR validation")
		}
		clientCfg = cfg
		return fake, nil
	}
	t.Cleanup(func() { newAzureClient = oldClient })

	backend := NewAzureLeaseBackend(ProviderSpec{}, Config{Provider: "azure", AzureLocation: "eastus", AzureResourceGroup: "rg"}, Runtime{Stderr: io.Discard}).(*azureLeaseBackend)
	_, err := backend.acquireOnce(context.Background(), false, "")
	if !errors.Is(err, listErr) {
		t.Fatalf("err=%v, want list failure", err)
	}
	if len(clientCfg.AzureSSHCIDRs) != 0 {
		t.Fatalf("AzureSSHCIDRs=%v, want detected CIDR provenance preserved as non-explicit", clientCfg.AzureSSHCIDRs)
	}
}

func TestAzureAcquireFailsClosedWhenSSHCIDRDetectionFails(t *testing.T) {
	oldValidate := validateAzureSSHCIDRsForAcquire
	validateAzureSSHCIDRsForAcquire = func(context.Context, Config) error {
		return errors.New("offline")
	}
	t.Cleanup(func() { validateAzureSSHCIDRsForAcquire = oldValidate })
	oldClient := newAzureClient
	newAzureClient = func(context.Context, Config) (azureClient, error) {
		t.Fatal("newAzureClient should not run when SSH CIDR detection fails")
		return nil, nil
	}
	t.Cleanup(func() { newAzureClient = oldClient })

	backend := NewAzureLeaseBackend(ProviderSpec{}, Config{Provider: "azure", AzureLocation: "eastus", AzureResourceGroup: "rg"}, Runtime{Stderr: io.Discard}).(*azureLeaseBackend)
	_, err := backend.acquireOnce(context.Background(), false, "")
	if err == nil || err.Error() != "offline" {
		t.Fatalf("err=%v, want detection failure", err)
	}
}

func TestAzureAcquireDoesNotRollbackReadyServer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	created := Server{
		CloudID: "crabbox-ready",
		Name:    "crabbox-ready",
		Labels:  map[string]string{"lease": "cbx_ready"},
	}
	created.PublicNet.IPv4.IP = "203.0.113.10"
	fake := &fakeAzureClient{
		created:   created,
		createCfg: azureAcquireTestConfig(),
	}
	oldClient := newAzureClient
	newAzureClient = func(context.Context, Config) (azureClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newAzureClient = oldClient })
	oldBootstrap := bootstrapManagedWindowsDesktop
	bootstrapManagedWindowsDesktop = func(context.Context, Config, *SSHTarget, string, io.Writer) error {
		return nil
	}
	t.Cleanup(func() { bootstrapManagedWindowsDesktop = oldBootstrap })

	backend := NewAzureLeaseBackend(ProviderSpec{}, azureAcquireTestConfig(), Runtime{Stderr: io.Discard}).(*azureLeaseBackend)
	lease, err := backend.acquireOnce(context.Background(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.CloudID != "crabbox-ready" {
		t.Fatalf("server=%s, want crabbox-ready", lease.Server.CloudID)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("deleted=%v, want no rollback on success", fake.deleted)
	}
	if len(fake.tagged) != 1 || fake.tagged[0] != "crabbox-ready" {
		t.Fatalf("tagged=%v, want ready tag update", fake.tagged)
	}
}

func azureAcquireTestConfig() Config {
	return Config{
		Provider:           "azure",
		AzureLocation:      "eastus",
		AzureResourceGroup: "rg",
		AzureSSHCIDRs:      []string{"198.51.100.7/32"},
	}
}

func TestAzureResolveRawVMRejectsWeakTags(t *testing.T) {
	weak := azureTestServer("crabbox-weak", "cbx_123456abcdef", "weak")
	delete(weak.Labels, "created_by")
	fake := &fakeAzureClient{servers: []Server{weak}}
	oldClient := newAzureClient
	newAzureClient = func(context.Context, Config) (azureClient, error) { return fake, nil }
	t.Cleanup(func() { newAzureClient = oldClient })

	backend := NewAzureLeaseBackend(ProviderSpec{}, azureAcquireTestConfig(), Runtime{Stderr: io.Discard}).(*azureLeaseBackend)
	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: weak.Name, ReleaseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "not Crabbox-managed") {
		t.Fatalf("lease=%#v err=%v, want ownership rejection", lease, err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("deleted=%v, want no destructive call", fake.deleted)
	}
}

func TestAzureListExcludesWeakTags(t *testing.T) {
	owned := azureTestServer("crabbox-owned", "cbx_123456abcdef", "owned")
	weak := azureTestServer("crabbox-weak", "cbx_fedcba654321", "weak")
	delete(weak.Labels, "provider")
	fake := &fakeAzureClient{servers: []Server{weak, owned}}
	oldClient := newAzureClient
	newAzureClient = func(context.Context, Config) (azureClient, error) { return fake, nil }
	t.Cleanup(func() { newAzureClient = oldClient })

	backend := NewAzureLeaseBackend(ProviderSpec{}, azureAcquireTestConfig(), Runtime{Stderr: io.Discard}).(*azureLeaseBackend)
	servers, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || servers[0].CloudID != owned.CloudID {
		t.Fatalf("servers=%#v, want only canonical owned VM", servers)
	}
}

func TestAzureReleaseRejectsForgedOrMismatchedOwnership(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*LeaseTarget)
	}{
		{
			name: "missing created-by tag",
			mutate: func(lease *LeaseTarget) {
				delete(lease.Server.Labels, "created_by")
			},
		},
		{
			name: "mismatched lease tag",
			mutate: func(lease *LeaseTarget) {
				lease.LeaseID = "cbx_fedcba654321"
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeAzureClient{}
			oldClient := newAzureClient
			newAzureClient = func(context.Context, Config) (azureClient, error) { return fake, nil }
			t.Cleanup(func() { newAzureClient = oldClient })

			lease := LeaseTarget{Server: azureTestServer("crabbox-owned", "cbx_123456abcdef", "owned"), LeaseID: "cbx_123456abcdef"}
			test.mutate(&lease)
			backend := NewAzureLeaseBackend(ProviderSpec{}, azureAcquireTestConfig(), Runtime{Stderr: io.Discard}).(*azureLeaseBackend)
			err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease})
			if err == nil || !strings.Contains(err.Error(), "matching canonical Crabbox ownership tags") {
				t.Fatalf("err=%v, want ownership rejection", err)
			}
			if len(fake.deleted) != 0 {
				t.Fatalf("deleted=%v, want no destructive call", fake.deleted)
			}
		})
	}
}

func TestAzureReleaseRemovesStoredLeaseKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	leaseID := "cbx_123456abcdef"
	keyPath, _, err := core.EnsureTestboxKeyForConfig(Config{}, leaseID)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeAzureClient{}
	oldClient := newAzureClient
	newAzureClient = func(context.Context, Config) (azureClient, error) { return fake, nil }
	t.Cleanup(func() { newAzureClient = oldClient })

	lease := LeaseTarget{Server: azureTestServer("crabbox-owned", leaseID, "owned"), LeaseID: leaseID}
	backend := NewAzureLeaseBackend(ProviderSpec{}, azureAcquireTestConfig(), Runtime{Stderr: io.Discard}).(*azureLeaseBackend)
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(keyPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stored lease key directory still exists: %v", err)
	}
}

func TestAzureCleanupSkipsWeakTagsAndDeletesCanonicalExpiredVM(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	owned := azureTestServer("crabbox-owned", "cbx_123456abcdef", "owned")
	owned.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	keyPath, _, err := core.EnsureTestboxKeyForConfig(Config{}, owned.Labels["lease"])
	if err != nil {
		t.Fatal(err)
	}
	weak := azureTestServer("crabbox-weak", "cbx_fedcba654321", "weak")
	delete(weak.Labels, "created_by")
	weak.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	fake := &fakeAzureClient{servers: []Server{weak, owned}}
	oldClient := newAzureClient
	newAzureClient = func(context.Context, Config) (azureClient, error) { return fake, nil }
	t.Cleanup(func() { newAzureClient = oldClient })

	var stderr strings.Builder
	backend := NewAzureLeaseBackend(ProviderSpec{}, azureAcquireTestConfig(), Runtime{Stderr: &stderr}).(*azureLeaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "skip server id=crabbox-weak") || !strings.Contains(stderr.String(), "canonical Crabbox ownership tags missing") {
		t.Fatalf("stderr=%q, want weak-tag skip diagnostic", stderr.String())
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != owned.CloudID {
		t.Fatalf("deleted=%v, want only canonical owned VM", fake.deleted)
	}
	if _, err := os.Stat(filepath.Dir(keyPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stored lease key directory still exists: %v", err)
	}
}

func TestAzureCleanupRevalidatesLiveOwnershipBeforeDelete(t *testing.T) {
	snapshot := azureTestServer("crabbox-stale", "cbx_123456abcdef", "stale")
	snapshot.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	live := snapshot
	live.Labels = maps.Clone(snapshot.Labels)
	live.Labels["lease"] = "cbx_fedcba654321"
	fake := &fakeAzureClient{
		servers: []Server{snapshot},
		get:     map[string]Server{snapshot.CloudID: live},
	}
	oldClient := newAzureClient
	newAzureClient = func(context.Context, Config) (azureClient, error) { return fake, nil }
	t.Cleanup(func() { newAzureClient = oldClient })

	var stderr strings.Builder
	backend := NewAzureLeaseBackend(ProviderSpec{}, azureAcquireTestConfig(), Runtime{Stderr: &stderr}).(*azureLeaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.getIDs) != 1 || fake.getIDs[0] != snapshot.CloudID {
		t.Fatalf("live lookups=%v, want %s", fake.getIDs, snapshot.CloudID)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("cleanup crossed changed ownership: deleted=%v", fake.deleted)
	}
	if !strings.Contains(stderr.String(), "does not match cleanup candidate lease") {
		t.Fatalf("stderr=%q, want changed-lease skip", stderr.String())
	}
}

func TestAzureCleanupRevalidatesLiveEligibilityBeforeDelete(t *testing.T) {
	snapshot := azureTestServer("crabbox-renewed", "cbx_123456abcdef", "renewed")
	snapshot.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	live := snapshot
	live.Labels = maps.Clone(snapshot.Labels)
	live.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(time.Hour))
	fake := &fakeAzureClient{servers: []Server{snapshot}, get: map[string]Server{snapshot.CloudID: live}}
	oldClient := newAzureClient
	newAzureClient = func(context.Context, Config) (azureClient, error) { return fake, nil }
	t.Cleanup(func() { newAzureClient = oldClient })

	var stderr strings.Builder
	backend := NewAzureLeaseBackend(ProviderSpec{}, azureAcquireTestConfig(), Runtime{Stderr: &stderr}).(*azureLeaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("cleanup deleted renewed VM: %v", fake.deleted)
	}
	if !strings.Contains(stderr.String(), "reason=live VM") {
		t.Fatalf("stderr=%q, want renewed-live skip", stderr.String())
	}
}

func TestAzureCleanupRejectsSameNameReplacementVM(t *testing.T) {
	snapshot := azureTestServer("crabbox-replaced", "cbx_123456abcdef", "replaced")
	snapshot.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	live := snapshot
	live.ImmutableID = "vmid-replacement"
	fake := &fakeAzureClient{servers: []Server{snapshot}, get: map[string]Server{snapshot.CloudID: live}}
	oldClient := newAzureClient
	newAzureClient = func(context.Context, Config) (azureClient, error) { return fake, nil }
	t.Cleanup(func() { newAzureClient = oldClient })

	var stderr strings.Builder
	backend := NewAzureLeaseBackend(ProviderSpec{}, azureAcquireTestConfig(), Runtime{Stderr: &stderr}).(*azureLeaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("cleanup deleted replacement VM: %v", fake.deleted)
	}
	if !strings.Contains(stderr.String(), "VM identity") {
		t.Fatalf("stderr=%q, want replacement identity skip", stderr.String())
	}
}

func TestAzureCleanupDeleteBoundaryUsesOriginalCandidate(t *testing.T) {
	snapshot := azureTestServer("candidate", "cbx_111111111111", "original")
	snapshot.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	live := snapshot
	live.Labels = maps.Clone(snapshot.Labels)
	live.Labels["slug"] = "changed"
	fake := &fakeAzureClient{servers: []Server{snapshot}, get: map[string]Server{snapshot.CloudID: live}}
	oldClient := newAzureClient
	newAzureClient = func(context.Context, Config) (azureClient, error) { return fake, nil }
	t.Cleanup(func() { newAzureClient = oldClient })

	backend := NewAzureLeaseBackend(ProviderSpec{}, azureAcquireTestConfig(), Runtime{Stderr: io.Discard}).(*azureLeaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.cleanupExpected) != 1 || fake.cleanupExpected[0].Labels["slug"] != "original" {
		t.Fatalf("cleanup boundary candidates=%v, want original list candidate", fake.cleanupExpected)
	}
}

func TestAzureCleanupContinuesWhenLiveCandidateAlreadyGone(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	missing := azureTestServer("missing", "cbx_111111111111", "missing")
	remaining := azureTestServer("remaining", "cbx_222222222222", "remaining")
	for _, server := range []*Server{&missing, &remaining} {
		server.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	}
	keyPath, _, err := core.EnsureTestboxKeyForConfig(Config{}, missing.Labels["lease"])
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeAzureClient{
		servers: []Server{missing, remaining},
		get:     map[string]Server{remaining.CloudID: remaining},
		getErrs: map[string]error{missing.CloudID: core.Exit(4, "azure vm not found: %s", missing.CloudID)},
	}
	oldClient := newAzureClient
	newAzureClient = func(context.Context, Config) (azureClient, error) { return fake, nil }
	t.Cleanup(func() { newAzureClient = oldClient })

	var stderr strings.Builder
	backend := NewAzureLeaseBackend(ProviderSpec{}, azureAcquireTestConfig(), Runtime{Stderr: &stderr}).(*azureLeaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != remaining.CloudID {
		t.Fatalf("deleted=%v, want only remaining candidate", fake.deleted)
	}
	if !strings.Contains(stderr.String(), "reason=live VM no longer exists") {
		t.Fatalf("stderr=%q, want already-gone skip", stderr.String())
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("stored lease key was not retained for recovery: %v", err)
	}
}

func azureTestServer(id, leaseID, slug string) Server {
	return Server{
		CloudID:     id,
		Name:        id,
		Provider:    "azure",
		ImmutableID: "vmid-" + id,
		Labels: map[string]string{
			"crabbox":    "true",
			"created_by": "crabbox",
			"provider":   "azure",
			"lease":      leaseID,
			"slug":       slug,
		},
	}
}
