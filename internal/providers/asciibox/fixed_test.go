package asciibox

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

type fixedBoxClock struct{ at time.Time }

func (c *fixedBoxClock) Now() time.Time { return c.at }

type fixedBoxAPI struct {
	fakeAPI
	t           *testing.T
	leaseID     string
	keys        []string
	lostReplies int
}

func (f *fixedBoxAPI) CreateBox(ctx context.Context, req createRequest) (boxData, error) {
	f.t.Helper()
	f.createReq = req
	f.keys = append(f.keys, req.IdempotencyKey)
	claim, err := core.ReadLeaseClaim(f.leaseID)
	if err != nil || claim.FixedCreateIntent == nil || claim.FixedCreateIntent.Journal.Submission.Count != len(f.keys) || claim.FixedCreateIntent.Attempt["idempotency_key"] != req.IdempotencyKey || req.IdempotencyKey == "" || claim.ProviderScope == "" {
		f.t.Fatalf("create preceded durable scoped keyed admission: %+v, %v", claim, err)
	}
	if _, ok := ctx.Deadline(); !ok {
		f.t.Fatal("keyed submission has no deadline")
	}
	for _, key := range f.keys {
		if key != req.IdempotencyKey {
			f.t.Fatal("idempotency key changed")
		}
	}
	if f.box.ID == "" {
		f.box = testBox()
	}
	if len(f.keys) <= f.lostReplies {
		return boxData{}, errors.New("lost create reply")
	}
	return f.box, nil
}

func fixedBoxFixture(t *testing.T) (*backend, *fixedBoxAPI, core.AcquireRequest, *fixedBoxClock) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("BOX_ORG", "personal")
	f := &fixedBoxAPI{t: t, leaseID: "cbx_123456789abc"}
	withFakeAPI(t, f)
	stubSSHWait(t)
	cfg := testConfig()
	cfg.TTL = 48 * time.Hour
	clock := &fixedBoxClock{at: time.Now().UTC()}
	rt := testRuntime()
	rt.Clock = clock
	b := NewBackend(Provider{}.Spec(), cfg, rt).(*backend)
	return b, f, core.AcquireRequest{RequestedLeaseID: f.leaseID, RequestedSlug: "fixed-box", Repo: core.Repo{Root: t.TempDir()}, Keep: true}, clock
}

func TestFixedBoxFreshReplayAndConflict(t *testing.T) {
	b, f, req, _ := fixedBoxFixture(t)
	f.box = testBox()
	f.box.IP = ""
	f.box.SSHEndpoint = "198.51.100.20:19036"
	first, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if first.SSH.Host != "198.51.100.20" || first.SSH.Port != "19036" {
		t.Fatalf("fixed SSH target: %+v", first.SSH)
	}
	before, _ := core.ReadLeaseClaim(req.RequestedLeaseID)
	if before.CloudID == "" || before.CloudID != first.Server.CloudID || before.ProviderScope != core.ProviderClaimScope(providerName, b.cfg) {
		t.Fatalf("fixed acquisition lost heartbeat identity: %+v", before)
	}
	if view, err := b.Status(t.Context(), core.StatusRequest{ID: req.RequestedLeaseID}); err != nil || !view.Ready || view.HasHost || view.SSHHost != first.SSH.Host || view.SSHPort != first.SSH.Port {
		t.Fatalf("fixed status: %+v %v", view, err)
	}
	after, _ := core.ReadLeaseClaim(req.RequestedLeaseID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("status mutated fixed claim")
	}
	replay, err := b.Acquire(t.Context(), req)
	if err != nil || first.Server.CloudID != replay.Server.CloudID || first.Server.ImmutableID == "" || first.Server.ImmutableID != replay.Server.ImmutableID || len(f.keys) != 1 || replay.SSH.Host != first.SSH.Host || replay.SSH.Port != first.SSH.Port {
		t.Fatalf("replay allocated or changed identity: %+v, %v", replay, err)
	}
	if f.createReq.NoEnv || before.Labels[boxNoEnvLabel] != "false" {
		t.Fatalf("default creation changed environment policy: request=%+v labels=%v", f.createReq, before.Labels)
	}
	if f.createReq.Type != "default" {
		t.Fatalf("fixed create size=%q want default", f.createReq.Type)
	}
	legacy, err := core.FixedIntentFingerprint("ascii-box/v1\x00", struct {
		Scope, Workdir, Slug, Pond string
		TTL, Idle                  time.Duration
		Keep                       bool
	}{before.ProviderScope, b.cfg.AsciiBox.Workdir, req.RequestedSlug, core.NormalizePondName(b.cfg.Pond), b.cfg.TTL, b.cfg.IdleTimeout, req.Keep})
	if err != nil || before.FixedCreateIntent.Fingerprint != legacy {
		t.Fatalf("default sizing changed legacy intent: got=%s want=%s err=%v", before.FixedCreateIntent.Fingerprint, legacy, err)
	}
	configured := b.cfg
	configured.Class = "fast"
	core.MarkClassExplicit(&configured)
	configured.ServerType, configured.ServerTypeExplicit = "default", true
	other := NewBackend(Provider{}.Spec(), configured, b.rt).(*backend)
	if replay, err := other.Acquire(t.Context(), req); err != nil || replay.Server.CloudID != first.Server.CloudID || len(f.keys) != 1 {
		t.Fatalf("explicit default did not resume the old lease: %+v %v", replay, err)
	}
	for _, change := range []string{"ttl", "size", "class", "no-env", "no-snapshots", "slug", "workdir", "scope", "generation", "id"} {
		t.Run(change, func(t *testing.T) {
			cfg, changed := b.cfg, req
			original := f.box
			switch change {
			case "ttl":
				changed.Options.TTL = time.Hour
			case "no-env":
				cfg.AsciiBox.NoEnv = true
			case "no-snapshots":
				cfg.AsciiBox.NoSnapshots = true
			case "size":
				cfg.ServerType, cfg.ServerTypeExplicit = "large", true
			case "class":
				cfg.Class = "fast"
				core.MarkClassExplicit(&cfg)
			case "slug":
				changed.RequestedSlug = "other"
			case "workdir":
				cfg.AsciiBox.Workdir = "/home/user/other"
			case "scope":
				t.Setenv("BOX_ORG", "org_other")
			case "generation":
				f.box.CreatedAt = "2026-09-20T12:00:00Z"
			case "id":
				f.getHook = func(string) (boxData, error) { box := original; box.ID = "bx_replacement"; return box, nil }
			}
			defer func() { f.box = original; f.getHook = nil }()
			other := NewBackend(Provider{}.Spec(), cfg, b.rt).(*backend)
			if _, err := other.Acquire(t.Context(), changed); err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
				t.Fatalf("expected conflict, got %v", err)
			}
			if len(f.keys) != 1 {
				t.Fatal("conflict submitted a create")
			}
		})
	}
}

func TestFixedBoxObservationsAndReusePreservePolicy(t *testing.T) {
	b, f, req, clock := fixedBoxFixture(t)
	req.Keep = false
	req.Options.TTL = 20 * time.Minute
	b.cfg.IdleTimeout = 5 * time.Minute
	f.box = testBox()
	f.box.UpdatedAt = "2026-08-30T12:01:00Z"
	f.box.ExpiresAt = "2026-08-30T13:00:00Z"
	lease, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	original, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || original.Labels["ttl_secs"] != "1200" || original.Labels["keep"] != "false" {
		t.Fatalf("fixed acquisition policy: %+v, %v", original, err)
	}
	b.cfg.TTL, b.cfg.IdleTimeout = 2*time.Hour, 45*time.Minute
	status, err := b.Status(t.Context(), core.StatusRequest{ID: req.RequestedLeaseID})
	if err != nil {
		t.Fatal(err)
	}
	list, err := b.List(t.Context(), core.ListRequest{})
	if err != nil || len(list) != 1 {
		t.Fatalf("inventory: %+v, %v", list, err)
	}
	readOnly, err := b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, StatusOnly: true, NoLocalStateMutations: true})
	if err != nil {
		t.Fatal(err)
	}
	created, _ := time.Parse(time.RFC3339Nano, boxCreationTime(f.box))
	updated, _ := time.Parse(time.RFC3339Nano, f.box.UpdatedAt.(string))
	for _, labels := range []map[string]string{status.Labels, list[0].Labels, readOnly.Server.Labels} {
		if labels["ttl_secs"] != "1200" || labels["idle_timeout_secs"] != "300" || labels["keep"] != "false" || labels["created_at"] != core.LeaseLabelTime(created) || labels["updated_at"] != core.LeaseLabelTime(updated) || labels["expires_at"] != f.box.ExpiresAt {
			t.Fatalf("fixed observation reset policy or native facts: %v", labels)
		}
	}
	after, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || !reflect.DeepEqual(original, after) {
		t.Fatalf("fixed observations mutated the claim: %+v, %v", after, err)
	}
	clock.at = clock.at.Add(time.Minute)
	newRepo := core.Repo{Root: t.TempDir()}
	if _, err := b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, Repo: newRepo}); err == nil || !strings.Contains(err.Error(), "belongs to another repository") {
		t.Fatalf("cross-repository reuse without reclaim: %v", err)
	}
	lease, err = b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, Repo: newRepo, Reclaim: true})
	if err != nil {
		t.Fatal(err)
	}
	after, err = core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || after.RepoRoot != newRepo.Root || after.CloudID != original.CloudID || after.CloudImmutableID != original.CloudImmutableID || !reflect.DeepEqual(after.FixedCreateIntent, original.FixedCreateIntent) || len(f.keys) != 1 {
		t.Fatalf("reclaim changed fixed identity or intent: %+v, %v", after, err)
	}
	for _, key := range []string{"created_at", "ttl_secs", "keep", "expires_at", "idle_timeout_secs"} {
		if lease.Server.Labels[key] != original.Labels[key] {
			t.Fatalf("fixed reuse reset %s: %q, want %q", key, lease.Server.Labels[key], original.Labels[key])
		}
	}
	clock.at = clock.at.Add(time.Minute)
	override := 10 * time.Minute
	if _, err := b.Touch(t.Context(), core.TouchRequest{Lease: lease, State: "working", IdleTimeoutOverride: &override}); err != nil {
		t.Fatal(err)
	}
	after, err = core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || after.IdleTimeoutSeconds != 600 || after.LastUsedAt != clock.at.Format(time.RFC3339) || after.Labels["ttl_secs"] != "1200" || after.Labels["keep"] != "false" || after.Labels["created_at"] != original.Labels["created_at"] {
		t.Fatalf("fixed heartbeat lost recorded policy: %+v, %v", after, err)
	}
}

func TestFixedBoxReclaimRejectsUnavailableClaims(t *testing.T) {
	for _, reason := range []string{"no-env", "intent-deleting", "journal-deleting", "deletion-operation", "deletion-completed", "completed-receipt", "operation-receipt", "binding-receipt", "status", "read-only", "release", "no-local-state"} {
		t.Run(reason, func(t *testing.T) {
			b, f, req, _ := fixedBoxFixture(t)
			if _, err := b.Acquire(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			reuse := core.ResolveRequest{ID: req.RequestedLeaseID, Repo: core.Repo{Root: t.TempDir()}, Reclaim: true}
			wantError := "entered cleanup"
			if err := core.WithDurableLeaseClaimLock(req.RequestedLeaseID, func(claim *core.LeaseClaim, _ bool, persist func() error) error {
				switch reason {
				case "no-env":
					b.cfg.AsciiBox.NoEnv = true
					wantError = "no-env"
				case "intent-deleting":
					claim.FixedCreateIntent.State = "deleting"
				case "journal-deleting":
					claim.FixedCreateIntent.Journal.Phase = "deleting"
				case "deletion-operation":
					claim.FixedCreateIntent.Attempt["deletion_operation_id"] = testDeletionID
				case "deletion-completed":
					claim.FixedCreateIntent.Attempt["deletion_completed"] = "true"
				case "completed-receipt":
					claim.Labels[boxDeletionLabel] = ""
				case "operation-receipt":
					claim.Labels[boxDeletionOperationLabel] = ""
				case "binding-receipt":
					claim.Labels[boxDeletionOperationBindingLabel] = ""
				case "status":
					reuse.StatusOnly = true
				case "read-only":
					reuse.StatusOnly, reuse.NoLocalStateMutations = true, true
				case "release":
					reuse.ReleaseOnly = true
				case "no-local-state":
					reuse.NoLocalStateMutations = true
				}
				return persist()
			}); err != nil {
				t.Fatal(err)
			}
			if reuse.StatusOnly || reuse.ReleaseOnly || reuse.NoLocalStateMutations {
				wantError = "belongs to another repository"
			}
			before, err := core.ReadLeaseClaim(req.RequestedLeaseID)
			if err != nil {
				t.Fatal(err)
			}
			f.prepareIDs = nil
			f.getHook = func(string) (boxData, error) {
				t.Fatal("rejected reclaim reached native observation")
				return boxData{}, nil
			}
			if _, err := b.Resolve(t.Context(), reuse); err == nil || !strings.Contains(err.Error(), wantError) {
				t.Fatalf("reclaim was not rejected at %s: %v", reason, err)
			}
			after, err := core.ReadLeaseClaim(req.RequestedLeaseID)
			if err != nil || !reflect.DeepEqual(before, after) || len(f.prepareIDs) != 0 || len(f.keys) != 1 {
				t.Fatalf("rejected reclaim changed ownership or reached SSH: %+v, %v", after, err)
			}
		})
	}
}

func TestFixedBoxReclaimRejectsConcurrentReplacement(t *testing.T) {
	b, f, req, _ := fixedBoxFixture(t)
	if _, err := b.Acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	var replacement core.LeaseClaim
	f.getHook = func(string) (boxData, error) {
		if err := core.WithDurableLeaseClaimLock(req.RequestedLeaseID, func(claim *core.LeaseClaim, _ bool, persist func() error) error {
			claim.RepoRoot = t.TempDir()
			return persist()
		}); err != nil {
			t.Fatal(err)
		}
		var err error
		replacement, err = core.ReadLeaseClaim(req.RequestedLeaseID)
		if err != nil {
			t.Fatal(err)
		}
		return f.box, nil
	}
	f.prepareIDs = nil
	_, err := b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, Repo: core.Repo{Root: t.TempDir()}, Reclaim: true})
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("stale reclaim did not reject replaced claim: %v", err)
	}
	after, readErr := core.ReadLeaseClaim(req.RequestedLeaseID)
	if readErr != nil || !reflect.DeepEqual(replacement, after) || len(f.prepareIDs) != 0 || len(f.keys) != 1 {
		t.Fatalf("stale reclaim changed replacement or reached SSH: %+v, %v", after, readErr)
	}
}

func TestFixedBoxTouchRejectsCleanup(t *testing.T) {
	for _, phase := range []string{"intent", "journal", "operation"} {
		t.Run(phase, func(t *testing.T) {
			b, _, req, _ := fixedBoxFixture(t)
			lease, err := b.Acquire(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			before, err := core.ReadLeaseClaim(req.RequestedLeaseID)
			if err != nil {
				t.Fatal(err)
			}
			held := core.CloneLeaseClaim(before)
			switch phase {
			case "intent":
				held.FixedCreateIntent.State = "deleting"
			case "journal":
				held.FixedCreateIntent.Journal.Phase = "deleting"
			case "operation":
				held.FixedCreateIntent.Attempt["deletion_operation_id"] = testDeletionID
			}
			core.SetServerLeaseClaimSnapshot(&lease.Server, held, true)
			if _, err := b.Touch(t.Context(), core.TouchRequest{Lease: lease, State: "ready"}); err == nil || (!strings.Contains(err.Error(), "not available for activity") && !strings.Contains(err.Error(), "entered cleanup")) {
				t.Fatalf("cleanup was not rejected before claim publication: %v", err)
			}
			after, err := core.ReadLeaseClaim(req.RequestedLeaseID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("refused touch mutated claim: %+v, %v", after, err)
			}
		})
	}
}

func TestFixedBoxLostReplyRecoveryWindow(t *testing.T) {
	for _, elapsed := range []time.Duration{time.Hour, 24 * time.Hour, 25 * time.Hour, -time.Second} {
		t.Run(elapsed.String(), func(t *testing.T) {
			b, f, req, clock := fixedBoxFixture(t)
			f.lostReplies = 1
			if _, err := b.Acquire(t.Context(), req); err == nil {
				t.Fatal("lost reply succeeded")
			}
			before, _ := core.ReadLeaseClaim(req.RequestedLeaseID)
			clock.at = clock.at.Add(elapsed)
			lease, err := b.Acquire(t.Context(), req)
			if elapsed == time.Hour {
				if err != nil || lease.Server.CloudID != f.box.ID || len(f.keys) != 2 {
					t.Fatalf("keyed recovery: %+v, %v", lease, err)
				}
				if _, err := b.Acquire(t.Context(), req); err != nil || len(f.keys) != 2 {
					t.Fatalf("adopt after recovery: %v", err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "lease_id_conflict") || len(f.keys) != 1 {
					t.Fatalf("unsafe resubmission: %v, calls=%d", err, len(f.keys))
				}
				after, _ := core.ReadLeaseClaim(req.RequestedLeaseID)
				if !reflect.DeepEqual(before, after) {
					t.Fatal("expired uncertainty changed custody")
				}
			}
		})
	}
}

func TestFixedBoxSecondLostReplyRetained(t *testing.T) {
	b, f, req, _ := fixedBoxFixture(t)
	f.lostReplies = 2
	for i := 0; i < 3; i++ {
		if _, err := b.Acquire(t.Context(), req); err == nil {
			t.Fatal("lost reply succeeded")
		}
	}
	if len(f.keys) != 2 {
		t.Fatalf("submissions=%d, want exactly two", len(f.keys))
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || claim.FixedCreateIntent.Journal.Submission.Count != 2 || claim.CloudID != "" {
		t.Fatalf("lost uncertainty: %+v %v", claim, err)
	}
	lease, err := b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err == nil || len(f.deletedIDs) != 0 {
		t.Fatalf("released unknown Box: %v", err)
	}
}

func TestFixedBoxReleaseTombstoneAndAbsence(t *testing.T) {
	for _, absent := range []bool{false, true} {
		t.Run(map[bool]string{false: "delete", true: "already-absent"}[absent], func(t *testing.T) {
			b, f, req, _ := fixedBoxFixture(t)
			lease, err := b.Acquire(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			f.deleted = absent
			if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
				t.Fatal(err)
			}
			claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
			if err != nil || claim.FixedCreateIntent.State != "released" || claim.FixedCreateIntent.Journal.Phase != "released" {
				t.Fatalf("no tombstone: %+v %v", claim, err)
			}
			if _, err := b.Acquire(t.Context(), req); err == nil || len(f.keys) != 1 {
				t.Fatalf("terminal replay allocated: %v", err)
			}
			if view, err := b.Status(t.Context(), core.StatusRequest{ID: req.RequestedLeaseID}); err != nil || view.State != "released" {
				t.Fatalf("terminal status: %+v %v", view, err)
			}
			released, err := b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: released}); err != nil {
				t.Fatal(err)
			}
			want := 1
			if absent {
				want = 0
			}
			if len(f.deletedIDs) != want {
				t.Fatalf("native deletes=%v", f.deletedIDs)
			}
			if keep, err := b.RetainLeaseClaimAfterReleaseWithClaim(lease, claim); !keep || err != nil {
				t.Fatalf("tombstone discarded: %v", err)
			}
		})
	}
}

func TestFixedBoxAbsentAfterCreateRetainsExactIdentity(t *testing.T) {
	b, f, req, _ := fixedBoxFixture(t)
	f.getHook = func(id string) (boxData, error) { return boxData{}, &boxNotFoundError{id: id} }
	for i := 0; i < 2; i++ {
		if _, err := b.Acquire(t.Context(), req); err == nil {
			t.Fatal("absent resource acquired")
		}
	}
	claim, _ := core.ReadLeaseClaim(req.RequestedLeaseID)
	if claim.CloudID != "bx_1" || claim.FixedCreateIntent.State != "prepared" || len(f.keys) != 1 {
		t.Fatalf("returned identity lost: %+v", claim)
	}
}

func TestFixedBoxDeletionWitnessRecovery(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "completed"}[completed], func(t *testing.T) {
			b, f, req, _ := fixedBoxFixture(t)
			lease, err := b.Acquire(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			f.releaseHook = func(id string) error {
				calls++
				if completed {
					return nil
				}
				return &boxDeletionIncompleteError{operation: boxDeletionOperation{ID: testDeletionID, Kind: "box", TargetID: id, Status: "pending"}, err: context.DeadlineExceeded}
			}
			f.listHook = func() ([]boxData, error) { return nil, errors.New("inventory unavailable") }
			if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err == nil {
				t.Fatal("incomplete release succeeded")
			}
			f.deleted = false
			f.listHook = nil
			f.deletionHook = func(target, id string) (boxDeletionOperation, error) {
				return boxDeletionOperation{ID: id, Kind: "box", TargetID: target, Status: "pending"}, nil
			}
			lease, err = b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err == nil || calls != 1 {
				t.Fatalf("repeated admitted native deletion: %v calls=%d", err, calls)
			}
			if _, err := b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID}); err == nil {
				t.Fatal("deleting resource reused")
			}
			f.deleted = true
			lease, err = b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil || calls != 1 {
				t.Fatalf("absence recovery: %v calls=%d", err, calls)
			}
		})
	}
}

func TestFixedBoxStopCommandPreservesTombstone(t *testing.T) {
	testutil.IsolateUserDirs(t)
	t.Chdir(t.TempDir())
	b, f, req, _ := fixedBoxFixture(t)
	if _, err := b.Acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	f.deleted = true
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("provider: ascii-box\nclass: fast\nasciiBox:\n  baseUrl: https://ascii.dev\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", configPath)
	for _, env := range []string{"CRABBOX_COORDINATOR", "CRABBOX_COORDINATOR_MODE", "CRABBOX_POND", "CRABBOX_TAILSCALE"} {
		t.Setenv(env, "")
	}
	var output bytes.Buffer
	if err := (core.App{Stdout: &output, Stderr: &output}).Run(t.Context(), []string{"stop", "--provider", "boat", "--id", req.RequestedLeaseID}); err != nil {
		t.Fatalf("stop: %v output=%s", err, output.String())
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || claim.FixedCreateIntent == nil || claim.FixedCreateIntent.State != "released" {
		t.Fatalf("CLI erased tombstone: %+v %v", claim, err)
	}
	if len(f.deletedIDs) != 0 {
		t.Fatal("absent fixed Box was deleted again")
	}
}

func TestFixedBoxCreatedAtSkew(t *testing.T) {
	b, f, req, _ := fixedBoxFixture(t)
	f.box = testBox()
	f.box.CreatedAt = "2026-10-03T18:51:06.063Z"
	f.getHook = func(string) (boxData, error) {
		if f.deleted {
			return boxData{}, &boxNotFoundError{id: f.box.ID}
		}
		read := f.box
		read.CreatedAt = "2026-10-03T18:51:06.052Z"
		return read, nil
	}
	lease, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || claim.CloudImmutableID != boxCreationTime(f.box) || claim.Labels[boxCreationLabel] != claim.CloudImmutableID {
		t.Fatalf("original create witness not retained: %+v %v", claim, err)
	}
	if _, err := b.Acquire(t.Context(), req); err != nil || len(f.keys) != 1 {
		t.Fatalf("skew prevented replay: %v creates=%d", err, len(f.keys))
	}
	lease, err = b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, Repo: req.Repo})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := core.ReadLeaseClaim(req.RequestedLeaseID)
	if after.CloudImmutableID != claim.CloudImmutableID || after.Labels[boxCreationLabel] != claim.CloudImmutableID {
		t.Fatal("reuse moved the original timestamp witness")
	}
	if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
}

func TestFixedBoxTimestampTolerance(t *testing.T) {
	b, f, req, _ := fixedBoxFixture(t)
	if _, err := b.Acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	claim, _ := core.ReadLeaseClaim(req.RequestedLeaseID)
	created, _ := time.Parse(time.RFC3339Nano, claim.CloudImmutableID)
	for _, skew := range []time.Duration{-5*time.Second - time.Nanosecond, -5 * time.Second, -11 * time.Millisecond, 0, 5 * time.Second, 5*time.Second + time.Nanosecond} {
		t.Run(skew.String(), func(t *testing.T) {
			f.box.CreatedAt = created.Add(skew).Format(time.RFC3339Nano)
			_, err := core.InspectFixedResource(t.Context(), fixedBoxKind, claim, core.FixedLeaseOperations[boxData]{ObserveExact: fixedBoxObserver(b.cfg, f)})
			if skew.Abs() <= 5*time.Second {
				if err != nil {
					t.Fatalf("in-window timestamp refused: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
				t.Fatalf("out-of-window timestamp accepted: %v", err)
			}
		})
	}
	f.box.CreatedAt = created.Add(6 * time.Second).Format(time.RFC3339Nano)
	if _, err := b.Acquire(t.Context(), req); err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Fatalf("out-of-window acquired claim reused: %v", err)
	}
	lease, err := b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err == nil || len(f.deletedIDs) != 0 {
		t.Fatalf("acquired claim got own-create cleanup exception: %v", err)
	}
}

func TestFixedBoxOwnCreateConflictCanInspectAndStop(t *testing.T) {
	for _, mismatch := range []string{"timestamp", "missing witness", "different id", "interrupted stop"} {
		t.Run(mismatch, func(t *testing.T) {
			b, f, req, _ := fixedBoxFixture(t)
			f.box = testBox()
			f.getHook = func(string) (boxData, error) {
				if f.deleted {
					return boxData{}, &boxNotFoundError{id: f.box.ID}
				}
				read := f.box
				switch mismatch {
				case "missing witness":
					read.CreatedAt = nil
				case "different id":
					read.ID = "bx_foreign"
				default:
					read.CreatedAt = "2026-08-30T12:00:06Z"
				}
				return read, nil
			}
			_, err := b.Acquire(t.Context(), req)
			if mismatch == "different id" {
				if err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
					t.Fatalf("foreign id not refused: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "ascii_box_identity_contract") || !strings.Contains(err.Error(), f.box.ID) || !strings.Contains(err.Error(), "crabbox stop") {
				t.Fatalf("missing actionable contract error: %v", err)
			}
			before, _ := core.ReadLeaseClaim(req.RequestedLeaseID)
			_, err = b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, StatusOnly: true, NoLocalStateMutations: true})
			if (err != nil) != (mismatch == "different id") {
				t.Fatalf("inspection: %v", err)
			}
			after, _ := core.ReadLeaseClaim(req.RequestedLeaseID)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("inspection changed claim")
			}
			lease, err := b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			if mismatch == "interrupted stop" {
				f.releaseHook = func(string) error { return errors.New("temporary release failure") }
				if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err == nil {
					t.Fatal("interrupted release succeeded")
				}
				f.releaseHook = nil
				lease, err = b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
				if err != nil {
					t.Fatal(err)
				}
			}
			err = b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease})
			if mismatch == "different id" {
				if err == nil || len(f.deletedIDs) != 0 {
					t.Fatalf("foreign id deleted: %v %v", err, f.deletedIDs)
				}
			} else if err != nil || !reflect.DeepEqual(f.deletedIDs, []string{f.box.ID}) {
				t.Fatalf("own create stranded: %v %v", err, f.deletedIDs)
			}
		})
	}
}

func TestBoxTimestampWitnessValidation(t *testing.T) {
	for _, test := range []struct {
		name, expected, actual string
		want                   bool
	}{
		{"equivalent zones", "2026-10-03T12:00:00-07:00", "2026-10-03T19:00:00Z", true},
		{"missing expected", "", "2026-10-03T19:00:00Z", false},
		{"invalid expected", "invalid", "2026-10-03T19:00:00Z", false},
		{"missing actual", "2026-10-03T19:00:00Z", "", false},
		{"invalid actual", "2026-10-03T19:00:00Z", "invalid", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := boxCreationTimeMatches(boxData{CreatedAt: test.actual}, test.expected); got != test.want {
				t.Fatalf("timestamp match=%t, want %t", got, test.want)
			}
		})
	}
}

func TestBoxTimestampWitnessDoesNotDrift(t *testing.T) {
	original := testBox()
	original.claimCreatedAt = boxCreationTime(original)
	within := original
	within.CreatedAt = "2026-08-30T12:00:04Z"
	if err := validateBoxIdentity(within, original); err != nil {
		t.Fatal(err)
	}
	merged := mergeBox(original, within)
	outside := within
	outside.CreatedAt = "2026-08-30T12:00:08Z"
	if err := validateBoxIdentity(outside, merged); err == nil {
		t.Fatal("successive reads moved the comparison window")
	}
}
