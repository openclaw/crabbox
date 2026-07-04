package fal

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type fakeFalAPI struct {
	instances      map[string]ComputeInstance
	getErr         error
	createErr      error
	createErrs     []error
	deleteErr      error
	createRequests []CreateInstanceRequest
	idempotency    []string
	deletedIDs     []string
	listCalls      int
}

func (f *fakeFalAPI) ListInstances(context.Context, int, string) (ListInstancesResponse, error) {
	f.listCalls++
	items := make([]ComputeInstance, 0, len(f.instances))
	for _, item := range f.instances {
		items = append(items, item)
	}
	return ListInstancesResponse{Instances: items}, nil
}

func (f *fakeFalAPI) GetInstance(_ context.Context, id string) (ComputeInstance, error) {
	if f.getErr != nil {
		return ComputeInstance{}, f.getErr
	}
	item, ok := f.instances[id]
	if !ok {
		return ComputeInstance{}, &APIError{StatusCode: 404, Status: "404 Not Found", Message: "not found"}
	}
	return item, nil
}

func (f *fakeFalAPI) CreateInstance(_ context.Context, req CreateInstanceRequest, idempotencyKey string) (ComputeInstance, error) {
	f.createRequests = append(f.createRequests, req)
	f.idempotency = append(f.idempotency, idempotencyKey)
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
	return item, nil
}

func (f *fakeFalAPI) DeleteInstance(_ context.Context, id string) error {
	f.deletedIDs = append(f.deletedIDs, id)
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.instances, id)
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

func TestFalStopRecoversAmbiguousCreateWithExactIdempotentRequest(t *testing.T) {
	api := &fakeFalAPI{createErrs: []error{io.ErrUnexpectedEOF, io.ErrUnexpectedEOF, io.ErrUnexpectedEOF, io.ErrUnexpectedEOF}}
	b := newFalTestBackend(t, api)
	b.cfg.Fal.InstanceType = string(InstanceTypeH100x8)
	b.cfg.Fal.Sector = string(Sector2)

	_, err := b.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "unreconciled-create"})
	if err == nil || !strings.Contains(err.Error(), "indeterminate after idempotent retry") {
		t.Fatalf("err=%v", err)
	}
	if len(api.createRequests) != 4 {
		t.Fatalf("createRequests=%#v", api.createRequests)
	}
	claim, ok, claimErr := core.ResolveLeaseClaimForProvider("unreconciled-create", providerName)
	if claimErr != nil || !ok || claim.CloudID != "" || claim.Labels["recovery"] != "ambiguous-create" {
		t.Fatalf("recovery claim=%#v ok=%v err=%v", claim, ok, claimErr)
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
	labels["created_at"] = strconv.FormatInt(time.Now().Add(-falCreateRecoveryWindow-time.Minute).Unix(), 10)
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
	if !strings.Contains(message, "bootstrap failed") || !strings.Contains(message, "persist fal recovery claim") || !strings.Contains(message, "delete unavailable") {
		t.Fatalf("err=%v", err)
	}
}

func TestFalResolveListAndReleaseRequireLocalClaim(t *testing.T) {
	api := &fakeFalAPI{instances: map[string]ComputeInstance{
		"inst_owned":   readyFalInstance("inst_owned", "203.0.113.10"),
		"inst_foreign": readyFalInstance("inst_foreign", "203.0.113.11"),
	}}
	b := newFalTestBackend(t, api)
	claimFalLease(t, b.cfg, "cbx_abcdef123456", "owned", "inst_owned", "203.0.113.10", false)

	lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "owned"})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != "cbx_abcdef123456" || lease.Server.CloudID != "inst_owned" || lease.SSH.Host != "203.0.113.10" {
		t.Fatalf("lease=%#v", lease)
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
	if len(views) != 1 || views[0].Status != "provider-verification-unavailable" {
		t.Fatalf("missing-credential views=%#v", views)
	}
}

func TestFalReleaseRetainsClaimWhenAbsenceIsNotAccountBound(t *testing.T) {
	api := &fakeFalAPI{instances: map[string]ComputeInstance{}}
	b := newFalTestBackend(t, api)
	claimFalLease(t, b.cfg, "cbx_absent12345", "absent", "inst_absent", "203.0.113.31", false)

	err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{
		LeaseID: "cbx_absent12345",
		Server:  core.Server{CloudID: "inst_absent", Provider: providerName, Labels: map[string]string{"lease": "cbx_absent12345"}},
	}})
	if err == nil || !strings.Contains(err.Error(), "absence is not account-bound") {
		t.Fatalf("release err=%v", err)
	}
	if len(api.deletedIDs) != 0 {
		t.Fatalf("delete issued after unverified absence: %#v", api.deletedIDs)
	}
	if _, ok, err := core.ResolveLeaseClaimForProvider("absent", providerName); err != nil || !ok {
		t.Fatalf("claim retained=%v err=%v", ok, err)
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
