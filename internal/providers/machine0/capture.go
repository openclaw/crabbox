package machine0

import (
	"context"
	"fmt"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *backend) advanceCheckpointCapture(ctx context.Context, req core.NativeCheckpointCreateRequest, claim LeaseClaim) (result core.NativeCheckpointCreateResult, err error) {
	if reason := checkpointRetirementPolicy(b.configForRun(), claim.Labels); reason != "" {
		return result, exit(2, "%s", reason)
	}
	if err := core.ValidateCheckpointCaptureClaim(claim, req.CheckpointID, req.Capture); err != nil {
		return result, err
	}
	if req.Persist == nil {
		return result, exit(2, "replayable checkpoint requires durable observation")
	}
	name := "crabbox-" + strings.TrimPrefix(req.CheckpointID, "chk_")
	metadata := req.Metadata
	if metadata == nil {
		metadata = map[string]string{metadataImageName: name, metadataCreatedImage: "true", metadataSourceMachine: claim.CloudID, "crabbox_checkpoint": req.CheckpointID, "crabbox_lease": claim.LeaseID}
	}
	if metadata[metadataImageName] != name || metadata[metadataSourceMachine] != claim.CloudID {
		return result, exit(2, "checkpoint capture metadata conflicts with its source binding")
	}
	result.Metadata = metadata
	req.Metadata = metadata
	remoteMetadata := map[string]string{"crabbox_checkpoint": req.CheckpointID, "crabbox_lease": claim.LeaseID, "crabbox_source": claim.CloudID}
	persist := func(phase string) error { req.Capture.Phase = phase; return req.Persist(result) }
	err = withClaimUnchanged(claim.LeaseID, claim, func() error {
		if metadata[metadataAccountID] == "" && req.Capture.Phase != "prepared" {
			return exit(2, "checkpoint has no captured Machine0 account identity; retain source for recovery")
		}
		accountID, err := b.api.AccountID(ctx)
		if err != nil {
			return err
		}
		if accountID == "" || (metadata[metadataAccountID] != "" && metadata[metadataAccountID] != accountID) {
			return exit(2, "Machine0 checkpoint account identity changed; retain source")
		}
		readSource := func() (machine, error) {
			if err := b.attestCheckpointAccount(ctx, metadata); err != nil {
				return machine{}, err
			}
			return b.readCheckpointSource(ctx, claim, req.Capture.SourceName)
		}
		item, err := b.readCheckpointSource(ctx, claim, req.Capture.SourceName)
		if err != nil {
			return err
		}
		if metadata[metadataAccountID] == "" {
			metadata[metadataAccountID] = accountID
			if err := req.Persist(result); err != nil {
				return err
			}
		}
		if err := b.attestCheckpointAccount(ctx, metadata); err != nil {
			return err
		}
		if req.Capture.Phase == "prepared" {
			images, err := b.api.ListImages(ctx)
			if err != nil {
				return err
			}
			for _, image := range images {
				if image.Name == name {
					return exit(2, "checkpoint image name already exists before submission; retain operation for inspection")
				}
			}
			if err := persist("stopping"); err != nil {
				return err
			}
		}
		if req.Capture.Phase == "stopping" {
			switch strings.ToUpper(strings.TrimSpace(item.Status)) {
			case "RUNNING":
				lease, err := b.prepareLease(ctx, item, req.Server, claim.LeaseID, false)
				if err != nil {
					return err
				}
				if err := b.prepareNativeImageSource(ctx, lease.SSH); err != nil {
					return err
				}
				item, err = readSource()
				if err != nil {
					return err
				}
				if !machineRunning(item.Status) {
					return nil
				}
				if err := b.api.Stop(ctx, item.Name); err != nil {
					return err
				}
				item, err = readSource()
				if err != nil {
					return err
				}
			case "STARTING", "STOPPING":
				return nil
			case "STOPPED":
			default:
				return exit(5, "checkpoint source state=%s cannot be captured; retain operation", item.Status)
			}
			if !strings.EqualFold(item.Status, "STOPPED") {
				return nil
			}
			if err := persist("submitting"); err != nil {
				return err
			}
			item, err = readSource()
			if err != nil {
				return err
			}
			if !strings.EqualFold(item.Status, "STOPPED") {
				return exit(5, "checkpoint source left STOPPED before submission; retain operation")
			}
			// A lost response (including death before invocation) stays submitting.
			// Absence from a later inventory never authorizes another save.
			if err := b.api.SaveImage(ctx, item.Name, name, remoteMetadata); err != nil {
				return err
			}
		}
		if req.Capture.Phase == "failed" {
			return nil
		}
		images, err := b.api.ListImages(ctx)
		if err != nil {
			return err
		}
		imageID := ""
		for _, image := range images {
			if image.Name == name {
				if imageID != "" || image.ID == "" {
					return exit(2, "checkpoint image inventory is ambiguous")
				}
				imageID = image.ID
				if metadata[metadataImageID] != "" && metadata[metadataImageID] != image.ID {
					return exit(2, "checkpoint image identity changed")
				}
			}
		}
		if imageID == "" {
			req.Capture.Error = "save submission is unresolved; retain source and replay this operation, never submit a second save"
			return req.Persist(result)
		}
		detail, err := b.api.GetImage(ctx, name)
		if err != nil {
			return err
		}
		if detail.Image.Name != name || detail.Image.ID != imageID {
			return exit(2, "checkpoint image identity changed")
		}
		var version machineImageVersion
		for _, candidate := range detail.Versions {
			if machine0ImageVersionMetadataMatches(candidate, remoteMetadata) {
				if version.Version != 0 {
					return exit(2, "multiple versions match checkpoint operation; retain all references")
				}
				version = candidate
			}
		}
		if version.Version == 0 {
			return exit(2, "checkpoint image has no exact operation version; retain source")
		}
		if metadata[metadataImageVersion] != "" && metadata[metadataImageVersion] != fmt.Sprint(version.Version) {
			return exit(2, "checkpoint image version changed")
		}
		result = machine0NativeCheckpointResult(req, claim, name, true, detail, version)
		req.Capture.Error = ""
		phase := "pending"
		if imageVersionReady(version) {
			phase = "ready"
		}
		if imageVersionTerminal(version) {
			phase = "failed"
			req.Capture.Error = "native snapshot failed; retain exact image and source for operator recovery"
		}
		return persist(phase)
	})
	return result, err
}

func (b *backend) releaseCheckpointSource(ctx context.Context, req ReleaseLeaseRequest) error {
	claim, exists, err := core.ReadLeaseClaimWithPresence(req.Lease.LeaseID)
	if err != nil {
		return err
	}
	if !exists {
		return exit(2, "checkpoint source claim is missing; retain the operation")
	}
	if err := core.AuthorizeCheckpointRelease(claim, req.CheckpointID); err != nil {
		return err
	}
	if reason := checkpointRetirementPolicy(b.configForRun(), claim.Labels); reason != "" {
		return exit(2, "%s", reason)
	}
	return fixedMachine0LeaseKind.FinalizeAfterCleanup(claim, func() error {
		resource, err := core.AuthorizedCheckpointReleaseResource(claim, req.CheckpointID)
		if err != nil {
			return err
		}
		name := claim.Labels["machine0_name"]
		if name == "" || req.Lease.Server.CloudID != claim.CloudID {
			return exit(2, "checkpoint source release identity changed")
		}
		findSource := func() (bool, error) {
			if err := b.attestCheckpointAccount(ctx, resource.Metadata); err != nil {
				return false, err
			}
			items, err := b.api.List(ctx)
			if err != nil {
				return false, err
			}
			found := false
			for _, item := range items {
				if item.Name == name && item.ID != claim.CloudID {
					return false, exit(2, "checkpoint source name now belongs to a replacement")
				}
				if item.ID == claim.CloudID {
					if item.Name != name {
						return false, exit(2, "checkpoint source name changed")
					}
					found = true
				}
			}
			return found, b.attestCheckpointAccount(ctx, resource.Metadata)
		}
		found, err := findSource()
		if err != nil || !found {
			return err
		}
		item, err := b.readCheckpointSource(ctx, claim, name)
		if err != nil {
			return err
		}
		switch strings.ToUpper(strings.TrimSpace(item.Status)) {
		case "STARTING", "STOPPING", "DELETING":
			return core.ErrCheckpointPending
		case "RUNNING", "STOPPED", "ERRORED":
		default:
			return exit(5, "checkpoint source state=%s is not safe to retire; retain operation", item.Status)
		}
		if err := b.attestCheckpointAccount(ctx, resource.Metadata); err != nil {
			return err
		}
		removeErr := b.api.Remove(ctx, name)
		found, err = findSource()
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		if removeErr != nil {
			return removeErr
		}
		return core.ErrCheckpointPending
	})
}

func (b *backend) CheckpointSourceAbsent(ctx context.Context, req core.CheckpointSourceRequest) (bool, error) {
	if err := b.attestCheckpointAccount(ctx, req.Resource.Metadata); err != nil {
		return false, err
	}
	items, err := b.api.List(ctx)
	if err != nil {
		return false, err
	}
	for _, item := range items {
		if item.Name == req.Capture.SourceName && item.ID != req.Capture.SourceID {
			return false, exit(2, "checkpoint source name now belongs to a replacement")
		}
		if item.ID == req.Capture.SourceID {
			return false, nil
		}
	}
	if err := b.attestCheckpointAccount(ctx, req.Resource.Metadata); err != nil {
		return false, err
	}
	return true, nil
}

func (b *backend) attestCheckpointAccount(ctx context.Context, metadata map[string]string) error {
	expected := metadata[metadataAccountID]
	if expected == "" {
		return exit(2, "checkpoint has no captured Machine0 account identity; retain source for recovery")
	}
	actual, err := b.api.AccountID(ctx)
	if err != nil {
		return err
	}
	if actual != expected {
		return exit(2, "Machine0 checkpoint account identity changed; retain checkpoint and source")
	}
	return nil
}
