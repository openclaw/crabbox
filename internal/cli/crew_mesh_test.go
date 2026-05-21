package cli

import (
	"context"
	"flag"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// crewMeshRecordingHandle is the test double for crewMeshHandle. It records
// the argv at Start() and blocks Wait() on the signal channel so the orchestration
// loop can be terminated deterministically without real ssh processes.
type crewMeshRecordingHandle struct {
	name    string
	args    []string
	started bool
	signal  chan struct{}
	mu      sync.Mutex
}

func (h *crewMeshRecordingHandle) Start() error {
	h.mu.Lock()
	h.started = true
	h.mu.Unlock()
	return nil
}

func (h *crewMeshRecordingHandle) Wait() error {
	<-h.signal
	return nil
}

func (h *crewMeshRecordingHandle) String() string {
	return h.name + " " + strings.Join(h.args, " ")
}

func (h *crewMeshRecordingHandle) Process() processSignaler { return testProcessSignaler{h.signal} }

// testProcessSignaler closes the underlying channel on the first signal so
// the handle's Wait() returns.
type testProcessSignaler struct {
	signal chan struct{}
}

func (p testProcessSignaler) Signal(_ os.Signal) error {
	select {
	case <-p.signal:
	default:
		close(p.signal)
	}
	return nil
}

func (p testProcessSignaler) Kill() error {
	return p.Signal(nil)
}

// crewMeshRecordingRunner mirrors the exedev backend's pattern: it captures
// every (name, args) invocation it sees so tests can assert on the full SSH
// argument vector without spawning processes.
type crewMeshRecordingRunner struct {
	mu      sync.Mutex
	calls   [][]string
	handles []*crewMeshRecordingHandle
}

func (r *crewMeshRecordingRunner) Command(_ context.Context, name string, args ...string) crewMeshHandle {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, append([]string{name}, args...))
	h := &crewMeshRecordingHandle{name: name, args: append([]string{}, args...), signal: make(chan struct{})}
	r.handles = append(r.handles, h)
	return h
}

func TestRequestedExposedPortsAcceptsValidPorts(t *testing.T) {
	got, err := requestedExposedPorts([]string{"8080", "9090", "9090", "80,443"})
	if err != nil {
		t.Fatalf("requestedExposedPorts: %v", err)
	}
	want := []string{"80", "443", "8080", "9090"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ports=%v want %v", got, want)
	}
}

func TestRequestedExposedPortsRejectsBadInput(t *testing.T) {
	cases := []string{"abc", "0", "70000", "-1", ""}
	for _, in := range cases {
		if _, err := requestedExposedPorts([]string{in}); err == nil {
			t.Fatalf("expected error for input %q", in)
		}
	}
}

func TestRequestedExposedPortsCaps(t *testing.T) {
	values := []string{}
	for port := 8000; port < 8000+crewMaxExposedPortsPerLease+1; port++ {
		values = append(values, intString(port))
	}
	if _, err := requestedExposedPorts(values); err == nil {
		t.Fatalf("expected error when more than %d ports requested", crewMaxExposedPortsPerLease)
	}
}

func intString(value int) string {
	if value == 0 {
		return "0"
	}
	digits := []byte{}
	negative := value < 0
	if negative {
		value = -value
	}
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	if negative {
		digits = append([]byte{'-'}, digits...)
	}
	return string(digits)
}

func TestApplyLeaseCreateFlagsSetsExposedPorts(t *testing.T) {
	defaults := Config{
		Provider:    "hetzner",
		Profile:     "default",
		Class:       "standard",
		TargetOS:    targetLinux,
		TTL:         time.Hour,
		IdleTimeout: 15 * time.Minute,
		Network:     NetworkAuto,
		Capacity:    CapacityConfig{Market: "spot"},
	}
	fs := flag.NewFlagSet("warmup", flag.ContinueOnError)
	values := registerLeaseCreateFlags(fs, defaults)
	if err := fs.Parse([]string{"--expose", "8080", "--expose", "9090"}); err != nil {
		t.Fatal(err)
	}
	cfg := defaults
	if err := applyLeaseCreateFlags(&cfg, fs, values); err != nil {
		t.Fatalf("applyLeaseCreateFlags: %v", err)
	}
	want := []string{"8080", "9090"}
	if !reflect.DeepEqual(cfg.ExposedPorts, want) {
		t.Fatalf("cfg.ExposedPorts=%v want %v", cfg.ExposedPorts, want)
	}
}

func TestDirectLeaseLabelsRecordExposedPorts(t *testing.T) {
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	cfg := Config{
		Class:        "standard",
		Profile:      "default",
		ProviderKey:  "crabbox-cbx-abcdef123456",
		ServerType:   "cpx62",
		Crew:         "alpha",
		ExposedPorts: []string{"8080", "9090"},
		TTL:          15 * time.Minute,
		IdleTimeout:  4 * time.Minute,
	}
	labels := directLeaseLabels(cfg, "cbx_abcdef123456", "blue-lobster", "hetzner", "", true, now)
	if labels[crewExposedPortsLabelKey] != "8080-9090" {
		t.Fatalf("crabbox_exposed_ports label=%q want 8080-9090; full=%#v", labels[crewExposedPortsLabelKey], labels)
	}
}

func TestDirectLeaseLabelsOmitExposedPortsWhenEmpty(t *testing.T) {
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	cfg := Config{
		Class:       "standard",
		Profile:     "default",
		ProviderKey: "crabbox-cbx-abcdef123456",
		ServerType:  "cpx62",
		TTL:         15 * time.Minute,
		IdleTimeout: 4 * time.Minute,
	}
	labels := directLeaseLabels(cfg, "cbx_abcdef123456", "blue-lobster", "hetzner", "", true, now)
	if _, ok := labels[crewExposedPortsLabelKey]; ok {
		t.Fatalf("expected no exposed-ports label when none requested; got %#v", labels)
	}
}

func TestParseExposedPortsLabelTolerantOfGarbage(t *testing.T) {
	got := parseExposedPortsLabel("8080-xyz-9090-bad-80")
	want := []int{80, 8080, 9090}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ports=%v want %v", got, want)
	}
	if got := parseExposedPortsLabel("  "); len(got) != 0 {
		t.Fatalf("empty label should produce no ports; got %v", got)
	}
	if got := parseExposedPortsLabel("99999999"); len(got) != 0 {
		t.Fatalf("out-of-range token should be dropped; got %v", got)
	}
}

func TestCrewMeshDoctorCounts(t *testing.T) {
	servers := []Server{
		{Name: "web", Labels: map[string]string{crewLabelKey: "alpha", crewExposedPortsLabelKey: "8080-9090"}},
		{Name: "client", Labels: map[string]string{crewLabelKey: "alpha"}},
		{Name: "worker", Labels: map[string]string{crewLabelKey: "alpha", crewExposedPortsLabelKey: "3000"}},
	}
	members, exposed, ports := crewMeshDoctorCounts(servers)
	if members != 3 || exposed != 2 || ports != 3 {
		t.Fatalf("counts=(%d,%d,%d) want (3,2,3)", members, exposed, ports)
	}
}

func TestCrewMeshDoctorCountsEmpty(t *testing.T) {
	members, exposed, ports := crewMeshDoctorCounts(nil)
	if members != 0 || exposed != 0 || ports != 0 {
		t.Fatalf("counts=(%d,%d,%d) want (0,0,0)", members, exposed, ports)
	}
}

func TestPrepareCrewMeshSummaryRendersHostsAndEnv(t *testing.T) {
	tmp := t.TempDir()
	members := []crewMember{
		{Name: "web", SSH: SSHTarget{User: "ubuntu", Host: "1.2.3.4", Port: "22"}, Ports: []int{8080}, Lease: "cbx_aaa"},
		{Name: "worker", SSH: SSHTarget{User: "ubuntu", Host: "5.6.7.8", Port: "22"}, Ports: []int{3000, 4000}, Lease: "cbx_bbb"},
	}
	allocPort := 60000
	opts := crewConnectOptions{
		Stdout:  io.Discard,
		Stderr:  io.Discard,
		HomeDir: tmp,
		PortAlloc: func(used map[int]bool) (int, error) {
			for {
				allocPort++
				if !used[allocPort] {
					return allocPort, nil
				}
			}
		},
	}
	summary, err := prepareCrewMeshSummary("alpha", members, opts)
	if err != nil {
		t.Fatalf("prepareCrewMeshSummary: %v", err)
	}
	if len(summary.Forwards) != 3 {
		t.Fatalf("forwards=%d want 3", len(summary.Forwards))
	}
	if summary.Forwards[0].Peer != "web" || summary.Forwards[0].RemotePort != 8080 {
		t.Fatalf("first forward unexpected: %#v", summary.Forwards[0])
	}
	for i := 1; i < len(summary.Forwards); i++ {
		if summary.Forwards[i].LocalPort == summary.Forwards[i-1].LocalPort {
			t.Fatalf("duplicate local port %d in forwards %#v", summary.Forwards[i].LocalPort, summary.Forwards)
		}
	}
	hostsBody, err := os.ReadFile(summary.HostsPath)
	if err != nil {
		t.Fatalf("read hosts: %v", err)
	}
	if !strings.Contains(string(hostsBody), "web.cbx") || !strings.Contains(string(hostsBody), "worker.cbx") {
		t.Fatalf("hosts file missing peer entries: %s", hostsBody)
	}
	envBody, err := os.ReadFile(summary.EnvPath)
	if err != nil {
		t.Fatalf("read env: %v", err)
	}
	if !strings.Contains(string(envBody), "CRABBOX_CREW_WEB_8080") {
		t.Fatalf("env file missing CRABBOX_CREW_WEB_8080: %s", envBody)
	}
	wantHostsPath := filepath.Join(tmp, crewMeshHostsRoot, "alpha", crewMeshHostsFileName)
	if summary.HostsPath != wantHostsPath {
		t.Fatalf("HostsPath=%q want %q", summary.HostsPath, wantHostsPath)
	}
}

func TestPrepareCrewMeshSummaryEmpty(t *testing.T) {
	opts := crewConnectOptions{Stdout: io.Discard, Stderr: io.Discard, HomeDir: t.TempDir()}
	summary, err := prepareCrewMeshSummary("alpha", nil, opts)
	if err != nil {
		t.Fatalf("prepareCrewMeshSummary empty: %v", err)
	}
	if len(summary.Forwards) != 0 {
		t.Fatalf("expected zero forwards from empty input; got %d", len(summary.Forwards))
	}
}

func TestPrepareCrewMeshSummarySkipsMembersWithoutPorts(t *testing.T) {
	tmp := t.TempDir()
	members := []crewMember{
		{Name: "no-ports", SSH: SSHTarget{User: "ubuntu", Host: "1.2.3.4", Port: "22"}},
		{Name: "web", SSH: SSHTarget{User: "ubuntu", Host: "5.6.7.8", Port: "22"}, Ports: []int{8080}},
	}
	allocPort := 60500
	opts := crewConnectOptions{
		Stdout:  io.Discard,
		Stderr:  io.Discard,
		HomeDir: tmp,
		PortAlloc: func(used map[int]bool) (int, error) {
			for {
				allocPort++
				if !used[allocPort] {
					return allocPort, nil
				}
			}
		},
	}
	summary, err := prepareCrewMeshSummary("alpha", members, opts)
	if err != nil {
		t.Fatalf("prepareCrewMeshSummary: %v", err)
	}
	if len(summary.Forwards) != 1 || summary.Forwards[0].Peer != "web" {
		t.Fatalf("expected only one forward for web; got %#v", summary.Forwards)
	}
}

func TestCrewMeshSSHArgsBuildsLocalForward(t *testing.T) {
	target := SSHTarget{User: "ubuntu", Host: "lease.example", Port: "22", Key: "/tmp/test-key"}
	fwd := crewMeshForward{Peer: "web", RemotePort: 8080, LocalPort: 51900, LeaseID: "cbx_x"}
	args := crewMeshSSHArgsForForward(target, fwd)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-L 127.0.0.1:51900:127.0.0.1:8080") {
		t.Fatalf("args missing -L spec: %v", args)
	}
	if !strings.Contains(joined, "ubuntu@lease.example") {
		t.Fatalf("args missing user@host: %v", args)
	}
	if !strings.Contains(joined, "-N") {
		t.Fatalf("args missing -N (no remote command): %v", args)
	}
	if !strings.Contains(joined, "ControlMaster=auto") && !strings.Contains(joined, "ControlMaster=no") {
		t.Fatalf("args missing ControlMaster option: %v", args)
	}
}

func TestRunCrewMeshForwardsLaunchesPerForwardAndTearsDown(t *testing.T) {
	runner := &crewMeshRecordingRunner{}
	members := []crewMember{
		{Name: "web", SSH: SSHTarget{User: "ubuntu", Host: "lease-web.example", Port: "22"}, Ports: []int{8080}},
		{Name: "worker", SSH: SSHTarget{User: "ubuntu", Host: "lease-worker.example", Port: "22"}, Ports: []int{3000, 4000}},
	}
	forwards := []crewMeshForward{
		{Peer: "web", RemotePort: 8080, LocalPort: 60000},
		{Peer: "worker", RemotePort: 3000, LocalPort: 60001},
		{Peer: "worker", RemotePort: 4000, LocalPort: 60002},
	}
	summary := crewMeshSummary{Forwards: forwards}
	opts := crewConnectOptions{Stdout: io.Discard, Stderr: io.Discard, Runner: runner}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- runCrewMeshForwards(ctx, opts, members, summary) }()
	deadline := time.After(2 * time.Second)
	for {
		runner.mu.Lock()
		count := len(runner.handles)
		runner.mu.Unlock()
		if count >= 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("only %d handles started after 2s", count)
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-time.After(2 * time.Second):
		t.Fatalf("runCrewMeshForwards did not return after context cancel")
	case err := <-errCh:
		if err != nil {
			t.Fatalf("runCrewMeshForwards: %v", err)
		}
	}
	if got := len(runner.calls); got != 3 {
		t.Fatalf("expected 3 ssh invocations, got %d (%v)", got, runner.calls)
	}
	// Verify each ssh invocation carries the right -L spec.
	wantSpecs := []string{
		"127.0.0.1:60000:127.0.0.1:8080",
		"127.0.0.1:60001:127.0.0.1:3000",
		"127.0.0.1:60002:127.0.0.1:4000",
	}
	gotSpecs := []string{}
	for _, call := range runner.calls {
		if call[0] != "ssh" {
			t.Fatalf("expected ssh invocation, got %v", call)
		}
		for i, arg := range call {
			if arg == "-L" && i+1 < len(call) {
				gotSpecs = append(gotSpecs, call[i+1])
			}
		}
	}
	sort.Strings(wantSpecs)
	sort.Strings(gotSpecs)
	if !reflect.DeepEqual(wantSpecs, gotSpecs) {
		t.Fatalf("forward specs=%v want %v", gotSpecs, wantSpecs)
	}
}

func TestEnvSafeName(t *testing.T) {
	cases := map[string]string{
		"web":         "WEB",
		"client-foo":  "CLIENT_FOO",
		"a.b/c":       "A_B_C",
		"  ":          "_",
		"123-name":    "123_NAME",
		"crew/peer-1": "CREW_PEER_1",
	}
	for in, want := range cases {
		if got := envSafeName(in); got != want {
			t.Fatalf("envSafeName(%q)=%q want %q", in, got, want)
		}
	}
}

func TestRenderCrewMeshHostsFileIncludesAllPeers(t *testing.T) {
	body := renderCrewMeshHostsFile([]crewMeshForward{
		{Peer: "web", RemotePort: 8080, LocalPort: 51820},
		{Peer: "worker", RemotePort: 3000, LocalPort: 51821},
	})
	if !strings.Contains(body, "127.0.0.1:51820") || !strings.Contains(body, "127.0.0.1:51821") {
		t.Fatalf("hosts body missing local ports: %s", body)
	}
	if !strings.Contains(body, "web.cbx") || !strings.Contains(body, "worker.cbx") {
		t.Fatalf("hosts body missing peer names: %s", body)
	}
}

func TestRenderCrewMeshEnvFileEmitsStableExports(t *testing.T) {
	body, exports := renderCrewMeshEnvFile([]crewMeshForward{
		{Peer: "web", RemotePort: 8080, LocalPort: 51820},
		{Peer: "worker", RemotePort: 3000, LocalPort: 51821},
	})
	if len(exports) != 2 {
		t.Fatalf("exports=%d want 2", len(exports))
	}
	if !strings.Contains(exports[0], "export CRABBOX_CREW_WEB_8080=127.0.0.1:51820") {
		t.Fatalf("unexpected first export: %q", exports[0])
	}
	if !strings.Contains(body, "CRABBOX_CREW_WORKER_3000=127.0.0.1:51821") {
		t.Fatalf("env body missing worker export: %s", body)
	}
}
