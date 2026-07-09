package sealosdevbox

import (
	"context"
	"fmt"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	devboxStatePaused   = "Paused"
	devboxStateShutdown = "Shutdown"
)

func (b *backend) ReleaseLease(ctx context.Context, req core.ReleaseLeaseRequest) error {
	if err := core.ValidateLeaseTargetProviderIdentity(req.Lease, req.ExpectedProviderIdentity); err != nil {
		return err
	}
	claim, err := b.revalidateClaimSnapshot(req.Lease.Server, req.Lease.LeaseID)
	if err != nil {
		return err
	}
	name := releaseDevboxName(req.Lease, b.cfg)
	if name == "" {
		return core.Exit(2, "sealos-devbox release requires a DevBox name")
	}
	if err := b.validateStoredClaimResource(claim, name); err != nil {
		return err
	}
	if got := strings.TrimSpace(req.Lease.Server.DisplayID()); got != "" && got != strings.TrimSpace(claim.CloudID) {
		return core.Exit(4, "Sealos DevBox %q release resource does not match bound claim %s", name, claim.CloudID)
	}
	expectedSlug := ""
	if req.Lease.Server.Labels != nil {
		expectedSlug = req.Lease.Server.Labels["slug"]
	}
	requestedName := name
	item, name, leaseID, slug, err := b.validateDevboxIdentity(ctx, requestedName, req.Lease.LeaseID, expectedSlug)
	if err != nil {
		if b.deleteOnRelease(req.Lease) && kubernetesObjectNotFound(err) && req.Lease.LeaseID != "" {
			if err := core.RemoveLeaseClaimIfUnchangedAfter(req.Lease.LeaseID, claim, func() error {
				if _, err := b.getDevbox(ctx, requestedName); err == nil {
					return core.Exit(4, "refusing to remove Sealos claim %s after DevBox %q reappeared", req.Lease.LeaseID, requestedName)
				} else if !kubernetesObjectNotFound(err) {
					return err
				}
				core.RemoveStoredTestboxKey(req.Lease.LeaseID)
				return nil
			}); err != nil {
				return err
			}
			return nil
		}
		return err
	}
	if !b.itemMatchesScope(item) {
		return core.Exit(4, "Sealos DevBox %q is outside the active provider scope", name)
	}
	if err := b.validateClaimBinding(claim, item); err != nil {
		return err
	}
	server := b.serverFromDevbox(item)
	server.Labels["lease"] = leaseID
	server.Labels["slug"] = slug
	if b.deleteOnRelease(req.Lease) {
		action := func() error {
			if err := b.patchDevboxState(ctx, name, item.Metadata.ResourceVersion, devboxStateShutdown, nil); err != nil {
				return err
			}
			validated, _, _, _, err := b.validateDevboxIdentity(ctx, name, leaseID, slug)
			if err != nil {
				if kubernetesObjectNotFound(err) {
					core.RemoveStoredTestboxKey(leaseID)
					return nil
				}
				return err
			}
			if !b.itemMatchesScope(validated) {
				return core.Exit(4, "refusing to delete Sealos DevBox %q after its provider scope changed", name)
			}
			if err := b.validateClaimBinding(claim, validated); err != nil {
				return err
			}
			if err := b.deleteDevbox(ctx, validated); err != nil {
				return err
			}
			core.RemoveStoredTestboxKey(leaseID)
			return nil
		}
		if err := core.RemoveLeaseClaimIfUnchangedAfter(leaseID, claim, action); err != nil {
			return err
		}
		return nil
	}
	server.Status = "paused"
	server.Labels["state"] = "paused"
	server.Labels["release"] = "pause"
	annotations := annotationsFromLeaseLabels(server.Labels)
	action := func() error {
		return b.patchDevboxState(ctx, name, item.Metadata.ResourceVersion, devboxStatePaused, annotations)
	}
	_, err = core.UpdateLeaseClaimEndpointIfUnchangedAfter(leaseID, claim, server, core.SSHTarget{}, action)
	return err
}

func (b *backend) ReleaseLeaseMessage(lease core.LeaseTarget) string {
	action := "paused"
	if b.deleteOnRelease(lease) {
		action = "deleted"
	}
	return fmt.Sprintf("%s Sealos DevBox lease=%s devbox=%s", action, lease.LeaseID, lease.Server.Name)
}

func (b *backend) RetainLeaseClaimAfterRelease(lease core.LeaseTarget) bool {
	return !b.deleteOnRelease(lease)
}

func (b *backend) deleteOnRelease(lease core.LeaseTarget) bool {
	if core.DeleteOnReleaseExplicit(b.cfg, providerName) {
		return b.cfg.SealosDevbox.DeleteOnRelease
	}
	if strings.EqualFold(strings.TrimSpace(lease.Server.Labels["release"]), "delete") {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(lease.Server.Labels["delete_on_release"]), "true") {
		return true
	}
	return b.cfg.SealosDevbox.DeleteOnRelease
}

func releaseDevboxName(lease core.LeaseTarget, cfg core.Config) string {
	if lease.Server.Labels != nil {
		if name := strings.TrimSpace(lease.Server.Labels["devbox_name"]); name != "" {
			return name
		}
	}
	if name := strings.TrimSpace(lease.Server.Name); name != "" {
		return name
	}
	cloudID := strings.TrimSpace(lease.Server.CloudID)
	if cloudID != "" {
		prefix := strings.TrimSpace(cfg.SealosDevbox.Namespace) + "/"
		return strings.TrimPrefix(cloudID, prefix)
	}
	return ""
}

func annotationsFromLeaseLabels(labels map[string]string) map[string]any {
	annotations := make(map[string]any, len(labels)+1)
	for key, value := range labels {
		key = strings.TrimSpace(key)
		if key == "" || key == "provider_scope" {
			continue
		}
		annotations[annotationBase+key] = value
	}
	annotations[annotationBase+"provider_scope"] = nil
	annotations[annotationBase+"gateway_host"] = nil
	annotations[annotationBase+"gateway_port"] = nil
	annotations[annotationBase+"node_host"] = nil
	return annotations
}
