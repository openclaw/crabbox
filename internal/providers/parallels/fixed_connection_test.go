package parallels

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// parallelsFixedMachine is one physical Parallels host: its own service identity and
// its own VM inventory.
type parallelsFixedMachine struct {
	serverID   string
	hardwareID string
	vms        []core.ParallelsVM
	nextUUID   int
	cloneCalls []string
	vmHome     string
	accountID  string
}

func (m *parallelsFixedMachine) findLocked(handle string) (core.ParallelsVM, bool) {
	for _, vm := range m.vms {
		if vm.Name == handle || vm.ID == handle {
			return vm, true
		}
	}
	return core.ParallelsVM{}, false
}

func (m *parallelsFixedMachine) addLocked(name, dir string) core.ParallelsVM {
	m.nextUUID++
	if strings.TrimSpace(dir) == "" {
		dir = m.vmHome
	}
	vm := core.ParallelsVM{
		ID: fmt.Sprintf("{%s-vm-%d}", m.serverID, m.nextUUID), Name: name, State: "stopped", IP: "10.211.55.9",
		Home: strings.TrimRight(dir, "/") + "/" + name + ".pvm/",
	}
	m.vms = append(m.vms, vm)
	return vm
}

// parallelsFixedFleetRunner fakes a multi-machine Parallels fleet reached over ssh,
// so a fleet entry can keep its display name while its host or account is
// repointed at a different machine.
type parallelsFixedFleetRunner struct {
	mu       sync.Mutex
	machines map[string]*parallelsFixedMachine
	// cloneErr simulates a lost clone reply: the VM is committed on the machine
	// but the caller never learns its identity.
	cloneErr error
}

func newParallelsFixedFleetRunner(source string, endpoints ...string) *parallelsFixedFleetRunner {
	runner := &parallelsFixedFleetRunner{machines: map[string]*parallelsFixedMachine{}}
	for i, endpoint := range endpoints {
		machine := &parallelsFixedMachine{
			serverID:   fmt.Sprintf("server-%d", i+1),
			hardwareID: fmt.Sprintf("hardware-%d", i+1),
			vmHome:     "/vms",
			accountID:  "501",
		}
		machine.vms = append(machine.vms, core.ParallelsVM{ID: fmt.Sprintf("{source-uuid-%d}", i+1), Name: source, State: "stopped", Home: "/vms/" + source + ".pvm/"})
		runner.machines[endpoint] = machine
	}
	return runner
}

func (r *parallelsFixedFleetRunner) machine(t *testing.T, endpoint string) *parallelsFixedMachine {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	machine, ok := r.machines[endpoint]
	if !ok {
		t.Fatalf("no fake Parallels machine at %q", endpoint)
	}
	return machine
}

func (r *parallelsFixedFleetRunner) crabboxVMNames(t *testing.T, endpoint string) []string {
	t.Helper()
	machine := r.machine(t, endpoint)
	r.mu.Lock()
	defer r.mu.Unlock()
	var names []string
	for _, vm := range machine.vms {
		if strings.HasPrefix(vm.Name, "crabbox-") {
			names = append(names, vm.Name)
		}
	}
	return names
}

// splitRemoteShellWords reverses cli.ShellWords for the simple single-quoted
// words the Parallels client emits.
func splitRemoteShellWords(command string) []string {
	var words []string
	var current strings.Builder
	quoted, started := false, false
	for i := 0; i < len(command); i++ {
		switch c := command[i]; {
		case c == '\'':
			quoted, started = !quoted, true
		case c == ' ' && !quoted:
			if started || current.Len() > 0 {
				words = append(words, current.String())
				current.Reset()
				started = false
			}
		default:
			current.WriteByte(c)
			started = true
		}
	}
	if started || current.Len() > 0 {
		words = append(words, current.String())
	}
	return words
}

func (r *parallelsFixedFleetRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	if !strings.HasSuffix(req.Name, "ssh") && !strings.HasSuffix(req.Name, "ssh.exe") {
		return core.LocalCommandResult{}, errors.New("fleet runner expects an ssh transport, got " + req.Name)
	}
	if len(req.Args) < 2 {
		return core.LocalCommandResult{}, errors.New("ssh invocation without a remote command")
	}
	accountTarget := req.Args[len(req.Args)-2]
	target := accountTarget
	if _, host, ok := strings.Cut(target, "@"); ok {
		target = host
	}
	words := splitRemoteShellWords(req.Args[len(req.Args)-1])
	for len(words) > 0 && strings.Contains(words[0], "=") && !strings.HasPrefix(words[0], "prl") {
		words = words[1:]
	}
	if len(words) == 0 {
		return core.LocalCommandResult{}, errors.New("empty remote command")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	machine, ok := r.machines[target]
	if account, exists := r.machines[accountTarget]; exists {
		machine, ok = account, true
	}
	if !ok {
		return core.LocalCommandResult{Stderr: "ssh: Could not resolve hostname " + target}, errors.New("ssh failed")
	}
	switch binary, args := words[0], words[1:]; binary {
	case "id":
		if len(args) == 1 && args[0] == "-u" {
			return core.LocalCommandResult{Stdout: machine.accountID + "\n"}, nil
		}
		return core.LocalCommandResult{}, errors.New("unexpected id arguments")
	case "mkdir", "rmdir":
		return core.LocalCommandResult{}, nil
	case "prlsrvctl":
		if len(args) == 0 || args[0] != "info" {
			return core.LocalCommandResult{}, errors.New("unexpected prlsrvctl command")
		}
		return core.LocalCommandResult{Stdout: fmt.Sprintf(`{"ID":%q,"Hardware Id":%q,"VM home":%q}`, machine.serverID, machine.hardwareID, machine.vmHome)}, nil
	case "prlctl":
		if len(args) == 0 {
			return core.LocalCommandResult{}, errors.New("empty prlctl command")
		}
		switch args[0] {
		case "list":
			if len(args) > 1 && args[1] == "-i" && !strings.HasPrefix(args[len(args)-1], "-") {
				vm, found := machine.findLocked(args[len(args)-1])
				if !found {
					return core.LocalCommandResult{Stderr: "The virtual machine could not be found."}, errors.New("prlctl list failed")
				}
				return core.LocalCommandResult{Stdout: encodeParallelsVMsDetailed([]core.ParallelsVM{vm})}, nil
			}
			if slices.Contains(args, "-i") {
				return core.LocalCommandResult{Stdout: encodeParallelsVMsDetailed(machine.vms)}, nil
			}
			return core.LocalCommandResult{Stdout: encodeParallelsVMs(machine.vms)}, nil
		case "snapshot-list":
			return core.LocalCommandResult{Stdout: "[]"}, nil
		case "clone":
			name, dst := "", ""
			for i := 0; i+1 < len(args); i++ {
				switch args[i] {
				case "--name":
					name = args[i+1]
				case "--dst":
					dst = args[i+1]
				}
			}
			if name == "" {
				return core.LocalCommandResult{}, errors.New("clone without --name")
			}
			if _, exists := machine.findLocked(name); exists {
				return core.LocalCommandResult{Stderr: "The virtual machine with this name already exists."}, errors.New("prlctl clone failed")
			}
			machine.cloneCalls = append(machine.cloneCalls, name)
			machine.addLocked(name, dst)
			if r.cloneErr != nil {
				return core.LocalCommandResult{Stderr: r.cloneErr.Error()}, r.cloneErr
			}
			return core.LocalCommandResult{}, nil
		case "start", "stop", "exec":
			for i := range machine.vms {
				if machine.vms[i].ID == args[1] || machine.vms[i].Name == args[1] {
					if args[0] == "start" {
						machine.vms[i].State = "running"
					}
				}
			}
			return core.LocalCommandResult{}, nil
		case "delete":
			kept := machine.vms[:0]
			for _, vm := range machine.vms {
				if vm.ID != args[1] && vm.Name != args[1] {
					kept = append(kept, vm)
				}
			}
			machine.vms = kept
			return core.LocalCommandResult{}, nil
		}
		return core.LocalCommandResult{}, errors.New("unexpected prlctl command: " + args[0])
	}
	return core.LocalCommandResult{}, errors.New("unexpected remote binary: " + words[0])
}

const (
	fleetMachineA = "machine-a.invalid"
	fleetMachineB = "machine-b.invalid"
)

func fixedParallelsFleetFixture(t *testing.T) (*leaseBackend, *parallelsFixedFleetRunner, core.AcquireRequest) {
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
	// One fleet entry with a stable display name, pointing at machine A.
	cfg.Parallels.Hosts = []core.ParallelsHostConfig{{Name: "fleet-a", Host: fleetMachineA, User: "builder"}}
	runner := newParallelsFixedFleetRunner(cfg.Parallels.Source, fleetMachineA, fleetMachineB)
	backend, ok := NewBackend(Provider{}.Spec(), cfg, core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	if !ok {
		t.Fatal("NewBackend did not return the Parallels lease backend")
	}
	return backend, runner, core.AcquireRequest{
		RequestedLeaseID: fixedTestLeaseID,
		RequestedSlug:    "quiet-lobster",
		Repo:             core.Repo{Root: filepath.Join(root, "repo")},
	}
}

// R2 — replay is bound to the connection, not to the fleet entry's display name.
//
// A lost clone reply leaves a prepared claim with no bound VM. Repointing the
// same `name:` at a different machine must not pass the scope check: replay
// would inspect the replacement, see no VM there, and clone a second one while
// the original keeps running on the old machine.
func TestParallelsFixedReplayRejectsRepointedFleetEntry(t *testing.T) {
	backend, runner, req := fixedParallelsFleetFixture(t)
	runner.cloneErr = errors.New("clone reply lost")
	if _, err := backend.Acquire(context.Background(), req); err == nil {
		t.Fatal("a lost clone reply must not report a usable lease")
	}
	if names := runner.crabboxVMNames(t, fleetMachineA); len(names) != 1 {
		t.Fatalf("machine A crabbox VMs=%v, want exactly the committed clone", names)
	}
	runner.cloneErr = nil

	// The display name is unchanged; only the machine behind it moved.
	backend.Cfg.Parallels.Hosts[0].Host = fleetMachineB

	_, err := backend.Acquire(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Fatalf("replay against a repointed fleet entry err=%v, want lease_id_conflict", err)
	}
	if names := runner.crabboxVMNames(t, fleetMachineB); len(names) != 0 {
		t.Fatalf("replay provisioned a second VM on the replacement machine: %v", names)
	}
	if names := runner.crabboxVMNames(t, fleetMachineA); len(names) != 1 {
		t.Fatalf("machine A crabbox VMs=%v, want the original clone retained", names)
	}
}

// R2b — release is bound to the same connection identity.
func TestParallelsFixedReleaseRejectsRepointedFleetEntry(t *testing.T) {
	backend, runner, req := fixedParallelsFleetFixture(t)
	lease, err := backend.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	backend.Cfg.Parallels.Hosts[0].Host = fleetMachineB
	err = backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease, Force: true})
	if err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Fatalf("release against a repointed fleet entry err=%v, want lease_id_conflict", err)
	}
	if names := runner.crabboxVMNames(t, fleetMachineA); len(names) != 1 {
		t.Fatalf("machine A crabbox VMs=%v, want the acquired VM retained", names)
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
	if err != nil || !exists || claim.FixedCreateIntent == nil {
		t.Fatalf("claim exists=%t err=%v", exists, err)
	}
	if claim.FixedCreateIntent.State == "released" {
		t.Fatal("a refused release wrote a terminal tombstone")
	}
}

// R2c — the same machine/account reached through a different address still
// owns its lease.
//
// Binding to the configured endpoint instead of the attested service would make
// a routine address change look like a different host, stranding running VMs
// with no CLI route to stop them. Only what the Parallels service reports about
// itself decides identity.
func TestParallelsFixedFollowsHostToANewAddress(t *testing.T) {
	backend, runner, req := fixedParallelsFleetFixture(t)
	first, err := backend.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	// The same machine/account now answers at a new address and fleet entry.
	runner.mu.Lock()
	moved := runner.machines[fleetMachineA]
	runner.machines[fleetMachineB] = moved
	runner.mu.Unlock()
	backend.Cfg.Parallels.Hosts = []core.ParallelsHostConfig{{Name: "fleet-renamed", Host: fleetMachineB, User: "builder"}}

	replay, err := backend.Acquire(context.Background(), req)
	if err != nil {
		t.Fatalf("replay after a host address change: %v", err)
	}
	if replay.LeaseID != first.LeaseID || replay.Server.CloudID != first.Server.CloudID {
		t.Fatalf("replay lease=%s vm=%s, want %s/%s", replay.LeaseID, replay.Server.CloudID, first.LeaseID, first.Server.CloudID)
	}
	if names := runner.crabboxVMNames(t, fleetMachineB); len(names) != 1 {
		t.Fatalf("crabbox VMs=%v, want exactly the original clone", names)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: replay, Force: true}); err != nil {
		t.Fatalf("release after a host address change: %v", err)
	}
	if names := runner.crabboxVMNames(t, fleetMachineB); len(names) != 0 {
		t.Fatalf("crabbox VMs=%v after release, want none", names)
	}
}

func TestParallelsFixedReleaseRejectsDifferentAccountInventory(t *testing.T) {
	backend, runner, req := fixedParallelsFleetFixture(t)
	lease, err := backend.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	runner.mu.Lock()
	original := runner.machines[fleetMachineA]
	runner.machines["operator@"+fleetMachineA] = &parallelsFixedMachine{
		serverID: original.serverID, hardwareID: original.hardwareID,
		accountID: "502", vmHome: "/Users/operator/Parallels",
	}
	runner.mu.Unlock()
	backend.Cfg.Parallels.Hosts[0].User = "operator"
	err = backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease})
	if err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Errorf("release through another account err=%v, want conflict", err)
	}
	claim, err := core.ReadLeaseClaim(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.FixedCreateIntent.State != "acquired" {
		t.Errorf("false absence retired a live lease: %s", claim.FixedCreateIntent.State)
	}
	if names := runner.crabboxVMNames(t, fleetMachineA); len(names) != 1 {
		t.Fatalf("original VMs=%v", names)
	}
}
