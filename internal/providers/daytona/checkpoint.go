package daytona

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	api "github.com/daytonaio/daytona/libs/api-client-go"
	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

var checkpointPollInterval = 2 * time.Second
var checkpointRecoveryTimeout = 3 * time.Minute
var checkpointIDPattern = regexp.MustCompile(`^chk_[a-f0-9]{16}$`)
var checkpointNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

func (Provider) NativeCheckpointSourceStatusOnly(cfg Config) bool {
	return cfg.Coordinator == "" && cfg.TargetOS == core.TargetLinux
}

func (Provider) NativeCheckpointCapability(req core.NativeCheckpointRequest) (core.NativeCheckpointCapability, bool) {
	if req.Config.Coordinator != "" || req.Config.TargetOS != core.TargetLinux || req.Server.CloudID == "" {
		return core.NativeCheckpointCapability{}, false
	}
	return core.NativeCheckpointCapability{Kind: core.CheckpointKindDaytona, Direct: true}, true
}

func (Provider) NativeCheckpointWorkdir(req core.NativeCheckpointWorkdirRequest) string {
	if req.Override != "" {
		return req.Override
	}
	cfg := req.Config
	cfg.WorkRoot = shared.FirstNonBlank(req.Server.Labels["work_root"], daytonaWorkRoot(cfg))
	return core.RemoteJoin(cfg, req.LeaseID, req.RepoName)
}

func snapshotClient(cfg Config, rt Runtime) (daytonaSnapshotAPI, error) {
	client, err := newDaytonaClient(cfg, rt)
	if err != nil {
		return nil, err
	}
	snapshots, ok := client.(daytonaSnapshotAPI)
	if !ok {
		return nil, errors.New("Daytona client does not support snapshots")
	}
	return snapshots, nil
}

func (Provider) CreateNativeCheckpoint(ctx context.Context, req core.NativeCheckpointCreateRequest) (result core.NativeCheckpointCreateResult, err error) {
	if _, ok := (Provider{}).NativeCheckpointCapability(core.NativeCheckpointRequest{Config: req.Config, Server: req.Server}); !ok {
		return result, exit(2, "Daytona native checkpoints require a direct Linux lease")
	}
	if !checkpointIDPattern.MatchString(req.CheckpointID) {
		return result, exit(2, "Daytona snapshot requires a canonical checkpoint ID")
	}
	name := "crabbox-" + req.CheckpointID
	if req.Name != "" {
		if len(req.Name) > 64 || !checkpointNamePattern.MatchString(req.Name) {
			return result, exit(2, "Daytona checkpoint name must be 1-64 letters, digits, dots, underscores, or hyphens, starting with a letter or digit")
		}
		name = req.Name + "-" + req.CheckpointID
	}
	rt := core.RuntimeForProviderOperation(req.Stderr)
	client, err := snapshotClient(req.Config, rt)
	if err != nil {
		return result, err
	}
	auth, err := daytonaAuthConfig(req.Config)
	if err != nil {
		return result, err
	}
	claim, claimed, err := resolveLeaseClaimForProvider(req.LeaseID, daytonaProvider)
	if err != nil {
		return result, err
	}
	if !claimed || claim.CloudID != req.Server.CloudID {
		return result, exit(4, "Daytona checkpoint requires an exact source lease claim")
	}
	timeout := req.WaitTimeout
	if timeout <= 0 {
		timeout = 45 * time.Minute
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err = core.WithLeaseClaimUnchanged(req.LeaseID, claim, func() (opErr error) {
		source, err := client.GetSandbox(waitCtx, req.Server.CloudID)
		if err != nil {
			return err
		}
		if lease, owned := daytonaSandboxOwnership(source); !owned || lease != req.LeaseID || source.GetId() != claim.CloudID {
			return exit(4, "Daytona checkpoint source ownership mismatch")
		}
		org := source.GetOrganizationId()
		if org == "" || auth.OrganizationID != "" && auth.OrganizationID != org {
			return exit(4, "Daytona source organization is missing or mismatched")
		}
		state := daytonaSandboxState(source)
		if state != "started" && state != "stopped" {
			return exit(2, "Daytona snapshot source must be started or stopped; state=%s", state)
		}
		if state == "started" && req.NoReboot {
			return exit(2, "Daytona filesystem snapshots require a stopped source; rerun with --no-reboot=false")
		}
		if _, err := client.GetSnapshot(waitCtx, name); err == nil {
			return exit(2, "Daytona snapshot %s already exists", name)
		} else if !daytonaIsNotFoundError(err) {
			return err
		}
		metadata := map[string]string{"api_url": daytonaAPIURL(req.Config, auth), "organization": org, "checkpoint": req.CheckpointID, "source": source.GetId(), "work_root": shared.FirstNonBlank(source.GetLabels()["work_root"], daytonaWorkRoot(req.Config)), "user": shared.FirstNonBlank(source.GetUser(), daytonaUser(req.Config)), "target": source.GetTarget()}
		stoppedByUs, snapshotRequested, snapshotSettled := false, false, false
		defer func() {
			if !stoppedByUs {
				return
			}
			recoveryCtx, cancel := context.WithTimeout(context.Background(), checkpointRecoveryTimeout)
			defer cancel()
			// An uncertain snapshot request may still be accepted asynchronously.
			// Never restart the source until the snapshot barrier is observed.
			if snapshotRequested && !snapshotSettled {
				snap, waitErr := waitDaytonaSnapshot(recoveryCtx, client, name, org, result.Metadata["snapshot_id"])
				if snap != nil {
					result = daytonaCheckpointResult(snap, metadata)
				}
				if waitErr != nil && (snap == nil || !daytonaSnapshotFailed(snap.GetState())) {
					opErr = errors.Join(opErr, fmt.Errorf("source sandbox %s left stopped: snapshot acceptance/completion is unconfirmed; inspect snapshot %s before restarting: %w", source.GetId(), name, waitErr))
					return
				}
				opErr = errors.Join(opErr, waitErr)
			}
			if err := waitDaytonaStopped(recoveryCtx, client, source.GetId()); err != nil {
				opErr = errors.Join(opErr, fmt.Errorf("source sandbox %s could not be confirmed stopped before restart: %w", source.GetId(), err))
				return
			}
			if _, err := client.StartSandbox(recoveryCtx, source.GetId()); err != nil {
				opErr = errors.Join(opErr, fmt.Errorf("restart Daytona source %s: %w", source.GetId(), err))
				return
			}
			if _, err := waitForDaytonaReady(recoveryCtx, client, source.GetId(), checkpointRecoveryTimeout); err != nil {
				opErr = errors.Join(opErr, err)
			}
		}()
		if state == "started" {
			toolbox, err := newDaytonaToolboxSandbox(req.Config, rt, source)
			if err != nil {
				return err
			}
			response, err := newDaytonaCommandRunner(toolbox).ExecuteCommand(waitCtx, "sync")
			if err != nil {
				return err
			}
			if responseExitCode(response) != 0 {
				return errors.New("flush Daytona checkpoint source failed")
			}
			stoppedByUs = true
			if err := client.StopSandbox(waitCtx, source.GetId()); err != nil {
				return err
			}
			if err := waitDaytonaStopped(waitCtx, client, source.GetId()); err != nil {
				return err
			}
		}
		// Preserve a recovery record even when allocation's response is lost.
		result = core.NativeCheckpointCreateResult{Image: core.NativeCheckpointImage{ID: name, Name: name, Provider: daytonaProvider, Kind: core.CheckpointKindDaytona, State: "pending", Direct: true}, Metadata: metadata}
		snapshotRequested = true
		createErr := client.CreateSnapshot(waitCtx, source.GetId(), name)
		if daytonaSnapshotRequestRejected(createErr) {
			snapshotRequested = false
			result = core.NativeCheckpointCreateResult{}
			return createErr
		}
		snap, waitErr := waitDaytonaSnapshot(waitCtx, client, name, org, "")
		if snap != nil {
			result = daytonaCheckpointResult(snap, metadata)
		}
		snapshotSettled = waitErr == nil || snap != nil && daytonaSnapshotFailed(snap.GetState())
		return errors.Join(createErr, waitErr)
	})
	return result, err
}

func daytonaCheckpointResult(snap *api.SnapshotDto, metadata map[string]string) core.NativeCheckpointCreateResult {
	metadata["snapshot_id"] = snap.GetId()
	return core.NativeCheckpointCreateResult{Image: core.NativeCheckpointImage{ID: snap.GetId(), Name: snap.GetName(), State: string(snap.GetState()), Provider: daytonaProvider, Kind: core.CheckpointKindDaytona, Direct: true}, Metadata: metadata}
}

func daytonaSnapshotFailed(state api.SnapshotState) bool {
	return state == api.SNAPSHOTSTATE_ERROR || state == api.SNAPSHOTSTATE_BUILD_FAILED
}

func daytonaSnapshotRequestRejected(err error) bool {
	var apiErr *api.GenericOpenAPIError
	if !errors.As(err, &apiErr) {
		return false
	}
	for _, code := range []string{"400 ", "401 ", "403 ", "404 ", "409 ", "422 ", "429 "} {
		if strings.HasPrefix(apiErr.Error(), code) {
			return true
		}
	}
	return false
}

func waitDaytonaSnapshot(ctx context.Context, client daytonaSnapshotAPI, name, org, expectedID string) (*api.SnapshotDto, error) {
	var last *api.SnapshotDto
	for {
		snap, err := client.GetSnapshot(ctx, name)
		if err != nil && !daytonaIsNotFoundError(err) {
			return last, err
		}
		if err == nil {
			if snap == nil || snap.GetId() == "" || snap.GetName() != name || snap.GetOrganizationId() != org || snap.GetGeneral() {
				return last, exit(4, "Daytona snapshot identity or organization mismatch")
			}
			if expectedID != "" && expectedID != snap.GetId() {
				return last, exit(4, "Daytona snapshot identity changed while waiting")
			}
			expectedID = snap.GetId()
			last = snap
			if snap.GetState() == api.SNAPSHOTSTATE_ACTIVE {
				return snap, nil
			}
			if daytonaSnapshotFailed(snap.GetState()) {
				return snap, exit(5, "Daytona snapshot %s failed: state=%s", name, snap.GetState())
			}
		}
		if err := shared.SleepContext(ctx, checkpointPollInterval); err != nil {
			return last, fmt.Errorf("wait for Daytona snapshot %s: %w", name, err)
		}
	}
}

func waitDaytonaStopped(ctx context.Context, client daytonaAPI, id string) error {
	for {
		sandbox, err := client.GetSandbox(ctx, id)
		if err != nil {
			return err
		}
		state := daytonaSandboxState(sandbox)
		if state == "stopped" {
			return nil
		}
		if daytonaStateFailed(state) {
			return exit(5, "Daytona source %s entered state=%s", id, state)
		}
		if err := shared.SleepContext(ctx, checkpointPollInterval); err != nil {
			return err
		}
	}
}

func daytonaCheckpointConfig(req core.NativeCheckpointResourceRequest) (Config, error) {
	cfg := req.Config
	if cfg.Provider == "" {
		var err error
		cfg, err = core.LoadConfig()
		if err != nil {
			return cfg, err
		}
	}
	cfg.Provider, cfg.Coordinator = daytonaProvider, ""
	if req.Image.Provider != daytonaProvider || req.Image.Kind != core.CheckpointKindDaytona || !req.Image.Direct || req.Image.ID == "" || req.Image.Name == "" || req.Metadata["organization"] == "" || !checkpointIDPattern.MatchString(req.Metadata["checkpoint"]) || req.Metadata["source"] == "" {
		return cfg, exit(4, "Daytona checkpoint is missing exact ownership metadata")
	}
	if req.Metadata["snapshot_id"] != req.Image.ID {
		return cfg, exit(4, "Daytona snapshot ID is unconfirmed; inspect snapshot %s and retain the recovery record", req.Image.Name)
	}
	auth, err := daytonaAuthConfig(cfg)
	if err != nil {
		return cfg, err
	}
	if req.Metadata["api_url"] != daytonaAPIURL(cfg, auth) || auth.OrganizationID != "" && auth.OrganizationID != req.Metadata["organization"] {
		return cfg, exit(4, "Daytona checkpoint API or organization scope mismatch")
	}
	cfg.Daytona.OrganizationID = req.Metadata["organization"]
	return cfg, nil
}

func loadDaytonaCheckpoint(ctx context.Context, client daytonaSnapshotAPI, req core.NativeCheckpointResourceRequest) (*api.SnapshotDto, error) {
	snap, err := client.GetSnapshot(ctx, req.Image.ID)
	if err != nil {
		return nil, err
	}
	if snap == nil || snap.GetId() != req.Image.ID || snap.GetName() != req.Image.Name || snap.GetOrganizationId() != req.Metadata["organization"] || snap.GetGeneral() {
		return nil, exit(4, "Daytona snapshot ownership mismatch")
	}
	return snap, nil
}

func (Provider) VerifyNativeCheckpoint(ctx context.Context, req core.NativeCheckpointResourceRequest) (core.NativeCheckpointVerifyResult, error) {
	cfg, err := daytonaCheckpointConfig(req)
	if err != nil {
		return core.NativeCheckpointVerifyResult{}, err
	}
	client, err := snapshotClient(cfg, core.RuntimeForProviderOperation(nil))
	if err != nil {
		return core.NativeCheckpointVerifyResult{}, err
	}
	snap, err := loadDaytonaCheckpoint(ctx, client, req)
	if err != nil {
		return core.NativeCheckpointVerifyResult{}, err
	}
	next := "wait_or_delete"
	if snap.GetState() == api.SNAPSHOTSTATE_ACTIVE {
		next = "fork_or_delete"
	} else if daytonaSnapshotFailed(snap.GetState()) {
		next = "delete"
	}
	return core.NativeCheckpointVerifyResult{ProviderState: string(snap.GetState()), NextAction: next}, nil
}

func (Provider) DeleteNativeCheckpoint(ctx context.Context, req core.NativeCheckpointResourceRequest) error {
	cfg, err := daytonaCheckpointConfig(req)
	if err != nil {
		return err
	}
	client, err := snapshotClient(cfg, core.RuntimeForProviderOperation(nil))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, checkpointRecoveryTimeout)
	defer cancel()
	if _, err := loadDaytonaCheckpoint(ctx, client, req); err != nil {
		return err
	}
	if err := client.DeleteSnapshot(ctx, req.Image.ID); err != nil {
		return err
	}
	for {
		if _, err := loadDaytonaCheckpoint(ctx, client, req); daytonaIsNotFoundError(err) {
			return nil
		} else if err != nil {
			return err
		}
		if err := shared.SleepContext(ctx, checkpointPollInterval); err != nil {
			return err
		}
	}
}

func (Provider) ApplyNativeCheckpointForkConfig(req core.NativeCheckpointForkRequest) error {
	resource := core.NativeCheckpointResourceRequest{Config: *req.Config, Image: core.NativeCheckpointImage{ID: req.Record.ImageID, Name: req.Record.Name, Provider: daytonaProvider, Kind: req.Record.Kind, Direct: req.Record.Direct}, Metadata: req.Record.Metadata}
	cfg, err := daytonaCheckpointConfig(resource)
	if err != nil {
		return err
	}
	client, err := snapshotClient(cfg, core.RuntimeForProviderOperation(nil))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	snap, err := loadDaytonaCheckpoint(ctx, client, resource)
	if err != nil {
		return err
	}
	if snap.GetState() != api.SNAPSHOTSTATE_ACTIVE {
		return exit(2, "Daytona snapshot %s is not active: state=%s", snap.GetName(), snap.GetState())
	}
	cfg.Daytona.Snapshot = snap.GetId()
	cfg.Daytona.Target = req.Record.Metadata["target"]
	cfg.Daytona.User = req.Record.Metadata["user"]
	cfg.Daytona.WorkRoot = req.Record.Metadata["work_root"]
	cfg.WorkRoot = daytonaWorkRoot(cfg)
	*req.Config = cfg
	return nil
}
