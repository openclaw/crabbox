package machine0

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const (
	metadataImageName     = "machine0_image_name"
	metadataAccountID     = "machine0_account_id"
	metadataImageID       = "machine0_image_id"
	metadataImageVersion  = "machine0_image_version"
	metadataCreatedImage  = "machine0_created_image"
	metadataSourceMachine = "machine0_source_machine"
)

type machineImage struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	Status             string   `json:"status"`
	Type               string   `json:"type"`
	Description        string   `json:"description"`
	Regions            []string `json:"regions"`
	Distribution       string   `json:"distribution"`
	DefaultSSHUsername string   `json:"defaultSSHUsername"`
}

type machineImageVersion struct {
	ID                         string         `json:"id"`
	Version                    int            `json:"version"`
	Status                     string         `json:"status"`
	DisplayStatus              string         `json:"displayStatus"`
	SnapshotStatus             string         `json:"snapshotStatus"`
	DefaultSSHUsername         string         `json:"defaultSSHUsername"`
	Regions                    []string       `json:"regions"`
	PricePerHour               *json.Number   `json:"pricePerHour"`
	TotalCost                  *json.Number   `json:"totalCost"`
	VersionSnapshotStorageCost *json.Number   `json:"versionSnapshotStorageCost"`
	Metadata                   map[string]any `json:"metadata"`
}

type machineImageDetail struct {
	Image    machineImage          `json:"image"`
	Versions []machineImageVersion `json:"versions"`
}

// An existing image without the recorded version remains an ordinary lookup
// error. Only the same admitted version deletion may accept this observation.
type checkpointImageVersionAbsentError struct {
	core.ExitError
}

func (e checkpointImageVersionAbsentError) Unwrap() error { return e.ExitError }

var providerOperationRuntime = core.RuntimeForProviderOperation

func (c *client) ListImages(ctx context.Context) ([]machineImage, error) {
	result, err := c.runRead(ctx, "images", "ls", "--json")
	if err != nil {
		return nil, err
	}
	var images []machineImage
	if err := decodeJSON(result.Stdout, &images); err != nil {
		return nil, exit(5, "parse machine0 images ls --json: %v", err)
	}
	for i, image := range images {
		if strings.TrimSpace(image.ID) == "" || strings.TrimSpace(image.Name) == "" || strings.TrimSpace(image.Status) == "" {
			return nil, exit(5, "invalid machine0 image list item %d", i)
		}
	}
	return images, nil
}

func (c *client) GetImage(ctx context.Context, name string) (machineImageDetail, error) {
	result, err := c.runRead(ctx, "images", "get", name, "--json")
	if err != nil {
		return machineImageDetail{}, err
	}
	var detail machineImageDetail
	if err := decodeJSON(result.Stdout, &detail); err != nil {
		return machineImageDetail{}, exit(5, "parse machine0 images get %s --json: %v", name, err)
	}
	if detail.Image.ID == "" || detail.Image.Name == "" {
		return machineImageDetail{}, exit(5, "invalid machine0 image details for %s", name)
	}
	for i, version := range detail.Versions {
		if version.Version <= 0 {
			return machineImageDetail{}, exit(5, "invalid machine0 image %s version at index %d", name, i)
		}
	}
	return detail, nil
}

func (c *client) SaveImage(ctx context.Context, machine, image string, metadata map[string]string) error {
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return exit(2, "encode Machine0 image metadata: %v", err)
	}
	_, err = c.run(ctx, "images", "save", machine, image, "--metadata", string(encoded))
	return err
}

func (c *client) RemoveImage(ctx context.Context, image string) error {
	_, err := c.run(ctx, "images", "rm", image, "--yes")
	return err
}

func (c *client) RemoveImageVersion(ctx context.Context, image string, version int) error {
	_, err := c.run(ctx, "images", "versions", "rm", image, strconv.Itoa(version), "--yes")
	return err
}

func (Provider) NativeCheckpointCapability(req core.NativeCheckpointRequest) (core.NativeCheckpointCapability, bool) {
	if firstNonBlank(req.Server.Provider, req.Config.Provider) != providerName || strings.TrimSpace(req.Server.CloudID) == "" {
		return core.NativeCheckpointCapability{}, false
	}
	capability := core.NativeCheckpointCapability{Kind: core.CheckpointKindMachine0, Direct: true, ReplayCapture: true, RetireSource: true}
	capability.RetireUnsupported = checkpointRetirementPolicy(req.Config, req.Server.Labels)
	if req.StrategyExplicit && core.NormalizeCheckpointStrategy(req.Strategy) == core.CheckpointStrategyDiskSnapshot {
		capability.CreateUnsupported = "Machine0 reusable images use --strategy image; suspend is available separately through `crabbox pause`"
	}
	return capability, true
}

func checkpointRetirementPolicy(cfg Config, labels map[string]string) string {
	if normalizeReleasePolicy(cfg.Machine0.ReleasePolicy) != "destroy" || labels["release_policy"] == "suspend" {
		return "checkpoint --retire-source requires the configured and claimed Machine0 release policy to allow destruction; suspend is not retirement"
	}
	return ""
}

func (Provider) NativeCheckpointWorkdir(req core.NativeCheckpointWorkdirRequest) string {
	if strings.TrimSpace(req.Override) != "" {
		return strings.TrimSpace(req.Override)
	}
	cfg := req.Config
	if workRoot := strings.TrimSpace(req.Server.Labels["work_root"]); workRoot != "" {
		cfg.WorkRoot = workRoot
	}
	return core.RemoteJoin(cfg, req.LeaseID, req.RepoName)
}

func (p Provider) CreateNativeCheckpoint(ctx context.Context, req core.NativeCheckpointCreateRequest) (core.NativeCheckpointCreateResult, error) {
	if core.NormalizeCheckpointStrategy(req.Strategy) == core.CheckpointStrategyDiskSnapshot {
		return core.NativeCheckpointCreateResult{}, exit(2, "Machine0 reusable images require --strategy image; use `crabbox pause` for suspend snapshots")
	}
	claim, claimed, err := resolveClaim(req.LeaseID)
	if err != nil {
		return core.NativeCheckpointCreateResult{}, err
	}
	if !claimed || claim.CloudID != req.Server.CloudID {
		return core.NativeCheckpointCreateResult{}, exit(2, "refusing Machine0 image save without an exact source lease claim")
	}
	configured, err := p.Configure(req.Config, providerOperationRuntime(req.Stderr))
	if err != nil {
		return core.NativeCheckpointCreateResult{}, err
	}
	return configured.(*backend).createNativeCheckpoint(ctx, req, claim)
}

func (b *backend) createNativeCheckpoint(ctx context.Context, req core.NativeCheckpointCreateRequest, claim LeaseClaim) (core.NativeCheckpointCreateResult, error) {
	if req.Capture != nil {
		return b.advanceCheckpointCapture(ctx, req, claim)
	}
	if claim.CheckpointCapture != nil {
		return core.NativeCheckpointCreateResult{}, core.AuthorizeCheckpointRelease(claim, "")
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "crabbox-" + strings.ToLower(strings.TrimPrefix(req.CheckpointID, "chk_"))
	}
	images, err := b.api.ListImages(ctx)
	if err != nil {
		return core.NativeCheckpointCreateResult{}, err
	}
	createdImage := true
	previousVersion := 0
	for _, image := range images {
		if image.Name != name {
			continue
		}
		createdImage = false
		detail, err := b.api.GetImage(ctx, name)
		if err != nil {
			return core.NativeCheckpointCreateResult{}, err
		}
		for _, version := range detail.Versions {
			previousVersion = max(previousVersion, version.Version)
		}
		break
	}
	remoteMetadata := map[string]string{
		"crabbox_checkpoint": req.CheckpointID,
		"crabbox_lease":      claim.LeaseID,
		"crabbox_source":     req.Server.CloudID,
	}
	var detail machineImageDetail
	var version machineImageVersion
	var lifecycleErr error
	snapshotTimeout := machine0CheckpointSnapshotTimeout(req.WaitTimeout, b.configForRun().Machine0.CreateTimeout)
	_, _, _, actionErr := updateClaimAction(claim.LeaseID, claim, func() (Server, SSHTarget, bool, error) {
		lookup := firstNonBlank(claim.Labels["machine0_name"], req.Server.Name, claim.CloudID)
		item, err := b.readCheckpointSource(ctx, claim, lookup)
		if err != nil {
			return Server{}, SSHTarget{}, false, err
		}

		stoppedByCheckpoint := false
		switch {
		case machineRunning(item.Status):
			if err := b.prepareNativeImageSource(ctx, req.Target); err != nil {
				return Server{}, SSHTarget{}, false, err
			}
			item, err = b.readCheckpointSource(ctx, claim, item.Name)
			if err != nil {
				return Server{}, SSHTarget{}, false, err
			}
			if !machineRunning(item.Status) {
				return Server{}, SSHTarget{}, false, exit(5, "Machine0 checkpoint source changed state before stop")
			}
			if err := b.api.Stop(ctx, item.Name); err != nil {
				return Server{}, SSHTarget{}, false, err
			}
			stoppedByCheckpoint = true
			if stopped, err := b.waitForStopped(ctx, item.Name, b.configForRun().Machine0.CreateTimeout); err != nil {
				lifecycleErr = err
			} else if err := validateMachineClaimOwnership(claim, stopped); err != nil {
				// The observed source was replaced; never save or restart its successor.
				return Server{}, SSHTarget{}, false, err
			}
		case strings.EqualFold(strings.TrimSpace(item.Status), "STOPPED"):
			// A pre-stopped source is already filesystem-consistent. Preserve its state.
		default:
			return Server{}, SSHTarget{}, false, exit(5, "machine0 machine %s must be RUNNING or STOPPED to create an image; state=%s", item.Name, item.Status)
		}

		if lifecycleErr == nil {
			if err := b.api.SaveImage(ctx, item.Name, name, remoteMetadata); err != nil {
				lifecycleErr = err
			} else {
				detail, version, lifecycleErr = b.waitForImageVersion(ctx, name, previousVersion, remoteMetadata, true, snapshotTimeout, req.Stderr, func(detail machineImageDetail, version machineImageVersion) error {
					if req.Persist == nil {
						return nil
					}
					return req.Persist(machine0NativeCheckpointResult(req, claim, name, createdImage, detail, version))
				})
			}
		}

		var server Server
		var target SSHTarget
		if stoppedByCheckpoint {
			var restartErr error
			server, target, restartErr = b.restartCheckpointSource(ctx, claim, item)
			if restartErr != nil {
				return Server{}, SSHTarget{}, false, restartErr
			}
		}
		return server, target, stoppedByCheckpoint, nil
	})
	result := machine0NativeCheckpointResult(req, claim, name, createdImage, detail, version)
	if actionErr != nil {
		return result, errors.Join(lifecycleErr, actionErr)
	}
	return result, lifecycleErr
}

func machine0CheckpointSnapshotTimeout(requested, fallback time.Duration) time.Duration {
	if requested > 0 {
		return requested
	}
	return fallback
}

// Native mutations accept names. This rejects replacements visible at each
// adapter boundary, not a privileged remote rename inside the opaque CLI call.
func (b *backend) readCheckpointSource(ctx context.Context, claim LeaseClaim, name string) (machine, error) {
	item, err := b.api.Get(ctx, name)
	if err != nil {
		return machine{}, err
	}
	if err := validateMachineClaimOwnership(claim, item); err != nil {
		return machine{}, err
	}
	if item.Name != name {
		return machine{}, exit(2, "checkpoint source name changed")
	}
	if fixedMachine0LeaseKind.IsFixedClaim(claim) {
		if err := validateFixedMachine0Ownership(claim, item); err != nil {
			return machine{}, err
		}
	}
	return item, nil
}

func (b *backend) restartCheckpointSource(ctx context.Context, claim LeaseClaim, stopped machine) (Server, SSHTarget, error) {
	timeout := b.configForRun().Machine0.CreateTimeout
	// Once this operation stops a running source, restoring it is a rollback
	// obligation even if the caller cancels the checkpoint wait.
	restartCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	_, err := b.readCheckpointSource(restartCtx, claim, stopped.Name)
	if err != nil {
		return Server{}, SSHTarget{}, err
	}
	if err := b.api.Start(restartCtx, stopped.Name); err != nil {
		return Server{}, SSHTarget{}, fmt.Errorf("restart Machine0 checkpoint source %s: %w", stopped.Name, err)
	}
	running, err := b.waitForRunningAfterStart(restartCtx, stopped.Name, timeout, func(previous, observed machine) (machine, error) {
		if fixedMachine0LeaseKind.IsFixedClaim(claim) {
			if previous.ID == "" {
				previous = stopped
			}
			return attestFixedMachine0Detail(claim, previous, observed)
		}
		return observed, nil
	})
	if err != nil {
		return Server{}, SSHTarget{}, fmt.Errorf("restart Machine0 checkpoint source %s: %w", stopped.Name, err)
	}
	if err := validateMachineClaimOwnership(claim, running); err != nil {
		return Server{}, SSHTarget{}, err
	}
	cfg := effectiveMachine0Config(b.configForRun(), running)
	server := b.serverFromMachine(running, claim, cfg)
	lease, err := b.prepareLeaseWithOptions(restartCtx, running, server, claim.LeaseID, machine0PrepareOptions{Check: true, ResetHostTrust: true})
	if err != nil {
		return Server{}, SSHTarget{}, fmt.Errorf("prepare restarted Machine0 checkpoint source %s: %w", stopped.Name, err)
	}
	return lease.Server, lease.SSH, nil
}

func machine0NativeCheckpointResult(req core.NativeCheckpointCreateRequest, claim LeaseClaim, name string, createdImage bool, detail machineImageDetail, version machineImageVersion) core.NativeCheckpointCreateResult {
	if strings.TrimSpace(detail.Image.ID) == "" || version.Version <= 0 {
		return core.NativeCheckpointCreateResult{}
	}
	metadata := map[string]string{metadataImageName: name, metadataImageID: detail.Image.ID, metadataImageVersion: strconv.Itoa(version.Version), metadataCreatedImage: strconv.FormatBool(createdImage), metadataSourceMachine: req.Server.CloudID, "crabbox_checkpoint": req.CheckpointID, "crabbox_lease": claim.LeaseID}
	if accountID := req.Metadata[metadataAccountID]; accountID != "" {
		metadata[metadataAccountID] = accountID
	}
	for key, value := range machine0ImageCostMetadata(version) {
		metadata[key] = value
	}
	return core.NativeCheckpointCreateResult{Image: core.NativeCheckpointImage{ID: fmt.Sprintf("%s@v%d", detail.Image.ID, version.Version), Name: name, State: imageVersionState(version), Provider: providerName, Kind: core.CheckpointKindMachine0, Region: req.Server.Labels["region"], ResourceID: detail.Image.ID, Architecture: firstNonBlank(req.Server.ServerType.Architecture, "amd64"), Direct: true}, Metadata: metadata}
}

func (Provider) VerifyNativeCheckpoint(ctx context.Context, req core.NativeCheckpointResourceRequest) (core.NativeCheckpointVerifyResult, error) {
	b, err := checkpointBackend(req)
	if err != nil {
		return core.NativeCheckpointVerifyResult{}, err
	}
	_, version, err := b.loadCheckpointImage(ctx, req)
	if errors.Is(err, core.ErrNativeCheckpointAbsent) {
		return core.NativeCheckpointVerifyResult{ProviderState: "missing", NextAction: "delete_local"}, nil
	}
	if err != nil {
		return core.NativeCheckpointVerifyResult{}, err
	}
	state := imageVersionState(version)
	next := "wait_or_delete"
	if imageVersionReady(version) {
		next = "fork_or_delete"
	}
	if imageVersionTerminal(version) {
		next = "delete"
	}
	return core.NativeCheckpointVerifyResult{ProviderState: state, NextAction: next}, nil
}

func (Provider) DeleteNativeCheckpoint(ctx context.Context, req core.NativeCheckpointResourceRequest) error {
	b, err := checkpointBackend(req)
	if err != nil {
		return err
	}
	return b.deleteNativeCheckpoint(ctx, req)
}

func (b *backend) deleteNativeCheckpoint(ctx context.Context, req core.NativeCheckpointResourceRequest) error {
	detail, version, err := b.loadCheckpointImage(ctx, req)
	if errors.Is(err, core.ErrNativeCheckpointAbsent) {
		return nil
	}
	if err != nil {
		return err
	}
	if req.Metadata[metadataAccountID] == "" {
		// A legacy image can be deleted while its exact identity is visible.
		// Bind only this invocation's absence check, never rewrite its history.
		accountID, err := b.api.AccountID(ctx)
		if err != nil {
			return err
		}
		metadata := make(map[string]string, len(req.Metadata)+1)
		for key, value := range req.Metadata {
			metadata[key] = value
		}
		metadata[metadataAccountID] = accountID
		req.Metadata = metadata
		detail, version, err = b.loadCheckpointImage(ctx, req)
		if err != nil {
			return err
		}
	}
	createdImage := req.Metadata[metadataCreatedImage] == "true"
	if createdImage {
		if len(detail.Versions) != 1 || detail.Versions[0].Version != version.Version {
			return exit(2, "refusing to remove Machine0 image %q: owned checkpoint version v%d is no longer the only version (%d versions found); remove the exact draft version with `machine0 images versions rm %s %d --yes` when applicable, or resolve the image versions manually", detail.Image.Name, version.Version, len(detail.Versions), detail.Image.Name, version.Version)
		}
		err = b.api.RemoveImage(ctx, detail.Image.Name)
	} else {
		err = b.api.RemoveImageVersion(ctx, detail.Image.Name, version.Version)
	}
	_, _, verifyErr := b.loadCheckpointImage(ctx, req)
	if errors.Is(verifyErr, core.ErrNativeCheckpointAbsent) {
		return nil
	}
	var versionAbsent checkpointImageVersionAbsentError
	if !createdImage && errors.As(verifyErr, &versionAbsent) {
		return nil
	}
	if verifyErr != nil {
		return errors.Join(err, verifyErr)
	}
	return errors.Join(err, core.ErrCheckpointPending)
}

func (Provider) ApplyNativeCheckpointForkConfig(req core.NativeCheckpointForkRequest) error {
	if req.Record.Kind != core.CheckpointKindMachine0 || !req.Record.Direct {
		return exit(2, "provider=machine0 does not support checkpoint kind=%s", req.Record.Kind)
	}
	name := strings.TrimSpace(req.Record.Metadata[metadataImageName])
	version, err := strconv.Atoi(req.Record.Metadata[metadataImageVersion])
	if name == "" || err != nil || version <= 0 {
		return exit(2, "Machine0 checkpoint image metadata is missing or invalid")
	}
	req.Config.Provider = providerName
	req.Config.Machine0.Image = name
	req.Config.Machine0.ImageVersion = version
	if req.Record.Region != "" {
		req.Config.Machine0.Region = req.Record.Region
	}
	return nil
}

func checkpointBackend(req core.NativeCheckpointResourceRequest) (*backend, error) {
	cfg, err := req.LoadConfig()
	if err != nil {
		return nil, err
	}
	p := Provider{}
	configured, err := p.Configure(cfg, providerOperationRuntime(nil))
	if err != nil {
		return nil, err
	}
	return configured.(*backend), nil
}

func (b *backend) waitForImageVersion(ctx context.Context, name string, previous int, expectedMetadata map[string]string, wait bool, timeout time.Duration, stderr interface{ Write([]byte) (int, error) }, observers ...func(machineImageDetail, machineImageVersion) error) (machineImageDetail, machineImageVersion, error) {
	pollCtx := ctx
	cancel := func() {}
	if wait {
		pollCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	var lastDetail machineImageDetail
	var lastVersion machineImageVersion
	result, err := shared.Poll(pollCtx, 0, b.configForRun().Machine0.PollInterval, b.sleep, func(observeCtx context.Context) (machineImageDetail, error) {
		return b.api.GetImage(observeCtx, name)
	}, func(_ context.Context, detail machineImageDetail, fetchErr error) (bool, error) {
		if fetchErr != nil {
			return false, fetchErr
		}
		sort.Slice(detail.Versions, func(i, j int) bool { return detail.Versions[i].Version > detail.Versions[j].Version })
		var version machineImageVersion
		for _, candidate := range detail.Versions {
			if candidate.Version > previous && machine0ImageVersionMetadataMatches(candidate, expectedMetadata) {
				version = candidate
				break
			}
		}
		if version.Version == 0 {
			if !wait {
				return false, exit(5, "Machine0 did not report a new image version for %s", name)
			}
			return false, nil
		}
		lastDetail, lastVersion = detail, version
		for _, observe := range observers {
			if err := observe(detail, version); err != nil {
				return false, err
			}
		}
		if !wait || imageVersionReady(version) {
			return true, nil
		}
		if imageVersionTerminal(version) {
			return false, exit(5, "Machine0 image %s v%d entered terminal state %s", name, version.Version, imageVersionState(version))
		}
		if stderr != nil {
			_, _ = fmt.Fprintf(stderr, "waiting image=%s version=%d state=%s\n", name, version.Version, imageVersionState(version))
		}
		return false, nil
	}, nil)
	if err != nil && wait && context.Cause(ctx) == nil && errors.Is(context.Cause(pollCtx), context.DeadlineExceeded) && errors.Is(err, context.DeadlineExceeded) {
		err = exit(5, "timed out waiting for Machine0 image %s", name)
	}
	if lastVersion.Version > 0 {
		return lastDetail, lastVersion, err
	}
	return result.Value, machineImageVersion{}, err
}

func machine0ImageVersionMetadataMatches(version machineImageVersion, expected map[string]string) bool {
	if len(expected) == 0 {
		return true
	}
	for _, key := range []string{"crabbox_checkpoint", "crabbox_lease", "crabbox_source"} {
		if expected[key] == "" || fmt.Sprint(version.Metadata[key]) != expected[key] {
			return false
		}
	}
	return true
}

func (b *backend) loadCheckpointImage(ctx context.Context, req core.NativeCheckpointResourceRequest) (machineImageDetail, machineImageVersion, error) {
	if req.Capture != nil && req.Capture.SourceDisposition == "abandon" {
		return machineImageDetail{}, machineImageVersion{}, exit(2, "checkpoint abandonment does not authorize image operations; retain the unresolved image obligation")
	}
	// Ordinary checkpoints predating account-bound retirement keep their existing
	// image contract. A bound capture must never treat another account as absence.
	if req.Metadata[metadataAccountID] != "" {
		if err := b.attestCheckpointAccount(ctx, req.Metadata[metadataAccountID]); err != nil {
			return machineImageDetail{}, machineImageVersion{}, err
		}
	}
	if req.Image.Provider != providerName || req.Image.Kind != core.CheckpointKindMachine0 || !req.Image.Direct {
		return machineImageDetail{}, machineImageVersion{}, exit(2, "refusing to operate on a non-direct Machine0 checkpoint")
	}
	name := req.Metadata[metadataImageName]
	versionNumber, err := strconv.Atoi(req.Metadata[metadataImageVersion])
	if name == "" || err != nil || versionNumber <= 0 {
		return machineImageDetail{}, machineImageVersion{}, exit(2, "Machine0 checkpoint metadata is missing image version identity")
	}
	if req.Metadata[metadataImageID] == "" || req.Metadata[metadataImageID] != req.Image.ResourceID || req.Metadata[metadataSourceMachine] == "" || req.Metadata["crabbox_checkpoint"] == "" || req.Metadata["crabbox_lease"] == "" {
		return machineImageDetail{}, machineImageVersion{}, exit(2, "Machine0 checkpoint is missing its exact resource binding")
	}
	images, err := b.api.ListImages(ctx)
	if err != nil {
		return machineImageDetail{}, machineImageVersion{}, err
	}
	found := false
	for _, image := range images {
		if image.Name == name || image.ID == req.Image.ResourceID {
			if image.Name != name || image.ID != req.Image.ResourceID || found {
				return machineImageDetail{}, machineImageVersion{}, exit(2, "Machine0 checkpoint image identity changed or is ambiguous")
			}
			found = true
		}
	}
	if !found {
		if req.Metadata[metadataAccountID] == "" {
			return machineImageDetail{}, machineImageVersion{}, exit(4, "Machine0 checkpoint image is not visible and its original account is unbound; retain checkpoint metadata and inspect the original account")
		}
		if req.Metadata[metadataAccountID] != "" {
			if err := b.attestCheckpointAccount(ctx, req.Metadata[metadataAccountID]); err != nil {
				return machineImageDetail{}, machineImageVersion{}, err
			}
		}
		return machineImageDetail{}, machineImageVersion{}, core.ErrNativeCheckpointAbsent
	}
	detail, err := b.api.GetImage(ctx, name)
	if err != nil {
		return machineImageDetail{}, machineImageVersion{}, err
	}
	if detail.Image.ID != req.Metadata[metadataImageID] || detail.Image.ID != req.Image.ResourceID || detail.Image.Name != name {
		return machineImageDetail{}, machineImageVersion{}, exit(2, "refusing Machine0 checkpoint operation with mismatched image identity")
	}
	for _, version := range detail.Versions {
		if version.Version == versionNumber {
			for _, key := range []string{"crabbox_checkpoint", "crabbox_lease", "crabbox_source"} {
				expected := req.Metadata[key]
				if key == "crabbox_source" {
					expected = req.Metadata[metadataSourceMachine]
				}
				if expected == "" || fmt.Sprint(version.Metadata[key]) != expected {
					return machineImageDetail{}, machineImageVersion{}, exit(2, "refusing Machine0 checkpoint operation with mismatched %s metadata", key)
				}
			}
			return detail, version, nil
		}
	}
	return detail, machineImageVersion{}, checkpointImageVersionAbsentError{exit(4, "Machine0 image %s version %d was not found", name, versionNumber)}
}

func imageVersionState(version machineImageVersion) string {
	return firstNonBlank(version.DisplayStatus, version.Status, "unknown")
}

func machine0ImageCostMetadata(version machineImageVersion) map[string]string {
	metadata := map[string]string{}
	for key, value := range map[string]*json.Number{
		"machine0_price_per_hour":                version.PricePerHour,
		"machine0_total_cost":                    version.TotalCost,
		"machine0_version_snapshot_storage_cost": version.VersionSnapshotStorageCost,
	} {
		if value != nil && value.String() != "" {
			metadata[key] = value.String()
		}
	}
	return metadata
}

func imageVersionReady(version machineImageVersion) bool {
	if !strings.EqualFold(strings.TrimSpace(version.SnapshotStatus), "READY") {
		return false
	}
	status := strings.ToUpper(strings.TrimSpace(version.Status))
	display := strings.ToUpper(strings.TrimSpace(version.DisplayStatus))
	if machine0ImageTerminalState(status) || machine0ImageTerminalState(display) {
		return false
	}
	return status == "ACTIVE" || status == "DRAFT" || display == "ACTIVE" || display == "DRAFT"
}
func imageVersionTerminal(version machineImageVersion) bool {
	status := strings.ToUpper(strings.TrimSpace(version.Status))
	display := strings.ToUpper(strings.TrimSpace(version.DisplayStatus))
	snapshot := strings.ToUpper(strings.TrimSpace(version.SnapshotStatus))
	for _, state := range []string{status, display, snapshot} {
		if machine0ImageTerminalState(state) {
			return true
		}
	}
	return false
}

func machine0ImageTerminalState(state string) bool {
	return state == "ERRORED" || state == "ERROR" || state == "FAILED" || state == "UNAVAILABLE"
}
