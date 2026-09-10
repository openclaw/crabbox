package daytona

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	api "github.com/daytonaio/daytona/libs/api-client-go"
	core "github.com/openclaw/crabbox/internal/cli"
)

func newFixedDaytonaFixture(t *testing.T) (*daytonaLifecycleFixture, *daytonaLeaseBackend, AcquireRequest) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX SSH executable fixture")
	}
	f, b, repo := newDaytonaLifecycleFixture(t)
	f.identityOrganization = "org-test"
	previousRecheck := fixedDaytonaAbsenceRecheckDelay
	fixedDaytonaAbsenceRecheckDelay = 5 * time.Millisecond
	t.Cleanup(func() { fixedDaytonaAbsenceRecheckDelay = previousRecheck })
	f.classSnapshot = &api.SnapshotDto{Id: "snapshot-exact-id", Name: "test-snapshot", State: api.SNAPSHOTSTATE_ACTIVE, Cpu: 1, Mem: 1, Disk: 3, RegionIds: []string{"us"}, Entrypoint: []string{}}
	f.classSnapshot.SetOrganizationId("org-test")
	f.classSnapshot.SetSandboxClass("container")
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	original := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/ssh-access") {
			w.Header().Set("Content-Type", "application/json")
			u, _ := url.Parse(f.server.URL)
			now := time.Now().UTC()
			_ = json.NewEncoder(w).Encode(api.NewSshAccessDto("access-fixture", "sandbox-test", "synthetic-ssh-token", now.Add(time.Hour), now, now, "ssh -p "+u.Port()+" synthetic-ssh-token@127.0.0.1"))
			return
		}
		original.ServeHTTP(w, r)
	})
	return f, b, AcquireRequest{Repo: repo, Keep: true, RequestedLeaseID: "cbx_012345abcdef", RequestedSlug: "fixed-project"}
}

func TestDaytonaFixedReplayAndTerminalOwnership(t *testing.T) {
	f, b, req := newFixedDaytonaFixture(t)
	first, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	beforeNonce, beforeFingerprint := f.sandbox.Labels["fixed_attempt"], f.sandbox.Labels["fixed_intent_sha256"]
	idle := 90 * time.Minute
	touched, err := b.Touch(t.Context(), TouchRequest{Lease: first, State: "ready", IdleTimeoutOverride: &idle})
	if err != nil || touched.Labels["fixed_attempt"] != beforeNonce || touched.Labels["fixed_intent_sha256"] != beforeFingerprint {
		t.Fatalf("heartbeat changed fixed ownership labels: %v", err)
	}
	// Rotating a credential within the same native organization must not change identity.
	b.cfg.Daytona.APIKey = "rotated-synthetic-credential"
	second, err := b.Acquire(t.Context(), req)
	if err != nil || second.LeaseID != req.RequestedLeaseID || second.Server.CloudID != first.Server.CloudID || f.sandboxCreates != 1 {
		t.Fatalf("replay changed allocation: first=%s second=%s creates=%d err=%v", first.LeaseID, second.LeaseID, f.sandboxCreates, err)
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
	if err != nil || !exists || claim.Provider != core.FixedDaytonaClaimProvider {
		t.Fatalf("missing fixed ownership: %v", err)
	}
	data, _ := json.Marshal(claim)
	if strings.Contains(string(data), "synthetic-ssh-token") || strings.Contains(string(data), "credential") {
		t.Fatal("credential persisted in fixed claim")
	}
	if err := b.ReleaseLease(t.Context(), ReleaseLeaseRequest{Lease: second}); err != nil {
		t.Fatal(err)
	}
	if err := b.Stop(t.Context(), StopRequest{ID: req.RequestedLeaseID}); err != nil {
		t.Fatal(err)
	}
	requestsAfterStop := len(f.paths)
	if _, err := b.Acquire(t.Context(), req); err == nil || f.sandboxCreates != 1 || f.deletes != 1 || len(f.paths) != requestsAfterStop {
		t.Fatalf("terminal lease replayed: err=%v creates=%d deletes=%d", err, f.sandboxCreates, f.deletes)
	}
}

func TestDaytonaFixedAmbiguousCreateDoesNotAllocateAgain(t *testing.T) {
	f, b, req := newFixedDaytonaFixture(t)
	f.createErrorStatus = http.StatusInternalServerError
	if _, err := b.Acquire(t.Context(), req); err == nil {
		t.Fatal("uncertain create unexpectedly succeeded")
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
	if err != nil || !exists || claim.FixedCreateIntent == nil || claim.FixedCreateIntent.Attempt["nonce"] == "" {
		t.Fatalf("submitted intent lost: %v", err)
	}
	f.createErrorStatus = 0
	restarted := &daytonaLeaseBackend{cfg: b.cfg, rt: b.rt}
	lease, err := restarted.Acquire(t.Context(), req)
	if err != nil || lease.LeaseID != req.RequestedLeaseID || f.sandboxCreates != 1 {
		t.Fatalf("restart resubmitted create: err=%v creates=%d", err, f.sandboxCreates)
	}
}

func TestDaytonaFixedPreparedReplayRechecksShape(t *testing.T) {
	for _, cleanupBound := range []bool{false, true} {
		t.Run(fmt.Sprintf("cleanup-bound=%t", cleanupBound), func(t *testing.T) {
			f, b, req := newFixedDaytonaFixture(t)
			f.responseMismatch = "response"
			if cleanupBound {
				f.createErrorStatus = http.StatusInternalServerError
			}
			if _, err := b.Acquire(t.Context(), req); err == nil {
				t.Fatal("invalid initial allocation succeeded")
			}
			if cleanupBound {
				f.deleteError = true
				if err := b.Stop(t.Context(), StopRequest{ID: req.RequestedLeaseID}); err == nil {
					t.Fatal("failed deletion unexpectedly succeeded")
				}
			}
			before, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
			if err != nil || !exists || before.CloudID == "" || before.FixedCreateIntent.State != "prepared" || cleanupBound && before.CloudImmutableID == "" {
				t.Fatalf("expected incomplete allocation with observed identity: %v", err)
			}
			if _, err := b.Acquire(t.Context(), req); err == nil {
				t.Fatal("retry accepted a sandbox whose initial shape attestation failed")
			}
			after, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
			if err != nil || !exists || after.FixedCreateIntent.State != "prepared" || f.sandboxCreates != 1 {
				t.Fatalf("failed shape retry published acquired or recreated: %v", err)
			}
		})
	}
}

func TestDaytonaFixedInterruptedAcquisitionNeedsPinnedSource(t *testing.T) {
	for _, scenario := range []string{"available", "retired", "changed ID", "changed name", "changed organization"} {
		t.Run(scenario, func(t *testing.T) {
			f, b, req := newFixedDaytonaFixture(t)
			original := f.server.Config.Handler
			f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/ssh-access") {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				original.ServeHTTP(w, r)
			})
			if _, err := b.Acquire(t.Context(), req); err == nil {
				t.Fatal("SSH interruption unexpectedly completed acquisition")
			}
			before, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
			if err != nil || !exists || before.FixedCreateIntent.State != "prepared" || before.CloudImmutableID == "" {
				t.Fatalf("interrupted shape-validated allocation was not retained: %v", err)
			}
			f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/snapshots/") {
					if r.URL.Path != "/snapshots/"+before.FixedCreateIntent.Attempt["snapshot_id"] {
						t.Errorf("incomplete retry resolved a mutable source selector: %s", r.URL.Path)
					}
					if scenario == "retired" {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					source := *f.classSnapshot
					switch scenario {
					case "changed ID":
						source.SetId("replacement-snapshot")
					case "changed name":
						source.SetName("replacement-name")
					case "changed organization":
						source.SetOrganizationId("another-org")
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(source)
					return
				}
				original.ServeHTTP(w, r)
			})
			lease, err := b.Acquire(t.Context(), req)
			if scenario != "available" {
				if err == nil {
					t.Fatal("incomplete acquisition bypassed its pinned source attestation")
				}
				if scenario == "retired" && (!strings.Contains(err.Error(), before.FixedCreateIntent.Attempt["snapshot_id"]) || !strings.Contains(err.Error(), "stop fixed lease "+req.RequestedLeaseID)) {
					t.Fatalf("missing source did not explain fixed-lease recovery: %v", err)
				}
				after, _, readErr := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
				if readErr != nil || after.FixedCreateIntent.State != "prepared" {
					t.Fatalf("retired source lost cleanup custody: %v", readErr)
				}
			} else if err != nil || lease.Server.CloudID != before.CloudID {
				t.Fatalf("same-resource recovery with its source failed: %v", err)
			}
			if f.sandboxCreates != 1 {
				t.Fatal("interrupted acquisition submitted another create")
			}
			if err := b.Stop(t.Context(), StopRequest{ID: req.RequestedLeaseID}); err != nil {
				t.Fatalf("incomplete acquisition could not be cleaned up: %v", err)
			}
		})
	}
}

func TestDaytonaFixedAPIKeyScopeAdmissionPreservesOrdinaryMode(t *testing.T) {
	for _, scenario := range []string{"empty inventory", "selected organization mismatch"} {
		t.Run(scenario, func(t *testing.T) {
			f, b, req := newFixedDaytonaFixture(t)
			if scenario == "empty inventory" {
				f.hideIdentitySandbox = true
			} else {
				b.cfg.Daytona.OrganizationID = "other-org"
			}
			if _, err := b.Acquire(t.Context(), req); err == nil || f.sandboxCreates != 0 {
				t.Fatalf("unattested scope allocated: err=%v creates=%d", err, f.sandboxCreates)
			}
			if _, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID); err != nil || exists {
				t.Fatalf("unattested scope published an intent: %v", err)
			}
			if scenario == "empty inventory" {
				if _, _, _, err := b.createDaytonaSandbox(t.Context(), req.Repo, true, false, "ordinary"); err != nil || f.sandboxCreates != 1 {
					t.Fatalf("ordinary API-key mode regressed: %v", err)
				}
			}
		})
	}
}

func TestDaytonaFixedPreparedIntentRecoveryBeforeSubmission(t *testing.T) {
	for _, scenario := range []string{"recover", "changed request", "expired", "submitted attempt"} {
		t.Run(scenario, func(t *testing.T) {
			f, b, req := newFixedDaytonaFixture(t)
			b.cfg.TTL = 30 * time.Minute
			client, err := newDaytonaClient(b.cfg, b.rt)
			if err != nil {
				t.Fatal(err)
			}
			scope, organization, err := fixedDaytonaContext(t.Context(), client)
			if err != nil {
				t.Fatal(err)
			}
			fingerprint, err := fixedDaytonaFingerprint(b.cfg, req, f.classSnapshot.GetId())
			if err != nil {
				t.Fatal(err)
			}
			createdAt := time.Now().UTC().Add(-10 * time.Minute)
			if scenario == "expired" {
				createdAt = time.Now().UTC().Add(-31 * time.Minute)
			}
			interrupted := errors.New("interrupted before attempt publication")
			_, err = core.AcquireFixedLease(core.FixedAcquireOptions{
				Kind: fixedDaytonaLeaseKind, LeaseID: req.RequestedLeaseID, RepoRoot: req.Repo.Root,
				TargetOS: targetLinux, TTL: b.cfg.TTL, IdleTimeout: b.cfg.IdleTimeout, Now: func() time.Time { return createdAt },
			}, func(context.Context, *core.LeaseClaim, bool) (core.FixedLeaseBinding, error) {
				return core.FixedLeaseBinding{ProviderScope: scope, Fingerprint: fingerprint, Slug: req.RequestedSlug}, nil
			}, func(_ context.Context, _ *core.LeaseClaim, intent *core.FixedCreateIntent, persist func() error) (LeaseTarget, error) {
				if scenario == "submitted attempt" {
					intent.Attempt = map[string]string{"organization": organization, "nonce": "submitted-but-unconfirmed"}
					if err := persist(); err != nil {
						return LeaseTarget{}, err
					}
				}
				return LeaseTarget{}, interrupted
			}, t.Context())
			if !errors.Is(err, interrupted) {
				t.Fatal(err)
			}
			before, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
			if err != nil || !exists || f.sandboxCreates != 0 {
				t.Fatalf("initial durable intent missing: %v", err)
			}
			if scenario == "changed request" {
				req.RequestedSlug = "different-request"
			}
			_, err = b.Acquire(t.Context(), req)
			after, exists, readErr := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
			if readErr != nil || !exists {
				t.Fatalf("recovery lost intent: %v", readErr)
			}
			if scenario == "recover" {
				if err != nil || f.sandboxCreates != 1 || after.FixedCreateIntent.CreatedAt != before.FixedCreateIntent.CreatedAt {
					t.Fatalf("unsubmitted recovery failed or reset deadline: %v", err)
				}
				minutes, ok := f.create.AdditionalProperties["ttlMinutes"].(float64)
				if !ok || minutes <= 0 || minutes > 20 {
					t.Fatalf("native TTL reset on recovery: %v", minutes)
				}
			} else {
				if err == nil || f.sandboxCreates != 0 {
					t.Fatalf("unsafe recovery submitted create: %v", err)
				}
				a, _ := json.Marshal(before)
				z, _ := json.Marshal(after)
				if string(a) != string(z) {
					t.Fatal("refused recovery changed durable intent")
				}
			}
		})
	}
}

func TestDaytonaFixedForkReplaySurvivesSourceRetirement(t *testing.T) {
	f, b, req := newFixedDaytonaFixture(t)
	req.RequestedCheckpointID = "chk_0123456789abcdef"
	req.CheckpointSource = &core.NativeCheckpointForkRecord{
		Kind: core.CheckpointKindDaytona, Direct: true, ImageID: f.classSnapshot.GetId(), Name: f.classSnapshot.GetName(),
		Metadata: map[string]string{"api_url": b.cfg.Daytona.APIURL, "organization": "org-test", "checkpoint": req.RequestedCheckpointID,
			"snapshot_id": f.classSnapshot.GetId(), "source": "original-source", "work_root": b.cfg.Daytona.WorkRoot, "user": "daytona", "target": "us"},
	}
	if err := (Provider{}).ApplyNativeCheckpointForkConfig(core.NativeCheckpointForkRequest{Config: &b.cfg, Record: *req.CheckpointSource}); err != nil {
		t.Fatal(err)
	}
	first, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	f.classSnapshot = nil
	f.hideIdentitySandbox = true
	inventoryReads := 0
	previousHandler := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/sandbox" {
			inventoryReads++
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"items":[],"nextCursor":null}`)
			return
		}
		previousHandler.ServeHTTP(w, r)
	})
	if err := (Provider{}).ApplyNativeCheckpointForkConfig(core.NativeCheckpointForkRequest{Config: &b.cfg, Record: *req.CheckpointSource}); err != nil {
		t.Fatal(err)
	}
	replayed, err := b.Acquire(t.Context(), req)
	if err != nil || replayed.Server.CloudID != first.Server.CloudID || f.sandboxCreates != 1 || inventoryReads != 0 {
		t.Fatalf("replay depended on retired snapshot: err=%v creates=%d", err, f.sandboxCreates)
	}
	req.CheckpointSource.Name = "changed-source"
	if _, err := b.Acquire(t.Context(), req); err == nil || f.sandboxCreates != 1 {
		t.Fatal("changed source bypassed the fixed checkpoint binding")
	}
}

func TestDaytonaFixedDrainedPoolReusesPrivateSnapshot(t *testing.T) {
	for _, scenario := range []string{"private", "general", "wrong organization"} {
		t.Run(scenario, func(t *testing.T) {
			f, b, req := newFixedDaytonaFixture(t)
			f.hideIdentitySandbox = true
			req.RequestedCheckpointID = "chk_0123456789abcdef"
			req.CheckpointSource = &core.NativeCheckpointForkRecord{
				Kind: core.CheckpointKindDaytona, Direct: true, ImageID: f.classSnapshot.GetId(), Name: f.classSnapshot.GetName(),
				Metadata: map[string]string{"api_url": b.cfg.Daytona.APIURL, "organization": "org-test", "checkpoint": req.RequestedCheckpointID,
					"snapshot_id": f.classSnapshot.GetId(), "source": "retired-source", "work_root": b.cfg.Daytona.WorkRoot, "user": "daytona", "target": "us"},
			}
			if err := (Provider{}).ApplyNativeCheckpointForkConfig(core.NativeCheckpointForkRequest{Config: &b.cfg, Record: *req.CheckpointSource}); err != nil {
				t.Fatal(err)
			}
			if scenario == "general" {
				f.classSnapshot.SetGeneral(true)
			}
			if scenario == "wrong organization" {
				f.classSnapshot.SetOrganizationId("other-org")
			}
			lease, err := b.Acquire(t.Context(), req)
			if scenario == "private" {
				if err != nil || lease.LeaseID != req.RequestedLeaseID || f.sandboxCreates != 1 {
					t.Fatalf("retained private snapshot could not refill a drained pool: %v", err)
				}
			} else if err == nil || f.sandboxCreates != 0 {
				t.Fatalf("unattested snapshot reached allocation: %v", err)
			}
		})
	}
}

func TestDaytonaFixedCleanupAcknowledgesDeletionBeforeTerminalWitness(t *testing.T) {
	f, b, req := newFixedDaytonaFixture(t)
	lease, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	// Native soft deletion keeps the renamed resource visible for a while.
	f.destroyingReads = 1000
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if err := b.ReleaseLease(ctx, ReleaseLeaseRequest{Lease: lease}); err == nil {
		t.Fatal("a still-visible resource retired the claim")
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
	if err != nil || !exists || claim.FixedCreateIntent.State == "released" || f.deletes != 1 ||
		claim.FixedCreateIntent.Attempt[fixedDaytonaDeletionAcknowledged] != "sandbox-test" || claim.FixedCreateIntent.Attempt[fixedDaytonaDeletionAcknowledged+"_at"] == "" {
		t.Fatalf("deletion acknowledgement was not durably recorded: claim=%+v err=%v", claim, err)
	}
	// An acknowledged deletion can no longer be replayed into a running lease.
	if _, err := b.Acquire(t.Context(), req); err == nil || f.sandboxCreates != 1 {
		t.Fatalf("acknowledged deletion was replayed: %v", err)
	}
	// After restart the resource is gone; the recorded acknowledgement makes
	// that 404 terminal without another DELETE.
	f.destroyingReads = 0
	restarted := &daytonaLeaseBackend{cfg: b.cfg, rt: b.rt}
	if err := restarted.Stop(t.Context(), StopRequest{ID: req.RequestedLeaseID}); err != nil {
		t.Fatal(err)
	}
	claim, exists, err = core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
	if err != nil || !exists || claim.FixedCreateIntent.State != "released" || f.deletes != 1 {
		t.Fatalf("acknowledged terminal reconciliation failed or deleted twice: %v", err)
	}
}

func TestDaytonaFixedCleanupPersistsUUIDBeforeLostDeleteResponse(t *testing.T) {
	f, b, req := newFixedDaytonaFixture(t)
	f.createErrorStatus = http.StatusInternalServerError
	if _, err := b.Acquire(t.Context(), req); err == nil {
		t.Fatal("create response loss unexpectedly succeeded")
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
	if err != nil || !exists || claim.CloudID != "" || f.sandboxCreates != 1 {
		t.Fatalf("expected a submitted attempt without a returned UUID: %v", err)
	}
	f.hideIdentitySandbox = true
	f.deleteErrorAfterApply = true
	if err := b.Stop(t.Context(), StopRequest{ID: req.RequestedLeaseID}); err == nil {
		t.Fatal("lost deletion response must remain unresolved")
	}
	claim, exists, err = core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
	if err != nil || !exists || claim.CloudID != "sandbox-test" || claim.CloudImmutableID != claim.CloudID || claim.FixedCreateIntent.State == "released" ||
		claim.FixedCreateIntent.Attempt[fixedDaytonaDeletionAcknowledged] != "" || f.deletes != 1 {
		t.Fatalf("first observed UUID was not retained before deletion: claim=%+v err=%v", claim, err)
	}
	// GET now hides the destroyed resource. A bare 404 without organization
	// context is not deletion proof; custody stays with the claim.
	restarted := &daytonaLeaseBackend{cfg: b.cfg, rt: b.rt}
	if err := restarted.Stop(t.Context(), StopRequest{ID: req.RequestedLeaseID}); err == nil {
		t.Fatal("bare 404 retired the claim without organization context")
	}
	retained, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
	if a, z := mustJSON(t, claim), mustJSON(t, retained); err != nil || !exists || a != z || f.deletes != 1 {
		t.Fatalf("unverified 404 changed custody: %v", err)
	}
	// With the organization re-established, an authorized exact-attempt search
	// that finds no live resource is the supported terminal witness.
	f.hideIdentitySandbox = false
	if err := restarted.Stop(t.Context(), StopRequest{ID: req.RequestedLeaseID}); err != nil {
		t.Fatal(err)
	}
	claim, exists, err = core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
	if err != nil || !exists || claim.FixedCreateIntent.State != "released" || f.sandboxCreates != 1 || f.deletes != 1 {
		t.Fatalf("fresh cleanup resubmitted or lost terminal custody: %v", err)
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestDaytonaFixedCleanupResolvesUnknownUUIDFromLiveInventory(t *testing.T) {
	for _, scenario := range []string{"live", "destroying", "absent", "absent without organization", "duplicate", "cursor", "wrong organization", "wrong snapshot", "wrong user", "empty target"} {
		t.Run(scenario, func(t *testing.T) {
			f, b, req := newFixedDaytonaFixture(t)
			f.createErrorStatus = http.StatusInternalServerError
			if _, err := b.Acquire(t.Context(), req); err == nil {
				t.Fatal("lost create response unexpectedly succeeded")
			}
			before, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
			if err != nil || !exists || before.CloudID != "" || before.FixedCreateIntent.State != "prepared" {
				t.Fatalf("expected submitted attempt without UUID: %v", err)
			}
			// Native deletion or expiry renames the sandbox; the attempt name is unusable.
			original := f.server.Config.Handler
			f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == "/sandbox/"+before.FixedCreateIntent.Attempt["name"] {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				original.ServeHTTP(w, r)
			})
			switch scenario {
			case "destroying":
				f.sandbox.SetState(api.SANDBOXSTATE_DESTROYING)
				f.sandbox.SetDesiredState(api.SANDBOXDESIREDSTATE_DESTROYED)
				f.destroyingReads = 1000
			case "absent":
				f.sandbox.SetState(api.SANDBOXSTATE_DESTROYED)
				f.sandbox.SetDesiredState(api.SANDBOXDESIREDSTATE_DESTROYED)
			case "absent without organization":
				f.sandbox.SetState(api.SANDBOXSTATE_DESTROYED)
				f.sandbox.SetDesiredState(api.SANDBOXDESIREDSTATE_DESTROYED)
				f.hideIdentitySandbox = true
			case "duplicate":
				f.duplicateAttemptInventory = true
			case "cursor":
				f.attemptInventoryCursor = "another-page"
			case "wrong organization":
				f.sandbox.SetOrganizationId("another-org")
			case "wrong snapshot":
				f.sandbox.SetSnapshot("another-snapshot")
			case "wrong user":
				f.sandbox.SetUser("another-user")
			case "empty target":
				f.sandbox.SetTarget("")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			err = b.Stop(ctx, StopRequest{ID: req.RequestedLeaseID})
			cancel()
			after, exists, readErr := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
			if readErr != nil || !exists || f.sandboxCreates != 1 {
				t.Fatalf("inventory resolution changed native resources or lost custody: %v", readErr)
			}
			switch scenario {
			case "live":
				if err != nil || after.FixedCreateIntent.State != "released" || f.deletes != 1 {
					t.Fatalf("live exact attempt was not adopted and deleted: %v", err)
				}
			case "destroying":
				if err == nil || after.CloudID != f.sandbox.GetId() || after.CloudImmutableID != after.CloudID || after.FixedCreateIntent.State != "prepared" ||
					after.FixedCreateIntent.Attempt[fixedDaytonaDeletionAcknowledged] != after.CloudID || f.deletes != 0 {
					t.Fatalf("observed destruction was not durably bound and acknowledged: %v", err)
				}
				f.destroyingReads = 0
				restarted := &daytonaLeaseBackend{cfg: b.cfg, rt: b.rt}
				if err := restarted.Stop(t.Context(), StopRequest{ID: req.RequestedLeaseID}); err != nil {
					t.Fatalf("acknowledged terminal recovery after restart failed: %v", err)
				}
				if final, _, _ := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID); final.FixedCreateIntent.State != "released" || f.deletes != 0 {
					t.Fatal("acknowledged native destruction was deleted again or not finalized")
				}
			case "absent":
				// The list index is eventually consistent; a never-observed UUID
				// cannot be finalized from an empty search.
				if err == nil || mustJSON(t, before) != mustJSON(t, after) || f.deletes != 0 || !strings.Contains(err.Error(), "absence is unverified") {
					t.Fatalf("empty inventory finalized a never-observed attempt: %v", err)
				}
			default:
				if err == nil || mustJSON(t, before) != mustJSON(t, after) || f.deletes != 0 {
					t.Fatalf("unverified inventory changed custody: %v", err)
				}
			}
		})
	}
}

func TestDaytonaFixtureRejectsDestroyedStateQueries(t *testing.T) {
	f, _, _ := newFixedDaytonaFixture(t)
	// Daytona v0.190.0 validates list states against every state except
	// destroyed; the fixture must model that rejection so no cleanup path can
	// depend on a destroyed-state inventory row.
	response, err := http.Get(f.server.URL + "/sandbox?states=destroyed&limit=1")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "each value must be one of the following values") {
		t.Fatalf("fixture accepted a destroyed-state query: %d %s", response.StatusCode, body)
	}
}

func TestDaytonaFixedNativeLabelsCannotRecreateLostClaim(t *testing.T) {
	f, b, req := newFixedDaytonaFixture(t)
	lease, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	core.RemoveLeaseClaim(req.RequestedLeaseID)
	if _, err := b.Touch(t.Context(), TouchRequest{Lease: lease, State: "ready"}); err == nil {
		t.Fatal("native labels authorized touch without a durable owner")
	}
	if _, err := b.Resolve(t.Context(), ResolveRequest{ID: req.RequestedLeaseID, Repo: req.Repo, Reclaim: true}); err == nil {
		t.Fatal("ordinary reclaim recreated a fixed owner from native labels")
	}
	if err := b.Stop(t.Context(), StopRequest{ID: req.RequestedLeaseID}); err == nil {
		t.Fatal("native labels authorized deletion without a durable owner")
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID); err != nil || exists || f.deletes != 0 || f.activity != 0 {
		t.Fatalf("orphaned fixed resource was mutated or adopted: %v", err)
	}
}

func TestDaytonaFixedReclaimPublishesRepositoryBeforeUse(t *testing.T) {
	for _, caller := range []string{"ssh", "toolbox"} {
		t.Run(caller, func(t *testing.T) {
			_, b, req := newFixedDaytonaFixture(t)
			lease, err := b.Acquire(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			before, _, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
			if err != nil {
				t.Fatal(err)
			}
			nextRepo := Repo{Root: t.TempDir(), Name: "another-project"}
			use := func(repo Repo, reclaim bool) error {
				if caller == "ssh" {
					_, err := b.Resolve(t.Context(), ResolveRequest{ID: req.RequestedLeaseID, Repo: repo, Reclaim: reclaim})
					return err
				}
				_, err := b.Run(t.Context(), RunRequest{ID: req.RequestedLeaseID, Repo: repo, Reclaim: reclaim, NoSync: true, Command: []string{"true"}})
				return err
			}
			if err := use(nextRepo, false); err == nil {
				t.Fatal("another repository used the lease without reclaim")
			}
			if err := use(nextRepo, true); err != nil {
				t.Fatal(err)
			}
			after, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
			if err != nil || !exists || after.RepoRoot != nextRepo.Root || after.CloudID != lease.Server.CloudID || after.ProviderScope != before.ProviderScope {
				t.Fatalf("reclaim was not persisted against the same native owner: %v", err)
			}
			a, _ := json.Marshal(before.FixedCreateIntent)
			z, _ := json.Marshal(after.FixedCreateIntent)
			if string(a) != string(z) {
				t.Fatal("repository transfer changed the fixed create intent")
			}
			if err := use(req.Repo, false); err == nil {
				t.Fatal("previous repository retained access after reclaim")
			}
		})
	}
}

func TestDaytonaFixedHeartbeatDoesNotRequireExecutionRepository(t *testing.T) {
	f, b, req := newFixedDaytonaFixture(t)
	if _, err := b.Acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	before, _, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(config, []byte(fmt.Sprintf("provider: daytona\nnetwork: public\ndaytona:\n  apiUrl: %q\n", b.cfg.Daytona.APIURL)), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", config)
	t.Setenv("CRABBOX_DAYTONA_API_KEY", b.cfg.Daytona.APIKey)
	t.Chdir(t.TempDir())
	var stdout, stderr bytes.Buffer
	err = (core.App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), []string{"heartbeat", "--provider", "daytona", "--id", req.RequestedLeaseID, "--json"})
	if err != nil {
		t.Fatalf("repository-less heartbeat failed: %v", err)
	}
	after, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
	if err != nil || !exists || after.RepoRoot != before.RepoRoot || f.activity != 1 {
		t.Fatalf("heartbeat changed execution ownership or did not touch native activity: %v", err)
	}
	a, _ := json.Marshal(before.FixedCreateIntent)
	z, _ := json.Marshal(after.FixedCreateIntent)
	if string(a) != string(z) {
		t.Fatal("heartbeat changed fixed intent")
	}
	if _, err := b.Resolve(t.Context(), ResolveRequest{ID: req.RequestedLeaseID}); err == nil {
		t.Fatal("repository-less execution resolution was admitted")
	}
}

func TestDaytonaFixedScopeAndResourceDriftPreserveClaim(t *testing.T) {
	for _, drift := range []string{"response organization", "selected organization", "nonce", "uuid", "unverified deletion"} {
		t.Run(drift, func(t *testing.T) {
			f, b, req := newFixedDaytonaFixture(t)
			lease, err := b.Acquire(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			before, _, _ := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
			switch drift {
			case "response organization":
				f.sandbox.SetOrganizationId("other-org")
			case "selected organization":
				b.cfg.Daytona.OrganizationID = "other-org"
			case "nonce":
				f.sandbox.Labels["fixed_attempt"] = "different-attempt"
			case "uuid":
				f.sandbox.Id = "other-sandbox"
			case "unverified deletion":
				f.deleteUnacknowledged = true
			}
			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer cancel()
			if err := b.ReleaseLease(ctx, ReleaseLeaseRequest{Lease: lease}); err == nil {
				t.Fatal("unverified release succeeded")
			}
			after, exists, _ := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
			a, _ := json.Marshal(before)
			z, _ := json.Marshal(after)
			if !exists || string(a) != string(z) {
				t.Fatal("failed cleanup changed durable ownership")
			}
			if drift != "unverified deletion" && f.deletes != 0 {
				t.Fatal("deleted after ownership drift")
			}
		})
	}
}

func TestDaytonaFixedCleanupRefusesChangedClaimBeforeDeletion(t *testing.T) {
	f, b, req := newFixedDaytonaFixture(t)
	if _, err := b.Acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	stale, _, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	// Another owner transfers the repository between the release read and the
	// fenced DELETE; the stale snapshot must not reach the native call.
	transferred := stale
	transferred.RepoRoot = t.TempDir()
	transferred, err = core.ReplaceLeaseClaimIfUnchangedDurableReturning(stale.LeaseID, stale, transferred)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.releaseFixed(t.Context(), stale, ""); err == nil {
		t.Fatal("stale claim snapshot released the lease")
	}
	after, _, _ := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
	if f.deletes != 0 || mustJSON(t, after) != mustJSON(t, transferred) {
		t.Fatalf("changed claim was deleted or mutated: deletes=%d", f.deletes)
	}
	if err := b.releaseFixed(t.Context(), transferred, ""); err != nil {
		t.Fatal(err)
	}
	if final, _, _ := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID); final.FixedCreateIntent.State != "released" || f.deletes != 1 {
		t.Fatalf("current owner could not release: deletes=%d", f.deletes)
	}
}

func TestDaytonaFixedAcknowledgedCleanupRevalidatesOrganization(t *testing.T) {
	f, b, req := newFixedDaytonaFixture(t)
	lease, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	f.destroyingReads = 1000
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if err := b.ReleaseLease(ctx, ReleaseLeaseRequest{Lease: lease}); err == nil {
		t.Fatal("a still-visible resource retired the claim")
	}
	claim, _, _ := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
	if claim.FixedCreateIntent.Attempt[fixedDaytonaDeletionAcknowledged] != "sandbox-test" || f.deletes != 1 {
		t.Fatalf("deletion acknowledgement missing: %+v", claim)
	}
	// The resource is gone now, but other credentials cannot turn that 404
	// into completion: absence is only meaningful under the claim's organization.
	f.destroyingReads = 0
	cfg := b.cfg
	cfg.Daytona.OrganizationID = "other-org"
	drifted := &daytonaLeaseBackend{cfg: cfg, rt: b.rt}
	if err := drifted.Stop(t.Context(), StopRequest{ID: req.RequestedLeaseID}); err == nil {
		t.Fatal("foreign organization retired an acknowledged claim")
	}
	after, _, _ := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
	if mustJSON(t, after) != mustJSON(t, claim) || f.deletes != 1 {
		t.Fatal("organization drift changed custody")
	}
	restarted := &daytonaLeaseBackend{cfg: b.cfg, rt: b.rt}
	if err := restarted.Stop(t.Context(), StopRequest{ID: req.RequestedLeaseID}); err != nil {
		t.Fatal(err)
	}
	if final, _, _ := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID); final.FixedCreateIntent.State != "released" || f.deletes != 1 {
		t.Fatalf("acknowledged terminal reconciliation failed: deletes=%d", f.deletes)
	}
}

func TestDaytonaFixedCleanupRetainsErroredPendingDeletion(t *testing.T) {
	for _, phase := range []string{"before acknowledgement", "after acknowledgement"} {
		t.Run(phase, func(t *testing.T) {
			f, b, req := newFixedDaytonaFixture(t)
			lease, err := b.Acquire(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			if phase == "after acknowledgement" {
				f.destroyingReads = 1000
				ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
				err = b.ReleaseLease(ctx, ReleaseLeaseRequest{Lease: lease})
				cancel()
				if err == nil {
					t.Fatal("a still-visible resource retired the claim")
				}
				f.destroyingReads = 0
			}
			// Native destruction errored: GET answers 404, but the errored resource
			// remains in inventory with a pending deletion.
			f.sandbox.SetState(api.SANDBOXSTATE_ERROR)
			f.sandbox.SetDesiredState(api.SANDBOXDESIREDSTATE_DESTROYED)
			before, _, _ := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
			restarted := &daytonaLeaseBackend{cfg: b.cfg, rt: b.rt}
			err = restarted.Stop(t.Context(), StopRequest{ID: req.RequestedLeaseID})
			after, _, _ := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
			if err == nil || !strings.Contains(err.Error(), "pending deletion") || after.FixedCreateIntent.State == "released" {
				t.Fatalf("errored pending deletion retired the claim: %v", err)
			}
			if phase == "before acknowledgement" && (mustJSON(t, before) != mustJSON(t, after) || f.deletes != 0) {
				t.Fatalf("errored resource changed custody or was deleted: deletes=%d", f.deletes)
			}
			// Once Daytona finishes the destruction, the same claim finalizes.
			f.sandbox.SetState(api.SANDBOXSTATE_DESTROYED)
			if err := restarted.Stop(t.Context(), StopRequest{ID: req.RequestedLeaseID}); err != nil {
				t.Fatal(err)
			}
			if final, _, _ := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID); final.FixedCreateIntent.State != "released" {
				t.Fatal("destroyed resource did not finalize")
			}
		})
	}
}
