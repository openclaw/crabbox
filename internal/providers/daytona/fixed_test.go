package daytona

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// Interrupt only after the provider has durably acknowledged deletion. Closing
// the witness channel also joins fixture access before the next simulated state.
func interruptDaytonaFixedCleanup(t *testing.T, f *daytonaLifecycleFixture, leaseID string, action func(context.Context) error) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	observed := make(chan struct{})
	observations := 0
	original := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		original.ServeHTTP(w, r)
		f.mu.Lock()
		visibleDetail := r.URL.Path == "/sandbox/sandbox-test" && !hiddenDaytonaDeletion(f.sandbox)
		f.mu.Unlock()
		if r.Method == http.MethodGet && (visibleDetail || r.URL.Path == "/sandbox/paginated") {
			claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
			if err == nil && exists && claim.FixedCreateIntent != nil && claim.FixedCreateIntent.Attempt["deletion_acknowledged_id"] == claim.CloudID {
				observations++
				// Let one pending observation reach the caller. An implementation
				// that incorrectly retires it must fail before this second read.
				if observations == 2 {
					close(observed)
					cancel()
				}
			}
		}
	})
	err := action(ctx)
	select {
	case <-observed:
	case <-ctx.Done():
		select {
		case <-observed:
		default:
			t.Fatalf("cleanup did not reach its durable acknowledgment: %v", err)
		}
	}
	f.server.Config.Handler = original
	return err
}

func TestDaytonaFixedLastSandboxDeletionReconcilesAfterRestart(t *testing.T) {
	f, b, req := newFixedDaytonaFixture(t)
	lease, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	f.hideIdentitySandbox = true
	f.deletionPending = true
	if err := interruptDaytonaFixedCleanup(t, f, req.RequestedLeaseID, func(ctx context.Context) error {
		return b.ReleaseLease(ctx, ReleaseLeaseRequest{Lease: lease})
	}); err == nil {
		t.Fatal("deletion acknowledgment alone retired the claim")
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
	if err != nil || !exists || claim.FixedCreateIntent.State == "released" || f.deletes != 1 ||
		claim.FixedCreateIntent.Attempt["deletion_acknowledged_id"] != lease.Server.CloudID ||
		claim.FixedCreateIntent.Attempt["deletion_indexed_id"] != lease.Server.CloudID {
		t.Fatalf("interrupted cleanup lost its durable witness: %v", err)
	}
	// Native deletion finishes after the process exits. No live sandbox is left
	// to attest the account, so recovery authenticates the persisted organization.
	f.sandbox.SetState(api.SANDBOXSTATE_DESTROYED)
	b.cfg.Daytona.APIKey = "rotated-synthetic-credential"
	restarted := &daytonaLeaseBackend{cfg: b.cfg, rt: b.rt}
	if err := restarted.Stop(t.Context(), StopRequest{ID: req.RequestedLeaseID}); err != nil {
		t.Fatal(err)
	}
	claim, exists, err = core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
	if err != nil || !exists || claim.FixedCreateIntent.State != "released" || f.deletes != 1 {
		t.Fatalf("terminal reconciliation failed or deleted twice: %v", err)
	}
}

func TestDaytonaFixedCleanupPersistsUUIDBeforeLostDeleteResponse(t *testing.T) {
	for _, remainsVisible := range []bool{false, true} {
		t.Run(fmt.Sprintf("visible=%t", remainsVisible), func(t *testing.T) {
			f, b, req := newFixedDaytonaFixture(t)
			f.createErrorStatus = http.StatusInternalServerError
			if _, err := b.Acquire(t.Context(), req); err == nil {
				t.Fatal("create response loss unexpectedly succeeded")
			}
			f.hideIdentitySandbox = true
			f.deleteErrorAfterApply = true
			f.deletionPending = remainsVisible
			if err := b.Stop(t.Context(), StopRequest{ID: req.RequestedLeaseID}); err == nil {
				t.Fatal("lost deletion response must remain unresolved")
			}
			claim, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
			if err != nil || !exists || claim.CloudID != "sandbox-test" || claim.CloudImmutableID != claim.CloudID ||
				claim.FixedCreateIntent.Attempt["deletion_acknowledged_id"] != "" || f.deletes != 1 {
				t.Fatalf("first UUID or ambiguous deletion custody lost: %+v %v", claim, err)
			}
			restarted := &daytonaLeaseBackend{cfg: b.cfg, rt: b.rt}
			if remainsVisible {
				err = interruptDaytonaFixedCleanup(t, f, req.RequestedLeaseID, func(ctx context.Context) error {
					return restarted.Stop(ctx, StopRequest{ID: req.RequestedLeaseID})
				})
			} else {
				err = restarted.Stop(t.Context(), StopRequest{ID: req.RequestedLeaseID})
			}
			claim, exists, readErr := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
			if err == nil || readErr != nil || !exists || claim.FixedCreateIntent.State == "released" || f.deletes != 1 {
				t.Fatalf("ambiguous cleanup retired/redeleted the resource: %v %v", err, readErr)
			}
			if remainsVisible {
				if claim.FixedCreateIntent.Attempt["deletion_acknowledged_id"] != claim.CloudID {
					t.Fatal("exact positive desired-destruction observation was not recorded")
				}
				f.sandbox.SetState(api.SANDBOXSTATE_DESTROYED)
				if err := restarted.Stop(t.Context(), StopRequest{ID: req.RequestedLeaseID}); err != nil || f.deletes != 1 {
					t.Fatalf("observed acknowledgment did not reconcile: %v", err)
				}
			} else if claim.FixedCreateIntent.Attempt["deletion_acknowledged_id"] != "" {
				t.Fatal("404 invented a deletion acknowledgment")
			}
		})
	}
}

func TestDaytonaFixedUncertainDeleteBlocksReuse(t *testing.T) {
	f, b, req := newFixedDaytonaFixture(t)
	lease, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	// A failed DELETE response does not establish whether the provider queued
	// deletion. The native resource can still report its original ready state.
	f.deleteError = true
	if err := b.ReleaseLease(t.Context(), ReleaseLeaseRequest{Lease: lease}); err == nil {
		t.Fatal("uncertain deletion unexpectedly completed")
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
	if err != nil || !exists || claim.FixedCreateIntent.Attempt["deletion_indexed_id"] != lease.Server.CloudID ||
		claim.FixedCreateIntent.Attempt["deletion_acknowledged_id"] != "" || f.sandbox.GetState() != api.SANDBOXSTATE_STARTED {
		t.Fatalf("missing uncertain cleanup fixture: %+v %v", claim, err)
	}
	requests := len(f.paths)
	if _, err := b.Acquire(t.Context(), req); err == nil {
		t.Fatal("fixed replay exposed a sandbox after an uncertain DELETE")
	}
	if _, err := b.Resolve(t.Context(), ResolveRequest{ID: req.RequestedLeaseID, Repo: req.Repo}); err == nil {
		t.Fatal("execution resolution exposed a sandbox after an uncertain DELETE")
	}
	if len(f.paths) != requests || f.sandboxCreates != 1 {
		t.Fatal("cleanup-only reuse contacted the provider or created another sandbox")
	}
	f.deleteError = false
	if err := b.Stop(t.Context(), StopRequest{ID: req.RequestedLeaseID}); err != nil {
		t.Fatalf("cleanup-only claim lost its stop recovery path: %v", err)
	}
	claim, exists, err = core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
	if err != nil || !exists || claim.FixedCreateIntent.State != "released" {
		t.Fatalf("reconciled cleanup did not publish its terminal claim: %v", err)
	}
}

func TestDaytonaFixedCleanupUnwitnessedAbsenceRetainsCustody(t *testing.T) {
	for _, unknownUUID := range []bool{false, true} {
		t.Run(fmt.Sprintf("unknown-uuid=%t", unknownUUID), func(t *testing.T) {
			f, b, req := newFixedDaytonaFixture(t)
			if unknownUUID {
				f.createErrorStatus = http.StatusInternalServerError
			}
			_, err := b.Acquire(t.Context(), req)
			if (err != nil) != unknownUUID {
				t.Fatalf("unexpected acquisition result: %v", err)
			}
			before, _, _ := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
			f.sandbox.SetState(api.SANDBOXSTATE_DESTROYED)
			f.sandbox.SetName("DESTROYED_" + f.sandbox.GetName())
			f.hideIdentitySandbox = true
			err = b.Stop(t.Context(), StopRequest{ID: req.RequestedLeaseID})
			after, exists, readErr := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
			a, _ := json.Marshal(before)
			z, _ := json.Marshal(after)
			if err == nil || readErr != nil || !exists || !bytes.Equal(a, z) || f.deletes != 0 || f.sandboxCreates != 1 {
				t.Fatalf("unwitnessed absence changed custody: %v %v", err, readErr)
			}
		})
	}
}

func TestDaytonaFixedCleanupFailureInclusiveInventory(t *testing.T) {
	for _, scenario := range []string{"absent", "prefix neighbor", "stale", "error", "build failed", "later page error", "later page absent", "repeated resource", "page failure", "wrong page", "changed total", "missing items", "null items", "missing total", "null total", "fractional total", "negative total", "null page", "null total pages", "inconsistent total pages", "short page", "wrong nonce", "changed organization"} {
		t.Run(scenario, func(t *testing.T) {
			f, b, req := newFixedDaytonaFixture(t)
			lease, err := b.Acquire(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			f.deletionPending = true
			err = interruptDaytonaFixedCleanup(t, f, req.RequestedLeaseID, func(ctx context.Context) error {
				return b.ReleaseLease(ctx, ReleaseLeaseRequest{Lease: lease})
			})
			if err == nil || f.deletes != 1 {
				t.Fatalf("missing interrupted deletion: %v", err)
			}
			f.sandbox.SetState(api.SANDBOXSTATE_DESTROYED)
			if scenario == "changed organization" {
				f.identityOrganization = "other-org"
			}
			item := *f.sandbox
			item.SetState(api.SANDBOXSTATE_ERROR)
			if scenario == "build failed" {
				item.SetState(api.SANDBOXSTATE_BUILD_FAILED)
			}
			if scenario == "stale" {
				item.SetState(api.SANDBOXSTATE_STARTED)
			}
			if scenario == "prefix neighbor" {
				item.SetId(item.GetId() + "-neighbor")
			}
			if scenario == "wrong nonce" {
				item.Labels = map[string]string{}
			}
			original := f.server.Config.Handler
			pages := 0
			f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/sandbox/paginated" {
					original.ServeHTTP(w, r)
					return
				}
				pages++
				if r.URL.Query().Get("states") != "" || r.URL.Query().Get("labels") != "" || r.URL.Query().Get("includeErroredDeleted") != "true" || r.URL.Query().Get("id") != lease.Server.CloudID {
					t.Errorf("unsupported or unscoped database inventory: %s", r.URL.RawQuery)
				}
				w.Header().Set("Content-Type", "application/json")
				body := map[string]any{"items": []api.Sandbox{item}, "total": 1, "page": 1, "totalPages": 1}
				switch scenario {
				case "absent", "missing items", "null items", "missing total", "null total", "fractional total", "negative total", "null page", "null total pages", "inconsistent total pages", "short page":
					body = map[string]any{"items": []api.Sandbox{}, "total": 0, "page": 1, "totalPages": 0}
					switch scenario {
					case "missing items":
						delete(body, "items")
					case "null items":
						body["items"] = nil
					case "missing total":
						delete(body, "total")
					case "null total":
						body["total"] = nil
					case "fractional total":
						body["total"] = 0.5
					case "negative total":
						body["total"] = -1
					case "null page":
						body["page"] = nil
					case "null total pages":
						body["totalPages"] = nil
					case "inconsistent total pages":
						body["totalPages"] = 1
					case "short page":
						body["total"], body["totalPages"] = 1, 1
					}
				case "later page error", "later page absent", "repeated resource", "page failure", "wrong page", "changed total":
					if r.URL.Query().Get("page") == "1" {
						neighbors := make([]api.Sandbox, 100)
						for i := range neighbors {
							neighbors[i] = item
							neighbors[i].SetId(fmt.Sprintf("%s-neighbor-%d", item.GetId(), i))
						}
						body = map[string]any{"items": neighbors, "total": 101, "page": 1, "totalPages": 2}
					} else {
						if scenario == "page failure" {
							w.WriteHeader(http.StatusServiceUnavailable)
							return
						}
						body["total"], body["page"], body["totalPages"] = 101, 2, 2
						if scenario == "later page absent" || scenario == "repeated resource" {
							neighbor := item
							neighbor.SetId(item.GetId() + "-neighbor-final")
							if scenario == "repeated resource" {
								neighbor.SetId(item.GetId() + "-neighbor-0")
							}
							body["items"] = []api.Sandbox{neighbor}
						}
						if scenario == "wrong page" {
							body["page"] = 1
						}
						if scenario == "changed total" {
							body["total"] = 102
						}
					}
				}
				_ = json.NewEncoder(w).Encode(body)
			})
			if scenario == "stale" {
				err = interruptDaytonaFixedCleanup(t, f, req.RequestedLeaseID, func(ctx context.Context) error { return b.Stop(ctx, StopRequest{ID: req.RequestedLeaseID}) })
			} else {
				err = b.Stop(t.Context(), StopRequest{ID: req.RequestedLeaseID})
			}
			claim, exists, readErr := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
			wantReleased := scenario == "absent" || scenario == "prefix neighbor" || scenario == "later page absent"
			if readErr != nil || !exists || (err == nil) != wantReleased || (claim.FixedCreateIntent.State == "released") != wantReleased || f.deletes != 1 {
				t.Fatalf("incorrect deletion reconciliation: err=%v read=%v state=%s deletes=%d", err, readErr, claim.FixedCreateIntent.State, f.deletes)
			}
			if (scenario == "later page error" || scenario == "later page absent" || scenario == "repeated resource" || scenario == "page failure" || scenario == "wrong page" || scenario == "changed total") && pages != 2 {
				t.Fatalf("incomplete pagination: pages=%d", pages)
			}
		})
	}
}

func TestDaytonaFixedCleanupRequiresDatabaseIdentity(t *testing.T) {
	f, b, req := newFixedDaytonaFixture(t)
	lease, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	original := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/sandbox/paginated" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"items":[],"total":0,"page":1,"totalPages":0}`)
			return
		}
		original.ServeHTTP(w, r)
	})
	if err := b.ReleaseLease(t.Context(), ReleaseLeaseRequest{Lease: lease}); err == nil || f.deletes != 0 {
		t.Fatalf("unobserved child reached deletion: %v", err)
	}
	f.server.Config.Handler = original
	if err := b.Stop(t.Context(), StopRequest{ID: req.RequestedLeaseID}); err != nil || f.deletes != 1 {
		t.Fatalf("database-visible child failed cleanup: %v", err)
	}
}

func TestDaytonaFixedReleasedStatusIsLocal(t *testing.T) {
	f, b, req := newFixedDaytonaFixture(t)
	lease, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ReleaseLease(t.Context(), ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	requests := len(f.paths)
	for _, wait := range []bool{false, true} {
		view, err := b.Status(t.Context(), StatusRequest{ID: req.RequestedLeaseID, Wait: wait})
		if err != nil || view.ID != req.RequestedLeaseID || view.State != "released" || view.Ready || view.HasHost || view.ServerID != "" || view.Host != "" {
			t.Fatalf("terminal status lost identity or advertised access: %+v %v", view, err)
		}
	}
	terminal, err := b.Resolve(t.Context(), ResolveRequest{ID: req.RequestedLeaseID, StatusOnly: true, NoLocalStateMutations: true})
	if err != nil || terminal.Server.Status != "released" || terminal.Server.CloudID != "" || terminal.SSH.Host != "" {
		t.Fatalf("terminal status-only resolution failed: %+v %v", terminal, err)
	}
	if _, err := b.Resolve(t.Context(), ResolveRequest{ID: req.RequestedLeaseID}); err == nil {
		t.Fatal("released lease admitted execution")
	}
	config := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(config, []byte(fmt.Sprintf("provider: daytona\nnetwork: public\ndaytona:\n  apiUrl: %q\n", b.cfg.Daytona.APIURL)), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", config)
	t.Setenv("CRABBOX_DAYTONA_API_KEY", b.cfg.Daytona.APIKey)
	for _, command := range [][]string{{"inspect"}, {"status"}, {"status", "--wait"}} {
		var stdout, stderr bytes.Buffer
		args := append(append([]string{}, command...), "--id", req.RequestedLeaseID, "--json")
		err := (core.App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), args)
		var view core.StatusView
		if decodeErr := json.Unmarshal(stdout.Bytes(), &view); decodeErr != nil || view.State != "released" || view.Ready || view.HasHost || view.ServerID != "" || view.Host != "" || view.SSHKey != "" {
			t.Fatalf("terminal CLI view is unusable: args=%v output=%s err=%v decode=%v", args, stdout.String(), err, decodeErr)
		}
		if (err != nil) != (len(command) == 2) {
			t.Fatalf("terminal wait outcome changed: args=%v err=%v", args, err)
		}
	}

	if len(f.paths) != requests {
		t.Fatal("local terminal inspection contacted the provider")
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
				f.deletionPending = true
			}
			if drift == "unverified deletion" {
				err = interruptDaytonaFixedCleanup(t, f, req.RequestedLeaseID, func(ctx context.Context) error {
					return b.ReleaseLease(ctx, ReleaseLeaseRequest{Lease: lease})
				})
			} else {
				err = b.ReleaseLease(t.Context(), ReleaseLeaseRequest{Lease: lease})
			}
			if err == nil {
				t.Fatal("unverified release succeeded")
			}
			after, exists, _ := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
			a, _ := json.Marshal(before)
			z, _ := json.Marshal(after)
			if !exists || after.FixedCreateIntent.State == "released" ||
				(drift != "unverified deletion" && string(a) != string(z)) ||
				(drift == "unverified deletion" && after.FixedCreateIntent.Attempt["deletion_acknowledged_id"] != after.CloudID) {
				t.Fatal("failed cleanup changed ownership or lost the acknowledged deletion")
			}
			if drift != "unverified deletion" && f.deletes != 0 {
				t.Fatal("deleted after ownership drift")
			}
		})
	}
}
