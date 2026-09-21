package scaleway

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	instance "github.com/scaleway/scaleway-sdk-go/api/instance/v1"
	"github.com/scaleway/scaleway-sdk-go/scw"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	rootVolumeContract  = "created-root-v1"
	volumeContractLabel = "scaleway_volume_contract"
	rootVolumeLabel     = "scaleway_root_volume_id"
	volumePendingLabel  = "scaleway_volume_tags_pending"
)

// Allocation provenance is immutable: never infer it from later attachments.
type rootVolumeManifest struct {
	contract string
	id       string
	pending  bool
}

func rootVolumeFromLabels(labels map[string]string) rootVolumeManifest {
	return rootVolumeManifest{contract: labels[volumeContractLabel], id: labels[rootVolumeLabel], pending: labels[volumePendingLabel] == "true"}
}

func (m rootVolumeManifest) addLabels(labels map[string]string) {
	if m.contract != "" {
		labels[volumeContractLabel] = m.contract
	}
	if m.id != "" {
		labels[rootVolumeLabel] = m.id
	}
}

func (m rootVolumeManifest) validate() error {
	if m.contract == "" && m.id == "" && !m.pending {
		return nil
	} // Leases created before volume tracking.
	id, err := uuid.Parse(m.id)
	if m.contract != rootVolumeContract || err != nil || id.String() != m.id || id == uuid.Nil {
		return core.Exit(4, "Scaleway root-volume allocation identity is incomplete or invalid; resources and claim retained")
	}
	return nil
}

func validateRootVolumeIdentity(claim core.LeaseClaim, server core.Server, cleanup bool) error {
	want, got := rootVolumeFromLabels(claim.Labels), rootVolumeFromLabels(server.Labels)
	if err := want.validate(); err != nil {
		return err
	}
	recovery := claim.Labels["recovery"]
	if want.pending && (!cleanup || recovery != "rollback-cleanup" && recovery != "kept-after-failure") {
		return core.Exit(4, "Scaleway root-volume publication remains pending; use targeted stop to finish cleanup")
	}
	// Only a claim written before the allocation's tag publication may lack
	// matching live tags, and only destruction may consume this exception.
	if cleanup && want.pending && want.id != "" && (recovery == "rollback-cleanup" || recovery == "kept-after-failure") && got.contract == want.contract && got.id == "" {
		return nil
	}
	if err := got.validate(); err != nil {
		return err
	}
	if want.contract == got.contract && want.id == got.id {
		return nil
	}
	return core.Exit(2, "Scaleway root-volume allocation identity differs from the local claim; refusing operation")
}

func (b *Backend) validateVolumeMutation(server core.Server) (core.LeaseClaim, bool, error) {
	claim, exists, err := core.ReadLeaseClaimWithPresence(server.Labels["lease"])
	if err != nil {
		return claim, exists, err
	}
	if !exists {
		if rootVolumeFromLabels(server.Labels).contract != "" || rootVolumeFromLabels(server.Labels).id != "" {
			return claim, exists, core.Exit(2, "Scaleway root-volume metadata requires an exact local allocation claim")
		}
		return claim, exists, nil
	}
	return claim, exists, validateScalewayClaimIdentity(claim, server, false)
}

func inspectRootVolume(ctx context.Context, client Client, manifest rootVolumeManifest, serverID string, detached bool) (*instance.Volume, error) {
	if err := manifest.validate(); err != nil {
		return nil, err
	}
	if manifest.id == "" {
		return nil, nil
	}
	resp, err := client.Instance().GetVolume(&instance.GetVolumeRequest{Zone: scw.Zone(client.Zone()), VolumeID: manifest.id}, scw.WithContext(ctx))
	if isScalewayNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if resp == nil || resp.Volume == nil {
		return nil, core.Exit(4, "Scaleway root-volume observation omitted its identity; claim retained")
	}
	volume := resp.Volume
	if volume.ID != manifest.id || volume.Project != client.ProjectID() || volume.Zone != scw.Zone(client.Zone()) {
		return nil, core.Exit(2, "Scaleway root-volume identity or project/zone changed; refusing cleanup")
	}
	if volume.Server != nil && (detached || volume.Server.ID != serverID) {
		return nil, core.Exit(4, "Scaleway root volume is still attached; resources and claim retained")
	}
	return volume, nil
}

func deleteRootVolume(ctx context.Context, client Client, manifest rootVolumeManifest, serverID string) error {
	volume, err := inspectRootVolume(ctx, client, manifest, serverID, true)
	if err != nil || volume == nil {
		return err
	}
	err = client.Instance().DeleteVolume(&instance.DeleteVolumeRequest{Zone: scw.Zone(client.Zone()), VolumeID: manifest.id}, scw.WithContext(ctx))
	if err != nil && !isScalewayNotFound(err) {
		return fmt.Errorf("delete allocation-created Scaleway root volume: %w", err)
	}
	return nil
}

func (b *Backend) deleteAllocationResources(ctx context.Context, client Client, serverID string, manifest rootVolumeManifest) error {
	if _, err := inspectRootVolume(ctx, client, manifest, serverID, false); err != nil {
		return err
	}
	if err := b.deleteServerResource(ctx, client, serverID); err != nil {
		return err
	}
	return deleteRootVolume(ctx, client, manifest, serverID)
}
