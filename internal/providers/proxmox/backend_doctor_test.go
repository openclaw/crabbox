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
	mutated     bool
	servers     []Server
}

func (c *fakeProxmoxDoctorClient) ListCrabboxServers(context.Context) ([]Server, error) {
	c.listCalls++
	return c.servers, nil
}

func (c *fakeProxmoxDoctorClient) CreateServer(context.Context, Config, string, string, string, bool) (Server, error) {
	c.mutated = true
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

func (c *fakeProxmoxDoctorClient) DeleteServer(context.Context, string) error {
	c.deleteCalls++
	c.mutated = true
	return nil
}

func (c *fakeProxmoxDoctorClient) SetLabels(context.Context, string, map[string]string) error {
	c.mutated = true
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
