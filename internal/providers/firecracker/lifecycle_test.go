package firecracker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type fakeMachine struct {
	pid      int
	guestIP  string
	startErr error
	startCtx context.Context
	stopErr  error
	started  int
	stopped  int
}

func (m *fakeMachine) Start(ctx context.Context) error {
	m.started++
	m.startCtx = ctx
	return m.startErr
}

func (m *fakeMachine) StopVMM() error {
	m.stopped++
	return m.stopErr
}

func (m *fakeMachine) PID() int        { return m.pid }
func (m *fakeMachine) GuestIP() string { return m.guestIP }

type fakeMachineFactory struct {
	machine *fakeMachine
	err     error
	launch  []machineLaunchConfig
}

func (f *fakeMachineFactory) New(_ context.Context, launch machineLaunchConfig) (machine, error) {
	f.launch = append(f.launch, launch)
	if f.err != nil {
		return nil, f.err
	}
	return f.machine, nil
}

type fakeProcessManager struct {
	identities map[int]processIdentity
	alive      map[int]bool
	signals    []syscall.Signal
	signalErr  error
}

func (f *fakeProcessManager) Capture(pid int) (processIdentity, error) {
	if f.identities == nil {
		f.identities = map[int]processIdentity{}
	}
	if f.alive == nil {
		f.alive = map[int]bool{}
	}
	identity, ok := f.identities[pid]
	if !ok {
		identity = processIdentity{PID: pid, Started: fmt.Sprintf("start-%d", pid), BootID: "boot-test"}
		f.identities[pid] = identity
	}
	f.alive[pid] = true
	return identity, nil
}

func (f *fakeProcessManager) Matches(identity processIdentity) bool {
	current, ok := f.identities[identity.PID]
	return ok && f.alive[identity.PID] && current == identity
}

func (f *fakeProcessManager) Signal(identity processIdentity, sig syscall.Signal) error {
	f.signals = append(f.signals, sig)
	if f.signalErr != nil {
		return f.signalErr
	}
	if f.alive == nil {
		f.alive = map[int]bool{}
	}
	f.alive[identity.PID] = false
	return nil
}

type lifecycleTestBackend struct {
	backend      *backend
	factory      *fakeMachineFactory
	processes    *fakeProcessManager
	stdout       *bytes.Buffer
	stderr       *bytes.Buffer
	stateRoot    string
	repoRoot     string
	cleanupCalls *int
}

func newLifecycleTestBackend(t *testing.T, cfg core.Config) lifecycleTestBackend {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	oldHost := firecrackerHostGOOS
	oldEnsureKey := ensureTestboxKey
	oldRemoveKey := removeTestboxKey
	firecrackerHostGOOS = "linux"
	ensureTestboxKey = func(_ Config, leaseID string) (string, string, error) {
		path, err := core.TestboxKeyPath(leaseID)
		if err != nil {
			return "", "", err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return "", "", err
		}
		if err := os.WriteFile(path, []byte("PRIVATE KEY"), 0o600); err != nil {
			return "", "", err
		}
		return path, "ssh-ed25519 AAAATEST firecracker-test", nil
	}
	removeTestboxKey = func(leaseID string) {
		path, err := core.TestboxKeyPath(leaseID)
		if err == nil {
			_ = os.RemoveAll(filepath.Dir(path))
		}
	}
	t.Cleanup(func() {
		firecrackerHostGOOS = oldHost
		ensureTestboxKey = oldEnsureKey
		removeTestboxKey = oldRemoveKey
	})

	stateRoot := t.TempDir()
	repoRoot := t.TempDir()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cleanupCalls := 0
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: stdout, Stderr: stderr}).(*backend)
	factory := &fakeMachineFactory{machine: &fakeMachine{pid: 4242, guestIP: "192.0.2.10"}}
	processes := &fakeProcessManager{
		identities: map[int]processIdentity{4242: {PID: 4242, Started: "start-4242", BootID: "boot-test"}},
		alive:      map[int]bool{},
	}
	b.stateRoot = func() (string, error) { return stateRoot, nil }
	b.machines = factory
	b.processes = processes
	b.waitForSSH = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }
	b.cleanupNetwork = func(context.Context, leaseStateRecord) error {
		// remove generated runtime markers so retained-artifact tests stay honest
		// without needing a real CNI stack.
		cleanupCalls++
		return nil
	}
	return lifecycleTestBackend{
		backend:      b,
		factory:      factory,
		processes:    processes,
		stdout:       stdout,
		stderr:       stderr,
		stateRoot:    stateRoot,
		repoRoot:     repoRoot,
		cleanupCalls: &cleanupCalls,
	}
}

func lifecycleConfig(t *testing.T) core.Config {
	t.Helper()
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Firecracker.Binary = "/usr/local/bin/firecracker"
	kernel := filepath.Join(t.TempDir(), "vmlinux")
	rootfs := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(kernel, []byte("kernel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootfs, []byte("rootfs"), 0o600); err != nil {
		t.Fatal(err)
	}
	cniConfDir := filepath.Join(t.TempDir(), "cni-conf")
	cniBinDir := filepath.Join(t.TempDir(), "cni-bin")
	if err := os.MkdirAll(cniConfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cniBinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg.Firecracker.Kernel = kernel
	cfg.Firecracker.RootFS = rootfs
	cfg.Firecracker.User = "runner"
	cfg.Firecracker.WorkRoot = "/work/crabbox"
	cfg.Firecracker.CPUs = 2
	cfg.Firecracker.MemoryMiB = 512
	cfg.Firecracker.DiskMiB = 4
	cfg.Firecracker.Network = "cni"
	cfg.Firecracker.CNINetwork = "test-firecracker"
	cfg.Firecracker.CNIConfDir = cniConfDir
	cfg.Firecracker.CNIBinDir = cniBinDir
	cfg.Firecracker.LaunchTimeout = 5 * time.Second
	cfg.Firecracker.DeleteOnRelease = true
	applyDefaults(&cfg)
	return cfg
}

func TestAcquireResolveListAndReleaseLifecycle(t *testing.T) {
	test := newLifecycleTestBackend(t, lifecycleConfig(t))

	lease, err := test.backend.Acquire(context.Background(), core.AcquireRequest{
		Repo: core.Repo{Root: test.repoRoot},
	})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if lease.LeaseID == "" || lease.SSH.Host != "192.0.2.10" || lease.SSH.Port != firecrackerSSHPort {
		t.Fatalf("lease=%#v", lease)
	}
	if got := test.factory.launch[0].CloudInitPath; got == "" {
		t.Fatalf("cloud-init path missing in machine launch: %#v", test.factory.launch[0])
	}
	if info, err := os.Stat(test.factory.launch[0].RootFSPath); err != nil {
		t.Fatalf("stat rootfs copy: %v", err)
	} else if info.Size() < 4*1024*1024 {
		t.Fatalf("rootfs size=%d want at least 4MiB", info.Size())
	}
	if _, err := os.Stat(test.factory.launch[0].CloudInitPath); err != nil {
		t.Fatalf("stat cloud-init drive: %v", err)
	}

	record, err := test.backend.readStateRecord(lease.LeaseID)
	if err != nil {
		t.Fatalf("readStateRecord: %v", err)
	}
	if record.GuestIP != "192.0.2.10" || record.PID != 4242 || record.Labels["state"] != "ready" {
		t.Fatalf("record=%#v", record)
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider(lease.LeaseID, providerName)
	if err != nil || !ok {
		t.Fatalf("ResolveLeaseClaimForProvider ok=%v err=%v", ok, err)
	}
	if claim.SSHHost != "192.0.2.10" {
		t.Fatalf("claim=%#v", claim)
	}

	resolved, err := test.backend.Resolve(context.Background(), core.ResolveRequest{ID: lease.LeaseID})
	if err != nil {
		t.Fatalf("Resolve by lease id: %v", err)
	}
	if resolved.SSH.Key == "" || resolved.SSH.Host != lease.SSH.Host {
		t.Fatalf("resolved=%#v", resolved)
	}
	resolved, err = test.backend.Resolve(context.Background(), core.ResolveRequest{ID: lease.Server.Labels["slug"]})
	if err != nil {
		t.Fatalf("Resolve by slug: %v", err)
	}
	if resolved.LeaseID != lease.LeaseID {
		t.Fatalf("resolved lease=%q want %q", resolved.LeaseID, lease.LeaseID)
	}

	views, err := test.backend.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(views) != 1 || views[0].Status != "ready" {
		t.Fatalf("views=%#v", views)
	}

	if err := test.backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}
	if len(test.processes.signals) != 1 || test.processes.signals[0] != syscall.SIGTERM {
		t.Fatalf("signals=%v", test.processes.signals)
	}
	if _, err := os.Stat(filepath.Join(test.stateRoot, firecrackerLeasesDirName, lease.LeaseID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lease state directory still exists: err=%v", err)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider(lease.LeaseID, providerName); err != nil || ok {
		t.Fatalf("stale claim ok=%v err=%v", ok, err)
	}
	keyPath, err := core.TestboxKeyPath(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("key path still exists: err=%v", err)
	}
}

func TestAcquirePassesLaunchTimeoutToMachineStart(t *testing.T) {
	cfg := lifecycleConfig(t)
	cfg.Firecracker.LaunchTimeout = 3 * time.Second
	test := newLifecycleTestBackend(t, cfg)

	if _, err := test.backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: test.repoRoot}}); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if test.factory.machine.startCtx == nil {
		t.Fatal("machine start context was not recorded")
	}
	deadline, ok := test.factory.machine.startCtx.Deadline()
	if !ok {
		t.Fatal("machine start context has no deadline")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > cfg.Firecracker.LaunchTimeout {
		t.Fatalf("machine start deadline remaining=%s want within %s", remaining, cfg.Firecracker.LaunchTimeout)
	}
}

func TestAcquireRecordsEffectiveTopLevelSSHUser(t *testing.T) {
	cfg := lifecycleConfig(t)
	cfg.SSHUser = "alice"
	cfg.Firecracker.User = core.BaseConfig().Firecracker.User
	test := newLifecycleTestBackend(t, cfg)

	lease, err := test.backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: test.repoRoot}})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if lease.SSH.User != "alice" {
		t.Fatalf("lease ssh user=%q want alice", lease.SSH.User)
	}
	record, err := test.backend.readStateRecord(lease.LeaseID)
	if err != nil {
		t.Fatalf("readStateRecord: %v", err)
	}
	if record.SSHUser != "alice" || record.Labels["ssh_user"] != "alice" {
		t.Fatalf("record ssh user=%q labels=%#v want alice", record.SSHUser, record.Labels)
	}
	resolved, err := test.backend.Resolve(context.Background(), core.ResolveRequest{ID: lease.LeaseID})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.SSH.User != "alice" {
		t.Fatalf("resolved ssh user=%q want alice", resolved.SSH.User)
	}
}

func TestAcquireRollbackRemovesStateOnSSHFailure(t *testing.T) {
	test := newLifecycleTestBackend(t, lifecycleConfig(t))
	test.backend.waitForSSH = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
		return errors.New("ssh not ready")
	}

	_, err := test.backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: test.repoRoot}})
	if err == nil || !strings.Contains(err.Error(), "ssh not ready") {
		t.Fatalf("Acquire err=%v", err)
	}
	if test.factory.machine.stopped != 1 {
		t.Fatalf("fake machine stop count=%d want 1", test.factory.machine.stopped)
	}
	if *test.cleanupCalls != 1 {
		t.Fatalf("cleanup calls=%d want 1", *test.cleanupCalls)
	}
	entries, err := os.ReadDir(filepath.Join(test.stateRoot, firecrackerLeasesDirName))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read state dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries=%v want none", entries)
	}
}

func TestReleaseRetainsArtifactsWhenDeleteOnReleaseDisabled(t *testing.T) {
	cfg := lifecycleConfig(t)
	cfg.Firecracker.DeleteOnRelease = false
	test := newLifecycleTestBackend(t, cfg)

	lease, err := test.backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: test.repoRoot}})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := test.backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}

	record, err := test.backend.readStateRecord(lease.LeaseID)
	if err != nil {
		t.Fatalf("readStateRecord: %v", err)
	}
	if record.PID != 0 || record.ProcessStarted != "" || record.Labels["state"] != "released" {
		t.Fatalf("record=%#v", record)
	}
	for _, path := range []string{record.RootFSPath, record.CloudInitPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("retained artifact %s err=%v", path, err)
		}
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider(lease.LeaseID, providerName); err != nil || ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
}

func TestCleanupDryRunReportsStateAndClaimWithoutMutating(t *testing.T) {
	test := newLifecycleTestBackend(t, lifecycleConfig(t))

	lease, err := test.backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: test.repoRoot}})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	test.processes.alive[4242] = false

	orphanServer := Server{
		CloudID:  "orphan-vmid",
		Provider: providerName,
		Name:     "orphan-vmid",
		Labels: map[string]string{
			"lease": lease.LeaseID + "-orphan",
			"slug":  "orphan-slug",
			"state": "ready",
		},
	}
	if err := core.ClaimLeaseTargetForConfig(lease.LeaseID+"-orphan", "orphan-slug", lifecycleConfig(t), orphanServer, SSHTarget{}, time.Minute); err != nil {
		t.Fatalf("ClaimLeaseTargetForConfig: %v", err)
	}

	if err := test.backend.Cleanup(context.Background(), core.CleanupRequest{DryRun: true}); err != nil {
		t.Fatalf("Cleanup dry-run: %v", err)
	}
	output := test.stdout.String()
	for _, want := range []string{
		"would remove firecracker lease=" + lease.LeaseID,
		"would remove firecracker claim lease=" + lease.LeaseID + "-orphan",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("cleanup output missing %q:\n%s", want, output)
		}
	}
	if _, err := test.backend.readStateRecord(lease.LeaseID); err != nil {
		t.Fatalf("state mutated unexpectedly: %v", err)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider(lease.LeaseID+"-orphan", providerName); err != nil || !ok {
		t.Fatalf("orphan claim ok=%v err=%v", ok, err)
	}
}
