package scaleway

import (
	"context"
	"maps"
	"time"

	instance "github.com/scaleway/scaleway-sdk-go/api/instance/v1"
	"github.com/scaleway/scaleway-sdk-go/scw"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

// Only an attested attempt can supply the initial root identity. Persist it before
// publishing disk/server tags so a lost response never selects a different disk.
func (b *Backend) bindFixedRoot(ctx context.Context, client Client, tx *core.FixedTransaction, item *instance.Server) error {
	claim := tx.Claim
	if err := b.validateFixedServer(client, *claim, item); err != nil {
		return err
	}
	labels := maps.Clone(claim.Labels)
	createdAt := claim.FixedCreateIntent.Attempt["root_created_at"]
	if labels[rootVolumeLabel] == "" {
		root := item.Volumes["0"]
		// Image-created local roots occupy slot 0 but Scaleway reports boot=false.
		if len(item.Volumes) != 1 || root == nil || item.CreationDate == nil || item.CreationDate.IsZero() {
			return core.FixedUncertainCustody(claim.LeaseID)
		}
		labels[rootVolumeLabel], labels[volumePendingLabel] = root.ID, "true"
		createdAt = item.CreationDate.UTC().Format(time.RFC3339Nano)
	}
	candidate := core.CloneLeaseClaim(*claim)
	candidate.Labels, candidate.CloudID = labels, item.ID
	candidate.FixedCreateIntent.Attempt["root_created_at"] = createdAt
	volume, err := inspectFixedRoot(ctx, client, candidate, labels[volumePendingLabel] == "true")
	if err != nil {
		return err
	}
	if labels[volumePendingLabel] == "true" && (volume == nil || volume.Server == nil || volume.Server.ID != item.ID || item.Volumes["0"] == nil || item.Volumes["0"].ID != labels[rootVolumeLabel]) {
		return core.FixedUncertainCustody(claim.LeaseID)
	}
	binding := core.FixedResourceBinding{CloudID: item.ID, ImmutableID: item.ID, Labels: labels}
	if createdAt != "" {
		binding.AttemptValues = map[string]string{"root_created_at": createdAt}
	}
	// Project tag writers are inside the operator trust boundary: like other fixed
	// providers, recovery assumes they preserve the journaled attempt's tags.
	if err := tx.Bind(binding); err != nil {
		return err
	}
	if labels[volumePendingLabel] != "true" {
		return nil
	}
	delete(labels, volumePendingLabel)
	if _, err := client.Instance().UpdateVolume(&instance.UpdateVolumeRequest{Zone: scw.Zone(client.Zone()), VolumeID: labels[rootVolumeLabel],
		Tags: ptrTags(shared.ReplaceCrabboxTags(volume.Tags, tagsFromLabels(labels)))}, scw.WithContext(ctx)); err != nil {
		return err
	}
	tags := shared.ReplaceCrabboxTags(item.Tags, tagsFromLabels(labels))
	if _, err := client.Instance().UpdateServer(&instance.UpdateServerRequest{Zone: scw.Zone(client.Zone()), ServerID: item.ID, Tags: &tags}, scw.WithContext(ctx)); err != nil {
		return err
	}
	item.Tags = tags
	return tx.Observe(core.FixedResourceBinding{Labels: labels})
}

func inspectFixedRoot(ctx context.Context, client Client, claim core.LeaseClaim, allowUntagged bool) (*instance.Volume, error) {
	if claim.Labels[rootVolumeLabel] == "" {
		if claim.CloudID != "" {
			return nil, core.FixedUncertainCustody(claim.LeaseID)
		}
		return nil, nil // A key-only attempt has no disk to delete.
	}
	volume, err := inspectRootVolume(ctx, client, rootVolumeFromLabels(claim.Labels), claim.CloudID, false)
	if err != nil || volume == nil {
		return volume, err
	}
	// Image-created disks and their server share an immutable creation instant.
	// A later attachment alone cannot attest the allocation's original root.
	if createdAt := claim.FixedCreateIntent.Attempt["root_created_at"]; createdAt != "" {
		created, err := time.Parse(time.RFC3339Nano, createdAt)
		if err != nil || volume.CreationDate == nil || !volume.CreationDate.Equal(created) {
			return nil, core.Exit(4, "lease_id_conflict: Scaleway root volume creation differs from its server allocation")
		}
	} else if claim.FixedCreateIntent.Attempt["volume_submitted"] == "" || claim.FixedCreateIntent.Attempt["nonce"] == "" || volume.Name != "crabbox-root-"+claim.FixedCreateIntent.Attempt["nonce"] {
		return nil, core.Exit(4, "lease_id_conflict: Scaleway root volume has no allocation evidence")
	}
	got := labelsFromTags(volume.Tags)
	if allowUntagged && len(got) == 0 {
		return volume, nil
	}
	for _, key := range []string{"crabbox", "lease", "slug", "provider", "target", "fixed_attempt", "fixed_intent_sha256", volumeContractLabel} {
		if got[key] == "" || got[key] != claim.Labels[key] || got[ownershipTagConflictLabel] != "" {
			return nil, core.Exit(4, "lease_id_conflict: Scaleway root volume does not match fixed create intent")
		}
	}
	return volume, nil
}
