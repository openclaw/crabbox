package proxmox

import (
	"context"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

type fakeProxmoxDoctorClient struct {
	listCalls int
	mutated   bool
	servers   []Server
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
	return Server{}, nil
}

func (c *fakeProxmoxDoctorClient) DeleteServer(context.Context, string) error {
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
