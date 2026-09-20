package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	isolateParallelsCapacityState(t)
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

func isolateParallelsCapacityState(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func TestParallelsCapacityExecutionIdentity(t *testing.T) {
	for _, tc := range []struct {
		name      string
		host      string
		user      string
		otherHost string
		otherUser string
		shared    bool
	}{
		{name: "display aliases and keys share remote capacity", host: "mac.example", user: "alice", otherHost: " mac.example ", otherUser: " alice ", shared: true},
		{name: "local aliases ignore remote account fields", user: "alice", otherUser: "bob", shared: true},
		{name: "different destinations", host: "mac.example", user: "alice", otherHost: "other.example", otherUser: "alice"},
		{name: "different accounts", host: "mac.example", user: "alice", otherHost: "mac.example", otherUser: "bob"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateParallelsCapacityState(t)
			cfg := parallelsCapacityTestConfig("template", 1)
			cfg.Parallels.Hosts[0].Name = "first-alias"
			cfg.Parallels.Hosts[0].Host = tc.host
			cfg.Parallels.Hosts[0].User = tc.user
			cfg.Parallels.Hosts[0].Key = "first-key"
			first := ParallelsCandidateConfigs(cfg)[0]
			cfg.Parallels.Hosts[0].Name = "second-alias"
			cfg.Parallels.Hosts[0].Host = tc.otherHost
			cfg.Parallels.Hosts[0].User = tc.otherUser
			cfg.Parallels.Hosts[0].Key = "second-key"
			second := ParallelsCandidateConfigs(cfg)[0]
			// Candidate configs retain Hosts; avoid changing the first candidate's
			// capacity lookup while constructing the second display alias.
			first.Parallels.Hosts = []ParallelsHostConfig{{Name: "first-alias", MaxVMs: 1}}
			release, err := lockParallelsFleetCapacity(context.Background(), first)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			otherRelease, err := lockParallelsFleetCapacity(ctx, second)
			if tc.shared {
				if otherRelease != nil {
					otherRelease()
				}
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("same execution identity acquired a second reservation: %v", err)
				}
			} else {
				if err != nil {
					t.Fatalf("independent execution identity blocked: %v", err)
				}
				otherRelease()
			}
		})
	}
}

func TestParallelsAdvisorySelectionDoesNotReserveCapacity(t *testing.T) {
	isolateParallelsCapacityState(t)
	const source = "template"
	cfg := parallelsCapacityTestConfig(source, 1)
	host := newParallelsCapacityFakeHost(source, 0)
	_, release, err := ReserveParallelsFleetCapacity(context.Background(), cfg, host, source)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := SelectParallelsFleetConfig(ctx, cfg, host, source); err != nil {
		t.Fatalf("advisory query blocked behind reservation: %v", err)
	}

	// A file in place of the state root makes any lock-state write fail even
	// when the test runs with elevated filesystem permissions.
	blockedState := filepath.Join(t.TempDir(), "state-file")
	if err := os.WriteFile(blockedState, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", blockedState)
	if _, err := SelectParallelsFleetConfig(context.Background(), cfg, host, source); err != nil {
		t.Fatalf("advisory query required writable state: %v", err)
	}
}

func TestParallelsUnlimitedCapacityBypassesReservation(t *testing.T) {
	isolateParallelsCapacityState(t)
	const source = "template"
	cfg := parallelsCapacityTestConfig(source, 1)
	host := newParallelsCapacityFakeHost(source, 0)
	_, release, err := ReserveParallelsFleetCapacity(context.Background(), cfg, host, source)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	cfg.Parallels.Hosts[0].MaxVMs = 0
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, unlimitedRelease, err := ReserveParallelsFleetCapacity(ctx, cfg, host, source)
	if err != nil {
		t.Fatalf("unlimited host waited on capacity reservation: %v", err)
	}
	unlimitedRelease()
}
