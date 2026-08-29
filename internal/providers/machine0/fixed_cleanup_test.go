package machine0

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestMachine0FixedReadinessFailureBindsIdentity(t *testing.T) {
	for _, phase := range []string{"terminal state", "later get failure", "SSH failure", "interrupted adoption"} {
		t.Run(phase, func(t *testing.T) {
			b, api, req := fixedMachine0TestFixture(t)
			failure := errors.New("readiness failed")
			if phase == "interrupted adoption" {
				cfg := b.configForRun()
				attempt := machine0CreateAttempt{Name: machine0MachineName(req.RequestedLeaseID, req.RequestedSlug), Size: cfg.Machine0.Size, Region: cfg.Machine0.Region, Image: cfg.Machine0.Image, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
				seedFixedMachine0PreparedClaim(t, b, req, &attempt)
				api.machine = fixedMachine0TestMachine(createMachineRequest{Name: attempt.Name, Size: attempt.Size, Region: attempt.Region, Image: attempt.Image})
				api.machine.Status, api.machine.IP = "CREATING", ""
				api.machines = []machine{api.machine}
			}
			reads := 0
			api.getFn = func(context.Context, string) (machine, error) {
				reads++
				if reads > 1 {
					return machine{}, failure
				}
				item := api.machine
				if phase == "terminal state" {
					item.Status, item.LastErrorMessage = "ERRORED", failure.Error()
				} else if phase == "later get failure" {
					item.Status, item.IP = "CREATING", ""
				}
				return item, nil
			}
			b.waitSSH = func(context.Context, *SSHTarget, time.Duration) error { return failure }
			if _, err := b.Acquire(context.Background(), req); err == nil || !strings.Contains(err.Error(), failure.Error()) {
				t.Fatalf("expected readiness failure, got %v", err)
			}
			claim := readFixedMachine0Claim(t, req.RequestedLeaseID)
			if claim.CloudID != api.machine.ID || claim.CloudImmutableID != api.machine.ID || claim.ProviderScope != machineScope(api.machine.ID) || claim.Labels["machine0_name"] != api.machine.Name || len(claim.FixedCreateIntent.Attempt) == 0 {
				t.Fatalf("observed resource was not durably bound before readiness failed: cloudID=%q immutableID=%q scope=%q labels=%v", claim.CloudID, claim.CloudImmutableID, claim.ProviderScope, claim.Labels)
			}
			api.machine.ID = "vm-substitute"
			api.machines = []machine{api.machine}
			creates := len(api.created)
			if _, err := b.Acquire(context.Background(), req); err == nil || len(api.created) != creates || len(api.removed) != 0 {
				t.Fatalf("replay accepted substitution or mutated resources: err=%v created=%d removed=%v", err, len(api.created), api.removed)
			}
		})
	}
}

func TestMachine0FixedNativeCreateResponseRemainsStoppable(t *testing.T) {
	for _, readFailure := range []bool{false, true} {
		t.Run(fmt.Sprintf("first_get_fails=%t", readFailure), func(t *testing.T) {
			b, _, req := fixedMachine0TestFixture(t)
			cfg := b.configForRun()
			item := fixedMachine0TestMachine(createMachineRequest{Name: machine0MachineName(req.RequestedLeaseID, req.RequestedSlug), Size: cfg.Machine0.Size, Region: cfg.Machine0.Region, Image: cfg.Machine0.Image})
			item.Status, item.IP, item.Key = "ERRORED", "", nil
			data, _ := json.Marshal(item)
			sizes, _ := json.Marshal([]machineSize{testSize()})
			get, create := "get\x00"+item.Name+"\x00--json", "new\x00"+item.Name+"\x00--size\x00"+item.Size+"\x00--region\x00"+item.Region+"\x00--image\x00"+item.Image
			runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
				"sizes\x00--all\x00--json": {Stdout: string(sizes)}, "ls\x00--json": {Stdout: "[]"}, "keys\x00ls\x00--json": {Stdout: "[]"},
				create: {Stdout: "VM is starting\n"}, get: {Stdout: string(data)},
			}}
			if readFailure {
				runner.responses[get] = core.LocalCommandResult{Stdout: "{"}
			}
			b.api = testClient(runner)
			if _, err := b.Acquire(context.Background(), req); err == nil {
				t.Fatal("expected post-create failure")
			}
			claim := readFixedMachine0Claim(t, req.RequestedLeaseID)
			if len(claim.FixedCreateIntent.Attempt) == 0 || (!readFailure && claim.CloudImmutableID != item.ID) {
				t.Fatalf("native allocation response lost recovery authority: %#v", claim)
			}
			runner.responses["ls\x00--json"] = core.LocalCommandResult{Stdout: "[" + string(data) + "]"}
			runner.responses[get] = core.LocalCommandResult{Stdout: string(data)}
			lease, err := b.Resolve(context.Background(), ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err != nil {
				t.Fatal(err)
			}
			assertFixedMachine0Tombstone(t, readFixedMachine0Claim(t, req.RequestedLeaseID))
			creates, removes := 0, 0
			for _, call := range runner.calls {
				switch call.Args[0] {
				case "new":
					creates++
				case "rm":
					removes++
					if strings.Join(call.Args, " ") != "rm "+item.Name+" --yes" {
						t.Fatalf("wrong native removal: %v", call.Args)
					}
				case "start", "ssh", "stop", "suspend":
					t.Fatalf("cleanup required runtime readiness: %v", call.Args)
				}
			}
			if creates != 1 || removes != 1 {
				t.Fatalf("creates=%d removes=%d", creates, removes)
			}
		})
	}
}

func TestMachine0FixedResolvedClaimReplacementFencesDeletion(t *testing.T) {
	b, api, req := fixedMachine0TestFixture(t)
	if _, err := b.Acquire(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	lease, err := b.Resolve(context.Background(), ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	claim := readFixedMachine0Claim(t, req.RequestedLeaseID)
	replacement := claim
	replacement.RepoRoot = t.TempDir()
	if err := core.ReplaceLeaseClaimIfUnchanged(claim.LeaseID, claim, replacement); err != nil {
		t.Fatal(err)
	}
	err = b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease})
	if err == nil || !strings.Contains(err.Error(), "claim changed") || len(api.removed) != 0 {
		t.Fatalf("stale resolve deleted replacement: err=%v removed=%v", err, api.removed)
	}
	if _, err := b.Resolve(context.Background(), ResolveRequest{ID: req.RequestedLeaseID, Repo: req.Repo}); err == nil {
		t.Fatal("resolve ignored repository binding")
	}
}

func TestMachine0FixedCleanupRetainsInvisiblePreparation(t *testing.T) {
	b, api, req := fixedMachine0TestFixture(t)
	b.waitSSH = func(context.Context, *SSHTarget, time.Duration) error { return errors.New("SSH failed") }
	if _, err := b.Acquire(context.Background(), req); err == nil {
		t.Fatal("expected interrupted acquisition")
	}
	before := readFixedMachine0Claim(t, req.RequestedLeaseID)
	api.machines = []machine{}
	if err := b.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if after := readFixedMachine0Claim(t, req.RequestedLeaseID); !reflect.DeepEqual(before, after) {
		t.Fatal("cleanup tombstoned a possibly pending creation")
	}
}

func TestMachine0FixedPreparedStopCommand(t *testing.T) {
	for _, tc := range []struct {
		name, state, failure                                                                string
		bound, acquired, absent, substitute, detailSubstitute, immutableMismatch, noAttempt bool
	}{
		{name: "creating", state: "CREATING"},
		{name: "stopped", state: "STOPPED"},
		{name: "suspended", state: "SUSPENDED"},
		{name: "errored", state: "ERRORED"},
		{name: "retained running", state: "RUNNING"},
		{name: "invisible attempt", absent: true, failure: "unresolved create attempt"},
		{name: "invisible observed preparation", bound: true, absent: true, failure: "unresolved create attempt"},
		{name: "authoritative acquired absence", bound: true, acquired: true, absent: true},
		{name: "same name replacement", bound: true, substitute: true, failure: "does not match acquired CloudID"},
		{name: "replacement before native removal", detailSubstitute: true, failure: "inventory resource identity"},
		{name: "immutable identity conflict", bound: true, immutableMismatch: true, failure: "inconsistent immutable identity"},
		{name: "provider scope conflict", bound: true, failure: "inconsistent immutable identity"},
		{name: "resource shape mismatch", failure: "durable create attempt"},
		{name: "selected key mismatch", failure: "durable selected SSH key"},
		{name: "name alone is not authority", noAttempt: true, failure: "no durable create attempt"},
		{name: "inventory unavailable", failure: "inventory failed"},
		{name: "inventory invalid JSON", failure: "parse machine0 ls"},
		{name: "remove failed", failure: "removal failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _, req := fixedMachine0TestFixture(t)
			if tc.name == "selected key mismatch" {
				b.cfg.Machine0.Key = "selected-key"
			}
			cfg := b.configForRun()
			attempt := machine0CreateAttempt{Name: machine0MachineName(req.RequestedLeaseID, req.RequestedSlug), Size: cfg.Machine0.Size, Region: cfg.Machine0.Region, Image: cfg.Machine0.Image, Key: cfg.Machine0.Key, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
			seedFixedMachine0PreparedClaim(t, b, req, &attempt)
			item := fixedMachine0TestMachine(createMachineRequest{Name: attempt.Name, Size: attempt.Size, Region: attempt.Region, Image: attempt.Image})
			item.Status, item.IP = blank(tc.state, "CREATING"), ""
			initial := readFixedMachine0Claim(t, req.RequestedLeaseID)
			replacement := initial
			intent := *initial.FixedCreateIntent
			replacement.FixedCreateIntent = &intent
			if tc.bound {
				replacement.CloudID, replacement.CloudImmutableID = item.ID, item.ID
				replacement.ProviderScope = machineScope(item.ID)
				replacement.Labels = machineLabels(cfg, item, req.RequestedLeaseID, req.RequestedSlug, false, time.Now())
			}
			if tc.acquired {
				intent.State = fixedMachine0IntentAcquired
				replacement.Labels["state"] = "ready"
			}
			if tc.name == "provider scope conflict" {
				replacement.ProviderScope = machineScope("other-resource")
			}
			if tc.immutableMismatch {
				replacement.CloudImmutableID = "vm-other"
			}
			if tc.noAttempt {
				intent.Attempt = nil
			}
			if err := core.ReplaceLeaseClaimIfUnchanged(initial.LeaseID, initial, replacement); err != nil {
				t.Fatal(err)
			}
			before := readFixedMachine0Claim(t, req.RequestedLeaseID)
			if tc.substitute {
				item.ID = "vm-substitute"
			}
			if tc.name == "resource shape mismatch" {
				item.Size = "other-size"
			}
			binDir := configureMachine0CommandFixture(t, item, "", "30m")
			callsPath, inventoryPath := filepath.Join(binDir, "calls"), filepath.Join(binDir, "inventory.json")
			if err := os.WriteFile(callsPath, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			inventory, _ := json.Marshal([]machine{item})
			if tc.absent {
				inventory = []byte("[]")
			}
			if tc.name == "inventory invalid JSON" {
				inventory = []byte("[")
			}
			if err := os.WriteFile(inventoryPath, inventory, 0o600); err != nil {
				t.Fatal(err)
			}
			detail := item
			if tc.detailSubstitute {
				detail.ID = "vm-substitute"
			}
			data, _ := json.Marshal(detail)
			listAction := fmt.Sprintf("/bin/cat %q", inventoryPath)
			if tc.name == "inventory unavailable" {
				listAction = "echo 'inventory failed' >&2; exit 1"
			}
			removeAction := fmt.Sprintf("printf '[]' > %q", inventoryPath)
			if tc.name == "remove failed" {
				removeAction = "echo 'removal failed' >&2; exit 1"
			}
			script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
case "$*" in
  'ls --json') %s ;;
  'get %s --json') printf '%%s\n' '%s' ;;
  'rm %s --yes') %s ;;
  *) echo 'unexpected native operation' >&2; exit 2 ;;
esac
`, callsPath, listAction, item.Name, data, item.Name, removeAction)
			if err := os.WriteFile(filepath.Join(binDir, "machine0"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			app := core.App{Stdout: &stdout, Stderr: &stderr}
			if tc.failure == "" {
				if err := app.Run(context.Background(), []string{"inspect", "--provider", providerName, "--id", req.RequestedSlug, "--json"}); err != nil || !strings.Contains(stdout.String(), item.ID) {
					t.Fatalf("prepared inspect failed: err=%v stdout=%s stderr=%s", err, stdout.String(), stderr.String())
				}
				var view struct {
					State string `json:"state"`
				}
				if err := json.Unmarshal(stdout.Bytes(), &view); err != nil || tc.absent && view.State != "deleted" {
					t.Fatalf("absent resource retained stale state: state=%q err=%v", view.State, err)
				}
				if after := readFixedMachine0Claim(t, req.RequestedLeaseID); !reflect.DeepEqual(before, after) {
					t.Fatal("read-only inspection changed claim")
				}
			}
			stop := []string{"stop", "--provider", providerName, "--id", req.RequestedLeaseID}
			err := app.Run(context.Background(), stop)
			if tc.failure != "" {
				if err == nil || !strings.Contains(err.Error(), tc.failure) {
					t.Fatalf("stop err=%v, want %q; stderr=%s", err, tc.failure, stderr.String())
				}
				after := readFixedMachine0Claim(t, req.RequestedLeaseID)
				if after.FixedCreateIntent.State == fixedMachine0IntentReleased || !reflect.DeepEqual(after.FixedCreateIntent, before.FixedCreateIntent) || after.RepoRoot != before.RepoRoot {
					t.Fatalf("failed stop lost durable ownership: %#v", after)
				}
			} else {
				if err != nil {
					t.Fatalf("stop failed: %v; stderr=%s", err, stderr.String())
				}
				assertFixedMachine0Tombstone(t, readFixedMachine0Claim(t, req.RequestedLeaseID))
				if err := app.Run(context.Background(), stop); err != nil {
					t.Fatalf("idempotent stop: %v", err)
				}
				if _, err := b.Acquire(context.Background(), req); err == nil {
					t.Fatal("released fixed ID was replayable")
				}
			}
			calls, err := os.ReadFile(callsPath)
			if err != nil {
				t.Fatal(err)
			}
			wantRemovals := 0
			if tc.failure == "" && !tc.absent || tc.name == "remove failed" {
				wantRemovals = 1
			}
			if strings.Count(string(calls), "rm "+item.Name+" --yes") != wantRemovals {
				t.Fatalf("native deletions differ from owned cleanup: calls=%s", calls)
			}
			for _, call := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
				if call == "" {
					continue
				}
				if call != "ls --json" && call != "get "+item.Name+" --json" && call != "rm "+item.Name+" --yes" {
					t.Fatalf("cleanup tried unrelated or readiness operation: %s", call)
				}
			}
		})
	}
}
