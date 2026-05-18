package gcp

import (
	"context"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

type fakeGCPDoctorClient struct {
	listCalls int
	mutated   bool
	servers   []Server
}

func (c *fakeGCPDoctorClient) ListCrabboxServers(context.Context) ([]Server, error) {
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

func (c *fakeGCPDoctorClient) DeleteServer(context.Context, string) error {
	c.mutated = true
	return nil
}

func (c *fakeGCPDoctorClient) SetLabels(context.Context, string, map[string]string) error {
	c.mutated = true
	return nil
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
