package proxmox

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type fakeProxmoxDoctorClient struct {
	listCalls   int
	getCalls    int
	deleteCalls int
	deletedIDs  []string
	mutated     bool
	servers     []Server
	created     Server
	setLabels   []map[string]string
}

func (c *fakeProxmoxDoctorClient) ListCrabboxServers(context.Context) ([]Server, error) {
	c.listCalls++
	return c.servers, nil
}

func (c *fakeProxmoxDoctorClient) CreateServer(context.Context, Config, string, string, string, bool) (Server, error) {
	c.mutated = true
	if c.created.CloudID != "" {
		return c.created, nil
	}
	return Server{}, nil
}

func (c *fakeProxmoxDoctorClient) GetServer(context.Context, string) (Server, error) {
	c.getCalls++
	if c.getCalls < 3 {
		return Server{CloudID: "101", Labels: map[string]string{"lease": "cbx_test", "slug": "test"}}, nil
	}
	server := Server{CloudID: "101", Labels: map[string]string{"lease": "cbx_test", "slug": "test"}}
	server.PublicNet.IPv4.IP = "192.0.2.10"
	return server, nil
}

func (c *fakeProxmoxDoctorClient) DeleteServer(_ context.Context, id string) error {
	c.deleteCalls++
	c.deletedIDs = append(c.deletedIDs, id)
	c.mutated = true
	return nil
}

func (c *fakeProxmoxDoctorClient) SetLabels(_ context.Context, _ string, labels map[string]string) error {
	c.mutated = true
	c.setLabels = append(c.setLabels, map[string]string{})
	for key, value := range labels {
		c.setLabels[len(c.setLabels)-1][key] = value
	}
	return nil
}

func TestProxmoxDoctorListsInventoryOnly(t *testing.T) {
	fake := &fakeProxmoxDoctorClient{servers: []Server{{CloudID: "101"}}}
	old := newClient
	newClient = func(Config) (proxmoxClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newClient = old })

	doctor, err := Provider{}.ConfigureDoctor(Config{}, Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := doctor.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider != "proxmox" || !strings.Contains(result.Message, "inventory=ready api=list mutation=false leases=1 runtime=unchecked") {
		t.Fatalf("result=%#v", result)
	}
	if fake.listCalls != 1 {
		t.Fatalf("list calls=%d, want 1", fake.listCalls)
	}
	if fake.mutated {
		t.Fatal("doctor called a mutating Proxmox method")
	}
}

func TestProxmoxAcquirePollsUntilServerIPIsAvailable(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeProxmoxDoctorClient{}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newClient = oldClient })
	oldWait := waitForSSHReadyFunc
	waitForSSHReadyFunc = func(_ context.Context, target *SSHTarget, _ io.Writer, _ string, _ time.Duration) error {
		if target.Host != "192.0.2.10" {
			t.Fatalf("ssh host=%q, want discovered IP", target.Host)
		}
		return nil
	}
	t.Cleanup(func() { waitForSSHReadyFunc = oldWait })
	oldPoll := proxmoxIPPollInterval
	proxmoxIPPollInterval = time.Millisecond
	t.Cleanup(func() { proxmoxIPPollInterval = oldPoll })

	backend := NewLeaseBackend(Provider{}.Spec(), Config{SSHUser: "root"}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	target, err := backend.Acquire(context.Background(), AcquireRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if target.Server.PublicNet.IPv4.IP != "192.0.2.10" {
		t.Fatalf("ip=%q, want discovered IP", target.Server.PublicNet.IPv4.IP)
	}
	if fake.getCalls != 3 {
		t.Fatalf("getCalls=%d, want 3", fake.getCalls)
	}
	if fake.deleteCalls != 0 {
		t.Fatal("delayed IP discovery should not delete the VM")
	}
}

func TestProxmoxAcquireInitializesNilLabels(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	created := Server{CloudID: "101"}
	created.PublicNet.IPv4.IP = "192.0.2.10"
	fake := &fakeProxmoxDoctorClient{
		created: created,
	}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newClient = oldClient })
	oldWait := waitForSSHReadyFunc
	waitForSSHReadyFunc = func(context.Context, *SSHTarget, io.Writer, string, time.Duration) error {
		return nil
	}
	t.Cleanup(func() { waitForSSHReadyFunc = oldWait })

	backend := NewLeaseBackend(Provider{}.Spec(), Config{SSHUser: "root"}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	target, err := backend.Acquire(context.Background(), AcquireRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if target.Server.Labels["state"] != "ready" {
		t.Fatalf("labels=%v, want state=ready", target.Server.Labels)
	}
	if len(fake.setLabels) != 1 || fake.setLabels[0]["state"] != "ready" {
		t.Fatalf("setLabels=%v, want state=ready", fake.setLabels)
	}
}

func TestProxmoxCleanupRemovesClaimAfterDelete(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_proxmox_cleanup"
	if err := core.ClaimLeaseForRepoProvider(leaseID, "old", "proxmox", t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProxmoxDoctorClient{servers: []Server{expiredProxmoxServer("101", leaseID)}}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newClient = oldClient })

	backend := NewLeaseBackend(Provider{}.Spec(), Config{}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if fake.deleteCalls != 1 {
		t.Fatalf("deleteCalls=%d, want 1", fake.deleteCalls)
	}
	if len(fake.deletedIDs) != 1 || fake.deletedIDs[0] != "101" {
		t.Fatalf("deletedIDs=%v, want [101]", fake.deletedIDs)
	}
	if _, ok, err := core.ResolveLeaseClaim(leaseID); err != nil || ok {
		t.Fatalf("claim ok=%t err=%v, want removed", ok, err)
	}
}

func TestProxmoxCleanupDryRunPreservesClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_proxmox_dryrun"
	if err := core.ClaimLeaseForRepoProvider(leaseID, "old", "proxmox", t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProxmoxDoctorClient{servers: []Server{expiredProxmoxServer("101", leaseID)}}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newClient = oldClient })

	backend := NewLeaseBackend(Provider{}.Spec(), Config{}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if fake.deleteCalls != 0 {
		t.Fatalf("deleteCalls=%d, want 0", fake.deleteCalls)
	}
	if _, ok, err := core.ResolveLeaseClaim(leaseID); err != nil || !ok {
		t.Fatalf("claim ok=%t err=%v, want preserved", ok, err)
	}
}

func TestProxmoxCleanupIgnoresInvalidClaimLabel(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeProxmoxDoctorClient{servers: []Server{expiredProxmoxServer("101", "../target")}}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newClient = oldClient })

	backend := NewLeaseBackend(Provider{}.Spec(), Config{}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deletedIDs) != 1 || fake.deletedIDs[0] != "101" {
		t.Fatalf("deletedIDs=%v, want [101]", fake.deletedIDs)
	}
}

func TestProxmoxCleanupWithoutLeaseLabelPreservesNumericClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := core.ClaimLeaseForRepoProvider("101", "numeric", "proxmox", t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	server := expiredProxmoxServer("101", "")
	delete(server.Labels, "lease")
	fake := &fakeProxmoxDoctorClient{servers: []Server{server}}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newClient = oldClient })

	backend := NewLeaseBackend(Provider{}.Spec(), Config{}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deletedIDs) != 1 || fake.deletedIDs[0] != "101" {
		t.Fatalf("deletedIDs=%v, want [101]", fake.deletedIDs)
	}
	if _, ok, err := core.ResolveLeaseClaim("101"); err != nil || !ok {
		t.Fatalf("numeric claim ok=%t err=%v, want preserved", ok, err)
	}
}

func expiredProxmoxServer(id, leaseID string) Server {
	return Server{
		CloudID: id,
		Name:    "crabbox-old",
		Labels: map[string]string{
			"lease":      leaseID,
			"slug":       "old",
			"keep":       "false",
			"state":      "ready",
			"expires_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		},
	}
}
