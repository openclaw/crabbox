package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/localcontainer"
)

type slowPreparationRuntime struct {
	started    chan struct{}
	heartbeats atomic.Int32
	refreshed  atomic.Int32
	removed    atomic.Int32
}

func (f *slowPreparationRuntime) Run(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	if req.Name != "docker" {
		return core.LocalCommandResult{}, nil // Host capacity sampling.
	}
	a := req.Args
	switch a[0] {
	case "version":
		return core.LocalCommandResult{Stdout: "test-runtime"}, nil
	case "image":
		if a[2] == "configured:stable" {
			return core.LocalCommandResult{Stdout: `[{"Id":"sha256:base"}]`}, nil
		}
		return core.LocalCommandResult{}, fmt.Errorf("No such image")
	case "run":
		if a[1] == "--rm" {
			if f.heartbeats.Load() == 0 {
				return core.LocalCommandResult{}, fmt.Errorf("preparation started before first heartbeat")
			}
			return core.LocalCommandResult{Stdout: "prepare"}, nil
		}
		close(f.started)
		<-ctx.Done()
		return core.LocalCommandResult{}, ctx.Err()
	case "ps":
		return core.LocalCommandResult{Stdout: "existing"}, nil
	case "inspect":
		return core.LocalCommandResult{Stdout: `[{"Id":"existing","State":{"Running":true},"Config":{"Labels":{"lease":"cbx_123456abcdef","crabbox.serve":"true","crabbox.serve.machine":"test-machine"}},"NetworkSettings":{"Ports":{"2222/tcp":[{"HostIp":"127.0.0.1","HostPort":"22222"}]}}}]`}, nil
	case "exec":
		f.refreshed.Add(1)
		return core.LocalCommandResult{}, nil
	case "rm":
		if a[2] == "existing" {
			f.removed.Add(1)
		}
		return core.LocalCommandResult{}, nil
	}
	return core.LocalCommandResult{}, fmt.Errorf("unexpected runtime command: %v", a)
}

func TestAgentRestartReconcilesDuringSlowImagePreparation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f := &slowPreparationRuntime{started: make(chan struct{})}
	l := assignment{ID: "cbx_123456abcdef", MachineID: "test-machine", CPUs: 1, MemoryMB: 1024, Image: "configured:stable", State: "running", ExpiresAt: time.Now().Add(time.Hour), IdleDeadline: time.Now().Add(time.Hour).UnixMilli()}
	reports := make(chan machine, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/serve/heartbeat" {
			t.Errorf("unexpected request: %s", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var m machine
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			t.Error(err)
		}
		if !m.Draining {
			f.heartbeats.Add(1)
			reports <- m
		}
		_ = json.NewEncoder(w).Encode(heartbeat{Assignments: []assignment{l}})
	}))
	defer server.Close()
	candidates, err := detectCandidates(nil, false, nil)
	if err != nil {
		t.Skip("no non-loopback IPv4 for agent integration")
	}
	dir := t.TempDir()
	saved := agent{opts: options{stateDir: dir}, state: localState{Machine: machine{MachineID: l.MachineID}, Assignments: map[string]assignment{l.ID: l}}}
	if err := saved.save(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- (Provider{}).RunAgent(ctx, core.Config{Coordinator: server.URL, CoordToken: "synthetic", LocalContainer: core.LocalContainerConfig{Runtime: "docker", Image: l.Image}}, core.Runtime{Exec: f, Stdout: io.Discard, Stderr: io.Discard}, []string{"--state-dir", dir, "--memory", "2g", "--cpus", "2", "--advertise-address", candidates[0].Address, "--no-cache-volumes", "--poll-interval", "500ms"})
	}()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("agent did not cancel and clean up preparation")
		}
	}()
	select {
	case <-f.started:
	case <-time.After(5 * time.Second):
		t.Fatal("preparation did not start after initial reconciliation")
	}
	// Preparation remains blocked until cancellation, across multiple real
	// polling intervals. Existing assignments must keep reaching the broker.
	for range 4 {
		select {
		case m := <-reports:
			if len(m.Leases) != 1 || m.Leases[0].ID != l.ID || m.Leases[0].State != "running" {
				t.Fatalf("lost existing assignment: %+v", m.Leases)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("slow preparation delayed heartbeat")
		}
	}
	if f.refreshed.Load() < 3 || f.removed.Load() != 0 {
		t.Fatalf("refreshes=%d removals=%d", f.refreshed.Load(), f.removed.Load())
	}
}

func TestAgentRejectsExcessLeaseCapacity(t *testing.T) {
	err := (Provider{}).RunAgent(context.Background(), core.Config{
		Coordinator: "https://coordinator.example.com",
		CoordToken:  "synthetic-local-test",
	}, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}, []string{
		"--state-dir", t.TempDir(), "--max-leases", "33",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid serve caps") {
		t.Fatalf("expected invalid serve caps, got %v", err)
	}
}
func TestFallbackOnlyCapacity(t *testing.T) {
	b := backend{cfg: core.Config{Serve: core.ServeConfig{Fallback: "aws"}}}
	for _, tt := range []struct {
		code int
		body string
		want bool
	}{{409, `{"error":"serve_capacity_unavailable"}`, true}, {409, `{"error":"other"}`, false}, {503, `{"error":"serve_capacity_unavailable"}`, false}, {401, `{"error":"unauthorized"}`, false}} {
		name, ok := b.AcquisitionFallback(core.CoordinatorHTTPError{StatusCode: tt.code, Message: tt.body})
		if ok != tt.want || ok && name != "aws" {
			t.Fatalf("%+v: %s %v", tt, name, ok)
		}
	}
}

type fakeContainers struct {
	items     []localcontainer.ServedContainer
	removed   []string
	created   int
	refreshed int
}

func (f *fakeContainers) List(context.Context) ([]localcontainer.ServedContainer, error) {
	return append([]localcontainer.ServedContainer(nil), f.items...), nil
}
func (f *fakeContainers) Create(context.Context, string, string, int, int, string, []core.CacheVolumeConfig, localcontainer.ServedContainerOptions) (localcontainer.ServedContainer, error) {
	f.created++
	return localcontainer.ServedContainer{}, fmt.Errorf("not expected")
}
func (f *fakeContainers) Remove(_ context.Context, c localcontainer.ServedContainer, _ string) error {
	f.removed = append(f.removed, c.ID)
	for i, x := range f.items {
		if c.ID == x.ID {
			f.items = append(f.items[:i], f.items[i+1:]...)
			break
		}
	}
	return nil
}
func (f *fakeContainers) Refresh(context.Context, localcontainer.ServedContainer, string, int64) error {
	f.refreshed++
	return nil
}
func (f *fakeContainers) HostKey(context.Context, localcontainer.ServedContainer) (string, error) {
	return "", fmt.Errorf("not ready")
}
func TestAgentReconciliationOwnershipAndDeadlines(t *testing.T) {
	for _, deadlineKind := range []string{"ttl", "idle", "unknown"} {
		t.Run(deadlineKind, func(t *testing.T) {
			id := "cbx_123456abcdef"
			machineID := "test-machine"
			f := &fakeContainers{items: []localcontainer.ServedContainer{{ID: "own", LeaseID: id, Labels: map[string]string{"crabbox.serve": "true", "crabbox.serve.machine": machineID}}, {ID: "foreign", Labels: map[string]string{"crabbox.serve": "true", "crabbox.serve.machine": "another"}}, {ID: "unlabeled"}}}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "heartbeat") {
					t.Error(r.URL.Path)
				}
				_ = json.NewEncoder(w).Encode(heartbeat{})
			}))
			defer server.Close()
			cfg := core.Config{Coordinator: server.URL, CoordToken: "synthetic-local-test"}
			c, err := core.NewCoordinatorClient(cfg)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			l := assignment{ID: id, ExpiresAt: now.Add(time.Hour), IdleDeadline: now.Add(time.Hour).UnixMilli()}
			if deadlineKind == "ttl" {
				l.ExpiresAt = now.Add(-time.Second)
			}
			if deadlineKind == "idle" {
				l.IdleDeadline = now.Add(-time.Second).UnixMilli()
			}
			a := agent{opts: options{stateDir: t.TempDir()}, state: localState{Machine: machine{MachineID: machineID}, Assignments: map[string]assignment{id: l}}, containers: f, coord: c, rt: core.Runtime{Stderr: io.Discard}}
			if err := a.poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(f.removed) != 1 || f.removed[0] != "own" {
				t.Fatalf("removed=%v", f.removed)
			}
		})
	}
}
func TestLocalDeadlinesWhileCoordinatorOffline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer server.Close()
	c, err := core.NewCoordinatorClient(core.Config{Coordinator: server.URL, CoordToken: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"ttl", "idle"} {
		t.Run(kind, func(t *testing.T) {
			l := assignment{ExpiresAt: time.Now().Add(time.Hour), IdleDeadline: time.Now().Add(time.Hour).UnixMilli()}
			if kind == "ttl" {
				l.ExpiresAt = time.Now().Add(-time.Second)
			} else {
				l.IdleDeadline = time.Now().Add(-time.Second).UnixMilli()
			}
			f := &fakeContainers{items: []localcontainer.ServedContainer{{ID: "owned", LeaseID: "cbx_123456abcdef", Labels: map[string]string{"crabbox.serve": "true", "crabbox.serve.machine": "m"}}}}
			a := agent{coord: c, containers: f, state: localState{Machine: machine{MachineID: "m"}, Assignments: map[string]assignment{"cbx_123456abcdef": l}}, rt: core.Runtime{Stderr: io.Discard}}
			if err := a.poll(context.Background()); err == nil {
				t.Fatal("expected unreachable broker")
			}
			if len(f.removed) != 1 {
				t.Fatal(f.removed)
			}
		})
	}
}

func (f *fakeContainers) Evidence(context.Context, localcontainer.ServedContainer) (*core.ImageEvidence, error) {
	return nil, nil
}

func TestClassSizes(t *testing.T) {
	specs := classSizes()
	want := []struct {
		class        string
		cpus, memory int
	}{{"standard", 8, 16}, {"fast", 16, 32}, {"large", 32, 64}, {"beast", 0, 0}}
	for i, w := range want {
		if specs[i].Class != w.class || specs[i].VCPUs != w.cpus || specs[i].MemoryGB != w.memory {
			t.Fatalf("%s: %+v", w.class, specs[i])
		}
	}
}

func TestServeTargetUsesSharedReadinessThroughProxy(t *testing.T) {
	target := core.SSHTarget{Port: "22", FallbackPorts: []string{"22"}}
	Provider{}.ConfigureSSHTarget(&target, "")
	if !target.SSHConfigProxy {
		t.Fatal("serve readiness must use the SSH proxy, never TCP dial the lease alias")
	}
	if target.FallbackPorts == nil || len(target.FallbackPorts) != 0 {
		t.Fatalf("fallback ports=%v", target.FallbackPorts)
	}
	if !strings.Contains(target.ReadyCheck, "git --version") || !strings.Contains(target.ReadyCheck, "rsync --version") {
		t.Fatal(target.ReadyCheck)
	}
	if strings.Contains(target.ReadyCheck, "/usr/local/bin/crabbox-ready") {
		t.Fatal("cloud readiness leaked into container")
	}
}

func TestRejectedOwnerStillCleansLocalDeadlines(t *testing.T) {
	for _, kind := range []string{"ttl", "idle"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(403)
				_, _ = io.WriteString(w, `{"error":"serve_not_enabled"}`)
			}))
			defer server.Close()
			c, err := core.NewCoordinatorClient(core.Config{Coordinator: server.URL, CoordToken: "synthetic"})
			if err != nil {
				t.Fatal(err)
			}
			l := assignment{ExpiresAt: time.Now().Add(time.Hour), IdleDeadline: time.Now().Add(time.Hour).UnixMilli()}
			if kind == "ttl" {
				l.ExpiresAt = time.Now().Add(-time.Second)
			} else {
				l.IdleDeadline = time.Now().Add(-time.Second).UnixMilli()
			}
			f := &fakeContainers{items: []localcontainer.ServedContainer{{ID: "owned", LeaseID: "cbx_123456abcdef", Labels: map[string]string{"crabbox.serve": "true", "crabbox.serve.machine": "m"}}}}
			a := agent{coord: c, containers: f, state: localState{Machine: machine{MachineID: "m"}, Assignments: map[string]assignment{"cbx_123456abcdef": l}}, rt: core.Runtime{Stderr: io.Discard}}
			if err := a.poll(context.Background()); err == nil || !strings.Contains(err.Error(), "serve_not_enabled") {
				t.Fatalf("error=%v", err)
			}
			if len(f.removed) != 1 {
				t.Fatal(f.removed)
			}
		})
	}
}

func TestAgentRejectsDifferentImage(t *testing.T) {
	const id = "cbx_123456abcdef"
	reports := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/failed") {
			var report map[string]string
			_ = json.NewDecoder(r.Body).Decode(&report)
			if report["reason"] != "serve_image_mismatch" {
				t.Error(report)
			}
			reports++
			return
		}
		_ = json.NewEncoder(w).Encode(heartbeat{Assignments: []assignment{{ID: id, MachineID: "m", CPUs: 1, MemoryMB: 1, Image: "foreign-image"}}})
	}))
	defer server.Close()
	c, err := core.NewCoordinatorClient(core.Config{Coordinator: server.URL, CoordToken: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeContainers{}
	a := agent{coord: c, containers: f, opts: options{stateDir: t.TempDir()}, state: localState{Machine: machine{MachineID: "m", CPUs: 2, MemoryMB: 2, Image: "configured-image"}}, rt: core.Runtime{Stderr: io.Discard}}
	if err := a.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.created != 0 || reports != 1 {
		t.Fatalf("created=%d reports=%d", f.created, reports)
	}
}

func TestShutdownRefreshesInventoryBeforeDrainingHeartbeat(t *testing.T) {
	for _, drain := range []bool{false, true} {
		t.Run(fmt.Sprint(drain), func(t *testing.T) {
			id := "cbx_123456abcdef"
			var got machine
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&got)
				_ = json.NewEncoder(w).Encode(heartbeat{})
			}))
			defer server.Close()
			coord, err := core.NewCoordinatorClient(core.Config{Coordinator: server.URL, CoordToken: "synthetic"})
			if err != nil {
				t.Fatal(err)
			}
			containers := &fakeContainers{items: []localcontainer.ServedContainer{{ID: "newly-started", LeaseID: id, Labels: map[string]string{"crabbox.serve": "true", "crabbox.serve.machine": "m"}}, {ID: "foreign", LeaseID: "cbx_000000000002", Labels: map[string]string{"crabbox.serve": "true", "crabbox.serve.machine": "other"}}}}
			a := agent{coord: coord, containers: containers, opts: options{stateDir: t.TempDir(), drain: drain}, state: localState{Machine: machine{MachineID: "m", Leases: []leaseView{{ID: "stale", State: "running"}}}}}
			if err := a.shutdown(); err != nil {
				t.Fatal(err)
			}
			if !got.Draining {
				t.Fatal("shutdown must drain")
			}
			if drain {
				if len(got.Leases) != 0 || len(containers.removed) != 1 {
					t.Fatalf("leases=%v removed=%v", got.Leases, containers.removed)
				}
			} else if len(got.Leases) != 1 || got.Leases[0].ID != id || len(containers.removed) != 0 {
				t.Fatalf("leases=%v removed=%v", got.Leases, containers.removed)
			}
		})
	}
}

// Two List calls precede each heartbeat. Alternating slow and fast polls expose
// both fixed-cadence compression and the ticker's buffered-tick catch-up.
type pacedContainers struct {
	fakeContainers
	lists int
	delay time.Duration
}

func (f *pacedContainers) List(ctx context.Context) ([]localcontainer.ServedContainer, error) {
	f.lists++
	if f.lists%4 == 1 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, nil
}

func TestAgentRunPacesHeartbeatReceipts(t *testing.T) {
	const interval = 500 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	var mu sync.Mutex
	var receipts []time.Time
	var accepted time.Time
	rateLimited, drained := 0, false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m machine
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if m.Draining {
			drained = true
		} else {
			now := time.Now()
			receipts = append(receipts, now)
			if len(receipts) == 4 {
				defer cancel()
			}
			if !accepted.IsZero() && now.Sub(accepted) < interval {
				rateLimited++
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"error":"serve_rate_limited"}`)
				return
			}
			accepted = now
		}
		_ = json.NewEncoder(w).Encode(heartbeat{})
	}))
	defer server.Close()
	coord, err := core.NewCoordinatorClient(core.Config{Coordinator: server.URL, CoordToken: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	a := agent{coord: coord, containers: &pacedContainers{delay: 450 * time.Millisecond}, opts: options{interval: interval, stateDir: t.TempDir()}, state: localState{Machine: machine{MachineID: "m"}}, rt: core.Runtime{Stderr: io.Discard}}
	if err := a.run(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if rateLimited != 0 || len(receipts) != 4 || !drained {
		t.Errorf("rate limited=%d receipts=%d drained=%t", rateLimited, len(receipts), drained)
	}
	for i := 1; i < len(receipts); i++ {
		gap := receipts[i].Sub(receipts[i-1])
		t.Logf("heartbeat gap %d: %s", i, gap)
		if gap < interval {
			t.Errorf("heartbeat gap %d = %s, want >= %s", i, gap, interval)
		}
	}
}
