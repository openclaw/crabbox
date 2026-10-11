package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/localcontainer"
)

func TestAddressSelection(t *testing.T) {
	for _, tt := range []struct{ ip, want string }{{"100.64.1.2/24", "100.64.1.2"}, {"100.127.255.1/24", "100.127.255.1"}, {"100.128.1.1/24", ""}, {"192.168.1.1/24", ""}} {
		_, n, _ := net.ParseCIDR(tt.ip)
		parts := strings.Split(tt.ip, "/")
		n.IP = net.ParseIP(parts[0])
		got, err := advertiseAddress("", []net.Addr{n})
		if tt.want == "" {
			if err == nil {
				t.Fatal("accepted non-CGNAT", tt.ip)
			}
		} else if err != nil || got != tt.want {
			t.Fatalf("%s: %s %v", tt.ip, got, err)
		}
	}
	for _, ip := range []string{"0.0.0.0", "::", "224.0.0.1", "hostname"} {
		if _, err := advertiseAddress(ip, nil); err == nil {
			t.Fatal("accepted", ip)
		}
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

func TestServeTargetUsesSharedReadinessAndOnlyPublishedPort(t *testing.T) {
	target := core.SSHTarget{Port: "30022", FallbackPorts: []string{"22"}}
	Provider{}.ConfigureSSHTarget(&target, "")
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
