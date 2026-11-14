package cli

import (
	"strings"
	"sync"
	"testing"
)

// The lease-local egress client keys every proxied connection by this ID on
// both bridge ends; the host rejects an "open" for an ID that is already in
// flight, and the client overwrites its pending/client-conn entries. Two
// concurrent CONNECTs that receive the same ID therefore both fail (one with an
// immediate 502, its twin with a 502 after egressOpenTimeout). IDs must stay
// unique however many connections are accepted at the same instant.
func TestNewLocalEgressConnIDUniqueUnderConcurrentAccepts(t *testing.T) {
	const (
		workers      = 16
		idsPerWorker = 20000
	)
	start := make(chan struct{})
	results := make([][]string, workers)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids := make([]string, idsPerWorker)
			<-start
			for i := range ids {
				ids[i] = newLocalEgressConnID()
			}
			results[w] = ids
		}()
	}
	close(start)
	wg.Wait()

	seen := make(map[string]struct{}, workers*idsPerWorker)
	duplicates := 0
	for _, ids := range results {
		for _, id := range ids {
			if !strings.HasPrefix(id, "conn_") {
				t.Fatalf("connection ID %q lost the conn_ prefix", id)
			}
			if strings.Trim(id, "abcdefghijklmnopqrstuvwxyz0123456789_") != "" {
				t.Fatalf("connection ID %q is not a lowercase alphanumeric token", id)
			}
			if _, ok := seen[id]; ok {
				duplicates++
				if duplicates <= 3 {
					t.Errorf("duplicate connection ID %q", id)
				}
				continue
			}
			seen[id] = struct{}{}
		}
	}
	if duplicates > 0 {
		t.Fatalf("%d of %d connection IDs generated concurrently were duplicates", duplicates, workers*idsPerWorker)
	}
}
