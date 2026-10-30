package utm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const checkpointMetadataKey = "utm"

type checkpointMetadata struct {
	Version                                                                           int `json:"metadataVersion"`
	LeaseID, VMID, Token, AppPath, StorageRoot, BundlePath, Backend, TargetOS         string
	SourceScope, SourceClaimedAt, SourceRevision, RepoRoot                            string
	UTMVersion, HostVersion, CaptureState, SnapshotID, SnapshotName, SnapshotModified string
	BackendIdentifier                                                                 string
	Phase                                                                             string
	IncludesRunningState                                                              bool
	Scope                                                                             checkpointScope
}

var (
	_ core.NativeCheckpointProvider             = Provider{}
	_ core.NativeCheckpointSourcePolicyProvider = Provider{}
	_ core.NativeCheckpointLifecycleProvider    = Provider{}
	_ core.NativeCheckpointRestoreProvider      = Provider{}
)

func (Provider) NativeCheckpointCapability(req core.NativeCheckpointRequest) (core.NativeCheckpointCapability, bool) {
	if strings.EqualFold(req.Strategy, core.CheckpointStrategyImage) {
		return core.NativeCheckpointCapability{}, false
	}
	return core.NativeCheckpointCapability{Kind: core.CheckpointKindUTM, Direct: true, RetireUnsupported: "UTM checkpoints live inside their source VM; --retire-source would delete the checkpoint; keep the lease until its checkpoints are no longer needed"}, true
}
func (Provider) NativeCheckpointSourceStatusOnly(core.Config) bool { return true }
func (Provider) NativeCheckpointWorkdir(req core.NativeCheckpointWorkdirRequest) string {
	if req.Override != "" {
		return req.Override
	}
	cfg := req.Config
	if root := req.Server.Labels["work_root"]; root != "" {
		cfg.WorkRoot = root
	}
	return core.RemoteJoin(cfg, req.LeaseID, req.RepoName)
}
func checkpointBackend(cfg core.Config, stderr io.Writer) (*backend, error) {
	configured, err := (Provider{}).Configure(cfg, core.RuntimeForProviderOperation(stderr))
	if err != nil {
		return nil, err
	}
	return configured.(*backend), nil
}
func (Provider) CreateNativeCheckpoint(ctx context.Context, req core.NativeCheckpointCreateRequest) (core.NativeCheckpointCreateResult, error) {
	b, err := checkpointBackend(req.Config, req.Stderr)
	if err != nil {
		return core.NativeCheckpointCreateResult{}, core.NativeCheckpointNotSubmittedError{Cause: err}
	}
	return b.captureCheckpoint(ctx, req)
}
func (b *backend) checkpointSourceVM(ctx context.Context, r record, stopped bool) (vm, error) {
	if r.State != "ready" || !r.Configured {
		return vm{}, fmt.Errorf("UTM checkpoint requires a completed retained lease (state=%s)", r.State)
	}
	vms, err := b.inventory(ctx)
	if err != nil {
		return vm{}, err
	}
	v, ok := findVM(vms, r.VMID)
	if !ok {
		return v, fmt.Errorf("UTM checkpoint source UUID is absent")
	}
	if err = validateClone(v, r); err != nil {
		return v, err
	}
	if stopped && v.Status != "stopped" {
		return v, fmt.Errorf("UTM checkpoint capture/restore requires stopped VM (status=%s); shut down the guest gracefully, then retry; Crabbox will not interrupt it", v.Status)
	}
	return v, nil
}

func (b *backend) checkpointSource(ctx context.Context, r record, stopped bool) (vm, checkpointScope, error) {
	v, err := b.checkpointSourceVM(ctx, r, stopped)
	if err != nil {
		return v, checkpointScope{}, err
	}
	scope, err := b.checkpointDiskScope(ctx, r, v)
	return v, scope, err
}
func (b *backend) checkpointClaim(r record, repoRoot string) (core.LeaseClaim, error) {
	claim, exists, err := core.ReadLeaseClaimWithPresence(r.LeaseID)
	if err != nil {
		return claim, err
	}
	if !exists || claim.Provider != "utm" || claim.CloudID != r.VMID || claim.ProviderScope != (Provider{}).ClaimScope(b.cfg) || claim.Revision == "" || claim.ClaimedAt == "" || repoRoot != "" && claim.RepoRoot != repoRoot {
		return claim, fmt.Errorf("UTM checkpoint requires the exact current repository source claim")
	}
	if hold := shared.ClaimActivityHoldState(claim); hold != "" || claim.CheckpointCapture != nil {
		return claim, fmt.Errorf("UTM checkpoint source activity is held by another operation")
	}
	return claim, nil
}
func (m checkpointMetadata) result(state string) core.NativeCheckpointCreateResult {
	data, _ := json.Marshal(m)
	id := m.SnapshotID
	if id == "" {
		id = "pending:" + m.SnapshotName
	}
	return core.NativeCheckpointCreateResult{Image: core.NativeCheckpointImage{ID: id, ResourceID: m.VMID, Name: m.SnapshotName, State: state, Provider: "utm", Kind: core.CheckpointKindUTM, Direct: true}, Metadata: map[string]string{checkpointMetadataKey: string(data)}}
}
func (b *backend) captureCheckpoint(ctx context.Context, req core.NativeCheckpointCreateRequest) (result core.NativeCheckpointCreateResult, err error) {
	submitted := false
	defer func() {
		if err != nil && !submitted {
			err = core.NativeCheckpointNotSubmittedError{Cause: err}
		}
	}()
	if req.Capture != nil {
		return result, fmt.Errorf("UTM checkpoints are lease-bound and cannot retire their source")
	}
	if req.Persist == nil {
		return result, fmt.Errorf("UTM capture requires durable checkpoint persistence")
	}
	if _, ok := (Provider{}).NativeCheckpointCapability(core.NativeCheckpointRequest{Strategy: req.Strategy}); !ok {
		return result, fmt.Errorf("UTM supports disk-snapshot strategy only")
	}
	unlock, err := b.lock(ctx)
	if err != nil {
		return result, err
	}
	defer unlock()
	r, err := b.lookup(req.LeaseID)
	if err != nil {
		return result, err
	}
	if req.Server.CloudID != r.VMID || req.Server.Provider != "utm" {
		return result, fmt.Errorf("UTM checkpoint source differs from ownership journal")
	}
	claim, exists, set := core.ServerLeaseClaimSnapshot(req.Server)
	if !set || !exists || claim.RepoRoot == "" {
		return result, fmt.Errorf("UTM checkpoint capture requires its admitted repository claim snapshot")
	}
	current, err := b.checkpointClaim(r, claim.RepoRoot)
	if err != nil {
		return result, err
	}
	if !reflect.DeepEqual(current, claim) {
		return result, fmt.Errorf("UTM checkpoint source claim changed after admission")
	}
	token, err := randomToken()
	if err != nil {
		return result, err
	}
	err = core.WithLeaseClaimUnchangedContext(ctx, r.LeaseID, claim, func() error {
		v, scope, err := b.checkpointSource(ctx, r, true)
		if err != nil {
			return err
		}
		version, host, err := b.checkpointHost(ctx, v.Backend)
		if err != nil {
			return err
		}
		// A fresh scan is rejected by UTM in disposable mode and when backend tools
		// are unavailable. The stopped-only contract never touches live overlays.
		before, err := b.snapshots(ctx, r.VMID, true)
		if err != nil {
			return err
		}
		m := checkpointMetadata{Version: 1, LeaseID: r.LeaseID, VMID: r.VMID, Token: r.Token, AppPath: r.AppPath, StorageRoot: b.cfg.UTM.StorageRoot, BundlePath: r.BundlePath, Backend: v.Backend, TargetOS: b.cfg.TargetOS, SourceScope: claim.ProviderScope, SourceClaimedAt: claim.ClaimedAt, SourceRevision: claim.Revision, RepoRoot: claim.RepoRoot, UTMVersion: version, HostVersion: host, CaptureState: v.Status, SnapshotName: "crabbox-" + r.LeaseID + "-" + token, Phase: "pending", Scope: scope}
		result = m.result("pending")
		if err = req.Persist(result); err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		submitted = true
		// Apple events can finish after caller cancellation. Recover the exact ID in
		// a bounded independent readback and persist it before honoring cancellation.
		captureCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		id, createErr := b.createSnapshot(captureCtx, r.VMID, m.SnapshotName)
		if _, existed := findSnapshot(before, id); existed {
			return fmt.Errorf("UTM capture returned a pre-existing snapshot UUID; no ownership accepted")
		}
		if id != "" {
			m.SnapshotID = id
			result = m.result("pending")
			if err = req.Persist(result); err != nil {
				return errors.Join(createErr, err)
			}
		}
		after, listErr := b.snapshots(captureCtx, r.VMID, true)
		if id == "" && listErr == nil {
			// Recovery discovers an ID only for a new exact opaque title. All later
			// effects use that UUID; there is never a mutation by display name.
			var found []nativeSnapshot
			for _, s := range after {
				_, existed := findSnapshot(before, s.ID)
				if !existed && s.Name == m.SnapshotName {
					found = append(found, s)
				}
			}
			if len(found) == 1 {
				id = found[0].ID
				m.SnapshotID = id
				result = m.result("pending")
				if err = req.Persist(result); err != nil {
					return errors.Join(createErr, err)
				}
			}
		}
		if createErr != nil || listErr != nil {
			return errors.Join(createErr, listErr)
		}
		s, ok := findSnapshot(after, id)
		if !ok {
			return fmt.Errorf("UTM snapshot capture did not establish exact ID; pending receipt retained")
		}
		m.BackendIdentifier, err = b.validateSnapshotData(captureCtx, r.BundlePath, s, "")
		if err != nil {
			return err
		}
		_, scopeAfter, err := b.checkpointSource(captureCtx, r, true)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(scope, scopeAfter) {
			return fmt.Errorf("UTM checkpoint disk scope changed during capture")
		}
		m.SnapshotName = s.Name
		m.IncludesRunningState = *s.IncludesRunningState
		m.SnapshotModified = s.Modified
		m.Phase = "available"
		result = m.result("available")
		// Caller cancellation retains captured identity but does not attest success.
		if err = ctx.Err(); err != nil {
			result.Image.State = "pending"
			m.Phase = "pending"
			result = m.result("pending")
			return errors.Join(err, req.Persist(result))
		}
		if err = req.Persist(result); err != nil {
			return err
		}
		result.CaptureEstablished = true
		return nil
	})
	return result, err
}
func checkpointMetadataFrom(req core.NativeCheckpointResourceRequest) (checkpointMetadata, error) {
	var m checkpointMetadata
	if req.Image.Provider != "utm" || req.Image.Kind != core.CheckpointKindUTM {
		return m, fmt.Errorf("not a UTM native checkpoint")
	}
	if err := json.Unmarshal([]byte(req.Metadata[checkpointMetadataKey]), &m); err != nil {
		return m, fmt.Errorf("decode UTM checkpoint ownership metadata: %w", err)
	}
	if m.Version != 1 || !validUUID(m.VMID) || m.LeaseID == "" || m.Token == "" || !filepath.IsAbs(m.AppPath) || !filepath.IsAbs(m.BundlePath) || !filepath.IsAbs(m.StorageRoot) || m.SourceClaimedAt == "" || m.SourceRevision == "" || m.SourceScope == "" || m.RepoRoot == "" || m.SnapshotName == "" || req.Image.ResourceID != m.VMID {
		return m, fmt.Errorf("UTM checkpoint lacks version 1 exact ownership/storage identity")
	}
	if m.Phase == "available" && m.BackendIdentifier == "" {
		return m, fmt.Errorf("UTM available checkpoint lacks exact backend state identity")
	}
	if m.SnapshotID != "" && (!validUUID(m.SnapshotID) || req.Image.ID != m.SnapshotID) {
		return m, fmt.Errorf("UTM checkpoint snapshot UUID differs from receipt")
	}
	if m.SnapshotID == "" && req.Image.ID != "pending:"+m.SnapshotName {
		return m, fmt.Errorf("UTM pending checkpoint identity differs from receipt")
	}
	return m, nil
}
func checkpointResourceBackend(req core.NativeCheckpointResourceRequest) (*backend, checkpointMetadata, error) {
	m, err := checkpointMetadataFrom(req)
	if err != nil {
		return nil, m, err
	}
	if req.LoadConfig == nil {
		return nil, m, fmt.Errorf("UTM checkpoint requires current provider configuration")
	}
	cfg, err := req.LoadConfig()
	if err != nil {
		return nil, m, err
	}
	cfg.Provider = "utm"
	cfg.TargetOS = m.TargetOS
	b, err := checkpointBackend(cfg, nil)
	if err != nil {
		return nil, m, err
	}
	if b.cfg.UTM.AppPath != m.AppPath || b.cfg.UTM.StorageRoot != m.StorageRoot {
		return nil, m, fmt.Errorf("UTM checkpoint application/storage scope differs from current configuration")
	}
	return b, m, nil
}
func (b *backend) resourceSource(m checkpointMetadata) (record, core.LeaseClaim, error) {
	r, err := b.lookup(m.LeaseID)
	if err != nil {
		return r, core.LeaseClaim{}, err
	}
	if r.VMID != m.VMID || r.Token != m.Token || r.BundlePath != m.BundlePath || r.AppPath != m.AppPath {
		return r, core.LeaseClaim{}, fmt.Errorf("UTM checkpoint source ownership journal changed")
	}
	claim, err := b.checkpointClaim(r, m.RepoRoot)
	if err != nil {
		return r, claim, err
	}
	if claim.ProviderScope != m.SourceScope || claim.ClaimedAt != m.SourceClaimedAt {
		return r, claim, fmt.Errorf("UTM checkpoint source claim generation changed")
	}
	return r, claim, nil
}
func (Provider) VerifyNativeCheckpoint(ctx context.Context, req core.NativeCheckpointResourceRequest) (core.NativeCheckpointVerifyResult, error) {
	b, m, err := checkpointResourceBackend(req)
	if err != nil {
		return core.NativeCheckpointVerifyResult{}, err
	}
	return b.verifyCheckpoint(ctx, req, m)
}
func (b *backend) verifyCheckpoint(ctx context.Context, req core.NativeCheckpointResourceRequest, m checkpointMetadata) (audit core.NativeCheckpointVerifyResult, err error) {
	unlock, err := b.lock(ctx)
	if err != nil {
		return audit, err
	}
	defer unlock()
	r, claim, err := b.resourceSource(m)
	if err != nil {
		return audit, err
	}
	err = core.WithLeaseClaimUnchangedContext(ctx, r.LeaseID, claim, func() error {
		v, scope, err := b.checkpointSource(ctx, r, false)
		if err != nil {
			return err
		}
		if v.Backend != m.Backend || !reflect.DeepEqual(scope, m.Scope) {
			return fmt.Errorf("UTM checkpoint backend or disk scope changed")
		}
		if _, _, err = b.checkpointHost(ctx, v.Backend); err != nil {
			return err
		}
		if !req.CaptureEstablished || m.SnapshotID == "" || m.Phase != "available" || req.Image.State != "available" {
			audit = core.NativeCheckpointVerifyResult{ProviderState: "pending", NextAction: "inspect_pending_capture", Error: "capture is not verified; retained receipt is not a complete checkpoint"}
			return nil
		}
		if v.Status != "stopped" {
			audit = core.NativeCheckpointVerifyResult{ProviderState: "unverified", NextAction: "stop_guest_then_verify", Error: "stop the guest gracefully to verify all native snapshot images"}
			return nil
		}
		all, err := b.snapshots(ctx, r.VMID, true)
		if err != nil {
			return err
		}
		s, ok := findSnapshot(all, m.SnapshotID)
		if !ok {
			audit = core.NativeCheckpointVerifyResult{ProviderState: "missing", NextAction: "delete_local"}
			return nil
		}
		if _, err = b.validateSnapshotData(ctx, r.BundlePath, s, m.BackendIdentifier); err != nil {
			audit = core.NativeCheckpointVerifyResult{ProviderState: "unavailable", NextAction: "delete_or_recreate", Error: err.Error()}
			return nil
		}
		if s.Modified != m.SnapshotModified || *s.IncludesRunningState != m.IncludesRunningState {
			return fmt.Errorf("UTM checkpoint was renamed or overwritten outside Crabbox; capture identity changed")
		}
		audit = core.NativeCheckpointVerifyResult{ProviderState: "available", NextAction: "restore_or_delete"}
		return nil
	})
	return audit, err
}
func (Provider) DeleteNativeCheckpoint(ctx context.Context, req core.NativeCheckpointResourceRequest) error {
	b, m, err := checkpointResourceBackend(req)
	if err != nil {
		return err
	}
	return b.deleteCheckpoint(ctx, req, m)
}
func (b *backend) deleteCheckpoint(ctx context.Context, req core.NativeCheckpointResourceRequest, m checkpointMetadata) error {
	if !validUUID(m.SnapshotID) {
		return fmt.Errorf("UTM pending capture has no returned snapshot UUID; retain receipt and reconcile exact snapshot identity before deletion")
	}
	unlock, err := b.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	r, claim, err := b.resourceSource(m)
	if err != nil {
		return err
	}
	return core.WithLeaseClaimUnchangedContext(ctx, r.LeaseID, claim, func() error {
		v, err := b.checkpointSourceVM(ctx, r, false)
		if err != nil {
			return err
		}
		if _, err = b.checkpointConfig(ctx, r, v); err != nil {
			return err
		}
		if v.Backend != m.Backend {
			return fmt.Errorf("UTM checkpoint backend changed")
		}
		if _, _, err = b.checkpointHost(ctx, v.Backend); err != nil {
			return err
		}
		all, err := b.snapshots(ctx, r.VMID, false)
		if err != nil {
			return err
		}
		if _, ok := findSnapshot(all, m.SnapshotID); !ok {
			return nil
		}
		// UTM's backend rejects live deletion of disk-only QEMU snapshots.
		if v.Status != "stopped" && v.Backend == "qemu" && !m.IncludesRunningState {
			return fmt.Errorf("UTM disk-only checkpoint deletion requires stopped QEMU VM; shut down guest and retry")
		}
		deleteErr := b.mutateSnapshot(ctx, "delete", r.VMID, m.SnapshotID)
		after, readErr := b.snapshots(ctx, r.VMID, false)
		if readErr != nil {
			return errors.Join(deleteErr, readErr)
		}
		if _, ok := findSnapshot(after, m.SnapshotID); ok {
			return errors.Join(deleteErr, fmt.Errorf("UTM snapshot deletion not confirmed; receipt retained for retry"))
		}
		return nil
	})
}
func (Provider) RestoreNativeCheckpoint(ctx context.Context, req core.NativeCheckpointRestoreRequest) (core.LeaseTarget, error) {
	b, err := checkpointBackend(req.Config, req.Stderr)
	if err != nil {
		return req.Lease, err
	}
	return b.restoreCheckpoint(ctx, req)
}
func (b *backend) restoreCheckpoint(ctx context.Context, req core.NativeCheckpointRestoreRequest) (lease core.LeaseTarget, err error) {
	lease = req.Lease
	m, err := checkpointMetadataFrom(req.Resource)
	if err != nil {
		return lease, err
	}
	if !req.Resource.CaptureEstablished || m.Phase != "available" || req.Resource.Image.State != "available" || !validUUID(m.SnapshotID) {
		return lease, fmt.Errorf("UTM checkpoint capture is not verified; pending receipt cannot be restored")
	}
	if b.cfg.UTM.AppPath != m.AppPath || b.cfg.UTM.StorageRoot != m.StorageRoot || req.Lease.LeaseID != m.LeaseID || req.Lease.Server.CloudID != m.VMID || req.Repo.Root != m.RepoRoot {
		return lease, fmt.Errorf("UTM restore must target the original owned lease and application/storage/repository scope")
	}
	unlock, err := b.lock(ctx)
	if err != nil {
		return lease, err
	}
	defer unlock()
	r, claim, err := b.resourceSource(m)
	if err != nil {
		return lease, err
	}
	expected, exists, set := core.ServerLeaseClaimSnapshot(req.Lease.Server)
	if !set || !exists || expected.Revision != claim.Revision {
		return lease, fmt.Errorf("UTM restore source claim changed after resolution")
	}
	err = core.WithLeaseClaimUnchangedContext(ctx, r.LeaseID, claim, func() error {
		v, scope, err := b.checkpointSource(ctx, r, true)
		if err != nil {
			return err
		}
		if v.Backend != m.Backend || !reflect.DeepEqual(scope, m.Scope) {
			return fmt.Errorf("UTM restore backend/disk scope differs from capture")
		}
		if _, _, err = b.checkpointHost(ctx, v.Backend); err != nil {
			return err
		}
		all, err := b.snapshots(ctx, r.VMID, !req.DryRun)
		if err != nil {
			return err
		}
		s, ok := findSnapshot(all, m.SnapshotID)
		if !ok {
			return fmt.Errorf("UTM exact snapshot UUID no longer exists")
		}
		if _, err = b.validateSnapshotData(ctx, r.BundlePath, s, m.BackendIdentifier); err != nil {
			return err
		}
		if s.Modified != m.SnapshotModified || *s.IncludesRunningState != m.IncludesRunningState {
			return fmt.Errorf("UTM snapshot was changed outside Crabbox; restore refused")
		}
		if req.DryRun {
			return nil
		}
		if err = b.mutateSnapshot(ctx, "restore", r.VMID, m.SnapshotID); err != nil {
			return err
		}
		manifest, err := b.snapshotManifest(ctx, r.BundlePath)
		if err != nil {
			return err
		}
		if !strings.EqualFold(manifest.CurrentParent, m.SnapshotID) {
			return fmt.Errorf("UTM did not establish the exact restored snapshot as current parent")
		}
		if _, _, err = b.checkpointSource(ctx, r, true); err != nil {
			return err
		}
		// Restore explicitly includes boot/resume and a fresh authenticated transport.
		// Acquisition, source sync and guest credential installation are never rerun.
		if err = b.control(ctx, "start", r.VMID); err != nil {
			return err
		}
		current, err := b.inventory(ctx)
		if err != nil {
			return err
		}
		restored, ok := findVM(current, r.VMID)
		if !ok || restored.Status != "started" {
			return fmt.Errorf("UTM restored VM did not reach started state")
		}
		if err = validateClone(restored, r); err != nil {
			return err
		}
		if v.Backend == "apple" {
			// The Apple acquisition adapter owns DHCP discovery. Retain the
			// current claim fence and refresh the endpoint without provisioning.
			refresher, ok := any(b).(interface {
				refreshMacOSAddress(context.Context, *record) error
			})
			if !ok {
				return fmt.Errorf("underlying UTM provider cannot refresh a restored Apple guest address")
			}
			if err = refresher.refreshMacOSAddress(ctx, &r); err != nil {
				return err
			}
		}
		lease = b.target(r, current)
		if err = waitForSSHReady(ctx, &lease.SSH, b.rt.Stderr, "UTM restored guest identity", b.cfg.UTM.StartupTimeout); err != nil {
			return fmt.Errorf("UTM snapshot restored, but verified SSH reconnection failed: %w", err)
		}
		core.SetServerLeaseClaimSnapshot(&lease.Server, claim, true)
		return nil
	})
	return lease, err
}
