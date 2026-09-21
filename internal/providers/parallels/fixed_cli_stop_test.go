package parallels

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"

	// The CLI's default provider must resolve for `stop --provider parallels`
	// to get past provider selection.
	_ "github.com/openclaw/crabbox/internal/providers/hetzner"
)

// The CLI entrypoint runs the real command runner, so these tests put an
// executable prlctl and prlsrvctl on PATH instead of injecting a fake runner.
// Each shim re-executes this test binary, which acts as the Parallels host when
// fakeParallelsStateEnv is set.
const (
	fakeParallelsStateEnv = "CRABBOX_TEST_FAKE_PARALLELS_STATE"
	fakeParallelsServerID = "cli-server-id"
	fakeParallelsHardware = "cli-hardware-id"
	// Best-effort SSH cleanup runs for real during stop. A loopback address on
	// the discard port fails immediately instead of spending a connect timeout
	// per attempt, and never reaches a service.
	fakeParallelsGuestIP   = "127.0.0.1"
	fakeParallelsGuestPort = "9"
	fakeParallelsVMHome    = "/vms"
)

type fakeParallelsState struct {
	VMs []core.ParallelsVM `json:"vms"`
	// GuestCalls records every prlctl subcommand that reaches into a guest, so
	// a test can assert that authorization happened before any guest I/O.
	GuestCalls []string `json:"guestCalls"`
}

func fakeParallelsStatePath() string { return os.Getenv(fakeParallelsStateEnv) }

func readFakeParallelsState(t *testing.T) fakeParallelsState {
	t.Helper()
	state, err := loadFakeParallelsState(fakeParallelsStatePath())
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func loadFakeParallelsState(path string) (fakeParallelsState, error) {
	var state fakeParallelsState
	data, err := os.ReadFile(path)
	if err != nil {
		return state, err
	}
	return state, json.Unmarshal(data, &state)
}

func writeFakeParallelsState(path string, state fakeParallelsState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// encodeFakeParallelsVMs mirrors real prlctl: the bundle path appears only in
// the `-i` info form, never in the plain listing.
func encodeFakeParallelsVMs(vms []core.ParallelsVM, detailed bool) string {
	items := make([]map[string]any, 0, len(vms))
	for _, vm := range vms {
		item := map[string]any{"ID": vm.ID, "Name": vm.Name, "State": vm.State, "ip_configured": vm.IP}
		if detailed {
			item["Home"] = vm.Home
		}
		items = append(items, item)
	}
	data, _ := json.Marshal(items)
	return string(data)
}

// runFakeParallelsBinary implements just enough of prlctl and prlsrvctl for the
// fixed-lease stop paths. It is the process the PATH shims exec.
func runFakeParallelsBinary(statePath string, args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "fake parallels binary: missing command")
		return 2
	}
	binary, rest := args[1], args[2:]
	state, err := loadFakeParallelsState(statePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake parallels binary:", err)
		return 2
	}
	find := func(handle string) (core.ParallelsVM, bool) {
		for _, vm := range state.VMs {
			if vm.Name == handle || vm.ID == handle {
				return vm, true
			}
		}
		return core.ParallelsVM{}, false
	}
	switch binary {
	case "id":
		fmt.Println("501")
		return 0
	case "mkdir", "rmdir":
		return 0
	case "prlsrvctl":
		if len(rest) == 0 || rest[0] != "info" {
			fmt.Fprintln(os.Stderr, "unexpected prlsrvctl command")
			return 2
		}
		fmt.Printf(`{"ID":%q,"Hardware Id":%q,"VM home":%q}`+"\n", fakeParallelsServerID, fakeParallelsHardware, fakeParallelsVMHome)
		return 0
	case "prlctl":
		if len(rest) == 0 {
			fmt.Fprintln(os.Stderr, "unexpected prlctl command")
			return 2
		}
		switch rest[0] {
		case "list":
			if len(rest) > 1 && rest[1] == "-i" && !strings.HasPrefix(rest[len(rest)-1], "-") {
				vm, ok := find(rest[len(rest)-1])
				if !ok {
					fmt.Fprintln(os.Stderr, "The virtual machine could not be found.")
					return 1
				}
				fmt.Println(encodeFakeParallelsVMs([]core.ParallelsVM{vm}, true))
				return 0
			}
			fmt.Println(encodeFakeParallelsVMs(state.VMs, slices.Contains(rest, "-i")))
			return 0
		case "snapshot-list":
			fmt.Println("[]")
			return 0
		case "clone":
			name, dst := "", ""
			for i := 0; i+1 < len(rest); i++ {
				switch rest[i] {
				case "--name":
					name = rest[i+1]
				case "--dst":
					dst = rest[i+1]
				}
			}
			if strings.TrimSpace(dst) == "" {
				dst = fakeParallelsVMHome
			}
			if name == "" {
				fmt.Fprintln(os.Stderr, "clone without --name")
				return 2
			}
			if _, exists := find(name); exists {
				fmt.Fprintln(os.Stderr, "The virtual machine with this name already exists.")
				return 1
			}
			state.VMs = append(state.VMs, core.ParallelsVM{
				ID: fmt.Sprintf("{cli-vm-%d}", len(state.VMs)), Name: name, State: "stopped", IP: fakeParallelsGuestIP,
				Home: strings.TrimRight(dst, "/") + "/" + name + ".pvm/",
			})
			if err := writeFakeParallelsState(statePath, state); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 2
			}
			return 0
		case "start":
			for i := range state.VMs {
				if state.VMs[i].ID == rest[1] || state.VMs[i].Name == rest[1] {
					state.VMs[i].State = "running"
				}
			}
			if err := writeFakeParallelsState(statePath, state); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 2
			}
			return 0
		case "stop":
			return 0
		case "exec":
			state.GuestCalls = append(state.GuestCalls, strings.Join(rest, " "))
			if err := writeFakeParallelsState(statePath, state); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 2
			}
			return 0
		case "delete":
			kept := state.VMs[:0]
			for _, vm := range state.VMs {
				if vm.ID != rest[1] && vm.Name != rest[1] {
					kept = append(kept, vm)
				}
			}
			state.VMs = kept
			if err := writeFakeParallelsState(statePath, state); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 2
			}
			return 0
		}
		fmt.Fprintln(os.Stderr, "unexpected prlctl command: "+rest[0])
		return 2
	}
	fmt.Fprintln(os.Stderr, "unexpected binary: "+binary)
	return 2
}

// fixedParallelsCLIFixture acquires a real fixed lease through the backend over
// executable prlctl/prlsrvctl shims, so the same host state is visible to the
// CLI entrypoint afterwards.
func fixedParallelsCLIFixture(t *testing.T) (core.LeaseTarget, string, string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Skip("test binary path unavailable")
	}
	root := t.TempDir()
	statePath := filepath.Join(root, "parallels-state.json")
	const source = "source-vm"
	if err := writeFakeParallelsState(statePath, fakeParallelsState{
		VMs: []core.ParallelsVM{{ID: "{cli-source-uuid}", Name: source, State: "stopped", Home: fakeParallelsVMHome + "/" + source + ".pvm/"}},
	}); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, binary := range []string{"prlctl", "prlsrvctl", "mkdir", "rmdir", "id"} {
		shim := fmt.Sprintf("#!/bin/sh\nexec %q __fake-parallels %s \"$@\"\n", self, binary)
		if err := os.WriteFile(filepath.Join(binDir, binary), []byte(shim), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv(fakeParallelsStateEnv, statePath)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	previous := waitForSSHReady
	waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }
	t.Cleanup(func() { waitForSSHReady = previous })

	cfg := fixedParallelsConfig()
	cfg.Parallels.Source = source
	cfg.SSHPort = fakeParallelsGuestPort
	// The real command runner reaches the PATH shims.
	backend, ok := NewBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	if !ok {
		t.Fatal("NewBackend did not return the Parallels lease backend")
	}
	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{
		RequestedLeaseID: fixedTestLeaseID,
		RequestedSlug:    "quiet-lobster",
		Repo:             core.Repo{Root: filepath.Join(root, "repo")},
	})
	if err != nil {
		t.Fatalf("acquire over PATH shims: %v", err)
	}
	return lease, statePath, source
}

func runCrabboxStop(t *testing.T, id string) (string, error) {
	t.Helper()
	var output strings.Builder
	err := (core.App{Stdout: &output, Stderr: &output}).Run(context.Background(),
		[]string{"stop", "--provider", "parallels", "--id", id})
	return output.String(), err
}

// R5 — fixed-lease recovery is reachable from `crabbox stop`.
//
// App.stop resolves before releasing, and Parallels resolution only returns
// live inventory matches. Without fixed-claim-aware release resolution, a
// terminal tombstone or an acquired VM that is already gone returns
// "parallels lease not found" and the durable release path is never entered,
// so the advertised missing-resource finalization and idempotent terminal stop
// work only in direct backend calls.
func TestParallelsFixedStopThroughCLIFinalizesMissingVM(t *testing.T) {
	lease, statePath, source := fixedParallelsCLIFixture(t)

	// The acquired VM disappears out of band; only the source remains.
	if err := writeFakeParallelsState(statePath, fakeParallelsState{
		VMs: []core.ParallelsVM{{ID: "{cli-source-uuid}", Name: source, State: "stopped"}},
	}); err != nil {
		t.Fatal(err)
	}

	output, err := runCrabboxStop(t, lease.LeaseID)
	if err != nil {
		t.Fatalf("crabbox stop of a missing fixed VM err=%v output=%s", err, output)
	}
	if strings.Contains(output, "lease not found") {
		t.Fatalf("stop reported the lease as unknown instead of finalizing it: %s", output)
	}
	tombstone, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
	if err != nil || !exists || tombstone.FixedCreateIntent == nil {
		t.Fatalf("terminal tombstone exists=%t err=%v", exists, err)
	}
	if tombstone.FixedCreateIntent.State != "released" {
		t.Fatalf("intent state=%q, want released", tombstone.FixedCreateIntent.State)
	}

	// R5b — a repeated stop of the terminal lease is an idempotent no-op.
	output, err = runCrabboxStop(t, lease.LeaseID)
	if err != nil {
		t.Fatalf("repeated crabbox stop err=%v output=%s", err, output)
	}
	if strings.Contains(output, "lease not found") {
		t.Fatalf("idempotent stop reported the terminal lease as unknown: %s", output)
	}
	again, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
	if err != nil || !exists || again.FixedCreateIntent == nil || again.FixedCreateIntent.State != "released" {
		t.Fatalf("idempotent stop disturbed the tombstone: %#v exists=%t err=%v", again, exists, err)
	}
}

// R5c — the live case still deletes through the CLI, and the tombstone stays.
func TestParallelsFixedStopThroughCLIDeletesLiveVM(t *testing.T) {
	lease, statePath, _ := fixedParallelsCLIFixture(t)

	output, err := runCrabboxStop(t, lease.LeaseID)
	if err != nil {
		t.Fatalf("crabbox stop err=%v output=%s", err, output)
	}
	state := readFakeParallelsState(t)
	_ = statePath
	for _, vm := range state.VMs {
		if vm.ID == lease.Server.CloudID {
			t.Fatalf("VM %q survived crabbox stop", vm.Name)
		}
	}
	tombstone, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
	if err != nil || !exists || tombstone.FixedCreateIntent == nil || tombstone.FixedCreateIntent.State != "released" {
		t.Fatalf("tombstone after CLI stop: %#v exists=%t err=%v", tombstone, exists, err)
	}
	if tombstone.Provider != core.FixedParallelsClaimProvider {
		t.Fatalf("tombstone provider=%q, want %q", tombstone.Provider, core.FixedParallelsClaimProvider)
	}
}

// R5d — an unfinished attempt reaches the durable release path through the CLI
// and reports uncertain custody instead of a generic ownership refusal, without
// deleting the VM occupying its recorded name.
func TestParallelsFixedStopThroughCLIRetainsCustodyForUnboundAttempt(t *testing.T) {
	lease, statePath, source := fixedParallelsCLIFixture(t)

	// Rewind the claim to an unfinished attempt: the clone reply was lost, so
	// no VM incarnation was ever recorded, while the VM itself exists.
	rewriteFixedClaim(t, lease.LeaseID, func(claim *core.LeaseClaim) {
		claim.CloudID, claim.CloudImmutableID = "", ""
		claim.FixedCreateIntent.State = "prepared"
		delete(claim.FixedCreateIntent.Attempt, "vm_uuid")
	})

	output, err := runCrabboxStop(t, lease.LeaseID)
	if err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Fatalf("crabbox stop of an unattested attempt err=%v output=%s", err, output)
	}
	if strings.Contains(err.Error(), "adopt it with an explicit --reclaim") {
		t.Fatalf("stop reported a generic ownership refusal instead of uncertain custody: %v", err)
	}
	state := readFakeParallelsState(t)
	var names []string
	for _, vm := range state.VMs {
		names = append(names, vm.Name)
	}
	if len(state.VMs) != 2 {
		t.Fatalf("host inventory=%v, want the source %q and the lease VM retained", names, source)
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
	if err != nil || !exists || claim.FixedCreateIntent == nil {
		t.Fatalf("custody was not retained: exists=%t err=%v", exists, err)
	}
	if claim.FixedCreateIntent.State == "released" {
		t.Fatal("a refused stop wrote a terminal tombstone")
	}
	_ = statePath
}

// R8 — stop authorizes a fixed lease before it touches any guest.
//
// The fixed-claim exception in release resolution let stop continue through the
// ordinary inventory path, which reads guest files to discover the SSH user and
// returns a usable SSH target; App.stop then runs remote connection cleanup
// before ReleaseLease. A running replacement VM at the recorded name could
// therefore be reached with the configured guest credentials before the
// eventual lease_id_conflict. Nothing may reach the guest until the recorded
// host and VM incarnation have been re-attested.
func TestParallelsFixedStopThroughCLIRejectsReplacementBeforeGuestIO(t *testing.T) {
	lease, statePath, source := fixedParallelsCLIFixture(t)
	name := ""
	for _, vm := range readFakeParallelsState(t).VMs {
		if vm.ID == lease.Server.CloudID {
			name = vm.Name
		}
	}
	if name == "" {
		t.Fatal("fixture did not record the acquired VM")
	}

	// A different, running VM now occupies the lease's recorded name.
	if err := writeFakeParallelsState(statePath, fakeParallelsState{VMs: []core.ParallelsVM{
		{ID: "{cli-source-uuid}", Name: source, State: "stopped"},
		{ID: "{cli-replacement-uuid}", Name: name, State: "running", IP: fakeParallelsGuestIP},
	}}); err != nil {
		t.Fatal(err)
	}

	output, err := runCrabboxStop(t, lease.LeaseID)
	if err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Fatalf("crabbox stop onto a replacement err=%v output=%s", err, output)
	}
	state := readFakeParallelsState(t)
	if len(state.GuestCalls) != 0 {
		t.Fatalf("stop issued %d guest command(s) before rejecting the replacement: %v", len(state.GuestCalls), state.GuestCalls)
	}
	if len(state.VMs) != 2 {
		t.Fatalf("host inventory changed: %+v", state.VMs)
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
	if err != nil || !exists || claim.FixedCreateIntent == nil {
		t.Fatalf("custody was not retained: exists=%t err=%v", exists, err)
	}
	if claim.FixedCreateIntent.State == "released" {
		t.Fatal("a refused stop wrote a terminal tombstone")
	}
}
