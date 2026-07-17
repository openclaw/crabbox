package fal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type fakeFalAPI struct {
	instances               map[string]ComputeInstance
	getErr                  error
	createErr               error
	createErrs              []error
	deleteErr               error
	createRequests          []CreateInstanceRequest
	idempotency             []string
	deletedIDs              []string
	listCalls               int
	createHook              func(CreateInstanceRequest, string, ComputeInstance)
	beforeCreateHook        func(string)
	afterDeleteHook         func(string)
	blockCreateUntilContext bool
	blockGetUntilContext    bool
	retainDeletedInstance   bool
	removeBeforeDeleteError bool
}

type mutableFalClock struct {
	now time.Time
}

func (c *mutableFalClock) Now() time.Time { return c.now }

func (f *fakeFalAPI) ListInstances(context.Context, int, string) (ListInstancesResponse, error) {
	f.listCalls++
	items := make([]ComputeInstance, 0, len(f.instances))
	for _, item := range f.instances {
		items = append(items, item)
	}
	return ListInstancesResponse{Instances: items}, nil
}

func (f *fakeFalAPI) GetInstance(ctx context.Context, id string) (ComputeInstance, error) {
	if f.blockGetUntilContext {
		<-ctx.Done()
		return ComputeInstance{}, ctx.Err()
	}
	if f.getErr != nil {
		return ComputeInstance{}, f.getErr
	}
	item, ok := f.instances[id]
	if !ok {
		return ComputeInstance{}, &APIError{StatusCode: 404, Status: "404 Not Found", Message: "not found"}
	}
	return item, nil
}

func (f *fakeFalAPI) CreateInstance(ctx context.Context, req CreateInstanceRequest, idempotencyKey string) (ComputeInstance, error) {
	f.createRequests = append(f.createRequests, req)
	f.idempotency = append(f.idempotency, idempotencyKey)
	if f.beforeCreateHook != nil {
		f.beforeCreateHook(idempotencyKey)
	}
	if f.blockCreateUntilContext {
		<-ctx.Done()
		return ComputeInstance{}, ctx.Err()
	}
	if len(f.createErrs) > 0 {
		err := f.createErrs[0]
		f.createErrs = f.createErrs[1:]
		if err != nil {
			return ComputeInstance{}, err
		}
	}
	if f.createErr != nil {
		return ComputeInstance{}, f.createErr
	}
	if f.instances == nil {
		f.instances = map[string]ComputeInstance{}
	}
	item := ComputeInstance{
		ID:           "inst_created",
		InstanceType: req.InstanceType,
		Sector:       req.Sector,
		Region:       "us-west",
		IP:           "203.0.113.42",
		Status:       InstanceStatusReady,
	}
	f.instances[item.ID] = item
	if f.createHook != nil {
		f.createHook(req, idempotencyKey, item)
	}
	return item, nil
}

func (f *fakeFalAPI) DeleteInstance(_ context.Context, id string) error {
	f.deletedIDs = append(f.deletedIDs, id)
	if f.afterDeleteHook != nil {
		f.afterDeleteHook(id)
	}
	if f.deleteErr != nil {
		if f.removeBeforeDeleteError {
			delete(f.instances, id)
		}
		return f.deleteErr
	}
	if !f.retainDeletedInstance {
		delete(f.instances, id)
	}
	return nil
}

func newFalTestBackend(t *testing.T, api *fakeFalAPI) *backend {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("XDG_STATE_HOME", home)
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.Fal.APIKey = "test-key"
	cfg.Fal.APIURL = "http://127.0.0.1:8080/v1"
	applyFalDefaults(&cfg)
	b := &backend{
		spec:         Provider{}.Spec(),
		cfg:          cfg,
		rt:           core.Runtime{Stdout: io.Discard, Stderr: io.Discard},
		pollInterval: time.Nanosecond,
		pollTimeout:  time.Second,
	}
	b.clientFactory = func(Config, Runtime) (computeAPI, error) { return api, nil }
	b.waitSSH = func(context.Context, *core.SSHTarget, string, time.Duration) error { return nil }
	return b
}

func persistFalCreateRecoveryClaim(t *testing.T, b *backend, leaseID, slug, reason string, keep bool, started time.Time) (core.LeaseClaim, CreateInstanceRequest) {
	t.Helper()
	cfg := b.configForRun()
	_, publicKey, err := core.EnsureTestboxKeyForConfig(cfg, leaseID)
	if err != nil {
		t.Fatal(err)
	}
	sector := Sector(cfg.Fal.Sector)
	if InstanceType(cfg.Fal.InstanceType) != InstanceTypeH100x8 {
		sector = ""
	}
	req := CreateInstanceRequest{InstanceType: InstanceType(cfg.Fal.InstanceType), SSHKey: publicKey, Sector: sector}
	if started.IsZero() {
		started = b.now()
	}
	claim, err := b.persistRecoveryClaimAtIfUnchanged(leaseID, slug, cfg, "", "", reason, keep, started, core.LeaseClaim{}, false, req)
	if err != nil {
		t.Fatal(err)
	}
	return claim, req
}

func markFalAcquireLifetimeClaimForTest(claim core.LeaseClaim) (core.LeaseClaim, error) {
	marked := claim
	marked.Labels = cloneLabels(claim.Labels)
	marked.Labels[falAcquireLifetimeLabel] = falAcquireLifetimeVersion
	if err := core.ReplaceLeaseClaimIfUnchangedDurable(claim.LeaseID, claim, marked); err != nil {
		return claim, err
	}
	return marked, nil
}

func TestFalAcquireCreatesInstanceWaitsAndClaimsLease(t *testing.T) {
	api := &fakeFalAPI{}
	b := newFalTestBackend(t, api)
	b.cfg.Fal.InstanceType = string(InstanceTypeH100x8)
	b.cfg.Fal.Sector = string(Sector2)
	b.cfg.Fal.User = "ubuntu"
	b.cfg.SSHUser = "ubuntu"

	lease, err := b.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "gpu-box"})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID == "" || lease.Server.CloudID != "inst_created" || lease.SSH.Host != "203.0.113.42" || lease.SSH.User != "ubuntu" {
		t.Fatalf("lease=%#v", lease)
	}
	if len(api.createRequests) != 1 {
		t.Fatalf("createRequests=%#v", api.createRequests)
	}
	req := api.createRequests[0]
	if req.InstanceType != InstanceTypeH100x8 || req.Sector != Sector2 || !strings.HasPrefix(req.SSHKey, "ssh-") {
		t.Fatalf("create request=%#v", req)
	}
	if len(api.idempotency) != 1 || api.idempotency[0] != lease.LeaseID {
		t.Fatalf("idempotency=%#v lease=%s", api.idempotency, lease.LeaseID)
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider("gpu-box", providerName)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if claim.CloudID != "inst_created" || claim.SSHHost != "203.0.113.42" || claim.Labels["provider"] != providerName || claim.Labels["sector"] != string(Sector2) {
		t.Fatalf("claim=%#v", claim)
	}
	if claim.ProviderScope != falClaimScope(b.cfg) {
		t.Fatalf("claim scope=%q want %q", claim.ProviderScope, falClaimScope(b.cfg))
	}
	if claim.Labels[falCredentialBindingLabel] == "" {
		t.Fatalf("claim missing credential binding: %#v", claim.Labels)
	}
	if lease.Server.Labels[falCredentialBindingLabel] != "" {
		t.Fatalf("credential binding leaked into lease labels: %#v", lease.Server.Labels)
	}
}

func TestFalAcquirePersistsReplayIntentBeforeProviderMutation(t *testing.T) {
	api := &fakeFalAPI{}
	b := newFalTestBackend(t, api)
	observed := false
	api.beforeCreateHook = func(leaseID string) {
		data, err := os.ReadFile(filepath.Join(os.Getenv("XDG_STATE_HOME"), "crabbox", "claims", leaseID+".json"))
		if err != nil {
			t.Fatalf("read pre-create claim: %v", err)
		}
		var claim core.LeaseClaim
		if err := json.Unmarshal(data, &claim); err != nil {
			t.Fatalf("decode pre-create claim: %v", err)
		}
		if claim.CloudID != "" || claim.Labels["recovery"] != "ambiguous-create-inflight" || claim.Labels["create_started_at"] == "" || claim.Labels[falAcquireLifetimeLabel] != falAcquireLifetimeVersion || claim.Labels[falCredentialBindingLabel] != falCredentialBinding(b.cfg) {
			t.Fatalf("pre-create claim=%#v", claim)
		}
		observed = true
	}

	lease, err := b.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "durable-intent"})
	if err != nil {
		t.Fatal(err)
	}
	if !observed || lease.Server.CloudID != "inst_created" {
		t.Fatalf("intent observed=%v lease=%#v", observed, lease)
	}
	snapshot, exists, set := core.ServerLeaseClaimSnapshot(lease.Server)
	if !set || !exists || snapshot.LeaseID != lease.LeaseID || snapshot.CloudID != lease.Server.CloudID {
		t.Fatalf("ready claim snapshot=%#v exists=%t set=%t", snapshot, exists, set)
	}
	if snapshot.Labels[falAcquireLifetimeLabel] != falAcquireLifetimeVersion {
		t.Fatalf("ready claim lifetime protocol=%q", snapshot.Labels[falAcquireLifetimeLabel])
	}
	if lease.Server.Labels[falAcquireLifetimeLabel] != "" {
		t.Fatalf("internal lifetime marker leaked to server labels: %#v", lease.Server.Labels)
	}
}

func TestFalAmbiguousCreateReplayRejectsCredentialRotation(t *testing.T) {
	api := &fakeFalAPI{}
	b := newFalTestBackend(t, api)
	const leaseID = "cbx_replay_credential"
	claim, _ := persistFalCreateRecoveryClaim(t, b, leaseID, "replay-credential", "ambiguous-create", false, b.now())
	keyPath, keyErr := core.TestboxKeyPath(leaseID)
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	originalBinding := claim.Labels[falCredentialBindingLabel]
	b.cfg.Fal.APIKey = "rotated-test-key"

	_, err := b.recoverAmbiguousCreateForRelease(context.Background(), api, claim, b.configForRun())
	if err == nil || !strings.Contains(err.Error(), "credential identity") {
		t.Fatalf("recovery err=%v", err)
	}
	if len(api.createRequests) != 0 || len(api.deletedIDs) != 0 {
		t.Fatalf("credential mismatch mutated provider create=%d delete=%#v", len(api.createRequests), api.deletedIDs)
	}
	retained, exists, readErr := core.ReadLeaseClaimWithPresence(leaseID)
	if readErr != nil || !exists || retained.Labels[falCredentialBindingLabel] != originalBinding {
		t.Fatalf("claim=%#v exists=%t err=%v", retained, exists, readErr)
	}
	if _, statErr := os.Stat(keyPath); statErr != nil {
		t.Fatalf("credential mismatch removed recovery key: %v", statErr)
	}
}

func TestFalCreateIntentReplayAtomicallyAddsAcquireLifetimeMarker(t *testing.T) {
	api := &fakeFalAPI{}
	b := newFalTestBackend(t, api)
	const leaseID = "cbx_replay_lifetime"
	claim, request := persistFalCreateRecoveryClaim(t, b, leaseID, "replay-lifetime", "create-intent", false, b.now())
	unmarked := claim
	unmarked.Labels = cloneLabels(claim.Labels)
	delete(unmarked.Labels, falAcquireLifetimeLabel)
	if err := core.ReplaceLeaseClaimIfUnchangedDurable(leaseID, claim, unmarked); err != nil {
		t.Fatal(err)
	}
	api.beforeCreateHook = func(string) {
		current, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
		if err != nil || !exists || current.Labels[falAcquireLifetimeLabel] != falAcquireLifetimeVersion {
			t.Fatalf("inflight claim=%#v exists=%t err=%v", current, exists, err)
		}
	}

	created, updated, ambiguous, err := b.replayFalCreateWithClaim(context.Background(), api, request, unmarked, b.configForRun(), "provisioning", false)
	if err != nil || ambiguous || created.ID == "" || updated.CloudID != created.ID || updated.Labels[falAcquireLifetimeLabel] != falAcquireLifetimeVersion {
		t.Fatalf("created=%#v updated=%#v ambiguous=%t err=%v", created, updated, ambiguous, err)
	}
	if err := b.deleteClaimedFalInstance(context.Background(), api, updated, b.configForRun(), created.ID); err != nil {
		t.Fatal(err)
	}
}

func TestFalAcquireAbortsBeforeMutationWhenIntentPersistenceFails(t *testing.T) {
	api := &fakeFalAPI{}
	b := newFalTestBackend(t, api)
	var leaseID string
	b.persistCreateIntent = func(id, _ string, _ Config, _ string, _ bool, _ time.Time, _ CreateInstanceRequest) (core.LeaseClaim, error) {
		leaseID = id
		return core.LeaseClaim{}, errors.New("claim store unavailable")
	}

	_, err := b.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "intent-write-fail"})
	if err == nil || !strings.Contains(err.Error(), "persist fal create intent before provider mutation") {
		t.Fatalf("acquire err=%v", err)
	}
	if len(api.createRequests) != 0 {
		t.Fatalf("provider mutation occurred without durable intent: %#v", api.createRequests)
	}
	keyPath, keyErr := core.TestboxKeyPath(leaseID)
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	if _, statErr := os.Stat(keyPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("rejected intent key still exists: %v", statErr)
	}
}

func TestFalAcquireAbortsBeforeIntentWhenKeySyncFails(t *testing.T) {
	api := &fakeFalAPI{}
	b := newFalTestBackend(t, api)
	var leaseID string
	b.ensureCreateKey = func(_ Config, id string) (string, string, error) {
		leaseID = id
		return "", "", errors.New("key sync unavailable")
	}

	_, err := b.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "key-sync-fail"})
	if err == nil || !strings.Contains(err.Error(), "key sync unavailable") {
		t.Fatalf("acquire err=%v", err)
	}
	if len(api.createRequests) != 0 {
		t.Fatalf("provider mutation occurred without durable key: %#v", api.createRequests)
	}
	if _, exists, readErr := core.ReadLeaseClaimWithPresence(leaseID); readErr != nil || exists {
		t.Fatalf("claim exists=%v err=%v", exists, readErr)
	}
	keyPath, keyErr := core.TestboxKeyPath(leaseID)
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	if _, statErr := os.Stat(keyPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unsynced key still exists: %v", statErr)
	}
}

func TestFalAcquireCleansKeyWhenFailedIntentResultWasNeverWritten(t *testing.T) {
	b := newFalTestBackend(t, &fakeFalAPI{})
	var leaseID string
	b.persistCreateIntent = func(id, _ string, _ Config, _ string, _ bool, _ time.Time, _ CreateInstanceRequest) (core.LeaseClaim, error) {
		leaseID = id
		return core.LeaseClaim{LeaseID: id}, errors.New("temp-file sync unavailable")
	}
	if _, err := b.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "unwritten-intent"}); err == nil || !strings.Contains(err.Error(), "temp-file sync unavailable") {
		t.Fatalf("acquire err=%v", err)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(leaseID); err != nil || exists {
		t.Fatalf("claim exists=%t err=%v", exists, err)
	}
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(keyPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unwritten intent key still exists: %v", statErr)
	}
}

func TestFalAcquireCleansUpPartiallyPersistedIntentBeforeReturning(t *testing.T) {
	api := &fakeFalAPI{}
	b := newFalTestBackend(t, api)
	var leaseID string
	b.persistCreateIntent = func(id, slug string, cfg Config, repoRoot string, keep bool, started time.Time, req CreateInstanceRequest) (core.LeaseClaim, error) {
		leaseID = id
		claim, err := b.persistRecoveryClaimAtIfUnchanged(id, slug, cfg, repoRoot, "", "create-intent", keep, started, core.LeaseClaim{}, false, req)
		if err != nil {
			return core.LeaseClaim{}, err
		}
		return claim, errors.New("directory sync unavailable")
	}

	_, err := b.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "partial-intent"})
	if err == nil || !strings.Contains(err.Error(), "directory sync unavailable") {
		t.Fatalf("acquire err=%v", err)
	}
	if len(api.createRequests) != 0 {
		t.Fatalf("provider mutation occurred after rejected intent write: %#v", api.createRequests)
	}
	if _, exists, readErr := core.ReadLeaseClaimWithPresence(leaseID); readErr != nil || exists {
		t.Fatalf("claim exists=%v err=%v", exists, readErr)
	}
	keyPath, keyErr := core.TestboxKeyPath(leaseID)
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	if _, statErr := os.Stat(keyPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("partial intent key still exists: %v", statErr)
	}
}

func TestFalAcquireCancellationBeforeProviderMutationCleansIntentAndKey(t *testing.T) {
	api := &fakeFalAPI{}
	b := newFalTestBackend(t, api)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var leaseID string
	var unlock func()
	b.persistCreateIntent = func(id, slug string, cfg Config, repoRoot string, keep bool, started time.Time, req CreateInstanceRequest) (core.LeaseClaim, error) {
		leaseID = id
		claim, err := b.persistRecoveryClaimAtIfUnchanged(id, slug, cfg, repoRoot, "", "create-intent", keep, started, core.LeaseClaim{}, false, req)
		if err != nil {
			return core.LeaseClaim{}, err
		}
		unlock, err = lockFalLeaseOperation(context.Background(), id)
		if err != nil {
			return core.LeaseClaim{}, err
		}
		cancel()
		return claim, nil
	}
	_, err := b.Acquire(ctx, core.AcquireRequest{RequestedSlug: "cancel-before-create"})
	if unlock != nil {
		unlock()
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("acquire err=%v", err)
	}
	if len(api.createRequests) != 0 {
		t.Fatalf("provider mutation occurred after canceled lock wait: %#v", api.createRequests)
	}
	if _, exists, readErr := core.ReadLeaseClaimWithPresence(leaseID); readErr != nil || exists {
		t.Fatalf("claim exists=%t err=%v", exists, readErr)
	}
	keyPath, keyErr := core.TestboxKeyPath(leaseID)
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	if _, statErr := os.Stat(keyPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("canceled create key still exists: %v", statErr)
	}
}

func TestFalAcquireFinalPublishDistinguishesConcurrentDeleteFromClaimLoss(t *testing.T) {
	for _, providerAbsent := range []bool{false, true} {
		t.Run(fmt.Sprintf("provider_absent_%t", providerAbsent), func(t *testing.T) {
			api := &fakeFalAPI{}
			b := newFalTestBackend(t, api)
			b.waitSSH = func(context.Context, *core.SSHTarget, string, time.Duration) error {
				claims, err := core.ListLeaseClaimsWithPrefix("cbx_")
				if err != nil || len(claims) != 1 {
					return fmt.Errorf("claims=%d err=%w", len(claims), err)
				}
				if err := core.RemoveLeaseClaimIfUnchanged(claims[0].LeaseID, claims[0]); err != nil {
					return err
				}
				if providerAbsent {
					delete(api.instances, "inst_created")
					return core.RemoveStoredTestboxKeyWithError(claims[0].LeaseID)
				}
				return nil
			}

			_, err := b.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "publish-race"})
			if err == nil {
				t.Fatal("expected final publication race")
			}
			if providerAbsent {
				if !errors.Is(err, errFalClaimMutationSuperseded) || len(api.deletedIDs) != 0 {
					t.Fatalf("superseded acquire err=%v deleted=%#v", err, api.deletedIDs)
				}
			} else if len(api.deletedIDs) != 1 || api.deletedIDs[0] != "inst_created" {
				t.Fatalf("live claim-loss cleanup err=%v deleted=%#v", err, api.deletedIDs)
			}
			claims, readErr := core.ListLeaseClaimsWithPrefix("cbx_")
			if readErr != nil || len(claims) != 0 {
				t.Fatalf("claims=%#v err=%v", claims, readErr)
			}
		})
	}
}

func TestFalAcquireFinalPublishRetainsOwnershipWhenGetAbsenceConflictsWithInventory(t *testing.T) {
	notFound := &APIError{StatusCode: 404, Status: "404 Not Found", Message: "not found"}
	api := &fakeFalAPI{}
	b := newFalTestBackend(t, api)
	b.waitSSH = func(context.Context, *core.SSHTarget, string, time.Duration) error {
		claims, err := core.ListLeaseClaimsWithPrefix("cbx_")
		if err != nil || len(claims) != 1 {
			return fmt.Errorf("claims=%d err=%w", len(claims), err)
		}
		if err := core.RemoveLeaseClaimIfUnchanged(claims[0].LeaseID, claims[0]); err != nil {
			return err
		}
		api.getErr = notFound
		return nil
	}

	_, err := b.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "publish-masked-absence"})
	if err == nil || errors.Is(err, errFalClaimMutationSuperseded) || !strings.Contains(err.Error(), errFalProviderAbsenceNotAccountBound.Error()) {
		t.Fatalf("acquire err=%v", err)
	}
	if len(api.deletedIDs) != 0 {
		t.Fatalf("provider mutation crossed conflicting absence proof: %#v", api.deletedIDs)
	}
	claims, readErr := core.ListLeaseClaimsWithPrefix("cbx_")
	if readErr != nil || len(claims) != 1 || claims[0].CloudID != "inst_created" || claims[0].Labels["recovery"] != "rollback-cleanup" {
		t.Fatalf("claims=%#v err=%v", claims, readErr)
	}
}

func TestFalAcquireFailureRejectsConcurrentStopAndRollsBack(t *testing.T) {
	api := &fakeFalAPI{}
	b := newFalTestBackend(t, api)
	b.waitSSH = func(context.Context, *core.SSHTarget, string, time.Duration) error {
		claims, err := core.ListLeaseClaimsWithPrefix("cbx_")
		if err != nil || len(claims) != 1 {
			return fmt.Errorf("claims=%d err=%w", len(claims), err)
		}
		claim := claims[0]
		stopErr := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{
			LeaseID: claim.LeaseID,
			Server:  core.Server{CloudID: claim.CloudID, Provider: providerName, Labels: map[string]string{"lease": claim.LeaseID}},
		}})
		if stopErr == nil || !strings.Contains(stopErr.Error(), "acquisition is still in progress") {
			return fmt.Errorf("concurrent stop err=%v", stopErr)
		}
		if len(api.deletedIDs) != 0 {
			return fmt.Errorf("concurrent stop deleted provider instance: %#v", api.deletedIDs)
		}
		if err := core.VerifyLeaseClaimUnchanged(claim.LeaseID, claim); err != nil {
			return fmt.Errorf("concurrent stop changed in-flight claim: %w", err)
		}
		return errors.New("ssh readiness failed after rejected concurrent stop")
	}

	_, err := b.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "failure-stop-race"})
	if err == nil || errors.Is(err, errFalClaimMutationSuperseded) || !strings.Contains(err.Error(), "ssh readiness failed after rejected concurrent stop") {
		t.Fatalf("acquire err=%v", err)
	}
	if len(api.deletedIDs) != 1 || api.deletedIDs[0] != "inst_created" {
		t.Fatalf("deletedIDs=%#v", api.deletedIDs)
	}
	claims, readErr := core.ListLeaseClaimsWithPrefix("cbx_")
	if readErr != nil || len(claims) != 0 {
		t.Fatalf("claims=%#v err=%v", claims, readErr)
	}
	if len(api.idempotency) != 1 {
		t.Fatalf("idempotency=%#v", api.idempotency)
	}
	keyPath, keyErr := core.TestboxKeyPath(api.idempotency[0])
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	if _, statErr := os.Stat(keyPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("rollback key still exists: %v", statErr)
	}
}

func TestFalAcquireRejectsCredentialRebindingResolveAndRollsBack(t *testing.T) {
	api := &fakeFalAPI{}
	b := newFalTestBackend(t, api)
	b.waitSSH = func(context.Context, *core.SSHTarget, string, time.Duration) error {
		claims, err := core.ListLeaseClaimsWithPrefix("cbx_")
		if err != nil || len(claims) != 1 {
			return fmt.Errorf("claims=%d err=%w", len(claims), err)
		}
		claim := claims[0]
		b.cfg.Fal.APIKey = "rotated-test-key"
		_, resolveErr := b.Resolve(context.Background(), core.ResolveRequest{ID: claim.LeaseID})
		if resolveErr == nil || !strings.Contains(resolveErr.Error(), "acquisition is still in progress") {
			return fmt.Errorf("credential-rebinding resolve err=%v", resolveErr)
		}
		if err := core.VerifyLeaseClaimUnchanged(claim.LeaseID, claim); err != nil {
			return fmt.Errorf("credential-rebinding resolve changed in-flight claim: %w", err)
		}
		return errors.New("ssh readiness failed after rejected credential rebinding")
	}

	_, err := b.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "rebind-resolve-race"})
	if err == nil || errors.Is(err, errFalClaimMutationSuperseded) || !strings.Contains(err.Error(), "ssh readiness failed after rejected credential rebinding") {
		t.Fatalf("acquire err=%v", err)
	}
	if len(api.deletedIDs) != 1 || api.deletedIDs[0] != "inst_created" {
		t.Fatalf("deletedIDs=%#v", api.deletedIDs)
	}
	claims, readErr := core.ListLeaseClaimsWithPrefix("cbx_")
	if readErr != nil || len(claims) != 0 {
		t.Fatalf("claims=%#v err=%v", claims, readErr)
	}
	if len(api.idempotency) != 1 {
		t.Fatalf("idempotency=%#v", api.idempotency)
	}
	keyPath, keyErr := core.TestboxKeyPath(api.idempotency[0])
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	if _, statErr := os.Stat(keyPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("rollback key still exists: %v", statErr)
	}
}

func TestFalReadySnapshotPreventsPostDeleteClaimRecreation(t *testing.T) {
	api := &fakeFalAPI{}
	b := newFalTestBackend(t, api)
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "snapshot-delete"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, exists, set := core.ServerLeaseClaimSnapshot(lease.Server)
	if !set || !exists {
		t.Fatalf("snapshot=%#v exists=%t set=%t", snapshot, exists, set)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	claimServer, err := falClaimServer(lease.Server, b.configForRun())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := core.ClaimLeaseTargetForRepoConfigScopeIfUnchanged(
		lease.LeaseID, snapshot.Slug, b.cfg, falClaimScope(b.configForRun()), claimServer, lease.SSH, "/repo", time.Minute, false, snapshot, true,
	); err == nil {
		t.Fatal("deleted ready claim was recreated from a stale acquired target")
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID); err != nil || exists {
		t.Fatalf("claim exists=%t err=%v", exists, err)
	}
}

func TestFalAcquireDefaultSingleGPUOmitsSector(t *testing.T) {
	api := &fakeFalAPI{}
	b := newFalTestBackend(t, api)
	b.cfg.Fal.Sector = string(Sector1)

	if _, err := b.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "single-gpu"}); err != nil {
		t.Fatal(err)
	}
	if len(api.createRequests) != 1 {
		t.Fatalf("createRequests=%#v", api.createRequests)
	}
	req := api.createRequests[0]
	if req.InstanceType != InstanceTypeH100x1 || req.Sector != "" {
		t.Fatalf("create request=%#v", req)
	}
}

func TestFalAcquireUsesExplicitGenericServerType(t *testing.T) {
	api := &fakeFalAPI{}
	b := newFalTestBackend(t, api)
	b.cfg.ServerType = " gpu_8x_h100_sxm5 "
	b.cfg.ServerTypeExplicit = true
	b.cfg.Fal.Sector = string(Sector2)

	if _, err := b.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "generic-type"}); err != nil {
		t.Fatal(err)
	}
	if len(api.createRequests) != 1 {
		t.Fatalf("createRequests=%#v", api.createRequests)
	}
	if got := api.createRequests[0]; got.InstanceType != InstanceTypeH100x8 || got.Sector != Sector2 {
		t.Fatalf("create request=%#v", got)
	}
}

func TestFalAcquireReturnsSSHPortUpdatedByReadinessProbe(t *testing.T) {
	api := &fakeFalAPI{}
	b := newFalTestBackend(t, api)
	b.cfg.SSHPort = "2222"
	b.waitSSH = func(_ context.Context, target *core.SSHTarget, _ string, _ time.Duration) error {
		if target.Port != "2222" {
			t.Fatalf("probe received port %q, want configured 2222", target.Port)
		}
		target.Port = "22"
		return nil
	}

	lease, err := b.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "fallback-port"})
	if err != nil {
		t.Fatal(err)
	}
	if lease.SSH.Port != "22" {
		t.Fatalf("returned ssh port=%q, want readiness-updated 22", lease.SSH.Port)
	}
	claim, ok, claimErr := core.ResolveLeaseClaimForProvider("fallback-port", providerName)
	if claimErr != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, claimErr)
	}
	if claim.SSHPort != 22 {
		t.Fatalf("persisted ssh port=%d, want readiness-updated 22", claim.SSHPort)
	}
}

func TestFalAcquireReconcilesAmbiguousCreateWithIdempotentRetry(t *testing.T) {
	api := &fakeFalAPI{createErrs: []error{io.ErrUnexpectedEOF}}
	b := newFalTestBackend(t, api)

	lease, err := b.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "retry-create"})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.CloudID != "inst_created" {
		t.Fatalf("lease=%#v", lease)
	}
	if len(api.createRequests) != 2 {
		t.Fatalf("createRequests=%#v", api.createRequests)
	}
	if api.idempotency[0] == "" || api.idempotency[0] != api.idempotency[1] || api.idempotency[0] != lease.LeaseID {
		t.Fatalf("idempotency=%#v lease=%s", api.idempotency, lease.LeaseID)
	}
	claim, ok, claimErr := core.ResolveLeaseClaimForProvider("retry-create", providerName)
	if claimErr != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, claimErr)
	}
	if claim.CloudID != "inst_created" {
		t.Fatalf("claim=%#v", claim)
	}
}

func TestFalAcquireReplaysExactNormalizedCreateRequest(t *testing.T) {
	api := &fakeFalAPI{createErrs: []error{io.ErrUnexpectedEOF}}
	b := newFalTestBackend(t, api)
	b.cfg.Fal.InstanceType = " " + string(InstanceTypeH100x8) + " "
	b.cfg.Fal.Sector = " " + string(Sector2) + " "

	if _, err := b.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "normalized-replay"}); err != nil {
		t.Fatal(err)
	}
	if len(api.createRequests) != 2 || api.createRequests[0] != api.createRequests[1] {
		t.Fatalf("create requests=%#v", api.createRequests)
	}
	want := CreateInstanceRequest{InstanceType: InstanceTypeH100x8, Sector: Sector2, SSHKey: api.createRequests[0].SSHKey}
	if api.createRequests[0] != want {
		t.Fatalf("create request=%#v want %#v", api.createRequests[0], want)
	}
}

func TestFalAcquireDoesNotReplayExplicitRateLimitRejection(t *testing.T) {
	api := &fakeFalAPI{createErr: &APIError{StatusCode: 429, Status: "429 Too Many Requests", Message: "rate limited"}}
	b := newFalTestBackend(t, api)

	_, err := b.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "rate-limited"})
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("err=%v", err)
	}
	if len(api.createRequests) != 1 {
		t.Fatalf("rate-limit rejection replayed create: requests=%d", len(api.createRequests))
	}
	if _, ok, claimErr := core.ResolveLeaseClaimForProvider("rate-limited", providerName); claimErr != nil || ok {
		t.Fatalf("rate-limit rejection persisted recovery claim: ok=%v err=%v", ok, claimErr)
	}
	keyPath, keyErr := core.TestboxKeyPath(api.idempotency[0])
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	if _, statErr := os.Stat(keyPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("rate-limit rejection key still exists: %v", statErr)
	}
}

func TestFalDefinitiveCreateRejectionCannotBeReplayedAfterCleanupFailure(t *testing.T) {
	api := &fakeFalAPI{createErr: &APIError{StatusCode: 403, Status: "403 Forbidden", Message: "forbidden"}}
	b := newFalTestBackend(t, api)
	cleanupErr := errors.New("key cleanup unavailable")
	b.removeLeaseKey = func(string) error { return cleanupErr }

	_, err := b.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "terminal-rejection"})
	if err == nil || !strings.Contains(err.Error(), "forbidden") || !strings.Contains(err.Error(), cleanupErr.Error()) {
		t.Fatalf("acquire err=%v", err)
	}
	if len(api.createRequests) != 1 {
		t.Fatalf("create requests=%d want one rejected request", len(api.createRequests))
	}
	leaseID := api.idempotency[0]
	claim, exists, readErr := core.ReadLeaseClaimWithPresence(leaseID)
	if readErr != nil || !exists || claim.CloudID != "" || claim.Labels["recovery"] != falCreateRejectedRecovery {
		t.Fatalf("claim=%#v exists=%v err=%v", claim, exists, readErr)
	}

	b.removeLeaseKey = nil
	target, err := b.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: target}); err != nil {
		t.Fatal(err)
	}
	if len(api.createRequests) != 1 {
		t.Fatalf("release replayed rejected create: requests=%d", len(api.createRequests))
	}
	if _, exists, readErr := core.ReadLeaseClaimWithPresence(leaseID); readErr != nil || exists {
		t.Fatalf("terminal rejection claim exists=%v err=%v", exists, readErr)
	}
}

func TestFalOperationLockUsesDurableDirectoryPreparation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "fresh", "state"))
	oldEnsure := ensureFalClaimNamespace
	t.Cleanup(func() { ensureFalClaimNamespace = oldEnsure })
	prepared := 0
	ensureFalClaimNamespace = func() error {
		prepared++
		stateDir, err := core.CrabboxStateDir()
		if err != nil {
			return err
		}
		return os.MkdirAll(filepath.Join(stateDir, "claims"), 0o700)
	}

	unlock, err := lockFalSlugAllocation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	stateDir, err := core.CrabboxStateDir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(stateDir, "claim-locks")
	if prepared != 1 {
		t.Fatalf("state namespace preparations=%d want 1", prepared)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Fatalf("lock directory stat=%v info=%v", err, info)
	}
}

func TestFalAcquireStopsBeforeProviderWhenLockNamespaceIsNotDurable(t *testing.T) {
	oldEnsure := ensureFalClaimNamespace
	t.Cleanup(func() { ensureFalClaimNamespace = oldEnsure })
	syncErr := errors.New("lock namespace sync unavailable")
	ensureFalClaimNamespace = func() error { return syncErr }
	api := &fakeFalAPI{}
	b := newFalTestBackend(t, api)

	_, err := b.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "unsafe-lock-namespace"})
	if err == nil || !strings.Contains(err.Error(), syncErr.Error()) {
		t.Fatalf("acquire err=%v", err)
	}
	if len(api.createRequests) != 0 {
		t.Fatalf("provider mutation occurred before durable lock setup: %#v", api.createRequests)
	}
}

func TestFalAcquireRetainsIntentAfterDefinitiveReplayRejection(t *testing.T) {
	api := &fakeFalAPI{createErrs: []error{
		io.ErrUnexpectedEOF,
		&APIError{StatusCode: 401, Status: "401 Unauthorized", Message: "unauthorized"},
	}}
	b := newFalTestBackend(t, api)

	_, err := b.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "replay-rejected"})
	if err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("acquire err=%v", err)
	}
	if len(api.idempotency) != 2 || api.idempotency[0] != api.idempotency[1] {
		t.Fatalf("idempotency=%#v", api.idempotency)
	}
	claim, exists, readErr := core.ReadLeaseClaimWithPresence(api.idempotency[0])
	if readErr != nil || !exists || claim.CloudID != "" || claim.Labels["recovery"] != "ambiguous-create" {
		t.Fatalf("claim=%#v exists=%v err=%v", claim, exists, readErr)
	}
	keyPath, keyErr := core.TestboxKeyPath(api.idempotency[0])
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	if _, statErr := os.Stat(keyPath); statErr != nil {
		t.Fatalf("rejected replay key missing: %v", statErr)
	}
}

func TestFalAcquireAnchorsTTLToCreateAttempt(t *testing.T) {
	api := &fakeFalAPI{}
	b := newFalTestBackend(t, api)
	started := time.Date(2026, time.July, 9, 12, 0, 0, 0, time.UTC)
	clock := &mutableFalClock{now: started}
	b.rt.Clock = clock
	b.cfg.TTL = 20 * time.Minute
	b.cfg.IdleTimeout = time.Hour
	b.waitSSH = func(context.Context, *core.SSHTarget, string, time.Duration) error {
		clock.now = started.Add(10 * time.Minute)
		return nil
	}

	lease, err := b.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "ttl-anchor"})
	if err != nil {
		t.Fatal(err)
	}
	wantCreated := strconv.FormatInt(started.Unix(), 10)
	wantExpires := strconv.FormatInt(started.Add(20*time.Minute).Unix(), 10)
	if lease.Server.Labels["created_at"] != wantCreated || lease.Server.Labels["expires_at"] != wantExpires {
		t.Fatalf("lease labels=%#v want created=%s expires=%s", lease.Server.Labels, wantCreated, wantExpires)
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider("ttl-anchor", providerName)
	if err != nil || !ok || claim.Labels["created_at"] != wantCreated || claim.Labels["expires_at"] != wantExpires {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
	}
}

func TestFalReconcileRefusesReplayAfterIdempotencyWindow(t *testing.T) {
	api := &fakeFalAPI{}
	b := newFalTestBackend(t, api)
	started := b.now().Add(-falCreateRecoveryWindow)
	req := CreateInstanceRequest{InstanceType: InstanceTypeH100x1, SSHKey: "ssh-ed25519 test"}
	claim, err := b.persistRecoveryClaimAtIfUnchanged("cbx_abcdef123456", "expired-replay", b.configForRun(), "", "", "ambiguous-create", false, started, core.LeaseClaim{}, false, req)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = b.reconcileAmbiguousCreate(context.Background(), api, req, claim, b.configForRun(), false, io.ErrUnexpectedEOF)
	if err == nil || !strings.Contains(err.Error(), "recovery window expired") {
		t.Fatalf("err=%v", err)
	}
	if len(api.createRequests) != 0 {
		t.Fatalf("expired idempotency replay issued create: %#v", api.createRequests)
	}
}

func TestFalReconcileDeadlinesReplayInsideIdempotencyWindow(t *testing.T) {
	api := &fakeFalAPI{blockCreateUntilContext: true}
	b := newFalTestBackend(t, api)
	started := time.Now().Add(-falCreateRecoveryWindow + 2*time.Second)
	req := CreateInstanceRequest{InstanceType: InstanceTypeH100x1, SSHKey: "ssh-ed25519 test"}
	claim, err := b.persistRecoveryClaimAtIfUnchanged("cbx_abcdef123456", "bounded-replay", b.configForRun(), "", "", "ambiguous-create", false, started, core.LeaseClaim{}, false, req)
	if err != nil {
		t.Fatal(err)
	}
	begin := time.Now()
	_, _, err = b.reconcileAmbiguousCreate(context.Background(), api, req, claim, b.configForRun(), false, io.ErrUnexpectedEOF)
	if err == nil || !strings.Contains(err.Error(), "idempotency replay window expired") {
		t.Fatalf("err=%v", err)
	}
	if elapsed := time.Since(begin); elapsed > 3*time.Second {
		t.Fatalf("replay outlived idempotency deadline: %v", elapsed)
	}
	if len(api.createRequests) != 1 {
		t.Fatalf("create requests=%d, want one bounded replay", len(api.createRequests))
	}
}

func TestFalStopRecoversAmbiguousCreateWithExactIdempotentRequest(t *testing.T) {
	api := &fakeFalAPI{createErrs: []error{io.ErrUnexpectedEOF, io.ErrUnexpectedEOF, io.ErrUnexpectedEOF, io.ErrUnexpectedEOF}}
	b := newFalTestBackend(t, api)
	b.cfg.Fal.InstanceType = string(InstanceTypeH100x8)
	b.cfg.Fal.Sector = string(Sector2)

	_, err := b.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "unreconciled-create"})
	if err == nil || !strings.Contains(err.Error(), "indeterminate after idempotent retry") ||
		!strings.Contains(err.Error(), "lease=") || !strings.Contains(err.Error(), "slug=unreconciled-create") ||
		!strings.Contains(err.Error(), "crabbox stop --provider fal --id") || !strings.Contains(err.Error(), "immediately") {
		t.Fatalf("err=%v", err)
	}
	if len(api.createRequests) != 4 {
		t.Fatalf("createRequests=%#v", api.createRequests)
	}
	claim, ok, claimErr := core.ResolveLeaseClaimForProvider("unreconciled-create", providerName)
	if claimErr != nil || !ok || claim.CloudID != "" || claim.Labels["recovery"] != "ambiguous-create" {
		t.Fatalf("recovery claim=%#v ok=%v err=%v", claim, ok, claimErr)
	}
	if claim.Labels["create_started_at"] == "" {
		t.Fatalf("recovery claim missing initial mutation time: %#v", claim.Labels)
	}
	views, err := b.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Status != "ambiguous-create" || views[0].CloudID != "" {
		t.Fatalf("recovery views=%#v", views)
	}
	target, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "unreconciled-create", ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if target.Server.CloudID != "inst_created" {
		t.Fatalf("target=%#v", target)
	}
	if len(api.createRequests) != 5 || api.idempotency[4] != claim.LeaseID {
		t.Fatalf("createRequests=%d idempotency=%#v", len(api.createRequests), api.idempotency)
	}
	if got := api.createRequests[4]; got.InstanceType != InstanceTypeH100x8 || got.Sector != Sector2 || !strings.HasPrefix(got.SSHKey, "ssh-") {
		t.Fatalf("recovery request=%#v", got)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: target}); err != nil {
		t.Fatal(err)
	}
	if len(api.deletedIDs) != 1 || api.deletedIDs[0] != "inst_created" {
		t.Fatalf("deletedIDs=%#v", api.deletedIDs)
	}
	if _, ok, claimErr := core.ResolveLeaseClaimForProvider("unreconciled-create", providerName); claimErr != nil || ok {
		t.Fatalf("recovery claim retained ok=%v err=%v", ok, claimErr)
	}
}

func TestFalStopRecoveryRetainsIntentAfterDefinitiveRejection(t *testing.T) {
	api := &fakeFalAPI{createErr: &APIError{StatusCode: 403, Status: "403 Forbidden", Message: "forbidden"}}
	b := newFalTestBackend(t, api)
	const leaseID = "cbx_rejected_recovery"
	claim, _ := persistFalCreateRecoveryClaim(t, b, leaseID, "rejected-recovery", "ambiguous-create", false, time.Time{})

	_, err := b.recoverAmbiguousCreateForRelease(context.Background(), api, claim, b.configForRun())
	if err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("recovery err=%v", err)
	}
	retained, exists, readErr := core.ReadLeaseClaimWithPresence(leaseID)
	if readErr != nil || !exists || retained.CloudID != "" || retained.Labels["recovery"] != "ambiguous-create" {
		t.Fatalf("claim=%#v exists=%v err=%v", retained, exists, readErr)
	}
	keyPath, keyErr := core.TestboxKeyPath(leaseID)
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	if _, statErr := os.Stat(keyPath); statErr != nil {
		t.Fatalf("rejected recovery key missing: %v", statErr)
	}
}

func TestFalStopCancelsCreateIntentWithoutProviderMutation(t *testing.T) {
	api := &fakeFalAPI{}
	b := newFalTestBackend(t, api)
	const leaseID = "cbx_cancel_create_intent"
	claim, _ := persistFalCreateRecoveryClaim(t, b, leaseID, "cancel-create-intent", "create-intent", false, time.Time{})

	target, err := b.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if target.LeaseID != leaseID || target.Server.CloudID != "" {
		t.Fatalf("target=%#v", target)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: target}); err != nil {
		t.Fatal(err)
	}
	if len(api.createRequests) != 0 || len(api.deletedIDs) != 0 {
		t.Fatalf("provider mutations create=%d delete=%#v", len(api.createRequests), api.deletedIDs)
	}
	if _, exists, readErr := core.ReadLeaseClaimWithPresence(claim.LeaseID); readErr != nil || exists {
		t.Fatalf("claim exists=%v err=%v", exists, readErr)
	}
	keyPath, keyErr := core.TestboxKeyPath(leaseID)
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	if _, statErr := os.Stat(keyPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("cancelled intent key still exists: %v", statErr)
	}
}

func TestFalStopRecoveryRefusesMissingOrReplacedKeyBeforeMutation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(string) error
		message string
	}{
		{
			name:    "missing public key",
			mutate:  func(path string) error { return os.Remove(path + ".pub") },
			message: "public key is unavailable",
		},
		{
			name: "replaced public key",
			mutate: func(path string) error {
				return os.WriteFile(path+".pub", []byte("ssh-ed25519 replaced-key\n"), 0o600)
			},
			message: "create request changed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeFalAPI{}
			b := newFalTestBackend(t, api)
			leaseID := "cbx_" + strings.ReplaceAll(tc.name, " ", "_")
			claim, _ := persistFalCreateRecoveryClaim(t, b, leaseID, "key-binding", "ambiguous-create", false, time.Time{})
			keyPath, err := core.TestboxKeyPath(leaseID)
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.mutate(keyPath); err != nil {
				t.Fatal(err)
			}

			if _, err := b.recoverAmbiguousCreateForRelease(context.Background(), api, claim, b.configForRun()); err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("recovery err=%v", err)
			}
			if len(api.createRequests) != 0 {
				t.Fatalf("provider mutation occurred with changed key: %#v", api.createRequests)
			}
			if _, exists, err := core.ReadLeaseClaimWithPresence(leaseID); err != nil || !exists {
				t.Fatalf("claim exists=%v err=%v", exists, err)
			}
		})
	}
}

func TestFalStopRecoveryRefusesWhitespaceChangedRequestBeforeMutation(t *testing.T) {
	api := &fakeFalAPI{}
	b := newFalTestBackend(t, api)
	b.cfg.Fal.InstanceType = string(InstanceTypeH100x8)
	b.cfg.Fal.Sector = string(Sector2)
	claim, _ := persistFalCreateRecoveryClaim(t, b, "cbx_changed_request", "changed-request", "ambiguous-create", false, time.Time{})
	labels := cloneLabels(claim.Labels)
	labels["sector"] = " " + string(Sector2) + " "
	claim, err := core.UpdateLeaseClaimLabelsIfUnchanged(claim.LeaseID, claim, labels)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := b.recoverAmbiguousCreateForRelease(context.Background(), api, claim, b.configForRun()); err == nil || !strings.Contains(err.Error(), "create request changed") {
		t.Fatalf("recovery err=%v", err)
	}
	if len(api.createRequests) != 0 {
		t.Fatalf("provider mutation occurred with changed request: %#v", api.createRequests)
	}
}

func TestFalAmbiguousCreateRecoveryResumesInterruptedInflightAttempt(t *testing.T) {
	api := &fakeFalAPI{}
	b := newFalTestBackend(t, api)
	claim, _ := persistFalCreateRecoveryClaim(t, b, "cbx_recovery123", "recovery-race", "ambiguous-create", false, time.Time{})
	inflight, err := falRecoveryClaimReplacement(claim, b.configForRun(), "", "ambiguous-create-inflight", false)
	if err != nil {
		t.Fatal(err)
	}
	inflight.Labels[falCreateAttemptLabel] = "interrupted-attempt"
	if err := core.ReplaceLeaseClaimIfUnchangedDurable(claim.LeaseID, claim, inflight); err != nil {
		t.Fatal(err)
	}

	updated, err := b.recoverAmbiguousCreateForRelease(context.Background(), api, inflight, b.configForRun())
	if err != nil {
		t.Fatal(err)
	}
	if updated.CloudID != "inst_created" || updated.Labels["recovery"] != "rollback-cleanup" {
		t.Fatalf("updated claim=%#v", updated)
	}
	if len(api.deletedIDs) != 0 {
		t.Fatalf("recovery deleted the resumed instance: %#v", api.deletedIDs)
	}
}

func TestFalConcurrentAmbiguousCreateRecoveryIssuesOneProviderRequest(t *testing.T) {
	api := &fakeFalAPI{}
	b := newFalTestBackend(t, api)
	claim, _ := persistFalCreateRecoveryClaim(t, b, "cbx_concurrent_recovery", "concurrent-recovery", "ambiguous-create", false, time.Time{})
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	api.beforeCreateHook = func(string) {
		once.Do(func() {
			close(started)
			<-release
		})
	}
	type result struct {
		claim core.LeaseClaim
		err   error
	}
	firstResult := make(chan result, 1)
	secondResult := make(chan result, 1)
	go func() {
		updated, err := b.recoverAmbiguousCreateForRelease(context.Background(), api, claim, b.configForRun())
		firstResult <- result{claim: updated, err: err}
	}()
	<-started
	go func() {
		updated, err := b.recoverAmbiguousCreateForRelease(context.Background(), api, claim, b.configForRun())
		secondResult <- result{claim: updated, err: err}
	}()
	close(release)

	first := <-firstResult
	second := <-secondResult
	if first.err != nil || first.claim.CloudID != "inst_created" {
		t.Fatalf("first recovery claim=%#v err=%v", first.claim, first.err)
	}
	if second.err == nil || !strings.Contains(second.err.Error(), "claim changed") {
		t.Fatalf("second recovery claim=%#v err=%v", second.claim, second.err)
	}
	if len(api.createRequests) != 1 {
		t.Fatalf("provider create requests=%d want 1", len(api.createRequests))
	}
}

func TestFalCreateWaiterHonorsCancellationWithoutHoldingClaimLock(t *testing.T) {
	api := &fakeFalAPI{}
	b := newFalTestBackend(t, api)
	claim, _ := persistFalCreateRecoveryClaim(t, b, "cbx_cancel_waiter", "cancel-waiter", "ambiguous-create", false, time.Time{})
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	api.beforeCreateHook = func(string) {
		once.Do(func() {
			close(started)
			<-release
		})
	}

	firstResult := make(chan error, 1)
	go func() {
		_, err := b.recoverAmbiguousCreateForRelease(context.Background(), api, claim, b.configForRun())
		firstResult <- err
	}()
	<-started

	inflight, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID)
	if err != nil || !exists {
		t.Fatalf("inflight claim=%#v exists=%t err=%v", inflight, exists, err)
	}
	if inflight.Labels["recovery"] != "ambiguous-create-inflight" || inflight.Labels[falCreateAttemptLabel] == "" {
		t.Fatalf("provider POST started without durable inflight claim: %#v", inflight)
	}
	verifyDone := make(chan error, 1)
	go func() { verifyDone <- core.VerifyLeaseClaimUnchanged(claim.LeaseID, inflight) }()
	select {
	case err := <-verifyDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("provider POST held the generic claim lock")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := b.recoverAmbiguousCreateForRelease(ctx, api, claim, b.configForRun()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second recovery err=%v want context deadline", err)
	}
	if len(api.createRequests) != 1 {
		t.Fatalf("provider create requests=%d want 1", len(api.createRequests))
	}
	close(release)
	if err := <-firstResult; err != nil {
		t.Fatal(err)
	}
}

func TestFalOperationLocksSerializeAcrossProcesses(t *testing.T) {
	if helper := os.Getenv("CRABBOX_FAL_LOCK_HELPER"); helper != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		var err error
		switch helper {
		case "lease":
			_, err = lockFalLeaseOperation(ctx, "cbx_cross_process")
		case "acquire":
			_, err = lockFalAcquireLifetime(ctx, "cbx_cross_process")
		case "slug":
			_, err = lockFalSlugAllocation(ctx)
		default:
			t.Fatalf("unknown lock helper %q", helper)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("lock err=%v want context deadline", err)
		}
		return
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, test := range []struct {
		name string
		lock func(context.Context) (func(), error)
	}{
		{name: "lease", lock: func(ctx context.Context) (func(), error) {
			return lockFalLeaseOperation(ctx, "cbx_cross_process")
		}},
		{name: "acquire", lock: func(ctx context.Context) (func(), error) {
			return lockFalAcquireLifetime(ctx, "cbx_cross_process")
		}},
		{name: "slug", lock: lockFalSlugAllocation},
	} {
		t.Run(test.name, func(t *testing.T) {
			unlock, err := test.lock(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestFalOperationLocksSerializeAcrossProcesses$")
			cmd.Env = append(os.Environ(), "CRABBOX_FAL_LOCK_HELPER="+test.name)
			output, runErr := cmd.CombinedOutput()
			unlock()
			if runErr != nil {
				t.Fatalf("helper err=%v output=%s", runErr, output)
			}
		})
	}
}

func TestFalRequestedSlugReservationSerializesClaimPublication(t *testing.T) {
	b := newFalTestBackend(t, &fakeFalAPI{})
	unlock, err := lockFalSlugAllocation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		slug string
		err  error
	}
	results := make(chan result, 2)
	for _, leaseID := range []string{"cbx_slug_race_one", "cbx_slug_race_two"} {
		leaseID := leaseID
		go func() {
			unlockSlug, err := lockFalSlugAllocation(context.Background())
			if err != nil {
				results <- result{err: err}
				return
			}
			defer unlockSlug()
			slug, err := core.AllocateClaimLeaseSlug(leaseID, "shared-name")
			if err == nil {
				_, err = b.persistRecoveryClaimAtIfUnchanged(
					leaseID, slug, b.configForRun(), "", "", "create-intent", false, b.now(), core.LeaseClaim{}, false,
					CreateInstanceRequest{InstanceType: InstanceTypeH100x1, SSHKey: "ssh-ed25519 synthetic"},
				)
			}
			results <- result{slug: slug, err: err}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	unlock()
	first := <-results
	second := <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("results=%#v %#v", first, second)
	}
	if first.slug == second.slug {
		t.Fatalf("concurrent requested slugs collided: %q", first.slug)
	}
	if first.slug != "shared-name" && second.slug != "shared-name" {
		t.Fatalf("requested slug was not preserved: %q %q", first.slug, second.slug)
	}
}

func TestFalSlugReservationSerializesCollisionSuffixAgainstDirectRequest(t *testing.T) {
	b := newFalTestBackend(t, &fakeFalAPI{})
	if _, err := b.persistRecoveryClaimAtIfUnchanged(
		"cbx_slug_seed", "shared-name", b.configForRun(), "", "", "create-intent", false, b.now(), core.LeaseClaim{}, false,
		CreateInstanceRequest{InstanceType: InstanceTypeH100x1, SSHKey: "ssh-ed25519 seed"},
	); err != nil {
		t.Fatal(err)
	}
	const firstLeaseID = "cbx_slug_suffix_one"
	const secondLeaseID = "cbx_slug_suffix_two"
	collisionSlug, err := core.AllocateClaimLeaseSlug(firstLeaseID, "shared-name")
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := lockFalSlugAllocation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		slug string
		err  error
	}
	results := make(chan result, 2)
	for _, item := range []struct {
		leaseID   string
		requested string
	}{
		{leaseID: firstLeaseID, requested: "shared-name"},
		{leaseID: secondLeaseID, requested: collisionSlug},
	} {
		item := item
		go func() {
			unlockSlug, err := lockFalSlugAllocation(context.Background())
			if err != nil {
				results <- result{err: err}
				return
			}
			defer unlockSlug()
			slug, err := core.AllocateClaimLeaseSlug(item.leaseID, item.requested)
			if err == nil {
				_, err = b.persistRecoveryClaimAtIfUnchanged(
					item.leaseID, slug, b.configForRun(), "", "", "create-intent", false, b.now(), core.LeaseClaim{}, false,
					CreateInstanceRequest{InstanceType: InstanceTypeH100x1, SSHKey: "ssh-ed25519 synthetic"},
				)
			}
			results <- result{slug: slug, err: err}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	unlock()
	first := <-results
	second := <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("results=%#v %#v", first, second)
	}
	if first.slug == second.slug {
		t.Fatalf("collision suffix was published twice: %q", first.slug)
	}
}

func TestFalStopRecoveryDeadlinesReplayInsideIdempotencyWindow(t *testing.T) {
	api := &fakeFalAPI{blockCreateUntilContext: true}
	b := newFalTestBackend(t, api)
	created := time.Unix(time.Now().Unix(), 0).UTC()
	clock := &mutableFalClock{now: created.Add(falCreateRecoveryWindow - 75*time.Millisecond)}
	b.rt.Clock = clock
	const leaseID = "cbx_recovery789"
	claim, _ := persistFalCreateRecoveryClaim(t, b, leaseID, "recovery-deadline", "ambiguous-create", false, created)

	begin := time.Now()
	_, err := b.recoverAmbiguousCreateForRelease(context.Background(), api, claim, b.configForRun())
	if err == nil || !strings.Contains(err.Error(), "recovery retry failed") || !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("recovery err=%v", err)
	}
	if elapsed := time.Since(begin); elapsed > time.Second {
		t.Fatalf("stop recovery outlived idempotency deadline: %v", elapsed)
	}
	if _, exists, readErr := core.ReadLeaseClaimWithPresence(leaseID); readErr != nil || !exists {
		t.Fatalf("claim exists=%v err=%v", exists, readErr)
	}
}

func TestFalAmbiguousCreateRecoveryCleansUpAfterClaimWriteFailure(t *testing.T) {
	api := &fakeFalAPI{}
	b := newFalTestBackend(t, api)
	const leaseID = "cbx_recovery456"
	claim, _ := persistFalCreateRecoveryClaim(t, b, leaseID, "recovery-write-fail", "ambiguous-create", false, time.Time{})
	b.recoveryClaimReplacement = func(current core.LeaseClaim, cfg Config, instanceID, reason string, keep bool) (core.LeaseClaim, error) {
		if strings.TrimSpace(instanceID) != "" {
			return core.LeaseClaim{}, errors.New("disk full")
		}
		return falRecoveryClaimReplacement(current, cfg, instanceID, reason, keep)
	}

	_, err := b.recoverAmbiguousCreateForRelease(context.Background(), api, claim, b.configForRun())
	if err == nil || !strings.Contains(err.Error(), "disk full") || !strings.Contains(err.Error(), "inst_created") {
		t.Fatalf("recovery err=%v", err)
	}
	if len(api.deletedIDs) != 1 || api.deletedIDs[0] != "inst_created" {
		t.Fatalf("known recovered instance was not cleaned up: %#v", api.deletedIDs)
	}
	if _, exists, readErr := core.ReadLeaseClaimWithPresence(leaseID); readErr != nil || exists {
		t.Fatalf("claim exists=%v err=%v", exists, readErr)
	}
}

func TestFalAcquireBindFailureRetainsImmediateCleanupClaim(t *testing.T) {
	api := &fakeFalAPI{deleteErr: errors.New("delete unavailable")}
	b := newFalTestBackend(t, api)
	writes := 0
	b.recoveryClaimReplacement = func(current core.LeaseClaim, cfg Config, instanceID, reason string, keep bool) (core.LeaseClaim, error) {
		if strings.TrimSpace(instanceID) != "" {
			writes++
			if writes <= 4 {
				return core.LeaseClaim{}, errors.New("claim write unavailable")
			}
		}
		return falRecoveryClaimReplacement(current, cfg, instanceID, reason, keep)
	}

	_, err := b.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "bind-failure-cleanup"})
	if err == nil || !strings.Contains(err.Error(), "claim write unavailable") || !strings.Contains(err.Error(), "delete unavailable") {
		t.Fatalf("acquire err=%v", err)
	}
	claim, exists, readErr := core.ReadLeaseClaimWithPresence(api.idempotency[0])
	if readErr != nil || !exists || claim.CloudID != "inst_created" || claim.Labels["recovery"] != "rollback-cleanup" {
		t.Fatalf("claim=%#v exists=%t err=%v", claim, exists, readErr)
	}
}

func TestFalKnownInstanceAdoptionRequiresDurableRewrite(t *testing.T) {
	b := newFalTestBackend(t, &fakeFalAPI{})
	claim, err := b.persistRecoveryClaimAtIfUnchanged(
		"cbx_known_rewrite",
		"known-rewrite",
		b.configForRun(),
		"",
		"inst_created",
		"provisioning",
		false,
		b.now(),
		core.LeaseClaim{},
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	b.recoveryClaimReplacement = func(core.LeaseClaim, Config, string, string, bool) (core.LeaseClaim, error) {
		return core.LeaseClaim{}, errors.New("directory sync unavailable")
	}

	_, exists, err := b.adoptOrBindKnownFalInstance(claim, b.configForRun(), "inst_created", "provisioning", false)
	if err == nil || !exists || !strings.Contains(err.Error(), "directory sync unavailable") {
		t.Fatalf("adopt exists=%v err=%v", exists, err)
	}
}

func TestFalStopRetainsAmbiguousClaimAfterIdempotencyWindow(t *testing.T) {
	api := &fakeFalAPI{createErrs: []error{io.ErrUnexpectedEOF, io.ErrUnexpectedEOF, io.ErrUnexpectedEOF, io.ErrUnexpectedEOF}}
	b := newFalTestBackend(t, api)

	_, err := b.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "expired-recovery"})
	if err == nil {
		t.Fatal("expected ambiguous create failure")
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider("expired-recovery", providerName)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	labels := cloneLabels(claim.Labels)
	labels["create_started_at"] = strconv.FormatInt(time.Now().Add(-falCreateRecoveryWindow-time.Minute).Unix(), 10)
	claim, err = core.UpdateLeaseClaimLabelsIfUnchanged(claim.LeaseID, claim, labels)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Resolve(context.Background(), core.ResolveRequest{ID: claim.LeaseID, ReleaseOnly: true}); err == nil || !strings.Contains(err.Error(), "recovery window expired") {
		t.Fatalf("resolve err=%v", err)
	}
	if len(api.createRequests) != 4 {
		t.Fatalf("expired recovery replayed create: requests=%d", len(api.createRequests))
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID); err != nil || !exists {
		t.Fatalf("claim exists=%v err=%v", exists, err)
	}
}

func TestFalStopRefusesAmbiguousCreateReplayWithDifferentCredential(t *testing.T) {
	api := &fakeFalAPI{createErrs: []error{io.ErrUnexpectedEOF, io.ErrUnexpectedEOF, io.ErrUnexpectedEOF, io.ErrUnexpectedEOF}}
	b := newFalTestBackend(t, api)

	_, err := b.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "credential-bound-recovery"})
	if err == nil {
		t.Fatal("expected ambiguous create failure")
	}
	b.cfg.Fal.APIKey = "different-test-key"
	_, err = b.Resolve(context.Background(), core.ResolveRequest{ID: "credential-bound-recovery", ReleaseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "different credential identity") {
		t.Fatalf("resolve err=%v", err)
	}
	if len(api.createRequests) != 4 {
		t.Fatalf("credential mismatch replayed create: requests=%d", len(api.createRequests))
	}
}

func TestFalAcquireRollsBackOnCallbackFailure(t *testing.T) {
	api := &fakeFalAPI{}
	b := newFalTestBackend(t, api)
	_, err := b.Acquire(context.Background(), core.AcquireRequest{
		RequestedSlug: "rollback",
		OnAcquired: func(core.LeaseTarget) error {
			return errors.New("controller rejected identity")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "controller rejected identity") {
		t.Fatalf("err=%v", err)
	}
	if len(api.deletedIDs) != 1 || api.deletedIDs[0] != "inst_created" {
		t.Fatalf("deletedIDs=%#v", api.deletedIDs)
	}
	if _, ok, claimErr := core.ResolveLeaseClaimForProvider("rollback", providerName); claimErr != nil || ok {
		t.Fatalf("rollback claim ok=%v err=%v", ok, claimErr)
	}
}

func TestFalAcquireKeepFailurePersistsRecoveryClaim(t *testing.T) {
	api := &fakeFalAPI{}
	b := newFalTestBackend(t, api)
	b.waitSSH = func(context.Context, *core.SSHTarget, string, time.Duration) error {
		return errors.New("ssh not ready")
	}
	_, err := b.Acquire(context.Background(), core.AcquireRequest{
		RequestedSlug: "keep-failed",
		Keep:          true,
	})
	if err == nil || !strings.Contains(err.Error(), "ssh not ready") {
		t.Fatalf("err=%v", err)
	}
	if len(api.deletedIDs) != 0 {
		t.Fatalf("keep failure deleted instance: %#v", api.deletedIDs)
	}
	claim, ok, claimErr := core.ResolveLeaseClaimForProvider("keep-failed", providerName)
	if claimErr != nil || !ok {
		t.Fatalf("recovery claim ok=%v err=%v", ok, claimErr)
	}
	if claim.CloudID != "inst_created" || claim.Labels["recovery"] != "keep-failed-acquire" || claim.Labels["keep"] != "true" {
		t.Fatalf("claim=%#v", claim)
	}
}

func TestFalKeepFailureRetainsKnownInstanceWhenClaimAndOperationStateAreUnavailable(t *testing.T) {
	api := &fakeFalAPI{
		instances: map[string]ComputeInstance{"inst_created": readyFalInstance("inst_created", "203.0.113.42")},
	}
	b := newFalTestBackend(t, api)
	stateFile := filepath.Join(t.TempDir(), "state-file")
	if err := os.WriteFile(stateFile, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", stateFile)
	err := b.handleFailedAcquire("inst_created", "cbx_abcdef123456", "keep-failed", b.configForRun(), "", true, errors.New("ssh not ready"))
	if err == nil || !strings.Contains(err.Error(), "ssh not ready") || !strings.Contains(err.Error(), "persist fal keep recovery claim") || !strings.Contains(err.Error(), "create claim lock directory") {
		t.Fatalf("err=%v", err)
	}
	if len(api.deletedIDs) != 0 {
		t.Fatalf("provider mutation issued without durable state: %#v", api.deletedIDs)
	}
	if _, ok := api.instances["inst_created"]; !ok {
		t.Fatal("known instance removed without durable cleanup serialization")
	}
}

func TestFalAcquireOnAcquiredFailureRollsBackEvenWithKeep(t *testing.T) {
	api := &fakeFalAPI{}
	b := newFalTestBackend(t, api)
	_, err := b.Acquire(context.Background(), core.AcquireRequest{
		RequestedSlug: "keep-callback-fail",
		Keep:          true,
		OnAcquired: func(core.LeaseTarget) error {
			return errors.New("controller rejected identity")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "controller rejected identity") {
		t.Fatalf("err=%v", err)
	}
	if len(api.deletedIDs) != 1 || api.deletedIDs[0] != "inst_created" {
		t.Fatalf("callback failure did not roll back: %#v", api.deletedIDs)
	}
	if _, ok, claimErr := core.ResolveLeaseClaimForProvider("keep-callback-fail", providerName); claimErr != nil || ok {
		t.Fatalf("rollback claim ok=%v err=%v", ok, claimErr)
	}
}

func TestFalAcquireAcknowledgesProviderIdentityBeforeSSHWait(t *testing.T) {
	api := &fakeFalAPI{}
	b := newFalTestBackend(t, api)
	b.waitSSH = func(context.Context, *core.SSHTarget, string, time.Duration) error {
		return errors.New("ssh not ready")
	}
	var observed core.LeaseTarget
	_, err := b.Acquire(context.Background(), core.AcquireRequest{
		RequestedSlug: "ack-first",
		Keep:          true,
		OnAcquired: func(target core.LeaseTarget) error {
			observed = target
			claim, ok, claimErr := core.ReadLeaseClaimWithPresence(target.LeaseID)
			if claimErr != nil || !ok || claim.CloudID != target.Server.CloudID || claim.Labels["recovery"] != "provisioning" {
				t.Fatalf("durable pre-readiness claim=%#v ok=%v err=%v", claim, ok, claimErr)
			}
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "ssh not ready") {
		t.Fatalf("err=%v", err)
	}
	if observed.Server.CloudID != "inst_created" || observed.SSH.Host != "203.0.113.42" || observed.LeaseID == "" {
		t.Fatalf("OnAcquired did not receive provider identity before SSH wait: %#v", observed)
	}
	claim, ok, claimErr := core.ResolveLeaseClaimForProvider("ack-first", providerName)
	if claimErr != nil || !ok || claim.CloudID != "inst_created" {
		t.Fatalf("recovery claim=%#v ok=%v err=%v", claim, ok, claimErr)
	}
}

func TestFalAcquireAcknowledgesProviderIdentityBeforeReadinessPolling(t *testing.T) {
	api := &fakeFalAPI{getErr: errors.New("readiness API unavailable")}
	b := newFalTestBackend(t, api)
	var observed core.LeaseTarget
	_, err := b.Acquire(context.Background(), core.AcquireRequest{
		RequestedSlug: "ack-before-readiness",
		Keep:          true,
		OnAcquired: func(target core.LeaseTarget) error {
			observed = target
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "readiness API unavailable") {
		t.Fatalf("err=%v", err)
	}
	if observed.Server.CloudID != "inst_created" || observed.LeaseID == "" || observed.Server.Labels["slug"] != "ack-before-readiness" {
		t.Fatalf("OnAcquired did not receive provider identity before readiness polling: %#v", observed)
	}
	claim, ok, claimErr := core.ResolveLeaseClaimForProvider("ack-before-readiness", providerName)
	if claimErr != nil || !ok || claim.CloudID != "inst_created" {
		t.Fatalf("recovery claim=%#v ok=%v err=%v", claim, ok, claimErr)
	}
}

func TestFalProvisioningClaimIsCleanupOnly(t *testing.T) {
	api := &fakeFalAPI{instances: map[string]ComputeInstance{
		"inst_provisioning": readyFalInstance("inst_provisioning", "203.0.113.42"),
	}}
	b := newFalTestBackend(t, api)
	claim, err := b.persistRecoveryClaimAtIfUnchanged(
		"cbx_abcdef123456",
		"provisioning",
		b.configForRun(),
		"",
		"inst_provisioning",
		"provisioning",
		false,
		b.now(),
		core.LeaseClaim{},
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Resolve(context.Background(), core.ResolveRequest{ID: claim.LeaseID}); err == nil || !strings.Contains(err.Error(), "still provisioning") {
		t.Fatalf("resolve err=%v", err)
	}
	if _, err := b.Touch(context.Background(), core.TouchRequest{Lease: core.LeaseTarget{LeaseID: claim.LeaseID}}); err == nil || !strings.Contains(err.Error(), "still provisioning") {
		t.Fatalf("touch err=%v", err)
	}
	target, err := b.Resolve(context.Background(), core.ResolveRequest{ID: claim.LeaseID, ReleaseOnly: true})
	if err != nil || target.Server.CloudID != "inst_provisioning" {
		t.Fatalf("release-only target=%#v err=%v", target, err)
	}
}

func TestFalAmbiguousCreateReportsRecoveryClaimFailure(t *testing.T) {
	b := newFalTestBackend(t, &fakeFalAPI{})
	stateFile := filepath.Join(t.TempDir(), "state-file")
	if err := os.WriteFile(stateFile, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", stateFile)
	err := b.persistRecoveryClaim("cbx_abcdef123456", "ambiguous", b.configForRun(), "", "", "ambiguous-create", false)
	if err == nil {
		t.Fatal("expected recovery claim persistence error")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("err=%v", err)
	}
}

func TestFalRollbackReportsRecoveryClaimFailureWhenCleanupFails(t *testing.T) {
	api := &fakeFalAPI{
		instances: map[string]ComputeInstance{"inst_created": readyFalInstance("inst_created", "203.0.113.42")},
		deleteErr: errors.New("delete unavailable"),
	}
	b := newFalTestBackend(t, api)
	stateFile := filepath.Join(t.TempDir(), "state-file")
	if err := os.WriteFile(stateFile, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", stateFile)
	err := b.rollbackAcquire("inst_created", "cbx_abcdef123456", "rollback", b.configForRun(), "", "rollback-cleanup", errors.New("bootstrap failed"))
	if err == nil {
		t.Fatal("expected rollback error")
	}
	message := err.Error()
	if !strings.Contains(message, "bootstrap failed") || !strings.Contains(message, "create fal claim namespace") {
		t.Fatalf("err=%v", err)
	}
}

func TestFalCleanupImmediatelyRetriesRollbackClaim(t *testing.T) {
	api := &fakeFalAPI{
		instances: map[string]ComputeInstance{"inst_created": readyFalInstance("inst_created", "203.0.113.42")},
		deleteErr: errors.New("delete unavailable"),
	}
	b := newFalTestBackend(t, api)
	err := b.rollbackAcquire("inst_created", "cbx_abcdef123456", "rollback", b.configForRun(), "", "rollback-cleanup", errors.New("bootstrap failed"))
	if err == nil || !strings.Contains(err.Error(), "delete unavailable") {
		t.Fatalf("rollback err=%v", err)
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider("rollback", providerName)
	if err != nil || !ok || claim.Labels["recovery"] != "rollback-cleanup" {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
	}

	api.deleteErr = nil
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(api.deletedIDs) != 2 || api.deletedIDs[1] != "inst_created" {
		t.Fatalf("deletedIDs=%#v", api.deletedIDs)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider("rollback", providerName); err != nil || ok {
		t.Fatalf("rollback claim retained ok=%v err=%v", ok, err)
	}
}

func TestFalRollbackFinalizesRecreatedClaimAfterProviderConfirmedAbsence(t *testing.T) {
	api := &fakeFalAPI{}
	b := newFalTestBackend(t, api)
	claim, err := b.persistRecoveryClaimAtIfUnchanged(
		"cbx_cleanup1234",
		"cleanup-race",
		b.configForRun(),
		"",
		"inst_created",
		"provisioning",
		false,
		b.now(),
		core.LeaseClaim{},
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := core.EnsureTestboxKey(claim.LeaseID); err != nil {
		t.Fatal(err)
	}
	seededKeyPath, err := core.TestboxKeyPath(claim.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(seededKeyPath); err != nil {
		t.Fatalf("seed rollback key: %v", err)
	}
	if err := core.RemoveLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
		t.Fatal(err)
	}

	cause := errors.New("ready transition lost race")
	rollbackErr := b.rollbackClaimedAcquire("inst_created", claim.LeaseID, claim.Slug, b.configForRun(), "", "rollback-cleanup", cause)
	if !errors.Is(rollbackErr, cause) || strings.Contains(rollbackErr.Error(), errFalProviderAbsenceNotAccountBound.Error()) {
		t.Fatalf("rollback err=%v", rollbackErr)
	}
	if len(api.deletedIDs) != 0 {
		t.Fatalf("rollback issued a second delete after concurrent cleanup: %#v", api.deletedIDs)
	}
	_, exists, readErr := core.ReadLeaseClaimWithPresence(claim.LeaseID)
	if readErr != nil || exists {
		t.Fatalf("claim exists=%v err=%v", exists, readErr)
	}
	keyPath, keyErr := core.TestboxKeyPath(claim.LeaseID)
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("key residue after provider-confirmed absence: %v", err)
	}
}

func TestFalRollbackReconcilesClaimVisibleAfterDurabilityError(t *testing.T) {
	api := &fakeFalAPI{instances: map[string]ComputeInstance{
		"inst_created": readyFalInstance("inst_created", "203.0.113.42"),
	}}
	b := newFalTestBackend(t, api)
	b.persistRollbackClaim = func(leaseID, slug string, cfg Config, repoRoot, instanceID, reason string, keep bool) (core.LeaseClaim, error) {
		claim, err := b.persistRecoveryClaimAtIfUnchanged(
			leaseID, slug, cfg, repoRoot, instanceID, reason, keep, time.Time{}, core.LeaseClaim{}, false,
		)
		if err != nil {
			return core.LeaseClaim{}, err
		}
		return claim, errors.New("ancestor sync unavailable after rename")
	}
	cause := errors.New("ready transition lost claim")
	err := b.rollbackAcquireAfterClaimRemoval("inst_created", "cbx_partial_durable", "partial-durable", b.configForRun(), "", "rollback-cleanup", cause)
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "ancestor sync unavailable after rename") {
		t.Fatalf("rollback err=%v", err)
	}
	if len(api.deletedIDs) != 1 || api.deletedIDs[0] != "inst_created" {
		t.Fatalf("deletedIDs=%#v", api.deletedIDs)
	}
	if _, exists, readErr := core.ReadLeaseClaimWithPresence("cbx_partial_durable"); readErr != nil || exists {
		t.Fatalf("claim exists=%t err=%v", exists, readErr)
	}
}

func TestFalRollbackRetainsInstanceWhenNoDurableClaimCanBeWritten(t *testing.T) {
	api := &fakeFalAPI{instances: map[string]ComputeInstance{
		"inst_created": readyFalInstance("inst_created", "203.0.113.42"),
	}}
	b := newFalTestBackend(t, api)
	b.persistRollbackClaim = func(string, string, Config, string, string, string, bool) (core.LeaseClaim, error) {
		return core.LeaseClaim{}, errors.New("claim write unavailable")
	}
	err := b.rollbackAcquireAfterClaimRemoval("inst_created", "cbx_no_durable_claim", "no-durable-claim", b.configForRun(), "", "rollback-cleanup", errors.New("ready transition lost claim"))
	if err == nil || !strings.Contains(err.Error(), "claim write unavailable") || !strings.Contains(err.Error(), "reconcile instance inst_created manually") {
		t.Fatalf("rollback err=%v", err)
	}
	if len(api.deletedIDs) != 0 {
		t.Fatalf("provider mutation occurred without durable ownership: %#v", api.deletedIDs)
	}
	if _, ok := api.instances["inst_created"]; !ok {
		t.Fatal("instance removed without durable cleanup ownership")
	}
}

func TestFalRollbackRefusesEmergencyDeleteAfterClaimConflict(t *testing.T) {
	api := &fakeFalAPI{instances: map[string]ComputeInstance{
		"inst_created": readyFalInstance("inst_created", "203.0.113.42"),
	}}
	b := newFalTestBackend(t, api)
	b.persistRollbackClaim = func(leaseID, slug string, cfg Config, repoRoot, _, reason string, keep bool) (core.LeaseClaim, error) {
		if _, err := b.persistRecoveryClaimAtIfUnchanged(
			leaseID, slug, cfg, repoRoot, "inst_other", reason, keep, time.Time{}, core.LeaseClaim{}, false,
		); err != nil {
			return core.LeaseClaim{}, err
		}
		return core.LeaseClaim{}, errors.New("claim changed")
	}
	err := b.rollbackAcquireAfterClaimRemoval("inst_created", "cbx_claim_conflict", "claim-conflict", b.configForRun(), "", "rollback-cleanup", errors.New("ready transition lost claim"))
	if err == nil || !strings.Contains(err.Error(), "concurrent recovery claim") {
		t.Fatalf("rollback err=%v", err)
	}
	if len(api.deletedIDs) != 0 {
		t.Fatalf("emergency delete crossed claim conflict: %#v", api.deletedIDs)
	}
	claim, exists, readErr := core.ReadLeaseClaimWithPresence("cbx_claim_conflict")
	if readErr != nil || !exists || claim.CloudID != "inst_other" {
		t.Fatalf("claim=%#v exists=%t err=%v", claim, exists, readErr)
	}
}

func TestFalRollbackReallocatesSlugBeforeRepublishingClaim(t *testing.T) {
	api := &fakeFalAPI{
		instances: map[string]ComputeInstance{"inst_created": readyFalInstance("inst_created", "203.0.113.42")},
		deleteErr: errors.New("delete unavailable"),
	}
	b := newFalTestBackend(t, api)
	if _, err := b.persistRecoveryClaimAtIfUnchanged(
		"cbx_slug_new_owner", "reused-slug", b.configForRun(), "", "inst_other", "ready", false, b.now(), core.LeaseClaim{}, false,
	); err != nil {
		t.Fatal(err)
	}
	err := b.rollbackAcquireAfterClaimRemoval("inst_created", "cbx_slug_old_owner", "reused-slug", b.configForRun(), "", "rollback-cleanup", errors.New("ready transition lost claim"))
	if err == nil || !strings.Contains(err.Error(), "delete unavailable") {
		t.Fatalf("rollback err=%v", err)
	}
	claim, exists, readErr := core.ReadLeaseClaimWithPresence("cbx_slug_old_owner")
	if readErr != nil || !exists || claim.Slug == "reused-slug" || !strings.HasPrefix(claim.Slug, "reused-slug-") {
		t.Fatalf("claim=%#v exists=%t err=%v", claim, exists, readErr)
	}
}

func TestFalRollbackDeletesLiveInstanceAfterClaimOnlyDisappearance(t *testing.T) {
	api := &fakeFalAPI{instances: map[string]ComputeInstance{
		"inst_created": readyFalInstance("inst_created", "203.0.113.42"),
	}}
	b := newFalTestBackend(t, api)
	claim, err := b.persistRecoveryClaimAtIfUnchanged(
		"cbx_cleanup5678",
		"claim-only-loss",
		b.configForRun(),
		"",
		"inst_created",
		"provisioning",
		false,
		b.now(),
		core.LeaseClaim{},
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.RemoveLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
		t.Fatal(err)
	}

	cause := errors.New("ready transition lost claim")
	if err := b.rollbackClaimedAcquire("inst_created", claim.LeaseID, claim.Slug, b.configForRun(), "", "rollback-cleanup", cause); !errors.Is(err, cause) {
		t.Fatalf("rollback err=%v", err)
	}
	if len(api.deletedIDs) != 1 || api.deletedIDs[0] != "inst_created" {
		t.Fatalf("orphaned live instance after claim loss: %#v", api.deletedIDs)
	}
	if _, exists := api.instances["inst_created"]; exists {
		t.Fatal("instance remained live after claim-only disappearance")
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID); err != nil || exists {
		t.Fatalf("claim exists=%v err=%v", exists, err)
	}
}

func TestFalRollbackReclaimsOwnershipWhenClaimLossCleanupFails(t *testing.T) {
	api := &fakeFalAPI{
		instances: map[string]ComputeInstance{
			"inst_created": readyFalInstance("inst_created", "203.0.113.42"),
		},
		deleteErr: errors.New("delete unavailable"),
	}
	b := newFalTestBackend(t, api)
	claim, err := b.persistRecoveryClaimAtIfUnchanged(
		"cbx_cleanup9012",
		"claim-loss-recovery",
		b.configForRun(),
		"",
		"inst_created",
		"provisioning",
		false,
		b.now(),
		core.LeaseClaim{},
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.RemoveLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
		t.Fatal(err)
	}

	err = b.rollbackClaimedAcquire("inst_created", claim.LeaseID, claim.Slug, b.configForRun(), "", "rollback-cleanup", errors.New("ready transition lost claim"))
	if err == nil || !strings.Contains(err.Error(), "delete unavailable") {
		t.Fatalf("rollback err=%v", err)
	}
	recovered, exists, readErr := core.ReadLeaseClaimWithPresence(claim.LeaseID)
	if readErr != nil || !exists || recovered.CloudID != "inst_created" || recovered.Labels["recovery"] != "rollback-cleanup" {
		t.Fatalf("recovered claim=%#v exists=%v err=%v", recovered, exists, readErr)
	}
}

func TestFalRollbackRetainsOwnershipWhenDeleteNotFoundDoesNotConfirmAbsence(t *testing.T) {
	notFound := &APIError{StatusCode: 404, Status: "404 Not Found", Message: "not found"}
	api := &fakeFalAPI{
		instances: map[string]ComputeInstance{
			"inst_created": readyFalInstance("inst_created", "203.0.113.42"),
		},
		deleteErr: notFound,
	}
	b := newFalTestBackend(t, api)
	b.pollInterval = time.Millisecond
	b.pollTimeout = 20 * time.Millisecond
	claim, err := b.persistRecoveryClaimAtIfUnchanged(
		"cbx_cleanup7890",
		"live-delete-not-found",
		b.configForRun(),
		"",
		"inst_created",
		"provisioning",
		false,
		b.now(),
		core.LeaseClaim{},
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.RemoveLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
		t.Fatal(err)
	}

	err = b.rollbackClaimedAcquire("inst_created", claim.LeaseID, claim.Slug, b.configForRun(), "", "rollback-cleanup", errors.New("ready transition lost claim"))
	if err == nil || !strings.Contains(err.Error(), "confirm fal instance inst_created deletion") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("rollback err=%v", err)
	}
	recovered, exists, readErr := core.ReadLeaseClaimWithPresence(claim.LeaseID)
	if readErr != nil || !exists || recovered.CloudID != "inst_created" {
		t.Fatalf("recovered claim=%#v exists=%v err=%v", recovered, exists, readErr)
	}
}

func TestFalRollbackRetainsClaimWhenNotFoundConflictsWithAccountInventory(t *testing.T) {
	notFound := &APIError{StatusCode: 404, Status: "404 Not Found", Message: "not found"}
	api := &fakeFalAPI{
		instances: map[string]ComputeInstance{
			"inst_created": readyFalInstance("inst_created", "203.0.113.42"),
		},
		getErr:    notFound,
		deleteErr: notFound,
	}
	b := newFalTestBackend(t, api)
	claim, err := b.persistRecoveryClaimAtIfUnchanged(
		"cbx_cleanup3456",
		"masked-absence",
		b.configForRun(),
		"",
		"inst_created",
		"provisioning",
		false,
		b.now(),
		core.LeaseClaim{},
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.RemoveLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
		t.Fatal(err)
	}

	err = b.rollbackClaimedAcquire("inst_created", claim.LeaseID, claim.Slug, b.configForRun(), "", "rollback-cleanup", errors.New("ready transition lost claim"))
	if err == nil || !strings.Contains(err.Error(), errFalProviderAbsenceNotAccountBound.Error()) {
		t.Fatalf("rollback err=%v", err)
	}
	recovered, exists, readErr := core.ReadLeaseClaimWithPresence(claim.LeaseID)
	if readErr != nil || !exists || recovered.CloudID != "inst_created" {
		t.Fatalf("recovered claim=%#v exists=%v err=%v", recovered, exists, readErr)
	}
	if _, exists := api.instances["inst_created"]; !exists {
		t.Fatal("masked-absence rollback deleted provider state without confirmation")
	}
}

func TestFalRollbackRetainsClaimWhenDeleteAbsenceIsUnverified(t *testing.T) {
	api := &fakeFalAPI{
		instances: map[string]ComputeInstance{"inst_created": readyFalInstance("inst_created", "203.0.113.42")},
		deleteErr: &APIError{StatusCode: 404, Status: "404 Not Found", Message: "not found"},
	}
	b := newFalTestBackend(t, api)
	err := b.rollbackAcquire("inst_created", "cbx_abcdef123456", "rollback", b.configForRun(), "", "rollback-cleanup", errors.New("bootstrap failed"))
	if err == nil || !strings.Contains(err.Error(), "fal cleanup failed") {
		t.Fatalf("rollback err=%v", err)
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider("rollback", providerName)
	if err != nil || !ok || claim.CloudID != "inst_created" || claim.Labels["recovery"] != "rollback-cleanup" {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
	}
}

func TestFalResolveListAndReleaseRequireLocalClaim(t *testing.T) {
	api := &fakeFalAPI{instances: map[string]ComputeInstance{
		"inst_owned":   readyFalInstance("inst_owned", "203.0.113.10"),
		"inst_foreign": readyFalInstance("inst_foreign", "203.0.113.11"),
	}}
	b := newFalTestBackend(t, api)
	claimFalLease(t, b.cfg, "cbx_abcdef123456", "owned", "inst_owned", "203.0.113.10", false)
	claim, exists, err := core.ReadLeaseClaimWithPresence("cbx_abcdef123456")
	if err != nil || !exists {
		t.Fatalf("ready claim exists=%t err=%v", exists, err)
	}
	if _, err := markFalAcquireLifetimeClaimForTest(claim); err != nil {
		t.Fatal(err)
	}

	lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "owned", Repo: core.Repo{Root: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != "cbx_abcdef123456" || lease.Server.CloudID != "inst_owned" || lease.SSH.Host != "203.0.113.10" {
		t.Fatalf("lease=%#v", lease)
	}
	claim, exists, err = core.ReadLeaseClaimWithPresence(lease.LeaseID)
	if err != nil || !exists || claim.Labels[falAcquireLifetimeLabel] != falAcquireLifetimeVersion {
		t.Fatalf("resolved claim lifetime marker=%q exists=%t err=%v", claim.Labels[falAcquireLifetimeLabel], exists, err)
	}
	if lease.Server.Labels[falAcquireLifetimeLabel] != "" {
		t.Fatalf("internal lifetime marker leaked after resolve: %#v", lease.Server.Labels)
	}
	lease, err = b.Resolve(context.Background(), core.ResolveRequest{ID: "inst_owned"})
	if err != nil || lease.LeaseID != "cbx_abcdef123456" {
		t.Fatalf("resolve by cloud id lease=%#v err=%v", lease, err)
	}
	if _, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "inst_foreign"}); err == nil || !strings.Contains(err.Error(), "not locally claimed") {
		t.Fatalf("foreign resolve err=%v", err)
	}
	views, err := b.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].CloudID != "inst_owned" {
		t.Fatalf("views=%#v", views)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if len(api.deletedIDs) != 1 || api.deletedIDs[0] != "inst_owned" {
		t.Fatalf("deletedIDs=%#v", api.deletedIDs)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider("owned", providerName); err != nil || ok {
		t.Fatalf("claim should be removed ok=%v err=%v", ok, err)
	}
}

func TestFalLifecycleRejectsClaimsFromAnotherAPIEndpoint(t *testing.T) {
	api := &fakeFalAPI{instances: map[string]ComputeInstance{
		"inst_owned": readyFalInstance("inst_owned", "203.0.113.10"),
	}}
	b := newFalTestBackend(t, api)
	claimFalLease(t, b.cfg, "cbx_abcdef123456", "owned", "inst_owned", "203.0.113.10", true)

	other := *b
	other.cfg.Fal.APIURL = "https://other.example.test/v1"
	if _, err := other.Resolve(context.Background(), core.ResolveRequest{ID: "owned"}); err == nil || !strings.Contains(err.Error(), "not locally claimed") {
		t.Fatalf("cross-endpoint resolve err=%v", err)
	}
	views, err := other.List(context.Background(), core.ListRequest{})
	if err != nil || len(views) != 0 {
		t.Fatalf("cross-endpoint views=%#v err=%v", views, err)
	}
	err = other.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{
		LeaseID: "cbx_abcdef123456",
		Server:  core.Server{CloudID: "inst_owned", Provider: providerName, Labels: map[string]string{"lease": "cbx_abcdef123456"}},
	}})
	if err == nil || !strings.Contains(err.Error(), "different API endpoint") {
		t.Fatalf("cross-endpoint release err=%v", err)
	}
	if err := other.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(api.deletedIDs) != 0 {
		t.Fatalf("cross-endpoint lifecycle deleted instances: %#v", api.deletedIDs)
	}
}

func TestFalResolveUsesPersistedSSHUserAndPort(t *testing.T) {
	api := &fakeFalAPI{instances: map[string]ComputeInstance{
		"inst_owned": readyFalInstance("inst_owned", "203.0.113.10"),
	}}
	b := newFalTestBackend(t, api)
	claimCfg := b.cfg
	claimCfg.Fal.User = "ubuntu"
	claimCfg.SSHUser = "ubuntu"
	claimCfg.SSHPort = "2222"
	claimFalLease(t, claimCfg, "cbx_abcdef123456", "owned", "inst_owned", "203.0.113.10", false)

	b.cfg.Fal.User = defaultUser
	b.cfg.SSHUser = defaultUser
	b.cfg.SSHPort = "22"
	lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "owned"})
	if err != nil {
		t.Fatal(err)
	}
	if lease.SSH.User != "ubuntu" || lease.SSH.Port != "2222" {
		t.Fatalf("ssh target=%#v, want persisted ubuntu:2222", lease.SSH)
	}
}

func TestFalStatusOnlyResolveDoesNotRequireSSHHost(t *testing.T) {
	api := &fakeFalAPI{instances: map[string]ComputeInstance{
		"inst_pending": {
			ID:           "inst_pending",
			InstanceType: InstanceTypeH100x1,
			Sector:       Sector1,
			Region:       "us-west",
			Status:       InstanceStatusProvisioning,
		},
	}}
	b := newFalTestBackend(t, api)
	claimFalLease(t, b.cfg, "cbx_pending123", "pending", "inst_pending", "", false)
	lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "pending", StatusOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.Status != string(InstanceStatusProvisioning) || lease.SSH.Host != "" {
		t.Fatalf("lease=%#v", lease)
	}
	if lease.Server.Labels["state"] != string(InstanceStatusProvisioning) {
		t.Fatalf("state label=%q, want live provisioning state", lease.Server.Labels["state"])
	}
	if _, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "pending"}); err == nil || !strings.Contains(err.Error(), "no SSH host") {
		t.Fatalf("non-status resolve err=%v", err)
	}
}

func TestFalStatusReadyProbeIncludesSSHWhenHostIsAvailable(t *testing.T) {
	api := &fakeFalAPI{instances: map[string]ComputeInstance{
		"inst_ready": readyFalInstance("inst_ready", "203.0.113.50"),
	}}
	b := newFalTestBackend(t, api)
	claimFalLease(t, b.cfg, "cbx_ready12345", "ready", "inst_ready", "203.0.113.50", false)
	lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "ready", StatusOnly: true, ReadyProbe: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.SSH.Host != "203.0.113.50" {
		t.Fatalf("ready probe ssh target=%#v", lease.SSH)
	}
}

func TestFalReleaseRetainsClaimOnAmbiguousProviderRead(t *testing.T) {
	api := &fakeFalAPI{
		instances: map[string]ComputeInstance{"inst_owned": readyFalInstance("inst_owned", "203.0.113.10")},
		getErr:    errors.New("temporary inventory failure"),
	}
	b := newFalTestBackend(t, api)
	claimFalLease(t, b.cfg, "cbx_abcdef123456", "owned", "inst_owned", "203.0.113.10", false)

	err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{
		LeaseID: "cbx_abcdef123456",
		Server:  core.Server{CloudID: "inst_owned", Provider: providerName, Labels: map[string]string{"lease": "cbx_abcdef123456"}},
	}})
	if err == nil || !strings.Contains(err.Error(), "temporary inventory failure") {
		t.Fatalf("err=%v", err)
	}
	if len(api.deletedIDs) != 0 {
		t.Fatalf("delete issued despite ambiguous read: %#v", api.deletedIDs)
	}
	if claim, ok, err := core.ResolveLeaseClaimForProvider("owned", providerName); err != nil || !ok || claim.CloudID != "inst_owned" {
		t.Fatalf("claim not retained: claim=%#v ok=%v err=%v", claim, ok, err)
	}
}

func TestFalReleaseRetainsClaimUntilDeletionIsConfirmed(t *testing.T) {
	api := &fakeFalAPI{
		instances:             map[string]ComputeInstance{"inst_owned": readyFalInstance("inst_owned", "203.0.113.10")},
		retainDeletedInstance: true,
	}
	b := newFalTestBackend(t, api)
	b.pollInterval = time.Millisecond
	b.pollTimeout = 20 * time.Millisecond
	claimFalLease(t, b.cfg, "cbx_delete_pending", "delete-pending", "inst_owned", "203.0.113.10", false)
	original, originalExists, originalErr := core.ReadLeaseClaimWithPresence("cbx_delete_pending")
	if originalErr != nil || !originalExists {
		t.Fatalf("original claim exists=%t err=%v", originalExists, originalErr)
	}

	release := core.ReleaseLeaseRequest{Lease: core.LeaseTarget{
		LeaseID: "cbx_delete_pending",
		Server:  core.Server{CloudID: "inst_owned", Provider: providerName, Labels: map[string]string{"lease": "cbx_delete_pending"}},
	}}
	err := b.ReleaseLease(context.Background(), release)
	if err == nil || !strings.Contains(err.Error(), "confirm fal instance inst_owned deletion") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	if len(api.deletedIDs) != 1 || api.deletedIDs[0] != "inst_owned" {
		t.Fatalf("deletedIDs=%#v", api.deletedIDs)
	}
	claim, ok, readErr := core.ReadLeaseClaimWithPresence("cbx_delete_pending")
	if readErr != nil || !ok || claim.CloudID != "inst_owned" || !falDeleteStateMatches(claim, falDeleteAcceptedLabel, "inst_owned") || falDeleteConfirmed(claim, "inst_owned") {
		t.Fatalf("claim=%#v exists=%t err=%v", claim, ok, readErr)
	}
	stale := original
	stale.Labels = cloneLabels(original.Labels)
	stale.Labels["state"] = "ready"
	if err := core.ReplaceLeaseClaimIfUnchangedDurable(original.LeaseID, original, stale); err == nil {
		t.Fatal("stale pre-delete writer replaced the accepted deletion marker")
	}
	if _, err := b.Touch(context.Background(), core.TouchRequest{Lease: core.LeaseTarget{LeaseID: claim.LeaseID}}); err == nil || !strings.Contains(err.Error(), "deletion is in progress") {
		t.Fatalf("touch err=%v", err)
	}
	if _, err := b.Resolve(context.Background(), core.ResolveRequest{ID: claim.LeaseID}); err == nil || !strings.Contains(err.Error(), "deletion is in progress") {
		t.Fatalf("resolve err=%v", err)
	}
	if _, err := b.Resolve(context.Background(), core.ResolveRequest{ID: claim.LeaseID, ReleaseOnly: true}); err != nil {
		t.Fatalf("release-only resolve err=%v", err)
	}
	if err := b.ReleaseLease(context.Background(), release); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("accepted pending release err=%v", err)
	}
	if len(api.deletedIDs) != 1 {
		t.Fatalf("accepted deletion was reissued while still visible: %#v", api.deletedIDs)
	}
	delete(api.instances, "inst_owned")
	if err := b.ReleaseLease(context.Background(), release); err != nil {
		t.Fatal(err)
	}
	if len(api.deletedIDs) != 1 {
		t.Fatalf("accepted deletion was reissued: %#v", api.deletedIDs)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID); err != nil || exists {
		t.Fatalf("claim exists=%t err=%v", exists, err)
	}
}

func TestFalReleaseFinalizesConfirmedDeletionAfterCredentialRotation(t *testing.T) {
	api := &fakeFalAPI{getErr: errors.New("provider access must not be required")}
	b := newFalTestBackend(t, api)
	const leaseID = "cbx_delete_confirmed"
	claimFalLease(t, b.cfg, leaseID, "delete-confirmed", "inst_owned", "203.0.113.10", false)
	keyPath, _, err := core.EnsureTestboxKeyForConfig(b.cfg, leaseID)
	if err != nil {
		t.Fatal(err)
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil || !exists {
		t.Fatalf("claim exists=%t err=%v", exists, err)
	}
	confirmed, err := persistFalDeleteState(claim, falDeleteConfirmedLabel, "inst_owned")
	if err != nil {
		t.Fatal(err)
	}
	unbound := confirmed
	unbound.CloudID = ""
	if err := core.ReplaceLeaseClaimIfUnchangedDurable(leaseID, confirmed, unbound); err != nil {
		t.Fatal(err)
	}
	b.cfg.Fal.APIKey = "rotated-test-key"

	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{
		LeaseID: leaseID,
		Server:  core.Server{CloudID: "inst_owned", Provider: providerName, Labels: map[string]string{"lease": leaseID}},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(leaseID); err != nil || exists {
		t.Fatalf("claim exists=%t err=%v", exists, err)
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("confirmed deletion key residue: %v", err)
	}
	if len(api.deletedIDs) != 0 {
		t.Fatalf("confirmed deletion contacted delete API: %#v", api.deletedIDs)
	}
}

func TestFalReleaseRetainsUnconfirmedDeletionAfterCredentialRotation(t *testing.T) {
	for _, state := range []struct {
		name  string
		label string
	}{
		{name: "attempted", label: falDeleteAttemptLabel},
		{name: "accepted", label: falDeleteAcceptedLabel},
	} {
		t.Run(state.name, func(t *testing.T) {
			for _, visible := range []bool{true, false} {
				name := "absent"
				instances := map[string]ComputeInstance{}
				if visible {
					name = "visible"
					instances["inst_owned"] = readyFalInstance("inst_owned", "203.0.113.10")
				}
				t.Run(name, func(t *testing.T) {
					api := &fakeFalAPI{instances: instances}
					b := newFalTestBackend(t, api)
					const leaseID = "cbx_delete_accepted"
					claimFalLease(t, b.cfg, leaseID, "delete-accepted", "inst_owned", "203.0.113.10", false)
					keyPath, _, err := core.EnsureTestboxKeyForConfig(b.cfg, leaseID)
					if err != nil {
						t.Fatal(err)
					}
					claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
					if err != nil || !exists {
						t.Fatalf("claim exists=%t err=%v", exists, err)
					}
					accepted, err := persistFalDeleteState(claim, state.label, "inst_owned")
					if err != nil {
						t.Fatal(err)
					}
					originalBinding := accepted.Labels[falCredentialBindingLabel]
					b.cfg.Fal.APIKey = "rotated-test-key"

					releaseErr := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{
						LeaseID: leaseID,
						Server:  core.Server{CloudID: "inst_owned", Provider: providerName, Labels: map[string]string{"lease": leaseID}},
					}})
					if releaseErr == nil || !strings.Contains(releaseErr.Error(), "credential rotation") {
						t.Fatalf("release err=%v", releaseErr)
					}
					retained, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
					if err != nil || !exists || !falDeleteStateMatches(retained, state.label, "inst_owned") || falDeleteConfirmed(retained, "inst_owned") {
						t.Fatalf("claim exists=%t err=%v", exists, err)
					}
					if got := retained.Labels[falCredentialBindingLabel]; got != originalBinding {
						t.Fatalf("accepted deletion binding changed from %q to %q", originalBinding, got)
					}
					if _, err := os.Stat(keyPath); err != nil {
						t.Fatalf("accepted deletion key missing: %v", err)
					}
					if len(api.deletedIDs) != 0 {
						t.Fatalf("accepted absent deletion was reissued: %#v", api.deletedIDs)
					}
				})
			}
		})
	}
}

func TestFalReleaseSerializesReadyClaimWithAcquireLifetime(t *testing.T) {
	api := &fakeFalAPI{instances: map[string]ComputeInstance{
		"inst_owned": readyFalInstance("inst_owned", "203.0.113.10"),
	}}
	b := newFalTestBackend(t, api)
	const leaseID = "cbx_ready_acquiring"
	claimFalLease(t, b.cfg, leaseID, "ready-acquiring", "inst_owned", "203.0.113.10", false)
	claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil || !exists {
		t.Fatalf("claim exists=%t err=%v", exists, err)
	}
	marked := claim
	marked.Labels = cloneLabels(claim.Labels)
	marked.Labels[falAcquireLifetimeLabel] = falAcquireLifetimeVersion
	if err := core.ReplaceLeaseClaimIfUnchangedDurable(leaseID, claim, marked); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockFalAcquireLifetime(context.Background(), leaseID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	if _, err := b.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, ReleaseOnly: true}); err == nil || !strings.Contains(err.Error(), "acquisition is still in progress") {
		t.Fatalf("resolve err=%v", err)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{
		LeaseID: leaseID,
		Server:  core.Server{CloudID: "inst_owned", Provider: providerName, Labels: map[string]string{"lease": leaseID}},
	}}); err == nil || !strings.Contains(err.Error(), "acquisition is still in progress") {
		t.Fatalf("release err=%v", err)
	}
	if len(api.deletedIDs) != 0 {
		t.Fatalf("active acquire instance deleted: %#v", api.deletedIDs)
	}
	if err := core.VerifyLeaseClaimUnchanged(leaseID, marked); err != nil {
		t.Fatalf("active acquire claim changed: %v", err)
	}
}

func TestFalCleanupResumesAcceptedDeletionImmediately(t *testing.T) {
	b := newFalTestBackend(t, &fakeFalAPI{})
	claimFalLease(t, b.cfg, "cbx_cleanup_accepted", "cleanup-accepted", "inst_owned", "203.0.113.10", false)
	claim, exists, err := core.ReadLeaseClaimWithPresence("cbx_cleanup_accepted")
	if err != nil || !exists {
		t.Fatalf("claim exists=%t err=%v", exists, err)
	}
	claim, err = persistFalDeleteState(claim, falDeleteAcceptedLabel, "inst_owned")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID); err != nil || exists {
		t.Fatalf("claim exists=%t err=%v", exists, err)
	}
}

func TestFalReleaseRetainsAcceptedClaimWhenKeyRemovalFails(t *testing.T) {
	api := &fakeFalAPI{instances: map[string]ComputeInstance{
		"inst_owned": readyFalInstance("inst_owned", "203.0.113.10"),
	}}
	b := newFalTestBackend(t, api)
	b.removeLeaseKey = func(string) error { return errors.New("key removal unavailable") }
	claimFalLease(t, b.cfg, "cbx_delete_key_fail", "delete-key-fail", "inst_owned", "203.0.113.10", false)
	release := core.ReleaseLeaseRequest{Lease: core.LeaseTarget{
		LeaseID: "cbx_delete_key_fail",
		Server:  core.Server{CloudID: "inst_owned", Provider: providerName, Labels: map[string]string{"lease": "cbx_delete_key_fail"}},
	}}
	if err := b.ReleaseLease(context.Background(), release); err == nil || !strings.Contains(err.Error(), "key removal unavailable") {
		t.Fatalf("release err=%v", err)
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence("cbx_delete_key_fail")
	if err != nil || !exists || !falDeleteStateMatches(claim, falDeleteAcceptedLabel, "inst_owned") {
		t.Fatalf("claim=%#v exists=%t err=%v", claim, exists, err)
	}
	b.removeLeaseKey = nil
	if err := b.ReleaseLease(context.Background(), release); err != nil {
		t.Fatal(err)
	}
	if len(api.deletedIDs) != 1 {
		t.Fatalf("accepted deletion was reissued: %#v", api.deletedIDs)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence("cbx_delete_key_fail"); err != nil || exists {
		t.Fatalf("claim exists=%t err=%v", exists, err)
	}
}

func TestFalReleaseAcceptsDeleteNotFoundAfterIdentityProof(t *testing.T) {
	api := &fakeFalAPI{
		instances:               map[string]ComputeInstance{"inst_owned": readyFalInstance("inst_owned", "203.0.113.10")},
		deleteErr:               &APIError{StatusCode: 404, Status: "404 Not Found", Message: "not found"},
		removeBeforeDeleteError: true,
	}
	b := newFalTestBackend(t, api)
	claimFalLease(t, b.cfg, "cbx_delete_race", "delete-race", "inst_owned", "203.0.113.10", false)
	err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{
		LeaseID: "cbx_delete_race",
		Server:  core.Server{CloudID: "inst_owned", Provider: providerName, Labels: map[string]string{"lease": "cbx_delete_race"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists, readErr := core.ReadLeaseClaimWithPresence("cbx_delete_race"); readErr != nil || exists {
		t.Fatalf("claim exists=%t err=%v", exists, readErr)
	}
}

func TestFalReleaseRetriesDeleteNotFoundWhenInstanceRemainsLive(t *testing.T) {
	api := &fakeFalAPI{
		instances: map[string]ComputeInstance{"inst_owned": readyFalInstance("inst_owned", "203.0.113.10")},
		deleteErr: &APIError{StatusCode: 404, Status: "404 Not Found", Message: "not found"},
	}
	b := newFalTestBackend(t, api)
	b.pollInterval = time.Millisecond
	b.pollTimeout = 20 * time.Millisecond
	claimFalLease(t, b.cfg, "cbx_delete_404_live", "delete-404-live", "inst_owned", "203.0.113.10", false)
	release := core.ReleaseLeaseRequest{Lease: core.LeaseTarget{
		LeaseID: "cbx_delete_404_live",
		Server:  core.Server{CloudID: "inst_owned", Provider: providerName, Labels: map[string]string{"lease": "cbx_delete_404_live"}},
	}}
	if err := b.ReleaseLease(context.Background(), release); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first release err=%v", err)
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence("cbx_delete_404_live")
	if err != nil || !exists || !falDeleteStateMatches(claim, falDeleteAttemptLabel, "inst_owned") || falDeleteAccepted(claim, "inst_owned") {
		t.Fatalf("claim=%#v exists=%t err=%v", claim, exists, err)
	}
	api.deleteErr = nil
	if err := b.ReleaseLease(context.Background(), release); err != nil {
		t.Fatal(err)
	}
	if len(api.deletedIDs) != 2 {
		t.Fatalf("delete requests=%#v", api.deletedIDs)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence("cbx_delete_404_live"); err != nil || exists {
		t.Fatalf("claim exists=%t err=%v", exists, err)
	}
}

func TestFalReleaseDoesNotAcceptDeleteNotFoundWhenInventoryStillContainsInstance(t *testing.T) {
	notFound := &APIError{StatusCode: 404, Status: "404 Not Found", Message: "not found"}
	api := &fakeFalAPI{
		instances: map[string]ComputeInstance{"inst_owned": readyFalInstance("inst_owned", "203.0.113.10")},
		deleteErr: notFound,
	}
	api.afterDeleteHook = func(string) { api.getErr = notFound }
	b := newFalTestBackend(t, api)
	b.pollInterval = time.Millisecond
	b.pollTimeout = 20 * time.Millisecond
	claimFalLease(t, b.cfg, "cbx_delete_404_masked", "delete-404-masked", "inst_owned", "203.0.113.10", false)
	err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{
		LeaseID: "cbx_delete_404_masked",
		Server:  core.Server{CloudID: "inst_owned", Provider: providerName, Labels: map[string]string{"lease": "cbx_delete_404_masked"}},
	}})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("release err=%v", err)
	}
	claim, exists, readErr := core.ReadLeaseClaimWithPresence("cbx_delete_404_masked")
	if readErr != nil || !exists || !falDeleteStateMatches(claim, falDeleteAttemptLabel, "inst_owned") || falDeleteAccepted(claim, "inst_owned") {
		t.Fatalf("claim=%#v exists=%t err=%v", claim, exists, readErr)
	}
}

func TestFalReleaseDoesNotFinalizeDeleteAttemptWhenInventoryConflicts(t *testing.T) {
	notFound := &APIError{StatusCode: 404, Status: "404 Not Found", Message: "not found"}
	b := newFalTestBackend(t, &fakeFalAPI{
		instances: map[string]ComputeInstance{"inst_owned": readyFalInstance("inst_owned", "203.0.113.10")},
		getErr:    notFound,
	})
	claimFalLease(t, b.cfg, "cbx_delete_attempt", "delete-attempt", "inst_owned", "203.0.113.10", false)
	claim, exists, err := core.ReadLeaseClaimWithPresence("cbx_delete_attempt")
	if err != nil || !exists {
		t.Fatalf("claim exists=%t err=%v", exists, err)
	}
	claim, err = persistFalDeleteState(claim, falDeleteAttemptLabel, "inst_owned")
	if err != nil {
		t.Fatal(err)
	}
	err = b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{
		LeaseID: claim.LeaseID,
		Server:  core.Server{CloudID: "inst_owned", Provider: providerName, Labels: map[string]string{"lease": claim.LeaseID}},
	}})
	if err == nil || !strings.Contains(err.Error(), errFalProviderAbsenceNotAccountBound.Error()) {
		t.Fatalf("release err=%v", err)
	}
	current, exists, readErr := core.ReadLeaseClaimWithPresence(claim.LeaseID)
	if readErr != nil || !exists || !falDeleteStateMatches(current, falDeleteAttemptLabel, "inst_owned") || falDeleteStateMatches(current, falDeleteAcceptedLabel, "inst_owned") {
		t.Fatalf("claim=%#v exists=%t err=%v", current, exists, readErr)
	}
}

func TestFalReleaseReconcilesAmbiguousDeleteWithCompleteAbsenceProof(t *testing.T) {
	api := &fakeFalAPI{
		instances:               map[string]ComputeInstance{"inst_owned": readyFalInstance("inst_owned", "203.0.113.10")},
		deleteErr:               context.Canceled,
		removeBeforeDeleteError: true,
	}
	b := newFalTestBackend(t, api)
	claimFalLease(t, b.cfg, "cbx_delete_ambiguous", "delete-ambiguous", "inst_owned", "203.0.113.10", false)
	release := core.ReleaseLeaseRequest{Lease: core.LeaseTarget{
		LeaseID: "cbx_delete_ambiguous",
		Server:  core.Server{CloudID: "inst_owned", Provider: providerName, Labels: map[string]string{"lease": "cbx_delete_ambiguous"}},
	}}
	if err := b.ReleaseLease(context.Background(), release); !errors.Is(err, context.Canceled) {
		t.Fatalf("first release err=%v", err)
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence("cbx_delete_ambiguous")
	if err != nil || !exists || !falDeleteStateMatches(claim, falDeleteAttemptLabel, "inst_owned") || falDeleteAccepted(claim, "inst_owned") {
		t.Fatalf("claim=%#v exists=%t err=%v", claim, exists, err)
	}
	if err := b.ReleaseLease(context.Background(), release); err != nil {
		t.Fatal(err)
	}
	if len(api.deletedIDs) != 1 {
		t.Fatalf("ambiguous deletion was reissued: %#v", api.deletedIDs)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence("cbx_delete_ambiguous"); err != nil || exists {
		t.Fatalf("claim exists=%t err=%v", exists, err)
	}
}

func TestFalReleaseRejectsClaimChangedBeforeDeletion(t *testing.T) {
	api := &fakeFalAPI{instances: map[string]ComputeInstance{
		"inst_owned": readyFalInstance("inst_owned", "203.0.113.10"),
	}}
	b := newFalTestBackend(t, api)
	claimFalLease(t, b.cfg, "cbx_abcdef123456", "owned", "inst_owned", "203.0.113.10", false)
	b.clientFactory = func(Config, Runtime) (computeAPI, error) {
		claim, ok, err := core.ReadLeaseClaimWithPresence("cbx_abcdef123456")
		if err != nil || !ok {
			return nil, fmt.Errorf("read claim before release: ok=%v err=%w", ok, err)
		}
		labels := cloneLabels(claim.Labels)
		labels["state"] = "renewed"
		if _, err := core.UpdateLeaseClaimLabelsIfUnchanged(claim.LeaseID, claim, labels); err != nil {
			return nil, err
		}
		return api, nil
	}
	err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{
		LeaseID: "cbx_abcdef123456",
		Server:  core.Server{CloudID: "inst_owned", Provider: providerName, Labels: map[string]string{"lease": "cbx_abcdef123456"}},
	}})
	if err == nil {
		t.Fatal("expected changed-claim release rejection")
	}
	if len(api.deletedIDs) != 0 {
		t.Fatalf("changed claim was deleted: %#v", api.deletedIDs)
	}
	if _, ok, err := core.ReadLeaseClaimWithPresence("cbx_abcdef123456"); err != nil || !ok {
		t.Fatalf("changed claim retained=%v err=%v", ok, err)
	}
}

func TestFalReleaseRefusesRecoveryClaimWithoutCloudID(t *testing.T) {
	api := &fakeFalAPI{instances: map[string]ComputeInstance{
		"inst_foreign": readyFalInstance("inst_foreign", "203.0.113.40"),
	}}
	b := newFalTestBackend(t, api)
	if err := b.persistRecoveryClaim("cbx_recovery123", "recovery", b.configForRun(), "", "", "ambiguous-create", true); err != nil {
		t.Fatal(err)
	}
	err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{
		LeaseID: "cbx_recovery123",
		Server:  core.Server{CloudID: "inst_foreign", Provider: providerName, Labels: map[string]string{"lease": "cbx_recovery123"}},
	}})
	if err == nil || !strings.Contains(err.Error(), "recovery is still pending") {
		t.Fatalf("err=%v", err)
	}
	if len(api.deletedIDs) != 0 {
		t.Fatalf("deleted unclaimed instance through recovery claim: %#v", api.deletedIDs)
	}
}

func TestFalCleanupDeletesOnlyExpiredClaimedInstances(t *testing.T) {
	api := &fakeFalAPI{instances: map[string]ComputeInstance{
		"inst_expired": readyFalInstance("inst_expired", "203.0.113.20"),
		"inst_foreign": readyFalInstance("inst_foreign", "203.0.113.21"),
	}}
	b := newFalTestBackend(t, api)
	claimFalLease(t, b.cfg, "cbx_expired1234", "expired", "inst_expired", "203.0.113.20", true)
	legacy, exists, err := core.ReadLeaseClaimWithPresence("cbx_expired1234")
	if err != nil || !exists {
		t.Fatalf("legacy ready claim exists=%t err=%v", exists, err)
	}
	marked := legacy
	legacy.Labels = cloneLabels(legacy.Labels)
	delete(legacy.Labels, falAcquireLifetimeLabel)
	if err := core.ReplaceLeaseClaimIfUnchangedDurable(legacy.LeaseID, marked, legacy); err != nil {
		t.Fatal(err)
	}

	if err := b.Cleanup(context.Background(), core.CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if len(api.deletedIDs) != 0 {
		t.Fatalf("dry-run deleted: %#v", api.deletedIDs)
	}
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(api.deletedIDs) != 1 || api.deletedIDs[0] != "inst_expired" {
		t.Fatalf("deletedIDs=%#v", api.deletedIDs)
	}
	if _, ok := api.instances["inst_foreign"]; !ok {
		t.Fatal("cleanup deleted unclaimed foreign instance")
	}
}

func TestFalCleanupRetainsUnmarkedLegacyRecoveryClaims(t *testing.T) {
	for _, tc := range []struct {
		recovery   string
		instanceID string
	}{
		{recovery: "create-intent"},
		{recovery: "ambiguous-create"},
		{recovery: "ambiguous-create-inflight"},
		{recovery: "provisioning", instanceID: "inst_legacy_provisioning"},
	} {
		for _, marker := range []string{"", " 1 ", "2"} {
			name := tc.recovery + "/marker-" + strings.ReplaceAll(marker, " ", "_")
			t.Run(name, func(t *testing.T) {
				api := &fakeFalAPI{instances: map[string]ComputeInstance{}}
				if tc.instanceID != "" {
					api.instances[tc.instanceID] = readyFalInstance(tc.instanceID, "203.0.113.42")
				}
				b := newFalTestBackend(t, api)
				var stderr strings.Builder
				b.rt.Stderr = &stderr
				leaseID := "cbx_legacy_" + strings.ReplaceAll(tc.recovery, "-", "_")
				var claim core.LeaseClaim
				if tc.instanceID == "" {
					claim, _ = persistFalCreateRecoveryClaim(t, b, leaseID, "legacy-recovery", tc.recovery, false, b.now())
				} else {
					var err error
					claim, err = b.persistRecoveryClaimAtIfUnchanged(
						leaseID, "legacy-recovery", b.configForRun(), "", tc.instanceID, tc.recovery, false, b.now(), core.LeaseClaim{}, false,
					)
					if err != nil {
						t.Fatal(err)
					}
				}
				marked := claim
				claim.Labels = cloneLabels(claim.Labels)
				if marker == "" {
					delete(claim.Labels, falAcquireLifetimeLabel)
				} else {
					claim.Labels[falAcquireLifetimeLabel] = marker
				}
				if err := core.ReplaceLeaseClaimIfUnchangedDurable(claim.LeaseID, marked, claim); err != nil {
					t.Fatal(err)
				}
				clientCalls := 0
				b.clientFactory = func(Config, Runtime) (computeAPI, error) {
					clientCalls++
					return api, nil
				}

				if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
					t.Fatal(err)
				}
				if clientCalls != 0 || len(api.createRequests) != 0 || len(api.deletedIDs) != 0 {
					t.Fatalf("legacy cleanup mutated provider clientCalls=%d create=%d delete=%#v", clientCalls, len(api.createRequests), api.deletedIDs)
				}
				if !strings.Contains(stderr.String(), "reason=legacy_recovery_pending") {
					t.Fatalf("stderr=%q", stderr.String())
				}
				current, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID)
				if err != nil || !exists || current.Labels[falAcquireLifetimeLabel] != marker {
					t.Fatalf("legacy claim exists=%t err=%v labels=%#v", exists, err, current.Labels)
				}
			})
		}
	}
}

func TestFalLegacyAmbiguousRecoveryPreservesUntrustedLifetimeMarker(t *testing.T) {
	for _, marker := range []string{"", " 1 ", "2"} {
		t.Run("marker-"+strings.ReplaceAll(marker, " ", "_"), func(t *testing.T) {
			api := &fakeFalAPI{createErr: io.ErrUnexpectedEOF}
			b := newFalTestBackend(t, api)
			claim, _ := persistFalCreateRecoveryClaim(t, b, "cbx_legacy_replay", "legacy-replay", "ambiguous-create", false, b.now())
			marked := claim
			claim.Labels = cloneLabels(claim.Labels)
			if marker == "" {
				delete(claim.Labels, falAcquireLifetimeLabel)
			} else {
				claim.Labels[falAcquireLifetimeLabel] = marker
			}
			if err := core.ReplaceLeaseClaimIfUnchangedDurable(claim.LeaseID, marked, claim); err != nil {
				t.Fatal(err)
			}

			if _, err := b.recoverAmbiguousCreateForRelease(context.Background(), api, claim, b.configForRun()); err == nil {
				t.Fatal("expected ambiguous recovery failure")
			}
			current, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID)
			if err != nil || !exists {
				t.Fatalf("legacy claim exists=%t err=%v", exists, err)
			}
			value, present := current.Labels[falAcquireLifetimeLabel]
			if value != marker || present != (marker != "") {
				t.Fatalf("legacy lifetime marker value=%q present=%t want value=%q present=%t", value, present, marker, marker != "")
			}
		})
	}
}

func TestFalCleanupRemovesAbandonedCreateIntentWithoutProviderMutation(t *testing.T) {
	b := newFalTestBackend(t, &fakeFalAPI{})
	const leaseID = "cbx_cleanup_intent"
	claim, _ := persistFalCreateRecoveryClaim(t, b, leaseID, "cleanup-intent", "create-intent", false, b.now())
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	clientCalls := 0
	b.clientFactory = func(Config, Runtime) (computeAPI, error) {
		clientCalls++
		return nil, errors.New("provider must not be called")
	}

	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if clientCalls != 0 {
		t.Fatalf("provider client calls=%d", clientCalls)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID); err != nil || exists {
		t.Fatalf("claim exists=%t err=%v", exists, err)
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("key stat err=%v, want not exist", err)
	}
}

func TestFalCleanupRecoversAmbiguousCreateAndDeletesExactInstance(t *testing.T) {
	for _, recovery := range []string{"ambiguous-create", "ambiguous-create-inflight"} {
		t.Run(recovery, func(t *testing.T) {
			api := &fakeFalAPI{}
			b := newFalTestBackend(t, api)
			leaseID := "cbx_cleanup_" + strings.ReplaceAll(recovery, "-", "_")
			claim, request := persistFalCreateRecoveryClaim(t, b, leaseID, "cleanup-ambiguous", recovery, false, b.now())

			if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
				t.Fatal(err)
			}
			if len(api.createRequests) != 1 || api.createRequests[0] != request {
				t.Fatalf("create requests=%#v want %#v", api.createRequests, request)
			}
			if len(api.idempotency) != 1 || api.idempotency[0] != claim.LeaseID {
				t.Fatalf("idempotency=%#v want %s", api.idempotency, claim.LeaseID)
			}
			if len(api.deletedIDs) != 1 || api.deletedIDs[0] != "inst_created" {
				t.Fatalf("deleted ids=%#v", api.deletedIDs)
			}
			if _, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID); err != nil || exists {
				t.Fatalf("claim exists=%t err=%v", exists, err)
			}
		})
	}
}

func TestFalCleanupDryRunReportsAmbiguousRecoveryWithoutMutation(t *testing.T) {
	api := &fakeFalAPI{}
	b := newFalTestBackend(t, api)
	var stderr strings.Builder
	b.rt.Stderr = &stderr
	claim, _ := persistFalCreateRecoveryClaim(t, b, "cbx_cleanup_dryrun", "cleanup-dryrun", "ambiguous-create", false, b.now())

	if err := b.Cleanup(context.Background(), core.CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if len(api.createRequests) != 0 || len(api.deletedIDs) != 0 || api.listCalls != 0 {
		t.Fatalf("dry-run provider mutations create=%d delete=%d list=%d", len(api.createRequests), len(api.deletedIDs), api.listCalls)
	}
	if !strings.Contains(stderr.String(), "reason=recover_ambiguous_create") {
		t.Fatalf("stderr=%q", stderr.String())
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID); err != nil || !exists {
		t.Fatalf("claim exists=%t err=%v", exists, err)
	}
}

func TestFalCleanupSkipsKeepRecoveryClaims(t *testing.T) {
	for _, tc := range []struct {
		name      string
		recovery  string
		instance  string
		startedAt time.Time
	}{
		{name: "intent", recovery: "create-intent"},
		{name: "ambiguous", recovery: "ambiguous-create"},
		{name: "known-id", recovery: "provisioning", instance: "inst_keep", startedAt: time.Now().Add(-24 * time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newFalTestBackend(t, &fakeFalAPI{})
			const leaseID = "cbx_cleanup_keep"
			var claim core.LeaseClaim
			if tc.instance == "" {
				claim, _ = persistFalCreateRecoveryClaim(t, b, leaseID, "cleanup-keep", tc.recovery, true, tc.startedAt)
			} else {
				var err error
				claim, err = b.persistRecoveryClaimAtIfUnchanged(leaseID, "cleanup-keep", b.configForRun(), "", tc.instance, tc.recovery, true, tc.startedAt, core.LeaseClaim{}, false)
				if err != nil {
					t.Fatal(err)
				}
			}
			clientCalls := 0
			b.clientFactory = func(Config, Runtime) (computeAPI, error) {
				clientCalls++
				return nil, errors.New("provider must not be called")
			}
			if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
				t.Fatal(err)
			}
			if clientCalls != 0 {
				t.Fatalf("provider client calls=%d", clientCalls)
			}
			if _, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID); err != nil || !exists {
				t.Fatalf("claim exists=%t err=%v", exists, err)
			}
		})
	}
}

func TestFalCleanupSkipsLiveIdentitylessAcquireClaims(t *testing.T) {
	for _, recovery := range []string{"create-intent", "ambiguous-create", "ambiguous-create-inflight"} {
		t.Run(recovery, func(t *testing.T) {
			b := newFalTestBackend(t, &fakeFalAPI{})
			var stderr strings.Builder
			b.rt.Stderr = &stderr
			claim, _ := persistFalCreateRecoveryClaim(t, b, "cbx_cleanup_fresh_"+strings.ReplaceAll(recovery, "-", "_"), "cleanup-fresh", recovery, false, b.now())
			unlockAcquire, err := lockFalAcquireLifetime(context.Background(), claim.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			defer unlockAcquire()
			clientCalls := 0
			b.clientFactory = func(Config, Runtime) (computeAPI, error) {
				clientCalls++
				return nil, errors.New("provider must not be called")
			}

			if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
				t.Fatal(err)
			}
			if clientCalls != 0 || !strings.Contains(stderr.String(), "reason=acquire-in-progress") {
				t.Fatalf("clientCalls=%d stderr=%q", clientCalls, stderr.String())
			}
			if _, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID); err != nil || !exists {
				t.Fatalf("claim exists=%t err=%v", exists, err)
			}
		})
	}
}

func TestFalCleanupOverridesKeepForForcedRecoveryStates(t *testing.T) {
	t.Run("terminal rejection", func(t *testing.T) {
		b := newFalTestBackend(t, &fakeFalAPI{})
		claim, _ := persistFalCreateRecoveryClaim(t, b, "cbx_cleanup_rejected_keep", "cleanup-rejected-keep", falCreateRejectedRecovery, true, b.now())
		if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
			t.Fatal(err)
		}
		if _, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID); err != nil || exists {
			t.Fatalf("claim exists=%t err=%v", exists, err)
		}
	})

	t.Run("rollback cleanup", func(t *testing.T) {
		api := &fakeFalAPI{instances: map[string]ComputeInstance{
			"inst_rollback_keep": readyFalInstance("inst_rollback_keep", "203.0.113.43"),
		}}
		b := newFalTestBackend(t, api)
		claim, err := b.persistRecoveryClaimAtIfUnchanged(
			"cbx_cleanup_rollback_keep", "cleanup-rollback-keep", b.configForRun(), "", "inst_rollback_keep", "rollback-cleanup", true, b.now(), core.LeaseClaim{}, false,
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
			t.Fatal(err)
		}
		if len(api.deletedIDs) != 1 || api.deletedIDs[0] != "inst_rollback_keep" {
			t.Fatalf("deleted ids=%#v", api.deletedIDs)
		}
		if _, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID); err != nil || exists {
			t.Fatalf("claim exists=%t err=%v", exists, err)
		}
	})
}

func TestFalCleanupFinishesDeletionAlreadyStartedForKeptLease(t *testing.T) {
	api := &fakeFalAPI{instances: map[string]ComputeInstance{
		"inst_keep_delete": readyFalInstance("inst_keep_delete", "203.0.113.42"),
	}}
	b := newFalTestBackend(t, api)
	const leaseID = "cbx_cleanup_keep_delete"
	claimFalLease(t, b.cfg, leaseID, "cleanup-keep-delete", "inst_keep_delete", "203.0.113.42", true)
	claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil || !exists {
		t.Fatalf("claim exists=%t err=%v", exists, err)
	}
	if _, err := persistFalDeleteState(claim, falDeleteAttemptLabel, "inst_keep_delete"); err != nil {
		t.Fatal(err)
	}
	unblockAcquire, err := lockFalAcquireLifetime(context.Background(), leaseID)
	if err != nil {
		t.Fatal(err)
	}
	defer unblockAcquire()

	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(api.deletedIDs) != 1 || api.deletedIDs[0] != "inst_keep_delete" {
		t.Fatalf("deleted ids=%#v", api.deletedIDs)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(leaseID); err != nil || exists {
		t.Fatalf("claim exists=%t err=%v", exists, err)
	}
}

func TestFalCleanupExpiredAmbiguousRecoveryRequiresManualReconciliation(t *testing.T) {
	api := &fakeFalAPI{}
	b := newFalTestBackend(t, api)
	claim, _ := persistFalCreateRecoveryClaim(t, b, "cbx_cleanup_expired", "cleanup-expired", "ambiguous-create", false, b.now().Add(-falCreateRecoveryWindow))

	err := b.Cleanup(context.Background(), core.CleanupRequest{})
	if err == nil || !strings.Contains(err.Error(), "recovery window expired") || !strings.Contains(err.Error(), "manual provider reconciliation") {
		t.Fatalf("cleanup err=%v", err)
	}
	if len(api.createRequests) != 0 || len(api.deletedIDs) != 0 {
		t.Fatalf("expired recovery mutated provider create=%d delete=%d", len(api.createRequests), len(api.deletedIDs))
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID); err != nil || !exists {
		t.Fatalf("claim exists=%t err=%v", exists, err)
	}
}

func TestFalCleanupExpiredRecoveryDoesNotStarveLaterForcedCleanup(t *testing.T) {
	api := &fakeFalAPI{instances: map[string]ComputeInstance{
		"inst_rollback": readyFalInstance("inst_rollback", "203.0.113.44"),
	}}
	b := newFalTestBackend(t, api)
	expired, _ := persistFalCreateRecoveryClaim(t, b, "cbx_a_expired", "expired", "ambiguous-create", false, b.now().Add(-falCreateRecoveryWindow))
	rollback, err := b.persistRecoveryClaimAtIfUnchanged(
		"cbx_z_rollback", "rollback", b.configForRun(), "", "inst_rollback", "rollback-cleanup", false, b.now(), core.LeaseClaim{}, false,
	)
	if err != nil {
		t.Fatal(err)
	}

	err = b.Cleanup(context.Background(), core.CleanupRequest{})
	if err == nil || !strings.Contains(err.Error(), expired.LeaseID) || !strings.Contains(err.Error(), "recovery window expired") {
		t.Fatalf("cleanup err=%v", err)
	}
	if len(api.deletedIDs) != 1 || api.deletedIDs[0] != rollback.CloudID {
		t.Fatalf("deleted ids=%#v want %s", api.deletedIDs, rollback.CloudID)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(expired.LeaseID); err != nil || !exists {
		t.Fatalf("expired claim exists=%t err=%v", exists, err)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(rollback.LeaseID); err != nil || exists {
		t.Fatalf("rollback claim exists=%t err=%v", exists, err)
	}
}

func TestFalCleanupCanceledAfterKeepClaimReportsCancellation(t *testing.T) {
	api := &fakeFalAPI{instances: map[string]ComputeInstance{
		"inst_rollback": readyFalInstance("inst_rollback", "203.0.113.45"),
	}}
	b := newFalTestBackend(t, api)
	claimFalLease(t, b.cfg, "cbx_a_keep", "keep", "inst_keep", "203.0.113.46", false)
	keep, exists, err := core.ReadLeaseClaimWithPresence("cbx_a_keep")
	if err != nil || !exists {
		t.Fatalf("keep claim exists=%t err=%v", exists, err)
	}
	kept := keep
	kept.Labels = cloneLabels(keep.Labels)
	kept.Labels["keep"] = "true"
	if err := core.ReplaceLeaseClaimIfUnchangedDurable(keep.LeaseID, keep, kept); err != nil {
		t.Fatal(err)
	}
	rollback, err := b.persistRecoveryClaimAtIfUnchanged(
		"cbx_z_rollback_cancel", "rollback-cancel", b.configForRun(), "", "inst_rollback", "rollback-cleanup", false, b.now(), core.LeaseClaim{}, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.Cleanup(ctx, core.CleanupRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cleanup err=%v want context canceled", err)
	}
	if len(api.deletedIDs) != 0 {
		t.Fatalf("canceled cleanup deleted=%#v", api.deletedIDs)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(rollback.LeaseID); err != nil || !exists {
		t.Fatalf("rollback claim exists=%t err=%v", exists, err)
	}
}

func TestFalCleanupSkipsLiveProvisioningAcquireAndCleansAfterExit(t *testing.T) {
	started := time.Date(2026, time.July, 9, 12, 0, 0, 0, time.UTC)
	clock := &mutableFalClock{now: started.Add(3 * time.Hour)}
	api := &fakeFalAPI{instances: map[string]ComputeInstance{"inst_provisioning": readyFalInstance("inst_provisioning", "203.0.113.42")}}
	b := newFalTestBackend(t, api)
	b.rt.Clock = clock
	claim, err := b.persistRecoveryClaimAtIfUnchanged(
		"cbx_cleanup_provisioning", "cleanup-provisioning", b.configForRun(), "", "inst_provisioning", "provisioning", false, started, core.LeaseClaim{}, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	claim, err = markFalAcquireLifetimeClaimForTest(claim)
	if err != nil {
		t.Fatal(err)
	}
	unlockAcquire, err := lockFalAcquireLifetime(context.Background(), claim.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	clientCalls := 0
	b.clientFactory = func(Config, Runtime) (computeAPI, error) {
		clientCalls++
		return api, nil
	}

	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if clientCalls != 0 || len(api.deletedIDs) != 0 {
		t.Fatalf("active acquire clientCalls=%d deleted=%#v", clientCalls, api.deletedIDs)
	}
	unlockAcquire()
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if clientCalls != 1 || len(api.deletedIDs) != 1 || api.deletedIDs[0] != "inst_provisioning" {
		t.Fatalf("stale acquire clientCalls=%d deleted=%#v", clientCalls, api.deletedIDs)
	}
}

func TestFalAcquireHoldsCleanupLivenessThroughOnAcquired(t *testing.T) {
	api := &fakeFalAPI{}
	b := newFalTestBackend(t, api)
	var cleanupErr error
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{
		RequestedSlug: "live-on-acquired",
		OnAcquired: func(core.LeaseTarget) error {
			cleanupErr = b.Cleanup(context.Background(), core.CleanupRequest{})
			return cleanupErr
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cleanupErr != nil || len(api.deletedIDs) != 0 {
		t.Fatalf("cleanup err=%v deleted=%#v", cleanupErr, api.deletedIDs)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID); err != nil || !exists {
		t.Fatalf("ready claim exists=%t err=%v", exists, err)
	}
}

func TestFalCleanupProvisioningCASRejectsConcurrentReadyPublication(t *testing.T) {
	started := time.Date(2026, time.July, 9, 12, 0, 0, 0, time.UTC)
	clock := &mutableFalClock{now: started}
	api := &fakeFalAPI{instances: map[string]ComputeInstance{"inst_ready": readyFalInstance("inst_ready", "203.0.113.42")}}
	b := newFalTestBackend(t, api)
	b.rt.Clock = clock
	b.pollTimeout = time.Minute
	claim, err := b.persistRecoveryClaimAtIfUnchanged(
		"cbx_cleanup_ready_race", "cleanup-ready-race", b.configForRun(), "", "inst_ready", "provisioning", false, started, core.LeaseClaim{}, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	claim, err = markFalAcquireLifetimeClaimForTest(claim)
	if err != nil {
		t.Fatal(err)
	}
	unlockReady, err := lockFalLeaseOperation(context.Background(), claim.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	clientRequested := make(chan struct{})
	b.clientFactory = func(Config, Runtime) (computeAPI, error) {
		close(clientRequested)
		return api, nil
	}

	cleanupDone := make(chan error, 1)
	go func() {
		cleanupDone <- b.Cleanup(context.Background(), core.CleanupRequest{})
	}()
	select {
	case <-clientRequested:
	case <-time.After(time.Second):
		unlockReady()
		t.Fatal("cleanup did not reach provider client")
	}
	current, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID)
	if err != nil || !exists {
		unlockReady()
		t.Fatalf("read provisioning claim: exists=%t err=%v", exists, err)
	}
	ready := current
	ready.Labels = cloneLabels(current.Labels)
	delete(ready.Labels, "recovery")
	ready.Labels["state"] = "ready"
	if err := core.ReplaceLeaseClaimIfUnchangedDurable(current.LeaseID, current, ready); err != nil {
		unlockReady()
		t.Fatal(err)
	}
	unlockReady()
	if err := <-cleanupDone; err == nil {
		t.Fatal("expected stale provisioning snapshot rejection")
	}
	if len(api.deletedIDs) != 0 {
		t.Fatalf("concurrently published ready instance deleted: %#v", api.deletedIDs)
	}
	current, exists, err = core.ReadLeaseClaimWithPresence(claim.LeaseID)
	if err != nil || !exists || current.Labels["state"] != "ready" {
		t.Fatalf("current claim=%#v exists=%t err=%v", current, exists, err)
	}
}

func TestFalWaitForInstanceReadyBoundsBlockedProviderRead(t *testing.T) {
	b := newFalTestBackend(t, &fakeFalAPI{})
	b.pollTimeout = 25 * time.Millisecond
	api := &fakeFalAPI{blockGetUntilContext: true}
	started := time.Now()
	_, err := b.waitForInstanceReady(context.Background(), api, "inst_blocked")
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "timed out waiting") {
		t.Fatalf("wait err=%v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("blocked provider read outlived poll timeout: %v", elapsed)
	}
}

func TestFalWaitForInstanceReadyRejectsChangedIdentity(t *testing.T) {
	api := &fakeFalAPI{instances: map[string]ComputeInstance{
		"inst_requested": readyFalInstance("inst_other", "203.0.113.42"),
	}}
	b := newFalTestBackend(t, api)
	if _, err := b.waitForInstanceReady(context.Background(), api, "inst_requested"); err == nil || !strings.Contains(err.Error(), "changed identity inst_other") {
		t.Fatalf("wait err=%v", err)
	}
}

func TestFalCleanupSkipsOtherCredentialClaimsWithoutBlockingMatches(t *testing.T) {
	api := &fakeFalAPI{instances: map[string]ComputeInstance{
		"inst_first":  readyFalInstance("inst_first", "203.0.113.20"),
		"inst_second": readyFalInstance("inst_second", "203.0.113.21"),
	}}
	b := newFalTestBackend(t, api)
	firstCfg := b.cfg
	secondCfg := b.cfg
	secondCfg.Fal.APIKey = "second-test-key"
	claimFalLease(t, firstCfg, "cbx_first123456", "first", "inst_first", "203.0.113.20", true)
	claimFalLease(t, secondCfg, "cbx_second12345", "second", "inst_second", "203.0.113.21", true)
	delete(api.instances, "inst_second")

	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(api.deletedIDs) != 1 || api.deletedIDs[0] != "inst_first" {
		t.Fatalf("first credential deletedIDs=%#v", api.deletedIDs)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider("second", providerName); err != nil || !ok {
		t.Fatalf("other credential claim retained=%v err=%v", ok, err)
	}

	b.cfg.Fal.APIKey = secondCfg.Fal.APIKey
	api.instances["inst_second"] = readyFalInstance("inst_second", "203.0.113.21")
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(api.deletedIDs) != 2 || api.deletedIDs[1] != "inst_second" {
		t.Fatalf("second credential deletedIDs=%#v", api.deletedIDs)
	}
}

func TestFalCleanupWithoutCredentialStillFinalizesConfirmedDeletion(t *testing.T) {
	b := newFalTestBackend(t, &fakeFalAPI{})
	const retainedLeaseID = "cbx_a_missing_key"
	const confirmedLeaseID = "cbx_z_confirmed"
	claimFalLease(t, b.cfg, retainedLeaseID, "missing-key", "inst_missing_key", "203.0.113.20", true)
	claimFalLease(t, b.cfg, confirmedLeaseID, "confirmed", "inst_confirmed", "203.0.113.21", false)
	keyPath, _, err := core.EnsureTestboxKeyForConfig(b.cfg, confirmedLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(confirmedLeaseID)
	if err != nil || !exists {
		t.Fatalf("claim exists=%t err=%v", exists, err)
	}
	if _, err := persistFalDeleteState(claim, falDeleteConfirmedLabel, "inst_confirmed"); err != nil {
		t.Fatal(err)
	}
	b.cfg.Fal.APIKey = ""
	b.clientFactory = func(Config, Runtime) (computeAPI, error) {
		return nil, errors.New("provider client must not be constructed")
	}

	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(retainedLeaseID); err != nil || !exists {
		t.Fatalf("unverified claim exists=%t err=%v", exists, err)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(confirmedLeaseID); err != nil || exists {
		t.Fatalf("confirmed claim exists=%t err=%v", exists, err)
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("confirmed deletion key residue: %v", err)
	}
}

func TestFalCleanupRejectsClaimChangedBeforeDeletion(t *testing.T) {
	api := &fakeFalAPI{instances: map[string]ComputeInstance{
		"inst_expired": readyFalInstance("inst_expired", "203.0.113.20"),
	}}
	b := newFalTestBackend(t, api)
	claimFalLease(t, b.cfg, "cbx_expired1234", "expired", "inst_expired", "203.0.113.20", true)
	b.clientFactory = func(Config, Runtime) (computeAPI, error) {
		claim, ok, err := core.ReadLeaseClaimWithPresence("cbx_expired1234")
		if err != nil || !ok {
			return nil, fmt.Errorf("read claim before cleanup: ok=%v err=%w", ok, err)
		}
		labels := cloneLabels(claim.Labels)
		labels["state"] = "renewed"
		if _, err := core.UpdateLeaseClaimLabelsIfUnchanged(claim.LeaseID, claim, labels); err != nil {
			return nil, err
		}
		return api, nil
	}
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err == nil {
		t.Fatal("expected changed-claim cleanup rejection")
	}
	if len(api.deletedIDs) != 0 {
		t.Fatalf("changed claim was deleted: %#v", api.deletedIDs)
	}
	if _, ok, err := core.ReadLeaseClaimWithPresence("cbx_expired1234"); err != nil || !ok {
		t.Fatalf("changed claim retained=%v err=%v", ok, err)
	}
}

func TestFalCleanupRetainsClaimWhenAbsenceIsNotAccountBound(t *testing.T) {
	api := &fakeFalAPI{instances: map[string]ComputeInstance{}}
	b := newFalTestBackend(t, api)
	claimFalLease(t, b.cfg, "cbx_absent12345", "absent", "inst_absent", "203.0.113.31", false)

	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if claim, ok, err := core.ResolveLeaseClaimForProvider("absent", providerName); err != nil || !ok || claim.CloudID != "inst_absent" {
		t.Fatalf("provider-absent claim=%#v ok=%v err=%v", claim, ok, err)
	}
	if len(api.deletedIDs) != 0 {
		t.Fatalf("provider-absent cleanup issued delete: %#v", api.deletedIDs)
	}
	views, err := b.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Status != "provider-absence-unverified" || views[0].CloudID != "inst_absent" {
		t.Fatalf("provider-absent views=%#v", views)
	}
}

func TestFalListShowsClaimsWhenProviderVerificationIsUnavailable(t *testing.T) {
	api := &fakeFalAPI{getErr: errors.New("control plane unavailable")}
	b := newFalTestBackend(t, api)
	claimFalLease(t, b.cfg, "cbx_unverified12", "unverified", "inst_unverified", "203.0.113.32", false)

	views, err := b.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Status != "provider-verification-unavailable" || views[0].CloudID != "inst_unverified" {
		t.Fatalf("views=%#v", views)
	}

	b.cfg.Fal.APIKey = ""
	views, err = b.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Status != "credential-binding-mismatch" {
		t.Fatalf("missing-credential views=%#v", views)
	}
	if views[0].Labels[falCredentialBindingLabel] != "" {
		t.Fatalf("credential binding leaked into list labels: %#v", views[0].Labels)
	}
}

func TestFalReleaseFinalizesProviderConfirmedAbsence(t *testing.T) {
	api := &fakeFalAPI{instances: map[string]ComputeInstance{}}
	b := newFalTestBackend(t, api)
	claimFalLease(t, b.cfg, "cbx_absent12345", "absent", "inst_absent", "203.0.113.31", false)
	keyPath, _, err := core.EnsureTestboxKeyForConfig(b.cfg, "cbx_absent12345")
	if err != nil {
		t.Fatal(err)
	}

	err = b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{
		LeaseID: "cbx_absent12345",
		Server:  core.Server{CloudID: "inst_absent", Provider: providerName, Labels: map[string]string{"lease": "cbx_absent12345"}},
	}})
	if err != nil {
		t.Fatalf("release err=%v", err)
	}
	if len(api.deletedIDs) != 0 {
		t.Fatalf("delete issued after unverified absence: %#v", api.deletedIDs)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider("absent", providerName); err != nil || ok {
		t.Fatalf("claim retained=%v err=%v", ok, err)
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("key residue after provider-confirmed absence: %v", err)
	}
}

func TestFalResolveRebindsRotatedCredentialAfterProviderProof(t *testing.T) {
	api := &fakeFalAPI{instances: map[string]ComputeInstance{
		"inst_rotated": readyFalInstance("inst_rotated", "203.0.113.33"),
	}}
	b := newFalTestBackend(t, api)
	claimFalLease(t, b.cfg, "cbx_rotated1234", "rotated", "inst_rotated", "203.0.113.33", false)
	b.cfg.Fal.APIKey = "rotated-test-key"

	lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "rotated", ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
	if err != nil || !ok {
		t.Fatalf("claim exists=%t err=%v", ok, err)
	}
	if got, want := claim.Labels[falCredentialBindingLabel], falCredentialBinding(b.cfg); got != want {
		t.Fatalf("credential binding=%q want %q", got, want)
	}
}

func TestFalCleanupDryRunDoesNotPersistCredentialRebinding(t *testing.T) {
	api := &fakeFalAPI{instances: map[string]ComputeInstance{
		"inst_rotated_dry_run": readyFalInstance("inst_rotated_dry_run", "203.0.113.35"),
	}}
	b := newFalTestBackend(t, api)
	claimFalLease(t, b.cfg, "cbx_rotateddry1", "rotated-dry-run", "inst_rotated_dry_run", "203.0.113.35", true)
	original, ok, err := core.ResolveLeaseClaimForProvider("rotated-dry-run", providerName)
	if err != nil || !ok {
		t.Fatalf("original claim retained=%v err=%v", ok, err)
	}
	originalBinding := original.Labels[falCredentialBindingLabel]
	b.cfg.Fal.APIKey = "rotated-test-key"

	if err := b.Cleanup(context.Background(), core.CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	retained, ok, err := core.ResolveLeaseClaimForProvider("rotated-dry-run", providerName)
	if err != nil || !ok {
		t.Fatalf("claim retained=%v err=%v", ok, err)
	}
	if got := retained.Labels[falCredentialBindingLabel]; got != originalBinding {
		t.Fatalf("dry-run changed credential binding from %q to %q", originalBinding, got)
	}
	if len(api.deletedIDs) != 0 {
		t.Fatalf("dry-run deleted=%#v", api.deletedIDs)
	}
}

func TestFalCleanupPropagatesCredentialRebindingVerificationFailure(t *testing.T) {
	api := &fakeFalAPI{getErr: errors.New("control plane unavailable")}
	b := newFalTestBackend(t, api)
	claimFalLease(t, b.cfg, "cbx_rebindfailure", "rebind-failure", "inst_rebind_failure", "203.0.113.36", true)
	b.cfg.Fal.APIKey = "rotated-test-key"

	err := b.Cleanup(context.Background(), core.CleanupRequest{})
	if err == nil || !strings.Contains(err.Error(), "control plane unavailable") {
		t.Fatalf("cleanup err=%v", err)
	}
	if _, exists, readErr := core.ReadLeaseClaimWithPresence("cbx_rebindfailure"); readErr != nil || !exists {
		t.Fatalf("claim exists=%t err=%v", exists, readErr)
	}
	if len(api.deletedIDs) != 0 {
		t.Fatalf("verification failure deleted=%#v", api.deletedIDs)
	}
}

func TestFalResolveRejectsRotatedCredentialWithoutProviderProof(t *testing.T) {
	api := &fakeFalAPI{instances: map[string]ComputeInstance{}}
	b := newFalTestBackend(t, api)
	claimFalLease(t, b.cfg, "cbx_otherworkspace", "other-workspace", "inst_other", "203.0.113.34", false)
	original, ok, err := core.ResolveLeaseClaimForProvider("other-workspace", providerName)
	if err != nil || !ok {
		t.Fatalf("original claim retained=%v err=%v", ok, err)
	}
	originalBinding := original.Labels[falCredentialBindingLabel]
	b.cfg.Fal.APIKey = "other-workspace-key"

	if _, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "other-workspace", ReleaseOnly: true}); err == nil || !strings.Contains(err.Error(), "credential rotation") {
		t.Fatalf("resolve err=%v", err)
	}
	retained, ok, err := core.ResolveLeaseClaimForProvider("other-workspace", providerName)
	if err != nil || !ok {
		t.Fatalf("claim retained=%v err=%v", ok, err)
	}
	if got := retained.Labels[falCredentialBindingLabel]; got != originalBinding {
		t.Fatalf("failed credential proof changed binding from %q to %q", originalBinding, got)
	}
}

func TestFalTouchPersistsLocalClaimLabels(t *testing.T) {
	api := &fakeFalAPI{}
	b := newFalTestBackend(t, api)
	claimFalLease(t, b.cfg, "cbx_touch123456", "touch", "inst_touch", "203.0.113.30", false)
	claim, ok, err := core.ResolveLeaseClaimForProvider("touch", providerName)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	server := core.Server{CloudID: "inst_touch", Provider: providerName, Labels: claim.Labels}
	touched, err := b.Touch(context.Background(), core.TouchRequest{
		Lease:       core.LeaseTarget{LeaseID: claim.LeaseID, Server: server},
		State:       "running",
		IdleTimeout: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if touched.Labels["state"] != "running" {
		t.Fatalf("touched=%#v", touched.Labels)
	}
	updated, ok, err := core.ResolveLeaseClaimForProvider("touch", providerName)
	if err != nil || !ok || updated.Labels["state"] != "running" {
		t.Fatalf("updated=%#v ok=%v err=%v", updated, ok, err)
	}
}

func readyFalInstance(id, ip string) ComputeInstance {
	return ComputeInstance{
		ID:           id,
		InstanceType: InstanceTypeH100x1,
		Sector:       Sector1,
		Region:       "us-west",
		IP:           ip,
		Status:       InstanceStatusReady,
	}
}

func claimFalLease(t *testing.T, cfg Config, leaseID, slug, cloudID, host string, expired bool) {
	t.Helper()
	labels := falLabels(cfg, leaseID, slug, false, time.Now().UTC())
	labels[falCredentialBindingLabel] = falCredentialBinding(cfg)
	labels["ssh_user"] = cfg.SSHUser
	labels["ssh_port"] = cfg.SSHPort
	if expired {
		labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	}
	server := core.Server{CloudID: cloudID, Provider: providerName, Name: slug, Status: "ready", Labels: labels}
	server.PublicNet.IPv4.IP = host
	server.ServerType.Name = cfg.Fal.InstanceType
	target := core.SSHTargetFromConfig(cfg, host)
	if err := core.ClaimLeaseTargetForConfig(leaseID, slug, cfg, server, target, cfg.IdleTimeout); err != nil {
		t.Fatal(err)
	}
}
