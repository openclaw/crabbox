package machine0

import (
	"context"
	"maps"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

const metadataSourceCleanupAccountID = "machine0_source_cleanup_account_id"

// The caller holds the current claim fence. This observes source ownership only;
// it cannot recover the original image account or infer whether a save occurred.
func (b *backend) PrepareNativeCheckpointAbandon(ctx context.Context, req core.NativeCheckpointCreateRequest) (map[string]string, error) {
	claim, exists, err := resolveClaim(req.LeaseID)
	if err != nil {
		return nil, err
	}
	capture := req.Capture
	if !exists || claim.LeaseID != req.LeaseID || claim.CheckpointCapture != nil || claim.Revision == "" ||
		capture == nil || capture.SourceDisposition != "abandon" || capture.Phase != "prepared" ||
		capture.SourceID != claim.CloudID || capture.SourceName != claim.Labels["machine0_name"] ||
		capture.SourceScope != claim.ProviderScope || capture.SourceRevision != claim.Revision ||
		capture.SourceClaimedAt != claim.ClaimedAt || capture.SourceName == "" ||
		req.Server.CloudID != claim.CloudID || req.Server.Name != capture.SourceName {
		return nil, exit(2, "checkpoint abandonment requires the exact current unbound Machine0 source claim")
	}
	intent := ""
	if claim.FixedCreateIntent != nil {
		intent = claim.FixedCreateIntent.Fingerprint
	}
	if capture.SourceIntent != intent {
		return nil, exit(2, "checkpoint abandonment source intent changed")
	}
	if reason := checkpointRetirementPolicy(b.configForRun(), claim.Labels); reason != "" {
		return nil, exit(2, "%s", reason)
	}
	if req.Metadata[metadataImageID] != "" || req.Metadata[metadataImageVersion] != "" {
		return nil, exit(2, "checkpoint abandonment cannot discard a recorded Machine0 image identity")
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "crabbox-" + strings.ToLower(strings.TrimPrefix(req.CheckpointID, "chk_"))
	}
	metadata := maps.Clone(req.Metadata)
	if metadata == nil {
		metadata = make(map[string]string)
	}
	for key, expected := range map[string]string{
		metadataImageName:              name,
		metadataSourceMachine:          claim.CloudID,
		"crabbox_checkpoint":           req.CheckpointID,
		"crabbox_lease":                claim.LeaseID,
		"crabbox_source":               claim.CloudID,
		"machine0_original_submission": "unknown",
	} {
		if expected == "" || metadata[key] != "" && metadata[key] != expected {
			return nil, exit(2, "checkpoint abandonment metadata conflicts with its source binding: %s", key)
		}
		metadata[key] = expected
	}
	accountID, err := b.api.AccountID(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(accountID) == "" || metadata[metadataSourceCleanupAccountID] != "" && metadata[metadataSourceCleanupAccountID] != accountID {
		return nil, exit(2, "Machine0 checkpoint source cleanup account identity changed; retain source")
	}
	metadata[metadataSourceCleanupAccountID] = accountID
	if _, err := b.readCheckpointSource(ctx, claim, capture.SourceName); err != nil {
		return nil, err
	}
	if err := b.attestCheckpointAccount(ctx, accountID); err != nil {
		return nil, err
	}
	return metadata, nil
}

func (b *backend) attestCheckpointSourceAccount(ctx context.Context, metadata map[string]string, capture *core.NativeCheckpointCapture) error {
	if capture == nil {
		return exit(2, "checkpoint source cleanup has no operation identity; retain source")
	}
	key := metadataAccountID
	switch capture.SourceDisposition {
	case "retire":
	case "abandon":
		// Cleanup attests only the source's current account. Never borrow the
		// original image account, or promote cleanup provenance to image authority.
		key = metadataSourceCleanupAccountID
	default:
		return exit(2, "checkpoint source cleanup has an unknown disposition; retain source")
	}
	return b.attestCheckpointAccount(ctx, metadata[key])
}
