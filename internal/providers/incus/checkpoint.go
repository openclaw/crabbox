package incus

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"strings"
	"time"

	"github.com/lxc/incus/v7/shared/api"
	core "github.com/openclaw/crabbox/internal/cli"
)

const checkpointProperty = "user.crabbox.checkpoint"
const sourceProperty = "user.crabbox.source_uuid"

func (Provider) NativeCheckpointCapability(req core.NativeCheckpointRequest) (core.NativeCheckpointCapability, bool) {
	capability := core.NativeCheckpointCapability{Kind: core.CheckpointKindIncus, Direct: true}
	if normalizeInstanceType(req.Config.Incus.InstanceType) != "container" {
		capability.CreateUnsupported = "Incus native checkpoints require a container; VM forks cannot replace credentials offline; use --mode archive"
	}
	return capability, true
}
func (Provider) NativeCheckpointSourceStatusOnly(Config) bool { return true }
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

func rejectAttachedDisks(inst api.Instance) error {
	for name, device := range inst.ExpandedDevices {
		if device["type"] == "disk" && device["path"] != "/" {
			return core.Exit(2, "Incus native checkpoint/fork excludes attached disks (device %s); use --mode archive or a root-disk-only container", name)
		}
	}
	for name, device := range inst.Devices {
		if device["type"] == "disk" && device["path"] != "/" {
			return core.Exit(2, "Incus native checkpoint/fork excludes attached disks (device %s)", name)
		}
	}
	return nil
}

func (Provider) CreateNativeCheckpoint(ctx context.Context, req core.NativeCheckpointCreateRequest) (result core.NativeCheckpointCreateResult, err error) {
	timeout := req.WaitTimeout
	if timeout <= 0 {
		timeout = 45 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cfg := req.Config
	applyDefaults(&cfg)
	client, err := newClient(cfg)
	if err != nil {
		return result, err
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(req.LeaseID)
	if err != nil {
		return result, err
	}
	if !exists || claim.FixedCreateIntent == nil {
		return result, core.Exit(4, "Incus capture requires a durable source claim; create a new lease with this version")
	}
	// The source claim lock also excludes Crabbox stop/reuse while snapshotting.
	err = core.WithDurableLeaseClaimLock(req.LeaseID, func(current *core.LeaseClaim, exists bool, _ func() error) error {
		if !exists || current.FixedCreateIntent == nil || current.FixedCreateIntent.State != "acquired" || current.CloudImmutableID != claim.CloudImmutableID || current.CloudID != req.Server.CloudID {
			return core.Exit(4, "Incus checkpoint source claim changed")
		}
		inst, _, err := client.GetInstance(current.CloudID)
		if err != nil {
			return err
		}
		if err := validateClaimInstance(client, *current, *inst); err != nil {
			return err
		}
		if inst.Type != "container" {
			return core.Exit(2, "Incus native checkpoints require a container; use --mode archive for VMs")
		}
		if err := rejectAttachedDisks(*inst); err != nil {
			return err
		}
		workdir := req.Workdir
		if workdir == "" {
			workdir = core.RemoteJoin(cfg, req.LeaseID, req.RepoName)
		}
		canonical, err := client.CanonicalPath(inst.Name, workdir)
		if err != nil {
			return fmt.Errorf("resolve Incus checkpoint workspace: %w", err)
		}
		if inst.IsActive() {
			mountinfo, err := client.ReadFile(inst.Name, "/proc/1/mountinfo")
			if err != nil {
				return fmt.Errorf("inspect Incus workspace mounts: %w", err)
			}
			if err := validateWorkspaceMounts(canonical, string(mountinfo)); err != nil {
				return err
			}
		}
		metadata := maps.Clone(current.Labels)
		metadata[checkpointProperty], metadata[sourceProperty] = req.CheckpointID, inst.Config["volatile.uuid"]
		metadata["source_name"], metadata["snapshot"] = inst.Name, "crabbox-"+req.CheckpointID
		metadata["ssh_user"], metadata["work_root"] = inst.Config[labelKey("ssh_user")], inst.Config[labelKey("work_root")]
		metadata["checkpoint_state"] = "pending"
		result = core.NativeCheckpointCreateResult{Image: core.NativeCheckpointImage{ID: "pending:" + req.CheckpointID, Name: req.Name, State: "pending", Provider: providerName, Kind: core.CheckpointKindIncus, Direct: true}, Metadata: metadata}
		// The canonical record must survive SIGKILL before the first remote mutation.
		if req.Persist == nil {
			return fmt.Errorf("Incus capture requires durable checkpoint persistence")
		}
		if err := req.Persist(result); err != nil {
			return err
		}
		if err := client.CreateSnapshot(ctx, inst.Name, metadata["snapshot"]); err != nil {
			return fmt.Errorf("Incus snapshot outcome uncertain; checkpoint record retains %s/%s for reconciliation: %w", inst.Name, metadata["snapshot"], err)
		}
		properties := map[string]string{checkpointProperty: req.CheckpointID, sourceProperty: metadata[sourceProperty], identityLabel: metadata[identityLabel]}
		fingerprint, err := client.PublishSnapshot(ctx, inst.Name, metadata["snapshot"], properties)
		if fingerprint != "" {
			result.Image.ID, result.Image.ResourceID = fingerprint, fingerprint
		}
		if err != nil {
			return fmt.Errorf("Incus publish outcome uncertain; checkpoint record retained: %w", err)
		}
		image, err := ownedCheckpointImage(client, result.Image, metadata)
		if err != nil {
			return err
		}
		if image == nil {
			return fmt.Errorf("Incus published image is missing; checkpoint record retained")
		}
		metadata["checkpoint_state"] = "available"
		result.Image.State, result.Image.Architecture = "available", image.Architecture
		if err := req.Persist(result); err != nil {
			return err
		}
		if err := deleteCaptureSnapshot(ctx, client, metadata); err != nil {
			return fmt.Errorf("Incus image is available but temporary snapshot cleanup failed (record retained): %w", err)
		}
		return nil
	})
	return result, err
}

func checkpointClient(metadata map[string]string) (instanceClient, error) {
	cfg, err := configFromMetadata(metadata)
	if err != nil {
		return nil, err
	}
	client, err := newClient(cfg)
	if err != nil {
		return nil, err
	}
	if err := verifyConnection(client, metadata[identityLabel]); err != nil {
		return nil, err
	}
	if metadata[checkpointProperty] == "" || metadata[sourceProperty] == "" {
		return nil, core.Exit(4, "Incus checkpoint has no ownership identity")
	}
	return client, nil
}

func ownedCheckpointImage(client instanceClient, recorded core.NativeCheckpointImage, metadata map[string]string) (*api.Image, error) {
	var image *api.Image
	if strings.HasPrefix(recorded.ID, "pending:") {
		images, err := client.ListImages()
		if err != nil {
			return nil, err
		}
		for i := range images {
			if images[i].Properties[checkpointProperty] == metadata[checkpointProperty] {
				if image != nil {
					return nil, core.Exit(4, "Incus checkpoint matches multiple images")
				}
				image = &images[i]
			}
		}
	} else {
		var err error
		image, err = client.GetImage(recorded.ID)
		if api.StatusErrorCheck(err, 404) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if image.Fingerprint != recorded.ID {
			return nil, core.Exit(4, "Incus checkpoint fingerprint mismatch")
		}
	}
	if image == nil {
		return nil, nil
	}
	if image.Public || image.AutoUpdate || image.Type != "container" ||
		image.Properties[checkpointProperty] != metadata[checkpointProperty] ||
		image.Properties[sourceProperty] != metadata[sourceProperty] || image.Properties[identityLabel] != metadata[identityLabel] {
		return nil, core.Exit(4, "Incus checkpoint image ownership mismatch; refusing use or deletion")
	}
	return image, nil
}

func deleteCaptureSnapshot(ctx context.Context, client instanceClient, metadata map[string]string) error {
	name, snapshot := metadata["source_name"], metadata["snapshot"]
	if name == "" || snapshot != "crabbox-"+metadata[checkpointProperty] {
		return core.Exit(4, "Incus checkpoint snapshot identity is invalid")
	}
	inst, _, err := client.GetInstance(name)
	if api.StatusErrorCheck(err, 404) {
		return nil
	}
	if err != nil {
		return err
	}
	if inst.Config["volatile.uuid"] != metadata[sourceProperty] {
		// Source-owned snapshots disappeared with the original instance. Never
		// touch a replacement instance, even if it has the same snapshot name.
		return nil
	}
	snap, err := client.GetSnapshot(name, snapshot)
	if api.StatusErrorCheck(err, 404) {
		return nil
	}
	if err != nil {
		return err
	}
	if snap.Config["volatile.uuid"] != metadata[sourceProperty] {
		return core.Exit(4, "Incus checkpoint snapshot ownership mismatch")
	}
	return client.DeleteSnapshot(ctx, name, snapshot)
}

func (Provider) VerifyNativeCheckpoint(ctx context.Context, req core.NativeCheckpointResourceRequest) (core.NativeCheckpointVerifyResult, error) {
	client, err := checkpointClient(req.Metadata)
	if err != nil {
		return core.NativeCheckpointVerifyResult{}, err
	}
	image, err := ownedCheckpointImage(client, req.Image, req.Metadata)
	if err != nil {
		return core.NativeCheckpointVerifyResult{}, err
	}
	if image == nil {
		if req.Metadata["checkpoint_state"] != "available" {
			return core.NativeCheckpointVerifyResult{ProviderState: "pending", NextAction: "check_runtime", Error: "Incus capture outcome remains uncertain; retain this record and inspect the recorded snapshot/image operation"}, nil
		}
		return core.NativeCheckpointVerifyResult{ProviderState: "missing", NextAction: "delete_local"}, nil
	}
	return core.NativeCheckpointVerifyResult{ProviderState: "available", NextAction: "fork_or_delete"}, nil
}

func (Provider) DeleteNativeCheckpoint(ctx context.Context, req core.NativeCheckpointResourceRequest) error {
	client, err := checkpointClient(req.Metadata)
	if err != nil {
		return err
	}
	image, err := ownedCheckpointImage(client, req.Image, req.Metadata)
	if err != nil {
		return err
	}
	if image == nil && req.Metadata["checkpoint_state"] != "available" {
		return core.Exit(4, "Incus capture outcome is still uncertain; refusing to erase its cleanup identity")
	}
	if image != nil {
		// Recovery by properties is not enough: commit the exact image before any
		// deletion so a lost reply can reconcile absence without losing identity.
		if req.Persist == nil {
			return fmt.Errorf("Incus deletion requires durable checkpoint persistence")
		}
		metadata := maps.Clone(req.Metadata)
		metadata["checkpoint_state"] = "available"
		recorded := req.Image
		recorded.ID, recorded.ResourceID, recorded.State = image.Fingerprint, image.Fingerprint, "available"
		if err := req.Persist(core.NativeCheckpointCreateResult{Image: recorded, Metadata: metadata}); err != nil {
			return err
		}
	}
	// Clear the temporary source-owned snapshot first; image and record survive failures.
	if err := deleteCaptureSnapshot(ctx, client, req.Metadata); err != nil {
		return err
	}
	if image == nil {
		return nil
	}
	if err := client.DeleteImage(ctx, image.Fingerprint); err != nil {
		return fmt.Errorf("delete Incus checkpoint image (record retained): %w", err)
	}
	return nil
}

func (Provider) ApplyNativeCheckpointForkConfig(req core.NativeCheckpointForkRequest) error {
	if req.Record.Kind != core.CheckpointKindIncus {
		return core.Exit(2, "invalid Incus checkpoint kind")
	}
	cfg, err := configFromMetadata(req.Record.Metadata)
	if err != nil {
		return err
	}
	client, err := checkpointClient(req.Record.Metadata)
	if err != nil {
		return err
	}
	image, err := ownedCheckpointImage(client, core.NativeCheckpointImage{ID: req.Record.ImageID}, req.Record.Metadata)
	if err != nil {
		return err
	}
	if image == nil {
		return core.Exit(4, "Incus checkpoint image is not available")
	}
	// Keep current launch/proxy settings, but pin control-plane routing and source user.
	req.Config.Incus.Remote, req.Config.Incus.Address, req.Config.Incus.Socket = cfg.Incus.Remote, cfg.Incus.Address, cfg.Incus.Socket
	req.Config.Incus.Project, req.Config.Incus.TLSServerCert, req.Config.Incus.InsecureTLS = cfg.Incus.Project, cfg.Incus.TLSServerCert, cfg.Incus.InsecureTLS
	req.Config.Incus.Image, req.Config.Incus.InstanceType = image.Fingerprint, "container"
	req.Config.Incus.User, req.Config.SSHUser = req.Record.Metadata["ssh_user"], req.Record.Metadata["ssh_user"]
	req.Config.Incus.WorkRoot, req.Config.WorkRoot = req.Record.Metadata["work_root"], req.Record.Metadata["work_root"]
	req.Config.Incus.CheckpointMetadata = maps.Clone(req.Record.Metadata)
	req.Config.ServerType = core.IncusServerTypeForConfig(*req.Config)
	return nil
}

func validateForkImage(client instanceClient, cfg Config) error {
	if err := verifyConnection(client, cfg.Incus.CheckpointMetadata[identityLabel]); err != nil {
		return err
	}
	image, err := ownedCheckpointImage(client, core.NativeCheckpointImage{ID: cfg.Incus.Image}, cfg.Incus.CheckpointMetadata)
	if err != nil {
		return err
	}
	if image == nil {
		return errors.New("Incus checkpoint image is missing")
	}
	return nil
}

func validateWorkspaceMounts(workdir, mountinfo string) error {
	if !path.IsAbs(workdir) || strings.TrimSpace(mountinfo) == "" {
		return core.Exit(2, "Incus checkpoint requires a verifiable root-disk workspace")
	}
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for _, line := range strings.Split(strings.TrimSpace(mountinfo), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			return fmt.Errorf("invalid Incus source mount inventory")
		}
		mount := path.Clean(unescape.Replace(fields[4]))
		if mount == "/" {
			continue
		}
		if workdir == mount || strings.HasPrefix(workdir, mount+"/") || strings.HasPrefix(mount, strings.TrimSuffix(workdir, "/")+"/") {
			return core.Exit(2, "Incus native checkpoint cannot capture workspace %s intersecting guest mount %s; use --mode archive", workdir, mount)
		}
	}
	return nil
}
