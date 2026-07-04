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
	name := releaseDevboxName(req.Lease, b.cfg)
	if name == "" {
		return core.Exit(2, "sealos-devbox release requires a DevBox name")
	}
	expectedSlug := ""
	if req.Lease.Server.Labels != nil {
		expectedSlug = req.Lease.Server.Labels["slug"]
	}
	item, name, leaseID, slug, err := b.validateDevboxIdentity(ctx, name, req.Lease.LeaseID, expectedSlug)
	if err != nil {
		if b.deleteOnRelease(req.Lease) && kubernetesObjectNotFound(err) && req.Lease.LeaseID != "" {
			core.RemoveLeaseClaim(req.Lease.LeaseID)
			core.RemoveStoredTestboxKey(req.Lease.LeaseID)
			return nil
		}
		return err
	}
	if !b.itemMatchesScope(item) {
		return core.Exit(4, "Sealos DevBox %q is outside the active provider scope", name)
	}
	server := b.serverFromDevbox(item)
	server.Labels["lease"] = leaseID
	server.Labels["slug"] = slug
	if b.deleteOnRelease(req.Lease) {
		if err := b.patchDevboxState(ctx, name, item.Metadata.ResourceVersion, devboxStateShutdown, nil); err != nil {
			return err
		}
		item, _, _, _, err = b.validateDevboxIdentity(ctx, name, leaseID, slug)
		if err != nil {
			if kubernetesObjectNotFound(err) {
				core.RemoveLeaseClaim(leaseID)
				core.RemoveStoredTestboxKey(leaseID)
				return nil
			}
			return err
		}
		if !b.itemMatchesScope(item) {
			return core.Exit(4, "refusing to delete Sealos DevBox %q after its provider scope changed", name)
		}
		if err := b.deleteDevbox(ctx, item); err != nil {
			return err
		}
		core.RemoveLeaseClaim(leaseID)
		core.RemoveStoredTestboxKey(leaseID)
		return nil
	}
	server.Status = "paused"
	server.Labels["state"] = "paused"
	server.Labels["release"] = "pause"
	annotations := annotationsFromLeaseLabels(server.Labels)
	claim, claimOK, err := core.ResolveLeaseClaimForProvider(leaseID, providerName)
	if err != nil {
		return err
	}
	action := func() error {
		return b.patchDevboxState(ctx, name, item.Metadata.ResourceVersion, devboxStatePaused, annotations)
	}
	if claimOK {
		_, err = core.UpdateLeaseClaimEndpointIfUnchangedAfter(leaseID, claim, server, core.SSHTarget{}, action)
		return err
	}
	if err := action(); err != nil {
		return err
	}
	return core.UpdateLeaseClaimEndpoint(leaseID, server, core.SSHTarget{})
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
