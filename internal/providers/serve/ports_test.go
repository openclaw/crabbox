package serve

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/localcontainer"
)

func TestParseSSHPorts(t *testing.T) {
	for _, tt := range []struct {
		spec string
		want []int
	}{
		{"8443", []int{8443}}, {"2200-2202", []int{2200, 2201, 2202}}, {"2200,2205-2207", []int{2200, 2205, 2206, 2207}}, {"1,65535", []int{1, 65535}}, {"22,22-23", []int{22, 23}},
	} {
		got, err := parseSSHPorts(tt.spec)
		if err != nil || !reflect.DeepEqual(got, tt.want) {
			t.Fatalf("%s: %v %v", tt.spec, got, err)
		}
	}
	for _, spec := range []string{"", "0", "65536", "2-1", "1-1025", "1-1024,2048", "-1", "+1", "1,", " 22", "1-2-3", "abc", "1-", "999999999999999999999"} {
		if _, err := parseSSHPorts(spec); err == nil {
			t.Errorf("accepted %q", spec)
		}
	}
	if got, err := parseSSHPorts("1-1024"); err != nil || len(got) != 1024 {
		t.Fatal(len(got), err)
	}
}
func freeTestPort(t *testing.T) (int, net.Listener) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return l.Addr().(*net.TCPAddr).Port, l
}
func TestPortAllocationSkipsBusyAndReleases(t *testing.T) {
	busy, listener := freeTestPort(t)
	defer listener.Close()
	free, freeListener := freeTestPort(t)
	_ = freeListener.Close()
	p, err := newPortPool(strconv.Itoa(busy)+","+strconv.Itoa(free), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if p.capacity() != 1 {
		t.Fatal(p.capacity())
	}
	got, err := p.acquire("one")
	if err != nil || got != strconv.Itoa(free) {
		t.Fatal(got, err)
	}
	if _, err = p.acquire("two"); err == nil || !strings.Contains(err.Error(), "serve_ssh_ports_exhausted") {
		t.Fatal(err)
	}
	p.release("one")
	if _, err = p.acquire("two"); err != nil {
		t.Fatal(err)
	}
	p.reconcile([]localcontainer.ServedContainer{{LeaseID: "existing", Port: strconv.Itoa(free)}})
	if p.capacity() != 1 {
		t.Fatal(p.capacity())
	}
}
func TestHeartbeatAdvertisesPortCapacity(t *testing.T) {
	busy, l := freeTestPort(t)
	defer l.Close()
	free, fl := freeTestPort(t)
	_ = fl.Close()
	p, _ := newPortPool(strconv.Itoa(busy)+","+strconv.Itoa(free), "127.0.0.1")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m machine
		_ = json.NewDecoder(r.Body).Decode(&m)
		if m.MaxLeases != 1 {
			t.Errorf("advertised capacity=%d", m.MaxLeases)
		}
		_ = json.NewEncoder(w).Encode(heartbeat{})
	}))
	defer server.Close()
	c, err := core.NewCoordinatorClient(core.Config{Coordinator: server.URL, CoordToken: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	a := agent{coord: c, containers: &fakeContainers{}, ports: p, opts: options{maxLeases: 8, stateDir: t.TempDir()}, state: localState{Machine: machine{MachineID: "m"}}, rt: core.Runtime{Stderr: io.Discard}}
	if err := a.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAssignmentFailsWhenPortPoolExhausted(t *testing.T) {
	busy, listener := freeTestPort(t)
	defer listener.Close()
	p, _ := newPortPool(strconv.Itoa(busy), "127.0.0.1")
	reports := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/failed") {
			var report map[string]string
			_ = json.NewDecoder(r.Body).Decode(&report)
			if !strings.Contains(report["reason"], "serve_ssh_ports_exhausted") {
				t.Error(report)
			}
			reports++
			return
		}
		var m machine
		_ = json.NewDecoder(r.Body).Decode(&m)
		if m.MaxLeases != 0 {
			t.Errorf("capacity=%d", m.MaxLeases)
		}
		_ = json.NewEncoder(w).Encode(heartbeat{Assignments: []assignment{{ID: "cbx_123456abcdef", MachineID: "m", CPUs: 1, MemoryMB: 1, Image: "configured", ExpiresAt: time.Now().Add(time.Hour), IdleDeadline: time.Now().Add(time.Hour).UnixMilli()}}})
	}))
	defer server.Close()
	c, err := core.NewCoordinatorClient(core.Config{Coordinator: server.URL, CoordToken: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeContainers{}
	a := agent{coord: c, containers: f, ports: p, opts: options{maxLeases: 2, cpus: 2, stateDir: t.TempDir()}, state: localState{Machine: machine{MachineID: "m", CPUs: 2, MemoryMB: 2, Image: "configured"}}, rt: core.Runtime{Stderr: io.Discard}}
	if err := a.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reports != 1 || f.created != 0 {
		t.Fatalf("reports=%d created=%d", reports, f.created)
	}
}
