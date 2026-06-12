package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// pondMeshRecordingHandle is the test double for pondMeshHandle. It records
// the argv at Start() and blocks Wait() on the signal channel so the orchestration
// loop can be terminated deterministically without real ssh processes.
type pondMeshRecordingHandle struct {
	name    string
	args    []string
	pid     int
	started bool
	signal  chan struct{}
	mu      sync.Mutex
}

func (h *pondMeshRecordingHandle) Start() error {
	h.mu.Lock()
	h.started = true
	h.mu.Unlock()
	return nil
}

func (h *pondMeshRecordingHandle) Wait() error {
	<-h.signal
	return nil
}

func (h *pondMeshRecordingHandle) String() string {
	return h.name + " " + strings.Join(h.args, " ")
}

func (h *pondMeshRecordingHandle) PID() int { return h.pid }

func (h *pondMeshRecordingHandle) Process() processSignaler { return testProcessSignaler{h.signal} }

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

// pondMeshRecordingRunner mirrors the exedev backend's pattern: it captures
// every (name, args) invocation it sees so tests can assert on the full SSH
// argument vector without spawning processes.
type pondMeshRecordingRunner struct {
	mu      sync.Mutex
	calls   [][]string
	handles []*pondMeshRecordingHandle
}

func (r *pondMeshRecordingRunner) Command(_ context.Context, name string, args ...string) pondMeshHandle {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, append([]string{name}, args...))
	h := &pondMeshRecordingHandle{name: name, args: append([]string{}, args...), pid: 1000 + len(r.handles), signal: make(chan struct{})}
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
	for port := 8000; port < 8000+pondMaxExposedPortsPerLease+1; port++ {
		values = append(values, intString(port))
	}
	if _, err := requestedExposedPorts(values); err == nil {
		t.Fatalf("expected error when more than %d ports requested", pondMaxExposedPortsPerLease)
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
		Pond:         "alpha",
		ExposedPorts: []string{"8080", "9090"},
		TTL:          15 * time.Minute,
		IdleTimeout:  4 * time.Minute,
	}
	labels := directLeaseLabels(cfg, "cbx_abcdef123456", "blue-lobster", "hetzner", "", true, now)
	if labels[pondExposedPortsLabelKey] != "8080-9090" {
		t.Fatalf("crabbox_exposed_ports label=%q want 8080-9090; full=%#v", labels[pondExposedPortsLabelKey], labels)
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
	if _, ok := labels[pondExposedPortsLabelKey]; ok {
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

func TestPondMeshDoctorCounts(t *testing.T) {
	servers := []Server{
		{Name: "web", Labels: map[string]string{pondLabelKey: "alpha", pondExposedPortsLabelKey: "8080-9090"}},
		{Name: "client", Labels: map[string]string{pondLabelKey: "alpha"}},
		{Name: "worker", Labels: map[string]string{pondLabelKey: "alpha", pondExposedPortsLabelKey: "3000"}},
	}
	members, exposed, ports := pondMeshDoctorCounts(servers)
	if members != 3 || exposed != 2 || ports != 3 {
		t.Fatalf("counts=(%d,%d,%d) want (3,2,3)", members, exposed, ports)
	}
}

func TestPondMeshDoctorCountsEmpty(t *testing.T) {
	members, exposed, ports := pondMeshDoctorCounts(nil)
	if members != 0 || exposed != 0 || ports != 0 {
		t.Fatalf("counts=(%d,%d,%d) want (0,0,0)", members, exposed, ports)
	}
}

func TestPreparePondMeshSummaryRendersHostsAndEnv(t *testing.T) {
	tmp := t.TempDir()
	members := []pondMember{
		{Name: "web", SSH: SSHTarget{User: "ubuntu", Host: "1.2.3.4", Port: "22"}, Ports: []int{8080}, Lease: "cbx_aaa"},
		{Name: "worker", SSH: SSHTarget{User: "ubuntu", Host: "5.6.7.8", Port: "22"}, Ports: []int{3000, 4000}, Lease: "cbx_bbb"},
	}
	allocPort := 60000
	opts := pondConnectOptions{
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
	summary, err := preparePondMeshSummary("alpha", members, opts)
	if err != nil {
		t.Fatalf("preparePondMeshSummary: %v", err)
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
	if !strings.Contains(string(envBody), "CRABBOX_POND_WEB_8080") {
		t.Fatalf("env file missing CRABBOX_POND_WEB_8080: %s", envBody)
	}
	wantHostsPath := filepath.Join(tmp, pondMeshHostsRoot, "alpha", pondMeshHostsFileName)
	if summary.HostsPath != wantHostsPath {
		t.Fatalf("HostsPath=%q want %q", summary.HostsPath, wantHostsPath)
	}
}

func TestPreparePondMeshSummaryEmpty(t *testing.T) {
	opts := pondConnectOptions{Stdout: io.Discard, Stderr: io.Discard, HomeDir: t.TempDir()}
	summary, err := preparePondMeshSummary("alpha", nil, opts)
	if err != nil {
		t.Fatalf("preparePondMeshSummary empty: %v", err)
	}
	if len(summary.Forwards) != 0 {
		t.Fatalf("expected zero forwards from empty input; got %d", len(summary.Forwards))
	}
}

func TestPreparePondMeshSummarySkipsMembersWithoutPorts(t *testing.T) {
	tmp := t.TempDir()
	members := []pondMember{
		{Name: "no-ports", SSH: SSHTarget{User: "ubuntu", Host: "1.2.3.4", Port: "22"}},
		{Name: "web", SSH: SSHTarget{User: "ubuntu", Host: "5.6.7.8", Port: "22"}, Ports: []int{8080}},
	}
	allocPort := 60500
	opts := pondConnectOptions{
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
	summary, err := preparePondMeshSummary("alpha", members, opts)
	if err != nil {
		t.Fatalf("preparePondMeshSummary: %v", err)
	}
	if len(summary.Forwards) != 1 || summary.Forwards[0].Peer != "web" {
		t.Fatalf("expected only one forward for web; got %#v", summary.Forwards)
	}
}

func TestPondMeshSSHArgsBuildsLocalForward(t *testing.T) {
	target := SSHTarget{User: "ubuntu", Host: "lease.example", Port: "22", Key: "/tmp/test-key"}
	fwd := pondMeshForward{Peer: "web", RemotePort: 8080, LocalPort: 51900, LeaseID: "cbx_x"}
	args := pondMeshSSHArgsForForward(target, fwd)
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
	if !strings.Contains(joined, "ExitOnForwardFailure=yes") {
		t.Fatalf("args missing ExitOnForwardFailure option: %v", args)
	}
	if !strings.Contains(joined, "ControlMaster=auto") && !strings.Contains(joined, "ControlMaster=no") {
		t.Fatalf("args missing ControlMaster option: %v", args)
	}
}

func TestRunPondMeshForwardsLaunchesPerForwardAndTearsDown(t *testing.T) {
	runner := &pondMeshRecordingRunner{}
	members := []pondMember{
		{Name: "web", Lease: "cbx_web", SSH: SSHTarget{User: "ubuntu", Host: "lease-web.example", Port: "22"}, Ports: []int{8080}},
		{Name: "worker", Lease: "cbx_worker", SSH: SSHTarget{User: "ubuntu", Host: "lease-worker.example", Port: "22"}, Ports: []int{3000, 4000}},
	}
	forwards := []pondMeshForward{
		{Peer: "web", RemotePort: 8080, LocalPort: 60000, LeaseID: "cbx_web"},
		{Peer: "worker", RemotePort: 3000, LocalPort: 60001, LeaseID: "cbx_worker"},
		{Peer: "worker", RemotePort: 4000, LocalPort: 60002, LeaseID: "cbx_worker"},
	}
	summary := pondMeshSummary{Forwards: forwards}
	opts := pondConnectOptions{Stdout: io.Discard, Stderr: io.Discard, Runner: runner}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- runPondMeshForwards(ctx, opts, members, summary) }()
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
		t.Fatalf("runPondMeshForwards did not return after context cancel")
	case err := <-errCh:
		if err != nil {
			t.Fatalf("runPondMeshForwards: %v", err)
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
	gotTargetsBySpec := map[string]string{}
	for _, call := range runner.calls {
		if call[0] != "ssh" {
			t.Fatalf("expected ssh invocation, got %v", call)
		}
		for i, arg := range call {
			if arg == "-L" && i+1 < len(call) {
				gotSpecs = append(gotSpecs, call[i+1])
				gotTargetsBySpec[call[i+1]] = call[len(call)-1]
			}
		}
	}
	sort.Strings(wantSpecs)
	sort.Strings(gotSpecs)
	if !reflect.DeepEqual(wantSpecs, gotSpecs) {
		t.Fatalf("forward specs=%v want %v", gotSpecs, wantSpecs)
	}
	if gotTargetsBySpec["127.0.0.1:60000:127.0.0.1:8080"] != "ubuntu@lease-web.example" {
		t.Fatalf("web forward target=%q", gotTargetsBySpec["127.0.0.1:60000:127.0.0.1:8080"])
	}
	if gotTargetsBySpec["127.0.0.1:60001:127.0.0.1:3000"] != "ubuntu@lease-worker.example" {
		t.Fatalf("worker forward target=%q", gotTargetsBySpec["127.0.0.1:60001:127.0.0.1:3000"])
	}
}

func TestIsPondMeshDaemonCommandRequiresForwardSpec(t *testing.T) {
	fwd := pondMeshForward{LocalPort: 51820, RemotePort: 8080}
	command := "ssh -N -o ExitOnForwardFailure=yes -L 127.0.0.1:51820:127.0.0.1:8080 ubuntu@example"
	if !isPondMeshDaemonCommand(command, fwd) {
		t.Fatalf("expected pond ssh tunnel command to match")
	}
	if isPondMeshDaemonCommand("sleep 600", fwd) {
		t.Fatalf("sleep command must not match")
	}
	if isPondMeshDaemonCommand("ssh -N -L 127.0.0.1:51821:127.0.0.1:8080 ubuntu@example", fwd) {
		t.Fatalf("ssh command for a different local port must not match")
	}
}

func TestPondMeshDaemonSupported(t *testing.T) {
	if pondMeshDaemonSupported("windows") {
		t.Fatalf("Windows export daemons should stay disabled until command validation is platform-aware")
	}
	if !pondMeshDaemonSupported("linux") || !pondMeshDaemonSupported("darwin") {
		t.Fatalf("Unix-like operator hosts should support export daemons")
	}
}

func TestStopPondMeshDaemonStateDropsNonMatchingPID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ps command validation is Unix-only in this test")
	}
	home := t.TempDir()
	path, err := pondMeshDaemonStatePath(home, "alpha", true)
	if err != nil {
		t.Fatal(err)
	}
	state := pondMeshDaemonState{
		Pond: "alpha",
		Processes: []pondMeshDaemonProcess{{
			PID:     os.Getpid(),
			Command: "sleep 600",
			Forward: pondMeshForward{
				Peer:       "web",
				LocalPort:  51820,
				RemotePort: 8080,
				LeaseID:    "cbx_web",
			},
		}},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	stopped, err := stopPondMeshDaemonState(home, "alpha")
	if err != nil {
		t.Fatalf("stopPondMeshDaemonState: %v", err)
	}
	if stopped != 0 {
		t.Fatalf("stopped=%d want 0", stopped)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("daemon state should be removed, stat err=%v", err)
	}
}

func TestPondSSHTargetsByLeaseAllowsDuplicatePeerNames(t *testing.T) {
	members := []pondMember{
		{Name: "web", Lease: "cbx_aws", SSH: SSHTarget{User: "ubuntu", Host: "aws.example", Port: "22"}},
		{Name: "web", Lease: "cbx_gcp", SSH: SSHTarget{User: "ubuntu", Host: "gcp.example", Port: "22"}},
	}
	targets := pondSSHTargetsByLease(members)
	if targets["cbx_aws"].Host != "aws.example" {
		t.Fatalf("cbx_aws target=%#v", targets["cbx_aws"])
	}
	if targets["cbx_gcp"].Host != "gcp.example" {
		t.Fatalf("cbx_gcp target=%#v", targets["cbx_gcp"])
	}
}

type pondMeshResolveRecordingBackend struct {
	ids          []string
	afterResolve func(LeaseTarget)
}

func (b *pondMeshResolveRecordingBackend) Spec() ProviderSpec { return ProviderSpec{Name: "hetzner"} }
func (b *pondMeshResolveRecordingBackend) Acquire(context.Context, AcquireRequest) (LeaseTarget, error) {
	return LeaseTarget{}, nil
}
func (b *pondMeshResolveRecordingBackend) Resolve(_ context.Context, req ResolveRequest) (LeaseTarget, error) {
	b.ids = append(b.ids, req.ID)
	lease := LeaseTarget{
		LeaseID: req.ID,
		Server: Server{
			CloudID:  req.ID,
			Provider: "hetzner",
			Name:     req.ID,
			Labels:   map[string]string{"provider": "hetzner", "lease": req.ID, "slug": req.ID, "state": "ready"},
		},
		SSH: SSHTarget{User: "ubuntu", Host: req.ID + ".example", Port: "22"},
	}
	if b.afterResolve != nil {
		b.afterResolve(lease)
	}
	return lease, nil
}
func (b *pondMeshResolveRecordingBackend) List(context.Context, ListRequest) ([]LeaseView, error) {
	return nil, nil
}
func (b *pondMeshResolveRecordingBackend) ReleaseLease(context.Context, ReleaseLeaseRequest) error {
	return nil
}
func (b *pondMeshResolveRecordingBackend) Touch(context.Context, TouchRequest) (Server, error) {
	return Server{}, nil
}

func TestCollectPondMembersResolvesByLeaseIDBeforeSlug(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	backend := &pondMeshResolveRecordingBackend{}
	servers := []Server{
		{Name: "server-a", Labels: map[string]string{pondLabelKey: "alpha", "slug": "web", "lease": "cbx_web_a", pondExposedPortsLabelKey: "8080"}},
		{Name: "server-b", Labels: map[string]string{pondLabelKey: "alpha", "slug": "web", "lease": "cbx_web_b", pondExposedPortsLabelKey: "9090"}},
	}
	for _, leaseID := range []string{"cbx_web_a", "cbx_web_b"} {
		if err := claimLeaseForRepoProvider(leaseID, leaseID, "hetzner", t.TempDir(), time.Hour, false); err != nil {
			t.Fatal(err)
		}
	}
	members, err := collectPondMembers(context.Background(), backend, Config{}, servers, "alpha")
	if err != nil {
		t.Fatalf("collectPondMembers: %v", err)
	}
	if !reflect.DeepEqual(backend.ids, []string{"cbx_web_a", "cbx_web_b"}) {
		t.Fatalf("resolve IDs=%v, want lease IDs before duplicate slug", backend.ids)
	}
	if members[0].Lease != "cbx_web_a" || members[1].Lease != "cbx_web_b" {
		t.Fatalf("members=%#v", members)
	}
	for _, leaseID := range []string{"cbx_web_a", "cbx_web_b"} {
		claim, ok, err := resolveLeaseClaimForProvider(leaseID, "hetzner")
		if err != nil || !ok || claim.SSHHost != leaseID+".example" || claim.SSHPort != 22 {
			t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
		}
	}
}

func TestCollectPondMembersRefreshesRetainedStoppedClaim(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	leaseID := "cbx_web_retained"
	server := Server{
		CloudID:  "server-web",
		Provider: "hetzner",
		Name:     "server-web",
		Labels: map[string]string{
			"provider":               "hetzner",
			"lease":                  leaseID,
			"slug":                   "web",
			"state":                  "stopped",
			pondLabelKey:             "alpha",
			pondExposedPortsLabelKey: "8080",
		},
	}
	if err := claimLeaseTargetForRepoConfig(leaseID, "web", Config{Provider: "hetzner"}, server, SSHTarget{}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	members, err := collectPondMembers(context.Background(), &pondMeshResolveRecordingBackend{}, Config{}, []Server{server}, "alpha")
	if err != nil {
		t.Fatalf("collectPondMembers: %v", err)
	}
	if len(members) != 1 || members[0].SSH.Host != leaseID+".example" {
		t.Fatalf("members=%#v", members)
	}
	claim, ok, err := resolveLeaseClaimForProvider(leaseID, "hetzner")
	if err != nil || !ok || claim.Labels["state"] != "ready" || claim.SSHHost != leaseID+".example" || claim.SSHPort != 22 {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
	}
}

func TestCollectPondMembersDoesNotRestoreStoppedClaim(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	leaseID := "cbx_web_stopped"
	labels := map[string]string{
		"provider":               "hetzner",
		"lease":                  leaseID,
		"slug":                   "web",
		"state":                  "running",
		pondLabelKey:             "alpha",
		pondExposedPortsLabelKey: "8080",
	}
	server := Server{CloudID: "server-web", Provider: "hetzner", Name: "server-web", Labels: labels}
	if err := claimLeaseTargetForRepoConfig(leaseID, "web", Config{Provider: "hetzner"}, server, SSHTarget{Host: "old.example", Port: "22"}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	var stopErr error
	backend := &pondMeshResolveRecordingBackend{afterResolve: func(LeaseTarget) {
		claim, ok, err := resolveLeaseClaimForProvider(leaseID, "hetzner")
		if err != nil || !ok {
			stopErr = fmt.Errorf("resolve claim: ok=%t err=%v", ok, err)
			return
		}
		stopped := server
		stopped.Labels = cloneStringMap(server.Labels)
		stopped.Labels["state"] = "stopped"
		_, stopErr = updateLeaseClaimEndpointIfUnchanged(leaseID, claim, stopped, SSHTarget{})
	}}
	_, err := collectPondMembers(context.Background(), backend, Config{}, []Server{server}, "alpha")
	if stopErr != nil {
		t.Fatal(stopErr)
	}
	if err == nil || !strings.Contains(err.Error(), "became inactive during resolve") {
		t.Fatalf("collectPondMembers err=%v", err)
	}
	claim, ok, err := resolveLeaseClaimForProvider(leaseID, "hetzner")
	if err != nil || !ok || claim.Labels["state"] != "stopped" || claim.SSHHost != "" || claim.SSHPort != 0 {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
	}
}

func TestCollectPondMembersDoesNotPreparePortlessMembers(t *testing.T) {
	backend := &pondMeshResolveRecordingBackend{}
	servers := []Server{
		{Name: "client", Labels: map[string]string{pondLabelKey: "alpha", "slug": "client", "lease": "cbx_client"}},
		{Name: "web", Labels: map[string]string{pondLabelKey: "alpha", "slug": "web", "lease": "cbx_web", pondExposedPortsLabelKey: "8080"}},
	}
	members, err := collectPondMembers(context.Background(), backend, Config{}, servers, "alpha")
	if err != nil {
		t.Fatalf("collectPondMembers: %v", err)
	}
	if !reflect.DeepEqual(backend.ids, []string{"cbx_web"}) {
		t.Fatalf("resolve IDs=%v, want only exposed member", backend.ids)
	}
	if len(members) != 2 || members[0].Name != "client" || members[0].Lease != "cbx_client" || len(members[0].Ports) != 0 || members[0].SSH.Host != "" {
		t.Fatalf("members=%#v", members)
	}
}

func TestEnvSafeName(t *testing.T) {
	cases := map[string]string{
		"web":         "WEB",
		"client-foo":  "CLIENT_FOO",
		"a.b/c":       "A_B_C",
		"  ":          "_",
		"---":         "_",
		"123-name":    "123_NAME",
		"pond/peer-1": "POND_PEER_1",
	}
	for in, want := range cases {
		if got := envSafeName(in); got != want {
			t.Fatalf("envSafeName(%q)=%q want %q", in, got, want)
		}
	}
}

func TestRenderPondMeshHostsFileIncludesAllPeers(t *testing.T) {
	body := renderPondMeshHostsFile([]pondMeshForward{
		{Peer: "web", RemotePort: 8080, LocalPort: 51820},
		{Peer: "worker", RemotePort: 3000, LocalPort: 51821},
	})
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "127.0.0.1:") {
			t.Fatalf("hosts body must stay /etc/hosts-compatible, got: %s", body)
		}
	}
	if !strings.Contains(body, "web.cbx") || !strings.Contains(body, "worker.cbx") {
		t.Fatalf("hosts body missing peer names: %s", body)
	}
	if !strings.Contains(body, "local=127.0.0.1:51820") || !strings.Contains(body, "local=127.0.0.1:51821") {
		t.Fatalf("hosts body missing local port comments: %s", body)
	}
}

func TestRenderPondMeshEnvFileEmitsStableExports(t *testing.T) {
	body, exports := renderPondMeshEnvFile([]pondMeshForward{
		{Peer: "web", RemotePort: 8080, LocalPort: 51820},
		{Peer: "worker", RemotePort: 3000, LocalPort: 51821},
	})
	if len(exports) != 2 {
		t.Fatalf("exports=%d want 2", len(exports))
	}
	if !strings.Contains(exports[0], "export CRABBOX_POND_WEB_8080=127.0.0.1:51820") {
		t.Fatalf("unexpected first export: %q", exports[0])
	}
	if !strings.Contains(body, "CRABBOX_POND_WORKER_3000=127.0.0.1:51821") {
		t.Fatalf("env body missing worker export: %s", body)
	}
}

func TestDisambiguatePondMemberNamesAvoidsShellExportCollisions(t *testing.T) {
	members := []pondMember{
		{Name: "web", Provider: "aws", Lease: "cbx_aws123456"},
		{Name: "web", Provider: "gcp", Lease: "cbx_gcp123456"},
		{Name: "web-aws", Provider: "hetzner", Lease: "cbx_hz123456"},
		{Name: "api-a", Provider: "runpod", Lease: "cbx_rp123456"},
		{Name: "api_a", Provider: "proxmox", Lease: "cbx_px123456"},
	}
	got := disambiguatePondMemberNames(members)
	seen := map[string]bool{}
	for _, member := range got {
		key := envSafeName(member.Name)
		if seen[key] {
			t.Fatalf("duplicate env-safe name %q in %#v", key, got)
		}
		seen[key] = true
	}
	if got[0].Name != "web-aws" {
		t.Fatalf("first duplicate name=%q want web-aws", got[0].Name)
	}
	if got[1].Name != "web-gcp" {
		t.Fatalf("second duplicate name=%q want web-gcp", got[1].Name)
	}
	if got[2].Name != "web-aws-hetzner" {
		t.Fatalf("preexisting colliding name=%q want web-aws-hetzner", got[2].Name)
	}
	if got[3].Name == got[4].Name || envSafeName(got[3].Name) == envSafeName(got[4].Name) {
		t.Fatalf("slug variants should be shell-safe unique: %#v", got)
	}
}

// TestCollectPondMembersAcrossProvidersFiltersByCapability is the cross-
// provider gating test for the capability refactor. It seeds claims for a
// mix of SSH-mesh-capable (Hetzner, RunPod) and URL-only (Islo, Modal)
// providers in the same pond, then asserts that `collectPondMembersAcrossProviders`:
//
//   - includes Hetzner and RunPod in the iteration (both advertise FeatureSSH);
//   - lands Islo and Modal in the `ineligible` slice (URLBridge-only, no SSH);
//   - and filters out claims that belong to a different pond.
//
// The actual `pondMember` list comes back empty because the test SSH backend's
// List() returns nil — the test is about the capability gate, not the member
// projection.
func TestCollectPondMembersAcrossProvidersFiltersByCapability(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "cbx_hetzner", Slug: "api", Provider: "hetzner", Pond: "alpha", RepoRoot: "/r"},
		{LeaseID: "cbx_runpod", Slug: "edge", Provider: "exe-dev", Pond: "alpha", RepoRoot: "/r"},
		{LeaseID: "isb_modal", Slug: "fn", Provider: "modal", Pond: "alpha", RepoRoot: "/r"},
		{LeaseID: "isb_islo", Slug: "share", Provider: "islo", Pond: "alpha", RepoRoot: "/r"},
		{LeaseID: "cbx_beta", Slug: "noise", Provider: "hetzner", Pond: "beta", RepoRoot: "/r"},
	})
	cfg := defaultConfig()
	_, ineligible, err := collectPondMembersAcrossProviders(context.Background(), Runtime{}, cfg, "alpha", "")
	if err != nil {
		t.Fatalf("collectPondMembersAcrossProviders: %v", err)
	}
	sort.Strings(ineligible)
	want := []string{"islo", "modal"}
	if !reflect.DeepEqual(ineligible, want) {
		t.Fatalf("ineligible = %v, want %v", ineligible, want)
	}
}

// TestCollectPondMembersAcrossProvidersHonorsProviderFilter verifies that
// `--provider X` still narrows the iteration to a single provider, even
// though the function now defaults to cross-provider mode.
func TestCollectPondMembersAcrossProvidersHonorsProviderFilter(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "cbx_hetzner", Slug: "api", Provider: "hetzner", Pond: "alpha", RepoRoot: "/r"},
		{LeaseID: "cbx_runpod", Slug: "edge", Provider: "exe-dev", Pond: "alpha", RepoRoot: "/r"},
	})
	cfg := defaultConfig()
	_, ineligible, err := collectPondMembersAcrossProviders(context.Background(), Runtime{}, cfg, "alpha", "exe-dev")
	if err != nil {
		t.Fatalf("collectPondMembersAcrossProviders: %v", err)
	}
	if len(ineligible) != 0 {
		t.Fatalf("expected no ineligible when filter excludes other providers, got %v", ineligible)
	}
}

// TestProviderCapabilitiesPrimary asserts the Primary()-pick stays deterministic
// across the capability set. Hetzner has both Tailscale and SSH; Primary must
// be Tailscale (the preferred peer plane). Islo has only URLBridge; Primary
// must be URL. A pure-SSH provider like RunPod must Primary to SSH. A
// no-capability provider returns TransportNone.
func TestProviderCapabilitiesPrimary(t *testing.T) {
	cases := []struct {
		provider string
		want     string
	}{
		{"hetzner", TransportTailnet},
		{"azure", TransportTailnet},
		{"gcp", TransportTailnet},
		{"aws", TransportSSH},     // FeatureSSH only; no FeatureTailscale yet
		{"proxmox", TransportSSH}, // legacy mapping was TransportTailnet — capability model corrects to SSH
		{"exe-dev", TransportSSH},
		{"daytona", TransportSSH},
		{"islo", TransportURL}, // outbound-only userspace Tailscale is not a dialable peer plane
		{"modal", TransportNone},
		{"cloudflare", TransportNone},
		{"blacksmith-testbox", TransportNone},
		{"unknown-provider", TransportNone},
	}
	for _, tc := range cases {
		if got := providerCapabilities(tc.provider).Primary(); got != tc.want {
			t.Errorf("providerCapabilities(%q).Primary() = %q, want %q", tc.provider, got, tc.want)
		}
	}
}

// TestProviderCapabilitiesAvailable asserts that providers expose ALL viable
// transports via Available(), not just the primary. The Hetzner case is the
// load-bearing one for the "SSH-mesh on Hetzner" change: Hetzner reports both
// tailnet AND ssh, so `pond connect` finds it eligible regardless of which
// is recommended.
func TestProviderCapabilitiesAvailable(t *testing.T) {
	cases := []struct {
		provider string
		want     []string
	}{
		{"hetzner", []string{TransportTailnet, TransportSSH}},
		{"azure", []string{TransportTailnet, TransportSSH}},
		{"gcp", []string{TransportTailnet, TransportSSH}},
		{"aws", []string{TransportSSH}},
		{"exe-dev", []string{TransportSSH}},
		{"islo", []string{TransportURL}},
		{"modal", nil},
		{"cloudflare", nil},
		{"blacksmith-testbox", nil},
	}
	for _, tc := range cases {
		got := providerCapabilities(tc.provider).Available()
		if len(got) == 0 {
			got = nil
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("providerCapabilities(%q).Available() = %v, want %v", tc.provider, got, tc.want)
		}
	}
}
