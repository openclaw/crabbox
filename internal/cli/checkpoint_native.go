package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type checkpointNativeCreateDriver interface {
	Create(context.Context, NativeCheckpointCreateRequest) (CoordinatorImage, error)
}

type directAWSAMICheckpointDriver struct{}

func (directAWSAMICheckpointDriver) Create(ctx context.Context, req NativeCheckpointCreateRequest) (CoordinatorImage, error) {
	name := req.Name
	if name == "" {
		name = defaultNativeImageName(req.LeaseID, req.RepoName)
	}
	client, err := newAWSClient(ctx, req.Config)
	if err != nil {
		return CoordinatorImage{}, err
	}
	if _, err := client.ValidateImageCheckpointSource(ctx, req.Server.CloudID); err != nil {
		return CoordinatorImage{}, err
	}
	if !isWindowsNativeTarget(req.Target) {
		if err := prepareNativeImageSource(ctx, req.Target); err != nil {
			return CoordinatorImage{}, err
		}
	}
	image, err := client.CreateImageCheckpoint(ctx, req.Server.CloudID, name, req.NoReboot)
	if err != nil {
		return CoordinatorImage{}, err
	}
	// Readiness can take minutes. Record the accepted AMI before another
	// network call so interruption cannot lose its account-scoped cleanup identity.
	if req.Persist != nil {
		if err := req.Persist(NativeCheckpointCreateResult{Image: NativeCheckpointImage{
			ID: image.ID, Name: image.Name, State: image.State, Provider: image.Provider,
			Kind: image.Kind, Region: image.Region, AccountID: image.AccountID,
			ResourceID: image.ResourceID, SnapshotIDs: image.SnapshotIDs, Direct: image.Direct,
		}}); err != nil {
			return image, err
		}
	}
	if req.Wait {
		waited, err := waitForDirectAWSImage(ctx, client, image.ID, image.AccountID, req.WaitTimeout, req.Stderr)
		if err != nil {
			return image, err
		}
		return waited, nil
	}
	return image, nil
}

type directAzureOSDiskCheckpointDriver struct{}

const azureSnapshotNameMaxLength = 80

func azureOSDiskSnapshotName(requested, leaseID, repoName string) (string, error) {
	if requested != "" {
		if len(requested) > azureSnapshotNameMaxLength || safeCaptureName(requested) != requested {
			return "", exit(2, "Azure snapshot name must be at most %d characters using only letters, digits, underscores, and hyphens", azureSnapshotNameMaxLength)
		}
		return requested, nil
	}
	repoName = strings.TrimSpace(repoName)
	if repoName == "" {
		repoName = "workspace"
	}
	timestamp := time.Now().UTC().Format("20060102-150405")
	suffix := "-" + timestamp
	seed := "crabbox-" + safeCaptureName(repoName) + "-" + strings.ReplaceAll(safeCaptureName(leaseID), "_", "-")
	if maxSeed := azureSnapshotNameMaxLength - len(suffix); len(seed) > maxSeed {
		seed = strings.TrimRight(seed[:maxSeed], "-_")
	}
	return seed + suffix, nil
}

func (directAzureOSDiskCheckpointDriver) Create(ctx context.Context, req NativeCheckpointCreateRequest) (CoordinatorImage, error) {
	if normalizeCheckpointStrategy(req.Strategy) == checkpointStrategyImage {
		return CoordinatorImage{}, exit(2, "Azure Windows checkpoints require --strategy disk-snapshot")
	}
	if req.NoReboot {
		return CoordinatorImage{}, exit(2, "Azure Windows checkpoints require a deallocated source VM for a consistent OS-disk snapshot; rerun with --no-reboot=false")
	}
	name, err := azureOSDiskSnapshotName(req.Name, req.LeaseID, req.RepoName)
	if err != nil {
		return CoordinatorImage{}, err
	}
	client, err := NewAzureClient(ctx, req.Config)
	if err != nil {
		return CoordinatorImage{}, err
	}
	snapshot, err := client.CreateOSDiskSnapshot(
		ctx,
		req.Server.CloudID,
		name,
		req.Config.AzureSnapshotSKU,
	)
	if err != nil {
		return CoordinatorImage{}, err
	}
	return coordinatorImageFromNativeCheckpoint(snapshot), nil
}

type coordinatorCheckpointDriver struct{}

func (coordinatorCheckpointDriver) Create(ctx context.Context, req NativeCheckpointCreateRequest) (CoordinatorImage, error) {
	if req.Config.Coordinator == "" {
		return CoordinatorImage{}, exit(2, "native checkpoints require a configured coordinator")
	}
	strategy := normalizeCheckpointStrategy(req.Strategy)
	capability, ok := providerNativeCheckpointCapability(req.Config, req.Server, req.Target, req.Strategy)
	if !ok || capability.Direct || capability.Kind == "" {
		return CoordinatorImage{}, exit(2, "native checkpoints support brokered AWS Linux/macOS leases and brokered Azure/GCP Linux leases only")
	}
	if capability.CreateUnsupported != "" {
		return CoordinatorImage{}, exit(2, "%s", capability.CreateUnsupported)
	}
	name := req.Name
	if name == "" {
		name = defaultNativeImageName(req.LeaseID, req.RepoName)
	}
	// Source retirement owns its durable capture journal and legacy image lifecycle.
	_, hasManagedContext := checkpointCreateContextFrom(ctx)
	managedCreate := req.CheckpointID != "" && (req.Capture == nil || hasManagedContext)
	var coord *CoordinatorClient
	var err error
	if !managedCreate {
		coord, err = configuredAdminCoordinator()
	} else {
		coord, err = configuredCheckpointCoordinatorFor(ctx)
	}
	if err != nil {
		return CoordinatorImage{}, err
	}
	if managedCreate {
		createContext, ok := checkpointCreateContextFrom(ctx)
		if !ok || createContext.Record.ID != req.CheckpointID {
			return CoordinatorImage{}, exit(2, "checkpoint creation metadata is unavailable")
		}
		if createContext.Retention.Mode != "manual" {
			if _, probeErr := coord.Checkpoints(ctx); probeErr != nil {
				if checkpointRouteUnsupported(probeErr) {
					return CoordinatorImage{}, checkpointUpgradeRequired(probeErr)
				}
				return CoordinatorImage{}, probeErr
			}
		}
	}
	if !isWindowsNativeTarget(req.Target) {
		if err := prepareNativeImageSource(ctx, req.Target); err != nil {
			return CoordinatorImage{}, err
		}
	}
	var image CoordinatorImage
	var managed *coordinatorCheckpoint
	if managedCreate {
		createContext, ok := checkpointCreateContextFrom(ctx)
		if !ok || createContext.Record.ID != req.CheckpointID {
			return CoordinatorImage{}, exit(2, "checkpoint creation metadata is unavailable")
		}
		if createContext.PersistOwnership != nil {
			if err := createContext.PersistOwnership(true); err != nil {
				return CoordinatorImage{}, err
			}
		}
		checkpoint, created, createErr := coord.CreateCheckpoint(ctx, createContext.Record, name, strategy, req.NoReboot, createContext.Retention)
		if createErr != nil {
			// A retained capture already owns its provider mutation; observe that exact
			// operation instead of submitting another capture after an uncertain result.
			if id, pending := checkpointPendingResponse(createErr, http.StatusServiceUnavailable); req.Wait && pending && id == req.CheckpointID {
				return waitForCheckpointImage(ctx, coord, id, "", req.WaitTimeout, req.Stderr)
			}
			if checkpointRouteUnsupported(createErr) {
				_, probeErr := coord.Checkpoints(ctx)
				if checkpointRouteUnsupported(probeErr) && createContext.Retention.Mode == "manual" {
					legacy, adminErr := configuredAdminCoordinator()
					if adminErr != nil {
						return CoordinatorImage{}, adminErr
					}
					if createContext.PersistOwnership != nil {
						if err := createContext.PersistOwnership(false); err != nil {
							return CoordinatorImage{}, err
						}
					}
					coord = legacy
					image, err = coord.CreateImage(ctx, req.LeaseID, name, req.NoReboot, strategy)
				} else if checkpointRouteUnsupported(probeErr) {
					return CoordinatorImage{}, checkpointUpgradeRequired(createErr)
				} else {
					return CoordinatorImage{}, createErr
				}
			} else {
				return CoordinatorImage{}, createErr
			}
		} else {
			image = created
			managed = &checkpoint
			image.managedCheckpoint = managed
		}
	} else {
		image, err = coord.CreateImage(ctx, req.LeaseID, name, req.NoReboot, strategy)
	}
	if err != nil {
		return CoordinatorImage{}, err
	}
	if req.Wait {
		var waited CoordinatorImage
		if managed != nil {
			waited, err = waitForCheckpointImage(ctx, coord, managed.ID, image.ID, req.WaitTimeout, req.Stderr)
		} else {
			waited, err = waitForImage(ctx, coord, image.ID, imageRefFromCoordinatorImage(image), req.WaitTimeout, req.Stderr)
		}
		if err != nil {
			return image, err
		}
		return waited, nil
	}
	return image, nil
}

func waitForCheckpointImage(ctx context.Context, coord *CoordinatorClient, checkpointID, imageID string, timeout time.Duration, stderr io.Writer) (result CoordinatorImage, err error) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	defer func() {
		if err != nil {
			err = fmt.Errorf("waiting for checkpoint %s; inspect retained capture with crabbox checkpoint inspect %s --verify: %w", checkpointID, checkpointID, err)
			var failure ExitError
			if errors.As(err, &failure) {
				err = errors.Join(exit(failure.Code, "%s", err), err)
			} else if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) && errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
				err = errors.Join(exit(5, "%s", err), err)
			}
		}
	}()
	for {
		checkpoint, err := coord.Checkpoint(waitCtx, checkpointID)
		if err != nil {
			return CoordinatorImage{}, err
		}
		state := checkpoint.State
		switch state {
		case "creating":
		case "failed":
			// Exhausted attempts retain a scheduled recovery while the provider
			// mutation remains uncertain. Only a failure without a retry is terminal.
			if checkpoint.RetryAt == "" {
				return CoordinatorImage{}, exit(5, "checkpoint %s failed: %s", checkpointID, blank(checkpoint.LastError, "provider capture failed"))
			}
		case "ready":
			if checkpoint.Image == nil || checkpoint.Image.ID == "" || (imageID != "" && checkpoint.Image.ID != imageID) {
				return CoordinatorImage{}, exit(5, "checkpoint %s provider image identity changed or is missing", checkpointID)
			}
			imageID = checkpoint.Image.ID
			image, err := coord.CheckpointImage(waitCtx, checkpointID)
			if _, pending := checkpointPendingResponse(err, http.StatusConflict); err != nil && !pending {
				return CoordinatorImage{}, err
			} else if err == nil {
				if image.ID != imageID || image.managedCheckpoint.CreatedAt != checkpoint.CreatedAt {
					return CoordinatorImage{}, exit(5, "checkpoint %s provider image identity changed", checkpointID)
				}
				state = image.State
				switch strings.ToLower(state) {
				case "available", "ready", "succeeded", "completed":
					return image, nil
				case "failed", "invalid":
					return CoordinatorImage{}, exit(5, "image %s failed", imageID)
				}
			}
		default:
			return CoordinatorImage{}, exit(5, "checkpoint %s cannot become ready in state %s", checkpointID, state)
		}
		_, _ = fmt.Fprintf(stderr, "waiting checkpoint=%s image=%s state=%s\n", checkpointID, blank(imageID, "pending"), state)
		select {
		case <-waitCtx.Done():
			return CoordinatorImage{}, waitCtx.Err()
		case <-time.After(15 * time.Second):
		}
	}
}

type directParallelsCheckpointDriver struct {
	Runner CommandRunner
}

func (d directParallelsCheckpointDriver) Create(ctx context.Context, req NativeCheckpointCreateRequest) (image CoordinatorImage, err error) {
	name := req.Name
	if name == "" {
		name = defaultNativeImageName(req.LeaseID, req.RepoName)
	}
	cfg := req.Config
	applyParallelsHostRefConfig(&cfg, firstNonBlank(req.Server.Labels["host"], req.Config.Parallels.Host))
	client := NewParallelsClient(cfg, d.Runner)
	vm, err := client.GetVM(ctx, req.Server.CloudID)
	if err != nil {
		return CoordinatorImage{}, err
	}
	if !parallelsPowerOffState(vm.State) {
		if req.NoReboot {
			return CoordinatorImage{}, exit(2, "Parallels native checkpoints require a powered-off VM for forkable linked clones; stop the VM first or rerun with --no-reboot=false")
		}
		restartAfter := parallelsRunningState(vm.State)
		if err := client.Stop(ctx, req.Server.CloudID); err != nil {
			return CoordinatorImage{}, err
		}
		if restartAfter {
			defer func() {
				if startErr := client.Start(ctx, req.Server.CloudID); err == nil && startErr != nil {
					err = startErr
				}
			}()
		}
	}
	snapshot, err := client.CreateSnapshot(ctx, req.Server.CloudID, name, "crabbox checkpoint "+req.LeaseID)
	if err != nil {
		return CoordinatorImage{}, err
	}
	if !parallelsPowerOffState(snapshot.State) {
		_ = client.DeleteSnapshot(ctx, req.Server.CloudID, snapshot.ID, false)
		return CoordinatorImage{}, exit(5, "Parallels snapshot %q state=%s is not forkable; expected poweroff", snapshot.Name, blank(snapshot.State, "unknown"))
	}
	return CoordinatorImage{
		ID:         snapshot.ID,
		Name:       snapshot.Name,
		State:      snapshot.State,
		Provider:   "parallels",
		Kind:       checkpointKindParallels,
		ResourceID: req.Server.CloudID,
		Region:     parallelsHostRefForConfig(cfg),
		Direct:     true,
	}, nil
}

func parallelsPowerOffState(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "poweroff", "powered off", "stopped":
		return true
	default:
		return false
	}
}

func parallelsRunningState(state string) bool {
	return strings.EqualFold(strings.TrimSpace(state), "running")
}

func nativeCheckpointCreateDriver(cfg Config, server Server, target SSHTarget, strategy string) (checkpointNativeCreateDriver, bool) {
	if _, ok := directParallelsNativeCheckpointKind(cfg, server, target, strategy); ok {
		return directParallelsCheckpointDriver{}, true
	}
	if kind, ok := directNativeCheckpointKind(cfg, server, target, strategy); ok {
		switch kind {
		case checkpointKindAWSAMI:
			return directAWSAMICheckpointDriver{}, true
		case checkpointKindAzureOS:
			return directAzureOSDiskCheckpointDriver{}, true
		}
	}
	if _, ok := nativeCheckpointKind(cfg, server, target, strategy); ok {
		return coordinatorCheckpointDriver{}, true
	}
	return nil, false
}

func (a App) createNativeCheckpointRequest(ctx context.Context, req NativeCheckpointCreateRequest) (CoordinatorImage, map[string]string, error) {
	if provider, ok := nativeCheckpointLifecycleProvider(req.Config, req.Server); ok {
		result, err := provider.CreateNativeCheckpoint(ctx, req)
		return coordinatorImageFromNativeCheckpoint(result.Image), result.Metadata, err
	}
	driver, ok := nativeCheckpointCreateDriver(req.Config, req.Server, req.Target, req.Strategy)
	if !ok {
		if req.Config.Coordinator == "" {
			return CoordinatorImage{}, nil, exit(2, "native checkpoints require a configured coordinator")
		}
		return CoordinatorImage{}, nil, exit(2, "native checkpoints support brokered AWS Linux/macOS leases and brokered Azure/GCP Linux leases only")
	}
	var image CoordinatorImage
	create := func() error {
		var err error
		image, err = driver.Create(ctx, req)
		return err
	}
	var err error
	if req.Capture != nil {
		claim, _, readErr := readLeaseClaimWithPresence(req.LeaseID)
		if readErr != nil {
			return image, nil, readErr
		}
		if err := ValidateCheckpointCaptureClaim(claim, req.CheckpointID, req.Capture); err != nil {
			return image, nil, err
		}
		err = withLeaseClaimUnchanged(req.LeaseID, claim, create)
	} else {
		err = create()
	}
	return image, nil, err
}

func (a App) createAWSAMICheckpoint(ctx context.Context, cfg Config, target SSHTarget, leaseID, name, repoName string, noReboot, wait bool, waitTimeout time.Duration) (CoordinatorImage, error) {
	image, _, err := a.createNativeCheckpointRequest(ctx, NativeCheckpointCreateRequest{
		Config: cfg, Server: Server{Provider: "aws", CloudID: leaseID}, Target: target,
		LeaseID: leaseID, Name: name, RepoName: repoName, Strategy: checkpointStrategyImage,
		NoReboot: noReboot, Wait: wait, WaitTimeout: waitTimeout, Stderr: a.Stderr,
	})
	return image, err
}

func coordinatorImageFromNativeCheckpoint(image NativeCheckpointImage) CoordinatorImage {
	return CoordinatorImage{
		ID:           image.ID,
		Name:         image.Name,
		State:        image.State,
		Provider:     image.Provider,
		Kind:         image.Kind,
		Region:       image.Region,
		AccountID:    image.AccountID,
		ResourceID:   image.ResourceID,
		SnapshotIDs:  image.SnapshotIDs,
		Architecture: image.Architecture,
		Direct:       image.Direct,
	}
}

func nativeCheckpointLifecycleProvider(cfg Config, server Server) (NativeCheckpointLifecycleProvider, bool) {
	providerName := firstNonBlank(server.Provider, cfg.Provider)
	provider, err := ProviderFor(providerName)
	if err != nil {
		return nil, false
	}
	lifecycle, ok := provider.(NativeCheckpointLifecycleProvider)
	return lifecycle, ok
}

func (a App) createDirectAWSAMICheckpoint(ctx context.Context, cfg Config, server Server, target SSHTarget, leaseID, name, repoName string, noReboot, wait bool, waitTimeout time.Duration) (CoordinatorImage, error) {
	return directAWSAMICheckpointDriver{}.Create(ctx, NativeCheckpointCreateRequest{
		Config:      cfg,
		Server:      server,
		Target:      target,
		LeaseID:     leaseID,
		Name:        name,
		RepoName:    repoName,
		NoReboot:    noReboot,
		Wait:        wait,
		WaitTimeout: waitTimeout,
		Stderr:      a.Stderr,
	})
}

func waitForDirectAWSImage(ctx context.Context, client *AWSClient, imageID, accountID string, timeout time.Duration, stderr io.Writer) (CoordinatorImage, error) {
	deadline := time.Now().Add(timeout)
	var last CoordinatorImage
	for {
		image, err := client.GetImageCheckpoint(ctx, imageID)
		if err != nil {
			return CoordinatorImage{}, err
		}
		if image.AccountID == "" {
			image.AccountID = accountID
		}
		last = image
		state := strings.ToLower(image.State)
		if state == "available" || state == "ready" || state == "succeeded" || state == "completed" {
			return image, nil
		}
		if state == "failed" || state == "invalid" {
			return CoordinatorImage{}, exit(5, "image %s failed", imageID)
		}
		if time.Now().After(deadline) {
			return CoordinatorImage{}, exit(5, "timed out waiting for image %s; last state=%s", imageID, last.State)
		}
		_, _ = fmt.Fprintf(stderr, "waiting image=%s state=%s\n", imageID, blank(image.State, "pending"))
		select {
		case <-ctx.Done():
			return CoordinatorImage{}, ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
}

func defaultNativeImageName(leaseID, repoName string) string {
	repoName = strings.TrimSpace(repoName)
	if repoName == "" {
		repoName = "workspace"
	}
	base := "crabbox-" + safeCaptureName(repoName) + "-" + strings.ReplaceAll(leaseID, "_", "-") + "-" + time.Now().UTC().Format("20060102-150405")
	if len(base) > 128 {
		return base[:128]
	}
	return base
}

func prepareNativeImageSource(ctx context.Context, target SSHTarget) error {
	command := remotePrepareNativeImageCommand()
	if out, err := runSSHCombinedOutput(ctx, target, "bash -lc "+shellQuote(command)); err != nil {
		return exit(7, "prepare native checkpoint source: %v: %s", err, trimFailureDetail(out))
	}
	return nil
}

func remotePrepareNativeImageCommand() string {
	// The distro interpreter owns cloud-init; project Python environments must not
	// change its paths. Keep real completion facts in tmpfs before cleaning their
	// disk targets, so the source stays ready without admitting an unbooted clone.
	return `set -e
if command -v cloud-init >/dev/null 2>&1; then
sudo /usr/bin/python3 -I -c ` + shellQuote(`import json, os, pathlib, shutil, subprocess, sys, tempfile
from cloudinit.cmd.devel import read_cfg_paths

cloud_init = [sys.executable, "-I", "-m", "cloudinit.cmd.main"]
def require_done():
    result = subprocess.run(cloud_init + ["status", "--format=json"], check=True, capture_output=True, text=True, timeout=30)
    if json.loads(result.stdout)["status"] != "done":
        raise RuntimeError("native checkpoint requires completed cloud-init initialization")

require_done()
paths = read_cfg_paths()
runtime = pathlib.Path(paths.run_dir).absolute()
cache = pathlib.Path(paths.cloud_dir).resolve()
for ancestor in (runtime, *runtime.parents):
    resolved = ancestor.resolve()
    if resolved == cache or cache in resolved.parents:
        raise RuntimeError("cloud-init runtime directory must be outside its cleaned disk cache")
runtime = runtime.resolve()
filesystem = subprocess.check_output(["stat", "-f", "-c", "%T", str(runtime)], text=True).strip()
if filesystem != "tmpfs":
    raise RuntimeError("cloud-init runtime directory must use tmpfs to exclude completion state from the image")
files = [runtime / "status.json", runtime / "result.json"]
for path in files:
    if not path.is_file():
        raise RuntimeError("cloud-init completion file is missing: " + str(path))
for path in files:
    fd, temporary = tempfile.mkstemp(prefix="." + path.name + "-", dir=runtime)
    os.close(fd)
    try:
        original = path.stat()
        shutil.copy2(path, temporary)
        os.chown(temporary, original.st_uid, original.st_gid)
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)
subprocess.run(cloud_init + ["clean", "--logs"], check=True)
require_done()
`) + `
fi
sync`
}

func nativeCheckpointKind(cfg Config, server Server, target SSHTarget, strategy string) (string, bool) {
	capability, ok := providerNativeCheckpointCapability(cfg, server, target, strategy)
	if !ok || capability.Direct {
		return "", false
	}
	if capability.Kind == "" {
		return "", false
	}
	return capability.Kind, true
}

func parallelsNativeCheckpointKind(cfg Config, server Server, strategy string) (string, bool) {
	return directParallelsNativeCheckpointKind(cfg, server, SSHTarget{}, strategy)
}

func directParallelsNativeCheckpointKind(cfg Config, server Server, target SSHTarget, strategy string) (string, bool) {
	capability, ok := providerNativeCheckpointCapability(cfg, server, target, strategy)
	if !ok || !capability.Direct || capability.Kind != checkpointKindParallels {
		return "", false
	}
	return capability.Kind, true
}

func directNativeCheckpointKind(cfg Config, server Server, target SSHTarget, strategy string) (string, bool) {
	capability, ok := providerNativeCheckpointCapability(cfg, server, target, strategy)
	if !ok || !capability.Direct {
		return "", false
	}
	if capability.Kind == "" {
		return "", false
	}
	return capability.Kind, true
}

func providerNativeCheckpointCapability(cfg Config, server Server, target SSHTarget, strategy string) (NativeCheckpointCapability, bool) {
	return nativeCheckpointCapability(NativeCheckpointRequest{
		Config: cfg, Server: server, Target: target,
		Strategy: normalizeCheckpointStrategy(strategy), StrategyExplicit: !isAutoCheckpointStrategy(strategy),
	})
}

// Native mode may use a direct image when the provider has no default disk
// snapshot. Selection changes the requested strategy, not user explicitness.
func nativeModeCheckpointCapability(cfg Config, server Server, target SSHTarget, strategy string) (NativeCheckpointCapability, bool) {
	req := NativeCheckpointRequest{Config: cfg, Server: server, Target: target, Strategy: normalizeCheckpointStrategy(strategy), StrategyExplicit: !isAutoCheckpointStrategy(strategy)}
	if capability, ok := nativeCheckpointCapability(req); ok && capability.Kind != "" {
		return capability, true
	}
	if !req.StrategyExplicit {
		req.Strategy = checkpointStrategyImage
		if capability, ok := nativeCheckpointCapability(req); ok && capability.Direct && capability.Kind != "" {
			return capability, true
		}
	}
	return NativeCheckpointCapability{}, false
}

func nativeCheckpointCapability(req NativeCheckpointRequest) (NativeCheckpointCapability, bool) {
	providerName := firstNonBlank(req.Server.Provider, req.Config.Provider)
	provider, err := ProviderFor(providerName)
	if err != nil {
		return NativeCheckpointCapability{}, false
	}
	capabilityProvider, ok := provider.(NativeCheckpointProvider)
	if !ok {
		return NativeCheckpointCapability{}, false
	}
	return capabilityProvider.NativeCheckpointCapability(req)
}

func (record checkpointRecord) nativeProvider() string {
	return firstNonBlank(record.Native.Provider, checkpointProviderForKind(record.Kind), record.Provider)
}

func (record checkpointRecord) nativeResourceID() string {
	switch record.Kind {
	case checkpointKindAzure, checkpointKindAzureOS, checkpointKindGCP, checkpointKindGCPDisk:
		return firstNonBlank(record.Native.Resource, record.Native.ImageID)
	default:
		return record.Native.ImageID
	}
}

func (record checkpointRecord) nativeDeleteID() string {
	if imageID := strings.TrimSpace(record.Native.ImageID); imageID != "" {
		return imageID
	}
	switch record.Kind {
	case checkpointKindAzure, checkpointKindAzureOS, checkpointKindGCP, checkpointKindGCPDisk:
		return strings.TrimSpace(record.Native.Resource)
	default:
		return ""
	}
}

func (record checkpointRecord) isDirectAWSAMI() bool {
	return record.nativeProvider() == "aws" && record.Kind == checkpointKindAWSAMI && record.Native.Direct
}

func (record *checkpointRecord) applyNativeImage(image CoordinatorImage, noReboot bool) {
	record.Kind = checkpointKindForProviderImage(image)
	record.Native.Provider = firstNonBlank(image.Provider, checkpointProviderForKind(record.Kind), record.Provider)
	record.Native.ImageID = image.ID
	record.Native.Kind = image.Kind
	record.Native.Name = image.Name
	record.Native.State = image.State
	record.Native.Region = image.Region
	record.Native.AccountID = image.AccountID
	record.Native.Project = image.Project
	record.Native.Resource = image.ResourceID
	record.Native.Architecture = image.Architecture
	record.Native.SnapshotIDs = image.SnapshotIDs
	record.Native.Direct = image.Direct
	record.Native.Strategy = checkpointStrategyForKind(record.Kind)
	record.Native.NoReboot = noReboot
}

func applyNativeImageCheckpointRecord(record *checkpointRecord, image CoordinatorImage, noReboot bool) {
	record.applyNativeImage(image, noReboot)
}

func applyAWSAMIImageCheckpointRecord(record *checkpointRecord, image CoordinatorImage, noReboot bool) {
	record.applyNativeImage(image, noReboot)
}

func nativeCheckpointResourceID(record checkpointRecord) string {
	return record.nativeResourceID()
}

func nativeCheckpointDeleteID(record checkpointRecord) string {
	return record.nativeDeleteID()
}

func nativeCheckpointResourceRequest(record checkpointRecord) NativeCheckpointResourceRequest {
	return NativeCheckpointResourceRequest{
		LoadConfig: loadConfig,
		Image: NativeCheckpointImage{
			ID:           record.Native.ImageID,
			Name:         record.Native.Name,
			State:        record.Native.State,
			Provider:     record.nativeProvider(),
			Kind:         record.Kind,
			Region:       record.Native.Region,
			ResourceID:   record.Native.Resource,
			Architecture: record.Native.Architecture,
			Direct:       record.Native.Direct,
		},
		Metadata: record.Native.Metadata,
		Capture:  record.Capture,
	}
}

func checkpointKindForProviderImage(image CoordinatorImage) string {
	switch image.Kind {
	case checkpointKindAWSEBS:
		return checkpointKindAWSEBS
	case checkpointKindAzureOS:
		return checkpointKindAzureOS
	case checkpointKindGCPDisk:
		return checkpointKindGCPDisk
	case checkpointKindHetzner:
		return checkpointKindHetzner
	case checkpointKindMachine0:
		return checkpointKindMachine0
	case checkpointKindDockerCommit:
		return checkpointKindDockerCommit
	case checkpointKindIncus:
		return checkpointKindIncus
	case checkpointKindDaytona:
		return checkpointKindDaytona
	}
	switch image.Provider {
	case "azure":
		return checkpointKindAzure
	case "gcp":
		return checkpointKindGCP
	case "hetzner":
		return checkpointKindHetzner
	case "machine0":
		return checkpointKindMachine0
	case "parallels":
		return checkpointKindParallels
	case "local-container":
		return checkpointKindDockerCommit
	default:
		return checkpointKindAWSAMI
	}
}

func checkpointStrategyForKind(kind string) string {
	switch kind {
	case checkpointKindAWSAMI, checkpointKindAzure, checkpointKindGCP, checkpointKindMachine0, checkpointKindDockerCommit:
		return checkpointStrategyImage
	case checkpointKindAWSEBS, checkpointKindAzureOS, checkpointKindGCPDisk, checkpointKindHetzner, checkpointKindParallels, checkpointKindDaytona, checkpointKindIncus:
		return checkpointStrategyDiskSnapshot
	default:
		return ""
	}
}

func directAWSCheckpointConfig(record checkpointRecord) (Config, bool) {
	if !record.isDirectAWSAMI() {
		return Config{}, false
	}
	cfg, err := loadConfig()
	if err != nil {
		return Config{}, false
	}
	setProviderSelection(&cfg, "aws", providerSelectionRecordedRun)
	if record.Native.Region != "" {
		cfg.AWSRegion = record.Native.Region
	}
	return cfg, true
}

func directAzureCheckpointConfig(record checkpointRecord) (Config, bool) {
	if record.Kind != checkpointKindAzureOS || !record.Native.Direct {
		return Config{}, false
	}
	cfg, err := loadConfig()
	if err != nil {
		return Config{}, false
	}
	setProviderSelection(&cfg, "azure", providerSelectionRecordedRun)
	if record.Native.Region != "" {
		cfg.AzureLocation = record.Native.Region
	}
	resourceID := nativeCheckpointResourceID(record)
	parts := strings.Split(strings.Trim(resourceID, "/"), "/")
	for index := 0; index+1 < len(parts); index += 1 {
		switch {
		case strings.EqualFold(parts[index], "subscriptions"):
			cfg.AzureSubscription = parts[index+1]
		case strings.EqualFold(parts[index], "resourceGroups"):
			cfg.AzureResourceGroup = parts[index+1]
		}
	}
	return cfg, cfg.AzureLocation != "" && cfg.AzureResourceGroup != ""
}

func verifyDirectAzureCheckpoint(ctx context.Context, audit checkpointAudit, cfg Config, providerID string) checkpointAudit {
	client, err := NewAzureClient(ctx, cfg)
	if err != nil {
		audit.ProviderState = "unknown"
		audit.NextAction = "check_auth_or_provider"
		audit.Error = err.Error()
		return audit
	}
	snapshot, err := client.GetOSDiskSnapshot(ctx, providerID)
	if err != nil {
		if isAzureNotFoundError(err) {
			audit.ProviderState = "missing"
			audit.NextAction = "delete_local"
			return audit
		}
		audit.ProviderState = "unknown"
		audit.NextAction = "check_auth_or_provider"
		audit.Error = err.Error()
		return audit
	}
	applyCheckpointImageAudit(&audit, coordinatorImageFromNativeCheckpoint(snapshot))
	return audit
}
func verifyDirectAWSCheckpoint(ctx context.Context, audit checkpointAudit, cfg Config, providerID, expectedAccountID string) checkpointAudit {
	client, clientErr := newAWSClient(ctx, cfg)
	if clientErr != nil {
		audit.ProviderState = "unknown"
		audit.NextAction = "check_auth_or_provider"
		audit.Error = clientErr.Error()
		return audit
	}
	return verifyDirectAWSCheckpointWithClient(ctx, audit, client, providerID, expectedAccountID)
}

func verifyDirectAWSCheckpointWithClient(ctx context.Context, audit checkpointAudit, client *AWSClient, providerID, expectedAccountID string) checkpointAudit {
	if guardErr := client.GuardAccount(ctx, expectedAccountID); guardErr != nil {
		audit.ProviderState = "unknown"
		audit.NextAction = "check_auth_or_provider"
		audit.Error = guardErr.Error()
		return audit
	}
	image, imageErr := client.GetImageCheckpoint(ctx, providerID)
	if imageErr != nil {
		if strings.Contains(imageErr.Error(), "InvalidAMIID.NotFound") || strings.Contains(imageErr.Error(), "aws image not found") {
			audit.ProviderState = "missing"
			audit.NextAction = "delete_local"
			return audit
		}
		audit.ProviderState = "unknown"
		audit.NextAction = "check_auth_or_provider"
		audit.Error = imageErr.Error()
		return audit
	}
	applyCheckpointImageAudit(&audit, image)
	return audit
}

func applyCheckpointImageAudit(audit *checkpointAudit, image CoordinatorImage) {
	audit.ProviderState = blank(image.State, "unknown")
	switch strings.ToLower(image.State) {
	case "available", "ready", "succeeded", "completed":
		audit.NextAction = "fork_or_delete"
	case "failed", "invalid":
		audit.NextAction = "delete"
	default:
		audit.NextAction = "wait_or_delete"
	}
}

func nativeCoordinatorImageRef(record checkpointRecord) CoordinatorImageRef {
	return CoordinatorImageRef{
		Provider: record.nativeProvider(),
		Region:   record.Native.Region,
		Project:  record.Native.Project,
		Kind:     firstNonBlank(record.Native.Kind, record.Kind),
	}
}

func nativeCheckpointForkRecord(record checkpointRecord) NativeCheckpointForkRecord {
	return NativeCheckpointForkRecord{
		Kind:         record.Kind,
		ImageID:      record.Native.ImageID,
		Name:         record.Native.Name,
		Resource:     record.Native.Resource,
		Region:       record.Native.Region,
		Project:      record.Native.Project,
		Direct:       record.Native.Direct,
		HostID:       record.HostID,
		TargetOS:     record.TargetOS,
		WindowsMode:  record.WindowsMode,
		Desktop:      record.Desktop,
		ServerType:   record.ServerType,
		Architecture: record.Native.Architecture,
		Metadata:     record.Native.Metadata,
	}
}

func coordinatorStatusCode(err error) int {
	var httpErr CoordinatorHTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode
	}
	return 0
}

func applyNativeCheckpointForkConfig(cfg *Config, fs *flag.FlagSet, record checkpointRecord) error {
	providerSource := providerSelectionRecordedRun
	if flagWasSet(fs, "provider") {
		providerSource = providerSelectionFlag
	}
	setProviderSelection(cfg, record.nativeProvider(), providerSource)
	if record.Native.Direct {
		cfg.Coordinator = ""
		cfg.CoordToken = ""
		cfg.CoordTokenCommand = nil
	} else if !record.coordinatorManaged() && cfg.CoordAdminToken != "" {
		cfg.CoordToken = cfg.CoordAdminToken
		cfg.CoordTokenCommand = nil
	}
	if record.TargetOS != "" {
		cfg.TargetOS = record.TargetOS
	}
	if record.WindowsMode != "" {
		cfg.WindowsMode = record.WindowsMode
	}
	if record.Desktop {
		// Desktop snapshots must rotate VNC credentials even when the fork omits --desktop.
		cfg.Desktop = true
	}
	provider, err := ProviderFor(cfg.Provider)
	if err != nil {
		return err
	}
	forkProvider, ok := provider.(NativeCheckpointForkProvider)
	if !ok {
		return exit(2, "provider=%s does not support native checkpoint fork config", cfg.Provider)
	}
	azureOSDisk := ""
	azureOSDiskExplicit := flagWasSet(fs, "azure-os-disk")
	if azureOSDiskExplicit {
		azureOSDisk = fs.Lookup("azure-os-disk").Value.String()
	}
	if err := forkProvider.ApplyNativeCheckpointForkConfig(NativeCheckpointForkRequest{
		Config:              cfg,
		Record:              nativeCheckpointForkRecord(record),
		MarketExplicit:      flagWasSet(fs, "market"),
		AzureOSDisk:         azureOSDisk,
		AzureOSDiskExplicit: azureOSDiskExplicit,
	}); err != nil {
		return err
	}
	if !flagWasSet(fs, "type") {
		if record.ServerType != "" && !flagWasSet(fs, "class") {
			cfg.ServerType = record.ServerType
			cfg.ServerTypeExplicit = true
		} else {
			cfg.ServerTypeExplicit = false
			cfg.ServerType = serverTypeForConfig(*cfg)
		}
	}
	return nil
}

func applyNativeCheckpointForkConfigAndFlags(cfg *Config, fs *flag.FlagSet, record checkpointRecord, providerFlags providerFlagValues) error {
	if err := applyNativeCheckpointForkConfig(cfg, fs, record); err != nil {
		return err
	}
	provider, err := ProviderFor(cfg.Provider)
	if err != nil {
		return err
	}
	flagProvider, ok := provider.(NativeCheckpointForkFlagProvider)
	if !ok {
		return nil
	}
	return flagProvider.ApplyNativeCheckpointForkFlags(cfg, fs, providerFlags[provider.Name()])
}

func applyAWSAMICheckpointForkConfig(cfg *Config, fs *flag.FlagSet, record checkpointRecord) error {
	return applyNativeCheckpointForkConfig(cfg, fs, record)
}
