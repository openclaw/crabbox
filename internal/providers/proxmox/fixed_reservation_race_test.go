package proxmox

import (
	"context"
	"errors"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type concurrentReservationClient struct {
	proxmoxClient
	arrived chan struct{}
	release chan struct{}
	clones  chan int
}

func (c *concurrentReservationClient) ListCrabboxServersCluster(context.Context) ([]core.Server, error) {
	return nil, nil
}

func (c *concurrentReservationClient) ListVMIDsInCluster(context.Context) ([]int, error) {
	return nil, nil
}

func (c *concurrentReservationClient) NextVMID(ctx context.Context) (int, error) {
	c.arrived <- struct{}{}
	select {
	case <-c.release:
		return 417, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (c *concurrentReservationClient) CreateServerWithVMID(_ context.Context, _ core.Config, _, _, _ string, _ bool, vmid int, _ map[string]string, _ func(core.Server) error) (core.Server, error) {
	c.clones <- vmid
	return core.Server{}, errors.New("synthetic uncertain clone outcome")
}

func TestProxmoxFixedConcurrentReservationsChooseDistinctVMIDs(t *testing.T) {
	backend, fake, first := fixedProxmoxFixture(t)
	client := &concurrentReservationClient{proxmoxClient: fake, arrived: make(chan struct{}, 2), release: make(chan struct{}), clones: make(chan int, 2)}
	newClient = func(core.Config) (proxmoxClient, error) { return client, nil }
	second := first
	second.RequestedLeaseID, second.RequestedSlug = "cbx_aaaaaaaaaaaa", "second-reservation"
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	results := make(chan error, 2)
	for _, req := range []core.AcquireRequest{first, second} {
		go func() { _, err := backend.Acquire(ctx, req); results <- err }()
	}
	select {
	case <-client.arrived:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Let both old planners read the same empty claim snapshot. A serialized
	// planner waits here until the first reservation becomes durable instead.
	select {
	case <-client.arrived:
	case <-time.After(time.Second):
	}
	close(client.release)
	for range 2 {
		if err := <-results; err == nil {
			t.Fatal("fixture clone unexpectedly succeeded")
		}
	}
	close(client.clones)
	ids := map[int]bool{}
	for id := range client.clones {
		if ids[id] {
			t.Fatalf("concurrent clone requests reused VMID %d", id)
		}
		ids[id] = true
	}
	if len(ids) != 2 || !ids[417] || !ids[418] {
		t.Fatalf("concurrent reservations did not both progress with distinct IDs: %v", ids)
	}
}
