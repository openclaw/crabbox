package scaleway

import (
	"context"
	"maps"

	instance "github.com/scaleway/scaleway-sdk-go/api/instance/v1"
	"github.com/scaleway/scaleway-sdk-go/scw"

	core "github.com/openclaw/crabbox/internal/cli"
)

// Journal the allocation-created root before server creation so inventory
// recovery never has to infer disk ownership from the server's attachments.
func prepareFixedRoot(ctx context.Context, client Client, tx *core.FixedTransaction, create bool) error {
	api := client.Instance()
	claim := tx.Claim
	zone := scw.Zone(client.Zone())
	labels := maps.Clone(claim.Labels)
	name := "crabbox-root-" + claim.FixedCreateIntent.Attempt["nonce"]
	validate := func(volume *instance.Volume) bool {
		if volume == nil || volume.ID == "" || labels[rootVolumeLabel] != "" && volume.ID != labels[rootVolumeLabel] || volume.Project != client.ProjectID() || volume.Zone != zone || volume.Name != name || volume.Server != nil {
			return false
		}
		got := labelsFromTags(volume.Tags)
		for _, key := range []string{"crabbox", "lease", "slug", "provider", "target", "fixed_attempt", "fixed_intent_sha256", volumeContractLabel} {
			if got[key] != labels[key] || got[key] == "" || got[ownershipTagConflictLabel] != "" {
				return false
			}
		}
		return true
	}
	var volume *instance.Volume
	if id := labels[rootVolumeLabel]; id != "" {
		response, err := client.Instance().GetVolume(&instance.GetVolumeRequest{Zone: zone, VolumeID: id}, scw.WithContext(ctx))
		if !create && isScalewayNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if response != nil {
			volume = response.Volume
		}
	} else if claim.FixedCreateIntent.Attempt["volume_submitted"] != "" {
		response, err := api.ListVolumes(&instance.ListVolumesRequest{Zone: zone, Project: scw.StringPtr(client.ProjectID()), Name: scw.StringPtr(name)}, scw.WithContext(ctx), scw.WithAllPages())
		if err != nil {
			return err
		}
		if response != nil {
			var found bool
			volume, found, err = core.SelectFixedCandidate(fixedLeaseKind, claim.LeaseID, response.Volumes, validate)
			if err != nil {
				return err
			}
			if !found {
				return core.FixedUncertainCustody(claim.LeaseID)
			}
		}
	} else {
		if !create {
			return nil
		}
		image, err := api.GetImage(&instance.GetImageRequest{Zone: zone, ImageID: claim.FixedCreateIntent.Attempt["image"]}, scw.WithContext(ctx))
		if err != nil {
			return err
		}
		if image == nil || image.Image == nil || image.Image.RootVolume == nil || image.Image.RootVolume.ID == "" || len(image.Image.ExtraVolumes) != 0 {
			return core.Exit(4, "Scaleway fixed image requires one identifiable root snapshot")
		}
		if err := tx.Observe(core.FixedResourceBinding{AttemptValues: map[string]string{"volume_submitted": "true", "root_snapshot": image.Image.RootVolume.ID}}); err != nil {
			return err
		}
		response, err := api.CreateVolume(&instance.CreateVolumeRequest{Zone: zone, Name: name, Project: scw.StringPtr(client.ProjectID()),
			Tags: tagsFromLabels(labels), VolumeType: image.Image.RootVolume.VolumeType, BaseSnapshot: scw.StringPtr(image.Image.RootVolume.ID)}, scw.WithContext(ctx))
		if err != nil {
			return err
		}
		if response != nil {
			volume = response.Volume
		}
	}
	if !validate(volume) {
		return core.Exit(4, "lease_id_conflict: Scaleway fixed root volume has no matching allocation evidence")
	}
	labels[rootVolumeLabel] = volume.ID
	if err := rootVolumeFromLabels(labels).validate(); err != nil {
		return err
	}
	return tx.Observe(core.FixedResourceBinding{Labels: labels})
}
