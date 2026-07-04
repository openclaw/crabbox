package sealosdevbox

import (
	"context"
	"fmt"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *backend) Cleanup(ctx context.Context, req core.CleanupRequest) error {
	items, err := b.listDevboxes(ctx)
	if err != nil {
		return err
	}
	observedLeaseIDs := map[string]bool{}
	observedNames := map[string]bool{}
	now := b.now()
	for _, item := range items {
		if leaseID := strings.TrimSpace(item.Metadata.Labels[leaseIDLabel]); leaseID != "" {
			observedLeaseIDs[leaseID] = true
		}
		if name := strings.TrimSpace(item.Metadata.Name); name != "" {
			observedNames[name] = true
		}
		if !b.cleanupItemMatchesScope(item) {
			b.printCleanupSkip(item, "outside active provider scope")
			continue
		}
		server := b.serverFromDevbox(item)
		leaseID := strings.TrimSpace(server.Labels["lease"])
		shouldDelete, reason := core.ShouldCleanupServer(server, now)
		if !shouldDelete {
			b.printCleanupSkip(item, reason)
			continue
		}
		fmt.Fprintf(b.rt.Stdout, "sealos-devbox cleanup delete devbox=%s lease=%s reason=%s dry_run=%t\n", server.Name, core.Blank(leaseID, "-"), reason, req.DryRun)
		if req.DryRun {
			continue
		}
		validated, _, _, _, err := b.validateDevboxIdentity(ctx, server.Name, leaseID, server.Labels["slug"])
		if err != nil {
			return err
		}
		if !b.cleanupItemMatchesScope(validated) {
			return core.Exit(4, "refusing to delete Sealos DevBox %q after its provider scope changed", server.Name)
		}
		if err := b.deleteDevbox(ctx, validated); err != nil {
			return err
		}
		if leaseID != "" {
			core.RemoveLeaseClaim(leaseID)
			core.RemoveStoredTestboxKey(leaseID)
		}
	}
	return b.cleanupStaleClaims(ctx, observedLeaseIDs, observedNames, req.DryRun)
}

func (b *backend) cleanupItemMatchesScope(item devboxItem) bool {
	if item.Metadata.Labels[managedByLabel] != "crabbox" {
		return false
	}
	if item.Metadata.Labels[providerLabel] != providerName {
		return false
	}
	return b.itemHasActiveScope(item)
}

func (b *backend) printCleanupSkip(item devboxItem, reason string) {
	name := strings.TrimSpace(item.Metadata.Name)
	if name == "" {
		name = "-"
	}
	leaseID := strings.TrimSpace(item.Metadata.Labels[leaseIDLabel])
	fmt.Fprintf(b.rt.Stderr, "skip sealos-devbox devbox=%s lease=%s reason=%s\n", name, core.Blank(leaseID, "-"), reason)
}

func (b *backend) cleanupStaleClaims(ctx context.Context, observedLeaseIDs, observedNames map[string]bool, dryRun bool) error {
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return err
	}
	for _, claim := range claims {
		if !b.claimMatchesScope(claim) || strings.TrimSpace(claim.LeaseID) == "" {
			continue
		}
		name := strings.TrimSpace(devboxNameFromClaim(claim, b.cfg))
		if observedLeaseIDs[claim.LeaseID] || observedNames[name] {
			continue
		}
		// The managed inventory is label-filtered. Only a direct name lookup can
		// prove that a drifted resource is actually absent before recovery state
		// and its SSH key are discarded.
		if name == "" {
			continue
		}
		if _, err := b.getDevbox(ctx, name); err == nil {
			continue
		} else if !kubernetesObjectNotFound(err) {
			return err
		}
		fmt.Fprintf(b.rt.Stdout, "sealos-devbox cleanup stale-claim lease=%s reason=absent dry_run=%t\n", claim.LeaseID, dryRun)
		if dryRun {
			continue
		}
		core.RemoveLeaseClaim(claim.LeaseID)
		core.RemoveStoredTestboxKey(claim.LeaseID)
	}
	return nil
}
