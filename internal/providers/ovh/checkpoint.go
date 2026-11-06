package ovh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

// OVH snapshots carry no labels or tags, so ownership is proven by the local
// checkpoint record plus this deterministic snapshot name. A snapshot whose
// local record is lost is never deleted by Crabbox; list it and remove it by hand.
const (
	snapshotNamePrefix = "crabbox-"

	checkpointMetadataCheckpoint     = "checkpoint"
	checkpointMetadataLease          = "lease"
	checkpointMetadataSourceInstance = "source_instance"
	checkpointMetadataSourceProject  = "source_project"
	checkpointMetadataSourceRegion   = "source_region"
	checkpointMetadataSourceType     = "source_type"
	checkpointSourceTypeInstance     = "instance"

	ovhRetirementUnsupported = "OVH source retirement is unsupported; keep the source and use ordinary snapshots"
)

type snapshotClient interface {
	GetInstance(context.Context, string, string) (Instance, error)
	CreateInstanceSnapshot(context.Context, string, string, string) (string, error)
	ListSnapshots(context.Context, string, string) ([]Image, error)
	GetSnapshot(context.Context, string, string) (Image, error)
	DeleteSnapshot(context.Context, string, string) error
}

var (
	_ core.NativeCheckpointProvider          = Provider{}
	_ core.NativeCheckpointLifecycleProvider = Provider{}
	_ core.NativeCheckpointForkProvider      = Provider{}

	newSnapshotClient = func(cfg core.Config, rt core.Runtime) (snapshotClient, error) {
		return newClient(cfg, rt)
	}
	prepareCheckpointSource = core.PrepareNativeImageSource
	// snapshotDiscoveryTimeout bounds how long create waits for OVH to list a
	// snapshot it accepted without returning its id.
	snapshotDiscoveryTimeout = 2 * time.Minute
	checkpointPollInterval   = 15 * time.Second
	checkpointNow            = time.Now
	checkpointSleep          = func(ctx context.Context, delay time.Duration) error {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}
)

func snapshotName(checkpointID string) string { return snapshotNamePrefix + checkpointID }

func (Provider) NativeCheckpointCapability(req core.NativeCheckpointRequest) (core.NativeCheckpointCapability, bool) {
	if shared.FirstNonBlank(req.Target.TargetOS, req.Config.TargetOS) != core.TargetLinux || strings.TrimSpace(req.Server.CloudID) == "" {
		return core.NativeCheckpointCapability{}, false
	}
	capability := core.NativeCheckpointCapability{Kind: core.CheckpointKindOVH, Direct: true, RetireUnsupported: ovhRetirementUnsupported}
	if core.NormalizeCheckpointStrategy(req.Strategy) == core.CheckpointStrategyImage {
		capability.CreateUnsupported = "OVH native checkpoints use project snapshots; --strategy image is unsupported, use --strategy disk-snapshot"
	}
	return capability, true
}

func (Provider) NativeCheckpointWorkdir(req core.NativeCheckpointWorkdirRequest) string {
	if override := strings.TrimSpace(req.Override); override != "" {
		return override
	}
	cfg := req.Config
	if workRoot := strings.TrimSpace(req.Server.Labels["work_root"]); workRoot != "" {
		cfg.WorkRoot = workRoot
	}
	return core.RemoteJoin(cfg, req.LeaseID, req.RepoName)
}

func (Provider) CreateNativeCheckpoint(ctx context.Context, req core.NativeCheckpointCreateRequest) (_ core.NativeCheckpointCreateResult, err error) {
	if req.Capture != nil {
		return core.NativeCheckpointCreateResult{}, core.Exit(2, "%s", ovhRetirementUnsupported)
	}
	submissionStarted := false
	defer func() {
		if err != nil && !submissionStarted {
			err = core.NativeCheckpointNotSubmittedError{Cause: err}
		}
	}()
	if shared.FirstNonBlank(req.Target.TargetOS, req.Config.TargetOS) != core.TargetLinux {
		return core.NativeCheckpointCreateResult{}, core.Exit(2, "OVH native checkpoints require a Linux lease")
	}
	if core.NormalizeCheckpointStrategy(req.Strategy) == core.CheckpointStrategyImage {
		return core.NativeCheckpointCreateResult{}, core.Exit(2, "OVH native checkpoints use project snapshots; --strategy image is unsupported, use --strategy disk-snapshot")
	}
	checkpointID := strings.TrimSpace(req.CheckpointID)
	if !validCheckpointID(checkpointID) {
		return core.NativeCheckpointCreateResult{}, core.Exit(2, "OVH snapshot creation requires a canonical checkpoint ID")
	}
	project := strings.TrimSpace(req.Config.OVH.ProjectID)
	if project == "" {
		return core.NativeCheckpointCreateResult{}, core.Exit(3, "CRABBOX_OVH_PROJECT_ID or ovh.projectId is required")
	}
	client, err := newSnapshotClient(req.Config, req.Runtime)
	if err != nil {
		return core.NativeCheckpointCreateResult{}, err
	}
	instanceID := strings.TrimSpace(req.Server.CloudID)
	live, err := client.GetInstance(ctx, project, instanceID)
	if err != nil {
		return core.NativeCheckpointCreateResult{}, fmt.Errorf("re-read OVH checkpoint source instance %s: %w", instanceID, err)
	}
	claim, region, err := requireExactCheckpointSource(live, req.Server, req.Config, req.LeaseID, instanceID)
	if err != nil {
		return core.NativeCheckpointCreateResult{}, err
	}
	name := snapshotName(checkpointID)
	existing, err := client.ListSnapshots(ctx, project, region)
	if err != nil {
		return core.NativeCheckpointCreateResult{}, err
	}
	if len(snapshotsNamed(existing, name)) > 0 {
		return core.NativeCheckpointCreateResult{}, core.Exit(2, "OVH snapshot name %q already exists; refusing to adopt it", name)
	}
	metadata := map[string]string{
		checkpointMetadataCheckpoint:     checkpointID,
		checkpointMetadataLease:          req.LeaseID,
		checkpointMetadataSourceInstance: instanceID,
		checkpointMetadataSourceProject:  project,
		checkpointMetadataSourceRegion:   region,
		checkpointMetadataSourceType:     checkpointSourceTypeInstance,
	}
	var snapshotID string
	if err := core.WithLeaseClaimUnchanged(req.LeaseID, claim, func() (createErr error) {
		if err := prepareCheckpointSource(ctx, req.Target); err != nil {
			return err
		}
		// From this call onward, even a lost response retains custody.
		submissionStarted = true
		snapshotID, createErr = client.CreateInstanceSnapshot(ctx, project, instanceID, name)
		return createErr
	}); err != nil {
		if !submissionStarted {
			return core.NativeCheckpointCreateResult{}, err
		}
		// A definitive client-error answer means OVH refused the request, so no
		// snapshot exists and the source reservation can be dropped.
		if isDeterminateCreateError(err) {
			submissionStarted = false
			return core.NativeCheckpointCreateResult{}, err
		}
		// A lost response may still have created the snapshot. The name is
		// deterministic, so wait for it to be listed and journal what exists.
		if recovered, recoverErr := discoverSnapshot(ctx, client, project, region, name); recoverErr == nil {
			result := checkpointResult(recovered, region, metadata)
			if req.Persist != nil {
				err = errors.Join(err, req.Persist(result))
			}
			return result, err
		}
		return core.NativeCheckpointCreateResult{}, err
	}
	// Journal the id OVH returned before any follow-up read can fail, so the
	// billed snapshot stays recoverable and deletable.
	if snapshotID != "" && req.Persist != nil {
		provisional := checkpointResult(Image{ID: snapshotID, Name: name, Status: "unknown"}, region, metadata)
		if err := req.Persist(provisional); err != nil {
			return provisional, err
		}
	}
	snapshot, err := locateSnapshot(ctx, client, project, region, name, snapshotID)
	if err != nil {
		if snapshotID != "" {
			return checkpointResult(Image{ID: snapshotID, Name: name, Status: "unknown"}, region, metadata), err
		}
		return core.NativeCheckpointCreateResult{}, err
	}
	result := checkpointResult(snapshot, region, metadata)
	// Keep the exact image recoverable while readiness polling is in flight.
	if req.Persist != nil {
		if err := req.Persist(result); err != nil {
			return result, err
		}
	}
	if !req.Wait {
		return result, nil
	}
	waited, err := waitForSnapshot(ctx, client, project, snapshot, req.WaitTimeout, req.Stderr)
	result = checkpointResult(waited, region, metadata)
	return result, err
}

func (Provider) VerifyNativeCheckpoint(ctx context.Context, req core.NativeCheckpointResourceRequest) (core.NativeCheckpointVerifyResult, error) {
	client, project, err := resourceClient(req)
	if err != nil {
		return core.NativeCheckpointVerifyResult{}, err
	}
	snapshot, err := loadAndValidateSnapshot(ctx, client, project, req)
	if err != nil {
		return core.NativeCheckpointVerifyResult{}, err
	}
	state := strings.TrimSpace(snapshot.Status)
	next := "wait_or_delete"
	if snapshotAvailable(state) {
		next = "fork_or_delete"
	} else if snapshotFailed(state) {
		next = "delete"
	}
	return core.NativeCheckpointVerifyResult{ProviderState: core.Blank(state, "unknown"), NextAction: next}, nil
}

func (Provider) DeleteNativeCheckpoint(ctx context.Context, req core.NativeCheckpointResourceRequest) error {
	client, project, err := resourceClient(req)
	if err != nil {
		return err
	}
	snapshot, err := loadAndValidateSnapshot(ctx, client, project, req)
	if err != nil {
		return err
	}
	if err := client.DeleteSnapshot(ctx, project, snapshot.ID); err != nil {
		if isOVHNotFound(err) {
			return core.Exit(2, "OVH snapshot %s disappeared before deletion; refusing to remove local metadata because project identity cannot be proven", snapshot.ID)
		}
		return err
	}
	return nil
}

func (Provider) ApplyNativeCheckpointForkConfig(req core.NativeCheckpointForkRequest) error {
	if req.Record.Kind != core.CheckpointKindOVH || !req.Record.Direct {
		return core.Exit(2, "provider=ovh does not support checkpoint kind=%s", req.Record.Kind)
	}
	if strings.TrimSpace(req.Record.ImageID) == "" {
		return core.Exit(2, "OVH checkpoint record has no snapshot ID")
	}
	if req.Record.Metadata[checkpointMetadataSourceType] != checkpointSourceTypeInstance {
		return core.Exit(2, "OVH checkpoint source type is missing or unsupported")
	}
	project := strings.TrimSpace(req.Config.OVH.ProjectID)
	if project == "" || req.Record.Metadata[checkpointMetadataSourceProject] != project {
		return core.Exit(2, "OVH checkpoint was captured in a different Public Cloud project; set ovh.projectId to the source project")
	}
	if req.Record.Region == "" || req.Record.Metadata[checkpointMetadataSourceRegion] != req.Record.Region {
		return core.Exit(2, "OVH checkpoint source region is missing or mismatched")
	}
	req.Config.OVH.Image = req.Record.ImageID
	core.SetOVHImageExplicit(req.Config)
	req.Config.OVH.Region = req.Record.Region
	return nil
}

func resourceClient(req core.NativeCheckpointResourceRequest) (snapshotClient, string, error) {
	if req.LoadConfig == nil {
		return nil, "", core.Exit(2, "OVH checkpoint operations require the current CLI configuration")
	}
	cfg, err := req.LoadConfig()
	if err != nil {
		return nil, "", err
	}
	project := strings.TrimSpace(cfg.OVH.ProjectID)
	if project == "" || project != req.Metadata[checkpointMetadataSourceProject] {
		return nil, "", core.Exit(2, "refusing to operate on an OVH checkpoint from a different Public Cloud project")
	}
	client, err := newSnapshotClient(cfg, req.Runtime)
	return client, project, err
}

func requireExactCheckpointSource(live Instance, carried core.Server, cfg core.Config, leaseID, instanceID string) (core.LeaseClaim, string, error) {
	claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		return core.LeaseClaim{}, "", fmt.Errorf("read ovh lease claim: %w", err)
	}
	if !exists {
		return core.LeaseClaim{}, "", core.Exit(2, "ovh lease=%s has no exact local claim; refusing snapshot creation", leaseID)
	}
	// The claim generation admitted with the request must still be the one on
	// disk; a lease reclaimed by another checkout is a different generation.
	if expected, expectedExists, set := core.ServerLeaseClaimSnapshot(carried); set && (!expectedExists || !reflect.DeepEqual(expected, claim)) {
		return core.LeaseClaim{}, "", fmt.Errorf("%w: lease %s claim changed; refusing snapshot creation", core.ErrReleaseLeaseOwnershipChanged, leaseID)
	}
	server := serverFromInstance(mergeInstanceLabels(live, claim.Labels), cfg)
	if err := validateOVHServerOwnership(server); err != nil {
		return core.LeaseClaim{}, "", err
	}
	if err := validateOVHClaim(claim, server); err != nil {
		return core.LeaseClaim{}, "", err
	}
	if live.ID != instanceID || server.Labels["lease"] != leaseID || live.Name != core.LeaseProviderName(leaseID, claim.Slug) {
		return core.LeaseClaim{}, "", core.Exit(2, "refusing OVH snapshot creation for source lease mismatch")
	}
	if server.Labels[ovhProjectLabel] != strings.TrimSpace(cfg.OVH.ProjectID) {
		return core.LeaseClaim{}, "", core.Exit(3, "ovh project mismatch for lease=%s", leaseID)
	}
	if tailscaleEnrolled(claim) {
		return core.LeaseClaim{}, "", core.Exit(2, "OVH native checkpoints cannot capture a Tailscale-enrolled lease: the snapshot would carry the source's tailnet identity and a fork would contend for it; create the checkpoint from a lease without --tailscale")
	}
	region := strings.TrimSpace(live.Region)
	if region == "" {
		return core.LeaseClaim{}, "", core.Exit(2, "OVH source instance %s has no region", instanceID)
	}
	return claim, region, nil
}

// tailscaleEnrolled reports whether the lease joined, or was asked to join, a
// tailnet. Guest preparation does not reset Tailscale state, so such a source
// is refused rather than captured with its identity.
func tailscaleEnrolled(claim core.LeaseClaim) bool {
	return strings.EqualFold(strings.TrimSpace(claim.Labels["tailscale"]), "true") ||
		strings.TrimSpace(claim.Labels["tailscale_state"]) != "" ||
		strings.TrimSpace(claim.TailscaleIPv4) != "" ||
		strings.TrimSpace(claim.TailscaleFQDN) != ""
}

func snapshotsNamed(snapshots []Image, name string) []Image {
	var matches []Image
	for _, snapshot := range snapshots {
		if snapshot.Name == name {
			matches = append(matches, snapshot)
		}
	}
	return matches
}

// locateSnapshot prefers the id OVH returned from the create call and checks it
// against our name; without an id it falls back to the unique name.
func locateSnapshot(ctx context.Context, client snapshotClient, project, region, name, id string) (Image, error) {
	if id == "" {
		return discoverSnapshot(ctx, client, project, region, name)
	}
	snapshot, err := client.GetSnapshot(ctx, project, id)
	if err != nil {
		return Image{}, fmt.Errorf("read OVH snapshot %s after create: %w", id, err)
	}
	if snapshot.ID != id || snapshot.Name != name {
		return Image{}, core.Exit(5, "OVH snapshot %s does not match requested name %q; find it in the project and delete it by hand", id, name)
	}
	return snapshot, nil
}

// discoverSnapshot finds the snapshot OVH accepted by its unique Crabbox name.
func discoverSnapshot(ctx context.Context, client snapshotClient, project, region, name string) (Image, error) {
	deadline := checkpointNow().Add(snapshotDiscoveryTimeout)
	for {
		remaining := deadline.Sub(checkpointNow())
		if remaining <= 0 {
			return Image{}, core.Exit(5, "OVH did not list snapshot %q after accepting it; find it in the project and delete it by hand", name)
		}
		listCtx, cancel := context.WithTimeout(ctx, remaining)
		snapshots, err := client.ListSnapshots(listCtx, project, region)
		cancel()
		if err != nil {
			return Image{}, err
		}
		switch matches := snapshotsNamed(snapshots, name); len(matches) {
		case 1:
			if matches[0].ID == "" {
				return Image{}, core.Exit(5, "OVH returned snapshot %q without an ID", name)
			}
			return matches[0], nil
		case 0:
		default:
			return Image{}, core.Exit(5, "OVH lists %d snapshots named %q; refusing to guess which is ours", len(matches), name)
		}
		if !checkpointNow().Before(deadline) {
			return Image{}, core.Exit(5, "OVH did not list snapshot %q after accepting it; find it in the project and delete it by hand", name)
		}
		if err := checkpointSleep(ctx, min(checkpointPollInterval, deadline.Sub(checkpointNow()))); err != nil {
			return Image{}, err
		}
	}
}

func waitForSnapshot(ctx context.Context, client snapshotClient, project string, initial Image, timeout time.Duration, stderr io.Writer) (Image, error) {
	last := initial
	deadline := checkpointNow().Add(timeout)
	timedOut := func() error {
		return core.Exit(5, "timed out waiting for OVH snapshot %s; last state=%s", last.ID, core.Blank(last.Status, "unknown"))
	}
	for {
		// Each read is bounded by what is left of the wait, so a slow API call
		// cannot outlive --wait-timeout and a late answer is never accepted.
		remaining := deadline.Sub(checkpointNow())
		if remaining <= 0 {
			return last, timedOut()
		}
		reqCtx, cancel := context.WithTimeout(ctx, remaining)
		current, err := client.GetSnapshot(reqCtx, project, last.ID)
		expired := reqCtx.Err() != nil && ctx.Err() == nil
		cancel()
		if err != nil {
			if expired {
				return last, timedOut()
			}
			return last, err
		}
		if current.Name == "" {
			current.Name = last.Name
		}
		last = current
		state := strings.ToLower(strings.TrimSpace(last.Status))
		if snapshotAvailable(state) {
			return last, nil
		}
		if snapshotFailed(state) {
			return last, core.Exit(5, "OVH snapshot %s failed; last state=%s", last.ID, core.Blank(last.Status, "unknown"))
		}
		if !checkpointNow().Before(deadline) {
			return last, timedOut()
		}
		if stderr != nil {
			fmt.Fprintf(stderr, "waiting image=%s state=%s\n", last.ID, core.Blank(last.Status, "saving"))
		}
		if err := checkpointSleep(ctx, min(checkpointPollInterval, deadline.Sub(checkpointNow()))); err != nil {
			return last, err
		}
	}
}

func loadAndValidateSnapshot(ctx context.Context, client snapshotClient, project string, req core.NativeCheckpointResourceRequest) (Image, error) {
	id := strings.TrimSpace(req.Image.ID)
	if id == "" {
		// The create response was lost before OVH named the snapshot. Never guess
		// a delete target from the name alone. Core holds such a record (and its
		// source) as an unresolved capture, exactly as for other native providers.
		return Image{}, core.Exit(2, "OVH checkpoint record has no snapshot ID (the create response was lost); it stays held as an unresolved capture. Look for snapshot %q in region %s in the OVH console; Crabbox will not delete a snapshot it cannot identify by id", snapshotName(req.Metadata[checkpointMetadataCheckpoint]), core.Blank(req.Image.Region, "unknown"))
	}
	snapshot, err := client.GetSnapshot(ctx, project, id)
	if err != nil {
		if isOVHNotFound(err) {
			return Image{}, core.Exit(2, "OVH snapshot %s was not found; refusing to infer absence because project identity cannot be proven", id)
		}
		return Image{}, err
	}
	if err := validateOwnedSnapshot(req, snapshot); err != nil {
		return Image{}, err
	}
	return snapshot, nil
}

func validateOwnedSnapshot(req core.NativeCheckpointResourceRequest, snapshot Image) error {
	if req.Image.Kind != core.CheckpointKindOVH || req.Image.Provider != providerName || !req.Image.Direct {
		return core.Exit(2, "refusing to operate on a non-direct OVH checkpoint record")
	}
	if snapshot.ID != strings.TrimSpace(req.Image.ID) {
		return core.Exit(2, "refusing to operate on a mismatched OVH snapshot identity")
	}
	checkpointID := req.Metadata[checkpointMetadataCheckpoint]
	if !validCheckpointID(checkpointID) || snapshot.Name != snapshotName(checkpointID) {
		return core.Exit(2, "refusing to operate on OVH snapshot %s with mismatched name", snapshot.ID)
	}
	if req.Metadata[checkpointMetadataLease] == "" || req.Metadata[checkpointMetadataSourceInstance] == "" {
		return core.Exit(2, "OVH checkpoint record is missing ownership or source binding metadata")
	}
	region := req.Image.Region
	if region == "" || region != req.Metadata[checkpointMetadataSourceRegion] || (snapshot.Region != "" && snapshot.Region != region) {
		return core.Exit(2, "refusing to operate on OVH snapshot %s with mismatched region", snapshot.ID)
	}
	return nil
}

func checkpointResult(snapshot Image, region string, metadata map[string]string) core.NativeCheckpointCreateResult {
	return core.NativeCheckpointCreateResult{
		CaptureEstablished: snapshotAvailable(snapshot.Status),
		Image: core.NativeCheckpointImage{
			ID:         snapshot.ID,
			Name:       snapshot.Name,
			State:      snapshot.Status,
			Provider:   providerName,
			Kind:       core.CheckpointKindOVH,
			Region:     region,
			ResourceID: snapshot.ID,
			Direct:     true,
		},
		Metadata: shared.CloneLabels(metadata),
	}
}

func snapshotAvailable(state string) bool {
	return strings.EqualFold(strings.TrimSpace(state), "active")
}

func snapshotFailed(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "error", "failed", "killed", "deleted", "deactivated", "pending_delete":
		return true
	default:
		return false
	}
}

func validCheckpointID(value string) bool {
	if !strings.HasPrefix(value, "chk_") || len(value) <= len("chk_") {
		return false
	}
	for _, r := range value[len("chk_"):] {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

// validateForkSource checks the checkpoint record against the live snapshot
// before a fork allocates anything, so a renamed, moved, or foreign snapshot
// is refused with the same predicate verify and delete use.
func validateForkSource(ctx context.Context, client API, project string, record core.NativeCheckpointForkRecord) error {
	lookup, ok := client.(snapshotClient)
	if !ok {
		return core.Exit(2, "ovh client cannot verify checkpoint snapshots")
	}
	if record.Kind != core.CheckpointKindOVH || !record.Direct {
		return core.Exit(2, "provider=ovh does not support checkpoint kind=%s", record.Kind)
	}
	if record.Metadata[checkpointMetadataSourceProject] != project {
		return core.Exit(2, "OVH checkpoint was captured in a different Public Cloud project; set ovh.projectId to the source project")
	}
	snapshot, err := lookup.GetSnapshot(ctx, project, strings.TrimSpace(record.ImageID))
	if err != nil {
		if isOVHNotFound(err) {
			return core.Exit(2, "OVH snapshot %s was not found; refusing to fork", record.ImageID)
		}
		return err
	}
	resource := core.NativeCheckpointResourceRequest{
		Image:    core.NativeCheckpointImage{ID: record.ImageID, Provider: providerName, Kind: record.Kind, Region: record.Region, Direct: record.Direct},
		Metadata: record.Metadata,
	}
	if err := validateOwnedSnapshot(resource, snapshot); err != nil {
		return err
	}
	if !snapshotAvailable(snapshot.Status) {
		return core.Exit(2, "OVH snapshot %s is %s, not active; wait for it or verify the checkpoint", snapshot.ID, core.Blank(snapshot.Status, "in an unknown state"))
	}
	return nil
}
