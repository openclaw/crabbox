package applecontainer

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// FinalizeAfterCleanup holds the exact claim fence across inspection and deletion.
func (b *backend) removeClaimedContainer(ctx context.Context, id string, claim core.LeaseClaim) error {
	if !fixedAppleContainerLeaseKind.IsFixedClaim(claim) {
		return b.removeContainer(ctx, id)
	}
	cfg := b.configForRun()
	intent := claim.FixedCreateIntent
	if intent.Version != fixedAppleContainerIntentVersion || !core.FixedSHA256(intent.Fingerprint) ||
		intent.ProviderScope != fixedAppleContainerProviderScope(cfg) || claim.ProviderScope != intent.ProviderScope ||
		(intent.State != "prepared" && intent.State != "acquired") ||
		claim.CloudID != id || claim.Slug != intent.Slug || strings.TrimSpace(claim.Labels["image"]) == "" {
		return core.Exit(4, "lease_id_conflict: invalid fixed apple-container cleanup binding for %s", claim.LeaseID)
	}
	// Cleanup follows the recorded image and pond, even if current defaults changed.
	cfg.AppleContainer.Image = claim.Labels["image"]
	cfg.Pond = claim.Labels["pond"]
	observed, err := b.inspectImageContainer(ctx, cfg, id)
	if err != nil {
		return err
	}
	if err := validateFixedAppleContainer(observed.container, cfg, claim.LeaseID, intent.Slug, intent.Fingerprint); err != nil {
		return err
	}
	if _, err := b.imageControl(ctx, cfg, []string{"delete", "--force", id}, 30*time.Second); err != nil {
		return err
	}
	return b.confirmFixedContainerAbsent(ctx, claim)
}

func (b *backend) confirmFixedContainerAbsent(ctx context.Context, claim core.LeaseClaim) error {
	cfg := b.configForRun()
	if claim.FixedCreateIntent.ProviderScope != fixedAppleContainerProviderScope(cfg) {
		return core.Exit(4, "lease_id_conflict: fixed apple-container cleanup scope changed for %s", claim.LeaseID)
	}
	result, err := b.imageControl(ctx, cfg, []string{"ls", "--all", "--format", "json"}, 30*time.Second)
	if err != nil {
		return err
	}
	var containers []inspectContainer
	if json.Unmarshal([]byte(result.Stdout), &containers) != nil || containers == nil {
		return core.Exit(5, "Apple Container cleanup inventory is invalid; fixed claim retained")
	}
	name := core.LeaseProviderName(claim.LeaseID, claim.FixedCreateIntent.Slug)
	for _, container := range containers {
		// Do not filter by ownership labels: altered labels cannot prove absence.
		if container.id() == "" || container.id() == name || container.id() == claim.CloudID || container.labels()["lease"] == claim.LeaseID {
			return core.Exit(5, "Apple Container cleanup absence is unconfirmed; fixed claim retained")
		}
	}
	return nil
}
