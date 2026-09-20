package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// parallelsCapacityFakeHost is a stateful stand-in for a Parallels host: it keeps a
// VM registry that `prlctl list` reads and `prlctl clone` mutates, so a clone issued
// by one caller becomes visible to every later list, as a real host behaves.
//
// A clone takes cloneDuration and registers its VM when it completes, modelling the
// window during which a multi-gigabyte clone is in flight. That window is the whole
// point: a capacity reservation is only sound if it spans it, because a caller that
// releases before its clone lands leaves the next caller counting an inventory that
// does not yet include it.
//
// The host records the peak number of concurrently registered crabbox- VMs, which is
// the quantity maxVMs is supposed to bound.
type parallelsCapacityFakeHost struct {
	mu            sync.Mutex
	vms           map[string]string // name -> id
	peak          int
	clones        int
	cloneDuration time.Duration
}

func newParallelsCapacityFakeHost(source string, cloneDuration time.Duration) *parallelsCapacityFakeHost {
	return &parallelsCapacityFakeHost{
		vms:           map[string]string{source: "{" + source + "-uuid}"},
		cloneDuration: cloneDuration,
	}
}

func (h *parallelsCapacityFakeHost) vmJSON(name, id string) map[string]any {
	return map[string]any{"ID": id, "Name": name, "State": "stopped", "ip_configured": "10.211.55.9"}
}

func (h *parallelsCapacityFakeHost) listJSON() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	items := make([]map[string]any, 0, len(h.vms))
	for name, id := range h.vms {
		items = append(items, h.vmJSON(name, id))
	}
	data, _ := json.Marshal(items)
	return string(data)
}

func (h *parallelsCapacityFakeHost) getJSON(handle string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	for name, id := range h.vms {
		if name == handle || id == handle || strings.Trim(id, "{}") == strings.Trim(handle, "{}") {
			data, _ := json.Marshal([]map[string]any{h.vmJSON(name, id)})
			return string(data)
		}
	}
	return "[]"
}

func (h *parallelsCapacityFakeHost) clone(name string) {
	time.Sleep(h.cloneDuration)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clones++
	h.vms[name] = fmt.Sprintf("{clone-%d-uuid}", h.clones)
	live := 0
	for vmName := range h.vms {
		if strings.HasPrefix(vmName, "crabbox-") {
			live++
		}
	}
	if live > h.peak {
		h.peak = live
	}
}

func (h *parallelsCapacityFakeHost) Run(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	args := req.Args
	switch {
	case len(args) >= 2 && args[0] == "list" && args[1] == "-a":
		return LocalCommandResult{Stdout: h.listJSON()}, nil
	case len(args) >= 2 && args[0] == "list" && args[1] == "-i":
		return LocalCommandResult{Stdout: h.getJSON(args[len(args)-1])}, nil
	case len(args) >= 4 && args[0] == "clone":
		name := ""
		for i := 0; i < len(args)-1; i++ {
			if args[i] == "--name" {
				name = args[i+1]
			}
		}
		h.clone(name)
		return LocalCommandResult{}, nil
	}
	return LocalCommandResult{Stdout: "[]"}, nil
}

func (h *parallelsCapacityFakeHost) stats() (peak, clones int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.peak, h.clones
}

func parallelsCapacityTestConfig(source string, maxVMs int) Config {
	cfg := baseConfig()
	cfg.Provider = "parallels"
	cfg.TargetOS = targetLinux
	cfg.Parallels.Source = source
	cfg.Parallels.CloneMode = "full"
	cfg.Parallels.Hosts = []ParallelsHostConfig{
		{Name: "fleet-host", Targets: []string{targetLinux}, MaxVMs: maxVMs},
	}
	return cfg
}

// TestParallelsFleetCapacityBoundsConcurrentForks is the regression test for the
// maxVMs check-then-act race. Each goroutine performs the same reserve-count-clone
// sequence the Parallels backend performs in acquireOnce
// (internal/providers/parallels/backend.go): reserve a fleet host, which counts live
// crabbox- VMs against maxVMs, clone into it, then release. The steps in between
// (bootstrap-key validation, slug allocation, snapshot resolution) do not touch
// capacity and are omitted.
//
// Before the reservation existed, every fork read the same pre-clone inventory and
// all 8 cloned onto a host configured maxVMs: 2. Releasing the reservation before
// the clone, or replacing it with the advisory SelectParallelsFleetConfig,
// reproduces that failure.
func TestParallelsFleetCapacityBoundsConcurrentForks(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const (
		source = "linux-template"
		forks  = 8
		maxVMs = 2
	)
	host := newParallelsCapacityFakeHost(source, 50*time.Millisecond)
	cfg := parallelsCapacityTestConfig(source, maxVMs)

	var wg sync.WaitGroup
	refusals := make([]error, forks)
	for i := 0; i < forks; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			ctx := context.Background()
			selected, release, err := ReserveParallelsFleetCapacity(ctx, cfg, host, source)
			if err != nil {
				refusals[index] = err
				return
			}
			defer release()
			leaseID := fmt.Sprintf("cbx_%012d", index)
			if _, err := NewParallelsClient(selected, host).Clone(ctx, source, "", leaseID, fmt.Sprintf("fork-%d", index), false); err != nil {
				t.Errorf("fork %d clone: %v", index, err)
			}
		}(i)
	}
	wg.Wait()

	refused := 0
	for _, err := range refusals {
		if err == nil {
			continue
		}
		refused++
		if !strings.Contains(err.Error(), "at maxVMs capacity") {
			t.Fatalf("fork refused for the wrong reason: %v", err)
		}
	}
	peak, clones := host.stats()
	if peak > maxVMs {
		t.Fatalf("host overfilled: peak crabbox VMs=%d maxVMs=%d clones=%d refused=%d", peak, maxVMs, clones, refused)
	}
	if clones != maxVMs || refused != forks-maxVMs {
		t.Fatalf("want %d clones and %d refusals, got clones=%d refused=%d peak=%d", maxVMs, forks-maxVMs, clones, refused, peak)
	}
}
