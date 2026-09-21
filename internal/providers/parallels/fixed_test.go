package parallels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const fixedTestLeaseID = "cbx_0123456789ab"

// parallelsFixedRunner is an in-memory prlctl fleet. It keeps VM identity
// (host-unique name plus an immutable UUID) so replay, adoption, and deletion
// are exercised against the real ParallelsClient command surface.
type parallelsFixedRunner struct {
	mu          sync.Mutex
	vms         []core.ParallelsVM
	nextUUID    int
	cloneCalls  []string
	deleteCalls []string
	startCalls  []string
	execCalls   []string
	// cloneErr simulates a failed or lost clone reply. cloneCommits keeps the
	// VM anyway, which is the lost-reply case.
	cloneErr    error
	cloneCommit bool
	listErr     error
	// listAllErr fails only the complete `prlctl list -a` inventory read, so a
	// test can break reconciliation while `list -i` lookups still answer.
	listAllErr    error
	snapshotErr   error
	snapshotsJSON string
	deleteErr     error
	// beforeClone observes durable state at the moment prlctl clone is invoked.
	beforeClone func()
	// afterClone runs once the clone has committed, so a test can break the
	// reconciliation that follows a successful create. It is called with the
	// runner lock already held and must not take it again.
	afterClone func()
	// serverID and hardwareID are the machine's attested Parallels service
	// identity, which a fixed lease's provider scope is derived from.
	serverID          string
	hardwareID        string
	vmHome            string
	serverIdentityErr error
	hostDirCalls      []string
	hostDirErr        error
	cloneDst          string
}

func newParallelsFixedRunner(source string) *parallelsFixedRunner {
	return &parallelsFixedRunner{
		vms:        []core.ParallelsVM{{ID: "{source-uuid}", Name: source, State: "stopped"}},
		serverID:   "local-server-id",
		hardwareID: "local-hardware-id",
		vmHome:     "/vms",
	}
}

// seedSource registers an additional template VM the fleet can clone from.
func (r *parallelsFixedRunner) seedSource(name string) core.ParallelsVM {
	r.mu.Lock()
	defer r.mu.Unlock()
	vm := core.ParallelsVM{ID: "{" + name + "-uuid}", Name: name, State: "stopped"}
	r.vms = append(r.vms, vm)
	return vm
}

func (r *parallelsFixedRunner) seed(name string) core.ParallelsVM {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.addLocked(name)
}

func (r *parallelsFixedRunner) addLocked(name string) core.ParallelsVM {
	return r.addAtLocked(name, r.vmHome)
}

// addAtLocked registers a VM whose bundle lives under dir, mirroring how
// prlctl places a clone under its --dst directory.
func (r *parallelsFixedRunner) addAtLocked(name, dir string) core.ParallelsVM {
	r.nextUUID++
	vm := core.ParallelsVM{
		ID:    fmt.Sprintf("{vm-uuid-%d}", r.nextUUID),
		Name:  name,
		State: "stopped",
		IP:    "10.211.55.9",
		Home:  strings.TrimRight(dir, "/") + "/" + name + ".pvm/",
	}
	r.vms = append(r.vms, vm)
	return vm
}

// seedAt registers an unrelated VM at a name, outside any attempt directory.
func (r *parallelsFixedRunner) seedAt(name string) core.ParallelsVM {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.addAtLocked(name, r.vmHome)
}

func (r *parallelsFixedRunner) find(name string) (core.ParallelsVM, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.findLocked(name)
}

func (r *parallelsFixedRunner) findLocked(handle string) (core.ParallelsVM, bool) {
	for _, vm := range r.vms {
		if vm.Name == handle || vm.ID == handle {
			return vm, true
		}
	}
	return core.ParallelsVM{}, false
}

// rename moves a VM to a different name while keeping its UUID, which is what
// an out-of-band `prlctl set --name` does to an acquired lease VM.
func (r *parallelsFixedRunner) rename(from, to string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.vms {
		if r.vms[i].Name == from {
			r.vms[i].Name = to
		}
	}
}

// remove deletes a VM out of band, as another management operation would.
func (r *parallelsFixedRunner) remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := r.vms[:0]
	for _, vm := range r.vms {
		if vm.ID != id {
			kept = append(kept, vm)
		}
	}
	r.vms = kept
}

func (r *parallelsFixedRunner) replaceUUID(name, uuid string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.vms {
		if r.vms[i].Name == name {
			r.vms[i].ID = uuid
		}
	}
}

func (r *parallelsFixedRunner) counts() (clones, deletes int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.cloneCalls), len(r.deleteCalls)
}

// encodeParallelsVMs renders the plain `prlctl list` form, which reports only
// uuid, name, status and address. encodeParallelsVMsDetailed adds the fields
// that only the `-i` info form carries, so a caller that reads a bundle path
// from the plain listing fails here exactly as it does against real prlctl.
func encodeParallelsVMs(vms []core.ParallelsVM) string {
	items := make([]map[string]any, 0, len(vms))
	for _, vm := range vms {
		items = append(items, map[string]any{"ID": vm.ID, "Name": vm.Name, "State": vm.State, "ip_configured": vm.IP})
	}
	data, err := json.Marshal(items)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func encodeParallelsVMsDetailed(vms []core.ParallelsVM) string {
	items := make([]map[string]any, 0, len(vms))
	for _, vm := range vms {
		items = append(items, map[string]any{"ID": vm.ID, "Name": vm.Name, "State": vm.State, "ip_configured": vm.IP, "Home": vm.Home})
	}
	data, err := json.Marshal(items)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func (r *parallelsFixedRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	if req.Name == "id" && slices.Equal(req.Args, []string{"-u"}) {
		return core.LocalCommandResult{Stdout: "501\n"}, nil
	}
	if req.Name == "mkdir" || req.Name == "rmdir" {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.hostDirCalls = append(r.hostDirCalls, req.Name+" "+strings.Join(req.Args, " "))
		if req.Name == "mkdir" && r.hostDirErr != nil {
			return core.LocalCommandResult{}, r.hostDirErr
		}
		return core.LocalCommandResult{}, nil
	}
	if req.Name == "prlsrvctl" {
		if len(req.Args) == 0 || req.Args[0] != "info" {
			return core.LocalCommandResult{}, errors.New("unexpected prlsrvctl command")
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.serverIdentityErr != nil {
			return core.LocalCommandResult{Stderr: r.serverIdentityErr.Error()}, r.serverIdentityErr
		}
		return core.LocalCommandResult{Stdout: fmt.Sprintf(`{"ID":%q,"Hardware Id":%q,"VM home":%q}`, r.serverID, r.hardwareID, r.vmHome)}, nil
	}
	if req.Name != "prlctl" || len(req.Args) == 0 {
		return core.LocalCommandResult{}, errors.New("unexpected command")
	}
	switch req.Args[0] {
	case "list":
		r.mu.Lock()
		listErr := r.listErr
		r.mu.Unlock()
		if listErr != nil {
			return core.LocalCommandResult{Stderr: listErr.Error()}, listErr
		}
		if len(req.Args) > 1 && req.Args[1] == "-i" && !strings.HasPrefix(req.Args[len(req.Args)-1], "-") {
			handle := req.Args[len(req.Args)-1]
			vm, ok := r.find(handle)
			if !ok {
				return core.LocalCommandResult{Stderr: "The virtual machine could not be found."}, errors.New("prlctl list failed")
			}
			return core.LocalCommandResult{Stdout: encodeParallelsVMsDetailed([]core.ParallelsVM{vm})}, nil
		}
		detailed := len(req.Args) > 1 && slices.Contains(req.Args, "-i")
		r.mu.Lock()
		listAllErr := r.listAllErr
		out := encodeParallelsVMs(r.vms)
		if detailed {
			out = encodeParallelsVMsDetailed(r.vms)
		}
		r.mu.Unlock()
		if listAllErr != nil {
			return core.LocalCommandResult{Stderr: listAllErr.Error()}, listAllErr
		}
		return core.LocalCommandResult{Stdout: out}, nil
	case "snapshot-list":
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.snapshotErr != nil {
			return core.LocalCommandResult{}, r.snapshotErr
		}
		return core.LocalCommandResult{Stdout: blankString(r.snapshotsJSON, "{}")}, nil
	case "clone":
		if r.beforeClone != nil {
			r.beforeClone()
		}
		name, dst := "", ""
		for i := 0; i+1 < len(req.Args); i++ {
			switch req.Args[i] {
			case "--name":
				name = req.Args[i+1]
			case "--dst":
				dst = req.Args[i+1]
			}
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if name == "" {
			return core.LocalCommandResult{}, errors.New("clone without --name")
		}
		r.cloneDst = dst
		if _, exists := r.findLocked(name); exists {
			// prlctl refuses a duplicate host-unique VM name.
			return core.LocalCommandResult{Stderr: "The virtual machine with this name already exists."}, errors.New("prlctl clone failed")
		}
		if r.cloneErr != nil && !r.cloneCommit {
			return core.LocalCommandResult{Stderr: r.cloneErr.Error()}, r.cloneErr
		}
		r.cloneCalls = append(r.cloneCalls, name)
		r.addAtLocked(name, blankString(dst, r.vmHome))
		if r.cloneErr != nil {
			return core.LocalCommandResult{Stderr: r.cloneErr.Error()}, r.cloneErr
		}
		if r.afterClone != nil {
			defer r.afterClone()
		}
		return core.LocalCommandResult{}, nil
	case "start":
		r.mu.Lock()
		defer r.mu.Unlock()
		r.startCalls = append(r.startCalls, req.Args[1])
		for i := range r.vms {
			if r.vms[i].ID != req.Args[1] && r.vms[i].Name != req.Args[1] {
				continue
			}
			if strings.EqualFold(r.vms[i].State, "running") {
				// prlctl refuses to start a VM that is not stopped.
				return core.LocalCommandResult{Stderr: "Unable to perform the operation because " + r.vms[i].Name + " is not stopped."}, errors.New("prlctl start failed")
			}
			r.vms[i].State = "running"
		}
		return core.LocalCommandResult{}, nil
	case "stop":
		return core.LocalCommandResult{}, nil
	case "exec":
		r.mu.Lock()
		defer r.mu.Unlock()
		r.execCalls = append(r.execCalls, req.Args[1])
		return core.LocalCommandResult{}, nil
	case "delete":
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.deleteErr != nil {
			return core.LocalCommandResult{Stderr: r.deleteErr.Error()}, r.deleteErr
		}
		r.deleteCalls = append(r.deleteCalls, req.Args[1])
		kept := r.vms[:0]
		for _, vm := range r.vms {
			if vm.ID != req.Args[1] && vm.Name != req.Args[1] {
				kept = append(kept, vm)
			}
		}
		r.vms = kept
		return core.LocalCommandResult{}, nil
	default:
		return core.LocalCommandResult{}, errors.New("unexpected prlctl command: " + req.Args[0])
	}
}

func blankString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func fixedParallelsConfig() core.Config {
	cfg := core.BaseConfig()
	cfg.Provider = "parallels"
	cfg.TargetOS = core.TargetLinux
	cfg.Parallels.Source = "source-vm"
	cfg.Parallels.CloneMode = "full"
	cfg.Parallels.User = "crabbox"
	cfg.Parallels.WorkRoot = "/home/crabbox/work"
	return cfg
}

func fixedParallelsFixture(t *testing.T) (*leaseBackend, *parallelsFixedRunner, core.AcquireRequest) {
	t.Helper()
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not available")
	}
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	previous := waitForSSHReady
	waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }
	t.Cleanup(func() { waitForSSHReady = previous })

	cfg := fixedParallelsConfig()
	runner := newParallelsFixedRunner(cfg.Parallels.Source)
	backend, ok := NewBackend(Provider{}.Spec(), cfg, core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	if !ok {
		t.Fatal("NewBackend did not return the Parallels lease backend")
	}
	req := core.AcquireRequest{
		RequestedLeaseID: fixedTestLeaseID,
		RequestedSlug:    "quiet-lobster",
		Repo:             core.Repo{Root: filepath.Join(root, "repo")},
	}
	return backend, runner, req
}

func fixedLeaseVMName(t *testing.T, leaseID string) string {
	t.Helper()
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.FixedCreateIntent == nil {
		t.Fatalf("lease %s has no durable create intent", leaseID)
	}
	return core.ParallelsLeaseVMName(leaseID, claim.FixedCreateIntent.Slug)
}

// 1 — fixed-ID support is advertised.
func TestParallelsAdvertisesFixedLeaseIDSupport(t *testing.T) {
	backend := NewBackend(Provider{}.Spec(), fixedParallelsConfig(), core.Runtime{Stdout: io.Discard, Stderr: io.Discard})
	capable, ok := backend.(core.IdempotentLeaseIDBackend)
	if !ok {
		t.Fatal("parallels backend does not implement core.IdempotentLeaseIDBackend")
	}
	if !capable.SupportsRequestedLeaseID() {
		t.Fatal("parallels backend does not advertise fixed idempotent lease IDs")
	}
}

// 2 — the first fixed acquire persists the intent and the VM name before prlctl clone.
func TestParallelsFixedAcquirePersistsIntentBeforeClone(t *testing.T) {
	backend, runner, req := fixedParallelsFixture(t)
	var atClone core.LeaseClaim
	var atCloneExists bool
	runner.cloneErr = errors.New("clone reply lost")
	runner.beforeClone = func() {
		claim, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
		if err != nil {
			t.Error(err)
			return
		}
		atClone, atCloneExists = claim, exists
	}

	if _, err := backend.Acquire(context.Background(), req); err == nil {
		t.Fatal("a failed clone must not report a usable lease")
	}
	if !atCloneExists || atClone.FixedCreateIntent == nil {
		t.Fatal("durable create intent was not persisted before prlctl clone")
	}
	intent := atClone.FixedCreateIntent
	if intent.State != "prepared" {
		t.Fatalf("intent state at clone time=%q, want prepared", intent.State)
	}
	if intent.Attempt["submission"] != "submitted" {
		t.Fatalf("submission at clone time=%q, want submitted", intent.Attempt["submission"])
	}
	wantName := core.ParallelsLeaseVMName(req.RequestedLeaseID, intent.Slug)
	if intent.Attempt["name"] != wantName {
		t.Fatalf("attempt name at clone time=%q, want %q", intent.Attempt["name"], wantName)
	}
	// The scope is the attested Parallels service identity, never a fleet
	// entry's display name, so repointing a name cannot pass as the same host.
	if !strings.HasPrefix(intent.ProviderScope, "prlsrv1:") || intent.ProviderScope != atClone.ProviderScope {
		t.Fatalf("provider scope=%q/%q, want an attested prlsrv1 connection identity", intent.ProviderScope, atClone.ProviderScope)
	}
	if strings.Contains(intent.ProviderScope, "local") {
		t.Fatalf("provider scope %q still carries a host display name", intent.ProviderScope)
	}
	if strings.TrimSpace(intent.Fingerprint) == "" {
		t.Fatal("intent fingerprint is empty")
	}
	if atClone.CloudID != "" {
		t.Fatalf("resource identity bound before the clone: %q", atClone.CloudID)
	}
	after, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if after.FixedCreateIntent == nil || after.FixedCreateIntent.Attempt["name"] != wantName {
		t.Fatalf("durable attempt not retained after failure: %+v", after.FixedCreateIntent)
	}
}

// 3 — an identical replay returns the same lease without a second clone.
func TestParallelsFixedReplayReturnsSameLeaseWithoutSecondClone(t *testing.T) {
	backend, runner, req := fixedParallelsFixture(t)
	first, err := backend.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if first.LeaseID != req.RequestedLeaseID {
		t.Fatalf("lease id=%q, want %q", first.LeaseID, req.RequestedLeaseID)
	}
	wantName := fixedLeaseVMName(t, req.RequestedLeaseID)
	if first.Server.Name != wantName {
		t.Fatalf("server name=%q, want %q", first.Server.Name, wantName)
	}

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			replay, err := backend.Acquire(context.Background(), req)
			if err != nil {
				t.Errorf("replay: %v", err)
				return
			}
			if replay.LeaseID != first.LeaseID || replay.Server.CloudID != first.Server.CloudID {
				t.Errorf("replay lease=%s vm=%s, want %s/%s", replay.LeaseID, replay.Server.CloudID, first.LeaseID, first.Server.CloudID)
			}
		}()
	}
	wg.Wait()

	clones, deletes := runner.counts()
	if clones != 1 {
		t.Fatalf("clone calls=%d, want 1", clones)
	}
	if deletes != 0 {
		t.Fatalf("delete calls=%d, want 0", deletes)
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.FixedCreateIntent.State != "acquired" {
		t.Fatalf("intent state=%q, want acquired", claim.FixedCreateIntent.State)
	}
	if claim.CloudID != first.Server.CloudID || claim.CloudImmutableID != first.Server.CloudID {
		t.Fatalf("claim identity=%q/%q, want %q", claim.CloudID, claim.CloudImmutableID, first.Server.CloudID)
	}
}

// 3b — replay is idempotent about power state: prlctl refuses `start` on a VM
// that is not stopped, so an adopted running VM must be left alone.
func TestParallelsFixedReplayDoesNotRestartRunningVM(t *testing.T) {
	backend, runner, req := fixedParallelsFixture(t)
	first, err := backend.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	runner.mu.Lock()
	startsAfterFirst := len(runner.startCalls)
	runner.mu.Unlock()

	replay, err := backend.Acquire(context.Background(), req)
	if err != nil {
		t.Fatalf("replay of a running lease: %v", err)
	}
	if replay.Server.CloudID != first.Server.CloudID {
		t.Fatalf("replay vm=%q, want %q", replay.Server.CloudID, first.Server.CloudID)
	}
	runner.mu.Lock()
	starts := len(runner.startCalls)
	runner.mu.Unlock()
	if starts != startsAfterFirst {
		t.Fatalf("start calls=%d, want %d: replay restarted a running VM", starts, startsAfterFirst)
	}
}

// 4 — intent drift fails lease_id_conflict.
func TestParallelsFixedIntentDriftFailsLeaseIDConflict(t *testing.T) {
	for _, drift := range []struct {
		name  string
		apply func(*core.Config, *core.AcquireRequest)
	}{
		{"source", func(cfg *core.Config, _ *core.AcquireRequest) { cfg.Parallels.Source = "other-source" }},
		// A mutable source name repointed at a different VM is drift too: the
		// fingerprint is taken over the immutable UUID the host resolves.
		{"source_vm_replaced_under_same_name", nil},
		{"clone_mode", func(cfg *core.Config, _ *core.AcquireRequest) { cfg.Parallels.CloneMode = "unlink" }},
		{"target_os", func(cfg *core.Config, _ *core.AcquireRequest) { cfg.TargetOS = core.TargetMacOS }},
		{"windows_mode", func(cfg *core.Config, _ *core.AcquireRequest) { cfg.WindowsMode = core.WindowsModeWSL2 }},
		{"guest_user", func(cfg *core.Config, _ *core.AcquireRequest) { cfg.SSHUser = "someone-else" }},
		{"work_root", func(cfg *core.Config, _ *core.AcquireRequest) { cfg.WorkRoot = "/srv/elsewhere" }},
		{"vm_root", func(cfg *core.Config, _ *core.AcquireRequest) { cfg.Parallels.VMRoot = "/Volumes/Other" }},
		{"checkpoint", func(_ *core.Config, req *core.AcquireRequest) { req.RequestedCheckpointID = "chk_other" }},
		{"slug", func(_ *core.Config, req *core.AcquireRequest) { req.RequestedSlug = "other-lobster" }},
		{"keep", func(_ *core.Config, req *core.AcquireRequest) { req.Keep = !req.Keep }},
		{"ttl", func(cfg *core.Config, _ *core.AcquireRequest) { cfg.TTL += time.Hour }},
		{"idle_timeout", func(cfg *core.Config, _ *core.AcquireRequest) { cfg.IdleTimeout += time.Minute }},
		{"ssh_port", func(cfg *core.Config, _ *core.AcquireRequest) { cfg.SSHPort = "2201" }},
		{"pond", func(cfg *core.Config, _ *core.AcquireRequest) { cfg.Pond = "other-pond" }},
		{"desktop", func(cfg *core.Config, _ *core.AcquireRequest) { cfg.Desktop = !cfg.Desktop }},
		{"ssh_fallback_ports", func(cfg *core.Config, _ *core.AcquireRequest) { cfg.SSHFallbackPorts = []string{"2222"} }},
	} {
		t.Run(drift.name, func(t *testing.T) {
			backend, runner, req := fixedParallelsFixture(t)
			if drift.name == "source" {
				// The drifted source must exist, or the replay would fail on
				// resolution instead of on the create identity.
				runner.seedSource("other-source")
			}
			if _, err := backend.Acquire(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			if drift.apply != nil {
				drift.apply(&backend.Cfg, &req)
			} else {
				runner.replaceUUID(backend.Cfg.Parallels.Source, "{replacement-source-uuid}")
			}
			_, err := backend.Acquire(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
				t.Fatalf("drifted replay err=%v, want lease_id_conflict", err)
			}
			if clones, _ := runner.counts(); clones != 1 {
				t.Fatalf("clone calls=%d, want 1", clones)
			}
		})
	}
}

// 5 — conflicting labels, VM UUID, or Parallels host scope fail closed.
func TestParallelsFixedConflictingIdentityFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		corrupt func(*testing.T, *leaseBackend, *parallelsFixedRunner, string)
		wantErr string
	}{
		{
			name: "vm_uuid_replaced",
			corrupt: func(t *testing.T, _ *leaseBackend, runner *parallelsFixedRunner, name string) {
				runner.replaceUUID(name, "{impostor-uuid}")
			},
			wantErr: "lease_id_conflict",
		},
		{
			// An unreachable candidate is not proof that the recorded
			// connection left the fleet, so custody is retained rather than
			// rebound or finalized.
			name: "host_connection_unattestable",
			corrupt: func(t *testing.T, backend *leaseBackend, _ *parallelsFixedRunner, _ string) {
				backend.Cfg.Parallels.Hosts = []core.ParallelsHostConfig{{Name: "elsewhere", Host: "elsewhere.example"}}
			},
			wantErr: "claim and key retained",
		},
		{
			// The same machine reporting a different service identity is a
			// different connection, whatever the fleet entry is called.
			name: "host_service_identity_replaced",
			corrupt: func(t *testing.T, _ *leaseBackend, runner *parallelsFixedRunner, _ string) {
				runner.mu.Lock()
				runner.serverID = "replacement-server-id"
				runner.mu.Unlock()
			},
			wantErr: "lease_id_conflict",
		},
		{
			name: "claim_bound_to_another_provider",
			corrupt: func(t *testing.T, _ *leaseBackend, _ *parallelsFixedRunner, _ string) {
				rewriteFixedClaim(t, fixedTestLeaseID, func(claim *core.LeaseClaim) { claim.Provider = "incus" })
			},
			wantErr: "lease_id_conflict",
		},
		{
			// The durable provider scope is the host binding. A claim pointing
			// at a connection no configured host attests is refused; the `host`
			// label is an operator-facing display name and is deliberately not
			// an ownership check.
			name: "claim_provider_scope_drift",
			corrupt: func(t *testing.T, _ *leaseBackend, _ *parallelsFixedRunner, _ string) {
				rewriteFixedClaim(t, fixedTestLeaseID, func(claim *core.LeaseClaim) {
					claim.ProviderScope = "prlsrv1:" + strings.Repeat("f", 64)
					claim.FixedCreateIntent.ProviderScope = claim.ProviderScope
				})
			},
			wantErr: "lease_id_conflict",
		},
		{
			name: "claim_lease_label_drift",
			corrupt: func(t *testing.T, _ *leaseBackend, _ *parallelsFixedRunner, _ string) {
				rewriteFixedClaim(t, fixedTestLeaseID, func(claim *core.LeaseClaim) { claim.Labels["lease"] = "cbx_ffffffffffff" })
			},
			wantErr: "lease_id_conflict",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend, runner, req := fixedParallelsFixture(t)
			if _, err := backend.Acquire(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			name := fixedLeaseVMName(t, req.RequestedLeaseID)
			tc.corrupt(t, backend, runner, name)
			_, err := backend.Acquire(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("replay err=%v, want %q", err, tc.wantErr)
			}
			if clones, deletes := runner.counts(); clones != 1 || deletes != 0 {
				t.Fatalf("clone/delete calls=%d/%d, want 1/0", clones, deletes)
			}
		})
	}
}

// 6 — ambiguous or missing post-clone state does not issue a second clone.
func TestParallelsFixedAmbiguousStateDoesNotReclone(t *testing.T) {
	t.Run("lost_clone_reply_recovers_its_own_vm", func(t *testing.T) {
		backend, runner, req := fixedParallelsFixture(t)
		runner.cloneErr, runner.cloneCommit = errors.New("reply lost after commit"), true
		if _, err := backend.Acquire(context.Background(), req); err == nil {
			t.Fatal("a lost clone reply must not report a usable lease")
		}
		name := fixedLeaseVMName(t, req.RequestedLeaseID)
		committed, ok := runner.find(name)
		if !ok {
			t.Fatalf("fixture did not commit VM %q", name)
		}
		runner.cloneErr, runner.cloneCommit = nil, false

		// The clone's UUID was never observed, but the bundle it was told to
		// create is inside this attempt's own directory. That is provider-side
		// evidence, so replay recovers the VM instead of cloning a second one.
		lease, err := backend.Acquire(context.Background(), req)
		if err != nil {
			t.Fatalf("replay after a lost reply: %v", err)
		}
		if lease.Server.CloudID != committed.ID {
			t.Fatalf("adopted vm=%q, want the committed clone %q", lease.Server.CloudID, committed.ID)
		}
		if clones, deletes := runner.counts(); clones != 1 || deletes != 0 {
			t.Fatalf("clone/delete calls=%d/%d, want 1/0", clones, deletes)
		}
	})

	t.Run("lost_clone_reply_with_a_foreign_name_occupant_fails_closed", func(t *testing.T) {
		backend, runner, req := fixedParallelsFixture(t)
		runner.cloneErr, runner.cloneCommit = errors.New("reply lost before commit"), false
		if _, err := backend.Acquire(context.Background(), req); err == nil {
			t.Fatal("a lost clone reply must not report a usable lease")
		}
		runner.cloneErr = nil

		// Nothing was created in the attempt's directory, and a VM from some
		// other operation now holds the name. Replay must neither adopt it nor
		// clone over it.
		name := fixedLeaseVMName(t, req.RequestedLeaseID)
		foreign := runner.seedAt(name)
		_, err := backend.Acquire(context.Background(), req)
		if err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
			t.Fatalf("replay onto a foreign name occupant err=%v, want lease_id_conflict", err)
		}
		if clones, deletes := runner.counts(); clones != 0 || deletes != 0 {
			t.Fatalf("clone/delete calls=%d/%d, want 0/0", clones, deletes)
		}
		runner.mu.Lock()
		execs := len(runner.execCalls)
		runner.mu.Unlock()
		if execs != 0 {
			t.Fatalf("guest exec calls=%d, want 0: a foreign VM received guest mutation", execs)
		}
		if _, ok := runner.find(name); !ok {
			t.Fatalf("foreign VM %q was removed", name)
		}
		claim, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
		if err != nil || !exists || claim.FixedCreateIntent == nil {
			t.Fatalf("custody was not retained: exists=%t err=%v", exists, err)
		}
		if claim.CloudID == foreign.ID || claim.CloudImmutableID == foreign.ID {
			t.Fatalf("the claim bound the foreign VM %q", foreign.ID)
		}
	})

	t.Run("inventory_error_does_not_clone", func(t *testing.T) {
		backend, runner, req := fixedParallelsFixture(t)
		if _, err := backend.Acquire(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		runner.mu.Lock()
		runner.listErr = errors.New("host unreachable")
		runner.mu.Unlock()
		if _, err := backend.Acquire(context.Background(), req); err == nil {
			t.Fatal("an unreadable inventory must not report a usable lease")
		}
		if clones, _ := runner.counts(); clones != 1 {
			t.Fatalf("clone calls=%d, want 1", clones)
		}
	})

	t.Run("acquired_vm_absent_does_not_clone", func(t *testing.T) {
		backend, runner, req := fixedParallelsFixture(t)
		lease, err := backend.Acquire(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		runner.mu.Lock()
		kept := runner.vms[:0]
		for _, vm := range runner.vms {
			if vm.ID != lease.Server.CloudID {
				kept = append(kept, vm)
			}
		}
		runner.vms = kept
		runner.mu.Unlock()
		_, err = backend.Acquire(context.Background(), req)
		if err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
			t.Fatalf("replay of a vanished acquired VM err=%v, want lease_id_conflict", err)
		}
		if clones, _ := runner.counts(); clones != 1 {
			t.Fatalf("clone calls=%d, want 1", clones)
		}
	})
}

// 7 — release confirms the exact identity and retains a terminal tombstone.
func TestParallelsFixedReleaseConfirmsIdentityAndRetainsTombstone(t *testing.T) {
	backend, runner, req := fixedParallelsFixture(t)
	lease, err := backend.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	name := fixedLeaseVMName(t, req.RequestedLeaseID)

	t.Run("impostor_uuid_is_refused", func(t *testing.T) {
		original := lease.Server.CloudID
		runner.replaceUUID(name, "{impostor-uuid}")
		err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease, Force: true})
		if err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
			t.Fatalf("release of an impostor VM err=%v, want lease_id_conflict", err)
		}
		if _, deletes := runner.counts(); deletes != 0 {
			t.Fatalf("delete calls=%d, want 0", deletes)
		}
		runner.replaceUUID(name, original)
	})

	previous, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease, Force: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := runner.find(name); ok {
		t.Fatalf("VM %q survived release", name)
	}
	if _, deletes := runner.counts(); deletes != 1 {
		t.Fatalf("delete calls=%d, want 1", deletes)
	}
	tombstone, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if !exists || tombstone.FixedCreateIntent == nil {
		t.Fatal("release did not retain a terminal tombstone")
	}
	if tombstone.FixedCreateIntent.State != "released" {
		t.Fatalf("tombstone state=%q, want released", tombstone.FixedCreateIntent.State)
	}
	if len(tombstone.FixedCreateIntent.Attempt) != 0 || tombstone.CloudID != "" || len(tombstone.Labels) != 0 {
		t.Fatalf("tombstone retained live identity: %+v", tombstone)
	}
	if tombstone.FixedCreateIntent.Fingerprint != previous.FixedCreateIntent.Fingerprint {
		t.Fatal("tombstone changed the durable intent fingerprint")
	}

	verifier, ok := core.Backend(backend).(core.ReleaseLeaseClaimRetentionVerifier)
	if !ok {
		t.Fatal("parallels backend cannot attest its retained terminal claim")
	}
	retained, err := verifier.RetainLeaseClaimAfterReleaseWithClaim(lease, previous)
	if err != nil {
		t.Fatal(err)
	}
	if !retained {
		t.Fatal("terminal tombstone was not retained")
	}
}

// 8 — a replay after release cannot create another VM.
func TestParallelsFixedReplayAfterReleaseCannotCreateAnotherVM(t *testing.T) {
	backend, runner, req := fixedParallelsFixture(t)
	lease, err := backend.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease, Force: true}); err != nil {
		t.Fatal(err)
	}
	_, err = backend.Acquire(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Fatalf("replay after release err=%v, want lease_id_conflict", err)
	}
	if clones, _ := runner.counts(); clones != 1 {
		t.Fatalf("clone calls=%d, want 1", clones)
	}

	// A second release of the terminal lease stays a no-op.
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease, Force: true}); err != nil {
		t.Fatalf("replayed release err=%v, want nil", err)
	}
	if _, deletes := runner.counts(); deletes != 1 {
		t.Fatalf("delete calls=%d, want 1", deletes)
	}
}

// 9 — ordinary non-fixed warmup is unchanged.
func TestParallelsOrdinaryWarmupRemainsUnchanged(t *testing.T) {
	backend, runner, req := fixedParallelsFixture(t)
	req.RequestedLeaseID = ""
	lease, err := backend.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !core.IsCanonicalLeaseID(lease.LeaseID) || lease.LeaseID == fixedTestLeaseID {
		t.Fatalf("ordinary warmup lease id=%q", lease.LeaseID)
	}
	claim, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.FixedCreateIntent != nil {
		t.Fatalf("ordinary warmup wrote a fixed create intent: %+v", claim.FixedCreateIntent)
	}
	if claim.Provider != "parallels" || claim.CloudID != lease.Server.CloudID {
		t.Fatalf("ordinary claim=%+v", claim)
	}
	if clones, _ := runner.counts(); clones != 1 {
		t.Fatalf("clone calls=%d, want 1", clones)
	}

	// The ordinary claim is not a tombstone, so release still removes it.
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease, Force: true}); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID); err != nil || exists {
		t.Fatalf("ordinary claim exists=%v err=%v, want removed", exists, err)
	}
}

// 10 — no slug-based adoption.
func TestParallelsFixedDoesNotAdoptBySlug(t *testing.T) {
	backend, runner, req := fixedParallelsFixture(t)
	legacy := runner.seed("crabbox-" + req.RequestedSlug)
	other := runner.seed(core.ParallelsLeaseVMName("cbx_ffffffffffff", req.RequestedSlug))

	lease, err := backend.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.CloudID == legacy.ID || lease.Server.CloudID == other.ID {
		t.Fatalf("fixed acquire adopted a slug lookalike: %q", lease.Server.CloudID)
	}
	wantName := fixedLeaseVMName(t, req.RequestedLeaseID)
	if lease.Server.Name != wantName {
		t.Fatalf("server name=%q, want the lease-derived %q", lease.Server.Name, wantName)
	}
	clones, deletes := runner.counts()
	if clones != 1 || deletes != 0 {
		t.Fatalf("clone/delete calls=%d/%d, want 1/0", clones, deletes)
	}
	if _, ok := runner.find(legacy.Name); !ok {
		t.Fatal("legacy slug VM was removed")
	}
	if _, ok := runner.find(other.Name); !ok {
		t.Fatal("another lease's VM was removed")
	}
}

// 7b — automatic cleanup sweeps a fixed lease without pruning its tombstone.
func TestParallelsFixedCleanupKeepsTerminalTombstone(t *testing.T) {
	backend, runner, req := fixedParallelsFixture(t)
	lease, err := backend.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	name := fixedLeaseVMName(t, req.RequestedLeaseID)
	// Age the lease out the way an idle or expired fleet VM ages out, without
	// touching the durable create intent's own TTL.
	expired := maps.Clone(lease.Server.Labels)
	expired["expires_at"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	core.NewParallelsClient(backend.Cfg, backend.RT.Exec).SetLeaseLabels(req.RequestedLeaseID, expired)
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := runner.find(name); ok {
		t.Fatalf("expired VM %q survived cleanup", name)
	}
	if _, deletes := runner.counts(); deletes != 1 {
		t.Fatalf("delete calls=%d, want 1", deletes)
	}
	tombstone, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if !exists || tombstone.FixedCreateIntent == nil || tombstone.FixedCreateIntent.State != "released" {
		t.Fatalf("cleanup pruned the terminal tombstone: exists=%v claim=%+v", exists, tombstone)
	}
	if _, err := backend.Acquire(context.Background(), req); err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Fatalf("replay after cleanup err=%v, want lease_id_conflict", err)
	}
	if clones, _ := runner.counts(); clones != 1 {
		t.Fatalf("clone calls=%d, want 1", clones)
	}
}

// 10b — --reclaim cannot re-bind a fixed lease and discard its create intent.
func TestParallelsFixedReclaimCannotRebindDurableIntent(t *testing.T) {
	backend, _, req := fixedParallelsFixture(t)
	lease, err := backend.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	// Break the exact VM binding so resolution no longer recognizes ownership,
	// which is the only path that could reach adoption.
	rewriteFixedClaim(t, req.RequestedLeaseID, func(claim *core.LeaseClaim) { claim.CloudID = "{stale-uuid}" })

	_, err = backend.Resolve(context.Background(), core.ResolveRequest{
		ID: req.RequestedLeaseID, Repo: req.Repo, Reclaim: true,
	})
	if err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Fatalf("reclaim of a fixed lease err=%v, want lease_id_conflict", err)
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.FixedCreateIntent == nil || claim.FixedCreateIntent.Attempt["name"] != lease.Server.Name {
		t.Fatalf("reclaim discarded the durable create intent: %+v", claim.FixedCreateIntent)
	}
}

func rewriteFixedClaim(t *testing.T, leaseID string, mutate func(*core.LeaseClaim)) {
	t.Helper()
	current, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil || !exists {
		t.Fatalf("read claim %s: exists=%v err=%v", leaseID, exists, err)
	}
	next := current
	next.Labels = maps.Clone(current.Labels)
	if current.FixedCreateIntent != nil {
		intent := *current.FixedCreateIntent
		intent.Attempt = maps.Clone(current.FixedCreateIntent.Attempt)
		next.FixedCreateIntent = &intent
	}
	mutate(&next)
	if _, err := core.ReplaceLeaseClaimIfUnchangedDurableAfter(leaseID, current, next, nil); err != nil {
		t.Fatal(err)
	}
}
