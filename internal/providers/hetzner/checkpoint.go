package hetzner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const (
	checkpointMetadataCheckpoint         = "checkpoint"
	checkpointMetadataLease              = "lease"
	checkpointMetadataSourceServer       = "source_server"
	checkpointMetadataSourceLocation     = "source_location"
	checkpointMetadataSourceArchitecture = "source_architecture"
	checkpointMetadataSourceType         = "source_type"
)

type hetznerSnapshotClient interface {
	GetServer(context.Context, int64) (Server, error)
	CreateServerSnapshot(context.Context, int64, string, map[string]string) (core.HetznerImage, error)
	GetImage(context.Context, int64) (core.HetznerImage, error)
	DeleteImage(context.Context, int64) error
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

func (Provider) CreateNativeCheckpoint(ctx context.Context, req core.NativeCheckpointCreateRequest) (core.NativeCheckpointCreateResult, error) {
	if strings.TrimSpace(req.Config.Coordinator) != "" {
		return core.NativeCheckpointCreateResult{}, core.Exit(2, "brokered Hetzner leases use archive checkpoints")
	}
	if firstNonBlank(req.Target.TargetOS, req.Config.TargetOS) != core.TargetLinux {
		return core.NativeCheckpointCreateResult{}, core.Exit(2, "Hetzner native checkpoints require a Linux lease")
	}
	if core.NormalizeCheckpointStrategy(req.Strategy) == core.CheckpointStrategyImage {
		return core.NativeCheckpointCreateResult{}, core.Exit(2, "Hetzner native checkpoints use project snapshots; --strategy image is unsupported, use --strategy disk-snapshot")
	}
	checkpointID := strings.TrimSpace(req.CheckpointID)
	if !validCheckpointID(checkpointID) {
		return core.NativeCheckpointCreateResult{}, core.Exit(2, "Hetzner snapshot creation requires a canonical checkpoint ID")
	}
	serverID, err := parsePositiveID(req.Server.CloudID, "server")
	if err != nil {
		return core.NativeCheckpointCreateResult{}, err
	}
	client, err := newHetznerSnapshotClient()
	if err != nil {
		return core.NativeCheckpointCreateResult{}, err
	}
	source, err := client.GetServer(ctx, serverID)
	if err != nil {
		return core.NativeCheckpointCreateResult{}, fmt.Errorf("re-read Hetzner checkpoint source server %d: %w", serverID, err)
	}
	source = normalizeHetznerServer(source)
	if source.ID != serverID || source.CloudID != strconv.FormatInt(serverID, 10) {
		return core.NativeCheckpointCreateResult{}, core.Exit(2, "refusing Hetzner snapshot creation from mismatched source server identity")
	}
	if err := validateHetznerServerOwnership(source, false); err != nil {
		return core.NativeCheckpointCreateResult{}, err
	}
	if source.Labels["lease"] != req.LeaseID {
		return core.NativeCheckpointCreateResult{}, core.Exit(2, "refusing Hetzner snapshot creation for source lease mismatch")
	}
	claim, err := requireExactHetznerClaim(source, req.LeaseID)
	if err != nil {
		return core.NativeCheckpointCreateResult{}, err
	}
	location := ""
	if source.Location != nil {
		location = strings.TrimSpace(source.Location.Name)
	}
	if location == "" {
		return core.NativeCheckpointCreateResult{}, core.Exit(2, "Hetzner source server %d has no location", serverID)
	}
	architecture, err := hetznerServerArchitecture(source)
	if err != nil {
		return core.NativeCheckpointCreateResult{}, err
	}
	metadata := map[string]string{
		checkpointMetadataCheckpoint:         checkpointID,
		checkpointMetadataLease:              req.LeaseID,
		checkpointMetadataSourceServer:       strconv.FormatInt(serverID, 10),
		checkpointMetadataSourceLocation:     location,
		checkpointMetadataSourceArchitecture: architecture,
		checkpointMetadataSourceType:         "server",
	}
	labels := checkpointLabels(metadata)
	description := strings.TrimSpace(req.Name)
	if description == "" {
		description = "Crabbox checkpoint " + checkpointID
	}
	var snapshot core.HetznerImage
	if err := core.WithLeaseClaimUnchanged(req.LeaseID, claim, func() error {
		if err := prepareHetznerCheckpointSource(ctx, req.Target); err != nil {
			return err
		}
		created, err := client.CreateServerSnapshot(ctx, serverID, description, labels)
		if err != nil {
			return err
		}
		snapshot = created
		return nil
	}); err != nil {
		return core.NativeCheckpointCreateResult{}, err
	}
	if snapshot.ID <= 0 {
		return core.NativeCheckpointCreateResult{}, core.Exit(5, "Hetzner returned no snapshot image ID")
	}
	if snapshot.Architecture == "" {
		snapshot.Architecture = architecture
	}
	result := hetznerCheckpointResult(snapshot, location, metadata)
	if err := validateCreatedHetznerSnapshot(snapshot, architecture); err != nil {
		return result, err
	}
	if !req.Wait {
		return result, nil
	}
	waited, err := waitForHetznerSnapshot(ctx, client, snapshot, req.WaitTimeout, req.Stderr)
	result = hetznerCheckpointResult(waited, location, metadata)
	if err != nil {
		return result, err
	}
	if err := validateCreatedHetznerSnapshot(waited, architecture); err != nil {
		return result, err
	}
	return result, nil
}

func (Provider) VerifyNativeCheckpoint(ctx context.Context, req core.NativeCheckpointResourceRequest) (core.NativeCheckpointVerifyResult, error) {
	client, err := newHetznerSnapshotClient()
	if err != nil {
		return core.NativeCheckpointVerifyResult{}, err
	}
	snapshot, err := loadAndValidateHetznerSnapshot(ctx, client, req)
	if err != nil {
		return core.NativeCheckpointVerifyResult{}, err
	}
	state := strings.TrimSpace(snapshot.Status)
	next := "wait_or_delete"
	if strings.EqualFold(state, "available") {
		next = "fork_or_delete"
	} else if failedHetznerSnapshotState(state) {
		next = "delete"
	}
	return core.NativeCheckpointVerifyResult{ProviderState: blank(state, "unknown"), NextAction: next}, nil
}

func (Provider) DeleteNativeCheckpoint(ctx context.Context, req core.NativeCheckpointResourceRequest) error {
	client, err := newHetznerSnapshotClient()
	if err != nil {
		return err
	}
	snapshot, err := loadAndValidateHetznerSnapshot(ctx, client, req)
	if err != nil {
		return err
	}
	if err := client.DeleteImage(ctx, snapshot.ID); err != nil {
		if core.IsHetznerNotFound(err) {
			return core.Exit(2, "Hetzner snapshot %d disappeared before deletion; refusing to remove local metadata because project identity cannot be proven", snapshot.ID)
		}
		return err
	}
	return nil
}

func loadAndValidateHetznerSnapshot(ctx context.Context, client hetznerSnapshotClient, req core.NativeCheckpointResourceRequest) (core.HetznerImage, error) {
	imageID, err := parsePositiveID(req.Image.ID, "snapshot")
	if err != nil {
		return core.HetznerImage{}, err
	}
	snapshot, err := client.GetImage(ctx, imageID)
	if err != nil {
		if core.IsHetznerNotFound(err) {
			return core.HetznerImage{}, core.Exit(2, "Hetzner snapshot %d was not found; refusing to infer absence because project identity cannot be proven", imageID)
		}
		return core.HetznerImage{}, err
	}
	if err := validateHetznerSnapshot(req, snapshot); err != nil {
		return core.HetznerImage{}, err
	}
	return snapshot, nil
}

func validateHetznerSnapshot(req core.NativeCheckpointResourceRequest, snapshot core.HetznerImage) error {
	if req.Image.Kind != core.CheckpointKindHetzner || req.Image.Provider != providerName || !req.Image.Direct {
		return core.Exit(2, "refusing to operate on a non-direct Hetzner checkpoint record")
	}
	if snapshot.Type != "snapshot" {
		return core.Exit(2, "refusing to operate on Hetzner image %d with type=%s", snapshot.ID, blank(snapshot.Type, "unknown"))
	}
	if strconv.FormatInt(snapshot.ID, 10) != strings.TrimSpace(req.Image.ID) {
		return core.Exit(2, "refusing to operate on a mismatched Hetzner snapshot identity")
	}
	expected := checkpointLabels(req.Metadata)
	if len(expected) != 10 {
		return core.Exit(2, "Hetzner checkpoint record is missing ownership or source binding metadata")
	}
	for key, value := range expected {
		if value == "" || snapshot.Labels[key] != value {
			return core.Exit(2, "refusing to operate on Hetzner snapshot %d with mismatched %s label", snapshot.ID, key)
		}
	}
	if req.Image.Region == "" || req.Image.Region != req.Metadata[checkpointMetadataSourceLocation] {
		return core.Exit(2, "refusing to operate on Hetzner snapshot %d with mismatched source location", snapshot.ID)
	}
	if req.Image.Architecture == "" || req.Image.Architecture != req.Metadata[checkpointMetadataSourceArchitecture] || snapshot.Architecture != req.Image.Architecture {
		return core.Exit(2, "refusing to operate on Hetzner snapshot %d with mismatched architecture", snapshot.ID)
	}
	if snapshot.CreatedFrom != nil && strconv.FormatInt(snapshot.CreatedFrom.ID, 10) != req.Metadata[checkpointMetadataSourceServer] {
		return core.Exit(2, "refusing to operate on Hetzner snapshot %d with mismatched source server", snapshot.ID)
	}
	return nil
}

func checkpointLabels(metadata map[string]string) map[string]string {
	if metadata == nil {
		return nil
	}
	return map[string]string{
		"crabbox":             "true",
		"created_by":          "crabbox",
		"provider":            providerName,
		"kind":                "checkpoint",
		"checkpoint":          metadata[checkpointMetadataCheckpoint],
		"lease":               metadata[checkpointMetadataLease],
		"source_server":       metadata[checkpointMetadataSourceServer],
		"source_location":     metadata[checkpointMetadataSourceLocation],
		"source_architecture": metadata[checkpointMetadataSourceArchitecture],
		"source_type":         metadata[checkpointMetadataSourceType],
	}
}

func hetznerCheckpointResult(snapshot core.HetznerImage, location string, metadata map[string]string) core.NativeCheckpointCreateResult {
	return core.NativeCheckpointCreateResult{
		Image: core.NativeCheckpointImage{
			ID:           strconv.FormatInt(snapshot.ID, 10),
			Name:         firstNonBlank(snapshot.Description, snapshot.Name),
			State:        snapshot.Status,
			Provider:     providerName,
			Kind:         core.CheckpointKindHetzner,
			Region:       location,
			ResourceID:   strconv.FormatInt(snapshot.ID, 10),
			Architecture: snapshot.Architecture,
			Direct:       true,
		},
		Metadata: cloneMetadata(metadata),
	}
}

func waitForHetznerSnapshot(ctx context.Context, client hetznerSnapshotClient, initial core.HetznerImage, timeout time.Duration, stderr io.Writer) (core.HetznerImage, error) {
	last := initial
	deadline := checkpointNow().Add(timeout)
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	timeoutError := func() error {
		return core.Exit(5, "timed out waiting for Hetzner snapshot %d; last state=%s", last.ID, blank(last.Status, "unknown"))
	}
	waitError := func(err error) error {
		if parentErr := ctx.Err(); parentErr != nil {
			return parentErr
		}
		if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
			return timeoutError()
		}
		return err
	}
	initialObservation := true
	var delay time.Duration
	result, err := shared.Poll(context.WithoutCancel(waitCtx), 0, time.Nanosecond,
		func(context.Context, time.Duration) error {
			if err := checkpointSleep(waitCtx, delay); err != nil {
				return waitError(err)
			}
			if parentErr := ctx.Err(); parentErr != nil {
				return parentErr
			}
			if !checkpointNow().Before(deadline) {
				return timeoutError()
			}
			if err := waitCtx.Err(); err != nil {
				return waitError(err)
			}
			return nil
		},
		func(context.Context) (core.HetznerImage, error) {
			if initialObservation {
				initialObservation = false
				return last, nil
			}
			next, err := client.GetImage(waitCtx, last.ID)
			if err != nil {
				return core.HetznerImage{}, err
			}
			if next.Description == "" {
				next.Description = last.Description
			}
			if next.Name == "" {
				next.Name = last.Name
			}
			if next.Architecture == "" {
				next.Architecture = last.Architecture
			}
			last = next
			return last, nil
		},
		func(_ context.Context, current core.HetznerImage, fetchErr error) (bool, error) {
			if fetchErr != nil {
				return false, waitError(fetchErr)
			}
			last = current
			state := strings.ToLower(strings.TrimSpace(last.Status))
			if state == "available" {
				return true, nil
			}
			if failedHetznerSnapshotState(state) {
				return false, core.Exit(5, "Hetzner snapshot %d failed; last state=%s", last.ID, blank(last.Status, "unknown"))
			}
			if parentErr := ctx.Err(); parentErr != nil {
				return false, parentErr
			}
			now := checkpointNow()
			if timeout <= 0 || !now.Before(deadline) {
				return false, timeoutError()
			}
			if err := waitCtx.Err(); err != nil {
				return false, waitError(err)
			}
			delay = min(checkpointPollInterval, deadline.Sub(now))
			return false, nil
		},
		func(shared.PollResult[core.HetznerImage]) {
			if stderr != nil {
				fmt.Fprintf(stderr, "waiting image=%d state=%s\n", last.ID, blank(last.Status, "creating"))
			}
		})
	if err != nil {
		return last, err
	}
	return result.Value, nil
}

func validateCreatedHetznerSnapshot(snapshot core.HetznerImage, expectedArchitecture string) error {
	if snapshot.Type != "" && snapshot.Type != "snapshot" {
		return core.Exit(5, "Hetzner returned image %d with unexpected type=%s", snapshot.ID, snapshot.Type)
	}
	if snapshot.Architecture != expectedArchitecture {
		return core.Exit(5, "Hetzner snapshot %d architecture=%s does not match source architecture=%s", snapshot.ID, blank(snapshot.Architecture, "unknown"), expectedArchitecture)
	}
	return nil
}

func failedHetznerSnapshotState(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "failed", "error", "unavailable":
		return true
	default:
		return false
	}
}

func hetznerServerArchitecture(server Server) (string, error) {
	imageArchitecture := ""
	if server.Image != nil {
		imageArchitecture = server.Image.Architecture
	}
	value := firstNonBlank(imageArchitecture, server.ServerType.Architecture, server.Labels["architecture"])
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "x86", "amd64", "x86_64":
		return "x86", nil
	case "arm", "arm64", "aarch64":
		return "arm", nil
	default:
		return "", core.Exit(2, "Hetzner source server %s has unsupported architecture=%s", server.DisplayID(), blank(value, "unknown"))
	}
}

func coreArchitectureFromHetzner(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "x86":
		return core.ArchitectureAMD64, nil
	case "arm":
		return core.ArchitectureARM64, nil
	default:
		return "", core.Exit(2, "unsupported Hetzner snapshot architecture=%s", blank(value, "unknown"))
	}
}

func parsePositiveID(value, kind string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || id <= 0 {
		return 0, core.Exit(2, "Hetzner %s ID must be a positive integer", kind)
	}
	return id, nil
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

func cloneMetadata(values map[string]string) map[string]string {
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

var (
	newHetznerSnapshotClient       = func() (hetznerSnapshotClient, error) { return core.NewHetznerClient() }
	prepareHetznerCheckpointSource = core.PrepareNativeImageSource
	checkpointPollInterval         = 15 * time.Second
	checkpointNow                  = time.Now
	checkpointSleep                = func(ctx context.Context, delay time.Duration) error {
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
