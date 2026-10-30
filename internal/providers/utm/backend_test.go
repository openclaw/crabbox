package utm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const sourceUUID = "11111111-1111-4111-8111-111111111111"
const otherUUID = "22222222-2222-4222-8222-222222222222"

type controlFixture struct {
	mu             sync.Mutex
	vms            []vm
	calls          []core.LocalCommandRequest
	next           int
	failAfterClone bool
	failDelete     bool
	hideClone      bool
	cancelClone    func()
	failInventory  bool
	actions        []string
	storageRoot    string
	nativePlists   bool
	failReload     bool
}

func preparedTemplate() vm {
	return vm{ID: sourceUUID, Name: "prepared template", Status: "stopped", Backend: "qemu", Configuration: configuration{Architecture: "aarch64", Networks: []network{{Index: 0, Mode: "emulated", Address: "52:54:00:00:00:01"}}}}
}
func (f *controlFixture) Run(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	if req.Name == "/usr/bin/plutil" {
		return f.runPlutil(ctx, req)
	}
	out := func(s string) (core.LocalCommandResult, error) { return core.LocalCommandResult{Stdout: s}, nil }
	if req.Name == "/usr/bin/osascript" {
		if len(req.Args) > 1 && req.Args[0] == "-l" {
			if f.failInventory {
				return core.LocalCommandResult{}, errors.New("automation denied")
			}
			vms := f.vms
			if f.hideClone {
				vms = []vm{f.vms[0]}
			}
			data, _ := json.Marshal(vms)
			return out(string(data))
		}
		script, _ := io.ReadAll(req.Stdin)
		field := func(pattern string) string {
			m := regexp.MustCompile(pattern).FindStringSubmatch(string(script))
			if len(m) != 2 {
				panic("bad scripting command: " + string(script))
			}
			return m[1]
		}
		id := field(`whose id is "([^"]+)"`)
		switch {
		case strings.Contains(string(script), "duplicate targetVM with properties"):
			req.Args = []string{"clone", id, "--name", field(`name:"([^"]+)"`)}
		case strings.Contains(string(script), "start targetVM"):
			req.Args = []string{"start", id}
		case strings.Contains(string(script), "stop targetVM by kill"):
			req.Args = []string{"stop", "--kill", id}
		case strings.Contains(string(script), "delete targetVM"):
			req.Args = []string{"delete", id}
		case strings.Contains(string(script), "reload configuration targetVM"):
			if !strings.Contains(string(script), "if status of targetVM is not stopped") {
				panic("reload lacks stopped guard")
			}
			if f.failReload {
				return core.LocalCommandResult{}, errors.New("reload failed")
			}
			for i := range f.vms {
				if f.vms[i].ID == id {
					if err := f.reloadBundle(ctx, &f.vms[i]); err != nil {
						return core.LocalCommandResult{}, err
					}
					return out("")
				}
			}
			return core.LocalCommandResult{}, errors.New("unknown reload UUID")
		default:
			panic("unexpected scripting mutation: " + string(script))
		}
	}
	f.actions = append(f.actions, req.Args[0])
	switch req.Args[0] {
	case "version", "--help":
		return out("UTM 5.0.8")
	case "clone":
		if req.Args[1] != sourceUUID || req.Args[2] != "--name" {
			panic("bad clone command")
		}
		f.next++
		cloned := preparedTemplate()
		cloned.ID = fmt.Sprintf("aaaaaaaa-aaaa-4aaa-8aaa-%012d", f.next)
		cloned.Name = req.Args[3]
		if err := f.createBundle(cloned); err != nil {
			return core.LocalCommandResult{}, err
		}
		f.vms = append(f.vms, cloned)
		if f.cancelClone != nil {
			f.cancelClone()
		}
		if f.failAfterClone {
			return core.LocalCommandResult{}, errors.New("clone client interrupted after server copy")
		}
		return out("")
	case "start":
		for i := range f.vms {
			if f.vms[i].ID == req.Args[1] {
				f.vms[i].Status = "started"
				return out("")
			}
		}
	case "stop":
		if len(req.Args) != 3 || req.Args[1] != "--kill" {
			panic("bad stop command")
		}
		for i := range f.vms {
			if f.vms[i].ID == req.Args[2] {
				f.vms[i].Status = "stopped"
				return out("")
			}
		}
	case "delete":
		if f.failDelete {
			return core.LocalCommandResult{}, errors.New("delete refused")
		}
		for i := range f.vms {
			if f.vms[i].ID == req.Args[1] {
				if req.Args[1] == sourceUUID || req.Args[1] == otherUUID {
					panic("deleting unrelated VM")
				}
				f.vms = append(f.vms[:i], f.vms[i+1:]...)
				return out("")
			}
		}
	}
	return core.LocalCommandResult{}, fmt.Errorf("unexpected control command: %v", req.Args)
}
func setup(t *testing.T) (*backend, *controlFixture) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := core.BaseConfig()
	cfg.Provider = "utm"
	cfg.TargetOS = core.TargetLinux
	cfg.Architecture = "arm64"
	cfg.UTM.Template = sourceUUID
	cfg.UTM.StorageRoot = t.TempDir()
	cfg.UTM.StartupTimeout = time.Second
	cfg.UTM.SSHKey = filepath.Join(t.TempDir(), "guest-key")
	if err := os.WriteFile(cfg.UTM.SSHKey, []byte("test credential placeholder"), 0600); err != nil {
		t.Fatal(err)
	}
	control := &controlFixture{storageRoot: cfg.UTM.StorageRoot, vms: []vm{preparedTemplate(), {ID: otherUUID, Name: "unrelated", Status: "started", Backend: "apple"}}}
	configured, err := (Provider{}).Configure(cfg, core.Runtime{Exec: control, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	b := configured.(*backend)
	b.hostOS = "darwin"
	b.hostArch = "arm64"
	original := waitForSSHReady
	waitForSSHReady = func(ctx context.Context, target *core.SSHTarget, _ io.Writer, _ string, _ time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if target.Host != "127.0.0.1" || len(target.FallbackPorts) != 0 || !strings.Contains(target.ReadyCheck, "/sys/class/net/") {
			return errors.New("missing exact guest identity check")
		}
		return nil
	}
	t.Cleanup(func() { waitForSSHReady = original })
	return b, control
}
func TestManagedLeaseLifecyclePreservesSourceAndRetainedIdentity(t *testing.T) {
	b, f := setup(t)
	ctx := context.Background()
	repo := core.Repo{Root: t.TempDir()}
	a, err := b.Acquire(ctx, core.AcquireRequest{Repo: repo, Keep: true})
	if err != nil {
		t.Fatal(err)
	}
	c, err := b.Acquire(ctx, core.AcquireRequest{Repo: repo, Keep: true})
	if err != nil {
		t.Fatal(err)
	}
	if a.Server.CloudID == sourceUUID || a.Server.CloudID == c.Server.CloudID || a.SSH.Port == c.SSH.Port {
		t.Fatalf("ambiguous identities: %#v / %#v", a, c)
	}
	records, err := b.records()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].MAC == records[1].MAC {
		t.Fatalf("network identity reused: %#v", records)
	}
	f.vms[2].Name = "operator renamed owned clone"
	reused, err := b.Resolve(ctx, core.ResolveRequest{ID: a.LeaseID, Repo: repo})
	if err != nil {
		t.Fatal(err)
	}
	if reused.Server.CloudID != a.Server.CloudID || reused.SSH.Port != a.SSH.Port {
		t.Fatal("retained resource changed")
	}
	if err = b.ReleaseLease(ctx, core.ReleaseLeaseRequest{Lease: reused}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{sourceUUID, otherUUID, c.Server.CloudID} {
		if _, ok := findVM(f.vms, id); !ok {
			t.Fatalf("unrelated VM %s removed", id)
		}
	}
	if _, ok := findVM(f.vms, a.Server.CloudID); ok {
		t.Fatal("released clone remains")
	}
	records, _ = b.records()
	if len(records) != 1 {
		t.Fatal("journal not removed after confirmed absence")
	}
}
func TestCloneFailureAfterCreationPreservesRecoverableUUID(t *testing.T) {
	b, f := setup(t)
	f.failAfterClone = true
	_, err := b.Acquire(context.Background(), core.AcquireRequest{})
	if err == nil {
		t.Fatal("expected clone error")
	}
	records, e := b.records()
	if e != nil || len(records) != 1 || records[0].VMID == "" || records[0].State != "recovery" {
		t.Fatalf("recovery journal=%#v err=%v", records, e)
	}
	if err = b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(f.vms) != 2 {
		t.Fatal("failed allocation clone not cleaned")
	}
}
func TestCanceledAcquisitionStillRecordsExactCloneBeforeReturning(t *testing.T) {
	b, f := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.cancelClone = cancel
	_, err := b.Acquire(ctx, core.AcquireRequest{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	records, _ := b.records()
	if len(records) != 1 || records[0].VMID == "" {
		t.Fatalf("lost canceled clone identity: %#v", records)
	}
	for _, action := range f.actions {
		if action == "start" {
			t.Fatal("booted clone after cancellation")
		}
	}
	if err = b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
}
func TestUnresolvedCloneNeverDeletesSourceOrByName(t *testing.T) {
	b, f := setup(t)
	f.hideClone = true
	_, err := b.Acquire(context.Background(), core.AcquireRequest{})
	if err == nil {
		t.Fatal("expected discovery error")
	}
	records, _ := b.records()
	if len(records) != 1 || records[0].VMID != "" {
		t.Fatal("unexpected discovery")
	}
	err = b.Cleanup(context.Background(), core.CleanupRequest{})
	if err == nil {
		t.Fatal("absence of unresolved identity should not erase recovery")
	}
	records, _ = b.records()
	if len(records) != 1 {
		t.Fatal("pending journal removed")
	}
	f.hideClone = false
	if err = b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(f.vms) != 2 {
		t.Fatal("unexpected remaining inventory")
	}
}
func TestFailedRemovalRemainsRetryable(t *testing.T) {
	b, f := setup(t)
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	f.failDelete = true
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err = b.ReleaseLease(ctx, core.ReleaseLeaseRequest{Lease: lease}); err == nil {
		t.Fatal("accepted failed deletion")
	}
	records, _ := b.records()
	if len(records) != 1 {
		t.Fatal("lost failed deletion journal")
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID); err != nil || !exists {
		t.Fatal("lost failed deletion claim")
	}
	f.failDelete = false
	if err = b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
}
func TestOwnershipMismatchAndReadOnlyDoctor(t *testing.T) {
	b, f := setup(t)
	result, err := b.Doctor(context.Background(), core.DoctorRequest{ProbeSSH: true})
	if err != nil || core.DoctorChecksStatus(result.Checks) != "ok" {
		t.Fatalf("doctor=%#v err=%v", result, err)
	}
	for _, c := range f.calls {
		if len(c.Args) > 0 && c.Args[0] != "--help" && c.Args[0] != "-l" {
			t.Fatalf("doctor mutated: %v", c.Args)
		}
	}
	records, _ := b.records()
	if len(records) != 0 {
		t.Fatal("doctor allocated journal")
	}
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{})
	if err != nil {
		t.Fatal(err)
	}
	lease.Server.CloudID = sourceUUID
	if err = b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err == nil {
		t.Fatal("accepted mismatched release identity")
	}
	f.vms[2].Configuration.Notes = "ownership removed"
	owned, _ := b.lookup(lease.LeaseID)
	lease.Server.CloudID = owned.VMID
	if err = b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err == nil {
		t.Fatal("deleted a clone with changed ownership marker")
	}
}
func TestTemplateAdmissionRejectsAmbiguousConfigurations(t *testing.T) {
	cases := map[string]func(*vm){"running": func(v *vm) { v.Status = "started" }, "apple": func(v *vm) { v.Backend = "apple" }, "shared": func(v *vm) { v.Configuration.Networks[0].Mode = "shared" }, "no-nic": func(v *vm) { v.Configuration.Networks = nil }, "two-nics": func(v *vm) { v.Configuration.Networks = append(v.Configuration.Networks, v.Configuration.Networks[0]) }, "qemu-args": func(v *vm) { v.Configuration.AdditionalArguments = []any{"-netdev user"} }, "unknown-arch": func(v *vm) { v.Configuration.Architecture = "riscv64" }}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			v := preparedTemplate()
			change(&v)
			if validateTemplate(v) == nil {
				t.Fatal("unsupported template admitted")
			}
		})
	}
}
func TestReadinessRequiresPlatformToolsAndMatchingMAC(t *testing.T) {
	b, _ := setup(t)
	r := record{MAC: "02:00:00:00:00:01", Architecture: "aarch64"}
	command := b.target(r, nil).SSH.ReadyCheck
	dir := t.TempDir()
	write := func(name, text string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+text+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	write("uname", `if [ "$1" = -s ]; then echo "$TEST_GUEST_OS"; else echo aarch64; fi`)
	write("cat", `echo "$TEST_GUEST_MAC"`)
	for _, tool := range []string{"bash", "tar", "rsync"} {
		write(tool, "exit 0")
	}
	run := func(osName, mac string) error {
		cmd := exec.Command("/bin/sh", "-c", command)
		cmd.Env = []string{"PATH=" + dir, "TEST_GUEST_OS=" + osName, "TEST_GUEST_MAC=" + mac}
		return cmd.Run()
	}
	if err := run("Linux", r.MAC); err != nil {
		t.Fatalf("valid guest rejected: %v", err)
	}
	if run("Darwin", r.MAC) == nil {
		t.Fatal("matching MAC bypassed guest OS")
	}
	if run("Linux", "02:00:00:00:00:02") == nil {
		t.Fatal("wrong guest MAC admitted")
	}
	if err := os.Remove(filepath.Join(dir, "rsync")); err != nil {
		t.Fatal(err)
	}
	if run("Linux", r.MAC) == nil {
		t.Fatal("missing sync tool admitted")
	}
}

func TestAcquiredCallbackFailureRollsBackExactClone(t *testing.T) {
	b, f := setup(t)
	_, err := b.Acquire(context.Background(), core.AcquireRequest{OnAcquired: func(lease core.LeaseTarget) error {
		if !validUUID(lease.Server.CloudID) || lease.Server.CloudID == sourceUUID {
			return errors.New("missing acquired identity")
		}
		return errors.New("caller refused allocation")
	}})
	if err == nil {
		t.Fatal("expected callback refusal")
	}
	records, _ := b.records()
	if len(records) != 0 || len(f.vms) != 2 {
		t.Fatalf("callback failure did not retire clone: journals=%#v vms=%#v", records, f.vms)
	}
}
func TestConcurrentAllocationsHaveDistinctRecordedNetworkIdentities(t *testing.T) {
	b, f := setup(t)
	const count = 3
	results := make(chan core.LeaseTarget, count)
	failures := make(chan error, count)
	for i := 0; i < count; i++ {
		go func() {
			lease, err := b.Acquire(context.Background(), core.AcquireRequest{Keep: true})
			results <- lease
			failures <- err
		}()
	}
	ids := map[string]bool{}
	ports := map[string]bool{}
	for i := 0; i < count; i++ {
		lease := <-results
		if err := <-failures; err != nil {
			t.Fatal(err)
		}
		if ids[lease.Server.CloudID] || ports[lease.SSH.Port] {
			t.Fatal("concurrent identity collision")
		}
		ids[lease.Server.CloudID] = true
		ports[lease.SSH.Port] = true
	}
	records, err := b.records()
	if err != nil {
		t.Fatal(err)
	}
	macs := map[string]bool{}
	for _, r := range records {
		if macs[r.MAC] {
			t.Fatal("concurrent MAC collision")
		}
		macs[r.MAC] = true
	}
	if len(f.vms) != count+2 {
		t.Fatal("concurrent clone inventory incomplete")
	}
}
func TestPendingRecoveryRejectsPreexistingMatchingNameAndMultipleNewMatches(t *testing.T) {
	b, _ := setup(t)
	token := strings.Repeat("a", 32)
	r := record{Version: 1, LeaseID: core.NewLeaseID(), AppPath: b.cfg.UTM.AppPath, SourceID: sourceUUID, OwnedName: "crabbox-utm-" + token, Token: token, Before: []string{sourceUUID, otherUUID}, Port: 2222, Architecture: "aarch64"}
	old := preparedTemplate()
	old.ID = otherUUID
	old.Name = r.OwnedName
	if err := b.reconcile(context.Background(), &r, []vm{old}); err == nil || r.VMID != "" {
		t.Fatal("bound preexisting VM by name")
	}
	a := old
	a.ID = "aaaaaaaa-aaaa-4aaa-8aaa-000000000001"
	c := a
	c.ID = "aaaaaaaa-aaaa-4aaa-8aaa-000000000002"
	if err := b.reconcile(context.Background(), &r, []vm{a, c}); err == nil || r.VMID != "" {
		t.Fatal("ambiguous discovery granted destructive ownership")
	}
}

func TestAuthenticatedReadinessFailureRetainsConfiguredCloneForDisposition(t *testing.T) {
	b, f := setup(t)
	waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
		return errors.New("authenticated NIC identity mismatch")
	}
	_, err := b.Acquire(context.Background(), core.AcquireRequest{})
	if err == nil || !strings.Contains(err.Error(), "identity mismatch") {
		t.Fatalf("err=%v", err)
	}
	records, err := b.records()
	if err != nil || len(records) != 1 || records[0].VMID == "" || !records[0].Configured || records[0].State != "recovery" {
		t.Fatalf("lost configured failure identity: %#v %v", records, err)
	}
	if err = b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(f.vms) != 2 {
		t.Fatal("readiness-failed clone not disposed")
	}
}
