package smolvm

import (
	"context"
	"fmt"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const machineCreatedLabel = "smolvm_created_at"

func validateMachineIdentity(actual, expected machineData) error {
	if strings.TrimSpace(expected.ID) == "" || expected.Name == "" || strings.TrimSpace(expected.CreatedAt) == "" ||
		actual.ID != expected.ID || actual.Name != expected.Name || actual.CreatedAt != expected.CreatedAt {
		return exit(2, "smolvm machine %q identity is missing or changed; retaining machine and claim", expected.ID)
	}
	return nil
}

func (b *backend) claimBinding(claim core.LeaseClaim) (shared.ClaimBinding, error) {
	scope, err := smolvmEndpoint(b.cfg)
	if err != nil {
		return shared.ClaimBinding{}, err
	}
	if !core.IsCanonicalLeaseID(claim.LeaseID) || claim.Slug == "" || strings.TrimSpace(claim.CloudID) == "" || claim.Revision == "" || strings.TrimSpace(claim.Labels[machineCreatedLabel]) == "" {
		return shared.ClaimBinding{}, exit(2, "smolvm lease %q requires an exact local ownership claim; legacy or unclaimed machines are retained", claim.LeaseID)
	}
	want := shared.ClaimBinding{
		Provider: providerName, ProviderScope: scope, ExactProviderScope: true,
		LeaseID: claim.LeaseID, Slug: claim.Slug, CloudID: claim.CloudID,
		RequiredLabels: map[string]string{
			"machine_id": claim.CloudID, "machine_name": machineName(claim.LeaseID, claim.Slug),
			machineCreatedLabel: claim.Labels[machineCreatedLabel],
		},
	}
	if err := shared.ValidateClaimBinding(claim, want); err != nil {
		return shared.ClaimBinding{}, exit(2, "smolvm exact local ownership claim mismatch: %v", err)
	}
	return want, nil
}

func machineFromClaim(claim core.LeaseClaim) machineData {
	return machineData{ID: claim.CloudID, Name: claim.Labels["machine_name"], CreatedAt: claim.Labels[machineCreatedLabel]}
}

// Inventory names are discovery hints, never authority to adopt or destroy.
func (b *backend) resolveOwnedMachine(id string) (core.LeaseClaim, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return core.LeaseClaim{}, exit(2, "smolvm requires a lease id, slug, or machine id/name")
	}
	if core.IsCanonicalLeaseID(id) {
		claim, exists, err := core.ReadLeaseClaimWithPresence(id)
		if err != nil {
			return core.LeaseClaim{}, err
		}
		if !exists {
			return core.LeaseClaim{}, exit(2, "smolvm %q has no exact local ownership claim", id)
		}
		_, err = b.claimBinding(claim)
		return claim, err
	}
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return core.LeaseClaim{}, err
	}
	var matches []core.LeaseClaim
	for _, claim := range claims {
		if claim.Provider == providerName && (id == claim.Slug || id == claim.CloudID || id == claim.Labels["machine_name"]) {
			matches = append(matches, claim)
		}
	}
	if len(matches) != 1 {
		return core.LeaseClaim{}, exit(2, "smolvm %q requires one unambiguous exact local ownership claim (found %d)", id, len(matches))
	}
	_, err = b.claimBinding(matches[0])
	return matches[0], err
}

func (b *backend) publishMachineClaim(leaseID, slug string, machine machineData, repo Repo) (core.LeaseClaim, error) {
	scope, err := smolvmEndpoint(b.cfg)
	if err != nil {
		return core.LeaseClaim{}, err
	}
	var published core.LeaseClaim
	err = core.WithDurableLeaseClaimLock(leaseID, func(claim *core.LeaseClaim, exists bool, persist func() error) error {
		if exists {
			return exit(2, "smolvm lease %s acquired a claim during creation; retaining machine", leaseID)
		}
		now := time.Now().UTC().Format(time.RFC3339)
		*claim = core.LeaseClaim{
			LeaseID: leaseID, Slug: slug, Provider: providerName, ProviderScope: scope, CloudID: machine.ID,
			RepoRoot: repo.Root, ClaimedAt: now, LastUsedAt: now, IdleTimeoutSeconds: int(b.cfg.IdleTimeout.Seconds()),
			Labels: map[string]string{"provider": providerName, "lease": leaseID, "slug": slug,
				"machine_id": machine.ID, "machine_name": machine.Name, machineCreatedLabel: machine.CreatedAt},
		}
		if err := persist(); err != nil {
			return err
		}
		published = *claim
		return nil
	})
	return published, err
}

func (b *backend) reuseMachine(ctx context.Context, client api, id, repoRoot string, reclaim bool) (core.LeaseClaim, error) {
	claim, err := b.resolveOwnedMachine(id)
	if err != nil {
		return core.LeaseClaim{}, err
	}
	if repoRoot == "" {
		return core.LeaseClaim{}, exit(2, "smolvm reuse requires repository context")
	}
	server := Server{Provider: providerName, CloudID: claim.CloudID, Labels: claim.Labels}
	return core.ClaimLeaseTargetForRepoConfigScopeIfUnchangedDurableAfter(claim.LeaseID, claim.Slug, b.cfg, claim.ProviderScope, server, core.SSHTarget{}, repoRoot, b.cfg.IdleTimeout, reclaim, claim, true, func() error {
		machine, err := client.GetMachine(ctx, claim.CloudID)
		if err != nil {
			return err
		}
		return validateMachineIdentity(machine, machineFromClaim(claim))
	})
}

func (b *backend) deleteOwnedMachine(ctx context.Context, client api, claim core.LeaseClaim) error {
	binding, err := b.claimBinding(claim)
	if err != nil {
		return err
	}
	return shared.RemoveExactClaimAfter(claim, binding, func() error {
		return deleteExactMachine(ctx, client, machineFromClaim(claim))
	})
}

func deleteExactMachine(ctx context.Context, client api, expected machineData) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := client.GetMachine(ctx, expected.ID)
	// Hosted 404 also means another tenant: initial absence cannot erase ownership.
	if err != nil {
		return fmt.Errorf("smolvm ownership lookup; retaining claim: %w", err)
	}
	if err := validateMachineIdentity(current, expected); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := client.DeleteMachine(ctx, expected.ID); err != nil {
		return err
	}
	for {
		current, err = client.GetMachine(ctx, expected.ID)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if isNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("smolvm deletion confirmation; retaining claim: %w", err)
		}
		if err := validateMachineIdentity(current, expected); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
