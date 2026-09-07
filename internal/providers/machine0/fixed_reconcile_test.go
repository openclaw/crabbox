package machine0

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestMachine0FixedCurrentPreflightFailureRemainsHeldAcrossCommands(t *testing.T) {
	for _, command := range []string{"inspect", "status", "stop"} {
		t.Run(command, func(t *testing.T) {
			b, api, req := fixedMachine0TestFixture(t)
			api.sizes = nil
			_, err := b.Acquire(context.Background(), req)
			assertMachine0Exit(t, err, 2, "not in the live catalog")
			before := readFixedMachine0Claim(t, req.RequestedLeaseID)
			if before.CloudID != "" || len(before.FixedCreateIntent.Attempt)+len(api.created) != 0 {
				t.Fatal("current preflight submitted or bound a resource")
			}
			// We know this producer did not submit. A later process has only the
			// same persisted shape as a legacy record with an erased attempt.
			dir := configureMachine0CommandFixture(t, machine{}, "", "15m")
			script := "#!/bin/sh\ncase \"$*\" in 'ls --json') printf '[]\\n' ;; *) exit 2 ;; esac\n"
			if err := os.WriteFile(filepath.Join(dir, "machine0"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			_, err = runSelectionCLI(command, "--provider", providerName, "--id", req.RequestedLeaseID)
			assertMachine0Exit(t, err, 4, "retain its claim")
			if after := readFixedMachine0Claim(t, req.RequestedLeaseID); !reflect.DeepEqual(before, after) {
				t.Fatal("later command inferred cancellation from empty record")
			}
		})
	}
}

func TestMachine0FixedEmptyLegacyRecordRemainsHeld(t *testing.T) {
	for _, policy := range []string{"destroy", "suspend"} {
		t.Run(policy, func(t *testing.T) {
			b, api, req := fixedMachine0TestFixture(t)
			b.cfg.Machine0.ReleasePolicy = policy
			// v0.47 could erase a submitted attempt. This shape proves no absence.
			seedFixedMachine0PreparedClaim(t, b, req, nil)
			before := readFixedMachine0Claim(t, req.RequestedLeaseID)
			if _, err := b.Acquire(context.Background(), req); err == nil {
				t.Error("empty legacy record authorized another create")
			}
			if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: req.RequestedLeaseID}}); err == nil {
				t.Error("empty legacy record was canceled as never started")
			}
			if after := readFixedMachine0Claim(t, req.RequestedLeaseID); !reflect.DeepEqual(before, after) {
				t.Error("unknown legacy record changed")
			}
			if len(api.created)+len(api.started)+len(api.removed)+len(api.suspended) != 0 {
				t.Error("unknown legacy record mutated a native resource")
			}
		})
	}
}

func TestMachine0FixedReadinessRejectsFreshReplacementBeforeStart(t *testing.T) {
	for _, operation := range []string{"acquire", "resolve", "resume"} {
		t.Run(operation, func(t *testing.T) {
			b, api, req := fixedMachine0TestFixture(t)
			b.cfg.Machine0.ImageVersion = 1
			if _, err := b.Acquire(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			before := readFixedMachine0Claim(t, req.RequestedLeaseID)
			pending := api.machine
			pending.Status, pending.IP = "STOPPING", ""
			replacement := pending
			replacement.ID, replacement.Status = "vm-replacement", "STOPPED"
			ready := replacement
			ready.Status, ready.IP = "RUNNING", "203.0.113.99"
			api.getSequence = []machine{pending, replacement, ready}
			var err error
			if operation == "acquire" {
				_, err = b.Acquire(context.Background(), req)
			} else if operation == "resume" {
				err = b.Resume(context.Background(), ResumeRequest{ID: req.RequestedLeaseID})
			} else {
				_, err = b.Resolve(context.Background(), ResolveRequest{ID: req.RequestedLeaseID, Prepare: true})
			}
			if err == nil || len(api.started)+len(api.removed) != 0 {
				t.Fatalf("observed replacement reached mutation: err=%v starts=%v removes=%v", err, api.started, api.removed)
			}
			if after := readFixedMachine0Claim(t, req.RequestedLeaseID); !reflect.DeepEqual(before, after) {
				t.Error("observed replacement changed claim")
			}
		})
	}
}

func TestMachine0FixedPreparedDetailMustAttestBeforeBinding(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*machine)
	}{
		{"missing version", func(m *machine) { m.ImageVersion = 0 }},
		{"wrong version", func(m *machine) { m.ImageVersion++ }},
		{"wrong image", func(m *machine) { m.Image = "different-image" }},
		{"wrong size", func(m *machine) { m.Size = "different-size" }},
		{"wrong region", func(m *machine) { m.Region = "different-region" }},
		{"wrong ID", func(m *machine) { m.ID = "vm-replacement" }},
		{"wrong name", func(m *machine) { m.Name = "replacement" }},
		{"missing key", func(m *machine) { m.Key = nil }},
		{"wrong key", func(m *machine) { m.Key = &machineKey{Name: "another-key"} }},
		{"conflicting key type", func(m *machine) { m.Key = &machineKey{Name: "ci", Type: "MANAGED"} }},
		{"failed detail", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, api, req := fixedMachine0TestFixture(t)
			b.cfg.Machine0.Key, b.cfg.Machine0.ImageVersion = "ci", 1
			cfg := b.configForRun()
			attempt := machine0CreateAttempt{Name: machine0MachineName(req.RequestedLeaseID, req.RequestedSlug), Size: cfg.Machine0.Size, Region: cfg.Machine0.Region, Image: cfg.Machine0.Image, ImageVersion: 1, Key: "ci", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
			seedFixedMachine0PreparedClaim(t, b, req, &attempt)
			before := readFixedMachine0Claim(t, req.RequestedLeaseID)
			detail := fixedMachine0TestMachine(createMachineRequest{Name: attempt.Name, Size: attempt.Size, Region: attempt.Region, Image: attempt.Image, ImageVersion: 1})
			summary := detail
			summary.ImageVersion, summary.Key = 0, &machineKey{Name: "ci", Type: "PUBLIC"}
			api.machines = []machine{summary}
			if tc.mutate != nil {
				tc.mutate(&detail)
				api.machine = detail
			} else {
				api.getFn = func(context.Context, string) (machine, error) { return machine{}, errors.New("detail failed") }
			}
			if _, err := b.Resolve(context.Background(), ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true}); err == nil {
				t.Error("unattested preparation resolved")
			}
			if _, err := b.Acquire(context.Background(), req); err == nil {
				t.Error("unattested preparation acquired")
			}
			if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: req.RequestedLeaseID}}); err == nil {
				t.Error("unattested preparation released")
			}
			if !reflect.DeepEqual(before, readFixedMachine0Claim(t, req.RequestedLeaseID)) || len(api.created)+len(api.started)+len(api.removed)+len(api.primed) != 0 {
				t.Fatal("unattested detail changed ownership or mutated a resource")
			}
		})
	}
}

func TestMachine0OrdinaryReleaseByLeaseIDUsesResolvedName(t *testing.T) {
	b, api, req := fixedMachine0TestFixture(t)
	req.RequestedLeaseID = ""
	lease, err := b.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: lease.LeaseID}}); err != nil {
		t.Fatal(err)
	}
	if len(api.removed) != 1 || api.removed[0] != lease.Server.Name {
		t.Fatalf("ordinary release failed to remove resolved native name: %v", api.removed)
	}
}

func TestMachine0FixedReadyResolutionPublishesCurrentSnapshot(t *testing.T) {
	b, api, req := fixedMachine0TestFixture(t)
	if _, err := b.Acquire(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	lease, err := b.Resolve(context.Background(), ResolveRequest{ID: req.RequestedLeaseID, Prepare: true})
	if err != nil {
		t.Fatal(err)
	}
	current := readFixedMachine0Claim(t, req.RequestedLeaseID)
	snapshot, exists, set := core.ServerLeaseClaimSnapshot(lease.Server)
	if !set || !exists || !reflect.DeepEqual(current, snapshot) {
		t.Fatal("ready resolution returned a stale claim generation")
	}
	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err != nil || len(api.removed) != 1 {
		t.Fatalf("current readiness snapshot could not release: %v", err)
	}
}

func TestMachine0FixedReleaseRacingCreateRejectsStaleSnapshot(t *testing.T) {
	b, api, req := fixedMachine0TestFixture(t)
	creating, finishCreate := make(chan LeaseTarget, 1), make(chan struct{})
	create := api.createFn
	api.createFn = func(ctx context.Context, request createMachineRequest) error {
		data, err := os.ReadFile(filepath.Join(os.Getenv("XDG_STATE_HOME"), "crabbox", "claims", req.RequestedLeaseID+".json"))
		if err != nil {
			return err
		}
		var claim LeaseClaim
		if err := json.Unmarshal(data, &claim); err != nil {
			return err
		}
		lease := LeaseTarget{LeaseID: claim.LeaseID}
		core.SetServerLeaseClaimSnapshot(&lease.Server, claim, true)
		creating <- lease
		<-finishCreate
		return create(ctx, request)
	}
	acquired, released := make(chan error, 1), make(chan error, 1)
	go func() { _, err := b.Acquire(context.Background(), req); acquired <- err }()
	var lease LeaseTarget
	select {
	case lease = <-creating: // Current producer persisted the attempt under lock.
	case err := <-acquired:
		t.Fatalf("create callback did not observe the durable attempt: %v", err)
	}
	go func() { released <- b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}) }()
	close(finishCreate)
	if err := <-acquired; err != nil {
		t.Fatal(err)
	}
	if err := <-released; err == nil {
		t.Fatal("stale pre-binding snapshot released the acquired resource")
	}
	claim := readFixedMachine0Claim(t, req.RequestedLeaseID)
	if claim.CloudID != api.machine.ID || claim.FixedCreateIntent.State != fixedMachine0IntentAcquired || len(api.removed)+len(api.suspended) != 0 {
		t.Fatal("racing release damaged the authoritative create")
	}
}

func TestMachine0FixedEarlyBindingAllowsCleanupAfterLostReadiness(t *testing.T) {
	b, api, req := fixedMachine0TestFixture(t)
	b.cfg.Machine0.ImageVersion = 1
	reads := 0
	api.getFn = func(context.Context, string) (machine, error) {
		reads++
		if reads > 1 {
			return machine{}, errors.New("readiness result lost")
		}
		item := api.machine
		item.Status, item.IP = "CREATING", ""
		return item, nil
	}
	if _, err := b.Acquire(context.Background(), req); err == nil {
		t.Fatal("expected lost readiness result")
	}
	claim := readFixedMachine0Claim(t, req.RequestedLeaseID)
	if claim.CloudID != api.machine.ID || claim.CloudImmutableID != api.machine.ID {
		t.Fatal("first attested detail was not durably bound")
	}
	api.getFn = nil
	summary := api.machine
	summary.ImageVersion = 0 // Real native inventory omits the pinned version.
	api.machines = []machine{summary}
	lease, err := b.Resolve(context.Background(), ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	assertFixedMachine0Tombstone(t, readFixedMachine0Claim(t, req.RequestedLeaseID))
	if len(api.created) != 1 || len(api.removed) != 1 || len(api.started)+len(api.savedImages) != 0 {
		t.Fatal("cleanup required creation, start, or save")
	}
}

func TestMachine0FixedFinalReleaseObservationRejectsReplacement(t *testing.T) {
	for _, policy := range []string{"destroy", "suspend"} {
		for _, field := range []string{"id", "version", "key", "key type"} {
			t.Run(policy+"/"+field, func(t *testing.T) {
				b, api, req := fixedMachine0TestFixture(t)
				b.cfg.Machine0.ReleasePolicy, b.cfg.Machine0.ImageVersion, b.cfg.Machine0.Key = policy, 1, "ci"
				lease, err := b.Acquire(context.Background(), req)
				if err != nil {
					t.Fatal(err)
				}
				before := readFixedMachine0Claim(t, req.RequestedLeaseID)
				api.machine.Key = &machineKey{Name: "ci", Type: "PUBLIC"}
				api.machines = []machine{api.machine}
				reads := 0
				api.getFn = func(context.Context, string) (machine, error) {
					reads++
					item := api.machine
					if reads == 2 {
						switch field {
						case "id":
							item.ID = "replacement"
						case "version":
							item.ImageVersion = 2
						case "key":
							item.Key = &machineKey{Name: "another-key"}
						case "key type":
							item.Key = &machineKey{Name: "ci", Type: "MANAGED"}
						}
					}
					return item, nil
				}
				if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err == nil {
					t.Fatal("release accepted changed final detail")
				}
				if reads != 2 || len(api.removed)+len(api.suspended) != 0 || !reflect.DeepEqual(before, readFixedMachine0Claim(t, req.RequestedLeaseID)) {
					t.Fatal("final detail mismatch mutated resource or claim")
				}
			})
		}
	}
}

func TestMachine0FixedCaptureHoldBlocksCanonicalDestroy(t *testing.T) {
	b, api, req := fixedMachine0TestFixture(t)
	lease, err := b.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	err = core.WithDurableLeaseClaimLock(lease.LeaseID, func(claim *LeaseClaim, _ bool, persist func() error) error {
		claim.CheckpointCapture = &core.CheckpointCaptureBinding{ID: "chk_capture", Revision: claim.Revision, BoundRevision: claim.Revision}
		return persist()
	})
	if err != nil {
		t.Fatal(err)
	}
	before := readFixedMachine0Claim(t, lease.LeaseID)
	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err == nil {
		t.Fatal("ordinary stop cut through capture hold")
	}
	if err := b.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(api.removed)+len(api.started) != 0 || !reflect.DeepEqual(before, readFixedMachine0Claim(t, lease.LeaseID)) {
		t.Fatal("capture hold lost ownership or resource")
	}
}
