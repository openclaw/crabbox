package azure

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type fakeAzureClient struct {
	deleted   []string
	tagged    []string
	servers   []Server
	listErr   error
	created   Server
	createCfg Config
	createErr error
	waitErr   error
	getErr    error
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
	if c.getErr != nil {
		return Server{}, c.getErr
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

func TestAzureCleanupSkipsWeakTagsAndDeletesCanonicalExpiredVM(t *testing.T) {
	owned := azureTestServer("crabbox-owned", "cbx_123456abcdef", "owned")
	owned.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
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
}

func azureTestServer(id, leaseID, slug string) Server {
	return Server{
		CloudID:  id,
		Name:     id,
		Provider: "azure",
		Labels: map[string]string{
			"crabbox":    "true",
			"created_by": "crabbox",
			"provider":   "azure",
			"lease":      leaseID,
			"slug":       slug,
		},
	}
}
