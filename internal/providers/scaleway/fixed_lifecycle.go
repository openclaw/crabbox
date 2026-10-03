package scaleway

import (
	"context"
	"maps"

	iam "github.com/scaleway/scaleway-sdk-go/api/iam/v1alpha1"
	instance "github.com/scaleway/scaleway-sdk-go/api/instance/v1"
	"github.com/scaleway/scaleway-sdk-go/scw"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *Backend) RetainLeaseClaimAfterReleaseWithClaim(lease core.LeaseTarget, previous core.LeaseClaim) (bool, error) {
	return fixedLeaseKind.RetainClaimAfterRelease(lease.LeaseID, previous, lease.Server.Labels["fixed_attempt"] != "", nil, nil)
}

func (b *Backend) resolveFixed(ctx context.Context, client Client, req core.ResolveRequest) (core.LeaseTarget, bool, error) {
	claim, exists, err := core.ResolveLeaseClaimForProvider(req.ID, providerName)
	if core.IsCanonicalLeaseID(req.ID) {
		claim, exists, err = core.ReadLeaseClaimWithPresence(req.ID)
	}
	if err != nil || !exists || claim.FixedCreateIntent == nil {
		return core.LeaseTarget{}, err != nil, err
	}
	if !fixedLeaseKind.IsFixedClaim(claim) || claim.ProviderScope != client.ProjectID() || claim.FixedCreateIntent.ProviderScope != client.ProjectID() ||
		claim.Labels["scaleway_zone"] != "" && claim.Labels["scaleway_zone"] != client.Zone() {
		return core.LeaseTarget{}, true, core.Exit(4, "lease_id_conflict: Scaleway fixed owner or project changed")
	}
	inspect := func(ctx context.Context, claim core.LeaseClaim) (core.LeaseTarget, error) {
		server := core.Server{Provider: providerName, CloudID: claim.CloudID, ImmutableID: claim.CloudImmutableID,
			Name: core.LeaseProviderName(claim.LeaseID, claim.Slug), Labels: maps.Clone(claim.Labels)}
		if claim.FixedCreateIntent.Attempt["server"] == "submitted" {
			item, err := b.loadFixedServer(ctx, client, claim)
			if err != nil && !(req.ReleaseOnly && claim.CloudID != "" && isScalewayNotFound(err)) {
				return core.LeaseTarget{}, err
			}
			if err == nil {
				server = b.serverFromScaleway(item)
				server.ImmutableID = item.ID
			}
		}
		ssh := core.SSHTargetFromConfig(b.cfgForRun(), server.PublicNet.IPv4.IP)
		if !req.ReleaseOnly {
			if !req.StatusOnly && (claim.FixedCreateIntent.State != "acquired" || server.CloudID == "") {
				return core.LeaseTarget{}, core.Exit(4, "Scaleway fixed acquisition is incomplete; replay warmup with the same lease ID")
			}
			if err := core.UseStoredTestboxKey(&ssh, claim.LeaseID); err != nil {
				return core.LeaseTarget{}, err
			}
		}
		return core.LeaseTarget{Server: server, SSH: ssh, LeaseID: claim.LeaseID}, nil
	}
	lease, err := core.ResolveFixedLeaseTarget(ctx, core.FixedResolveOptions{Kind: fixedLeaseKind, Request: req, Expected: claim,
		Provider: providerName, ResourceName: core.LeaseProviderName(claim.LeaseID, claim.Slug), TerminalState: "released", Now: b.clockNow},
		func(ctx context.Context, claim *core.LeaseClaim, _ func() error) (core.LeaseTarget, error) {
			return inspect(ctx, *claim)
		}, inspect)
	return lease, true, err
}

func (b *Backend) releaseFixed(ctx context.Context, client Client, expected core.LeaseClaim) error {
	err := core.DeleteFixedResource(ctx, fixedLeaseKind, expected, core.FixedLeaseOperations[*instance.Server]{
		Release: &core.FixedReleasePolicy{SkipTerminalObservation: true},
		ObserveExact: func(ctx context.Context, tx *core.FixedTransaction, _ core.FixedObserveMode) (core.FixedObservation[*instance.Server], error) {
			claim := tx.Claim
			if claim.ProviderScope != client.ProjectID() || b.validateProviderIdentity(claim.Labels, client) != nil {
				return core.FixedObservation[*instance.Server]{}, core.Exit(4, "lease_id_conflict: Scaleway fixed scope changed")
			}
			var item *instance.Server
			var err error
			if claim.FixedCreateIntent.Attempt["server"] == "submitted" {
				item, err = b.loadFixedServer(ctx, client, *claim)
				if claim.CloudID != "" && isScalewayNotFound(err) {
					err = nil
				}
				if err == nil && item != nil {
					err = tx.Bind(core.FixedResourceBinding{CloudID: item.ID, ImmutableID: item.ID})
				}
			} else {
				err = prepareFixedRoot(ctx, client, tx, false)
			}
			if err != nil {
				return core.FixedObservation[*instance.Server]{}, err
			}
			return core.FixedObservation[*instance.Server]{Candidates: []*instance.Server{item}}, nil
		},
		DeleteExact: func(ctx context.Context, tx *core.FixedTransaction, _ *instance.Server) error {
			claim := tx.Claim
			key, err := b.fixedKey(ctx, client, tx, "", false)
			if err != nil {
				return err
			}
			labels := claim.Labels
			if err := tx.Record("deleting"); err != nil {
				return err
			}
			if claim.CloudID != "" {
				if err := b.deleteAllocationResources(ctx, client, claim.CloudID, rootVolumeFromLabels(labels)); err != nil {
					return err
				}
			} else if labels[rootVolumeLabel] != "" {
				if err := deleteRootVolume(ctx, client, rootVolumeFromLabels(labels), ""); err != nil {
					return err
				}
			}
			if key != nil {
				if err := client.IAM().DeleteSSHKey(&iam.DeleteSSHKeyRequest{SSHKeyID: key.ID}, scw.WithContext(ctx)); err != nil && !isScalewayNotFound(err) {
					return err
				}
			}
			return nil
		},
	}, b.clockNow)
	if err == nil {
		core.RemoveStoredTestboxKey(expected.LeaseID)
	}
	return err
}
