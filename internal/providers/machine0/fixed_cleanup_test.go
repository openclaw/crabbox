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

func TestMachine0ResumeRejectsTerminalFixedLease(t *testing.T) {
	for _, released := range []bool{false, true} {
		t.Run(fmt.Sprintf("released=%t", released), func(t *testing.T) {
			b, api, req := fixedMachine0TestFixture(t)
			lease, err := b.Acquire(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if released {
				if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err != nil {
					t.Fatal(err)
				}
			}
			api.machines = []machine{}
			before := readFixedMachine0Claim(t, req.RequestedLeaseID)
			starts := len(api.started)
			if err := b.Resume(context.Background(), ResumeRequest{ID: req.RequestedLeaseID}); err == nil {
				t.Fatal("resume reported success without a live resource")
			}
			if len(api.started) != starts || !reflect.DeepEqual(before, readFixedMachine0Claim(t, req.RequestedLeaseID)) {
				t.Fatal("terminal resume mutated provider or claim")
			}
		})
	}
}

func TestMachine0OrdinaryDestroyRechecksReservedCapture(t *testing.T) {
	for _, resourcePresent := range []bool{false, true} {
		t.Run(fmt.Sprintf("resource_present=%t", resourcePresent), func(t *testing.T) {
			b, api, req := fixedMachine0TestFixture(t)
			req.RequestedLeaseID = ""
			lease, err := b.Acquire(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			claim, exists, err := resolveClaim(lease.LeaseID)
			if err != nil || !exists {
				t.Fatalf("claim exists=%t err=%v", exists, err)
			}
			if err := core.AuthorizeCheckpointRelease(claim, ""); err != nil {
				t.Fatal(err)
			}
			// A reservation can appear after outer release admission without
			// changing the source revision. Finalization must recheck under lock.
			err = core.WithLeaseClaimUnchanged(claim.LeaseID, claim, func() error {
				dir := filepath.Join(os.Getenv("XDG_STATE_HOME"), "crabbox", "checkpoints", "chk_release_race")
				if err := os.MkdirAll(dir, 0o700); err != nil {
					return err
				}
				data, err := json.Marshal(map[string]any{"id": "chk_release_race", "kind": "machine0-image", "provider": providerName, "leaseId": claim.LeaseID, "capture": map[string]string{"sourceDisposition": "retire", "phase": "prepared"}})
				if err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(dir, "checkpoint.json"), data, 0o600)
			})
			if err != nil {
				t.Fatal(err)
			}
			if !resourcePresent {
				lease.Server.Name = ""
			}
			if err := b.destroyClaimedMachine(context.Background(), claim, lease); err == nil {
				t.Fatal("previous admission bypassed reserved capture")
			}
			after, exists, err := resolveClaim(claim.LeaseID)
			if err != nil || !exists || !reflect.DeepEqual(after, claim) || len(api.removed) != 0 {
				t.Fatalf("capture lost source/claim: exists=%t err=%v removes=%v", exists, err, api.removed)
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
				"sizes\x00--all\x00--json": {Stdout: string(sizes)}, "ls\x00--json": {Stdout: "[]"},
				"keys\x00ls\x00--json":        {Stdout: `[{"name":"ci","type":"MANAGED","isDefault":true}]`},
				"keys\x00get\x00ci\x00--json": {Stdout: `{"name":"ci","type":"MANAGED"}`},
				create:                        {Stdout: "VM is starting\n"}, get: {Stdout: string(data)},
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
		{name: "acquired absence lacks account authority", bound: true, acquired: true, absent: true, failure: "absence is unverified"},
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

func TestMachine0ResolveReclaimsExistingLease(t *testing.T) {
	for _, fixed := range []bool{false, true} {
		t.Run(fmt.Sprint(fixed), func(t *testing.T) {
			b, api, req := fixedMachine0TestFixture(t)
			if !fixed {
				req.RequestedLeaseID = ""
			}
			lease, err := b.Acquire(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			before, _, err := resolveClaim(lease.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			other := core.Repo{Root: t.TempDir()}
			resolved, err := b.Resolve(context.Background(), ResolveRequest{ID: lease.LeaseID, Repo: other, Reclaim: true, Prepare: true})
			if err != nil {
				t.Fatal(err)
			}
			after, _, err := resolveClaim(lease.LeaseID)
			if err != nil || after.RepoRoot != other.Root || after.CloudID != before.CloudID || !reflect.DeepEqual(after.FixedCreateIntent, before.FixedCreateIntent) || len(api.created) != 1 || resolved.Server.CloudID != before.CloudID {
				t.Fatalf("reclaim did not transfer exact lease: err=%v before=%+v after=%+v", err, before, after)
			}
			if _, err := b.Resolve(context.Background(), ResolveRequest{ID: lease.LeaseID, Repo: other, Prepare: true}); err != nil {
				t.Fatalf("new owner resolve: %v", err)
			}
		})
	}
}

func TestMachine0CheckpointAccountFencesAbsenceAndRelease(t *testing.T) {
	for _, tc := range []struct {
		name, stored, current string
		fail                  bool
	}{
		{"same", "fixture-account", "fixture-account", false},
		{"switched", "fixture-account", "other-account", true},
		{"legacy unbound", "", "fixture-account", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, api, req := fixedMachine0TestFixture(t)
			lease, err := b.Acquire(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			const checkpointID = "chk_account_fence"
			err = core.WithDurableLeaseClaimLock(lease.LeaseID, func(claim *core.LeaseClaim, _ bool, persist func() error) error {
				claim.CheckpointCapture = &core.CheckpointCaptureBinding{ID: checkpointID, Revision: claim.Revision}
				return persist()
			})
			if err != nil {
				t.Fatal(err)
			}
			claim, _, err := resolveClaim(lease.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			capture := core.NativeCheckpointCapture{SourceDisposition: "retire", Phase: "retiring", SourceID: claim.CloudID, SourceName: claim.Labels["machine0_name"], SourceScope: claim.ProviderScope, SourceRevision: claim.CheckpointCapture.Revision, SourceClaimedAt: claim.ClaimedAt}
			metadata := map[string]string{metadataAccountID: tc.stored}
			dir := filepath.Join(os.Getenv("XDG_STATE_HOME"), "crabbox", "checkpoints", checkpointID)
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(map[string]any{"id": checkpointID, "leaseId": claim.LeaseID, "kind": core.CheckpointKindMachine0, "provider": providerName, "capture": capture, "native": map[string]any{"metadata": metadata}})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "checkpoint.json")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			api.accountID = tc.current
			api.machines = []machine{}
			absent, err := b.CheckpointSourceAbsent(context.Background(), core.CheckpointSourceRequest{Capture: capture, Resource: core.NativeCheckpointResourceRequest{Metadata: metadata}})
			if (err != nil) != tc.fail || absent == tc.fail {
				t.Fatalf("absence=%t err=%v", absent, err)
			}
			err = b.releaseCheckpointSource(context.Background(), ReleaseLeaseRequest{Lease: lease, CheckpointID: checkpointID})
			if (err != nil) != tc.fail {
				t.Fatalf("release=%v", err)
			}
			after, _, err := resolveClaim(lease.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.fail && !reflect.DeepEqual(after, claim) {
				t.Fatal("refused retirement changed claim")
			}
			if !tc.fail {
				assertFixedMachine0Tombstone(t, after)
			}
			if len(api.removed) != 0 || len(api.started) != 0 || len(api.savedImages) != 0 {
				t.Fatal("absence proof mutated provider")
			}
			unchanged, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(unchanged, data) {
				t.Fatal("provider changed capture journal")
			}
		})
	}
}

func TestMachine0CaptureAccountBindingPrecedesEffects(t *testing.T) {
	for _, phase := range []string{"prepared", "stopping", "submitting", "pending", "ready", "failed"} {
		t.Run(phase, func(t *testing.T) {
			b, api, req := fixedMachine0TestFixture(t)
			lease, err := b.Acquire(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			const id = "chk_account_persist"
			if err := core.WithDurableLeaseClaimLock(lease.LeaseID, func(claim *core.LeaseClaim, _ bool, persist func() error) error {
				claim.CheckpointCapture = &core.CheckpointCaptureBinding{ID: id, Revision: claim.Revision}
				return persist()
			}); err != nil {
				t.Fatal(err)
			}
			claim, _, err := resolveClaim(lease.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			capture := &core.NativeCheckpointCapture{Phase: phase, SourceID: claim.CloudID, SourceName: claim.Labels["machine0_name"], SourceScope: claim.ProviderScope, SourceRevision: claim.CheckpointCapture.Revision, SourceClaimedAt: claim.ClaimedAt}
			persistErr := errors.New("durable account write failed")
			writes := 0
			_, err = b.advanceCheckpointCapture(context.Background(), core.NativeCheckpointCreateRequest{CheckpointID: id, Server: lease.Server, Capture: capture, Persist: func(result core.NativeCheckpointCreateResult) error {
				writes++
				if result.Metadata[metadataAccountID] != "fixture-account" || capture.Phase != "prepared" {
					t.Fatal("account not observed before first phase transition")
				}
				return persistErr
			}}, claim)
			if phase == "prepared" {
				if !errors.Is(err, persistErr) || writes != 1 {
					t.Fatalf("persistence=%v writes=%d", err, writes)
				}
			} else if err == nil || !strings.Contains(err.Error(), "account identity") || writes != 0 {
				t.Fatalf("unbound legacy phase adopted account: err=%v writes=%d", err, writes)
			}
			if len(api.stopped) != 0 || len(api.started) != 0 || len(api.savedImages) != 0 || len(api.removed) != 0 {
				t.Fatal("failed binding caused remote effect")
			}
		})
	}
}
